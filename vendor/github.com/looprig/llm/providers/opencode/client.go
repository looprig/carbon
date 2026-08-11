// Package opencode provides the OpenCode Zen OpenAI-compatible endpoint.
package opencode

import (
	"github.com/looprig/inference"
	"github.com/looprig/inference/auth"
	model "github.com/looprig/inference/model"

	"github.com/looprig/llm"
	"github.com/looprig/llm/providers/internal/simple"
)

const DefaultBaseURL = "https://opencode.ai/zen/v1"

const DefaultBaseURLGo = "https://opencode.ai/zen/go/v1"

type Option = simple.Option

func New(selected model.Model, key auth.APIKey, options ...Option) (inference.Client, error) {
	if selected.Provider != model.ProviderName(llm.ProviderOpenCode) {
		return nil, &model.ValidationError{Field: "Provider", Reason: "opencode constructor received a different provider model"}
	}
	definition := simple.Definition{
		Provider:       llm.ProviderOpenCode,
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
