package p302ai

import "github.com/looprig/llm/providers/internal/compat"

// WithHeader adds a documented gateway header for a 302.AI deployment.
func WithHeader(name, value string) Option { return compat.WithHeader(name, value) }
