package app

import (
	"context"
	"os"
	"testing"

	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	model "github.com/looprig/inference/model"
)

func TestCompileACPCatalogCompilesNativeProfilesAlongsideGateway(t *testing.T) {
	gateway := fixtureGatewaySource("gateway-model", &fakeLLM{})
	compiled, err := CompileACPCatalog(ACPCatalogInput{
		SubagentTypes:  []identity.AgentName{"planner", "builder", "reviewer"},
		GatewayTargets: []ACPGatewaySource{gateway},
		NativeProfiles: []ACPNativeProfile{
			{Harness: "codex", Enabled: true, Models: []loop.ModelAlias{"native-z", "native-a"}},
			{Harness: "claude-code", Enabled: false, Models: []loop.ModelAlias{"disabled-only"}},
		},
		Defaults: map[identity.AgentName]configuredDelegateDefault{
			"planner":  {Harness: "codex", Source: loop.RuntimeSourceNative, Model: "native-a"},
			"builder":  {Harness: "codex", Source: loop.RuntimeSourceGateway, Model: "gateway-model", Effort: model.EffortMedium},
			"reviewer": {Harness: "codex", Source: loop.RuntimeSourceGateway, Model: "gateway-model", Effort: model.EffortMedium},
		},
		ClaudeSmall: "gateway-model",
	})
	if err != nil {
		t.Fatalf("CompileACPCatalog() error = %v", err)
	}

	plannerNative, err := compiled.RuntimeCatalog.ResolveWithExplicitSource("planner", "codex", loop.RuntimeSourceNative, "native-a", model.EffortNone, false)
	if err != nil {
		t.Fatalf("resolve explicit native source: %v", err)
	}
	if plannerNative.Source != loop.RuntimeSourceNative || plannerNative.SelectionKind != loop.RuntimeSelectionExplicit || plannerNative.ModelAlias != "native-a" || plannerNative.Credential != loop.CredentialNativeAuth {
		t.Fatalf("explicit native resolution = %#v", plannerNative)
	}
	if plannerNative.Target.Name == "" || plannerNative.Target.Name == string(plannerNative.ModelAlias) {
		t.Fatalf("explicit native target = %#v, want opaque non-alias identity", plannerNative.Target)
	}

	builderDefault, err := compiled.RuntimeCatalog.Resolve("builder", "", "", model.EffortNone)
	if err != nil {
		t.Fatalf("resolve omitted-source gateway default: %v", err)
	}
	if builderDefault.Source != loop.RuntimeSourceGateway || builderDefault.ModelAlias != "gateway-model" || builderDefault.Credential != loop.CredentialGatewayBacked {
		t.Fatalf("omitted-source resolution = %#v, want gateway", builderDefault)
	}

	for _, entry := range compiled.RuntimeCatalog.EntriesFor("reviewer") {
		if entry.AgentHarness == "claude-code" && entry.Source == loop.RuntimeSourceNative {
			t.Fatalf("disabled Claude profile created native entry: %#v", entry)
		}
	}
}

func TestCompileACPCatalogRejectsNativeAliasCollisionWithGateway(t *testing.T) {
	_, err := CompileACPCatalog(ACPCatalogInput{
		SubagentTypes:  []identity.AgentName{"worker"},
		GatewayTargets: []ACPGatewaySource{fixtureGatewaySource("shared", &fakeLLM{})},
		NativeProfiles: []ACPNativeProfile{{Harness: "codex", Enabled: true, Models: []loop.ModelAlias{"shared"}}},
		Defaults: map[identity.AgentName]configuredDelegateDefault{
			"worker": {Harness: "codex", Source: loop.RuntimeSourceGateway, Model: "shared", Effort: model.EffortMedium},
		},
		ClaudeSmall: "shared",
	})
	if err == nil {
		t.Fatal("CompileACPCatalog() succeeded with native/gateway alias collision")
	}
}

func TestNewACPCompositionPreflightsManagedNativeWithoutProxyOrModel(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := CompileACPCatalog(ACPCatalogInput{
		SubagentTypes: []identity.AgentName{"worker"},
		NativeACP: map[string]ACPNativeProfile{
			"codex": {Harness: "codex", Enabled: true},
		},
		Defaults: map[identity.AgentName]configuredDelegateDefault{
			"worker": {Harness: "codex", Source: loop.RuntimeSourceNative},
		},
	})
	if err != nil {
		t.Fatalf("CompileACPCatalog() error = %v", err)
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
	entry := composition.Catalog.RuntimeCatalog.EntriesFor("worker")
	if len(entry) != 1 || entry[0].SelectionKind != loop.RuntimeSelectionHarnessManaged || len(entry[0].Models) != 0 {
		t.Fatalf("managed native filtered entries = %#v", entry)
	}
}

func TestNewACPCompositionPreflightsExplicitNativeAliasesWithoutGateway(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := CompileACPCatalog(ACPCatalogInput{
		SubagentTypes: []identity.AgentName{"worker"},
		NativeACP: map[string]ACPNativeProfile{
			"codex": {Harness: "codex", Enabled: true, Models: []loop.ModelAlias{"native-z", "native-a"}},
		},
		Defaults: map[identity.AgentName]configuredDelegateDefault{
			"worker": {Harness: "codex", Source: loop.RuntimeSourceNative, Model: "native-a"},
		},
	})
	if err != nil {
		t.Fatalf("CompileACPCatalog() error = %v", err)
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
	entry := composition.Catalog.RuntimeCatalog.EntriesFor("worker")
	if len(entry) != 1 || len(entry[0].Models) != 2 || entry[0].Source != loop.RuntimeSourceNative {
		t.Fatalf("explicit native filtered entries = %#v", entry)
	}
}

func TestNewACPCompositionPreflightsClaudeNativeAliasesIndependently(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := CompileACPCatalog(ACPCatalogInput{
		SubagentTypes: []identity.AgentName{"worker"},
		NativeACP: map[string]ACPNativeProfile{
			"claude-code": {Harness: "claude-code", Enabled: true, Models: []loop.ModelAlias{"seed-fails", "later-ready"}},
		},
		Defaults: map[identity.AgentName]configuredDelegateDefault{
			"worker": {Harness: "claude-code", Source: loop.RuntimeSourceNative, Model: "seed-fails"},
		},
	})
	if err != nil {
		t.Fatalf("CompileACPCatalog() error = %v", err)
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
	entries := composition.Catalog.RuntimeCatalog.EntriesFor("worker")
	if len(entries) != 0 {
		t.Fatalf("Claude native filtered entries = %#v, want configured default source dropped", entries)
	}
	if composition.Catalog.HasProfile("acp/claude-code") {
		t.Fatal("Claude native profile survived after its configured default was removed")
	}
}

func TestACPChildModelAliasesUseNativeSelectionKind(t *testing.T) {
	managedCatalog, err := CompileACPCatalog(ACPCatalogInput{
		SubagentTypes: []identity.AgentName{"worker"},
		NativeACP:     map[string]ACPNativeProfile{"codex": {Harness: "codex", Enabled: true}},
		Defaults: map[identity.AgentName]configuredDelegateDefault{
			"worker": {Harness: "codex", Source: loop.RuntimeSourceNative},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	managed, err := managedCatalog.RuntimeCatalog.ResolveWithExplicitSource("worker", "codex", loop.RuntimeSourceNative, "", model.EffortNone, false)
	if err != nil {
		t.Fatal(err)
	}
	mainAlias, smallAlias, err := acpChildModelAliases(managedCatalog, "worker", "codex", managed)
	if err != nil {
		t.Fatal(err)
	}
	if mainAlias != "" || smallAlias != "" {
		t.Fatalf("managed native child aliases = %q/%q, want empty selectors", mainAlias, smallAlias)
	}

	explicitCatalog, err := CompileACPCatalog(ACPCatalogInput{
		SubagentTypes: []identity.AgentName{"worker"},
		NativeACP:     map[string]ACPNativeProfile{"codex": {Harness: "codex", Enabled: true, Models: []loop.ModelAlias{"native-a"}}},
		Defaults: map[identity.AgentName]configuredDelegateDefault{
			"worker": {Harness: "codex", Source: loop.RuntimeSourceNative, Model: "native-a"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	explicit, err := explicitCatalog.RuntimeCatalog.ResolveWithExplicitSource("worker", "codex", loop.RuntimeSourceNative, "native-a", model.EffortNone, false)
	if err != nil {
		t.Fatal(err)
	}
	mainAlias, smallAlias, err = acpChildModelAliases(explicitCatalog, "worker", "codex", explicit)
	if err != nil {
		t.Fatal(err)
	}
	if mainAlias != "native-a" || smallAlias != "" {
		t.Fatalf("explicit native child aliases = %q/%q, want selected alias only", mainAlias, smallAlias)
	}
}

func TestACPChildConfigResolvesManagedNativeWithoutGateway(t *testing.T) {
	catalog, err := CompileACPCatalog(ACPCatalogInput{
		SubagentTypes: []identity.AgentName{"builder"},
		NativeACP:     map[string]ACPNativeProfile{"codex": {Harness: "codex", Enabled: true}},
		Defaults: map[identity.AgentName]configuredDelegateDefault{
			"builder": {Harness: "codex", Source: loop.RuntimeSourceNative},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	definition, err := loop.Define(
		loop.WithName(identity.AgentName("builder")),
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
