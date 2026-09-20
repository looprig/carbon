package app

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/looprig/acp/protocol"
	"github.com/looprig/carbon/internal/catalog/carbon"
	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/foreign"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/inference"
	model "github.com/looprig/inference/model"
	"github.com/looprig/inference/stream"
)

func agentRuntimeModel(provider model.ProviderName, format model.APIFormat, name string) model.Model {
	selected := model.CustomModel(provider, format, "", name, model.WithTools(), model.WithThinking())
	selected.Limits = model.ContextLimits{WindowTokens: 128_000}
	return selected
}

func agentToolRuntimeCatalog(t *testing.T, client inference.Client) loop.RuntimeCatalog {
	t.Helper()
	compiled, err := CompileAgentRuntimeCatalog(AgentRuntimeCatalogInput{
		GatewayTargets: []ACPGatewaySource{
			{
				Alias: "alpha", Description: "Fast implementation model.", Client: client,
				Model:         agentRuntimeModel("openai", model.APIFormatOpenAIResponses, "alpha-model"),
				DefaultEffort: model.EffortLow,
				Efforts:       []model.Effort{model.EffortLow, model.EffortMedium},
			},
			{
				Alias: "beta", Description: "Deep analysis model.", Client: client,
				Model:         agentRuntimeModel("anthropic", model.APIFormatAnthropic, "beta-model"),
				DefaultEffort: model.EffortHigh,
				Efforts:       []model.Effort{model.EffortHigh, model.EffortMax},
			},
		},
		PrimerTarget: runtimeCatalogPrimer(),
	})
	if err != nil {
		t.Fatalf("CompileAgentRuntimeCatalog() error = %v", err)
	}
	if _, err := compiled.RuntimeCatalog.ResolveWithExplicitSource(carbon.Name, looprigRuntimeHarness, loop.RuntimeSourceNative, "alpha", model.EffortLow, true); err != nil {
		t.Fatalf("compiled native alpha/low selection: %v", err)
	}
	return compiled.RuntimeCatalog
}

func fullAgentToolRuntimeCatalog(t *testing.T, client inference.Client) loop.RuntimeCatalog {
	t.Helper()
	compiled, err := CompileAgentRuntimeCatalog(AgentRuntimeCatalogInput{
		GatewayTargets: []ACPGatewaySource{
			{
				Alias: "alpha", Description: "Codex-capable implementation model.", Client: client,
				Model:         agentRuntimeModel("openai", model.APIFormatOpenAIResponses, "alpha-model"),
				DefaultEffort: model.EffortLow,
				Efforts:       []model.Effort{model.EffortLow, model.EffortMedium},
			},
			{
				Alias: "beta", Description: "Claude-capable analysis model.", Client: client,
				Model:         agentRuntimeModel("anthropic", model.APIFormatAnthropic, "beta-model"),
				DefaultEffort: model.EffortHigh,
				Efforts:       []model.Effort{model.EffortHigh, model.EffortMax},
			},
		},
		ClaudeSmall:  "alpha",
		PrimerTarget: runtimeCatalogPrimer(),
	})
	if err != nil {
		t.Fatalf("CompileAgentRuntimeCatalog() full catalog error = %v", err)
	}
	entries := compiled.RuntimeCatalog.EntriesFor(carbon.Name)
	if len(entries) != 3 {
		t.Fatalf("full Carbon runtime catalog entries = %d, want native default plus Codex and Claude ACP alternatives: %#v", len(entries), entries)
	}
	seen := map[loop.AgentHarnessName]bool{}
	for _, entry := range entries {
		seen[entry.AgentHarness] = true
		if entry.AgentHarness != looprigRuntimeHarness && entry.Default {
			t.Fatalf("ACP runtime entry unexpectedly default: %#v", entry)
		}
	}
	for _, harness := range []loop.AgentHarnessName{looprigRuntimeHarness, "codex", "claude-code"} {
		if !seen[harness] {
			t.Fatalf("full Carbon runtime catalog omitted %q: %#v", harness, entries)
		}
	}
	return compiled.RuntimeCatalog
}

type recordingAgentRuntimeClient struct {
	mu       sync.Mutex
	requests []inference.Request
}

func (c *recordingAgentRuntimeClient) record(req inference.Request) {
	c.mu.Lock()
	c.requests = append(c.requests, req)
	c.mu.Unlock()
}

func (c *recordingAgentRuntimeClient) Invoke(_ context.Context, req inference.Request) (*inference.Response, error) {
	c.record(req)
	return &inference.Response{Message: &content.AIMessage{Message: content.Message{
		Role: content.RoleAssistant, Blocks: []content.Block{&content.TextBlock{Text: "native child complete"}},
	}}}, nil
}

func (c *recordingAgentRuntimeClient) Stream(_ context.Context, req inference.Request) (*stream.StreamReader[content.Chunk], error) {
	c.record(req)
	chunks := finalText("native child complete")
	index := 0
	return stream.NewStreamReader(func() (content.Chunk, error) {
		if index == len(chunks) {
			return nil, io.EOF
		}
		chunk := chunks[index]
		index++
		return chunk, nil
	}, nil), nil
}

func recordingAgentRuntimeRequests(client *recordingAgentRuntimeClient) []inference.Request {
	client.mu.Lock()
	defer client.mu.Unlock()
	return append([]inference.Request(nil), client.requests...)
}

func agentRuntimeRouter(t *testing.T, parent, native inference.Client) inference.Client {
	t.Helper()
	alpha := agentRuntimeModel("openai", model.APIFormatOpenAIResponses, "alpha-model")
	beta := agentRuntimeModel("anthropic", model.APIFormatAnthropic, "beta-model")
	router, err := newModelRoutingClient([]modelBinding{
		{Model: testModel(), Client: parent},
		{Model: alpha, Client: native},
		{Model: beta, Client: native},
	})
	if err != nil {
		t.Fatalf("newModelRoutingClient() error = %v", err)
	}
	return router
}

func findInferenceTool(t *testing.T, req inference.Request, name string) inference.Tool {
	t.Helper()
	for _, info := range req.Tools {
		if info.Name == name {
			return info
		}
	}
	t.Fatalf("inference request omitted %s", name)
	return inference.Tool{}
}

func TestAgentToolsNoACPProductionSurfaceAndNativeSelection(t *testing.T) {
	client := &managedScript{}
	native := &recordingAgentRuntimeClient{}
	catalog := agentToolRuntimeCatalog(t, native)
	var parentSteps int
	client.fn = func(_ context.Context, req inference.Request) ([]content.Chunk, error) {
		if !requestHasRole(req, carbon.Name) {
			return nil, fmt.Errorf("unexpected role in no-ACP integration request")
		}
		if parentSteps == 0 {
			parentSteps++
			want := []string{"ListAgents", "MessageAgent", "StartAgent", "StopAgent"}
			got := make([]string, 0, len(want))
			for _, name := range want {
				if slices.Contains(toolNamesFromRequest(req), name) {
					got = append(got, name)
				}
			}
			slices.Sort(got)
			if !slices.Equal(got, want) {
				return nil, fmt.Errorf("agent tool bundle = %v, want %v", got, want)
			}
			if slices.Contains(toolNamesFromRequest(req), "Subagent") || slices.Contains(toolNamesFromRequest(req), "WaitAgent") {
				return nil, fmt.Errorf("legacy collaboration tool advertised: %v", toolNamesFromRequest(req))
			}
			start := findInferenceTool(t, req, "StartAgent")
			for _, fragment := range []string{"<available_agents>", "<available_agent_runtimes>", "alpha", "Fast implementation model.", "beta", "Deep analysis model.", "looprig"} {
				if !strings.Contains(start.Description, fragment) {
					return nil, fmt.Errorf("StartAgent description omitted %q: %q", fragment, start.Description)
				}
			}
			for _, fragment := range []string{"action", "subagent_type", "run_in_background", "delegate_id", "request_id"} {
				if strings.Contains(string(start.Schema), fragment) {
					return nil, fmt.Errorf("StartAgent schema contains removed field %q: %s", fragment, start.Schema)
				}
			}
			if !strings.Contains(string(start.Schema), "alpha") || !strings.Contains(string(start.Schema), "beta") {
				return nil, fmt.Errorf("StartAgent schema omitted ordinary model choices: %s", start.Schema)
			}
			if strings.Contains(req.System, "Fast implementation model.") || strings.Contains(req.System, "Deep analysis model.") {
				return nil, fmt.Errorf("system prompt leaked runtime model descriptions: %q", req.System)
			}
			return startAgentCall("native-start", `{"agent_type":"carbon","instructions":"implement","model":"alpha","effort":"low"}`), nil
		}
		var result struct {
			Response string `json:"response"`
		}
		if err := json.Unmarshal([]byte(lastToolText(req)), &result); err != nil {
			return nil, fmt.Errorf("native StartAgent result %q: %w (native requests=%#v)", lastToolText(req), err, recordingAgentRuntimeRequests(native))
		}
		if result.Response != "native child complete" {
			return nil, fmt.Errorf("native StartAgent response = %q", result.Response)
		}
		return finalText("native selection complete"), nil
	}

	agent := newTestAgent(t, agentRuntimeRouter(t, client, native), Config{RuntimeCatalog: catalog})
	got, observed := runManagedTurnObserved(t, agent, "use the native model")
	if got != "native selection complete" {
		for _, raw := range observed {
			if failed, ok := raw.(event.TurnFailed); ok {
				t.Logf("turn failure: %v", failed.Err)
			}
		}
		t.Fatalf("native integration final = %q", got)
	}
	requests := recordingAgentRuntimeRequests(native)
	if len(requests) != 1 || requests[0].Model.Name != "alpha-model" || requests[0].Model.Sampling.Effort != model.EffortLow {
		t.Fatalf("native child requests = %#v, want alpha-model/low", requests)
	}
	var childRuntime *event.AgentRuntime
	for _, raw := range observed {
		if started, ok := raw.(event.LoopStarted); ok && started.AgentName == carbon.Name && started.AgentRuntime != nil {
			childRuntime = started.AgentRuntime
		}
	}
	if childRuntime == nil || childRuntime.Harness != "looprig" || childRuntime.Source != string(loop.RuntimeSourceNative) || childRuntime.ModelAlias != "alpha" {
		t.Fatalf("native child runtime = %+v, want looprig/native alpha", childRuntime)
	}
}

func TestAgentToolsNativeACPSchemaUsesConfiguredModelEfforts(t *testing.T) {
	client := &managedScript{}
	native := &recordingAgentRuntimeClient{}
	compiled, err := CompileAgentRuntimeCatalog(AgentRuntimeCatalogInput{
		NativeACP: map[string]ACPNativeProfile{
			"codex": {
				Harness: "codex", Enabled: true,
				ModelOptions: []ACPNativeModelOption{{
					Alias: "native-model", Model: "native-model",
					Efforts: []model.Effort{model.EffortMedium, model.EffortHigh}, DefaultEffort: model.EffortHigh,
				}},
			},
		},
		PrimerTarget: runtimeCatalogPrimer(),
	})
	if err != nil {
		t.Fatalf("CompileAgentRuntimeCatalog() error = %v", err)
	}
	client.fn = func(_ context.Context, req inference.Request) ([]content.Chunk, error) {
		start := findInferenceTool(t, req, "StartAgent")
		var schema any
		if err := json.Unmarshal(start.Schema, &schema); err != nil {
			return nil, fmt.Errorf("decode StartAgent schema: %w", err)
		}
		efforts, ok := findNativeModelEfforts(schema, "native-model")
		if !ok {
			return nil, fmt.Errorf("StartAgent schema omitted native model effort branch: %s", start.Schema)
		}
		if !slices.Equal(efforts, []string{"medium", "high"}) {
			return nil, fmt.Errorf("native model efforts = %v, want [medium high]", efforts)
		}
		return finalText("schema verified"), nil
	}

	agent := newTestAgent(t, agentRuntimeRouter(t, client, native), Config{RuntimeCatalog: compiled.RuntimeCatalog})
	if got := runManagedTurn(t, agent, "inspect the native runtime schema"); got != "schema verified" {
		t.Fatalf("schema test final = %q", got)
	}
}

func TestAgentToolsNativeACPSchemaUsesFriendlyNativeAliases(t *testing.T) {
	client := &managedScript{}
	native := &recordingAgentRuntimeClient{}
	compiled, err := CompileAgentRuntimeCatalog(AgentRuntimeCatalogInput{
		NativeACP: map[string]ACPNativeProfile{
			"codex": {
				Harness: "codex", Enabled: true,
				ModelOptions: []ACPNativeModelOption{{
					Alias: "friendly-codex", Model: "actual-codex",
					Efforts: []model.Effort{model.EffortHigh}, DefaultEffort: model.EffortHigh,
				}},
			},
			"claude-code": {
				Harness: "claude-code", Enabled: true,
				ModelOptions: []ACPNativeModelOption{{
					Alias: "friendly-claude", Model: "actual-claude",
					Efforts: []model.Effort{model.EffortHigh}, DefaultEffort: model.EffortHigh,
				}},
			},
		},
		PrimerTarget: runtimeCatalogPrimer(),
	})
	if err != nil {
		t.Fatalf("CompileAgentRuntimeCatalog() error = %v", err)
	}
	client.fn = func(_ context.Context, req inference.Request) ([]content.Chunk, error) {
		start := findInferenceTool(t, req, "StartAgent")
		schema := string(start.Schema)
		for _, alias := range []string{"friendly-codex", "friendly-claude"} {
			if !strings.Contains(schema, alias) {
				return nil, fmt.Errorf("StartAgent schema omitted friendly alias %q", alias)
			}
		}
		for _, adapterID := range []string{"actual-codex", "actual-claude"} {
			if strings.Contains(schema, adapterID) {
				return nil, fmt.Errorf("StartAgent schema leaked adapter model ID %q", adapterID)
			}
		}
		return finalText("schema verified"), nil
	}

	agent := newTestAgent(t, agentRuntimeRouter(t, client, native), Config{RuntimeCatalog: compiled.RuntimeCatalog})
	if got := runManagedTurn(t, agent, "inspect friendly native aliases"); got != "schema verified" {
		t.Fatalf("friendly alias schema test final = %q", got)
	}
}

func findNativeModelEfforts(value any, alias string) ([]string, bool) {
	object, ok := value.(map[string]any)
	if !ok {
		if list, ok := value.([]any); ok {
			for _, item := range list {
				if efforts, found := findNativeModelEfforts(item, alias); found {
					return efforts, true
				}
			}
		}
		return nil, false
	}
	if properties, ok := object["properties"].(map[string]any); ok {
		modelProperty, modelOK := properties["model"].(map[string]any)
		effortProperty, effortOK := properties["effort"].(map[string]any)
		modelMatches := modelProperty["const"] == alias
		if values, ok := modelProperty["enum"].([]any); ok && len(values) == 1 && values[0] == alias {
			modelMatches = true
		}
		if modelOK && effortOK && modelMatches {
			values, ok := effortProperty["enum"].([]any)
			if !ok {
				return nil, false
			}
			efforts := make([]string, 0, len(values))
			for _, value := range values {
				text, ok := value.(string)
				if !ok {
					return nil, false
				}
				efforts = append(efforts, text)
			}
			return efforts, true
		}
	}
	for _, child := range object {
		if efforts, found := findNativeModelEfforts(child, alias); found {
			return efforts, true
		}
	}
	return nil, false
}

func TestAssembledStartAgentPlainPayloadUsesLoopRigNativeDefault(t *testing.T) {
	client := &managedScript{}
	native := &recordingAgentRuntimeClient{}
	catalog := fullAgentToolRuntimeCatalog(t, native)
	step := 0
	client.fn = func(_ context.Context, req inference.Request) ([]content.Chunk, error) {
		if !requestHasRole(req, carbon.Name) {
			return nil, fmt.Errorf("unexpected role in plain StartAgent integration request")
		}
		if step == 0 {
			step++
			const plainPayload = `{"agent_type":"carbon","instructions":"implement"}`
			var fields map[string]json.RawMessage
			if err := json.Unmarshal([]byte(plainPayload), &fields); err != nil {
				return nil, fmt.Errorf("plain StartAgent payload is invalid JSON: %w", err)
			}
			if len(fields) != 2 {
				return nil, fmt.Errorf("plain StartAgent payload fields = %v, want exactly ordinary required fields", fields)
			}
			return startAgentCall("plain-default-start", plainPayload), nil
		}
		var result struct {
			Response string `json:"response"`
		}
		if err := json.Unmarshal([]byte(lastToolText(req)), &result); err != nil {
			return nil, fmt.Errorf("plain StartAgent result %q: %w", lastToolText(req), err)
		}
		if result.Response != "native child complete" {
			return nil, fmt.Errorf("plain StartAgent response = %q", result.Response)
		}
		return finalText("plain default complete"), nil
	}

	agent := newTestAgent(t, agentRuntimeRouter(t, client, native), Config{RuntimeCatalog: catalog})
	got, observed := runManagedTurnObserved(t, agent, "use the ordinary default runtime")
	if got != "plain default complete" {
		t.Fatalf("plain default final = %q", got)
	}
	requests := recordingAgentRuntimeRequests(native)
	if len(requests) != 1 || requests[0].Model.Name != "alpha-model" {
		t.Fatalf("plain default child requests = %#v, want looprig/native alpha-model", requests)
	}
	var childRuntime *event.AgentRuntime
	for _, raw := range observed {
		if started, ok := raw.(event.LoopStarted); ok && started.AgentName == carbon.Name && started.AgentRuntime != nil {
			childRuntime = started.AgentRuntime
		}
	}
	if childRuntime == nil || childRuntime.Harness != "looprig" || childRuntime.Source != string(loop.RuntimeSourceNative) || childRuntime.Profile != string(looprigRuntimeProfile) || childRuntime.SelectionKind != string(loop.RuntimeSelectionExplicit) || childRuntime.ModelAlias != "alpha" {
		t.Fatalf("plain default child runtime = %+v, want looprig/native explicit alpha", childRuntime)
	}
}

func TestAgentToolsRejectIncompatibleNativeModelEffort(t *testing.T) {
	client := &managedScript{}
	native := &recordingAgentRuntimeClient{}
	catalog := agentToolRuntimeCatalog(t, native)
	var result string
	step := 0
	client.fn = func(_ context.Context, req inference.Request) ([]content.Chunk, error) {
		if !requestHasRole(req, carbon.Name) {
			return finalText("unexpected child"), nil
		}
		if step == 0 {
			step++
			return startAgentCall("invalid-native", `{"agent_type":"carbon","instructions":"reject","model":"beta","effort":"low"}`), nil
		}
		result = lastToolText(req)
		return finalText("invalid selection handled"), nil
	}
	agent := newTestAgent(t, agentRuntimeRouter(t, client, native), Config{RuntimeCatalog: catalog})
	if got := runManagedTurn(t, agent, "reject an incompatible model effort"); got != "invalid selection handled" {
		t.Fatalf("final = %q", got)
	}
	if !strings.Contains(result, "tool preparation failed") || !strings.Contains(result, `effort "low" is unavailable for model "beta"`) {
		t.Fatalf("incompatible selection result = %q, want bounded unavailable-effort preparation error", result)
	}
}

func TestAssembledStartAgentACPFailureUsesSafeDetail(t *testing.T) {
	const safeDetail = "ACP error -32000: usage limit reached; resets at 3:00 PM"
	const googleAPIKey = "AIzaSyA-0123456789_AbCdEfGhIjKlMnOpQrStUv"
	probe, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		if strings.Contains(err.Error(), "operation not permitted") || strings.Contains(err.Error(), "listen tcp") {
			t.Skipf("loopback listeners unavailable in this sandbox: %v", err)
		}
		t.Fatalf("probe loopback listener: %v", err)
	}
	_ = probe.Close()
	client := &managedScript{}
	native := &recordingAgentRuntimeClient{}
	compiled, err := CompileAgentRuntimeCatalog(AgentRuntimeCatalogInput{
		NativeACP: map[string]ACPNativeProfile{"codex": {
			Harness: "codex", Enabled: true,
			ModelOptions: []ACPNativeModelOption{{
				Alias: "native-codex", Model: "native-codex",
				Efforts: []model.Effort{model.EffortHigh}, DefaultEffort: model.EffortHigh,
			}},
		}},
		PrimerTarget: runtimeCatalogPrimer(),
	})
	if err != nil {
		t.Fatalf("CompileAgentRuntimeCatalog(): %v", err)
	}
	protocolCause := fmt.Errorf("stderr path=/private/acp token=secret")
	protocolFailure := protocol.AuthRequired("usage limit reached; resets at 3:00 PM; credential "+googleAPIKey, protocolCause).WithData(map[string]string{
		"path": "/private/acp", "url": "https://provider.invalid/token", "token": "secret",
	})
	composition := &ACPComposition{
		Catalog: compiled,
		Live: func(
			context.Context,
			uuid.UUID, uuid.UUID, loop.Provenance, foreign.EventPublisher, loop.BoundDefinition,
			func() (uuid.UUID, error), *event.Factory,
		) (loop.Backend, string, error) {
			return nil, "", boundedACPChildError(protocolFailure)
		},
		Restored: func(
			context.Context,
			uuid.UUID, uuid.UUID, loop.Provenance, foreign.EventPublisher, loop.BoundDefinition,
			func() (uuid.UUID, error), *event.Factory, foreign.RestoredForeign,
		) (loop.Backend, error) {
			return nil, boundedACPChildError(protocolFailure)
		},
	}
	step := 0
	client.fn = func(_ context.Context, req inference.Request) ([]content.Chunk, error) {
		if !requestHasRole(req, carbon.Name) {
			return nil, fmt.Errorf("unexpected role in ACP failure integration request")
		}
		if step == 0 {
			step++
			return startAgentCall("acp-failure", `{"agent_type":"carbon","instructions":"run ACP","agent_harness":"codex","model":"native-codex","effort":"high"}`), nil
		}
		got := lastToolText(req)
		if strings.Contains(got, googleAPIKey) {
			return nil, fmt.Errorf("StartAgent ACP failure result leaked bare credential")
		}
		wantPrefix := "StartAgent failed: " + safeDetail
		if !strings.HasPrefix(got, wantPrefix) || !strings.Contains(got, "[REDACTED]") {
			return nil, fmt.Errorf("StartAgent ACP failure result %q did not preserve safe detail and redact credential", got)
		}
		return finalText("ACP failure formatted safely"), nil
	}
	agent := newTestAgent(t, agentRuntimeRouter(t, client, native), Config{
		ACPChildren:    composition,
		RuntimeCatalog: compiled.RuntimeCatalog,
	})
	if got := runManagedTurn(t, agent, "start the ACP child"); got != "ACP failure formatted safely" {
		t.Fatalf("final = %q", got)
	}
}

// ---- R1.2 step 7: the capture/truncation roster guard ----------------------

// resultReaderToolName is the tool a truncated or captured tool result would tell
// the model to call in order to read the rest. It is named here as a STRING rather
// than imported because no released module ships it: see the test below.
const resultReaderToolName = "read_tool_result"

// TestCarbonAdvertisesNoUnregisteredResultReader is runbook 08 R1.2 step 7 in the
// only shape that is honest against today's released modules.
//
// Step 7 asks for a roster test proving every model-visible capture/truncation
// marker names an actually registered `read_tool_result`, and step 6 asks Carbon to
// register that tool whenever a per-session SessionObjectStore reader is bound.
// NEITHER IS EXECUTABLE: tools v0.12.0 ships no `read_tool_result` definition, and
// neither does any other released module. The tool does not exist, so there is
// nothing to register and no marker that could name it.
//
// What IS assertable is the rule step 6 states from the other side — "a composition
// without object capture does not advertise a broken reader" — and this holds both
// halves of it: the roster registers no such tool, and no production file in this
// package so much as names one. The second half is what makes it non-vacuous: the
// moment somebody writes the marker text without the registration, this fails.
func TestCarbonAdvertisesNoUnregisteredResultReader(t *testing.T) {
	t.Parallel()

	set := mustExecutorSet(t, t.TempDir())
	for _, def := range carbonToolDefinitions(set, nil, nil) {
		if def.Name() == resultReaderToolName {
			t.Fatalf("carbon registers %q; this guard was written for a roster that does not, and must be replaced by the positive test step 7 asks for",
				resultReaderToolName)
		}
	}

	offenders, err := carbonSourceOffenders(resultReaderToolName)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	for _, path := range offenders {
		t.Errorf("%s names %q, which no Carbon session registers; a composition without object capture must not advertise a reader that is not there",
			path, resultReaderToolName)
	}
}

// TestCarbonWiresNoToolResultCapture is the other half of the coupling above, and it
// is a SOURCE assertion on purpose.
//
// The property it holds is an absence, and an absence cannot be observed from a
// built rig: rig.Define exposes no accessor for the capture wiring, so a test that
// tried to assert it from a value would have to assert something else instead and
// would drift from the thing it claims. Scanning for the call is exact.
//
// When this test fails, it is NOT a regression. It means Carbon has gained durable
// tool-result capture, and the correct response is to delete this test and write the
// positive one step 6 and step 7 describe: register the reader whenever the
// per-session object store is bound, and prove every truncation marker names it.
func TestCarbonWiresNoToolResultCapture(t *testing.T) {
	t.Parallel()

	offenders, err := carbonCallOffenders("rig", "WithToolResultCapture")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	for _, path := range offenders {
		t.Errorf("%s wires rig.WithToolResultCapture; Carbon now captures tool results, so it owes a registered %q and the roster test that names it (runbook 08 R1.2 steps 6-7)",
			path, resultReaderToolName)
	}
}

// carbonSourceOffenders reports every non-test .go file in the MODULE that mentions
// needle, as a list of module-relative paths.
//
// THE SCOPE IS THE MODULE AND NOT THIS PACKAGE, and that is the whole difference
// between a tripwire and a decoration. An earlier version read os.ReadDir(".") —
// internal/app alone — and therefore could not see cmd/carbon, which is exactly where
// R1.3 composes the rig and where rig.WithToolResultCapture would most naturally be
// wired. A guard that cannot see the place the thing will be written is a guard that
// stays green through the event it exists to catch.
func carbonSourceOffenders(needle string) ([]string, error) {
	return carbonSourceOffendersUnder(filepath.Join("..", ".."), needle)
}

// carbonSourceOffendersUnder is the scanner proper, taking its root so it can be
// falsified against a tree a test builds. carbonSourceOffenders has no falsifier of
// its own precisely because its root is fixed; this one does.
func carbonSourceOffendersUnder(root, needle string) ([]string, error) {
	var offenders []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			// .git holds packed objects that would match anything; bin holds a
			// compiled binary; docs is prose about the code and not the code.
			switch entry.Name() {
			case ".git", "bin", "docs":
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		source, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(source), needle) {
			offenders = append(offenders, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return offenders, nil
}

// carbonCallOffenders reports every non-test .go file in the module that CALLS
// pkg.name, found in the syntax tree rather than in the text.
//
// A textual scan cannot do this job. The capture guard's own failure message names
// rig.WithToolResultCapture, and so does LaunchScope's doc comment explaining why
// Carbon has no object prefix — so a substring scan reports the explanation of the
// absence as evidence of the presence, which is the most confusing possible false
// positive. Matching a call expression is exact and cannot be tripped by prose.
func carbonCallOffenders(pkg, name string) ([]string, error) {
	return carbonCallOffendersUnder(filepath.Join("..", ".."), pkg, name)
}

// carbonCallOffendersUnder is carbonCallOffenders with its root supplied, so it can
// be falsified against a tree a test builds.
func carbonCallOffendersUnder(root, pkg, name string) ([]string, error) {
	var offenders []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "bin", "docs":
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != name {
				return true
			}
			ident, ok := selector.X.(*ast.Ident)
			if !ok || ident.Name != pkg {
				return true
			}
			offenders = append(offenders, path)
			return true
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return offenders, nil
}

// TestCarbonCallOffendersFindsCallsAndNotProse is the falsifier for the capture
// tripwire, and the prose row is the one that earns it: this very package explains
// in a comment why it does NOT wire rig.WithToolResultCapture, and a scanner that
// counted that as a wiring would report the explanation of an absence as the
// presence.
func TestCarbonCallOffendersFindsCallsAndNotProse(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	write := func(rel, src string) string {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(full, []byte(src), 0o600); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
		return full
	}

	const calls = "package p\n\nvar rig struct{ WithToolResultCapture func() int }\n\nvar _ = rig.WithToolResultCapture()\n"
	const prose = "package p\n\n// This package does not call rig.WithToolResultCapture, deliberately.\nvar _ = 1\n"
	const other = "package p\n\nvar other struct{ WithToolResultCapture func() int }\n\nvar _ = other.WithToolResultCapture()\n"

	offender := write("cmd/carbon/orchestration.go", calls)
	write("internal/app/explanation.go", prose)        // prose is not a wiring
	write("internal/app/other.go", other)              // a different package's function
	write("internal/app/orchestration_test.go", calls) // a test may call it

	got, err := carbonCallOffendersUnder(root, "rig", "WithToolResultCapture")
	if err != nil {
		t.Fatalf("carbonCallOffendersUnder: %v", err)
	}
	if len(got) != 1 || got[0] != offender {
		t.Fatalf("offenders = %v, want exactly [%s]", got, offender)
	}
}

// TestCarbonSourceOffendersScansTheWholeModule is the falsifier the capture tripwire
// did not have.
//
// It pins BOTH properties the guards depend on: the scan reaches a sibling package
// (cmd/carbon is the one that matters), and it ignores _test.go files, since a test
// may legitimately name the thing the guard forbids in production code — this very
// file does.
func TestCarbonSourceOffendersScansTheWholeModule(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	write := func(rel, src string) string {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(full, []byte(src), 0o600); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
		return full
	}

	const mentions = "package p\n\nvar _ = \"needle-xyzzy\"\n"
	const clean = "package p\n\nvar _ = \"something else\"\n"

	// The sibling package is the whole point: this is where R1.3 composes.
	command := write("cmd/carbon/orchestration.go", mentions)
	internal := write("internal/app/wiring.go", mentions)
	write("internal/app/wiring_test.go", mentions) // a test may name it
	write("internal/app/clean.go", clean)
	write("docs/plans/note.go", mentions) // prose tree, skipped

	got, err := carbonSourceOffendersUnder(root, "needle-xyzzy")
	if err != nil {
		t.Fatalf("carbonSourceOffendersUnder: %v", err)
	}
	want := map[string]bool{command: true, internal: true}
	if len(got) != len(want) {
		t.Fatalf("offenders = %v, want exactly %v", got, want)
	}
	for _, path := range got {
		if !want[path] {
			t.Errorf("unexpected offender %s", path)
		}
	}

	// And it must find nothing when there is nothing, or the guards above pass for
	// the wrong reason forever.
	empty, err := carbonSourceOffendersUnder(root, "no-such-needle-here")
	if err != nil {
		t.Fatalf("carbonSourceOffendersUnder: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("offenders for an absent needle = %v, want none", empty)
	}
}
