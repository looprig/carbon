package command

import (
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/identity"
)

// GateRoute is the routing key for a gate reply. identity.Coordinates locates the
// target loop — the session dispatches by GateRoute.LoopID — and ToolExecutionID
// names the pending gate the loop matches against (its pendingGates key). It is
// embedded in the gate commands so a reply carries both the dispatch target (the
// loop) and the match key (the tool call), replacing the former route-to-primary.
//
// A zero coordinate means "unspecified at this granularity": a GateRoute with only
// LoopID set addresses a loop without naming a turn. The loop resolves the gate by
// ToolExecutionID, so the coordinates are the addressing envelope, not the match key.
type GateRoute struct {
	identity.Coordinates
	GateID          uuid.UUID `json:"gate_id,omitzero"`
	ToolExecutionID uuid.UUID `json:"tool_execution_id,omitzero"`
}
