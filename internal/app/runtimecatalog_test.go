package app

import (
	"context"
	"reflect"
	"testing"

	"github.com/looprig/coderig/internal/catalog/generic"
	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/inference"
	model "github.com/looprig/inference/model"
	"github.com/looprig/inference/stream"
)

func TestCompileAgentRuntimeCatalogContainsOnlyGenericWithInProcessDefault(t *testing.T) {
	targets := runtimeCatalogTargets()
	compiled, err := CompileAgentRuntimeCatalog(AgentRuntimeCatalogInput{GatewayTargets: targets})
	if err != nil {
		t.Fatalf("CompileAgentRuntimeCatalog() error = %v", err)
	}

	entries := compiled.RuntimeCatalog.EntriesFor(generic.Name)
	if len(entries) != 2 {
		t.Fatalf("Generic entries = %d, want ordinary and codex rows: %#v", len(entries), entries)
	}
	defaults := 0
	for _, entry := range entries {
		if entry.Default {
			defaults++
			if entry.AgentHarness != looprigRuntimeHarness || entry.Profile != looprigRuntimeProfile || entry.Source != loop.RuntimeSourceNative {
				t.Fatalf("default entry = %#v, want looprig/native native entry", entry)
			}
		}
	}
	if defaults != 1 {
		t.Fatalf("Generic defaults = %d, want exactly one: %#v", defaults, entries)
	}
	// Legacy identities are rejection fixtures: the compiled catalog is Generic-only.
	for _, legacy := range []identity.AgentName{"planner", "builder", "reviewer"} {
		if got := compiled.RuntimeCatalog.EntriesFor(legacy); got != nil {
			t.Fatalf("legacy entries for %q = %#v, want nil", legacy, got)
		}
	}
}

func TestCompileAgentRuntimeCatalogKeepsACPChoicesExplicitAndNonDefault(t *testing.T) {
	targets := runtimeCatalogTargets()
	compiled, err := CompileAgentRuntimeCatalog(AgentRuntimeCatalogInput{
		GatewayTargets: targets,
		ClaudeSmall:    targets[0].Alias,
	})
	if err != nil {
		t.Fatalf("CompileAgentRuntimeCatalog() error = %v", err)
	}

	entries := compiled.RuntimeCatalog.EntriesFor(generic.Name)
	if len(entries) != 3 {
		t.Fatalf("Generic entries = %d, want ordinary plus two ACP rows: %#v", len(entries), entries)
	}
	for _, entry := range entries {
		switch entry.AgentHarness {
		case "codex", "claude-code":
			if entry.Default {
				t.Fatalf("ACP entry unexpectedly default: %#v", entry)
			}
			if entry.SelectionKind != loop.RuntimeSelectionExplicit {
				t.Fatalf("ACP entry selection kind = %q, want explicit", entry.SelectionKind)
			}
		default:
			if !entry.Default {
				t.Fatalf("ordinary entry unexpectedly non-default: %#v", entry)
			}
		}
	}

	for _, harness := range []loop.AgentHarnessName{"codex", "claude-code"} {
		resolved, err := compiled.RuntimeCatalog.Resolve(generic.Name, harness, targets[0].Alias, targets[0].Efforts[0])
		if err != nil {
			t.Fatalf("Resolve(%q) error = %v", harness, err)
		}
		if resolved.AgentHarness != harness || resolved.Profile != loop.RuntimeProfileName("acp/"+string(harness)) {
			t.Fatalf("Resolve(%q) = %#v", harness, resolved)
		}
	}
	resolved, err := compiled.RuntimeCatalog.Resolve(generic.Name, "", "", model.EffortNone)
	if err != nil {
		t.Fatalf("default Resolve() error = %v", err)
	}
	if resolved.AgentHarness != looprigRuntimeHarness || resolved.Profile != looprigRuntimeProfile {
		t.Fatalf("default Resolve() = %#v, want looprig/native", resolved)
	}
}

func TestCompileAgentRuntimeCatalogUsesPrimerOnlyForInProcessFallback(t *testing.T) {
	primer := runtimeCatalogPrimer()
	compiled, err := CompileAgentRuntimeCatalog(AgentRuntimeCatalogInput{PrimerTarget: primer})
	if err != nil {
		t.Fatalf("CompileAgentRuntimeCatalog() error = %v", err)
	}
	entries := compiled.RuntimeCatalog.EntriesFor(generic.Name)
	if len(entries) != 1 || !entries[0].Default || entries[0].AgentHarness != looprigRuntimeHarness {
		t.Fatalf("Generic entries = %#v, want one in-process default", entries)
	}
	if len(entries[0].Models) != 1 || entries[0].Models[0].Alias != primer.Alias {
		t.Fatalf("ordinary fallback models = %#v, want primer only", entries[0].Models)
	}
	if got := compiled.RuntimeCatalog.EntriesFor(generic.Name); len(got) != 1 {
		t.Fatalf("primer fallback leaked ACP rows: %#v", got)
	}
	resolved, err := compiled.RuntimeCatalog.Resolve(generic.Name, "", "", model.EffortNone)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if resolved.AgentHarness != looprigRuntimeHarness || resolved.Profile != looprigRuntimeProfile || resolved.ModelAlias != primer.Alias || resolved.Effort != primer.DefaultEffort {
		t.Fatalf("Resolve() = %#v, want looprig/native primer target", resolved)
	}
}

func TestCompileAgentRuntimeCatalogEntriesAreDeterministic(t *testing.T) {
	first := testRuntimeCatalogCompilation(t, false)
	second := testRuntimeCatalogCompilation(t, true)
	if first.RuntimeCatalog.Digest() != second.RuntimeCatalog.Digest() {
		t.Fatalf("catalog digest changed with input order: %s != %s", first.RuntimeCatalog.Digest(), second.RuntimeCatalog.Digest())
	}
	if !reflect.DeepEqual(first.RuntimeCatalog.EntriesFor(generic.Name), second.RuntimeCatalog.EntriesFor(generic.Name)) {
		t.Fatal("catalog entries changed with input order")
	}
}

func TestCompileAgentRuntimeCatalogHasOneGenericEntryPerProfile(t *testing.T) {
	target := runtimeCatalogTargets()[0]
	compiled, err := CompileAgentRuntimeCatalog(AgentRuntimeCatalogInput{
		GatewayTargets: []ACPGatewaySource{target},
		ClaudeSmall:    target.Alias,
		NativeACP: map[string]ACPNativeProfile{
			"codex": {Harness: "codex", Enabled: true, Models: []loop.ModelAlias{"native-a"}},
		},
	})
	if err != nil {
		t.Fatalf("CompileAgentRuntimeCatalog() error = %v", err)
	}
	entries := compiled.RuntimeCatalog.EntriesFor(generic.Name)
	seen := make(map[loop.RuntimeProfileName]struct{}, len(entries))
	for _, entry := range entries {
		if _, duplicate := seen[entry.Profile]; duplicate {
			t.Fatalf("duplicate Generic runtime profile %q: %#v", entry.Profile, entries)
		}
		seen[entry.Profile] = struct{}{}
	}
	if len(entries) != 3 {
		t.Fatalf("Generic entries = %d, want looprig/native plus one Claude and one Codex ACP entry: %#v", len(entries), entries)
	}
	codex, err := compiled.RuntimeCatalog.ResolveWithExplicitSource(generic.Name, "codex", loop.RuntimeSourceNative, "native-a", model.EffortNone, false)
	if err != nil {
		t.Fatalf("Resolve explicit native Codex model: %v", err)
	}
	if codex.Profile != "acp/codex" || codex.Source != loop.RuntimeSourceNative {
		t.Fatalf("native Codex resolution = %#v", codex)
	}
}

func TestCompileAgentRuntimeCatalogFiltersUnavailableACPWithoutRepairingDefault(t *testing.T) {
	targets := runtimeCatalogTargets()
	compiled, err := CompileAgentRuntimeCatalog(AgentRuntimeCatalogInput{GatewayTargets: targets, ClaudeSmall: targets[0].Alias})
	if err != nil {
		t.Fatal(err)
	}
	filtered, err := filterACPPreflightCatalog(compiled, nil)
	if err != nil {
		t.Fatal(err)
	}
	entries := filtered.RuntimeCatalog.EntriesFor(generic.Name)
	if len(entries) != 1 || entries[0].AgentHarness != looprigRuntimeHarness || !entries[0].Default {
		t.Fatalf("filtered entries = %#v, want ordinary Generic default only", entries)
	}
}

func testRuntimeCatalogCompilation(t *testing.T, reverse bool) ACPCompiledCatalog {
	t.Helper()
	targets := runtimeCatalogTargets()
	if reverse {
		targets[0], targets[1] = targets[1], targets[0]
	}
	compiled, err := CompileAgentRuntimeCatalog(AgentRuntimeCatalogInput{GatewayTargets: targets})
	if err != nil {
		t.Fatal(err)
	}
	return compiled
}

func runtimeCatalogTargets() []ACPGatewaySource {
	return []ACPGatewaySource{
		{Alias: "zeta", Description: "Zeta model.", Client: &runtimeCatalogClient{}, Model: model.CustomModel("openai", model.APIFormatOpenAIResponses, "", "zeta", model.WithTools(), model.WithThinking()), DefaultEffort: model.EffortLow, Efforts: []model.Effort{model.EffortLow}},
		{Alias: "alpha", Description: "Alpha model.", Client: &runtimeCatalogClient{}, Model: model.CustomModel("openai", model.APIFormatOpenAIResponses, "", "alpha", model.WithTools(), model.WithThinking()), DefaultEffort: model.EffortMedium, Efforts: []model.Effort{model.EffortMedium, model.EffortHigh}},
	}
}

func runtimeCatalogPrimer() GenericRuntimeSource {
	return GenericRuntimeSource{
		Alias: "primer", Description: "Configured primer.", Client: &runtimeCatalogClient{},
		Model:         model.CustomModel("openai", model.APIFormatOpenAIResponses, "", "primer", model.WithTools(), model.WithThinking()),
		DefaultEffort: model.EffortMedium, Efforts: []model.Effort{model.EffortLow, model.EffortMedium},
	}
}

func TestConfiguredPrimerRuntimeTargetPinsAnAdmittedEffort(t *testing.T) {
	primerModel := model.CustomModel("openai", model.APIFormatOpenAIResponses, "", "primer", model.WithTools(), model.WithThinking())
	configured := productionModels{
		PrimerAlias:   "primer",
		PrimerClient:  &runtimeCatalogClient{},
		PrimerModel:   primerModel,
		PrimerEfforts: []model.Effort{model.EffortHigh},
	}
	target := configuredPrimerRuntimeTarget(configured)
	if target.DefaultEffort != model.EffortHigh {
		t.Fatalf("configured primer default effort = %q, want admitted high effort", target.DefaultEffort)
	}
}

type runtimeCatalogClient struct{}

func (*runtimeCatalogClient) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	return nil, nil
}

func (*runtimeCatalogClient) Stream(context.Context, inference.Request) (*stream.StreamReader[content.Chunk], error) {
	return nil, nil
}
