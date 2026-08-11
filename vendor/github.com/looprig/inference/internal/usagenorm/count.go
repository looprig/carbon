package usagenorm

import (
	"bytes"
	"errors"
	"strconv"

	"github.com/looprig/core/content"
	usage "github.com/looprig/inference/usage"
)

// Count preserves whether a JSON count field was absent and defers semantic
// validation until the owning codec supplies its typed field identity.
type Count struct {
	present bool
	raw     []byte
}

// UnmarshalJSON captures one syntactically valid JSON value without losing
// explicit null or numeric representation details.
func (c *Count) UnmarshalJSON(data []byte) error {
	c.present = true
	c.raw = append(c.raw[:0], data...)
	return nil
}

// Present reports whether the JSON field was explicitly present.
func (c Count) Present() bool { return c.present }

// TokenCount returns zero for an absent count and rejects malformed or
// negative explicit values before converting an int64 to TokenCount.
func (c Count) TokenCount(field Field) (content.TokenCount, error) {
	normalizedField, err := normalizationField(field)
	if err != nil {
		return 0, err
	}
	if !c.present {
		return 0, nil
	}

	raw := bytes.TrimSpace(c.raw)
	if bytes.Equal(raw, []byte("null")) {
		return 0, scalarError(normalizedField, usage.UsageNormalizationReasonNull)
	}
	if !isNumber(raw) {
		return 0, scalarError(normalizedField, usage.UsageNormalizationReasonInvalidType)
	}
	if bytes.ContainsAny(raw, ".eE") {
		return 0, scalarError(normalizedField, usage.UsageNormalizationReasonFractional)
	}

	value, parseErr := strconv.ParseInt(string(raw), 10, 64)
	if parseErr != nil {
		if errors.Is(parseErr, strconv.ErrRange) {
			return 0, scalarError(normalizedField, usage.UsageNormalizationReasonOutOfRange)
		}
		return 0, scalarError(normalizedField, usage.UsageNormalizationReasonInvalidType)
	}
	if value < 0 {
		return 0, &usage.UsageNormalizationError{
			Field: normalizedField, Reason: usage.UsageNormalizationReasonNegative, Value: value,
		}
	}
	return content.TokenCount(value), nil
}

// OptionalTokenCount returns zero when an optional usage-detail field is absent
// or explicitly null. Every present non-null value retains TokenCount's strict
// numeric validation.
func (c Count) OptionalTokenCount(field Field) (content.TokenCount, error) {
	if !c.present || bytes.Equal(bytes.TrimSpace(c.raw), []byte("null")) {
		return 0, nil
	}
	return c.TokenCount(field)
}

func isNumber(raw []byte) bool {
	return len(raw) > 0 && (raw[0] == '-' || raw[0] >= '0' && raw[0] <= '9')
}

func scalarError(field usage.UsageNormalizationField, reason usage.UsageNormalizationReason) error {
	return &usage.UsageNormalizationError{Field: field, Reason: reason}
}
