package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
	"github.com/looprig/inference/codec/bedrockconverse"
	contextcount "github.com/looprig/inference/contextcount"
	model "github.com/looprig/inference/model"
	stream "github.com/looprig/inference/stream"
	"github.com/looprig/storage"
	"github.com/looprig/storage/memstore"
)

// TestAcceptanceCompactionClientScriptsAreIndependent pins the test seam used by
// the composition acceptance suite: ordinary streamed turns and one-shot hustle
// calls must consume separate scripts and retain separate request captures.
func TestAcceptanceCompactionClientScriptsAreIndependent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
	}{
		{name: "stream and invoke are independently scripted"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &fakeLLM{
				streamSteps: []fakeStreamStep{{chunks: []content.Chunk{&content.TextChunk{Text: "turn"}}}},
				invokeSteps: []fakeInvokeStep{{response: &inference.Response{
					Message: &content.AIMessage{Message: content.Message{Role: content.RoleAssistant, Blocks: []content.Block{&content.TextBlock{Text: "oneshot"}}}},
				}}},
			}
			stream, err := client.Stream(context.Background(), inference.Request{System: "turn-system"})
			if err != nil {
				t.Fatalf("Stream() error = %v", err)
			}
			if _, err := stream.Next(); err != nil {
				t.Fatalf("Stream().Next() error = %v", err)
			}
			if _, err := client.Invoke(context.Background(), inference.Request{System: "compact-system"}); err != nil {
				t.Fatalf("Invoke() error = %v", err)
			}
			streamRequests, invokeRequests := client.capturedRequests()
			if len(streamRequests) != 1 || streamRequests[0].System != "turn-system" {
				t.Errorf("stream requests = %+v, want one turn request", streamRequests)
			}
			if len(invokeRequests) != 1 || invokeRequests[0].System != "compact-system" {
				t.Errorf("invoke requests = %+v, want one compaction request", invokeRequests)
			}
		})
	}
}

type acceptanceCompactionInput struct {
	Version            uint8              `json:"version"`
	Basis              json.RawMessage    `json:"basis"`
	Model              json.RawMessage    `json:"model"`
	RequestFingerprint string             `json:"request_fingerprint"`
	Transcript         []json.RawMessage  `json:"transcript"`
	MaxSummaryTokens   content.TokenCount `json:"max_summary_tokens"`
}

type acceptanceCompactionOutput struct {
	Version            uint8           `json:"version"`
	Basis              json.RawMessage `json:"basis"`
	Model              json.RawMessage `json:"model"`
	RequestFingerprint string          `json:"request_fingerprint"`
	Summary            string          `json:"summary"`
}

func acceptanceCompactionResponse(summary string, usage content.Usage) func(inference.Request) (*inference.Response, error) {
	return func(request inference.Request) (*inference.Response, error) {
		if len(request.Messages) != 1 {
			return nil, &acceptanceCompactionFixtureError{Field: "messages"}
		}
		message, ok := request.Messages[0].(*content.UserMessage)
		if !ok || message == nil || len(message.Blocks) != 1 {
			return nil, &acceptanceCompactionFixtureError{Field: "message"}
		}
		block, ok := message.Blocks[0].(*content.TextBlock)
		if !ok || block == nil {
			return nil, &acceptanceCompactionFixtureError{Field: "block"}
		}
		var input acceptanceCompactionInput
		if err := json.Unmarshal([]byte(block.Text), &input); err != nil {
			return nil, &acceptanceCompactionFixtureError{Field: "input", Cause: err}
		}
		raw, err := json.Marshal(acceptanceCompactionOutput{
			Version: input.Version, Basis: input.Basis, Model: input.Model,
			RequestFingerprint: input.RequestFingerprint, Summary: summary,
		})
		if err != nil {
			return nil, &acceptanceCompactionFixtureError{Field: "output", Cause: err}
		}
		return &inference.Response{
			Message: &content.AIMessage{Message: content.Message{
				Role: content.RoleAssistant, Blocks: []content.Block{&content.TextBlock{Text: string(raw)}},
			}},
			Usage: &usage,
		}, nil
	}
}

type acceptanceCompactionFixtureError struct {
	Field string
	Cause error
}

func (e *acceptanceCompactionFixtureError) Error() string {
	if e.Cause == nil {
		return "carbon test: invalid compaction fixture field " + e.Field
	}
	return "carbon test: invalid compaction fixture field " + e.Field + ": " + e.Cause.Error()
}

func (e *acceptanceCompactionFixtureError) Unwrap() error { return e.Cause }

func openAcceptanceAgentWithClient(t *testing.T, client inference.Client) (*RuntimeAgent, *sessionStores) {
	t.Helper()
	stores := mustHeadlessTestStores(t)
	agent, err := newSessionOverStores(context.Background(), client, newModelFactoryFor(testModel()), Config{}, stores, t.TempDir())
	if err != nil {
		t.Fatalf("newSessionOverStores() error = %v", err)
	}
	t.Cleanup(func() { _ = agent.Close(context.Background()) })
	return agent, stores
}

func acceptanceEventsUntil(t *testing.T, stream event.Subscription, stop func(event.Event) bool) []event.Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var events []event.Event
	for {
		select {
		case delivery, ok := <-stream.Events():
			if !ok {
				t.Fatal("event stream closed before expected event")
			}
			events = append(events, delivery.Event)
			if stop(delivery.Event) {
				return events
			}
		case <-ctx.Done():
			t.Fatalf("event wait timed out: %v", ctx.Err())
		}
	}
}

type acceptanceSubmitter interface {
	Submit(context.Context, []content.Block) (uuid.UUID, error)
}

func acceptanceSubmitTurn(t *testing.T, agent acceptanceSubmitter, stream event.Subscription, text string) []event.Event {
	t.Helper()
	if _, err := agent.Submit(context.Background(), []content.Block{&content.TextBlock{Text: text}}); err != nil {
		t.Fatalf("Submit(%q) error = %v", text, err)
	}
	return acceptanceEventsUntil(t, stream, func(ev event.Event) bool {
		_, ok := ev.(event.TurnDone)
		return ok
	})
}

func acceptanceMessageText(t *testing.T, message content.Conversation) string {
	t.Helper()
	var blocks []content.Block
	switch typed := message.(type) {
	case *content.UserMessage:
		blocks = typed.Blocks
	case *content.AIMessage:
		blocks = typed.Blocks
	case *content.ToolResultMessage:
		blocks = typed.Blocks
	default:
		t.Fatalf("message = %T, want user, assistant, or tool result", message)
	}
	if len(blocks) != 1 {
		t.Fatalf("message blocks = %d, want 1", len(blocks))
	}
	text, ok := blocks[0].(*content.TextBlock)
	if !ok || text == nil {
		t.Fatalf("message block = %T, want *content.TextBlock", blocks[0])
	}
	return text.Text
}

type acceptanceOversizedTool struct {
	output string
}

func (acceptanceOversizedTool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{
		Name:   "CompactionFixtureTool",
		Desc:   "returns a bounded-test fixture payload",
		Schema: json.RawMessage(`{"type":"object"}`),
	}, nil
}

func (acceptanceOversizedTool) PrepareCall(_ context.Context, _ uuid.UUID, _ string) (tool.Request, tool.PreparedArtifact, error) {
	return tool.Request{ToolName: "CompactionFixtureTool", Summary: "fixture output"}, nil, nil
}

func (t acceptanceOversizedTool) InvokableRun(context.Context, string) (*tool.ToolResult, error) {
	return tool.TextResult(t.output), nil
}

func acceptanceOversizedToolDefinition(output string) tool.Definition {
	return tool.NewDefinition("CompactionFixtureTool", 0, func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
		return []tool.InvokableTool{acceptanceOversizedTool{output: output}}, nil
	})
}

func openAcceptanceAgentWithAdditionalTools(t *testing.T, client inference.Client, extras ...tool.Definition) (*sessionAdapter, *sessionStores) {
	t.Helper()
	root := t.TempDir()
	access, cfg := headlessTestAccess(t, Config{}, root)
	definition, err := carbonTestDefinitionWithAdditionalTools(client, testModel(), cfg, access, extras)
	if err != nil {
		t.Fatalf("carbonTestDefinitionWithAdditionalTools() error = %v", err)
	}
	stores := mustHeadlessTestStores(t)
	assembly, err := buildRig(definition, stores, root, cfg, false)
	if err != nil {
		t.Fatalf("buildRig() error = %v", err)
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
	return agent, stores
}

func acceptanceTurnMessages(events []event.Event) content.AgenticMessages {
	var messages content.AgenticMessages
	for _, ev := range events {
		switch typed := ev.(type) {
		case event.TurnStarted:
			messages = append(messages, typed.Message)
		case event.StepDone:
			messages = append(messages, typed.Messages...)
		}
	}
	return messages
}

func acceptanceMessagesJSON(t *testing.T, messages content.AgenticMessages) []string {
	t.Helper()
	encoded := make([]string, len(messages))
	for i, message := range messages {
		raw, err := json.Marshal(message)
		if err != nil {
			t.Fatalf("json.Marshal(message[%d]) error = %v", i, err)
		}
		encoded[i] = string(raw)
	}
	return encoded
}

func acceptanceFindToolResult(t *testing.T, request inference.Request) string {
	t.Helper()
	for _, message := range request.Messages {
		if typed, ok := message.(*content.ToolResultMessage); ok {
			return acceptanceMessageText(t, typed)
		}
	}
	t.Fatalf("request has no tool result: %d messages", len(request.Messages))
	return ""
}

func acceptanceRuntimeMessageCount(t *testing.T, request inference.Request) int {
	t.Helper()
	count := 0
	for _, message := range request.Messages {
		if acceptanceMessageContainsRuntimeContext(message) {
			count++
		}
	}
	return count
}

func acceptanceRuntimeMessageJSON(t *testing.T, request inference.Request) string {
	t.Helper()
	for _, message := range request.Messages {
		if acceptanceMessageContainsRuntimeContext(message) {
			raw, err := json.Marshal(message)
			if err != nil {
				t.Fatalf("json.Marshal(runtime request message) error = %v", err)
			}
			return string(raw)
		}
	}
	t.Fatalf("request has no runtime context message")
	return ""
}

func acceptanceMessageContainsRuntimeContext(message content.Conversation) bool {
	var blocks []content.Block
	switch typed := message.(type) {
	case *content.UserMessage:
		blocks = typed.Blocks
	case *content.AIMessage:
		blocks = typed.Blocks
	case *content.SystemMessage:
		blocks = typed.Blocks
	case *content.ToolResultMessage:
		blocks = typed.Blocks
	default:
		return false
	}
	for _, block := range blocks {
		text, ok := block.(*content.TextBlock)
		if ok && text != nil && strings.Contains(text.Text, "<runtime_context>") {
			return true
		}
	}
	return false
}

func acceptanceAssertBedrockRuntimeWire(t *testing.T, request inference.Request, wantToolResults int) {
	t.Helper()
	body, err := bedrockconverse.EncodeRequest(request)
	if err != nil {
		t.Fatalf("bedrockconverse.EncodeRequest() error = %v", err)
	}
	var wire struct {
		Messages []struct {
			Role    string                       `json:"role"`
			Content []map[string]json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatalf("json.Unmarshal(Bedrock request) error = %v", err)
	}
	for i := 1; i < len(wire.Messages); i++ {
		if wire.Messages[i-1].Role == wire.Messages[i].Role {
			t.Errorf("Bedrock messages[%d:%d] have adjacent %q roles", i-1, i, wire.Messages[i].Role)
		}
	}
	runtimeMarkers := bytes.Count(body, []byte("runtime_context"))
	if runtimeMarkers != 2 {
		t.Errorf("Bedrock runtime_context marker count = %d, want one opening/closing pair", runtimeMarkers)
	}
	toolResults := 0
	toolResultMessages := 0
	for _, message := range wire.Messages {
		messageToolResults := 0
		for _, block := range message.Content {
			if _, ok := block["toolResult"]; ok {
				messageToolResults++
			}
		}
		if messageToolResults > 0 {
			toolResultMessages++
			toolResults += messageToolResults
		}
	}
	if toolResults != wantToolResults {
		t.Errorf("Bedrock toolResult blocks = %d, want %d", toolResults, wantToolResults)
	}
	if wantToolResults > 0 && toolResultMessages != 1 {
		t.Errorf("Bedrock tool-result messages = %d, want one grouped user turn", toolResultMessages)
	}
}

// TestAcceptanceCompactionProjectsHeadAndRetainsExactTail drives the real
// Carbon composition through a tool continuation, three completed user
// segments, and manual compaction. It deliberately compares the journal's
// unprojected retained suffix with the model-facing projected transcript: the
// two are different contracts and must stay different.
func TestAcceptanceBedrockRuntimeContextProjectsCompactionHeadAndRetainsExactTail(t *testing.T) {
	t.Parallel()
	const (
		summary     = `<conversation_summary><goal>summary-only authority</goal><constraints/><decisions/><state>old head reduced</state><open_items>keep retained users genuine</open_items></conversation_summary>`
		headMarker  = "OLD-HEAD-PROSE"
		bodyMarker  = "TOOL-BODY-UNIQUE-MIDDLE"
		tailMarker  = "TOOL-TAIL-MARKER"
		rawThinking = "RAW-THINKING-MUST-NOT-REACH-SUMMARY"
	)
	toolHead := "TOOL-HEAD-MARKER\n"
	toolOutput := toolHead + strings.Repeat("body filler ", 7000) + bodyMarker + strings.Repeat(" body filler", 7000) + "\n" + tailMarker
	if len(toolOutput) <= 50*1024 {
		t.Fatalf("fixture tool output = %d bytes, want larger than Carbon's 50KiB result limit", len(toolOutput))
	}
	client := &fakeLLM{
		streamSteps: []fakeStreamStep{
			{chunks: []content.Chunk{
				&content.ThinkingChunk{Thinking: rawThinking},
				&content.ToolUseChunk{Index: 0, ID: "compaction-fixture-tool-1", Name: "CompactionFixtureTool", InputJSON: `{}`},
				&content.ToolUseChunk{Index: 1, ID: "compaction-fixture-tool-2", Name: "CompactionFixtureTool", InputJSON: `{}`},
			}},
			{chunks: []content.Chunk{&content.TextChunk{Text: headMarker + " final"}}},
			{chunks: []content.Chunk{&content.TextChunk{Text: "RETAINED-ASSISTANT-ONE"}}},
			{chunks: []content.Chunk{&content.TextChunk{Text: "RETAINED-ASSISTANT-TWO"}}},
			{chunks: []content.Chunk{&content.TextChunk{Text: "RETAINED-ASSISTANT-TWO"}}},
		},
		invokeSteps: []fakeInvokeStep{{respond: acceptanceCompactionResponse(summary, content.Usage{InputTokens: 17, OutputTokens: 3})}},
	}
	agent, _ := openAcceptanceAgentWithAdditionalTools(t, client, acceptanceOversizedToolDefinition(toolOutput))
	stream, err := agent.Subscribe(event.EventFilter{Ephemeral: event.LoopScope{All: true}, Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	defer func() { _ = stream.Close() }()

	firstEvents := acceptanceSubmitTurn(t, agent, stream, "OLD-HEAD-USER")
	streamRequests, _ := client.capturedRequests()
	if len(streamRequests) < 2 {
		t.Fatalf("first tool turn captured %d stream requests, want initial and tool-continuation requests", len(streamRequests))
	}
	continuation := streamRequests[1]
	if continuation.TransientMessages != 1 {
		t.Errorf("tool continuation TransientMessages = %d, want 1 runtime tail", continuation.TransientMessages)
	}
	acceptanceAssertBedrockRuntimeWire(t, streamRequests[0], 0)
	acceptanceAssertBedrockRuntimeWire(t, continuation, 2)
	toolText := acceptanceFindToolResult(t, continuation)
	if len(toolText) > 50*1024 {
		t.Errorf("Carbon-shaped tool result = %d bytes, want <= 50KiB", len(toolText))
	}
	if len(toolText) >= len(toolOutput) {
		t.Errorf("Carbon-shaped tool result = %d bytes, want bounded below raw %d-byte output", len(toolText), len(toolOutput))
	}
	for _, marker := range []string{toolHead, tailMarker, "[tool output truncated:"} {
		if !strings.Contains(toolText, marker) {
			t.Errorf("Carbon-shaped tool result missing %q: %q", marker, toolText[:min(len(toolText), 256)])
		}
	}
	if strings.Contains(toolText, bodyMarker) {
		t.Errorf("Carbon-shaped tool result retained middle-only marker %q", bodyMarker)
	}
	if got := acceptanceRuntimeMessageCount(t, continuation); got != 1 {
		t.Errorf("tool continuation runtime context count = %d, want 1", got)
	}
	if got := acceptanceRuntimeMessageJSON(t, continuation); strings.Count(got, "runtime_context") != 2 {
		t.Errorf("tool continuation runtime context marker count = %d, want one opening/closing pair", strings.Count(got, "runtime_context"))
	}
	firstJournal := acceptanceMessagesJSON(t, acceptanceTurnMessages(firstEvents))
	firstJoined := strings.Join(firstJournal, "\n")
	if !strings.Contains(firstJoined, "OLD-HEAD-USER") || !strings.Contains(firstJoined, headMarker) || !strings.Contains(firstJoined, rawThinking) {
		t.Errorf("first unprojected journal = %q, want old-head prose and raw thinking before projection", firstJoined)
	}

	retainedOneEvents := acceptanceSubmitTurn(t, agent, stream, "RETAINED-USER-ONE")
	retainedTwoEvents := acceptanceSubmitTurn(t, agent, stream, "RETAINED-USER-TWO")
	wantRetained := append(acceptanceTurnMessages(retainedOneEvents), acceptanceTurnMessages(retainedTwoEvents)...)
	wantRetainedJSON := acceptanceMessagesJSON(t, wantRetained)

	idle, ok := agent.Controller().(interface{ WaitIdle(context.Context) error })
	if !ok {
		t.Fatal("session controller does not expose WaitIdle")
	}
	idleCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := idle.WaitIdle(idleCtx); err != nil {
		t.Fatalf("WaitIdle() error = %v", err)
	}
	if _, err := agent.CompactToLoop(context.Background(), agent.ActiveLoopID()); err != nil {
		t.Fatalf("CompactToLoop() error = %v", err)
	}
	compactionEvents := acceptanceEventsUntil(t, stream, func(ev event.Event) bool {
		_, ok := ev.(event.CompactionCommitted)
		return ok
	})
	committed := compactionEvents[len(compactionEvents)-1].(event.CompactionCommitted)
	if err := event.ValidateEvent(committed); err != nil {
		t.Fatalf("CompactionCommitted validation error = %v", err)
	}
	if got := acceptanceMessageText(t, committed.Summary); got != summary {
		t.Errorf("committed summary = %q, want fixture summary", got)
	}
	gotRetainedJSON := acceptanceMessagesJSON(t, committed.Retained)
	if !reflect.DeepEqual(gotRetainedJSON, wantRetainedJSON) {
		t.Errorf("committed Retained = %v, want exact unprojected newest two segments %v", gotRetainedJSON, wantRetainedJSON)
	}
	if strings.Contains(strings.Join(gotRetainedJSON, "\n"), "summary-only authority") {
		t.Error("summary content was injected into committed Retained")
	}
	for i := range committed.Retained {
		if committed.Retained[i] == wantRetained[i] {
			t.Errorf("committed Retained[%d] aliases source event message", i)
		}
	}
	if committed.PostContext.Basis.Revision != committed.Basis.Revision+1 || committed.PostContext.Basis.ThroughEventID != committed.EventID {
		t.Errorf("PostContext basis = %+v, want revision %d through committed event %s", committed.PostContext.Basis, committed.Basis.Revision+1, committed.EventID)
	}

	streamRequests, invokeRequests := client.capturedRequests()
	if len(invokeRequests) != 1 {
		t.Fatalf("captured Invoke requests = %d, want exactly one compaction request", len(invokeRequests))
	}
	invoke := invokeRequests[0]
	if invoke.System != conversationCompactionPrompt || len(invoke.Tools) != 0 {
		t.Errorf("compaction Invoke system/tools = %q/%d, want Carbon prompt/no tools", invoke.System, len(invoke.Tools))
	}
	inputText := acceptanceMessageText(t, invoke.Messages[0])
	if got := len([]byte(inputText)); got > conversationCompactionInputBytes {
		t.Errorf("serialized compaction adapter input = %d bytes, want <= configured hustle InputBytes %d", got, conversationCompactionInputBytes)
	}
	var input acceptanceCompactionInput
	if err := json.Unmarshal([]byte(inputText), &input); err != nil {
		t.Fatalf("compaction input JSON error = %v", err)
	}
	if len(input.Transcript) == 0 {
		t.Fatal("compaction transcript is empty")
	}
	projected := strings.Join(rawMessagesAsStrings(input.Transcript), "\n")
	if !strings.Contains(projected, "OLD-HEAD-USER") || !strings.Contains(projected, headMarker) {
		t.Errorf("projected compaction transcript = %q, want selected old head/user prose", projected)
	}
	for _, marker := range []string{"TOOL-HEAD-MARKER", tailMarker, "[tool result truncated for compaction:"} {
		if !strings.Contains(projected, marker) {
			t.Errorf("projected compaction transcript missing %q", marker)
		}
	}
	if strings.Contains(projected, rawThinking) {
		t.Errorf("projected compaction transcript leaked raw thinking payload %q", rawThinking)
	}
	if !strings.Contains(projected, "[thinking omitted for compaction]") {
		t.Error("projected compaction transcript lacks typed thinking placeholder")
	}
	for _, excluded := range []string{"RETAINED-USER-ONE", "RETAINED-ASSISTANT-ONE", "RETAINED-USER-TWO", "RETAINED-ASSISTANT-TWO"} {
		if strings.Contains(projected, excluded) {
			t.Errorf("projected compaction transcript included retained newest-segment text %q", excluded)
		}
	}
	if got := acceptanceRuntimeMessageJSON(t, streamRequests[0]); got != acceptanceRuntimeMessageJSON(t, streamRequests[1]) {
		t.Error("tool continuation changed or duplicated the runtime context marker")
	}

	postEvents := acceptanceSubmitTurn(t, agent, stream, "POST-COMPACTION-USER")
	if done := postEvents[len(postEvents)-1].(event.TurnDone); acceptanceMessageText(t, done.Message) != "RETAINED-ASSISTANT-TWO" {
		t.Errorf("post-compaction TurnDone = %q, want scripted response", acceptanceMessageText(t, done.Message))
	}
	streamRequests, _ = client.capturedRequests()
	next := streamRequests[len(streamRequests)-1]
	if next.TransientMessages != 1 {
		t.Errorf("next request TransientMessages = %d, want 1 runtime tail", next.TransientMessages)
	}
	if got := acceptanceRuntimeMessageCount(t, next); got != 1 {
		t.Errorf("next request runtime context count = %d, want 1", got)
	}
	acceptanceAssertBedrockRuntimeWire(t, next, 0)
	expectedNext := append(content.AgenticMessages{committed.Summary}, wantRetained...)
	expectedNext = append(expectedNext, &content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: []content.Block{&content.TextBlock{Text: "POST-COMPACTION-USER"}}}})
	if len(next.Messages) != len(expectedNext)+1 {
		t.Fatalf("next request messages = %d, want %d plus runtime context", len(next.Messages), len(expectedNext)+1)
	}
	for i, expected := range expectedNext {
		gotJSON, err := json.Marshal(next.Messages[i])
		if err != nil {
			t.Fatalf("json.Marshal(next.Messages[%d]) error = %v", i, err)
		}
		wantJSON, err := json.Marshal(expected)
		if err != nil {
			t.Fatalf("json.Marshal(expected next message[%d]) error = %v", i, err)
		}
		if !bytes.Equal(gotJSON, wantJSON) {
			t.Errorf("next request message[%d] = %s, want %s", i, gotJSON, wantJSON)
		}
	}
}

func rawMessagesAsStrings(messages []json.RawMessage) []string {
	values := make([]string, len(messages))
	for i, message := range messages {
		values[i] = string(message)
	}
	return values
}

func TestAcceptanceManualCompactionUsesOneShotAndResetsNextContext(t *testing.T) {
	t.Parallel()
	const summary = `<conversation_summary><goal>ship</goal><constraints></constraints><decisions></decisions><state>first turn complete</state><open_items>continue</open_items></conversation_summary>`
	turnUsage := content.Usage{InputTokens: 11, OutputTokens: 5}
	compactionUsage := content.Usage{InputTokens: 7, OutputTokens: 2}
	secondUsage := content.Usage{InputTokens: 3, OutputTokens: 1}
	tests := []struct {
		name string
	}{
		{name: "idle manual compaction is isolated and the next turn starts from summary"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &fakeLLM{
				streamSteps: []fakeStreamStep{
					{chunks: []content.Chunk{&content.TextChunk{Text: "first reply"}}, result: &stream.StreamResult{Usage: &turnUsage}},
					{chunks: []content.Chunk{&content.TextChunk{Text: "retained reply one"}}},
					{chunks: []content.Chunk{&content.TextChunk{Text: "retained reply two"}}},
					{chunks: []content.Chunk{&content.TextChunk{Text: "second reply"}}, result: &stream.StreamResult{Usage: &secondUsage}},
				},
				invokeSteps: []fakeInvokeStep{{respond: acceptanceCompactionResponse(summary, compactionUsage)}},
			}
			agent, _ := openAcceptanceAgentWithClient(t, client)
			stream, err := agent.Subscribe(event.EventFilter{
				Ephemeral: event.LoopScope{All: true}, Enduring: event.LoopScope{All: true},
			})
			if err != nil {
				t.Fatalf("Subscribe() error = %v", err)
			}
			defer func() { _ = stream.Close() }()

			if _, err := agent.Submit(context.Background(), []content.Block{&content.TextBlock{Text: "first user"}}); err != nil {
				t.Fatalf("first Submit() error = %v", err)
			}
			firstEvents := acceptanceEventsUntil(t, stream, func(ev event.Event) bool {
				_, ok := ev.(event.TurnDone)
				return ok
			})
			firstDone := firstEvents[len(firstEvents)-1].(event.TurnDone)
			if firstDone.Usage != turnUsage || acceptanceMessageText(t, firstDone.Message) != "first reply" {
				t.Errorf("first TurnDone = usage %+v message %q, want ordinary stream result", firstDone.Usage, acceptanceMessageText(t, firstDone.Message))
			}
			acceptanceSubmitTurn(t, agent, stream, "retained user one")
			acceptanceSubmitTurn(t, agent, stream, "retained user two")
			idle, ok := agent.Controller().(interface{ WaitIdle(context.Context) error })
			if !ok {
				t.Fatal("session controller does not expose WaitIdle")
			}
			idleCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := idle.WaitIdle(idleCtx); err != nil {
				t.Fatalf("WaitIdle() error = %v", err)
			}
			if _, err := agent.CompactToLoop(context.Background(), agent.ActiveLoopID()); err != nil {
				t.Fatalf("CompactToLoop() error = %v", err)
			}
			compactionEvents := acceptanceEventsUntil(t, stream, func(ev event.Event) bool {
				_, ok := ev.(event.CompactionCommitted)
				return ok
			})
			committed := compactionEvents[len(compactionEvents)-1].(event.CompactionCommitted)
			if got := acceptanceMessageText(t, committed.Summary); got != summary {
				t.Errorf("committed summary = %q, want %q", got, summary)
			}
			for _, ev := range append(firstEvents, compactionEvents...) {
				switch ev.(type) {
				case event.HustleStarted, event.HustleCompleted, event.HustleFailed:
					t.Errorf("public subscription exposed internal event %T", ev)
				}
			}

			secondEvents := acceptanceSubmitTurn(t, agent, stream, "second user")
			secondDone := secondEvents[len(secondEvents)-1].(event.TurnDone)
			if secondDone.Usage != secondUsage {
				t.Errorf("second TurnDone usage = %+v, want %+v", secondDone.Usage, secondUsage)
			}

			streamRequests, invokeRequests := client.capturedRequests()
			if len(streamRequests) != 4 || len(invokeRequests) != 1 {
				t.Fatalf("captured Stream/Invoke requests = %d/%d, want 4/1", len(streamRequests), len(invokeRequests))
			}
			invoke := invokeRequests[0]
			if invoke.System != conversationCompactionPrompt || len(invoke.Tools) != 0 || invoke.Model.Key() != streamRequests[0].Model.Key() {
				t.Errorf("Invoke request = system match %v tools %d model %v, want exact compaction prompt/no tools/current model", invoke.System == conversationCompactionPrompt, len(invoke.Tools), invoke.Model.Key())
			}
			inputText := acceptanceMessageText(t, invoke.Messages[0])
			var input acceptanceCompactionInput
			if err := json.Unmarshal([]byte(inputText), &input); err != nil {
				t.Fatalf("compaction input JSON error = %v", err)
			}
			if input.Version != 1 || input.MaxSummaryTokens != conversationCompactionPolicy().MaxSummaryTokens || len(input.Transcript) != 2 {
				t.Errorf("compaction input = version %d budget %d transcript %d, want exact v1/budget/two-message context", input.Version, input.MaxSummaryTokens, len(input.Transcript))
			}
			if !strings.Contains(string(input.Transcript[0]), "first user") || !strings.Contains(string(input.Transcript[1]), "first reply") {
				t.Errorf("compaction transcript = %s, want exact completed first turn", input.Transcript)
			}
			var nextTexts []string
			for _, message := range streamRequests[3].Messages {
				nextTexts = append(nextTexts, acceptanceMessageText(t, message))
			}
			wantNext := []string{summary, "retained user one", "retained reply one", "retained user two", "retained reply two", "second user"}
			if len(nextTexts) != len(wantNext)+1 || !strings.HasPrefix(nextTexts[len(nextTexts)-1], "<runtime_context>") {
				t.Errorf("next request context = %q, want retained tail plus new user and fresh runtime context", nextTexts)
			} else {
				for i, want := range wantNext {
					if nextTexts[i] != want {
						t.Errorf("next request message[%d] = %q, want %q", i, nextTexts[i], want)
					}
				}
			}
			if firstDone.Usage == compactionUsage || secondDone.Usage == compactionUsage {
				t.Error("compaction Invoke usage leaked into ordinary turn accounting")
			}
		})
	}
}

func TestAcceptanceCompactionRejectsEveryXMLFailureDomain(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		summary string
	}{
		{name: "syntax", summary: `<conversation_summary><goal>x</goal>`},
		{name: "root", summary: `<summary><goal>x</goal><constraints/><decisions/><state>y</state><open_items/></summary>`},
		{name: "structure", summary: `<conversation_summary><state>y</state><goal>x</goal><constraints/><decisions/><open_items/></conversation_summary>`},
		{name: "content", summary: `<conversation_summary><goal> </goal><constraints/><decisions/><state>y</state><open_items/></conversation_summary>`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			turnUsage := content.Usage{OutputTokens: 1}
			client := &fakeLLM{
				streamSteps: []fakeStreamStep{
					{chunks: []content.Chunk{&content.TextChunk{Text: "reply"}}, result: &stream.StreamResult{Usage: &turnUsage}},
					{chunks: []content.Chunk{&content.TextChunk{Text: "retained reply one"}}},
					{chunks: []content.Chunk{&content.TextChunk{Text: "retained reply two"}}},
				},
				invokeSteps: []fakeInvokeStep{{respond: acceptanceCompactionResponse(tt.summary, content.Usage{OutputTokens: 1})}},
			}
			agent, _ := openAcceptanceAgentWithClient(t, client)
			stream, err := agent.Subscribe(event.EventFilter{Enduring: event.LoopScope{All: true}})
			if err != nil {
				t.Fatalf("Subscribe() error = %v", err)
			}
			defer func() { _ = stream.Close() }()
			if _, err := agent.Submit(context.Background(), []content.Block{&content.TextBlock{Text: "seed"}}); err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			acceptanceEventsUntil(t, stream, func(ev event.Event) bool { _, ok := ev.(event.TurnDone); return ok })
			acceptanceSubmitTurn(t, agent, stream, "retained user one")
			acceptanceSubmitTurn(t, agent, stream, "retained user two")
			if _, err := agent.CompactToLoop(context.Background(), agent.ActiveLoopID()); err != nil {
				t.Fatalf("CompactToLoop() error = %v", err)
			}
			events := acceptanceEventsUntil(t, stream, func(ev event.Event) bool { _, ok := ev.(event.CompactionRejected); return ok })
			rejected := events[len(events)-1].(event.CompactionRejected)
			if rejected.RejectReason != event.CompactRejectInvalidSummary {
				t.Errorf("CompactionRejected reason = %v, want invalid summary", rejected.RejectReason)
			}
		})
	}
}

type acceptanceInferenceError struct{ Message string }

func (e *acceptanceInferenceError) Error() string { return e.Message }

func TestAcceptanceCompactionExecutionFailureIsSoft(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
	}{
		{name: "one-shot provider failure rejects compaction but admits another turn"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &fakeLLM{
				streamSteps: []fakeStreamStep{
					{chunks: []content.Chunk{&content.TextChunk{Text: "before"}}},
					{chunks: []content.Chunk{&content.TextChunk{Text: "retained reply one"}}},
					{chunks: []content.Chunk{&content.TextChunk{Text: "retained reply two"}}},
					{chunks: []content.Chunk{&content.TextChunk{Text: "after"}}},
				},
				invokeSteps: []fakeInvokeStep{{err: &acceptanceInferenceError{Message: "provider unavailable"}}},
			}
			agent, _ := openAcceptanceAgentWithClient(t, client)
			stream, err := agent.Subscribe(event.EventFilter{Enduring: event.LoopScope{All: true}})
			if err != nil {
				t.Fatalf("Subscribe() error = %v", err)
			}
			defer func() { _ = stream.Close() }()
			if _, err := agent.Submit(context.Background(), []content.Block{&content.TextBlock{Text: "before"}}); err != nil {
				t.Fatalf("first Submit() error = %v", err)
			}
			acceptanceEventsUntil(t, stream, func(ev event.Event) bool { _, ok := ev.(event.TurnDone); return ok })
			acceptanceSubmitTurn(t, agent, stream, "retained user one")
			acceptanceSubmitTurn(t, agent, stream, "retained user two")
			if _, err := agent.CompactToLoop(context.Background(), agent.ActiveLoopID()); err != nil {
				t.Fatalf("CompactToLoop() error = %v", err)
			}
			events := acceptanceEventsUntil(t, stream, func(ev event.Event) bool { _, ok := ev.(event.CompactionRejected); return ok })
			if got := events[len(events)-1].(event.CompactionRejected).RejectReason; got != event.CompactRejectExecutionFailed {
				t.Errorf("CompactionRejected reason = %v, want execution failed", got)
			}
			if _, err := agent.Submit(context.Background(), []content.Block{&content.TextBlock{Text: "after"}}); err != nil {
				t.Fatalf("post-rejection Submit() error = %v", err)
			}
			continued := acceptanceEventsUntil(t, stream, func(ev event.Event) bool { _, ok := ev.(event.TurnDone); return ok })
			if got := acceptanceMessageText(t, continued[len(continued)-1].(event.TurnDone).Message); got != "after" {
				t.Errorf("post-rejection response = %q, want %q", got, "after")
			}
		})
	}
}

func TestAcceptanceCompactionUsesModelChangedBeforeAttempt(t *testing.T) {
	t.Parallel()
	const summary = `<conversation_summary><goal>ship</goal><constraints/><decisions/><state>changed model</state><open_items/></conversation_summary>`
	tests := []struct {
		name string
	}{
		{name: "current loop model is resolved when the hustle starts"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &fakeLLM{
				streamSteps: []fakeStreamStep{
					{chunks: []content.Chunk{&content.TextChunk{Text: "reply"}}},
					{chunks: []content.Chunk{&content.TextChunk{Text: "retained reply one"}}},
					{chunks: []content.Chunk{&content.TextChunk{Text: "retained reply two"}}},
				},
				invokeSteps: []fakeInvokeStep{{respond: acceptanceCompactionResponse(summary, content.Usage{OutputTokens: 1})}},
			}
			agent, _ := openAcceptanceAgentWithClient(t, client)
			stream, err := agent.Subscribe(event.EventFilter{Enduring: event.LoopScope{All: true}})
			if err != nil {
				t.Fatalf("Subscribe() error = %v", err)
			}
			defer func() { _ = stream.Close() }()
			if _, err := agent.Submit(context.Background(), []content.Block{&content.TextBlock{Text: "seed"}}); err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			acceptanceEventsUntil(t, stream, func(ev event.Event) bool { _, ok := ev.(event.TurnDone); return ok })
			acceptanceSubmitTurn(t, agent, stream, "retained user one")
			acceptanceSubmitTurn(t, agent, stream, "retained user two")
			handle, ok := agent.Controller().Loop(agent.ActiveLoopID())
			if !ok {
				t.Fatal("active loop handle not found")
			}
			controller, ok := handle.(loop.Controller)
			if !ok {
				t.Fatal("active loop handle does not expose Change")
			}
			changed := testModel()
			changed.Name = "fake-model-changed"
			if err := controller.Change(context.Background(), loop.ChangeModel(changed)); err != nil {
				t.Fatalf("Change(model) error = %v", err)
			}
			if _, err := agent.CompactToLoop(context.Background(), agent.ActiveLoopID()); err != nil {
				t.Fatalf("CompactToLoop() error = %v", err)
			}
			acceptanceEventsUntil(t, stream, func(ev event.Event) bool { _, ok := ev.(event.CompactionCommitted); return ok })
			_, invokes := client.capturedRequests()
			if len(invokes) != 1 || invokes[0].Model.Name != changed.Name {
				t.Errorf("Invoke model = %+v, want changed model %q", invokes, changed.Name)
			}
		})
	}
}

type acceptanceContextCounter struct {
	mu         sync.Mutex
	counts     []content.TokenCount
	capability contextcount.CounterCapability
}

func (c *acceptanceContextCounter) CountContext(_ context.Context, request inference.Request) (contextcount.ContextCount, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.counts) == 0 {
		return contextcount.ContextCount{}, &acceptanceCompactionFixtureError{Field: "counter script"}
	}
	count := c.counts[0]
	if len(c.counts) > 1 {
		c.counts = c.counts[1:]
	}
	return contextcount.ContextCount{Model: request.Model.Key(), InputTokens: count, Quality: c.capability.Quality}, nil
}

func (c *acceptanceContextCounter) CounterCapability() contextcount.CounterCapability {
	return c.capability
}

func openAcceptanceAgentWithContextPolicy(t *testing.T, client inference.Client, counter contextcount.ContextCounter) *sessionAdapter {
	t.Helper()
	selectedModel := testModel()
	selectedModel.Limits = model.ContextLimits{WindowTokens: 100, MaxInputTokens: 80, MaxOutputTokens: 20}
	capability := counter.CounterCapability()
	compaction := conversationCompactionPolicy()
	compaction.CounterPolicy = loop.CounterPolicyRequireExact
	compaction.ReservedOutput = 20
	compaction.SafetyMargin = 0
	compaction.MaxSummaryTokens = 10
	policy := conversationContextPolicy{
		counter: counter,
		capability: contextcount.InferenceCapability{
			Provider: contextcount.ProviderID(selectedModel.Provider), Transport: contextcount.InferenceTransportLocal, Retention: contextcount.RetentionNone,
		},
		compaction: compaction, summaryFragment: conversationSummaryConsumptionFragment,
		summaryRevision: conversationSummaryConsumptionRevision,
	}
	if err := compaction.Validate(capability); err != nil {
		t.Fatalf("compaction policy validation error = %v", err)
	}
	root := t.TempDir()
	access, cfg := headlessTestAccess(t, Config{}, root)
	definition, err := carbonTestDefinitionWithContextPolicy(client, selectedModel, cfg, policy, access)
	if err != nil {
		t.Fatalf("carbonTestDefinitionWithContextPolicy() error = %v", err)
	}
	stores := mustHeadlessTestStores(t)
	assembly, err := buildRig(definition, stores, root, cfg, false)
	if err != nil {
		t.Fatalf("buildRig() error = %v", err)
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
	return agent
}

func TestAcceptanceAutomaticThresholdPausesAtSafeBoundary(t *testing.T) {
	t.Parallel()
	const summary = `<conversation_summary><goal>ship</goal><constraints/><decisions/><state>threshold compacted</state><open_items/></conversation_summary>`
	tests := []struct {
		name string
	}{
		{name: "queued next turn cannot start while automatic compaction is blocked"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			invokeEntered := make(chan struct{})
			invokeRelease := make(chan struct{})
			secondStreamEntered := make(chan struct{})
			client := &fakeLLM{
				streamSteps: []fakeStreamStep{
					{chunks: []content.Chunk{&content.TextChunk{Text: "first reply"}}},
					{chunks: []content.Chunk{&content.TextChunk{Text: "retained reply one"}}},
					{chunks: []content.Chunk{&content.TextChunk{Text: "retained reply two"}}},
					{chunks: []content.Chunk{&content.TextChunk{Text: "second reply"}}, entered: secondStreamEntered},
				},
				invokeSteps: []fakeInvokeStep{{
					respond: acceptanceCompactionResponse(summary, content.Usage{OutputTokens: 1}),
					entered: invokeEntered, release: invokeRelease,
				}},
			}
			counterCapability := contextcount.CounterCapability{
				Transport: contextcount.CounterTransportLocal, Retention: contextcount.RetentionNone,
				TokenizerRev: "carbon-acceptance-exact-v1", Quality: contextcount.CountQualityExactLocal,
			}
			// The first five measurements cover the pre/post admission checks for
			// the first two complete turns and the third turn's initial request.
			// Only the third turn's post-check crosses the threshold, after three
			// user-anchored segments exist for KeepRecentSegments=2 to retain.
			counter := &acceptanceContextCounter{counts: []content.TokenCount{20, 20, 20, 20, 20, 65, 20, 20, 20}, capability: counterCapability}
			agent := openAcceptanceAgentWithContextPolicy(t, client, counter)
			stream, err := agent.Subscribe(event.EventFilter{Ephemeral: event.LoopScope{All: true}, Enduring: event.LoopScope{All: true}})
			if err != nil {
				t.Fatalf("Subscribe() error = %v", err)
			}
			defer func() { _ = stream.Close() }()
			acceptanceSubmitTurn(t, agent, stream, "first")
			acceptanceSubmitTurn(t, agent, stream, "retained user one")
			if _, err := agent.Submit(context.Background(), []content.Block{&content.TextBlock{Text: "retained user two"}}); err != nil {
				t.Fatalf("third Submit() error = %v", err)
			}
			select {
			case <-invokeEntered:
			case <-time.After(5 * time.Second):
				t.Fatal("automatic compaction did not reach Invoke")
			}
			if _, err := agent.Submit(context.Background(), []content.Block{&content.TextBlock{Text: "second"}}); err != nil {
				t.Fatalf("queued Submit() error = %v", err)
			}
			select {
			case <-secondStreamEntered:
				t.Fatal("next ordinary inference started before automatic compaction completed")
			case <-time.After(100 * time.Millisecond):
			}
			close(invokeRelease)
			events := acceptanceEventsUntil(t, stream, func(ev event.Event) bool { _, ok := ev.(event.CompactionCommitted); return ok })
			committed := events[len(events)-1].(event.CompactionCommitted)
			if committed.Reason != event.CompactionReasonAutomatic {
				t.Errorf("compaction reason = %v, want automatic", committed.Reason)
			}
			select {
			case <-secondStreamEntered:
			case <-time.After(5 * time.Second):
				t.Fatal("queued next turn did not start after automatic compaction completed")
			}
		})
	}
}

type acceptanceFinalizationError struct{}

func (*acceptanceFinalizationError) Error() string {
	return "carbon test: compaction terminal append failed"
}

type failCompactionTerminalLedger struct {
	storage.Ledger
	mu     sync.Mutex
	armed  bool
	err    error
	failed chan struct{}
}

func (l *failCompactionTerminalLedger) arm() {
	l.mu.Lock()
	l.armed = true
	l.mu.Unlock()
}

func (l *failCompactionTerminalLedger) Append(ctx context.Context, name string, expected uint64, payload []byte) error {
	var envelope struct {
		Body []byte `json:"body"`
	}
	decoded := json.Unmarshal(payload, &envelope) == nil
	l.mu.Lock()
	shouldFail := l.armed && decoded && bytes.Contains(envelope.Body, []byte("CompactionCommitted"))
	if shouldFail {
		l.armed = false
		close(l.failed)
	}
	l.mu.Unlock()
	if shouldFail {
		return l.err
	}
	return l.Ledger.Append(ctx, name, expected, payload)
}

func TestAcceptanceCompactionFinalizationFailureFaultsSession(t *testing.T) {
	t.Parallel()
	const summary = `<conversation_summary><goal>ship</goal><constraints/><decisions/><state>ready</state><open_items/></conversation_summary>`
	tests := []struct {
		name string
	}{
		{name: "durable terminal append failure is hard and rejects future admission"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			finalizationErr := &acceptanceFinalizationError{}
			invokeEntered := make(chan struct{})
			invokeRelease := make(chan struct{})
			base := memstore.New()
			ledger := &failCompactionTerminalLedger{Ledger: base.Ledger, err: finalizationErr, failed: make(chan struct{})}
			backend, err := storage.NewComposite(ledger, base.Leaser, base.KV, base.Blobs)
			if err != nil {
				t.Fatalf("storage.NewComposite() error = %v", err)
			}
			stores, err := openStores(backend)
			if err != nil {
				t.Fatalf("openStores() error = %v", err)
			}
			stores.resourceStorage = testResourceStorageProvider{base: t.TempDir()}
			client := &fakeLLM{
				streamSteps: []fakeStreamStep{
					{chunks: []content.Chunk{&content.TextChunk{Text: "reply"}}},
					{chunks: []content.Chunk{&content.TextChunk{Text: "retained reply one"}}},
					{chunks: []content.Chunk{&content.TextChunk{Text: "retained reply two"}}},
				},
				invokeSteps: []fakeInvokeStep{{
					respond: acceptanceCompactionResponse(summary, content.Usage{OutputTokens: 1}),
					entered: invokeEntered, release: invokeRelease,
				}},
			}
			root := t.TempDir()
			access, cfg := headlessTestAccess(t, Config{}, root)
			definition, err := carbonTestDefinition(client, testModel(), cfg, access)
			if err != nil {
				t.Fatalf("carbonTestDefinition() error = %v", err)
			}
			assembly, err := buildRig(definition, stores, root, cfg, false)
			if err != nil {
				t.Fatalf("buildRig() error = %v", err)
			}
			controller, err := assembly.NewSession(context.Background())
			if err != nil {
				t.Fatalf("NewSession() error = %v", err)
			}
			agent, err := newSessionAdapter(context.Background(), controller, stores.session, false)
			if err != nil {
				t.Fatalf("newSessionAdapter() error = %v", err)
			}
			defer func() { _ = agent.Close(context.Background()) }()
			stream, err := agent.Subscribe(event.EventFilter{Enduring: event.LoopScope{All: true}})
			if err != nil {
				t.Fatalf("Subscribe() error = %v", err)
			}
			defer func() { _ = stream.Close() }()
			if _, err := agent.Submit(context.Background(), []content.Block{&content.TextBlock{Text: "seed"}}); err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			acceptanceEventsUntil(t, stream, func(ev event.Event) bool { _, ok := ev.(event.TurnDone); return ok })
			acceptanceSubmitTurn(t, agent, stream, "retained user one")
			acceptanceSubmitTurn(t, agent, stream, "retained user two")
			ledger.arm()
			if _, err := agent.CompactToLoop(context.Background(), agent.ActiveLoopID()); err != nil {
				t.Fatalf("CompactToLoop() error = %v", err)
			}
			select {
			case <-invokeEntered:
			case <-time.After(5 * time.Second):
				t.Fatal("compaction did not reach Invoke")
			}
			close(invokeRelease)
			select {
			case <-ledger.failed:
			case <-time.After(5 * time.Second):
				t.Fatal("compaction terminal did not reach the durable ledger")
			}
			idle, ok := agent.Controller().(interface{ WaitIdle(context.Context) error })
			if !ok {
				t.Fatal("session controller does not expose WaitIdle")
			}
			faultCtx, faultCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer faultCancel()
			faultTicker := time.NewTicker(time.Millisecond)
			defer faultTicker.Stop()
			for {
				waitErr := idle.WaitIdle(faultCtx)
				if errors.Is(waitErr, finalizationErr) {
					break
				}
				if waitErr != nil {
					t.Fatalf("WaitIdle() error = %T %v, want finalization fault", waitErr, waitErr)
				}
				select {
				case <-faultTicker.C:
				case <-faultCtx.Done():
					t.Fatalf("session did not report finalization fault: %v", faultCtx.Err())
				}
			}
			if _, err := agent.Submit(context.Background(), []content.Block{&content.TextBlock{Text: "must reject"}}); !errors.Is(err, finalizationErr) {
				t.Errorf("Submit() after hard finalization failure = %T %v, want fault cause", err, err)
			}
		})
	}
}
