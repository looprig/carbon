package app

import (
	"context"
	"reflect"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/inference"
	model "github.com/looprig/inference/model"
	"github.com/looprig/inference/stream"
)

func TestCompileAgentRuntimeCatalogWithoutACP(t *testing.T) {
	clientA := &runtimeCatalogClient{}
	clientB := &runtimeCatalogClient{}
	targets := []ACPGatewaySource{
		{
			Alias: "alpha", Description: "Fast local coding model.", Client: clientA,
			Model:         model.CustomModel("openai", model.APIFormatOpenAIResponses, "", "alpha-model", model.WithTools(), model.WithThinking()),
			DefaultEffort: model.EffortMedium,
			Efforts:       []model.Effort{model.EffortLow, model.EffortMedium},
		},
		{
			Alias: "beta", Description: "Deep local coding model.", Client: clientB,
			Model:         model.CustomModel("anthropic", model.APIFormatAnthropic, "", "beta-model", model.WithTools(), model.WithThinking()),
			DefaultEffort: model.EffortHigh,
			Efforts:       []model.Effort{model.EffortHigh, model.EffortMax},
		},
	}
	compiled, err := CompileAgentRuntimeCatalog(AgentRuntimeCatalogInput{
		AgentTypes:     []identity.AgentName{"planner", "builder", "reviewer"},
		GatewayTargets: targets,
	})
	if err != nil {
		t.Fatalf("CompileAgentRuntimeCatalog() error = %v", err)
	}

	for _, role := range []identity.AgentName{"planner", "builder", "reviewer"} {
		entries := compiled.RuntimeCatalog.EntriesFor(role)
		if len(entries) != 1 {
			t.Fatalf("EntriesFor(%q) = %d rows, want one ordinary row: %#v", role, len(entries), entries)
		}
		entry := entries[0]
		if entry.AgentHarness != looprigRuntimeHarness || entry.Profile != looprigRuntimeProfile || entry.Source != loop.RuntimeSourceNative || !entry.Default {
			t.Fatalf("ordinary entry = %#v", entry)
		}
		if entry.Description != looprigRuntimeDescription {
			t.Fatalf("ordinary description = %q", entry.Description)
		}
		for _, target := range targets {
			option, ok := runtimeOptionByAlias(entry.Models, target.Alias)
			if !ok || option.Description != target.Description {
				t.Fatalf("model %q option = %#v, want description %q", target.Alias, option, target.Description)
			}
			for _, effort := range target.Efforts {
				resolved, err := compiled.RuntimeCatalog.ResolveWithExplicitSource(role, looprigRuntimeHarness, loop.RuntimeSourceNative, target.Alias, effort, true)
				if err != nil {
					t.Fatalf("Resolve(%q, %q, %q) error = %v", role, target.Alias, effort, err)
				}
				client, selected, err := compiled.NativeTarget(resolved)
				if err != nil {
					t.Fatalf("NativeTarget(%q, %q) error = %v", target.Alias, effort, err)
				}
				wantClient := clientA
				if target.Alias == "beta" {
					wantClient = clientB
				}
				if client != wantClient || selected.Key() != target.Model.Key() || selected.Sampling.Effort != effort {
					t.Fatalf("NativeTarget(%q, %q) = client %T model %#v", target.Alias, effort, client, selected)
				}
			}
		}
		for _, candidate := range entries {
			if candidate.AgentHarness == "codex" || candidate.AgentHarness == "claude-code" {
				t.Fatalf("ACP row unexpectedly present: %#v", candidate)
			}
		}
	}
}

func TestCompileAgentRuntimeCatalogMergesACPRowsAndDescriptions(t *testing.T) {
	target := ACPGatewaySource{
		Alias: "alpha", Description: "Configured coding model.", Client: &runtimeCatalogClient{},
		Model:         model.CustomModel("openai", model.APIFormatOpenAIResponses, "", "alpha-model", model.WithTools(), model.WithThinking()),
		DefaultEffort: model.EffortMedium,
		Efforts:       []model.Effort{model.EffortMedium, model.EffortHigh},
	}
	defaults := map[identity.AgentName]configuredDelegateDefault{
		"planner":  {Harness: "codex", Model: "alpha", Effort: model.EffortMedium},
		"builder":  {Harness: "codex", Model: "alpha", Effort: model.EffortMedium},
		"reviewer": {Harness: "codex", Model: "alpha", Effort: model.EffortMedium},
	}
	acp, err := CompileACPCatalog(ACPCatalogInput{
		AgentTypes: []identity.AgentName{"planner", "builder", "reviewer"}, GatewayTargets: []ACPGatewaySource{target},
		Defaults: defaults,
	})
	if err != nil {
		t.Fatalf("CompileACPCatalog() error = %v", err)
	}
	compiled, err := CompileAgentRuntimeCatalog(AgentRuntimeCatalogInput{
		AgentTypes: []identity.AgentName{"planner", "builder", "reviewer"}, GatewayTargets: []ACPGatewaySource{target},
		Defaults: defaults, ACP: acp,
	})
	if err != nil {
		t.Fatalf("CompileAgentRuntimeCatalog() error = %v", err)
	}

	entries := compiled.RuntimeCatalog.EntriesFor("planner")
	if len(entries) != 2 {
		t.Fatalf("planner entries = %d, want looprig + codex rows: %#v", len(entries), entries)
	}
	var ordinary, codex loop.RuntimeCatalogEntry
	for _, entry := range entries {
		switch entry.AgentHarness {
		case looprigRuntimeHarness:
			ordinary = entry
		case "codex":
			codex = entry
		}
	}
	if ordinary.Profile != looprigRuntimeProfile || ordinary.Default {
		t.Fatalf("ordinary row = %#v, want non-default alongside configured ACP default", ordinary)
	}
	if !codex.Default || codex.Description != codexRuntimeDescription {
		t.Fatalf("codex row = %#v", codex)
	}
	option, ok := runtimeOptionByAlias(codex.Models, "alpha")
	if !ok || option.Description != target.Description {
		t.Fatalf("codex model option = %#v", option)
	}
	resolved, err := compiled.RuntimeCatalog.Resolve("planner", "", "", model.EffortNone)
	if err != nil || resolved.AgentHarness != "codex" {
		t.Fatalf("default resolve = %#v, %v", resolved, err)
	}
}

func TestFilterACPPreflightCatalogKeepsOrdinaryRowsWhenACPFails(t *testing.T) {
	target := ACPGatewaySource{
		Alias: "alpha", Description: "Configured coding model.", Client: &runtimeCatalogClient{},
		Model:         model.CustomModel("openai", model.APIFormatOpenAIResponses, "", "alpha-model", model.WithTools(), model.WithThinking()),
		DefaultEffort: model.EffortMedium, Efforts: []model.Effort{model.EffortMedium},
	}
	acp, err := CompileACPCatalog(ACPCatalogInput{
		AgentTypes: []identity.AgentName{"planner"}, GatewayTargets: []ACPGatewaySource{target},
		Defaults: map[identity.AgentName]configuredDelegateDefault{"planner": {Harness: "codex", Model: "alpha", Effort: model.EffortMedium}},
	})
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := CompileAgentRuntimeCatalog(AgentRuntimeCatalogInput{
		AgentTypes: []identity.AgentName{"planner"}, GatewayTargets: []ACPGatewaySource{target}, ACP: acp,
	})
	if err != nil {
		t.Fatal(err)
	}
	filtered, err := filterACPPreflightCatalog(compiled, nil)
	if err != nil {
		t.Fatal(err)
	}
	entries := filtered.RuntimeCatalog.EntriesFor("planner")
	if len(entries) != 1 || entries[0].AgentHarness != looprigRuntimeHarness || !entries[0].Default {
		t.Fatalf("filtered entries = %#v, want ordinary default only", entries)
	}
}

func runtimeOptionByAlias(options []loop.RuntimeModelOption, alias loop.ModelAlias) (loop.RuntimeModelOption, bool) {
	for _, option := range options {
		if option.Alias == alias {
			return option, true
		}
	}
	return loop.RuntimeModelOption{}, false
}

func TestCompileAgentRuntimeCatalogEntriesAreDeterministic(t *testing.T) {
	first := testRuntimeCatalogCompilation(t, false)
	second := testRuntimeCatalogCompilation(t, true)
	if first.RuntimeCatalog.Digest() != second.RuntimeCatalog.Digest() {
		t.Fatalf("catalog digest changed with input order: %s != %s", first.RuntimeCatalog.Digest(), second.RuntimeCatalog.Digest())
	}
	if !reflect.DeepEqual(first.RuntimeCatalog.EntriesFor("planner"), second.RuntimeCatalog.EntriesFor("planner")) {
		t.Fatal("catalog entries changed with input order")
	}
}

func testRuntimeCatalogCompilation(t *testing.T, reverse bool) ACPCompiledCatalog {
	t.Helper()
	targets := []ACPGatewaySource{
		{Alias: "zeta", Description: "Zeta model.", Client: &runtimeCatalogClient{}, Model: model.CustomModel("openai", model.APIFormatOpenAIResponses, "", "zeta", model.WithTools(), model.WithThinking()), DefaultEffort: model.EffortLow, Efforts: []model.Effort{model.EffortLow}},
		{Alias: "alpha", Description: "Alpha model.", Client: &runtimeCatalogClient{}, Model: model.CustomModel("openai", model.APIFormatOpenAIResponses, "", "alpha", model.WithTools(), model.WithThinking()), DefaultEffort: model.EffortLow, Efforts: []model.Effort{model.EffortLow}},
	}
	roles := []identity.AgentName{"reviewer", "planner"}
	if reverse {
		targets[0], targets[1] = targets[1], targets[0]
		roles[0], roles[1] = roles[1], roles[0]
	}
	compiled, err := CompileAgentRuntimeCatalog(AgentRuntimeCatalogInput{AgentTypes: roles, GatewayTargets: targets})
	if err != nil {
		t.Fatal(err)
	}
	return compiled
}

type runtimeCatalogClient struct{}

func (*runtimeCatalogClient) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	return nil, nil
}

func (*runtimeCatalogClient) Stream(context.Context, inference.Request) (*stream.StreamReader[content.Chunk], error) {
	return nil, nil
}
