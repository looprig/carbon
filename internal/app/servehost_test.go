package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/session"
	"github.com/looprig/inference"
	mcpharness "github.com/looprig/mcp/pkg/harness"
)

// newTestServeHost opens a ServeHost over a fresh durable store and a fresh
// workspace, with the inference client injected so no models.json or network is
// needed. It chdirs into the workspace because OpenServeHost resolves the workspace
// root from the process working directory, exactly as the TUI path does.
func newTestServeHost(t *testing.T) *ServeHost {
	t.Helper()
	t.Chdir(t.TempDir())
	host, err := OpenServeHost(context.Background(), Config{HomeDir: t.TempDir()}, t.TempDir(),
		WithServeInferenceClient(func() (inference.Client, ModelFactory, error) {
			return &fakeLLM{}, newModelFactoryFor(testModel()), nil
		}))
	if err != nil {
		t.Fatalf("OpenServeHost: %v", err)
	}
	t.Cleanup(func() { _ = host.Close(context.Background()) })
	return host
}

// TestOpenServeHostBuildsOneProcessLifetimeRig proves the serve composition seam
// exists and hands out ONE rig for the whole process rather than one per
// session-open. SessionStoreFactory.Open builds a fresh sessionAccess, loop
// definition and rig on EVERY open because the TUI hosts one session at a time;
// serve.Handler cannot work that way — it holds a single Rig for the life of the
// server and calls NewSession/RestoreSession on it. Everything that is a pure
// function of (Config, workspace root) therefore has to be built exactly once, here.
func TestOpenServeHostBuildsOneProcessLifetimeRig(t *testing.T) {
	host := newTestServeHost(t)

	if host.rig == nil {
		t.Fatal("ServeHost.rig is nil")
	}
	if host.Catalog() == nil {
		t.Fatal("ServeHost.Catalog() is nil; the read plane lists sessions from it")
	}
	if host.SessionStore() == nil {
		t.Fatal("ServeHost.SessionStore() is nil; the read plane replays journal pages from it")
	}
	if host.access == nil {
		t.Fatal("ServeHost.access is nil; the shared rig closes over the access wiring")
	}
	if host.serveConfig.AccessConfigRev == "" {
		t.Error("serveConfig.AccessConfigRev is empty; the access digest was not folded into the host config")
	}
	if host.serveConfig.AccessConfigRev != host.access.configRev {
		t.Errorf("serveConfig.AccessConfigRev = %q, want the access wiring's %q", host.serveConfig.AccessConfigRev, host.access.configRev)
	}
}

// TestOpenServeHostUsesTheDurableStoreAtDataDir pins the backend choice. The headless
// memstore path exists for tests and for `carbon` with no persistence; carbon serve
// serves the user's real, persisted sessions, so a host that quietly composed over
// memstore would present an empty session list on every restart.
func TestOpenServeHostUsesTheDurableStoreAtDataDir(t *testing.T) {
	t.Chdir(t.TempDir())
	dataDir := t.TempDir()
	host, err := OpenServeHost(context.Background(), Config{HomeDir: t.TempDir()}, dataDir,
		WithServeInferenceClient(func() (inference.Client, ModelFactory, error) {
			return &fakeLLM{}, newModelFactoryFor(testModel()), nil
		}))
	if err != nil {
		t.Fatalf("OpenServeHost: %v", err)
	}
	t.Cleanup(func() { _ = host.Close(context.Background()) })

	entries, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("dataDir is empty after OpenServeHost; the durable fsstore backend was never opened there")
	}
	if host.fs == nil {
		t.Fatal("ServeHost.fs is nil; nothing owns the durable backend's Close")
	}
	if host.stores.resourceStorage == nil {
		t.Error("stores.resourceStorage is nil; process-service tools would have no persisted resource root")
	}
}

// TestOpenServeHostFailsWithoutLeakingTheAccessWiring proves the construction is
// transactional. buildSessionAccess allocates a scratch HOME and a sandbox executor
// set; a failure after it must close them, or every failed `carbon serve` start
// leaves a scratch directory and a live grant behind.
func TestOpenServeHostFailsWithoutLeakingTheAccessWiring(t *testing.T) {
	t.Chdir(t.TempDir())
	home := t.TempDir()
	// A data directory that cannot become a directory makes fsstore.Open fail, which
	// is the first failure point AFTER the access wiring has been built.
	blocked := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The executor set roots its owned scratch HOME at os.TempDir(); pointing that
	// at an empty directory of our own makes the leak directly observable.
	scratchRoot := t.TempDir()
	t.Setenv("TMPDIR", scratchRoot)
	host, err := OpenServeHost(context.Background(), Config{HomeDir: home}, blocked,
		WithServeInferenceClient(func() (inference.Client, ModelFactory, error) {
			return &fakeLLM{}, newModelFactoryFor(testModel()), nil
		}))
	if err == nil {
		_ = host.Close(context.Background())
		t.Fatal("OpenServeHost over a non-directory data dir succeeded, want a store init failure")
	}
	var initErr *StoreInitError
	if !errors.As(err, &initErr) {
		t.Fatalf("err = %T %v, want *StoreInitError", err, err)
	}
	leftovers, err := os.ReadDir(scratchRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 0 {
		names := make([]string, 0, len(leftovers))
		for _, entry := range leftovers {
			names = append(names, entry.Name())
		}
		t.Errorf("scratch root still holds %v after a failed OpenServeHost; the access wiring was leaked", names)
	}
}

// TestServeHostCloseIsIdempotent pins the teardown contract: Close's body runs
// exactly once, so a shutdown path that both defers Close and calls it on a signal
// does not tear anything down twice.
//
// The credential admission is where "twice" actually hurts, and why the once guard is
// not decoration. credentialRuntime.endSession decrements a PROCESS-WIDE counter that
// is shared by every borrower of the same credential home (credentials.go's
// process registry), and it is not itself idempotent. A second Close would therefore
// decrement some OTHER borrower's admission, and an in-process logout would see the
// drain gate open while a live session still holds credentials. This test gives the
// runtime two admissions - the host's and a simulated second borrower's - and
// requires the second borrower's to survive two Closes.
func TestServeHostCloseIsIdempotent(t *testing.T) {
	t.Chdir(t.TempDir())
	runtime := &credentialRuntime{}
	// A lease with more than one borrow: Release decrements it without closing the
	// shared runtime, which is exactly the production shape when a second
	// composition in this process holds the same credential home.
	lease := &credentialRegistryLease{
		registry: &credentialRegistry{entries: make(map[string]*credentialRegistryEntry)},
		key:      "shared-home",
		entry:    &credentialRegistryEntry{runtime: runtime, borrows: 2},
	}
	loader := func(context.Context, string) (productionModels, error) {
		configured, err := serveModelsFixtureLoader(runtime)(context.Background(), "")
		if err != nil {
			return productionModels{}, err
		}
		configured.credentialLease = lease
		return configured, nil
	}

	host, err := OpenServeHost(context.Background(), Config{HomeDir: t.TempDir()}, t.TempDir(), withServeModelsLoader(loader))
	if err != nil {
		t.Fatalf("OpenServeHost: %v", err)
	}
	// The second borrower's admission, taken after the host's.
	if err := runtime.beginSession(); err != nil {
		t.Fatalf("beginSession for the second borrower: %v", err)
	}
	if got := runtime.activeSessions(); got != 2 {
		t.Fatalf("activeSessions = %d, want 2 (the host's admission plus the second borrower's)", got)
	}

	ctx := context.Background()
	if err := host.Close(ctx); err != nil {
		t.Fatalf("Close #1: %v", err)
	}
	if err := host.Close(ctx); err != nil {
		t.Fatalf("Close #2: %v", err)
	}
	if got := runtime.activeSessions(); got != 1 {
		t.Errorf("activeSessions after two Closes = %d, want 1 (Close released the host's admission exactly once)", got)
	}
	if lease.entry.borrows != 1 {
		t.Errorf("lease borrows = %d, want 1 (Close released the lease exactly once)", lease.entry.borrows)
	}
	runtime.endSession()
}

// TestOpenServeHostEndsTheCredentialAdmissionOnClose proves the process-long
// credential admission resolveServeModels takes is released exactly once, at host
// teardown. A host that never ends it pins the drain gate an in-process logout waits
// on for the life of the process even after the server has stopped.
func TestOpenServeHostEndsTheCredentialAdmissionOnClose(t *testing.T) {
	t.Chdir(t.TempDir())
	runtime := &credentialRuntime{}
	host, err := OpenServeHost(context.Background(), Config{HomeDir: t.TempDir()}, t.TempDir(),
		withServeModelsLoader(serveModelsFixtureLoader(runtime)))
	if err != nil {
		t.Fatalf("OpenServeHost: %v", err)
	}
	if got := runtime.activeSessions(); got != 1 {
		t.Fatalf("activeSessions after OpenServeHost = %d, want 1", got)
	}
	if err := host.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := runtime.activeSessions(); got != 0 {
		t.Errorf("activeSessions after Close = %d, want 0", got)
	}
	if closed, _ := runtime.lifecycleState(); !closed {
		t.Error("the credential runtime was not closed by ServeHost.Close")
	}
}

// newTestServeHostWithMCP is newTestServeHost plus a real (if MCP-silent)
// <home>/mcp.json, so the per-session MCP composition is actually exercised rather
// than short-circuited by the zero-value no-MCP path.
func newTestServeHostWithMCP(t *testing.T) *ServeHost {
	t.Helper()
	t.Chdir(t.TempDir())
	home := t.TempDir()
	writeMCPConfigFixture(t, home, "sh")
	host, err := OpenServeHost(context.Background(), Config{HomeDir: home}, t.TempDir(),
		WithServeInferenceClient(func() (inference.Client, ModelFactory, error) {
			return &fakeLLM{}, newModelFactoryFor(testModel()), nil
		}))
	if err != nil {
		t.Fatalf("OpenServeHost: %v", err)
	}
	t.Cleanup(func() { _ = host.Close(context.Background()) })
	return host
}

// TestServeHostNewSessionAttachesItsOwnMCPManager proves the MCP composition is PER
// SESSION, not per rig. mcpharness.Manager.BindSession is a compare-and-swap that
// permanently binds one Manager to one session id, so a shared rig serving N sessions
// needs N Managers. The negative half of the proof is the direct one: the Manager the
// first session bound REFUSES a second session id, which is exactly why reusing it
// would be a bug rather than an optimisation.
func TestServeHostNewSessionAttachesItsOwnMCPManager(t *testing.T) {
	host := newTestServeHostWithMCP(t)
	ctx := context.Background()

	first, err := host.NewSession(ctx)
	if err != nil {
		t.Fatalf("NewSession #1: %v", err)
	}
	firstID := first.SessionID()
	firstManager := host.live.mcp.manager
	if firstManager == nil {
		t.Fatal("the first session has no MCP Manager despite a valid mcp.json")
	}

	// The direct proof of why the Manager cannot be hoisted to the host: the one the
	// first session bound refuses any other session id.
	other, err := uuid.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := firstManager.BindSession(other); !errors.Is(err, mcpharness.ErrAlreadyBound) {
		t.Fatalf("rebinding the first Manager = %v, want mcpharness.ErrAlreadyBound", err)
	}

	if err := host.CloseLive(ctx, firstID); err != nil {
		t.Fatalf("CloseLive(%v): %v", firstID, err)
	}

	second, err := host.NewSession(ctx)
	if err != nil {
		t.Fatalf("NewSession #2: %v", err)
	}
	if second.SessionID() == firstID {
		t.Fatal("the second NewSession returned the first session id")
	}
	secondManager := host.live.mcp.manager
	if secondManager == nil {
		t.Fatal("the second session has no MCP Manager")
	}
	if secondManager == firstManager {
		t.Fatal("both sessions share one MCP Manager; BindSession would have refused the second")
	}
	if host.live == nil || host.live.id != second.SessionID() {
		t.Fatalf("live session = %+v, want %v", host.live, second.SessionID())
	}
}

// TestServeHostNewSessionRefusesAHandoffWhileASessionIsLive is the core of the
// confirmed-handoff contract. Carbon composes the rig with
// rig.WithExclusiveWorkspace, whose root lease conflicts even inside one process, so
// one live session per workspace root is a real constraint. The rejected design was
// to satisfy it by silently closing the incumbent: a click in a browser would then
// kill a session that may be mid-turn, with no confirmation and no way to decline.
// Instead the second open REFUSES, naming the session that must be handed off, and
// the incumbent is untouched.
func TestServeHostNewSessionRefusesAHandoffWhileASessionIsLive(t *testing.T) {
	host := newTestServeHost(t)
	ctx := context.Background()

	first, err := host.NewSession(ctx)
	if err != nil {
		t.Fatalf("NewSession #1: %v", err)
	}
	firstID := first.SessionID()

	second, err := host.NewSession(ctx)
	if err == nil {
		t.Fatalf("NewSession #2 = %v, want a handoff refusal", second.SessionID())
	}
	var handoff *LiveSessionHandoffError
	if !errors.As(err, &handoff) {
		t.Fatalf("NewSession #2 err = %T %v, want *LiveSessionHandoffError", err, err)
	}
	if handoff.LiveID != firstID {
		t.Errorf("handoff.LiveID = %v, want the incumbent %v", handoff.LiveID, firstID)
	}
	if host.live == nil || host.live.id != firstID {
		t.Fatalf("live session = %+v, want the untouched incumbent %v", host.live, firstID)
	}
	assertSessionAlive(t, first, "the incumbent was shut down by a refused handoff")
}

// TestServeHostCloseLiveConfirmsWhichSessionItCloses proves the handoff is CONFIRMED
// rather than blind. A browser shows "session A is running; switching will close it",
// and by the time the human answers, another tab may already have handed off to B.
// Closing "whatever is live" would then kill B on A's behalf, so CloseLive names the
// session it expects to close and refuses if that is no longer the live one.
func TestServeHostCloseLiveConfirmsWhichSessionItCloses(t *testing.T) {
	host := newTestServeHost(t)
	ctx := context.Background()

	live, err := host.NewSession(ctx)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	stale, err := uuid.New()
	if err != nil {
		t.Fatal(err)
	}

	err = host.CloseLive(ctx, stale)
	var handoff *LiveSessionHandoffError
	if !errors.As(err, &handoff) {
		t.Fatalf("CloseLive(stale) err = %T %v, want *LiveSessionHandoffError", err, err)
	}
	if handoff.LiveID != live.SessionID() {
		t.Errorf("handoff.LiveID = %v, want the actual live session %v", handoff.LiveID, live.SessionID())
	}
	assertSessionAlive(t, live, "CloseLive closed a session the caller did not name")

	if err := host.CloseLive(ctx, live.SessionID()); err != nil {
		t.Fatalf("CloseLive(live): %v", err)
	}
	if host.live != nil {
		t.Fatalf("live session = %+v after CloseLive, want none", host.live)
	}
	if _, ok := host.LiveSession(); ok {
		t.Error("LiveSession() still reports a live session after CloseLive")
	}
	// The root lease is released with the session, so the next open now succeeds.
	if _, err := host.NewSession(ctx); err != nil {
		t.Fatalf("NewSession after a confirmed handoff: %v", err)
	}
}

// TestServeHostCloseLiveOnAnIdleHostIsANoOp pins the race the pre-flight route will
// hit constantly: two tabs both confirm a handoff, the first wins, and the second
// must not be turned into an error page for having asked to close something already
// gone.
func TestServeHostCloseLiveOnAnIdleHostIsANoOp(t *testing.T) {
	host := newTestServeHost(t)
	absent, err := uuid.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := host.CloseLive(context.Background(), absent); err != nil {
		t.Fatalf("CloseLive on an idle host = %v, want nil", err)
	}
}

// TestServeHostLiveSessionReportsTheHandoffTarget covers the read half of the
// pre-flight seam: GET /ui/live answers from this, so a UI can show which session is
// running BEFORE it asks the human to confirm closing it.
func TestServeHostLiveSessionReportsTheHandoffTarget(t *testing.T) {
	host := newTestServeHost(t)
	ctx := context.Background()

	if id, ok := host.LiveSession(); ok {
		t.Fatalf("LiveSession() = (%v, true) on a fresh host, want no live session", id)
	}
	live, err := host.NewSession(ctx)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	id, ok := host.LiveSession()
	if !ok {
		t.Fatal("LiveSession() reports no live session while one is open")
	}
	if id != live.SessionID() {
		t.Errorf("LiveSession() = %v, want %v", id, live.SessionID())
	}
}

// TestServeHostSessionsReportTheirOwnDeath guards the eviction contract harness's
// live registry depends on. serve registers a session and watches its optional
// Done() channel so a dead session is evicted instead of pinning a subscription and a
// goroutine forever; absence of Done is treated as "live forever". A ServeHost that
// returned a WRAPPER around the real session would silently opt every session out of
// eviction unless the wrapper forwarded Done, so the sessions handed out here must
// keep reporting their own death.
func TestServeHostSessionsReportTheirOwnDeath(t *testing.T) {
	host := newTestServeHost(t)
	ctx := context.Background()

	live, err := host.NewSession(ctx)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	reporter, ok := live.(interface{ Done() <-chan struct{} })
	if !ok {
		t.Fatalf("the session ServeHost handed out (%T) does not expose Done(); harness's live registry can never evict it", live)
	}
	select {
	case <-reporter.Done():
		t.Fatal("Done() is already closed for a session that has not been shut down")
	default:
	}
	if err := host.CloseLive(ctx, live.SessionID()); err != nil {
		t.Fatalf("CloseLive: %v", err)
	}
	select {
	case <-reporter.Done():
	default:
		t.Fatal("Done() did not close after the session was shut down; the registry entry would outlive the session")
	}
}

// assertSessionAlive fails when sess has begun shutting down.
func assertSessionAlive(t *testing.T, sess session.SessionController, message string) {
	t.Helper()
	reporter, ok := sess.(interface{ Done() <-chan struct{} })
	if !ok {
		t.Fatalf("session %T does not expose Done(); liveness is unobservable", sess)
	}
	select {
	case <-reporter.Done():
		t.Fatal(message)
	default:
	}
}

// TestServeHostFailedAdoptReleasesTheWorkspaceRoot covers the one failure path that
// happens AFTER the rig has already opened a session. The session holds the exclusive
// workspace root lease from the moment it exists, so an adopt failure that returned
// without shutting it down would strand the root: every later open would fail with an
// opaque workspace-busy error for the rest of the process's life, with no session the
// caller could hand off.
//
// The failure used here is the real one: <home>/mcp.json changing after the rig baked
// its configuration digest into the fingerprint.
func TestServeHostFailedAdoptReleasesTheWorkspaceRoot(t *testing.T) {
	t.Chdir(t.TempDir())
	home := t.TempDir()
	ctx := context.Background()
	host, err := OpenServeHost(ctx, Config{HomeDir: home}, t.TempDir(),
		WithServeInferenceClient(func() (inference.Client, ModelFactory, error) {
			return &fakeLLM{}, newModelFactoryFor(testModel()), nil
		}))
	if err != nil {
		t.Fatalf("OpenServeHost: %v", err)
	}
	t.Cleanup(func() { _ = host.Close(ctx) })

	// The rig was defined with no MCP configuration; the file appears afterwards.
	writeMCPConfigFixture(t, home, "sh")
	if _, err := host.NewSession(ctx); err == nil {
		t.Fatal("NewSession after mcp.json changed succeeded, want a drift refusal")
	} else {
		var drift *MCPConfigDriftError
		if !errors.As(err, &drift) {
			t.Fatalf("NewSession err = %T %v, want *MCPConfigDriftError", err, err)
		}
	}
	if host.live != nil {
		t.Fatalf("live session = %+v after a failed adopt, want none", host.live)
	}

	// Restoring the original configuration must make the host usable again, which is
	// only true if the failed adopt released the workspace root lease.
	if err := os.Remove(filepath.Join(home, "mcp.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := host.NewSession(ctx); err != nil {
		t.Fatalf("NewSession after a failed adopt: %v (the workspace root lease was stranded)", err)
	}
}

// TestServeHostHasSessionDistinguishesNeverPersisted is the input to the HTTP 404.
// harness maps every RestoreSession error except its own session-not-found class to a
// bare 500, and rig.RestoreSession has no not-found error class at all, so the
// existence check has to happen BEFORE the rig is touched. The listing catalog is the
// cheap authority for it: it reads the index only, with no lease and no replay.
func TestServeHostHasSessionDistinguishesNeverPersisted(t *testing.T) {
	host := newTestServeHost(t)
	ctx := context.Background()

	absent, err := uuid.New()
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := host.HasSession(ctx, absent); err != nil || ok {
		t.Fatalf("HasSession(absent) = (%v, %v), want (false, nil)", ok, err)
	}

	sess, err := host.NewSession(ctx)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if ok, err := host.HasSession(ctx, sess.SessionID()); err != nil || !ok {
		t.Fatalf("HasSession(live) = (%v, %v), want (true, nil)", ok, err)
	}
}

// TestServeHostHasSessionIgnoresAnotherWorkspacesSessions scopes the existence check
// to the root this host serves. The session store is global (it lives under the
// looprig home, not the workspace), so the catalog spans every checkout carbon has
// ever run in; a rig binds ONE root. Reporting a foreign session as present would turn
// a request the deployment can never serve into a workspace-root config mismatch deep
// inside the rig -- a 500 -- where "no such session here" is the honest answer.
func TestServeHostHasSessionIgnoresAnotherWorkspacesSessions(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	home := t.TempDir()
	build := WithServeInferenceClient(func() (inference.Client, ModelFactory, error) {
		return &fakeLLM{}, newModelFactoryFor(testModel()), nil
	})

	t.Chdir(t.TempDir()) // workspace A
	inA, err := OpenServeHost(ctx, Config{HomeDir: home}, dataDir, build)
	if err != nil {
		t.Fatalf("OpenServeHost in workspace A: %v", err)
	}
	sessA, err := inA.NewSession(ctx)
	if err != nil {
		t.Fatalf("NewSession in workspace A: %v", err)
	}
	idA := sessA.SessionID()
	if err := inA.Close(ctx); err != nil {
		t.Fatalf("Close in workspace A: %v", err)
	}

	t.Chdir(t.TempDir()) // workspace B, same global store
	inB, err := OpenServeHost(ctx, Config{HomeDir: home}, dataDir, build)
	if err != nil {
		t.Fatalf("OpenServeHost in workspace B: %v", err)
	}
	t.Cleanup(func() { _ = inB.Close(ctx) })

	if ok, err := inB.HasSession(ctx, idA); err != nil || ok {
		t.Fatalf("HasSession(workspace A's session) = (%v, %v), want (false, nil)", ok, err)
	}
}

// TestServeHostRestoreReturnsTheAlreadyLiveSession pins the attach case. The exclusive
// root lease makes a second restore of the same id impossible, and re-restoring would
// in any case orphan the subscribers already streaming from the live controller, so
// the live controller itself is returned.
func TestServeHostRestoreReturnsTheAlreadyLiveSession(t *testing.T) {
	host := newTestServeHost(t)
	ctx := context.Background()

	live, err := host.NewSession(ctx)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	attached, err := host.RestoreSession(ctx, live.SessionID())
	if err != nil {
		t.Fatalf("RestoreSession(live): %v", err)
	}
	if attached != live {
		t.Fatalf("RestoreSession(live) returned a different controller (%p) than the live one (%p)", attached, live)
	}
	assertSessionAlive(t, live, "restoring the live session shut it down")
}

// TestServeHostRestoreRefusesAHandoffWhileAnotherSessionIsLive is the restore half of
// the confirmed-handoff contract: clicking a different session in the list must not
// silently kill the one that is running.
func TestServeHostRestoreRefusesAHandoffWhileAnotherSessionIsLive(t *testing.T) {
	host := newTestServeHost(t)
	ctx := context.Background()

	first, err := host.NewSession(ctx)
	if err != nil {
		t.Fatalf("NewSession #1: %v", err)
	}
	firstID := first.SessionID()
	if err := host.CloseLive(ctx, firstID); err != nil {
		t.Fatalf("CloseLive: %v", err)
	}
	second, err := host.NewSession(ctx)
	if err != nil {
		t.Fatalf("NewSession #2: %v", err)
	}

	_, err = host.RestoreSession(ctx, firstID)
	var handoff *LiveSessionHandoffError
	if !errors.As(err, &handoff) {
		t.Fatalf("RestoreSession while another session is live = %T %v, want *LiveSessionHandoffError", err, err)
	}
	if handoff.LiveID != second.SessionID() {
		t.Errorf("handoff.LiveID = %v, want the incumbent %v", handoff.LiveID, second.SessionID())
	}
	assertSessionAlive(t, second, "a refused restore shut the incumbent down")

	// After the handoff is confirmed the restore succeeds.
	if err := host.CloseLive(ctx, second.SessionID()); err != nil {
		t.Fatalf("CloseLive(second): %v", err)
	}
	restored, err := host.RestoreSession(ctx, firstID)
	if err != nil {
		t.Fatalf("RestoreSession(%v) after a confirmed handoff: %v", firstID, err)
	}
	if restored.SessionID() != firstID {
		t.Fatalf("restored id = %v, want %v", restored.SessionID(), firstID)
	}
	if host.live == nil || host.live.mcp.manager != nil {
		// No mcp.json in this host, so the assembly is the zero value; the point of
		// the check is that the restored session was adopted at all.
		if host.live == nil {
			t.Fatal("a restored session was not recorded as live")
		}
	}
}

// TestServeHostRestoresASessionTheTUIPathCreated is the cross-composition proof that
// the process-lifetime rig is fingerprint-compatible with the one
// SessionStoreFactory builds per open. `carbon serve` exists to serve the user's
// EXISTING sessions, so a serve host that composed even slightly differently -- a
// missing MCP configuration digest, a different access digest, different delegation
// limits -- would reject every one of them as configuration drift.
func TestServeHostRestoresASessionTheTUIPathCreated(t *testing.T) {
	ctx := context.Background()
	t.Chdir(t.TempDir())
	dataDir := t.TempDir()
	home := t.TempDir()
	writeMCPConfigFixture(t, home, "sh")
	cfg := Config{HomeDir: home}

	factory, err := NewSessionStoreFactory(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	factory.buildClient = func() (inference.Client, ModelFactory, error) {
		return &fakeLLM{}, newModelFactoryFor(testModel()), nil
	}
	agent, err := factory.Open(ctx, SessionSelector{}, cfg)
	if err != nil {
		t.Fatalf("SessionStoreFactory.Open: %v", err)
	}
	id := agent.SessionID()
	if err := agent.Close(ctx); err != nil {
		t.Fatalf("agent.Close: %v", err)
	}
	if err := factory.Close(); err != nil {
		t.Fatalf("factory.Close: %v", err)
	}

	host, err := OpenServeHost(ctx, cfg, dataDir,
		WithServeInferenceClient(func() (inference.Client, ModelFactory, error) {
			return &fakeLLM{}, newModelFactoryFor(testModel()), nil
		}))
	if err != nil {
		t.Fatalf("OpenServeHost: %v", err)
	}
	t.Cleanup(func() { _ = host.Close(ctx) })

	if ok, err := host.HasSession(ctx, id); err != nil || !ok {
		t.Fatalf("HasSession(TUI session) = (%v, %v), want (true, nil)", ok, err)
	}
	restored, err := host.RestoreSession(ctx, id)
	if err != nil {
		t.Fatalf("RestoreSession(%v) of a session the TUI path created: %v", id, err)
	}
	if restored.SessionID() != id {
		t.Fatalf("restored id = %v, want %v", restored.SessionID(), id)
	}
}

// TestServeHostHasSessionPropagatesACatalogFailure pins the half of HasSession's
// contract that is easy to get backwards. A FAILED index read is not evidence that
// the session is absent, and reporting it as absent would answer 404 for a session
// that exists -- telling a user their work is gone because a disk read hiccuped. The
// boolean means "definitely not here"; anything else is an error.
func TestServeHostHasSessionPropagatesACatalogFailure(t *testing.T) {
	ctx := context.Background()
	t.Chdir(t.TempDir())
	dataDir := t.TempDir()
	host, err := OpenServeHost(ctx, Config{HomeDir: t.TempDir()}, dataDir,
		WithServeInferenceClient(func() (inference.Client, ModelFactory, error) {
			return &fakeLLM{}, newModelFactoryFor(testModel()), nil
		}))
	if err != nil {
		t.Fatalf("OpenServeHost: %v", err)
	}
	t.Cleanup(func() { _ = host.Close(ctx) })

	sess, err := host.NewSession(ctx)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	id := sess.SessionID()
	if err := host.CloseLive(ctx, id); err != nil {
		t.Fatalf("CloseLive: %v", err)
	}
	// Corrupt the session's listing-index entry so the index read itself fails.
	entry := filepath.Join(dataDir, "kv", "sessions", id.String())
	if _, err := os.Stat(entry); err != nil {
		t.Fatalf("listing index entry %s: %v", entry, err)
	}
	if err := os.WriteFile(entry, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	ok, err := host.HasSession(ctx, id)
	if err == nil {
		t.Fatalf("HasSession with a failing catalog read = (%v, nil), want the read error", ok)
	}
	if ok {
		t.Error("HasSession reported true on a failed read")
	}
}

// TestServeHostHasSessionTrustsTheLiveSessionOverTheIndex pins why the live check
// comes BEFORE the catalog read rather than after it. A session this host is
// currently serving is servable by definition -- a browser may be streaming its
// events right now -- so a missing or damaged listing-index entry must not be allowed
// to turn its restore into a 404 and disconnect it.
func TestServeHostHasSessionTrustsTheLiveSessionOverTheIndex(t *testing.T) {
	ctx := context.Background()
	t.Chdir(t.TempDir())
	dataDir := t.TempDir()
	host, err := OpenServeHost(ctx, Config{HomeDir: t.TempDir()}, dataDir,
		WithServeInferenceClient(func() (inference.Client, ModelFactory, error) {
			return &fakeLLM{}, newModelFactoryFor(testModel()), nil
		}))
	if err != nil {
		t.Fatalf("OpenServeHost: %v", err)
	}
	t.Cleanup(func() { _ = host.Close(ctx) })

	live, err := host.NewSession(ctx)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if err := os.Remove(filepath.Join(dataDir, "kv", "sessions", live.SessionID().String())); err != nil {
		t.Fatalf("removing the listing index entry: %v", err)
	}
	if ok, err := host.HasSession(ctx, live.SessionID()); err != nil || !ok {
		t.Fatalf("HasSession(live, index entry gone) = (%v, %v), want (true, nil)", ok, err)
	}
}
