package tool

import (
	"context"
	"reflect"
	"time"

	"github.com/looprig/core/uuid"
)

// SessionResource is session-owned state shared by tool definitions. Activate
// late-binds live session services after construction and restore planning;
// Shutdown releases the resource during session teardown.
type SessionResource interface {
	Activate(context.Context, SessionResourceServices) error
	Shutdown(context.Context) error
}

// SessionResourceRegistry atomically resolves one session-owned resource by
// key. The factory receives a private storage directory reserved for that key.
type SessionResourceRegistry interface {
	GetOrCreate(context.Context, string, func(string) (SessionResource, error)) (SessionResource, error)
}

// ProcessLifecyclePublisher durably publishes bounded, metadata-only process
// lifecycle transitions.
type ProcessLifecyclePublisher interface {
	PublishProcessLifecycle(context.Context, ProcessLifecycleMetadata) error
}

// ProcessCompletionNotifier submits a bounded process terminal notification to
// the process owner's loop.
type ProcessCompletionNotifier interface {
	NotifyProcessCompletion(context.Context, ProcessCompletionNotification) error
}

// WorkflowActivityPublisher durably publishes bounded workflow lifecycle
// metadata for one owning Harness session. The neutral DTO keeps pkg/tool below
// the sealed event package in the dependency graph; the session runtime converts
// it into event.WorkflowActivity and validates it before the Hub/journal path.
type WorkflowActivityPublisher interface {
	PublishWorkflowActivity(context.Context, WorkflowActivityMetadata) error
}

// WorkflowActivityMetadata is the transport-neutral, bounded input to the
// trusted workflow publication seam. EventID is a stable source activity ID and
// must be preserved across retries; the session runtime uses the validated
// OccurredAt as the stable creation envelope when it stamps the sealed event.
type WorkflowActivityMetadata struct {
	EventID           uuid.UUID `json:"event_id,omitzero"`
	SessionID         uuid.UUID `json:"session_id,omitzero"`
	RunID             uuid.UUID `json:"run_id"`
	WorkflowName      string    `json:"workflow_name"`
	WorkflowVersion   string    `json:"workflow_version"`
	Kind              string    `json:"kind"`
	Status            string    `json:"status"`
	VertexID          uuid.UUID `json:"vertex_id,omitzero"`
	VertexLabel       string    `json:"vertex_label,omitempty"`
	CompletedVertices uint32    `json:"completed_vertices,omitzero"`
	TotalVertices     uint32    `json:"total_vertices,omitzero"`
	Message           string    `json:"message,omitempty"`
	OccurredAt        time.Time `json:"occurred_at"`
}

// SessionResourceServices is the immutable late-bound service set supplied to
// every session resource after the live session has been constructed. Its zero
// value is invalid; use NewSessionResourceServices.
type SessionResourceServices struct {
	processLifecyclePublisher ProcessLifecyclePublisher
	processCompletionNotifier ProcessCompletionNotifier
	workflowActivityPublisher WorkflowActivityPublisher
}

// SessionResourceServicesValidationError reports a missing or typed-nil
// late-bound session service.
type SessionResourceServicesValidationError struct {
	Field string
}

func (e *SessionResourceServicesValidationError) Error() string {
	return "tool: invalid session resource services: " + e.Field
}

// NewSessionResourceServices constructs an immutable, fully populated service
// set and rejects both nil interfaces and interfaces containing typed nils.
func NewSessionResourceServices(
	publisher ProcessLifecyclePublisher,
	notifier ProcessCompletionNotifier,
	workflowPublisher WorkflowActivityPublisher,
) (SessionResourceServices, error) {
	services := SessionResourceServices{
		processLifecyclePublisher: publisher,
		processCompletionNotifier: notifier,
		workflowActivityPublisher: workflowPublisher,
	}
	if err := services.Validate(); err != nil {
		return SessionResourceServices{}, err
	}
	return services, nil
}

// Validate reports whether the service set is safe to give to a session
// resource.
func (s SessionResourceServices) Validate() error {
	if s.processLifecyclePublisher == nil ||
		nilReflectValue(reflect.ValueOf(s.processLifecyclePublisher)) {
		return &SessionResourceServicesValidationError{Field: "process_lifecycle_publisher"}
	}
	if s.processCompletionNotifier == nil ||
		nilReflectValue(reflect.ValueOf(s.processCompletionNotifier)) {
		return &SessionResourceServicesValidationError{Field: "process_completion_notifier"}
	}
	if s.workflowActivityPublisher == nil ||
		nilReflectValue(reflect.ValueOf(s.workflowActivityPublisher)) {
		return &SessionResourceServicesValidationError{Field: "workflow_activity_publisher"}
	}
	return nil
}

// ProcessLifecyclePublisher returns the validated lifecycle publisher.
func (s SessionResourceServices) ProcessLifecyclePublisher() ProcessLifecyclePublisher {
	return s.processLifecyclePublisher
}

// ProcessCompletionNotifier returns the validated completion notifier.
func (s SessionResourceServices) ProcessCompletionNotifier() ProcessCompletionNotifier {
	return s.processCompletionNotifier
}

// WorkflowActivityPublisher returns the validated trusted workflow publisher.
func (s SessionResourceServices) WorkflowActivityPublisher() WorkflowActivityPublisher {
	return s.workflowActivityPublisher
}

// ProcessBinding contains the session-scoped capabilities supplied to process
// tool definitions.
type ProcessBinding struct {
	Registry SessionResourceRegistry
}
