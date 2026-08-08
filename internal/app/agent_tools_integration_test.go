package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/looprig/coderig/internal/catalog/generic"
	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/event"
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
	if _, err := compiled.RuntimeCatalog.ResolveWithExplicitSource(generic.Name, looprigRuntimeHarness, loop.RuntimeSourceNative, "alpha", model.EffortLow, true); err != nil {
		t.Fatalf("compiled native alpha/low selection: %v", err)
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
		if !requestHasRole(req, generic.Name) {
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
			return startAgentCall("native-start", `{"agent_type":"generic","instructions":"implement","model":"alpha","effort":"low"}`), nil
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
		if started, ok := raw.(event.LoopStarted); ok && started.AgentName == generic.Name && started.AgentRuntime != nil {
			childRuntime = started.AgentRuntime
		}
	}
	if childRuntime == nil || childRuntime.Harness != "looprig" || childRuntime.Source != string(loop.RuntimeSourceNative) || childRuntime.ModelAlias != "alpha" {
		t.Fatalf("native child runtime = %+v, want looprig/native alpha", childRuntime)
	}
}

func TestAgentToolsRejectIncompatibleNativeModelEffort(t *testing.T) {
	client := &managedScript{}
	native := &recordingAgentRuntimeClient{}
	catalog := agentToolRuntimeCatalog(t, native)
	var result string
	step := 0
	client.fn = func(_ context.Context, req inference.Request) ([]content.Chunk, error) {
		if !requestHasRole(req, generic.Name) {
			return finalText("unexpected child"), nil
		}
		if step == 0 {
			step++
			return startAgentCall("invalid-native", `{"agent_type":"generic","instructions":"reject","model":"beta","effort":"low"}`), nil
		}
		result = lastToolText(req)
		return finalText("invalid selection handled"), nil
	}
	agent := newTestAgent(t, agentRuntimeRouter(t, client, native), Config{RuntimeCatalog: catalog})
	if got := runManagedTurn(t, agent, "reject an incompatible model effort"); got != "invalid selection handled" {
		t.Fatalf("final = %q", got)
	}
	if !strings.Contains(result, "unknown_runtime") {
		t.Fatalf("incompatible selection result = %q, want bounded unknown_runtime error", result)
	}
}
