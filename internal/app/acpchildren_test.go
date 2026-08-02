package app

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/looprig/harness/pkg/foreign"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
	model "github.com/looprig/inference/model"
)

func TestACPPostureForRole(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		role string
		want string
	}{
		{role: "planner", want: "read-only"},
		{role: "reviewer", want: "read-only"},
		{role: "builder", want: "workspace-write"},
	} {
		t.Run(test.role, func(t *testing.T) {
			got, err := acpPostureFor(test.role)
			if err != nil || string(got) != test.want {
				t.Fatalf("posture = %q, %v; want %q", got, err, test.want)
			}
		})
	}
	for _, role := range []string{"operator", "unknown"} {
		if _, err := acpPostureFor(role); err == nil {
			t.Fatalf("stale/unknown role posture for %q succeeded", role)
		}
	}
}

func TestNewACPCompositionPreflightsProfilesAndFiltersEnv(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	compiled := testACPGatewayCatalog(t)
	composition, err := NewACPComposition(ACPChildrenConfig{
		Catalog:       compiled,
		Executables:   map[loop.AgentHarnessName]string{"claude-code": executable, "codex": "relative/codex"},
		WorkspaceRoot: t.TempDir(),
		Env:           []string{"PATH=/bin", "SECRET=must-not-pass", "LANG=C"},
		EnvAllowlist:  []string{"PATH", "LANG"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := composition.Registry.Builder("acp/claude-code"); err != nil {
		t.Fatalf("Claude profile missing: %v", err)
	}
	if _, _, err := composition.Registry.Builder("acp/codex"); err == nil {
		t.Fatal("Codex profile registered despite failed executable preflight")
	}
	if !composition.Catalog.HasProfile("acp/claude-code") {
		t.Fatal("Claude profile disappeared from the filtered catalog")
	}
	if composition.Catalog.HasProfile("acp/codex") || len(composition.Catalog.RuntimeCatalog.EntriesFor("worker")) != 1 {
		t.Fatalf("filtered catalog still advertises the failed Codex connector: %#v", composition.Catalog.RuntimeCatalog.EntriesFor("worker"))
	}
	if got := filterACPEnv([]string{"PATH=/bin", "SECRET=x", "LANG=C"}, []string{"PATH", "LANG"}); len(got) != 2 || got[0] != "PATH=/bin" || got[1] != "LANG=C" {
		t.Fatalf("filtered env = %#v", got)
	}
}

func TestACPChildEnvIsCredentialScoped(t *testing.T) {
	config := ACPChildrenConfig{
		Env: []string{
			"HOME=/Users/alice",
			"XDG_CONFIG_HOME=/Users/alice/.config",
			"XDG_DATA_HOME=/Users/alice/.local/share",
			"PATH=/usr/bin",
			"LANG=C",
			"ANTHROPIC_API_KEY=should-not-pass",
			"SECRET_TOKEN=should-not-pass",
		},
		NativeEnvAllowlist:  acpNativeAuthEnvAllowlist,
		GatewayEnvAllowlist: acpGatewayEnvAllowlist,
	}

	native := config.envForCredential(loop.CredentialNativeAuth)
	if !containsEnv(native, "HOME=/Users/alice") ||
		!containsEnv(native, "XDG_CONFIG_HOME=/Users/alice/.config") ||
		!containsEnv(native, "XDG_DATA_HOME=/Users/alice/.local/share") {
		t.Fatalf("native env lost login locations: %#v", native)
	}
	if !containsEnv(native, "PATH=/usr/bin") || !containsEnv(native, "LANG=C") {
		t.Fatalf("native env lost safe process configuration: %#v", native)
	}

	gateway := config.envForCredential(loop.CredentialGatewayBacked)
	if containsEnvKey(gateway, "HOME") || containsEnvKey(gateway, "XDG_CONFIG_HOME") || containsEnvKey(gateway, "XDG_DATA_HOME") {
		t.Fatalf("gateway env inherited harness login locations: %#v", gateway)
	}
	if containsEnvKey(gateway, "ANTHROPIC_API_KEY") || containsEnvKey(gateway, "SECRET_TOKEN") {
		t.Fatalf("gateway env inherited a secret: %#v", gateway)
	}
	if !containsEnv(gateway, "PATH=/usr/bin") || !containsEnv(gateway, "LANG=C") {
		t.Fatalf("gateway env lost safe process configuration: %#v", gateway)
	}
}

func containsEnv(env []string, wanted string) bool {
	for _, entry := range env {
		if entry == wanted {
			return true
		}
	}
	return false
}

func containsEnvKey(env []string, key string) bool {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return true
		}
	}
	return false
}

func TestNewACPCompositionBuildsNativeAuthProfileWithoutGateway(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
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
	if resolved.Profile != "acp/codex" || resolved.Credential != loop.CredentialNativeAuth {
		t.Fatalf("native runtime = %+v, want ACP native-auth profile", resolved)
	}
	composition, err := NewACPComposition(ACPChildrenConfig{
		Catalog:       compiled,
		Executables:   map[loop.AgentHarnessName]string{"codex": executable},
		WorkspaceRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := composition.Registry.Builder("acp/codex"); err != nil {
		t.Fatalf("native ACP profile missing from registry: %v", err)
	}
}

func TestProductionACPCompositionWithoutCredentialsIsEmptyAndBounded(t *testing.T) {
	for _, name := range []string{acpAnthropicAPIKeyEnv, acpOpenAIAPIKeyEnv, acpClaudeExecutableEnv, acpCodexExecutableEnv} {
		t.Setenv(name, "")
	}
	composition, err := newProductionACPComposition()
	if err != nil {
		t.Fatal(err)
	}
	if composition == nil || composition.Catalog.RuntimeCatalog.HasEntries() {
		t.Fatalf("production no-credential composition = %#v, want empty catalog", composition)
	}
}

func TestACPBoundRuntimeResolutionUsesPinnedSelectors(t *testing.T) {
	t.Parallel()
	compiled, err := CompileACPCatalog(ACPCatalogInput{
		SubagentTypes:  []identity.AgentName{"builder"},
		GatewayClients: map[model.ProviderName]inference.Client{"anthropic": &fakeLLM{}, "openai": &fakeLLM{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	definition, err := loop.Define(
		loop.WithName(identity.AgentName("builder")),
		loop.WithInference(&fakeLLM{}, testModel()),
		loop.WithPolicyRevision("acp-child-test"),
	)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := definition.Bind(context.Background(), tool.Bindings{SessionID: mustUUID(t), LoopID: mustUUID(t)})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := compiled.RuntimeCatalog.Resolve("builder", "codex", "gpt-5.6-luna", model.EffortMax)
	if err != nil {
		t.Fatal(err)
	}
	bound, err = loop.OverrideBoundRuntimeSelection(bound, resolved.Profile, resolved.ModelAlias, resolved.Target, resolved.Effort)
	if err != nil {
		t.Fatal(err)
	}
	got, harness, err := resolveACPBoundRuntime(compiled, bound)
	if err != nil {
		t.Fatal(err)
	}
	if harness != "codex" || got.ModelAlias != "gpt-5.6-luna" || got.Effort != model.EffortMax {
		t.Fatalf("resolved = %#v harness=%q", got, harness)
	}
}

func TestNewACPCompositionWithoutCatalogHasNoProfiles(t *testing.T) {
	t.Parallel()
	composition, err := NewACPComposition(ACPChildrenConfig{Executables: map[loop.AgentHarnessName]string{"codex": "/bin/sh"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := composition.Registry.Builder("acp/codex"); err == nil {
		t.Fatal("empty catalog registered ACP profile")
	}
	if composition.Live == nil || composition.Restored == nil {
		t.Fatal("composition did not install bounded dispatchers")
	}
	var unknown *foreign.UnknownProfileError
	_, _, err = composition.Registry.Builder("acp/codex")
	if !errors.As(err, &unknown) {
		t.Fatalf("empty registry error = %v, want UnknownProfileError", err)
	}
}
