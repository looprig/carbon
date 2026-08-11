package sessionruntime

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/harness/pkg/workspacestore"
)

// workspace_restore.go implements manual workspace rewind (design §"Manual rewind").
// RestoreWorkspace is control-plane only and valid while idle: it takes the exclusive
// restore permit, stages and verifies the target ref, replaces the target SAFELY, then
// appends WorkspaceRestored{Ref} AFTER the filesystem commit. Append failure faults the
// session because the live tree changed without advancing the durable pointer.
//
// Per-session roots use a verified whole-root swap (materialize into an empty sibling
// staging dir → verify → rename live root to a sibling backup → rename staging to root →
// remove backup; a second-rename failure restores the backup). Fixed exclusive/shared
// roots NEVER rename or recursively wipe the configured root: they materialize the ref
// into a sibling staging dir, build a manifest, take the affected paths in sorted order,
// keep a rollback copy of every affected existing file, then commit replacements and
// deletions deterministically. Failure rolls back in reverse order.

// RestoreWorkspace rewinds the managed workspace to ref. It fails closed with
// *WorkspaceNotConfiguredError when no placement is configured, and with a typed
// *WorkspaceRestoreError on any admission, staging, or commit failure.
func (s *Session) RestoreWorkspace(ctx context.Context, ref workspacestore.Ref) error {
	if s.ws == nil || s.wsCoordinator == nil {
		return &WorkspaceNotConfiguredError{}
	}
	if err := ctx.Err(); err != nil {
		return &WorkspaceRestoreError{Kind: WorkspaceRestoreContextDone, Cause: err}
	}
	if err := s.restoreAdmissible(); err != nil {
		return err
	}
	// Suspend NEW process admission before requesting the exclusive checkpoint
	// permit below, so nothing starts a fresh background process against a
	// workspace that is about to be swapped out from under it. resumeAdmission runs
	// on EVERY return path (the defer below): a failed or successful rewind must
	// never strand the registry suspended. This does not force-stop an already
	// admitted resource's live process — a registered SessionResource is a
	// long-lived supervisor the checked publication path and the session's own
	// Shutdown already own tearing down exactly once (session_resources.go); an
	// already-running writable lifetime lease still drains the existing cooperative
	// way, through the permit acquisition immediately below.
	if s.resources != nil {
		s.resources.suspendAdmission()
		defer s.resources.resumeAdmission()
	}
	// Exclusive restore permit: drains active managed mutations and writable
	// background-process lifetime leases, then blocks new ones, so the swap sees no
	// cooperative writer. Read-only lifetime leases remain compatible with the
	// snapshot. It is the control-plane, idle-time exclusion the design mandates;
	// the caller invokes it while idle.
	permit, err := s.wsCoordinator.Acquire(ctx, tool.WorkspaceOperationCheckpoint, "")
	if err != nil {
		return &WorkspaceRestoreError{Kind: WorkspaceRestorePermitFailed, Cause: err}
	}
	defer permit.Release()
	// Fail closed if the exclusive root lease was lost while we waited for the permit.
	if err := s.wsCoordinator.Healthy(); err != nil {
		return &WorkspaceRestoreError{Kind: WorkspaceRestoreLeaseUnhealthy, Cause: err}
	}

	switch s.wsMode {
	case PlacementSession:
		if err := restoreSessionRoot(ctx, s.ws, s.wsRoot, ref); err != nil {
			return &WorkspaceRestoreError{Kind: WorkspaceRestoreSwapFailed, Cause: err}
		}
	case PlacementExclusive, PlacementShared:
		// The scratch dirs are session-unique (shared roots take no lease and each session
		// has its own coordinator, so two sessions rewinding the same shared root must not
		// collide on sibling staging/rollback dirs).
		if err := restoreFixedRoot(ctx, s.ws, s.wsRoot, s.sessionID.String(), ref); err != nil {
			// A rollback-undo failure left the live tree partially applied WITHOUT advancing
			// the durable pointer — a silent divergence. Fault the session so the
			// inconsistency is not invisible, rather than returning a plain error.
			var re *WorkspaceRestoreError
			if errors.As(err, &re) && re.Kind == WorkspaceRestoreRollbackFailed {
				s.faultWorkspaceInconsistent(err)
				return err
			}
			return &WorkspaceRestoreError{Kind: WorkspaceRestoreSwapFailed, Cause: err}
		}
	default:
		return &WorkspaceNotConfiguredError{}
	}

	// The live tree now matches ref. Append WorkspaceRestored ONLY after the filesystem
	// commit succeeded; on append failure the session faults (the hub's durable tap raises
	// it) — the tree changed without advancing the durable pointer.
	stamped, err := s.factory.Stamp(event.Header{Coordinates: identity.Coordinates{SessionID: s.sessionID}})
	if err != nil {
		return &WorkspaceRestoreError{Kind: WorkspaceRestoreAppendFailed, Cause: err}
	}
	if err := s.PublishEvent(ctx, event.WorkspaceRestored{Header: stamped, Ref: string(ref)}); err != nil {
		return &WorkspaceRestoreError{Kind: WorkspaceRestoreAppendFailed, Cause: err}
	}
	return nil
}

// restoreAdmissible rejects a rewind on a closing or faulted session (fail closed). It is
// the control-plane admission gate; the exclusive permit provides the concurrency
// exclusion once admitted.
func (s *Session) restoreAdmissible() error {
	s.loopsMu.RLock()
	defer s.loopsMu.RUnlock()
	if s.faulted {
		return &WorkspaceRestoreError{Kind: WorkspaceRestoreFaulted, Cause: s.faultErr}
	}
	if s.closing {
		return &WorkspaceRestoreError{Kind: WorkspaceRestoreClosing}
	}
	return nil
}

// restoreSessionRoot performs the verified whole-root swap for a per-session root. root
// must be the injective, non-symlinked baseDir/<sessionID> path — a symlinked root is
// refused (never follow a symlink to swap arbitrary paths).
func restoreSessionRoot(ctx context.Context, ws *workspacestore.Store, root string, ref workspacestore.Ref) error {
	if err := refuseSymlink(root); err != nil {
		return err
	}
	staging := sessionStagingPath(root)
	backup := sessionBackupPath(root)
	if err := os.RemoveAll(staging); err != nil {
		return err
	}
	// Materialize verifies the ref's digest into the empty staging dir.
	if err := ws.Materialize(ctx, ref, staging); err != nil {
		return err
	}
	if err := os.RemoveAll(backup); err != nil {
		_ = os.RemoveAll(staging)
		return err
	}
	rootExists := pathExists(root)
	if rootExists {
		if err := os.Rename(root, backup); err != nil {
			_ = os.RemoveAll(staging)
			return err
		}
	}
	if err := os.Rename(staging, root); err != nil {
		// Second rename failed: restore the backup so the live root is never left absent.
		if rootExists {
			_ = os.Rename(backup, root)
		}
		_ = os.RemoveAll(staging)
		return err
	}
	_ = os.RemoveAll(backup)
	return nil
}

// restoreFixedRoot performs the manifest reconcile for a fixed exclusive/shared root. It
// never renames or recursively wipes the configured root; it materializes the ref into a
// session-unique sibling staging dir, then replaces/deletes files deterministically with
// rollback copies. token makes the sibling scratch dirs session-unique so concurrent
// shared-root rewinds do not collide.
//
// LIMITATION: the manifest is built from the ref's REGULAR files only. The reconcile
// replaces changed files and deletes files absent from the ref, but does not prune
// now-empty directories, directories absent from the ref, or non-regular entries. The
// restored fixed tree is therefore file-equivalent to the ref but not necessarily
// byte-identical in its directory structure — a documented limitation, not a bug.
func restoreFixedRoot(ctx context.Context, ws *workspacestore.Store, root, token string, ref workspacestore.Ref) error {
	if err := refuseSymlink(root); err != nil {
		return err
	}
	staging := siblingScratch(root, "restore-stage", token)
	if err := os.RemoveAll(staging); err != nil {
		return err
	}
	if err := ws.Materialize(ctx, ref, staging); err != nil {
		return err
	}
	defer os.RemoveAll(staging)

	manifest, err := relRegularFiles(staging)
	if err != nil {
		return err
	}
	current, err := relRegularFiles(root)
	if err != nil {
		return err
	}
	inManifest := make(map[string]struct{}, len(manifest))
	for _, rel := range manifest {
		inManifest[rel] = struct{}{}
	}

	rollback := siblingScratch(root, "restore-rollback", token)
	if err := os.RemoveAll(rollback); err != nil {
		return err
	}
	if err := os.MkdirAll(rollback, 0o700); err != nil {
		return err
	}
	defer os.RemoveAll(rollback)

	rec := &reconcile{root: root, staging: staging, rollback: rollback}
	// Replacements, then deletions, both in sorted order for deterministic commit.
	for _, rel := range manifest {
		if err := rec.replace(rel); err != nil {
			return rollbackOrFault(rec, err)
		}
	}
	deletions := make([]string, 0, len(current))
	for _, rel := range current {
		if _, keep := inManifest[rel]; !keep {
			deletions = append(deletions, rel)
		}
	}
	sort.Strings(deletions)
	for _, rel := range deletions {
		if err := rec.delete(rel); err != nil {
			return rollbackOrFault(rec, err)
		}
	}
	return nil
}

// rollbackOrFault rolls back the applied mutations after a commit failure. If the rollback
// ITSELF fails, the live tree is partially applied and the durable pointer is unchanged —
// a silent inconsistency — so it returns a typed WorkspaceRestoreRollbackFailed error the
// caller escalates to a session fault. Otherwise it returns the original commit error.
func rollbackOrFault(rec *reconcile, cause error) error {
	if rbErr := rec.rollbackAll(); rbErr != nil {
		return &WorkspaceRestoreError{Kind: WorkspaceRestoreRollbackFailed, Cause: rbErr}
	}
	return cause
}

// reconcile tracks the rollback copies made during a fixed-root commit so a mid-commit
// failure can be undone in reverse order.
type reconcile struct {
	root     string
	staging  string
	rollback string
	undo     []func() error
}

// replace copies staging/rel over root/rel, keeping a rollback copy of any existing file
// (or recording a delete-on-rollback when the destination is new).
//
// SECURITY: withinRoot is purely LEXICAL, so before writing it also rejects any SYMLINK in
// the live-root path components (rejectSymlinkComponents) — otherwise a symlinked directory
// or file already present in the live root would let os.MkdirAll/OpenFile write THROUGH the
// symlink to a target outside the workspace. Reachable on shared roots (external writers)
// and on exclusive roots if the agent's own Bash created a symlink.
func (r *reconcile) replace(rel string) error {
	dst := filepath.Join(r.root, rel)
	if !withinRoot(r.root, dst) {
		return &WorkspaceRestoreError{Kind: WorkspaceRestoreEscape}
	}
	if err := rejectSymlinkComponents(r.root, dst); err != nil {
		return err
	}
	existed := pathExists(dst)
	if existed {
		saved := filepath.Join(r.rollback, rel)
		if err := os.MkdirAll(filepath.Dir(saved), 0o700); err != nil {
			return err
		}
		if err := copyFileContents(dst, saved); err != nil {
			return err
		}
		r.undo = append(r.undo, func() error { return copyFileContents(saved, dst) })
	} else {
		r.undo = append(r.undo, func() error { return removeIfExists(dst) })
	}
	// Restored workspace parents preserve the pre-existing 0755 compatibility contract;
	// rollback scratch dirs use 0700 and restored files retain their source permissions.
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil { // #nosec G301 -- workspace parent compatibility; sensitive descendants are narrower
		return err
	}
	return copyFileContents(filepath.Join(r.staging, rel), dst)
}

// delete removes root/rel, keeping a rollback copy so it can be restored on failure. The
// same symlink-component guard applies so a rollback restore never writes through a symlink.
func (r *reconcile) delete(rel string) error {
	dst := filepath.Join(r.root, rel)
	if !withinRoot(r.root, dst) {
		return &WorkspaceRestoreError{Kind: WorkspaceRestoreEscape}
	}
	if err := rejectSymlinkComponents(r.root, dst); err != nil {
		return err
	}
	saved := filepath.Join(r.rollback, rel)
	if err := os.MkdirAll(filepath.Dir(saved), 0o700); err != nil {
		return err
	}
	if err := copyFileContents(dst, saved); err != nil {
		return err
	}
	r.undo = append(r.undo, func() error { return copyFileContents(saved, dst) })
	return os.Remove(dst)
}

// rollbackAll undoes applied replacements/deletions in reverse order, returning the FIRST
// undo failure (while still attempting the rest). A non-nil result means the rollback could
// not fully restore the tree — the caller faults the session.
func (r *reconcile) rollbackAll() error {
	var firstErr error
	for i := len(r.undo) - 1; i >= 0; i-- {
		if err := r.undo[i](); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// rejectSymlinkComponents rejects a write whose live-root path (root exclusive → dst
// inclusive) traverses OR ends at a symlink. It Lstat's each existing component; the first
// symlink is refused with a typed error, and the first non-existent component ends the walk
// (everything below it will be created as real dirs/files, so no symlink can be traversed).
func rejectSymlinkComponents(root, dst string) error {
	rel, err := filepath.Rel(root, dst)
	if err != nil {
		return &WorkspaceRestoreError{Kind: WorkspaceRestoreEscape, Cause: err}
	}
	current := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return &WorkspaceRestoreError{Kind: WorkspaceRestoreSymlinkComponent, Cause: err}
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return &WorkspaceRestoreError{Kind: WorkspaceRestoreSymlinkComponent}
		}
	}
	return nil
}

// refuseSymlink returns a typed error when path exists and is a symlink — the design's
// "refusal to remove arbitrary/symlink paths".
func refuseSymlink(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return &WorkspaceRestoreError{Kind: WorkspaceRestoreSymlinkRoot}
	}
	return nil
}

// siblingScratch derives a sibling scratch path of root for staging/rollback. token makes
// the name session-unique so concurrent restores of the same (unleased) shared root — each
// with its own coordinator — never share, and thus never RemoveAll, each other's scratch
// dirs. A blank token yields the bare deterministic name (used only by callers whose roots
// are already disjoint).
func siblingScratch(root, suffix, token string) string {
	name := "." + filepath.Base(root) + "." + suffix
	if token != "" {
		name += "." + token
	}
	return filepath.Join(filepath.Dir(root), name)
}

// withinRoot reports whether path is equal to or beneath root after cleaning — a
// containment guard for reconcile writes.
func withinRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	if rel == ".." || filepath.IsAbs(rel) {
		return false
	}
	if len(rel) >= 3 && rel[:3] == ".."+string(filepath.Separator) {
		return false
	}
	return true
}

// relRegularFiles returns the root-relative paths of every regular file beneath root,
// sorted. A missing root yields an empty set (a fresh shared root that no writer created).
func relRegularFiles(root string) ([]string, error) {
	var out []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) && path == root {
				return filepath.SkipDir
			}
			return err
		}
		if info.IsDir() || !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// copyFileContents copies src to dst, creating dst (truncating if present). It preserves
// the source's permission bits. Both paths are trusted (contained-and-validated by the
// caller).
func copyFileContents(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	in, err := os.Open(src) // #nosec G304 — src is a caller-validated contained path
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm()) // #nosec G304 — dst is contained
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// WorkspaceRestoreErrorKind classifies a RestoreWorkspace failure.
type WorkspaceRestoreErrorKind string

const (
	WorkspaceRestoreContextDone      WorkspaceRestoreErrorKind = "context_done"
	WorkspaceRestoreFaulted          WorkspaceRestoreErrorKind = "faulted"
	WorkspaceRestoreClosing          WorkspaceRestoreErrorKind = "closing"
	WorkspaceRestorePermitFailed     WorkspaceRestoreErrorKind = "permit_failed"
	WorkspaceRestoreLeaseUnhealthy   WorkspaceRestoreErrorKind = "lease_unhealthy"
	WorkspaceRestoreSymlinkRoot      WorkspaceRestoreErrorKind = "symlink_root"
	WorkspaceRestoreSymlinkComponent WorkspaceRestoreErrorKind = "symlink_component"
	WorkspaceRestoreEscape           WorkspaceRestoreErrorKind = "path_escape"
	WorkspaceRestoreSwapFailed       WorkspaceRestoreErrorKind = "swap_failed"
	WorkspaceRestoreRollbackFailed   WorkspaceRestoreErrorKind = "rollback_failed"
	WorkspaceRestoreAppendFailed     WorkspaceRestoreErrorKind = "append_failed"
)

// WorkspaceRestoreError is the typed failure of a workspace rewind. Kind classifies the
// stage; Cause chains the underlying error (a *workspacestore error, a filesystem error,
// or the coordinator's typed acquisition/health error).
type WorkspaceRestoreError struct {
	Kind  WorkspaceRestoreErrorKind
	Cause error
}

func (e *WorkspaceRestoreError) Error() string {
	msg := "sessionruntime: workspace restore failed (" + string(e.Kind) + ")"
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}
	return msg
}

func (e *WorkspaceRestoreError) Unwrap() error { return e.Cause }
