package app

import (
	"strings"
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

func TestCompileACPCatalogNativeRowsCoexistWithGatewayRows(t *testing.T) {
	compiled, err := CompileACPCatalog(ACPCatalogInput{
		SubagentTypes: []identity.AgentName{"worker"},
		GatewayClients: map[model.ProviderName]inference.Client{
			"openai": &fakeLLM{},
		},
		NativeAuth: []ACPNativeAuthSource{{
			Harness: "codex", Alias: "native-codex-model", Model: testModel(),
			DefaultEffort: model.EffortNone, Efforts: []model.Effort{model.EffortNone},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	entries := compiled.RuntimeCatalog.EntriesFor("worker")
	var codex loop.RuntimeCatalogEntry
	for _, entry := range entries {
		if entry.AgentHarness == "codex" {
			codex = entry
			break
		}
	}
	if codex.AgentHarness != "codex" {
		t.Fatalf("codex entry missing: %#v", entries)
	}
	var nativeFound bool
	for _, option := range codex.Models {
		if option.Alias == "native-codex-model" {
			nativeFound = true
			if option.Credential != loop.CredentialNativeAuth {
				t.Fatalf("native option credential = %q, want %q", option.Credential, loop.CredentialNativeAuth)
			}
			if option.NativeSmallModel != "" {
				t.Fatalf("Codex native small model = %q, want empty", option.NativeSmallModel)
			}
		}
	}
	if !nativeFound {
		t.Fatalf("native option missing from mixed codex entry: %#v", codex.Models)
	}

	native, err := compiled.RuntimeCatalog.Resolve("worker", "codex", "native-codex-model", model.EffortNone)
	if err != nil {
		t.Fatalf("native codex resolve: %v", err)
	}
	if native.Credential != loop.CredentialNativeAuth {
		t.Fatalf("native credential = %q, want %q", native.Credential, loop.CredentialNativeAuth)
	}
	if _, err := compiled.RuntimeCatalog.Resolve("worker", "claude-code", "native-codex-model", model.EffortNone); err == nil {
		t.Fatal("native codex alias resolved under claude-code")
	}
	if _, err := compiled.ResolveGatewayTarget("native-codex-model", model.EffortNone); err == nil {
		t.Fatal("native codex alias unexpectedly has gateway target")
	}
}

func TestCompileACPCatalogCarriesNativeSmallModelIntoRuntimeIdentity(t *testing.T) {
	compiled, err := CompileACPCatalog(ACPCatalogInput{
		SubagentTypes: []identity.AgentName{"worker"},
		NativeAuth: []ACPNativeAuthSource{{
			Harness: "claude-code", Alias: "native-claude-model", Model: testModel(),
			SmallModel: "claude-native-small", DefaultEffort: model.EffortNone,
			Efforts: []model.Effort{model.EffortNone},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := compiled.RuntimeCatalog.Resolve("worker", "claude-code", "native-claude-model", model.EffortNone)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.NativeSmallModel != "claude-native-small" {
		t.Fatalf("NativeSmallModel = %q, want %q", resolved.NativeSmallModel, "claude-native-small")
	}
	if compiled.RuntimeCatalog.Digest() == "" {
		t.Fatal("native catalog digest is empty")
	}
}

func TestCompileACPCatalogRejectsNativeGatewayAliasCollision(t *testing.T) {
	_, err := CompileACPCatalog(ACPCatalogInput{
		SubagentTypes: []identity.AgentName{"worker"},
		GatewayClients: map[model.ProviderName]inference.Client{
			"openai": &fakeLLM{},
		},
		NativeAuth: []ACPNativeAuthSource{{
			Harness: "codex", Alias: "gpt-5.6-luna", Model: testModel(),
			DefaultEffort: model.EffortNone, Efforts: []model.Effort{model.EffortNone},
		}},
	})
	if err == nil {
		t.Fatal("CompileACPCatalog() accepted one alias with native-auth and gateway-backed credentials")
	}
	if !strings.Contains(err.Error(), "credential alias collision") {
		t.Fatalf("CompileACPCatalog() error = %q, want credential alias collision", err)
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
