package llama

import "github.com/looprig/llm/providers/internal/simple"

// WithHeader adds a provider or gateway header to requests.
func WithHeader(name, value string) Option { return simple.WithHeader(name, value) }

// WithReasoningEffort adds the documented OpenAI-compatible reasoning_effort
// request field for models that support reasoning controls.
func WithReasoningEffort(effort string) Option { return simple.WithReasoningEffort(effort) }

// WithServiceTier adds the documented OpenAI-compatible service_tier field.
func WithServiceTier(tier string) Option { return simple.WithServiceTier(tier) }
