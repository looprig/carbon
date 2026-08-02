package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
	foreignbackend "github.com/looprig/foreignloops/backend"
	"github.com/looprig/foreignloops/driver"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/foreign"
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

type task33ACPProbe struct {
	mu          sync.Mutex
	runtimes    []loop.RuntimeIdentity
	smallModels []string
	cancels     []string
}

func (p *task33ACPProbe) recordRuntime(runtime loop.RuntimeIdentity) {
	p.mu.Lock()
	p.runtimes = append(p.runtimes, runtime)
	p.mu.Unlock()
}

func (p *task33ACPProbe) recordSessionCancel(harness loop.AgentHarnessName) {
	p.mu.Lock()
	p.cancels = append(p.cancels, string(harness)+":session/cancel")
	p.mu.Unlock()
}

func (p *task33ACPProbe) recordSmallModel(alias string) {
	p.mu.Lock()
	p.smallModels = append(p.smallModels, alias)
	p.mu.Unlock()
}

func (p *task33ACPProbe) snapshot() (runtimes []loop.RuntimeIdentity, smallModels, cancels []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]loop.RuntimeIdentity(nil), p.runtimes...), append([]string(nil), p.smallModels...), append([]string(nil), p.cancels...)
}

// task33ACPStream is the normalized stream emitted by the fake ACP process.
// Codex remains in-flight until its turn context is cancelled; Claude emits a
// complete turn immediately. The backend package then supplies the real
// TurnStarted/TurnDone/TurnInterrupted and session-binding event lifecycle.
type task33ACPStream struct {
	events chan driver.Event
	stop   chan struct{}
	done   chan struct{}
	once   sync.Once
}

func newTask33ACPStream(ctx context.Context, agent *task33ACPAgent, turn driver.Turn, complete bool) *task33ACPStream {
	stream := &task33ACPStream{
		events: make(chan driver.Event, 2),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	go func() {
		defer close(stream.done)
		defer close(stream.events)
		select {
		case stream.events <- driver.Event{Kind: driver.KindInit, SessionID: agent.sessionID}:
		case <-ctx.Done():
			return
		case <-stream.stop:
			return
		}
		if complete {
			select {
			case stream.events <- driver.Event{Kind: driver.KindTerminalOK, Message: task33AIMessage("task33 claude answer")}:
			case <-ctx.Done():
				return
			case <-stream.stop:
				return
			}
			return
		}
		select {
		case <-ctx.Done():
			// The fake ACP process receives the backend cancellation as its
			// session/cancel protocol action.
			agent.cancelSession()
		case <-stream.stop:
		}
	}()
	return stream
}

func (s *task33ACPStream) Events() <-chan driver.Event { return s.events }

func (*task33ACPStream) History() (driver.History, error) {
	return driver.History{Available: false}, nil
}

func (s *task33ACPStream) Close() error {
	s.once.Do(func() {
		close(s.stop)
		<-s.done
	})
	return nil
}

type task33ACPAgent struct {
	harness    loop.AgentHarnessName
	sessionID  string
	modelAlias string
	smallModel string
	binding    launch.ProxyBinding
	probe      *task33ACPProbe
	complete   bool
}

func (a *task33ACPAgent) Spawn(ctx context.Context, turn driver.Turn) (driver.Stream, error) {
	if a.harness == "claude-code" {
		if a.smallModel == "" {
			return nil, fmt.Errorf("task33: fake Claude received no small-model selection")
		}
		// This is the fake ACP process's model-selection receipt. The real
		// Claude adapter applies the same value during session setup, before
		// the first prompt is sent.
		a.probe.recordSmallModel(a.smallModel)
	}
	if err := a.promptGateway(ctx, turn); err != nil {
		return nil, err
	}
	return newTask33ACPStream(ctx, a, turn, a.complete), nil
}

func (*task33ACPAgent) Close() error { return nil }

func (a *task33ACPAgent) cancelSession() {
	a.probe.recordSessionCancel(a.harness)
}

func (a *task33ACPAgent) promptGateway(ctx context.Context, turn driver.Turn) error {
	var path string
	var body string
	switch a.harness {
	case "codex":
		path = "/v1/responses"
		body = fmt.Sprintf(`{"model":%q,"reasoning":{"effort":"low"},"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":%q}]}],"max_output_tokens":16}`, a.modelAlias, firstTask33Text(turn.Input))
	case "claude-code":
		path = "/v1/messages"
		body = fmt.Sprintf(`{"model":%q,"output_config":{"effort":"low"},"max_tokens":16,"messages":[{"role":"user","content":[{"type":"text","text":%q}]}]}`, a.modelAlias, firstTask33Text(turn.Input))
	default:
		return fmt.Errorf("task33: unsupported fake harness")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.binding.BaseURL+path, bytes.NewBufferString(body))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+a.binding.Token)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("task33: fake gateway request returned status %d", response.StatusCode)
	}
	return nil
}

func firstTask33Text(blocks []content.Block) string {
	for _, block := range blocks {
		if text, ok := block.(*content.TextBlock); ok {
			return text.Text
		}
	}
	return "task33"
}

func task33AIMessage(text string) *content.AIMessage {
	return &content.AIMessage{Message: content.Message{
		Role:   content.RoleAssistant,
		Blocks: []content.Block{&content.TextBlock{Text: text}},
	}}
}

type task33ACPFactory struct {
	catalog   loop.RuntimeCatalog
	compiled  ACPCompiledCatalog
	workspace string
	probe     *task33ACPProbe
}

func (f *task33ACPFactory) live(
	loopCtx context.Context,
	sessionID, loopID uuid.UUID,
	parent loop.Provenance,
	pub foreign.EventPublisher,
	cfg loop.BoundDefinition,
	idGen func() (uuid.UUID, error),
	fac *event.Factory,
) (loop.Backend, string, error) {
	return f.build(loopCtx, sessionID, loopID, parent, pub, cfg, idGen, fac, "")
}

func (f *task33ACPFactory) restored(
	loopCtx context.Context,
	sessionID, loopID uuid.UUID,
	parent loop.Provenance,
	pub foreign.EventPublisher,
	cfg loop.BoundDefinition,
	idGen func() (uuid.UUID, error),
	fac *event.Factory,
	seed foreign.RestoredForeign,
) (loop.Backend, error) {
	backend, _, err := f.build(loopCtx, sessionID, loopID, parent, pub, cfg, idGen, fac, seed.ForeignSID)
	return backend, err
}

func (f *task33ACPFactory) build(
	loopCtx context.Context,
	sessionID, loopID uuid.UUID,
	parent loop.Provenance,
	pub foreign.EventPublisher,
	cfg loop.BoundDefinition,
	idGen func() (uuid.UUID, error),
	fac *event.Factory,
	seedSID string,
) (loop.Backend, string, error) {
	runtime := cfg.RuntimeIdentity()
	harness := loop.AgentHarnessName(strings.TrimPrefix(string(runtime.Profile), "acp/"))
	resolved, err := f.catalog.ResolveTargetAlias(cfg.Name(), harness, runtime.ModelAlias, runtime.Effort)
	if err != nil {
		return nil, "", err
	}
	if resolved.Credential != loop.CredentialGatewayBacked {
		return nil, "", fmt.Errorf("task33: fake ACP expected gateway-backed runtime")
	}
	ownedGateway, err := NewACPGateway(loopCtx, f.compiled, resolved)
	if err != nil {
		return nil, "", err
	}
	smallAlias := ""
	if harness == "claude-code" {
		_, smallAlias, err = acpChildModelAliases(f.compiled, cfg.Name(), harness, resolved)
		if err != nil {
			_ = ownedGateway.Close(context.Background())
			return nil, "", err
		}
	}
	f.probe.recordRuntime(runtime)
	sessionIDValue := seedSID
	if sessionIDValue == "" {
		sessionIDValue = "task33-" + string(harness) + "-session"
	}
	agent := &task33ACPAgent{
		harness:    harness,
		sessionID:  sessionIDValue,
		modelAlias: string(resolved.TargetAlias),
		smallModel: smallAlias,
		binding:    ownedGateway.Binding(),
		probe:      f.probe,
		complete:   harness == "claude-code",
	}
	posture, err := acpPostureFor(string(cfg.Name()))
	if err != nil {
		_ = ownedGateway.Close(context.Background())
		return nil, "", err
	}
	backendState, _, err := foreignbackend.New(
		loopCtx,
		sessionID,
		loopID,
		parent,
		pub,
		cfg,
		foreignbackend.Config{
			Agent:   agent,
			Cwd:     f.workspace,
			Posture: task33PermissionPosture(posture),
			SIDMode: foreignbackend.SIDLateBound,
		},
		idGen,
		fac,
	)
	if err != nil {
		_ = ownedGateway.Close(context.Background())
		return nil, "", err
	}
	return wrapACPGatewayBackend(backendState, ownedGateway), sessionIDValue, nil
}

func task33PermissionPosture(posture driver.Posture) driver.PermissionPosture {
	if posture == driver.PostureWorkspaceWrite {
		return driver.PostureAcceptEdits
	}
	return driver.PostureDefault
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
	probe := &task33ACPProbe{}
	workspace := t.TempDir()
	factory := &task33ACPFactory{catalog: compiled.RuntimeCatalog, compiled: compiled, workspace: workspace, probe: probe}
	var registry foreign.BuilderRegistry
	if err := registry.Register("acp/codex", factory.live, factory.restored); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register("acp/claude-code", factory.live, factory.restored); err != nil {
		t.Fatal(err)
	}
	composition := &ACPComposition{
		Catalog:  compiled,
		Registry: &registry,
		Live:     dispatchACPBuilder(&registry),
		Restored: dispatchACPRestoredBuilder(&registry),
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
	runtimes, smallModels, cancels := probe.snapshot()
	if !task33ContainsString(smallModels, "sonnet-5") {
		t.Fatalf("fake Claude small-model selections = %v, want sonnet-5", smallModels)
	}
	if !task33ContainsString(cancels, "codex:session/cancel") {
		t.Fatalf("fake ACP cancel calls = %v, want codex session/cancel", cancels)
	}
	if len(runtimes) != 2 {
		t.Fatalf("fake ACP runtime constructions = %d, want 2", len(runtimes))
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

func task33ContainsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

var _ foreign.Builder = (*task33ACPFactory)(nil).live
var _ foreign.RestoredBuilder = (*task33ACPFactory)(nil).restored
var _ driver.Stream = (*task33ACPStream)(nil)
var _ loop.Backend = (*foreignbackend.Loop)(nil)
var _ command.Command = command.Shutdown{}
