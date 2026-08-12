//go:build integration

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
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/inference"
	stream "github.com/looprig/inference/stream"
)

func TestCompactionRestoreUsesSummaryAndRetainsPrivilegedAudit(t *testing.T) {
	const summary = `<conversation_summary><goal>restore work</goal><constraints/><decisions/><state>first turn compacted</state><open_items>continue</open_items></conversation_summary>`
	tests := []struct {
		name string
	}{
		{name: "restored runtime starts at latest summary while journal retains superseded and internal records"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workspace := t.TempDir()
			t.Chdir(workspace)
			factory := newIntegrationFactory(t)
			originalClient := &fakeLLM{
				streamSteps: []fakeStreamStep{
					{chunks: []content.Chunk{&content.TextChunk{Text: "first reply"}}},
					{chunks: []content.Chunk{&content.TextChunk{Text: "retained reply one"}}},
					{chunks: []content.Chunk{&content.TextChunk{Text: "retained reply two"}}},
					{chunks: []content.Chunk{&content.TextChunk{Text: "post-compaction reply"}}},
				},
				invokeSteps: []fakeInvokeStep{{respond: acceptanceCompactionResponse(summary, content.Usage{OutputTokens: 1})}},
			}
			agent, err := factory.openWithClient(context.Background(), originalClient, newModelFactoryFor(testModel()), SessionSelector{}, Config{})
			if err != nil {
				t.Fatalf("openWithClient(new) error = %v", err)
			}
			sessionID := agent.SessionID()
			stream, err := agent.Subscribe(event.EventFilter{Enduring: event.LoopScope{All: true}})
			if err != nil {
				t.Fatalf("Subscribe() error = %v", err)
			}
			if _, err := agent.Submit(context.Background(), []content.Block{&content.TextBlock{Text: "first user"}}); err != nil {
				t.Fatalf("first Submit() error = %v", err)
			}
			firstEvents := acceptanceEventsUntil(t, stream, func(ev event.Event) bool { _, ok := ev.(event.TurnDone); return ok })
			if firstMessages := acceptanceMessagesJSON(t, acceptanceTurnMessages(firstEvents)); len(firstMessages) < 2 || !strings.Contains(strings.Join(firstMessages, "\n"), "first user") {
				t.Fatalf("first completed segment = %v, want first user and reply", firstMessages)
			}
			retainedOneEvents := acceptanceSubmitTurn(t, agent, stream, "retained user one")
			retainedTwoEvents := acceptanceSubmitTurn(t, agent, stream, "retained user two")
			wantRetained := append(acceptanceTurnMessages(retainedOneEvents), acceptanceTurnMessages(retainedTwoEvents)...)
			if _, err := agent.CompactToLoop(context.Background(), agent.ActiveLoopID()); err != nil {
				t.Fatalf("CompactToLoop() error = %v", err)
			}
			compactionEvents := acceptanceEventsUntil(t, stream, func(ev event.Event) bool { _, ok := ev.(event.CompactionCommitted); return ok })
			committed := compactionEvents[len(compactionEvents)-1].(event.CompactionCommitted)
			if err := event.ValidateEvent(committed); err != nil {
				t.Fatalf("live CompactionCommitted validation error = %v", err)
			}
			if got := acceptanceMessagesJSON(t, committed.Retained); !reflect.DeepEqual(got, acceptanceMessagesJSON(t, wantRetained)) {
				t.Fatalf("live committed Retained = %v, want newest two segments %v", got, acceptanceMessagesJSON(t, wantRetained))
			}
			if _, err := agent.Submit(context.Background(), []content.Block{&content.TextBlock{Text: "post-compaction user"}}); err != nil {
				t.Fatalf("post-compaction Submit() error = %v", err)
			}
			postCompactionEvents := acceptanceEventsUntil(t, stream, func(ev event.Event) bool { _, ok := ev.(event.TurnDone); return ok })
			postCompactionMessages := acceptanceTurnMessages(postCompactionEvents)
			liveStreamRequests, liveInvokes := originalClient.capturedRequests()
			if len(liveInvokes) != 1 || len(liveStreamRequests) != 4 {
				t.Fatalf("live Stream/Invoke requests = %d/%d, want 4/1", len(liveStreamRequests), len(liveInvokes))
			}
			livePostRequest := liveStreamRequests[len(liveStreamRequests)-1]
			if livePostRequest.TransientMessages != 1 {
				t.Errorf("live post-compaction TransientMessages = %d, want 1", livePostRequest.TransientMessages)
			}
			if len(livePostRequest.Messages) != len(wantRetained)+3 {
				t.Errorf("live post-compaction messages = %d, want summary + retained tail + user + runtime", len(livePostRequest.Messages))
			}
			if got := acceptanceRuntimeMessageCount(t, livePostRequest); got != 1 {
				t.Errorf("live post-compaction runtime context count = %d, want 1", got)
			}
			if err := stream.Close(); err != nil {
				t.Fatalf("subscription Close() error = %v", err)
			}
			if err := agent.Close(context.Background()); err != nil {
				t.Fatalf("original Close() error = %v", err)
			}

			rawReplayer, err := factory.stores.session.OpenInternalEventReplayer(sessionID, sessionstore.ReplayRequest{})
			if err != nil {
				t.Fatalf("OpenInternalEventReplayer() error = %v", err)
			}
			rawEvents := drainEventReplay(t, rawReplayer)
			assertCompactionRawAudit(t, rawEvents)

			restoredClient := &fakeLLM{streamSteps: []fakeStreamStep{{chunks: []content.Chunk{&content.TextChunk{Text: "third reply"}}}}}
			restored, err := factory.openWithClient(
				context.Background(), restoredClient, newModelFactoryFor(testModel()),
				SessionSelector{Resume: sessionID}, Config{},
			)
			if err != nil {
				t.Fatalf("openWithClient(restore) error = %v", err)
			}
			defer func() { _ = restored.Close(context.Background()) }()
			backlog, err := restored.ReplayBacklog(context.Background())
			if err != nil {
				t.Fatalf("ReplayBacklog() error = %v", err)
			}
			for _, ev := range backlog {
				switch ev.(type) {
				case event.HustleStarted, event.HustleCompleted, event.HustleFailed:
					t.Errorf("public restore backlog exposed internal audit event %T", ev)
				}
			}
			if !hasType(backlog, event.CompactionCommitted{}) {
				t.Errorf("public restore backlog missing CompactionCommitted: %v", typeNames(backlog))
			}
			var restoredCommitted event.CompactionCommitted
			for _, ev := range backlog {
				if value, ok := ev.(event.CompactionCommitted); ok {
					restoredCommitted = value
				}
			}
			if acceptanceMessageText(t, restoredCommitted.Summary) != acceptanceMessageText(t, committed.Summary) ||
				!reflect.DeepEqual(acceptanceMessagesJSON(t, restoredCommitted.Retained), acceptanceMessagesJSON(t, committed.Retained)) ||
				restoredCommitted.Basis != committed.Basis || restoredCommitted.PostContext != committed.PostContext {
				t.Errorf("restored CompactionCommitted differs from live: restored basis/post=%+v/%+v retained=%v; live basis/post=%+v/%+v retained=%v", restoredCommitted.Basis, restoredCommitted.PostContext, acceptanceMessagesJSON(t, restoredCommitted.Retained), committed.Basis, committed.PostContext, acceptanceMessagesJSON(t, committed.Retained))
			}
			firstTerminalPreserved := false
			for _, ev := range backlog {
				done, ok := ev.(event.TurnDone)
				if ok && acceptanceMessageText(t, done.Message) == "first reply" {
					firstTerminalPreserved = true
				}
			}
			if !firstTerminalPreserved {
				t.Errorf("public restore backlog lost superseded terminal response: %v", typeNames(backlog))
			}

			restoredStream, err := restored.Subscribe(event.EventFilter{Enduring: event.LoopScope{All: true}})
			if err != nil {
				t.Fatalf("restored Subscribe() error = %v", err)
			}
			defer func() { _ = restoredStream.Close() }()
			if _, err := restored.Submit(context.Background(), []content.Block{&content.TextBlock{Text: "restored next user"}}); err != nil {
				t.Fatalf("restored Submit() error = %v", err)
			}
			acceptanceEventsUntil(t, restoredStream, func(ev event.Event) bool { _, ok := ev.(event.TurnDone); return ok })
			streamRequests, invokes := restoredClient.capturedRequests()
			if len(invokes) != 0 || len(streamRequests) != 1 {
				t.Fatalf("restored Stream/Invoke requests = %d/%d, want 1/0", len(streamRequests), len(invokes))
			}
			restoredRequest := streamRequests[0]
			if restoredRequest.TransientMessages != 1 {
				t.Errorf("restored request TransientMessages = %d, want 1", restoredRequest.TransientMessages)
			}
			if got := acceptanceRuntimeMessageCount(t, restoredRequest); got != 1 {
				t.Errorf("restored request runtime context count = %d, want 1", got)
			}
			texts := make([]string, 0, len(restoredRequest.Messages))
			for _, message := range restoredRequest.Messages {
				if acceptanceMessageContainsRuntimeContext(message) {
					texts = append(texts, "<runtime_context>")
					continue
				}
				texts = append(texts, acceptanceMessageText(t, message))
			}
			joined := strings.Join(texts, "\n")
			restoredTail := append(append(content.AgenticMessages(nil), wantRetained...), postCompactionMessages...)
			if len(texts) != len(restoredTail)+3 || texts[0] != summary || !strings.Contains(joined, "retained user one") ||
				!strings.Contains(joined, "retained reply one") || !strings.Contains(joined, "retained user two") ||
				!strings.Contains(joined, "retained reply two") || !strings.Contains(joined, "post-compaction user") ||
				!strings.Contains(joined, "post-compaction reply") || !strings.Contains(joined, "restored next user") {
				t.Errorf("restored request context = %q, want summary plus exact retained/post-compaction tail and new input", texts)
			}
			if strings.Contains(joined, "first user") || strings.Contains(joined, "first reply") {
				t.Errorf("restored runtime context retained superseded pre-compaction text: %q", texts)
			}
			for i, retained := range restoredTail {
				gotJSON, err := json.Marshal(restoredRequest.Messages[i+1])
				if err != nil {
					t.Fatalf("json.Marshal(restored retained message[%d]) error = %v", i, err)
				}
				wantJSON, err := json.Marshal(retained)
				if err != nil {
					t.Fatalf("json.Marshal(expected retained message[%d]) error = %v", i, err)
				}
				if !bytes.Equal(gotJSON, wantJSON) {
					t.Errorf("restored request retained message[%d] = %s, want %s", i, gotJSON, wantJSON)
				}
			}
		})
	}
}

// provenanceClassifierClient captures the real command-safety wire input while
// delegating the strict response contract to the existing classifier fixture.
// Carbon's ordinary/compaction lane remains the shared fakeLLM fixture.
type provenanceClassifierClient struct {
	mu               sync.Mutex
	delegate         *scriptedClassifierClient
	classifierInputs []string
}

// openCompactionPermissionAgent mirrors the existing live permission-review
// acceptance helper: the Carbon definition and classifier registration use
// AccessTrusted, while the actual interactive access binding uses
// AccessReadOnly so Bash opens a real permission gate. The durable factory
// stores are reused for the restore half of the test.
func openCompactionPermissionAgent(t *testing.T, factory *SessionStoreFactory, agentClient, classifierClient inference.Client, cfg Config, root string, selector SessionSelector) *RuntimeAgent {
	t.Helper()
	accessCfg := cfg
	accessCfg.AccessProfile = AccessReadOnly
	access, err := buildSessionAccess(accessCfg, root, true)
	if err != nil {
		t.Fatalf("buildSessionAccess(split profile) error = %v", err)
	}
	t.Cleanup(func() { _ = access.Close() })
	cfg.AccessConfigRev = access.configRev
	definition, err := carbonTestDefinition(agentClient, testModel(), cfg, access)
	if err != nil {
		t.Fatalf("carbonTestDefinition(split profile) error = %v", err)
	}
	permissionReview, err := newPermissionReviewRegistration(cfg, classifierClient)
	if err != nil {
		t.Fatalf("newPermissionReviewRegistration(split profile) error = %v", err)
	}
	adapter, err := openSessionWithDefinition(context.Background(), definition, cfg, factory.stores, root, selector, permissionReview)
	if err != nil {
		t.Fatalf("openSessionWithDefinition(split profile) error = %v", err)
	}
	agent := newRuntimeAgentWithPrimerCandidates(adapter, adapter.Controller(), root, access, "", nil, nil)
	t.Cleanup(func() { _ = agent.Close(context.Background()) })
	return agent
}

func (c *provenanceClassifierClient) Invoke(ctx context.Context, request inference.Request) (*inference.Response, error) {
	if len(request.Messages) != 1 {
		return nil, errors.New("provenanceClassifierClient: request missing sole input")
	}
	user, ok := request.Messages[0].(*content.UserMessage)
	if !ok || len(user.Blocks) != 1 {
		return nil, errors.New("provenanceClassifierClient: request malformed")
	}
	block, ok := user.Blocks[0].(*content.TextBlock)
	if !ok || block == nil {
		return nil, errors.New("provenanceClassifierClient: request is not text")
	}
	c.mu.Lock()
	c.classifierInputs = append(c.classifierInputs, block.Text)
	c.mu.Unlock()
	return c.delegate.Invoke(ctx, request)
}

func (c *provenanceClassifierClient) Stream(ctx context.Context, request inference.Request) (*stream.StreamReader[content.Chunk], error) {
	return c.delegate.Stream(ctx, request)
}

func (c *provenanceClassifierClient) inputs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.classifierInputs...)
}

type compactionClassifierContext struct {
	Context struct {
		Entries []struct {
			Origin    string `json:"origin"`
			Kind      string `json:"kind"`
			Content   string `json:"content"`
			Truncated bool   `json:"truncated"`
		} `json:"entries"`
	} `json:"context"`
}

func assertCompactionPermissionProvenance(t *testing.T, label, input, summary string) []string {
	t.Helper()
	var decoded compactionClassifierContext
	if err := json.Unmarshal([]byte(input), &decoded); err != nil {
		t.Fatalf("%s classifier input JSON error = %v", label, err)
	}
	var retained []string
	summaryEntries := 0
	for _, entry := range decoded.Context.Entries {
		if strings.Contains(entry.Content, "summary-only authority") {
			summaryEntries++
			if entry.Origin != "runtime" || entry.Kind != "runtime_context" || entry.Truncated {
				t.Errorf("%s summary authority entry = %+v, want untruncated runtime/runtime_context", label, entry)
			}
		}
		for _, user := range []string{"retained user one", "retained user two"} {
			if strings.Contains(entry.Content, user) {
				if entry.Origin != "user" || entry.Kind != "user_message" || entry.Truncated {
					t.Errorf("%s retained user %q authority entry = %+v, want untruncated user/user_message", label, user, entry)
				}
				retained = append(retained, user+"|"+entry.Origin+"|"+entry.Kind+"|"+entry.Content)
			}
		}
		if strings.Contains(entry.Content, summary) && entry.Origin == "user" {
			t.Errorf("%s summary was escalated to human authority: %+v", label, entry)
		}
	}
	if summaryEntries == 0 {
		t.Errorf("%s classifier context omitted the compaction summary", label)
	}
	if len(retained) != 2 {
		t.Errorf("%s classifier context retained user authority entries = %v, want both retained users", label, retained)
	}
	return retained
}

func TestCompactionRestorePreservesPermissionReviewProvenance(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	workspace := t.TempDir()
	t.Chdir(workspace)
	const summary = `<conversation_summary><goal>summary-only authority</goal><constraints/><decisions/><state>compacted</state><open_items>retain genuine users</open_items></conversation_summary>`
	agentClient := &fakeLLM{
		streamSteps: []fakeStreamStep{
			{chunks: []content.Chunk{&content.TextChunk{Text: "head reply"}}},
			{chunks: []content.Chunk{&content.TextChunk{Text: "retained reply one"}}},
			{chunks: []content.Chunk{&content.TextChunk{Text: "retained reply two"}}},
			{chunks: bashToolCall("compaction-permission-live", "echo compaction-provenance-live")},
			{chunks: []content.Chunk{&content.TextChunk{Text: "live permission reply"}}},
			{chunks: bashToolCall("compaction-permission-restored", "echo compaction-provenance-restored")},
			{chunks: []content.Chunk{&content.TextChunk{Text: "restored permission reply"}}},
		},
		invokeSteps: []fakeInvokeStep{{respond: acceptanceCompactionResponse(summary, content.Usage{InputTokens: 13, OutputTokens: 2})}},
	}
	classifierClient := &provenanceClassifierClient{delegate: newScriptedClassifierClient()}
	cfg := Config{AccessProfile: AccessTrusted, PermissionReviewEnabled: true, PermissionReviewModel: permissionReviewTestModel()}
	factory := newIntegrationFactory(t)
	agent := openCompactionPermissionAgent(t, factory, agentClient, classifierClient, cfg, workspace, SessionSelector{})
	sessionID := agent.SessionID()
	streamEvents, err := agent.Subscribe(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	acceptanceSubmitTurn(t, agent, streamEvents, "old head user")
	acceptanceSubmitTurn(t, agent, streamEvents, "retained user one")
	acceptanceSubmitTurn(t, agent, streamEvents, "retained user two")
	if _, err := agent.CompactToLoop(context.Background(), agent.ActiveLoopID()); err != nil {
		t.Fatalf("CompactToLoop() error = %v", err)
	}
	compactionEvents := acceptanceEventsUntil(t, streamEvents, func(ev event.Event) bool { _, ok := ev.(event.CompactionCommitted); return ok })
	committed := compactionEvents[len(compactionEvents)-1].(event.CompactionCommitted)
	if err := event.ValidateEvent(committed); err != nil {
		t.Fatalf("live provenance CompactionCommitted validation error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := agent.Submit(ctx, []content.Block{&content.TextBlock{Text: "run live permission command"}}); err != nil {
		t.Fatalf("live permission Submit() error = %v", err)
	}
	gateID, _ := permissionGateWait(t, ctx, streamEvents, 10*time.Second)
	if err := waitForClassifierGateResolution(t, ctx, streamEvents, gateID, 10*time.Second); err != nil {
		t.Logf("live classifier inputs captured before resolution failure: %v", classifierClient.inputs())
		t.Fatalf("live permission gate did not auto-resolve: %v", err)
	}
	mustTurnDone(t, drainToTurnTerminal(t, ctx, streamEvents))
	liveInputs := classifierClient.inputs()
	if len(liveInputs) != 1 {
		t.Fatalf("live classifier inputs = %d, want exactly one", len(liveInputs))
	}
	liveRetained := assertCompactionPermissionProvenance(t, "live", liveInputs[0], summary)
	if err := streamEvents.Close(); err != nil {
		t.Fatalf("live subscription Close() error = %v", err)
	}
	if err := agent.Close(context.Background()); err != nil {
		t.Fatalf("live agent Close() error = %v", err)
	}

	restored := openCompactionPermissionAgent(t, factory, agentClient, classifierClient, cfg, workspace, SessionSelector{Resume: sessionID})
	backlog, err := restored.ReplayBacklog(context.Background())
	if err != nil {
		t.Fatalf("restored ReplayBacklog() error = %v", err)
	}
	var restoredCommitted event.CompactionCommitted
	for _, ev := range backlog {
		if value, ok := ev.(event.CompactionCommitted); ok {
			restoredCommitted = value
		}
	}
	if acceptanceMessageText(t, restoredCommitted.Summary) != acceptanceMessageText(t, committed.Summary) ||
		!reflect.DeepEqual(acceptanceMessagesJSON(t, restoredCommitted.Retained), acceptanceMessagesJSON(t, committed.Retained)) ||
		restoredCommitted.Basis != committed.Basis || restoredCommitted.PostContext != committed.PostContext {
		t.Fatalf("restored compaction terminal changed summary/retained/basis: live=%+v/%+v/%v restored=%+v/%+v/%v", committed.Basis, committed.PostContext, acceptanceMessagesJSON(t, committed.Retained), restoredCommitted.Basis, restoredCommitted.PostContext, acceptanceMessagesJSON(t, restoredCommitted.Retained))
	}
	restoredStream, err := restored.Subscribe(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatalf("restored Subscribe() error = %v", err)
	}
	defer func() { _ = restoredStream.Close() }()
	if _, err := restored.Submit(ctx, []content.Block{&content.TextBlock{Text: "run restored permission command"}}); err != nil {
		t.Fatalf("restored permission Submit() error = %v", err)
	}
	restoredGateID, _ := permissionGateWait(t, ctx, restoredStream, 10*time.Second)
	if err := waitForClassifierGateResolution(t, ctx, restoredStream, restoredGateID, 10*time.Second); err != nil {
		t.Fatalf("restored permission gate did not auto-resolve: %v", err)
	}
	mustTurnDone(t, drainToTurnTerminal(t, ctx, restoredStream))
	allInputs := classifierClient.inputs()
	if len(allInputs) != 2 {
		t.Fatalf("live/restored classifier inputs = %d, want two", len(allInputs))
	}
	restoredRetained := assertCompactionPermissionProvenance(t, "restored", allInputs[1], summary)
	if !reflect.DeepEqual(liveRetained, restoredRetained) {
		t.Errorf("retained authority entries changed across restore: live=%v restored=%v", liveRetained, restoredRetained)
	}
}

func assertCompactionRawAudit(t *testing.T, events []event.Event) {
	t.Helper()
	wants := []struct {
		name  string
		found bool
	}{
		{name: "superseded first TurnStarted"},
		{name: "superseded first StepDone"},
		{name: "internal HustleStarted"},
		{name: "internal HustleCompleted"},
		{name: "public CompactionCommitted"},
	}
	turns := 0
	steps := 0
	for _, ev := range events {
		switch ev.(type) {
		case event.TurnStarted:
			turns++
		case event.StepDone:
			steps++
		case event.HustleStarted:
			wants[2].found = true
		case event.HustleCompleted:
			wants[3].found = true
		case event.CompactionCommitted:
			wants[4].found = true
		}
	}
	wants[0].found = turns >= 2
	wants[1].found = steps >= 2
	for _, want := range wants {
		if !want.found {
			t.Errorf("privileged raw journal missing %s: %v", want.name, typeNames(events))
		}
	}
}
