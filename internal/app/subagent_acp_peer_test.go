package app

import (
	"bytes"
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

	"github.com/looprig/acp/protocol"
)

const (
	task33ACPHelperPath             = "task33-acp-helper"
	task33NativeClaudeACPHelperPath = "task33-native-claude-acp-helper"
	task33NativeCodexACPHelperPath  = "task33-native-codex-acp-helper"
	taskACPPostureHelperPath        = "task-acp-posture-helper"
	acpPostureReceiptName           = "acp-posture.receipt"
	acpPostureWriteName             = "acp-posture-write.txt"
)

// testIsolatedHome is the process-wide temporary directory TestMain
// establishes as HOME (and platform equivalents) before running this
// package's ordinary tests. It is set exactly once, before m.Run(), and is
// read-only afterward. home_test.go's TestLooprigHomeIsolatedFromRealHOME
// uses it to prove looprigHome(Config{}) actually resolves here rather than
// to the real developer machine's HOME.
var testIsolatedHome string

// TestMain turns the test binary into a deterministic ACP protocol peer when
// the production ACP child factory launches it. The parent test only supplies
// a gateway-safe PATH marker, so ordinary package tests never enter this path.
//
// For every other invocation (ordinary package tests), TestMain first
// isolates the process's HOME (and platform equivalents) to a fresh,
// per-run temporary directory before handing off to m.Run(). This makes it
// structurally impossible for a bare Config{} exercised anywhere in this
// package -- directly or via openRuntimeAgent's shared MCP-loading path --
// to resolve looprigHome to the real developer machine's ~/.looprig and so
// read a real mcp.json or spawn real MCP server connections as a side
// effect of `go test`. See home_test.go's TestLooprigHomeIsolatedFromRealHOME
// for the isolation proof.
func TestMain(m *testing.M) {
	switch os.Getenv("PATH") {
	case task33ACPHelperPath, task33NativeClaudeACPHelperPath, task33NativeCodexACPHelperPath:
		os.Exit(runTask33ACPHelper())
	}
	os.Exit(runPackageTestsWithIsolatedHome(m))
}

// runPackageTestsWithIsolatedHome sets up the process-wide isolated HOME and
// runs the package's ordinary tests. It is factored out of TestMain so the
// exit-code path stays a single, unambiguous `return m.Run()`: cleanup runs
// via defer regardless of how m.Run() finishes, but never substitutes or
// swallows the real exit code, which CI depends on to detect failures.
func runPackageTestsWithIsolatedHome(m *testing.M) int {
	home, err := os.MkdirTemp("", "coderig-internal-app-test-home-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, "internal/app TestMain: create isolated HOME:", err)
		return 1
	}
	defer os.RemoveAll(home)

	testIsolatedHome = home
	applyProcessHome(func(key, value string) {
		// os.Setenv (unlike t.Setenv) returns an error, but it is
		// practically infallible for these fixed, well-formed key names;
		// report and move on rather than aborting the whole run over it.
		if err := os.Setenv(key, value); err != nil {
			fmt.Fprintf(os.Stderr, "internal/app TestMain: setenv %s: %v\n", key, err)
		}
	}, home)

	return m.Run()
}

type task33ACPHelperState struct {
	mu         sync.Mutex
	harness    string
	workspace  string
	session    protocol.SessionID
	baseURL    string
	token      string
	mainModel  string
	smallModel string
	cancel     chan struct{}
	cancelOnce sync.Once
}

func runTask33ACPHelper() int {
	helperPath := os.Getenv("PATH")
	native := helperPath == task33NativeClaudeACPHelperPath || helperPath == task33NativeCodexACPHelperPath
	state := &task33ACPHelperState{
		harness: "codex",
		baseURL: strings.TrimSuffix(parseTask33Arg("model_providers.looprig.base_url"), "/v1"),
		token:   os.Getenv("LOOPRIG_PROXY_TOKEN"),
		cancel:  make(chan struct{}),
	}
	if helperPath == task33NativeClaudeACPHelperPath {
		state.harness = "claude-code"
	}
	if baseURL := os.Getenv("ANTHROPIC_BASE_URL"); baseURL != "" {
		state.harness = "claude-code"
		state.baseURL = baseURL
		state.token = os.Getenv("ANTHROPIC_AUTH_TOKEN")
	}
	if !native && (state.baseURL == "" || state.token == "") {
		fmt.Fprintln(os.Stderr, "task33 ACP helper: missing gateway binding")
		return 1
	}
	if native && (state.baseURL != "" || state.token != "") {
		fmt.Fprintln(os.Stderr, "task33 ACP helper: native launch received gateway binding")
		return 1
	}

	conn := protocol.NewConn(os.Stdin, os.Stdout, protocol.ConnOptions{})
	peer := protocol.NewClientConn(conn)
	defer conn.Close()
	ready := make(chan struct{})

	conn.Handle(string(protocol.MethodInitialize), func(context.Context, string, json.RawMessage) (any, error) {
		<-ready
		return protocol.InitializeResponse{ProtocolVersion: protocol.CurrentProtocolVersion}, nil
	})
	conn.Handle(string(protocol.MethodSessionNew), func(_ context.Context, _ string, params json.RawMessage) (any, error) {
		var request protocol.NewSessionRequest
		if err := json.Unmarshal(params, &request); err != nil {
			return nil, protocol.InvalidParams("task33 session/new", nil)
		}
		state.mu.Lock()
		state.workspace = request.Cwd
		if state.harness == "claude-code" {
			state.session = "task33-claude-code-session"
			state.mainModel = "sonnet-5@high"
		} else if native {
			state.session = "task33-codex-session"
			state.mainModel = "native-codex"
		} else {
			state.session = "task33-codex-session"
			state.mainModel = parseTask33Arg("model")
		}
		session := state.session
		mainModel := state.mainModel
		harness := state.harness
		state.mu.Unlock()

		response := protocol.NewSessionResponse{SessionID: session}
		if harness == "claude-code" {
			category := protocol.SessionConfigOptionCategoryModel
			response.ConfigOptions = []protocol.SessionConfigOption{{
				Category: &category,
				ID:       "model",
				Name:     "Model",
				Select: &protocol.SessionConfigSelect{
					CurrentValue: protocol.SessionConfigValueID(mainModel),
					Options: protocol.SessionConfigSelectOptions{Ungrouped: []protocol.SessionConfigSelectOption{
						{Name: "Sonnet", Value: "sonnet-5@high"},
						{Name: "Sonnet small", Value: "sonnet-5"},
					}},
				},
			}}
			response.Modes = &protocol.SessionModeState{
				CurrentModeID: "default",
				AvailableModes: []protocol.SessionMode{
					{ID: "default", Name: "Default"},
					{ID: "acceptEdits", Name: "Accept edits"},
				},
			}
		} else if native {
			category := protocol.SessionConfigOptionCategoryModel
			response.ConfigOptions = []protocol.SessionConfigOption{{
				Category: &category,
				ID:       "model",
				Name:     "Model",
				Select: &protocol.SessionConfigSelect{
					CurrentValue: protocol.SessionConfigValueID(mainModel),
					Options: protocol.SessionConfigSelectOptions{Ungrouped: []protocol.SessionConfigSelectOption{{
						Name: "Native Codex", Value: protocol.SessionConfigValueID(mainModel),
					}}},
				},
			}}
		}
		return response, nil
	})
	conn.Handle(string(protocol.MethodSessionSetConfigOption), func(_ context.Context, _ string, params json.RawMessage) (any, error) {
		var request protocol.SetSessionConfigOptionRequest
		if err := json.Unmarshal(params, &request); err != nil || request.ValueID == nil {
			return nil, protocol.InvalidParams("task33 session/set_config_option", nil)
		}
		value := string(*request.ValueID)
		state.mu.Lock()
		if state.harness == "codex" {
			state.mainModel = value
			response := task33CodexConfigResponseLocked(state)
			state.mu.Unlock()
			return response, nil
		}
		if strings.Contains(value, "@") {
			state.mainModel = value
		} else {
			state.smallModel = value
			workspace := state.workspace
			state.mu.Unlock()
			if err := writeTask33Receipt(workspace, "task33-claude-small-model.receipt", value); err != nil {
				return nil, err
			}
			return task33ClaudeConfigResponse(state), nil
		}
		response := task33ClaudeConfigResponseLocked(state)
		state.mu.Unlock()
		return response, nil
	})
	conn.Handle(string(protocol.MethodSessionSetMode), func(context.Context, string, json.RawMessage) (any, error) {
		return protocol.SetSessionModeResponse{}, nil
	})
	conn.Handle(string(protocol.MethodSessionPrompt), func(ctx context.Context, _ string, params json.RawMessage) (any, error) {
		var request protocol.PromptRequest
		if err := json.Unmarshal(params, &request); err != nil {
			return nil, protocol.InvalidParams("task33 session/prompt", nil)
		}
		state.mu.Lock()
		harness := state.harness
		session := state.session
		modelAlias := state.mainModel
		workspace := state.workspace
		baseURL := state.baseURL
		token := state.token
		state.mu.Unlock()
		if modelAlias == "" {
			return nil, protocol.InvalidParams("task33 session/prompt model", nil)
		}
		if err := task33HelperGatewayRequest(ctx, harness, baseURL, token, modelAlias, task33PromptText(request)); err != nil {
			return nil, err
		}
		if harness == "claude-code" {
			if err := peer.SessionUpdate(ctx, protocol.SessionNotification{
				SessionID: session,
				Update: protocol.SessionUpdate{AgentMessageChunk: &protocol.ContentChunk{
					Content: protocol.ContentBlock{Text: &protocol.TextContent{Text: "task33 claude answer"}},
				}},
			}); err != nil {
				return nil, err
			}
			return protocol.PromptResponse{StopReason: protocol.StopReasonEndTurn}, nil
		}
		select {
		case <-state.cancel:
			if err := writeTask33Receipt(workspace, "task33-codex-cancel.receipt", "session/cancel"); err != nil {
				return nil, err
			}
			return protocol.PromptResponse{StopReason: protocol.StopReasonCancelled}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-conn.Done():
			return nil, protocol.InvalidParams("task33 connection closed", nil)
		}
	})
	conn.HandleNotify(string(protocol.MethodSessionCancel), func(_ context.Context, _ string, params json.RawMessage) {
		var notification protocol.CancelNotification
		if json.Unmarshal(params, &notification) == nil {
			state.cancelOnce.Do(func() { close(state.cancel) })
		}
	})

	close(ready)
	<-conn.Done()
	return 0
}

func task33ClaudeConfigResponse(state *task33ACPHelperState) protocol.SetSessionConfigOptionResponse {
	state.mu.Lock()
	defer state.mu.Unlock()
	return task33ClaudeConfigResponseLocked(state)
}

func task33ClaudeConfigResponseLocked(state *task33ACPHelperState) protocol.SetSessionConfigOptionResponse {
	category := protocol.SessionConfigOptionCategoryModel
	return protocol.SetSessionConfigOptionResponse{ConfigOptions: []protocol.SessionConfigOption{{
		Category: &category,
		ID:       "model",
		Name:     "Model",
		Select: &protocol.SessionConfigSelect{
			CurrentValue: protocol.SessionConfigValueID(state.mainModel),
			Options: protocol.SessionConfigSelectOptions{Ungrouped: []protocol.SessionConfigSelectOption{
				{Name: "Sonnet", Value: "sonnet-5@high"},
				{Name: "Sonnet small", Value: "sonnet-5"},
			}},
		},
	}}}
}

func task33CodexConfigResponseLocked(state *task33ACPHelperState) protocol.SetSessionConfigOptionResponse {
	category := protocol.SessionConfigOptionCategoryModel
	return protocol.SetSessionConfigOptionResponse{ConfigOptions: []protocol.SessionConfigOption{{
		Category: &category,
		ID:       "model",
		Name:     "Model",
		Select: &protocol.SessionConfigSelect{
			CurrentValue: protocol.SessionConfigValueID(state.mainModel),
			Options: protocol.SessionConfigSelectOptions{Ungrouped: []protocol.SessionConfigSelectOption{{
				Name: "Native Codex", Value: protocol.SessionConfigValueID(state.mainModel),
			}}},
		},
	}}}
}

func task33HelperGatewayRequest(ctx context.Context, harness, baseURL, token, modelAlias, prompt string) error {
	path := "/v1/responses"
	body := map[string]any{
		"model":     modelAlias,
		"reasoning": map[string]any{"effort": "low"},
		"input": []any{map[string]any{
			"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": prompt}},
		}},
		"max_output_tokens": 16,
	}
	if harness == "claude-code" {
		path = "/v1/messages"
		body = map[string]any{
			"model":         modelAlias,
			"output_config": map[string]any{"effort": "low"},
			"max_tokens":    16,
			"messages": []any{map[string]any{
				"role":    "user",
				"content": []any{map[string]any{"type": "text", "text": prompt}},
			}},
		}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+path, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("task33 helper gateway returned status %d", response.StatusCode)
	}
	return nil
}

func task33PromptText(request protocol.PromptRequest) string {
	for _, block := range request.Prompt {
		if block.Text != nil {
			return block.Text.Text
		}
	}
	return "task33"
}

func parseTask33Arg(key string) string {
	args := os.Args[1:]
	for i := 0; i+1 < len(args); i++ {
		if args[i] != "-c" {
			continue
		}
		name, value, ok := strings.Cut(args[i+1], "=")
		if ok && name == key {
			return value
		}
	}
	return ""
}

func writeTask33Receipt(workspace, name, value string) error {
	if workspace == "" {
		return fmt.Errorf("task33 helper: empty workspace for receipt")
	}
	return os.WriteFile(filepath.Join(workspace, name), []byte(value), 0o600)
}
