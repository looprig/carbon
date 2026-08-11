// Package minimax provides MiniMax's native Anthropic Messages endpoint.
package minimax

import (
	"github.com/looprig/inference"
	"github.com/looprig/inference/auth"
	model "github.com/looprig/inference/model"

	"github.com/looprig/llm"
	"github.com/looprig/llm/providers/internal/simple"
)

const DefaultBaseURL = "https://api.minimax.io/anthropic/v1"

type Option = simple.Option

func New(selected model.Model, key auth.APIKey, options ...Option) (inference.Client, error) {
	defaults := []Option{simple.WithHeader("anthropic-version", "2023-06-01")}
	defaults = append(defaults, options...)
	return simple.New(selected, key, simple.Definition{
		Provider:       llm.ProviderMiniMax,
		DefaultBaseURL: DefaultBaseURL,
		DefaultPath:    "/messages",
		Authentication: auth.AuthAPIKey,
		KeyHeader:      "x-api-key",
	}, defaults...)
}
