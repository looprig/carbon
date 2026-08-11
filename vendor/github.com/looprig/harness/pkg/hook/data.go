package hook

import (
	"encoding/json"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
	"github.com/looprig/inference/stream"
)

// Call is the immutable typed snapshot supplied when an operation begins.
// Exactly one operation-specific payload must be non-nil and match Operation.
type Call struct {
	// Operation selects the one matching operation-specific payload below.
	Operation Operation
	// StartedAt is the runtime-owned operation start time.
	StartedAt time.Time
	// Coordinates locate the operation in the session/loop/turn/step hierarchy.
	Coordinates identity.Coordinates
	// AgentName is the immutable attribution name of the executing loop.
	AgentName identity.AgentName
	// Cause is the direct causal edge that initiated the operation.
	Cause identity.Cause

	// Exactly one pointer below is non-nil and matches Operation.
	Turn          *TurnData
	Step          *StepData
	Inference     *InferenceData
	Compaction    *CompactionData
	ToolCall      *ToolCallData
	GateWait      *GateWaitData
	ToolExecution *ToolExecutionData
	JournalAppend *JournalAppendData
}

// Result is the terminal snapshot supplied to an around hook.
type Result struct {
	Call
	// EndedAt is the runtime-owned operation completion time.
	EndedAt time.Time
	// Outcome is the bounded terminal classification.
	Outcome Outcome
	// Err is the original trusted in-process terminal error and is not
	// deep-cloned. Consumers must redact or classify it before exporting it to
	// logs, telemetry, or another trust boundary.
	Err error
}

// TurnData describes one bounded turn.
type TurnData struct {
	// Index is the turn's loop-local index.
	Index event.TurnIndex
	// Input is the user message that initiated the turn, when available.
	Input *content.UserMessage
}

// StepData describes one bounded inference/tool step within a turn.
type StepData struct {
	// Index is the step's zero-based turn-local index.
	Index StepIndex
}

// InferenceData carries the provider-neutral request and terminal model output.
// Terminal fields are nil until their corresponding values exist.
type InferenceData struct {
	// Request is the provider-neutral request submitted to inference.
	Request *inference.Request
	// AIMessage is the completed assistant message, when produced.
	AIMessage *content.AIMessage
	// StreamResult is authoritative terminal provider metadata, when produced.
	StreamResult *stream.StreamResult
}

// CompactionData carries one transcript-compaction attempt and its optional
// terminal summary.
type CompactionData struct {
	// AttemptID correlates the operation with compaction lifecycle events.
	AttemptID event.CompactAttemptID
	// Input is the exact transcript and context identity being compacted.
	Input *loop.CompactionInput
	// Output is the validated summary, when compaction succeeds.
	Output *loop.CompactionOutput
}

// ToolCallData describes the semantic tool-call operation, including permission
// resolution and its normalized terminal result.
type ToolCallData struct {
	// ToolExecutionID is the runtime-minted identity for this attempted call.
	ToolExecutionID uuid.UUID
	// ToolUseID is the model-supplied call identity.
	ToolUseID string
	// ToolName is the normalized invoked tool name.
	ToolName string
	// Summary is the bounded, redacted call summary.
	Summary string
	// ArgsJSON is the raw model-supplied argument object.
	ArgsJSON json.RawMessage
	// PermissionEffect is the terminal approve/deny decision, when known.
	PermissionEffect event.PermissionDecisionEffect
	// PermissionReason is the bounded decision reason.
	PermissionReason string
	// Result is the normalized terminal tool result, including pre-execution
	// failures, when the semantic call has completed.
	Result *tool.ToolResult
	// ResultPreview is the bounded terminal tool-output preview.
	ResultPreview string
	// IsError reports whether the semantic call ended in an error.
	IsError bool
}

// GateWaitData describes the time spent waiting for one gate resolution.
type GateWaitData struct {
	// GateID identifies the gate being awaited.
	GateID gate.ID
	// Kind identifies the user-facing gate scenario.
	Kind gate.Kind
	// Resolver identifies the component responsible for resolving the gate.
	Resolver gate.ResolverKind
	// Blocks identifies the execution scope held by the gate.
	Blocks gate.Blocks
	// Effect identifies what resolution does to execution.
	Effect gate.Effect
	// Answer is the validated live answer, when one was delivered.
	Answer *gate.Answer
}

// ToolExecutionData describes only the approved tool execution boundary.
type ToolExecutionData struct {
	// ToolExecutionID is the runtime-minted identity for this execution.
	ToolExecutionID uuid.UUID
	// ToolUseID is the model-supplied call identity.
	ToolUseID string
	// ToolName is the normalized invoked tool name.
	ToolName string
	// ArgsJSON is the raw model-supplied argument object.
	ArgsJSON json.RawMessage
	// Result is the tool's terminal content, when produced.
	Result *tool.ToolResult
	// ResultPreview is the bounded terminal output preview.
	ResultPreview string
	// IsError reports whether execution ended in an error.
	IsError bool
}

// JournalAppendData describes one bounded durable append without exposing
// serialized record bytes.
type JournalAppendData struct {
	// Family is the closed record family.
	Family RecordFamily
	// RecordID is the record's bounded textual identity.
	RecordID string
}

// ValidateCall validates the closed operation-payload union.
func ValidateCall(call Call) error {
	if !call.Operation.Valid() {
		return &CallError{Kind: CallUnknownOperation, Operation: call.Operation}
	}

	payloads := 0
	payloads += boolInt(call.Turn != nil)
	payloads += boolInt(call.Step != nil)
	payloads += boolInt(call.Inference != nil)
	payloads += boolInt(call.Compaction != nil)
	payloads += boolInt(call.ToolCall != nil)
	payloads += boolInt(call.GateWait != nil)
	payloads += boolInt(call.ToolExecution != nil)
	payloads += boolInt(call.JournalAppend != nil)
	if payloads != 1 || !call.payloadMatchesOperation() {
		return &CallError{Kind: CallInvalidPayload, Operation: call.Operation}
	}
	return nil
}

func (call Call) payloadMatchesOperation() bool {
	switch call.Operation {
	case OperationTurn:
		return call.Turn != nil
	case OperationStep:
		return call.Step != nil
	case OperationInference:
		return call.Inference != nil
	case OperationCompaction:
		return call.Compaction != nil
	case OperationToolCall:
		return call.ToolCall != nil
	case OperationGateWait:
		return call.GateWait != nil
	case OperationToolExecution:
		return call.ToolExecution != nil
	case OperationJournalAppend:
		return call.JournalAppend != nil
	default:
		return false
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
