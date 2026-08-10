package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLooprigHome(t *testing.T) {
	t.Run("empty uses user home", func(t *testing.T) {
		got, err := looprigHome(Config{})
		if err != nil {
			t.Fatal(err)
		}
		home, _ := os.UserHomeDir()
		if got != filepath.Join(home, ".looprig", "carbon") {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("absolute override wins", func(t *testing.T) {
		dir := t.TempDir()
		got, err := looprigHome(Config{HomeDir: dir})
		if err != nil {
			t.Fatal(err)
		}
		if got != dir {
			t.Fatalf("got %q want %q", got, dir)
		}
	})
	t.Run("relative override rejected", func(t *testing.T) {
		if _, err := looprigHome(Config{HomeDir: "rel/path"}); err == nil {
			t.Fatal("want error for relative HomeDir")
		}
	})
}

// TestLooprigHomeIsolatedFromRealHOME proves TestMain's process-wide HOME
// isolation (subagent_acp_peer_test.go) actually reaches looprigHome: a bare
// Config{} -- exactly what most of this package's tests use, including
// indirectly through openRuntimeAgent's shared MCP-loading path
// (loadMCPConfig -> looprigHome) -- must resolve under the temporary
// directory TestMain created for this test run, never the real developer
// machine's HOME.
//
// This is the property the whole isolation fix depends on: without it,
// `go test ./internal/app/...` would read a real ~/.looprig/mcp.json (once
// one exists on a developer's machine, which this task's background notes
// is imminent) and attempt real MCP server connections as a side effect of
// running unit tests.
func TestLooprigHomeIsolatedFromRealHOME(t *testing.T) {
	if testIsolatedHome == "" {
		t.Fatal("testIsolatedHome is empty: TestMain did not establish process-wide HOME isolation")
	}

	got, err := looprigHome(Config{})
	if err != nil {
		t.Fatalf("looprigHome(Config{}) error = %v", err)
	}

	// The decisive assertion: looprigHome(Config{}) must resolve to exactly
	// the isolated directory TestMain created, not wherever the real
	// developer machine's HOME points.
	want := filepath.Join(testIsolatedHome, ".looprig", "carbon")
	if got != want {
		t.Fatalf("looprigHome(Config{}) = %q, want %q (TestMain's isolated HOME)", got, want)
	}

	// Defense in depth, independent of the exact-match assertion above: the
	// isolated directory must live under the OS temp directory and carry
	// TestMain's own MkdirTemp prefix -- never something that looks like a
	// real user home directory (e.g. /Users/<name> or /home/<name>).
	if !strings.HasPrefix(filepath.Base(testIsolatedHome), "carbon-internal-app-test-home-") {
		t.Fatalf("isolated HOME %q does not carry TestMain's temp-dir prefix", testIsolatedHome)
	}
	if !strings.HasPrefix(testIsolatedHome, os.TempDir()) {
		t.Fatalf("isolated HOME %q is not under the OS temp directory %q", testIsolatedHome, os.TempDir())
	}
}
