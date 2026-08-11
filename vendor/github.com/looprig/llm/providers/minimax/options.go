package minimax

import "github.com/looprig/llm/providers/internal/simple"

func WithHeader(name, value string) Option { return simple.WithHeader(name, value) }

func WithThinkingBudget(budget int) Option { return simple.WithThinkingBudget(budget) }
