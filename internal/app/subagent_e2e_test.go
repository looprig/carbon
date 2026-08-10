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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/looprig/acp/launch"
	"github.com/looprig/acp/protocol"
	"github.com/looprig/carbon/internal/catalog/carbon"
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
	switch os.Getenv("PATH") {
	case task6ACPPermissionHelperPath:
		os.Exit(runTask6ACPPermissionHelper(false))
	case taskACPPostureHelperPath:
		os.Exit(runTask6ACPPermissionHelper(true))
	}
}

func runTask6ACPPermissionHelper(postureMatrix bool) int {
	conn := protocol.NewConn(os.Stdin, os.Stdout, protocol.ConnOptions{})
	peer := protocol.NewClientConn(conn)
	defer conn.Close()
	ready := make(chan struct{})
	var workspace string
	var stateMu sync.Mutex
	setWorkspace := func(root string) {
		stateMu.Lock()
		workspace = root
		stateMu.Unlock()
	}
	getWorkspace := func() string {
		stateMu.Lock()
		defer stateMu.Unlock()
		return workspace
	}

	conn.Handle(string(protocol.MethodInitialize), func(context.Context, string, json.RawMessage) (any, error) {
		<-ready
		return protocol.InitializeResponse{ProtocolVersion: protocol.CurrentProtocolVersion}, nil
	})
	conn.Handle(string(protocol.MethodSessionNew), func(_ context.Context, _ string, params json.RawMessage) (any, error) {
		var request protocol.NewSessionRequest
		if err := json.Unmarshal(params, &request); err != nil {
			return nil, protocol.InvalidParams("task6 session/new", nil)
		}
		id := protocol.SessionID("task6-permission-session")
		if postureMatrix {
			id = "acp-posture-session"
		}
		setWorkspace(request.Cwd)
		return posturePermissionNewSessionResponse(id), nil
	})
	conn.Handle(string(protocol.MethodSessionLoad), func(_ context.Context, _ string, params json.RawMessage) (any, error) {
		var request protocol.LoadSessionRequest
		if err := json.Unmarshal(params, &request); err != nil {
			return nil, protocol.InvalidParams("task6 session/load", nil)
		}
		setWorkspace(request.Cwd)
		response := posturePermissionNewSessionResponse(request.SessionID)
		return protocol.LoadSessionResponse{ConfigOptions: response.ConfigOptions, Modes: response.Modes}, nil
	})
	conn.Handle(string(protocol.MethodSessionSetConfigOption), func(_ context.Context, _ string, _ json.RawMessage) (any, error) {
		return posturePermissionSetConfigResponse(), nil
	})
	conn.Handle(string(protocol.MethodSessionSetMode), func(context.Context, string, json.RawMessage) (any, error) {
		return protocol.SetSessionModeResponse{}, nil
	})
	conn.Handle(string(protocol.MethodSessionPrompt), func(ctx context.Context, _ string, params json.RawMessage) (any, error) {
		var request protocol.PromptRequest
		if err := json.Unmarshal(params, &request); err != nil {
			return nil, protocol.InvalidParams("task6 session/prompt", nil)
		}
		kind := protocol.ToolKindEdit
		title := "task6 outside-posture mutation"
		workspace := getWorkspace()
		targetPath := filepath.Join(filepath.Dir(workspace), "task6-outside.txt")
		receiptName := "task6-permission.receipt"
		if postureMatrix {
			title = "ACP posture in-workspace edit"
			targetPath = filepath.Join(workspace, "inside.txt")
			receiptName = acpPostureReceiptName
		}
		response, err := peer.RequestPermission(ctx, protocol.RequestPermissionRequest{
			SessionID: request.SessionID,
			Options: []protocol.PermissionOption{
				{Name: "Allow once", Kind: protocol.PermissionOptionKindAllowOnce, OptionID: "allow-once"},
				{Name: "Reject once", Kind: protocol.PermissionOptionKindRejectOnce, OptionID: "reject-once"},
			},
			ToolCall: protocol.ToolCallUpdate{
				Kind:  &kind,
				Title: &title,
				Content: []protocol.ToolCallContent{{Diff: &protocol.Diff{
					Path: targetPath,
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
		if postureMatrix && selected == "allow-once" {
			if err := os.WriteFile(filepath.Join(workspace, acpPostureWriteName), []byte("workspace-write"), 0o600); err != nil {
				return nil, err
			}
		}
		receiptPath := filepath.Join(workspace, receiptName)
		if err := os.WriteFile(receiptPath+".tmp", []byte(selected), 0o600); err != nil {
			return nil, err
		}
		if err := os.Rename(receiptPath+".tmp", receiptPath); err != nil {
			return nil, err
		}
		if err := peer.SessionUpdate(ctx, protocol.SessionNotification{
			SessionID: request.SessionID,
			Update: protocol.SessionUpdate{AgentMessageChunk: &protocol.ContentChunk{
				Content: protocol.ContentBlock{Text: &protocol.TextContent{Text: posturePermissionUpdateText(postureMatrix)}},
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

func posturePermissionNewSessionResponse(sessionID protocol.SessionID) protocol.NewSessionResponse {
	category := protocol.SessionConfigOptionCategoryModel
	return protocol.NewSessionResponse{
		SessionID: sessionID,
		ConfigOptions: []protocol.SessionConfigOption{{
			Category: &category,
			ID:       "model",
			Name:     "Model",
			Select: &protocol.SessionConfigSelect{
				CurrentValue: "sonnet-5",
				Options: protocol.SessionConfigSelectOptions{Ungrouped: []protocol.SessionConfigSelectOption{
					{Name: "Sonnet", Value: "sonnet-5"},
				}},
			},
		}},
		Modes: &protocol.SessionModeState{
			CurrentModeID: "default",
			AvailableModes: []protocol.SessionMode{
				{ID: "default", Name: "Default"},
				{ID: "acceptEdits", Name: "Accept edits"},
			},
		},
	}
}

func posturePermissionSetConfigResponse() protocol.SetSessionConfigOptionResponse {
	category := protocol.SessionConfigOptionCategoryModel
	return protocol.SetSessionConfigOptionResponse{ConfigOptions: []protocol.SessionConfigOption{{
		Category: &category,
		ID:       "model",
		Name:     "Model",
		Select: &protocol.SessionConfigSelect{
			CurrentValue: "sonnet-5",
			Options: protocol.SessionConfigSelectOptions{Ungrouped: []protocol.SessionConfigSelectOption{
				{Name: "Sonnet", Value: "sonnet-5"},
			}},
		},
	}}}
}

func posturePermissionUpdateText(postureMatrix bool) string {
	if postureMatrix {
		return "ACP posture probe complete"
	}
	return "task6 permission denied"
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
	compiled, err := CompileAgentRuntimeCatalog(AgentRuntimeCatalogInput{
		GatewayTargets: configured.ACP,
	})
	if err != nil {
		t.Fatalf("CompileAgentRuntimeCatalog: %v", err)
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
	parent := &managedScript{fn: func(_ context.Context, req inference.Request) ([]content.Chunk, error) {
		switch step {
		case 0:
			step++
			return startAgentCall("task6-permission", `{"agent_type":"carbon","instructions":"request outside write","agent_harness":"codex","model":"fixture-model","effort":"none"}`), nil
		case 1:
			step++
			return finalText("task6 parent complete"), nil
		default:
			return nil, fmt.Errorf("unexpected task6 parent step %d", step)
		}
	}}
	access, cfg := headlessTestAccess(t, Config{ACPChildren: composition, RuntimeCatalog: compiled.RuntimeCatalog}, workspace)
	definition, err := carbonTestDefinition(parent, testModel(), cfg, access)
	if err != nil {
		t.Fatalf("carbonTestDefinition: %v", err)
	}
	stores, err := openTestStores(t)
	if err != nil {
		t.Fatal(err)
	}
	registration, err := newConversationHustleRegistration()
	if err != nil {
		t.Fatal(err)
	}
	assembly, err := buildRigWithRegistrationAndACP(definition, stores, workspace, cfg, false,
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
	compiled, err := CompileAgentRuntimeCatalog(AgentRuntimeCatalogInput{
		GatewayTargets: legacyTestGatewayTargets(map[model.ProviderName]inference.Client{
			"openai":    openAI,
			"anthropic": anthropic,
		}),
		ClaudeSmall: "sonnet-5",
	})
	if err != nil {
		t.Fatalf("CompileAgentRuntimeCatalog() error = %v", err)
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
		if !requestHasRole(req, carbon.Name) {
			return nil, fmt.Errorf("task33: unexpected non-parent inference request")
		}
		prior := lastToolText(req)
		switch step {
		case 0:
			step++
			return startAgentCall("task33-claude-start", `{"agent_type":"carbon","instructions":"check it","agent_harness":"claude-code","model":"sonnet-5","effort":"high"}`), nil
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

	access, cfg := headlessTestAccess(t, Config{ACPChildren: composition, RuntimeCatalog: compiled.RuntimeCatalog}, workspace)
	definition, err := carbonTestDefinition(client, testModel(), cfg, access)
	if err != nil {
		t.Fatalf("carbonTestDefinition() error = %v", err)
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
		definition,
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
	result := runManagedTurn(t, agent, "run the Carbon ACP child")
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

func TestNativeCodexStartAgentSelectsModelAndEffortOverWire(t *testing.T) {
	parent := &managedScript{}
	compiled, err := CompileAgentRuntimeCatalog(AgentRuntimeCatalogInput{
		NativeACP: map[string]ACPNativeProfile{"codex": {
			Harness: "codex", Enabled: true,
			ModelOptions: []ACPNativeModelOption{{
				Alias: "friendly-luna", Model: "gpt-5.6-luna",
				Efforts: []model.Effort{model.EffortHigh, model.EffortMax}, DefaultEffort: model.EffortMax,
			}},
		}},
		PrimerTarget: runtimeCatalogPrimer(),
	})
	if err != nil {
		t.Fatalf("CompileAgentRuntimeCatalog() error = %v", err)
	}
	workspace := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() error = %v", err)
	}
	composition, err := NewACPComposition(ACPChildrenConfig{
		Catalog:            compiled,
		Executables:        map[loop.AgentHarnessName]string{"codex": executable},
		WorkspaceRoot:      workspace,
		Env:                []string{"PATH=" + task33NativeCodexACPHelperPath},
		NativeEnvAllowlist: []string{"PATH"},
	})
	if err != nil {
		t.Fatalf("NewACPComposition() error = %v", err)
	}

	step := 0
	parent.fn = func(_ context.Context, req inference.Request) ([]content.Chunk, error) {
		if !requestHasRole(req, carbon.Name) {
			return nil, fmt.Errorf("unexpected role in native Codex StartAgent request")
		}
		switch step {
		case 0:
			step++
			start := findInferenceTool(t, req, "StartAgent")
			schema := string(start.Schema)
			if !strings.Contains(schema, "friendly-luna") {
				return nil, fmt.Errorf("StartAgent schema omitted friendly native alias")
			}
			if strings.Contains(schema, "gpt-5.6-luna") {
				return nil, fmt.Errorf("StartAgent schema leaked adapter model ID")
			}
			return startAgentCall("task33-native-codex-start", `{"agent_type":"carbon","instructions":"check it","agent_harness":"codex","model":"friendly-luna","effort":"max"}`), nil
		case 1:
			if !strings.Contains(lastToolText(req), `"response":"task33 native codex answer"`) {
				return nil, fmt.Errorf("native Codex StartAgent did not return the expected response")
			}
			return finalText("native Codex selection complete"), nil
		default:
			return nil, fmt.Errorf("unexpected native Codex parent step %d", step)
		}
	}

	access, cfg := headlessTestAccess(t, Config{ACPChildren: composition, RuntimeCatalog: compiled.RuntimeCatalog}, workspace)
	definition, err := carbonTestDefinition(parent, testModel(), cfg, access)
	if err != nil {
		t.Fatalf("carbonTestDefinition() error = %v", err)
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
		definition, stores, workspace, cfg, false,
		rig.DelegationLimits{Depth: delegationSpawnDepth, Quota: delegationSpawnQuota},
		registration, permissionReviewRegistration{}, composition,
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
	if got := runManagedTurn(t, agent, "run the native Codex child"); got != "native Codex selection complete" {
		t.Fatalf("native Codex parent final = %q", got)
	}

	state, err := os.ReadFile(filepath.Join(workspace, task33NativeCodexStateReceipt))
	if err != nil {
		t.Fatalf("read native Codex peer state: %v", err)
	}
	if got, want := string(state), "model=gpt-5.6-luna\neffort=max\ncalls=model=gpt-5.6-luna,reasoning_effort=max\n"; got != want {
		t.Fatalf("native Codex peer state = %q, want %q", got, want)
	}

	sessionID := agent.SessionID()
	if err := agent.Close(context.Background()); err != nil {
		t.Fatalf("native Codex initial Close() error = %v", err)
	}
	receiptPath := filepath.Join(workspace, task33NativeCodexStateReceipt)
	if err := os.Remove(receiptPath); err != nil {
		t.Fatalf("remove fresh native Codex peer state: %v", err)
	}
	if _, err := os.Stat(receiptPath); !os.IsNotExist(err) {
		t.Fatalf("fresh native Codex peer state after removal: err = %v, want not exist", err)
	}
	restoredController, err := assembly.RestoreSession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("native Codex RestoreSession() error = %v", err)
	}
	restored, err := newSessionAdapter(context.Background(), restoredController, stores.session, true)
	if err != nil {
		t.Fatalf("native Codex restored adapter error = %v", err)
	}
	t.Cleanup(func() { _ = restored.Close(context.Background()) })
	restoredState, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatalf("read restored native Codex peer state: %v", err)
	}
	if got, want := string(restoredState), "model=gpt-5.6-luna\neffort=max\ncalls=model=gpt-5.6-luna,reasoning_effort=max\n"; got != want {
		t.Fatalf("restored native Codex peer state = %q, want %q", got, want)
	}
}

func TestNativeCodexUnavailableEffortFailsLazilyWithoutInvalidWireCall(t *testing.T) {
	compiled, err := CompileAgentRuntimeCatalog(AgentRuntimeCatalogInput{
		NativeACP: map[string]ACPNativeProfile{"codex": {
			Harness: "codex", Enabled: true,
			ModelOptions: []ACPNativeModelOption{{
				Alias: "friendly-terra", Model: "gpt-5.6-terra",
				Efforts: []model.Effort{model.EffortMax}, DefaultEffort: model.EffortMax,
			}},
		}},
		PrimerTarget: runtimeCatalogPrimer(),
	})
	if err != nil {
		t.Fatalf("CompileAgentRuntimeCatalog() error = %v", err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() error = %v", err)
	}
	workspace := t.TempDir()
	preflightCalls := 0
	composition, err := NewACPComposition(ACPChildrenConfig{
		Catalog:            compiled,
		Executables:        map[loop.AgentHarnessName]string{"codex": executable},
		WorkspaceRoot:      workspace,
		Env:                []string{"PATH=" + task33NativeCodexACPHelperPath},
		NativeEnvAllowlist: []string{"PATH"},
		executablePreflight: func(context.Context, ACPExecutableProbe) ACPPreflightResult {
			preflightCalls++
			return ACPPreflightResult{Ready: true}
		},
	})
	if err != nil {
		t.Fatalf("NewACPComposition() error = %v", err)
	}
	if preflightCalls != 0 {
		t.Fatalf("native selection preflight calls = %d, want zero", preflightCalls)
	}
	resolved, err := composition.Catalog.RuntimeCatalog.ResolveWithExplicitSource(
		carbon.Name, "codex", loop.RuntimeSourceNative, "friendly-terra", model.EffortMax, true,
	)
	if err != nil {
		t.Fatalf("ResolveWithExplicitSource() error = %v", err)
	}
	bound := testACPChildBound(t, resolved)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	backend, _, err := composition.Live(
		ctx, mustUUID(t), mustUUID(t), loop.Provenance{}, acpPostureMatrixPublisher{}, bound,
		func() (uuid.UUID, error) { return uuid.New() },
		event.NewFactory(func() (uuid.UUID, error) { return uuid.New() }, time.Now),
	)
	if err == nil {
		_ = backend
		t.Fatal("native Codex unavailable effort selection succeeded")
	}
	statePath := filepath.Join(workspace, task33NativeCodexStateReceipt)
	if _, statErr := os.Stat(statePath); !os.IsNotExist(statErr) {
		t.Fatalf("native Codex final state receipt after unavailable effort: err = %v, want not exist", statErr)
	}
	state, err := os.ReadFile(filepath.Join(workspace, task33NativeCodexCallsReceipt))
	if err != nil {
		t.Fatalf("read native Codex wire calls after bounded failure: %v", err)
	}
	if got, want := string(state), "calls=model=gpt-5.6-terra\n"; got != want {
		t.Fatalf("native Codex wire calls after unavailable effort = %q, want %q", got, want)
	}
}

var task33SensitivePayloadPattern = regexp.MustCompile(`(?i)(https?://|(?:^|[\s"'])/[^\s"']*|[A-Za-z]:[\\/]|secret|token|api[_-]?key)`)

func assertTask33DurableEvents(t *testing.T, events []event.Event, claude agentHandle) {
	t.Helper()
	want := map[identity.AgentName]event.AgentRuntime{
		carbon.Name: {
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
		t.Fatalf("durable ACP LoopStarted identities = %v, want one Carbon child", ids)
	}
	if bound[carbon.Name] != "task33-claude-code-session" {
		t.Fatalf("durable ACP session bindings = %v", bound)
	}
	if claude.AgentID == "" {
		t.Fatal("agent handle was empty")
	}
}
