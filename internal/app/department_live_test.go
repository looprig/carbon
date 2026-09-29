package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/looprig/core/content"
	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/session"
	"github.com/looprig/host"
	"github.com/looprig/host/department"
)

func TestCarbonHostEnablesTextReasoningAndToolStepPreviews(t *testing.T) {
	options := carbonHostLiveTextOptions()
	if options == nil || !options.IncludeReasoning || !options.IncludeToolSteps {
		t.Fatalf("Carbon Host live options = %+v, want text with reasoning and tool steps", options)
	}
	var _ *host.LiveTextOptions = options
}

// toolStepDeliveries are one tool call's two ephemeral events, as harness
// v0.42.0 emits them: joined to the committed StepDone by tool_use_id.
func toolStepDeliveries(t *testing.T) (event.Delivery, event.Delivery) {
	t.Helper()
	header := event.Header{Coordinates: identity.Coordinates{SessionID: mustUUIDForTest(t), LoopID: mustUUIDForTest(t), TurnID: mustUUIDForTest(t), StepID: mustUUIDForTest(t)}}
	execution := mustUUIDForTest(t)
	started := event.ToolCallStarted{Header: header, ToolExecutionID: execution, ToolUseID: "toolu_live", ToolName: "Bash", Summary: "ls -la"}
	completed := event.ToolCallCompleted{Header: header, ToolExecutionID: execution, ToolUseID: "toolu_live", ToolName: "Bash", ElapsedMillis: 42, ResultPreview: "total 0"}
	return event.Delivery{Event: started}, event.Delivery{Event: completed}
}

func nextLiveBody(t *testing.T, stream <-chan department.LivePublication) []byte {
	t.Helper()
	select {
	case item, ok := <-stream:
		if !ok {
			t.Fatal("live stream closed")
		}
		if item.Ephemeral != nil {
			return item.Ephemeral.Body
		}
		if item.Enduring != nil {
			return item.Enduring.Body
		}
		t.Fatalf("live stream yielded %+v", item)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for a live publication")
	}
	return nil
}

func TestCarbonLiveOptionsForwardToolSteps(t *testing.T) {
	controller := &liveTestController{subscription: &liveTestSubscription{events: make(chan event.Delivery, 4)}}
	runtime := &carbonRuntime{controller: controller, scope: LaunchScope{TenantID: "tenant-a", SessionID: "session-a"}}
	var subscriber department.LiveOptionsSubscriber = runtime
	stream, err := subscriber.SubscribeLivePublicWith(t.Context(), department.LiveOptions{IncludeToolSteps: true})
	if err != nil {
		t.Fatal(err)
	}
	started, completed := toolStepDeliveries(t)
	controller.subscription.events <- started
	controller.subscription.events <- completed
	for _, want := range [][]string{
		{`"type":"ToolCallStarted"`, `"tool_use_id":"toolu_live"`, `"tool_name":"Bash"`, `"summary":"ls -la"`},
		{`"type":"ToolCallCompleted"`, `"tool_use_id":"toolu_live"`, `"tool_name":"Bash"`, `"elapsed_ms":42`, `"result_preview":"total 0"`},
	} {
		body := nextLiveBody(t, stream)
		for _, member := range want {
			if !json.Valid(body) || !bytes.Contains(body, []byte(member)) {
				t.Fatalf("tool step body = %s, want %s", body, member)
			}
		}
	}
	if got := runtime.DroppedLivePreviews(); got != 0 {
		t.Fatalf("dropped previews = %d, want 0", got)
	}
}

// Without IncludeToolSteps a tool event crosses nothing (and is not a drop:
// it was never asked for), and reasoning follows its own option.
func TestCarbonLiveOptionsOmitUnrequestedClasses(t *testing.T) {
	controller := &liveTestController{subscription: &liveTestSubscription{events: make(chan event.Delivery, 8)}}
	runtime := &carbonRuntime{controller: controller, scope: LaunchScope{TenantID: "tenant-a", SessionID: "session-a"}}
	stream, err := runtime.SubscribeLivePublicWith(t.Context(), department.LiveOptions{IncludeReasoning: true})
	if err != nil {
		t.Fatal(err)
	}
	started, completed := toolStepDeliveries(t)
	header := event.Header{Coordinates: identity.Coordinates{SessionID: mustUUIDForTest(t), LoopID: mustUUIDForTest(t), TurnID: mustUUIDForTest(t)}}
	controller.subscription.events <- started
	controller.subscription.events <- completed
	controller.subscription.events <- event.Delivery{Event: event.TokenDelta{Header: header, Chunk: &content.ThinkingChunk{Thinking: "reason"}}}
	controller.subscription.events <- event.Delivery{Event: event.TokenDelta{Header: header, Chunk: &content.TextChunk{Text: "visible"}}}
	for _, want := range []string{"reason", "visible"} {
		if body := nextLiveBody(t, stream); !bytes.Contains(body, []byte(want)) || bytes.Contains(body, []byte("ToolCall")) {
			t.Fatalf("live body = %s, want %q and no tool step", body, want)
		}
	}
	if got := runtime.DroppedLivePreviews(); got != 0 {
		t.Fatalf("dropped previews = %d, want 0", got)
	}
}

// A tool event the projection refuses (no step id) is counted as a drop, not
// forwarded.
func TestCarbonLiveOptionsCountUnprojectableToolSteps(t *testing.T) {
	controller := &liveTestController{subscription: &liveTestSubscription{events: make(chan event.Delivery, 4)}}
	runtime := &carbonRuntime{controller: controller, scope: LaunchScope{TenantID: "tenant-a", SessionID: "session-a"}}
	stream, err := runtime.SubscribeLivePublicWith(t.Context(), department.LiveOptions{IncludeToolSteps: true})
	if err != nil {
		t.Fatal(err)
	}
	header := event.Header{Coordinates: identity.Coordinates{SessionID: mustUUIDForTest(t), LoopID: mustUUIDForTest(t), TurnID: mustUUIDForTest(t)}}
	controller.subscription.events <- event.Delivery{Event: event.ToolCallStarted{Header: header, ToolExecutionID: mustUUIDForTest(t), ToolName: "Bash"}}
	controller.subscription.events <- event.Delivery{Event: event.TokenDelta{Header: header, Chunk: &content.TextChunk{Text: "after"}}}
	if body := nextLiveBody(t, stream); !bytes.Contains(body, []byte("after")) {
		t.Fatalf("live body = %s, want the text after the refused tool step", body)
	}
	if got := runtime.DroppedLivePreviews(); got != 1 {
		t.Fatalf("dropped previews = %d, want the unprojectable tool step counted", got)
	}
}

type liveTestSubscription struct{ events chan event.Delivery }

func (s *liveTestSubscription) Events() <-chan event.Delivery { return s.events }
func (s *liveTestSubscription) Close() error                  { return nil }
func (s *liveTestSubscription) Err() error                    { return nil }

type liveTestController struct {
	session.SessionController
	subscription *liveTestSubscription
	filter       event.EventFilter
}

func (s *liveTestController) SubscribeEvents(filter event.EventFilter) (event.Subscription, error) {
	s.filter = filter
	return s.subscription, nil
}
func (s *liveTestController) CommittedPublicEvents() (session.CommittedPublicEventSource, bool) {
	return s, true
}
func (s *liveTestController) SubscribeCommittedPublicEvents(event.EventFilter) (event.Subscription, error) {
	return s.subscription, nil
}

func TestCarbonLivePublicationsCarryTextReasoningAndCommittedBytes(t *testing.T) {
	controller := &liveTestController{subscription: &liveTestSubscription{events: make(chan event.Delivery, 4)}}
	runtime := &carbonRuntime{controller: controller, scope: LaunchScope{TenantID: "tenant-a", SessionID: "session-a"}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stream, err := runtime.SubscribeLivePublicWithReasoning(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !controller.filter.Enduring.All || !controller.filter.Ephemeral.All {
		t.Fatal("live stream omitted a loop scope")
	}
	coordinates := identity.Coordinates{SessionID: mustUUIDForTest(t), LoopID: mustUUIDForTest(t), TurnID: mustUUIDForTest(t)}
	header := event.Header{Coordinates: coordinates}
	controller.subscription.events <- event.Delivery{Event: event.TokenDelta{Header: header, Chunk: &content.TextChunk{Text: "draft"}}}
	controller.subscription.events <- event.Delivery{Event: event.TokenDelta{Header: header, Chunk: &content.ThinkingChunk{Thinking: "reason"}}}
	committed := json.RawMessage(`{"type":"TurnStarted","text":"exact"}`)
	controller.subscription.events <- event.Delivery{Event: event.TurnStarted{Header: header}, EventID: "event-1", JournalSeq: 7, CoveredThrough: 7, PublicBody: committed}
	for _, want := range []string{"draft", "reason", "exact"} {
		select {
		case item := <-stream:
			var body []byte
			if item.Ephemeral != nil {
				body = item.Ephemeral.Body
			} else if item.Enduring != nil {
				body = item.Enduring.Body
			}
			if !json.Valid(body) || !bytes.Contains(body, []byte(want)) {
				t.Fatalf("publication = %+v, want %q", item, want)
			}
			if item.Enduring != nil && (item.Enduring.EventID != sessionwire.EventID("event-1") || item.Enduring.JournalSeq != 7) {
				t.Fatalf("committed identity changed: %+v", item.Enduring)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for %q", want)
		}
	}
}

func TestCarbonTextOnlyStreamDropsReasoningAndOversizedDeltas(t *testing.T) {
	controller := &liveTestController{subscription: &liveTestSubscription{events: make(chan event.Delivery, 4)}}
	runtime := &carbonRuntime{controller: controller, scope: LaunchScope{TenantID: "tenant-a", SessionID: "session-a"}}
	stream, err := runtime.SubscribeLivePublic(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	header := event.Header{Coordinates: identity.Coordinates{SessionID: mustUUIDForTest(t), LoopID: mustUUIDForTest(t), TurnID: mustUUIDForTest(t)}}
	controller.subscription.events <- event.Delivery{Event: event.TokenDelta{Header: header, Chunk: &content.ThinkingChunk{Thinking: "private reasoning"}}}
	controller.subscription.events <- event.Delivery{Event: event.TokenDelta{Header: header, Chunk: &content.TextChunk{Text: string(bytes.Repeat([]byte("x"), 2049))}}}
	controller.subscription.events <- event.Delivery{Event: event.TokenDelta{Header: header, Chunk: &content.TextChunk{Text: "visible"}}}
	deadline := time.After(3 * time.Second)
	for runtime.DroppedLivePreviews() != 1 {
		select {
		case <-deadline:
			t.Fatal("oversized preview was not counted")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	select {
	case item := <-stream:
		if item.Ephemeral == nil || !bytes.Contains(item.Ephemeral.Body, []byte("visible")) || bytes.Contains(item.Ephemeral.Body, []byte("private reasoning")) {
			t.Fatalf("first preview = %+v, want only visible text", item)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no text preview")
	}
}

func TestCarbonLiveStreamCountsBufferDrops(t *testing.T) {
	controller := &liveTestController{subscription: &liveTestSubscription{events: make(chan event.Delivery, 32)}}
	runtime := &carbonRuntime{controller: controller, scope: LaunchScope{TenantID: "tenant-a", SessionID: "session-a"}}
	_, err := runtime.SubscribeLivePublic(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	header := event.Header{Coordinates: identity.Coordinates{SessionID: mustUUIDForTest(t), LoopID: mustUUIDForTest(t), TurnID: mustUUIDForTest(t)}}
	for i := 0; i < 17; i++ {
		controller.subscription.events <- event.Delivery{Event: event.TokenDelta{Header: header, Chunk: &content.TextChunk{Text: "preview"}}}
	}
	deadline := time.After(3 * time.Second)
	for runtime.DroppedLivePreviews() != 1 {
		select {
		case <-deadline:
			t.Fatalf("buffer drops = %d, want 1", runtime.DroppedLivePreviews())
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

func TestCarbonLiveStreamTerminatesOnMissingCommittedFields(t *testing.T) {
	for _, tc := range []struct {
		name     string
		delivery event.Delivery
	}{
		{"missing public event ID", event.Delivery{Event: event.TurnStarted{}, JournalSeq: 9, PublicBody: json.RawMessage(`{"type":"TurnStarted"}`)}},
		{"missing committed body", event.Delivery{Event: event.TurnStarted{}, JournalSeq: 9, EventID: "event-9"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			controller := &liveTestController{subscription: &liveTestSubscription{events: make(chan event.Delivery, 2)}}
			runtime := &carbonRuntime{controller: controller, scope: LaunchScope{TenantID: "tenant-a", SessionID: "session-a"}}
			stream, err := runtime.SubscribeLivePublic(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			controller.subscription.events <- tc.delivery
			controller.subscription.events <- event.Delivery{Event: event.TurnStarted{}, JournalSeq: 10, EventID: "event-10", PublicBody: json.RawMessage(`{"type":"TurnStarted"}`)}
			select {
			case item := <-stream:
				if item.Terminal == nil || item.Enduring != nil {
					t.Fatalf("missing committed fields yielded %+v", item)
				}
				var missing *MissingCommittedPublicationError
				if !errors.As(item.Terminal, &missing) || missing.JournalSeq != 9 {
					t.Fatalf("terminal error = %v, want missing committed publication at sequence 9", item.Terminal)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("missing committed fields did not terminate the stream")
			}
			select {
			case _, ok := <-stream:
				if ok {
					t.Fatal("stream yielded another item after the terminal error")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("stream remained open after the terminal error")
			}
		})
	}
}
