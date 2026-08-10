//go:build integration

package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/inference"
	stream "github.com/looprig/inference/stream"
	mcpclient "github.com/looprig/mcp/pkg/client"
)

// mcp_live_integration_test.go is Task 12's live, real-process stdio MCP
// round-trip suite: it proves this package's mcp.go/mcpconfig.go/assembly.go
// assembly against a REAL child process speaking REAL newline-delimited
// JSON-RPC (internal/app/testdata/mcpfixture, a hand-rolled fixture -- see
// that package's own doc for why it does not use the official go-sdk), not
// against the fake/no-network doubles mcp_test.go and mcp_integration_test.go
// use. It drives every scenario through the exact production API surface the
// CLI/TUI uses (openRuntimeAgent, sessionadapter's Submit/Subscribe/
// RespondGate, RuntimeAgent.Close) -- see permission_review_integration_test.go,
// this file's closest sibling in style and build tag, for the established
// pattern this file follows.
//
// # Why a hand-rolled fixture
//
// mcp/internal/mcptest is that module's OWN fixture, built on
// github.com/modelcontextprotocol/go-sdk -- and it is an `internal` package
// of a different module, so it is not importable here regardless. Adding the
// go-sdk to carbon itself as a new test-only dependency was explicitly
// declined (carbon's CLAUDE.md requires approval before any new third-party
// dependency). Carbon's own integration surface is its assembly/gating/
// env-baseline/degradation behavior, not MCP protocol conformance -- that is
// the mcp module's job, already covered by its own test suite -- so a
// minimal, purpose-built fixture speaking only the wire surface these
// scenarios need is the right scope here, not a full protocol
// implementation.

// mcpFixturePkg is the import path buildMCPFixture compiles, mirroring
// github.com/looprig/mcp/internal/mcptest.BuildFixture's own pattern (read
// for reference while building this file): a real `go build` into a
// t.TempDir()-scoped binary, once per top-level test (cheap after the first,
// via Go's build cache).
const mcpFixturePkg = "github.com/looprig/carbon/internal/app/testdata/mcpfixture"

// mcpFixtureBuildTimeout bounds the fixture's build. A cold build is well
// under a second (it has no dependencies beyond the standard library); this
// is a liveness guard, not a latency budget.
const mcpFixtureBuildTimeout = 3 * time.Minute

// mcpFixtureUnlistedEnvVar mirrors the exact literal
// testdata/mcpfixture/main.go hardcodes as echoUnlistedEnvVar. The two
// cannot share a constant (the fixture is a separate `package main`, not an
// importable package), so this is a second copy of the same literal by
// necessity; TestMCPLiveStdioChildEnvBaseline is what keeps the two honest
// (a name mismatch would make that test's "must NOT be visible" assertion
// vacuously true instead of a real proof).
const mcpFixtureUnlistedEnvVar = "CARBON_MCP_FIXTURE_UNLISTED_TEST_VAR"

// buildMCPFixture compiles testdata/mcpfixture and returns the path to the
// binary, t.TempDir()-scoped so it is removed at test end and never races
// another test over the same path.
func buildMCPFixture(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), mcpFixtureBuildTimeout)
	defer cancel()

	out := filepath.Join(t.TempDir(), "mcpfixture")
	root, err := mcpFixtureModuleRoot(ctx)
	if err != nil {
		t.Fatalf("mcp fixture: %v", err)
	}

	// Explicit argv, never a shell string; out is under this test's own
	// TempDir. Mirrors mcptest.BuildFixture's own command exactly.
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", out, mcpFixturePkg) // #nosec G204 -- fixed argv; out is under the test's own TempDir
	cmd.Dir = root
	cmd.Env = append(cmd.Environ(), "CGO_ENABLED=0")
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("mcp fixture: building %s: %v\n%s", mcpFixturePkg, err, combined)
	}
	return out
}

// mcpFixtureModuleRoot returns carbon's own module root, asking the go tool
// rather than walking up from this file's path.
func mcpFixtureModuleRoot(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "go", "env", "GOMOD") // #nosec G204 -- fixed argv, no external input
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("locating module root: %w", err)
	}
	gomod := strings.TrimSpace(string(out))
	if gomod == "" || gomod == "/dev/null" {
		return "", errors.New("locating module root: no go.mod found")
	}
	return filepath.Dir(gomod), nil
}

// mcpLiveServerSpec is one mcpServers entry this file writes into a real
// mcp.json, in the same wire shape mcpconfig.go's mcpServerConfig decodes.
type mcpLiveServerSpec struct {
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Roles   []string          `json:"roles,omitempty"`
}

// writeLiveMCPConfig writes a real <home>/mcp.json naming one binding,
// reusing writeModelConfigFixture's file-hygiene write (modelconfig_test.go,
// untagged -- available here) for the same 0600 mode mcp.json's own hygiene
// contract requires.
func writeLiveMCPConfig(t *testing.T, home, binding string, spec mcpLiveServerSpec) {
	t.Helper()
	body := map[string]any{"mcpServers": map[string]mcpLiveServerSpec{binding: spec}}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal mcp.json fixture: %v", err)
	}
	writeModelConfigFixture(t, filepath.Join(home, "mcp.json"), data, 0o600)
}

// openLiveMCPAgent builds a REAL RuntimeAgent over openRuntimeAgent -- the
// exact production session-open path (Task 10) -- against a NEW in-memory
// store and a throwaway workspace, isolated per test the same way
// mcp_integration_test.go's mustOpenStores/newSessionOverStores fixtures are.
func openLiveMCPAgent(t *testing.T, ctx context.Context, client inference.Client, cfg Config, interactive bool) *RuntimeAgent {
	t.Helper()
	stores := mustOpenStores(t)
	agent, err := openRuntimeAgent(ctx, client, newModelFactoryFor(testModel()), cfg, stores, t.TempDir(), SessionSelector{}, interactive)
	if err != nil {
		t.Fatalf("openRuntimeAgent() error = %v", err)
	}
	t.Cleanup(func() { _ = agent.Close(context.Background()) })
	return agent
}

// waitMCPBindingSettled polls agent.mgr.Status() (a cheap, synchronous,
// always-current query -- not an ephemeral event a subscriber could race)
// until binding reaches a terminal connection state (Ready, Degraded,
// Failed, or Closed/Closing), or timeout elapses. It is a bounded poll, not
// a blind fixed sleep: the real subprocess's dial+handshake time is not
// something this test should guess at.
func waitMCPBindingSettled(t *testing.T, agent *RuntimeAgent, binding string, timeout time.Duration) mcpclient.Status {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		for _, st := range agent.mgr.Status() {
			if st.Name != binding {
				continue
			}
			switch st.Client.State {
			case mcpclient.StateReady, mcpclient.StateDegraded, mcpclient.StateFailed, mcpclient.StateClosed, mcpclient.StateClosing:
				return st.Client
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("mcp binding %q did not settle within %v", binding, timeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitMCPBindingReady is waitMCPBindingSettled plus the assertion this
// file's happy-path scenarios actually want: the binding reached
// StateReady, not merely some terminal state.
func waitMCPBindingReady(t *testing.T, agent *RuntimeAgent, binding string, timeout time.Duration) {
	t.Helper()
	st := waitMCPBindingSettled(t, agent, binding, timeout)
	if st.State != mcpclient.StateReady {
		t.Fatalf("mcp binding %q settled to state %v, want StateReady (failure: %+v)", binding, st.State, st.Failure)
	}
}

// mcpEchoScript is a deterministic fake inference.Client driving ONE
// Carbon turn that calls one named tool exactly once with argsJSON, then
// answers with a final text message once it observes the tool's own result
// text. It mirrors permission_review_integration_test.go's bashScript
// (same file, //go:build integration, so that type is already compiled
// alongside this one) generalized to an arbitrary tool name instead of a
// hardcoded "Bash".
type mcpEchoScript struct {
	mu               sync.Mutex
	toolName         string
	argsJSON         string
	calls            int
	observedToolText string
	awaitingResult   bool
}

func (s *mcpEchoScript) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	return nil, errors.New("mcpEchoScript.Invoke not used")
}

func (s *mcpEchoScript) Stream(ctx context.Context, req inference.Request) (*stream.StreamReader[content.Chunk], error) {
	s.mu.Lock()
	s.calls++
	var chunks []content.Chunk
	if s.awaitingResult {
		s.observedToolText = lastToolText(req)
		s.awaitingResult = false
		chunks = finalText("integration turn complete")
	} else {
		s.awaitingResult = true
		chunks = namedToolCall("mcp-live-1", s.toolName, s.argsJSON)
	}
	s.mu.Unlock()
	i := 0
	return stream.NewStreamReader(func() (content.Chunk, error) {
		if i == len(chunks) {
			return nil, io.EOF
		}
		c := chunks[i]
		i++
		return c, nil
	}, nil), nil
}

// waitPermissionRequestedAndGateOpened watches sub for both the
// event.PermissionRequested carrying the exact tool.Request a gated MCP call
// built (ToolName and Requirements[].Scope -- event.GateOpened's own public
// envelope carries no payload at all, per runner.go's own doc: "GateOpened
// ... carries NO private payload") and the matching event.GateOpened, up to
// timeout, and returns once BOTH have arrived.
//
// It watches for both in a single pass rather than waiting for one and then
// the other, because they are NOT delivered in the order their names
// suggest: verified empirically against a real gate round trip, GateOpened
// is published from inside ActivateGate (harness/internal/sessionruntime/
// gates.go), which runs and is acked BEFORE approvalRequesterFor ever emits
// PermissionRequested (harness/internal/loopruntime/runner.go). A helper
// that watched for PermissionRequested first would silently discard
// GateOpened while scanning past it, and a subsequent, separate wait for
// GateOpened would then hang.
func waitPermissionRequestedAndGateOpened(t *testing.T, ctx context.Context, sub event.Subscription, timeout time.Duration) (event.PermissionRequested, gate.ID, uuid.UUID) {
	t.Helper()
	deadline := time.After(timeout)
	var (
		requested      event.PermissionRequested
		haveRequested  bool
		gateID         gate.ID
		toolExecution  uuid.UUID
		haveGateOpened bool
	)
	for !haveRequested || !haveGateOpened {
		select {
		case delivery := <-sub.Events():
			switch ev := delivery.Event.(type) {
			case event.PermissionRequested:
				requested = ev
				haveRequested = true
			case event.GateOpened:
				if ev.Gate.Kind == gate.KindPermission {
					gateID = ev.Gate.ID
					toolExecution = ev.Gate.Subject.ToolExecutionID
					haveGateOpened = true
				}
			}
		case <-deadline:
			t.Fatalf("did not observe both PermissionRequested and GateOpened within %v (requested=%v, gateOpened=%v)", timeout, haveRequested, haveGateOpened)
		case <-ctx.Done():
			t.Fatalf("context done waiting for permission events: %v", ctx.Err())
		}
	}
	return requested, gateID, toolExecution
}

// waitPermissionDecided watches sub for the next event.PermissionDecided, up
// to timeout. This is the headless counterpart to waitPermissionRequested:
// a headless evaluator never opens an interactive gate at all (gate.go's
// EvaluationApprovalRequired path returns straight to resolveAccess), so
// PermissionDecided{Effect: Deny} is the observable trail instead.
func waitPermissionDecided(t *testing.T, ctx context.Context, sub event.Subscription, timeout time.Duration) event.PermissionDecided {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case delivery := <-sub.Events():
			if pd, ok := delivery.Event.(event.PermissionDecided); ok {
				return pd
			}
		case <-deadline:
			t.Fatal("permission decided event did not arrive within deadline")
		case <-ctx.Done():
			t.Fatalf("context done waiting for permission decided: %v", ctx.Err())
		}
	}
}

// waitToolCallCompleted watches sub for the next event.ToolCallCompleted, up
// to timeout.
func waitToolCallCompleted(t *testing.T, ctx context.Context, sub event.Subscription, timeout time.Duration) event.ToolCallCompleted {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case delivery := <-sub.Events():
			if tc, ok := delivery.Event.(event.ToolCallCompleted); ok {
				return tc
			}
		case <-deadline:
			t.Fatal("tool call completed event did not arrive within deadline")
		case <-ctx.Done():
			t.Fatalf("context done waiting for tool call completed: %v", ctx.Err())
		}
	}
}

// waitIntegrationStatus watches sub for the next event.IntegrationStatus
// naming binding, up to timeout.
// waitIntegrationStatus watches sub for binding's IntegrationStatus reaching
// want, up to timeout. A connecting binding publishes more than one status
// on its way there (at least Starting, then its eventual outcome -- events.go's
// own doc: "the latest one supersedes every earlier one"), so this skips
// past any status for binding that does not yet match want rather than
// returning on the first one seen.
func waitIntegrationStatus(t *testing.T, ctx context.Context, sub event.Subscription, binding string, want event.IntegrationState, timeout time.Duration) event.IntegrationStatus {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case delivery := <-sub.Events():
			if is, ok := delivery.Event.(event.IntegrationStatus); ok && is.Name == binding && is.State == want {
				return is
			}
		case <-deadline:
			t.Fatalf("integration status %q for binding %q did not arrive within deadline", want, binding)
		case <-ctx.Done():
			t.Fatalf("context done waiting for integration status: %v", ctx.Err())
		}
	}
}

// ================================================================
// Assertion 1: session opens with one stdio binding; primer sees
// mcp__<binding>__<tool>.
// ================================================================

// TestMCPLiveStdioSessionOpensAndPrimerSeesQualifiedTool proves the whole
// construction/connect/catalog path against a real subprocess: a session
// opened with one real stdio MCP server produces exactly one Ready binding,
// and the active primer's own visible tool set (Manager.SessionTools, the
// same source the Adopter installs a Loop's runtime toolset from) carries
// the qualified model-facing name "mcp__fixture__echo" -- catalog.go's
// "mcp__<binding>__<raw-tool>" scheme, confirmed by reading
// mcp/internal/catalog/identity.go directly rather than assumed.
func TestMCPLiveStdioSessionOpensAndPrimerSeesQualifiedTool(t *testing.T) {
	fixture := buildMCPFixture(t)
	home := t.TempDir()
	writeLiveMCPConfig(t, home, "fixture", mcpLiveServerSpec{Command: fixture})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	agent := openLiveMCPAgent(t, ctx, &fakeLLM{}, Config{HomeDir: home}, false)

	waitMCPBindingReady(t, agent, "fixture", 15*time.Second)

	statuses := agent.mgr.Status()
	if len(statuses) != 1 {
		t.Fatalf("mgr.Status() = %+v, want exactly one binding", statuses)
	}
	if statuses[0].Name != "fixture" {
		t.Errorf("mgr.Status()[0].Name = %q, want %q", statuses[0].Name, "fixture")
	}

	loopID := agent.sess.ActiveLoop().ID()
	defs := agent.mgr.SessionTools(loopID, string(activePrimerName))
	if len(defs) != 1 {
		t.Fatalf("mgr.SessionTools(primer) = %d definitions, want 1", len(defs))
	}
	names := defs[0].ProducedToolNames()
	if len(names) != 1 || names[0] != "mcp__fixture__echo" {
		t.Errorf("SessionTools(primer)[0].ProducedToolNames() = %v, want [mcp__fixture__echo]", names)
	}
}

// ================================================================
// Assertion 2: first invoke raises a gate ask with identity
// mcp:<binding>:<tool> (headless: typed approval-required denial).
// ================================================================

// TestMCPLiveStdioFirstInvokeInteractiveRaisesPermissionGate drives a real
// Submit() -> primer calls mcp__fixture__echo -> PermissionRequested/
// GateOpened round trip, and asserts the exact identity design's summary
// promises: ToolInvokeIdentity(binding, rawTool) = "mcp:fixture:echo" (read
// directly from mcp/pkg/harness/tools.go's ToolInvokeIdentity, which
// resolves this file's own task brief's "mcp:<binding>:<tool>" vs
// "mcp:<binding>:<raw-tool>" ambiguity: it is the RAW tool name). It then
// approves the gate and confirms the call actually executes against the
// real fixture.
func TestMCPLiveStdioFirstInvokeInteractiveRaisesPermissionGate(t *testing.T) {
	fixture := buildMCPFixture(t)
	home := t.TempDir()
	writeLiveMCPConfig(t, home, "fixture", mcpLiveServerSpec{Command: fixture})

	client := &mcpEchoScript{toolName: "mcp__fixture__echo", argsJSON: `{"text":"hello-mcp"}`}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	agent := openLiveMCPAgent(t, ctx, client, Config{HomeDir: home}, true)

	waitMCPBindingReady(t, agent, "fixture", 15*time.Second)
	// Force the primer's toolset up to date before Submit(): the eager
	// Install() call openRuntimeAgent's attach() makes runs BEFORE Start()
	// necessarily settles an OPTIONAL binding's real subprocess dial (Start
	// only blocks for Required bindings, and mcpBindingFor never marks one
	// Required), so the very first Install can race the connection and
	// install an empty toolset. Calling Install again after the binding is
	// confirmed Ready removes that race deterministically, using the exact
	// production API (Adopter.Install is documented idempotent/cheap when
	// nothing changed) rather than a sleep.
	if err := agent.adopter.Install(ctx, agent.sess.ActiveLoop().ID(), string(activePrimerName)); err != nil {
		t.Fatalf("adopter.Install() error = %v", err)
	}

	sub, err := agent.Subscribe(event.EventFilter{Ephemeral: event.LoopScope{All: true}, Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	defer func() { _ = sub.Close() }()

	if _, err := agent.Submit(ctx, []content.Block{&content.TextBlock{Text: "call the echo tool"}}); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}

	requested, gateID, _ := waitPermissionRequestedAndGateOpened(t, ctx, sub, 10*time.Second)
	if requested.Request.ToolName != "mcp__fixture__echo" {
		t.Errorf("PermissionRequested.Request.ToolName = %q, want %q", requested.Request.ToolName, "mcp__fixture__echo")
	}
	if len(requested.Request.Requirements) != 1 {
		t.Fatalf("PermissionRequested.Request.Requirements = %+v, want exactly one", requested.Request.Requirements)
	}
	wantIdentity := "mcp:fixture:echo"
	if got := requested.Request.Requirements[0].Scope; got != wantIdentity {
		t.Errorf("Requirements[0].Scope = %q, want %q", got, wantIdentity)
	}
	if got := requested.Request.Requirements[0].Match; got != wantIdentity {
		t.Errorf("Requirements[0].Match = %q, want %q", got, wantIdentity)
	}
	if requested.Request.Requirements[0].Kind != capabilityToolInvoke {
		t.Errorf("Requirements[0].Kind = %q, want %q", requested.Request.Requirements[0].Kind, capabilityToolInvoke)
	}

	if err := respondApprove(t, ctx, agent, gateID); err != nil {
		t.Fatalf("RespondGate() error = %v", err)
	}
	mustTurnDone(t, drainToTurnTerminal(t, ctx, sub))

	client.mu.Lock()
	observed := client.observedToolText
	client.mu.Unlock()
	if !strings.Contains(observed, "echo:hello-mcp") || !strings.Contains(observed, "PATH=") {
		t.Errorf("observed tool result = %q, want the real fixture's echo output", observed)
	}
}

// TestMCPLiveStdioFirstInvokeHeadlessDeniesWithTypedApprovalRequired proves
// the headless half of assertion 2: with no interactive approver wired
// (gate.NewHeadlessEvaluator, accessGate.Authorize's headless branch,
// toolsets.go), the SAME gated tool.invoke requirement never opens an
// askable gate at all -- it resolves straight to
// *gate.EvaluationError{Kind: EvaluationApprovalRequired} inside
// resolveAccess (harness/internal/loopruntime/runner.go), which the caller
// documents resolving to event.PermissionDecided{Effect: Deny,
// Reason: "access_error"} plus a model-visible "error: permission denied"
// tool result -- never a PermissionRequested/GateOpened, and never a hang.
func TestMCPLiveStdioFirstInvokeHeadlessDeniesWithTypedApprovalRequired(t *testing.T) {
	fixture := buildMCPFixture(t)
	home := t.TempDir()
	writeLiveMCPConfig(t, home, "fixture", mcpLiveServerSpec{Command: fixture})

	client := &mcpEchoScript{toolName: "mcp__fixture__echo", argsJSON: `{"text":"hello-mcp"}`}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	agent := openLiveMCPAgent(t, ctx, client, Config{HomeDir: home}, false)

	waitMCPBindingReady(t, agent, "fixture", 15*time.Second)
	if err := agent.adopter.Install(ctx, agent.sess.ActiveLoop().ID(), string(activePrimerName)); err != nil {
		t.Fatalf("adopter.Install() error = %v", err)
	}

	sub, err := agent.Subscribe(event.EventFilter{Ephemeral: event.LoopScope{All: true}, Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	defer func() { _ = sub.Close() }()

	if _, err := agent.Submit(ctx, []content.Block{&content.TextBlock{Text: "call the echo tool"}}); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}

	decided := waitPermissionDecided(t, ctx, sub, 10*time.Second)
	if decided.Effect != event.PermissionEffectDeny {
		t.Errorf("PermissionDecided.Effect = %q, want %q", decided.Effect, event.PermissionEffectDeny)
	}
	if decided.Subject != "mcp__fixture__echo" {
		t.Errorf("PermissionDecided.Subject = %q, want %q", decided.Subject, "mcp__fixture__echo")
	}

	completed := waitToolCallCompleted(t, ctx, sub, 10*time.Second)
	if !completed.IsError {
		t.Errorf("ToolCallCompleted.IsError = false, want true (headless denial)")
	}
	if !strings.Contains(completed.ResultPreview, "permission denied") {
		t.Errorf("ToolCallCompleted.ResultPreview = %q, want it to contain %q", completed.ResultPreview, "permission denied")
	}

	// The turn still completes normally: a headless denial is a fail-closed
	// tool result the model reads and reacts to, never a faulted session.
	mustTurnDone(t, drainToTurnTerminal(t, ctx, sub))

	client.mu.Lock()
	observed := client.observedToolText
	client.mu.Unlock()
	if !strings.Contains(observed, "permission denied") {
		t.Errorf("observed tool result = %q, want the headless denial text", observed)
	}
}

// ================================================================
// Assertion 3: a Carbon-only binding is hidden from legacy role names. The
// legacy names below are intentional rejection fixtures.
// ================================================================

// TestMCPLiveStdioCarbonOnlyVisibility proves Visibility restriction against
// a real, live-connected binding: explicit Carbon visibility reaches the
// active Carbon primer, while former role names remain hidden. It uses
// Manager.SessionTools directly because the initial discovery populates the
// live catalog before any later adoption refresh is involved.
func TestMCPLiveStdioCarbonOnlyVisibility(t *testing.T) {
	fixture := buildMCPFixture(t)
	home := t.TempDir()
	writeLiveMCPConfig(t, home, "fixture", mcpLiveServerSpec{Command: fixture, Roles: []string{"carbon"}})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	agent := openLiveMCPAgent(t, ctx, &fakeLLM{}, Config{HomeDir: home}, false)

	waitMCPBindingReady(t, agent, "fixture", 15*time.Second)

	primerLoopID := agent.sess.ActiveLoop().ID()
	primerDefs := agent.mgr.SessionTools(primerLoopID, string(activePrimerName))
	if len(primerDefs) != 1 || len(primerDefs[0].ProducedToolNames()) != 1 || primerDefs[0].ProducedToolNames()[0] != "mcp__fixture__echo" {
		t.Fatalf("SessionTools(primer) = %+v, want exactly [mcp__fixture__echo] (Carbon visibility)", primerDefs)
	}

	// Any fresh, non-colliding loop id works here: Visibility's Named()
	// selector matches on loop NAME alone and ignores loopID entirely (the
	// same established pattern mcp_test.go's own
	// TestMCPDefinitionsStdioHappyPath documents and relies on).
	for _, legacy := range []string{"planner", "builder", "reviewer"} {
		legacyDefs := agent.mgr.SessionTools(mustUUID(t), legacy)
		if len(legacyDefs) != 0 {
			t.Errorf("SessionTools(%s) = %+v, want none for legacy role", legacy, legacyDefs)
		}
	}
}

// ================================================================
// Assertion 4: a resolvable-but-dead server degrades: session opens, tools
// absent, an integration status event is observed.
// ================================================================

// TestMCPLiveStdioDeadServerDegradesSessionOpen proves the optional-binding
// degradation path (design's "Required and optional servers") against a
// real, resolvable command that exits immediately: /bin/sh -c "sleep 0.2;
// exit 1" (resolvable via exec.LookPath, so it passes mcpDefinitions'
// construction-time check same as any other stdio spec -- the failure is a
// RUNTIME connect failure, not a config error). The session still opens
// successfully, the dead binding's tools are simply absent from every loop,
// and the Manager's own event.IntegrationStatus reaches the session's event
// stream naming it Failed.
//
// This test manually replicates openRuntimeAgent's own composition (the
// same private helpers it calls, in the same order -- buildSessionAccess,
// newMCPSessionAssembly, carbonTestDefinition, newPermissionReviewRegistration,
// openSessionWithDefinition, mcpSessionAssembly.attach) instead of calling
// it directly, for one reason: it needs to Subscribe() to the session's
// events BEFORE attach() runs Start() and the dead binding's connect
// attempt begins. event.IntegrationStatus is Ephemeral (events.go's own
// doc: "the latest one supersedes every earlier one"), so a subscriber that
// only exists AFTER openRuntimeAgent has already returned could genuinely
// race a near-instant failure and never see it -- the small deliberate
// sleep in the dead command's own exit widens that window further, but
// subscribing before Start() removes the race outright rather than
// widening it and hoping.
func TestMCPLiveStdioDeadServerDegradesSessionOpen(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	home := t.TempDir()
	writeLiveMCPConfig(t, home, "dead", mcpLiveServerSpec{
		Command: "/bin/sh",
		Args:    []string{"-c", "sleep 0.2; exit 1"},
	})
	cfg := Config{HomeDir: home}
	root := t.TempDir()

	access, err := buildSessionAccess(cfg, root, false)
	if err != nil {
		t.Fatalf("buildSessionAccess() error = %v", err)
	}
	t.Cleanup(func() { _ = access.Close() })
	cfg.AccessConfigRev = access.configRev

	mcpAssembly, err := newMCPSessionAssembly(cfg)
	if err != nil {
		t.Fatalf("newMCPSessionAssembly() error = %v", err)
	}
	cfg.MCPConfigRev = mcpAssembly.configRev()

	client := &fakeLLM{}
	definition, err := carbonTestDefinition(client, testModel(), cfg, access)
	if err != nil {
		t.Fatalf("carbonTestDefinition() error = %v", err)
	}
	permissionReview, err := newPermissionReviewRegistration(cfg, client)
	if err != nil {
		t.Fatalf("newPermissionReviewRegistration() error = %v", err)
	}
	stores := mustOpenStores(t)
	adapter, err := openSessionWithDefinition(ctx, definition, cfg, stores, root, SessionSelector{}, permissionReview)
	if err != nil {
		t.Fatalf("openSessionWithDefinition() error = %v", err)
	}

	sub, err := adapter.Subscribe(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	defer func() { _ = sub.Close() }()

	if err := mcpAssembly.attach(ctx, adapter.Controller(), false); err != nil {
		_ = adapter.Close(ctx)
		t.Fatalf("attach() error = %v, want session open to SUCCEED even though the binding will fail (optional, Required: false)", err)
	}
	agent := newRuntimeAgentWithMCP(adapter, adapter.Controller(), root, access, mcpAssembly.manager, mcpAssembly.adopter, mcpAssembly.recorder, cfg.PrimerAlias, cfg.PrimerEfforts, cfg.PrimerCandidates)
	t.Cleanup(func() { _ = agent.Close(context.Background()) })

	// Session opened successfully despite the dead binding: this alone is
	// the degradation contract's first half.
	if agent.mgr == nil {
		t.Fatal("agent.mgr = nil, want a constructed Manager even for an all-dead-binding session")
	}

	status := waitIntegrationStatus(t, ctx, sub, "dead", event.IntegrationFailed, 10*time.Second)
	if status.Source != "mcp" {
		t.Errorf("IntegrationStatus.Source = %q, want %q", status.Source, "mcp")
	}

	st := waitMCPBindingSettled(t, agent, "dead", 10*time.Second)
	if st.State != mcpclient.StateFailed {
		t.Errorf("mgr.Status() dead binding state = %v, want StateFailed", st.State)
	}

	loopID := agent.sess.ActiveLoop().ID()
	defs := agent.mgr.SessionTools(loopID, string(activePrimerName))
	if len(defs) != 0 {
		t.Errorf("SessionTools(primer) = %+v, want none: the dead binding's tools must be absent from every loop", defs)
	}
}

// ================================================================
// Assertion 5: child env baseline -- server sees PATH, does not see an
// unlisted parent var.
// ================================================================

// TestMCPLiveStdioChildEnvBaseline proves mcpEnvVarsFrom/mcpEnvPassThrough's
// fixed baseline end to end: the fixture's own "echo" tool reports the PATH
// and mcpFixtureUnlistedEnvVar values it observed in ITS OWN process, and
// this test sets mcpFixtureUnlistedEnvVar in the TEST's process (never in
// the mcp.json spec's env map, and it is not one of the fixed pass-through
// names PATH/HOME/TMPDIR/LANG/LC_ALL) -- so a leaked value would mean the
// stdio transport's allowlist regressed to inheriting this process's wider
// environment.
func TestMCPLiveStdioChildEnvBaseline(t *testing.T) {
	t.Setenv(mcpFixtureUnlistedEnvVar, "should-not-reach-the-child")

	fixture := buildMCPFixture(t)
	home := t.TempDir()
	writeLiveMCPConfig(t, home, "fixture", mcpLiveServerSpec{Command: fixture})

	client := &mcpEchoScript{toolName: "mcp__fixture__echo", argsJSON: `{"text":"env-check"}`}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// AccessTrusted: this scenario is about the child's OWN environment, not
	// about permission gating (already covered by assertion 2); trusted
	// access lets tool.invoke's gate resolve without a human/approval step
	// so the call executes in one turn. tool.invoke is unconditionally
	// Gated regardless of profile (access.go's productAccessSource.AccessFor),
	// so interactive construction is still required to auto-resolve it.
	agent := openLiveMCPAgent(t, ctx, client, Config{HomeDir: home, AccessProfile: AccessTrusted}, true)

	waitMCPBindingReady(t, agent, "fixture", 15*time.Second)
	if err := agent.adopter.Install(ctx, agent.sess.ActiveLoop().ID(), string(activePrimerName)); err != nil {
		t.Fatalf("adopter.Install() error = %v", err)
	}

	sub, err := agent.Subscribe(event.EventFilter{Ephemeral: event.LoopScope{All: true}, Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	defer func() { _ = sub.Close() }()

	if _, err := agent.Submit(ctx, []content.Block{&content.TextBlock{Text: "call the echo tool"}}); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	gateID, _ := permissionGateWait(t, ctx, sub, 10*time.Second)
	if err := respondApprove(t, ctx, agent, gateID); err != nil {
		t.Fatalf("RespondGate() error = %v", err)
	}
	mustTurnDone(t, drainToTurnTerminal(t, ctx, sub))

	client.mu.Lock()
	observed := client.observedToolText
	client.mu.Unlock()

	if !strings.Contains(observed, "PATH=") || strings.Contains(observed, "PATH=\n") {
		t.Errorf("observed tool result = %q, want a non-empty inherited PATH", observed)
	}
	if !strings.Contains(observed, "UNLISTED=\n") && !strings.HasSuffix(strings.TrimRight(observed, "\n"), "UNLISTED=") {
		t.Errorf("observed tool result = %q, want UNLISTED= empty (the unlisted var must not reach the child)", observed)
	}
	if strings.Contains(observed, "should-not-reach-the-child") {
		t.Fatalf("observed tool result = %q, the child observed the unlisted parent env var -- env allowlist regression", observed)
	}
}

// ================================================================
// Assertion 6: close-exactly-once (double-close of RuntimeAgent).
// ================================================================

// TestMCPLiveStdioRuntimeAgentCloseIsSafeTwice targets Task 10's own
// flagged gap directly: RuntimeAgent.Close's doc states "mgr.Close is
// independently idempotent, but RuntimeAgent.Close itself is not guarded
// against being called twice", and separately, Adopter.Close's idempotency
// was "structurally safe but not documented as idempotent". This calls
// Close on a REAL RuntimeAgent with a REAL, live-connected MCP session
// twice and asserts the second call does not panic -- the regression this
// assertion exists to catch.
func TestMCPLiveStdioRuntimeAgentCloseIsSafeTwice(t *testing.T) {
	fixture := buildMCPFixture(t)
	home := t.TempDir()
	writeLiveMCPConfig(t, home, "fixture", mcpLiveServerSpec{Command: fixture})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	stores := mustOpenStores(t)
	agent, err := openRuntimeAgent(ctx, &fakeLLM{}, newModelFactoryFor(testModel()), Config{HomeDir: home}, stores, t.TempDir(), SessionSelector{}, false)
	if err != nil {
		t.Fatalf("openRuntimeAgent() error = %v", err)
	}

	waitMCPBindingReady(t, agent, "fixture", 15*time.Second)

	if err := agent.Close(ctx); err != nil {
		t.Fatalf("first Close() error = %v, want a clean close of a healthy session", err)
	}

	var secondErr error
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("second Close() panicked: %v (double-close bug)", r)
			}
		}()
		secondErr = agent.Close(ctx)
	}()
	t.Logf("second Close() error = %v", secondErr)

	// The Manager itself is independently documented idempotent
	// (mcpharness.Manager.Close's own doc: "It is idempotent: a second
	// Close returns nil without touching anything") -- confirm that
	// property held through RuntimeAgent's own double-close, not just that
	// nothing panicked.
	if agent.mgr != nil {
		if err := agent.mgr.Close(ctx); err != nil {
			t.Errorf("mgr.Close() after RuntimeAgent double-close error = %v, want nil (Manager.Close is documented idempotent)", err)
		}
	}
}
