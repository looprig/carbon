package app

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/looprig/acp/protocol"
	"github.com/looprig/coderig/internal/catalog/generic"
	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/foreign"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
	model "github.com/looprig/inference/model"
	"github.com/looprig/mcp/pkg/collab"
)

// collabMCPIntegrationCapture is the secret-free shape of the ACP MCP
// descriptor observed by the services-aware builder. The token is retained
// only for byte comparisons; test failures never format it.
type collabMCPIntegrationCapture struct {
	command    string
	name       string
	args       []string
	env        []protocol.EnvVariable
	endpoint   string
	token      []byte
	agentSID   string
	foreignSID string
}

type collabMCPIntegrationRecorder struct {
	mu         sync.Mutex
	executable string
	live       []collabMCPIntegrationCapture
	restored   []collabMCPIntegrationCapture
}

func (r *collabMCPIntegrationRecorder) capture(services foreign.Services, agentSID, foreignSID string) (collabMCPIntegrationCapture, error) {
	server, err := collabMCPServerFor(r.executable, services.Broker)
	if err != nil {
		return collabMCPIntegrationCapture{}, err
	}
	if server.Stdio == nil {
		return collabMCPIntegrationCapture{}, errors.New("coderig test: collaboration descriptor omitted stdio")
	}
	stdio := server.Stdio
	token := services.Broker.Capability()
	return collabMCPIntegrationCapture{
		command:    stdio.Command,
		name:       stdio.Name,
		args:       append([]string(nil), stdio.Args...),
		env:        append([]protocol.EnvVariable(nil), stdio.Env...),
		endpoint:   services.Broker.Endpoint(),
		token:      append([]byte(nil), token...),
		agentSID:   agentSID,
		foreignSID: foreignSID,
	}, nil
}

func (r *collabMCPIntegrationRecorder) liveServices(
	_ context.Context,
	_, _ uuid.UUID,
	_ loop.Provenance,
	_ foreign.EventPublisher,
	_ loop.BoundDefinition,
	_ func() (uuid.UUID, error),
	_ *event.Factory,
	services foreign.Services,
) (loop.Backend, string, error) {
	capture, err := r.capture(services, "acp-live-session", "acp-live-session")
	if err != nil {
		return nil, "", err
	}
	r.mu.Lock()
	r.live = append(r.live, capture)
	r.mu.Unlock()
	return newACPCompositionBackend(), "acp-live-session", nil
}

func (r *collabMCPIntegrationRecorder) restoredServices(
	_ context.Context,
	_, _ uuid.UUID,
	_ loop.Provenance,
	_ foreign.EventPublisher,
	_ loop.BoundDefinition,
	_ func() (uuid.UUID, error),
	_ *event.Factory,
	seed foreign.RestoredForeign,
	services foreign.Services,
) (loop.Backend, error) {
	capture, err := r.capture(services, seed.AgentSessionID, seed.ForeignSID)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.restored = append(r.restored, capture)
	r.mu.Unlock()
	return newACPCompositionBackend(), nil
}

func (r *collabMCPIntegrationRecorder) snapshot() (live, restored []collabMCPIntegrationCapture) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]collabMCPIntegrationCapture(nil), r.live...), append([]collabMCPIntegrationCapture(nil), r.restored...)
}

func collabMCPIntegrationComposition(t *testing.T, catalog loop.RuntimeCatalog, recorder *collabMCPIntegrationRecorder) *ACPComposition {
	t.Helper()
	var registry foreign.BuilderRegistry
	if err := registry.RegisterServices("acp/codex", recorder.liveServices, recorder.restoredServices); err != nil {
		t.Fatalf("register collaboration services builders: %v", err)
	}
	return &ACPComposition{
		Catalog: ACPCompiledCatalog{
			RuntimeCatalog: catalog,
			profiles:       map[loop.RuntimeProfileName]struct{}{"acp/codex": {}},
		},
		Registry:         &registry,
		Live:             dispatchACPBuilder(&registry),
		Restored:         dispatchACPRestoredBuilder(&registry),
		LiveServices:     dispatchACPServicesBuilder(&registry),
		RestoredServices: dispatchACPServicesRestoredBuilder(&registry),
	}
}

func TestCollabMCPIntegrationNewAndRestoredForeignConstruction(t *testing.T) {
	requireCollabMCPIntegrationSocket(t)

	executable := filepath.Join(t.TempDir(), collabMCPExecutableName)
	writeExecutable(t, executable)
	recorder := &collabMCPIntegrationRecorder{executable: executable}
	catalog := gatewayRuntimeCatalogForTask31(t, map[model.ProviderName]inference.Client{
		"anthropic": &fakeLLM{},
		"openai":    &fakeLLM{},
	})
	composition := collabMCPIntegrationComposition(t, catalog, recorder)
	assembly, _, probe := testACPDelegationRig(t, Config{
		ACPChildren:    composition,
		RuntimeCatalog: catalog,
	})
	ctx := context.Background()
	live, err := assembly.NewSession(ctx)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	started, err := probe.captured().Execute(ctx, tool.DelegateRequest{
		Operation:       tool.DelegateStart,
		AgentType:       string(generic.Name),
		Message:         "exercise collaboration construction",
		WaitForResponse: false,
		Runtime:         codexMaxRuntime(),
	})
	if err != nil {
		_ = live.Shutdown(ctx)
		t.Fatalf("StartAgent: %v", err)
	}
	if started.AgentID.IsZero() {
		_ = live.Shutdown(ctx)
		t.Fatal("StartAgent returned a zero child id")
	}
	liveCaptures, restoredCaptures := recorder.snapshot()
	if len(liveCaptures) != 1 || len(restoredCaptures) != 0 {
		_ = live.Shutdown(ctx)
		t.Fatalf("foreign construction counts = live:%d restored:%d, want 1:0", len(liveCaptures), len(restoredCaptures))
	}
	assertCollabMCPIntegrationDescriptor(t, liveCaptures[0], executable)
	if liveCaptures[0].agentSID != "acp-live-session" || liveCaptures[0].foreignSID != "acp-live-session" {
		_ = live.Shutdown(ctx)
		t.Fatalf("new ACP session identities were not captured as expected")
	}
	firstToken := append([]byte(nil), liveCaptures[0].token...)

	sessionID := live.SessionID()
	if err := live.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown live session: %v", err)
	}

	// Restore uses the durable ACP session identity but must mint a new broker
	// capability before the restored services-aware builder is called.
	restored, err := assembly.RestoreSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("RestoreSession: %v", err)
	}
	defer func() { _ = restored.Shutdown(ctx) }()
	_, restoredCaptures = recorder.snapshot()
	if len(restoredCaptures) != 1 {
		t.Fatalf("restored foreign construction count = %d, want 1", len(restoredCaptures))
	}
	assertCollabMCPIntegrationDescriptor(t, restoredCaptures[0], executable)
	if restoredCaptures[0].agentSID != "acp-live-session" || restoredCaptures[0].foreignSID != "acp-live-session" {
		t.Fatal("restored ACP builder did not receive the durable agent session identity")
	}
	if bytes.Equal(firstToken, restoredCaptures[0].token) {
		t.Fatal("restored ACP construction replayed the live collaboration capability")
	}
	if restoredCaptures[0].endpoint == liveCaptures[0].endpoint {
		t.Fatal("restored ACP construction reused the live collaboration broker endpoint")
	}
}

func TestCollabMCPIntegrationNativeGenericRetainsInProcessMessageAgent(t *testing.T) {
	requireCollabMCPIntegrationSocket(t)
	client := &managedScript{}
	var sawMessageAgent bool
	client.fn = func(_ context.Context, req inference.Request) ([]content.Chunk, error) {
		if !requestHasRole(req, generic.Name) {
			return nil, errors.New("coderig test: native request lost Generic identity")
		}
		sawMessageAgent = slices.Contains(toolNamesFromRequest(req), "MessageAgent")
		return finalText("native Generic complete"), nil
	}

	// The collaboration executable is deliberately present in Config, but no
	// foreign child is started. Generic's existing Harness-managed MessageAgent
	// must remain in its in-process tool roster.
	path := filepath.Join(t.TempDir(), collabMCPExecutableName)
	writeExecutable(t, path)
	agent := newTestAgent(t, client, Config{CollabMCPExecutable: path})
	if got := runManagedTurn(t, agent, "inspect native collaboration tools"); got != "native Generic complete" {
		t.Fatalf("native Generic result = %q", got)
	}
	if !sawMessageAgent {
		t.Fatal("native Generic request omitted the in-process MessageAgent")
	}
}

func TestCollabMCPIntegrationMissingBinaryFailsClosed(t *testing.T) {
	configured := configuredProductionModelsForTest("configured-only")
	missing := filepath.Join(t.TempDir(), collabMCPExecutableName)
	_, err := withProductionACPChildren(context.Background(), Config{CollabMCPExecutable: missing}, configured)
	if err == nil {
		t.Fatal("withProductionACPChildren accepted a missing collaboration MCP executable")
	}
	if !errors.Is(err, errCollabMCPExecutableUnavailable) {
		t.Fatalf("missing collaboration MCP error = %T, want errCollabMCPExecutableUnavailable", err)
	}
}

func assertCollabMCPIntegrationDescriptor(t *testing.T, capture collabMCPIntegrationCapture, executable string) {
	t.Helper()
	if capture.name != collabMCPExecutableName {
		t.Fatalf("MCP server name = %q, want %q", capture.name, collabMCPExecutableName)
	}
	if capture.command != executable {
		t.Fatalf("MCP command = %q, want resolved executable", capture.command)
	}
	if len(capture.args) != 0 {
		t.Fatalf("MCP args = %#v, want no endpoint or token arguments", capture.args)
	}
	if len(capture.env) != 2 {
		t.Fatalf("MCP environment entry count = %d, want exactly 2", len(capture.env))
	}
	wantToken, err := collab.EncodeCapabilityToken(capture.token)
	if err != nil {
		t.Fatalf("encode captured capability: %v", err)
	}
	want := []protocol.EnvVariable{
		{Name: collab.EndpointEnv, Value: capture.endpoint},
		{Name: collab.TokenEnv, Value: wantToken},
	}
	if len(capture.env) != len(want) || capture.env[0].Name != want[0].Name || capture.env[0].Value != want[0].Value || capture.env[1].Name != want[1].Name || capture.env[1].Value != want[1].Value {
		t.Fatalf("MCP environment names/order/values do not match the broker descriptor")
	}
	if capture.endpoint == "" || len(capture.token) != collab.CapabilityBytes {
		t.Fatal("MCP descriptor omitted the broker endpoint or fixed-size capability")
	}
}

func requireCollabMCPIntegrationSocket(t *testing.T) {
	t.Helper()
	if os.PathSeparator != '/' {
		t.Skip("collaboration MCP integration requires Unix-domain sockets")
	}
	probeBase := "/private/tmp"
	if _, err := os.Stat(probeBase); err != nil {
		probeBase = os.TempDir()
	}
	probeRoot, err := os.MkdirTemp(probeBase, "cmt-")
	if err != nil {
		t.Fatalf("create short Unix socket probe directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(probeRoot) })
	probe := filepath.Join(probeRoot, "probe.sock")
	listener, err := net.Listen("unix", probe)
	if err != nil {
		if strings.Contains(err.Error(), "operation not permitted") || strings.Contains(err.Error(), "permission denied") {
			t.Skipf("host Unix sockets unavailable in this runner: %v", err)
		}
		t.Fatalf("probe Unix socket: %v", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("close Unix socket probe: %v", err)
	}
}

var _ foreign.ServicesBuilder = (*collabMCPIntegrationRecorder)(nil).liveServices
var _ foreign.ServicesRestoredBuilder = (*collabMCPIntegrationRecorder)(nil).restoredServices
