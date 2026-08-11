package hustle

import (
	"encoding/json"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/identity"
)

// RunID identifies one hustle invocation.
type RunID uuid.UUID

// Stage identifies the bounded execution phase in which a hustle failed.
type Stage uint8

const (
	StageUnknown Stage = iota
	StageQueue
	StageModelResolution
	StageInference
	StageOutput
	StageTerminal
	StageFinalization
)

// Valid reports whether the stage is a durable, recognized failure phase.
func (s Stage) Valid() bool { return s >= StageQueue && s <= StageFinalization }

// ReasonCode is the bounded, security-safe classification of a hustle failure.
type ReasonCode uint8

const (
	ReasonUnknown ReasonCode = iota
	ReasonRejected
	ReasonCanceled
	ReasonTimeout
	ReasonModelResolution
	ReasonInference
	ReasonInvalidOutput
	ReasonTerminal
	ReasonFinalization
	ReasonInternal
)

// Valid reports whether the reason is recognized for durable audit.
func (r ReasonCode) Valid() bool { return r >= ReasonRejected && r <= ReasonInternal }

// TerminalStatus is the bounded outcome dimension used by durable usage
// aggregates. Interrupted attempts are deliberately not terminal.
type TerminalStatus uint8

const (
	TerminalStatusUnknown TerminalStatus = iota
	TerminalStatusCompleted
	TerminalStatusFailed
)

// Valid reports whether the value is a durable terminal outcome.
func (s TerminalStatus) Valid() bool {
	return s == TerminalStatusCompleted || s == TerminalStatusFailed
}

// RetryPolicy selects the immutable, bounded retry behavior of one definition.
// The zero value preserves the historical single-attempt behavior.
type RetryPolicy uint8

const (
	RetryPolicyNone RetryPolicy = iota
	// RetryPolicyClassifiedOnce permits one clean restart after a closed set of
	// transient inference or recoverable terminal-parse failures.
	RetryPolicyClassifiedOnce
)

// Valid reports whether the policy is a recognized immutable behavior.
func (p RetryPolicy) Valid() bool {
	return p == RetryPolicyNone || p == RetryPolicyClassifiedOnce
}

const recoverableTerminalValidationMessage = "hustle: recoverable malformed terminal output"

// recoverableTerminalValidationError is intentionally private so consumers
// cannot attach arbitrary causes or provider output to the retry marker.
type recoverableTerminalValidationError struct{}

func (*recoverableTerminalValidationError) Error() string {
	return recoverableTerminalValidationMessage
}

// NewRecoverableTerminalValidationError returns the sealed marker a strict
// classifier adapter may return when terminal decoding or wire-shape
// validation is malformed but safe to retry. It must not be used for domain
// decisions, basis mismatches, unsafe results, or operational failures.
func NewRecoverableTerminalValidationError() error {
	return &recoverableTerminalValidationError{}
}

// IsRecoverableTerminalValidationError reports whether err contains the
// package-owned malformed-terminal marker. Matching is typed and never
// inspects error text.
func IsRecoverableTerminalValidationError(err error) bool {
	_, ok := err.(*recoverableTerminalValidationError)
	return ok
}

// ReasonAllowed reports whether reason is a valid durable classification for
// stage. The closed matrix prevents impossible stage/reason audit records.
func ReasonAllowed(stage Stage, reason ReasonCode) bool {
	switch stage {
	case StageQueue:
		return reason == ReasonRejected || reason == ReasonCanceled || reason == ReasonTimeout || reason == ReasonInternal
	case StageModelResolution:
		return reason == ReasonCanceled || reason == ReasonTimeout || reason == ReasonModelResolution || reason == ReasonInternal
	case StageInference:
		return reason == ReasonCanceled || reason == ReasonTimeout || reason == ReasonInference || reason == ReasonInternal
	case StageOutput:
		return reason == ReasonCanceled || reason == ReasonTimeout || reason == ReasonInvalidOutput || reason == ReasonInternal
	case StageTerminal:
		return reason == ReasonTimeout || reason == ReasonTerminal || reason == ReasonInternal
	case StageFinalization:
		return reason == ReasonTimeout || reason == ReasonFinalization || reason == ReasonInternal
	default:
		return false
	}
}

// Request is the shared runtime's data-only serialization envelope.
type Request struct {
	Name  Name
	Cause identity.Cause
	Input json.RawMessage
	// SecurityCeiling is the per-invocation evidence-tool containment ceiling
	// THIS SPECIFIC run's evidence catalog must be bound against (design
	// §13.1, §21). Empty means no per-request override: a Hustle without an
	// evidence-tool concept (e.g. compaction) never sets it, and never reaches
	// the evidence-binding path that would consume it. A permission-review
	// Hustle always sets it from that review's own frozen basis
	// (gate.ReviewBasis.SecurityCeiling, captured once at StartPermissionReview
	// — see internal/sessionruntime/gates.go's respondFromClassifier doc
	// comment), never a session-wide constant, so a long session's later
	// review is bound against ITS OWN current ceiling rather than one frozen
	// at controller construction.
	SecurityCeiling string
}

// Result is the validated serialized output and normalized usage.
type Result struct {
	Output json.RawMessage
	Usage  *content.Usage
}

// Outcome carries exactly one terminal result or error.
type Outcome struct {
	Result *Result
	Err    error
}
