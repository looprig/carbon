package app

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestProductFamilyEligibility pins Carbon's automatic Bash-family catalog to
// EXACTLY the five safe git subcommands: neighbors and exec-capable prefixes are
// out.
func TestProductFamilyEligibility(t *testing.T) {
	t.Parallel()
	eligible := productFamilyEligibility()

	in := [][]string{
		{"git", "log"},
		{"git", "status"},
		{"git", "diff"},
		{"git", "show"},
		{"git", "push"},
	}
	for _, tokens := range in {
		if !eligible(tokens) {
			t.Errorf("eligible(%v) = false, want true", tokens)
		}
	}

	out := [][]string{
		{"git"},                     // bare prefix, not a family
		{"git", "commit"},           // neighbor, not eligible
		{"git", "logs"},             // typo neighbor
		{"git", "log", "--oneline"}, // full invocation, not the family prefix
		{"git", "clone"},
		{"npm", "run"},
		{"find", "."},
		{"env"},
		nil,
		{},
	}
	for _, tokens := range out {
		if eligible(tokens) {
			t.Errorf("eligible(%v) = true, want false", tokens)
		}
	}
}

// TestDefaultPermissionsPath proves the interactive default path is
// <home>/workspaces/<sha256(canonical-workspace)>/permissions.json for the
// given already-resolved looprig home, and that a relative or empty
// workspace fails closed regardless of home.
func TestDefaultPermissionsPath(t *testing.T) {
	t.Parallel()

	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}
	looprigRoot := filepath.Join(home, ".looprig")
	ws := "/canonical/workspace/root"
	got, err := defaultPermissionsPath(looprigRoot, ws)
	if err != nil {
		t.Fatalf("defaultPermissionsPath(%q, %q) error = %v", looprigRoot, ws, err)
	}
	digest := sha256.Sum256([]byte(ws))
	want := filepath.Join(looprigRoot, "workspaces", hex.EncodeToString(digest[:]), "permissions.json")
	if got != want {
		t.Errorf("defaultPermissionsPath = %q, want %q", got, want)
	}

	for _, bad := range []string{"", "relative/path", "./x"} {
		if _, err := defaultPermissionsPath(looprigRoot, bad); err == nil {
			t.Errorf("defaultPermissionsPath(%q) = nil error; want fail-closed", bad)
		}
	}
}

// TestDefaultPermissionsPathHomeDirOverride proves an overridden looprig home
// changes where the interactive permission file resolves to, independent of
// the process's real HOME.
func TestDefaultPermissionsPathHomeDirOverride(t *testing.T) {
	t.Parallel()

	override := t.TempDir()
	looprigRoot, err := looprigHome(Config{HomeDir: override})
	if err != nil {
		t.Fatalf("looprigHome(Config{HomeDir: %q}) error = %v", override, err)
	}
	if looprigRoot != override {
		t.Fatalf("looprigHome(Config{HomeDir: %q}) = %q, want %q", override, looprigRoot, override)
	}

	ws := "/canonical/workspace/root"
	got, err := defaultPermissionsPath(looprigRoot, ws)
	if err != nil {
		t.Fatalf("defaultPermissionsPath(%q, %q) error = %v", looprigRoot, ws, err)
	}
	digest := sha256.Sum256([]byte(ws))
	want := filepath.Join(override, "workspaces", hex.EncodeToString(digest[:]), "permissions.json")
	if got != want {
		t.Errorf("defaultPermissionsPath(override) = %q, want %q", got, want)
	}
	if !strings.HasPrefix(got, override) {
		t.Errorf("defaultPermissionsPath(override) = %q, want prefix %q", got, override)
	}
}

// TestInteractivePermissionConfig proves interactive assembly derives the
// home-based path (for the already-resolved looprig home it is given) and
// attaches the family catalog.
func TestInteractivePermissionConfig(t *testing.T) {
	t.Parallel()

	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}
	looprigRoot := filepath.Join(home, ".looprig")
	ws := "/canonical/ws"
	cfg, err := interactivePermissionConfig(looprigRoot, ws)
	if err != nil {
		t.Fatalf("interactivePermissionConfig error = %v", err)
	}
	if !strings.HasPrefix(cfg.Path, filepath.Join(looprigRoot, "workspaces")) {
		t.Errorf("interactive Path = %q, want under %q", cfg.Path, filepath.Join(looprigRoot, "workspaces"))
	}
	if cfg.FamilyEligible == nil {
		t.Fatal("interactive config has no family catalog")
	}
	if !cfg.FamilyEligible([]string{"git", "push"}) {
		t.Error("interactive family catalog missing git push")
	}
}

// TestInteractivePermissionConfigHomeDirOverride proves interactive assembly
// resolves the permission path under an overridden looprig home rather than
// the process's real HOME.
func TestInteractivePermissionConfigHomeDirOverride(t *testing.T) {
	t.Parallel()

	override := t.TempDir()
	looprigRoot, err := looprigHome(Config{HomeDir: override})
	if err != nil {
		t.Fatalf("looprigHome(Config{HomeDir: %q}) error = %v", override, err)
	}
	ws := "/canonical/ws"
	cfg, err := interactivePermissionConfig(looprigRoot, ws)
	if err != nil {
		t.Fatalf("interactivePermissionConfig error = %v", err)
	}
	if !strings.HasPrefix(cfg.Path, filepath.Join(override, "workspaces")) {
		t.Errorf("interactive Path = %q, want under %q", cfg.Path, filepath.Join(override, "workspaces"))
	}
}

// TestHeadlessPermissionConfig proves headless assembly passes an explicit path
// through, uses an empty rule set for an empty path, rejects a relative path, and
// NEVER derives a HOME path.
func TestHeadlessPermissionConfig(t *testing.T) {
	t.Parallel()

	home, err := os.UserHomeDir()
	if err != nil {
		home = "/nonexistent-home-sentinel"
	}

	// Explicit absolute path passes through unchanged with the catalog attached.
	explicit := "/etc/carbon/permissions.json"
	cfg, err := headlessPermissionConfig(explicit)
	if err != nil {
		t.Fatalf("headlessPermissionConfig(%q) error = %v", explicit, err)
	}
	if cfg.Path != explicit {
		t.Errorf("headless Path = %q, want %q", cfg.Path, explicit)
	}
	if cfg.FamilyEligible == nil {
		t.Error("headless config has no family catalog")
	}

	// Empty path => empty rule set, never HOME.
	empty, err := headlessPermissionConfig("")
	if err != nil {
		t.Fatalf("headlessPermissionConfig(\"\") error = %v", err)
	}
	if empty.Path != "" {
		t.Errorf("headless empty Path = %q, want \"\" (never HOME)", empty.Path)
	}
	if strings.Contains(empty.Path, home) && home != "" && home != "/" {
		t.Errorf("headless config searched HOME: %q", empty.Path)
	}

	// A relative path fails closed.
	if _, err := headlessPermissionConfig("relative/permissions.json"); err == nil {
		t.Error("headlessPermissionConfig(relative) = nil error; want fail-closed")
	}
}
