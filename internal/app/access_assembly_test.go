package app

import (
	"context"
	"strings"
	"testing"
)

// TestSessionAccessPerRoleExecutorSeparation proves the session access wiring gives each role
// its OWN executor set and each Loop ID its OWN executor within a set. Builder uses the selected
// writable profile; planner and reviewer each use separate read-only sets. A repeated Loop ID
// is memoized to the same executor.
func TestSessionAccessPerRoleExecutorSeparation(t *testing.T) {
	access, _ := headlessTestAccess(t, Config{}, t.TempDir())

	if access.plannerSet == access.builderSet || access.builderSet == access.reviewerSet || access.plannerSet == access.reviewerSet {
		t.Fatal("roles share an executor set, want separate sets per role authority")
	}

	planner, err := access.plannerSet.For("planner-loop")
	if err != nil {
		t.Fatalf("plannerSet.For: %v", err)
	}
	builder, err := access.builderSet.For("builder-loop")
	if err != nil {
		t.Fatalf("builderSet.For: %v", err)
	}
	if planner == builder {
		t.Fatal("planner and builder resolved the SAME executor")
	}
	if again, _ := access.plannerSet.For("planner-loop"); again != planner {
		t.Fatal("repeated planner Loop ID did not memoize to the same executor")
	}

	reviewer, err := access.reviewerSet.For("reviewer-loop")
	if err != nil {
		t.Fatalf("reviewerSet.For: %v", err)
	}
	if reviewer == builder {
		t.Fatal("reviewer executor equals builder executor; the restricted role must use a separate set")
	}
}

// TestSessionAccessCloseIsIdempotent proves the runtime agent can close the session's executor
// sets exactly once: a second Close is a no-op returning the same result, and materialized
// executors are released.
func TestSessionAccessCloseIsIdempotent(t *testing.T) {
	access, err := buildHeadlessAccess(Config{}, t.TempDir())
	if err != nil {
		t.Fatalf("buildHeadlessAccess: %v", err)
	}
	if _, err := access.builderSet.For("live-loop"); err != nil {
		t.Fatalf("For: %v", err)
	}
	if err := access.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := access.Close(); err != nil {
		t.Fatalf("second Close (must be a no-op): %v", err)
	}
}

// TestRestoreRejectsAccessProfileDrift proves the fixed-profile rule at the durable boundary:
// a session opened under one selected profile cannot be restored under a different one, because
// the changed profile changes the access-config digest folded into the rig fingerprint. The
// same profile restores cleanly (new/restore parity over one assembly path).
func TestRestoreRejectsAccessProfileDrift(t *testing.T) {
	stores, err := openTestStores(t)
	if err != nil {
		t.Fatalf("openTestStores: %v", err)
	}
	root := t.TempDir()

	// Open a new session under the ReadOnly profile and shut it down.
	access, cfg := headlessTestAccess(t, Config{AccessProfile: AccessReadOnly}, root)
	definitions, err := swarmDefinitions(&fakeLLM{}, testModel(), cfg, access)
	if err != nil {
		t.Fatalf("swarmDefinitions: %v", err)
	}
	assembly, err := buildRig(definitions, stores, root, cfg, false)
	if err != nil {
		t.Fatalf("buildRig: %v", err)
	}
	controller, err := assembly.NewSession(context.Background())
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	sid := controller.SessionID()
	if err := controller.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	restore := func(t *testing.T, profile AccessProfile) error {
		t.Helper()
		racc, rcfg := headlessTestAccess(t, Config{AccessProfile: profile}, root)
		rdefs, err := swarmDefinitions(&fakeLLM{}, testModel(), rcfg, racc)
		if err != nil {
			t.Fatalf("swarmDefinitions: %v", err)
		}
		rasm, err := buildRig(rdefs, stores, root, rcfg, false)
		if err != nil {
			t.Fatalf("buildRig: %v", err)
		}
		rctrl, err := rasm.RestoreSession(context.Background(), sid)
		if err == nil {
			_ = rctrl.Shutdown(context.Background())
		}
		return err
	}

	if err := restore(t, AccessTrusted); err == nil {
		t.Fatal("restore under a DIFFERENT access profile succeeded, want a configuration mismatch rejection")
	}
	if err := restore(t, AccessReadOnly); err != nil {
		t.Fatalf("restore under the SAME access profile failed: %v", err)
	}
}

// TestSessionPresenterProjectsDiagnostics proves the runtime agent's SessionPresenter reports
// the session's fixed profile, workspace root, and the permission-load diagnostics captured in
// the access wiring (the manual out-of-catalog family notices the TUI shows before the first
// gate).
func TestSessionPresenterProjectsDiagnostics(t *testing.T) {
	access := &sessionAccess{
		profileName: string(AccessTrusted),
		workspace:   "/work/root",
		diagnostics: []string{"allow family \"git commit\" is outside the automatic eligibility catalog"},
	}
	agent := &RuntimeAgent{root: "/work/root", access: access}

	presentation := agent.SessionPresentation()
	if presentation.ProfileName != string(AccessTrusted) {
		t.Errorf("ProfileName = %q, want %q", presentation.ProfileName, AccessTrusted)
	}
	if presentation.WorkspaceRoot != "/work/root" {
		t.Errorf("WorkspaceRoot = %q, want /work/root", presentation.WorkspaceRoot)
	}
	if len(presentation.PermissionDiagnostics) != 1 || !strings.Contains(presentation.PermissionDiagnostics[0], "git commit") {
		t.Errorf("PermissionDiagnostics = %v, want the out-of-catalog family notice", presentation.PermissionDiagnostics)
	}
}

// TestSessionAccessDigestOmitsProxyCredentials proves an organization proxy's upstream
// credentials never reach the durable access-config digest: only the route's redacted
// fingerprint and guarantee bits contribute.
func TestSessionAccessDigestOmitsProxyCredentials(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://alice:sup3rsecret@proxy.example:8080")
	t.Setenv("NO_PROXY", "")

	access, err := buildHeadlessAccess(Config{AccessProfile: AccessTrusted}, t.TempDir())
	if err != nil {
		t.Fatalf("buildHeadlessAccess: %v", err)
	}
	t.Cleanup(func() { _ = access.Close() })

	if strings.Contains(access.configRev, "sup3rsecret") || strings.Contains(access.configRev, "alice") {
		t.Fatalf("access config digest leaked upstream proxy credentials: %q", access.configRev)
	}
}
