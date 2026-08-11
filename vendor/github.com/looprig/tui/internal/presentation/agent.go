package presentation

import (
	"context"
	"encoding/json"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
)

// EventStream is the narrow consumer-facing handle the TUI reads whole-session
// events from. It is event.Subscription — the read+teardown contract the session
// hub's *EventSubscription satisfies structurally — so the TUI depends on the
// interface, not the concrete hub type. Events yields the filtered fan-in stream;
// it closes on Close or on a hub-forced loss, after which Err reports the typed
// cause (nil for an intentional Close).
type EventStream = event.Subscription

// Agent is the narrow surface the TUI drives. *coding.Coding satisfies it
// structurally; the TUI never imports any agent package.
type Agent interface {
	// SessionID returns the durable, non-zero identity of the current session.
	// The startup banner reads it directly so a restored or /clear-replaced agent
	// always displays its own session rather than stale composition metadata.
	SessionID() uuid.UUID
	// Submit sends input fire-and-forget as a queueable UserInput; the returned
	// InputID correlates the Reply events (Cause.CommandID) that report the outcome.
	// It targets the session's ACTIVE loop — the currently selected default input
	// target the single-loop convenience Screen and the modern viewport both use.
	Submit(ctx context.Context, blocks []content.Block) (uuid.UUID, error)
	// SubmitToLoop sends input fire-and-forget to a SPECIFIC loop — the modern
	// viewport's FOCUSED loop — rather than the active loop, so a submit while focused on a
	// subagent runs a new turn on THAT loop. It is the loop-targeted counterpart of
	// Submit (same fire-and-forget InputID/Cause.CommandID contract, human agency); a
	// loopID equal to the ACTIVE loop id behaves exactly like Submit.
	SubmitToLoop(ctx context.Context, loopID uuid.UUID, blocks []content.Block) (uuid.UUID, error)
	// CompactToLoop requests manual conversation compaction for one exact loop.
	// The modern viewport supplies its focused loop so this action never redirects
	// through the session's active-loop convenience target.
	CompactToLoop(ctx context.Context, loopID uuid.UUID) (uuid.UUID, error)
	// ActiveLoopID returns the current default input target.
	ActiveLoopID() uuid.UUID
	Interrupt(ctx context.Context) (bool, error)
	Close(ctx context.Context) error
	// AcceptsImages reports the current model capability for loopID — whether the model
	// bound to that loop accepts image blocks — so buildBlocks can reject image @path
	// tokens at the boundary instead of failing mid-turn. It is keyed on the loop because
	// a multi-loop session runs heterogeneous models: the focused subagent's model, not
	// the session's, governs a submission to that loop. Query it per submission (the loop's
	// model can change) and fail closed for an unknown loop.
	AcceptsImages(loopID uuid.UUID) bool

	// Subscribe attaches a whole-session event consumer to the agent's session
	// fan-in with the given filter and returns its EventStream. It is the seam the
	// TUI uses to observe events across the entire session (every loop): a session
	// subscription spans turns and loops. The caller must Close the returned stream
	// when done. Use AllLoopsEventFilter for the whole-session all-loop delivery.
	Subscribe(filter event.EventFilter) (EventStream, error)

	// ReplayBacklog returns the RESTORED session's historical Enduring events for a
	// cold-restore repaint, in session order. It is the backlog seam the TUI folds
	// off the update loop (restoreBacklogCmd) to rebuild the committed transcript +
	// pending gates BEFORE attaching the live Subscribe stream. The slice is
	// materialized (the data layer is sub-second for realistic sizes) so the consumer
	// drains it without owning a cursor's lifetime. A NEW (non-restored) session
	// returns nil/empty — the TUI then skips the repaint and behaves exactly as a
	// fresh session. A read failure returns a typed error the fold surfaces as a
	// non-fatal restore-error notice (history could not repaint; the live stream is
	// unaffected). The events are the session's all-loop Enduring history — never
	// the live 256-cap hub buffer. ctx bounds the read.
	ReplayBacklog(ctx context.Context) ([]event.Event, error)

	// Approve resolves a pending tool-call permission gate with one of the two
	// approve actions — gate.ApprovalApprove (once, persists nothing) or
	// gate.ApprovalApproveAlwaysWorkspace (atomically persists the displayed reusable
	// rule candidates before execution). loopID is the loop that opened the gate (the
	// PermissionRequested event's Header.LoopID) so the reply is dispatched to the
	// right loop in a multi-loop session; callID identifies the gate. The agent
	// wrapper delegates to its session; the action string is exactly one the gate's
	// controls advertised, which the session validates.
	Approve(ctx context.Context, loopID, callID uuid.UUID, action gate.ApprovalAction) error
	// Deny resolves a pending tool-call permission gate by failing it closed
	// (fail-secure); nothing is persisted. loopID names the gate-opening loop so the
	// reply reaches the right loop. The wrapper delegates to its session.
	Deny(ctx context.Context, loopID, callID uuid.UUID) error
	// ProvideAnswer supplies the user's reply to a pending AskUser request
	// identified by callID. loopID names the gate-opening loop so the answer reaches
	// the right loop. It is the TUI-facing name for the session's ProvideUserInput;
	// the wrapper delegates to it.
	ProvideAnswer(ctx context.Context, loopID, callID uuid.UUID, answer string) error
	// RespondGate answers a HOST-RAISED gate identified by gateID — a form gate
	// (gate.KindForm) or an open-url gate (gate.KindOpenURL). Unlike the three
	// above it names the GATE directly: such a gate is observed through GateOpened,
	// which carries the gate id, so there is nothing to look up. It is one method
	// for both kinds because the act is identical — an advertised action sent to a
	// gate id — and only an open-url gate's empty values distinguish them.
	// action is one of the gate.FormAction* values; values carries a form accept's
	// answers keyed by schema field name, already encoded as the JSON types the
	// schema calls for, and is nil for any other action (and always nil for an
	// open-url gate, which has no fields). The session validates
	// them against the gate's authoritative schema and rejects an action the gate
	// never offered, so this is a request, not a command.
	RespondGate(ctx context.Context, gateID gate.ID, action string, values map[string]json.RawMessage) error
}

// AllLoopsEventFilter is the TUI's declared interest for a session subscription: BOTH
// classes deliver from EVERY loop — Ephemeral is All and Enduring is All. It takes no loop
// id because neither scope discriminates by loop, and session-scoped events
// (SessionStarted/Active/Idle/Stopped, ActiveLoopChanged) bypass the loop filter and always
// deliver.
//
// The TUI renders every loop's WHOLE live stream (a user can focus any subagent loop and
// watch its live tokens stream), so it must actually RECEIVE every loop's live Ephemeral
// firehose. The widened scope also delivers each loop's tool-lifecycle events, which fold
// into that loop's live projection. An active-only Ephemeral scope would starve projections
// of a subagent's live output, freezing a focused subagent view at Enduring StepDone
// granularity. The whole-session hub buffer is bounded and has no replay, so the TUI opens
// ONE all-loops subscription at startup and never re-subscribes; focus is then a pure view
// filter over already-received, already-projected state.
func AllLoopsEventFilter() event.EventFilter {
	return event.EventFilter{
		Ephemeral: event.LoopScope{All: true},
		Enduring:  event.LoopScope{All: true},
	}
}

// OpenAgent constructs a fresh Agent. The composition root binds it to its session factory.
// On /clear the TUI closes the current Agent before invoking OpenAgent, allowing exclusive
// resources to transfer to the replacement. A failed open is terminal because the closed
// current Agent cannot be resumed. Implementations MUST honor context cancellation: runtime
// shutdown waits a bounded interval for an in-flight handoff, after which it returns fatal and
// the coordinator closes any replacement that arrives late.
type OpenAgent func(context.Context) (Agent, error)
