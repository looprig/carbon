// Package opencode provides the OpenCode Go endpoint using the shared Chat,
// Responses, and Anthropic transport semantics.
package opencode

import (
	"github.com/looprig/inference"
	"github.com/looprig/inference/auth"
	model "github.com/looprig/inference/model"

	"github.com/looprig/llm"
	"github.com/looprig/llm/providers/internal/simple"
)

const DefaultBaseURL = "https://opencode.ai/zen/go/v1"

type Option = simple.Option

func New(selected model.Model, key auth.APIKey, options ...Option) (inference.Client, error) {
	definition := simple.Definition{
		Provider:       llm.ProviderOpenCodeGo,
		DefaultBaseURL: DefaultBaseURL,
		Authentication: auth.AuthAPIKey,
	}
	defaults := options
	switch selected.APIFormat {
	case model.APIFormatOpenAIResponses:
		definition.DefaultPath = "/responses"
	case model.APIFormatAnthropic:
		definition.DefaultPath = "/messages"
		definition.KeyHeader = "x-api-key"
		defaults = append([]Option{simple.WithHeader("anthropic-version", "2023-06-01")}, defaults...)
	default:
		definition.DefaultPath = "/chat/completions"
	}
	return simple.New(selected, key, definition, defaults...)
}
