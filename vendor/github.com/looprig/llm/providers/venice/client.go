// Package venice provides Venice AI's documented OpenAI Chat and Responses
// endpoints.
package venice

import (
	"github.com/looprig/inference"
	"github.com/looprig/inference/auth"
	model "github.com/looprig/inference/model"

	"github.com/looprig/llm"
	"github.com/looprig/llm/providers/internal/simple"
)

const DefaultBaseURL = "https://api.venice.ai/api/v1"

type Option = simple.Option

func WithHeader(name, value string) Option { return simple.WithHeader(name, value) }

func WithReasoningEffort(value string) Option { return simple.WithReasoningEffort(value) }

func WithServiceTier(value string) Option { return simple.WithServiceTier(value) }

// WithVeniceParameters attaches Venice's documented provider-specific request
// controls, such as disable_thinking or enable_web_search.
func WithVeniceParameters(parameters map[string]any) Option {
	copy := make(map[string]any, len(parameters))
	for key, value := range parameters {
		copy[key] = value
	}
	return simple.WithBodyField("venice_parameters", copy)
}

func New(selected model.Model, key auth.APIKey, options ...Option) (inference.Client, error) {
	definition := simple.Definition{
		Provider:       llm.ProviderVenice,
		DefaultBaseURL: DefaultBaseURL,
		Authentication: auth.AuthAPIKey,
	}
	if selected.APIFormat == model.APIFormatOpenAIResponses {
		definition.DefaultPath = "/responses"
	} else {
		definition.DefaultPath = "/chat/completions"
	}
	return simple.New(selected, key, definition, options...)
}
