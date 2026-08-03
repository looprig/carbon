package app

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/looprig/acp/launch"
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

func TestBoundedACPChildErrorDoesNotExposeProcessDetails(t *testing.T) {
	t.Parallel()
	cause := errors.New("stdio: process exited at /private/login/home; https://provider.invalid/token")
	got := boundedACPChildError(cause)
	if got.Error() != "coderig: ACP child unavailable" {
		t.Fatalf("bounded error = %q, want fixed category", got)
	}
	if strings.Contains(got.Error(), "/private/login/home") || strings.Contains(got.Error(), "provider.invalid") {
		t.Fatalf("bounded error leaked process details: %q", got)
	}
	if boundedACPChildError(context.Canceled) != context.Canceled {
		t.Fatal("context cancellation was not preserved")
	}
}

func TestNewACPCompositionPreflightsProfilesAndFiltersEnv(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	compiled := testACPGatewayCatalog(t)
	composition, err := NewACPComposition(ACPChildrenConfig{
		Catalog:                 compiled,
		Executables:             map[loop.AgentHarnessName]string{"claude-code": executable, "codex": "relative/codex"},
		WorkspaceRoot:           t.TempDir(),
		Env:                     []string{"PATH=/bin", "SECRET=must-not-pass", "LANG=C"},
		EnvAllowlist:            []string{"PATH", "LANG"},
		gatewayPreflightBinding: &launch.ProxyBinding{BaseURL: "http://127.0.0.1:1", Token: "test-token"},
		executablePreflight: func(context.Context, ACPExecutableProbe) ACPPreflightResult {
			return ACPPreflightResult{Ready: true, AdvertisedModels: []string{"fable-5", "sonnet-5", "opus-5", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"}}
		},
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
		Defaults: map[identity.AgentName]configuredDelegateDefault{
			"worker": {Harness: "codex", Model: "native-model", Effort: model.EffortNone},
		},
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
		executablePreflight: func(context.Context, ACPExecutableProbe) ACPPreflightResult {
			return ACPPreflightResult{Ready: true}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := composition.Registry.Builder("acp/codex"); err != nil {
		t.Fatalf("native ACP profile missing from registry: %v", err)
	}
}

func TestACPChildEnvironmentAndGatewayPreflightExcludeParentSecrets(t *testing.T) {
	t.Parallel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	compiled := testACPGatewayCatalog(t)
	probes := make([]ACPExecutableProbe, 0, 2)
	composition, err := NewACPComposition(ACPChildrenConfig{
		Catalog:       compiled,
		Executables:   map[loop.AgentHarnessName]string{"claude-code": executable, "codex": executable},
		WorkspaceRoot: "/workspace/project",
		Env: []string{
			"HOME=/private/login",
			"XDG_CONFIG_HOME=/private/login/.config",
			"PATH=/usr/bin",
			"LANG=C",
			"PROVIDER_SENTINEL=task6-obvious-fake-provider-key",
			"MODELS_JSON_PATH=/private/login/.looprig/models.json",
			`MODELS_JSON_CONTENT={"api_key":"task6-obvious-fake-provider-key"}`,
			"NATIVE_PERMISSION_PATH=/private/login/.looprig/workspaces/hash/permissions.json",
			`NATIVE_PERMISSION_CONTENT={"rules":["must-not-pass"]}`,
			"ANTHROPIC_API_KEY=task6-obvious-fake-anthropic-key",
			"OPENAI_API_KEY=task6-obvious-fake-openai-key",
			"CLAUDE_CODE_ACP_NATIVE_MODELS=native=obsolete",
			"CODEX_ACP_NATIVE_MODELS=native=obsolete",
		},
		NativeEnvAllowlist:  acpNativeAuthEnvAllowlist,
		GatewayEnvAllowlist: acpGatewayEnvAllowlist,
		gatewayPreflightBinding: &launch.ProxyBinding{
			BaseURL: "http://127.0.0.1:1",
			Token:   "test-token",
		},
		executablePreflight: func(_ context.Context, probe ACPExecutableProbe) ACPPreflightResult {
			probes = append(probes, probe)
			if probe.Credential != loop.CredentialGatewayBacked || probe.SharedProxy == nil || probe.SharedProxy.BaseURL != "http://127.0.0.1:1" || probe.SharedProxy.Token != "test-token" {
				return ACPPreflightResult{}
			}
			if len(probe.Env) != 2 || probe.Env[0] != "PATH=/usr/bin" || probe.Env[1] != "LANG=C" {
				t.Fatalf("gateway child env = %#v, want only safe process values", probe.Env)
			}
			if probe.Harness == "claude-code" {
				if !containsString(probe.Models, "sonnet-5@high") || !containsString(probe.Models, "sonnet-5@max") {
					t.Fatalf("Claude preflight models = %#v, want concrete effort aliases", probe.Models)
				}
				return ACPPreflightResult{Ready: true, AdvertisedModels: []string{"sonnet-5", "sonnet-5@high", "fable-5@high"}}
			}
			return ACPPreflightResult{Ready: true}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(probes) == 0 {
		t.Fatal("gateway preflight did not run")
	}
	claudeEntries := composition.Catalog.RuntimeCatalog.EntriesFor("worker")
	var claude loop.RuntimeCatalogEntry
	for _, entry := range claudeEntries {
		if entry.AgentHarness == "claude-code" {
			claude = entry
			break
		}
	}
	if claude.AgentHarness != "claude-code" {
		t.Fatalf("Claude gateway entry missing: %#v", claudeEntries)
	}
	if len(claude.Models) != 1 || claude.Models[0].Alias != "sonnet-5" {
		t.Fatalf("Claude advertised unsupported aliases: %#v", claude.Models)
	}
	if len(claude.Models[0].Efforts) != 2 || claude.Models[0].Efforts[0] != model.EffortMedium || claude.Models[0].Efforts[1] != model.EffortHigh {
		t.Fatalf("Claude advertised unsupported efforts: %#v", claude.Models[0].Efforts)
	}
	if _, _, err := composition.Registry.Builder("acp/claude-code"); err != nil {
		t.Fatalf("gateway-only Claude profile was removed: %v", err)
	}
}

func TestNewACPCompositionRejectsUnavailableConfiguredDefaultHarness(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	compiled := testACPGatewayCatalog(t)
	composition, err := NewACPComposition(ACPChildrenConfig{
		Catalog:       compiled,
		Executables:   map[loop.AgentHarnessName]string{"claude-code": executable, "codex": executable},
		WorkspaceRoot: "/workspace/project",
		gatewayPreflightBinding: &launch.ProxyBinding{
			BaseURL: "http://127.0.0.1:1",
			Token:   "test-token",
		},
		executablePreflight: func(_ context.Context, probe ACPExecutableProbe) ACPPreflightResult {
			if probe.Harness == "claude-code" {
				return ACPPreflightResult{}
			}
			return ACPPreflightResult{Ready: true}
		},
	})
	if err != nil {
		t.Fatalf("NewACPComposition() failed when configured default harness was unavailable: %v", err)
	}
	if composition == nil {
		t.Fatal("NewACPComposition() returned nil composition")
	}
	if entries := composition.Catalog.RuntimeCatalog.EntriesFor("worker"); len(entries) != 0 {
		t.Fatalf("NewACPComposition() substituted another harness: %#v", entries)
	}
}

func TestNewACPCompositionPreflightHonorsCanceledContext(t *testing.T) {
	t.Parallel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	compiled := testACPGatewayCatalog(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	composition, err := NewACPComposition(ACPChildrenConfig{
		Catalog:          compiled,
		Executables:      map[loop.AgentHarnessName]string{"claude-code": executable, "codex": executable},
		WorkspaceRoot:    "/workspace/project",
		preflightContext: ctx,
		gatewayPreflightBinding: &launch.ProxyBinding{
			BaseURL: "http://127.0.0.1:1",
			Token:   "test-token",
		},
		executablePreflight: func(ctx context.Context, _ ACPExecutableProbe) ACPPreflightResult {
			calls++
			if ctx.Err() == nil {
				t.Error("preflight callback received an uncanceled context")
			}
			return ACPPreflightResult{}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if composition == nil {
		t.Fatal("NewACPComposition() returned nil composition")
	}
	if calls > 1 {
		t.Fatalf("canceled preflight continued across %d target probes", calls)
	}
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func TestNewACPCompositionNativePreflightKeepsNativeEnvironment(t *testing.T) {
	t.Parallel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := CompileACPCatalog(ACPCatalogInput{
		SubagentTypes: []identity.AgentName{"worker"},
		Defaults: map[identity.AgentName]configuredDelegateDefault{
			"worker": {Harness: "codex", Model: "native-model", Effort: model.EffortNone},
		},
		NativeAuth: []ACPNativeAuthSource{{
			Harness: "codex", Alias: "native-model", Model: testModel(),
			DefaultEffort: model.EffortNone, Efforts: []model.Effort{model.EffortNone},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got ACPExecutableProbe
	composition, err := NewACPComposition(ACPChildrenConfig{
		Catalog:       compiled,
		Executables:   map[loop.AgentHarnessName]string{"codex": executable},
		WorkspaceRoot: "/workspace/project",
		Env: []string{
			"HOME=/private/login",
			"XDG_CONFIG_HOME=/private/login/.config",
			"PATH=/usr/bin",
			"LANG=C",
			"SECRET=must-not-pass",
		},
		NativeEnvAllowlist: acpNativeAuthEnvAllowlist,
		executablePreflight: func(_ context.Context, probe ACPExecutableProbe) ACPPreflightResult {
			got = probe
			if probe.Credential != loop.CredentialNativeAuth || probe.SharedProxy != nil {
				return ACPPreflightResult{}
			}
			if !containsEnv(probe.Env, "HOME=/private/login") || !containsEnv(probe.Env, "XDG_CONFIG_HOME=/private/login/.config") {
				return ACPPreflightResult{}
			}
			if containsEnvKey(probe.Env, "SECRET") {
				return ACPPreflightResult{}
			}
			return ACPPreflightResult{Ready: true}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Harness != "codex" || got.Model == "" {
		t.Fatalf("native preflight probe = %#v", got)
	}
	if _, _, err := composition.Registry.Builder("acp/codex"); err != nil {
		t.Fatalf("native profile was removed: %v", err)
	}
}

func TestACPChildModelAliasesUseConcreteGatewayTargetsAndNativeModels(t *testing.T) {
	t.Parallel()
	compiled := testACPGatewayCatalog(t)
	claude, err := compiled.RuntimeCatalog.Resolve("worker", "claude-code", "sonnet-5", model.EffortHigh)
	if err != nil {
		t.Fatal(err)
	}
	mainAlias, smallAlias, err := acpChildModelAliases(compiled, "worker", "claude-code", claude)
	if err != nil {
		t.Fatal(err)
	}
	if mainAlias != "sonnet-5@high" || smallAlias != "sonnet-5" {
		t.Fatalf("Claude child aliases = %q/%q, want sonnet-5@high/sonnet-5", mainAlias, smallAlias)
	}

	codex, err := compiled.RuntimeCatalog.Resolve("worker", "codex", "gpt-5.6-luna", model.EffortMax)
	if err != nil {
		t.Fatal(err)
	}
	mainAlias, smallAlias, err = acpChildModelAliases(compiled, "worker", "codex", codex)
	if err != nil {
		t.Fatal(err)
	}
	if mainAlias != "gpt-5.6-luna@max" || smallAlias != "" {
		t.Fatalf("Codex child aliases = %q/%q, want gpt-5.6-luna@max/empty", mainAlias, smallAlias)
	}

	nativeCatalog, err := CompileACPCatalog(ACPCatalogInput{
		SubagentTypes: []identity.AgentName{"worker"},
		Defaults: map[identity.AgentName]configuredDelegateDefault{
			"worker": {Harness: "codex", Model: "native-model", Effort: model.EffortNone},
		},
		NativeAuth: []ACPNativeAuthSource{{
			Harness: "codex", Alias: "native-model", Model: testModel(),
			DefaultEffort: model.EffortNone, Efforts: []model.Effort{model.EffortNone},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	native, err := nativeCatalog.RuntimeCatalog.Resolve("worker", "codex", "native-model", model.EffortNone)
	if err != nil {
		t.Fatal(err)
	}
	mainAlias, smallAlias, err = acpChildModelAliases(nativeCatalog, "worker", "codex", native)
	if err != nil {
		t.Fatal(err)
	}
	if mainAlias != string(native.ModelAlias) || smallAlias != "" {
		t.Fatalf("native child aliases = %q/%q, want %q/empty", mainAlias, smallAlias, native.ModelAlias)
	}
}

func TestACPBoundRuntimeResolutionUsesPinnedSelectors(t *testing.T) {
	t.Parallel()
	compiled, err := CompileACPCatalog(ACPCatalogInput{
		SubagentTypes: []identity.AgentName{"builder"},
		GatewayTargets: legacyTestGatewayTargets(map[model.ProviderName]inference.Client{
			"anthropic": &fakeLLM{}, "openai": &fakeLLM{},
		}),
		Defaults:    legacyTestDefaults([]identity.AgentName{"builder"}),
		ClaudeSmall: "sonnet-5",
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
	bound, err = loop.OverrideBoundRuntimeSelection(bound, resolved.Profile, resolved.TargetAlias, resolved.Target, resolved.Effort)
	if err != nil {
		t.Fatal(err)
	}
	got, harness, err := resolveACPBoundRuntime(compiled, bound)
	if err != nil {
		t.Fatal(err)
	}
	if harness != "codex" || got.ModelAlias != "gpt-5.6-luna" || got.TargetAlias != "gpt-5.6-luna@max" || got.Effort != model.EffortMax {
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
