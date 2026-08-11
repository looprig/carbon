package contextcount

import (
	"context"

	"github.com/looprig/core/content"
	"github.com/looprig/inference"
	"github.com/looprig/inference/model"
)

// ProviderID identifies the provider or gateway that owns a transport path.
type ProviderID string

// TokenizerRevision identifies the tokenization algorithm used by a counter.
type TokenizerRevision string

// SecurityIdentity is a digest of canonical endpoint and security-policy identity.
type SecurityIdentity [32]byte

// CountQuality describes how a context count was produced.
type CountQuality uint8

const (
	CountQualityUnknown CountQuality = iota
	CountQualityExactProvider
	CountQualityExactLocal
	CountQualityHeuristicEstimate
)

// ContextCount is the normalized input-token occupancy of one complete request.
type ContextCount struct {
	Model       model.ModelKey
	InputTokens content.TokenCount
	Quality     CountQuality
}

// ContextCounter counts the complete provider-neutral request and declares its
// secret-free trust posture without performing I/O.
type ContextCounter interface {
	CountContext(context.Context, inference.Request) (ContextCount, error)
	CounterCapability() CounterCapability
}

// ContextCountFunc adapts a function to the ContextCounter contract.
type ContextCountFunc func(context.Context, inference.Request) (ContextCount, error)

// ContextCounterFunc combines a count function with its fixed trust metadata.
type ContextCounterFunc struct {
	Count      ContextCountFunc
	Capability CounterCapability
}

// CountContext calls the count function and rejects structurally invalid results.
func (c ContextCounterFunc) CountContext(ctx context.Context, req inference.Request) (ContextCount, error) {
	if c.Count == nil {
		return ContextCount{}, &ContextCountError{Cause: ErrContextCountFunctionMissing}
	}
	count, err := c.Count(ctx, req)
	if err != nil {
		return ContextCount{}, err
	}
	if err := count.Model.Validate(); err != nil {
		return ContextCount{}, &ContextCountError{Model: count.Model, Quality: count.Quality, Cause: err}
	}
	if !validCountQuality(count.Quality) {
		return ContextCount{}, &ContextCountError{Model: count.Model, Quality: count.Quality, Cause: ErrContextCountQualityInvalid}
	}
	if count.Model != req.Model.Key() {
		return ContextCount{}, &ContextCountError{Model: count.Model, Quality: count.Quality, Cause: ErrContextCountModelMismatch}
	}
	if count.Quality != c.Capability.Quality {
		return ContextCount{}, &ContextCountError{Model: count.Model, Quality: count.Quality, Cause: ErrContextCountCapabilityQualityMismatch}
	}
	return count, nil
}

// CounterCapability returns the adapter's declared metadata without I/O.
func (c ContextCounterFunc) CounterCapability() CounterCapability { return c.Capability }

// CounterTransport describes where request bytes travel for counting.
type CounterTransport uint8

const (
	CounterTransportUnknown CounterTransport = iota
	CounterTransportLocal
	CounterTransportSameEndpoint
	CounterTransportSeparateEndpoint
)

// RetentionPosture describes provider-declared retention of request input.
type RetentionPosture uint8

const (
	RetentionUnknown RetentionPosture = iota
	RetentionNone
	RetentionEphemeral
	RetentionLogged
)

// CounterCapability declares the trust and quality posture of a context counter.
type CounterCapability struct {
	Provider         ProviderID
	Transport        CounterTransport
	SecurityIdentity SecurityIdentity
	Retention        RetentionPosture
	TokenizerRev     TokenizerRevision
	Quality          CountQuality
}

// Validate rejects incomplete or contradictory counter metadata.
func (c CounterCapability) Validate() error {
	if c.Transport == CounterTransportUnknown {
		return capabilityError(CapabilityKindCounter, CapabilityFieldTransport, CapabilityValidationReasonUnknown)
	}
	if c.Transport > CounterTransportSeparateEndpoint {
		return capabilityError(CapabilityKindCounter, CapabilityFieldTransport, CapabilityValidationReasonOutOfRange)
	}
	if c.Retention == RetentionUnknown {
		return capabilityError(CapabilityKindCounter, CapabilityFieldRetention, CapabilityValidationReasonUnknown)
	}
	if c.Retention > RetentionLogged {
		return capabilityError(CapabilityKindCounter, CapabilityFieldRetention, CapabilityValidationReasonOutOfRange)
	}
	if c.TokenizerRev == "" {
		return capabilityError(CapabilityKindCounter, CapabilityFieldTokenizerRevision, CapabilityValidationReasonEmpty)
	}
	if c.Quality == CountQualityUnknown {
		return capabilityError(CapabilityKindCounter, CapabilityFieldQuality, CapabilityValidationReasonUnknown)
	}
	if !validCountQuality(c.Quality) {
		return capabilityError(CapabilityKindCounter, CapabilityFieldQuality, CapabilityValidationReasonOutOfRange)
	}
	return c.validateTransportMetadata()
}

func (c CounterCapability) validateTransportMetadata() error {
	switch c.Transport {
	case CounterTransportLocal:
		if c.SecurityIdentity != (SecurityIdentity{}) {
			return capabilityError(CapabilityKindCounter, CapabilityFieldSecurityIdentity, CapabilityValidationReasonMustBeZero)
		}
	case CounterTransportSameEndpoint, CounterTransportSeparateEndpoint:
		if c.Provider == "" {
			return capabilityError(CapabilityKindCounter, CapabilityFieldProvider, CapabilityValidationReasonEmpty)
		}
		if c.SecurityIdentity == (SecurityIdentity{}) {
			return capabilityError(CapabilityKindCounter, CapabilityFieldSecurityIdentity, CapabilityValidationReasonEmpty)
		}
	case CounterTransportUnknown:
		return capabilityError(CapabilityKindCounter, CapabilityFieldTransport, CapabilityValidationReasonUnknown)
	default:
		return capabilityError(CapabilityKindCounter, CapabilityFieldTransport, CapabilityValidationReasonOutOfRange)
	}
	return nil
}

// InferenceTransport describes the inference request's transport protection.
type InferenceTransport uint8

const (
	InferenceTransportUnknown InferenceTransport = iota
	InferenceTransportLocal
	InferenceTransportTLS
	InferenceTransportAttestedTLS
	InferenceTransportEndToEndEncrypted
)

// InferenceCapability declares the inference path's trust posture.
type InferenceCapability struct {
	Provider         ProviderID
	Transport        InferenceTransport
	SecurityIdentity SecurityIdentity
	Retention        RetentionPosture
}

// Validate rejects incomplete or contradictory inference metadata. An unknown
// retention posture remains structurally valid and is handled fail-closed by
// compatibility checks.
func (c InferenceCapability) Validate() error {
	if c.Transport == InferenceTransportUnknown {
		return capabilityError(CapabilityKindInference, CapabilityFieldTransport, CapabilityValidationReasonUnknown)
	}
	if c.Transport > InferenceTransportEndToEndEncrypted {
		return capabilityError(CapabilityKindInference, CapabilityFieldTransport, CapabilityValidationReasonOutOfRange)
	}
	if c.Retention > RetentionLogged {
		return capabilityError(CapabilityKindInference, CapabilityFieldRetention, CapabilityValidationReasonOutOfRange)
	}
	switch c.Transport {
	case InferenceTransportLocal:
		if c.SecurityIdentity != (SecurityIdentity{}) {
			return capabilityError(CapabilityKindInference, CapabilityFieldSecurityIdentity, CapabilityValidationReasonMustBeZero)
		}
	case InferenceTransportTLS, InferenceTransportAttestedTLS, InferenceTransportEndToEndEncrypted:
		if c.Provider == "" {
			return capabilityError(CapabilityKindInference, CapabilityFieldProvider, CapabilityValidationReasonEmpty)
		}
		if c.SecurityIdentity == (SecurityIdentity{}) {
			return capabilityError(CapabilityKindInference, CapabilityFieldSecurityIdentity, CapabilityValidationReasonEmpty)
		}
	case InferenceTransportUnknown:
		return capabilityError(CapabilityKindInference, CapabilityFieldTransport, CapabilityValidationReasonUnknown)
	default:
		return capabilityError(CapabilityKindInference, CapabilityFieldTransport, CapabilityValidationReasonOutOfRange)
	}
	return nil
}

// CompatibleCounter reports whether a counter avoids weakening an inference
// path's transport, identity, and retention posture.
func CompatibleCounter(inf InferenceCapability, counter CounterCapability) error {
	if err := inf.Validate(); err != nil {
		return compatibilityError(inf, counter, CounterCompatibilityInvalidInference, err)
	}
	if err := counter.Validate(); err != nil {
		return compatibilityError(inf, counter, CounterCompatibilityInvalidCounter, err)
	}
	if providerNeutralCounter(counter) {
		return nil
	}

	switch counter.Transport {
	case CounterTransportLocal:
		switch inf.Transport {
		case InferenceTransportLocal, InferenceTransportTLS, InferenceTransportAttestedTLS, InferenceTransportEndToEndEncrypted:
		case InferenceTransportUnknown:
			return compatibilityError(inf, counter, CounterCompatibilityTransportDowngrade, nil)
		default:
			return compatibilityError(inf, counter, CounterCompatibilityTransportDowngrade, nil)
		}
		if counter.Provider == "" || counter.Provider != inf.Provider {
			return compatibilityError(inf, counter, CounterCompatibilityProviderMismatch, nil)
		}
	case CounterTransportSameEndpoint:
		switch inf.Transport {
		case InferenceTransportTLS, InferenceTransportAttestedTLS, InferenceTransportEndToEndEncrypted:
		case InferenceTransportLocal, InferenceTransportUnknown:
			return compatibilityError(inf, counter, CounterCompatibilityTransportDowngrade, nil)
		default:
			return compatibilityError(inf, counter, CounterCompatibilityTransportDowngrade, nil)
		}
		if counter.Provider != inf.Provider {
			return compatibilityError(inf, counter, CounterCompatibilityProviderMismatch, nil)
		}
		if counter.SecurityIdentity != inf.SecurityIdentity {
			return compatibilityError(inf, counter, CounterCompatibilityIdentityMismatch, nil)
		}
	case CounterTransportSeparateEndpoint:
		switch inf.Transport {
		case InferenceTransportTLS:
		case InferenceTransportLocal, InferenceTransportAttestedTLS, InferenceTransportEndToEndEncrypted:
			return compatibilityError(inf, counter, CounterCompatibilityTransportDowngrade, nil)
		case InferenceTransportUnknown:
			return compatibilityError(inf, counter, CounterCompatibilityTransportDowngrade, nil)
		default:
			return compatibilityError(inf, counter, CounterCompatibilityTransportDowngrade, nil)
		}
		if counter.Provider != inf.Provider {
			return compatibilityError(inf, counter, CounterCompatibilityProviderMismatch, nil)
		}
	case CounterTransportUnknown:
		return compatibilityError(inf, counter, CounterCompatibilityTransportDowngrade, nil)
	default:
		return compatibilityError(inf, counter, CounterCompatibilityTransportDowngrade, nil)
	}

	if !retentionCompatible(inf.Retention, counter.Retention) {
		return compatibilityError(inf, counter, CounterCompatibilityRetentionDowngrade, nil)
	}
	return nil
}

func providerNeutralCounter(counter CounterCapability) bool {
	return counter.Transport == CounterTransportLocal &&
		counter.Retention == RetentionNone &&
		counter.Provider == "" &&
		counter.SecurityIdentity == (SecurityIdentity{})
}

func retentionCompatible(inferenceRetention, counterRetention RetentionPosture) bool {
	switch inferenceRetention {
	case RetentionNone:
		return counterRetention == RetentionNone
	case RetentionEphemeral:
		return counterRetention == RetentionNone || counterRetention == RetentionEphemeral
	case RetentionLogged:
		return counterRetention == RetentionNone || counterRetention == RetentionEphemeral || counterRetention == RetentionLogged
	case RetentionUnknown:
		return false
	default:
		return false
	}
}

func validCountQuality(quality CountQuality) bool {
	switch quality {
	case CountQualityExactProvider, CountQualityExactLocal, CountQualityHeuristicEstimate:
		return true
	case CountQualityUnknown:
		return false
	default:
		return false
	}
}

func capabilityError(kind CapabilityKind, field CapabilityField, reason CapabilityValidationReason) error {
	return &CapabilityValidationError{Capability: kind, Field: field, Reason: reason}
}

func compatibilityError(inf InferenceCapability, counter CounterCapability, reason CounterCompatibilityReason, cause error) error {
	return &CounterCompatibilityError{Inference: inf, Counter: counter, Reason: reason, Cause: cause}
}

var _ ContextCounter = ContextCounterFunc{}
