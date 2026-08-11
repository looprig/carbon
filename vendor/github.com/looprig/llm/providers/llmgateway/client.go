// Package llmgateway provides LLM Gateway's documented OpenAI Chat and
// Anthropic Messages proxy endpoints.
package llmgateway

import (
	"github.com/looprig/inference"
	"github.com/looprig/inference/auth"
	model "github.com/looprig/inference/model"

	"github.com/looprig/llm"
	"github.com/looprig/llm/providers/internal/simple"
)

const DefaultBaseURL = "https://api.llmgateway.io/v1"

type Option = simple.Option

func WithHeader(name, value string) Option { return simple.WithHeader(name, value) }

func WithReasoningEffort(value string) Option { return simple.WithReasoningEffort(value) }

func WithThinkingBudget(budget int) Option { return simple.WithThinkingBudget(budget) }

func New(selected model.Model, key auth.APIKey, options ...Option) (inference.Client, error) {
	defaults := append([]Option(nil), options...)
	definition := simple.Definition{
		Provider:       llm.ProviderLLMGateway,
		DefaultBaseURL: DefaultBaseURL,
		Authentication: auth.AuthAPIKey,
	}
	if selected.APIFormat == model.APIFormatAnthropic {
		definition.DefaultPath = "/messages"
		defaults = append([]Option{simple.WithHeader("anthropic-version", "2023-06-01")}, defaults...)
	} else {
		definition.DefaultPath = "/chat/completions"
	}
	return simple.New(selected, key, definition, defaults...)
}
