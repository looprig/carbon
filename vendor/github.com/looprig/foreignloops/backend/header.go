package backend

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
)

func fillForeignHeader(input event.Event, sessionID, loopID, turnID, stepID uuid.UUID) event.Event {
	switch typed := input.(type) {
	case event.TurnStarted:
		typed.Header.SessionID, typed.Header.LoopID, typed.Header.TurnID = sessionID, loopID, turnID
		return typed
	case event.ForeignSessionBound:
		typed.Header.SessionID, typed.Header.LoopID = sessionID, loopID
		return typed
	case event.DelegateRequestAccepted:
		typed.Header.SessionID, typed.Header.LoopID = sessionID, loopID
		return typed
	case event.InputQueued:
		typed.Header.SessionID, typed.Header.LoopID = sessionID, loopID
		return typed
	case event.InputCancelled:
		typed.Header.SessionID, typed.Header.LoopID, typed.Header.TurnID = sessionID, loopID, turnID
		return typed
	case event.TurnFoldedInto:
		typed.Header.SessionID, typed.Header.LoopID, typed.Header.TurnID = sessionID, loopID, turnID
		return typed
	case event.TurnDone:
		typed.Header.SessionID, typed.Header.LoopID, typed.Header.TurnID = sessionID, loopID, turnID
		return typed
	case event.TurnFailed:
		typed.Header.SessionID, typed.Header.LoopID, typed.Header.TurnID = sessionID, loopID, turnID
		return typed
	case event.TurnInterrupted:
		typed.Header.SessionID, typed.Header.LoopID, typed.Header.TurnID = sessionID, loopID, turnID
		return typed
	case event.StepDone:
		typed.Header.SessionID, typed.Header.LoopID, typed.Header.TurnID, typed.Header.StepID = sessionID, loopID, turnID, stepID
		return typed
	case event.TokenDelta:
		typed.Header.SessionID, typed.Header.LoopID, typed.Header.TurnID, typed.Header.StepID = sessionID, loopID, turnID, stepID
		return typed
	case event.ToolCallStarted:
		typed.Header.SessionID, typed.Header.LoopID, typed.Header.TurnID, typed.Header.StepID = sessionID, loopID, turnID, stepID
		return typed
	case event.ToolCallCompleted:
		typed.Header.SessionID, typed.Header.LoopID, typed.Header.TurnID, typed.Header.StepID = sessionID, loopID, turnID, stepID
		return typed
	default:
		return input
	}
}

func withForeignHeader(input event.Event, header event.Header) event.Event {
	switch typed := input.(type) {
	case event.TurnStarted:
		typed.Header = header
		return typed
	case event.ForeignSessionBound:
		typed.Header = header
		return typed
	case event.DelegateRequestAccepted:
		typed.Header = header
		return typed
	case event.InputCancelled:
		typed.Header = header
		return typed
	case event.TurnFoldedInto:
		typed.Header = header
		return typed
	case event.StepDone:
		typed.Header = header
		return typed
	case event.TurnDone:
		typed.Header = header
		return typed
	case event.TurnFailed:
		typed.Header = header
		return typed
	case event.TurnInterrupted:
		typed.Header = header
		return typed
	default:
		return input
	}
}

func (l *Loop) publisher(ctx context.Context, turnID, stepID uuid.UUID) func(event.Event) {
	return func(input event.Event) {
		if err := l.publishActor(ctx, turnID, stepID, input); err != nil {
			slog.Error("foreignloop: event publish to session fan-in failed", "event", fmt.Sprintf("%T", input), "error", err)
		}
	}
}

// publishActor is the only backend path that stamps and publishes an event.
// Enduring events use the checked publisher so the actor never advances local
// lifecycle state beyond a transition that the session accepted durably.
func (l *Loop) publishActor(ctx context.Context, turnID, stepID uuid.UUID, input event.Event) error {
	input = fillForeignHeader(input, l.sessionID, l.loopID, turnID, stepID)
	// TurnFoldedInto is an authoritative handback transition. Keep the
	// explicit type check alongside the semantic class check so a Harness
	// event-class refactor cannot silently downgrade the fold to an unchecked
	// publication: steering resolution must observe the durable fold before it
	// records injected delivery.
	_, fold := input.(event.TurnFoldedInto)
	if input.Class() == event.Enduring || fold {
		header, err := l.fac.Stamp(input.EventHeader())
		if err != nil {
			return err
		}
		input = withForeignHeader(input, header)
		return l.pub.PublishEventChecked(ctx, input)
	}
	return l.pub.PublishEvent(ctx, input)
}

func (l *Loop) publishAcceptance(ctx context.Context, commandID uuid.UUID) error {
	return l.publishActor(ctx, uuid.UUID{}, uuid.UUID{}, event.DelegateRequestAccepted{Header: event.Header{Cause: identity.Cause{CommandID: commandID}}})
}
