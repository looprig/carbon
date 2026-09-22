package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A pooled session's runtime context names the session root, and git state is
// read inside that root only: an enclosing repository (for example one holding
// the server's data directory) is never reported to the model. Git runs inside
// the session sandbox (runtime_context_escape_test.go holds that it never runs
// with the server's own authority).
func TestSessionRuntimeContextNamesRootAndStopsGitDiscoveryAtIt(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	enclosing := t.TempDir()
	gitRepo(t, enclosing, "enclosing-branch")
	if err := os.WriteFile(filepath.Join(enclosing, "operator-secret-name.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(enclosing, "session-workspaces", "s1")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	access := sessionAccessAt(t, AccessTrusted, root, true)
	requireSandboxedCommands(t, access, root)
	text := runtimeContextText(t, access)
	if !strings.Contains(text, "cwd: "+root+"\n") {
		t.Fatalf("runtime context does not name the session root %q:\n%s", root, text)
	}
	for _, leaked := range []string{"enclosing-branch", "operator-secret-name.txt", "git "} {
		if strings.Contains(text, leaked) {
			t.Fatalf("runtime context reports the enclosing repository (%q):\n%s", leaked, text)
		}
	}

	// A session root that is itself a repository reports its own state — read by
	// git running inside the session sandbox, never by the server process.
	gitRepo(t, root, "session-branch")
	text = runtimeContextText(t, access)
	if !strings.Contains(text, "git branch: session-branch\n") {
		t.Fatalf("runtime context omits the session root's own repository:\n%s", text)
	}
}

// gitRepo makes dir a repository on branch with one commit, so HEAD resolves.
func gitRepo(t *testing.T, dir, branch string) {
	t.Helper()
	for _, args := range [][]string{
		{"init", "-q", "-b", branch},
		{"-c", "user.name=t", "-c", "user.email=t@example.invalid", "-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", "seed"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
}
