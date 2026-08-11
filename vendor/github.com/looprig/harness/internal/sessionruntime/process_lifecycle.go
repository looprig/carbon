package sessionruntime

import (
	"context"
	"errors"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hub"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/tool"
)

// errCheckedProcessLifecyclePublisherSessionID/Hub report a composition-wiring
// bug: the checked publisher must be constructed with the non-zero session it
// durably publishes for and a non-nil hub to publish through.
var (
	errCheckedProcessLifecyclePublisherSessionID = errors.New("session: checked process lifecycle publisher requires a non-zero owning session id")
	errCheckedProcessLifecyclePublisherHub       = errors.New("session: checked process lifecycle publisher requires a non-nil hub")
)

// ProcessLifecycleOwnerMismatchError reports that a Tools-supplied
// ProcessLifecycleMetadata named a session other than the one the checked
// publisher was attached to. A process resource is bound to exactly one session
// for its whole lifetime, so metadata naming a different session can only be a
// bug or a forged call — never a legitimate publish — and is rejected fail-secure
// before it ever reaches the durable journal or the hub.
type ProcessLifecycleOwnerMismatchError struct {
	Want uuid.UUID
	Got  uuid.UUID
}

func (e *ProcessLifecycleOwnerMismatchError) Error() string {
	return "session: process lifecycle metadata session " + e.Got.String() +
		" does not match owning session " + e.Want.String()
}

// checkedProcessLifecyclePublisher is Task 24B's real implementation of
// tool.ProcessLifecyclePublisher: it validates the Tools-supplied coordinates and
// lifecycle matrix, builds the matching sealed pkg/event record preserving every
// DTO field byte-for-byte and stamping only the Harness envelope CreatedAt (never
// replacing the Tools-supplied EventID), and durably appends + publishes it live
// through the session hub. Because the hub's own append-before-apply publish path
// gates its state mutation and live broadcast on whether the durable append
// produced a genuinely NEW frame (see pkg/hub), a deduplicated retry — the
// underlying journal reporting AppendResult.Appended=false for an already-durable
// EventID — durably appends nothing further and broadcasts nothing: this method
// still returns nil (the record is durably persisted, just not by this call).
type checkedProcessLifecyclePublisher struct {
	sessionID uuid.UUID
	hub       *hub.Hub
	now       func() time.Time
}

// newCheckedProcessLifecyclePublisher constructs the checked publisher bound to
// sessionID, publishing through h. now defaults to time.Now when nil (tests pin
// it for deterministic envelope timestamps).
func newCheckedProcessLifecyclePublisher(
	sessionID uuid.UUID,
	h *hub.Hub,
	now func() time.Time,
) (*checkedProcessLifecyclePublisher, error) {
	if sessionID.IsZero() {
		return nil, errCheckedProcessLifecyclePublisherSessionID
	}
	if h == nil {
		return nil, errCheckedProcessLifecyclePublisherHub
	}
	if now == nil {
		now = time.Now
	}
	return &checkedProcessLifecyclePublisher{sessionID: sessionID, hub: h, now: now}, nil
}

// PublishProcessLifecycle validates metadata against the owning session, the
// bounded neutral DTO's own invariants, and the sealed event's identity profile,
// then durably appends and publishes the built event live. It returns the first
// validation error verbatim (never durably appending or publishing an invalid or
// misrouted record) and otherwise the hub's append/publish error unchanged, so an
// append failure faults the caller exactly like any other checked publish.
func (p *checkedProcessLifecyclePublisher) PublishProcessLifecycle(
	ctx context.Context,
	metadata tool.ProcessLifecycleMetadata,
) error {
	if metadata.SessionID != p.sessionID {
		return &ProcessLifecycleOwnerMismatchError{Want: p.sessionID, Got: metadata.SessionID}
	}
	if err := metadata.Validate(); err != nil {
		return err
	}
	ev, err := buildProcessLifecycleEvent(metadata, p.now())
	if err != nil {
		return err
	}
	if err := event.ValidateEvent(ev); err != nil {
		return err
	}
	return p.hub.PublishEventChecked(ctx, ev)
}

var _ tool.ProcessLifecyclePublisher = (*checkedProcessLifecyclePublisher)(nil)

// buildProcessLifecycleEvent maps metadata.Kind to its matching sealed pkg/event
// concrete type. The Header carries ONLY envelope metadata absent from the
// neutral DTO — SessionID/LoopID coordinates (repeated from metadata, required by
// the event's identity profile) and createdAt (the Harness envelope creation
// time) — and metadata.EventID unchanged: this function never mints or replaces
// an EventID. The Process field carries metadata verbatim, byte-for-byte.
func buildProcessLifecycleEvent(metadata tool.ProcessLifecycleMetadata, createdAt time.Time) (event.Event, error) {
	header := event.Header{
		Coordinates: identity.Coordinates{SessionID: metadata.SessionID, LoopID: metadata.LoopID},
		EventID:     metadata.EventID,
		CreatedAt:   createdAt,
	}
	switch metadata.Kind {
	case tool.ProcessLifecycleStarted:
		return event.ProcessStarted{Header: header, Process: metadata}, nil
	case tool.ProcessLifecycleBackgrounded:
		return event.ProcessBackgrounded{Header: header, Process: metadata}, nil
	case tool.ProcessLifecycleCompleted:
		return event.ProcessCompleted{Header: header, Process: metadata}, nil
	case tool.ProcessLifecycleStopRequested:
		return event.ProcessStopRequested{Header: header, Process: metadata}, nil
	case tool.ProcessLifecycleLost:
		return event.ProcessLost{Header: header, Process: metadata}, nil
	default:
		return nil, &tool.ProcessLifecycleValidationError{Field: "kind"}
	}
}
