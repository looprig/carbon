package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

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
		{"single-root", NewServeHostLauncher(&ServeHost{workspace: "/single"}), false},
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
