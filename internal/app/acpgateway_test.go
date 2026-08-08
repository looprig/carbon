package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/looprig/coderig/internal/catalog/generic"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/inference"
	"github.com/looprig/inference/gateway"
	model "github.com/looprig/inference/model"
)

func TestBuildACPGatewayPlanUsesStrictFixedRoutes(t *testing.T) {
	t.Parallel()
	compiled := testACPGatewayCatalog(t)

	claude, err := compiled.RuntimeCatalog.Resolve(generic.Name, "claude-code", "gpt-5.6-luna", model.EffortMax)
	if err != nil {
		t.Fatal(err)
	}
	claudePlan, err := buildACPGatewayPlan(compiled, claude)
	if err != nil {
		t.Fatal(err)
	}
	if len(claudePlan.routes) != 2 {
		t.Fatalf("Claude routes = %d, want 2", len(claudePlan.routes))
	}
	main, err := claudePlan.resolver.Resolve(context.Background(), model.APIFormatAnthropic, string(claude.TargetAlias))
	if err != nil {
		t.Fatal(err)
	}
	if main.Model.Sampling.Effort != model.EffortMax || !main.AuthoritativeEffort {
		t.Fatalf("Claude main target = %#v, want max/authoritative", main)
	}
	small, err := claudePlan.resolver.Resolve(context.Background(), model.APIFormatAnthropic, "sonnet-5")
	if err != nil {
		t.Fatal(err)
	}
	if small.Model.Sampling.Effort != model.EffortMedium {
		t.Fatalf("Claude small target effort = %q, want medium", small.Model.Sampling.Effort)
	}
	var unknown *gateway.UnknownRouteError
	if _, err := claudePlan.resolver.Resolve(context.Background(), model.APIFormatAnthropic, "not-admitted"); !errors.As(err, &unknown) {
		t.Fatalf("unknown Claude alias error = %v, want UnknownRouteError", err)
	}

	codex, err := compiled.RuntimeCatalog.Resolve(generic.Name, "codex", "gpt-5.6-luna", model.EffortMax)
	if err != nil {
		t.Fatal(err)
	}
	codexPlan, err := buildACPGatewayPlan(compiled, codex)
	if err != nil {
		t.Fatal(err)
	}
	if len(codexPlan.routes) != 1 {
		t.Fatalf("Codex routes = %d, want 1", len(codexPlan.routes))
	}
	target, err := codexPlan.resolver.Resolve(context.Background(), model.APIFormatOpenAIResponses, string(codex.TargetAlias))
	if err != nil {
		t.Fatal(err)
	}
	if target.Model.Provider != "openai" || target.Model.Sampling.Effort != model.EffortMax || !target.AuthoritativeEffort {
		t.Fatalf("Codex target = %#v", target)
	}
	if _, err := codexPlan.resolver.Resolve(context.Background(), model.APIFormatOpenAIResponses, "gpt-5.6-luna"); !errors.As(err, &unknown) {
		t.Fatalf("unknown Codex alias error = %v, want UnknownRouteError", err)
	}
}

func TestBuildACPGatewayPlanSeparatesClaudeMainAndSmallEffortAliases(t *testing.T) {
	t.Parallel()
	compiled := testACPGatewayCatalog(t)

	for _, effort := range []model.Effort{model.EffortMedium, model.EffortHigh, model.EffortMax} {
		effort := effort
		t.Run(string(effort), func(t *testing.T) {
			t.Parallel()
			selected, err := compiled.RuntimeCatalog.Resolve(generic.Name, "claude-code", "sonnet-5", effort)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := buildACPGatewayPlan(compiled, selected)
			if err != nil {
				t.Fatalf("buildACPGatewayPlan(%s) error = %v", effort, err)
			}
			wantRoutes := 1
			if effort != model.EffortMedium {
				wantRoutes = 2
			}
			if len(plan.routes) != wantRoutes {
				t.Fatalf("Claude routes = %d, want %d", len(plan.routes), wantRoutes)
			}
			target, err := plan.resolver.Resolve(context.Background(), model.APIFormatAnthropic, string(selected.TargetAlias))
			if err != nil {
				t.Fatal(err)
			}
			if target.Model.Sampling.Effort != effort || !target.AuthoritativeEffort {
				t.Fatalf("Claude main target = %#v, want %s/authoritative", target, effort)
			}
			if effort != model.EffortMedium {
				small, err := plan.resolver.Resolve(context.Background(), model.APIFormatAnthropic, "sonnet-5")
				if err != nil {
					t.Fatal(err)
				}
				if small.Model.Sampling.Effort != model.EffortMedium {
					t.Fatalf("Claude small target = %#v, want medium", small)
				}
			}
		})
	}
}

func TestNewACPGatewayNativeAuthHasNoBinding(t *testing.T) {
	t.Parallel()
	compiled, err := CompileAgentRuntimeCatalog(AgentRuntimeCatalogInput{
		NativeACP: map[string]ACPNativeProfile{
			"codex": {Harness: "codex", Enabled: true, Models: []loop.ModelAlias{"native-model"}},
		},
		PrimerTarget: runtimeCatalogPrimer(),
	})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := compiled.RuntimeCatalog.Resolve(generic.Name, "codex", "native-model", model.EffortNone)
	if err != nil {
		t.Fatal(err)
	}
	owned, err := NewACPGateway(context.Background(), compiled, resolved)
	if err != nil {
		t.Fatal(err)
	}
	if owned != nil {
		t.Fatalf("native gateway = %#v, want nil", owned)
	}
}

func TestNewACPGatewayCloseIsIdempotent(t *testing.T) {
	compiled := testACPGatewayCatalog(t)
	resolved, err := compiled.RuntimeCatalog.Resolve(generic.Name, "codex", "gpt-5.6-luna", model.EffortMax)
	if err != nil {
		t.Fatal(err)
	}
	owned, err := NewACPGateway(context.Background(), compiled, resolved)
	if err != nil {
		if strings.Contains(err.Error(), "operation not permitted") || strings.Contains(err.Error(), "listen tcp") {
			t.Skipf("loopback listeners unavailable in this sandbox: %v", err)
		}
		t.Fatal(err)
	}
	if owned.Binding().BaseURL == "" || owned.Binding().Token == "" {
		t.Fatalf("binding = %#v, want ready loopback binding", owned.Binding())
	}
	if err := owned.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := owned.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func testACPGatewayCatalog(t *testing.T) ACPCompiledCatalog {
	t.Helper()
	compiled, err := CompileAgentRuntimeCatalog(AgentRuntimeCatalogInput{
		GatewayTargets: legacyTestGatewayTargets(map[model.ProviderName]inference.Client{
			"anthropic": &fakeLLM{},
			"openai":    &fakeLLM{},
		}),
		ClaudeSmall: "sonnet-5",
	})
	if err != nil {
		t.Fatal(err)
	}
	return compiled
}

func legacyTestGatewayTargets(clients map[model.ProviderName]inference.Client) []ACPGatewaySource {
	definitions := []struct {
		alias    loop.ModelAlias
		provider model.ProviderName
		format   model.APIFormat
		name     string
		efforts  []model.Effort
	}{
		{alias: "fable-5", provider: "anthropic", format: model.APIFormatAnthropic, name: "claude-fable-5", efforts: []model.Effort{model.EffortLow, model.EffortMedium, model.EffortHigh, model.EffortMax}},
		{alias: "sonnet-5", provider: "anthropic", format: model.APIFormatAnthropic, name: "claude-sonnet-5", efforts: []model.Effort{model.EffortLow, model.EffortMedium, model.EffortHigh, model.EffortMax}},
		{alias: "opus-5", provider: "anthropic", format: model.APIFormatAnthropic, name: "claude-opus-5", efforts: []model.Effort{model.EffortLow, model.EffortMedium, model.EffortHigh, model.EffortMax}},
		{alias: "gpt-5.6-sol", provider: "openai", format: model.APIFormatOpenAIResponses, name: "gpt-5.6-sol", efforts: []model.Effort{model.EffortNone, model.EffortLow, model.EffortMedium, model.EffortHigh, model.EffortMax}},
		{alias: "gpt-5.6-terra", provider: "openai", format: model.APIFormatOpenAIResponses, name: "gpt-5.6-terra", efforts: []model.Effort{model.EffortNone, model.EffortLow, model.EffortMedium, model.EffortHigh, model.EffortMax}},
		{alias: "gpt-5.6-luna", provider: "openai", format: model.APIFormatOpenAIResponses, name: "gpt-5.6-luna", efforts: []model.Effort{model.EffortNone, model.EffortLow, model.EffortMedium, model.EffortHigh, model.EffortMax}},
	}
	var targets []ACPGatewaySource
	for _, definition := range definitions {
		client := clients[definition.provider]
		if client == nil {
			continue
		}
		targets = append(targets, ACPGatewaySource{
			Alias: definition.alias, Client: client,
			Model:         model.CustomModel(definition.provider, definition.format, "", definition.name, model.WithTools(), model.WithThinking()),
			DefaultEffort: model.EffortMedium, Efforts: append([]model.Effort(nil), definition.efforts...),
		})
	}
	return targets
}
