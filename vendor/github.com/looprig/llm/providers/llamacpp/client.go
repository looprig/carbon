// Package llamacpp provides the local llama.cpp llama-server OpenAI-compatible API.
//
// The canonical model identity is llm.ProviderLlamaCPP ("llama.cpp"), matching
// OpenCode's documented custom local provider. The constructor still accepts
// the former llm.ProviderLlama identity for callers that explicitly selected
// this package before the hosted Meta Llama provider was added.
package llamacpp

import (
	"github.com/looprig/inference"
	"github.com/looprig/inference/auth"
	model "github.com/looprig/inference/model"

	"github.com/looprig/llm"
	"github.com/looprig/llm/providers/internal/simple"
)

const DefaultBaseURL = "http://127.0.0.1:8080/v1"

type Option = simple.Option

func New(selected model.Model, key auth.APIKey, options ...Option) (inference.Client, error) {
	provider := llm.ProviderLlamaCPP
	if selected.Provider == model.ProviderName(llm.ProviderLlama) {
		provider = llm.ProviderLlama
	}
	return simple.New(selected, key, simple.Definition{
		Provider:       provider,
		DefaultBaseURL: DefaultBaseURL,
		DefaultPath:    "/chat/completions",
		Authentication: auth.AuthNone,
	}, options...)
}
