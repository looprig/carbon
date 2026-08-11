package hustleruntime

import (
	"github.com/looprig/harness/pkg/hustle"
)

// ConfigErrorReason identifies an invalid controller construction boundary.
type ConfigErrorReason string

const (
	ConfigInvalidContext      ConfigErrorReason = "invalid_context"
	ConfigInvalidConcurrent   ConfigErrorReason = "invalid_concurrent"
	ConfigInvalidQueued       ConfigErrorReason = "invalid_queued"
	ConfigCapacityOverflow    ConfigErrorReason = "capacity_overflow"
	ConfigInvalidSessionID    ConfigErrorReason = "invalid_session_id"
	ConfigInvalidDefinitions  ConfigErrorReason = "invalid_definitions"
	ConfigInvalidTimeout      ConfigErrorReason = "invalid_timeout"
	ConfigMissingCollaborator ConfigErrorReason = "missing_collaborator"
)

// ConfigError reports invalid scheduler limits without retaining collaborators.
type ConfigError struct {
	Reason ConfigErrorReason
	Field  string
}

func (e *ConfigError) Error() string {
	message := "hustleruntime: invalid controller configuration: " + string(e.Reason)
	if e.Field != "" {
		message += " (" + e.Field + ")"
	}
	return message
}

// ConfigEvidenceKindError reports that a registered hustle definition's
// evidence tools may declare a Requirement.Kind the consumer's
// runtime.evidence.AllowedKinds allowlist does not cover. It is raised at
// controller CONSTRUCTION (newRuntimeController), not on the definition's
// first real evidence-tool call, so a missing kind names itself immediately
// rather than surfacing later as an opaque EvidenceFailureForbiddenCapability
// deep inside a live classifier review.
type ConfigEvidenceKindError struct {
	Name hustle.Name
	Kind string
}

func (e *ConfigEvidenceKindError) Error() string {
	return "hustleruntime: hustle " + string(e.Name) + " evidence tools declare kind " +
		e.Kind + " not present in runtime.evidence.allowed_kinds"
}

// AdmissionErrorReason identifies a rejection before lane ownership commits.
type AdmissionErrorReason string

const (
	AdmissionInvalidContext       AdmissionErrorReason = "invalid_context"
	AdmissionInvalidParticipation AdmissionErrorReason = "invalid_participation"
	AdmissionNilFinalizer         AdmissionErrorReason = "nil_finalizer"
	AdmissionRunID                AdmissionErrorReason = "run_id"
	AdmissionFull                 AdmissionErrorReason = "full"
	AdmissionClosed               AdmissionErrorReason = "closed"
	AdmissionPoisoned             AdmissionErrorReason = "poisoned"
)

// AdmissionError has no RunID because admission failures are pre-ownership.
type AdmissionError struct {
	Reason        AdmissionErrorReason
	Participation hustle.Participation
	Cause         error
}

func (e *AdmissionError) Error() string {
	return "hustleruntime: admission rejected: " + string(e.Reason)
}

func (e *AdmissionError) Unwrap() error { return e.Cause }

// RequestErrorReason identifies a pre-ownership runtime request rejection.
type RequestErrorReason string

const (
	RequestInvalidContext     RequestErrorReason = "invalid_context"
	RequestRuntimeUnavailable RequestErrorReason = "runtime_unavailable"
	RequestUnknownDefinition  RequestErrorReason = "unknown_definition"
	RequestInvalidCause       RequestErrorReason = "invalid_cause"
	RequestInvalidInput       RequestErrorReason = "invalid_input"
	RequestInputTooLarge      RequestErrorReason = "input_too_large"
	RequestNilValidator       RequestErrorReason = "nil_validator"
)

// RequestError reports a rejection before a run owns capacity or a RunID.
type RequestError struct {
	Reason RequestErrorReason
	Name   hustle.Name
	Cause  error
}

func (e *RequestError) Error() string { return "hustleruntime: request rejected: " + string(e.Reason) }

func (e *RequestError) Unwrap() error { return e.Cause }

// QueueFailureReason classifies a terminal failure while an owned node waits for
// an execution slot.
type QueueFailureReason string

const (
	QueueFailureCanceled QueueFailureReason = "canceled"
	QueueFailureTimeout  QueueFailureReason = "timeout"
	QueueFailureClosed   QueueFailureReason = "closed"
	QueueFailurePoisoned QueueFailureReason = "poisoned"
)

// QueueFailureError is both the returned owned-run failure and the exact error
// supplied to its finalizer.
type QueueFailureError struct {
	RunID         hustle.RunID
	Participation hustle.Participation
	Stage         hustle.Stage
	Reason        QueueFailureReason
	Cause         error
	FinalizerErr  *FinalizerError
	TerminalErr   error
	CleanupErr    error
}

func (e *QueueFailureError) Error() string {
	return "hustleruntime: owned run failed in queue: " + string(e.Reason)
}

func (e *QueueFailureError) Unwrap() []error {
	errors := make([]error, 0, 4)
	if e.Cause != nil {
		errors = append(errors, e.Cause)
	}
	if e.FinalizerErr != nil {
		errors = append(errors, e.FinalizerErr)
	}
	if e.TerminalErr != nil {
		errors = append(errors, e.TerminalErr)
	}
	if e.CleanupErr != nil {
		errors = append(errors, e.CleanupErr)
	}
	return errors
}

// RunError is the primary typed failure of an owned run after admission.
type RunError struct {
	Name         hustle.Name
	RunID        hustle.RunID
	Stage        hustle.Stage
	ReasonCode   hustle.ReasonCode
	Cause        error
	TerminalErr  error
	FinalizerErr *FinalizerError
	CleanupErr   error
}

func (e *RunError) Error() string { return "hustleruntime: owned run failed" }

func (e *RunError) Unwrap() []error {
	errors := make([]error, 0, 4)
	if e.Cause != nil {
		errors = append(errors, e.Cause)
	}
	if e.TerminalErr != nil {
		errors = append(errors, e.TerminalErr)
	}
	if e.FinalizerErr != nil {
		errors = append(errors, e.FinalizerErr)
	}
	if e.CleanupErr != nil {
		errors = append(errors, e.CleanupErr)
	}
	return errors
}

// OutputFailureReason is the closed, security-safe generic extraction failure.
type OutputFailureReason string

const (
	OutputFailureInvalidShape OutputFailureReason = "invalid_shape"
	OutputFailureEmptyText    OutputFailureReason = "empty_text"
	OutputFailureTooLarge     OutputFailureReason = "too_large"
	OutputFailureInvalidJSON  OutputFailureReason = "invalid_json"
)

// Valid reports whether the reason is a recognized extraction failure.
func (r OutputFailureReason) Valid() bool {
	return r == OutputFailureInvalidShape || r == OutputFailureEmptyText ||
		r == OutputFailureTooLarge || r == OutputFailureInvalidJSON
}

// OutputError reports an invalid provider response or consumer validation
// result without retaining response content. Reason is populated for generic
// extraction failures; callback failures retain their typed Cause instead.
type OutputError struct {
	Reason OutputFailureReason
	Cause  error
}

// Valid reports whether exactly one bounded failure classification or wrapped
// validation cause is present.
func (e *OutputError) Valid() bool {
	if e == nil {
		return false
	}
	return (e.Reason.Valid() && e.Cause == nil) || (e.Reason == "" && e.Cause != nil)
}

func (e *OutputError) Error() string {
	if e.Reason.Valid() {
		return "hustleruntime: invalid hustle output (" + string(e.Reason) + ")"
	}
	return "hustleruntime: invalid hustle output"
}

func (e *OutputError) Unwrap() error { return e.Cause }

// ToolResponseFailureReason is a closed, security-safe classification for an
// invalid structured-output-with-tools response. Values describe only the
// response shape; they never retain provider-controlled content.
type ToolResponseFailureReason string

const (
	ToolResponseFailureInvalidShape       ToolResponseFailureReason = "invalid_shape"
	ToolResponseFailureFinishReason       ToolResponseFailureReason = "finish_reason"
	ToolResponseFailureUnknownTool        ToolResponseFailureReason = "unknown_tool"
	ToolResponseFailureMalformedArguments ToolResponseFailureReason = "malformed_arguments"
	ToolResponseFailureMissingCallID      ToolResponseFailureReason = "missing_call_id"
	ToolResponseFailureDuplicateCallID    ToolResponseFailureReason = "duplicate_call_id"
	ToolResponseFailureMixed              ToolResponseFailureReason = "mixed_response"
	ToolResponseFailureDuplicateTerminal  ToolResponseFailureReason = "duplicate_terminal"
	ToolResponseFailureInvalidTerminal    ToolResponseFailureReason = "invalid_terminal"
	ToolResponseFailureTooLarge           ToolResponseFailureReason = "too_large"
)

// Valid reports whether the reason is a recognized response-shape failure.
func (r ToolResponseFailureReason) Valid() bool {
	switch r {
	case ToolResponseFailureInvalidShape,
		ToolResponseFailureFinishReason,
		ToolResponseFailureUnknownTool,
		ToolResponseFailureMalformedArguments,
		ToolResponseFailureMissingCallID,
		ToolResponseFailureDuplicateCallID,
		ToolResponseFailureMixed,
		ToolResponseFailureDuplicateTerminal,
		ToolResponseFailureInvalidTerminal,
		ToolResponseFailureTooLarge:
		return true
	default:
		return false
	}
}

// ToolResponseError reports one bounded response-shape classification. It
// deliberately has no cause or caller-controlled metadata.
type ToolResponseError struct {
	Reason ToolResponseFailureReason
}

// Valid reports whether the error contains exactly one recognized reason.
func (e *ToolResponseError) Valid() bool {
	return e != nil && e.Reason.Valid()
}

func (e *ToolResponseError) Error() string {
	if e.Valid() {
		return "hustleruntime: invalid tool response (" + string(e.Reason) + ")"
	}
	return "hustleruntime: invalid tool response"
}

// EvidenceFailureReason is a closed, security-safe classification for evidence
// binding, preparation, authorization, and execution failures.
type EvidenceFailureReason string

const (
	EvidenceFailureInvalidBinding        EvidenceFailureReason = "invalid_binding"
	EvidenceFailureUnknownTool           EvidenceFailureReason = "unknown_tool"
	EvidenceFailureUnprepared            EvidenceFailureReason = "unprepared"
	EvidenceFailurePreparation           EvidenceFailureReason = "preparation"
	EvidenceFailureInvalidRequest        EvidenceFailureReason = "invalid_request"
	EvidenceFailureAmbiguousIdentity     EvidenceFailureReason = "ambiguous_identity"
	EvidenceFailureForbiddenCapability   EvidenceFailureReason = "forbidden_capability"
	EvidenceFailureContainmentRefused    EvidenceFailureReason = "containment_refused"
	EvidenceFailureAccessRefused         EvidenceFailureReason = "access_refused"
	EvidenceFailureExecution             EvidenceFailureReason = "execution"
	EvidenceFailureInvalidResult         EvidenceFailureReason = "invalid_result"
	EvidenceFailureResultTooLarge        EvidenceFailureReason = "result_too_large"
	EvidenceFailureEvidenceTooLarge      EvidenceFailureReason = "evidence_too_large"
	EvidenceFailureRoundsExceeded        EvidenceFailureReason = "rounds_exceeded"
	EvidenceFailureCallsExceeded         EvidenceFailureReason = "calls_exceeded"
	EvidenceFailureCallsPerRoundExceeded EvidenceFailureReason = "calls_per_round_exceeded"
	EvidenceFailureCanceled              EvidenceFailureReason = "canceled"
	EvidenceFailureDeadline              EvidenceFailureReason = "deadline"
	EvidenceFailureInternal              EvidenceFailureReason = "internal"
)

// Valid reports whether the reason is a recognized evidence failure.
func (r EvidenceFailureReason) Valid() bool {
	switch r {
	case EvidenceFailureInvalidBinding,
		EvidenceFailureUnknownTool,
		EvidenceFailureUnprepared,
		EvidenceFailurePreparation,
		EvidenceFailureInvalidRequest,
		EvidenceFailureAmbiguousIdentity,
		EvidenceFailureForbiddenCapability,
		EvidenceFailureContainmentRefused,
		EvidenceFailureAccessRefused,
		EvidenceFailureExecution,
		EvidenceFailureInvalidResult,
		EvidenceFailureResultTooLarge,
		EvidenceFailureEvidenceTooLarge,
		EvidenceFailureRoundsExceeded,
		EvidenceFailureCallsExceeded,
		EvidenceFailureCallsPerRoundExceeded,
		EvidenceFailureCanceled,
		EvidenceFailureDeadline,
		EvidenceFailureInternal:
		return true
	default:
		return false
	}
}

// EvidenceError deliberately retains no tool arguments, results, dependency
// errors, or panic values.
type EvidenceError struct {
	Reason EvidenceFailureReason
}

func (e *EvidenceError) Valid() bool { return e != nil && e.Reason.Valid() }

func (e *EvidenceError) Error() string {
	if e.Valid() {
		return "hustleruntime: evidence call failed (" + string(e.Reason) + ")"
	}
	return "hustleruntime: evidence call failed"
}

// CallbackPanicError is the redacted recovery product for a consumer callback.
// It deliberately retains no panic value.
type CallbackPanicError struct {
	Stage hustle.Stage
}

func (e *CallbackPanicError) Error() string { return "hustleruntime: consumer callback panicked" }

// WorkerPoisonError reports that an inference worker ignored cancellation long
// enough to disable both lanes. It retains no provider response or request.
type WorkerPoisonError struct {
	RunID hustle.RunID
	Cause error
}

func (e *WorkerPoisonError) Error() string {
	return "hustleruntime: inference worker poisoned controller"
}

func (e *WorkerPoisonError) Unwrap() error { return e.Cause }

// WorkerPanicError is the redacted recovery product for an inference client
// panic. It deliberately retains no panic value.
type WorkerPanicError struct {
	RunID hustle.RunID
}

func (e *WorkerPanicError) Error() string { return "hustleruntime: inference client panicked" }

// EvidenceWorkerPanicError is the redacted recovery product for an unexpected
// panic anywhere inside evidence catalog binding or evidence execution.
type EvidenceWorkerPanicError struct {
	RunID hustle.RunID
}

func (e *EvidenceWorkerPanicError) Error() string {
	return "hustleruntime: evidence worker panicked"
}

// AuditOperation identifies the checked lifecycle publication step which
// failed.
type AuditOperation string

const (
	AuditStamp   AuditOperation = "stamp"
	AuditPublish AuditOperation = "publish"
)

// AuditError reports one bounded internal lifecycle publication failure.
type AuditError struct {
	Operation  AuditOperation
	Stage      hustle.Stage
	ReasonCode hustle.ReasonCode
	Cause      error
}

func (e *AuditError) Error() string {
	return "hustleruntime: lifecycle audit failed: " + string(e.Operation)
}

func (e *AuditError) Unwrap() error { return e.Cause }

// ActivityOperation identifies the activity edge which failed.
type ActivityOperation string

const (
	ActivityAcquire ActivityOperation = "acquire"
	ActivityRelease ActivityOperation = "release"
)

// ActivityError reports blocking-activity acquisition or release failure.
type ActivityError struct {
	RunID     hustle.RunID
	Operation ActivityOperation
	Cause     error
}

func (e *ActivityError) Error() string {
	return "hustleruntime: activity operation failed: " + string(e.Operation)
}

func (e *ActivityError) Unwrap() error { return e.Cause }

// FinalizerError preserves a consumer callback failure with owned-run identity.
type FinalizerError struct {
	RunID      hustle.RunID
	Cause      error
	CleanupErr error
}

func (e *FinalizerError) Error() string { return "hustleruntime: run finalizer failed" }

func (e *FinalizerError) Unwrap() []error {
	errors := make([]error, 0, 2)
	if e.Cause != nil {
		errors = append(errors, e.Cause)
	}
	if e.CleanupErr != nil {
		errors = append(errors, e.CleanupErr)
	}
	return errors
}

// RunStateError reports misuse of the internal ownership seam without silently
// corrupting lane accounting.
type RunStateError struct {
	RunID hustle.RunID
	State runState
}

func (e *RunStateError) Error() string { return "hustleruntime: invalid owned-run state" }

// CloseError reports finalizer failures encountered while resolving queued nodes.
type CloseError struct {
	Failures []*FinalizerError
}

func (e *CloseError) Error() string { return "hustleruntime: close finalization failed" }

func (e *CloseError) Unwrap() []error {
	errors := make([]error, len(e.Failures))
	for index, failure := range e.Failures {
		errors[index] = failure
	}
	return errors
}
