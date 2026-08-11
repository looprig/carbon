package p302ai

import (
	"github.com/looprig/inference/auth"
	contextcount "github.com/looprig/inference/contextcount"

	"github.com/looprig/llm"
	"github.com/looprig/llm/providers/internal/compat"
)

// NewCounter reports that 302.AI documents no exact input-token endpoint.
func NewCounter(_ auth.APIKey) (contextcount.ContextCounter, error) {
	return compat.UnsupportedCounter(llm.Provider302AI, "openai")
}
