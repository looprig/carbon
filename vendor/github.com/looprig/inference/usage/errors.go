package usage

import (
	"strconv"

	"github.com/looprig/core/content"
)

type UsageNormalizationField string

const (
	UsageNormalizationFieldInputTokens         UsageNormalizationField = "InputTokens"
	UsageNormalizationFieldOutputTokens        UsageNormalizationField = "OutputTokens"
	UsageNormalizationFieldCacheReadTokens     UsageNormalizationField = "CacheReadTokens"
	UsageNormalizationFieldCacheCreationTokens UsageNormalizationField = "CacheCreationTokens"
	UsageNormalizationFieldReasoningTokens     UsageNormalizationField = "ReasoningTokens"
	UsageNormalizationFieldContextTokens       UsageNormalizationField = "ContextTokens"
	UsageNormalizationFieldTotalTokens         UsageNormalizationField = "TotalTokens"
)

type UsageNormalizationReason string

const (
	UsageNormalizationReasonNegative               UsageNormalizationReason = "negative"
	UsageNormalizationReasonComponentsExceedTotal  UsageNormalizationReason = "components exceed total"
	UsageNormalizationReasonOverflow               UsageNormalizationReason = "overflow"
	UsageNormalizationReasonReasoningExceedsOutput UsageNormalizationReason = "reasoning exceeds output"
	UsageNormalizationReasonNull                   UsageNormalizationReason = "null"
	UsageNormalizationReasonFractional             UsageNormalizationReason = "fractional"
	UsageNormalizationReasonOutOfRange             UsageNormalizationReason = "out of range"
	UsageNormalizationReasonInvalidType            UsageNormalizationReason = "invalid type"
	UsageNormalizationReasonInvalidField           UsageNormalizationReason = "invalid field"
	UsageNormalizationReasonTotalMismatch          UsageNormalizationReason = "total mismatch"
	UsageNormalizationReasonDomainValidation       UsageNormalizationReason = "domain validation"
)

type UsageNormalizationError struct {
	Field  UsageNormalizationField
	Reason UsageNormalizationReason
	Value  int64
	Left   content.TokenCount
	Right  content.TokenCount
	Cause  error
}

func (e *UsageNormalizationError) Error() string {
	message := "inference: cannot normalize usage field " + string(e.Field) + ": " + string(e.Reason)
	switch e.Reason {
	case UsageNormalizationReasonNegative:
		message += " (" + strconv.FormatInt(e.Value, 10) + ")"
	case UsageNormalizationReasonComponentsExceedTotal, UsageNormalizationReasonOverflow, UsageNormalizationReasonTotalMismatch:
		message += " (left=" + strconv.FormatUint(uint64(e.Left), 10) +
			", right=" + strconv.FormatUint(uint64(e.Right), 10) + ")"
	case UsageNormalizationReasonReasoningExceedsOutput:
		if e.Left != 0 || e.Right != 0 {
			message += " (left=" + strconv.FormatUint(uint64(e.Left), 10) +
				", right=" + strconv.FormatUint(uint64(e.Right), 10) + ")"
		}
	}
	if e.Cause != nil {
		message += ": " + e.Cause.Error()
	}
	return message
}

func (e *UsageNormalizationError) Unwrap() error { return e.Cause }
