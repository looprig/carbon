package app

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	sessionwire "github.com/looprig/core/sessionwire/v1"
)

func TestSessionWorkspaceRootIsStableAndIsolated(t *testing.T) {
	data := t.TempDir()
	a, err := materializeSessionWorkspaceRoot(data, "tenant/a", "session")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a, "work.txt"), []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}
	again, err := materializeSessionWorkspaceRoot(data, "tenant/a", "session")
	if err != nil {
		t.Fatal(err)
	}
	if again != a {
		t.Fatalf("restored root %q, want %q", again, a)
	}
	if b, err := os.ReadFile(filepath.Join(again, "work.txt")); err != nil || string(b) != "kept" {
		t.Fatalf("restored content %q, %v", b, err)
	}
	other, err := materializeSessionWorkspaceRoot(data, "tenant", "a/session")
	if err != nil {
		t.Fatal(err)
	}
	if other == a {
		t.Fatal("different identities share a root")
	}
	for _, root := range []string{a, other} {
		if filepath.Dir(root) != filepath.Join(data, "session-workspaces") {
			t.Fatalf("root escaped data directory: %q", root)
		}
	}
}

func TestSessionWorkspaceRootRefusesMissingIdentity(t *testing.T) {
	_, err := materializeSessionWorkspaceRoot(t.TempDir(), sessionwire.TenantID("tenant"), "")
	if !errors.Is(err, ErrNoSessionIdentity) {
		t.Fatalf("missing session = %v", err)
	}
}
