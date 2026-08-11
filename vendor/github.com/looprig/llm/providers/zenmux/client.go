// Package zenmux provides ZenMux's documented OpenAI, Responses, and
// Anthropic protocol-conversion endpoints.
package zenmux

import (
	"github.com/looprig/inference"
	"github.com/looprig/inference/auth"
	model "github.com/looprig/inference/model"

	"github.com/looprig/llm"
	"github.com/looprig/llm/providers/internal/simple"
)

const (
	DefaultBaseURL          = "https://zenmux.ai/api/v1"
	DefaultAnthropicBaseURL = "https://zenmux.ai/api/anthropic/v1"
)

type Option = simple.Option

func WithHeader(name, value string) Option { return simple.WithHeader(name, value) }

func WithReasoningEffort(value string) Option { return simple.WithReasoningEffort(value) }

func WithThinkingBudget(budget int) Option { return simple.WithThinkingBudget(budget) }

func New(selected model.Model, key auth.APIKey, options ...Option) (inference.Client, error) {
	defaults := append([]Option(nil), options...)
	definition := simple.Definition{
		Provider:       llm.ProviderZenMux,
		DefaultBaseURL: DefaultBaseURL,
		Authentication: auth.AuthAPIKey,
	}
	switch selected.APIFormat {
	case model.APIFormatOpenAIResponses:
		definition.DefaultPath = "/responses"
	case model.APIFormatAnthropic:
		definition.DefaultBaseURL = DefaultAnthropicBaseURL
		definition.DefaultPath = "/messages"
		definition.KeyHeader = "x-api-key"
		defaults = append([]Option{simple.WithHeader("anthropic-version", "2023-06-01")}, defaults...)
	default:
		definition.DefaultPath = "/chat/completions"
	}
	return simple.New(selected, key, definition, defaults...)
}
