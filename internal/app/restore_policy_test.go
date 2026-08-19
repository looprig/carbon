package app

import (
	"context"
	"testing"

	"github.com/looprig/carbon/internal/catalog/carbon"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/session"
	model "github.com/looprig/inference/model"
)

func TestCarbonRestorePolicyAcceptsEphemeralWarnDrift(t *testing.T) {
	t.Parallel()
	assessment := event.DriftAssessment{Changes: []event.DriftChange{
		{Category: event.DriftExternal, Severity: event.DriftWarn},
		{Category: event.DriftRuntime, Field: "catalog_rev", Severity: event.DriftWarn},
		{Category: event.DriftRuntimeSkills, Severity: event.DriftWarn},
		{Category: event.DriftPermission, Severity: event.DriftWarn},
		{Category: event.DriftPermission, Field: "posture", Severity: event.DriftWarn},
		{Category: event.DriftConfinement, Severity: event.DriftWarn},
		{Category: event.DriftApp, Field: "access_profile", Severity: event.DriftWarn},
	}}
	decision, err := (carbonRestoreDecider{}).DecideRestore(context.Background(), assessment)
	if err != nil {
		t.Fatal(err)
	}
	if !decision.Accept || decision.Source != event.DecisionSourcePolicy || decision.Message == "" {
		t.Fatalf("decision = %+v, want bounded policy acceptance", decision)
	}
}

func TestCarbonRestorePolicyRejectsCriticalWarnDrift(t *testing.T) {
	t.Parallel()
	tests := []event.DriftChange{
		{Category: event.DriftWorkspace, Severity: event.DriftWarn},
		{Category: event.DriftTrust, Severity: event.DriftWarn},
		{Category: event.DriftAgentKind, Severity: event.DriftWarn},
		{Category: event.DriftAgentName, Severity: event.DriftWarn},
		{Category: event.DriftAdapter, Severity: event.DriftWarn},
		{Category: event.DriftHookPolicy, Severity: event.DriftWarn},
		{Category: event.DriftRuntime, Field: "profile", Severity: event.DriftWarn},
		{Category: event.DriftRuntime, Field: "identity_rev", Severity: event.DriftWarn},
		{Category: event.DriftPermission, Field: "review_configured", Severity: event.DriftWarn},
		{Category: event.DriftPermission, Field: "review_policy_rev", Severity: event.DriftWarn},
	}
	for _, change := range tests {
		change := change
		t.Run(string(change.Category)+"/"+change.Field, func(t *testing.T) {
			decision, err := (carbonRestoreDecider{}).DecideRestore(context.Background(), event.DriftAssessment{Changes: []event.DriftChange{change}})
			if err != nil {
				t.Fatal(err)
			}
			if decision.Accept {
				t.Fatalf("decision = %+v, want rejection for %+v", decision, change)
			}
		})
	}
}

func TestCarbonRuntimeRestoreResolverSelectsCurrentDefaultForSameHarness(t *testing.T) {
	t.Parallel()
	catalog, err := loop.NewRuntimeCatalog([]loop.RuntimeCatalogEntry{intTestCatalogEntry("codex", "current", model.Model{Provider: "provider", Name: "current-target"}, true)})
	if err != nil {
		t.Fatal(err)
	}
	request := session.RuntimeRestoreRequest{
		AgentName: identity.AgentName(carbon.Name),
		Harness:   "codex",
		Profile:   "acp/codex",
		Mismatch:  session.RestoreRuntimeTargetMismatch,
		Catalog:   catalog,
	}
	resolved, err := (carbonRuntimeRestoreResolver{}).ResolveRuntimeRestore(context.Background(), request)
	if err != nil {
		t.Fatalf("ResolveRuntimeRestore: %v", err)
	}
	if resolved.AgentHarness != "codex" || resolved.ModelAlias != "current" || resolved.Target.Name != "current-target" {
		t.Fatalf("resolved = %+v, want current codex default", resolved)
	}
}

func intTestCatalogEntry(harness loop.AgentHarnessName, alias loop.ModelAlias, target model.Model, isDefault bool) loop.RuntimeCatalogEntry {
	return loop.RuntimeCatalogEntry{
		AgentType: identity.AgentName(carbon.Name), AgentHarness: harness, Profile: loop.RuntimeProfileName("acp/" + string(harness)),
		Credential: loop.CredentialGatewayBacked, Default: isDefault, DefaultModel: alias,
		Models: []loop.RuntimeModelOption{{Alias: alias, Target: target, DefaultEffort: model.EffortMedium, Efforts: []model.Effort{model.EffortMedium}}},
	}
}
