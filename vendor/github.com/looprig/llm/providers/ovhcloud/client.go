// Package ovhcloud provides the documented ovhcloud OpenAI-compatible API.
package ovhcloud

import (
	"github.com/looprig/inference"
	"github.com/looprig/inference/auth"
	model "github.com/looprig/inference/model"

	"github.com/looprig/llm"
	"github.com/looprig/llm/providers/internal/simple"
)

const DefaultBaseURL = "https://oai.endpoints.kepler.ai.cloud.ovh.net/v1"

type Option = simple.Option

func New(selected model.Model, key auth.APIKey, options ...Option) (inference.Client, error) {
	return simple.New(selected, key, simple.Definition{
		Provider:       llm.ProviderOVHCloud,
		DefaultBaseURL: DefaultBaseURL,
		DefaultPath:    "/chat/completions",
		Authentication: auth.AuthAPIKey,
	}, options...)
}
