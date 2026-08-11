package usagenorm

import (
	"errors"

	"github.com/looprig/core/content"
	usage "github.com/looprig/inference/usage"
)

// ValidateUsage applies the canonical core usage invariants and preserves typed
// core validation failures through normalization.
func ValidateUsage(value content.Usage) error {
	if err := value.Validate(); err != nil {
		normalized := NormalizeValidationError(err)
		var normalizationErr *usage.UsageNormalizationError
		if errors.As(normalized, &normalizationErr) &&
			normalizationErr.Reason == usage.UsageNormalizationReasonReasoningExceedsOutput {
			normalizationErr.Left = value.OutputTokens
			normalizationErr.Right = value.ReasoningTokens
		}
		return normalized
	}
	return nil
}

// NormalizeValidationError maps known core invariants specifically and keeps
// future invariants truthful through a generic reason and exact cause.
func NormalizeValidationError(err error) error {
	var validationErr *content.UsageValidationError
	if !errors.As(err, &validationErr) {
		return &usage.UsageNormalizationError{
			Reason: usage.UsageNormalizationReasonDomainValidation,
			Cause:  err,
		}
	}
	field := coreValidationField(validationErr.Field)
	reason := usage.UsageNormalizationReasonDomainValidation
	if validationErr.Field == content.UsageFieldReasoningTokens &&
		validationErr.Reason == content.UsageValidationReasonReasoningExceedsOutput {
		reason = usage.UsageNormalizationReasonReasoningExceedsOutput
	}
	return &usage.UsageNormalizationError{Field: field, Reason: reason, Cause: validationErr}
}

func coreValidationField(field content.UsageField) usage.UsageNormalizationField {
	switch field {
	case content.UsageFieldInputTokens:
		return usage.UsageNormalizationFieldInputTokens
	case content.UsageFieldOutputTokens:
		return usage.UsageNormalizationFieldOutputTokens
	case content.UsageFieldCacheReadTokens:
		return usage.UsageNormalizationFieldCacheReadTokens
	case content.UsageFieldCacheCreationTokens:
		return usage.UsageNormalizationFieldCacheCreationTokens
	case content.UsageFieldReasoningTokens:
		return usage.UsageNormalizationFieldReasoningTokens
	case content.UsageFieldContextTokens:
		return usage.UsageNormalizationFieldContextTokens
	case content.UsageFieldTotalTokens:
		return usage.UsageNormalizationFieldTotalTokens
	default:
		return usage.UsageNormalizationField(field)
	}
}
