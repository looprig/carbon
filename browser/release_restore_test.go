package browser_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/looprig/carbon/browser"
	"github.com/looprig/core/content"
	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/inference"
	"github.com/looprig/inference/model"
	"github.com/looprig/inference/stream"
	"github.com/looprig/llm"
)

// scriptedWorkspaceClient plays a model that believes the workspace path it is
// told. Its first turn writes a file at <cwd>/notes.txt, its second turn (after
// a Host generation change) reads the same path back. Every request is
// recorded so the test can inspect exactly what the model saw.
type scriptedWorkspaceClient struct {
	mu       *sync.Mutex
	requests *[]inference.Request
	notify   chan<- struct{}
}

const workspaceBytes = "workspace-bytes-v1\n"

func (scriptedWorkspaceClient) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	return nil, errors.New("unexpected Invoke")
}

func (c scriptedWorkspaceClient) Stream(_ context.Context, req inference.Request) (*stream.StreamReader[content.Chunk], error) {
	c.mu.Lock()
	call := len(*c.requests)
	*c.requests = append(*c.requests, req)
	c.mu.Unlock()
	select {
	case c.notify <- struct{}{}:
	default:
	}
	cwd := modelVisibleCWD(req)
	var chunks []content.Chunk
	switch call {
	case 0:
		input, _ := json.Marshal(map[string]string{"path": filepath.Join(cwd, "notes.txt"), "content": workspaceBytes})
		chunks = []content.Chunk{&content.ToolUseChunk{Index: 0, ID: "write-notes", Name: "WriteFile", InputJSON: string(input)}}
	case 2:
		input, _ := json.Marshal(map[string]string{"path": filepath.Join(cwd, "notes.txt")})
		chunks = []content.Chunk{&content.ToolUseChunk{Index: 0, ID: "read-notes", Name: "ReadFile", InputJSON: string(input)}}
	default:
		chunks = []content.Chunk{&content.TextChunk{Text: fmt.Sprintf("browser reply turn %d", call)}}
	}
	next := 0
	return stream.NewStreamReader(func() (content.Chunk, error) {
		if next == len(chunks) {
			return nil, io.EOF
		}
		next++
		return chunks[next-1], nil
	}, nil), nil
}

// modelVisibleCWD extracts the "cwd:" line of the runtime context the model
// was sent, or "" when the request carried none.
func modelVisibleCWD(req inference.Request) string {
	encoded, err := json.Marshal(req.Messages)
	if err != nil {
		return ""
	}
	text := string(encoded)
	at := strings.LastIndex(text, "cwd: ")
	if at < 0 {
		return ""
	}
	rest := text[at+len("cwd: "):]
	if end := strings.Index(rest, `\n`); end >= 0 {
		return rest[:end]
	}
	return ""
}

// R1.5 step 4. A released session keeps its history, the bytes its agent wrote
// into its workspace, and the workspace path the model was told; a later local
// Host generation restores it — including the workspace bytes, from the durable
// snapshot, after the local copy is removed. The only release path the released Host (v0.6.0)
// exposes to a composition is the drain on Stop — host.Compose wires no
// WorkStates source, so it never warm-evicts on WarmTTL — so the release under
// test is the Host drain's checkpoint-and-release, followed by a new Host
// generation over the same root.
//
// The model-visible path is taken from what the model was actually sent, not
// from Carbon's own path derivation, and the model writes and reads through
// that path, so a composition that told the model one directory while its
// tools served another would fail here.
func TestReleasedSessionKeepsHistoryWorkspaceAndPathAcrossHostGenerations(t *testing.T) {
	processDir := t.TempDir()
	t.Chdir(processDir)
	cfg := browserFixture(t)
	cfg.Factory.ReconcileLimits.Interval = 25 * time.Millisecond
	var mu sync.Mutex
	var requests []inference.Request
	calls := make(chan struct{}, 16)
	cfg.ClientBuilder = func() (inference.Client, func() model.Model, error) {
		return scriptedWorkspaceClient{mu: &mu, requests: &requests, notify: calls}, func() model.Model {
			return model.CustomModel(model.ProviderName(llm.ProviderLMStudio), model.APIFormatOpenAI,
				"http://localhost:1234/v1", "browser-test", model.WithTools(),
				model.WithContextLimits(model.ContextLimits{WindowTokens: 128_000}))
		}, nil
	}
	request := func(base, method, path string, payload any) (int, []byte) {
		t.Helper()
		var body []byte
		if payload != nil {
			var err error
			if body, err = json.Marshal(payload); err != nil {
				t.Fatal(err)
			}
		}
		r, err := http.NewRequest(method, base+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer browser-test-token")
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "http://127.0.0.1")
		response, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, data
	}
	awaitJournal := func(base, want string) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			status, body := request(base, http.MethodGet, "/v1/sessions/browser-session-1/journal?limit=500", nil)
			if status == http.StatusOK && strings.Contains(string(body), want) {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("journal never recorded %q", want)
	}
	recorded := func(i int) inference.Request {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()
		if len(requests) <= i {
			t.Fatalf("model saw %d requests, want at least %d", len(requests), i+1)
		}
		return requests[i]
	}

	first, err := browser.Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Stop(context.Background()) })
	base := "http://" + first.Addr().String()
	create := sessionwire.CreateRequest{CommandEnvelope: sessionwire.CommandEnvelope{Version: sessionwire.CurrentWireVersion,
		CommandID: "release-create-1"}, SessionID: "browser-session-1", AgentID: "carbon",
		Blocks: json.RawMessage(`[{"type":"text","text":"first-marker"}]`)}
	if status, body := request(base, http.MethodPost, "/v1/sessions", create); status != http.StatusCreated {
		t.Fatalf("create = %d %s", status, body)
	}
	// 60 s, not 15: under -race with several packages running, the first
	// hostlink.attach can exceed its deadline, and Factory's placement sweep
	// then retries it on its next pass. The create is durable and is placed;
	// the bound only has to cover a retried placement, not a single attach.
	select {
	case <-calls:
	case <-time.After(60 * time.Second):
		t.Fatal("create never reached the model")
	}
	told := modelVisibleCWD(recorded(0))
	sessionWorkspaces := filepath.Join(cfg.Storage.DataDir, "session-workspaces") + string(filepath.Separator)
	if !strings.HasPrefix(told, sessionWorkspaces) {
		t.Fatalf("model was told cwd %q; want the session's own workspace under %s (process cwd is %s)", told, sessionWorkspaces, processDir)
	}
	awaitJournal(base, "browser reply turn 1")
	written, err := os.ReadFile(filepath.Join(told, "notes.txt"))
	if err != nil || string(written) != workspaceBytes {
		t.Fatalf("agent write at the model-visible path = %q, %v; want %q", written, err, workspaceBytes)
	}
	if err := first.Stop(context.Background()); err != nil {
		t.Fatalf("stop first Host generation: %v", err)
	}
	// A later generation may run on another machine, so the bytes must come back
	// from the released session's durable workspace snapshot, not from a local
	// directory that happened to survive: remove the local copy first.
	if err := os.Remove(filepath.Join(told, "notes.txt")); err != nil {
		t.Fatalf("remove the local copy before the next generation: %v", err)
	}

	cfg.Host.Generation++
	second, err := browser.Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("start next Host generation: %v", err)
	}
	t.Cleanup(func() { _ = second.Stop(context.Background()) })
	base = "http://" + second.Addr().String()
	if status, body := request(base, http.MethodGet, "/v1/sessions/browser-session-1/status", nil); status != http.StatusOK ||
		!strings.Contains(string(body), string(sessionwire.SessionResidencyCold)) {
		t.Fatalf("released session status on next generation = %d %s, want cold", status, body)
	}
	input := sessionwire.InputRequest{CommandEnvelope: sessionwire.CommandEnvelope{Version: sessionwire.CurrentWireVersion,
		CommandID: "release-input-1"}, SessionID: "browser-session-1",
		Blocks: json.RawMessage(`[{"type":"text","text":"second-marker"}]`)}
	if status, body := request(base, http.MethodPost, "/v1/sessions/browser-session-1/input", input); status != http.StatusOK {
		t.Fatalf("input after release = %d %s", status, body)
	}
	awaitJournal(base, "browser reply turn 3")

	restored := recorded(2)
	if got := modelVisibleCWD(restored); got != told {
		t.Fatalf("restored model was told cwd %q, want the pre-release workspace %q", got, told)
	}
	history, err := json.Marshal(restored.Messages)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"first-marker", "write-notes", "browser reply turn 1", "second-marker"} {
		if !strings.Contains(string(history), want) {
			t.Fatalf("restored model context lacks %q: %s", want, history)
		}
	}
	// The restored agent's own ReadFile result — not the transcript, which already
	// carries generation 1's WriteFile input — must hold the pre-release bytes.
	readBack, found, isError := toolResultText(recorded(3).Messages, "read-notes")
	if !found || isError || !strings.Contains(readBack, strings.TrimSpace(workspaceBytes)) {
		t.Fatalf("restored agent's ReadFile of %s/notes.txt = found %v, error %v, %q; want the pre-release bytes %q",
			told, found, isError, readBack, workspaceBytes)
	}
	if onDisk, err := os.ReadFile(filepath.Join(told, "notes.txt")); err != nil || string(onDisk) != workspaceBytes {
		t.Fatalf("workspace bytes after the next generation restored = %q, %v; want %q", onDisk, err, workspaceBytes)
	}
}

// toolResultText returns the text of the tool result answering toolUseID, whether
// it is carried as a tool-result message or as a tool-result block.
func toolResultText(messages content.AgenticMessages, toolUseID string) (text string, found, isError bool) {
	var b strings.Builder
	collect := func(blocks []content.Block) {
		for _, block := range blocks {
			if tb, ok := block.(*content.TextBlock); ok {
				b.WriteString(tb.Text)
			}
		}
	}
	for _, message := range messages {
		switch m := message.(type) {
		case *content.ToolResultMessage:
			if m.ToolUseID == toolUseID {
				found, isError = true, isError || m.IsError
				collect(m.Blocks)
			}
		case *content.UserMessage:
			for _, block := range m.Blocks {
				if r, ok := block.(*content.ToolResultBlock); ok && r.ToolUseID == toolUseID {
					found, isError = true, isError || r.IsError
					collect(r.Content)
				}
			}
		}
	}
	return b.String(), found, isError
}
