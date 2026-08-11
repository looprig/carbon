package azure

import (
	"github.com/looprig/inference/auth"
	contextcount "github.com/looprig/inference/contextcount"
	model "github.com/looprig/inference/model"
	"github.com/looprig/llm"
)

// NewCounter reports Azure's deliberate counter boundary. Azure's modern
// Responses endpoint does not expose OpenAI's /responses/input_tokens route,
// so returning an estimator here would violate the exact ContextCounter
// contract.
func NewCounter(_ auth.APIKey) (contextcount.ContextCounter, error) {
	return nil, &llm.CounterSupportError{
		Provider:  llm.ProviderAzure,
		Reason:    llm.CounterSupportExactUnavailable,
		APIFormat: model.APIFormatOpenAIResponses,
	}
}
