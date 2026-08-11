package loop

import (
	"time"

	"github.com/looprig/core/content"
	contextcount "github.com/looprig/inference/contextcount"
)

// ContextObservationPolicy owns hard-admission settings for a non-compacting
// loop. Every value is explicit; harness supplies no timeout or limit defaults.
type ContextObservationPolicy struct {
	ReservedOutput content.TokenCount
	SafetyMargin   content.TokenCount
	CountTimeout   time.Duration
}

// ContextObservationPolicyField identifies one rejected observation setting.
type ContextObservationPolicyField string

const (
	ContextObservationFieldReservedOutput ContextObservationPolicyField = "ReservedOutput"
	ContextObservationFieldSafetyMargin   ContextObservationPolicyField = "SafetyMargin"
	ContextObservationFieldCountTimeout   ContextObservationPolicyField = "CountTimeout"
)

// ContextObservationPolicyError reports invalid explicit observation policy.
type ContextObservationPolicyError struct {
	Field ContextObservationPolicyField
}

func (e *ContextObservationPolicyError) Error() string {
	return "loop: invalid context observation policy field " + string(e.Field)
}

// Validate checks policy values against already-validated counter metadata.
func (p ContextObservationPolicy) Validate(capability contextcount.CounterCapability) error {
	if p.ReservedOutput == 0 {
		return &ContextObservationPolicyError{Field: ContextObservationFieldReservedOutput}
	}
	if p.CountTimeout <= 0 {
		return &ContextObservationPolicyError{Field: ContextObservationFieldCountTimeout}
	}
	if capability.Quality == contextcount.CountQualityHeuristicEstimate && p.SafetyMargin == 0 {
		return &ContextObservationPolicyError{Field: ContextObservationFieldSafetyMargin}
	}
	return nil
}
