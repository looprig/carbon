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
// touches a marker OUTSIDE the workspace and outside any sandbox-writable path
// (the marker must never appear, proving confinement) and ALSO touches a
// second marker INSIDE the workspace (proving the hook actually ran at all,
// so the outside-marker-absent assertion is not vacuously satisfied by git
// never invoking the hook in the first place — see O5 in
// docs/plans/2026-08-29-factory-host-orchestration-implementation/CLAUDE_REVIEW_CARBON_R1.5_REGATE.md).

type gitPlant struct {
	name  string
	plant func(t *testing.T, repo, marker, insideMarker string)
}

func gitPlants() []gitPlant {
	return []gitPlant{
		{name: "core.fsmonitor", plant: plantFSMonitor},
		{name: "clean filter", plant: plantCleanFilter},
	}
}

// plantFSMonitor makes repo a repository whose core.fsmonitor hook touches
// insideMarker then marker. `git status` consults the monitor on every run.
func plantFSMonitor(t *testing.T, repo, marker, insideMarker string) {
	t.Helper()
	gitRepo(t, repo, "planted")
	runGit(t, repo, "config", "core.fsmonitor", "touch '"+insideMarker+"'; touch '"+marker+"'; false")
}

// plantCleanFilter makes repo a repository with a tracked, modified *.txt file
// whose .gitattributes clean filter touches insideMarker then marker. `git
// status` runs the clean filter to compare a racily-modified file's content
// with the index.
func plantCleanFilter(t *testing.T, repo, marker, insideMarker string) {
	t.Helper()
	gitRepo(t, repo, "planted")
	runGit(t, repo, "config", "filter.x.clean", "sh -c 'touch \""+insideMarker+"\"; touch \""+marker+"\"; cat'")
	if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("*.txt filter=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "notes.txt"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Commit with the filter disabled so the markers are not touched by setup.
	runGit(t, repo, "-c", "filter.x.clean=cat", "add", ".gitattributes", "notes.txt")
	runGit(t, repo, "-c", "filter.x.clean=cat", "-c", "user.name=t", "-c", "user.email=t@example.invalid",
		"-c", "commit.gpgsign=false", "commit", "-q", "-m", "track")
	if err := os.WriteFile(filepath.Join(repo, "notes.txt"), []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("setup itself ran the planted filter")
	}
	if _, err := os.Stat(insideMarker); err == nil {
		t.Fatal("setup itself ran the planted filter (inside marker)")
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

// insideMarker returns a path inside root that a planted hook touches to
// prove it ran at all, regardless of whether it ran confined or escaped.
func insideMarker(root string) string {
	return filepath.Join(root, ".carbon-hook-ran-marker")
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

// requireSandboxGitEnv is the CI escape hatch for the "sandbox cannot run git
// on this host" skip: set to "1" on any CI lane whose sandbox is known to be
// able to run git (so the skip would otherwise silently hide a real
// regression, per O5 in CLAUDE_REVIEW_CARBON_R1.5_REGATE.md).
const requireSandboxGitEnv = "CARBON_REQUIRE_SANDBOX_GIT"

// sandboxGitAvailable reports whether `git --version` runs successfully
// inside the session sandbox for access, without skipping or failing — the
// caller decides what an unavailable sandbox means for its own assertions.
func sandboxGitAvailable(access *sessionAccess, dir string) (ok bool, reason string) {
	executor, err := access.set.For(runtimeContextExecutorKey)
	if err != nil {
		return false, "session sandbox unavailable: " + err.Error()
	}
	if _, code, err := executor.RunArgv(context.Background(), dir, []string{"git", "--version"}); err != nil || code != 0 {
		return false, "cannot run git inside the session sandbox on this host"
	}
	return true, ""
}

// skipOrFailUnlessRequired is the shared skip/fail decision for "this host's
// sandbox cannot run git": an ordinary developer machine gets an explicit,
// clearly-reasoned skip, while any CI lane that sets
// CARBON_REQUIRE_SANDBOX_GIT=1 (because its sandbox is known to support git)
// treats the same condition as a failure instead, so the underlying
// discrimination this probe carries is never silently lost on that lane.
func skipOrFailUnlessRequired(t *testing.T, reason string) {
	t.Helper()
	if os.Getenv(requireSandboxGitEnv) == "1" {
		t.Fatalf("%s (failing rather than skipping because %s=1)", reason, requireSandboxGitEnv)
	}
	t.Skipf("%s (set %s=1 in CI to fail instead of skip)", reason, requireSandboxGitEnv)
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
// not mistaken for a regression on a host without sandbox support. On a CI lane
// known to support sandboxed git, set CARBON_REQUIRE_SANDBOX_GIT=1 so the same
// condition fails loudly instead of skipping silently.
func requireSandboxedCommands(t *testing.T, access *sessionAccess, dir string) {
	t.Helper()
	if ok, reason := sandboxGitAvailable(access, dir); !ok {
		skipOrFailUnlessRequired(t, reason)
	}
}

// Pooled: the session root is the model's writable workspace.
func TestPooledRuntimeContextNeverRunsPlantedGitConfigWithHostAuthority(t *testing.T) {
	requireGit(t)
	for _, plant := range gitPlants() {
		t.Run(plant.name, func(t *testing.T) {
			root := canonicalTempDir(t)
			marker := escapeMarker(t)
			inside := insideMarker(root)
			plant.plant(t, root, marker, inside)
			access := sessionAccessAt(t, AccessTrusted, root, true)
			_ = runtimeContextText(t, access)
			// The escape assertion below is unconditional: it never skips,
			// because omitting git entirely is itself a safe (if weaker)
			// outcome for it. The inside-marker assertion that follows is what
			// proves the hook actually ran, so this test is not vacuous; it
			// can only be checked where the sandbox can run git at all.
			assertNotHostExecuted(t, marker)
			if ok, reason := sandboxGitAvailable(access, root); !ok {
				skipOrFailUnlessRequired(t, reason+"; cannot prove the planted hook ran (inside marker check skipped) though the outside-marker check above still ran and found no escape")
			}
			if _, err := os.Stat(inside); err != nil {
				t.Fatalf("planted git hook never ran inside the sandbox (inside marker %s missing); the outside-marker-absent assertion above may be vacuous", inside)
			}
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
			inside := insideMarker(root)
			plant.plant(t, root, marker, inside)
			t.Chdir(root)
			access := sessionAccessAt(t, AccessTrusted, root, false)
			_ = runtimeContextText(t, access)
			assertNotHostExecuted(t, marker)
			if ok, reason := sandboxGitAvailable(access, root); !ok {
				skipOrFailUnlessRequired(t, reason+"; cannot prove the planted hook ran (inside marker check skipped) though the outside-marker check above still ran and found no escape")
			}
			if _, err := os.Stat(inside); err != nil {
				t.Fatalf("planted git hook never ran inside the sandbox (inside marker %s missing); the outside-marker-absent assertion above may be vacuous", inside)
			}
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
			inside := insideMarker(root)
			plant.plant(t, root, marker, inside)
			t.Chdir(root)
			text := soleText(t, NewRuntimeContextProvider().Blocks(context.Background()))
			assertNotHostExecuted(t, marker)
			// Neither marker should exist: the sandbox-less provider invokes
			// no git at all, so it cannot even confine the hook — it must
			// never call it.
			assertNotHostExecuted(t, inside)
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
