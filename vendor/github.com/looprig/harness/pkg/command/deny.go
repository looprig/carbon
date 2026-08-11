package command

import "github.com/looprig/core/uuid"

// DenyToolCall denies a pending tool call identified by ToolExecutionID. Like
// ApproveToolCall it is a fire-and-route control command with no Ack: the actor
// routes it by GateToolExecutionID to the permission gate, which fails the call closed
// (fail-secure). Denial carries no scope — nothing is ever persisted on a deny.
type DenyToolCall struct {
	Header
	// GateRoute locates the loop (the session dispatches by LoopID) and names the
	// pending gate (ToolExecutionID), which the actor matches against.
	GateRoute
}

func (DenyToolCall) isCommand() {}

// GateToolExecutionID returns the tool-call id this command targets, so the actor can
// route it to the matching pending gate.
func (c DenyToolCall) GateToolExecutionID() uuid.UUID { return c.GateRoute.ToolExecutionID }
