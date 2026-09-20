//go:build integration

package browser_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
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
)

type gateModelClient struct {
	mu       sync.Mutex
	calls    int
	requests chan inference.Request
}

func (*gateModelClient) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	return nil, errors.New("unexpected Invoke")
}

func (c *gateModelClient) Stream(_ context.Context, req inference.Request) (*stream.StreamReader[content.Chunk], error) {
	c.mu.Lock()
	call := c.calls
	c.calls++
	c.mu.Unlock()
	select {
	case c.requests <- req:
	default:
	}
	var chunk content.Chunk = &content.TextChunk{Text: "gate answer reached Carbon"}
	if call == 0 {
		chunk = &content.ToolUseChunk{Index: 0, ID: "ask-browser-1", Name: "AskUser",
			InputJSON: `{"question":"Choose a colour"}`}
	}
	used := false
	return stream.NewStreamReader(func() (content.Chunk, error) {
		if used {
			return nil, io.EOF
		}
		used = true
		return chunk, nil
	}, nil), nil
}

func TestBrowserGateAnswerReachesResidentCarbonAgent(t *testing.T) {
	cfg := browserFixture(t)
	client := &gateModelClient{requests: make(chan inference.Request, 4)}
	original := cfg.ClientBuilder
	cfg.ClientBuilder = func() (inference.Client, func() model.Model, error) {
		_, descriptor, err := original()
		return client, descriptor, err
	}
	s, err := browser.Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = s.Stop(ctx)
	})
	base := "http://" + s.Addr().String()
	csrfToken := ""
	request := func(method, path string, body any) (int, string) {
		t.Helper()
		var encoded []byte
		if body != nil {
			var err error
			encoded, err = json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
		}
		req, err := http.NewRequest(method, base+path, bytes.NewReader(encoded))
		if err != nil {
			t.Fatal(err)
		}
		req.AddCookie(&http.Cookie{Name: "browser_session", Value: "browser-test-token"})
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "http://127.0.0.1")
		if method == http.MethodPost && csrfToken != "" {
			req.Header.Set("X-CSRF-Token", csrfToken)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(data)
	}
	csrfStatus, csrfBody := request(http.MethodGet, "/v1/csrf-token", nil)
	if csrfStatus != http.StatusOK {
		t.Fatalf("CSRF mint = %d %s", csrfStatus, csrfBody)
	}
	var issued struct {
		Token string `json:"csrf_token"`
	}
	if err := json.Unmarshal([]byte(csrfBody), &issued); err != nil || issued.Token == "" {
		t.Fatalf("CSRF token = %q %v", csrfBody, err)
	}
	csrfToken = issued.Token
	const session = sessionwire.SessionID("browser-gate-session")
	status, body := request(http.MethodPost, "/v1/sessions", sessionwire.CreateRequest{
		CommandEnvelope: sessionwire.CommandEnvelope{Version: sessionwire.CurrentWireVersion, CommandID: "browser-gate-create"},
		SessionID:       session, AgentID: "carbon", Blocks: json.RawMessage(`[{"type":"text","text":"ask me"}]`),
	})
	if status != http.StatusCreated {
		t.Fatalf("create = %d %s", status, body)
	}
	var projected sessionwire.GateProjection
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		status, body = request(http.MethodGet, "/v1/sessions/"+string(session)+"/gates", nil)
		if status != http.StatusOK {
			t.Fatalf("gates = %d %s", status, body)
		}
		var page sessionwire.GatePage
		if err := page.UnmarshalJSON([]byte(body)); err != nil {
			t.Fatal(err)
		}
		if len(page.Gates) != 0 {
			projected = page.Gates[0]
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if projected.GateID == "" || projected.Answerability != sessionwire.GateAnswerabilityResident || projected.OpenedJournalSeq == 0 {
		t.Fatalf("Carbon AskUser gate did not become resident: %+v", projected)
	}
	if !strings.Contains(projected.Prompt.Title+" "+projected.Prompt.Body, "Choose a colour") {
		t.Fatalf("projected prompt = %+v", projected.Prompt)
	}
	status, body = request(http.MethodPost, "/v1/sessions/"+string(session)+"/gates/"+string(projected.GateID), sessionwire.GateResponseRequest{
		CommandEnvelope: sessionwire.CommandEnvelope{Version: sessionwire.CurrentWireVersion, CommandID: "browser-gate-answer"},
		SessionID:       session, GateID: projected.GateID, Action: "answer",
		Values:                 map[string]json.RawMessage{"answer": json.RawMessage(`"ULTRAMARINE"`)},
		ExpectedOpenJournalSeq: projected.OpenedJournalSeq,
	})
	if status != http.StatusAccepted {
		t.Fatalf("gate answer = %d %s", status, body)
	}
	select {
	case <-client.requests:
	case <-time.After(5 * time.Second):
		t.Fatal("first model request absent")
	}
	select {
	case resumed := <-client.requests:
		found := false
		for _, message := range resumed.Messages {
			result, ok := message.(*content.ToolResultMessage)
			if !ok || result.ToolUseID != "ask-browser-1" || result.IsError {
				continue
			}
			encoded, _ := json.Marshal(result)
			found = strings.Contains(string(encoded), "ULTRAMARINE")
		}
		if !found {
			encoded, _ := json.Marshal(resumed.Messages)
			t.Fatalf("resumed model did not receive AskUser tool result: %s", encoded)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Carbon agent did not resume after gate answer")
	}
	settled := false
	deadline = time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		gateStatus, gateBody := request(http.MethodGet, "/v1/sessions/"+string(session)+"/gates", nil)
		journalStatus, journalBody := request(http.MethodGet, "/v1/sessions/"+string(session)+"/journal", nil)
		var page sessionwire.GatePage
		if gateStatus == http.StatusOK && page.UnmarshalJSON([]byte(gateBody)) == nil && len(page.Gates) == 0 &&
			journalStatus == http.StatusOK && strings.Contains(journalBody, "gate answer reached Carbon") {
			settled = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !settled {
		t.Fatal("answered gate did not clear with continuation in durable public journal")
	}
}
