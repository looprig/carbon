package loopruntime

import (
	"context"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
)

// stampLoopHeader returns ev with its producer-identity Header completed from the
// loop's identity for the session fan-in. It fills ONLY zero header fields (so an
// event that already carries finer identity — TurnStarted/StepDone/TurnFoldedInto/
// InputCancelled stamp SessionID/LoopID/TurnID/StepID and Cause at their producers —
// is preserved exactly). Header-less events (TokenDelta, the turn terminals, and gate/
// tool lifecycle events) get SessionID + LoopID + TurnID from the loop and active turn
// here. Gate/tool events carry their tool id on their own body (e.ToolExecutionID), not the
// header, so they need no extra header field.
//
// This is the LOOP stamping its own producer identity (the actor is the loop's
// authoritative producer), NOT the hub inferring it — the fan-in never repairs
// identity. loopID is loopState.id; turnID is the active turn id (zero between turns).
func stampLoopHeader(ev event.Event, sessionID, loopID, turnID uuid.UUID) event.Event {
	switch e := ev.(type) {
	case event.TokenDelta:
		e.Header = fillTurnScoped(e.Header, sessionID, loopID, turnID)
		return e
	case event.TurnDone:
		e.Header = fillTurnScoped(e.Header, sessionID, loopID, turnID)
		return e
	case event.TurnFailed:
		e.Header = fillTurnScoped(e.Header, sessionID, loopID, turnID)
		return e
	case event.TurnInterrupted:
		e.Header = fillTurnScoped(e.Header, sessionID, loopID, turnID)
		return e
	case event.PermissionRequested:
		e.Header = fillTurnScoped(e.Header, sessionID, loopID, turnID)
		return e
	case event.PermissionDecided:
		e.Header = fillTurnScoped(e.Header, sessionID, loopID, turnID)
		return e
	case event.UserInputRequested:
		e.Header = fillTurnScoped(e.Header, sessionID, loopID, turnID)
		return e
	case event.ToolCallStarted:
		e.Header = fillTurnScoped(e.Header, sessionID, loopID, turnID)
		return e
	case event.ToolCallCompleted:
		e.Header = fillTurnScoped(e.Header, sessionID, loopID, turnID)
		return e
	case event.TurnStarted:
		e.Header = fillLoopScoped(e.Header, sessionID, loopID)
		return e
	case event.DelegateRequestAccepted:
		e.Header = fillLoopScoped(e.Header, sessionID, loopID)
		return e
	case event.StepDone:
		e.Header = fillLoopScoped(e.Header, sessionID, loopID)
		return e
	case event.TurnFoldedInto:
		e.Header = fillLoopScoped(e.Header, sessionID, loopID)
		return e
	case event.InputCancelled:
		e.Header = fillLoopScoped(e.Header, sessionID, loopID)
		return e
	case event.LoopModeChanged:
		// A mode change is loop-scoped, not turn-scoped: fill only SessionID/LoopID (the
		// active turn id, if any, is deliberately NOT stamped — the change applies to the
		// loop, taking effect at the next turn boundary).
		e.Header = fillLoopScoped(e.Header, sessionID, loopID)
		return e
	case event.LoopInferenceChanged:
		e.Header = fillLoopScoped(e.Header, sessionID, loopID)
		return e
	case event.LoopExternalToolsetChanged:
		// An external toolset replacement is loop-scoped for the same reason a mode change
		// is: it applies to the loop and takes effect at the next turn boundary, so the
		// active turn id is deliberately NOT stamped.
		e.Header = fillLoopScoped(e.Header, sessionID, loopID)
		return e
	case event.ContextMeasured:
		e.Header = fillLoopScoped(e.Header, sessionID, loopID)
		return e
	case event.ContextPressure:
		e.Header = fillLoopScoped(e.Header, sessionID, loopID)
		return e
	case event.CompactionStarted:
		e.Header = fillLoopScoped(e.Header, sessionID, loopID)
		return e
	case event.CompactionCommitted:
		e.Header = fillLoopScoped(e.Header, sessionID, loopID)
		return e
	case event.CompactionRejected:
		e.Header = fillLoopScoped(e.Header, sessionID, loopID)
		return e
	case event.CompactWaiterResolved:
		e.Header = fillLoopScoped(e.Header, sessionID, loopID)
		return e
	case event.CompactWaiterRejected:
		e.Header = fillLoopScoped(e.Header, sessionID, loopID)
		return e
	case event.InputQueued:
		// Loop-scoped reply event: no turn exists yet (the input is only queued), so
		// fill SessionID/LoopID and PRESERVE the producer-set Cause.CommandID == InputID.
		e.Header = fillLoopScoped(e.Header, sessionID, loopID)
		return e
	case event.TurnRejected:
		// Loop-scoped reply event: nothing started, so there is no turn — fill only
		// SessionID/LoopID and PRESERVE Cause.CommandID == InputID (and Cause.LoopID for
		// a rejected SubagentResult, were that ever possible).
		e.Header = fillLoopScoped(e.Header, sessionID, loopID)
		return e
	default:
		// Session-scoped events (SessionStarted/Active/Idle/Stopped) carry their own
		// SessionID; any future un-enumerated type cannot be header-repaired (the sealed
		// event interface has no generic Header setter), so return it unchanged.
		return ev
	}
}

// withLoopHeader returns ev with its embedded Header REPLACED by h. It is the
// write-back counterpart to stampLoopHeader: the publish chokepoint reads an
// event's Header (already coordinate-stamped), mints its persistence identity
// (EventID + CreatedAt) via the Factory, then writes the completed Header back
// through here. It enumerates the ENDURING loop-event types plus the deliberately
// factory-stamped low-volume CompactionStarted progress event. Other Ephemeral and
// session-scoped events never reach this path, so the default returns ev unchanged.
func withLoopHeader(ev event.Event, h event.Header) event.Event {
	switch e := ev.(type) {
	case event.TurnStarted:
		e.Header = h
		return e
	case event.DelegateRequestAccepted:
		e.Header = h
		return e
	case event.StepDone:
		e.Header = h
		return e
	case event.TurnFoldedInto:
		e.Header = h
		return e
	case event.InputCancelled:
		e.Header = h
		return e
	case event.LoopModeChanged:
		e.Header = h
		return e
	case event.LoopInferenceChanged:
		e.Header = h
		return e
	case event.LoopExternalToolsetChanged:
		e.Header = h
		return e
	case event.ContextMeasured:
		e.Header = h
		return e
	case event.TurnRejected:
		e.Header = h
		return e
	case event.LoopIdle:
		e.Header = h
		return e
	case event.TurnDone:
		e.Header = h
		return e
	case event.TurnFailed:
		e.Header = h
		return e
	case event.TurnInterrupted:
		e.Header = h
		return e
	case event.PermissionRequested:
		e.Header = h
		return e
	case event.PermissionDecided:
		e.Header = h
		return e
	case event.UserInputRequested:
		e.Header = h
		return e
	case event.CompactionStarted:
		e.Header = h
		return e
	case event.CompactionCommitted:
		e.Header = h
		return e
	case event.CompactionRejected:
		e.Header = h
		return e
	case event.CompactWaiterResolved:
		e.Header = h
		return e
	case event.CompactWaiterRejected:
		e.Header = h
		return e
	default:
		return ev
	}
}

func stampLoopEvent(ev event.Event, factory *event.Factory, sessionID, loopID, turnID uuid.UUID) (event.Event, error) {
	stamped := stampLoopHeader(ev, sessionID, loopID, turnID)
	if stamped.Class() != event.Enduring {
		if _, ok := stamped.(event.CompactionStarted); !ok {
			return stamped, nil
		}
	}
	var h event.Header
	var err error
	switch value := stamped.(type) {
	case event.CompactWaiterResolved:
		h, err = factory.StampCompactWaiterResolved(value)
	case event.CompactWaiterRejected:
		h, err = factory.StampCompactWaiterRejected(value)
	default:
		h, err = factory.Stamp(stamped.EventHeader())
	}
	if err != nil {
		return nil, err
	}
	return withLoopHeader(stamped, h), nil
}

// fillLoopScoped ensures SessionID + LoopID are present without disturbing the
// already-stamped TurnID/StepID/Cause a producer set.
func fillLoopScoped(h event.Header, sessionID, loopID uuid.UUID) event.Header {
	if h.SessionID.IsZero() {
		h.SessionID = sessionID
	}
	if h.LoopID.IsZero() {
		h.LoopID = loopID
	}
	return h
}

// stampStepID returns ev with Coordinates.StepID set to stepID for the five
// tool/gate events ONLY (PermissionRequested/PermissionDecided/UserInputRequested/
// ToolCallStarted/ToolCallCompleted). Those events are emitted by the runner with a header that
// stampLoopHeader later completes from the loop/turn identity; stampLoopHeader's
// fillTurnScoped fills only ZERO fields, so it never repairs StepID. Stamping it
// here at emit time — where the active step's id is known — is what lets the
// "ToolExecutionID requires StepID" invariant hold for these events.
//
// It stamps ONLY the five tool/gate events: any other event (TokenDelta, the turn
// terminals, the submit-resolution events, …) is returned unchanged, so an event
// that must keep StepID zero is never touched. StepID is set unconditionally on the
// five (the runner emits them with a zero header), which is correct: the step is the
// authoritative producer of its own tool/gate events.
func stampStepID(ev event.Event, stepID uuid.UUID) event.Event {
	switch e := ev.(type) {
	case event.PermissionRequested:
		e.StepID = stepID
		return e
	case event.PermissionDecided:
		e.StepID = stepID
		return e
	case event.UserInputRequested:
		e.StepID = stepID
		return e
	case event.ToolCallStarted:
		e.StepID = stepID
		return e
	case event.ToolCallCompleted:
		e.StepID = stepID
		return e
	default:
		return ev
	}
}

// stepStampingEmit wraps an emit sink so every event it carries is first passed
// through stampStepID(_, stepID): the five tool/gate events get this step's StepID,
// every other event passes through untouched. runTurn builds it per step (around
// RunBatch) so the runner — which has no step identity — emits StepID-stamped
// tool/gate events without ever depending on the step.
func stepStampingEmit(emit func(event.Event), stepID uuid.UUID) func(event.Event) {
	return func(ev event.Event) { emit(stampStepID(ev, stepID)) }
}

func stepStampingContextEmit(emit eventEmitter, stepID uuid.UUID) eventEmitter {
	if emit == nil {
		return nil
	}
	return func(ctx context.Context, ev event.Event) {
		emit(ctx, stampStepID(ev, stepID))
	}
}

// fillTurnScoped fills SessionID + LoopID + TurnID for a header-less turn-scoped
// event (TokenDelta, terminals). Only zero fields are filled.
func fillTurnScoped(h event.Header, sessionID, loopID, turnID uuid.UUID) event.Header {
	h = fillLoopScoped(h, sessionID, loopID)
	if h.TurnID.IsZero() {
		h.TurnID = turnID
	}
	return h
}
