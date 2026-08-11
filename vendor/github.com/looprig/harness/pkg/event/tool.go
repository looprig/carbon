package event

import (
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/tool"
)

// PermissionDecisionEffect is the durable approve/deny outcome for a non-gated
// permission decision. Ask is intentionally absent: gated asks are represented by
// GateOpened/GateResolved, not PermissionDecided.
type PermissionDecisionEffect string

const (
	PermissionEffectApprove PermissionDecisionEffect = "approve"
	PermissionEffectDeny    PermissionDecisionEffect = "deny"
)

// PermissionRequested is emitted when a tool call needs interactive approval.
// The per-turn stream (TUI) renders the typed prepared Request — the summary
// plus the displayed unmet requirement and candidate descriptions. The wire
// carries the same typed request through the strict request decoder; it never
// carries grant tokens or raw tool arguments (tool.Request has neither).
type PermissionRequested struct {
	enduring
	loopScoped
	Header
	ToolExecutionID uuid.UUID `json:"tool_execution_id,omitzero"`
	// Request is validated at the durable boundary (tool.ValidateRequest on
	// marshal, the strict gate.DecodeRequest on unmarshal), so a malformed or
	// token-bearing record can neither be journaled nor restored. It is
	// projected by the marshaler rather than serialized directly.
	Request tool.Request `json:"-"`
}

// PermissionDecided is emitted for a non-gated permission decision. Subject and
// Audit are redacted summaries; grant tokens and raw args must never appear here.
type PermissionDecided struct {
	enduring
	loopScoped
	Header
	ToolExecutionID uuid.UUID                `json:"tool_execution_id,omitzero"`
	Effect          PermissionDecisionEffect `json:"effect,omitempty"`
	Reason          string                   `json:"reason,omitempty"`
	Subject         string                   `json:"subject,omitempty"`
	Audit           string                   `json:"audit,omitempty"`
}

// UserInputRequested is emitted when a tool (AskUser) needs free-form input. The
// per-turn stream gets the full Question and Choices for rendering.
type UserInputRequested struct {
	enduring
	loopScoped
	Header
	ToolExecutionID uuid.UUID `json:"tool_execution_id,omitzero"`
	Question        string    `json:"question,omitempty"`
	Choices         []string  `json:"choices,omitempty"`
}

// ToolCallStarted is emitted when an approved tool begins executing. Summary is
// capped at construction (never raw args).
type ToolCallStarted struct {
	ephemeral
	loopScoped
	Header
	ToolExecutionID uuid.UUID `json:"tool_execution_id,omitzero"`
	ToolName        string    `json:"tool_name,omitempty"`
	Summary         string    `json:"summary,omitempty"`
}

// ToolCallCompleted is emitted when a tool finishes. ResultPreview is the capped
// tool output for the TUI.
type ToolCallCompleted struct {
	ephemeral
	loopScoped
	Header
	ToolExecutionID uuid.UUID `json:"tool_execution_id,omitzero"`
	IsError         bool      `json:"is_error,omitzero"`
	ResultPreview   string    `json:"result_preview,omitempty"`
}

func (PermissionRequested) isEvent() {}
func (PermissionDecided) isEvent()   {}
func (UserInputRequested) isEvent()  {}
func (ToolCallStarted) isEvent()     {}
func (ToolCallCompleted) isEvent()   {}
