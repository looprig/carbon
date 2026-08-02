package app

import (
	"testing"

	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/inference"
	"github.com/looprig/inference/model"
)

func TestCompileACPCatalogFrozenMatrix(t *testing.T) {
	clients := map[model.ProviderName]inference.Client{
		"anthropic": &fakeLLM{},
		"openai":    &fakeLLM{},
	}
	first, err := CompileACPCatalog(ACPCatalogInput{SubagentTypes: []identity.AgentName{"worker"}, GatewayClients: clients})
	if err != nil {
		t.Fatalf("CompileACPCatalog() error = %v", err)
	}
	second, err := CompileACPCatalog(ACPCatalogInput{SubagentTypes: []identity.AgentName{"worker"}, GatewayClients: clients})
	if err != nil {
		t.Fatalf("second CompileACPCatalog() error = %v", err)
	}
	if first.RuntimeCatalog.Digest() != second.RuntimeCatalog.Digest() {
		t.Fatalf("digest changed: %q != %q", first.RuntimeCatalog.Digest(), second.RuntimeCatalog.Digest())
	}

	wantAliases := map[loop.ModelAlias]bool{
		"fable-5": false, "sonnet-5": false, "opus-5": false,
		"gpt-5.6-sol": false, "gpt-5.6-terra": false, "gpt-5.6-luna": false,
	}
	entries := first.RuntimeCatalog.EntriesFor("worker")
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	for _, entry := range entries {
		if entry.Credential != loop.CredentialGatewayBacked {
			t.Errorf("%s credential = %q", entry.AgentHarness, entry.Credential)
		}
		if entry.Profile != loop.RuntimeProfileName("acp/"+string(entry.AgentHarness)) {
			t.Errorf("%s profile = %q", entry.AgentHarness, entry.Profile)
		}
		if entry.AgentHarness == "claude-code" && (!entry.NeedsSmallModel || entry.SmallModel != "sonnet-5") {
			t.Errorf("claude small model = %q required=%v", entry.SmallModel, entry.NeedsSmallModel)
		}
		seen := make(map[loop.ModelAlias]bool)
		for _, option := range entry.Models {
			seen[option.Alias] = true
			if option.Alias == "deepseek-v4-flash" || option.Alias == "gemma-4-31b" {
				t.Errorf("primer alias %q leaked into delegate catalog", option.Alias)
			}
			for _, effort := range option.Efforts {
				if effort == "xhigh" || effort == "ultra" {
					t.Errorf("invalid effort %q advertised", effort)
				}
			}
		}
		for alias := range wantAliases {
			if !seen[alias] {
				t.Errorf("%s missing alias %q", entry.AgentHarness, alias)
			}
		}
	}
}

func TestCompileACPCatalogCrossDialectAndAuthoritativeTarget(t *testing.T) {
	compiled, err := CompileACPCatalog(ACPCatalogInput{
		SubagentTypes: []identity.AgentName{"worker"},
		GatewayClients: map[model.ProviderName]inference.Client{
			"anthropic": &fakeLLM{}, "openai": &fakeLLM{},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, harness := range []loop.AgentHarnessName{"codex", "claude-code"} {
		resolved, err := compiled.RuntimeCatalog.Resolve("worker", harness, "gpt-5.6-luna", model.EffortMax)
		if err != nil {
			t.Fatalf("Resolve(%s, luna@max): %v", harness, err)
		}
		if resolved.Target.Provider != "openai" || resolved.Effort != model.EffortMax {
			t.Fatalf("Resolve(%s) = provider %q effort %q", harness, resolved.Target.Provider, resolved.Effort)
		}
	}
	target, err := compiled.ResolveGatewayTarget("gpt-5.6-luna", model.EffortMax)
	if err != nil {
		t.Fatal(err)
	}
	if !target.AuthoritativeEffort || target.Model.Sampling.Effort != model.EffortMax || target.Client == nil {
		t.Fatalf("target = %#v", target)
	}
}

func TestCompileACPCatalogCredentialGatingAndNativeFallback(t *testing.T) {
	compiled, err := CompileACPCatalog(ACPCatalogInput{
		SubagentTypes: []identity.AgentName{"worker"},
		NativeAuth: []ACPNativeAuthSource{{
			Harness: "codex", Alias: "codex-native", Model: testModel(),
			Efforts: []model.Effort{model.EffortLow, model.EffortHigh}, DefaultEffort: model.EffortHigh,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := compiled.RuntimeCatalog.Resolve("worker", "codex", "codex-native", model.EffortHigh)
	if err != nil {
		t.Fatalf("native codex resolve: %v", err)
	}
	if resolved.Credential != loop.CredentialNativeAuth {
		t.Fatalf("credential = %q", resolved.Credential)
	}
	if _, err := compiled.RuntimeCatalog.Resolve("worker", "claude-code", "codex-native", model.EffortHigh); err == nil {
		t.Fatal("codex native alias resolved under claude-code")
	}
	if _, err := compiled.ResolveGatewayTarget("codex-native", model.EffortHigh); err == nil {
		t.Fatal("native-auth alias unexpectedly has gateway target")
	}
	for _, alias := range []loop.ModelAlias{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"} {
		if _, err := compiled.RuntimeCatalog.Resolve("worker", "codex", alias, model.EffortHigh); err == nil {
			t.Errorf("unconfigured OpenAI alias %q resolved", alias)
		}
	}
}

func TestCompileACPCatalogExtraProvider(t *testing.T) {
	extraClient := &fakeLLM{}
	extraModel := model.CustomModel("openrouter", model.APIFormatOpenAI, "https://openrouter.ai/api/v1", "vendor/model", model.WithTools())
	compiled, err := CompileACPCatalog(ACPCatalogInput{
		SubagentTypes: []identity.AgentName{"worker"},
		ExtraGatewayTargets: []ACPGatewaySource{{
			Alias: "extra-reasoner", Client: extraClient, Model: extraModel,
			Efforts: []model.Effort{model.EffortNone, model.EffortHigh}, DefaultEffort: model.EffortHigh,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := compiled.RuntimeCatalog.Resolve("worker", "codex", "extra-reasoner", model.EffortHigh)
	if err != nil {
		t.Fatalf("Resolve(codex, extra): %v", err)
	}
	if resolved.Target.Provider != "openrouter" {
		t.Fatalf("provider = %q", resolved.Target.Provider)
	}
}
