package command

import (
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/gate"
)

// ApproveToolCall approves the pending tool call identified by ToolExecutionID
// with exactly one of the two approve actions. It is a fire-and-route control
// command: the actor routes it by GateToolExecutionID to the permission gate
// blocked on that call, so there is no Ack (the gate's unblocking and the
// subsequent ToolCallStarted event are the observable effect, not a reply on
// this command).
//
// The command carries no scope, no grant tokens, and no persistence payload:
// gate.ApprovalApprove approves once and writes nothing, while
// gate.ApprovalApproveAlwaysWorkspace additionally instructs the evaluator to
// atomically persist the displayed reusable rule candidates before execution.
// Fresh execution-bound grants are minted by the evaluator AFTER the decision
// and travel only in the prepared execution contract, never on this wire.
type ApproveToolCall struct {
	Header
	// GateRoute locates the loop (the session dispatches by LoopID) and names the
	// pending gate (ToolExecutionID), which the actor matches against.
	GateRoute
	// Action is gate.ApprovalApprove or gate.ApprovalApproveAlwaysWorkspace.
	// Any other value — including gate.ApprovalDeny, which travels on
	// DenyToolCall — fails ValidateCommand closed, so a malformed or legacy
	// record can never decode into an approval.
	Action gate.ApprovalAction `json:"action"`
}

func (ApproveToolCall) isCommand() {}

// GateToolExecutionID returns the tool-call id this command targets, so the actor can
// route it to the matching pending gate.
func (c ApproveToolCall) GateToolExecutionID() uuid.UUID { return c.GateRoute.ToolExecutionID }
