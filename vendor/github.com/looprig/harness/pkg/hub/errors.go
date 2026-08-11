package hub

import (
	"fmt"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/hustle"
)

// PublishBoundaryReason identifies why an event was rejected at a hub publication
// boundary. The closed reason set lets callers inspect the denial without parsing
// an error string or retaining the event payload in the error.
type PublishBoundaryReason string

const (
	PublishBoundaryNilEvent   PublishBoundaryReason = "nil_event"
	PublishBoundaryVisibility PublishBoundaryReason = "visibility"
	PublishBoundaryClass      PublishBoundaryReason = "class"
	PublishBoundarySession    PublishBoundaryReason = "session"
	PublishBoundaryType       PublishBoundaryReason = "type"
	PublishBoundaryInvalid    PublishBoundaryReason = "invalid"
)

// PublishBoundaryError reports a fail-closed event publication denial.
type PublishBoundaryError struct {
	Reason    PublishBoundaryReason
	EventType string
	Cause     error
}

func (e *PublishBoundaryError) Error() string {
	message := "hub: event publication denied"
	if e.EventType != "" {
		message += " for " + e.EventType
	}
	if e.Reason != "" {
		message += ": " + string(e.Reason)
	}
	if e.Cause != nil {
		message += ": " + e.Cause.Error()
	}
	return message
}

func (e *PublishBoundaryError) Unwrap() error { return e.Cause }

// HustleActivityReason identifies an activity-acquisition contract violation.
type HustleActivityReason string

const (
	HustleActivityInvalidRunID HustleActivityReason = "invalid_run_id"
	HustleActivityDuplicate    HustleActivityReason = "duplicate_run_id"
	HustleActivityStopped      HustleActivityReason = "session_stopped"
)

// HustleActivityError reports that a blocking hustle activity could not be
// inserted into the hub's quiescence set.
type HustleActivityError struct {
	Reason HustleActivityReason
	RunID  hustle.RunID
}

func (e *HustleActivityError) Error() string {
	return fmt.Sprintf("hub: hustle activity denied: %s", e.Reason)
}

// TurnStartReservationReason identifies why a loop could not reserve the Hub's
// activity transition for its opening TurnStarted publication.
type TurnStartReservationReason string

const (
	TurnStartReservationInvalidLoop TurnStartReservationReason = "invalid_loop_id"
	TurnStartReservationStopped     TurnStartReservationReason = "session_stopped"
	TurnStartReservationMismatch    TurnStartReservationReason = "publication_mismatch"
	TurnStartReservationReleased    TurnStartReservationReason = "released"
	TurnStartReservationReused      TurnStartReservationReason = "reused"
)

// TurnStartReservationError reports a denied or mismatched one-shot turn-start
// activity reservation.
type TurnStartReservationError struct {
	Reason TurnStartReservationReason
	LoopID uuid.UUID
}

func (e *TurnStartReservationError) Error() string {
	return fmt.Sprintf("hub: turn-start activity reservation denied: %s", e.Reason)
}
