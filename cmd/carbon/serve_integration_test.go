//go:build integration

package main

// serve_integration_test.go drives `carbon serve` end to end: a real CLI dispatch, a
// real process-lifetime rig over a real fsstore, a real loopback listener, and a real
// HTTP client. It carries the `integration` build tag because it binds a socket and
// writes durable state — carbon's own convention for process/filesystem/network/
// durable-storage boundaries (Makefile's test-integration target).
//
// The inference client is scripted (there is no network and no models.json), but
// NOTHING else is faked: the gate that opens is a real permission gate produced by
// Carbon's real trusted-profile access policy, and the answer travels the real
// harness gate route.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	carbon "github.com/looprig/carbon/internal/app"
	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	gatepkg "github.com/looprig/harness/pkg/gate"
	"github.com/looprig/inference"
	"github.com/looprig/inference/model"
	"github.com/looprig/inference/stream"
	"github.com/looprig/llm"
	"github.com/looprig/wui"
)

// --- the scripted inference client ------------------------------------------

// scriptedClient is a minimal inference.Client whose Stream is driven by a caller
// supplied script. It is deliberately NOT a copy of internal/app's fakeLLM: that type
// is a package-internal test fixture, and this package needs only "return these chunks
// for call N".
type scriptedClient struct {
	mu    sync.Mutex
	calls int
	fn    func(call int, req inference.Request) []content.Chunk
}

func (c *scriptedClient) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	return nil, fmt.Errorf("carbon serve test: Invoke is not scripted")
}

func (c *scriptedClient) Stream(_ context.Context, req inference.Request) (*stream.StreamReader[content.Chunk], error) {
	c.mu.Lock()
	call := c.calls
	c.calls++
	c.mu.Unlock()

	chunks := c.fn(call, req)
	index := 0
	next := func() (content.Chunk, error) {
		if index == len(chunks) {
			return nil, io.EOF
		}
		chunk := chunks[index]
		index++
		return chunk, nil
	}
	return stream.NewStreamReader(next, nil), nil
}

// testServeModel is a secret-free, tool-capable model descriptor. The scripted client
// never dials it; carbon needs a valid model to assemble the loop definition and a
// resolvable context window to compute occupancy.
func testServeModel() model.Model {
	return model.CustomModel(
		model.ProviderName(llm.ProviderLMStudio), model.APIFormatOpenAI,
		"http://localhost:1234/v1", "carbon-serve-test",
		model.WithTools(),
		model.WithContextLimits(model.ContextLimits{WindowTokens: 128_000}),
	)
}

// --- the fixture ------------------------------------------------------------

// serveFixture is one running `carbon serve` process composition.
type serveFixture struct {
	base   string // http://127.0.0.1:<port>
	client *http.Client

	csrfMu sync.Mutex
	csrf   string
}

// startTestServe boots `carbon serve` through the REAL CLI dispatch
// (runWithServeOpener) on an ephemeral loopback port, over a fresh workspace, a fresh
// durable store and a fresh Carbon home, with the inference client injected. It
// returns once the server has printed its resolved address, and tears the process
// composition down at cleanup.
func startTestServe(t *testing.T, script func(call int, req inference.Request) []content.Chunk) *serveFixture {
	t.Helper()

	workspace := t.TempDir()
	t.Chdir(workspace)
	home := t.TempDir()
	// LooprigHome resolves from HOME when Config.HomeDir is unset, and the CLI has no
	// flag for it, so the environment is the only seam the real dispatch offers.
	t.Setenv("HOME", home)
	dataDir := t.TempDir()

	open := func(ctx context.Context, cfg carbon.Config, dir string) (serveHostAPI, error) {
		return carbon.OpenServeHost(ctx, cfg, dir,
			carbon.WithServeInferenceClient(func() (inference.Client, carbon.ModelFactory, error) {
				selected := testServeModel()
				return &scriptedClient{fn: script}, func() model.Model { return selected }, nil
			}))
	}

	var out, errOut syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- runWithServeOpener(ctx, []string{
			"serve", "--addr", "127.0.0.1:0", "--data-dir", dataDir, "--access-profile", "trusted",
		}, open, &out, &errOut)
	}()
	// Registered FIRST so it runs LAST (t.Cleanup is LIFO): a diagnostic dump is only
	// complete once the server has finished shutting down and written its last line.
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("carbon serve stdout:\n%s\ncarbon serve stderr:\n%s", out.String(), errOut.String())
		}
	})
	t.Cleanup(func() {
		cancel()
		select {
		case code := <-done:
			if code != exitOK {
				t.Errorf("carbon serve exited %d, want %d (stderr: %s)", code, exitOK, errOut.String())
			}
		case <-time.After(20 * time.Second):
			t.Error("carbon serve did not exit within 20s of cancellation")
		}
	})
	printed := waitForSubstring(t, &out, "http://127.0.0.1:")
	base := strings.TrimSpace(printed[strings.Index(printed, "http://"):])
	return &serveFixture{base: base, client: &http.Client{Timeout: 30 * time.Second}}
}

// token fetches (once) and caches wui's CSRF token. Every control route is wrapped in
// wui's CSRFGuard, so a client that never mints one gets a permanent 403 on every
// state-changing request — the exact failure the SPA would hit.
func (f *serveFixture) token(t *testing.T) string {
	t.Helper()
	f.csrfMu.Lock()
	defer f.csrfMu.Unlock()
	if f.csrf != "" {
		return f.csrf
	}
	var body struct {
		Token string `json:"csrf_token"`
	}
	f.getJSON(t, "/v1/csrf-token", http.StatusOK, &body)
	if body.Token == "" {
		t.Fatal("GET /v1/csrf-token returned no token")
	}
	f.csrf = body.Token
	return f.csrf
}

// do issues one request against the fixture, stamping the CSRF header on every
// non-GET.
func (f *serveFixture) do(t *testing.T, method, path string, body any) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode %s %s body: %v", method, path, err)
		}
		reader = strings.NewReader(string(encoded))
	}
	req, err := http.NewRequest(method, f.base+path, reader)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if method != http.MethodGet {
		req.Header.Set(wui.CSRFHeaderName, f.token(t))
	}
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

// decodeInto asserts the status and decodes the body when out is non-nil.
func decodeInto(t *testing.T, resp *http.Response, method, path string, want int, out any) {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s %s body: %v", method, path, err)
	}
	if resp.StatusCode != want {
		t.Fatalf("%s %s = %d, want %d (body %q)", method, path, resp.StatusCode, want, raw)
	}
	if out == nil {
		return
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("decode %s %s body: %v (body %q)", method, path, err, raw)
	}
}

func (f *serveFixture) postJSON(t *testing.T, path string, body any, want int, out any) {
	t.Helper()
	decodeInto(t, f.do(t, http.MethodPost, path, body), http.MethodPost, path, want, out)
}

func (f *serveFixture) getJSON(t *testing.T, path string, want int, out any) {
	t.Helper()
	decodeInto(t, f.do(t, http.MethodGet, path, nil), http.MethodGet, path, want, out)
}

// --- the SSE stream ---------------------------------------------------------

// sseFrame is one decoded Server-Sent Event: the frame class from the `event:` line
// (only ever "enduring" or "ephemeral" — the event TYPE is inside the payload, not
// here) and the decoded `data:` body.
type sseFrame struct {
	class string
	data  map[string]any
}

// sseStream is an open events subscription.
type sseStream struct {
	resp   *http.Response
	reader *bufio.Reader
	cancel context.CancelFunc
}

func (s *sseStream) Close() {
	s.cancel()
	_ = s.resp.Body.Close()
}

// openSSE attaches to a session's live event stream. It returns only after the
// response HEADERS arrive, which is the synchronisation point that matters: harness
// subscribes BEFORE writing them (handlers_events.go), so a turn submitted after this
// returns cannot be missed.
//
// THIS BLOCKS FOR UP TO 20 SECONDS AND THAT IS NOT A HANG. handleEvents calls
// WriteHeader(200) but never flushes, so net/http holds the response head in its
// buffer until the handler's first real write — the first event, or the keep-alive
// ping on serve's fixed 20s defaultHeartbeatInterval (no Option exposes it). On an
// idle session the ping is what unblocks this call. It is a real latency the browser
// pays too (EventSource stays CONNECTING until then); it belongs to harness, so it is
// waited out here rather than papered over with a sleep that would race the
// subscription.
func (f *serveFixture) openSSE(t *testing.T, sid uuid.UUID) *sseStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.base+"/v1/sessions/"+sid.String()+"/events", nil)
	if err != nil {
		cancel()
		t.Fatalf("build events request: %v", err)
	}
	// No client timeout: the stream is long-lived by design.
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		cancel()
		t.Fatalf("GET events: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		cancel()
		t.Fatalf("GET events = %d, want 200 (body %q)", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		_ = resp.Body.Close()
		cancel()
		t.Fatalf("events Content-Type = %q, want text/event-stream", ct)
	}
	stream := &sseStream{resp: resp, reader: bufio.NewReader(resp.Body), cancel: cancel}
	t.Cleanup(stream.Close)
	return stream
}

// next reads the next complete SSE frame, skipping `: ping` heartbeat comments.
func (s *sseStream) next() (sseFrame, error) {
	frame := sseFrame{}
	var data string
	for {
		line, err := s.reader.ReadString('\n')
		if err != nil {
			return sseFrame{}, err
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if frame.class == "" || data == "" {
				frame, data = sseFrame{}, ""
				continue
			}
			if err := json.Unmarshal([]byte(data), &frame.data); err != nil {
				return sseFrame{}, fmt.Errorf("decode sse data %q: %w", data, err)
			}
			return frame, nil
		case strings.HasPrefix(line, ":"):
			// A heartbeat comment; carries no event.
		case strings.HasPrefix(line, "event: "):
			frame.class = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data += strings.TrimPrefix(line, "data: ")
		}
	}
}

// awaitEvent scans forward for the next ENDURING frame whose payload event type is
// want, and returns that event object.
//
// The type is read from data.event.type and is PascalCase — the `event:` SSE field
// only ever carries the frame CLASS ("enduring"/"ephemeral"), never the event name.
func awaitEvent(t *testing.T, s *sseStream, want string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	seen := []string{}
	for time.Now().Before(deadline) {
		frame, err := s.next()
		if err != nil {
			t.Fatalf("waiting for %s: stream ended after %v: %v", want, seen, err)
		}
		if frame.class != "enduring" {
			continue
		}
		ev, ok := frame.data["event"].(map[string]any)
		if !ok {
			t.Fatalf("enduring frame carried no event object: %v", frame.data)
		}
		name, _ := ev["type"].(string)
		seen = append(seen, name)
		if name == want {
			return ev
		}
	}
	t.Fatalf("no %s event within 30s; saw %v", want, seen)
	return nil
}

// --- assertions over the read plane ----------------------------------------

type sessionListResponse struct {
	Sessions []struct {
		SessionID uuid.UUID `json:"session_id"`
	} `json:"sessions"`
}

func (l sessionListResponse) contains(id uuid.UUID) bool {
	for _, s := range l.Sessions {
		if s.SessionID == id {
			return true
		}
	}
	return false
}

type journalResponse struct {
	Events []struct {
		JournalSeq uint64         `json:"journal_seq"`
		Event      map[string]any `json:"event"`
	} `json:"events"`
}

// types lists the journal's event type tags, in order.
func (j journalResponse) types() []string {
	out := make([]string, 0, len(j.Events))
	for _, e := range j.Events {
		name, _ := e.Event["type"].(string)
		out = append(out, name)
	}
	return out
}

func (j journalResponse) contains(want string) bool {
	for _, got := range j.types() {
		if got == want {
			return true
		}
	}
	return false
}

// gatedWriteScript is the two-step script every gate test runs: call 0 asks to write a
// file outside the workspace (a Gated HostWrite under the trusted profile), and every
// later call answers with plain text so the turn completes.
func gatedWriteScript(path string) func(int, inference.Request) []content.Chunk {
	return func(call int, _ inference.Request) []content.Chunk {
		if call == 0 {
			return []content.Chunk{&content.ToolUseChunk{
				Index:     0,
				ID:        "serve-gate-1",
				Name:      "WriteFile",
				InputJSON: fmt.Sprintf(`{"path":%q,"content":"written through carbon serve\n"}`, path),
			}}
		}
		return []content.Chunk{&content.TextChunk{Text: "done"}}
	}
}

// idleScript never calls a tool: it answers every turn with one text block.
func idleScript(_ int, _ inference.Request) []content.Chunk {
	return []content.Chunk{&content.TextChunk{Text: "ok"}}
}

// createSession runs POST /v1/sessions and returns the new id.
func (f *serveFixture) createSession(t *testing.T) uuid.UUID {
	t.Helper()
	var created struct {
		SessionID uuid.UUID `json:"session_id"`
	}
	f.postJSON(t, "/v1/sessions", nil, http.StatusCreated, &created)
	if created.SessionID.IsZero() {
		t.Fatal("POST /v1/sessions returned a zero session id")
	}
	return created.SessionID
}

// submit runs POST /v1/sessions/{sid}/input with one text block.
func (f *serveFixture) submit(t *testing.T, sid uuid.UUID, text string) {
	t.Helper()
	// 200, not 202: pkg/serve's handleInput answers OK with the minted command id
	// (handlers_control.go). Only the GATE route is 202.
	var accepted struct {
		CommandID uuid.UUID `json:"command_id"`
	}
	f.postJSON(t, "/v1/sessions/"+sid.String()+"/input",
		map[string]any{"blocks": []map[string]any{{"type": "text", "Text": text}}},
		http.StatusOK, &accepted)
	if accepted.CommandID.IsZero() {
		t.Fatal("POST input returned no command id; the submission is uncorrelatable on the stream")
	}
}

// --- the tests --------------------------------------------------------------

// TestServeEndToEnd drives every in-scope flow of the wui design §5 against a real
// `carbon serve` composition, in order:
//
//  1. POST /v1/sessions               -> 201, the session is live
//  2. GET  /v1/sessions               -> it appears in the listing catalog
//  3. GET  /v1/sessions/{sid}/events  -> SSE attaches
//  4. POST /v1/sessions/{sid}/input   -> 202, TurnStarted arrives on the stream
//  5. a GateOpened frame arrives on the STREAM (gates are never polled)
//  6. POST /v1/sessions/{sid}/gates/{gid} -> 202, GateResolved follows
//  7. GET  /v1/sessions/{sid}/journal -> the cold transcript replays
func TestServeEndToEnd(t *testing.T) {
	// An absolute path OUTSIDE the served workspace root: writing it is a HostWrite,
	// which access.go's AccessTrusted case declares Gated — the cheapest reliable way
	// to make Carbon's real policy open a real permission gate.
	hostWrite := filepath.Join(t.TempDir(), "gated.txt")
	fixture := startTestServe(t, gatedWriteScript(hostWrite))

	sid := fixture.createSession(t)

	var list sessionListResponse
	fixture.getJSON(t, "/v1/sessions", http.StatusOK, &list)
	if !list.contains(sid) {
		t.Fatalf("created session %v absent from the listing", sid)
	}

	stream := fixture.openSSE(t, sid)
	fixture.submit(t, sid, "write the file")
	awaitEvent(t, stream, "TurnStarted")

	opened := awaitEvent(t, stream, "GateOpened")
	gate, ok := opened["gate"].(map[string]any)
	if !ok {
		t.Fatalf("GateOpened carried no gate object: %v", opened)
	}
	gid, _ := gate["id"].(string)
	if gid == "" {
		t.Fatalf("GateOpened carried no gate id: %v", gate)
	}
	// Compared against harness's own constant, never a copied literal: the wui design
	// answers permission gates and nothing else, so a kind drift must break here.
	if kind, _ := gate["kind"].(string); kind != string(gatepkg.KindPermission) {
		t.Fatalf("gate kind = %q, want %q", kind, gatepkg.KindPermission)
	}

	fixture.postJSON(t, "/v1/sessions/"+sid.String()+"/gates/"+gid,
		map[string]any{"action": string(gatepkg.ApprovalApprove)}, http.StatusAccepted, nil)
	resolved := awaitEvent(t, stream, "GateResolved")
	if action, _ := resolved["action"].(string); action != string(gatepkg.ApprovalApprove) {
		t.Errorf("GateResolved action = %q, want %q", action, gatepkg.ApprovalApprove)
	}
	awaitEvent(t, stream, "TurnDone")

	// The approval really did admit the host write: the gate is a policy decision the
	// tool then acts on, not a decoration.
	if _, err := os.Stat(hostWrite); err != nil {
		t.Errorf("approved host write did not happen: %v", err)
	}

	var journal journalResponse
	fixture.getJSON(t, "/v1/sessions/"+sid.String()+"/journal?from_journal_seq=0", http.StatusOK, &journal)
	for _, want := range []string{"TurnStarted", "GateOpened", "GateResolved", "StepDone", "TurnDone"} {
		if !journal.contains(want) {
			t.Errorf("journal has no %s; the cold transcript would not render it (saw %v)", want, journal.types())
		}
	}
}

// TestServeAttachTwiceIsIdempotent proves the second browser tab works.
//
// handleRestore's attach-or-restore contract (harness v0.30.0) must answer 200
// {"restored":false} for an already-live sid WITHOUT consulting the rig. If it did
// consult it, one of two things happens and both are fatal: the rebuild contends the
// session's single-writer lease and 500s on a session that is right there and healthy,
// or it mints a SECOND runtime over the same journal and every existing subscriber is
// orphaned — still connected, permanently silent.
//
// The orphan case is why this test holds a stream open ACROSS the restores and then
// drives a turn: a status assertion alone cannot tell a preserved runtime from a
// replaced one.
func TestServeAttachTwiceIsIdempotent(t *testing.T) {
	fixture := startTestServe(t, idleScript)
	sid := fixture.createSession(t)
	stream := fixture.openSSE(t, sid)

	restore := "/v1/sessions/" + sid.String() + "/restore"
	for attempt := 1; attempt <= 2; attempt++ {
		var body struct {
			SessionID uuid.UUID `json:"session_id"`
			Restored  bool      `json:"restored"`
		}
		fixture.postJSON(t, restore, nil, http.StatusOK, &body)
		if body.SessionID != sid {
			t.Fatalf("restore #%d returned %v, want %v", attempt, body.SessionID, sid)
		}
		if body.Restored {
			t.Fatalf("restore #%d reported restored=true; an already-live session was rebuilt through the rig", attempt)
		}
	}

	fixture.submit(t, sid, "still the same runtime?")
	awaitEvent(t, stream, "TurnStarted")
	awaitEvent(t, stream, "TurnDone")
}

// TestServeRestoreOfANeverPersistedSessionIs404 pins the one error mapping carbon
// mints itself. pkg/serve turns EVERY RestoreSession failure that is not its own
// SessionNotFoundError into a bare 500, and rig.RestoreSession has no not-found class,
// so a genuinely absent session is a 404 only because serveRig checks the catalog
// before touching the rig.
//
// The envelope is NESTED — {"error":{"code":…}} — which is the shape pkg/serve, wui and
// carbon's own /ui routes all write. A test decoding a flat {"code":…} reads "" and
// passes against any error at all.
func TestServeRestoreOfANeverPersistedSessionIs404(t *testing.T) {
	fixture := startTestServe(t, idleScript)

	absent, err := uuid.New()
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Error struct {
			Code      string `json:"code"`
			Retryable bool   `json:"retryable"`
		} `json:"error"`
	}
	fixture.postJSON(t, "/v1/sessions/"+absent.String()+"/restore", nil, http.StatusNotFound, &body)
	if body.Error.Code != "session_not_found" {
		t.Fatalf("code = %q, want session_not_found", body.Error.Code)
	}
	if body.Error.Retryable {
		t.Error("a 404 restore was marked retryable; a never-persisted session never becomes present")
	}
}

// TestServeSecondSessionIsAConfirmedHandoffNotASilentClose is the behaviour design §5
// and 00-plan §3.7 both insist on. Carbon's rig places the workspace with
// rig.WithExclusiveWorkspace, so only one session may be live over a root at a time —
// but the incumbent may be mid-turn with a browser watching it, so opening a second
// session REFUSES rather than closing the first.
//
// The refusal, the pre-flight read that names the incumbent, the confirmation that
// ends it, and the open that then succeeds are all asserted here, because each one
// alone is satisfiable by a silent serialize.
func TestServeSecondSessionIsAConfirmedHandoffNotASilentClose(t *testing.T) {
	fixture := startTestServe(t, idleScript)
	incumbent := fixture.createSession(t)
	stream := fixture.openSSE(t, incumbent)

	// Opening a second session is refused, and refusing must not touch the first.
	var refusal struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	fixture.postJSON(t, "/v1/sessions", nil, http.StatusInternalServerError, &refusal)
	if refusal.Error.Code != "internal" {
		t.Errorf("refused create code = %q, want internal (pkg/serve maps every NewSession failure there)", refusal.Error.Code)
	}
	var live struct {
		Live      bool   `json:"live"`
		SessionID string `json:"session_id"`
	}
	fixture.getJSON(t, "/ui/live", http.StatusOK, &live)
	if !live.Live || live.SessionID != incumbent.String() {
		t.Fatalf("/ui/live = %+v after a refused create, want the incumbent %v still live", live, incumbent)
	}
	// The incumbent is not merely REGISTERED — it still runs turns.
	fixture.submit(t, incumbent, "am I still alive?")
	awaitEvent(t, stream, "TurnDone")

	// A confirmation naming the WRONG session is a 409 that changes nothing.
	other, err := uuid.New()
	if err != nil {
		t.Fatal(err)
	}
	var mismatch struct {
		Error struct {
			Code      string `json:"code"`
			Retryable bool   `json:"retryable"`
		} `json:"error"`
	}
	fixture.postJSON(t, "/ui/handoff", map[string]any{"session_id": other.String()}, http.StatusConflict, &mismatch)
	if mismatch.Error.Code != "live_session_mismatch" {
		t.Errorf("mismatched handoff code = %q, want live_session_mismatch", mismatch.Error.Code)
	}
	if !mismatch.Error.Retryable {
		t.Error("a live-session mismatch is not marked retryable; the client would give up instead of re-reading /ui/live")
	}
	fixture.getJSON(t, "/ui/live", http.StatusOK, &live)
	if !live.Live || live.SessionID != incumbent.String() {
		t.Fatalf("/ui/live = %+v after a MISMATCHED handoff, want the incumbent untouched", live)
	}

	// The confirmation that names the incumbent ends it, and the second session then
	// opens.
	resp := fixture.do(t, http.MethodPost, "/ui/handoff", map[string]any{"session_id": incumbent.String()})
	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("confirmed handoff = %d, want 204 (body %q)", resp.StatusCode, body)
	}
	_ = resp.Body.Close()

	successor := fixture.createSession(t)
	if successor == incumbent {
		t.Fatal("the create after the handoff returned the incumbent's id")
	}
	fixture.getJSON(t, "/ui/live", http.StatusOK, &live)
	if !live.Live || live.SessionID != successor.String() {
		t.Fatalf("/ui/live = %+v, want the successor %v", live, successor)
	}
}

// TestServeConcurrentColdRestoreDoesNotFail covers the race pkg/serve's own
// handleRestore documents but cannot close: two clients opening the same NOT-yET-live
// session both miss its liveness check, so both reach the rig.
//
// In carbon that race is not a 500. ServeHost holds one mutex across the whole of an
// open, so the loser observes the winner's live session and is handed the SAME
// controller; handleRestore's registerIfAbsent then answers it restored=false. Both
// clients end up attached to one runtime, which is the outcome a browser needs. This
// test pins that, because the alternative — a bare 500 on a healthy session — is
// indistinguishable at the wire from a real fault.
func TestServeConcurrentColdRestoreDoesNotFail(t *testing.T) {
	fixture := startTestServe(t, idleScript)
	sid := fixture.createSession(t)
	// Make it COLD: persisted, with nothing live over the workspace root.
	resp := fixture.do(t, http.MethodPost, "/ui/handoff", map[string]any{"session_id": sid.String()})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("handoff to make the session cold = %d, want 204", resp.StatusCode)
	}
	_ = resp.Body.Close()

	const clients = 4
	codes := make(chan int, clients)
	var start sync.WaitGroup
	start.Add(1)
	var done sync.WaitGroup
	for i := 0; i < clients; i++ {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			resp := fixture.do(t, http.MethodPost, "/v1/sessions/"+sid.String()+"/restore", nil)
			defer func() { _ = resp.Body.Close() }()
			_, _ = io.Copy(io.Discard, resp.Body)
			codes <- resp.StatusCode
		}()
	}
	start.Done()
	done.Wait()
	close(codes)
	for code := range codes {
		if code != http.StatusOK {
			t.Errorf("concurrent cold restore = %d, want 200; a healthy session was reported as a server fault", code)
		}
	}
}

// TestServeSessionPresentationTracksTheLiveSession drives the presentation route
// against the real host rather than a stub: the state is a function of what is
// actually live and of the workspace root the process actually serves, and neither is
// observable from a unit test of the handler.
func TestServeSessionPresentationTracksTheLiveSession(t *testing.T) {
	fixture := startTestServe(t, idleScript)
	sid := fixture.createSession(t)

	type row struct {
		Attach    string `json:"attach"`
		Workspace string `json:"workspace"`
		Reason    string `json:"reason"`
	}
	var body map[string]row
	fixture.getJSON(t, "/ui/session-presentation", http.StatusOK, &body)
	got, ok := body[sid.String()]
	if !ok {
		t.Fatalf("the live session %v is absent from the presentation map: %v", sid, body)
	}
	if got.Attach != "live" {
		t.Errorf("attach = %q for the session this process is holding, want live", got.Attach)
	}
	if got.Workspace == "" {
		t.Error("the live row published no workspace root")
	}
	if got.Reason != "" {
		t.Errorf("reason = %q on an attachable row; a reason belongs to read_only rows only", got.Reason)
	}
	servedRoot := got.Workspace

	// After the handoff the very same row is resumable, still in this workspace: the
	// state tracks liveness, it is not baked in at creation.
	resp := fixture.do(t, http.MethodPost, "/ui/handoff", map[string]any{"session_id": sid.String()})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("handoff = %d, want 204", resp.StatusCode)
	}
	_ = resp.Body.Close()

	body = nil
	fixture.getJSON(t, "/ui/session-presentation", http.StatusOK, &body)
	got, ok = body[sid.String()]
	if !ok {
		t.Fatalf("the session %v vanished from the presentation map after the handoff", sid)
	}
	if got.Attach != "resumable" {
		t.Errorf("attach = %q after the handoff, want resumable", got.Attach)
	}
	if got.Workspace != servedRoot {
		t.Errorf("workspace = %q after the handoff, want the unchanged %q", got.Workspace, servedRoot)
	}
}
