package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/inference"
	"github.com/looprig/inference/gateway"
	model "github.com/looprig/inference/model"
)

func TestBuildACPGatewayPlanUsesStrictFixedRoutes(t *testing.T) {
	t.Parallel()
	compiled := testACPGatewayCatalog(t)

	claude, err := compiled.RuntimeCatalog.Resolve("worker", "claude-code", "gpt-5.6-luna", model.EffortMax)
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
	main, err := claudePlan.resolver.Resolve(context.Background(), model.APIFormatAnthropic, "gpt-5.6-luna")
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

	codex, err := compiled.RuntimeCatalog.Resolve("worker", "codex", "gpt-5.6-luna", model.EffortMax)
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
	target, err := codexPlan.resolver.Resolve(context.Background(), model.APIFormatOpenAIResponses, "gpt-5.6-luna")
	if err != nil {
		t.Fatal(err)
	}
	if target.Model.Provider != "openai" || target.Model.Sampling.Effort != model.EffortMax || !target.AuthoritativeEffort {
		t.Fatalf("Codex target = %#v", target)
	}
	if _, err := codexPlan.resolver.Resolve(context.Background(), model.APIFormatOpenAIResponses, "sonnet-5"); !errors.As(err, &unknown) {
		t.Fatalf("unknown Codex alias error = %v, want UnknownRouteError", err)
	}
}

func TestBuildACPGatewayPlanRejectsClaudeMainSmallRouteEffortCollision(t *testing.T) {
	t.Parallel()
	compiled := testACPGatewayCatalog(t)

	for _, effort := range []model.Effort{model.EffortHigh, model.EffortMax} {
		effort := effort
		t.Run(string(effort), func(t *testing.T) {
			t.Parallel()
			selected, err := compiled.RuntimeCatalog.Resolve("worker", "claude-code", "sonnet-5", effort)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := buildACPGatewayPlan(compiled, selected); err == nil {
				t.Fatalf("buildACPGatewayPlan() accepted one Claude route for %s main effort and default small effort", effort)
			} else if !strings.Contains(err.Error(), "main and small model route collision") {
				t.Fatalf("buildACPGatewayPlan() error = %q, want route collision", err)
			}
		})
	}
}

func TestBuildACPGatewayPlanAllowsClaudeMainSmallRouteAtDefaultEffort(t *testing.T) {
	t.Parallel()
	compiled := testACPGatewayCatalog(t)

	selected, err := compiled.RuntimeCatalog.Resolve("worker", "claude-code", "sonnet-5", model.EffortMedium)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := buildACPGatewayPlan(compiled, selected)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.routes) != 1 {
		t.Fatalf("Claude routes = %d, want one shared default-effort route", len(plan.routes))
	}
	target, err := plan.resolver.Resolve(context.Background(), model.APIFormatAnthropic, "sonnet-5")
	if err != nil {
		t.Fatal(err)
	}
	if target.Model.Sampling.Effort != model.EffortMedium || !target.AuthoritativeEffort {
		t.Fatalf("shared Claude target = %#v, want medium/authoritative", target)
	}
}

func TestNewACPGatewayNativeAuthHasNoBinding(t *testing.T) {
	t.Parallel()
	compiled, err := CompileACPCatalog(ACPCatalogInput{
		SubagentTypes: []identity.AgentName{"worker"},
		NativeAuth: []ACPNativeAuthSource{{
			Harness: "codex", Alias: "native-model", Model: testModel(),
			DefaultEffort: model.EffortNone, Efforts: []model.Effort{model.EffortNone},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := compiled.RuntimeCatalog.Resolve("worker", "codex", "native-model", model.EffortNone)
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
	resolved, err := compiled.RuntimeCatalog.Resolve("worker", "codex", "gpt-5.6-luna", model.EffortMax)
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
	compiled, err := CompileACPCatalog(ACPCatalogInput{
		SubagentTypes: []identity.AgentName{"worker"},
		GatewayClients: map[model.ProviderName]inference.Client{
			"anthropic": &fakeLLM{},
			"openai":    &fakeLLM{},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return compiled
}
