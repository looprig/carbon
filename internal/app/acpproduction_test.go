package app

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/looprig/acp/launch"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/inference/model"
)

func TestPreflightProductionACPExecutableEnforcesAdapterSpecificSelectors(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		harness    loop.AgentHarnessName
		credential loop.CredentialMode
		model      string
		smallModel string
		proxy      bool
		helperPath string
		wantReady  bool
	}{
		{
			name:    "Claude gateway requires both selectors",
			harness: "claude-code", credential: loop.CredentialGatewayBacked,
			model: "sonnet-5@high", smallModel: "sonnet-5", proxy: true,
			helperPath: task33ACPHelperPath, wantReady: true,
		},
		{
			name:    "Claude gateway rejects a missing small selector",
			harness: "claude-code", credential: loop.CredentialGatewayBacked,
			model: "sonnet-5@high", proxy: true,
			helperPath: task33ACPHelperPath,
		},
		{
			name:    "Codex gateway permits only the main selector",
			harness: "codex", credential: loop.CredentialGatewayBacked,
			model: "gpt-5.6-luna", proxy: true,
			helperPath: task33ACPHelperPath, wantReady: true,
		},
		{
			name:    "Codex gateway rejects a small selector",
			harness: "codex", credential: loop.CredentialGatewayBacked,
			model: "gpt-5.6-luna", smallModel: "small", proxy: true,
			helperPath: task33ACPHelperPath,
		},
		{
			name:    "Claude native managed",
			harness: "claude-code", credential: loop.CredentialNativeAuth,
			helperPath: task33NativeClaudeACPHelperPath, wantReady: true,
		},
		{
			name:    "Claude native explicit",
			harness: "claude-code", credential: loop.CredentialNativeAuth,
			model: "sonnet-5@high", smallModel: "sonnet-5",
			helperPath: task33NativeClaudeACPHelperPath, wantReady: true,
		},
		{
			name:    "Claude native rejects a partial selector",
			harness: "claude-code", credential: loop.CredentialNativeAuth,
			model:      "sonnet-5@high",
			helperPath: task33NativeClaudeACPHelperPath,
		},
		{
			name:    "Codex native managed",
			harness: "codex", credential: loop.CredentialNativeAuth,
			helperPath: task33NativeCodexACPHelperPath, wantReady: true,
		},
		{
			name:    "Codex native explicit",
			harness: "codex", credential: loop.CredentialNativeAuth,
			model:      "gpt-5.6-luna",
			helperPath: task33NativeCodexACPHelperPath, wantReady: true,
		},
		{
			name:    "Codex native rejects a small selector",
			harness: "codex", credential: loop.CredentialNativeAuth,
			model: "gpt-5.6-luna", smallModel: "small",
			helperPath: task33NativeCodexACPHelperPath,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			probe := ACPExecutableProbe{
				ACPNativeAuthProbe: ACPNativeAuthProbe{
					Harness:       tt.harness,
					Executable:    executable,
					WorkspaceRoot: t.TempDir(),
					Env:           []string{"PATH=" + tt.helperPath},
				},
				Credential: tt.credential,
				Model:      tt.model,
				SmallModel: tt.smallModel,
			}
			if tt.proxy {
				probe.SharedProxy = &launch.ProxyBinding{BaseURL: "http://127.0.0.1:1", Token: "preflight-token"}
			}
			result := preflightProductionACPExecutable(context.Background(), probe)
			if result.Ready != tt.wantReady {
				t.Fatalf("preflight result = %#v, want Ready=%v", result, tt.wantReady)
			}
			if tt.wantReady && tt.harness == "claude-code" && len(result.AdvertisedModels) == 0 {
				t.Fatalf("Claude preflight advertised no model selectors: %#v", result)
			}
		})
	}
}

func TestProductionACPCompositionUsesOnlyConfiguredGatewayRows(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(acpCodexExecutableEnv, executable)
	t.Setenv(acpClaudeExecutableEnv, executable)
	t.Setenv("ANTHROPIC_API_KEY", "must-not-create-a-row")
	t.Setenv("OPENAI_API_KEY", "must-not-create-a-row")
	t.Setenv("CLAUDE_CODE_ACP_NATIVE_MODELS", "native=fixed-sonnet")
	t.Setenv("CODEX_ACP_NATIVE_MODELS", "native=fixed-gpt")

	configured := configuredProductionModelsForTest("configured-only")
	composition, err := newProductionACPCompositionWithPreflight(context.Background(), configured, func(_ context.Context, probe ACPExecutableProbe) ACPPreflightResult {
		if probe.Credential != loop.CredentialGatewayBacked {
			t.Fatalf("production preflight credential = %q, want gateway-backed", probe.Credential)
		}
		if probe.Model != "configured-only" {
			t.Fatalf("production preflight model = %q, want exact configured default", probe.Model)
		}
		if probe.Harness == "claude-code" && probe.SmallModel != "configured-only" {
			t.Fatalf("Claude small model = %q, want exact configured alias", probe.SmallModel)
		}
		for _, entry := range probe.Env {
			name, _, _ := strings.Cut(entry, "=")
			switch name {
			case "ANTHROPIC_API_KEY", "OPENAI_API_KEY", "CLAUDE_CODE_ACP_NATIVE_MODELS", "CODEX_ACP_NATIVE_MODELS", "HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME":
				t.Fatalf("production gateway child env contains forbidden %s", name)
			}
		}
		return ACPPreflightResult{Ready: true, AdvertisedModels: append([]string(nil), probe.Models...)}
	})
	if err != nil {
		t.Fatal(err)
	}
	entries := composition.Catalog.RuntimeCatalog.EntriesFor("builder")
	if len(entries) != 2 {
		t.Fatalf("builder entries = %#v, want configured Claude and Codex entries", entries)
	}
	for _, entry := range entries {
		if entry.Credential != loop.CredentialGatewayBacked || entry.DefaultModel != "configured-only" {
			t.Fatalf("production entry = %#v, want configured gateway default", entry)
		}
		if len(entry.Models) != 1 || entry.Models[0].Alias != "configured-only" {
			t.Fatalf("production models = %#v, want only configured-only", entry.Models)
		}
	}
}

func TestProductionACPCompositionWiresConfiguredManagedNativeProfile(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(acpClaudeExecutableEnv, executable)
	t.Setenv(acpCodexExecutableEnv, executable)

	configured := configuredProductionModelsForTest("configured-only")
	configured.NativeACP = map[string]ACPNativeProfile{
		"codex": {Harness: "codex", Enabled: true},
	}
	for role := range configured.Defaults {
		configured.Defaults[role] = configuredDelegateDefault{Harness: "codex", Source: loop.RuntimeSourceNative}
	}
	var nativeProbes int
	composition, err := newProductionACPCompositionWithPreflight(context.Background(), configured, func(_ context.Context, probe ACPExecutableProbe) ACPPreflightResult {
		if probe.Credential == loop.CredentialNativeAuth {
			nativeProbes++
			if probe.SharedProxy != nil || probe.Model != "" || probe.SmallModel != "" || len(probe.Models) != 0 {
				t.Fatalf("managed native production probe = %#v", probe)
			}
			return ACPPreflightResult{Ready: true}
		}
		return ACPPreflightResult{Ready: true, AdvertisedModels: append([]string(nil), probe.Models...)}
	})
	if err != nil {
		t.Fatal(err)
	}
	if nativeProbes != 1 {
		t.Fatalf("managed native production probes = %d, want 1", nativeProbes)
	}
	entries := composition.Catalog.RuntimeCatalog.EntriesFor("builder")
	var managed loop.RuntimeCatalogEntry
	for _, entry := range entries {
		if entry.Source == loop.RuntimeSourceNative {
			managed = entry
			break
		}
	}
	if managed.SelectionKind != loop.RuntimeSelectionHarnessManaged || len(managed.Models) != 0 || managed.DefaultModel != "" || managed.SmallModel != "" {
		t.Fatalf("production managed native entry = %#v", managed)
	}
}

func TestProductionACPCompositionDoesNotFallbackWhenConfiguredDefaultHarnessIsUnavailable(t *testing.T) {
	t.Setenv(acpCodexExecutableEnv, "")
	t.Setenv(acpClaudeExecutableEnv, "")

	configured := configuredProductionModelsForTest("configured-only")
	for role := range configured.Defaults {
		configured.Defaults[role] = configuredDelegateDefault{Harness: "codex", Model: "configured-only", Effort: model.EffortNone}
	}
	composition, err := newProductionACPCompositionWithPreflight(context.Background(), configured, func(_ context.Context, probe ACPExecutableProbe) ACPPreflightResult {
		return ACPPreflightResult{Ready: true, AdvertisedModels: append([]string(nil), probe.Models...)}
	})
	if err != nil {
		t.Fatalf("unavailable configured default blocked native startup: %v", err)
	}
	if composition == nil {
		t.Fatal("unavailable configured default returned nil composition")
	}
	if entries := composition.Catalog.RuntimeCatalog.EntriesFor("builder"); len(entries) != 0 {
		t.Fatalf("unavailable configured default fell back to another harness: %#v", entries)
	}
}

func configuredProductionModelsForTest(alias loop.ModelAlias) productionModels {
	selected := model.CustomModel("lmstudio", model.APIFormatOpenAI, "http://localhost:1234/v1", "configured-provider-model", model.WithTools())
	defaults := make(map[identity.AgentName]configuredDelegateDefault, 3)
	for _, role := range []identity.AgentName{"planner", "builder", "reviewer"} {
		defaults[role] = configuredDelegateDefault{Harness: "codex", Model: alias, Effort: model.EffortNone}
	}
	return productionModels{
		PrimerClient: &fakeLLM{},
		PrimerModel:  selected,
		ACP: []ACPGatewaySource{{
			Alias: alias, Client: &fakeLLM{}, Model: selected,
			DefaultEffort: model.EffortNone, Efforts: []model.Effort{model.EffortNone},
		}},
		Defaults:    defaults,
		ClaudeSmall: alias,
		ConfigRev:   "configured-revision",
	}
}
