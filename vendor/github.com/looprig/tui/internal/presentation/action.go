package presentation

import (
	"encoding/json"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/gate"
)

// uiActionKind tags a uiAction. It is a closed enum: the interactionModel
// produces a uiAction, and Screen (a later task) switches on Kind to drive the
// agent. A tagged struct — never an `any` payload — keeps the contract typed end
// to end (CLAUDE.md: strict typing, no interface{} in business logic).
type uiActionKind uint8

const (
	// uiNoop is the zero value: the key was consumed (e.g. an edit) and there is
	// nothing for Screen to act on. It is also the deferral result for non-compose
	// modes until modal routing lands (Task 8).
	uiNoop uiActionKind = iota
	// uiSubmit carries composed prose in Text for Screen to start/queue a turn.
	uiSubmit
	// uiRunSlash carries a known slash command name in Slash for Screen to dispatch.
	uiRunSlash
	// uiApprove resolves a permission gate (ToolExecutionID) with the chosen approval
	// action (Approval) — gate.ApprovalApprove (once, persists nothing) or
	// gate.ApprovalApproveAlwaysWorkspace (persists the displayed candidate rules).
	uiApprove
	// uiDeny resolves a permission gate (ToolExecutionID) fail-secure (gate.ApprovalDeny).
	uiDeny
	// uiAnswer supplies the AskUser reply Text for the gate ToolExecutionID. (Task 8.)
	uiAnswer
	// uiInterrupt requests a turn interrupt. (Produced in Task 8.)
	uiInterrupt
	// uiGateRespond answers a HOST-RAISED gate (GateID) with GateAction and, for a
	// form accept, Values. Unlike the permission/AskUser actions it names the GATE
	// directly rather than a tool execution, because such a gate is folded from
	// GateOpened, which names it outright.
	//
	// It serves both host-raised kinds — form and open-url — because the response
	// is the same act in both: an action the gate advertised, sent to a gate id.
	// Only the Values differ (an open-url gate has none to send), which is what a
	// tagged union's zero fields are for.
	uiGateRespond
)

// uiAction is the single typed result the interactionModel hands back from an
// Update. It is a tagged union: Kind selects which fields are meaningful, and the
// rest stay at their zero values. Defining every variant up front lets later
// tasks PRODUCE approve/deny/answer/interrupt without changing this contract;
// this task only ever produces uiSubmit / uiRunSlash / uiNoop.
type uiAction struct {
	Kind uiActionKind
	Text string // uiSubmit / uiAnswer payload
	// LoopID is the gate-opening loop's id, carried for uiApprove/uiDeny/uiAnswer so
	// Screen dispatches the gate reply to the loop that produced the prompt (stamped
	// on the pending prompt from its request event's Header.LoopID), not
	// unconditionally to the active loop. Zero for non-gate actions.
	LoopID          uuid.UUID
	ToolExecutionID uuid.UUID           // uiApprove / uiDeny / uiAnswer target gate
	Approval        gate.ApprovalAction // uiApprove decision (Approve / Approve always for this workspace)
	Slash           string              // uiRunSlash command name (e.g. "/help")

	// GateID is the uiGateRespond target gate.
	GateID gate.ID
	// GateAction is the uiGateRespond action: one of the gate.FormAction* values.
	// The gate must have advertised it in Prompt.Controls — the session refuses
	// any action a gate did not offer.
	GateAction string
	// Values carries a form accept's answers, keyed by schema field name and
	// already encoded as the JSON types gate.ParseFormAnswers expects (string for
	// text/select, bool for confirm). It is nil for any other action, and always
	// nil for an open-url gate, which has no fields to answer.
	Values map[string]json.RawMessage
}
