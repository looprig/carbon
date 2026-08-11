package content

import "strconv"

// TokenCount is a normalized count of model tokens.
type TokenCount uint64

// UsageField identifies a normalized usage value or derived total.
type UsageField string

const (
	UsageFieldInputTokens         UsageField = "InputTokens"
	UsageFieldOutputTokens        UsageField = "OutputTokens"
	UsageFieldCacheReadTokens     UsageField = "CacheReadTokens"
	UsageFieldCacheCreationTokens UsageField = "CacheCreationTokens"
	UsageFieldReasoningTokens     UsageField = "ReasoningTokens"
	UsageFieldContextTokens       UsageField = "ContextTokens"
	UsageFieldTotalTokens         UsageField = "TotalTokens"
)

// UsageValidationReason identifies why normalized usage is invalid.
type UsageValidationReason string

const UsageValidationReasonReasoningExceedsOutput UsageValidationReason = "exceeds OutputTokens"

const maximumTokenCount TokenCount = ^TokenCount(0)

// Usage is normalized model token usage.
type Usage struct {
	InputTokens         TokenCount
	OutputTokens        TokenCount
	CacheReadTokens     TokenCount
	CacheCreationTokens TokenCount
	ReasoningTokens     TokenCount
}

// UsageValidationError reports an invalid relationship between usage fields.
type UsageValidationError struct {
	Field  UsageField
	Reason UsageValidationReason
}

func (e *UsageValidationError) Error() string {
	return "content: invalid usage field " + string(e.Field) + ": " + string(e.Reason)
}

// UsageOverflowError reports a token-count addition that cannot be represented.
type UsageOverflowError struct {
	Field UsageField
	Left  TokenCount
	Right TokenCount
}

func (e *UsageOverflowError) Error() string {
	return "content: usage field " + string(e.Field) + " overflow: " +
		strconv.FormatUint(uint64(e.Left), 10) + " + " +
		strconv.FormatUint(uint64(e.Right), 10)
}

// Validate verifies relationships between usage fields.
func (u Usage) Validate() error {
	if u.ReasoningTokens > u.OutputTokens {
		return &UsageValidationError{
			Field:  UsageFieldReasoningTokens,
			Reason: UsageValidationReasonReasoningExceedsOutput,
		}
	}
	return nil
}

// ContextTokens returns all input tokens that occupy model context.
func (u Usage) ContextTokens() (TokenCount, error) {
	input, err := addTokenCounts(UsageFieldContextTokens, u.InputTokens, u.CacheReadTokens)
	if err != nil {
		return 0, err
	}
	return addTokenCounts(UsageFieldContextTokens, input, u.CacheCreationTokens)
}

// TotalTokens returns context plus output tokens.
func (u Usage) TotalTokens() (TokenCount, error) {
	contextTokens, err := u.ContextTokens()
	if err != nil {
		return 0, err
	}
	return addTokenCounts(UsageFieldTotalTokens, contextTokens, u.OutputTokens)
}

// Add validates and combines two usage values field by field.
func (u Usage) Add(other Usage) (Usage, error) {
	if err := u.Validate(); err != nil {
		return Usage{}, err
	}
	if err := other.Validate(); err != nil {
		return Usage{}, err
	}

	return addValidUsage(u, other)
}

func addValidUsage(left Usage, right Usage) (Usage, error) {
	var sum Usage
	var err error
	if sum.ReasoningTokens, err = addTokenCounts(UsageFieldReasoningTokens, left.ReasoningTokens, right.ReasoningTokens); err != nil {
		return Usage{}, err
	}
	if sum.InputTokens, err = addTokenCounts(UsageFieldInputTokens, left.InputTokens, right.InputTokens); err != nil {
		return Usage{}, err
	}
	if sum.OutputTokens, err = addTokenCounts(UsageFieldOutputTokens, left.OutputTokens, right.OutputTokens); err != nil {
		return Usage{}, err
	}
	if sum.CacheReadTokens, err = addTokenCounts(UsageFieldCacheReadTokens, left.CacheReadTokens, right.CacheReadTokens); err != nil {
		return Usage{}, err
	}
	if sum.CacheCreationTokens, err = addTokenCounts(UsageFieldCacheCreationTokens, left.CacheCreationTokens, right.CacheCreationTokens); err != nil {
		return Usage{}, err
	}
	return sum, nil
}

func addTokenCounts(field UsageField, left TokenCount, right TokenCount) (TokenCount, error) {
	if right > maximumTokenCount-left {
		return 0, &UsageOverflowError{Field: field, Left: left, Right: right}
	}
	return left + right, nil
}
