package app

// This compile-only dependency-surface probe covers both the completed rig
// migration and Task 34's context/hustle additions. It builds only when the
// planned core, inference, LLM, harness, and CLI APIs are all present through
// Carbon's retained local replaces. It carries no test functions because its value
// is that the package cannot compile against a partial dependency rollout.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

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
	_ = event.LoopStarted{DisplayName: "carbon"}
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

// TestServeDependenciesArePinned proves carbon names the harness release carrying
// the attach-or-restore fix (wui design §8.1) and a usable wui release. A harness
// below v0.30 restores an already-live session by overwriting the registry entry,
// orphaning every subscriber; and wui v0.1.1 is the first wui whose module zip
// carries the BUILT SPA bundle — v0.1.0 was tagged from a development tree, ships
// only the dist/index.html placeholder, and is retracted upstream, so naming it
// would serve every browser the string "build the app to replace this placeholder"
// with no downstream repair possible (module zips are source-only).
func TestServeDependenciesArePinned(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	gomod := string(data)

	if !strings.Contains(gomod, "github.com/looprig/harness v0.30.") {
		t.Error(`go.mod does not require github.com/looprig/harness v0.30.x`)
	}

	wui := regexp.MustCompile(`github\.com/looprig/wui (v\S+)`).FindStringSubmatch(gomod)
	if wui == nil {
		t.Fatal("go.mod does not require github.com/looprig/wui")
	}
	if wui[1] == "v0.1.0" {
		t.Errorf("go.mod requires the retracted wui %s, which ships the placeholder SPA instead of the built bundle", wui[1])
	}
}
