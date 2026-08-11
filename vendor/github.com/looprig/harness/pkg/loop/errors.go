package loop

import "github.com/looprig/harness/pkg/event"

type ConfigErrorKind string

const (
	ConfigMissingClient    ConfigErrorKind = "missing_client"
	ConfigInvalidModel     ConfigErrorKind = "invalid_model"
	ConfigMissingPublisher ConfigErrorKind = "missing_publisher"
)

// ConfigError reports an invalid resolved definition or missing actor-runtime
// collaborator discovered during internal binding. Public callers normally encounter
// DefinitionError from Define or BindError from Definition.Bind before this seam.
type ConfigError struct {
	Kind  ConfigErrorKind
	Cause error
}

func (e *ConfigError) Error() string {
	switch e.Kind {
	case ConfigMissingClient:
		return "loop: runtime binding error: inference client is required"
	case ConfigInvalidModel:
		return "loop: runtime binding error: model is invalid"
	case ConfigMissingPublisher:
		return "loop: runtime binding error: event publisher is required"
	default:
		return "loop: runtime binding error"
	}
}
func (e *ConfigError) Unwrap() error { return e.Cause }

// IDGenerationError is the typed cause logged when the actor cannot mint a TurnID
// from crypto/rand while starting a turn from an accepted submit. The turn is not
// started; the submit's outcome is a published event.TurnRejected{RejectInternal}
// (or, for a never-rejected agent hand-back, event.InputCancelled{CancelTurnFailed}).
// It remains a distinct typed error so the failure is greppable/testable, even though
// it is only logged — no event carries the error cause itself.
type IDGenerationError struct{ Cause error }

func (e *IDGenerationError) Error() string {
	if e.Cause == nil {
		return "loop: turn id generation failed"
	}
	return "loop: turn id generation failed: " + e.Cause.Error()
}
func (e *IDGenerationError) Unwrap() error { return e.Cause }

// InputRejectedError is the actor's point-to-point admission refusal for a managed
// delegate input. The matching TurnRejected remains the durable/event-stream result;
// this typed error prevents the synchronous acceptance waiter from returning a handle.
type InputRejectedError struct {
	Reason event.RejectReason
	Cause  error
}

func (e *InputRejectedError) Error() string {
	if e.Cause != nil {
		return "loop: input rejected: " + e.Cause.Error()
	}
	return "loop: input rejected"
}
func (e *InputRejectedError) Unwrap() error { return e.Cause }

// PolicyRevisionMarshalError is the programmer-error panic value PolicyRevision raises if
// the fully-owned, total projection it builds cannot be JSON-marshaled. The projection is
// composed only of marshalable types, so a failure is a code defect (an unmarshalable field
// was introduced), never a runtime or input condition. PolicyRevision panics with it rather
// than returning a nil-collapsed digest, because a silent sha256(nil) would defeat the
// restore config-mismatch drift detection that consumes the digest.
type PolicyRevisionMarshalError struct{ Cause error }

func (e *PolicyRevisionMarshalError) Error() string {
	if e.Cause == nil {
		return "loop: policy-revision projection failed to marshal"
	}
	return "loop: policy-revision projection failed to marshal: " + e.Cause.Error()
}
func (e *PolicyRevisionMarshalError) Unwrap() error { return e.Cause }

// CommitCancelReason distinguishes why a per-step commit handshake did not reach
// the actor's commit point.
type CommitCancelReason string

const (
	// CommitTurnCancelled means the turn context was cancelled (Interrupt/Shutdown)
	// before the actor committed the step. runTurn returns a TurnInterrupted and the
	// in-flight step is discarded; committed steps stay committed.
	CommitTurnCancelled CommitCancelReason = "turn cancelled"
)

// CommitError is returned by turnConfig.commit when the ctx-cancellable commit
// handshake cannot deliver a completed step to the actor. The turn goroutine stops
// and surfaces a terminal without wedging; the already-committed steps remain in
// loopState.msgs. Callers MAY errors.As to distinguish cancel reasons; today the
// only reason is CommitTurnCancelled and callers treat any commit error as an
// interrupt. Reason is reserved for a later phase (e.g. Shutdown-vs-Interrupt).
type CommitError struct {
	Reason CommitCancelReason
	Cause  error
}

func (e *CommitError) Error() string {
	if e.Cause != nil {
		return "loop: commit handshake: " + string(e.Reason) + ": " + e.Cause.Error()
	}
	return "loop: commit handshake: " + string(e.Reason)
}
func (e *CommitError) Unwrap() error { return e.Cause }
