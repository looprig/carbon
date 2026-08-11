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

var (
	errCheckedWorkflowActivityPublisherSessionID = errors.New("session: workflow activity publisher requires a non-zero owning session id")
	errCheckedWorkflowActivityPublisherHub       = errors.New("session: workflow activity publisher requires a non-nil hub")
)

// WorkflowActivityOwnerMismatchError reports a workflow activity addressed to a
// session other than the one that owns the resource. A session resource never
// gets a generic cross-session event publisher.
type WorkflowActivityOwnerMismatchError struct {
	Want uuid.UUID
	Got  uuid.UUID
}

func (e *WorkflowActivityOwnerMismatchError) Error() string {
	return "session: workflow activity session " + e.Got.String() +
		" does not match owning session " + e.Want.String()
}

type checkedWorkflowActivityPublisher struct {
	sessionID uuid.UUID
	hub       *hub.Hub
	factory   *event.Factory
}

func newCheckedWorkflowActivityPublisher(
	sessionID uuid.UUID,
	h *hub.Hub,
	now func() time.Time,
) (*checkedWorkflowActivityPublisher, error) {
	if sessionID.IsZero() {
		return nil, errCheckedWorkflowActivityPublisherSessionID
	}
	if h == nil {
		return nil, errCheckedWorkflowActivityPublisherHub
	}
	if now == nil {
		now = time.Now
	}
	return &checkedWorkflowActivityPublisher{
		sessionID: sessionID,
		hub:       h,
		factory:   event.NewFactory(uuid.New, now),
	}, nil
}

// PublishWorkflowActivity maps the neutral resource DTO to the one sealed
// WorkflowActivity event, preserves its deterministic EventID, validates its
// complete identity/body, and sends it through the ordinary durable Hub path.
// OccurredAt is also the stable creation envelope for this source activity: it
// lets a retry after a process restart reproduce the exact durable payload while
// retaining the event's separate source-occurrence field. The publisher never
// accepts a generic event.Event, so a resource cannot use this trusted seam to
// bypass the event union or publish another event type.
func (p *checkedWorkflowActivityPublisher) PublishWorkflowActivity(
	ctx context.Context,
	metadata tool.WorkflowActivityMetadata,
) error {
	if metadata.SessionID != p.sessionID {
		return &WorkflowActivityOwnerMismatchError{Want: p.sessionID, Got: metadata.SessionID}
	}
	ev := event.WorkflowActivity{
		Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: metadata.SessionID},
			EventID:     metadata.EventID,
			CreatedAt:   metadata.OccurredAt,
		},
		RunID:             metadata.RunID,
		WorkflowName:      metadata.WorkflowName,
		WorkflowVersion:   metadata.WorkflowVersion,
		Kind:              event.WorkflowActivityKind(metadata.Kind),
		Status:            event.WorkflowRunStatus(metadata.Status),
		VertexID:          metadata.VertexID,
		VertexLabel:       metadata.VertexLabel,
		CompletedVertices: metadata.CompletedVertices,
		TotalVertices:     metadata.TotalVertices,
		Message:           metadata.Message,
		OccurredAt:        metadata.OccurredAt,
	}
	stamped, err := p.factory.StampWorkflowActivity(ev, metadata.EventID)
	if err != nil {
		return err
	}
	return p.hub.PublishEventChecked(ctx, stamped)
}

var _ tool.WorkflowActivityPublisher = (*checkedWorkflowActivityPublisher)(nil)
