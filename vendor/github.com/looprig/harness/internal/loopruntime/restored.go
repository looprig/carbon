package loopruntime

import (
	"context"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	model "github.com/looprig/inference/model"
)

// RestoredState is the pre-built committed state a restored loop comes up with: the
// folded message history and the turn count from the durable journal. It is the loop
// half of the Restore constructor's payoff — the session folds a loop's Enduring
// events (foldLoop) into these two values and seeds a fresh actor with them so
// the resumed loop's history is byte-for-byte what it committed before teardown.
//
// Msgs is the committed conversation ONLY — it does NOT carry a SystemMessage. The
// loop never stores the system prompt in loopState.msgs; the prompt rides
// runtimeConfig.System and is sent on every request, so a restored loop "re-seeds" the
// system prompt simply by carrying the same runtimeConfig. TurnIndex is the count of turns
// already started, so the next live turn numbers from TurnIndex+1 (installActiveTurn
// increments it), continuing the loop's numbering without a gap.
type RestoredState struct {
	Msgs      content.AgenticMessages
	TurnIndex event.TurnIndex

	// DerivedPrefix counts the leading messages in Msgs that are a
	// compaction-generated summary rather than genuine human-authored
	// conversation (folded from the same CompactionCommitted event that
	// replaces Msgs entirely — sessionruntime's foldLoop). It seeds
	// loopState.msgsDerivedPrefix (see that field's doc comment for the full
	// rationale): without it, a restored loop's next turn would clone Msgs
	// into cfg.base with no marker at all, and capturePermissionReviewContext
	// would credit a model-generated compaction summary with
	// gate.ReviewContextOriginUser — genuine human authorization.
	DerivedPrefix int

	// Mode is the loop's LAST durably-selected mode (folded from LoopModeChanged, last write
	// wins); HasMode distinguishes "the loop changed mode" (reapply Mode, which may be the
	// base "") from "the loop never changed mode" (come up under the definition's initial
	// mode). NewRestored re-resolves the mode's model/effort/tools/instructions from the
	// fresh bound definition, so only the NAME is carried across restore.
	Mode    loop.ModeName
	HasMode bool

	// Runtime is the latest durable resolved runtime, whether selected by start,
	// mode change, or direct inference change. The live bound model supplies the
	// transport fields while this durable payload restores identity, limits, and effort.
	Runtime    event.ModelRuntime
	HasRuntime bool

	Context    event.ContextMeasurement
	HasContext bool

	Basis    event.ContextBasis
	HasBasis bool

	AutomaticBasis    event.ContextBasis
	HasAutomaticBasis bool

	// PendingProcessNotifications are Task 24C's undelivered process
	// completion notifications reconstructed at restore: the session replays
	// this loop's durable ProcessNotification commands and subtracts any
	// whose CommandID already appears as the cause of one of this loop's
	// durable Enduring events (already consumed before the crash). NewRestored
	// seeds them directly into the actor's live de-dup guard — the SAME
	// representation a live delivery populates — so restore never re-dispatches
	// them through Loop.Commands (there is no live sender at restore time) and
	// never re-appends them (they are already durable).
	PendingProcessNotifications []tool.ProcessCompletionNotification
}

// NewRestored constructs a loop SEEDED with pre-built committed state and starts its
// actor goroutine IDLE — the restore counterpart to New. New spawns an empty loop that
// commits its first message at the first submit; NewRestored seeds loopState.msgs +
// turnIndex from the journal fold so the resumed loop already holds its prior history
// and numbers its next turn correctly. Everything else is identical to New: the same
// config validation/defaulting, the same actor goroutine, the same idle status — the
// ONLY difference is the seeded initial state.
//
// loopCtx, sessionID, loopID, events, and cfg mean exactly what they do in New. loopID
// MUST be the loop's ORIGINAL id (the session passes the root loop's recovered id)
// so identity is stable across restore. seed is the folded committed state; a zero
// RestoredState (empty Msgs, zero TurnIndex) yields a loop indistinguishable from a
// freshly New'd one.
func NewRestored(loopCtx context.Context, sessionID, loopID uuid.UUID, parent loop.Provenance, events eventPublisher, bound loop.BoundDefinition, seed RestoredState) (*Loop, error) {
	return NewRestoredWithCompactor(loopCtx, sessionID, loopID, parent, events, bound, seed, nil, nil)
}

// NewRestoredWithCompactor is the restored counterpart to
// NewInModeWithCompactor. It installs the focused executor while preserving the
// restore-folded mode and inference runtime. reviewContext mirrors
// NewInModeWithCompactor's parameter of the same name — see its doc comment.
func NewRestoredWithCompactor(
	loopCtx context.Context,
	sessionID, loopID uuid.UUID,
	parent loop.Provenance,
	events eventPublisher,
	bound loop.BoundDefinition,
	seed RestoredState,
	compactor Compactor,
	reviewContext *ReviewContext,
) (*Loop, error) {
	return NewRestoredWithRuntime(
		loopCtx,
		sessionID,
		loopID,
		parent,
		events,
		bound,
		seed,
		RuntimeDependencies{Compactor: compactor, ReviewContext: reviewContext},
	)
}

// NewRestoredWithRuntime is the restored counterpart to
// NewInModeWithRuntime.
func NewRestoredWithRuntime(
	loopCtx context.Context,
	sessionID, loopID uuid.UUID,
	parent loop.Provenance,
	events eventPublisher,
	bound loop.BoundDefinition,
	seed RestoredState,
	deps RuntimeDependencies,
) (*Loop, error) {
	// Resolve config at the RESTORED mode (last LoopModeChanged) rather than the definition's
	// initial mode, so a loop that changed mode before teardown resumes under it. When the
	// loop never changed mode, "" resolves to the initial mode (the pre-change behavior).
	modeName := bound.InitialMode()
	if seed.HasMode {
		modeName = seed.Mode
	}
	// Resolve by EXACT name (configForMode, not configFromBound): modeName is always already
	// concrete here — the definition's initial mode when the loop never changed, or the exact
	// folded mode when it did (including "" for a durable SetMode("") to the base mode) — so
	// the ""→initial remap must NOT fire. Remapping would resolve the initial mode's
	// system/model/EFFORT/TOOLS under a "" label, diverging from what the loop crashed under
	// (a security-relevant tool-set mismatch) and from liveViewFor's exact base resolution.
	cfg, err := configForMode(bound, modeName)
	if err != nil {
		return nil, err
	}
	if seed.HasRuntime {
		// Kept in lockstep with sessionruntime/loop_change.go's applyModelRuntime — see the
		// design doc's "Both grafting sites, kept in lockstep". That function grafts the SAME
		// fields onto the lightweight reported Handle view; a divergent edit here would leave
		// the actor's real running model out of sync with what Handle.Model() reports.
		if seed.Runtime.APIFormat != "" {
			cfg.Model.APIFormat = seed.Runtime.APIFormat
		}
		if seed.Runtime.BaseURL != "" {
			cfg.Model.BaseURL = seed.Runtime.BaseURL
		}
		cfg.Model.Provider = seed.Runtime.Key.Provider
		cfg.Model.Name = seed.Runtime.Key.Model
		cfg.Model.Limits = seed.Runtime.Limits
		cfg.Model.Sampling = cfg.Model.Sampling.Clone()
		cfg.Model.Sampling.Effort = seed.Runtime.Effort
	}
	// Hard restore-time validation: a folded runtime whose transport is no longer a
	// member of the bound definition's declared ContextTransport set fails restore
	// unconditionally (see RestoreTransportMismatchError's doc comment). Gated on
	// bound.ContextCounter() != nil so a definition with no context counting configured
	// at all (ContextTransportCapability always reports false, having nothing declared)
	// is unaffected — exactly the modeDefinition-style fixtures the rest of this package
	// restores against today.
	if seed.HasRuntime && bound.ContextCounter() != nil {
		if _, ok := bound.ContextTransportCapability(cfg.Model); !ok {
			return nil, &RestoreTransportMismatchError{
				Provider: cfg.Model.Provider, APIFormat: cfg.Model.APIFormat, BaseURL: cfg.Model.BaseURL,
			}
		}
	}
	if err := installRuntimeDependencies(loopCtx, &cfg, deps); err != nil {
		return nil, err
	}
	cfg.reviewContext = deps.ReviewContext.toInternal()
	return newLoopWithSeed(loopCtx, sessionID, loopID, parent, events, cfg, bound, modeName, &seed)
}

func newRestoredWithConfig(loopCtx context.Context, sessionID, loopID uuid.UUID, events eventPublisher, cfg runtimeConfig, seed RestoredState) (*Loop, error) {
	return newLoopWithSeed(loopCtx, sessionID, loopID, Provenance{}, events, cfg, nil, seed.Mode, &seed)
}

// snapshotRequest is the actor-served committed-state query handshake. The actor is the
// SOLE mutator of loopState.msgs/turnIndex (no locks), so a consistent read must go
// THROUGH the actor: a caller sends a request on cfg.snapshots and the actor replies a
// defensive clone on reply. reply is buffered(1) so the actor never blocks delivering
// it. This is the restore-verification + future-snapshot primitive (Snapshot below).
type snapshotRequest struct {
	reply chan<- loopSnapshot
}

// loopSnapshot is a consistent, defensively-cloned view of the loop's committed state
// returned by Snapshot: the conversation history and the turn count. Both are read by
// the actor and cloned before hand-off so the caller can never race the live state.
type loopSnapshot struct {
	msgs      content.AgenticMessages
	turnIndex event.TurnIndex
}

// Snapshot returns a consistent view of the loop's committed conversation and turn
// count by querying the actor (the sole owner of loopState), so the read never races a
// concurrent commit. It is the restore-verification primitive (the session proves a
// restored loop's history matches the original) and the hook a future dormant-snapshot
// writer reads from. It returns a typed *SnapshotError if the loop has exited (its
// actor is gone) or ctx is done before the actor replies — never a partial or zero
// view.
func (l *Loop) Snapshot(ctx context.Context) (content.AgenticMessages, event.TurnIndex, error) {
	reply := make(chan loopSnapshot, 1)
	select {
	case l.snapshots <- snapshotRequest{reply: reply}:
	case <-l.Done:
		return nil, 0, &SnapshotError{Reason: SnapshotLoopExited}
	case <-ctx.Done():
		return nil, 0, &SnapshotError{Reason: SnapshotContextDone, Cause: ctx.Err()}
	}
	select {
	case snap := <-reply:
		return snap.msgs, snap.turnIndex, nil
	case <-l.Done:
		return nil, 0, &SnapshotError{Reason: SnapshotLoopExited}
	case <-ctx.Done():
		return nil, 0, &SnapshotError{Reason: SnapshotContextDone, Cause: ctx.Err()}
	}
}

// SnapshotErrorReason classifies why a Snapshot could not return a consistent view.
type SnapshotErrorReason string

const (
	// SnapshotLoopExited means the actor goroutine has exited (Loop.Done closed), so
	// there is no live state to read.
	SnapshotLoopExited SnapshotErrorReason = "loop_exited"
	// SnapshotContextDone means the caller's context was cancelled before the actor
	// replied.
	SnapshotContextDone SnapshotErrorReason = "context_done"
)

// SnapshotError is returned by Snapshot when it cannot obtain a consistent view of the
// loop's committed state. Cause chains the underlying ctx error when present.
type SnapshotError struct {
	Reason SnapshotErrorReason
	Cause  error
}

func (e *SnapshotError) Error() string {
	switch e.Reason {
	case SnapshotLoopExited:
		return "loop: snapshot failed: loop exited"
	case SnapshotContextDone:
		if e.Cause != nil {
			return "loop: snapshot failed: context done: " + e.Cause.Error()
		}
		return "loop: snapshot failed: context done"
	default:
		return "loop: snapshot failed"
	}
}

func (e *SnapshotError) Unwrap() error { return e.Cause }

// RestoreTransportMismatchError reports a durably-folded model runtime whose
// transport is no longer a member of the current bound definition's declared
// ContextTransport set. Restore fails unconditionally on this error — there
// is no coherent "resume anyway" answer for a resolved model whose trust
// tier can no longer be determined, so it does not route through
// RestoreDecider/WithAllowConfigMismatch (see the design doc's "New
// restore-time hard validation" section).
type RestoreTransportMismatchError struct {
	Provider  model.ProviderName
	APIFormat model.APIFormat
	BaseURL   string
}

func (e *RestoreTransportMismatchError) Error() string {
	return "loopruntime: restored model transport is not a declared ContextTransport"
}
