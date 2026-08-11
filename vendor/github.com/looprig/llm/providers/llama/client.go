// Package llama provides Meta's hosted Llama API through its documented
// OpenAI-compatible Chat Completions endpoint.
package llama

import (
	"os"
	"strings"

	"github.com/looprig/inference"
	"github.com/looprig/inference/auth"
	model "github.com/looprig/inference/model"

	"github.com/looprig/llm"
	"github.com/looprig/llm/providers/internal/simple"
)

const (
	DefaultBaseURL    = "https://api.llama.com/compat/v1"
	APIKeyEnvironment = "LLAMA_API_KEY" // #nosec G101 -- environment variable name, not a credential value
)

type Option = simple.Option

// New constructs a hosted Meta Llama Chat Completions client. An explicit key
// takes precedence; when it is empty, the documented LLAMA_API_KEY environment
// variable is used.
func New(selected model.Model, key auth.APIKey, options ...Option) (inference.Client, error) {
	if key == "" {
		key = auth.APIKey(strings.TrimSpace(os.Getenv(APIKeyEnvironment)))
	}
	return simple.New(selected, key, simple.Definition{
		Provider:       llm.ProviderLlama,
		DefaultBaseURL: DefaultBaseURL,
		DefaultPath:    "/chat/completions",
		Authentication: auth.AuthAPIKey,
	}, options...)
}
