package event

import "github.com/looprig/core/uuid"

// DelegateDeliveryState is the durable resolution vocabulary for a delegated
// message's delivery attempt. The zero value is intentionally not a state: an
// intent and a fallback queue are represented by the exact journaled command
// record, while these values record only the foreign-delivery outcomes that
// must survive replay.
type DelegateDeliveryState string

const (
	// DelegateDeliverySteerAttemptReserved is appended before a foreign adapter
	// may admit the steering request to its writer. It is an intermediate state:
	// restore may correlate a later turn terminal or replay a durable fallback;
	// only an intent-only reservation is repaired as resolved_unknown.
	DelegateDeliverySteerAttemptReserved DelegateDeliveryState = "steer_attempt_reserved"
	// DelegateDeliveryResolvedUnknown records an ambiguous delivery result. It
	// is terminal for automatic delivery recovery: the request may have reached
	// the adapter, so no fallback is allowed.
	DelegateDeliveryResolvedUnknown DelegateDeliveryState = "resolved_unknown"
	// DelegateDeliveryResolvedUntrackable records an adapter lifecycle breach
	// that delivered outside the host-owned turn contract. No synthetic turn or
	// automatic fallback may follow it.
	DelegateDeliveryResolvedUntrackable DelegateDeliveryState = "resolved_untrackable"
)

// Valid reports whether s is one of the closed durable delivery states.
func (s DelegateDeliveryState) Valid() bool {
	switch s {
	case DelegateDeliverySteerAttemptReserved,
		DelegateDeliveryResolvedUnknown,
		DelegateDeliveryResolvedUntrackable:
		return true
	default:
		return false
	}
}

// DelegateDeliveryStateChanged is the durable state transition for a foreign
// delegate delivery attempt. It deliberately carries only the request id, the
// target loop, and the closed state vocabulary in its payload. The event never
// embeds command.UserInput (which would create an event↔command import cycle),
// broker tokens, or model-visible origin/session identity. The existing v1 event
// envelope versions this record on the journal wire.
//
// It is session-scoped because the session owns the reservation and adjudicates
// adapter delivery; TargetLoopID is explicit so the state remains addressable
// without pretending the target loop produced the event.
type DelegateDeliveryStateChanged struct {
	enduring
	sessionScoped
	Header
	RequestID    uuid.UUID             `json:"request_id"`
	TargetLoopID uuid.UUID             `json:"target_loop_id"`
	State        DelegateDeliveryState `json:"state"`
}
