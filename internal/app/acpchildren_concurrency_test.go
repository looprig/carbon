package app

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	model "github.com/looprig/inference/model"
)

// acpPreflightSleep is the deterministic per-call delay used by the fakes
// below. It is small enough to keep the suite fast but large enough that
// scheduling jitter cannot plausibly account for the sequential-vs-
// concurrent gap being measured (a sequential pair takes roughly 2x this,
// a concurrent pair roughly 1x).
const acpPreflightSleep = 50 * time.Millisecond

// acpConcurrentPreflightBudget is the generous upper bound asserted for a
// truly concurrent pair of acpPreflightSleep-length preflights. It sits well
// under the ~100ms a sequential pair would take, and well above the ~50ms a
// concurrent pair actually takes, so ordinary CI scheduling jitter cannot
// flip the assertion.
const acpConcurrentPreflightBudget = 90 * time.Millisecond

// testACPMinimalGatewayCatalog compiles a catalog with exactly one
// gateway-backed model alias shared by both harnesses, so preflighting
// claude-code and codex against it each issues exactly one call to a
// preflight fake: claude-code's shared-gateway path always issues a single
// combined (Model, SmallModel) call regardless of alias count, and with a
// single alias configured, codex's per-alias loop also issues exactly one
// call. That makes wall-clock timing assertions exact instead of dependent
// on how many aliases a broader fixture happens to configure.
func testACPMinimalGatewayCatalog(t *testing.T) ACPCompiledCatalog {
	t.Helper()
	compiled, err := CompileACPCatalog(ACPCatalogInput{
		AgentTypes: []identity.AgentName{"worker"},
		GatewayTargets: []ACPGatewaySource{{
			Alias:  "shared-model",
			Client: &fakeLLM{},
			Model: model.CustomModel(
				"anthropic", model.APIFormatAnthropic, "", "shared-model",
				model.WithTools(), model.WithThinking(),
			),
			DefaultEffort: model.EffortMedium,
			Efforts:       []model.Effort{model.EffortMedium},
		}},
		Defaults: map[identity.AgentName]configuredDelegateDefault{
			"worker": {Harness: "claude-code", Model: "shared-model", Effort: model.EffortMedium},
		},
		ClaudeSmall: "shared-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	return compiled
}

// TestNewACPCompositionPreflightsHarnessesConcurrently proves the Level 1
// win described in the parallelization brief: claude-code's and codex's
// preflights run on independent goroutines, so total wall-clock stays near
// the slower one instead of their sum. Before the fix, NewACPComposition
// preflighted the two fixed harnesses in a strictly sequential loop, so this
// same shape (two different harnesses, each with a deterministic
// acpPreflightSleep-length fake) would have taken roughly 2x
// acpPreflightSleep; it now takes roughly 1x.
func TestNewACPCompositionPreflightsHarnessesConcurrently(t *testing.T) {
	t.Parallel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	compiled := testACPMinimalGatewayCatalog(t)

	var mu sync.Mutex
	calls := make(map[loop.AgentHarnessName]int)
	fake := func(_ context.Context, probe ACPExecutableProbe) ACPPreflightResult {
		mu.Lock()
		calls[probe.Harness]++
		mu.Unlock()
		time.Sleep(acpPreflightSleep)
		if probe.Harness == "claude-code" {
			return ACPPreflightResult{Ready: true, AdvertisedModels: []string{"shared-model"}}
		}
		return ACPPreflightResult{Ready: true}
	}

	start := time.Now()
	composition, err := NewACPComposition(ACPChildrenConfig{
		Catalog:             compiled,
		Executables:         map[loop.AgentHarnessName]string{"claude-code": executable, "codex": executable},
		WorkspaceRoot:       t.TempDir(),
		executablePreflight: fake,
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := composition.Registry.Builder("acp/claude-code"); err != nil {
		t.Fatalf("claude-code profile missing: %v", err)
	}
	if _, _, err := composition.Registry.Builder("acp/codex"); err != nil {
		t.Fatalf("codex profile missing: %v", err)
	}

	mu.Lock()
	claudeCalls, codexCalls := calls["claude-code"], calls["codex"]
	mu.Unlock()
	if claudeCalls != 1 || codexCalls != 1 {
		t.Fatalf("preflight call counts = claude-code:%d codex:%d, want exactly one each", claudeCalls, codexCalls)
	}

	if elapsed >= acpConcurrentPreflightBudget {
		t.Fatalf(
			"NewACPComposition preflighted claude-code and codex sequentially: elapsed=%v, want well under %v (2x%v sequential sum)",
			elapsed, acpConcurrentPreflightBudget, acpPreflightSleep,
		)
	}
}

// TestPreflightACPProfileRunsGatewayAndNativeManagedConcurrently proves the
// Level 2 win: within one harness, the gateway preflight and the
// harness-managed native preflight run on independent goroutines and write
// disjoint decision state, so their wall-clock cost is also the max of the
// two, not the sum. Before the fix, preflightACPProfile ran these two calls
// strictly sequentially.
func TestPreflightACPProfileRunsGatewayAndNativeManagedConcurrently(t *testing.T) {
	t.Parallel()
	compiled, err := CompileACPCatalog(ACPCatalogInput{
		AgentTypes: []identity.AgentName{"worker"},
		GatewayTargets: []ACPGatewaySource{{
			Alias:  "shared-model",
			Client: &fakeLLM{},
			Model: model.CustomModel(
				"anthropic", model.APIFormatAnthropic, "", "shared-model",
				model.WithTools(), model.WithThinking(),
			),
			DefaultEffort: model.EffortMedium,
			Efforts:       []model.Effort{model.EffortMedium},
		}},
		Defaults: map[identity.AgentName]configuredDelegateDefault{
			"worker": {Harness: "claude-code", Model: "shared-model", Effort: model.EffortMedium},
		},
		ClaudeSmall: "shared-model",
		NativeACP:   map[string]ACPNativeProfile{"claude-code": {Enabled: true}},
	})
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	calls := make(map[loop.CredentialMode]int)
	fake := func(_ context.Context, probe ACPExecutableProbe) ACPPreflightResult {
		mu.Lock()
		calls[probe.Credential]++
		mu.Unlock()
		time.Sleep(acpPreflightSleep)
		if probe.Credential == loop.CredentialGatewayBacked {
			return ACPPreflightResult{Ready: true, AdvertisedModels: []string{"shared-model"}}
		}
		return ACPPreflightResult{Ready: true}
	}
	config := ACPChildrenConfig{
		Catalog:             compiled,
		WorkspaceRoot:       "/workspace/project",
		executablePreflight: fake,
	}

	start := time.Now()
	decision := preflightACPProfile(context.Background(), config, "claude-code", fake)
	elapsed := time.Since(start)

	if !decision.gatewayReady || !decision.nativeManagedReady {
		t.Fatalf("expected both gateway and native-managed preflight to succeed: %#v", decision)
	}
	mu.Lock()
	gatewayCalls, nativeCalls := calls[loop.CredentialGatewayBacked], calls[loop.CredentialNativeAuth]
	mu.Unlock()
	if gatewayCalls != 1 || nativeCalls != 1 {
		t.Fatalf("preflight call counts = gateway:%d native:%d, want exactly one each", gatewayCalls, nativeCalls)
	}

	if elapsed >= acpConcurrentPreflightBudget {
		t.Fatalf(
			"preflightACPProfile ran the gateway and native-managed preflights sequentially: elapsed=%v, want well under %v (2x%v sequential sum)",
			elapsed, acpConcurrentPreflightBudget, acpPreflightSleep,
		)
	}
}
