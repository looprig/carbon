package app

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/session"
	"github.com/looprig/harness/pkg/workspacestore"
	model "github.com/looprig/inference/model"
	mcpharness "github.com/looprig/mcp/pkg/harness"
)

// mcp_integration_test.go proves Task 10's wiring: openRuntimeAgent's
// session-open path optionally discovers <home>/mcp.json, constructs an
// mcpharness.Manager, and attaches it to the live session -- entirely
// wiring-shaped, over a fake session/controller where a full real session
// isn't needed, and a real (if trivial) /bin/sh stdio binding where
// mcpDefinitions/mcpharness.NewManager need one to construct against. The
// live, real-tool-call round trip is a later task's //go:build integration
// suite, not this file's.

// mcpFixtureCommand is the real-but-trivial stdio command every test in this
// file uses to build an actual, connectable (if MCP-silent) binding, exactly
// like mcpconfig_test.go/mcp_test.go's own fixtures.
const mcpFixtureCommand = "/bin/sh"

// writeMCPConfigFixture writes a minimal, valid mcp.json under home naming
// one stdio server, mode 0600 per the file's hygiene contract.
func writeMCPConfigFixture(t *testing.T, home, serverName string) {
	t.Helper()
	path := filepath.Join(home, "mcp.json")
	body := `{"mcpServers":{"` + serverName + `":{"command":"` + mcpFixtureCommand + `"}}}`
	writeModelConfigFixture(t, path, []byte(body), 0o600)
}

// writeBadMCPConfigFixture writes an mcp.json naming a command that cannot
// possibly resolve via exec.LookPath, so mcpDefinitions fails at transport
// construction -- before any Manager is ever built -- matching
// mcp_test.go's TestMCPDefinitionsStdioMissingCommandFailsClosed fixture.
func writeBadMCPConfigFixture(t *testing.T, home, serverName string) {
	t.Helper()
	path := filepath.Join(home, "mcp.json")
	body := `{"mcpServers":{"` + serverName + `":{"command":"definitely-not-a-command-xyzzy"}}}`
	writeModelConfigFixture(t, path, []byte(body), 0o600)
}

func mustOpenStores(t *testing.T) *sessionStores {
	t.Helper()
	stores, err := openTestStores(t)
	if err != nil {
		t.Fatalf("openTestStores() error = %v", err)
	}
	return stores
}

// TestOpenRuntimeAgentNoMCPConfigLeavesManagerNil is the primary regression
// gate: with no mcp.json at all, openRuntimeAgent's assembled RuntimeAgent
// carries nil mgr/adopter fields -- zero change from the pre-Task-10 shape.
func TestOpenRuntimeAgentNoMCPConfigLeavesManagerNil(t *testing.T) {
	ctx := context.Background()
	stores := mustOpenStores(t)
	cfg := Config{HomeDir: t.TempDir()} // no mcp.json written here

	agent, err := newSessionOverStores(ctx, &fakeLLM{}, newModelFactoryFor(testModel()), cfg, stores, t.TempDir())
	if err != nil {
		t.Fatalf("newSessionOverStores() error = %v", err)
	}
	t.Cleanup(func() { _ = agent.Close(ctx) })

	if agent.mgr != nil {
		t.Errorf("agent.mgr = %v, want nil (no mcp.json)", agent.mgr)
	}
	if agent.adopter != nil {
		t.Errorf("agent.adopter = %v, want nil (no mcp.json)", agent.adopter)
	}
}

// TestOpenRuntimeAgentValidMCPConfigWiresManagerAndAdopter is the
// success-path proof: a valid mcp.json produces a RuntimeAgent with non-nil
// mgr/adopter, and openRuntimeAgent returning success at all is itself proof
// BindSession/Start/StartAdoption ran -- StartAdoption refuses an unbound
// Manager (ErrNotBound) and would have failed the whole open otherwise.
func TestOpenRuntimeAgentValidMCPConfigWiresManagerAndAdopter(t *testing.T) {
	ctx := context.Background()
	stores := mustOpenStores(t)
	home := t.TempDir()
	writeMCPConfigFixture(t, home, "sh")
	cfg := Config{HomeDir: home}

	agent, err := newSessionOverStores(ctx, &fakeLLM{}, newModelFactoryFor(testModel()), cfg, stores, t.TempDir())
	if err != nil {
		t.Fatalf("newSessionOverStores() error = %v", err)
	}

	if agent.mgr == nil {
		t.Fatal("agent.mgr = nil, want a constructed Manager")
	}
	if agent.adopter == nil {
		t.Fatal("agent.adopter = nil, want a constructed Adopter")
	}
	if digest := agent.mgr.ConfigDigest(); digest == "" {
		t.Error("agent.mgr.ConfigDigest() is empty, want a non-empty digest for a Manager with a binding")
	}
	statuses := agent.mgr.Status()
	if len(statuses) != 1 || statuses[0].Name != "sh" {
		t.Errorf("agent.mgr.Status() = %+v, want exactly the \"sh\" binding", statuses)
	}

	if err := agent.Close(ctx); err != nil {
		t.Fatalf("agent.Close() error = %v, want the full adapter/adopter/manager/access chain to close cleanly", err)
	}
}

// TestRuntimeAgentMCPNoticesSurfacesReportedNotices proves the fix for the
// Reporter-is-permanently-unreachable finding: a notice reported through the
// Manager's own Reporter during a real MCP session (a genuine mcp.json, a
// real /bin/sh binding, assembled via the production newSessionOverStores
// path) is actually observable afterward via RuntimeAgent.MCPNotices() --
// not merely constructed and captured into a local variable nothing ever
// reads again. The eager Install call attach makes already reports a real
// NoticeAdopted through this exact recorder (proof the wiring is live, not
// just present), so this test starts from whatever that produced and proves
// one more notice -- reported directly through agent.recorder, the exact
// mcpharness.Reporter instance the constructed Manager holds as
// Deps.Reporter, the same call the Manager itself makes internally on
// collision/adoption-failure -- becomes visible too, without needing to
// engineer a real tool-name collision (which would require two live,
// protocol-speaking MCP servers to produce).
func TestRuntimeAgentMCPNoticesSurfacesReportedNotices(t *testing.T) {
	ctx := context.Background()
	stores := mustOpenStores(t)
	home := t.TempDir()
	writeMCPConfigFixture(t, home, "sh")
	cfg := Config{HomeDir: home}

	agent, err := newSessionOverStores(ctx, &fakeLLM{}, newModelFactoryFor(testModel()), cfg, stores, t.TempDir())
	if err != nil {
		t.Fatalf("newSessionOverStores() error = %v", err)
	}
	t.Cleanup(func() { _ = agent.Close(ctx) })

	if agent.recorder == nil {
		t.Fatal("agent.recorder = nil, want the constructed mcpNoticeRecorder threaded through from mcpSessionAssembly")
	}
	before := agent.MCPNotices()

	loopID := mcpTestLoopID(t)
	agent.recorder.Report(mcpharness.Notice{Kind: mcpharness.NoticeToolNameCollision, Binding: "sh", LoopID: loopID, Message: "duplicate tool name"})

	got := agent.MCPNotices()
	if len(got) != len(before)+1 {
		t.Fatalf("MCPNotices() len = %d, want %d (one more than before the report)", len(got), len(before)+1)
	}
	last := got[len(got)-1]
	if last.Kind != mcpharness.NoticeToolNameCollision || last.Binding != "sh" || last.LoopID != loopID || last.Message != "duplicate tool name" {
		t.Errorf("MCPNotices() last entry = %+v, want the just-reported notice preserved", last)
	}
}

// TestOpenRuntimeAgentInteractiveWiresMCPToo proves the interactive path
// (SessionStoreFactory.openWithClient's flag) gets the same MCP composition
// as headless -- openRuntimeAgent's interactive flag only changes whether the
// gate opener binds (proven separately, at the mcpSessionAssembly.attach
// level below), never whether the Manager itself is built and attached.
func TestOpenRuntimeAgentInteractiveWiresMCPToo(t *testing.T) {
	ctx := context.Background()
	stores := mustOpenStores(t)
	home := t.TempDir()
	writeMCPConfigFixture(t, home, "sh")
	cfg := Config{HomeDir: home}

	agent, err := openRuntimeAgent(ctx, &fakeLLM{}, newModelFactoryFor(testModel()), cfg, stores, t.TempDir(), SessionSelector{}, true)
	if err != nil {
		t.Fatalf("openRuntimeAgent(interactive) error = %v", err)
	}
	t.Cleanup(func() { _ = agent.Close(ctx) })

	if agent.mgr == nil {
		t.Fatal("agent.mgr = nil, want a constructed Manager on the interactive path too")
	}
	if agent.adopter == nil {
		t.Fatal("agent.adopter = nil, want a constructed Adopter on the interactive path too")
	}
}

// TestOpenRuntimeAgentBadMCPCommandFailsClosed is the construction-failure
// scenario the task names explicitly: a spec whose command cannot resolve
// fails inside mcpDefinitions, which happens BEFORE any Manager is ever
// constructed (newMCPSessionAssembly builds the whole binding list before
// calling mcpharness.NewManager), so openRuntimeAgent must surface the typed
// *MCPConfigError and return a nil agent, with the access wiring closed via
// the same partial-failure path every other pre-session error already uses.
func TestOpenRuntimeAgentBadMCPCommandFailsClosed(t *testing.T) {
	ctx := context.Background()
	stores := mustOpenStores(t)
	home := t.TempDir()
	writeBadMCPConfigFixture(t, home, "ghost")
	cfg := Config{HomeDir: home}

	agent, err := newSessionOverStores(ctx, &fakeLLM{}, newModelFactoryFor(testModel()), cfg, stores, t.TempDir())
	if agent != nil {
		t.Fatalf("agent = %T, want nil", agent)
	}
	var configErr *MCPConfigError
	if !errors.As(err, &configErr) {
		t.Fatalf("error = %T %v, want *MCPConfigError", err, err)
	}
	if configErr.Binding != "ghost" {
		t.Errorf("MCPConfigError.Binding = %q, want ghost", configErr.Binding)
	}
}

// TestOpenRuntimeAgentLateFailureClosesMCPManager exercises the interesting
// failure path through the real function: a Manager IS successfully
// constructed (a valid mcp.json), but a LATER step -- here, restoring a
// session id that was never created in this store -- fails, so
// openRuntimeAgent's fail() cleanup must run with a non-nil manager.
// openRuntimeAgent never leaks the manager to a failed caller, so this test
// proves the path is exercised cleanly (an error surfaces promptly, no
// hang); mcpSessionAssembly.close's actual closing behavior is independently
// proven against a real Manager by
// TestMCPSessionAssemblyCloseReallyClosesTheManager below.
func TestOpenRuntimeAgentLateFailureClosesMCPManager(t *testing.T) {
	ctx := context.Background()
	stores := mustOpenStores(t)
	home := t.TempDir()
	writeMCPConfigFixture(t, home, "sh")
	cfg := Config{HomeDir: home}

	neverCreated, err := uuid.New()
	if err != nil {
		t.Fatal(err)
	}

	agent, err := openRuntimeAgent(ctx, &fakeLLM{}, newModelFactoryFor(testModel()), cfg, stores, t.TempDir(), SessionSelector{Resume: neverCreated}, false)
	if agent != nil {
		t.Fatalf("agent = %T, want nil (restore of a session that was never created)", agent)
	}
	if err == nil {
		t.Fatal("error = nil, want the restore failure surfaced")
	}
}

// TestMCPSessionAssemblyZeroValueIsSafeNoOp proves the "no mcp.json" case at
// the assembly level directly: every method on a zero-manager assembly is a
// safe no-op, which is what lets openRuntimeAgent call configRev/close/attach
// unconditionally regardless of whether mcp.json existed.
func TestMCPSessionAssemblyZeroValueIsSafeNoOp(t *testing.T) {
	ctx := context.Background()
	assembly, err := newMCPSessionAssembly(Config{HomeDir: t.TempDir()})
	if err != nil {
		t.Fatalf("newMCPSessionAssembly() error = %v", err)
	}
	if assembly.manager != nil {
		t.Errorf("manager = %v, want nil (no mcp.json)", assembly.manager)
	}
	if got := assembly.configRev(); got != "" {
		t.Errorf("configRev() = %q, want empty", got)
	}

	assembly.close(ctx) // must not panic

	sess := &fakeMCPSessionController{}
	if err := assembly.attach(ctx, sess, true); err != nil {
		t.Errorf("attach() on zero assembly error = %v, want nil (no-op)", err)
	}
}

// TestMCPSessionAssemblyCloseReallyClosesTheManager is the "prove no leak,
// not just no panic" test the task asks for: after close(), the real,
// underlying Manager must genuinely refuse further use (ErrManagerClosed),
// not merely have been left alone.
func TestMCPSessionAssemblyCloseReallyClosesTheManager(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	writeMCPConfigFixture(t, home, "sh")

	assembly, err := newMCPSessionAssembly(Config{HomeDir: home})
	if err != nil {
		t.Fatalf("newMCPSessionAssembly() error = %v", err)
	}
	if assembly.manager == nil {
		t.Fatal("manager = nil, want a constructed Manager")
	}

	assembly.close(ctx)

	if err := assembly.manager.Start(context.Background()); !errors.Is(err, mcpharness.ErrManagerClosed) {
		t.Errorf("Start() after close() error = %v, want %v", err, mcpharness.ErrManagerClosed)
	}
}

// TestMCPSessionAssemblyAttachBindsGateHostOnlyWhenInteractive proves the
// headless/interactive split at the point it actually happens: attach binds
// the gate opener's host only when interactive is true. mcpGateOpener.host
// is directly inspectable here (same package), so this needs no round trip
// through OpenGate to observe.
func TestMCPSessionAssemblyAttachBindsGateHostOnlyWhenInteractive(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	writeMCPConfigFixture(t, home, "sh")

	sessionID := mcpTestLoopID(t) // any fresh, non-zero uuid works as a session id here
	loopID := mcpTestLoopID(t)

	tests := []struct {
		name        string
		interactive bool
	}{
		{"interactive binds the gate host", true},
		{"headless never binds the gate host", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assembly, err := newMCPSessionAssembly(Config{HomeDir: home})
			if err != nil {
				t.Fatalf("newMCPSessionAssembly() error = %v", err)
			}
			t.Cleanup(func() { assembly.close(ctx) })

			sess := &fakeMCPSessionController{sessionID: sessionID, activeLoopID: loopID}
			if err := assembly.attach(ctx, sess, tt.interactive); err != nil {
				t.Fatalf("attach() error = %v", err)
			}

			bound := assembly.opener.host != nil
			if bound != tt.interactive {
				t.Errorf("gate opener bound = %v, want %v", bound, tt.interactive)
			}
		})
	}
}

// TestMCPSessionAssemblyAttachAlwaysBindsEventPublisher proves the other
// half of the split: publishing an integration status is not a human-input
// capability, so the event publisher binds (and genuinely forwards) in BOTH
// the interactive and headless cases.
func TestMCPSessionAssemblyAttachAlwaysBindsEventPublisher(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	writeMCPConfigFixture(t, home, "sh")

	sessionID := mcpTestLoopID(t)
	loopID := mcpTestLoopID(t)

	tests := []struct {
		name        string
		interactive bool
	}{
		{"interactive", true},
		{"headless", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assembly, err := newMCPSessionAssembly(Config{HomeDir: home})
			if err != nil {
				t.Fatalf("newMCPSessionAssembly() error = %v", err)
			}
			t.Cleanup(func() { assembly.close(ctx) })

			sess := &fakeMCPSessionController{sessionID: sessionID, activeLoopID: loopID}
			if err := assembly.attach(ctx, sess, tt.interactive); err != nil {
				t.Fatalf("attach() error = %v", err)
			}

			if assembly.events.target == nil {
				t.Fatal("event publisher target = nil, want bound regardless of interactive")
			}
			ev := event.SessionActive{}
			if err := assembly.events.PublishEvent(ctx, ev); err != nil {
				t.Fatalf("PublishEvent() error = %v", err)
			}
			calls := sess.publishedEvents()
			if len(calls) != 1 {
				t.Fatalf("session PublishEvent calls = %d, want 1", len(calls))
			}
		})
	}
}

// fakeLoopHandle is a minimal loop.Handle for mcpSessionAssembly.attach's
// sess.ActiveLoop() call.
type fakeLoopHandle struct{ id uuid.UUID }

func (h fakeLoopHandle) ID() uuid.UUID       { return h.id }
func (h fakeLoopHandle) Mode() loop.ModeName { return "" }
func (h fakeLoopHandle) Model() model.Model  { return model.Model{} }

// fakeMCPEventSubscription is a minimal event.Subscription: never delivers
// anything and closes cleanly. StartAdoption only needs a working
// subscription to exist; this file's tests never drive a LoopIdle event
// through it (Install is called eagerly and directly, not via a boundary).
type fakeMCPEventSubscription struct {
	events chan event.Delivery
}

func newFakeMCPEventSubscription() *fakeMCPEventSubscription {
	return &fakeMCPEventSubscription{events: make(chan event.Delivery)}
}
func (s *fakeMCPEventSubscription) Events() <-chan event.Delivery { return s.events }
func (s *fakeMCPEventSubscription) Close() error                  { close(s.events); return nil }
func (s *fakeMCPEventSubscription) Err() error                    { return nil }

// errFakeMCPSessionControllerUnimplemented is returned by every
// fakeMCPSessionController method mcpSessionAssembly.attach never calls. It
// exists so an accidental future call fails loudly and specifically instead
// of a zero value being mistaken for success.
var errFakeMCPSessionControllerUnimplemented = errors.New("fakeMCPSessionController: method not implemented for this test")

// fakeMCPSessionController is a minimal, in-memory session.SessionController
// double covering exactly what mcpSessionAssembly.attach and
// mcpharness.Manager.StartAdoption need: a SessionID, an ActiveLoop, a
// SubscribeEvents that hands back a live (if silent) subscription, and a
// LoopController returning (nil, false) -- which is not a shortfall to work
// around: Adopter.install's own installerFor treats "no installer for this
// Loop" as an ordinary, non-error outcome (mcp/pkg/harness/adoption.go), so
// the eager Install call attach makes is a clean no-op against this fake.
//
// It also implements session.GateHost and mcpharness.EventPublisher, exactly
// like a real session does, so attach's own soft type-assertions
// (`sess.(session.GateHost)`, `sess.(mcpharness.EventPublisher)`) succeed the
// same way they would against the real thing; every other method a real
// caller might reach but attach never does is a stub that fails loudly.
type fakeMCPSessionController struct {
	sessionID    uuid.UUID
	activeLoopID uuid.UUID

	mu     sync.Mutex
	events []event.Event
}

func (c *fakeMCPSessionController) SessionID() uuid.UUID { return c.sessionID }
func (c *fakeMCPSessionController) ActiveLoop() loop.Handle {
	return fakeLoopHandle{id: c.activeLoopID}
}
func (c *fakeMCPSessionController) Loop(uuid.UUID) (loop.Handle, bool) {
	return fakeLoopHandle{id: c.activeLoopID}, true
}
func (c *fakeMCPSessionController) Submit(context.Context, []content.Block) (uuid.UUID, error) {
	return uuid.UUID{}, errFakeMCPSessionControllerUnimplemented
}
func (c *fakeMCPSessionController) SubmitToLoop(context.Context, uuid.UUID, []content.Block) (uuid.UUID, error) {
	return uuid.UUID{}, errFakeMCPSessionControllerUnimplemented
}
func (c *fakeMCPSessionController) Compact(context.Context) (uuid.UUID, error) {
	return uuid.UUID{}, errFakeMCPSessionControllerUnimplemented
}
func (c *fakeMCPSessionController) CompactToLoop(context.Context, uuid.UUID) (uuid.UUID, error) {
	return uuid.UUID{}, errFakeMCPSessionControllerUnimplemented
}
func (c *fakeMCPSessionController) SubscribeEvents(event.EventFilter) (event.Subscription, error) {
	return newFakeMCPEventSubscription(), nil
}
func (c *fakeMCPSessionController) RespondGate(context.Context, gate.GateResponse) error {
	return errFakeMCPSessionControllerUnimplemented
}
func (c *fakeMCPSessionController) Interrupt(context.Context) (bool, error) {
	return false, errFakeMCPSessionControllerUnimplemented
}
func (c *fakeMCPSessionController) SetActiveLoop(context.Context, uuid.UUID) error {
	return errFakeMCPSessionControllerUnimplemented
}
func (c *fakeMCPSessionController) LoopController(uuid.UUID) (loop.Controller, bool) {
	return nil, false
}
func (c *fakeMCPSessionController) CheckpointWorkspace(context.Context) (workspacestore.Ref, error) {
	return "", errFakeMCPSessionControllerUnimplemented
}
func (c *fakeMCPSessionController) RestoreWorkspace(context.Context, workspacestore.Ref) error {
	return errFakeMCPSessionControllerUnimplemented
}
func (c *fakeMCPSessionController) Shutdown(context.Context) error { return nil }

// OpenHostGate, AwaitGateAnswer, and CloseGate satisfy session.GateHost.
// mcpSessionAssembly.attach only ever calls Bind with this value -- Bind
// stores the reference, it never calls a method on it -- so these are never
// exercised by this file's tests; they fail loudly if that ever changes.
func (c *fakeMCPSessionController) OpenHostGate(context.Context, uuid.UUID, gate.Gate, gate.Payload) (gate.ID, error) {
	return gate.ID{}, errFakeMCPSessionControllerUnimplemented
}
func (c *fakeMCPSessionController) AwaitGateAnswer(context.Context, gate.ID) (gate.Answer, error) {
	return gate.Answer{}, errFakeMCPSessionControllerUnimplemented
}
func (c *fakeMCPSessionController) CloseGate(context.Context, gate.ID, gate.CloseReason) error {
	return errFakeMCPSessionControllerUnimplemented
}

// PublishEvent satisfies mcpharness.EventPublisher and records what it was
// handed, so a test can prove mcpEventPublisher's forwarding actually
// reaches the bound target.
func (c *fakeMCPSessionController) PublishEvent(_ context.Context, ev event.Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, ev)
	return nil
}

func (c *fakeMCPSessionController) publishedEvents() []event.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]event.Event(nil), c.events...)
}

var (
	_ session.SessionController = (*fakeMCPSessionController)(nil)
	_ session.GateHost          = (*fakeMCPSessionController)(nil)
	_ mcpharness.EventPublisher = (*fakeMCPSessionController)(nil)
)
