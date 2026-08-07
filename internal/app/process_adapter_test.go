package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/sandbox"
)

// process_adapter_test.go proves process_adapter.go's mechanical Sandbox<->
// Harness type mapping, per Task 26B Step 1's contract list. It deliberately
// exercises REAL *sandbox.Executor/ExecutorSet values (never a fake or an
// injected interface seam) so grant/access/error assertions are checked
// against Sandbox's own real verification logic as the oracle -- exactly
// what "without CodeRig parsing opaque grants" means in practice.

// --- test helpers -----------------------------------------------------------

// unconfinedExecutorSet builds a real ExecutorSet under CodeRig's Unconfined
// access profile: Command/Workspace/HostRead/HostWrite/Network are all
// Allow, so PrepareProcess needs no grants and Start actually spawns and
// supervises a real process on every platform, including Darwin -- where a
// Sandboxed (Seatbelt-confined) executor's Start fails closed with
// ErrLifetimeContainmentUnavailable today (SPEC Task 12c: no kernel-enforced
// Darwin containment primitive yet; Sandbox's own facade_test.go documents
// and asserts exactly this darwin-only gap). These tests exercise this
// adapter's OWN mechanical mapping, not Sandbox's platform-specific
// containment coverage (already Sandbox's own responsibility), so Unconfined
// is the correct, portable choice for exercising a real Start/Wait/Signal/
// Resize/Close lifecycle from this package.
func unconfinedExecutorSet(t *testing.T, root string, max int) *sandbox.ExecutorSet {
	t.Helper()
	profile, err := coderigProfile(AccessUnconfined, root)
	if err != nil {
		t.Fatalf("coderigProfile(unconfined): %v", err)
	}
	set, err := sandbox.NewExecutorSet(profile, sandbox.WithScratchRoot(t.TempDir()), sandbox.WithMaxExecutors(max))
	if err != nil {
		t.Fatalf("NewExecutorSet: %v", err)
	}
	t.Cleanup(func() { _ = set.Close() })
	return set
}

func unconfinedExecutor(t *testing.T, root string) *sandbox.Executor {
	t.Helper()
	set := unconfinedExecutorSet(t, root, 4)
	executor, err := set.For("adapter-test")
	if err != nil {
		t.Fatalf("ExecutorSet.For: %v", err)
	}
	return executor
}

// rawProfile builds a Sandboxed profile with precise, independently
// controlled WorkspaceWrite and Command authority -- used by tests that need
// a specific ProcessAccessKind or a specific Command Gated/Allow posture,
// which none of CodeRig's three named product profiles expose independently.
func rawProfile(t *testing.T, root string, workspaceWrite, command sandbox.Access) *sandbox.Profile {
	t.Helper()
	profile, err := sandbox.NewProfile(sandbox.ProfileConfig{
		WorkspaceRoot:  root,
		Home:           sandbox.IsolatedHome,
		Isolation:      sandbox.Sandboxed,
		WorkspaceRead:  sandbox.Allow,
		WorkspaceWrite: workspaceWrite,
		HostRead:       sandbox.Deny,
		HostWrite:      sandbox.Deny,
		Network:        sandbox.Deny,
		Command:        command,
	})
	if err != nil {
		t.Fatalf("NewProfile: %v", err)
	}
	return profile
}

func newUUID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New: %v", err)
	}
	return id
}

func issueGrant(t *testing.T, executor *sandbox.Executor, executionID, command, cwd, kind, scope, class, target string) string {
	t.Helper()
	grant, err := executor.IssueGrant(context.Background(), executionID, command, cwd, kind, scope, class, target, time.Now().Add(time.Minute).UnixMilli())
	if err != nil {
		t.Fatalf("IssueGrant(%s): %v", class, err)
	}
	return grant
}

func waitProcess(t *testing.T, proc tool.Process) tool.ProcessResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := proc.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	return result
}

// --- Step 1: "Harness request maps every field exactly once to Sandbox
// options" ; "no Coderig parsing opaque grants" for Command/Directory/PTY ---

// TestProcessAdapterMapProcessRequestFieldsExactly is a pure, direct
// field-equality check of mapProcessRequest against every
// tool.ProcessRequest field, including Deadline: nothing downstream
// (Sandbox's own PrepareProcess, nor anything in the tools package) reads
// or enforces ProcessOptions.Deadline today, so there is no behavioral
// effect to observe end-to-end -- this test instead proves the field is
// threaded through to the constructed sandbox.ProcessOptions value
// unmodified, so a future regression is caught even before anything
// downstream starts enforcing it.
func TestProcessAdapterMapProcessRequestFieldsExactly(t *testing.T) {
	t.Parallel()
	execID := newUUID(t)
	deadline := time.Now().Add(5 * time.Minute)
	req := tool.ProcessRequest{
		Command:           "true",
		Directory:         "/some/dir",
		Grants:            []string{"grant-a", "grant-b"},
		OriginExecutionID: execID,
		Deadline:          deadline,
		PTY:               true,
	}
	got := mapProcessRequest(req)
	want := sandbox.ProcessOptions{
		Command:     "true",
		Directory:   "/some/dir",
		ExecutionID: execID.String(),
		Grants:      []string{"grant-a", "grant-b"},
		TTY:         true,
		Deadline:    deadline,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mapProcessRequest = %+v, want %+v", got, want)
	}
	if !got.Deadline.Equal(deadline) {
		t.Fatalf("mapProcessRequest Deadline = %v, want %v", got.Deadline, deadline)
	}
}

func TestProcessAdapterCommandAndDirectoryMapExactly(t *testing.T) {
	t.Parallel()
	root := canonicalTempDir(t)
	executor := unconfinedExecutor(t, root)
	adapter := processRunnerAdapter{exec: executor}
	ctx := context.Background()

	// Directory: the child writes a marker file with a RELATIVE path, which
	// only lands at filepath.Join(root, "marker.txt") if Sandbox actually
	// received root as its own working directory -- not this test's own
	// process cwd.
	prepared, err := adapter.PrepareProcess(ctx, tool.ProcessRequest{
		Command:           "printf %s mapped > marker.txt && exit 7",
		Directory:         root,
		OriginExecutionID: newUUID(t),
	})
	if err != nil {
		t.Fatalf("PrepareProcess: %v", err)
	}
	proc, err := prepared.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	result := waitProcess(t, proc)
	_ = proc.Close(ctx)

	// Command: the exit code proves the exact command string ran, not some
	// other/garbled one.
	if result.ExitCode != 7 {
		t.Fatalf("ExitCode = %d, want 7 (Command not mapped exactly)", result.ExitCode)
	}
	marker := filepath.Join(root, "marker.txt")
	body, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("marker file at Directory %q not found: %v (Directory not mapped exactly)", root, err)
	}
	if string(body) != "mapped" {
		t.Fatalf("marker file contents = %q, want %q", body, "mapped")
	}
}

func TestProcessAdapterGrantsAndOriginExecutionIDPreserved(t *testing.T) {
	t.Parallel()
	root := canonicalTempDir(t)
	profile := rawProfile(t, root, sandbox.Deny, sandbox.Gated)
	set, err := sandbox.NewExecutorSet(profile, sandbox.WithScratchRoot(t.TempDir()), sandbox.WithMaxExecutors(1))
	if err != nil {
		t.Fatalf("NewExecutorSet: %v", err)
	}
	t.Cleanup(func() { _ = set.Close() })
	executor, err := set.For("grant-preserve")
	if err != nil {
		t.Fatalf("ExecutorSet.For: %v", err)
	}
	adapter := processRunnerAdapter{exec: executor}

	const command = "true"
	boundExecID := newUUID(t)

	// Each subtest mints its OWN fresh grant token: Sandbox's grant
	// verification consumes (marks used) a token on its first successful
	// PrepareProcess, and rejects any later reuse with ErrGrantReplay before
	// it ever reaches execution-ID/command binding checks -- reusing one
	// token across subtests would make "wrong origin execution id" observe
	// replay, not the binding mismatch this subtest is actually proving.
	freshGrant := func(t *testing.T) string {
		t.Helper()
		return issueGrant(t, executor, boundExecID.String(), command, root, "command.execute", "", sandbox.GrantClassCommandStart, command)
	}

	t.Run("matching grant and execution id succeeds", func(t *testing.T) {
		prepared, err := adapter.PrepareProcess(context.Background(), tool.ProcessRequest{
			Command: command, Directory: root, Grants: []string{freshGrant(t)}, OriginExecutionID: boundExecID,
		})
		if err != nil {
			t.Fatalf("PrepareProcess with matching grant/execution id: %v", err)
		}
		_ = prepared.Close()
	})

	t.Run("wrong origin execution id is rejected", func(t *testing.T) {
		wrongExecID := newUUID(t)
		_, err := adapter.PrepareProcess(context.Background(), tool.ProcessRequest{
			Command: command, Directory: root, Grants: []string{freshGrant(t)}, OriginExecutionID: wrongExecID,
		})
		if err == nil {
			t.Fatal("PrepareProcess with mismatched execution id unexpectedly succeeded")
		}
		if !errors.Is(err, sandbox.ErrGrantWrongExecution) {
			t.Fatalf("error = %v, want to unwrap sandbox.ErrGrantWrongExecution (proves OriginExecutionID reached Sandbox unmodified)", err)
		}
	})

	t.Run("grants forwarded opaquely, not parsed or dropped", func(t *testing.T) {
		_, err := adapter.PrepareProcess(context.Background(), tool.ProcessRequest{
			Command: command, Directory: root, Grants: []string{"not-a-real-grant-token"}, OriginExecutionID: boundExecID,
		})
		if err == nil {
			t.Fatal("PrepareProcess with a garbage grant token unexpectedly succeeded")
		}
		if errors.Is(err, sandbox.ErrGrantRequired) {
			t.Fatal("garbage grant treated as if no grant were supplied at all -- CodeRig must forward Grants verbatim, not drop or filter them")
		}
	})
}

func TestProcessAdapterPTYFieldMapsToStreamMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY coverage is Unix-only for this test")
	}
	t.Parallel()
	root := canonicalTempDir(t)

	t.Run("pipes", func(t *testing.T) {
		t.Parallel()
		executor := unconfinedExecutor(t, root)
		adapter := processRunnerAdapter{exec: executor}
		ctx := context.Background()
		prepared, err := adapter.PrepareProcess(ctx, tool.ProcessRequest{
			Command: "echo pipes-mode 1>&2", Directory: root, OriginExecutionID: newUUID(t), PTY: false,
		})
		if err != nil {
			t.Fatalf("PrepareProcess: %v", err)
		}
		proc, err := prepared.Start(ctx)
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		if got := proc.StreamMode(); got != tool.ProcessStreamModePipes {
			t.Fatalf("StreamMode = %v, want Pipes", got)
		}
		errOut, err := io.ReadAll(proc.Stderr())
		if err != nil {
			t.Fatalf("ReadAll(Stderr): %v", err)
		}
		if !bytes.Contains(errOut, []byte("pipes-mode")) {
			t.Fatalf("Stderr = %q, want a live pipe carrying pipes-mode", errOut)
		}
		waitProcess(t, proc)
		_ = proc.Close(ctx)
	})

	t.Run("pty", func(t *testing.T) {
		t.Parallel()
		executor := unconfinedExecutor(t, root)
		adapter := processRunnerAdapter{exec: executor}
		ctx := context.Background()
		prepared, err := adapter.PrepareProcess(ctx, tool.ProcessRequest{
			Command: "true", Directory: root, OriginExecutionID: newUUID(t), PTY: true,
		})
		if err != nil {
			t.Fatalf("PrepareProcess: %v", err)
		}
		proc, err := prepared.Start(ctx)
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		if got := proc.StreamMode(); got != tool.ProcessStreamModePTY {
			t.Fatalf("StreamMode = %v, want PTY", got)
		}
		// Sandbox's own PTY-mode Process contract: Stderr is a synthetic,
		// permanently-empty, ALREADY-CLOSED reader (never a second live
		// pipe). Observing that end-to-end here is what actually proves
		// req.PTY reached Sandbox's real ProcessOptions.TTY: this adapter's
		// own StreamMode() above only echoes back its own stored copy of
		// req.PTY (see processAdapter's doc comment for why it must), so it
		// alone cannot catch a bug that failed to forward PTY into
		// sandbox.ProcessOptions.
		errOut, err := io.ReadAll(proc.Stderr())
		if err != nil {
			t.Fatalf("ReadAll(Stderr): %v", err)
		}
		if len(errOut) != 0 {
			t.Fatalf("PTY-mode Stderr = %q, want empty (Sandbox's synthetic closed reader)", errOut)
		}
		waitProcess(t, proc)
		_ = proc.Close(ctx)
	})
}

// --- Step 1: "Sandbox prepared-process access maps exactly to Harness
// access without Coderig parsing opaque grants" -----------------------------

func TestProcessAdapterPreparedAccessKindMapsExactly(t *testing.T) {
	t.Parallel()
	root := canonicalTempDir(t)

	for _, tc := range []struct {
		name  string
		write sandbox.Access
		want  tool.WorkspaceAccessKind
	}{
		{"read-only", sandbox.Deny, tool.WorkspaceAccessReadOnly},
		{"scoped-write", sandbox.Gated, tool.WorkspaceAccessScopedWrite},
		{"broad-write", sandbox.Allow, tool.WorkspaceAccessBroadWrite},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			profile := rawProfile(t, root, tc.write, sandbox.Allow)
			set, err := sandbox.NewExecutorSet(profile, sandbox.WithScratchRoot(t.TempDir()), sandbox.WithMaxExecutors(1))
			if err != nil {
				t.Fatalf("NewExecutorSet: %v", err)
			}
			t.Cleanup(func() { _ = set.Close() })
			executor, err := set.For("access-kind")
			if err != nil {
				t.Fatalf("ExecutorSet.For: %v", err)
			}
			adapter := processRunnerAdapter{exec: executor}
			prepared, err := adapter.PrepareProcess(context.Background(), tool.ProcessRequest{
				Command: "true", Directory: root, OriginExecutionID: newUUID(t),
			})
			if err != nil {
				t.Fatalf("PrepareProcess: %v", err)
			}
			defer func() { _ = prepared.Close() }()
			access := prepared.EffectiveWorkspaceAccess()
			if access.Kind != tc.want {
				t.Fatalf("Kind = %v, want %v", access.Kind, tc.want)
			}
		})
	}
}

func TestProcessAdapterPreparedAccessCarriesGrantedWritePaths(t *testing.T) {
	t.Parallel()
	root := canonicalTempDir(t)
	profile := rawProfile(t, root, sandbox.Gated, sandbox.Allow)
	set, err := sandbox.NewExecutorSet(profile, sandbox.WithScratchRoot(t.TempDir()), sandbox.WithMaxExecutors(1))
	if err != nil {
		t.Fatalf("NewExecutorSet: %v", err)
	}
	t.Cleanup(func() { _ = set.Close() })
	executor, err := set.For("scoped-write")
	if err != nil {
		t.Fatalf("ExecutorSet.For: %v", err)
	}
	adapter := processRunnerAdapter{exec: executor}

	writeTarget := filepath.Join(root, "scoped-write-target.txt")
	execID := newUUID(t)
	grant := issueGrant(t, executor, execID.String(), "true", root, "filesystem.write", writeTarget, sandbox.GrantClassFilesystemPathWrite, writeTarget)

	prepared, err := adapter.PrepareProcess(context.Background(), tool.ProcessRequest{
		Command: "true", Directory: root, Grants: []string{grant}, OriginExecutionID: execID,
	})
	if err != nil {
		t.Fatalf("PrepareProcess: %v", err)
	}
	defer func() { _ = prepared.Close() }()

	access := prepared.EffectiveWorkspaceAccess()
	if access.Kind != tool.WorkspaceAccessScopedWrite {
		t.Fatalf("Kind = %v, want ScopedWrite", access.Kind)
	}
	paths := access.WritePaths()
	if len(paths) != 1 || paths[0] != writeTarget {
		t.Fatalf("WritePaths = %v, want [%s]", paths, writeTarget)
	}
}

func TestProcessAdapterMapAccessKindFailsClosedOnUnrecognizedKind(t *testing.T) {
	t.Parallel()
	if got := mapAccessKind(sandbox.ProcessAccessKind(99)); got != tool.WorkspaceAccessReadOnly {
		t.Fatalf("mapAccessKind(unrecognized) = %v, want ReadOnly (fail closed)", got)
	}
}

// --- Step 1: "Harness lease acquisition occurs between adapter prepare and
// start" ----------------------------------------------------------------

func TestProcessAdapterEffectiveAccessAvailableBetweenPrepareAndStart(t *testing.T) {
	t.Parallel()
	root := canonicalTempDir(t)
	executor := unconfinedExecutor(t, root)
	adapter := processRunnerAdapter{exec: executor}
	ctx := context.Background()

	prepared, err := adapter.PrepareProcess(ctx, tool.ProcessRequest{
		Command: "true", Directory: root, OriginExecutionID: newUUID(t),
	})
	if err != nil {
		t.Fatalf("PrepareProcess: %v", err)
	}

	// Read EffectiveWorkspaceAccess() repeatedly BEFORE Start -- exactly the
	// window a caller (Harness's WorkspaceLifetimeCoordinator, driven by
	// tools/bash's supervised.go) uses to acquire its own workspace
	// lifetime lease -- and confirm it is stable and does not itself
	// trigger a spawn or otherwise consume the preparation.
	first := prepared.EffectiveWorkspaceAccess()
	second := prepared.EffectiveWorkspaceAccess()
	if first.Kind != second.Kind ||
		!reflect.DeepEqual(first.WritePaths(), second.WritePaths()) ||
		!reflect.DeepEqual(first.WriteTrees(), second.WriteTrees()) {
		t.Fatalf("EffectiveWorkspaceAccess unstable across repeated pre-Start calls: %+v vs %+v", first, second)
	}

	proc, err := prepared.Start(ctx)
	if err != nil {
		t.Fatalf("Start (after repeated pre-Start access reads): %v", err)
	}
	waitProcess(t, proc)
	_ = proc.Close(ctx)
}

// --- Step 1: "prepared close/start single-use semantics map exactly" ------

func TestProcessAdapterPreparedSingleUse(t *testing.T) {
	t.Parallel()
	root := canonicalTempDir(t)

	t.Run("second start fails", func(t *testing.T) {
		t.Parallel()
		executor := unconfinedExecutor(t, root)
		adapter := processRunnerAdapter{exec: executor}
		ctx := context.Background()
		prepared, err := adapter.PrepareProcess(ctx, tool.ProcessRequest{Command: "true", Directory: root, OriginExecutionID: newUUID(t)})
		if err != nil {
			t.Fatalf("PrepareProcess: %v", err)
		}
		proc, err := prepared.Start(ctx)
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		waitProcess(t, proc)
		defer func() { _ = proc.Close(ctx) }()

		if _, err := prepared.Start(ctx); err == nil {
			t.Fatal("second Start unexpectedly succeeded")
		} else if !errors.Is(err, sandbox.ErrProcessAlreadyStarted) {
			t.Fatalf("second Start error = %v, want to unwrap sandbox.ErrProcessAlreadyStarted", err)
		}
	})

	t.Run("start after close fails", func(t *testing.T) {
		t.Parallel()
		executor := unconfinedExecutor(t, root)
		adapter := processRunnerAdapter{exec: executor}
		ctx := context.Background()
		prepared, err := adapter.PrepareProcess(ctx, tool.ProcessRequest{Command: "true", Directory: root, OriginExecutionID: newUUID(t)})
		if err != nil {
			t.Fatalf("PrepareProcess: %v", err)
		}
		if err := prepared.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if _, err := prepared.Start(ctx); err == nil {
			t.Fatal("Start after Close unexpectedly succeeded")
		} else if !errors.Is(err, sandbox.ErrProcessClosed) {
			t.Fatalf("Start-after-Close error = %v, want to unwrap sandbox.ErrProcessClosed", err)
		}
	})

	t.Run("close after start is a harmless no-op", func(t *testing.T) {
		t.Parallel()
		executor := unconfinedExecutor(t, root)
		adapter := processRunnerAdapter{exec: executor}
		ctx := context.Background()
		prepared, err := adapter.PrepareProcess(ctx, tool.ProcessRequest{Command: "true", Directory: root, OriginExecutionID: newUUID(t)})
		if err != nil {
			t.Fatalf("PrepareProcess: %v", err)
		}
		proc, err := prepared.Start(ctx)
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		waitProcess(t, proc)
		defer func() { _ = proc.Close(ctx) }()

		if err := prepared.Close(); err != nil {
			t.Fatalf("post-Start Close = %v, want nil (harmless no-op)", err)
		}
		if err := prepared.Close(); err != nil {
			t.Fatalf("second post-Start Close = %v, want nil (idempotent)", err)
		}
	})
}

// --- Step 1: "Sandbox process streams/wait/resize/signal/close map
// exactly" ------------------------------------------------------------------

func TestProcessAdapterProcessStreamsWaitCloseMapExactly(t *testing.T) {
	t.Parallel()
	root := canonicalTempDir(t)
	executor := unconfinedExecutor(t, root)
	adapter := processRunnerAdapter{exec: executor}
	ctx := context.Background()

	prepared, err := adapter.PrepareProcess(ctx, tool.ProcessRequest{
		Command: "cat", Directory: root, OriginExecutionID: newUUID(t),
	})
	if err != nil {
		t.Fatalf("PrepareProcess: %v", err)
	}
	proc, err := prepared.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	const payload = "process-adapter-stream-roundtrip"
	if _, err := proc.Stdin().Write([]byte(payload)); err != nil {
		t.Fatalf("Stdin.Write: %v", err)
	}
	if err := proc.Stdin().Close(); err != nil {
		t.Fatalf("Stdin.Close: %v", err)
	}

	result := waitProcess(t, proc)
	if result.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", result.ExitCode)
	}
	if result.Reason != tool.ProcessTerminalExited {
		t.Fatalf("Reason = %v, want ProcessTerminalExited", result.Reason)
	}
	if result.StartedAt.IsZero() || result.FinishedAt.Before(result.StartedAt) {
		t.Fatalf("StartedAt/FinishedAt = %v/%v, want populated and ordered", result.StartedAt, result.FinishedAt)
	}

	out, err := io.ReadAll(proc.Stdout())
	if err != nil {
		t.Fatalf("ReadAll(Stdout): %v", err)
	}
	if string(out) != payload {
		t.Fatalf("Stdout = %q, want %q", out, payload)
	}

	if err := proc.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := proc.Close(ctx); err != nil {
		t.Fatalf("Close (idempotent): %v", err)
	}
}

func TestProcessAdapterResizePassesThrough(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY coverage is Unix-only for this test")
	}
	t.Parallel()
	root := canonicalTempDir(t)
	executor := unconfinedExecutor(t, root)
	adapter := processRunnerAdapter{exec: executor}
	ctx := context.Background()

	prepared, err := adapter.PrepareProcess(ctx, tool.ProcessRequest{
		Command: "sleep 1", Directory: root, OriginExecutionID: newUUID(t), PTY: true,
	})
	if err != nil {
		t.Fatalf("PrepareProcess: %v", err)
	}
	proc, err := prepared.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = proc.Close(ctx) }()

	if err := proc.Resize(ctx, 40, 120); err != nil {
		t.Fatalf("Resize on a live PTY process: %v", err)
	}
	waitProcess(t, proc)
}

// --- Step 1: "optional Sandbox activity values map exactly to Harness
// values, invalid activity broadens invalidation, and channel closure
// precedes wait" -------------------------------------------------------------

func TestProcessAdapterTranslateActivity(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		in   sandbox.ProcessActivityKind
		want tool.WorkspaceActivityKind
	}{
		{"write", sandbox.ProcessActivityWrite, tool.WorkspaceActivityWrite},
		{"broad write", sandbox.ProcessActivityBroadWrite, tool.WorkspaceActivityBroadWrite},
		{"invalid broadens", sandbox.ProcessActivityKind(99), tool.WorkspaceActivityBroadWrite},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := translateActivity(sandbox.ProcessActivity{Kind: tc.in})
			if got.EffectiveKind() != tc.want {
				t.Fatalf("EffectiveKind = %v, want %v", got.EffectiveKind(), tc.want)
			}
		})
	}
}

func TestProcessAdapterActivitiesChannelClosesBeforeWaitReturns(t *testing.T) {
	t.Parallel()
	root := canonicalTempDir(t)
	executor := unconfinedExecutor(t, root)
	adapter := processRunnerAdapter{exec: executor}
	ctx := context.Background()

	prepared, err := adapter.PrepareProcess(ctx, tool.ProcessRequest{
		Command: "true", Directory: root, OriginExecutionID: newUUID(t),
	})
	if err != nil {
		t.Fatalf("PrepareProcess: %v", err)
	}
	proc, err := prepared.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	source, ok := proc.(tool.ProcessActivitySource)
	if !ok {
		t.Fatal("processAdapter does not implement tool.ProcessActivitySource")
	}

	waitProcess(t, proc)

	// By the time a successful Wait has returned, this adapter's own
	// translated Activities channel is GUARANTEED already closed (Wait
	// blocks on activitiesEnd before returning on the success path -- see
	// processAdapter.Wait's doc comment) -- so a receive here must be
	// immediately ready with ok=false, never block.
	if _, stillOpen := <-source.Activities(); stillOpen {
		t.Fatal("Activities channel still open after Wait returned, want closed before Wait returns")
	}

	_ = proc.Close(ctx)
}

// --- Step 1: signal mapping and ProcessTerminalReason derivation -----------

func TestProcessAdapterSignalMapsAndDerivesReason(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signal-death semantics tested here are Unix-specific")
	}
	t.Parallel()
	root := canonicalTempDir(t)

	for _, tc := range []struct {
		name string
		kind tool.ProcessSignal
		want tool.ProcessTerminalReason
	}{
		{"interrupt", tool.ProcessSignalInterrupt, tool.ProcessTerminalInterrupted},
		{"terminate", tool.ProcessSignalTerminate, tool.ProcessTerminalTerminated},
		{"kill", tool.ProcessSignalKill, tool.ProcessTerminalKilled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			executor := unconfinedExecutor(t, root)
			adapter := processRunnerAdapter{exec: executor}
			ctx := context.Background()
			prepared, err := adapter.PrepareProcess(ctx, tool.ProcessRequest{
				Command: "sleep 30", Directory: root, OriginExecutionID: newUUID(t),
			})
			if err != nil {
				t.Fatalf("PrepareProcess: %v", err)
			}
			proc, err := prepared.Start(ctx)
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			defer func() { _ = proc.Close(ctx) }()

			if err := proc.Signal(ctx, tc.kind); err != nil {
				t.Fatalf("Signal(%v): %v", tc.kind, err)
			}
			result := waitProcess(t, proc)
			if result.ExitCode != -1 {
				t.Fatalf("ExitCode = %d, want -1 (Go's os/exec signal-death convention)", result.ExitCode)
			}
			if result.Reason != tc.want {
				t.Fatalf("Reason = %v, want %v", result.Reason, tc.want)
			}
		})
	}
}

func TestProcessAdapterUnattributedSignalDeathMapsToRunnerShutdown(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signal-death semantics tested here are Unix-specific")
	}
	t.Parallel()
	root := canonicalTempDir(t)
	profile, err := coderigProfile(AccessUnconfined, root)
	if err != nil {
		t.Fatalf("coderigProfile: %v", err)
	}
	set, err := sandbox.NewExecutorSet(profile, sandbox.WithScratchRoot(t.TempDir()), sandbox.WithMaxExecutors(1))
	if err != nil {
		t.Fatalf("NewExecutorSet: %v", err)
	}
	executor, err := set.For("shutdown-race")
	if err != nil {
		t.Fatalf("ExecutorSet.For: %v", err)
	}
	adapter := processRunnerAdapter{exec: executor}
	ctx := context.Background()

	prepared, err := adapter.PrepareProcess(ctx, tool.ProcessRequest{
		Command: "sleep 30", Directory: root, OriginExecutionID: newUUID(t),
	})
	if err != nil {
		t.Fatalf("PrepareProcess: %v", err)
	}
	proc, err := prepared.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	type outcome struct {
		result tool.ProcessResult
		err    error
	}
	resultCh := make(chan outcome, 1)
	go func() {
		waitCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		result, err := proc.Wait(waitCtx)
		resultCh <- outcome{result, err}
	}()

	// ExecutorSet.Close cancels its shared lifecycle.ctx, which reaches
	// every live lease -- including this already-STARTED process's, via
	// exec.CommandContext's own ctx-cancellation kill -- WITHOUT this
	// adapter's own Signal ever being called. Close blocks until the
	// process is confirmed reaped (executorLifecycle.wait), so by the time
	// it returns, resultCh is guaranteed to already have a value.
	if err := set.Close(); err != nil {
		t.Fatalf("ExecutorSet.Close: %v", err)
	}

	got := <-resultCh
	if got.err != nil {
		t.Fatalf("Wait: %v", got.err)
	}
	if got.result.ExitCode != -1 {
		t.Fatalf("ExitCode = %d, want -1 (signal death)", got.result.ExitCode)
	}
	if got.result.Reason != tool.ProcessTerminalRunnerShutdown {
		t.Fatalf("Reason = %v, want ProcessTerminalRunnerShutdown", got.result.Reason)
	}
	_ = proc.Close(ctx)
}

// TestProcessAdapterDeriveReasonExitCodeWinsOverSignalSeverity is a pure,
// direct unit test of deriveReason's central invariant -- doc-commented but,
// before this test, never directly exercised: deriveReason is a pure
// function of (exitCode, adapter.signalState), and ExitCode always wins.
// TestProcessAdapterSignalMapsAndDerivesReason (real process spawning) only
// ever exercises a process that DIES FROM the requested signal; this table
// additionally covers the doc comment's own worked example -- "a process
// that caught SIGTERM and chose to exit(0) exited; it was not killed" -- by
// pairing every recorded signal severity (including none) against both a
// signal-death exit code (-1) and non-signal-death exit codes (0 and a
// representative nonzero value), with no real process spawning at all.
func TestProcessAdapterDeriveReasonExitCodeWinsOverSignalSeverity(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		exitCode int
		severity signalSeverity
		want     tool.ProcessTerminalReason
	}{
		{"signal death, killed", -1, signalKilled, tool.ProcessTerminalKilled},
		{"signal death, terminated", -1, signalTerminated, tool.ProcessTerminalTerminated},
		{"signal death, interrupted", -1, signalInterrupted, tool.ProcessTerminalInterrupted},
		{"signal death, no recorded signal", -1, signalNone, tool.ProcessTerminalRunnerShutdown},
		{"clean exit 0 beats killed", 0, signalKilled, tool.ProcessTerminalExited},
		{"clean exit 0 beats terminated", 0, signalTerminated, tool.ProcessTerminalExited},
		{"clean exit 0 beats interrupted", 0, signalInterrupted, tool.ProcessTerminalExited},
		{"clean exit 0, no recorded signal", 0, signalNone, tool.ProcessTerminalExited},
		{"nonzero exit beats killed", 7, signalKilled, tool.ProcessTerminalExited},
		{"nonzero exit beats terminated", 130, signalTerminated, tool.ProcessTerminalExited},
		{"nonzero exit beats interrupted", 2, signalInterrupted, tool.ProcessTerminalExited},
		{"nonzero exit, no recorded signal", 1, signalNone, tool.ProcessTerminalExited},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := &processAdapter{}
			p.recordSignal(tc.severity)
			if got := p.deriveReason(tc.exitCode); got != tc.want {
				t.Fatalf("deriveReason(%d) with recorded severity %v = %v, want %v", tc.exitCode, tc.severity, got, tc.want)
			}
		})
	}
}

// TestProcessAdapterRecordSignalNeverDowngrades is a direct, real-signal-free
// unit test of recordSignal's own doc-commented guarantee: severity only
// ever escalates (kill dominates terminate dominates interrupt), regardless
// of call order.
func TestProcessAdapterRecordSignalNeverDowngrades(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		calls []signalSeverity
		want  signalSeverity
	}{
		{"kill then interrupt stays killed", []signalSeverity{signalKilled, signalInterrupted}, signalKilled},
		{"kill then terminate stays killed", []signalSeverity{signalKilled, signalTerminated}, signalKilled},
		{"terminate then interrupt stays terminated", []signalSeverity{signalTerminated, signalInterrupted}, signalTerminated},
		{"interrupt then terminate escalates", []signalSeverity{signalInterrupted, signalTerminated}, signalTerminated},
		{"terminate then kill escalates", []signalSeverity{signalTerminated, signalKilled}, signalKilled},
		{"repeated same severity stays put", []signalSeverity{signalTerminated, signalTerminated}, signalTerminated},
		{"interrupt then kill escalates", []signalSeverity{signalInterrupted, signalKilled}, signalKilled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := &processAdapter{}
			for _, sev := range tc.calls {
				p.recordSignal(sev)
			}
			if got := signalSeverity(p.signalState.Load()); got != tc.want {
				t.Fatalf("signalState after calls %v = %v, want %v", tc.calls, got, tc.want)
			}
		})
	}
}

func TestProcessAdapterMapProcessSignalRejectsUnknownKind(t *testing.T) {
	t.Parallel()
	if _, _, ok := mapProcessSignal(tool.ProcessSignal(99)); ok {
		t.Fatal("mapProcessSignal accepted an unrecognized signal kind")
	}
	for _, kind := range []tool.ProcessSignal{tool.ProcessSignalInterrupt, tool.ProcessSignalTerminate, tool.ProcessSignalKill} {
		if _, _, ok := mapProcessSignal(kind); !ok {
			t.Fatalf("mapProcessSignal(%v) rejected a valid, known kind", kind)
		}
	}
}

// --- Step 1: "Sandbox error codes map to Harness codes without losing
// causes" ---------------------------------------------------------------

func TestProcessAdapterErrorMapping(t *testing.T) {
	t.Parallel()
	generic := errors.New("boom")

	tests := []struct {
		name     string
		mapper   func(error) error
		err      error
		wantCode tool.ProcessErrorCode
	}{
		{"prepare/generic", mapPrepareProcessError, generic, tool.ProcessErrorSetupFailed},
		{"prepare/tty unsupported", mapPrepareProcessError, sandbox.ErrProcessTTYUnsupported, tool.ProcessErrorPTYUnavailable},
		{"prepare/conpty unavailable", mapPrepareProcessError, sandbox.ErrProcessConPTYUnavailable, tool.ProcessErrorPTYUnavailable},
		{"prepare/grant bad mac", mapPrepareProcessError, sandbox.ErrGrantBadMAC, tool.ProcessErrorSetupFailed},
		{"prepare/grant expired", mapPrepareProcessError, sandbox.ErrGrantExpired, tool.ProcessErrorSetupFailed},
		{"prepare/grant wrong command", mapPrepareProcessError, sandbox.ErrGrantWrongCommand, tool.ProcessErrorSetupFailed},
		{"prepare/grant wrong execution", mapPrepareProcessError, sandbox.ErrGrantWrongExecution, tool.ProcessErrorSetupFailed},
		{"prepare/grant wrong cwd", mapPrepareProcessError, sandbox.ErrGrantWrongWorkingDirectory, tool.ProcessErrorSetupFailed},
		{"prepare/grant profile mismatch", mapPrepareProcessError, sandbox.ErrGrantProfileMismatch, tool.ProcessErrorSetupFailed},
		{"prepare/grant guarantee mismatch", mapPrepareProcessError, sandbox.ErrGrantGuaranteeMismatch, tool.ProcessErrorSetupFailed},
		{"prepare/grant route mismatch", mapPrepareProcessError, sandbox.ErrGrantRouteMismatch, tool.ProcessErrorSetupFailed},
		{"prepare/grant replay", mapPrepareProcessError, sandbox.ErrGrantReplay, tool.ProcessErrorSetupFailed},
		{"prepare/grant required", mapPrepareProcessError, sandbox.ErrGrantRequired, tool.ProcessErrorSetupFailed},
		{"prepare/grant denied", mapPrepareProcessError, sandbox.ErrGrantDenied, tool.ProcessErrorSetupFailed},
		{"prepare/executor closed", mapPrepareProcessError, sandbox.ErrExecutorClosed, tool.ProcessErrorSetupFailed},

		{"start/generic", mapStartError, generic, tool.ProcessErrorSpawnFailed},
		{"start/lifetime containment unavailable", mapStartError, sandbox.ErrLifetimeContainmentUnavailable, tool.ProcessErrorLifetimeEnforcementUnavailable},
		{"start/tty unsupported", mapStartError, sandbox.ErrProcessTTYUnsupported, tool.ProcessErrorPTYUnavailable},
		{"start/conpty unavailable", mapStartError, sandbox.ErrProcessConPTYUnavailable, tool.ProcessErrorPTYUnavailable},
		{"start/already started", mapStartError, sandbox.ErrProcessAlreadyStarted, tool.ProcessErrorSpawnFailed},
		{"start/closed", mapStartError, sandbox.ErrProcessClosed, tool.ProcessErrorSpawnFailed},

		{"signal/generic", mapSignalError, generic, tool.ProcessErrorSignalFailed},
		{"resize/generic", mapResizeError, generic, tool.ProcessErrorPTYUnavailable},
		{"wait/generic", mapWaitError, generic, tool.ProcessErrorWaitFailed},
		{"close/generic", mapCloseError, generic, tool.ProcessErrorTeardownFailed},
		{"close/executor set closed", mapCloseError, sandbox.ErrExecutorSetClosed, tool.ProcessErrorTeardownFailed},
		{"close/executor limit", mapCloseError, sandbox.ErrExecutorLimit, tool.ProcessErrorTeardownFailed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.mapper(tc.err)
			var procErr *tool.ProcessError
			if !errors.As(got, &procErr) {
				t.Fatalf("mapped error = %v (%T), want *tool.ProcessError", got, got)
			}
			if procErr.Code != tc.wantCode {
				t.Fatalf("Code = %v, want %v", procErr.Code, tc.wantCode)
			}
			if !errors.Is(got, tc.err) {
				t.Fatalf("mapped error does not unwrap to original cause %v (Cause = %v)", tc.err, procErr.Cause)
			}
		})
	}

	for _, mapper := range []func(error) error{
		mapPrepareProcessError, mapStartError, mapSignalError, mapResizeError, mapWaitError, mapCloseError,
	} {
		if got := mapper(nil); got != nil {
			t.Fatalf("mapper(nil) = %v, want nil", got)
		}
	}
}

// --- Step 1: "adapter satisfies tool.AsyncProcessRunner" -------------------

func TestProcessAdapterSatisfiesAsyncProcessRunner(t *testing.T) {
	t.Parallel()
	var _ tool.AsyncProcessRunner = processRunnerAdapter{}
	var _ tool.PreparedProcess = preparedProcessAdapter{}
	var _ tool.Process = (*processAdapter)(nil)
	var _ tool.ProcessActivitySource = (*processAdapter)(nil)

	root := canonicalTempDir(t)
	set := unconfinedExecutorSet(t, root, 1)
	resolver := newProcessRunnerResolver(set)

	// The resolver is invoked directly, with a bare context and a LoopID --
	// no rig.Rig, tool.ProcessBinding, harness lifecycle option, or
	// provider of any kind is constructed anywhere in this test, proving
	// the resolver (and the adapter it produces) needs none of that
	// machinery to function.
	runner, err := resolver(context.Background(), newUUID(t))
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	if runner == nil {
		t.Fatal("resolver returned a nil runner")
	}
	if _, err := runner.PrepareProcess(context.Background(), tool.ProcessRequest{
		Command: "true", Directory: root, OriginExecutionID: newUUID(t),
	}); err != nil {
		t.Fatalf("PrepareProcess through the resolver-produced runner: %v", err)
	}
}

// --- Step 1: resolver behavior ----------------------------------------------

func TestProcessAdapterResolverCapturesExecutorSetAndUsesExactKey(t *testing.T) {
	t.Parallel()
	root := canonicalTempDir(t)
	set := unconfinedExecutorSet(t, root, 1)
	resolver := newProcessRunnerResolver(set)

	loopID := newUUID(t)
	runner, err := resolver(context.Background(), loopID)
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	adapter, ok := runner.(processRunnerAdapter)
	if !ok {
		t.Fatalf("resolver returned %T, want processRunnerAdapter", runner)
	}

	direct, err := set.For(loopID.String())
	if err != nil {
		t.Fatalf("set.For(loopID.String()) directly: %v", err)
	}
	if adapter.exec != direct {
		t.Fatal("resolver's wrapped executor is not the identical instance set.For(loopID.String()) returns -- the resolver used a different key")
	}

	// max=1 above: a second, DIFFERENT loop id must hit ErrExecutorLimit,
	// proving the first resolver call consumed exactly one memoization slot
	// under exactly the key loopID.String() -- not some other, wrong key
	// that would have left this slot free.
	other := newUUID(t)
	if _, err := resolver(context.Background(), other); !errors.Is(err, sandbox.ErrExecutorLimit) {
		t.Fatalf("resolver(other loop id) with max=1 already consumed = %v, want ErrExecutorLimit", err)
	}
}

func TestProcessAdapterResolverPreservesLookupFailures(t *testing.T) {
	t.Parallel()
	root := canonicalTempDir(t)
	set := unconfinedExecutorSet(t, root, 1)
	resolver := newProcessRunnerResolver(set)

	first := newUUID(t)
	if _, err := resolver(context.Background(), first); err != nil {
		t.Fatalf("first resolver call: %v", err)
	}
	second := newUUID(t)
	runner, err := resolver(context.Background(), second)
	if runner != nil {
		t.Fatalf("resolver returned a non-nil runner alongside a lookup failure: %v", runner)
	}
	if !errors.Is(err, sandbox.ErrExecutorLimit) {
		t.Fatalf("resolver lookup failure = %v, want to preserve sandbox.ErrExecutorLimit unwrapped", err)
	}
}
