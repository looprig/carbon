package app

// This compile-only dependency-surface probe covers both the completed rig
// migration and Task 34's context/hustle additions. It builds only when the
// planned core, inference, LLM, harness, and CLI APIs are all present through
// CodeRig's retained local replaces. It carries no test functions because its value
// is that the package cannot compile against a partial dependency rollout.

import (
	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/rig"
	"github.com/looprig/harness/pkg/session"
	"github.com/looprig/inference/contextcount"

	model "github.com/looprig/inference/model"
	"github.com/looprig/llm"
	"github.com/looprig/tui"
)

var (
	_ = rig.Define
	_ = loop.Define
	_ session.SessionController
	_ = event.ActiveLoopChanged{}
	_ = event.LoopStarted{DisplayName: "generic"}
	_ tui.Agent
	_ = loop.WithDisplayName
	_ = rig.WithOffloadGC

	_ content.TokenCount = 1
	_                    = model.ContextLimits{WindowTokens: 1}
	_                    = contextcount.InferenceCapability{}
	_                    = contextcount.CounterCapability{}
	_                    = contextcount.NewEstimator
	_                    = llm.ProviderChutes
	_                    = llm.ProviderPhala
	_                    = llm.ProviderLMStudio

	_                       = command.Compact{}
	_                       = event.ContextMeasured{}
	_                       = event.CompactionStarted{}
	_                       = event.CompactionCommitted{}
	_                       = event.CompactionRejected{}
	_                       = event.HustleStarted{}
	_                       = event.HustleCompleted{}
	_                       = event.HustleFailed{}
	_ event.EventVisibility = event.Public
	_                       = event.ShouldDeliver
	_                       = hustle.Define
	_                       = loop.WithContextCounter
	_                       = loop.WithInferenceCapability
	_                       = loop.WithContextObservation
	_                       = loop.WithCompaction
	_                       = rig.WithHustles
	_                       = rig.WithHustleLimits
)
