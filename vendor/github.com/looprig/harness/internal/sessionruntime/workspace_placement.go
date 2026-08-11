package sessionruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/workspacestore"
	"github.com/looprig/storage"
)

// workspace_placement.go implements the OPTIONAL managed-workspace placement the rig
// declares (design §"Optional workspace lifecycle" / §"Placement details"). A placement
// resolves, per session, to a concrete workspace root, an (optional) exclusive root
// lease, and the ONE session-scoped mutation coordinator every loop's file/Bash tools
// serialize through. Without a placement the session has no managed root, no coordinator,
// and CheckpointWorkspace/RestoreWorkspace fail closed (see checkpoint.go).
//
// Three placement modes, matching the design table:
//
//   - Exclusive: one canonical fixed root fenced by a single storage.Leaser lease named
//     workspace-roots/<sha256(canonical-root)>. The session lease is acquired first (by
//     the lifecycle), then the root lease here; root-lease loss faults the session.
//   - Per-session: baseDir/<sessionID>, isolated by construction — no lease. Restore uses
//     a staged verify/swap.
//   - Shared: one canonical fixed root, deliberately NO lease; every checkpoint is fuzzy.

// WorkspacePlacementMode selects how a session's managed workspace root is provisioned.
type WorkspacePlacementMode uint8

const (
	// PlacementNone is the zero value: no managed workspace.
	PlacementNone WorkspacePlacementMode = iota
	// PlacementExclusive fences one canonical fixed root with an exclusive root lease.
	PlacementExclusive
	// PlacementSession derives an isolated baseDir/<sessionID> root per session.
	PlacementSession
	// PlacementShared shares one canonical fixed root with no lease.
	PlacementShared
)

// WorkspacePlacement is the resolved-at-Define descriptor the Lifecycle carries to bring
// up a session's managed workspace. rig builds it from its placement options and validates
// the persistence-overlap invariant before constructing the Lifecycle. Store is the blob
// store checkpoints/restores go through; Root is the canonical fixed root (exclusive and
// shared); BaseDir is the canonical per-session base (session mode); Leaser + LeaseName
// are the exclusive root lease (exclusive mode only).
type WorkspacePlacement struct {
	Mode      WorkspacePlacementMode
	Store     *workspacestore.Store
	Root      string
	BaseDir   string
	Leaser    storage.Leaser
	LeaseName string
}

// Configured reports whether the placement provisions a managed workspace.
func (p WorkspacePlacement) Configured() bool {
	return p.Mode != PlacementNone && p.Store != nil
}

// resolvedPlacement is the per-session result of bringing up a placement: the concrete
// root, the session mutation coordinator, the root-lease release hook (nil for non-leased
// modes), and the lease-loss channel the session watches to fault on ownership loss.
type resolvedPlacement struct {
	mode        WorkspacePlacementMode
	store       *workspacestore.Store
	root        string
	coordinator *workspaceCoordinator
	rootRelease func(context.Context) error
	leaseLost   <-chan struct{}
}

// resolveForNew brings up the placement for a fresh session id: it derives the root,
// runs per-session startup recovery, acquires the exclusive root lease when required, and
// builds the session-scoped coordinator wired to the lease health. It does NOT materialize
// any seed — the lifecycle does that after resolution and before session construction so
// the seed lands in an empty root. On root-lease contention it returns a typed
// *WorkspaceRootBusyError WITHOUT having mutated anything; the caller releases the session
// lease.
func (p WorkspacePlacement) resolveForNew(ctx context.Context, sid uuid.UUID) (*resolvedPlacement, error) {
	root, err := p.rootFor(sid)
	if err != nil {
		return nil, err
	}
	if p.Mode == PlacementSession {
		if err := recoverSessionRoot(root); err != nil {
			return nil, err
		}
	}
	if p.Mode != PlacementExclusive {
		return &resolvedPlacement{mode: p.Mode, store: p.Store, root: root, coordinator: newWorkspaceCoordinator(nil)}, nil
	}
	lease, err := p.Leaser.Acquire(ctx, p.LeaseName)
	if err != nil {
		var held *storage.LeaseHeldError
		_ = errors.As(err, &held)
		var holderEpoch uint64
		if held != nil {
			holderEpoch = held.HolderEpoch
		}
		return nil, &WorkspaceRootBusyError{Root: root, HolderEpoch: holderEpoch, Cause: err}
	}
	health := &rootLeaseHealth{lost: lease.Lost()}
	return &resolvedPlacement{
		mode:        p.Mode,
		store:       p.Store,
		root:        root,
		coordinator: newWorkspaceCoordinator(health),
		rootRelease: lease.Release,
		leaseLost:   lease.Lost(),
	}, nil
}

// rootFor returns the concrete workspace root for a session id. Fixed roots (exclusive /
// shared) are already canonical; the per-session root is baseDir/<sessionID>, the
// injective non-symlinked destination the design mandates.
func (p WorkspacePlacement) rootFor(sid uuid.UUID) (string, error) {
	switch p.Mode {
	case PlacementExclusive, PlacementShared:
		return p.Root, nil
	case PlacementSession:
		if sid.IsZero() {
			return "", &PlacementResolutionError{Reason: "per-session placement requires a non-zero session id"}
		}
		return filepath.Join(p.BaseDir, sid.String()), nil
	default:
		return "", &PlacementResolutionError{Reason: "unconfigured placement has no root"}
	}
}

// recoverSessionRoot performs the design's per-session startup recovery: it removes an
// abandoned staging directory, and — when the live root is absent — restores an orphaned
// backup left by a crash between the two renames of a prior swap. It never touches the
// root when the root already exists (the common warm-start path).
func recoverSessionRoot(root string) error {
	parent := filepath.Dir(root)
	if err := establishCanonicalDirectory(parent); err != nil {
		return err
	}
	staging := sessionStagingPath(root)
	backup := sessionBackupPath(root)
	if err := removeIfExists(staging); err != nil {
		return &WorkspaceRecoveryError{Path: staging, Reason: "remove abandoned staging", Cause: err}
	}
	rootInfo, rootErr := os.Lstat(root)
	switch {
	case rootErr == nil:
		if err := requireRealDirectory(root, rootInfo); err != nil {
			return err
		}
		// Root present: a leftover backup is stale history; remove it.
		backupInfo, err := os.Lstat(backup)
		if err == nil {
			if err := requireRealDirectory(backup, backupInfo); err != nil {
				return err
			}
			if err := os.RemoveAll(backup); err != nil {
				return &WorkspaceRecoveryError{Path: backup, Reason: "remove stale backup", Cause: err}
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return &WorkspaceRecoveryError{Path: backup, Reason: "inspect stale backup", Cause: err}
		}
		return nil
	case !errors.Is(rootErr, os.ErrNotExist):
		return &WorkspaceRecoveryError{Path: root, Reason: "inspect live root", Cause: rootErr}
	}

	backupInfo, backupErr := os.Lstat(backup)
	switch {
	case backupErr == nil:
		if err := requireRealDirectory(backup, backupInfo); err != nil {
			return err
		}
		if err := establishCanonicalDirectory(parent); err != nil {
			return err
		}
		if err := os.Rename(backup, root); err != nil {
			return &WorkspaceRecoveryError{Path: backup, Reason: "restore orphaned backup", Cause: err}
		}
		return revalidateRealDirectory(root)
	case !errors.Is(backupErr, os.ErrNotExist):
		return &WorkspaceRecoveryError{Path: backup, Reason: "inspect orphaned backup", Cause: backupErr}
	}

	// A brand-new per-session placement has neither a live root nor a backup.
	// The canonical parent is established above; create ONLY the final UUID
	// component exclusively so an attacker cannot substitute a destination.
	if err := establishCanonicalDirectory(parent); err != nil {
		return err
	}
	if err := os.Mkdir(root, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return &WorkspaceRecoveryError{Path: root, Reason: "create live root", Cause: err}
	}
	return revalidateRealDirectory(root)
}

func requireRealDirectory(path string, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 {
		return &WorkspaceRecoveryError{Path: path, Reason: "symlink is not an allowed workspace directory"}
	}
	if !info.IsDir() {
		return &WorkspaceRecoveryError{Path: path, Reason: "path is not a directory"}
	}
	return nil
}

func revalidateRealDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return &WorkspaceRecoveryError{Path: path, Reason: "revalidate directory", Cause: err}
	}
	return requireRealDirectory(path, info)
}

// establishCanonicalDirectory creates missing components one at a time and
// rejects every symlink/non-directory component. The final EvalSymlinks equality
// check detects stable parent substitution relative to the canonical path frozen
// by rig.Define.
func establishCanonicalDirectory(path string) error {
	clean := filepath.Clean(path)
	volume := filepath.VolumeName(clean)
	remainder := strings.TrimPrefix(clean, volume)
	separator := string(os.PathSeparator)
	if !strings.HasPrefix(remainder, separator) {
		return &WorkspaceRecoveryError{Path: path, Reason: "base parent is not absolute"}
	}
	current := volume + separator
	for _, component := range strings.Split(strings.TrimPrefix(remainder, separator), separator) {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return &WorkspaceRecoveryError{Path: current, Reason: "establish canonical base parent", Cause: err}
			}
			info, err = os.Lstat(current)
		}
		if err != nil {
			return &WorkspaceRecoveryError{Path: current, Reason: "inspect canonical base parent", Cause: err}
		}
		if err := requireRealDirectory(current, info); err != nil {
			return err
		}
	}
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return &WorkspaceRecoveryError{Path: clean, Reason: "resolve canonical base parent", Cause: err}
	}
	if resolved != clean {
		return &WorkspaceRecoveryError{Path: clean, Reason: "base parent no longer matches its canonical definition"}
	}
	return nil
}

// sessionStagingPath and sessionBackupPath derive the two sibling scratch paths a
// per-session staged swap uses. They are siblings of the root (same parent) so a rename
// is atomic within one filesystem, and are named deterministically from the root's base
// so recovery can find them without any external state.
func sessionStagingPath(root string) string {
	return filepath.Join(filepath.Dir(root), "."+filepath.Base(root)+".staging")
}

func sessionBackupPath(root string) string {
	return filepath.Join(filepath.Dir(root), "."+filepath.Base(root)+".backup")
}

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func removeIfExists(path string) error {
	if !pathExists(path) {
		return nil
	}
	return os.RemoveAll(path)
}

// rootIsEmpty reports whether root is absent or an empty directory — the precondition for
// materializing a seed or an exclusive attach-vs-materialize decision. A non-directory or
// a non-empty directory is not empty. A read error is surfaced so the caller fails closed.
func rootIsEmpty(root string) (bool, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return true, nil
		}
		return false, err
	}
	return len(entries) == 0, nil
}

// materializeSeed restores the seed ref into the (empty) session root before construction.
// It is valid only for per-session and an EMPTY exclusive root; shared placement and a
// non-empty root are rejected fail-closed. The ref must resolve in the configured store
// (workspacestore.Materialize verifies it). resolved is the per-session resolution.
func (p WorkspacePlacement) materializeSeed(ctx context.Context, resolved *resolvedPlacement, seed workspacestore.Ref) error {
	if resolved == nil {
		return &WorkspaceSeedError{Reason: "seed requires a configured placement"}
	}
	if p.Mode == PlacementShared {
		return &WorkspaceSeedError{Reason: "shared placement cannot be seeded"}
	}
	empty, err := rootIsEmpty(resolved.root)
	if err != nil {
		return &WorkspaceSeedError{Reason: "cannot inspect workspace root", Cause: err}
	}
	if !empty {
		return &WorkspaceSeedError{Reason: "seed requires an empty workspace root"}
	}
	if err := resolved.store.Materialize(ctx, seed, resolved.root); err != nil {
		return &WorkspaceSeedError{Reason: "materialize seed failed", Cause: err}
	}
	return nil
}

// WorkspaceSeedError reports an invalid or failed workspace seed: a shared placement, a
// non-empty root, or a materialize failure (a ref that does not resolve). It is a
// fail-closed NewSession failure — the session never comes up on a bad seed.
type WorkspaceSeedError struct {
	Reason string
	Cause  error
}

func (e *WorkspaceSeedError) Error() string {
	msg := "sessionruntime: workspace seed rejected: " + e.Reason
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}
	return msg
}

func (e *WorkspaceSeedError) Unwrap() error { return e.Cause }

// rootLeaseHealth reports the exclusive root lease's health to the coordinator. Healthy
// returns a typed error once the lease's Lost channel has closed (expiry/takeover), so a
// structured mutator refuses to commit after ownership is lost (fail-secure).
type rootLeaseHealth struct {
	lost <-chan struct{}
}

func (h *rootLeaseHealth) Healthy() error {
	if h == nil || h.lost == nil {
		return nil
	}
	select {
	case <-h.lost:
		return &WorkspaceRootLeaseLostError{}
	default:
		return nil
	}
}

// PlacementResolutionError reports an invalid placement resolution (e.g. a per-session
// placement with a zero session id). It is a construction-time, fail-closed failure.
type PlacementResolutionError struct {
	Reason string
}

func (e *PlacementResolutionError) Error() string {
	return "sessionruntime: invalid workspace placement: " + e.Reason
}
