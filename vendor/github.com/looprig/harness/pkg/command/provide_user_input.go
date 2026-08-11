package command

import "github.com/looprig/core/uuid"

// ProvideUserInput supplies the user's Answer to a pending AskUser request
// identified by ToolExecutionID. Like the approve/deny pair it is a fire-and-route control
// command with no Ack: the actor routes it by GateToolExecutionID to the user-input gate
// blocked on that call, which delivers Answer to the waiting tool.
type ProvideUserInput struct {
	Header
	// GateRoute locates the loop (the session dispatches by LoopID) and names the
	// pending gate (ToolExecutionID), which the actor matches against.
	GateRoute
	Answer string `json:"answer,omitempty"`
}

func (ProvideUserInput) isCommand() {}

// GateToolExecutionID returns the tool-call id this command targets, so the actor can
// route it to the matching pending gate.
func (c ProvideUserInput) GateToolExecutionID() uuid.UUID { return c.GateRoute.ToolExecutionID }
