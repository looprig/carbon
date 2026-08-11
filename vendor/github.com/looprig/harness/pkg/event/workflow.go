package event

import (
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/looprig/core/uuid"
)

// WorkflowActivityKind is the closed lifecycle vocabulary projected from a
// workflow run. It deliberately describes safe user-facing milestones rather
// than exposing Flow's internal checkpoint or state shape.
type WorkflowActivityKind string

const (
	WorkflowActivityRunStarted      WorkflowActivityKind = "run_started"
	WorkflowActivityVertexCompleted WorkflowActivityKind = "vertex_completed"
	WorkflowActivityRunInterrupted  WorkflowActivityKind = "run_interrupted"
	WorkflowActivityRunResumed      WorkflowActivityKind = "run_resumed"
	WorkflowActivityRunCompleted    WorkflowActivityKind = "run_completed"
	WorkflowActivityRunCancelled    WorkflowActivityKind = "run_cancelled"
	WorkflowActivityRunFailed       WorkflowActivityKind = "run_failed"

	// Kind-prefixed aliases keep the enum discoverable for callers that group
	// constants by the declared type name.
	WorkflowActivityKindRunStarted      = WorkflowActivityRunStarted
	WorkflowActivityKindVertexCompleted = WorkflowActivityVertexCompleted
	WorkflowActivityKindRunInterrupted  = WorkflowActivityRunInterrupted
	WorkflowActivityKindRunResumed      = WorkflowActivityRunResumed
	WorkflowActivityKindRunCompleted    = WorkflowActivityRunCompleted
	WorkflowActivityKindRunCancelled    = WorkflowActivityRunCancelled
	WorkflowActivityKindRunFailed       = WorkflowActivityRunFailed
)

// Valid reports whether k is one of the durable activity kinds.
func (k WorkflowActivityKind) Valid() bool {
	switch k {
	case WorkflowActivityRunStarted,
		WorkflowActivityVertexCompleted,
		WorkflowActivityRunInterrupted,
		WorkflowActivityRunResumed,
		WorkflowActivityRunCompleted,
		WorkflowActivityRunCancelled,
		WorkflowActivityRunFailed:
		return true
	default:
		return false
	}
}

// WorkflowRunStatus is the bounded status projection carried by an activity.
// The failed value is included for workflow-host failures that occur after a
// durable run exists; Flow itself may represent recoverable execution errors as
// an interrupted run.
type WorkflowRunStatus string

const (
	WorkflowRunStatusRunning     WorkflowRunStatus = "running"
	WorkflowRunStatusInterrupted WorkflowRunStatus = "interrupted"
	WorkflowRunStatusCompleted   WorkflowRunStatus = "completed"
	WorkflowRunStatusCancelled   WorkflowRunStatus = "cancelled"
	WorkflowRunStatusFailed      WorkflowRunStatus = "failed"
)

// Valid reports whether s is one of the durable workflow statuses.
func (s WorkflowRunStatus) Valid() bool {
	switch s {
	case WorkflowRunStatusRunning,
		WorkflowRunStatusInterrupted,
		WorkflowRunStatusCompleted,
		WorkflowRunStatusCancelled,
		WorkflowRunStatusFailed:
		return true
	default:
		return false
	}
}

// Bounds for fields originating in workflow definitions or execution state.
// They are byte limits at the durable boundary; UTF-8 validity is checked
// separately so a multi-byte rune can never be split by a producer.
const (
	MaxWorkflowNameBytes            = 64
	MaxWorkflowVersionBytes         = 64
	MaxWorkflowVertexLabelBytes     = 128
	MaxWorkflowActivityMessageBytes = 512
	MaxWorkflowActivityProgress     = 1_000_000
)

// WorkflowActivity is the public, durable session notification for one safe
// workflow transition. It carries identifiers and bounded display metadata only:
// no checkpoint state, policy text, model output, document fragment, or metadata
// map belongs in this event.
//
// The EventID is normally a deterministic source activity ID supplied through
// Factory.StampWorkflowActivity. That specialized path is the only factory path
// that preserves an externally derived identity; generic Factory.Stamp continues
// to mint fresh IDs for ordinary event construction.
type WorkflowActivity struct {
	enduring
	sessionScoped
	Header

	RunID             uuid.UUID            `json:"run_id"`
	WorkflowName      string               `json:"workflow_name"`
	WorkflowVersion   string               `json:"workflow_version"`
	Kind              WorkflowActivityKind `json:"kind"`
	Status            WorkflowRunStatus    `json:"status"`
	VertexID          uuid.UUID            `json:"vertex_id,omitzero"`
	VertexLabel       string               `json:"vertex_label,omitempty"`
	CompletedVertices uint32               `json:"completed_vertices,omitzero"`
	TotalVertices     uint32               `json:"total_vertices,omitzero"`
	Message           string               `json:"message,omitempty"`
	OccurredAt        time.Time            `json:"occurred_at"`
}

func (WorkflowActivity) isEvent() {}

func validateWorkflowActivity(e WorkflowActivity) error {
	const name EventName = "WorkflowActivity"
	if e.Visibility() != Public {
		return &InvalidEventError{Event: name, Field: FieldVisibility, Rule: RuleInvalid}
	}
	if e.RunID.IsZero() {
		return &InvalidEventError{Event: name, Field: FieldRunID, Rule: RuleRequired}
	}
	if !validWorkflowDefinitionName(e.WorkflowName) {
		rule := RuleInvalid
		if e.WorkflowName == "" {
			rule = RuleRequired
		}
		return &InvalidEventError{Event: name, Field: FieldWorkflowName, Rule: rule}
	}
	if !validWorkflowVersion(e.WorkflowVersion) {
		rule := RuleInvalid
		if e.WorkflowVersion == "" {
			rule = RuleRequired
		}
		return &InvalidEventError{Event: name, Field: FieldWorkflowVersion, Rule: rule}
	}
	if !e.Kind.Valid() {
		return &InvalidEventError{Event: name, Field: FieldActivityKind, Rule: RuleInvalid}
	}
	if !e.Status.Valid() {
		return &InvalidEventError{Event: name, Field: FieldStatus, Rule: RuleInvalid}
	}
	if e.VertexLabel != "" && e.VertexID.IsZero() {
		return &InvalidEventError{Event: name, Field: FieldVertexID, Rule: RuleInvalid}
	}
	if e.VertexLabel != "" && !validWorkflowText(e.VertexLabel, MaxWorkflowVertexLabelBytes, true) {
		return &InvalidEventError{Event: name, Field: FieldVertexLabel, Rule: RuleInvalid}
	}
	if e.TotalVertices > MaxWorkflowActivityProgress || e.CompletedVertices > MaxWorkflowActivityProgress || e.CompletedVertices > e.TotalVertices {
		return &InvalidEventError{Event: name, Field: FieldProgress, Rule: RuleInvalid}
	}
	if !validWorkflowText(e.Message, MaxWorkflowActivityMessageBytes, true) {
		return &InvalidEventError{Event: name, Field: FieldMessage, Rule: RuleInvalid}
	}
	if e.OccurredAt.IsZero() {
		return &InvalidEventError{Event: name, Field: FieldOccurredAt, Rule: RuleRequired}
	}
	return nil
}

func validWorkflowDefinitionName(value string) bool {
	if len(value) < 3 || len(value) > MaxWorkflowNameBytes || !utf8.ValidString(value) {
		return false
	}
	for i, r := range value {
		if i == 0 {
			if r < 'a' || r > 'z' {
				return false
			}
			continue
		}
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
}

func validWorkflowVersion(value string) bool {
	if value == "" || len(value) > MaxWorkflowVersionBytes || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.IsSpace(r) || r == '/' || r == '\\' || r == ':' {
			return false
		}
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune("._-+", r) {
			return false
		}
	}
	return true
}

func validWorkflowText(value string, maxBytes int, allowSpaces bool) bool {
	if value == "" {
		return true
	}
	if len(value) > maxBytes || !utf8.ValidString(value) || strings.TrimSpace(value) == "" {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) || (!allowSpaces && unicode.IsSpace(r)) {
			return false
		}
	}
	return true
}
