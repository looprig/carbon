package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/session"
	"github.com/looprig/host/department"
	"github.com/looprig/inference"
)

func TestPooledLauncherKeepsTwoTenantSessionsLiveAndRestoresTheirOwnRoots(t *testing.T) {
	ctx := context.Background()
	data := t.TempDir()
	launcher, err := OpenPooledLauncher(ctx, Config{HomeDir: t.TempDir()}, data,
		WithServeInferenceClient(func() (inference.Client, ModelFactory, error) {
			return &fakeLLM{}, newModelFactoryFor(testModel()), nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = launcher.Close(ctx) })
	var sessions []session.SessionController
	for _, tenant := range []sessionwire.TenantID{"tenant-a", "tenant-b"} {
		id, err := uuid.New()
		if err != nil {
			t.Fatal(err)
		}
		sess, err := launcher.Launch(ctx, LaunchScope{TenantID: tenant, SessionID: "same-session-name", AgentID: CarbonAgentID, Placement: sessionwire.HostPlacementPooled, RigSessionID: id})
		if err != nil {
			t.Fatalf("launch %s: %v", tenant, err)
		}
		if sess.SessionID() != id {
			t.Fatalf("launch %s identity %s, want %s", tenant, sess.SessionID(), id)
		}
		sessions = append(sessions, sess)
	}
	first, err := sessionWorkspaceRoot(data, "tenant-a", "same-session-name")
	if err != nil {
		t.Fatal(err)
	}
	second, err := sessionWorkspaceRoot(data, "tenant-b", "same-session-name")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("two tenants share workspace")
	}
	if err := os.WriteFile(filepath.Join(first, "owned-by-a"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(second, "owned-by-a")); !os.IsNotExist(err) {
		t.Fatalf("tenant-b sees tenant-a's file: %v", err)
	}
	for _, sess := range sessions {
		if err := sess.Shutdown(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := launcher.Launch(ctx, LaunchScope{TenantID: "tenant-a", SessionID: "same-session-name", AgentID: CarbonAgentID, Placement: sessionwire.HostPlacementPooled, RigSessionID: sessions[0].SessionID(), Restore: true}); err != nil {
		t.Fatalf("restore tenant-a: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(first, "owned-by-a")); err != nil || string(b) != "a" {
		t.Fatalf("restore lost workspace: %q %v", b, err)
	}
}

func TestPooledLauncherCheckpointSelectsExactTenantSession(t *testing.T) {
	ctx := context.Background()
	launcher, err := OpenPooledLauncher(ctx, Config{HomeDir: t.TempDir()}, t.TempDir(),
		WithServeInferenceClient(func() (inference.Client, ModelFactory, error) {
			return &fakeLLM{}, newModelFactoryFor(testModel()), nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = launcher.Close(ctx) })
	id, err := uuid.New()
	if err != nil {
		t.Fatal(err)
	}
	controller, err := launcher.Launch(ctx, LaunchScope{TenantID: "tenant-a", SessionID: "same-session", AgentID: CarbonAgentID, Placement: sessionwire.HostPlacementPooled, RigSessionID: id})
	if err != nil {
		t.Fatal(err)
	}
	if err := launcher.Checkpoint(ctx, "tenant-b", "same-session"); err == nil {
		t.Fatal("other tenant checkpointed session")
	}
	if err := launcher.Checkpoint(ctx, "tenant-a", "other-session"); err == nil {
		t.Fatal("other session checkpointed")
	}
	if err := launcher.Checkpoint(ctx, "tenant-a", "same-session"); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if err := controller.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestPooledLauncherPreparedCompatibilityRejectsDrift(t *testing.T) {
	ctx := context.Background()
	launcher, err := OpenPooledLauncher(ctx, Config{HomeDir: t.TempDir()}, t.TempDir(), WithServeInferenceClient(func() (inference.Client, ModelFactory, error) {
		return &fakeLLM{}, newModelFactoryFor(testModel()), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = launcher.Close(ctx) })
	id, err := launcher.PrepareCompatibility(ctx)
	if err != nil || id == "" {
		t.Fatalf("prepare: %q %v", id, err)
	}
	launcher.cfg.AccessProfile = AccessTrusted
	runtimeID, err := uuid.New()
	if err != nil {
		t.Fatal(err)
	}
	_, err = launcher.Launch(ctx, LaunchScope{TenantID: "tenant-a", SessionID: "session-a", AgentID: CarbonAgentID, Placement: sessionwire.HostPlacementPooled, RigSessionID: runtimeID})
	var drift *PooledCompatibilityDriftError
	if !errors.As(err, &drift) {
		t.Fatalf("launch after drift = %v, want typed refusal", err)
	}
}

func TestPooledHostCompatibilityNormalizesOnlyWorkspaceRoot(t *testing.T) {
	cfg := Config{HomeDir: t.TempDir()}
	a, err := buildSessionAccess(cfg, t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := buildSessionAccess(cfg, t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if a.configRev == b.configRev {
		t.Fatal("actual Harness access fingerprints must remain root-sensitive")
	}
	cfg.AccessConfigRev = a.pooledConfigRev
	one := CarbonCompatibilityID(cfg)
	cfg.AccessConfigRev = b.pooledConfigRev
	if two := CarbonCompatibilityID(cfg); one != two {
		t.Fatalf("pooled identity drifted across roots: %q vs %q", one, two)
	}
}

func TestPooledCheckpointRefusesActualJournalAppendFault(t *testing.T) {
	ctx := context.Background()
	launcher, err := OpenPooledLauncher(ctx, Config{HomeDir: t.TempDir()}, t.TempDir(), WithServeInferenceClient(func() (inference.Client, ModelFactory, error) {
		return &fakeLLM{}, newModelFactoryFor(testModel()), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = launcher.Close(ctx) })
	id, err := uuid.New()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := launcher.Launch(ctx, LaunchScope{TenantID: "tenant-a", SessionID: "session-a", AgentID: CarbonAgentID, Placement: sessionwire.HostPlacementPooled, RigSessionID: id}); err != nil {
		t.Fatal(err)
	}
	journalRoot := launcher.tenants["tenant-a"].fs.StoragePaths()[0]
	streams := filepath.Join(journalRoot, "streams")
	moved := streams + "-offline"
	if err := os.Rename(streams, moved); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Rename(moved, streams) }()
	if err := launcher.Checkpoint(ctx, "tenant-a", "session-a"); err == nil {
		t.Fatal("checkpoint accepted workspace snapshot whose durable journal append failed")
	}
}

func TestPooledRigPersistsDistinctRootsAndRefusesMovedRootRestore(t *testing.T) {
	ctx := context.Background()
	data := t.TempDir()
	home := t.TempDir()
	build := func() ServeHostOption {
		return WithServeInferenceClient(func() (inference.Client, ModelFactory, error) {
			return &fakeLLM{}, newModelFactoryFor(testModel()), nil
		})
	}
	first, err := OpenPooledLauncher(ctx, Config{HomeDir: home}, data, build())
	if err != nil {
		t.Fatal(err)
	}
	id, err := first.PrepareCompatibility(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var controllers []session.SessionController
	var ids []uuid.UUID
	for _, name := range []sessionwire.SessionID{"session-a", "session-b"} {
		runtimeID, err := uuid.New()
		if err != nil {
			t.Fatal(err)
		}
		controller, err := first.Launch(ctx, LaunchScope{TenantID: "tenant-a", SessionID: name, AgentID: CarbonAgentID, Placement: sessionwire.HostPlacementPooled, RigSessionID: runtimeID})
		if err != nil {
			t.Fatal(err)
		}
		controllers, ids = append(controllers, controller), append(ids, runtimeID)
	}
	metas, err := first.tenants["tenant-a"].stores.catalog.ListSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var roots []string
	var revs []string
	for _, meta := range metas {
		for _, runtimeID := range ids {
			if meta.SessionID == runtimeID {
				roots = append(roots, meta.ConfigFingerprint.WorkspaceRoot)
				revs = append(revs, meta.ConfigFingerprint.NativePermissionPolicyRev)
			}
		}
	}
	if len(roots) != 2 || roots[0] == "" || roots[1] == "" || roots[0] == roots[1] {
		t.Fatalf("persisted pooled roots = %v", roots)
	}
	if len(revs) != 2 || revs[0] != revs[1] {
		t.Fatalf("pooled access revisions differ across roots: %v", revs)
	}
	for _, controller := range controllers {
		releaser, ok := controller.(session.Releaser)
		if !ok {
			t.Fatal("pooled controller lacks nonterminal release")
		}
		if err := releaser.ReleaseResidency(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := first.Close(ctx); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(t.TempDir(), "moved-root")
	if err := os.Rename(data, moved); err != nil {
		t.Fatal(err)
	}
	second, err := OpenPooledLauncher(ctx, Config{HomeDir: home}, moved, build())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close(ctx)
	if next, err := second.PrepareCompatibility(ctx); err != nil || next != id {
		t.Fatalf("pooled compatibility changed with root: %q %v", next, err)
	}
	_, err = second.Launch(ctx, LaunchScope{TenantID: "tenant-a", SessionID: "session-a", AgentID: CarbonAgentID, Placement: sessionwire.HostPlacementPooled, RigSessionID: ids[0], Restore: true})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "workspace") {
		t.Fatalf("moved-root restore = %v, want workspace fingerprint refusal", err)
	}
}

func TestCarbonDepartmentDerivesPoolingFromLauncher(t *testing.T) {
	pooled, err := OpenPooledLauncher(context.Background(), Config{}, t.TempDir(),
		WithServeInferenceClient(func() (inference.Client, ModelFactory, error) {
			return &fakeLLM{}, newModelFactoryFor(testModel()), nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pooled.Close(context.Background()) })
	for _, tc := range []struct {
		name     string
		launcher SessionLauncher
		pooled   bool
	}{
		{"pooled", pooled, true},
		{"single-root", singleRootLauncher{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dept, err := NewCarbonDepartment(tc.launcher, department.CompatibilityID("test-build"))
			if err != nil {
				t.Fatal(err)
			}
			target, err := dept.Target(CarbonAgentID)
			if err != nil {
				t.Fatal(err)
			}
			if got := target.Capabilities().SupportsPooled; got != tc.pooled {
				t.Fatalf("SupportsPooled = %t, want %t", got, tc.pooled)
			}
		})
	}
}

func TestPooledLauncherWorkspaceProviderUsesTheLaunchRoot(t *testing.T) {
	ctx := context.Background()
	data := t.TempDir()
	launcher, err := OpenPooledLauncher(ctx, Config{}, data)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = launcher.Close(ctx) })
	root, err := launcher.EnsureWorkspace(ctx, "tenant/a", "session/1")
	if err != nil {
		t.Fatal(err)
	}
	want, err := sessionWorkspaceRoot(data, "tenant/a", "session/1")
	if err != nil {
		t.Fatal(err)
	}
	if root != want {
		t.Fatalf("workspace provider returned %q, launch wants %q", root, want)
	}
	if err := os.WriteFile(filepath.Join(root, "work"), []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := launcher.ReleaseWorkspace(ctx, "tenant/a", "session/1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "work")); err != nil {
		t.Fatalf("release erased restorable work: %v", err)
	}
}

func TestPooledLauncherProvidesDistinctDurableTenantJournals(t *testing.T) {
	ctx := context.Background()
	data := t.TempDir()
	launcher, err := OpenPooledLauncher(ctx, Config{}, data)
	if err != nil {
		t.Fatal(err)
	}
	a, err := launcher.JournalStoreForTenant("tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := launcher.JournalStoreForTenant("tenant-b")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two tenants received the same journal store")
	}
	again, err := launcher.JournalStoreForTenant("tenant-a")
	if err != nil || again != a {
		t.Fatalf("tenant-a journal cache = %p, %v, want %p", again, err, a)
	}
	if err := launcher.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := launcher.JournalStoreForTenant("tenant-a"); err == nil {
		t.Fatal("closed launcher issued a journal reader")
	}
	if _, err := launcher.Launch(ctx, LaunchScope{TenantID: "tenant-a", SessionID: "closed", AgentID: CarbonAgentID}); err == nil {
		t.Fatal("closed launcher accepted a launch")
	}
	reopened, err := OpenPooledLauncher(ctx, Config{}, data)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close(ctx) })
	if _, err := reopened.JournalStoreForTenant("tenant-a"); err != nil {
		t.Fatalf("reopen tenant-a journal: %v", err)
	}
	if _, err := reopened.JournalStoreForTenant("tenant-b"); err != nil {
		t.Fatalf("reopen tenant-b journal: %v", err)
	}
}

func TestPooledLauncherDoesNotSerializeOtherTenantBehindSlowLaunch(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	var calls atomic.Int32
	launcher, err := OpenPooledLauncher(context.Background(), Config{HomeDir: t.TempDir()}, t.TempDir(),
		WithServeInferenceClient(func() (inference.Client, ModelFactory, error) {
			if calls.Add(1) == 1 {
				close(entered)
				<-release
			}
			return &fakeLLM{}, newModelFactoryFor(testModel()), nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unblock(); _ = launcher.Close(context.Background()) })
	firstID, _ := uuid.New()
	secondID, _ := uuid.New()
	firstDone := make(chan error, 1)
	go func() {
		_, err := launcher.Launch(context.Background(), LaunchScope{TenantID: "tenant-a", SessionID: "session-a", AgentID: CarbonAgentID, RigSessionID: firstID})
		firstDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first launch did not enter client construction")
	}
	duplicateDone := make(chan error, 1)
	go func() {
		_, err := launcher.Launch(context.Background(), LaunchScope{TenantID: "tenant-a", SessionID: "session-a", AgentID: CarbonAgentID, RigSessionID: firstID})
		duplicateDone <- err
	}()
	select {
	case err := <-duplicateDone:
		if err == nil {
			t.Fatal("second launch reused an in-flight runtime ID")
		}
	case <-time.After(time.Second):
		t.Fatal("in-flight runtime ID was not reserved promptly")
	}
	otherDone := make(chan error, 1)
	go func() {
		if _, err := launcher.JournalStoreForTenant("tenant-b"); err != nil {
			otherDone <- err
			return
		}
		_, err := launcher.Launch(context.Background(), LaunchScope{TenantID: "tenant-b", SessionID: "session-b", AgentID: CarbonAgentID, RigSessionID: secondID})
		otherDone <- err
	}()
	select {
	case err := <-otherDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second tenant blocked behind first launch")
	}
	unblock()
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first launch did not finish")
	}
}

func TestPooledLauncherCloseWaitIsBoundedWhileLaunchCleanupContinues(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	data := t.TempDir()
	launcher, err := OpenPooledLauncher(context.Background(), Config{HomeDir: t.TempDir()}, data,
		WithServeInferenceClient(func() (inference.Client, ModelFactory, error) {
			close(entered)
			<-release
			return &fakeLLM{}, newModelFactoryFor(testModel()), nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unblock(); _ = launcher.Close(context.Background()) })
	id, _ := uuid.New()
	launchDone := make(chan error, 1)
	go func() {
		_, err := launcher.Launch(context.Background(), LaunchScope{TenantID: "tenant-a", SessionID: "session-a", AgentID: CarbonAgentID, RigSessionID: id})
		launchDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("launch did not stall")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := launcher.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close wait = %v, want caller deadline", err)
	}
	if _, err := launcher.JournalStoreForTenant("tenant-a"); err == nil {
		t.Fatal("closing launcher issued journal reader")
	}
	duplicateDone := make(chan error, 1)
	go func() {
		_, err := launcher.Launch(context.Background(), LaunchScope{TenantID: "tenant-b", SessionID: "other", AgentID: CarbonAgentID, RigSessionID: id})
		duplicateDone <- err
	}()
	select {
	case err := <-duplicateDone:
		if err == nil {
			t.Fatal("closing launcher admitted launch")
		}
	case <-time.After(time.Second):
		t.Fatal("closing launcher blocked new launch")
	}
	unblock()
	select {
	case err := <-launchDone:
		var closed *StoreClosedError
		if !errors.As(err, &closed) {
			t.Fatalf("inflight launch = %v, want closed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("inflight launch did not leave")
	}
	if err := launcher.Close(context.Background()); err != nil {
		t.Fatalf("later Close did not observe cleanup: %v", err)
	}
	reopened, err := OpenPooledLauncher(context.Background(), Config{}, data)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close(context.Background()) })
	if _, err := reopened.JournalStoreForTenant("tenant-a"); err != nil {
		t.Fatalf("tenant journal did not reopen after final Close: %v", err)
	}
}

// A pooled session's stdio MCP servers start in that session's workspace root,
// never in the server process's working directory: a filesystem or git server
// with relative/default roots must serve the tenant's session, not the process.
func TestPooledLauncherStartsStdioMCPServersInTheSessionRoot(t *testing.T) {
	ctx := context.Background()
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not installed")
	}
	t.Chdir(t.TempDir())
	home := t.TempDir()
	report := filepath.Join(canonicalTempDir(t), "mcp-cwd")
	config, err := json.Marshal(map[string]any{"mcpServers": map[string]any{
		"cwdprobe": map[string]any{"command": shell, "args": []string{"-c", `pwd -P > "$0"`, report}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "mcp.json"), config, 0o600); err != nil {
		t.Fatal(err)
	}
	data := t.TempDir()
	launcher, err := OpenPooledLauncher(ctx, Config{HomeDir: home}, data,
		WithServeInferenceClient(func() (inference.Client, ModelFactory, error) {
			return &fakeLLM{}, newModelFactoryFor(testModel()), nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = launcher.Close(ctx) })
	id, err := uuid.New()
	if err != nil {
		t.Fatal(err)
	}
	sess, err := launcher.Launch(ctx, LaunchScope{TenantID: "tenant-a", SessionID: "mcp-session", AgentID: CarbonAgentID,
		Placement: sessionwire.HostPlacementPooled, RigSessionID: id})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	t.Cleanup(func() { _ = sess.Shutdown(ctx) })
	root, err := sessionWorkspaceRoot(data, "tenant-a", "mcp-session")
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		got, err := os.ReadFile(report)
		if err == nil && len(got) > 0 {
			if strings.TrimSpace(string(got)) != want {
				t.Fatalf("stdio MCP server started in %q, want the session root %q", strings.TrimSpace(string(got)), want)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("stdio MCP server never reported its working directory: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// PooledLauncher refuses a launch whose scope names a workspace root other than
// the one it serves for that tenant session, before composing anything.
func TestPooledLauncherRefusesAForeignWorkspaceRoot(t *testing.T) {
	ctx := context.Background()
	data := t.TempDir()
	launcher, err := OpenPooledLauncher(ctx, Config{HomeDir: t.TempDir()}, data,
		WithServeInferenceClient(func() (inference.Client, ModelFactory, error) {
			return &fakeLLM{}, newModelFactoryFor(testModel()), nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = launcher.Close(ctx) })
	id, err := uuid.New()
	if err != nil {
		t.Fatal(err)
	}
	foreign := t.TempDir()
	sess, err := launcher.Launch(ctx, LaunchScope{TenantID: "tenant-a", SessionID: "s1", AgentID: CarbonAgentID,
		Placement: sessionwire.HostPlacementPooled, RigSessionID: id, WorkspaceRoot: foreign})
	var foreignErr *ForeignWorkspaceRootError
	if !errors.As(err, &foreignErr) {
		if sess != nil {
			_ = sess.Shutdown(ctx)
		}
		t.Fatalf("launch with a foreign workspace root = %v, want *ForeignWorkspaceRootError", err)
	}
	served, err := sessionWorkspaceRoot(data, "tenant-a", "s1")
	if err != nil {
		t.Fatal(err)
	}
	if foreignErr.Want != foreign || foreignErr.Served != served || foreignErr.SessionID != "s1" {
		t.Fatalf("ForeignWorkspaceRootError = %+v, want Want=%q Served=%q", foreignErr, foreign, served)
	}
}
