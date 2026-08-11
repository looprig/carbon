// Package groq provides the documented groq OpenAI-compatible API.
package groq

import (
	"github.com/looprig/inference"
	"github.com/looprig/inference/auth"
	model "github.com/looprig/inference/model"

	"github.com/looprig/llm"
	"github.com/looprig/llm/providers/internal/simple"
)

const DefaultBaseURL = "https://api.groq.com/openai/v1"

type Option = simple.Option

func New(selected model.Model, key auth.APIKey, options ...Option) (inference.Client, error) {
	return simple.New(selected, key, simple.Definition{
		Provider:       llm.ProviderGroq,
		DefaultBaseURL: DefaultBaseURL,
		DefaultPath:    "/chat/completions",
		Authentication: auth.AuthAPIKey,
	}, options...)
}
