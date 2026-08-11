package zenmux

import (
	"github.com/looprig/inference/auth"
	contextcount "github.com/looprig/inference/contextcount"
	model "github.com/looprig/inference/model"

	"github.com/looprig/llm"
	"github.com/looprig/llm/providers/internal/compat"
)

func NewCounter(_ auth.APIKey) (contextcount.ContextCounter, error) {
	return compat.UnsupportedCounter(llm.ProviderZenMux, model.APIFormatOpenAI)
}
