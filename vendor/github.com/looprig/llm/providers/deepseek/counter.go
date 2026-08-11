package deepseek

import (
	"github.com/looprig/inference/auth"
	contextcount "github.com/looprig/inference/contextcount"

	"github.com/looprig/llm"
	"github.com/looprig/llm/providers/internal/simple"
)

func NewCounter(_ auth.APIKey) (contextcount.ContextCounter, error) {
	return simple.NewCounter(llm.ProviderDeepSeek)
}
