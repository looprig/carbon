package loop

import (
	"math/bits"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/event"
	model "github.com/looprig/inference/model"
)

// ResolvedContextLimits is the checked result of applying one loop policy to
// model metadata. SafetyMargin is reflected only in InputLimit and is always
// subtracted after the raw minimum is selected.
type ResolvedContextLimits struct {
	ReservedOutput content.TokenCount
	RawInputLimit  content.TokenCount
	InputLimit     content.TokenCount
}

// ContextLimitUnknownError reports that model metadata and policy do not yield
// a safe non-zero input denominator.
type ContextLimitUnknownError struct {
	Model model.ModelKey
	Cause error
}

func (e *ContextLimitUnknownError) Error() string {
	message := "loop: context input limit unavailable for " + string(e.Model.Provider) + "/" + e.Model.Model
	if e.Cause != nil {
		return message + ": " + e.Cause.Error()
	}
	return message
}

func (e *ContextLimitUnknownError) Unwrap() error { return e.Cause }

// ContextLimitError reports that an authoritative candidate-request measurement
// reached or exceeded its resolved hard input limit.
type ContextLimitError struct {
	Measurement event.ContextMeasurement
}

func (e *ContextLimitError) Error() string {
	return "loop: candidate request reached the context input limit"
}

// OccupancyError reports an invalid zero denominator.
type OccupancyError struct{ Limit content.TokenCount }

func (e *OccupancyError) Error() string { return "loop: occupancy input limit must be non-zero" }

// ResolveContextLimits applies explicit output reservation and safety margin to
// known model limits without inventing values for unknown fields.
func ResolveContextLimits(model model.ModelKey, limits model.ContextLimits, reservedOutput, safetyMargin content.TokenCount) (ResolvedContextLimits, error) {
	unknown := func(cause error) (ResolvedContextLimits, error) {
		return ResolvedContextLimits{}, &ContextLimitUnknownError{Model: model, Cause: cause}
	}
	if err := model.Validate(); err != nil {
		return unknown(err)
	}
	if err := limits.Validate(); err != nil {
		return unknown(err)
	}
	reserved := reservedOutput
	if limits.MaxOutputTokens != 0 && reserved > limits.MaxOutputTokens {
		reserved = limits.MaxOutputTokens
	}
	var raw content.TokenCount
	if limits.WindowTokens != 0 {
		if reserved >= limits.WindowTokens {
			return unknown(nil)
		}
		raw = limits.WindowTokens - reserved
	}
	if limits.MaxInputTokens != 0 && (raw == 0 || limits.MaxInputTokens < raw) {
		raw = limits.MaxInputTokens
	}
	if raw == 0 || safetyMargin >= raw {
		return unknown(nil)
	}
	return ResolvedContextLimits{ReservedOutput: reserved, RawInputLimit: raw, InputLimit: raw - safetyMargin}, nil
}

// OccupancyBasisPoints calculates floor(used*10_000/limit) with checked
// 128-bit multiplication and clamps over-limit display values at 100%.
func OccupancyBasisPoints(used, limit content.TokenCount) (event.BasisPoints, error) {
	if limit == 0 {
		return 0, &OccupancyError{Limit: limit}
	}
	if used >= limit {
		return event.FullScaleBasisPoints, nil
	}
	hi, lo := bits.Mul64(uint64(used), uint64(event.FullScaleBasisPoints))
	quotient, _ := bits.Div64(hi, lo, uint64(limit))
	if quotient >= uint64(event.FullScaleBasisPoints) {
		return event.FullScaleBasisPoints, nil
	}
	return event.BasisPoints(quotient), nil
}
