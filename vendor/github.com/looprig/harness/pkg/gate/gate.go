// Package gate defines the durable domain envelope for human and policy gates
// and the generic three-state access evaluator.
//
// The evaluator routes each normalized requirement kind to exactly one
// structural AccessSource, applies deny-before-allow precedence across
// configured access and stored rules, and combines every unmet gated
// capability into one approval carrying exactly the Approve, Approve always
// for this workspace, and Deny actions.
//
// Boundaries are deliberate: this package never imports an enforcement
// package such as sandbox (access, rule, and grant seams are structural,
// built-in-typed interfaces), never parses raw tool arguments (tools prepare
// typed requests; see pkg/tool), and never defines a permission-file format
// (durable rule matching and persistence are consumer-provided).
package gate

import "github.com/looprig/core/uuid"

// ID is the shared gate identifier type. It aliases uuid.UUID so existing text
// and JSON codecs are preserved exactly.
type ID = uuid.UUID

// Kind identifies the user-facing gate scenario.
type Kind string

// ResolverKind identifies the owner responsible for resolving a gate response.
type ResolverKind string

// Blocks names the execution scope held while a gate is open.
type Blocks string

// Effect identifies what resolving the gate does to execution.
type Effect string

// CloseReason records why an open gate was closed.
type CloseReason string

// Criticality classifies whether a gate must survive restore boundaries.
type Criticality string

const (
	// KindPermission is a tool permission approval gate.
	KindPermission Kind = "harness.permission"
	// KindAskUser is an explicit user-question gate.
	KindAskUser Kind = "harness.ask_user"
	// KindForm is a structured human-input gate: a bounded set of typed fields
	// (including a confirmation-only field) answered by a human or by policy. It
	// is protocol-neutral — any integration that needs structured human input
	// opens one.
	KindForm Kind = "harness.form"
	// KindOpenURL is a gate asking a human to open an action URL out-of-band
	// (typically a browser) and, when RequiresCompletion is set, to report back.
	//
	// An open-url gate is inherently EPHEMERAL: its action target is bound to a
	// live out-of-band exchange and is deliberately never journaled (see
	// OpenURLPayload), so it can never be restored. ValidateGate rejects an
	// open-url gate marked Restorable.
	KindOpenURL Kind = "harness.open_url"
)

const (
	// ResolverLoop routes responses to a loop-local gate resolver.
	ResolverLoop ResolverKind = "loop"
	// ResolverSession routes responses through the session gate resolver.
	ResolverSession ResolverKind = "session"
)

const (
	// BlocksToolCall means the gate blocks a single tool call.
	BlocksToolCall Blocks = "tool_call"
	// BlocksSession means the gate blocks session progress.
	BlocksSession Blocks = "session"
)

const (
	// EffectResume resumes work that was parked on the gate.
	EffectResume Effect = "resume"
	// EffectInitiate starts follow-up work from the gate response.
	EffectInitiate Effect = "initiate"
	// EffectControl applies a control-plane response.
	EffectControl Effect = "control"
)

const (
	// CloseAnswered records a direct response.
	CloseAnswered CloseReason = "answered"
	// ClosePolicyResponse records an automatic policy response.
	ClosePolicyResponse CloseReason = "policy_response"
	// CloseAbandoned records that the gate was left unresolved.
	CloseAbandoned CloseReason = "abandoned"
	// CloseOwnerClosed records that the owning resolver closed the gate.
	CloseOwnerClosed CloseReason = "owner_closed"
	// CloseRestoreUnavailable records that a restorable gate could not be restored.
	CloseRestoreUnavailable CloseReason = "restore_unavailable"
)

const (
	// GateCritical marks a gate that must be restored or resolved explicitly.
	GateCritical Criticality = "critical"
	// GateNonCritical marks a gate that may be abandoned across restore.
	GateNonCritical Criticality = "non_critical"
)

// Subject identifies the work item a gate is about.
type Subject struct {
	ToolExecutionID ID     `json:"tool_execution_id,omitzero"`
	ToolUseID       string `json:"tool_use_id,omitempty"`
	TurnID          ID     `json:"turn_id,omitzero"`
	StepID          ID     `json:"step_id,omitzero"`
	InputID         ID     `json:"input_id,omitzero"`
}

// Route carries the identifiers needed to deliver a response to a gate.
type Route struct {
	GateID          ID `json:"gate_id,omitzero"`
	LoopID          ID `json:"loop_id,omitzero"`
	ToolExecutionID ID `json:"tool_execution_id,omitzero"`
}

// Gate is the durable envelope for an open human or policy-resolved gate.
type Gate struct {
	ID             ID             `json:"id,omitzero"`
	Kind           Kind           `json:"kind,omitempty"`
	Resolver       ResolverKind   `json:"resolver,omitempty"`
	Blocks         Blocks         `json:"blocks,omitempty"`
	Effect         Effect         `json:"effect,omitempty"`
	Criticality    Criticality    `json:"criticality,omitempty"`
	Subject        Subject        `json:"subject,omitzero"`
	Prompt         Prompt         `json:"prompt,omitzero"`
	ResponsePolicy ResponsePolicy `json:"response_policy,omitzero"`
	Restorable     bool           `json:"restorable,omitzero"`
}
