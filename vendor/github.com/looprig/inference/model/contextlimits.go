package model

import (
	"strconv"

	"github.com/looprig/core/content"
)

// ContextLimits describes a model's known context capacity. Zero fields are
// explicitly unknown and are resolved by policy rather than guessed here.
type ContextLimits struct {
	WindowTokens    content.TokenCount
	MaxInputTokens  content.TokenCount
	MaxOutputTokens content.TokenCount
}

// ContextLimitField identifies one ContextLimits field.
type ContextLimitField string

const (
	ContextLimitFieldMaxInputTokens  ContextLimitField = "MaxInputTokens"
	ContextLimitFieldMaxOutputTokens ContextLimitField = "MaxOutputTokens"
)

// ContextLimitValidationReason identifies why a context limit is invalid.
type ContextLimitValidationReason string

const ContextLimitValidationReasonExceedsWindow ContextLimitValidationReason = "exceeds WindowTokens"

// ContextLimitsValidationError reports a cap that contradicts a known shared
// context window.
type ContextLimitsValidationError struct {
	Field        ContextLimitField
	Reason       ContextLimitValidationReason
	Value        content.TokenCount
	WindowTokens content.TokenCount
}

func (e *ContextLimitsValidationError) Error() string {
	return "inference: invalid context limit " + string(e.Field) + " " +
		strconv.FormatUint(uint64(e.Value), 10) + ": " + string(e.Reason) + " " +
		strconv.FormatUint(uint64(e.WindowTokens), 10)
}

// Validate verifies relationships that can be established from known fields.
// Independent input and output maxima need not sum below the shared window;
// request admission accounts for their combined use.
func (l ContextLimits) Validate() error {
	if l.WindowTokens == 0 {
		return nil
	}
	if l.MaxInputTokens > l.WindowTokens {
		return &ContextLimitsValidationError{
			Field:        ContextLimitFieldMaxInputTokens,
			Reason:       ContextLimitValidationReasonExceedsWindow,
			Value:        l.MaxInputTokens,
			WindowTokens: l.WindowTokens,
		}
	}
	if l.MaxOutputTokens > l.WindowTokens {
		return &ContextLimitsValidationError{
			Field:        ContextLimitFieldMaxOutputTokens,
			Reason:       ContextLimitValidationReasonExceedsWindow,
			Value:        l.MaxOutputTokens,
			WindowTokens: l.WindowTokens,
		}
	}
	return nil
}
