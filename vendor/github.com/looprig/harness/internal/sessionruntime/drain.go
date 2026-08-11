package sessionruntime

import (
	"context"
	"errors"
	"strings"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/tool"
)

const maxDelegateOutputBytes = 256 << 10

// drainCorrelationState is the phase boundary between the opening resolution
// event and the target terminal. A caller may stop observing after the opening
// event has been consumed; retaining this state lets a session-owned cleanup
// drain resume phase two without scanning for a second opening.
type drainCorrelationState struct {
	turnID   uuid.UUID
	loopID   uuid.UUID
	haveTurn bool
	lastStep string
}

// drainFailedError wraps a TurnFailed.Err terminal: the sub-loop's turn ended on
// a non-cancellation provider/LLM error. Cause is the typed cause the loop
// carried; callers errors.As to this type and errors.Is/As through Unwrap to the
// original cause.
type drainFailedError struct {
	Cause error
}

func (e *drainFailedError) Error() string {
	if e.Cause == nil {
		return "drain: turn failed"
	}
	return "drain: turn failed: " + e.Cause.Error()
}
func (e *drainFailedError) Unwrap() error { return e.Cause }

// drainInterruptedError is returned when the sub-loop's turn ended on a
// TurnInterrupted terminal — the caller went away (ctx cancel) and the helper's
// fail-safe interrupt stopped the loop, or a distributed Interrupt reached it.
// There is no partial result.
type drainInterruptedError struct {
	// observer reports that the caller's observation context expired before a
	// correlated target terminal was observed. A target TurnInterrupted uses the
	// zero value and therefore still represents an idle terminal agent.
	observer bool
}

func (e *drainInterruptedError) Error() string { return "drain: turn interrupted" }

func drainObserverExpired(err error) bool {
	var interrupted *drainInterruptedError
	return errors.As(err, &interrupted) && interrupted.observer
}

// drainCancelledError reports that an admitted queued delegate request left the
// child inbox before it ever opened a turn.
type drainCancelledError struct{ Reason event.CancelReason }

func (e *drainCancelledError) Error() string { return "drain: queued input cancelled" }

// A queued submit cancelled before any turn starts surfaces as
// *drainCancelledError, correlated by Cause.CommandID exactly like TurnRejected.
// A submit refused before any turn starts (TurnRejected for the submit's
// Cause.CommandID) surfaces as the package's existing *TurnRejectedError: its
// Reason and event.TurnRejected.Reason are the same type (event.RejectReason),
// so there is no second rejected-error type or duplicated reason→string switch.

// drainLostError is returned when sub.Events() closes before a terminal arrives:
// the hub force-closed the subscription (Cause set to sub.Err()) or the sub-loop
// exited with no terminal (Cause nil). Either way no final text exists.
type drainLostError struct {
	Cause error
}

func (e *drainLostError) Error() string {
	if e.Cause == nil {
		return "drain: subscription closed before terminal event"
	}
	return "drain: subscription lost: " + e.Cause.Error()
}
func (e *drainLostError) Unwrap() error { return e.Cause }

// drainToFinalText drains a (sub-)loop's events from a caller-owned subscription
// to the terminal for the submit identified by commandID, and returns the final
// assistant text (or a typed error per the §5 failure contract).
//
// Correlation is two-phase: phase 1 scans for the opening event.TurnStarted whose
// Header.Cause.CommandID == commandID and captures its TurnID and LoopID; phase 2
// matches StepDone/terminal events by BOTH that TurnID and LoopID (they do not
// carry Cause.CommandID). The LoopID check is fail-secure defense-in-depth: the
// subscription is scoped to one sub-loop so every event should already carry that
// loop's id, and a phase-2 event from a different loop is thereby provably ignored
// rather than merely "can't happen". Events for other commands/turns interleaved
// on the stream are ignored. A TurnRejected for commandID in phase 1 means the
// submit was refused and no turn will ever start, so it short-circuits to the
// package's typed *TurnRejectedError.
//
// This legacy helper takes final text from TurnDone.Message and falls back to the latest
// StepDone assistant text. Managed delegation uses drainDelegateAnswer below, where
// StepDone is progress only and the correlated TurnDone.Message is the exact answer.
//
// ctx is the calling turn's context and interrupt is the optional loop-targeted
// Interrupt bound to the sub-loop. Submits carry no ctx, so cancelling ctx cannot
// reach the sub-loop's turn — only an explicit Interrupt can on the legacy path.
// Native managed delegation passes nil: its accepted foldable request must not be
// retracted or interrupted by caller cancellation. On ctx.Done() the helper calls
// a non-nil interrupt EXACTLY ONCE (fail-safe) and otherwise returns the typed
// interrupted result without emitting a control command.
// This fail-safe relies on the sub-loop honouring the interrupt by producing a
// terminal (or the subscription closing); a pathologically wedged provider that
// ignores its turn ctx after the interrupt is a known, pre-existing liveness corner.
//
// CALLER RESPONSIBILITY: subscribe BEFORE submitting. The hub has no replay, so a
// subscription created after the submit could miss the opening TurnStarted and the
// helper would then block until ctx-cancel or subscription loss. The helper does
// not — and cannot — enforce this ordering; it is the one subtlety the caller owns.
func drainToFinalText(ctx context.Context, sub event.Subscription, commandID uuid.UUID, interrupt func()) (string, error) {
	return drainCorrelated(ctx, sub, commandID, interrupt, true, nil)
}

// drainDelegateAnswer resolves delegation requests from the correlated TurnDone.Message
// only. StepDone is progress, never a substitute terminal answer.
func drainDelegateAnswer(ctx context.Context, sub event.Subscription, commandID uuid.UUID, interrupt func()) (string, error) {
	return drainDelegateAnswerObserved(ctx, sub, commandID, interrupt, nil)
}

// drainDelegateAnswerObserved reports when the correlated request leaves the child queue
// and opens a turn. Managed foreground and background drains use the same lifecycle edge.
func drainDelegateAnswerObserved(ctx context.Context, sub event.Subscription, commandID uuid.UUID, interrupt, onTurnStarted func()) (string, error) {
	var onOpening func(tool.DelegateDeliveryStatus)
	if onTurnStarted != nil {
		onOpening = func(tool.DelegateDeliveryStatus) { onTurnStarted() }
	}
	return drainDelegateAnswerObservedWithDisposition(ctx, sub, commandID, interrupt, onOpening)
}

func drainDelegateAnswerObservedWithDisposition(ctx context.Context, sub event.Subscription, commandID uuid.UUID, interrupt func(), onOpening func(tool.DelegateDeliveryStatus)) (string, error) {
	return drainDelegateAnswerObservedWithState(ctx, sub, commandID, interrupt, onOpening, nil)
}

func drainCorrelated(ctx context.Context, sub event.Subscription, commandID uuid.UUID, interrupt func(), stepFallback bool, onOpening func(tool.DelegateDeliveryStatus)) (string, error) {
	return drainCorrelatedWithState(ctx, sub, commandID, interrupt, stepFallback, onOpening, nil)
}

// drainDelegateAnswerObservedWithState is the resumable managed-delegation
// drain. A non-nil state is owned by the session coordinator and is shared by
// the caller-facing observer and its cleanup-only continuation.
func drainDelegateAnswerObservedWithState(ctx context.Context, sub event.Subscription, commandID uuid.UUID, interrupt func(), onOpening func(tool.DelegateDeliveryStatus), state *drainCorrelationState) (string, error) {
	return drainCorrelatedWithState(ctx, sub, commandID, interrupt, false, onOpening, state)
}

func drainCorrelatedWithState(ctx context.Context, sub event.Subscription, commandID uuid.UUID, interrupt func(), stepFallback bool, onOpening func(tool.DelegateDeliveryStatus), state *drainCorrelationState) (string, error) {
	if state == nil {
		state = &drainCorrelationState{}
	}
	var (
		fired     bool // guards the single fail-safe interrupt() on ctx.Done()
		ctxClosed bool // once true, await terminal/close without re-selecting ctx.Done()
	)

	for {
		// After ctx fired and interrupt() ran, stop selecting on ctx.Done() (it
		// stays selectable once cancelled — re-selecting it would busy-loop). Just
		// await the sub-loop's terminal (or a subscription loss).
		if ctxClosed {
			d, ok := <-sub.Events()
			if !ok {
				return "", &drainLostError{Cause: sub.Err()}
			}
			if text, done, err := handleCorrelatedEvent(d.Event, commandID, state, stepFallback, onOpening); done {
				return text, err
			}
			continue
		}

		select {
		case d, ok := <-sub.Events():
			if !ok {
				return "", &drainLostError{Cause: sub.Err()}
			}
			if text, done, err := handleCorrelatedEvent(d.Event, commandID, state, stepFallback, onOpening); done {
				return text, err
			}
		case <-ctx.Done():
			// Boundary cancel: the submit carried no ctx, so translate it into a
			// single loop-targeted Interrupt and keep draining for the resulting
			// TurnInterrupted terminal.
			if !stepFallback {
				// Managed delegation has a real caller-supplied deadline. Do not turn that
				// bound into an unbounded terminal drain when a provider ignores cancel.
				// The session-owned interrupt path is fire-and-forget; the caller closes
				// its subscription immediately after this typed terminal is returned.
				if interrupt != nil {
					go interrupt()
				}
				return "", &drainInterruptedError{observer: true}
			}
			ctxClosed = true
			if !fired {
				fired = true
				if interrupt != nil {
					interrupt()
				}
			}
		}
	}
}

func handleCorrelatedEvent(
	ev event.Event,
	commandID uuid.UUID,
	state *drainCorrelationState,
	stepFallback bool,
	onOpening func(tool.DelegateDeliveryStatus),
) (text string, done bool, err error) {
	if !state.haveTurn {
		// Phase 1: await the opening resolution event for our submit.
		switch e := ev.(type) {
		case event.TurnStarted:
			if e.Cause.CommandID == commandID {
				state.turnID = e.Coordinates.TurnID
				state.loopID = e.Coordinates.LoopID
				state.haveTurn = true
				if onOpening != nil {
					onOpening(tool.DelegateDeliveryQueued)
				}
			}
		case event.TurnFoldedInto:
			// A native busy-child MessageAgent request is resolved by a fold
			// into the already-running turn rather than a new TurnStarted. The
			// terminal still belongs to this exact (LoopID, TurnID) pair.
			if e.Cause.CommandID == commandID {
				state.turnID = e.Coordinates.TurnID
				state.loopID = e.Coordinates.LoopID
				state.haveTurn = true
				if onOpening != nil {
					onOpening(tool.DelegateDeliveryInjected)
				}
			}
		case event.TurnRejected:
			if e.Cause.CommandID == commandID {
				return "", true, &TurnRejectedError{Reason: e.Reason}
			}
		case event.InputCancelled:
			if e.Cause.CommandID == commandID {
				return "", true, &drainCancelledError{Reason: e.Reason}
			}
		}
		return "", false, nil
	}

	// Phase 2: match StepDone/terminal events by the captured TurnID AND LoopID.
	// LoopID is the fail-secure cross-check (the sub is scoped to one loop, so a
	// phase-2 event from a different loop is provably ignored, not "can't happen").
	switch e := ev.(type) {
	case event.StepDone:
		if e.Coordinates.TurnID == state.turnID && e.Coordinates.LoopID == state.loopID {
			if t := stepDoneText(e.Messages); t != "" {
				state.lastStep = t
			}
		}
	case event.TurnDone:
		if e.Coordinates.TurnID == state.turnID && e.Coordinates.LoopID == state.loopID {
			final := aiText(e.Message)
			if final == "" && stepFallback {
				final = state.lastStep
			}
			return final, true, nil
		}
	case event.TurnFailed:
		if e.Coordinates.TurnID == state.turnID && e.Coordinates.LoopID == state.loopID {
			return "", true, &drainFailedError{Cause: e.Err}
		}
	case event.TurnInterrupted:
		if e.Coordinates.TurnID == state.turnID && e.Coordinates.LoopID == state.loopID {
			return "", true, &drainInterruptedError{}
		}
	}
	return "", false, nil
}

// aiText concatenates the text of every TextBlock in an AIMessage. A nil message
// or a message with no text blocks yields the empty string.
func aiText(m *content.AIMessage) string {
	if m == nil {
		return ""
	}
	var b strings.Builder
	for _, blk := range m.Blocks {
		if tb, ok := blk.(*content.TextBlock); ok {
			remaining := maxDelegateOutputBytes - b.Len()
			if remaining <= 0 {
				break
			}
			if len(tb.Text) > remaining {
				b.WriteString(tb.Text[:remaining])
				break
			}
			b.WriteString(tb.Text)
		}
	}
	return b.String()
}

// stepDoneText extracts the assistant text from a StepDone's committed group. A
// StepDone carries the step's single AIMessage followed by its ToolResultMessages
// (content.AgenticMessages, a sealed []Conversation union); the fallback text is
// the AIMessage's concatenated TextBlocks. The empty string means no assistant
// text in this step (e.g. a pure tool-use step).
func stepDoneText(msgs content.AgenticMessages) string {
	for _, m := range msgs {
		if ai, ok := m.(*content.AIMessage); ok {
			return aiText(ai)
		}
	}
	return ""
}

// Compile-time assertions that every typed exit error satisfies error.
var (
	_ error = (*drainFailedError)(nil)
	_ error = (*drainInterruptedError)(nil)
	_ error = (*drainCancelledError)(nil)
	_ error = (*drainLostError)(nil)
)
