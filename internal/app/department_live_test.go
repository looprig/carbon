package app

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/looprig/core/content"
	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/session"
	"github.com/looprig/host"
)

func TestCarbonHostEnablesTextAndReasoningPreviews(t *testing.T) {
	options := carbonHostLiveTextOptions()
	if options == nil || !options.IncludeReasoning {
		t.Fatalf("Carbon Host live options = %+v, want text with reasoning", options)
	}
	var _ *host.LiveTextOptions = options
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
	select {
	case item := <-stream:
		if item.Ephemeral == nil || !bytes.Contains(item.Ephemeral.Body, []byte("visible")) || bytes.Contains(item.Ephemeral.Body, []byte("private reasoning")) {
			t.Fatalf("first preview = %+v, want only visible text", item)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no text preview")
	}
}

func TestCarbonLiveStreamTerminatesOnMissingCommittedFields(t *testing.T) {
	controller := &liveTestController{subscription: &liveTestSubscription{events: make(chan event.Delivery, 1)}}
	runtime := &carbonRuntime{controller: controller, scope: LaunchScope{TenantID: "tenant-a", SessionID: "session-a"}}
	stream, err := runtime.SubscribeLivePublic(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	controller.subscription.events <- event.Delivery{Event: event.TurnStarted{}, JournalSeq: 9}
	select {
	case item := <-stream:
		if item.Terminal == nil || item.Enduring != nil {
			t.Fatalf("missing committed fields yielded %+v", item)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("missing committed fields did not terminate the stream")
	}
}
