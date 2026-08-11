package usagenorm

import (
	"github.com/looprig/core/content"
	usage "github.com/looprig/inference/usage"
)

const maximumTokenCount content.TokenCount = ^content.TokenCount(0)

// AddTokenCounts adds normalized counts without wrapping.
func AddTokenCounts(field Field, left, right content.TokenCount) (content.TokenCount, error) {
	normalizedField, err := normalizationField(field)
	if err != nil {
		return 0, err
	}
	if right > maximumTokenCount-left {
		return 0, &usage.UsageNormalizationError{
			Field: normalizedField, Reason: usage.UsageNormalizationReasonOverflow,
			Left: left, Right: right,
		}
	}
	return left + right, nil
}

// SubtractTokenCounts removes two disjoint components from a reported total.
func SubtractTokenCounts(field Field, total, first, second content.TokenCount) (content.TokenCount, error) {
	normalizedField, err := normalizationField(field)
	if err != nil {
		return 0, err
	}
	components, err := AddTokenCounts(field, first, second)
	if err != nil {
		return 0, err
	}
	if components > total {
		return 0, &usage.UsageNormalizationError{
			Field: normalizedField, Reason: usage.UsageNormalizationReasonComponentsExceedTotal,
			Left: total, Right: components,
		}
	}
	return total - components, nil
}

// RequireEqual rejects contradictory reported and calculated values.
func RequireEqual(field Field, reported, calculated content.TokenCount) error {
	normalizedField, err := normalizationField(field)
	if err != nil {
		return err
	}
	if reported == calculated {
		return nil
	}
	return &usage.UsageNormalizationError{
		Field: normalizedField, Reason: usage.UsageNormalizationReasonTotalMismatch,
		Left: reported, Right: calculated,
	}
}
