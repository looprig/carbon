package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/looprig/inference"
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
