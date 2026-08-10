package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/looprig/carbon/internal/catalog/carbon"
	"github.com/looprig/core/content"
	"github.com/looprig/foreignloops/driver"
	"github.com/looprig/harness/pkg/foreign"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/inference"
	model "github.com/looprig/inference/model"
	"github.com/looprig/mcp/pkg/collab"
)

func TestResolveCollabMCPExecutableUsesExplicitAbsoluteExecutable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "carbon-collab-mcp")
	writeExecutable(t, path)

	got, err := resolveCollabMCPExecutableFrom(path, "")
	if err != nil {
		t.Fatalf("resolveCollabMCPExecutableFrom() error = %v", err)
	}
	if got != path {
		t.Fatalf("resolveCollabMCPExecutableFrom() = %q, want %q", got, path)
	}
}

func TestResolveCollabMCPExecutableFallsBackToVerifiedSibling(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "carbon")
	sibling := filepath.Join(dir, "carbon-collab-mcp")
	writeExecutable(t, sibling)

	got, err := resolveCollabMCPExecutableFrom("", current)
	if err != nil {
		t.Fatalf("resolveCollabMCPExecutableFrom() error = %v", err)
	}
	if got != sibling {
		t.Fatalf("resolveCollabMCPExecutableFrom() = %q, want %q", got, sibling)
	}
}

func TestResolveCollabMCPExecutableRejectsSymlinkedParent(t *testing.T) {
	base := t.TempDir()
	first := filepath.Join(base, "first")
	second := filepath.Join(base, "second")
	for _, dir := range []string{first, second} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeExecutable(t, filepath.Join(dir, collabMCPExecutableName))
	}
	parent := filepath.Join(base, "current")
	if err := os.Symlink(first, parent); err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(parent, collabMCPExecutableName)
	if _, err := resolveCollabMCPExecutableFrom(candidate, ""); err == nil {
		t.Fatal("resolveCollabMCPExecutableFrom() followed a symlinked parent")
	}
	if err := os.Remove(parent); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(second, parent); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveCollabMCPExecutableFrom(candidate, ""); err == nil {
		t.Fatal("resolveCollabMCPExecutableFrom() accepted a retargeted symlinked parent")
	}
}

func TestResolveCollabMCPExecutableRejectsSymlinkedDefaultSibling(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	writeExecutable(t, target)
	sibling := filepath.Join(dir, collabMCPExecutableName)
	if err := os.Symlink(target, sibling); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveCollabMCPExecutableFrom("", filepath.Join(dir, "carbon")); err == nil {
		t.Fatal("resolveCollabMCPExecutableFrom() accepted a symlinked default sibling")
	}
}

func TestResolveCollabMCPExecutableRejectsUnsafeCandidates(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	writeExecutable(t, target)
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	plain := filepath.Join(dir, "plain")
	if err := os.WriteFile(plain, []byte("plain"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		path    string
		current string
	}{
		{name: "relative explicit", path: "relative/carbon-collab-mcp"},
		{name: "missing explicit", path: filepath.Join(dir, "missing")},
		{name: "non executable explicit", path: plain},
		{name: "symlink explicit", path: link},
		{name: "missing sibling", current: filepath.Join(dir, "carbon")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := resolveCollabMCPExecutableFrom(tt.path, tt.current); err == nil {
				t.Fatal("resolveCollabMCPExecutableFrom() error = nil, want fail-closed error")
			}
		})
	}
}

func TestCollabMCPServerForUsesDescriptorOnlyInEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), collabMCPExecutableName)
	writeExecutable(t, path)
	capability := make([]byte, collab.CapabilityBytes)
	for i := range capability {
		capability[i] = byte(i + 1)
	}
	descriptor := foreign.NewBrokerDescriptor("/private/broker.sock", capability)

	server, err := collabMCPServerFor(path, descriptor)
	if err != nil {
		t.Fatalf("collabMCPServerFor() error = %v", err)
	}
	if server.Stdio == nil {
		t.Fatal("collabMCPServerFor() returned no stdio server")
	}
	if server.Stdio.Command != path {
		t.Fatalf("MCP command = %q, want %q", server.Stdio.Command, path)
	}
	if len(server.Stdio.Args) != 0 {
		t.Fatalf("MCP args = %#v, want no token/endpoint args", server.Stdio.Args)
	}
	if len(server.Stdio.Env) != 2 {
		t.Fatalf("MCP env = %#v, want endpoint and token", server.Stdio.Env)
	}
	if server.Stdio.Env[0].Name != collab.EndpointEnv || server.Stdio.Env[0].Value != "/private/broker.sock" {
		t.Fatalf("endpoint env = %#v", server.Stdio.Env[0])
	}
	if server.Stdio.Env[1].Name != collab.TokenEnv || server.Stdio.Env[1].Value != "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20" {
		t.Fatalf("token env = %#v", server.Stdio.Env[1])
	}
}

func TestCollabMCPServerForRejectsMissingDescriptor(t *testing.T) {
	path := filepath.Join(t.TempDir(), collabMCPExecutableName)
	writeExecutable(t, path)
	if _, err := collabMCPServerFor(path, foreign.BrokerDescriptor{}); err == nil {
		t.Fatal("collabMCPServerFor() error = nil, want missing broker descriptor failure")
	}
}

func TestCollabMCPSecretsStayOutOfPersistenceAndDiagnostics(t *testing.T) {
	path := filepath.Join(t.TempDir(), collabMCPExecutableName)
	writeExecutable(t, path)
	endpoint := "/private/carbon-collab-" + strings.Repeat("e", 24) + ".sock"
	capability := []byte(strings.Repeat("c", collab.CapabilityBytes))
	token, err := collab.EncodeCapabilityToken(capability)
	if err != nil {
		t.Fatalf("EncodeCapabilityToken() error = %v", err)
	}

	// The descriptor is deliberately accepted by the transient ACP config, but
	// neither its endpoint nor its derived bearer token is a durable app field.
	fields := agentFingerprintFields(Config{CollabMCPExecutable: path})
	if fields.ExternalCapabilityRev != "" {
		t.Fatalf("collaboration MCP unexpectedly changed ExternalCapabilityRev: %q", fields.ExternalCapabilityRev)
	}
	durable, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal fingerprint fields: %v", err)
	}
	for _, secret := range []string{endpoint, token} {
		if strings.Contains(string(durable), secret) {
			t.Fatalf("fingerprint fields persisted collaboration secret %q: %s", secret, durable)
		}
	}

	// A malformed descriptor must produce a bounded diagnostic rather than
	// echoing the endpoint or token into shutdown/error presentation paths.
	_, err = collabMCPServerFor(path, foreign.NewBrokerDescriptor(endpoint, capability[:len(capability)-1]))
	if err == nil {
		t.Fatal("collabMCPServerFor() error = nil, want malformed descriptor failure")
	}
	for _, secret := range []string{endpoint, token} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("collaboration diagnostic leaked secret %q: %v", secret, err)
		}
	}
}

func TestACPMessageAgentCompositionLeavesNativeCarbonInProcess(t *testing.T) {
	client := &managedScript{}
	seenMessageAgent := false
	client.fn = func(_ context.Context, req inference.Request) ([]content.Chunk, error) {
		if !requestHasRole(req, carbon.Name) {
			t.Fatalf("native request did not carry Carbon identity")
		}
		seenMessageAgent = slices.Contains(toolNamesFromRequest(req), "MessageAgent")
		return finalText("native Carbon complete"), nil
	}

	// Supplying the collaboration executable on Config must not turn the native
	// Carbon definition into an ACP child. Its existing managed tool bundle is
	// still built in-process by Harness, including MessageAgent.
	agent := newTestAgent(t, client, Config{
		CollabMCPExecutable: filepath.Join(t.TempDir(), collabMCPExecutableName),
	})
	if got := runManagedTurn(t, agent, "inspect native collaboration tools"); got != "native Carbon complete" {
		t.Fatalf("native Carbon result = %q", got)
	}
	if !seenMessageAgent {
		t.Fatal("native Carbon request omitted in-process MessageAgent")
	}
}

func TestNewACPCompositionRegistersServicesAwareBuildersForCollabMCP(t *testing.T) {
	executable := filepath.Join(t.TempDir(), collabMCPExecutableName)
	writeExecutable(t, executable)
	composition, err := NewACPComposition(ACPChildrenConfig{
		Catalog:             testACPGatewayCatalog(t),
		Executables:         map[loop.AgentHarnessName]string{"claude-code": executable, "codex": executable},
		WorkspaceRoot:       t.TempDir(),
		CollabMCPExecutable: executable,
		GatewayEnvAllowlist: []string{"PATH"},
		NativeEnvAllowlist:  []string{"PATH"},
		Env:                 []string{"PATH=/bin"},
	})
	if err != nil {
		t.Fatalf("NewACPComposition() error = %v", err)
	}
	if composition.LiveServices == nil || composition.RestoredServices == nil {
		t.Fatalf("services builders = (%v, %v), want both configured", composition.LiveServices, composition.RestoredServices)
	}
	if !composition.Registry.HasServicesBuilder("acp/claude-code") || !composition.Registry.HasServicesBuilder("acp/codex") {
		t.Fatal("ACP registry did not retain services-aware registrations")
	}
}

func TestProductionACPCompositionResolvesCollabMCPOnceBeforeSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), collabMCPExecutableName)
	writeExecutable(t, path)
	cfg, err := withProductionACPChildren(context.Background(), Config{CollabMCPExecutable: path}, configuredProductionModelsForTest("configured-only"))
	if err != nil {
		t.Fatalf("withProductionACPChildren() error = %v", err)
	}
	if cfg.ACPChildren == nil || cfg.ACPChildren.LiveServices == nil || cfg.ACPChildren.RestoredServices == nil {
		t.Fatalf("ACP composition services = %#v, want configured services", cfg.ACPChildren)
	}
	if cfg.ACPChildren.collabMCPExecutable != path {
		t.Fatalf("resolved collaboration executable = %q, want %q", cfg.ACPChildren.collabMCPExecutable, path)
	}
}

func TestProductionACPCompositionRejectsUnavailableExplicitCollabMCP(t *testing.T) {
	_, err := withProductionACPChildren(context.Background(), Config{CollabMCPExecutable: "relative/carbon-collab-mcp"}, configuredProductionModelsForTest("configured-only"))
	if err == nil {
		t.Fatal("withProductionACPChildren() error = nil, want invalid explicit path failure")
	}
}

func TestProductionACPCompositionFailsClosedWhenCollabMCPIsMissing(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	canonicalExecutable, err := filepath.EvalSymlinks(executable)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(acpClaudeExecutableEnv, canonicalExecutable)
	t.Setenv(acpCodexExecutableEnv, canonicalExecutable)
	_, err = withProductionACPChildren(context.Background(), Config{}, configuredProductionModelsForTest("configured-only"))
	if err == nil {
		t.Fatal("withProductionACPChildren() succeeded without the collaboration MCP sibling")
	}
}

func TestProductionACPCompositionKeepsNoACPSetupWithoutCollabMCP(t *testing.T) {
	configured := configuredProductionModelsForTest("configured-only")
	configured.ACP = nil
	configured.ClaudeSmall = ""
	configured.PrimerAlias = "configured-only"
	configured.PrimerEfforts = []model.Effort{model.EffortNone}
	cfg, err := withProductionACPChildren(context.Background(), Config{}, configured)
	if err != nil {
		t.Fatalf("withProductionACPChildren() error = %v", err)
	}
	if cfg.ACPChildren == nil {
		t.Fatal("withProductionACPChildren() returned no composition")
	}
	if cfg.ACPChildren.LiveServices != nil || cfg.ACPChildren.RestoredServices != nil {
		t.Fatalf("no-ACP composition unexpectedly installed services builders: %#v", cfg.ACPChildren)
	}
}

func TestACPChildConfigInjectsCollabMCPDescriptorForNewAndRestore(t *testing.T) {
	path := filepath.Join(t.TempDir(), collabMCPExecutableName)
	writeExecutable(t, path)
	compiled, err := CompileAgentRuntimeCatalog(AgentRuntimeCatalogInput{
		NativeACP: map[string]ACPNativeProfile{"codex": {
			Harness: "codex", Enabled: true,
			ModelOptions: []ACPNativeModelOption{{Alias: "native", Model: "native", Efforts: []model.Effort{model.EffortNone}, DefaultEffort: model.EffortNone}},
		}},
		PrimerTarget: runtimeCatalogPrimer(),
	})
	if err != nil {
		t.Fatalf("CompileAgentRuntimeCatalog() error = %v", err)
	}
	resolved, err := compiled.RuntimeCatalog.ResolveWithExplicitSource(carbon.Name, "codex", loop.RuntimeSourceNative, "native", model.EffortNone, true)
	if err != nil {
		t.Fatalf("ResolveWithExplicitSource() error = %v", err)
	}
	bound := testACPChildBound(t, resolved)
	factory := &acpChildFactory{config: ACPChildrenConfig{
		Catalog: compiled, AccessProfile: AccessReadOnly, posture: driver.PostureReadOnly,
		CollabMCPExecutable: path,
	}}
	capability := make([]byte, collab.CapabilityBytes)
	services := foreign.NewServices(foreign.NewBrokerDescriptor("/tmp/broker.sock", capability), nil)
	_, cfg, gateway, err := factory.configForServices(context.Background(), bound, "", services)
	if err != nil {
		t.Fatalf("configForServices(new) error = %v", err)
	}
	if gateway != nil {
		t.Fatal("native config unexpectedly owns gateway")
	}
	if len(cfg.McpServers) != 1 || cfg.McpServers[0].Stdio == nil {
		t.Fatalf("new MCP servers = %#v, want one stdio server", cfg.McpServers)
	}
	_, restoredCfg, _, err := factory.configForServices(context.Background(), bound, "restored-agent", services)
	if err != nil {
		t.Fatalf("configForServices(restore) error = %v", err)
	}
	if len(restoredCfg.McpServers) != 1 || restoredCfg.McpServers[0].Stdio == nil {
		t.Fatalf("restore MCP servers = %#v, want one stdio server", restoredCfg.McpServers)
	}
}

func TestACPChildConfigRejectsCollabMCPReplacementDrift(t *testing.T) {
	path := filepath.Join(t.TempDir(), collabMCPExecutableName)
	writeExecutable(t, path)
	snapshot, ok := verifiedExecutableSnapshotFor(path)
	if !ok {
		t.Fatal("verifiedExecutableSnapshotFor() rejected the test executable")
	}
	compiled, err := CompileAgentRuntimeCatalog(AgentRuntimeCatalogInput{
		NativeACP: map[string]ACPNativeProfile{"codex": {
			Harness: "codex", Enabled: true,
			ModelOptions: []ACPNativeModelOption{{Alias: "native", Model: "native", Efforts: []model.Effort{model.EffortNone}, DefaultEffort: model.EffortNone}},
		}},
		PrimerTarget: runtimeCatalogPrimer(),
	})
	if err != nil {
		t.Fatalf("CompileAgentRuntimeCatalog() error = %v", err)
	}
	bound := testACPChildBound(t, mustResolveNativeACPCollabTask16(t, compiled))
	factory := &acpChildFactory{config: ACPChildrenConfig{
		Catalog: compiled, AccessProfile: AccessReadOnly, posture: driver.PostureReadOnly,
		CollabMCPExecutable: path, collabMCPSnapshot: &snapshot,
	}}
	replacement := path + ".replacement"
	writeExecutable(t, replacement)
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	capability := make([]byte, collab.CapabilityBytes)
	services := foreign.NewServices(foreign.NewBrokerDescriptor("/tmp/broker.sock", capability), nil)
	if _, _, _, err := factory.configForServices(context.Background(), bound, "", services); err == nil {
		t.Fatal("configForServices() accepted a replaced collaboration executable")
	}
}

func mustResolveNativeACPCollabTask16(t *testing.T, compiled ACPCompiledCatalog) loop.Resolved {
	t.Helper()
	resolved, err := compiled.RuntimeCatalog.ResolveWithExplicitSource(carbon.Name, "codex", loop.RuntimeSourceNative, "native", model.EffortNone, true)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func writeExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}
