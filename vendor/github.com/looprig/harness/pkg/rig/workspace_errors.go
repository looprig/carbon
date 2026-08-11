package rig

import "github.com/looprig/harness/pkg/session"

// WorkspaceRootBusyError and WorkspaceRootLeaseLostError are re-exported at the
// rig composition surface while remaining canonical session lifecycle errors.
type WorkspaceRootBusyError = session.WorkspaceRootBusyError
type WorkspaceRootLeaseLostError = session.WorkspaceRootLeaseLostError
type WorkspaceRecoveryError = session.WorkspaceRecoveryError

// workspace_errors.go defines the typed failures the placement options and their
// Define-time validation raise (design §"Optional workspace lifecycle"). Every failure is
// a concrete struct so callers can errors.As it.

// WorkspacePlacementErrorKind classifies a workspace placement validation failure.
type WorkspacePlacementErrorKind string

const (
	// WorkspaceMultiplePlacements: more than one placement option was supplied. Exactly
	// one of WithExclusiveWorkspace / WithSessionWorkspaces / WithSharedWorkspace may be used.
	WorkspaceMultiplePlacements WorkspacePlacementErrorKind = "multiple_placements"
	// WorkspaceNilStore: a placement was given a nil workspace store.
	WorkspaceNilStore WorkspacePlacementErrorKind = "nil_store"
	// WorkspaceNilLeaser: an exclusive placement was given a nil root leaser.
	WorkspaceNilLeaser WorkspacePlacementErrorKind = "nil_leaser"
	// WorkspaceEmptyRoot: a placement root/base dir was empty or whitespace.
	WorkspaceEmptyRoot WorkspacePlacementErrorKind = "empty_root"
	// WorkspaceCanonicalizeFailed: a root/base dir could not be canonicalized.
	WorkspaceCanonicalizeFailed WorkspacePlacementErrorKind = "canonicalize_failed"
	// WorkspaceLeaseNameInvalid: the derived root lease name violated the storage grammar.
	WorkspaceLeaseNameInvalid WorkspacePlacementErrorKind = "lease_name_invalid"
	// WorkspaceToolWithoutPlacement: a workspace-requiring tool definition with no placement.
	WorkspaceToolWithoutPlacement WorkspacePlacementErrorKind = "workspace_tool_without_placement"
)

// WorkspacePlacementError reports an invalid workspace placement declaration at rig.Define.
type WorkspacePlacementError struct {
	Kind  WorkspacePlacementErrorKind
	Name  string
	Cause error
}

func (e *WorkspacePlacementError) Error() string {
	msg := "rig: invalid workspace placement (" + string(e.Kind) + ")"
	if e.Name != "" {
		msg += ": " + e.Name
	}
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}
	return msg
}

func (e *WorkspacePlacementError) Unwrap() error { return e.Cause }

// PersistenceOverlapError reports that a discoverable persistence path is equal to or
// beneath the managed workspace root, so appending a boundary or checkpoint event would
// mutate the very tree being captured. Persistence must live OUTSIDE the workspace.
// PersistencePath is the offending canonical path; Root is the canonical workspace root
// (or per-session base dir) it overlaps.
type PersistenceOverlapError struct {
	PersistencePath string
	Root            string
}

func (e *PersistenceOverlapError) Error() string {
	return "rig: persistence path " + e.PersistencePath + " overlaps workspace root " + e.Root
}

// SessionOptionErrorKind classifies a NewSession option failure.
type SessionOptionErrorKind string

const (
	// SessionOptionNil: a nil NewSession option was supplied.
	SessionOptionNil SessionOptionErrorKind = "nil_option"
	// SessionOptionDuplicateSeed: WithSeedSnapshot was supplied more than once.
	SessionOptionDuplicateSeed SessionOptionErrorKind = "duplicate_seed"
	// SessionOptionEmptySeed: WithSeedSnapshot was given an empty ref.
	SessionOptionEmptySeed SessionOptionErrorKind = "empty_seed"
)

// SessionOptionError reports an invalid NewSession option.
type SessionOptionError struct {
	Kind SessionOptionErrorKind
}

func (e *SessionOptionError) Error() string {
	return "rig: invalid session option (" + string(e.Kind) + ")"
}
