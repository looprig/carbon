package app

import (
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

	"github.com/looprig/acp/launch"
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
	"github.com/looprig/storage/memstore"
)

// task33QueuedResult is the model-facing result returned by a background start.
// The runtime block is intentionally the advertised selector block: profile and
// credential details remain durable Harness identity, not model-facing data.
type task33QueuedResult struct {
	DelegateID string               `json:"delegate_id"`
	RequestID  string               `json:"request_id"`
	Status     string               `json:"status"`
	Runtime    *task33RuntimeResult `json:"runtime"`
}

type task33RuntimeResult struct {
	AgentHarness string `json:"agent_harness"`
	Model        string `json:"model"`
	Effort       string `json:"effort"`
}

type task33StatusResult struct {
	Children []struct {
		DelegateID string `json:"delegate_id"`
		Status     string `json:"status"`
	} `json:"children"`
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

func (c *task33InferenceClient) capturedRequests() []inference.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]inference.Request(nil), c.requests...)
}

func TestSubagentLunaMaxConcurrentEndToEnd(t *testing.T) {
	openAI := &task33InferenceClient{}
	anthropic := &task33InferenceClient{}
	compiled, err := CompileACPCatalog(ACPCatalogInput{
		SubagentTypes: []identity.AgentName{planner.Name, builder.Name, reviewer.Name},
		GatewayClients: map[model.ProviderName]inference.Client{
			"openai":    openAI,
			"anthropic": anthropic,
		},
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
	var codex, claude task33QueuedResult
	var toolResults []string
	var statusResult string
	client := &managedScript{}
	client.fn = func(_ context.Context, req inference.Request) ([]content.Chunk, error) {
		if !requestHasRole(req, builder.Name) {
			return nil, fmt.Errorf("task33: unexpected non-parent inference request")
		}
		prior := lastToolText(req)
		if prior != "" {
			toolResults = append(toolResults, prior)
		}
		switch step {
		case 0:
			step++
			return toolCall("task33-codex-start", `{"description":"impl","prompt":"do the work","subagent_type":"builder","agent_harness":"codex","model":"gpt-5.6-luna","effort":"max","run_in_background":true}`), nil
		case 1:
			if err := decodeTask33Queued(prior, &codex, task33RuntimeResult{AgentHarness: "codex", Model: "gpt-5.6-luna", Effort: "max"}); err != nil {
				return nil, err
			}
			step++
			return toolCall("task33-claude-start", `{"description":"review","prompt":"check it","subagent_type":"reviewer","agent_harness":"claude-code","model":"sonnet-5","effort":"high","run_in_background":true}`), nil
		case 2:
			if err := decodeTask33Queued(prior, &claude, task33RuntimeResult{AgentHarness: "claude-code", Model: "sonnet-5", Effort: "high"}); err != nil {
				return nil, err
			}
			step++
			return toolCall("task33-status", `{"action":"status"}`), nil
		case 3:
			statusResult = prior
			var status task33StatusResult
			if err := json.Unmarshal([]byte(prior), &status); err != nil {
				return nil, fmt.Errorf("task33 status result: %w", err)
			}
			if !task33StatusHas(status, codex.DelegateID) || !task33StatusHas(status, claude.DelegateID) {
				return nil, fmt.Errorf("task33 status omitted siblings: %s", prior)
			}
			step++
			return toolCall("task33-interrupt", fmt.Sprintf(`{"action":"interrupt","delegate_id":%q}`, codex.DelegateID)), nil
		case 4:
			if !strings.Contains(prior, codex.DelegateID) || !strings.Contains(prior, `"status":"interrupted"`) {
				return nil, fmt.Errorf("task33 interrupt result = %q", prior)
			}
			step++
			return toolCall("task33-wait-claude", fmt.Sprintf(`{"action":"wait","delegate_id":%q,"request_id":%q}`, claude.DelegateID, claude.RequestID)), nil
		case 5:
			if prior != "task33 claude answer" {
				return nil, fmt.Errorf("task33 Claude wait result = %q", prior)
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
	stores, err := openStores(memstore.New())
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
	result := runManagedTurn(t, agent, "run both children")
	if result != "task33 complete" {
		t.Fatalf("parent final = %q", result)
	}
	sessionID := agent.SessionID()
	if err := agent.Close(context.Background()); err != nil {
		t.Fatalf("agent.Close() error = %v", err)
	}

	assertTask33ProviderRequests(t, openAI.capturedRequests(), model.EffortMax, "openai")
	assertTask33ProviderRequests(t, anthropic.capturedRequests(), model.EffortHigh, "anthropic")
	if got, err := os.ReadFile(filepath.Join(workspace, "task33-claude-small-model.receipt")); err != nil || string(got) != "sonnet-5" {
		t.Fatalf("fake Claude small-model receipt = %q, error = %v; want sonnet-5", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(workspace, "task33-codex-cancel.receipt")); err != nil || string(got) != "session/cancel" {
		t.Fatalf("fake ACP cancel receipt = %q, error = %v; want session/cancel", got, err)
	}
	if !strings.Contains(statusResult, codex.DelegateID) || !strings.Contains(statusResult, claude.DelegateID) {
		t.Fatalf("status result = %q, want both delegate ids", statusResult)
	}
	for _, payload := range toolResults {
		if task33SensitivePayloadPattern.MatchString(payload) {
			t.Fatalf("tool result contains secret/path/URL-looking data: %q", payload)
		}
	}

	assertTask33DurableEvents(t, replayACPEvents(t, stores.session, sessionID), codex, claude)
}

var task33SensitivePayloadPattern = regexp.MustCompile(`(?i)(https?://|(?:^|[\s"'])/[^\s"']*|[A-Za-z]:[\\/]|secret|token|api[_-]?key)`)

func decodeTask33Queued(text string, got *task33QueuedResult, want task33RuntimeResult) error {
	if err := json.Unmarshal([]byte(text), got); err != nil {
		return fmt.Errorf("task33 queued result %q: %w", text, err)
	}
	if got.DelegateID == "" || got.RequestID == "" || got.Status != "queued" || got.Runtime == nil {
		return fmt.Errorf("task33 queued result missing full runtime: %q", text)
	}
	if *got.Runtime != want {
		return fmt.Errorf("task33 runtime = %+v, want %+v", *got.Runtime, want)
	}
	return nil
}

func task33StatusHas(status task33StatusResult, delegateID string) bool {
	for _, child := range status.Children {
		if child.DelegateID == delegateID {
			return true
		}
	}
	return false
}

func assertTask33ProviderRequests(t *testing.T, requests []inference.Request, effort model.Effort, provider string) {
	t.Helper()
	if len(requests) == 0 {
		t.Fatalf("fake %s provider saw no requests", provider)
	}
	for _, request := range requests {
		if request.Override == nil {
			t.Fatalf("fake %s request has no ingress sampling override", provider)
		}
		if request.Override.Effort != effort {
			t.Errorf("fake %s effective override effort = %q, want %q", provider, request.Override.Effort, effort)
		}
	}
}

func assertTask33DurableEvents(t *testing.T, events []event.Event, codex, claude task33QueuedResult) {
	t.Helper()
	want := map[identity.AgentName]event.AgentRuntime{
		builder.Name: {
			Harness: "codex", Profile: "acp/codex", CredentialMode: "gateway-backed",
			ModelAlias: "gpt-5.6-luna@max",
		},
		reviewer.Name: {
			Harness: "claude-code", Profile: "acp/claude-code", CredentialMode: "gateway-backed",
			ModelAlias: "sonnet-5@high", SmallModelAlias: "sonnet-5",
		},
	}
	ids := map[identity.AgentName]uuid.UUID{}
	bound := make(map[identity.AgentName]string)
	interrupted := make(map[uuid.UUID]bool)
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
		case event.TurnInterrupted:
			interrupted[ev.LoopID] = true
		}
	}
	if len(ids) != 2 {
		t.Fatalf("durable ACP LoopStarted identities = %v, want builder and reviewer", ids)
	}
	if bound[builder.Name] != "task33-codex-session" || bound[reviewer.Name] != "task33-claude-code-session" {
		t.Fatalf("durable ACP session bindings = %v", bound)
	}
	if !interrupted[ids[builder.Name]] {
		t.Fatalf("codex child loop %s has no durable TurnInterrupted", ids[builder.Name])
	}
	if interrupted[ids[reviewer.Name]] {
		t.Fatalf("Claude child loop %s was interrupted", ids[reviewer.Name])
	}
	if codex.DelegateID == "" || claude.DelegateID == "" {
		t.Fatal("queued delegate handles were empty")
	}
}
