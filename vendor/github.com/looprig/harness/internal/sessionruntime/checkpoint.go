package sessionruntime

import (
	"context"

	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/harness/pkg/workspacestore"
)

// CheckpointWorkspace durably snapshots the session's configured workspace root and
// records a WorkspaceCheckpointed enduring event pointing at the snapshot — the resume
// token the restore path materializes from. It returns the snapshot Ref (also carried on
// the event) so the caller can correlate the checkpoint.
//
// Snapshot-before-append: the archive bytes are durable (Snapshot's Blobs.Put has
// returned) BEFORE the event is appended, so a crash between the two leaks an
// unreferenced blob (GC's job) — never a dangling ref. That ordering is guaranteed by
// call order here (Snapshot returns before PublishEvent runs), not by any stronger
// signal.
//
// The WorkspaceCheckpointed is Enduring, so it flows through the hub's REQUIRED durable
// tap: the event is appended synchronously BEFORE fan-out, and a durable-append failure
// surfaces as a SESSION FAULT (the hub latches it via ReportFault) exactly like every
// other enduring event — PublishEvent itself returns nil on that path. This method adds
// no stronger durability signal; on the happy path the event IS durably appended by the
// time it returns.
//
// WHEN to call this — a quiescence point (a turn done, a user question, about to suspend)
// — is the composition root's decision, consistent with looprig-as-SDK and the
// foreign-loop quiescence model; the session only exposes the capability. Without
// WithWorkspaceCheckpointing the capability is unconfigured and this fails closed with a typed
// *WorkspaceNotConfiguredError, having touched nothing. ctx bounds the snapshot I/O and
// the durable append.
func (s *Session) CheckpointWorkspace(ctx context.Context) (workspacestore.Ref, error) {
	if s.ws == nil {
		return "", &WorkspaceNotConfiguredError{}
	}
	if s.checkpoints != nil {
		return s.checkpoints.manual(ctx)
	}
	// When a placement coordinator is wired, hold the exclusive checkpoint permit around
	// the snapshot so no managed mutation overlaps the walk — that is what makes the
	// recorded ref honestly quiescent for exclusive/per-session roots. The bare
	// WithWorkspaceCheckpointing path (no coordinator) skips this.
	if s.wsCoordinator != nil {
		permit, err := s.wsCoordinator.Acquire(ctx, tool.WorkspaceOperationCheckpoint, "")
		if err != nil {
			return "", err
		}
		defer permit.Release()
		if err := s.wsCoordinator.Healthy(); err != nil {
			return "", err
		}
	}
	// Snapshot first: on success the archive bytes are durable in Blobs before we append
	// the event that points at them. Any failure is already a typed *workspacestore
	// error (e.g. *SnapshotError), returned unwrapped so the caller can errors.As it.
	ref, err := s.ws.Snapshot(ctx, s.wsRoot)
	if err != nil {
		return "", err
	}
	// Stamp the event's EventID + CreatedAt through the session Factory (same seam as
	// SessionStarted/LoopStarted). A crypto/rand failure fails the checkpoint cleanly
	// before publishing a zero-EventID event — mirrors newLoop's stamp-failure mapping.
	stamped, err := s.factory.Stamp(event.Header{Coordinates: identity.Coordinates{SessionID: s.sessionID}})
	if err != nil {
		return "", &SessionError{Kind: SessionIDGenerationFailed, Cause: err}
	}
	// A manual checkpoint carries Trigger=Manual (zero Cause). Consistency reflects the
	// placement: shared roots admit external writers, so they are honestly fuzzy; exclusive
	// and per-session roots (and the bare single-writer store) are quiescent.
	consistency := event.SnapshotQuiescent
	if s.wsMode == PlacementShared {
		consistency = event.SnapshotFuzzy
	}
	// Publish on the passed ctx (this method has a real caller ctx, unlike newLoop which
	// publishes on the session lifetime). The Enduring event takes the hub's durable-tap
	// branch: appended before fan-out, faulting the session on append failure.
	if err := s.PublishEvent(ctx, event.WorkspaceCheckpointed{
		Header:      stamped,
		Ref:         string(ref),
		Consistency: consistency,
		Trigger:     event.SnapshotTriggerManual,
	}); err != nil {
		return "", &SessionError{Kind: SessionContextDone, Cause: err}
	}
	return ref, nil
}

// recordSeedCheckpoint journals a WorkspaceCheckpointed for the materialized seed ref as
// the session's FIRST workspace checkpoint (design §"Seeding"). It is Enduring, so it
// flows through the hub's checked durable tap: on append failure the construction
// transaction aborts before any LoopStarted. Trigger=Seed carries a zero Cause;
// Consistency is Quiescent (a seed materializes into an empty, unmutated root).
func (s *Session) recordSeedCheckpoint(ctx context.Context, ref workspacestore.Ref) error {
	stamped, err := s.factory.Stamp(event.Header{Coordinates: identity.Coordinates{SessionID: s.sessionID}})
	if err != nil {
		return &SessionError{Kind: SessionIDGenerationFailed, Cause: err}
	}
	if err := s.PublishEventChecked(ctx, event.WorkspaceCheckpointed{
		Header:      stamped,
		Ref:         string(ref),
		Consistency: event.SnapshotQuiescent,
		Trigger:     event.SnapshotTriggerSeed,
	}); err != nil {
		return &SessionError{Kind: SessionContextDone, Cause: err}
	}
	return nil
}

func withInitialWorkspaceCheckpoint(ref workspacestore.Ref) Option {
	return func(s *Session) { s.initialWorkspaceCheckpoint = ref }
}
