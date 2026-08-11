package llama

import (
	"github.com/looprig/inference/auth"
	contextcount "github.com/looprig/inference/contextcount"

	"github.com/looprig/llm"
	"github.com/looprig/llm/providers/internal/simple"
)

// NewCounter reports that the hosted Llama compatibility API documents no
// exact input-token counting endpoint in this package's supported contract.
func NewCounter(_ auth.APIKey) (contextcount.ContextCounter, error) {
	return simple.NewCounter(llm.ProviderLlama)
}
