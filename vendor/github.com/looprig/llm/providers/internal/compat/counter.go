package compat

import (
	contextcount "github.com/looprig/inference/contextcount"
	model "github.com/looprig/inference/model"

	"github.com/looprig/llm"
)

// UnsupportedCounter returns the explicit exact-counter boundary shared by
// providers that document no matching token-count endpoint.
func UnsupportedCounter(provider llm.Provider, apiFormat model.APIFormat) (contextcount.ContextCounter, error) {
	return nil, &llm.CounterSupportError{
		Provider:  provider,
		Reason:    llm.CounterSupportExactUnavailable,
		APIFormat: apiFormat,
	}
}
