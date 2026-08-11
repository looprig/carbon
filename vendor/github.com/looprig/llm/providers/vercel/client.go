// Package vercel provides Vercel AI Gateway's documented OpenAI Chat,
// Responses, and Anthropic Messages endpoints.
package vercel

import (
	"github.com/looprig/inference"
	"github.com/looprig/inference/auth"
	model "github.com/looprig/inference/model"

	"github.com/looprig/llm"
	"github.com/looprig/llm/providers/internal/simple"
)

const (
	DefaultOpenAIBaseURL    = "https://ai-gateway.vercel.sh/v1"
	DefaultAnthropicBaseURL = "https://ai-gateway.vercel.sh/v1"
)

type Option = simple.Option

func WithHeader(name, value string) Option { return simple.WithHeader(name, value) }

func WithReasoningEffort(value string) Option { return simple.WithReasoningEffort(value) }

func WithMetadata(metadata map[string]string) Option {
	return simple.WithBodyField("metadata", metadata)
}

func New(selected model.Model, key auth.APIKey, options ...Option) (inference.Client, error) {
	definition := simple.Definition{
		Provider:       llm.ProviderVercel,
		Authentication: auth.AuthAPIKey,
	}
	if selected.APIFormat == model.APIFormatAnthropic {
		definition.DefaultBaseURL = DefaultAnthropicBaseURL
		definition.DefaultPath = "/messages"
		defaults := []Option{simple.WithHeader("anthropic-version", "2023-06-01")}
		defaults = append(defaults, options...)
		return simple.New(selected, key, definition, defaults...)
	}
	definition.DefaultBaseURL = DefaultOpenAIBaseURL
	if selected.APIFormat == model.APIFormatOpenAIResponses {
		definition.DefaultPath = "/responses"
	} else {
		definition.DefaultPath = "/chat/completions"
	}
	defaults := append([]Option(nil), options...)
	return simple.New(selected, key, definition, defaults...)
}
