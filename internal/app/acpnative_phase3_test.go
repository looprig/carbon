package app

import (
	"context"
	"os"
	"reflect"
	"testing"

	"github.com/looprig/carbon/internal/catalog/carbon"
	"github.com/looprig/foreignloops/driver"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
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

	carbonNative, err := compiled.RuntimeCatalog.ResolveWithExplicitSource(carbon.Name, "codex", loop.RuntimeSourceNative, "native-a", model.EffortNone, false)
	if err != nil {
		t.Fatalf("resolve explicit native source: %v", err)
	}
	if carbonNative.Source != loop.RuntimeSourceNative || carbonNative.SelectionKind != loop.RuntimeSelectionExplicit || carbonNative.ModelAlias != "native-a" || carbonNative.Credential != loop.CredentialNativeAuth {
		t.Fatalf("explicit native resolution = %#v", carbonNative)
	}
	if carbonNative.Target.Name == "" || carbonNative.Target.Name == string(carbonNative.ModelAlias) {
		t.Fatalf("explicit native target = %#v, want opaque non-alias identity", carbonNative.Target)
	}

	carbonDefault, err := compiled.RuntimeCatalog.Resolve(carbon.Name, "", "", model.EffortNone)
	if err != nil {
		t.Fatalf("resolve omitted-source gateway default: %v", err)
	}
	if carbonDefault.AgentHarness != looprigRuntimeHarness || carbonDefault.Source != loop.RuntimeSourceNative || carbonDefault.Credential != loop.CredentialNativeAuth {
		t.Fatalf("deterministic resolution = %#v, want looprig/native default", carbonDefault)
	}

	for _, entry := range compiled.RuntimeCatalog.EntriesFor(carbon.Name) {
		if entry.AgentHarness == "claude-code" && entry.Source == loop.RuntimeSourceNative {
			t.Fatalf("disabled Claude profile created native entry: %#v", entry)
		}
	}
}

func TestProductionNativeModelOptionsCompileExactEffortsAndDefault(t *testing.T) {
	config := decodeModelConfigWithNativeACP(t, `{"codex":{"enabled":true,"models":[
		{"model":"gpt-5.6-sol","efforts":["high","medium"],"default_effort":"medium"}
	]}}`)
	normalized, err := normalizeModelConfig(config)
	if err != nil {
		t.Fatalf("normalizeModelConfig() error = %v", err)
	}
	configured, err := compileProductionModels(normalized, func(model.Model, modelClientInput) (inference.Client, error) {
		return &fakeLLM{}, nil
	})
	if err != nil {
		t.Fatalf("compileProductionModels() error = %v", err)
	}
	compiled, err := CompileAgentRuntimeCatalog(AgentRuntimeCatalogInput{
		NativeACP:    configured.NativeACP,
		PrimerTarget: configuredPrimerRuntimeTarget(configured),
	})
	if err != nil {
		t.Fatalf("CompileAgentRuntimeCatalog() error = %v", err)
	}
	entries := compiled.RuntimeCatalog.EntriesFor(carbon.Name)
	var codex loop.RuntimeCatalogEntry
	for _, entry := range entries {
		if entry.AgentHarness == "codex" {
			codex = entry
			break
		}
	}
	if codex.AgentHarness != "codex" || len(codex.Models) != 1 {
		t.Fatalf("codex entry = %#v, want one configured native model", codex)
	}
	option := codex.Models[0]
	if option.Alias != "gpt-5.6-sol" || option.DefaultEffort != model.EffortMedium {
		t.Fatalf("native option = %#v, want gpt-5.6-sol/default medium", option)
	}
	wantEfforts := []model.Effort{model.EffortMedium, model.EffortHigh}
	if !reflect.DeepEqual(option.Efforts, wantEfforts) {
		t.Fatalf("native efforts = %v, want exactly %v", option.Efforts, wantEfforts)
	}
	if containsACPEffort(option.Efforts, model.EffortNone) {
		t.Fatalf("native efforts = %v, must not invent EffortNone", option.Efforts)
	}
	if _, err := compiled.RuntimeCatalog.ResolveWithExplicitEffort(carbon.Name, "codex", option.Alias, model.EffortNone, true); err == nil {
		t.Fatal("explicit EffortNone unexpectedly resolved for a medium/high-only native model")
	}
}

func TestCompileAgentRuntimeCatalogRejectsNativeModelProjectionMismatch(t *testing.T) {
	_, err := CompileAgentRuntimeCatalog(AgentRuntimeCatalogInput{
		NativeACP: map[string]ACPNativeProfile{
			"codex": {
				Harness: "codex", Enabled: true,
				Models: []loop.ModelAlias{"configured-alias"},
				ModelOptions: []ACPNativeModelOption{{
					Alias: "different-alias", Model: "adapter-model",
					Efforts: []model.Effort{model.EffortMedium}, DefaultEffort: model.EffortMedium,
				}},
			},
		},
		PrimerTarget: runtimeCatalogPrimer(),
	})
	if err == nil {
		t.Fatal("CompileAgentRuntimeCatalog() accepted disagreeing native model projections")
	}
}

func TestCompileAgentRuntimeCatalogRejectsEmptyTypedNativeModelOptions(t *testing.T) {
	_, err := CompileAgentRuntimeCatalog(AgentRuntimeCatalogInput{
		NativeACP: map[string]ACPNativeProfile{
			"codex": {Harness: "codex", Enabled: true, ModelOptions: []ACPNativeModelOption{}},
		},
		PrimerTarget: runtimeCatalogPrimer(),
	})
	if err == nil {
		t.Fatal("CompileAgentRuntimeCatalog() accepted an empty typed native model allowlist")
	}
}

func TestNewACPCompositionDoesNotPreflightOrFilterConfiguredNativeRows(t *testing.T) {
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
	var calls int
	composition, err := NewACPComposition(ACPChildrenConfig{
		Catalog:       compiled,
		Executables:   map[loop.AgentHarnessName]string{"codex": executable},
		WorkspaceRoot: "/workspace/project",
		executablePreflight: func(context.Context, ACPExecutableProbe) ACPPreflightResult {
			calls++
			return ACPPreflightResult{}
		},
	})
	if err != nil {
		t.Fatalf("NewACPComposition() error = %v", err)
	}
	if calls != 0 {
		t.Fatalf("ACP preflight callback calls = %d, want zero during composition", calls)
	}
	entries := composition.Catalog.RuntimeCatalog.EntriesFor(carbon.Name)
	var codex loop.RuntimeCatalogEntry
	for _, entry := range entries {
		if entry.AgentHarness == "codex" {
			codex = entry
			break
		}
	}
	if codex.AgentHarness != "codex" || len(codex.Models) != 2 {
		t.Fatalf("codex rows after transient adapter failure = %#v, want both configured rows", codex)
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

func TestNewACPCompositionKeepsManagedNativeWithoutLivePreflight(t *testing.T) {
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
	if len(probes) != 0 {
		t.Fatalf("managed native probes = %#v, want no startup probes", probes)
	}
	if !composition.Catalog.HasProfile("acp/codex") {
		t.Fatal("managed native profile was removed after successful preflight")
	}
	entries := composition.Catalog.RuntimeCatalog.EntriesFor(carbon.Name)
	var managed *loop.RuntimeCatalogEntry
	for i := range entries {
		if entries[i].AgentHarness == "codex" && entries[i].SelectionKind == loop.RuntimeSelectionHarnessManaged {
			managed = &entries[i]
		}
	}
	if managed == nil || len(managed.Models) != 0 {
		t.Fatalf("managed native entries = %#v", entries)
	}
}

func TestNewACPCompositionKeepsExplicitNativeAliasesWithoutLivePreflight(t *testing.T) {
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
	if len(probes) != 0 {
		t.Fatalf("explicit native probes = %#v, want no startup probes", probes)
	}
	entries := composition.Catalog.RuntimeCatalog.EntriesFor(carbon.Name)
	var entry *loop.RuntimeCatalogEntry
	for i := range entries {
		if entries[i].AgentHarness == "codex" {
			entry = &entries[i]
		}
	}
	if entry == nil || len(entry.Models) != 2 || entry.Source != loop.RuntimeSourceNative {
		t.Fatalf("explicit native entries = %#v", entries)
	}
}

func TestNewACPCompositionKeepsClaudeNativeAliasesWithoutLivePreflight(t *testing.T) {
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
	if len(probes) != 0 {
		t.Fatalf("Claude native probes = %#v, want no startup probes", probes)
	}
	entries := composition.Catalog.RuntimeCatalog.EntriesFor(carbon.Name)
	var claude *loop.RuntimeCatalogEntry
	for i := range entries {
		if entries[i].AgentHarness == "claude-code" {
			claude = &entries[i]
		}
	}
	if claude == nil || len(claude.Models) != 2 {
		t.Fatalf("Claude native entries = %#v, want both configured aliases", entries)
	}
	if !composition.Catalog.HasProfile("acp/claude-code") {
		t.Fatal("Claude native profile was removed despite passing static checks")
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
	managed, err := managedCatalog.RuntimeCatalog.ResolveWithExplicitSource(carbon.Name, "codex", loop.RuntimeSourceNative, "", model.EffortNone, false)
	if err != nil {
		t.Fatal(err)
	}
	mainAlias, smallAlias, err := acpChildModelAliases(managedCatalog, carbon.Name, "codex", managed)
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
	explicit, err := explicitCatalog.RuntimeCatalog.ResolveWithExplicitSource(carbon.Name, "codex", loop.RuntimeSourceNative, "native-a", model.EffortNone, false)
	if err != nil {
		t.Fatal(err)
	}
	mainAlias, smallAlias, err = acpChildModelAliases(explicitCatalog, carbon.Name, "codex", explicit)
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
		loop.WithName(carbon.Name),
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
		AccessProfile:      AccessReadOnly,
		posture:            driver.PostureReadOnly,
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
	if resolved.Source != loop.RuntimeSourceNative || resolved.SelectionKind != loop.RuntimeSelectionHarnessManaged || childConfig.Credential != loop.CredentialNativeAuth || childConfig.ModelAlias != "" || childConfig.SmallModelAlias != "" || childConfig.Binding.BaseURL != "" || childConfig.Binding.Token != "" || childConfig.AgentSessionID != "existing-session" || childConfig.Posture != driver.PostureReadOnly {
		t.Fatalf("managed native child config = %#v resolved=%#v", childConfig, resolved)
	}
}
