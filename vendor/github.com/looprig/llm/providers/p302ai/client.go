// Package p302ai provides the 302.AI OpenAI-compatible Chat Completions API.
package p302ai

import (
	"github.com/looprig/inference"
	"github.com/looprig/inference/auth"
	model "github.com/looprig/inference/model"

	"github.com/looprig/llm"
	"github.com/looprig/llm/providers/internal/compat"
)

const defaultBaseURL = "https://api.302.ai/v1"

// Option customizes documented 302.AI request behavior.
type Option = compat.Option

// WithReasoningEffort is intentionally not exposed: 302.AI's current API guide
// documents model/messages but no shared reasoning control. The package keeps
// the option surface empty until the provider documents one.

// New constructs a 302.AI Chat Completions client.
func New(selected model.Model, key auth.APIKey, options ...Option) (inference.Client, error) {
	return compat.NewProvider(selected, key, compat.Definition{
		Provider:       llm.Provider302AI,
		DefaultBaseURL: defaultBaseURL,
		DefaultPath:    "/chat/completions",
		Authentication: auth.AuthAPIKey,
	}, options...)
}
