package retry

import (
	"context"
	"errors"

	"github.com/looprig/inference/failure"
)

// Retryable reports whether err is worth another attempt: a transient
// transport/network failure or a provider status that signals pressure
// (408, 429, 5xx). Context cancellation is never retryable, even when it
// wraps a retryable cause. Exported so other callers (hustleruntime holds a
// private copy of this predicate today) can converge on one definition.
func Retryable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var exhausted *ExhaustedError
	if errors.As(err, &exhausted) {
		return false
	}
	var netErr *failure.NetworkError
	if errors.As(err, &netErr) {
		return true
	}
	var apiErr *failure.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status == 408 || apiErr.Status == 429 || apiErr.Status >= 500 && apiErr.Status <= 599
	}
	return false
}
