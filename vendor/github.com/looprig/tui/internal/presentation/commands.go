package presentation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
)

// blinkInterval is the cadence of the live-surface animation tick: the streaming
// assistant dot blinks and the running tool spinner steps once per interval. ~450ms
// reads as a calm "working" pulse — fast enough to feel live, slow enough not to
// strobe or churn the render loop.
const blinkInterval = 450 * time.Millisecond

// interruptTimeout bounds an Interrupt ack so Update never waits on a wedged session.
const interruptTimeout = 2 * time.Second

// compactDispatchTimeout bounds a manual compaction request so a wedged session
// cannot strand the slash-command dispatch.
const compactDispatchTimeout = 2 * time.Second

// closeTimeout bounds a best-effort close so a hung session cannot wedge quit.
const closeTimeout = 5 * time.Second

// promptDispatchTimeout bounds an approve/deny/answer send so Update never blocks
// on a wedged session resolving a permission or AskUser gate. It mirrors
// interruptTimeout's shape: the dispatch is fire-and-route, so a lost or late send
// is self-healing (the next terminal event clears the prompt queue regardless).
const promptDispatchTimeout = 2 * time.Second

// subscribeWith attaches the session-lifetime subscription with the GIVEN event filter
// and reports the outcome. It is the ONE event source for the whole session — established
// once at startup (batched into Init) and re-established after a /clear swaps the agent;
// the returned stream spans every turn and loop and closes only on Close, hub-forced loss,
// or hub teardown, never per turn. The filter is INJECTED by the embedding shell via
// sessionCore.subscribe (the AllLoopsEventFilter all-loop scope), so the subscription scope
// is chosen at the composition seam, not hard-coded in the command.
func subscribeWith(agent Agent, filter event.EventFilter) tea.Cmd {
	return func() tea.Msg {
		sub, err := agent.Subscribe(filter)
		return subscribedMsg{sub: sub, err: err}
	}
}

// subNext receives exactly one event from the session subscription and maps it to
// a tea.Msg: a closed channel → subClosedMsg carrying the typed termination cause
// (nil for an intentional Close, a *hub.SubscriptionLossError for a hub-forced
// loss), otherwise eventMsg. Re-dispatch it after each event to drive the
// continuous reader forward without a drain goroutine. It NEVER EOFs per turn —
// the subscription is whole-session.
func subNext(sub EventStream) tea.Cmd {
	return func() tea.Msg {
		// Defensive: during the /clear re-subscribe window m.sub is briefly nil
		// (closed, awaiting the new subscribedMsg). A re-arm constructed from that
		// transient nil must be a no-op, not a nil-deref panic — the fresh
		// subscription's reader is started by handleSubscribed.
		if sub == nil {
			return nil
		}
		d, ok := <-sub.Events()
		if !ok {
			return subClosedMsg{err: sub.Err()}
		}
		return eventMsg{ev: d.Event, journalSeq: d.JournalSeq}
	}
}

// submitCmd sends blocks fire-and-forget via Submit under the app context and
// reports the outcome, capturing the loop-assigned InputID and echoing back the
// submitted blocks. The loop owns queueing, so there is no per-turn reader to
// install and no status branching here: Submit returns immediately once the input
// is enqueued, and the loop publishes the turn-lifecycle + content events back on
// the subscription. On success the (InputID, blocks) let handleSubmitResult record
// the submit so the queued affordance can show once the loop's InputQueued event
// arrives; the authoritative user row is committed later from the
// TurnStarted/TurnFoldedInto Message, never optimistically at submit. A non-nil err
// lets Update surface a faint, non-fatal send failure.
func submitCmd(ctx context.Context, agent Agent, blocks []content.Block) tea.Cmd {
	return func() tea.Msg {
		id, err := agent.Submit(ctx, blocks)
		return submitResultMsg{inputID: id, blocks: blocks, err: err}
	}
}

// submitToLoopCmd is submitCmd's loop-targeted counterpart: it sends blocks
// fire-and-forget to a SPECIFIC loop (the modern viewport's focused loop) via
// SubmitToLoop and reports the outcome in the SAME submitResultMsg shape, so
// handleSubmitResult records the submit and surfaces a send failure identically to the
// primary path. loopID is the focused loop the modern composer routes to; everything
// else mirrors submitCmd (the loop still owns queueing, so there is no per-turn reader
// and no status branching here).
func submitToLoopCmd(ctx context.Context, agent Agent, loopID uuid.UUID, blocks []content.Block) tea.Cmd {
	return func() tea.Msg {
		id, err := agent.SubmitToLoop(ctx, loopID, blocks)
		return submitResultMsg{inputID: id, blocks: blocks, err: err}
	}
}

// compactToLoopCmd requests manual compaction of one exact loop under a bounded
// context. The command reports the harness correlation id and preserves the
// returned error unchanged for the presentation reducer.
func compactToLoopCmd(ctx context.Context, agent Agent, loopID uuid.UUID) tea.Cmd {
	return func() tea.Msg {
		c, cancel := context.WithTimeout(ctx, compactDispatchTimeout)
		defer cancel()
		id, err := agent.CompactToLoop(c, loopID)
		return compactResultMsg{commandID: id, err: err}
	}
}

// interruptTurn issues a bounded Interrupt and reports the result, so Update
// never blocks on the session's interrupt ack.
func interruptTurn(ctx context.Context, agent Agent) tea.Cmd {
	return func() tea.Msg {
		ictx, cancel := context.WithTimeout(ctx, interruptTimeout)
		defer cancel()
		cancelled, err := agent.Interrupt(ictx)
		return interruptResultMsg{cancelled: cancelled, err: err}
	}
}

// promptResultMsg reports the outcome of a bounded prompt dispatch (approve,
// deny, or provide-answer). Only the error matters at the UI: the optimistic-pop
// design needs no ack, so a nil err is a silent success and a non-nil err lets
// Update surface a faint failure line. It is a tea.Msg.
type promptResultMsg struct{ err error }

// approveCmd issues a bounded Approve for a pending permission gate and reports the
// result, so Update never blocks on the session resolving the gate. loopID is the
// gate-opening loop (so the reply is dispatched there); callID identifies the gate;
// action is the chosen approve decision (gate.ApprovalApprove or
// gate.ApprovalApproveAlwaysWorkspace).
func approveCmd(ctx context.Context, agent Agent, loopID, callID uuid.UUID, action gate.ApprovalAction) tea.Cmd {
	return func() tea.Msg {
		c, cancel := context.WithTimeout(ctx, promptDispatchTimeout)
		defer cancel()
		return promptResultMsg{err: agent.Approve(c, loopID, callID, action)}
	}
}

// denyCmd issues a bounded Deny (fail-secure) for a pending permission gate and
// reports the result, so Update never blocks on the session failing it closed.
// loopID is the gate-opening loop so the reply is dispatched there.
func denyCmd(ctx context.Context, agent Agent, loopID, callID uuid.UUID) tea.Cmd {
	return func() tea.Msg {
		c, cancel := context.WithTimeout(ctx, promptDispatchTimeout)
		defer cancel()
		return promptResultMsg{err: agent.Deny(c, loopID, callID)}
	}
}

// provideAnswerCmd issues a bounded ProvideAnswer for a pending AskUser request and
// reports the result, so Update never blocks on the session consuming the answer.
// loopID is the gate-opening loop so the answer is dispatched there.
func provideAnswerCmd(ctx context.Context, agent Agent, loopID, callID uuid.UUID, answer string) tea.Cmd {
	return func() tea.Msg {
		c, cancel := context.WithTimeout(ctx, promptDispatchTimeout)
		defer cancel()
		return promptResultMsg{err: agent.ProvideAnswer(c, loopID, callID, answer)}
	}
}

// respondGateCmd issues a bounded RespondGate for a pending host-raised gate (form
// or open-url) and reports the result, so Update never blocks on the session
// validating the answer. gateID names the gate directly (GateOpened carried it);
// action is a gate.FormAction* value and values a form accept's answers (nil for
// an open-url gate).
func respondGateCmd(ctx context.Context, agent Agent, gateID gate.ID, action string, values map[string]json.RawMessage) tea.Cmd {
	return func() tea.Msg {
		c, cancel := context.WithTimeout(ctx, promptDispatchTimeout)
		defer cancel()
		return promptResultMsg{err: agent.RespondGate(c, gateID, action, values)}
	}
}

// reopenAgent hands ownership from old to a fresh /clear session. It closes old first so
// exclusive resources (notably a workspace root lease) are released before open constructs
// the replacement. Once close starts there is no rollback: a close/open error is terminal and
// the Update loop exits instead of pretending the old session remains live. ctx is the
// APPLICATION-lifetime context: OpenAgent implementations may derive the replacement
// session's own lifetime from it, so it must not be wrapped in a short construction timeout
// or cancelled when OpenAgent returns.
func reopenAgent(ctx context.Context, old Agent, open OpenAgent, handoff *reopenHandoff) tea.Cmd {
	return func() tea.Msg {
		result := reopenResultMsg{handoff: handoff}
		defer func() { handoff.complete(result) }()
		closeCtx, closeCancel := context.WithTimeout(context.Background(), closeTimeout)
		err := old.Close(closeCtx)
		closeCancel()
		if err != nil {
			result.err = fmt.Errorf("close current session: %w", err)
			return result
		}
		a, err := open(ctx)
		if err != nil {
			if !isNilAgent(a) {
				partialCloseCtx, partialCloseCancel := context.WithTimeout(context.Background(), closeTimeout)
				if closeErr := a.Close(partialCloseCtx); closeErr != nil {
					err = errors.Join(err, fmt.Errorf("close partial replacement: %w", closeErr))
				}
				partialCloseCancel()
			}
			a = nil
		}
		result.agent, result.err = a, err
		return result
	}
}

// isNilAgent recognizes both a nil interface and an interface holding a typed nil.
// Session factories commonly return concrete pointer agents, and converting a failed nil
// pointer to Agent otherwise makes it appear present and unsafe to clean up.
func isNilAgent(agent Agent) bool {
	if agent == nil {
		return true
	}
	value := reflect.ValueOf(agent)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

// closeAgent closes agent best-effort under a bounded Background context (not
// the app context, which may already be cancelled on quit), so a hung session
// cannot wedge the exit.
func closeAgent(agent Agent) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
		defer cancel()
		_ = agent.Close(ctx) // best-effort; Close is idempotent, nothing actionable at the UI
		return nil
	}
}

// closeAgentForQuit runs the deferred-/clear replacement close through its exactly-once
// coordinator. Update consumes the result before clearing model ownership and quitting.
func closeAgentForQuit(handoff *agentCloseHandoff) tea.Cmd {
	return func() tea.Msg {
		return closeForQuitResultMsg{handoff: handoff, err: handoff.close()}
	}
}

func closeStaleReopen(closing *staleReopenClose) tea.Cmd {
	return func() tea.Msg {
		return staleReopenCloseMsg{
			closing:  closing,
			closeErr: closing.handoff.close(),
		}
	}
}
