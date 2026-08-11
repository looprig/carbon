package rig

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"

	"github.com/looprig/harness/internal/sessionruntime"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/harness/pkg/workspacestore"
	"github.com/looprig/storage"
)

// workspace.go implements the declarative workspace PLACEMENT options (design §"Optional
// workspace lifecycle" / §"Placement details"). Exactly one placement may be configured.
// Each option records a pending placement on the definition state; Define validates that
// at most one was supplied, canonicalizes the root/base, derives the exclusive root lease
// name, checks the persistence-overlap invariant, and builds the resolved
// sessionruntime.WorkspacePlacement forwarded to the lifecycle.

// leaseNamePrefix namespaces the exclusive root lease so lexical/symlink aliases of the
// SAME canonical root contend on the SAME backend name.
const leaseNamePrefix = "workspace-roots/"

// pendingPlacement is the raw, unvalidated placement a placement option records. Define
// canonicalizes and validates it into a sessionruntime.WorkspacePlacement.
type pendingPlacement struct {
	mode    sessionruntime.WorkspacePlacementMode
	store   *workspacestore.Store
	root    string // exclusive/shared fixed root (raw)
	baseDir string // per-session base (raw)
	leaser  storage.Leaser
}

// WithExclusiveWorkspace declares one canonical fixed root fenced by a single exclusive
// root lease from leaser. The session lease is acquired first, then the root lease named
// workspace-roots/<sha256(canonical-root)>, so lexical/symlink aliases of the root contend.
func WithExclusiveWorkspace(store *workspacestore.Store, root string, leaser storage.Leaser) Option {
	return func(state *definitionState) error {
		state.placements = append(state.placements, pendingPlacement{
			mode:   sessionruntime.PlacementExclusive,
			store:  store,
			root:   root,
			leaser: leaser,
		})
		return nil
	}
}

// WithSessionWorkspaces declares per-session roots derived as baseDir/<sessionID>, isolated
// by construction (no root lease).
func WithSessionWorkspaces(store *workspacestore.Store, baseDir string) Option {
	return func(state *definitionState) error {
		state.placements = append(state.placements, pendingPlacement{
			mode:    sessionruntime.PlacementSession,
			store:   store,
			baseDir: baseDir,
		})
		return nil
	}
}

// WithSharedWorkspace declares one canonical fixed root shared with concurrent harness
// sessions, humans, and external tools — deliberately NO root lease; every checkpoint is
// stamped fuzzy.
func WithSharedWorkspace(store *workspacestore.Store, root string) Option {
	return func(state *definitionState) error {
		state.placements = append(state.placements, pendingPlacement{
			mode:  sessionruntime.PlacementShared,
			store: store,
			root:  root,
		})
		return nil
	}
}

// resolvePlacement validates and canonicalizes the single pending placement into a
// sessionruntime.WorkspacePlacement, returning the placement plus the canonical region
// (root or base) used for the persistence-overlap check and the fingerprint. It enforces
// exactly-one placement and non-nil dependencies.
func resolvePlacement(placements []pendingPlacement) (sessionruntime.WorkspacePlacement, string, error) {
	if len(placements) == 0 {
		return sessionruntime.WorkspacePlacement{}, "", nil
	}
	if len(placements) > 1 {
		return sessionruntime.WorkspacePlacement{}, "", &WorkspacePlacementError{Kind: WorkspaceMultiplePlacements}
	}
	p := placements[0]
	if p.store == nil {
		return sessionruntime.WorkspacePlacement{}, "", &WorkspacePlacementError{Kind: WorkspaceNilStore}
	}
	switch p.mode {
	case sessionruntime.PlacementExclusive:
		return resolveFixedPlacement(p, true)
	case sessionruntime.PlacementShared:
		return resolveFixedPlacement(p, false)
	case sessionruntime.PlacementSession:
		return resolveSessionPlacement(p)
	default:
		return sessionruntime.WorkspacePlacement{}, "", &WorkspacePlacementError{Kind: WorkspaceEmptyRoot}
	}
}

// resolveFixedPlacement canonicalizes an exclusive/shared fixed root. Exclusive also
// requires a non-nil leaser and derives the hashed root lease name.
func resolveFixedPlacement(p pendingPlacement, exclusive bool) (sessionruntime.WorkspacePlacement, string, error) {
	root, err := canonicalPath(p.root)
	if err != nil {
		return sessionruntime.WorkspacePlacement{}, "", err
	}
	placement := sessionruntime.WorkspacePlacement{Mode: p.mode, Store: p.store, Root: root}
	if exclusive {
		if p.leaser == nil {
			return sessionruntime.WorkspacePlacement{}, "", &WorkspacePlacementError{Kind: WorkspaceNilLeaser}
		}
		name := rootLeaseName(root)
		if err := storage.ValidateName(name); err != nil {
			return sessionruntime.WorkspacePlacement{}, "", &WorkspacePlacementError{Kind: WorkspaceLeaseNameInvalid, Cause: err}
		}
		placement.Leaser = p.leaser
		placement.LeaseName = name
	}
	return placement, root, nil
}

// resolveSessionPlacement canonicalizes a per-session base dir.
func resolveSessionPlacement(p pendingPlacement) (sessionruntime.WorkspacePlacement, string, error) {
	base, err := canonicalPath(p.baseDir)
	if err != nil {
		return sessionruntime.WorkspacePlacement{}, "", err
	}
	return sessionruntime.WorkspacePlacement{Mode: p.mode, Store: p.store, BaseDir: base}, base, nil
}

// rootLeaseName derives the exclusive root lease name workspace-roots/<sha256(canonical)>.
func rootLeaseName(canonicalRoot string) string {
	sum := sha256.Sum256([]byte(canonicalRoot))
	return leaseNamePrefix + hex.EncodeToString(sum[:])
}

// canonicalPath canonicalizes a root/base dir with Abs + Clean, and EvalSymlinks when the
// path (or its resolvable prefix) exists, so lexical/symlink aliases converge on one
// canonical form (design §"Placement details"). A path that does not yet exist falls back
// to Abs + Clean of the longest existing ancestor joined with the remainder.
func canonicalPath(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", &WorkspacePlacementError{Kind: WorkspaceEmptyRoot}
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		return "", &WorkspacePlacementError{Kind: WorkspaceCanonicalizeFailed, Name: raw, Cause: err}
	}
	abs = filepath.Clean(abs)
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}
	return canonicalizeNonexistent(abs), nil
}

// canonicalizeNonexistent resolves the longest existing ancestor of abs with EvalSymlinks
// (defeating a symlinked parent) and rejoins the non-existent remainder. abs is already
// Abs+Clean, so walking parents terminates at the filesystem root.
func canonicalizeNonexistent(abs string) string {
	remainder := ""
	current := abs
	for {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			if remainder == "" {
				return resolved
			}
			return filepath.Join(resolved, remainder)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return abs // reached the root without an existing ancestor
		}
		remainder = filepath.Join(filepath.Base(current), remainder)
		current = parent
	}
}

// pathOverlapsRoot reports whether candidate is equal to or beneath root (both canonical).
// An ancestor of root (root beneath candidate) or a disjoint path does NOT overlap.
func pathOverlapsRoot(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

// requiresWorkspaceTool reports whether any tool definition in any loop's modes requires a
// workspace binding. Such a tool with no placement makes the rig invalid at Define (the
// session could never satisfy the RequiresWorkspace binding fail-closed).
func requiresWorkspaceTool(loops []loop.Definition) bool {
	for _, def := range loops {
		if def.ToolRequirements()&tool.RequiresWorkspace != 0 {
			return true
		}
	}
	return false
}

// checkPersistenceOverlap fails closed when any discoverable persistence path (the session
// journal/catalog/leases and the workspace blob store) is equal to or beneath the managed
// workspace region, so a boundary or checkpoint append can never mutate the captured tree.
func checkPersistenceOverlap(store *sessionstore.Store, placement sessionruntime.WorkspacePlacement, region string) error {
	var paths []string
	if store != nil {
		sessionPaths, err := store.PersistencePaths()
		if err != nil {
			return err
		}
		paths = append(paths, sessionPaths...)
	}
	if placement.Store != nil {
		wsPaths, err := placement.Store.PersistencePaths()
		if err != nil {
			return err
		}
		paths = append(paths, wsPaths...)
	}
	for _, raw := range paths {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		canon, err := canonicalPath(raw)
		if err != nil {
			return err
		}
		if pathOverlapsRoot(region, canon) {
			return &PersistenceOverlapError{PersistencePath: canon, Root: region}
		}
	}
	return nil
}

// placementFingerprint folds the placement mode and canonical region into the workspace
// fingerprint field so a placement change (mode or path) is a durable config change.
func placementFingerprint(placement sessionruntime.WorkspacePlacement, region string) string {
	return placementModeName(placement.Mode) + ":" + region
}

func placementModeName(mode sessionruntime.WorkspacePlacementMode) string {
	switch mode {
	case sessionruntime.PlacementExclusive:
		return "exclusive"
	case sessionruntime.PlacementSession:
		return "session"
	case sessionruntime.PlacementShared:
		return "shared"
	default:
		return "none"
	}
}
