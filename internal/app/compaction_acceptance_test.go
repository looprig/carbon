package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/inference"
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
		return "coderig test: invalid compaction fixture field " + e.Field
	}
	return "coderig test: invalid compaction fixture field " + e.Field + ": " + e.Cause.Error()
}

func (e *acceptanceCompactionFixtureError) Unwrap() error { return e.Cause }

func openAcceptanceAgentWithClient(t *testing.T, client inference.Client) (*RuntimeAgent, *swarmStores) {
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

func acceptanceMessageText(t *testing.T, message content.Conversation) string {
	t.Helper()
	var blocks []content.Block
	switch typed := message.(type) {
	case *content.UserMessage:
		blocks = typed.Blocks
	case *content.AIMessage:
		blocks = typed.Blocks
	default:
		t.Fatalf("message = %T, want user or assistant", message)
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

			if _, err := agent.Submit(context.Background(), []content.Block{&content.TextBlock{Text: "second user"}}); err != nil {
				t.Fatalf("second Submit() error = %v", err)
			}
			secondEvents := acceptanceEventsUntil(t, stream, func(ev event.Event) bool {
				_, ok := ev.(event.TurnDone)
				return ok
			})
			secondDone := secondEvents[len(secondEvents)-1].(event.TurnDone)
			if secondDone.Usage != secondUsage {
				t.Errorf("second TurnDone usage = %+v, want %+v", secondDone.Usage, secondUsage)
			}

			streamRequests, invokeRequests := client.capturedRequests()
			if len(streamRequests) != 2 || len(invokeRequests) != 1 {
				t.Fatalf("captured Stream/Invoke requests = %d/%d, want 2/1", len(streamRequests), len(invokeRequests))
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
			for _, message := range streamRequests[1].Messages {
				nextTexts = append(nextTexts, acceptanceMessageText(t, message))
			}
			if len(nextTexts) != 3 || nextTexts[0] != summary || nextTexts[1] != "second user" || !strings.HasPrefix(nextTexts[2], "<runtime_context>") {
				t.Errorf("next request context = %q, want summary, new user input, then fresh runtime context", nextTexts)
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
				streamSteps: []fakeStreamStep{{chunks: []content.Chunk{&content.TextChunk{Text: "reply"}}, result: &stream.StreamResult{Usage: &turnUsage}}},
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
				streamSteps: []fakeStreamStep{{chunks: []content.Chunk{&content.TextChunk{Text: "reply"}}}},
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
	definitions, err := swarmDefinitionsWithContextPolicy(client, selectedModel, cfg, policy, access)
	if err != nil {
		t.Fatalf("swarmDefinitionsWithContextPolicy() error = %v", err)
	}
	stores := mustHeadlessTestStores(t)
	assembly, err := buildRig(definitions, stores, root, cfg, false)
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
					{chunks: []content.Chunk{&content.TextChunk{Text: "second reply"}}, entered: secondStreamEntered},
				},
				invokeSteps: []fakeInvokeStep{{
					respond: acceptanceCompactionResponse(summary, content.Usage{OutputTokens: 1}),
					entered: invokeEntered, release: invokeRelease,
				}},
			}
			counterCapability := contextcount.CounterCapability{
				Transport: contextcount.CounterTransportLocal, Retention: contextcount.RetentionNone,
				TokenizerRev: "coderig-acceptance-exact-v1", Quality: contextcount.CountQualityExactLocal,
			}
			counter := &acceptanceContextCounter{counts: []content.TokenCount{65, 20, 20, 20}, capability: counterCapability}
			agent := openAcceptanceAgentWithContextPolicy(t, client, counter)
			stream, err := agent.Subscribe(event.EventFilter{Ephemeral: event.LoopScope{All: true}, Enduring: event.LoopScope{All: true}})
			if err != nil {
				t.Fatalf("Subscribe() error = %v", err)
			}
			defer func() { _ = stream.Close() }()
			if _, err := agent.Submit(context.Background(), []content.Block{&content.TextBlock{Text: "first"}}); err != nil {
				t.Fatalf("first Submit() error = %v", err)
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
	return "coderig test: compaction terminal append failed"
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
				streamSteps: []fakeStreamStep{{chunks: []content.Chunk{&content.TextChunk{Text: "reply"}}}},
				invokeSteps: []fakeInvokeStep{{
					respond: acceptanceCompactionResponse(summary, content.Usage{OutputTokens: 1}),
					entered: invokeEntered, release: invokeRelease,
				}},
			}
			root := t.TempDir()
			access, cfg := headlessTestAccess(t, Config{}, root)
			definitions, err := swarmDefinitions(client, testModel(), cfg, access)
			if err != nil {
				t.Fatalf("swarmDefinitions() error = %v", err)
			}
			assembly, err := buildRig(definitions, stores, root, cfg, false)
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
