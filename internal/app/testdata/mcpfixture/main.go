// Command mcpfixture is a minimal, hand-rolled MCP stdio server used only by
// carbon's own live round-trip integration test
// (internal/app/mcp_live_integration_test.go, //go:build integration).
//
// It deliberately does NOT use github.com/modelcontextprotocol/go-sdk: the
// sibling mcp module already depends on that SDK and already tests MCP
// protocol conformance against it (see mcp/internal/mcptest), and adding it
// to carbon as well was explicitly declined (carbon's CLAUDE.md requires
// approval before any new third-party dependency, even test-only). Carbon's
// own integration test exists to prove ITS assembly/gating/env-baseline/
// degradation behavior against a REAL subprocess and REAL newline-delimited
// JSON-RPC framing -- not MCP protocol correctness, which is out of scope
// here and already covered elsewhere. So this fixture implements exactly the
// wire surface that test's scenarios need, and nothing more.
//
// # Wire shape
//
// Newline-delimited JSON-RPC 2.0 over stdin/stdout (one message per line,
// flushed immediately), matching the MCP spec and verified empirically
// against the real client in github.com/looprig/mcp/pkg/client (which wraps
// the official go-sdk client internally, so its wire behavior IS the real
// SDK's). Two things worth calling out because they are not obvious from the
// spec text alone:
//
//   - The SDK client tries a SEP-2575 "server/discover" stateless-first
//     handshake before falling back to the legacy handshake. This fixture
//     refuses it with an ordinary JSON-RPC "method not found", which is
//     exactly what makes the real client fall back to the legacy
//     initialize -> notifications/initialized -> tools/list -> tools/call
//     sequence below -- the only sequence this fixture speaks.
//   - The client validates the server's initialize response protocolVersion
//     against its OWN fixed supported-version list, so this fixture cannot
//     invent a version string; it echoes back whatever the client sent
//     (which, after the discover fallback, is always a version the client
//     itself already supports).
//
// # Behavior
//
// One tool, "echo": given {"text": "..."}, it replies with that text plus
// two environment facts it observed in ITS OWN process --  PATH and one
// fixed, deliberately UNLISTED variable name (see echoUnlistedEnvVar) -- so
// the integration test can assert the child's environment baseline without
// needing a second, purpose-built tool.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// echoUnlistedEnvVar is an environment variable name the "echo" tool always
// reports but that mcp_live_integration_test.go's env-baseline scenario
// never lists in the spawned binding's mcp.json env map and never adds to
// the fixed stdio pass-through baseline (PATH/HOME/TMPDIR/LANG/LC_ALL). The
// test sets it in ITS OWN process (t.Setenv) and asserts it does NOT reach
// this child. The literal is duplicated (not imported) in the test file:
// this is a separate `package main`, not an importable package.
const echoUnlistedEnvVar = "CARBON_MCP_FIXTURE_UNLISTED_TEST_VAR"

// fallbackProtocolVersion is used only if a request's own protocolVersion is
// somehow empty (should not happen against the real client; defensive).
const fallbackProtocolVersion = "2025-11-25"

const codeMethodNotFound = -32601

// rpcIn is the generic incoming JSON-RPC 2.0 message shape: a request when ID
// is present, a notification when it is absent.
type rpcIn struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// rpcErr is a JSON-RPC 2.0 error object.
type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// rpcOut is the outgoing response envelope. ID is filled in by the caller
// (run) from the request it answers; a handler never sets it, so a handler
// can never mismatch a reply to the wrong request.
type rpcOut struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcErr         `json:"error,omitempty"`
}

func main() {
	if err := run(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "mcpfixture: %v\n", err)
		os.Exit(1)
	}
}

// run drives the read-eval-write loop until stdin closes. Per the MCP stdio
// shutdown contract (a client signals "stop" by closing stdin), a clean EOF
// with nothing outstanding is a normal, zero-status exit -- and this fixture
// never has anything outstanding at EOF, because it is strictly
// synchronous: it always finishes writing one response before reading the
// next request.
func run(r io.Reader, w io.Writer) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4<<20)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var in rpcIn
		if err := json.Unmarshal(line, &in); err != nil {
			// Malformed input would be this fixture's own bug (its only
			// caller is this test's real MCP client), not something to
			// crash the process over. Report to stderr -- stdout is the
			// protocol and must never carry anything else -- and keep
			// serving the rest of the session.
			fmt.Fprintf(os.Stderr, "mcpfixture: decode: %v\n", err)
			continue
		}
		if len(in.ID) == 0 {
			// A notification (e.g. notifications/initialized). MCP never
			// replies to one, successfully or not.
			continue
		}
		out := handle(in)
		out.JSONRPC = "2.0"
		out.ID = in.ID
		if err := enc.Encode(&out); err != nil {
			return fmt.Errorf("write response: %w", err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read request: %w", err)
	}
	return nil
}

// handle dispatches one request to its typed result. It never returns an
// error itself -- every failure this fixture can produce is a JSON-RPC
// error object in the response, exactly like a real server's would be.
func handle(in rpcIn) rpcOut {
	switch in.Method {
	case "initialize":
		return handleInitialize(in)
	case "tools/list":
		return handleToolsList()
	case "tools/call":
		return handleToolsCall(in)
	default:
		// Includes "server/discover" (SEP-2575's stateless-first probe): see
		// this file's package doc for why refusing it as an ordinary unknown
		// method is exactly the response that makes the real client fall
		// back to the legacy handshake this fixture actually speaks.
		return rpcOut{Error: &rpcErr{
			Code:    codeMethodNotFound,
			Message: fmt.Sprintf("method not found: %s", in.Method),
		}}
	}
}

type initializeParams struct {
	ProtocolVersion string `json:"protocolVersion"`
}

// handleInitialize answers the legacy MCP handshake. The protocol version is
// mirrored from the request: the real client already validated that version
// against its own supported set before sending it (see this file's package
// doc), so echoing it back is always accepted, and it avoids this fixture
// having to hardcode or guess a version string of its own.
func handleInitialize(in rpcIn) rpcOut {
	var params initializeParams
	_ = json.Unmarshal(in.Params, &params) // best-effort; see fallback below
	version := params.ProtocolVersion
	if version == "" {
		version = fallbackProtocolVersion
	}
	return rpcOut{Result: map[string]any{
		"protocolVersion": version,
		"capabilities": map[string]any{
			"tools": map[string]any{},
		},
		"serverInfo": map[string]any{
			"name":    "carbon-mcp-fixture",
			"version": "0.0.1",
		},
	}}
}

// handleToolsList answers with the one tool this fixture ever offers: echo.
func handleToolsList() rpcOut {
	return rpcOut{Result: map[string]any{
		"tools": []any{
			map[string]any{
				"name": "echo",
				"description": "Echoes back the given text, plus this process's own PATH " +
					"and CARBON_MCP_FIXTURE_UNLISTED_TEST_VAR environment values, " +
					"for carbon's env-baseline integration test.",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"text": map[string]any{"type": "string"},
					},
					"required": []string{"text"},
				},
			},
		},
	}}
}

type callToolParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type echoArgs struct {
	Text string `json:"text"`
}

// handleToolsCall answers a tools/call. The only tool is "echo"; anything
// else is a protocol-level tool error result (IsError), matching the MCP
// convention that a bad tool name is a result, not a JSON-RPC error.
func handleToolsCall(in rpcIn) rpcOut {
	var params callToolParams
	if err := json.Unmarshal(in.Params, &params); err != nil {
		return rpcOut{Result: errorToolResult(fmt.Sprintf("bad params: %v", err))}
	}
	if params.Name != "echo" {
		return rpcOut{Result: errorToolResult(fmt.Sprintf("unknown tool %q", params.Name))}
	}
	var args echoArgs
	if len(params.Arguments) > 0 {
		if err := json.Unmarshal(params.Arguments, &args); err != nil {
			return rpcOut{Result: errorToolResult(fmt.Sprintf("bad arguments: %v", err))}
		}
	}

	path := os.Getenv("PATH")
	unlisted := os.Getenv(echoUnlistedEnvVar)
	text := fmt.Sprintf("echo:%s\nPATH=%s\nUNLISTED=%s", args.Text, path, unlisted)
	return rpcOut{Result: map[string]any{
		"content": []any{
			map[string]any{"type": "text", "text": text},
		},
	}}
}

func errorToolResult(msg string) map[string]any {
	return map[string]any{
		"isError": true,
		"content": []any{
			map[string]any{"type": "text", "text": msg},
		},
	}
}
