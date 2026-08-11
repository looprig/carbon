package contextcount

import (
	"errors"
	"fmt"

	"github.com/looprig/inference/model"
)

var (
	ErrContextCountFunctionMissing           = errors.New("context count function is missing")
	ErrContextCountQualityInvalid            = errors.New("context count quality is invalid")
	ErrContextCountModelMismatch             = errors.New("context count model does not match request model")
	ErrContextCountCapabilityQualityMismatch = errors.New("context count quality does not match declared capability")
)

type ContextCountError struct {
	Model   model.ModelKey
	Quality CountQuality
	Cause   error
}

func (e *ContextCountError) Error() string {
	cause := "unknown cause"
	if e.Cause != nil {
		cause = e.Cause.Error()
	}
	return fmt.Sprintf("inference: context count for model %q/%q with quality %d failed: %s", e.Model.Provider, e.Model.Model, e.Quality, cause)
}

func (e *ContextCountError) Unwrap() error { return e.Cause }

type CapabilityKind string

const (
	CapabilityKindCounter   CapabilityKind = "counter"
	CapabilityKindInference CapabilityKind = "inference"
)

type CapabilityField string

const (
	CapabilityFieldProvider          CapabilityField = "Provider"
	CapabilityFieldTransport         CapabilityField = "Transport"
	CapabilityFieldSecurityIdentity  CapabilityField = "SecurityIdentity"
	CapabilityFieldRetention         CapabilityField = "Retention"
	CapabilityFieldTokenizerRevision CapabilityField = "TokenizerRev"
	CapabilityFieldQuality           CapabilityField = "Quality"
)

type CapabilityValidationReason string

const (
	CapabilityValidationReasonUnknown    CapabilityValidationReason = "unknown"
	CapabilityValidationReasonOutOfRange CapabilityValidationReason = "out of range"
	CapabilityValidationReasonEmpty      CapabilityValidationReason = "must not be empty"
	CapabilityValidationReasonMustBeZero CapabilityValidationReason = "must be zero"
)

type CapabilityValidationError struct {
	Capability CapabilityKind
	Field      CapabilityField
	Reason     CapabilityValidationReason
}

func (e *CapabilityValidationError) Error() string {
	return fmt.Sprintf("inference: invalid %s capability field %s: %s", e.Capability, e.Field, e.Reason)
}

type CounterCompatibilityReason string

const (
	CounterCompatibilityInvalidInference   CounterCompatibilityReason = "invalid inference capability"
	CounterCompatibilityInvalidCounter     CounterCompatibilityReason = "invalid counter capability"
	CounterCompatibilityProviderMismatch   CounterCompatibilityReason = "provider mismatch"
	CounterCompatibilityIdentityMismatch   CounterCompatibilityReason = "security identity mismatch"
	CounterCompatibilityTransportDowngrade CounterCompatibilityReason = "transport downgrade"
	CounterCompatibilityRetentionDowngrade CounterCompatibilityReason = "retention downgrade"
)

type CounterCompatibilityError struct {
	Inference InferenceCapability
	Counter   CounterCapability
	Reason    CounterCompatibilityReason
	Cause     error
}

func (e *CounterCompatibilityError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("inference: incompatible counter: %s: %v", e.Reason, e.Cause)
	}
	return "inference: incompatible counter: " + string(e.Reason)
}

func (e *CounterCompatibilityError) Unwrap() error { return e.Cause }
