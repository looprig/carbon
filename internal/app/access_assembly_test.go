package app

import (
	"context"
	"strings"
	"testing"
)

// TestSessionAccessUsesOneCarbonExecutorSet proves one session has one access
// authority. Executor identities remain loop-scoped within that set, while
// repeated lookups for one Carbon loop are memoized.
func TestSessionAccessUsesOneCarbonExecutorSet(t *testing.T) {
	access, err := buildHeadlessAccess(Config{}, t.TempDir())
	if err != nil {
		t.Fatalf("buildHeadlessAccess: %v", err)
	}
	t.Cleanup(func() { _ = access.Close() })

	if access.set == nil {
		t.Fatal("session access set is nil")
	}
	if access.gate == nil {
		t.Fatal("session access gate is nil")
	}
	if access.policyRev == "" {
		t.Fatal("session access policy revision is empty")
	}

	first, err := access.set.For("generic-loop")
	if err != nil {
		t.Fatalf("set.For(generic-loop): %v", err)
	}
	second, err := access.set.For("generic-loop")
	if err != nil {
		t.Fatalf("set.For(generic-loop) repeat: %v", err)
	}
	if second != first {
		t.Fatal("repeated Carbon loop ID did not memoize to the same executor")
	}
	other, err := access.set.For("generic-child-loop")
	if err != nil {
		t.Fatalf("set.For(generic-child-loop): %v", err)
	}
	if other == first {
		t.Fatal("different Carbon loop IDs resolved to the same executor")
	}
}

// TestSessionAccessCloseIsIdempotent proves the one executor set is released
// exactly once and repeated shutdown remains harmless.
func TestSessionAccessCloseIsIdempotent(t *testing.T) {
	access, err := buildHeadlessAccess(Config{}, t.TempDir())
	if err != nil {
		t.Fatalf("buildHeadlessAccess: %v", err)
	}
	if _, err := access.set.For("live-loop"); err != nil {
		t.Fatalf("set.For: %v", err)
	}
	if err := access.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := access.Close(); err != nil {
		t.Fatalf("second Close (must be a no-op): %v", err)
	}
}

// TestRestoreRejectsAccessProfileDrift proves the fixed-profile rule at the
// durable boundary. A profile change changes the single access digest.
func TestRestoreRejectsAccessProfileDrift(t *testing.T) {
	stores, err := openTestStores(t)
	if err != nil {
		t.Fatalf("openTestStores: %v", err)
	}
	root := t.TempDir()

	access, cfg := headlessTestAccess(t, Config{AccessProfile: AccessReadOnly}, root)
	definition, err := carbonTestDefinition(&fakeLLM{}, testModel(), cfg, access)
	if err != nil {
		t.Fatalf("carbonTestDefinition: %v", err)
	}
	assembly, err := buildRig(definition, stores, root, cfg, false)
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
		rdef, err := carbonTestDefinition(&fakeLLM{}, testModel(), rcfg, racc)
		if err != nil {
			t.Fatalf("carbonTestDefinition: %v", err)
		}
		rasm, err := buildRig(rdef, stores, root, rcfg, false)
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
		t.Fatal("restore under a different access profile succeeded")
	}
	if err := restore(t, AccessReadOnly); err != nil {
		t.Fatalf("restore under the same access profile failed: %v", err)
	}
}

// TestSessionPresenterProjectsDiagnostics proves the runtime agent's session
// presentation still exposes the fixed profile and permission diagnostics.
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

// TestSessionAccessDigestOmitsProxyCredentials proves upstream proxy
// credentials never enter the durable access-config digest.
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
