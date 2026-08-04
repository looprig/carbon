package app

import (
	"reflect"
	"strings"
	"testing"

	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/inference/model"
)

func TestCompileACPCatalogUsesConfiguredTargetsAndDefaults(t *testing.T) {
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
	defaults := map[identity.AgentName]configuredDelegateDefault{
		"planner":  {Harness: "claude-code", Model: "fixture-b", Effort: model.EffortMax},
		"builder":  {Harness: "codex", Model: "fixture-a", Effort: model.EffortHigh},
		"reviewer": {Harness: "codex", Model: "fixture-a", Effort: model.EffortLow},
	}

	compiled, err := CompileACPCatalog(ACPCatalogInput{
		GatewayTargets: targets,
		Defaults:       defaults,
		ClaudeSmall:    "fixture-b",
	})
	if err != nil {
		t.Fatalf("CompileACPCatalog() error = %v", err)
	}

	for _, role := range []identity.AgentName{"planner", "builder", "reviewer"} {
		for _, harness := range []loop.AgentHarnessName{"claude-code", "codex"} {
			for _, target := range targets {
				resolved, err := compiled.RuntimeCatalog.ResolveWithExplicitEffort(role, harness, target.Alias, target.Efforts[0], true)
				if err != nil {
					t.Errorf("Resolve(%s, %s, %s) error = %v", role, harness, target.Alias, err)
					continue
				}
				if resolved.ModelAlias != target.Alias || resolved.Effort != target.Efforts[0] {
					t.Errorf("Resolve(%s, %s, %s) = alias %q effort %q", role, harness, target.Alias, resolved.ModelAlias, resolved.Effort)
				}
			}
		}

		resolved, err := compiled.RuntimeCatalog.Resolve(role, "", "", model.EffortNone)
		if err != nil {
			t.Fatalf("Resolve(%s default) error = %v", role, err)
		}
		want := defaults[role]
		if resolved.AgentHarness != want.Harness || resolved.ModelAlias != want.Model || resolved.Effort != want.Effort {
			t.Errorf("Resolve(%s default) = %s/%s@%s, want %s/%s@%s", role, resolved.AgentHarness, resolved.ModelAlias, resolved.Effort, want.Harness, want.Model, want.Effort)
		}
	}

	claude, err := compiled.RuntimeCatalog.Resolve("planner", "claude-code", "fixture-a", model.EffortLow)
	if err != nil {
		t.Fatal(err)
	}
	if claude.SmallModel != "fixture-b" {
		t.Fatalf("Claude small alias = %q, want fixture-b", claude.SmallModel)
	}
	for _, oldAlias := range []loop.ModelAlias{"fable-5", "sonnet-5", "opus-5", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"} {
		if _, err := compiled.RuntimeCatalog.Resolve("builder", "codex", oldAlias, model.EffortMedium); err == nil {
			t.Errorf("unconfigured old alias %q resolved", oldAlias)
		}
	}
}

func TestCompileACPCatalogDerivesDistinctEffortTargets(t *testing.T) {
	compiled := compileFixtureACPCatalog(t)
	low, err := compiled.RuntimeCatalog.ResolveWithExplicitEffort("worker", "codex", "fixture-a", model.EffortLow, true)
	if err != nil {
		t.Fatal(err)
	}
	high, err := compiled.RuntimeCatalog.ResolveWithExplicitEffort("worker", "codex", "fixture-a", model.EffortHigh, true)
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

func TestCompileACPCatalogRejectsDuplicateAliasesAndInvalidDefaults(t *testing.T) {
	target := fixtureGatewaySource("fixture-a", &fakeLLM{})
	tests := []struct {
		name  string
		input ACPCatalogInput
	}{
		{
			name: "duplicate alias",
			input: ACPCatalogInput{
				AgentTypes:  []identity.AgentName{"worker"},
				GatewayTargets: []ACPGatewaySource{target, target},
				Defaults: map[identity.AgentName]configuredDelegateDefault{
					"worker": {Harness: "codex", Model: "fixture-a", Effort: model.EffortMedium},
				},
				ClaudeSmall: "fixture-a",
			},
		},
		{
			name: "missing default",
			input: ACPCatalogInput{
				AgentTypes: []identity.AgentName{"worker"}, GatewayTargets: []ACPGatewaySource{target}, ClaudeSmall: "fixture-a",
			},
		},
		{
			name: "extra default",
			input: ACPCatalogInput{
				AgentTypes: []identity.AgentName{"worker"}, GatewayTargets: []ACPGatewaySource{target}, ClaudeSmall: "fixture-a",
				Defaults: map[identity.AgentName]configuredDelegateDefault{
					"worker": {Harness: "codex", Model: "fixture-a", Effort: model.EffortMedium},
					"other":  {Harness: "codex", Model: "fixture-a", Effort: model.EffortMedium},
				},
			},
		},
		{
			name: "unavailable default harness",
			input: ACPCatalogInput{
				AgentTypes: []identity.AgentName{"worker"}, GatewayTargets: []ACPGatewaySource{target},
				Defaults: map[identity.AgentName]configuredDelegateDefault{
					"worker": {Harness: "claude-code", Model: "fixture-a", Effort: model.EffortMedium},
				},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := CompileACPCatalog(test.input); err == nil {
				t.Fatal("CompileACPCatalog() succeeded")
			}
		})
	}
}

func TestACPGatewayTargetReturnsExactClientAndAuthoritativeEffort(t *testing.T) {
	client := &fakeLLM{}
	targetModel := model.CustomModel("openai", model.APIFormatOpenAIResponses, "", "provider-a", model.WithTools(), model.WithThinking())
	compiled, err := CompileACPCatalog(ACPCatalogInput{
		AgentTypes: []identity.AgentName{"worker"},
		GatewayTargets: []ACPGatewaySource{{
			Alias: "fixture-a", Client: client, Model: targetModel,
			DefaultEffort: model.EffortMedium, Efforts: []model.Effort{model.EffortLow, model.EffortMedium, model.EffortHigh},
		}},
		Defaults: map[identity.AgentName]configuredDelegateDefault{
			"worker": {Harness: "codex", Model: "fixture-a", Effort: model.EffortMedium},
		},
		ClaudeSmall: "fixture-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := compiled.RuntimeCatalog.ResolveWithExplicitEffort("worker", "codex", "fixture-a", model.EffortHigh, true)
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

func TestCompileACPCatalogErrorDoesNotEchoAliases(t *testing.T) {
	const sentinel = "test-secret-do-not-log"
	target := fixtureGatewaySource("fixture-a", &fakeLLM{})
	target.Alias = loop.ModelAlias(sentinel)
	_, err := CompileACPCatalog(ACPCatalogInput{
		AgentTypes:  []identity.AgentName{"worker"},
		GatewayTargets: []ACPGatewaySource{target, target},
		Defaults: map[identity.AgentName]configuredDelegateDefault{
			"worker": {Harness: "codex", Model: loop.ModelAlias(sentinel), Effort: model.EffortMedium},
		},
		ClaudeSmall: loop.ModelAlias(sentinel),
	})
	if err == nil {
		t.Fatal("CompileACPCatalog() succeeded")
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Fatalf("error exposed configured alias: %q", err)
	}
}

func compileFixtureACPCatalog(t *testing.T) ACPCompiledCatalog {
	t.Helper()
	compiled, err := CompileACPCatalog(ACPCatalogInput{
		AgentTypes:  []identity.AgentName{"worker"},
		GatewayTargets: []ACPGatewaySource{fixtureGatewaySource("fixture-a", &fakeLLM{})},
		Defaults: map[identity.AgentName]configuredDelegateDefault{
			"worker": {Harness: "codex", Model: "fixture-a", Effort: model.EffortMedium},
		},
		ClaudeSmall: "fixture-a",
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
