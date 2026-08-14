package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/looprig/acp/launch"
	"github.com/looprig/carbon/internal/catalog/carbon"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/inference/model"
)

func TestPreflightProductionACPExecutableEnforcesAdapterSpecificSelectors(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		harness    loop.AgentHarnessName
		credential loop.CredentialMode
		model      string
		smallModel string
		proxy      bool
		helperPath string
		wantReady  bool
	}{
		{
			name:    "Claude gateway requires both selectors",
			harness: "claude-code", credential: loop.CredentialGatewayBacked,
			model: "sonnet-5@high", smallModel: "sonnet-5", proxy: true,
			helperPath: task33ACPHelperPath, wantReady: true,
		},
		{
			name:    "Claude gateway rejects a missing small selector",
			harness: "claude-code", credential: loop.CredentialGatewayBacked,
			model: "sonnet-5@high", proxy: true,
			helperPath: task33ACPHelperPath,
		},
		{
			name:    "Codex gateway permits only the main selector",
			harness: "codex", credential: loop.CredentialGatewayBacked,
			model: "gpt-5.6-luna", proxy: true,
			helperPath: task33ACPHelperPath, wantReady: true,
		},
		{
			name:    "Codex gateway rejects a small selector",
			harness: "codex", credential: loop.CredentialGatewayBacked,
			model: "gpt-5.6-luna", smallModel: "small", proxy: true,
			helperPath: task33ACPHelperPath,
		},
		{
			name:    "Claude native managed",
			harness: "claude-code", credential: loop.CredentialNativeAuth,
			helperPath: task33NativeClaudeACPHelperPath, wantReady: true,
		},
		{
			name:    "Claude native explicit",
			harness: "claude-code", credential: loop.CredentialNativeAuth,
			model: "sonnet-5@high", smallModel: "sonnet-5",
			helperPath: task33NativeClaudeACPHelperPath, wantReady: true,
		},
		{
			name:    "Claude native rejects a partial selector",
			harness: "claude-code", credential: loop.CredentialNativeAuth,
			model:      "sonnet-5@high",
			helperPath: task33NativeClaudeACPHelperPath,
		},
		{
			name:    "Codex native managed",
			harness: "codex", credential: loop.CredentialNativeAuth,
			helperPath: task33NativeCodexACPHelperPath, wantReady: true,
		},
		{
			name:    "Codex native explicit",
			harness: "codex", credential: loop.CredentialNativeAuth,
			model:      "gpt-5.6-luna",
			helperPath: task33NativeCodexACPHelperPath, wantReady: true,
		},
		{
			name:    "Codex native rejects a small selector",
			harness: "codex", credential: loop.CredentialNativeAuth,
			model: "gpt-5.6-luna", smallModel: "small",
			helperPath: task33NativeCodexACPHelperPath,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			probe := ACPExecutableProbe{
				ACPNativeAuthProbe: ACPNativeAuthProbe{
					Harness:       tt.harness,
					Executable:    executable,
					WorkspaceRoot: t.TempDir(),
					Env:           []string{"PATH=" + tt.helperPath},
				},
				Credential: tt.credential,
				Model:      tt.model,
				SmallModel: tt.smallModel,
			}
			if tt.proxy {
				probe.SharedProxy = &launch.ProxyBinding{BaseURL: "http://127.0.0.1:1", Token: "preflight-token"}
			}
			result := preflightProductionACPExecutable(context.Background(), probe)
			if result.Ready != tt.wantReady {
				t.Fatalf("preflight result = %#v, want Ready=%v", result, tt.wantReady)
			}
			if tt.wantReady && tt.harness == "claude-code" && len(result.AdvertisedModels) == 0 {
				t.Fatalf("Claude preflight advertised no model selectors: %#v", result)
			}
		})
	}
}

func TestProductionACPCompositionRejectsInvalidAccessProfileBeforeCatalog(t *testing.T) {
	t.Parallel()
	_, err := newProductionACPCompositionWithPreflight(context.Background(), AccessProfile("invalid"), productionModels{}, nil)
	if err != errACPAccessProfileUnavailable {
		t.Fatalf("invalid production ACP access profile error = %v, want bounded access-profile error", err)
	}
}

func TestProductionACPCompositionKeepsConfiguredGatewayRowsAlongsideOrdinaryRows(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(acpCodexExecutableEnv, executable)
	t.Setenv(acpClaudeExecutableEnv, executable)
	t.Setenv("ANTHROPIC_API_KEY", "must-not-create-a-row")
	t.Setenv("OPENAI_API_KEY", "must-not-create-a-row")
	t.Setenv("CLAUDE_CODE_ACP_NATIVE_MODELS", "native=fixed-sonnet")
	t.Setenv("CODEX_ACP_NATIVE_MODELS", "native=fixed-gpt")

	configured := configuredProductionModelsForTest("configured-only")
	composition, err := newProductionACPCompositionWithPreflight(context.Background(), DefaultAccessProfile, configured, func(_ context.Context, probe ACPExecutableProbe) ACPPreflightResult {
		if probe.Credential != loop.CredentialGatewayBacked {
			t.Fatalf("production preflight credential = %q, want gateway-backed", probe.Credential)
		}
		if probe.Model != "configured-only" {
			t.Fatalf("production preflight model = %q, want configured model", probe.Model)
		}
		if probe.Harness == "claude-code" && probe.SmallModel != "configured-only" {
			t.Fatalf("Claude small model = %q, want exact configured alias", probe.SmallModel)
		}
		for _, entry := range probe.Env {
			name, _, _ := strings.Cut(entry, "=")
			switch name {
			case "ANTHROPIC_API_KEY", "OPENAI_API_KEY", "CLAUDE_CODE_ACP_NATIVE_MODELS", "CODEX_ACP_NATIVE_MODELS", "HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME":
				t.Fatalf("production gateway child env contains forbidden %s", name)
			}
		}
		return ACPPreflightResult{Ready: true, AdvertisedModels: append([]string(nil), probe.Models...)}
	})
	if err != nil {
		t.Fatal(err)
	}
	entries := composition.Catalog.RuntimeCatalog.EntriesFor(carbon.Name)
	if len(entries) != 3 {
		t.Fatalf("Carbon entries = %#v, want ordinary plus configured Claude and Codex entries", entries)
	}
	var gatewayRows, nativeRows int
	for _, entry := range entries {
		if entry.Source == loop.RuntimeSourceNative {
			nativeRows++
			if entry.AgentHarness != "looprig" || entry.Profile != "looprig/native" || entry.DefaultModel != "configured-only" {
				t.Fatalf("ordinary production entry = %#v, want configured model row", entry)
			}
		} else {
			gatewayRows++
			if entry.Credential != loop.CredentialGatewayBacked || entry.DefaultModel != "configured-only" {
				t.Fatalf("production gateway entry = %#v, want configured model row", entry)
			}
		}
		if len(entry.Models) != 1 || entry.Models[0].Alias != "configured-only" {
			t.Fatalf("production models = %#v, want only configured-only", entry.Models)
		}
	}
	if gatewayRows != 2 || nativeRows != 1 {
		t.Fatalf("gateway rows=%d ordinary rows=%d, want 2 and 1", gatewayRows, nativeRows)
	}
}

func TestResolveACPExecutable(t *testing.T) {
	dir := t.TempDir()
	configuredPath := filepath.Join(dir, "configured-claude-agent-acp")
	writeFakeACPExecutable(t, configuredPath)

	pathDir := t.TempDir()
	currentExecutable := filepath.Join(pathDir, acpClaudeAdapterName)
	writeFakeACPExecutable(t, currentExecutable)
	deprecatedExecutable := filepath.Join(pathDir, acpDeprecatedClaudeAdapterName)
	writeFakeACPExecutable(t, deprecatedExecutable)
	t.Setenv("PATH", pathDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	claudeNames := []string{acpClaudeAdapterName, acpDeprecatedClaudeAdapterName}
	tests := []struct {
		name           string
		env            string
		configured     string
		wellKnownNames []string
		want           string
		wantMatched    string
	}{
		{name: "env var wins", env: "/env/claude-agent-acp", configured: configuredPath, wellKnownNames: claudeNames, want: "/env/claude-agent-acp"},
		{name: "config wins over PATH", env: "", configured: configuredPath, wellKnownNames: claudeNames, want: configuredPath},
		{
			name: "prefers the current adapter over the deprecated one", wellKnownNames: claudeNames,
			want: currentExecutable, wantMatched: acpClaudeAdapterName,
		},
		{
			// The migration fallback: only the renamed package is
			// installed, so Claude ACP keeps working -- but the caller
			// learns which name matched so it can say so out loud.
			name: "falls back to the deprecated adapter and reports it",
			wellKnownNames: []string{
				"no-such-acp-adapter-binary", acpDeprecatedClaudeAdapterName,
			},
			want: deprecatedExecutable, wantMatched: acpDeprecatedClaudeAdapterName,
		},
		{name: "nothing resolves", wellKnownNames: []string{"no-such-acp-adapter-binary"}, want: ""},
		{name: "no well-known names", wellKnownNames: nil, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, matched := resolveACPExecutable(tt.env, tt.configured, tt.wellKnownNames...)
			if got != tt.want {
				t.Fatalf("resolveACPExecutable() = %q, want %q", got, tt.want)
			}
			if matched != tt.wantMatched {
				t.Fatalf("resolveACPExecutable() matched = %q, want %q", matched, tt.wantMatched)
			}
		})
	}
}

// TestProductionACPDiscoveryPrefersCurrentClaudeAdapter proves the production
// composition path -- not just the resolution helper -- discovers
// @agentclientprotocol/claude-agent-acp's binary, and that falling back to
// the renamed, deprecated @zed-industries/claude-code-acp binary reaches the
// operator through the existing bounded ACP diagnostics rather than silently.
func TestProductionACPDiscoveryPrefersCurrentClaudeAdapter(t *testing.T) {
	tests := []struct {
		name           string
		installed      []string
		wantBase       string
		wantDeprecated bool
	}{
		{name: "current only", installed: []string{acpClaudeAdapterName}, wantBase: acpClaudeAdapterName},
		{
			name:      "both installed",
			installed: []string{acpClaudeAdapterName, acpDeprecatedClaudeAdapterName},
			wantBase:  acpClaudeAdapterName,
		},
		{
			name:           "deprecated only",
			installed:      []string{acpDeprecatedClaudeAdapterName},
			wantBase:       acpDeprecatedClaudeAdapterName,
			wantDeprecated: true,
		},
		{name: "neither installed", installed: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pathDir := t.TempDir()
			for _, name := range tt.installed {
				writeFakeACPExecutable(t, filepath.Join(pathDir, name))
			}
			t.Setenv("PATH", pathDir)
			t.Setenv(acpClaudeExecutableEnv, "")
			t.Setenv(acpCodexExecutableEnv, "")

			composition, err := newProductionACPCompositionWithPreflight(
				context.Background(), DefaultAccessProfile,
				configuredProductionModelsForTest("configured-only"), nil,
			)
			if err != nil {
				t.Fatalf("newProductionACPCompositionWithPreflight() error = %v", err)
			}
			gotDeprecated := false
			for _, diagnostic := range composition.Diagnostics {
				if diagnostic == acpDiagnosticDeprecatedClaudeAdapter() {
					gotDeprecated = true
				}
			}
			if gotDeprecated != tt.wantDeprecated {
				t.Fatalf("deprecated-adapter diagnostic = %t, want %t (diagnostics %#v)", gotDeprecated, tt.wantDeprecated, composition.Diagnostics)
			}
			if got := composition.Catalog.HasProfile("acp/claude-code"); got != (tt.wantBase != "") {
				t.Fatalf("acp/claude-code profile present = %t, want %t", got, tt.wantBase != "")
			}
			if tt.wantBase == "" {
				return
			}
			resolved, _ := resolveACPExecutable("", "", acpClaudeAdapterName, acpDeprecatedClaudeAdapterName)
			if filepath.Base(resolved) != tt.wantBase {
				t.Fatalf("discovered executable = %q, want a %q binary", resolved, tt.wantBase)
			}
		})
	}
}

// TestACPDeprecatedClaudeAdapterDiagnosticIsBoundedAndSecretFree holds the
// deprecation notice to the same shape as every other ACP diagnostic: one
// short line, no filesystem path, no stderr or provider content.
func TestACPDeprecatedClaudeAdapterDiagnosticIsBoundedAndSecretFree(t *testing.T) {
	t.Parallel()
	diagnostic := acpDiagnosticDeprecatedClaudeAdapter()
	if !strings.HasPrefix(diagnostic, "acp: claude-code ") {
		t.Fatalf("diagnostic = %q, want the shared \"acp: <harness> \" prefix", diagnostic)
	}
	if strings.ContainsAny(diagnostic, "\n\r\x00") || len(diagnostic) > 256 {
		t.Fatalf("diagnostic = %q, want one bounded single-line string", diagnostic)
	}
	if !strings.Contains(diagnostic, acpDeprecatedClaudeAdapterName) || !strings.Contains(diagnostic, "@agentclientprotocol/claude-agent-acp") {
		t.Fatalf("diagnostic = %q, want both the deprecated name and the remediation package", diagnostic)
	}
}

func writeFakeACPExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func configuredProductionModelsForTest(alias loop.ModelAlias) productionModels {
	selected := model.CustomModel("lmstudio", model.APIFormatOpenAI, "http://localhost:1234/v1", "configured-provider-model", model.WithTools())
	return productionModels{
		PrimerClient: &fakeLLM{},
		PrimerModel:  selected,
		ACP: []ACPGatewaySource{{
			Alias: alias, Client: &fakeLLM{}, Model: selected,
			DefaultEffort: model.EffortNone, Efforts: []model.Effort{model.EffortNone},
		}},
		ClaudeSmall: alias,
		ConfigRev:   "configured-revision",
	}
}
