package app

import (
	"reflect"
	"strings"
	"testing"

	"github.com/looprig/coderig/internal/catalog/generic"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/inference/model"
)

func TestCompileACPRuntimeEntriesProducesNonDefaultGenericRows(t *testing.T) {
	target := fixtureGatewaySource("fixture-a", &fakeLLM{})
	raw, err := compileACPRuntimeEntries(acpCatalogInput{
		GatewayTargets: []ACPGatewaySource{target},
		ClaudeSmall:    target.Alias,
	})
	if err != nil {
		t.Fatalf("compileACPRuntimeEntries() error = %v", err)
	}
	if len(raw.entries) != 2 {
		t.Fatalf("raw entries = %#v, want Claude and Codex rows", raw.entries)
	}
	for _, entry := range raw.entries {
		if entry.AgentType != generic.Name || entry.Default {
			t.Fatalf("raw ACP entry = %#v, want generic non-default row", entry)
		}
	}
}

func TestCompileAgentRuntimeCatalogUsesConfiguredTargetsAndDeterministicDefault(t *testing.T) {
	clientA := &fakeLLM{}
	clientB := &fakeLLM{}
	targets := []ACPGatewaySource{
		{
			Alias: "fixture-a", Client: clientA,
			Model:         model.CustomModel("openai", model.APIFormatOpenAIResponses, "", "provider-a", model.WithTools(), model.WithThinking()),
			DefaultEffort: model.EffortMedium,
			Efforts:       []model.Effort{model.EffortLow, model.EffortMedium, model.EffortHigh},
		},
		{
			Alias: "fixture-b", Client: clientB,
			Model:         model.CustomModel("anthropic", model.APIFormatAnthropic, "", "provider-b", model.WithTools(), model.WithThinking()),
			DefaultEffort: model.EffortLow,
			Efforts:       []model.Effort{model.EffortLow, model.EffortMax},
		},
	}
	compiled, err := CompileAgentRuntimeCatalog(AgentRuntimeCatalogInput{
		GatewayTargets: targets,
		ClaudeSmall:    "fixture-b",
	})
	if err != nil {
		t.Fatalf("CompileAgentRuntimeCatalog() error = %v", err)
	}

	entries := compiled.RuntimeCatalog.EntriesFor(generic.Name)
	if len(entries) != 3 {
		t.Fatalf("Generic entries = %d, want looprig/native plus Claude and Codex rows: %#v", len(entries), entries)
	}
	defaults := 0
	for _, entry := range entries {
		if entry.AgentType != generic.Name {
			t.Fatalf("entry agent type = %q, want generic", entry.AgentType)
		}
		if entry.Default {
			defaults++
			if entry.AgentHarness != looprigRuntimeHarness || entry.Profile != looprigRuntimeProfile || entry.Source != loop.RuntimeSourceNative {
				t.Fatalf("default entry = %#v, want looprig/native native entry", entry)
			}
			continue
		}
		if entry.AgentHarness != "codex" && entry.AgentHarness != "claude-code" {
			t.Fatalf("non-default entry = %#v, want ACP harness", entry)
		}
	}
	if defaults != 1 {
		t.Fatalf("Generic defaults = %d, want exactly one: %#v", defaults, entries)
	}

	for _, harness := range []loop.AgentHarnessName{"claude-code", "codex"} {
		for _, target := range targets {
			resolved, err := compiled.RuntimeCatalog.ResolveWithExplicitEffort(generic.Name, harness, target.Alias, target.Efforts[0], true)
			if err != nil {
				t.Errorf("Resolve(%s, %s) error = %v", harness, target.Alias, err)
				continue
			}
			if resolved.AgentType != generic.Name || resolved.AgentHarness != harness || resolved.ModelAlias != target.Alias || resolved.Effort != target.Efforts[0] {
				t.Errorf("Resolve(%s, %s) = %#v", harness, target.Alias, resolved)
			}
		}
	}
	resolved, err := compiled.RuntimeCatalog.Resolve(generic.Name, "", "", model.EffortNone)
	if err != nil {
		t.Fatalf("Resolve(generic default) error = %v", err)
	}
	if resolved.AgentHarness != looprigRuntimeHarness || resolved.Profile != looprigRuntimeProfile {
		t.Errorf("Resolve(generic default) = %#v, want looprig/native", resolved)
	}
	for _, oldAlias := range []loop.ModelAlias{"fable-5", "sonnet-5", "opus-5", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"} {
		if _, err := compiled.RuntimeCatalog.Resolve(generic.Name, "codex", oldAlias, model.EffortMedium); err == nil {
			t.Errorf("unconfigured old alias %q resolved", oldAlias)
		}
	}
}

func TestCompileAgentRuntimeCatalogDerivesDistinctEffortTargets(t *testing.T) {
	compiled := compileFixtureACPCatalog(t)
	low, err := compiled.RuntimeCatalog.ResolveWithExplicitEffort(generic.Name, "codex", "fixture-a", model.EffortLow, true)
	if err != nil {
		t.Fatal(err)
	}
	high, err := compiled.RuntimeCatalog.ResolveWithExplicitEffort(generic.Name, "codex", "fixture-a", model.EffortHigh, true)
	if err != nil {
		t.Fatal(err)
	}
	if low.TargetAlias == high.TargetAlias {
		t.Fatalf("target aliases are both %q for different efforts", low.TargetAlias)
	}
	if low.TargetAlias != "fixture-a@low" || high.TargetAlias != "fixture-a@high" {
		t.Fatalf("target aliases = %q and %q, want fixture-a@low and fixture-a@high", low.TargetAlias, high.TargetAlias)
	}
}

func TestCompileAgentRuntimeCatalogRejectsDuplicateAliases(t *testing.T) {
	target := fixtureGatewaySource("fixture-a", &fakeLLM{})
	if _, err := CompileAgentRuntimeCatalog(AgentRuntimeCatalogInput{
		GatewayTargets: []ACPGatewaySource{target, target},
		ClaudeSmall:    "fixture-a",
	}); err == nil {
		t.Fatal("CompileAgentRuntimeCatalog() succeeded")
	}
}

func TestACPGatewayTargetReturnsExactClientAndAuthoritativeEffort(t *testing.T) {
	client := &fakeLLM{}
	targetModel := model.CustomModel("openai", model.APIFormatOpenAIResponses, "", "provider-a", model.WithTools(), model.WithThinking())
	compiled, err := CompileAgentRuntimeCatalog(AgentRuntimeCatalogInput{
		GatewayTargets: []ACPGatewaySource{{
			Alias: "fixture-a", Client: client, Model: targetModel,
			DefaultEffort: model.EffortMedium, Efforts: []model.Effort{model.EffortLow, model.EffortMedium, model.EffortHigh},
		}},
		ClaudeSmall: "fixture-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := compiled.RuntimeCatalog.ResolveWithExplicitEffort(generic.Name, "codex", "fixture-a", model.EffortHigh, true)
	if err != nil {
		t.Fatal(err)
	}
	got, err := compiled.GatewayTarget(resolved)
	if err != nil {
		t.Fatal(err)
	}
	if got.Client != client || !got.AuthoritativeEffort || got.Model.Sampling.Effort != model.EffortHigh || got.ID != "fixture-a@high" {
		t.Fatalf("GatewayTarget() returned wrong binding")
	}
	wantModel := targetModel.Clone()
	wantModel.Sampling.Effort = model.EffortHigh
	if !reflect.DeepEqual(got.Model, wantModel) {
		t.Fatalf("GatewayTarget() model did not preserve descriptor")
	}
}

func TestCompileAgentRuntimeCatalogErrorDoesNotEchoAliases(t *testing.T) {
	const sentinel = "test-secret-do-not-log"
	target := fixtureGatewaySource("fixture-a", &fakeLLM{})
	target.Alias = loop.ModelAlias(sentinel)
	_, err := CompileAgentRuntimeCatalog(AgentRuntimeCatalogInput{
		GatewayTargets: []ACPGatewaySource{target, target},
		ClaudeSmall:    loop.ModelAlias(sentinel),
	})
	if err == nil {
		t.Fatal("CompileAgentRuntimeCatalog() succeeded")
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Fatalf("error exposed configured alias: %q", err)
	}
}

func compileFixtureACPCatalog(t *testing.T) ACPCompiledCatalog {
	t.Helper()
	compiled, err := CompileAgentRuntimeCatalog(AgentRuntimeCatalogInput{
		GatewayTargets: []ACPGatewaySource{fixtureGatewaySource("fixture-a", &fakeLLM{})},
		ClaudeSmall:    "fixture-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	return compiled
}

func fixtureGatewaySource(alias loop.ModelAlias, client *fakeLLM) ACPGatewaySource {
	return ACPGatewaySource{
		Alias: alias, Client: client,
		Model:         model.CustomModel("openai", model.APIFormatOpenAIResponses, "", "provider-model", model.WithTools(), model.WithThinking()),
		DefaultEffort: model.EffortMedium,
		Efforts:       []model.Effort{model.EffortLow, model.EffortMedium, model.EffortHigh},
	}
}
