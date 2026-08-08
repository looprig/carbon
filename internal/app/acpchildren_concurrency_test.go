package app

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

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
	compiled, err := CompileAgentRuntimeCatalog(AgentRuntimeCatalogInput{
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
		ClaudeSmall: "shared-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	return compiled
}

// TestNewACPCompositionDoesNotStartHarnessPreflights proves composition does
// not launch either configured ACP executable merely to advertise a profile.
func TestNewACPCompositionDoesNotStartHarnessPreflights(t *testing.T) {
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
	if claudeCalls != 0 || codexCalls != 0 {
		t.Fatalf("preflight call counts = claude-code:%d codex:%d, want zero during composition", claudeCalls, codexCalls)
	}

	if elapsed >= acpConcurrentPreflightBudget {
		t.Fatalf("NewACPComposition performed unexpected startup work: elapsed=%v", elapsed)
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
	compiled, err := CompileAgentRuntimeCatalog(AgentRuntimeCatalogInput{
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
