package hustleruntime

import (
	"context"
	"errors"

	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/inference/failure"
)

type retryFailureClass uint8

const (
	retryFailureNone retryFailureClass = iota
	retryFailureTransientInference
	retryFailureRecoverableTerminal
)

// classifyRetryFailure is deliberately closed over provider-neutral typed
// failures. It never inspects provider-controlled error text.
func classifyRetryFailure(err error) retryFailureClass {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return retryFailureNone
	}
	var networkErr *failure.NetworkError
	if errors.As(err, &networkErr) {
		return retryFailureTransientInference
	}
	var apiErr *failure.APIError
	if errors.As(err, &apiErr) && transientAPIStatus(apiErr.Status) {
		return retryFailureTransientInference
	}
	var responseErr *ToolResponseError
	if errors.As(err, &responseErr) && responseErr.Valid() &&
		responseErr.Reason == ToolResponseFailureInvalidTerminal {
		return retryFailureRecoverableTerminal
	}
	outputErr, ok := err.(*OutputError)
	if ok && hustle.IsRecoverableTerminalValidationError(outputErr.Cause) {
		return retryFailureRecoverableTerminal
	}
	return retryFailureNone
}

func transientAPIStatus(status int) bool {
	return status == 408 || status == 429 || status >= 500 && status <= 599
}

func shouldRetry(policy hustle.RetryPolicy, attempt int, contextErr error, runErr *RunError, poisoned bool) bool {
	if policy != hustle.RetryPolicyClassifiedOnce || attempt != 0 ||
		contextErr != nil || poisoned || runErr == nil {
		return false
	}
	class := classifyRetryFailure(runErr.Cause)
	transientInference := class == retryFailureTransientInference &&
		runErr.Stage == hustle.StageInference &&
		runErr.ReasonCode == hustle.ReasonInference
	recoverableTerminal := class == retryFailureRecoverableTerminal &&
		runErr.Stage == hustle.StageOutput &&
		runErr.ReasonCode == hustle.ReasonInvalidOutput
	return transientInference || recoverableTerminal
}
