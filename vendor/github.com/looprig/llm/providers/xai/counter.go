package xai

import (
	"github.com/looprig/inference/auth"
	contextcount "github.com/looprig/inference/contextcount"
	model "github.com/looprig/inference/model"
	"github.com/looprig/llm"
)

// NewCounter reports xAI's deliberate counter boundary. xAI documents text
// tokenization, but not an exact full Responses-input counter that includes the
// same messages, tools, and provider options as inference; silently estimating
// here would violate the ContextCounter contract.
func NewCounter(_ auth.APIKey) (contextcount.ContextCounter, error) {
	return nil, &llm.CounterSupportError{
		Provider:  llm.ProviderXAI,
		Reason:    llm.CounterSupportExactUnavailable,
		APIFormat: model.APIFormatOpenAIResponses,
	}
}
