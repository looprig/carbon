package browser_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	centrifugego "github.com/centrifugal/centrifuge-go"
	"github.com/looprig/carbon/browser"
	carbon "github.com/looprig/carbon/internal/app"
	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/inference"
	"github.com/looprig/inference/model"
	"github.com/looprig/llm"
	"github.com/looprig/sessionstore"
)

// TestBrowserBodiesCarryOnlyPublicIdentities is release audit R5.2's H1 (and
// M1) regression, through Carbon's real browser composition: Factory, the
// pooled Host and a real harness runtime.
//
// A harness public body names the RUNTIME session id on every event and the
// RUNTIME command id on every command-caused event; both are private. Neither
// may reach a browser — not on the live tail Host relays, not from /journal
// Factory serves. Both must instead carry the public session id, and a
// command-caused event's cause.command_id must be the CommandID the client
// admitted. The model endpoint (base_url) and the Host's physical workspace
// path must not reach a browser either (harness v0.39.1).
//
// The private ids are read from the durable stores after the server stops,
// because nothing a browser can reach will name them.
func TestBrowserBodiesCarryOnlyPublicIdentities(t *testing.T) {
	const (
		publicSession = sessionwire.SessionID("browser-session-1")
		createCommand = sessionwire.CommandID("privacy-create-1")
		inputCommand  = sessionwire.CommandID("privacy-input-2")
		modelBaseURL  = "http://localhost:1234/v1"
	)
	cfg := browserFixture(t)
	cfg.Factory.ReconcileLimits.Interval = 25 * time.Millisecond
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	cfg.ClientBuilder = func() (inference.Client, func() model.Model, error) {
		return client{release: release, replies: &atomic.Int32{}}, func() model.Model {
			return model.CustomModel(model.ProviderName(llm.ProviderLMStudio), model.APIFormatOpenAI,
				modelBaseURL, "browser-test", model.WithTools(),
				model.WithContextLimits(model.ContextLimits{WindowTokens: 128_000}))
		}, nil
	}
	s, err := browser.Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Start = %v", err)
	}
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			_ = s.Stop(context.Background())
		}
	})
	base := "http://" + s.Addr().String()
	request := func(method, path string, body any) (int, []byte) {
		t.Helper()
		var reader io.Reader
		if body != nil {
			encoded, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			reader = bytes.NewReader(encoded)
		}
		r, err := http.NewRequest(method, base+path, reader)
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer browser-test-token")
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "http://127.0.0.1")
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, data
	}

	create := sessionwire.CreateRequest{CommandEnvelope: sessionwire.CommandEnvelope{Version: sessionwire.CurrentWireVersion, CommandID: createCommand},
		SessionID: publicSession, AgentID: "carbon", Blocks: json.RawMessage(`[{"type":"text","text":"hello"}]`)}
	if status, body := request(http.MethodPost, "/v1/sessions", create); status != http.StatusCreated {
		t.Fatalf("create = %d %s", status, body)
	}
	viewer, live := captureAllLive(t, base)
	defer viewer.Close()
	releaseOnce.Do(func() { close(release) })
	awaitJournalContains(t, request, "browser reply 1")
	// Wait until Factory has bound the viewer's live tail (it announces the bind
	// with a session.reset); a command admitted after that has its effect relayed
	// LIVE.
	live.awaitReset(t)
	input := sessionwire.InputRequest{CommandEnvelope: sessionwire.CommandEnvelope{Version: sessionwire.CurrentWireVersion, CommandID: inputCommand},
		SessionID: publicSession, Blocks: json.RawMessage(`[{"type":"text","text":"again"}]`)}
	if status, body := request(http.MethodPost, "/v1/sessions/browser-session-1/input", input); status != http.StatusOK {
		t.Fatalf("input = %d %s", status, body)
	}
	awaitJournalContains(t, request, "browser reply 2")
	liveBodies := live.await(t, func(bodies []json.RawMessage) bool {
		caused, replied := false, false
		for _, body := range bodies {
			caused = caused || causeCommand(t, body) == string(inputCommand)
			replied = replied || strings.Contains(string(body), "browser reply 2")
		}
		return caused && replied
	})
	status, journalBody := request(http.MethodGet, "/v1/sessions/browser-session-1/journal?limit=500", nil)
	if status != http.StatusOK {
		t.Fatalf("journal = %d %s", status, journalBody)
	}
	var page sessionwire.JournalPage
	if err := json.Unmarshal(journalBody, &page); err != nil {
		t.Fatal(err)
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	stopped = true

	// The private identities, from the durable stores.
	stores, err := carbon.OpenServeStorage(context.Background(), carbon.Config{HomeDir: cfg.Runtime.HomeDir},
		carbon.ServeStorageConfig{DataDir: cfg.Storage.DataDir, DefaultTenant: cfg.Storage.DefaultTenant})
	if err != nil {
		t.Fatal(err)
	}
	defer stores.Close(context.Background())
	entry, err := stores.ControlStore().GetCatalogEntry(context.Background(), sessionstore.GetCatalogEntryRequest{TenantID: "local", SessionID: publicSession})
	if err != nil {
		t.Fatal(err)
	}
	private := []string{entry.Record.Binding.RuntimeSessionID}
	for _, command := range []sessionwire.CommandID{createCommand, inputCommand} {
		record, err := stores.ControlStore().GetDispositionCommand(context.Background(), sessionstore.GetDispositionCommandRequest{
			TenantID: "local", SessionID: publicSession, CommandID: command})
		if err != nil {
			t.Fatal(err)
		}
		private = append(private, string(record.Record.Descriptor.RuntimeCommandID))
	}
	for _, id := range private {
		if id == "" || id == string(publicSession) {
			t.Fatalf("private id %q cannot be told apart from a public one", id)
		}
	}
	forbidden := append([]string{"base_url", modelBaseURL, cfg.Storage.DataDir, "session-workspaces"}, private...)
	if resolved, err := filepath.EvalSymlinks(cfg.Storage.DataDir); err == nil {
		forbidden = append(forbidden, resolved)
	}

	var journalBodies []json.RawMessage
	for _, event := range page.Events {
		journalBodies = append(journalBodies, event.Body)
	}
	for name, bodies := range map[string][]json.RawMessage{"/journal": journalBodies, "live tail": liveBodies} {
		if len(bodies) == 0 {
			t.Fatalf("%s delivered no bodies", name)
		}
		publicNamed := false
		for _, body := range bodies {
			for _, secret := range forbidden {
				if strings.Contains(string(body), secret) {
					t.Errorf("%s body carries %q: %s", name, secret, body)
				}
			}
			if strings.Contains(string(body), `"session_id":"`+string(publicSession)+`"`) {
				publicNamed = true
			}
			if cause := causeCommand(t, body); cause != "" && cause != string(createCommand) && cause != string(inputCommand) {
				t.Errorf("%s body names cause.command_id %q, not an admitted CommandID: %s", name, cause, body)
			}
		}
		if !publicNamed {
			t.Errorf("no %s body names the public session id", name)
		}
	}
	if !anyCause(t, journalBodies, createCommand) || !anyCause(t, journalBodies, inputCommand) {
		t.Error("/journal has no event caused by an admitted command naming its public CommandID")
	}
	if !anyCause(t, liveBodies, inputCommand) {
		t.Error("the live tail has no event caused by the admitted input naming its public CommandID")
	}
}

// causeCommand returns a body's cause.command_id, or "".
func causeCommand(t *testing.T, body json.RawMessage) string {
	t.Helper()
	var decoded struct {
		Cause *struct {
			CommandID string `json:"command_id"`
		} `json:"cause"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil || decoded.Cause == nil {
		return ""
	}
	return decoded.Cause.CommandID
}

func anyCause(t *testing.T, bodies []json.RawMessage, command sessionwire.CommandID) bool {
	t.Helper()
	for _, body := range bodies {
		if causeCommand(t, body) == string(command) {
			return true
		}
	}
	return false
}

func awaitJournalContains(t *testing.T, request func(string, string, any) (int, []byte), want string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		status, body := request(http.MethodGet, "/v1/sessions/browser-session-1/journal?limit=500", nil)
		if status == http.StatusOK && strings.Contains(string(body), want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("journal never recorded %q: %d %s", want, status, body)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// liveBodies collects the body of every enduring publication a viewer sees.
type liveBodies struct {
	mu     sync.Mutex
	bodies []json.RawMessage
	resets int
}

func (l *liveBodies) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.resets++
}

func (l *liveBodies) awaitReset(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		l.mu.Lock()
		resets := l.resets
		l.mu.Unlock()
		if resets > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the viewer's live tail was never bound (no session.reset)")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func (l *liveBodies) add(body json.RawMessage) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.bodies = append(l.bodies, append(json.RawMessage(nil), body...))
}

func (l *liveBodies) await(t *testing.T, done func([]json.RawMessage) bool) []json.RawMessage {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		l.mu.Lock()
		snapshot := append([]json.RawMessage(nil), l.bodies...)
		l.mu.Unlock()
		if done(snapshot) {
			return snapshot
		}
		if time.Now().After(deadline) {
			t.Fatalf("the live tail never delivered the expected records; got %d bodies", len(snapshot))
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func captureAllLive(t *testing.T, base string) (*centrifugego.Client, *liveBodies) {
	t.Helper()
	viewer := centrifugego.NewJsonClient("ws"+strings.TrimPrefix(base, "http")+"/v1/realtime", centrifugego.Config{
		Token: "browser-test-token", Data: []byte(`{"protocol_version":"1"}`),
		Header:           http.Header{"Authorization": {"Bearer browser-test-token"}, "Origin": {"http://127.0.0.1"}},
		HandshakeTimeout: 10 * time.Second, LogLevel: centrifugego.LogLevelNone,
	})
	connected := make(chan struct{}, 1)
	viewer.OnConnected(func(centrifugego.ConnectedEvent) {
		select {
		case connected <- struct{}{}:
		default:
		}
	})
	if err := viewer.Connect(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-connected:
	case <-time.After(10 * time.Second):
		t.Fatal("viewer did not connect")
	}
	sub, err := viewer.NewSubscription("session:local:browser-session-1")
	if err != nil {
		t.Fatal(err)
	}
	collected := &liveBodies{}
	subscribed := make(chan struct{}, 1)
	sub.OnSubscribed(func(centrifugego.SubscribedEvent) {
		select {
		case subscribed <- struct{}{}:
		default:
		}
	})
	sub.OnPublication(func(event centrifugego.PublicationEvent) {
		var publication sessionwire.EnduringPublication
		if publication.UnmarshalJSON(event.Data) == nil {
			collected.add(publication.Body)
			return
		}
		var reset sessionwire.SessionReset
		if reset.UnmarshalJSON(event.Data) == nil {
			collected.reset()
		}
	})
	if err := sub.Subscribe(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-subscribed:
	case <-time.After(10 * time.Second):
		t.Fatal("viewer did not subscribe")
	}
	return viewer, collected
}
