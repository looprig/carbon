package app

import (
	"context"
	"os"
	"testing"

	"github.com/looprig/coderig/internal/catalog/generic"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	model "github.com/looprig/inference/model"
)

func TestCompileAgentRuntimeCatalogCompilesNativeProfilesAlongsideGateway(t *testing.T) {
	gateway := fixtureGatewaySource("gateway-model", &fakeLLM{})
	compiled, err := CompileAgentRuntimeCatalog(AgentRuntimeCatalogInput{
		GatewayTargets: []ACPGatewaySource{gateway},
		NativeACP: map[string]ACPNativeProfile{
			"codex":       {Harness: "codex", Enabled: true, Models: []loop.ModelAlias{"native-z", "native-a"}},
			"claude-code": {Harness: "claude-code", Enabled: false, Models: []loop.ModelAlias{"disabled-only"}},
		},
		ClaudeSmall: "gateway-model",
	})
	if err != nil {
		t.Fatalf("CompileAgentRuntimeCatalog() error = %v", err)
	}

	genericNative, err := compiled.RuntimeCatalog.ResolveWithExplicitSource(generic.Name, "codex", loop.RuntimeSourceNative, "native-a", model.EffortNone, false)
	if err != nil {
		t.Fatalf("resolve explicit native source: %v", err)
	}
	if genericNative.Source != loop.RuntimeSourceNative || genericNative.SelectionKind != loop.RuntimeSelectionExplicit || genericNative.ModelAlias != "native-a" || genericNative.Credential != loop.CredentialNativeAuth {
		t.Fatalf("explicit native resolution = %#v", genericNative)
	}
	if genericNative.Target.Name == "" || genericNative.Target.Name == string(genericNative.ModelAlias) {
		t.Fatalf("explicit native target = %#v, want opaque non-alias identity", genericNative.Target)
	}

	genericDefault, err := compiled.RuntimeCatalog.Resolve(generic.Name, "", "", model.EffortNone)
	if err != nil {
		t.Fatalf("resolve omitted-source gateway default: %v", err)
	}
	if genericDefault.AgentHarness != looprigRuntimeHarness || genericDefault.Source != loop.RuntimeSourceNative || genericDefault.Credential != loop.CredentialNativeAuth {
		t.Fatalf("deterministic resolution = %#v, want looprig/native default", genericDefault)
	}

	for _, entry := range compiled.RuntimeCatalog.EntriesFor(generic.Name) {
		if entry.AgentHarness == "claude-code" && entry.Source == loop.RuntimeSourceNative {
			t.Fatalf("disabled Claude profile created native entry: %#v", entry)
		}
	}
}

func TestCompileAgentRuntimeCatalogRejectsNativeAliasCollisionWithGateway(t *testing.T) {
	_, err := CompileAgentRuntimeCatalog(AgentRuntimeCatalogInput{
		GatewayTargets: []ACPGatewaySource{fixtureGatewaySource("shared", &fakeLLM{})},
		NativeACP:      map[string]ACPNativeProfile{"codex": {Harness: "codex", Enabled: true, Models: []loop.ModelAlias{"shared"}}},
		ClaudeSmall:    "shared",
	})
	if err == nil {
		t.Fatal("CompileAgentRuntimeCatalog() succeeded with native/gateway alias collision")
	}
}

func TestNewACPCompositionPreflightsManagedNativeWithoutProxyOrModel(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := CompileAgentRuntimeCatalog(AgentRuntimeCatalogInput{
		NativeACP: map[string]ACPNativeProfile{
			"codex": {Harness: "codex", Enabled: true},
		},
		PrimerTarget: runtimeCatalogPrimer(),
	})
	if err != nil {
		t.Fatalf("CompileAgentRuntimeCatalog() error = %v", err)
	}
	var probes []ACPExecutableProbe
	composition, err := NewACPComposition(ACPChildrenConfig{
		Catalog:            compiled,
		Executables:        map[loop.AgentHarnessName]string{"codex": executable},
		WorkspaceRoot:      "/workspace/project",
		Env:                []string{"HOME=/private/login", "PATH=/usr/bin", "SECRET=do-not-pass"},
		NativeEnvAllowlist: []string{"HOME", "PATH"},
		executablePreflight: func(_ context.Context, probe ACPExecutableProbe) ACPPreflightResult {
			probes = append(probes, probe)
			if probe.Credential != loop.CredentialNativeAuth || probe.SharedProxy != nil || probe.Model != "" || probe.SmallModel != "" || len(probe.Models) != 0 {
				return ACPPreflightResult{}
			}
			return ACPPreflightResult{Ready: true}
		},
	})
	if err != nil {
		t.Fatalf("NewACPComposition() error = %v", err)
	}
	if len(probes) != 1 {
		t.Fatalf("managed native probes = %#v, want one model-less probe", probes)
	}
	if !composition.Catalog.HasProfile("acp/codex") {
		t.Fatal("managed native profile was removed after successful preflight")
	}
	entries := composition.Catalog.RuntimeCatalog.EntriesFor(generic.Name)
	var managed *loop.RuntimeCatalogEntry
	for i := range entries {
		if entries[i].AgentHarness == "codex" && entries[i].SelectionKind == loop.RuntimeSelectionHarnessManaged {
			managed = &entries[i]
		}
	}
	if managed == nil || len(managed.Models) != 0 {
		t.Fatalf("managed native filtered entries = %#v", entries)
	}
}

func TestNewACPCompositionPreflightsExplicitNativeAliasesWithoutGateway(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := CompileAgentRuntimeCatalog(AgentRuntimeCatalogInput{
		NativeACP: map[string]ACPNativeProfile{
			"codex": {Harness: "codex", Enabled: true, Models: []loop.ModelAlias{"native-z", "native-a"}},
		},
		PrimerTarget: runtimeCatalogPrimer(),
	})
	if err != nil {
		t.Fatalf("CompileAgentRuntimeCatalog() error = %v", err)
	}
	var probes []ACPExecutableProbe
	composition, err := NewACPComposition(ACPChildrenConfig{
		Catalog:       compiled,
		Executables:   map[loop.AgentHarnessName]string{"codex": executable},
		WorkspaceRoot: "/workspace/project",
		executablePreflight: func(_ context.Context, probe ACPExecutableProbe) ACPPreflightResult {
			probes = append(probes, probe)
			if probe.Credential != loop.CredentialNativeAuth || probe.SharedProxy != nil || probe.Model == "" || len(probe.Models) != 1 || probe.Models[0] != probe.Model {
				return ACPPreflightResult{}
			}
			return ACPPreflightResult{Ready: true}
		},
	})
	if err != nil {
		t.Fatalf("NewACPComposition() error = %v", err)
	}
	if len(probes) != 2 || probes[0].Model != "native-a" || probes[1].Model != "native-z" {
		t.Fatalf("explicit native probes = %#v, want sorted aliases", probes)
	}
	entries := composition.Catalog.RuntimeCatalog.EntriesFor(generic.Name)
	var entry *loop.RuntimeCatalogEntry
	for i := range entries {
		if entries[i].AgentHarness == "codex" {
			entry = &entries[i]
		}
	}
	if entry == nil || len(entry.Models) != 2 || entry.Source != loop.RuntimeSourceNative {
		t.Fatalf("explicit native filtered entries = %#v", entries)
	}
}

func TestNewACPCompositionPreflightsClaudeNativeAliasesIndependently(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := CompileAgentRuntimeCatalog(AgentRuntimeCatalogInput{
		NativeACP: map[string]ACPNativeProfile{
			"claude-code": {Harness: "claude-code", Enabled: true, Models: []loop.ModelAlias{"seed-fails", "later-ready"}},
		},
		PrimerTarget: runtimeCatalogPrimer(),
	})
	if err != nil {
		t.Fatalf("CompileAgentRuntimeCatalog() error = %v", err)
	}
	var probes []ACPExecutableProbe
	composition, err := NewACPComposition(ACPChildrenConfig{
		Catalog:       compiled,
		Executables:   map[loop.AgentHarnessName]string{"claude-code": executable},
		WorkspaceRoot: "/workspace/project",
		executablePreflight: func(_ context.Context, probe ACPExecutableProbe) ACPPreflightResult {
			probes = append(probes, probe)
			if probe.Credential != loop.CredentialNativeAuth || probe.SharedProxy != nil || probe.Model != probe.SmallModel || len(probe.Models) != 1 || probe.Models[0] != probe.Model {
				return ACPPreflightResult{}
			}
			return ACPPreflightResult{Ready: probe.Model == "later-ready"}
		},
	})
	if err != nil {
		t.Fatalf("NewACPComposition() error = %v", err)
	}
	if len(probes) != 2 || probes[0].Model != "later-ready" || probes[1].Model != "seed-fails" {
		t.Fatalf("Claude native probes = %#v, want sorted independent aliases", probes)
	}
	entries := composition.Catalog.RuntimeCatalog.EntriesFor(generic.Name)
	var claude *loop.RuntimeCatalogEntry
	for i := range entries {
		if entries[i].AgentHarness == "claude-code" {
			claude = &entries[i]
		}
	}
	if claude == nil || len(claude.Models) != 1 || claude.Models[0].Alias != "later-ready" {
		t.Fatalf("Claude native filtered entries = %#v, want only the ready alias", entries)
	}
	if !composition.Catalog.HasProfile("acp/claude-code") {
		t.Fatal("Claude native profile was removed despite a ready alias")
	}
}

func TestACPChildModelAliasesUseNativeSelectionKind(t *testing.T) {
	managedCatalog, err := CompileAgentRuntimeCatalog(AgentRuntimeCatalogInput{
		NativeACP:    map[string]ACPNativeProfile{"codex": {Harness: "codex", Enabled: true}},
		PrimerTarget: runtimeCatalogPrimer(),
	})
	if err != nil {
		t.Fatal(err)
	}
	managed, err := managedCatalog.RuntimeCatalog.ResolveWithExplicitSource(generic.Name, "codex", loop.RuntimeSourceNative, "", model.EffortNone, false)
	if err != nil {
		t.Fatal(err)
	}
	mainAlias, smallAlias, err := acpChildModelAliases(managedCatalog, generic.Name, "codex", managed)
	if err != nil {
		t.Fatal(err)
	}
	if mainAlias != "" || smallAlias != "" {
		t.Fatalf("managed native child aliases = %q/%q, want empty selectors", mainAlias, smallAlias)
	}

	explicitCatalog, err := CompileAgentRuntimeCatalog(AgentRuntimeCatalogInput{
		NativeACP:    map[string]ACPNativeProfile{"codex": {Harness: "codex", Enabled: true, Models: []loop.ModelAlias{"native-a"}}},
		PrimerTarget: runtimeCatalogPrimer(),
	})
	if err != nil {
		t.Fatal(err)
	}
	explicit, err := explicitCatalog.RuntimeCatalog.ResolveWithExplicitSource(generic.Name, "codex", loop.RuntimeSourceNative, "native-a", model.EffortNone, false)
	if err != nil {
		t.Fatal(err)
	}
	mainAlias, smallAlias, err = acpChildModelAliases(explicitCatalog, generic.Name, "codex", explicit)
	if err != nil {
		t.Fatal(err)
	}
	if mainAlias != "native-a" || smallAlias != "" {
		t.Fatalf("explicit native child aliases = %q/%q, want selected alias only", mainAlias, smallAlias)
	}
}

func TestACPChildConfigResolvesManagedNativeWithoutGateway(t *testing.T) {
	catalog, err := CompileAgentRuntimeCatalog(AgentRuntimeCatalogInput{
		NativeACP:    map[string]ACPNativeProfile{"codex": {Harness: "codex", Enabled: true}},
		PrimerTarget: runtimeCatalogPrimer(),
	})
	if err != nil {
		t.Fatal(err)
	}
	definition, err := loop.Define(
		loop.WithName(generic.Name),
		loop.WithInference(&fakeLLM{}, testModel()),
		loop.WithPolicyRevision("native-phase3"),
	)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := definition.Bind(context.Background(), tool.Bindings{SessionID: mustUUID(t), LoopID: mustUUID(t)})
	if err != nil {
		t.Fatal(err)
	}
	bound, err = loop.OverrideBoundRuntimeManaged(bound, "acp/codex")
	if err != nil {
		t.Fatal(err)
	}
	factory := &acpChildFactory{config: ACPChildrenConfig{
		Catalog:            catalog,
		Executables:        map[loop.AgentHarnessName]string{"codex": "/bin/codex"},
		NativeEnvAllowlist: []string{"HOME"},
	}}
	resolved, childConfig, ownedGateway, err := factory.configFor(context.Background(), bound, "existing-session")
	if err != nil {
		t.Fatal(err)
	}
	if ownedGateway != nil {
		t.Fatal("managed native child constructed a gateway")
	}
	if resolved.Source != loop.RuntimeSourceNative || resolved.SelectionKind != loop.RuntimeSelectionHarnessManaged || childConfig.Credential != loop.CredentialNativeAuth || childConfig.ModelAlias != "" || childConfig.SmallModelAlias != "" || childConfig.Binding.BaseURL != "" || childConfig.Binding.Token != "" || childConfig.AgentSessionID != "existing-session" {
		t.Fatalf("managed native child config = %#v resolved=%#v", childConfig, resolved)
	}
}
