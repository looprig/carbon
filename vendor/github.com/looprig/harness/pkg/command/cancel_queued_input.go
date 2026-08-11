package command

import (
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/identity"
)

// CancelQueuedInput retracts a still-queued submit (a UserInput/SubagentResult
// that received an InputQueued disposition). It is routed to the loop like the
// gate commands; the loop resolves it against its OWN inbox (race-free, since the
// actor is the sole owner of the queue), so there is no session-side TOCTOU.
//
// Coordinates selects the target loop; TargetCommandID names the queued submit to
// remove (it is the submit command's Header.CommandID, returned in
// InputQueued.Cause.CommandID).
//
// It is fire-and-forget — there is no Ack. Its outcome is observable as events,
// not a point-to-point reply: when the input was still queued the loop publishes
// the Enduring event.InputCancelled{CancelClientRetracted} keyed by TargetCommandID.
// When it had already started or folded into a turn — or was never queued — the
// retract is a pure no-op: the issuer infers "already committed / unknown" from the
// event.TurnStarted / event.TurnFoldedInto it already saw for that command.
type CancelQueuedInput struct {
	Header
	identity.Coordinates
	TargetCommandID uuid.UUID `json:"target_command_id,omitzero"`
}

func (CancelQueuedInput) isCommand() {}
