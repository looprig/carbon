package browser_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/looprig/carbon/browser"
	"github.com/looprig/core/content"
	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/fsstore"
	"github.com/looprig/inference"
	"github.com/looprig/inference/model"
	"github.com/looprig/inference/stream"
)

// retentionLines is how many lines the scripted Bash command prints: about
// 150 KB, three times Carbon's 50 KiB model preview, so the result is shaped
// and retained.
const retentionLines = 3000

// retentionCommand prints retentionOutput() exactly.
const retentionCommand = `awk 'BEGIN { for (i = 0; i < 3000; i++) printf "line %05d abcdefghijklmnopqrstuvwxyz0123456789\n", i }'`

func retentionOutput() string {
	var b strings.Builder
	for i := range retentionLines {
		fmt.Fprintf(&b, "line %05d abcdefghijklmnopqrstuvwxyz0123456789\n", i)
	}
	return b.String()
}

var captureIDPattern = regexp.MustCompile(`read_tool_result capture_id="([0-9a-f-]{36})"`)

// retentionModel is a scripted model that acts only on what it is shown. For
// "run big" it calls Bash, then follows the retention marker it was shown with
// read_tool_result, then finishes; anything else gets a plain reply.
type retentionModel struct {
	mu    sync.Mutex
	pages []string
	shown []string
}

func (*retentionModel) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	return nil, errors.New("unexpected Invoke")
}

func (m *retentionModel) Stream(_ context.Context, req inference.Request) (*stream.StreamReader[content.Chunk], error) {
	chunk := m.next(req)
	used := false
	return stream.NewStreamReader(func() (content.Chunk, error) {
		if used {
			return nil, io.EOF
		}
		used = true
		return chunk, nil
	}, nil), nil
}

// next counts every tool result in the request: Carbon appends a runtime
// context user message after the history, so the count must not reset on a
// user message (each session here has exactly one turn).
func (m *retentionModel) next(req inference.Request) content.Chunk {
	userText, toolText, toolUses := "", "", 0
	for _, message := range req.Messages {
		switch msg := message.(type) {
		case *content.UserMessage:
			for _, block := range msg.Blocks {
				if text, ok := block.(*content.TextBlock); ok {
					userText += text.Text + "\n"
				}
			}
		case *content.ToolResultMessage:
			toolUses++
			for _, block := range msg.Blocks {
				if text, ok := block.(*content.TextBlock); ok {
					toolText = text.Text
				}
			}
		}
	}
	if !strings.Contains(userText, "run big") {
		return &content.TextChunk{Text: "small reply"}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	switch toolUses {
	case 0:
		input, _ := json.Marshal(map[string]string{"command": retentionCommand})
		return &content.ToolUseChunk{Index: 0, ID: "bash-big-1", Name: "Bash", InputJSON: string(input)}
	case 1:
		m.shown = append(m.shown, toolText)
		found := captureIDPattern.FindStringSubmatch(toolText)
		if found == nil {
			return &content.TextChunk{Text: "no retention marker"}
		}
		input, _ := json.Marshal(map[string]any{"capture_id": found[1], "offset": 0})
		return &content.ToolUseChunk{Index: 0, ID: "read-big-1", Name: "read_tool_result", InputJSON: string(input)}
	default:
		m.pages = append(m.pages, toolText)
		return &content.TextChunk{Text: "retained output read"}
	}
}

func (m *retentionModel) snapshot() (shown, pages []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.shown...), append([]string(nil), m.pages...)
}

type retentionClient struct {
	t    *testing.T
	base string
	csrf string
}

func (c *retentionClient) do(method, path string, body any, header http.Header) (int, http.Header, []byte) {
	c.t.Helper()
	var encoded []byte
	if body != nil {
		var err error
		if encoded, err = json.Marshal(body); err != nil {
			c.t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, c.base+path, bytes.NewReader(encoded))
	if err != nil {
		c.t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: "browser_session", Value: "browser-test-token"})
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://127.0.0.1")
	if method == http.MethodPost && c.csrf != "" {
		req.Header.Set("X-CSRF-Token", c.csrf)
	}
	for key, values := range header {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, data
}

func (c *retentionClient) create(session sessionwire.SessionID, text string) {
	c.t.Helper()
	blocks, _ := json.Marshal([]map[string]string{{"type": "text", "text": text}})
	status, _, body := c.do(http.MethodPost, "/v1/sessions", sessionwire.CreateRequest{
		CommandEnvelope: sessionwire.CommandEnvelope{Version: sessionwire.CurrentWireVersion, CommandID: sessionwire.CommandID("create-" + string(session))},
		SessionID:       session, AgentID: "carbon", Blocks: blocks,
	}, nil)
	if status != http.StatusCreated {
		c.t.Fatalf("create %s = %d %s", session, status, body)
	}
}

// waitJournal polls a session's public journal until it contains want.
func (c *retentionClient) waitJournal(session sessionwire.SessionID, want string) string {
	c.t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	last := ""
	for time.Now().Before(deadline) {
		status, _, body := c.do(http.MethodGet, "/v1/sessions/"+string(session)+"/journal", nil, nil)
		if status == http.StatusOK {
			last = string(body)
			if strings.Contains(last, want) {
				return last
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	c.t.Fatalf("journal of %s never contained %q; last page: %.2000s", session, want, last)
	return ""
}

var objectIDPattern = regexp.MustCompile(`"object_id":"(v1:tool-result:[a-z0-9]+:[0-9a-f]{64})"`)

// A large Bash result in a browser-served Carbon session is retained in the
// session's runtime journal store; the model pages it back with
// read_tool_result; Factory's object route serves the exact bytes to that
// session and answers every foreign or forged reference with the one
// absent-object 404.
func TestBrowserServesRetainedToolOutputOnlyToItsSession(t *testing.T) {
	cfg := browserFixture(t)
	scripted := &retentionModel{}
	original := cfg.ClientBuilder
	cfg.ClientBuilder = func() (inference.Client, func() model.Model, error) {
		_, descriptor, err := original()
		return scripted, descriptor, err
	}
	s, err := browser.Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = s.Stop(ctx)
	})
	c := &retentionClient{t: t, base: "http://" + s.Addr().String()}
	status, _, body := c.do(http.MethodGet, "/v1/csrf-token", nil, nil)
	var issued struct {
		Token string `json:"csrf_token"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &issued) != nil || issued.Token == "" {
		t.Fatalf("CSRF mint = %d %s", status, body)
	}
	c.csrf = issued.Token

	const big, small = sessionwire.SessionID("retention-big"), sessionwire.SessionID("retention-small")
	c.create(big, "run big")
	c.create(small, "hello")
	journal := c.waitJournal(big, "retained output read")
	c.waitJournal(small, "small reply")

	// The object Carbon captured: the Bash tool's complete stream, which for a
	// clean exit is the output followed by its exit-code line.
	output := retentionOutput()
	want := output + "[exit code: 0]"
	wantDigest := sha256.Sum256([]byte(want))

	found := objectIDPattern.FindStringSubmatch(journal)
	if found == nil {
		t.Fatalf("the big session's journal names no tool-result object: %.4000s", journal)
	}
	objectID := found[1]
	if n := len(objectIDPattern.FindAllString(journal, -1)); n != 1 {
		t.Fatalf("journal names %d tool-result objects, want exactly the Bash capture (read_tool_result pages are never re-retained)", n)
	}

	// The model saw a preview with the marker, not the whole output, and paged
	// the exact bytes back.
	shown, pages := scripted.snapshot()
	if len(shown) != 1 || len(shown[0]) > 50*1024 || !strings.Contains(shown[0], "read_tool_result capture_id=") || !strings.HasPrefix(shown[0], "line 00000 ") {
		t.Fatalf("the model was shown %d previews; first %.300q", len(shown), shown)
	}
	if len(pages) != 1 {
		t.Fatalf("the model paged %d times, want 1", len(pages))
	}
	page := pages[0]
	footer := strings.LastIndex(page, "\n[capture ")
	if footer <= 0 {
		t.Fatalf("read_tool_result page has no capture footer: %.300q", page)
	}
	if body := page[:footer]; !strings.HasPrefix(want, body) || len(body) < 16*1024 {
		t.Fatalf("read_tool_result page (%d bytes) is not a prefix of the captured output", len(body))
	}
	if len(page) > 50*1024 {
		t.Fatalf("read_tool_result page is %d bytes, over the model preview", len(page))
	}

	objectPath := "/v1/sessions/" + string(big) + "/objects/" + objectID
	// Metadata: the exact size and digest.
	status, _, body = c.do(http.MethodGet, objectPath+"/metadata", nil, nil)
	var metadata sessionwire.ObjectMetadata
	if status != http.StatusOK || json.Unmarshal(body, &metadata) != nil {
		t.Fatalf("metadata = %d %s", status, body)
	}
	if metadata.SizeBytes != uint64(len(want)) || metadata.Digest != "sha256:"+hex.EncodeToString(wantDigest[:]) || metadata.Reference.ObjectID != objectID {
		t.Fatalf("metadata = %+v, want %d bytes, digest %x", metadata, len(want), wantDigest)
	}
	// Whole object.
	status, header, body := c.do(http.MethodGet, objectPath, nil, nil)
	if status != http.StatusOK || string(body) != want {
		t.Fatalf("object = %d, %d bytes (want %d)", status, len(body), len(want))
	}
	if header.Get("X-Object-Digest") != metadata.Digest {
		t.Fatalf("X-Object-Digest = %q, want %q", header.Get("X-Object-Digest"), metadata.Digest)
	}
	// A Range page.
	status, _, body = c.do(http.MethodGet, objectPath, nil, http.Header{"Range": {"bytes=10000-19999"}})
	if status != http.StatusPartialContent || string(body) != want[10000:20000] {
		t.Fatalf("range page = %d, %d bytes", status, len(body))
	}
	// No answer names the private runtime session id.
	for _, check := range []string{string(body), journal} {
		if strings.Contains(check, `"runtime_session_id"`) {
			t.Fatal("a public answer names the runtime session id")
		}
	}

	// The session list still answers after a capture, naming both sessions.
	status, _, body = c.do(http.MethodGet, "/v1/sessions", nil, nil)
	if status != http.StatusOK || !strings.Contains(string(body), string(big)) || !strings.Contains(string(body), string(small)) {
		t.Fatalf("session list after a capture = %d %s", status, body)
	}

	// Every refusal is the ONE absent-object answer, byte-identical to a
	// never-issued reference's, and carries none of the captured bytes.
	forged := "v1:tool-result:00000000000000000000000000:" + hex.EncodeToString(wantDigest[:])
	absentStatus, absentHeader, absent := c.do(http.MethodGet, "/v1/sessions/"+string(big)+"/objects/"+forged, nil, nil)
	if absentStatus != http.StatusNotFound || absentHeader.Get("X-Object-Digest") != "" {
		t.Fatalf("forged reference = %d %s", absentStatus, absent)
	}
	for name, path := range map[string]string{
		"the capture read through another session":    "/v1/sessions/" + string(small) + "/objects/" + objectID,
		"its metadata through another session":        "/v1/sessions/" + string(small) + "/objects/" + objectID + "/metadata",
		"the forged reference's metadata":             "/v1/sessions/" + string(big) + "/objects/" + forged + "/metadata",
		"a random reference in the capture's session": "/v1/sessions/" + string(big) + "/objects/v1:tool-result:00000000000000000000000000:" + strings.Repeat("ab", 32),
	} {
		status, header, body := c.do(http.MethodGet, path, nil, nil)
		if status != http.StatusNotFound || !bytes.Equal(body, absent) || header.Get("X-Object-Digest") != "" || bytes.Contains(body, []byte("line 0")) {
			t.Fatalf("%s = %d %s, want the absent-object 404 %s", name, status, body, absent)
		}
	}
}

// Start refuses a data directory written before fsstore v0.6.0 with the typed,
// actionable error, before any runtime is built.
func TestStartRefusesAPreV060DataDirectory(t *testing.T) {
	cfg := browserFixture(t)
	legacy := filepath.Join(cfg.Storage.DataDir, "kv", "sessions")
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "0f1e2d3c-4b5a-4968-8776-655443322110"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := browser.Start(context.Background(), cfg)
	if s != nil {
		_ = s.Stop(context.Background())
	}
	var refused *browser.LegacyDataRootError
	if !errors.As(err, &refused) || refused.Root != cfg.Storage.DataDir || !errors.Is(err, fsstore.ErrLegacyLayout) ||
		!strings.Contains(err.Error(), "move or delete this directory") {
		t.Fatalf("Start over a pre-v0.6.0 data directory = %v, want *browser.LegacyDataRootError naming %s", err, cfg.Storage.DataDir)
	}
}
