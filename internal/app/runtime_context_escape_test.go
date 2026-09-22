package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The runtime-context provider must never execute git with the Carbon process's
// own authority in a directory the model can write. Git honours repository-local
// configuration that runs commands (core.fsmonitor, filter.<x>.clean via
// .gitattributes, and more that cannot be enumerated), so a model that plants
// such configuration in its writable workspace would otherwise have its command
// run unconfined by the host on the next turn. Each probe plants a command that
// touches a marker OUTSIDE the workspace and outside any sandbox-writable path;
// the marker must never appear.

type gitPlant struct {
	name  string
	plant func(t *testing.T, repo, marker string)
}

func gitPlants() []gitPlant {
	return []gitPlant{
		{name: "core.fsmonitor", plant: plantFSMonitor},
		{name: "clean filter", plant: plantCleanFilter},
	}
}

// plantFSMonitor makes repo a repository whose core.fsmonitor hook touches
// marker. `git status` consults the monitor on every run.
func plantFSMonitor(t *testing.T, repo, marker string) {
	t.Helper()
	gitRepo(t, repo, "planted")
	runGit(t, repo, "config", "core.fsmonitor", "touch '"+marker+"'; false")
}

// plantCleanFilter makes repo a repository with a tracked, modified *.txt file
// whose .gitattributes clean filter touches marker. `git status` runs the clean
// filter to compare a racily-modified file's content with the index.
func plantCleanFilter(t *testing.T, repo, marker string) {
	t.Helper()
	gitRepo(t, repo, "planted")
	runGit(t, repo, "config", "filter.x.clean", "sh -c 'touch \""+marker+"\"; cat'")
	if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("*.txt filter=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "notes.txt"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Commit with the filter disabled so the marker is not touched by setup.
	runGit(t, repo, "-c", "filter.x.clean=cat", "add", ".gitattributes", "notes.txt")
	runGit(t, repo, "-c", "filter.x.clean=cat", "-c", "user.name=t", "-c", "user.email=t@example.invalid",
		"-c", "commit.gpgsign=false", "commit", "-q", "-m", "track")
	if err := os.WriteFile(filepath.Join(repo, "notes.txt"), []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("setup itself ran the planted filter")
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

// escapeMarker returns a path in a fresh directory that is neither inside the
// workspace nor writable by a Trusted-profile sandbox (host writes are gated).
func escapeMarker(t *testing.T) string {
	t.Helper()
	return filepath.Join(canonicalTempDir(t), "host-executed-marker")
}

func assertNotHostExecuted(t *testing.T, marker string) {
	t.Helper()
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("runtime context executed tenant-planted git configuration with host authority (marker %s exists)", marker)
	}
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
}

// sessionAccessAt builds the real session access wiring (executor set, gate)
// for profile over root, as composition does; pooled marks it the way the pooled
// launcher does. It is closed at test end.
func sessionAccessAt(t *testing.T, profile AccessProfile, root string, pooled bool) *sessionAccess {
	t.Helper()
	access, err := buildHeadlessAccess(Config{AccessProfile: profile}, root)
	if err != nil {
		t.Fatalf("buildHeadlessAccess(%q): %v", profile, err)
	}
	t.Cleanup(func() { _ = access.Close() })
	access.sessionRootContext = pooled
	return access
}

// runtimeContextText renders the provider composition selects for access.
func runtimeContextText(t *testing.T, access *sessionAccess) string {
	t.Helper()
	return soleText(t, runtimeContextProviderFor(access).Blocks(context.Background()))
}

// requireSandboxedCommands skips when this host cannot run a command inside a
// Trusted session sandbox, so a positive "git state is still reported" check is
// not mistaken for a regression on a host without sandbox support. The escape
// probes never skip on this: omitting git is a safe outcome for them.
func requireSandboxedCommands(t *testing.T, access *sessionAccess, dir string) {
	t.Helper()
	executor, err := access.set.For(runtimeContextExecutorKey)
	if err != nil {
		t.Skipf("session sandbox unavailable: %v", err)
	}
	if _, code, err := executor.RunArgv(context.Background(), dir, []string{"git", "--version"}); err != nil || code != 0 {
		t.Skipf("cannot run git inside the session sandbox on this host: code=%d err=%v", code, err)
	}
}

// Pooled: the session root is the model's writable workspace.
func TestPooledRuntimeContextNeverRunsPlantedGitConfigWithHostAuthority(t *testing.T) {
	requireGit(t)
	for _, plant := range gitPlants() {
		t.Run(plant.name, func(t *testing.T) {
			root := canonicalTempDir(t)
			marker := escapeMarker(t)
			plant.plant(t, root, marker)
			_ = runtimeContextText(t, sessionAccessAt(t, AccessTrusted, root, true))
			assertNotHostExecuted(t, marker)
		})
	}
}

// TUI/headless: the operator's working directory is the model's writable
// workspace under the Trusted profile.
func TestProcessRuntimeContextNeverRunsPlantedGitConfigWithHostAuthority(t *testing.T) {
	requireGit(t)
	for _, plant := range gitPlants() {
		t.Run(plant.name, func(t *testing.T) {
			root := canonicalTempDir(t)
			marker := escapeMarker(t)
			plant.plant(t, root, marker)
			t.Chdir(root)
			_ = runtimeContextText(t, sessionAccessAt(t, AccessTrusted, root, false))
			assertNotHostExecuted(t, marker)
		})
	}
}

// The public, sandbox-less constructor reports no git state at all rather than
// falling back to running git with the process's authority.
func TestSandboxlessRuntimeContextNeverRunsGit(t *testing.T) {
	requireGit(t)
	for _, plant := range gitPlants() {
		t.Run(plant.name, func(t *testing.T) {
			root := canonicalTempDir(t)
			marker := escapeMarker(t)
			plant.plant(t, root, marker)
			t.Chdir(root)
			text := soleText(t, NewRuntimeContextProvider().Blocks(context.Background()))
			assertNotHostExecuted(t, marker)
			if strings.Contains(text, "git ") {
				t.Fatalf("sandbox-less provider reported git state:\n%s", text)
			}
		})
	}
}

// Git state is kept, not dropped: run inside the sandbox, a TUI/headless session
// still reports its branch and status, including an ENCLOSING repository when
// the process starts in a subdirectory (discovery is not ceilinged there).
func TestProcessRuntimeContextReportsGitStateFromInsideTheSandbox(t *testing.T) {
	requireGit(t)
	repo := canonicalTempDir(t)
	gitRepo(t, repo, "operator-branch")
	sub := filepath.Join(repo, "pkg")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "new.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)
	access := sessionAccessAt(t, AccessTrusted, sub, false)
	requireSandboxedCommands(t, access, sub)
	text := runtimeContextText(t, access)
	for _, want := range []string{"cwd: " + sub + "\n", "git branch: operator-branch\n", "git status: 1 changed"} {
		if !strings.Contains(text, want) {
			t.Fatalf("runtime context missing %q:\n%s", want, text)
		}
	}
}

// A profile whose commands are gated (ReadOnly) cannot run the probe without a
// grant, so git state is omitted rather than run outside the sandbox.
func TestReadOnlyRuntimeContextOmitsGitState(t *testing.T) {
	requireGit(t)
	repo := canonicalTempDir(t)
	gitRepo(t, repo, "readonly-branch")
	t.Chdir(repo)
	text := runtimeContextText(t, sessionAccessAt(t, AccessReadOnly, repo, false))
	if strings.Contains(text, "git ") {
		t.Fatalf("ReadOnly runtime context reported git state:\n%s", text)
	}
	if !strings.Contains(text, "cwd: "+repo+"\n") {
		t.Fatalf("ReadOnly runtime context lost its cwd:\n%s", text)
	}
}

// TUI and headless composition never take the pooled session-root context: they
// are told the process working directory and still discover an enclosing
// repository. Only the pooled launcher sets sessionRootContext.
func TestTUIAndHeadlessAccessKeepProcessRuntimeContext(t *testing.T) {
	requireGit(t)
	t.Setenv("HOME", t.TempDir()) // interactive access derives its permission store from HOME
	repo := canonicalTempDir(t)
	gitRepo(t, repo, "operator-branch")
	sub := filepath.Join(repo, "cmd")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)
	builds := map[string]func() (*sessionAccess, error){
		"headless": func() (*sessionAccess, error) { return buildHeadlessAccess(Config{AccessProfile: AccessTrusted}, sub) },
		"tui": func() (*sessionAccess, error) {
			return buildSessionAccess(Config{AccessProfile: AccessTrusted}, sub, true)
		},
	}
	for name, build := range builds {
		t.Run(name, func(t *testing.T) {
			access, err := build()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = access.Close() })
			if access.sessionRootContext {
				t.Fatal("TUI/headless access carries the pooled session-root runtime context")
			}
			requireSandboxedCommands(t, access, sub)
			text := runtimeContextText(t, access)
			for _, want := range []string{"cwd: " + sub + "\n", "git branch: operator-branch\n"} {
				if !strings.Contains(text, want) {
					t.Fatalf("runtime context missing %q:\n%s", want, text)
				}
			}
		})
	}
}
