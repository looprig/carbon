package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/looprig/acp/launch"
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
	composition, err := newProductionACPCompositionWithPreflight(context.Background(), configured, func(_ context.Context, probe ACPExecutableProbe) ACPPreflightResult {
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
	entries := composition.Catalog.RuntimeCatalog.EntriesFor("builder")
	if len(entries) != 3 {
		t.Fatalf("builder entries = %#v, want ordinary plus configured Claude and Codex entries", entries)
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
	configuredPath := filepath.Join(dir, "configured-claude-code-acp")
	writeFakeACPExecutable(t, configuredPath)

	pathDir := t.TempDir()
	pathExecutable := filepath.Join(pathDir, "claude-code-acp")
	writeFakeACPExecutable(t, pathExecutable)
	t.Setenv("PATH", pathDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	tests := []struct {
		name          string
		env           string
		configured    string
		wellKnownName string
		want          string
	}{
		{name: "env var wins", env: "/env/claude-code-acp", configured: configuredPath, wellKnownName: "claude-code-acp", want: "/env/claude-code-acp"},
		{name: "config wins over PATH", env: "", configured: configuredPath, wellKnownName: "claude-code-acp", want: configuredPath},
		{name: "falls back to PATH", env: "", configured: "", wellKnownName: "claude-code-acp", want: pathExecutable},
		{name: "nothing resolves", env: "", configured: "", wellKnownName: "no-such-acp-adapter-binary", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveACPExecutable(tt.env, tt.configured, tt.wellKnownName)
			if got != tt.want {
				t.Fatalf("resolveACPExecutable() = %q, want %q", got, tt.want)
			}
		})
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
