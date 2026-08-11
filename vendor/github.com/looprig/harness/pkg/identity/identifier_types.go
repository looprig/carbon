// Package identity holds the shared correlation types used across the loop,
// command, and event packages: the Coordinates quartet, the Cause causal edge,
// and the Agency audit enum. It sits below event/command and imports only
// internal/uuid, so embedding these types never forms an import cycle.
package identity

import (
	"strconv"

	"github.com/looprig/core/uuid"
)

// Coordinates is the four nesting ids that locate any action in the hierarchy:
// session ▸ loop ▸ turn ▸ step. Declared once and embedded wherever the full
// quartet is needed (event.Header, Cause, GateRoute). Named Coordinates — not
// Scope — because event.Scope is already the delivery-scope enum.
type Coordinates struct {
	SessionID uuid.UUID `json:"session_id,omitzero"`
	LoopID    uuid.UUID `json:"loop_id,omitzero"`
	TurnID    uuid.UUID `json:"turn_id,omitzero"`
	StepID    uuid.UUID `json:"step_id,omitzero"`
}

// AgentName is the immutable attribution name a loop runs under — the role/identity
// of the agent driving that loop (e.g. "operator", "code reviewer"). It is stamped on
// the loop's LoopStarted at creation and never changes for the life of the loop, so the
// durable record carries a stable answer to "which agent produced this?".
//
// The zero value (the empty string) means UNSET: a plain loop that was started without
// an attribution name, or a record persisted before AgentName existed. Restore treats an
// empty stored name as distinct from a configured non-empty one (it does not silently
// accept the legacy zero as a match) so a name change is never resumed unnoticed.
type AgentName string

// Agency records who performed an action — per action, not per turn. It is an
// audit/observability record (who approved, who interrupted), not the gate
// decision. The zero value is AgencyMachine, the fail-secure default: a
// missing value reads as "our code did it", so we never falsely claim a human
// acted.
type Agency uint8

const (
	AgencyMachine Agency = iota // 0 — the DEFAULT: our code did it
	AgencyUser                  // a human did it
)

// String renders the agency for logs. Unknown values render as Agency(n).
func (a Agency) String() string {
	switch a {
	case AgencyMachine:
		return "machine"
	case AgencyUser:
		return "user"
	default:
		return "Agency(" + strconv.Itoa(int(a)) + ")"
	}
}

// Cause is the direct causal edge — "the thing that caused this" — expressed in
// the same id vocabulary. Which fields are set varies per event/command; most
// are omitted. Agency is a copy of the causing command's Header.Agency, carried
// so an event can surface agency without chasing the command.
type Cause struct {
	Coordinates               // the cause's coordinates (full quartet, mostly omitted)
	CommandID       uuid.UUID `json:"command_id,omitzero"`
	EventID         uuid.UUID `json:"event_id,omitzero"`
	ToolExecutionID uuid.UUID `json:"tool_execution_id,omitzero"`
	Agency          Agency    `json:"agency,omitzero"` // the causing command's agency (machine by default)
}
