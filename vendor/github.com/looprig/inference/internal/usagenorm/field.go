// Package usagenorm validates and normalizes provider token-usage wire values.
package usagenorm

import usage "github.com/looprig/inference/usage"

// Field is the closed set of usage fields normalization may report.
type Field uint8

const (
	FieldInputTokens Field = iota + 1
	FieldOutputTokens
	FieldCacheReadTokens
	FieldCacheCreationTokens
	FieldReasoningTokens
	FieldContextTokens
	FieldTotalTokens
)

func normalizationField(field Field) (usage.UsageNormalizationField, error) {
	switch field {
	case FieldInputTokens:
		return usage.UsageNormalizationFieldInputTokens, nil
	case FieldOutputTokens:
		return usage.UsageNormalizationFieldOutputTokens, nil
	case FieldCacheReadTokens:
		return usage.UsageNormalizationFieldCacheReadTokens, nil
	case FieldCacheCreationTokens:
		return usage.UsageNormalizationFieldCacheCreationTokens, nil
	case FieldReasoningTokens:
		return usage.UsageNormalizationFieldReasoningTokens, nil
	case FieldContextTokens:
		return usage.UsageNormalizationFieldContextTokens, nil
	case FieldTotalTokens:
		return usage.UsageNormalizationFieldTotalTokens, nil
	default:
		return "", &usage.UsageNormalizationError{Reason: usage.UsageNormalizationReasonInvalidField}
	}
}
