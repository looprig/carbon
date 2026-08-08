package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"testing"

	"github.com/looprig/acp/launch"
	"github.com/looprig/acp/protocol"
	"github.com/looprig/coderig/internal/catalog/builder"
	"github.com/looprig/coderig/internal/catalog/planner"
	"github.com/looprig/coderig/internal/catalog/reviewer"
	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/rig"
	"github.com/looprig/inference"
	model "github.com/looprig/inference/model"
	inferenceStream "github.com/looprig/inference/stream"
)

const task6ACPPermissionHelperPath = "task6-acp-permission-helper"

func init() {
	if os.Getenv("PATH") == task6ACPPermissionHelperPath {
		os.Exit(runTask6ACPPermissionHelper())
	}
}

func runTask6ACPPermissionHelper() int {
	conn := protocol.NewConn(os.Stdin, os.Stdout, protocol.ConnOptions{})
	peer := protocol.NewClientConn(conn)
	defer conn.Close()
	ready := make(chan struct{})
	var workspace string
	const sessionID protocol.SessionID = "task6-permission-session"

	conn.Handle(string(protocol.MethodInitialize), func(context.Context, string, json.RawMessage) (any, error) {
		<-ready
		return protocol.InitializeResponse{ProtocolVersion: protocol.CurrentProtocolVersion}, nil
	})
	conn.Handle(string(protocol.MethodSessionNew), func(_ context.Context, _ string, params json.RawMessage) (any, error) {
		var request protocol.NewSessionRequest
		if err := json.Unmarshal(params, &request); err != nil {
			return nil, protocol.InvalidParams("task6 session/new", nil)
		}
		workspace = request.Cwd
		return protocol.NewSessionResponse{SessionID: sessionID}, nil
	})
	conn.Handle(string(protocol.MethodSessionPrompt), func(ctx context.Context, _ string, _ json.RawMessage) (any, error) {
		kind := protocol.ToolKindEdit
		title := "task6 outside-posture mutation"
		response, err := peer.RequestPermission(ctx, protocol.RequestPermissionRequest{
			SessionID: sessionID,
			Options: []protocol.PermissionOption{
				{Name: "Allow once", Kind: protocol.PermissionOptionKindAllowOnce, OptionID: "allow-once"},
				{Name: "Reject once", Kind: protocol.PermissionOptionKindRejectOnce, OptionID: "reject-once"},
			},
			ToolCall: protocol.ToolCallUpdate{
				Kind:  &kind,
				Title: &title,
				Content: []protocol.ToolCallContent{{Diff: &protocol.Diff{
					Path: filepath.Join(filepath.Dir(workspace), "task6-outside.txt"),
				}}},
			},
		})
		if err != nil {
			return nil, err
		}
		selected := ""
		if response != nil && response.Outcome.Selected != nil {
			selected = string(response.Outcome.Selected.OptionID)
		}
		if err := os.WriteFile(filepath.Join(workspace, "task6-permission.receipt"), []byte(selected), 0o600); err != nil {
			return nil, err
		}
		if err := peer.SessionUpdate(ctx, protocol.SessionNotification{
			SessionID: sessionID,
			Update: protocol.SessionUpdate{AgentMessageChunk: &protocol.ContentChunk{
				Content: protocol.ContentBlock{Text: &protocol.TextContent{Text: "task6 permission denied"}},
			}},
		}); err != nil {
			return nil, err
		}
		return protocol.PromptResponse{StopReason: protocol.StopReasonEndTurn}, nil
	})
	close(ready)
	<-conn.Done()
	return 0
}

func TestACPRequestPermissionDeniesOutsidePostureWithoutNativePermissionWrites(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("NO_PROXY", "")
	workspace := canonicalTempDir(t)

	modelPath := filepath.Join(home, ".looprig", "models.json")
	permissionPath, err := defaultPermissionsPath(filepath.Join(home, ".looprig"), workspace)
	if err != nil {
		t.Fatalf("defaultPermissionsPath: %v", err)
	}
	modelBytes := []byte(`{"version":2,"api_key":"task6-obvious-fake-provider-key"}`)
	permissionBytes := []byte(`{"version":1,"rules":[]}`)
	for path, data := range map[string][]byte{modelPath: modelBytes, permissionPath: permissionBytes} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("create fixture directory: %v", err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatalf("write boundary fixture: %v", err)
		}
	}
	modelBefore, _ := os.Stat(modelPath)
	permissionBefore, _ := os.Stat(permissionPath)

	provider := &task33InferenceClient{}
	configured := configuredProductionModelsForTest("fixture-model")
	configured.ACP[0].Client = provider
	compiled, err := CompileACPCatalog(ACPCatalogInput{
		AgentTypes:     []identity.AgentName{builder.Name},
		GatewayTargets: configured.ACP,
	})
	if err != nil {
		t.Fatalf("CompileACPCatalog: %v", err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	composition, err := NewACPComposition(ACPChildrenConfig{
		Catalog:       compiled,
		Executables:   map[loop.AgentHarnessName]string{"codex": executable},
		WorkspaceRoot: workspace,
		Env: []string{
			"PATH=" + task6ACPPermissionHelperPath,
			"TMPDIR=" + workspace,
			"PROVIDER_SENTINEL=task6-obvious-fake-provider-key",
			"MODELS_JSON_PATH=" + modelPath,
			"MODELS_JSON_CONTENT=" + string(modelBytes),
			"NATIVE_PERMISSION_PATH=" + permissionPath,
			"NATIVE_PERMISSION_CONTENT=" + string(permissionBytes),
			"ANTHROPIC_API_KEY=task6-obvious-fake-anthropic-key",
			"OPENAI_API_KEY=task6-obvious-fake-openai-key",
			"CLAUDE_CODE_ACP_NATIVE_MODELS=native=obsolete",
			"CODEX_ACP_NATIVE_MODELS=native=obsolete",
		},
		GatewayEnvAllowlist: acpGatewayEnvAllowlist,
		gatewayPreflightBinding: &launch.ProxyBinding{
			BaseURL: "http://127.0.0.1:1",
			Token:   "task6-preflight-token",
		},
		executablePreflight: func(_ context.Context, probe ACPExecutableProbe) ACPPreflightResult {
			if len(probe.Env) != 2 || probe.Env[0] != "PATH="+task6ACPPermissionHelperPath || probe.Env[1] != "TMPDIR="+workspace {
				t.Fatalf("ACP preflight env = %#v, want only explicit process values", probe.Env)
			}
			return ACPPreflightResult{Ready: true}
		},
	})
	if err != nil {
		t.Fatalf("NewACPComposition: %v", err)
	}

	step := 0
	parent := &managedScript{fn: func(_ context.Context, _ inference.Request) ([]content.Chunk, error) {
		switch step {
		case 0:
			step++
			return startAgentCall("task6-permission", `{"agent_type":"builder","instructions":"request outside write"}`), nil
		case 1:
			step++
			return finalText("task6 parent complete"), nil
		default:
			return nil, fmt.Errorf("unexpected task6 parent step %d", step)
		}
	}}
	access, cfg := headlessTestAccess(t, Config{ACPChildren: composition}, workspace)
	definitions, err := swarmDefinitions(parent, testModel(), cfg, access)
	if err != nil {
		t.Fatalf("swarmDefinitions: %v", err)
	}
	stores, err := openTestStores(t)
	if err != nil {
		t.Fatal(err)
	}
	registration, err := newConversationHustleRegistration()
	if err != nil {
		t.Fatal(err)
	}
	assembly, err := buildRigWithRegistrationAndACP(definitions, stores, workspace, cfg, false,
		rig.DelegationLimits{Depth: delegationSpawnDepth, Quota: delegationSpawnQuota},
		registration, permissionReviewRegistration{}, composition)
	if err != nil {
		t.Fatalf("buildRigWithRegistrationAndACP: %v", err)
	}
	controller, err := assembly.NewSession(context.Background())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	agent, err := newSessionAdapter(context.Background(), controller, stores.session, false)
	if err != nil {
		t.Fatalf("newSessionAdapter: %v", err)
	}
	t.Cleanup(func() { _ = agent.Close(context.Background()) })
	if got := runManagedTurn(t, agent, "exercise ACP permission posture"); got != "task6 parent complete" {
		t.Fatalf("parent result = %q", got)
	}
	if got, err := os.ReadFile(filepath.Join(workspace, "task6-permission.receipt")); err != nil || string(got) != "reject-once" {
		t.Fatalf("permission receipt = %q, error = %v; want reject-once", got, err)
	}

	for path, before := range map[string]struct {
		data []byte
		info os.FileInfo
	}{modelPath: {modelBytes, modelBefore}, permissionPath: {permissionBytes, permissionBefore}} {
		afterBytes, readErr := os.ReadFile(path)
		afterInfo, statErr := os.Stat(path)
		if readErr != nil || statErr != nil || !bytes.Equal(afterBytes, before.data) || !afterInfo.ModTime().Equal(before.info.ModTime()) || afterInfo.Mode() != before.info.Mode() {
			t.Fatalf("ACP permission flow modified %s", filepath.Base(path))
		}
	}
}

// task33InferenceClient is the fake provider behind each child-owned gateway.
// Keeping the capture at inference.Request is what proves the fixed gateway
// applied the selected effort before the request reached the provider.
type task33InferenceClient struct {
	mu       sync.Mutex
	requests []inference.Request
}

func (c *task33InferenceClient) Invoke(_ context.Context, req inference.Request) (*inference.Response, error) {
	c.mu.Lock()
	c.requests = append(c.requests, req)
	c.mu.Unlock()
	return &inference.Response{
		Message: &content.AIMessage{Message: content.Message{
			Role:   content.RoleAssistant,
			Blocks: []content.Block{&content.TextBlock{Text: "task33 provider answer"}},
		}},
		FinishReason: inferenceStream.FinishReasonStop,
	}, nil
}

func (c *task33InferenceClient) Stream(_ context.Context, req inference.Request) (*inferenceStream.StreamReader[content.Chunk], error) {
	c.mu.Lock()
	c.requests = append(c.requests, req)
	c.mu.Unlock()
	return inferenceStream.NewStreamReader(func() (content.Chunk, error) { return nil, io.EOF }, nil), nil
}

func TestAgentRuntimeChoicesEndToEnd(t *testing.T) {
	openAI := &task33InferenceClient{}
	anthropic := &task33InferenceClient{}
	compiled, err := CompileACPCatalog(ACPCatalogInput{
		AgentTypes: []identity.AgentName{planner.Name, builder.Name, reviewer.Name},
		GatewayTargets: legacyTestGatewayTargets(map[model.ProviderName]inference.Client{
			"openai":    openAI,
			"anthropic": anthropic,
		}),
		ClaudeSmall: "sonnet-5",
	})
	if err != nil {
		t.Fatalf("CompileACPCatalog() error = %v", err)
	}
	workspace := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() error = %v", err)
	}
	composition, err := NewACPComposition(ACPChildrenConfig{
		Catalog: compiled,
		Executables: map[loop.AgentHarnessName]string{
			"codex":       executable,
			"claude-code": executable,
		},
		WorkspaceRoot:       workspace,
		Env:                 []string{"PATH=" + task33ACPHelperPath, "TMPDIR=" + workspace},
		GatewayEnvAllowlist: []string{"PATH", "TMPDIR"},
		gatewayPreflightBinding: &launch.ProxyBinding{
			BaseURL: "http://127.0.0.1:1",
			Token:   "task33-preflight-token",
		},
		executablePreflight: func(_ context.Context, probe ACPExecutableProbe) ACPPreflightResult {
			return ACPPreflightResult{Ready: true, AdvertisedModels: append([]string(nil), probe.Models...)}
		},
	})
	if err != nil {
		t.Fatalf("NewACPComposition() error = %v", err)
	}

	var step int
	var claude agentHandle
	var toolResult string
	client := &managedScript{}
	client.fn = func(_ context.Context, req inference.Request) ([]content.Chunk, error) {
		if !requestHasRole(req, builder.Name) {
			return nil, fmt.Errorf("task33: unexpected non-parent inference request")
		}
		prior := lastToolText(req)
		switch step {
		case 0:
			step++
			return startAgentCall("task33-claude-start", `{"agent_type":"reviewer","instructions":"check it","agent_harness":"claude-code","model":"sonnet-5","effort":"high"}`), nil
		case 1:
			toolResult = prior
			var foreground struct {
				AgentID  string `json:"agent_id"`
				Response string `json:"response"`
			}
			if err := json.Unmarshal([]byte(prior), &foreground); err != nil {
				return nil, fmt.Errorf("task33 foreground result: %w", err)
			}
			claude.AgentID = foreground.AgentID
			if foreground.Response != "task33 claude answer" {
				return nil, fmt.Errorf("task33 Claude response = %q", foreground.Response)
			}
			step++
			return finalText("task33 complete"), nil
		default:
			return nil, fmt.Errorf("task33: unexpected parent step %d", step)
		}
	}

	access, cfg := headlessTestAccess(t, Config{ACPChildren: composition}, workspace)
	definitions, err := swarmDefinitions(client, testModel(), cfg, access)
	if err != nil {
		t.Fatalf("swarmDefinitions() error = %v", err)
	}
	stores, err := openTestStores(t)
	if err != nil {
		t.Fatal(err)
	}
	registration, err := newConversationHustleRegistration()
	if err != nil {
		t.Fatal(err)
	}
	assembly, err := buildRigWithRegistrationAndACP(
		definitions,
		stores,
		workspace,
		cfg,
		false,
		rig.DelegationLimits{Depth: delegationSpawnDepth, Quota: delegationSpawnQuota},
		registration,
		permissionReviewRegistration{},
		composition,
	)
	if err != nil {
		t.Fatalf("buildRigWithRegistrationAndACP() error = %v", err)
	}
	controller, err := assembly.NewSession(context.Background())
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	agent, err := newSessionAdapter(context.Background(), controller, stores.session, false)
	if err != nil {
		t.Fatalf("newSessionAdapter() error = %v", err)
	}
	t.Cleanup(func() { _ = agent.Close(context.Background()) })
	result := runManagedTurn(t, agent, "run the reviewer")
	if result != "task33 complete" {
		t.Fatalf("parent final = %q", result)
	}
	if got, err := os.ReadFile(filepath.Join(workspace, "task33-claude-small-model.receipt")); err != nil || string(got) != "sonnet-5" {
		t.Fatalf("fake Claude small-model receipt = %q, error = %v; want sonnet-5", got, err)
	}
	if task33SensitivePayloadPattern.MatchString(toolResult) {
		t.Fatalf("tool result contains secret/path/URL-looking data: %q", toolResult)
	}
	assertTask33DurableEvents(t, replayACPEvents(t, stores.session, agent.SessionID()), claude)
}

var task33SensitivePayloadPattern = regexp.MustCompile(`(?i)(https?://|(?:^|[\s"'])/[^\s"']*|[A-Za-z]:[\\/]|secret|token|api[_-]?key)`)

func assertTask33DurableEvents(t *testing.T, events []event.Event, claude agentHandle) {
	t.Helper()
	want := map[identity.AgentName]event.AgentRuntime{
		reviewer.Name: {
			Harness: "claude-code", Profile: "acp/claude-code", CredentialMode: "gateway-backed",
			Source: "gateway", SelectionKind: "explicit",
			ModelAlias: "sonnet-5@high", SmallModelAlias: "sonnet-5",
		},
	}
	ids := map[identity.AgentName]uuid.UUID{}
	bound := make(map[identity.AgentName]string)
	for _, raw := range events {
		switch ev := raw.(type) {
		case event.LoopStarted:
			if ev.AgentRuntime == nil {
				continue
			}
			wantRuntime, ok := want[ev.AgentName]
			if !ok {
				t.Fatalf("unexpected durable ACP runtime for %q: %+v", ev.AgentName, *ev.AgentRuntime)
			}
			if *ev.AgentRuntime != wantRuntime {
				t.Fatalf("durable %s AgentRuntime = %+v, want %+v", ev.AgentName, *ev.AgentRuntime, wantRuntime)
			}
			ids[ev.AgentName] = ev.LoopID
		case event.LoopAgentSessionBound:
			for role, id := range ids {
				if ev.LoopID == id {
					bound[role] = ev.ACPSessionID
				}
			}
		}
	}
	if len(ids) != 1 {
		t.Fatalf("durable ACP LoopStarted identities = %v, want reviewer", ids)
	}
	if bound[reviewer.Name] != "task33-claude-code-session" {
		t.Fatalf("durable ACP session bindings = %v", bound)
	}
	if claude.AgentID == "" {
		t.Fatal("agent handle was empty")
	}
}
