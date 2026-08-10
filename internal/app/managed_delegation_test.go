package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/looprig/carbon/internal/catalog/carbon"
	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
	stream "github.com/looprig/inference/stream"
	"github.com/looprig/tui"
)

// managedScript is a deterministic provider fake that drives model-facing
// managed-agent tools through the real bound inference request.
type managedScript struct {
	mu sync.Mutex
	fn func(context.Context, inference.Request) ([]content.Chunk, error)
}

func (*managedScript) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	return nil, errors.New("managedScript.Invoke not used")
}

func (s *managedScript) Stream(ctx context.Context, req inference.Request) (*stream.StreamReader[content.Chunk], error) {
	s.mu.Lock()
	chunks, err := s.fn(ctx, req)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	i := 0
	return stream.NewStreamReader(func() (content.Chunk, error) {
		if i == len(chunks) {
			return nil, io.EOF
		}
		chunk := chunks[i]
		i++
		return chunk, nil
	}, nil), nil
}

func startAgentCall(id, input string) []content.Chunk { return namedToolCall(id, "StartAgent", input) }
func messageAgentCall(id, input string) []content.Chunk {
	return namedToolCall(id, "MessageAgent", input)
}

func namedToolCall(id, name, input string) []content.Chunk {
	return []content.Chunk{&content.ToolUseChunk{Index: 0, ID: id, Name: name, InputJSON: input}}
}

func finalText(text string) []content.Chunk { return []content.Chunk{&content.TextChunk{Text: text}} }

func lastToolText(req inference.Request) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		msg, ok := req.Messages[i].(*content.ToolResultMessage)
		if !ok {
			continue
		}
		for _, block := range msg.Blocks {
			if text, ok := block.(*content.TextBlock); ok {
				return text.Text
			}
		}
	}
	return ""
}

type agentHandle struct {
	AgentID string `json:"agent_id"`
	Name    string `json:"name"`
	State   string `json:"state"`
}

func runManagedTurn(t *testing.T, agent tui.Agent, prompt string) string {
	t.Helper()
	text, _ := runManagedTurnObserved(t, agent, prompt)
	return text
}

func runManagedTurnObserved(t *testing.T, agent tui.Agent, prompt string) (string, []event.Event) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	sub, err := agent.Subscribe(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Close() }()
	commandID, err := agent.Submit(ctx, []content.Block{&content.TextBlock{Text: prompt}})
	if err != nil {
		t.Fatal(err)
	}
	var turnID uuid.UUID
	var observed []event.Event
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("turn timed out after events %s: %v", eventTypes(observed), ctx.Err())
		case delivery := <-sub.Events():
			observed = append(observed, delivery.Event)
			switch ev := delivery.Event.(type) {
			case event.TurnStarted:
				if ev.Cause.CommandID == commandID {
					turnID = ev.TurnID
				}
			case event.TurnDone:
				if ev.TurnID == turnID && !turnID.IsZero() {
					return aiMessageText(ev.Message), observed
				}
			case event.TurnFailed:
				if ev.TurnID == turnID && !turnID.IsZero() {
					t.Fatalf("turn failed: %v", ev.Err)
				}
			}
		}
	}
}

func eventTypes(events []event.Event) string {
	names := make([]string, len(events))
	for i, ev := range events {
		names[i] = fmt.Sprintf("%T", ev)
	}
	return strings.Join(names, ",")
}

func toolNamesFromRequest(req inference.Request) []string {
	names := make([]string, len(req.Tools))
	for i := range req.Tools {
		names[i] = req.Tools[i].Name
	}
	sort.Strings(names)
	return names
}

func requestHasRole(req inference.Request, name identity.AgentName) bool {
	return name == carbon.Name && strings.Contains(req.System, "You are Carbon")
}

// TestManagedAgentSelfDelegates proves Carbon is both the primer and the only
// managed delegate target, while harness still owns depth/quota enforcement.
func TestManagedAgentSelfDelegates(t *testing.T) {
	client := &managedScript{}
	calls := 0
	client.fn = func(_ context.Context, req inference.Request) ([]content.Chunk, error) {
		calls++
		if !requestHasRole(req, carbon.Name) {
			return nil, fmt.Errorf("request did not carry Carbon identity")
		}
		if calls == 1 {
			return startAgentCall("carbon-child", `{"agent_type":"carbon","instructions":"inspect"}`), nil
		}
		return finalText("Carbon child complete"), nil
	}
	stores, err := openTestStores(t)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := newSessionOverStores(context.Background(), client, newModelFactoryFor(testModel()), Config{}, stores, t.TempDir())
	if err != nil {
		t.Fatalf("newSessionOverStores: %v", err)
	}
	t.Cleanup(func() { _ = agent.Close(context.Background()) })
	if got := runManagedTurn(t, agent, "delegate focused inspection"); got != "Carbon child complete" {
		t.Fatalf("Carbon self-delegation result = %q", got)
	}
}

// delegateProbe captures the rig-bound, parent-scoped agent controller at bind.
type delegateProbe struct {
	mu         sync.Mutex
	controller tool.DelegateController
}

func (p *delegateProbe) definition() tool.Definition {
	return tool.NewDefinition("delegate-probe", tool.RequiresDelegateController, func(_ context.Context, bindings tool.Bindings) ([]tool.InvokableTool, error) {
		p.mu.Lock()
		p.controller = bindings.Delegate
		p.mu.Unlock()
		return []tool.InvokableTool{probeTool{}}, nil
	})
}

func (p *delegateProbe) captured() tool.DelegateController {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.controller
}

type probeTool struct{}

func (probeTool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{Name: "delegate-probe", Desc: "test probe", Schema: json.RawMessage(`{"type":"object"}`)}, nil
}

func (probeTool) InvokableRun(context.Context, string) (*tool.ToolResult, error) {
	return tool.TextResult("ok"), nil
}
