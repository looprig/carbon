// Package deepseek provides the documented deepseek OpenAI-compatible API.
package deepseek

import (
	"github.com/looprig/inference"
	"github.com/looprig/inference/auth"
	model "github.com/looprig/inference/model"

	"github.com/looprig/llm"
	"github.com/looprig/llm/providers/internal/simple"
)

const DefaultBaseURL = "https://api.deepseek.com"

type Option = simple.Option

func New(selected model.Model, key auth.APIKey, options ...Option) (inference.Client, error) {
	return simple.New(selected, key, simple.Definition{
		Provider:       llm.ProviderDeepSeek,
		DefaultBaseURL: DefaultBaseURL,
		DefaultPath:    "/chat/completions",
		Authentication: auth.AuthAPIKey,
	}, options...)
}
