package app

import (
	"context"
	"os"
	"testing"

	"github.com/looprig/harness/pkg/loop"
	model "github.com/looprig/inference/model"
)

func TestProductionACPCompositionUsesNativeDiscoverySeam(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{acpAnthropicAPIKeyEnv, acpOpenAIAPIKeyEnv} {
		t.Setenv(name, "")
	}
	t.Setenv(acpClaudeExecutableEnv, executable)
	t.Setenv(acpCodexExecutableEnv, executable)

	var probes []ACPNativeAuthProbe
	composition, err := newProductionACPCompositionWithDiscovery(context.Background(), func(_ context.Context, probe ACPNativeAuthProbe) ([]ACPNativeAuthSource, error) {
		probes = append(probes, probe)
		if probe.Harness != "claude-code" {
			return nil, nil
		}
		return []ACPNativeAuthSource{{
			Harness: "claude-code", Alias: "native-claude-model", Model: testModel(),
			SmallModel: "native-claude-small", DefaultEffort: model.EffortNone,
			Efforts: []model.Effort{model.EffortNone},
		}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(probes) != 2 {
		t.Fatalf("native discovery probes = %d, want 2", len(probes))
	}
	if composition == nil {
		t.Fatal("composition = nil")
	}

	entries := composition.Catalog.RuntimeCatalog.EntriesFor("builder")
	if len(entries) != 1 || entries[0].AgentHarness != "claude-code" {
		t.Fatalf("operator entries = %#v, want Claude native entry", entries)
	}
	resolved, err := composition.Catalog.RuntimeCatalog.Resolve("builder", "claude-code", "native-claude-model", model.EffortNone)
	if err != nil {
		t.Fatalf("native Claude resolve: %v", err)
	}
	if resolved.Credential != loop.CredentialNativeAuth {
		t.Fatalf("credential = %q, want %q", resolved.Credential, loop.CredentialNativeAuth)
	}
}

func TestDiscoverProductionACPNativeAuthUsesOnlyBoundedModelProjection(t *testing.T) {
	t.Parallel()

	sources, err := discoverProductionACPNativeAuth(context.Background(), ACPNativeAuthProbe{
		Harness: "claude-code",
		Env: []string{
			"HOME=/private/login-home",
			"ANTHROPIC_API_KEY=must-not-be-read",
			"CLAUDE_CODE_NATIVE_MODELS=native=wrong-name",
			acpClaudeNativeModelsEnv + "=subscription=claude-sonnet-native,fast=claude-haiku-native",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 2 {
		t.Fatalf("native sources = %d, want 2", len(sources))
	}
	if sources[0].Alias != "subscription" || sources[0].Model.Name != "claude-sonnet-native" {
		t.Fatalf("first native source = %#v", sources[0])
	}
	if sources[0].SmallModel != "claude-sonnet-native" || sources[1].SmallModel != "claude-sonnet-native" {
		t.Fatalf("Claude small model values = %q, %q", sources[0].SmallModel, sources[1].SmallModel)
	}

	codex, err := discoverProductionACPNativeAuth(context.Background(), ACPNativeAuthProbe{
		Harness: "codex",
		Env:     []string{acpCodexNativeModelsEnv + "=native=gpt-native"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(codex) != 1 || codex[0].Model.Name != "gpt-native" || codex[0].SmallModel != "" {
		t.Fatalf("Codex native source = %#v", codex)
	}
}

func TestProductionACPCompositionDiscoversConfiguredNativeModels(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{acpAnthropicAPIKeyEnv, acpOpenAIAPIKeyEnv} {
		t.Setenv(name, "")
	}
	t.Setenv(acpClaudeExecutableEnv, executable)
	t.Setenv(acpCodexExecutableEnv, "")
	t.Setenv(acpClaudeNativeModelsEnv, "subscription=claude-subscription-model")
	t.Setenv(acpCodexNativeModelsEnv, "")

	composition, err := newProductionACPComposition()
	if err != nil {
		t.Fatal(err)
	}
	entries := composition.Catalog.RuntimeCatalog.EntriesFor("builder")
	if len(entries) != 1 || entries[0].AgentHarness != "claude-code" {
		t.Fatalf("native production entries = %#v, want one Claude entry", entries)
	}
	resolved, err := composition.Catalog.RuntimeCatalog.Resolve("builder", "claude-code", "subscription", model.EffortNone)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Credential != loop.CredentialNativeAuth {
		t.Fatalf("resolved credential = %q, want native-auth", resolved.Credential)
	}
}

func TestProductionACPCompositionNativeDiscoveryCoexistsWithGatewayAndOmitsAbsentLogin(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(acpAnthropicAPIKeyEnv, "")
	t.Setenv(acpOpenAIAPIKeyEnv, "test-openai-key")
	t.Setenv(acpClaudeExecutableEnv, executable)
	t.Setenv(acpCodexExecutableEnv, executable)

	composition, err := newProductionACPCompositionWithDiscovery(context.Background(), func(_ context.Context, probe ACPNativeAuthProbe) ([]ACPNativeAuthSource, error) {
		if probe.Harness != "codex" {
			return nil, nil
		}
		return []ACPNativeAuthSource{{
			Harness: "codex", Alias: "native-codex-model", Model: testModel(),
			DefaultEffort: model.EffortNone, Efforts: []model.Effort{model.EffortNone},
		}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	entries := composition.Catalog.RuntimeCatalog.EntriesFor("builder")
	var codex loop.RuntimeCatalogEntry
	for _, entry := range entries {
		if entry.AgentHarness == "codex" {
			codex = entry
			break
		}
	}
	if codex.AgentHarness != "codex" {
		t.Fatalf("codex entry missing: %#v", entries)
	}
	var nativeFound, gatewayFound bool
	for _, option := range codex.Models {
		switch option.Alias {
		case "native-codex-model":
			nativeFound = option.Credential == loop.CredentialNativeAuth
		case "gpt-5.6-luna":
			gatewayFound = true
		}
	}
	if !nativeFound || !gatewayFound {
		t.Fatalf("codex mixed models native=%v gateway=%v: %#v", nativeFound, gatewayFound, codex.Models)
	}
	if len(composition.Catalog.RuntimeCatalog.EntriesFor("builder")) != 1 {
		t.Fatalf("absent Claude login unexpectedly added a native row: %#v", entries)
	}
}
