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

// acpPreflightSleep is the deterministic per-call delay used by the
// composition-only fake below.
const acpPreflightSleep = 50 * time.Millisecond

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

	composition, err := NewACPComposition(ACPChildrenConfig{
		Catalog:             compiled,
		Executables:         map[loop.AgentHarnessName]string{"claude-code": executable, "codex": executable},
		WorkspaceRoot:       t.TempDir(),
		executablePreflight: fake,
	})
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
	bothStarted := make(chan struct{})
	var signalBothStarted sync.Once
	fake := func(ctx context.Context, probe ACPExecutableProbe) ACPPreflightResult {
		mu.Lock()
		calls[probe.Credential]++
		if calls[loop.CredentialGatewayBacked]+calls[loop.CredentialNativeAuth] == 2 {
			signalBothStarted.Do(func() { close(bothStarted) })
		}
		mu.Unlock()

		// Neither call may finish until both have started. A sequential
		// implementation therefore cannot satisfy the barrier and fails via
		// the bounded context instead of relying on scheduler-sensitive timing.
		select {
		case <-bothStarted:
		case <-ctx.Done():
			return ACPPreflightResult{}
		}
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

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	decision := preflightACPProfile(ctx, config, "claude-code", fake)

	if !decision.gatewayReady || !decision.nativeManagedReady {
		t.Fatalf("expected both gateway and native-managed preflight to succeed: %#v", decision)
	}
	mu.Lock()
	gatewayCalls, nativeCalls := calls[loop.CredentialGatewayBacked], calls[loop.CredentialNativeAuth]
	mu.Unlock()
	if gatewayCalls != 1 || nativeCalls != 1 {
		t.Fatalf("preflight call counts = gateway:%d native:%d, want exactly one each", gatewayCalls, nativeCalls)
	}
}
