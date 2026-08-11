// Package deepinfra provides Deep Infra's documented OpenAI and Anthropic
// compatible endpoints.
package deepinfra

import (
	"github.com/looprig/inference"
	"github.com/looprig/inference/auth"
	model "github.com/looprig/inference/model"

	"github.com/looprig/llm"
	"github.com/looprig/llm/providers/internal/simple"
)

const (
	DefaultOpenAIBaseURL    = "https://api.deepinfra.com/v1/openai"
	DefaultAnthropicBaseURL = "https://api.deepinfra.com/anthropic/v1"
)

type Option = simple.Option

func New(selected model.Model, key auth.APIKey, options ...Option) (inference.Client, error) {
	definition := simple.Definition{
		Provider:       llm.ProviderDeepInfra,
		DefaultBaseURL: DefaultOpenAIBaseURL,
		DefaultPath:    "/chat/completions",
		Authentication: auth.AuthAPIKey,
	}
	if selected.APIFormat == model.APIFormatAnthropic {
		definition.DefaultBaseURL = DefaultAnthropicBaseURL
		definition.DefaultPath = "/messages"
		definition.KeyHeader = "x-api-key"
		defaults := []Option{simple.WithHeader("anthropic-version", "2023-06-01")}
		defaults = append(defaults, options...)
		return simple.New(selected, key, definition, defaults...)
	}
	return simple.New(selected, key, definition, options...)
}
