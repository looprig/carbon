// supervised.go routes a SUPERVISED Bash call (background or yield_time_ms,
// normalized by prepare.go's normalizeSupervision into bashArtifact.supervised)
// through the shared, runner-free process.Supervisor. A legacy call never
// reaches this file: bash.go's InvokableRun dispatches to runSupervised only
// when the prepared artifact says so; every other call keeps executing the
// unchanged synchronous `sh -c` (or injected tool.CommandRunner) path.
//
// The concrete BashTool built by NewSupervisedFactory already owns, from its
// bound construction data (Bindings, resolved once at Build), the async
// process runner, the session resource registry, the owning session/loop
// identity, the workspace coordinator, and the observation capability.
// runSupervised never looks up invocation provenance to select a runner —
// b.asyncRunner is a fixed field set at Build.
package bash

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/tools/internal/definition"
	"github.com/looprig/tools/internal/workspace"
	"github.com/looprig/tools/process"
)

// SupervisedFactory builds a session-supervised BashTool bound to bindings
// and the caller's already-resolved, validated tool.AsyncProcessRunner.
// bindings must satisfy tool.RequiresWorkspace|tool.RequiresProcessServices
// (the harness Definition.Build boundary validates SessionID/LoopID/
// Workspace/Process before any factory ever runs); runner is a per-Build
// input the caller resolved from the validated bound LoopID BEFORE calling
// this factory — never derived here from ctx or invocation provenance.
// Task 19's root definition owns that resolver; this task only accepts its
// already-resolved result.
type SupervisedFactory func(bindings tool.Bindings, runner tool.AsyncProcessRunner) (*BashTool, error)

// NewSupervisedFactory validates and seals Bash options once, exactly like
// NewFactory, for a session-supervised binding. It never adds a Runner field
// to tool.ProcessBinding and never accepts a runner as one of options: the
// async runner is a distinct per-Build input to the returned SupervisedFactory,
// not a sealed BashOption.
func NewSupervisedFactory(options ...BashOption) (SupervisedFactory, error) {
	config, err := resolveBashOptions(options)
	if err != nil {
		return nil, err
	}
	return func(bindings tool.Bindings, runner tool.AsyncProcessRunner) (*BashTool, error) {
		if workspace.IsNil(runner) {
			return nil, &definition.BuildError{Definition: bashToolName, Dependency: "async_runner"}
		}
		if bindings.Workspace == nil {
			return nil, &definition.BuildError{Definition: bashToolName, Dependency: "workspace"}
		}
		if bindings.Process == nil || workspace.IsNil(bindings.Process.Registry) {
			return nil, &definition.BuildError{Definition: bashToolName, Dependency: "process_registry"}
		}
		bound := config
		bound.coord = bindings.Workspace.Coordinator
		bound.obs = bindings.Workspace.Observations
		b := newBash(bindings.Workspace.Root, bound)
		b.asyncRunner = runner
		b.registry = bindings.Process.Registry
		b.owner = process.Owner{SessionID: bindings.SessionID, LoopID: bindings.LoopID}
		return b, nil
	}, nil
}

// runSupervised is InvokableRun's dispatch target for a supervised call: dir
// is the already-revalidated confined spawn directory (identical
// prepare/run-time consistency check InvokableRun already performs for the
// legacy path). It never returns a Go error — every failure renders as a
// supervisedErrorResult tool-result, matching InvokableRun's documented
// contract.
func (b *BashTool) runSupervised(ctx context.Context, call tool.PreparedCall, art *bashArtifact, dir string) (*tool.ToolResult, error) {
	if workspace.IsNil(b.asyncRunner) {
		return supervisedErrorResult(string(process.CodeLifetimeEnforcementUnavailable)), nil
	}
	lifetimeCoord, ok := b.coord.(tool.WorkspaceLifetimeCoordinator)
	if !ok || workspace.IsNil(lifetimeCoord) {
		return supervisedErrorResult(string(process.CodeLifetimeEnforcementUnavailable)), nil
	}
	if workspace.IsNil(b.registry) {
		return supervisedErrorResult(string(process.CodeLifetimeEnforcementUnavailable)), nil
	}

	// Step 1: PrepareProcess reserves enforcement resources WITHOUT
	// spawning anything (tool.AsyncProcessRunner's own doc contract) —
	// strictly before any lease acquisition below.
	req := tool.ProcessRequest{
		Command:           art.command,
		Directory:         dir,
		Grants:            call.Grants,
		OriginExecutionID: call.ExecutionID,
		Deadline:          processDeadline(art),
		PTY:               art.tty,
	}
	prepared, err := b.asyncRunner.PrepareProcess(ctx, req)
	if err != nil {
		return supervisedErrorResult(classifyPrepareProcessError(err)), nil
	}

	// Step 2: the PREPARED, authoritative access (never any caller-declared
	// or guessed access) determines the exact lifetime lease.
	access := prepared.EffectiveWorkspaceAccess()
	permit, err := lifetimeCoord.AcquireLifetime(ctx, access)
	if err != nil {
		_ = prepared.Close()
		return supervisedErrorResult(string(process.CodeLifetimeEnforcementUnavailable)), nil
	}

	// Step 3: obtain the ONE shared, runner-free Supervisor session
	// resource. A failure here still holds the lease and the preparation —
	// both must be released/closed before returning.
	resource, err := b.registry.GetOrCreate(ctx, process.SupervisorResourceKey, process.NewSupervisorResource)
	if err != nil {
		permit.Release()
		_ = prepared.Close()
		return supervisedErrorResult(string(process.CodeProcessSetupFailed)), nil
	}
	sr, ok := resource.(*process.SupervisorResource)
	if !ok || sr == nil || sr.Supervisor == nil {
		permit.Release()
		_ = prepared.Close()
		return supervisedErrorResult(string(process.CodeProcessSetupFailed)), nil
	}

	// Step 4: hand the prepared process to the runner-free Supervisor.Start
	// only now that the lease is held. Start consumes prepared exactly
	// once and, on ANY failure of its own, already releases the lease,
	// closes prepared, and reverses its quota reservation itself (see its
	// doc comment) — runSupervised must NOT repeat any of that here.
	origin := process.Origin{ToolExecutionID: call.ExecutionID}
	lease := leaseFromPermit{permit: permit}
	ceiling := storageCeiling(art)
	yield := process.YieldSettings{Yield: art.background}

	handle, err := sr.Supervisor.Start(ctx, b.owner, origin, prepared, lease, nil, nil, ceiling, yield)
	if err != nil {
		return supervisedErrorResult(classifyProcessError(err, process.CodeSpawnFailed)), nil
	}

	// The process is now durably registered (Supervisor.Start persists its
	// StateStarting manifest and registers the entry before ever returning
	// a Handle) — this is the "spawn" invalidation point.
	b.invalidateObservations()

	// Start's own manifest write (StateRunning, StartedAt) is already
	// durable by the time it returns the Handle, so a best-effort reload
	// here already reflects it — for BOTH the live and terminal shapes
	// below, not only the terminal one.
	startedAt := loadStartedAt(sr, handle)

	if art.background {
		// Explicit background: return immediately, exactly as soon as
		// registration is durable, without waiting for any output at all.
		//
		// watchAndInvalidate is deliberately rooted in context.Background(),
		// never ctx: the whole point of background/yielded supervision is
		// that the process (and this watcher) keeps running after this
		// invocation's own request-scoped ctx ends when InvokableRun
		// returns, hence the explicit #nosec below.
		go b.watchAndInvalidate(sr.Supervisor, handle) // #nosec G118 -- watcher deliberately outlives the request ctx; see comment above
		return liveSupervisedResult(string(handle), 0, "", startedAt), nil
	}

	budget := time.Duration(art.yieldTimeMS) * time.Millisecond
	terminal, waitErr := waitForTerminal(ctx, sr.Supervisor, b.owner, handle, budget, b.invalidateObservations)
	if waitErr != nil || !terminal {
		// Either the budget elapsed with the command still running, or the
		// invocation ctx ended first — either way the process itself keeps
		// running untouched; hand off to the detached watcher and report a
		// LIVE result. Same deliberate context.Background() rationale as
		// the explicit-background branch above.
		go b.watchAndInvalidate(sr.Supervisor, handle) // #nosec G118 -- watcher deliberately outlives the request ctx; see comment above
		return liveSupervisedResult(string(handle), 0, "", startedAt), nil
	}

	status, exitCode, reason, finishedAt := readTerminalOutcome(sr, handle)
	output := readTerminalOutput(ctx, sr, b.owner, handle)
	return terminalSupervisedResult(status, exitCode, reason, startedAt, finishedAt, output), nil
}

// waitForTerminal blocks, in a sequence of process.Supervisor.Wait(WaitAny)
// calls each bounded by the remaining budget, until handle reaches a
// terminal state or budget elapses (returns terminal=false, err=nil — never
// treated as a hard failure) or ctx itself ends first (also terminal=false,
// err=nil: the caller must still hand off to the detached watcher rather
// than treat a canceled invocation as a process failure). onWake, when
// non-nil, is called once per observed change (including the final terminal
// one) — the caller's "intermediate invalidation" hook.
func waitForTerminal(ctx context.Context, supervisor *process.Supervisor, owner process.Owner, handle process.Handle, budget time.Duration, onWake func()) (terminal bool, err error) {
	if budget <= 0 {
		return false, nil
	}
	deadline := time.Now().Add(budget)
	var generation uint64
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false, nil
		}
		waitCtx, cancel := context.WithTimeout(ctx, remaining)
		statuses, waitErr := supervisor.Wait(waitCtx, owner, process.WaitAny, []process.WaitTarget{{Handle: handle, Generation: generation}})
		cancel()
		if waitErr != nil {
			if errors.Is(waitErr, context.DeadlineExceeded) || errors.Is(waitErr, context.Canceled) {
				return false, nil
			}
			return false, waitErr
		}
		if len(statuses) == 0 {
			return false, nil
		}
		status := statuses[0]
		generation = status.Generation
		if onWake != nil {
			onWake()
		}
		if status.Terminal {
			return true, nil
		}
	}
}

// watchAndInvalidate runs detached from the invocation context (via
// context.Background(), never ctx — mirroring tool.PreparedProcess's own
// documented "the returned Process lives ... independently of the Start
// context" contract, which every post-handoff observer must honor
// identically): it keeps polling the shared Supervisor's exported Wait API
// for handle until it reaches a terminal state, invalidating the bound
// loop's observation set on every wake. This is what makes "runner activity
// triggers intermediate invalidation" and "completion invalidates the bound
// loop observations" true for a call that has already returned its handle
// to the model — including the explicit-background case, which never waits
// at all before returning.
func (b *BashTool) watchAndInvalidate(supervisor *process.Supervisor, handle process.Handle) {
	ctx := context.Background()
	var generation uint64
	for {
		statuses, err := supervisor.Wait(ctx, b.owner, process.WaitAny, []process.WaitTarget{{Handle: handle, Generation: generation}})
		if err != nil || len(statuses) == 0 {
			return
		}
		status := statuses[0]
		generation = status.Generation
		b.invalidateObservations()
		if status.Terminal {
			return
		}
	}
}

// loadStartedAt reads handle's own durable Manifest.StartedAt, best-effort:
// a reload failure or a not-yet-started manifest yields the zero Time,
// which supervisedResult's formatTime/omitempty simply omits.
func loadStartedAt(sr *process.SupervisorResource, handle process.Handle) time.Time {
	manifest, err := sr.Manifests.Load(handle)
	if err != nil || manifest.StartedAt == nil {
		return time.Time{}
	}
	return *manifest.StartedAt
}

// readTerminalOutcome reads handle's own durable terminal Manifest —
// process.State, process.Result, and Manifest.FinishedAt — never a value
// Bash computed or guessed itself. A reload failure conservatively reports
// "failed" rather than fabricating a success.
func readTerminalOutcome(sr *process.SupervisorResource, handle process.Handle) (status string, exitCode *int, reason string, finishedAt time.Time) {
	manifest, err := sr.Manifests.Load(handle)
	if err != nil {
		return string(process.StateFailed), nil, "failed", time.Time{}
	}
	if manifest.FinishedAt != nil {
		finishedAt = *manifest.FinishedAt
	}
	return string(manifest.State), manifest.Result.ExitCode, manifest.Result.Reason, finishedAt
}

// readTerminalOutput reads handle's bounded, safe-text-rendered combined
// stdout+stderr through process's own model-facing ProcessOutput tool
// (process/output_tool.go's ProcessOutputTool) -- the exact same rendering
// path (spool read, safe-text normalization, DefaultMaxInlineResultBytes
// cap) a model-issued ProcessOutput call over this same handle would use,
// never a second, parallel reimplementation of it. This is safe to call
// only because the entry is still resolvable in the shared Supervisor's
// registry at this point: recordTerminal (process/supervisor.go) adds a
// just-terminalized entry to retention bookkeeping and evicts only when a
// session's retained-completed-process count already exceeds its
// configured limit, so the entry this call just observed going terminal is
// never its own session's eviction victim.
//
// terminalOutputArgs's process_id is the only field ever set: cursor,
// limit_bytes, and encoding are left at PrepareCall's own documented
// defaults (0, DefaultMaxInlineResultBytes, safe_text) -- the spec's shown
// terminal Bash result carries a plain "output" string with no cursor, gap,
// or truncation indicator of its own, so this call asks for nothing beyond
// that plain rendered text.
//
// Best-effort: PrepareCall failing, InvokableRun returning a whole-call
// structural error, or this one result carrying its own per-process error
// (e.g. a not_found this call's own eviction-avoidance reasoning above
// makes theoretical, not a case worth failing the outer Bash call over)
// all yield the empty string rather than failing runSupervised's already-
// terminal outcome -- mirroring readTerminalOutcome's own
// never-fail-the-outer-call discipline.
func readTerminalOutput(ctx context.Context, sr *process.SupervisorResource, owner process.Owner, handle process.Handle) string {
	out := process.NewProcessOutput(sr.Supervisor, owner)

	argsJSON, err := json.Marshal(terminalOutputArgs{ProcessID: string(handle)})
	if err != nil {
		return ""
	}

	// PrepareCall ignores both its ctx and executionID parameters (see its
	// own doc comment), so a zero uuid.UUID is exactly as good as a freshly
	// minted one here.
	req, artifact, err := out.PrepareCall(ctx, uuid.UUID{}, string(argsJSON))
	if err != nil {
		return ""
	}
	runCtx := loop.WithPreparedCall(ctx, tool.PreparedCall{Request: req, Artifact: artifact})

	result, err := out.InvokableRun(runCtx, string(argsJSON))
	if err != nil || result == nil || len(result.Content) == 0 {
		return ""
	}
	block, ok := result.Content[0].(*content.TextBlock)
	if !ok {
		return ""
	}

	var decoded terminalOutputResult
	if err := json.Unmarshal([]byte(block.Text), &decoded); err != nil || decoded.Error != "" {
		return ""
	}
	return decoded.Output
}

// terminalOutputArgs is the minimal ProcessOutput argsJSON readTerminalOutput
// sends: exactly the single required field, letting PrepareCall apply every
// other documented default itself.
type terminalOutputArgs struct {
	ProcessID string `json:"process_id"`
}

// terminalOutputResult decodes only the two fields readTerminalOutput cares
// about out of ProcessOutput's full per-process JSON shape (output_tool.go's
// processOutputResult) -- every other field it renders (cursors, gap,
// artifact, manifest metadata) is already sourced independently by
// readTerminalOutcome/loadStartedAt from the same durable Manifest, so this
// decode target intentionally stays narrow.
type terminalOutputResult struct {
	Output string `json:"output"`
	Error  string `json:"error"`
}

// processDeadline maps the prepared, frozen supervision settings to
// tool.ProcessRequest's Deadline: a zero Time means "no process deadline"
// (art.noDeadline — supervised `timeout: 0`), otherwise a hard deadline
// art.timeout (already clamped at preparation) out from now.
func processDeadline(art *bashArtifact) time.Time {
	if art.noDeadline {
		return time.Time{}
	}
	return time.Now().Add(art.timeout)
}

// storageCeiling maps the prepared `max_output_bytes` setting to the
// Supervisor's per-process StorageCeiling. ProcessRequest carries no
// output-limit field of its own (output retention is Tools' own concern,
// not the runner's), so max_output_bytes reaches admission through
// StorageCeiling.SpoolBytes, not through tool.ProcessRequest. An absent
// setting yields the zero StorageCeiling, which Supervisor.Start's own
// reserveQuota falls back from to its configured per-process default.
func storageCeiling(art *bashArtifact) process.StorageCeiling {
	if !art.hasMaxOutputBytes {
		return process.StorageCeiling{}
	}
	return process.StorageCeiling{SpoolBytes: art.maxOutputBytes}
}

// classifyProcessError renders err's stable process.Code when err is a
// *process.Error (as every failure Supervisor.Start itself returns is), or
// fallback otherwise. It never renders err's free-form Cause.
func classifyProcessError(err error, fallback process.Code) string {
	var perr *process.Error
	if errors.As(err, &perr) {
		return string(perr.Code)
	}
	return string(fallback)
}

// classifyPrepareProcessError renders a tool.AsyncProcessRunner.PrepareProcess
// failure into runSupervised's stable, model-facing process.Code. A
// PrepareProcess failure never spawns anything either way (Start is never
// even reached), so "no fallback to pipes" already holds regardless of this
// classification; what this function adds is naming the real reason. A
// runner that classifies its own failure through Harness's typed
// tool.ProcessError (e.g. tool.ProcessErrorPTYUnavailable, when `tty:true`
// could not be honored — spec "tty: true requests a real PTY/ConPTY.
// Failure to allocate one returns pty_unavailable; it never falls back to
// pipes") reports that exact code, distinguishing a PTY-allocation failure
// from every other setup failure. Every other error (untyped, or a
// tool.ProcessErrorCode this package's own domain has no direct counterpart
// for) conservatively reports process_setup_failed, exactly matching this
// call's behavior before this classification existed.
func classifyPrepareProcessError(err error) string {
	var perr *tool.ProcessError
	if errors.As(err, &perr) && perr.Code == tool.ProcessErrorPTYUnavailable {
		return string(process.CodePTYUnavailable)
	}
	return string(process.CodeProcessSetupFailed)
}

// leaseFromPermit adapts a tool.WorkspacePermit (Release with no return
// value) to process.Lease (Release() error) — both exported types with
// exported methods, so, unlike process's package-private lifecycleSink/
// completionNotifier (left nil below; Task 24 wires real lifecycle/
// notification services), Bash CAN implement this one itself.
type leaseFromPermit struct{ permit tool.WorkspacePermit }

func (l leaseFromPermit) Release() error {
	l.permit.Release()
	return nil
}

// The shared *process.Supervisor session resource every supervised Bash call
// (and every ProcessOutput/ProcessInput/ProcessStop call in the same
// session) obtains is the single exported process.SupervisorResource
// (process/session_resource.go), keyed by process.SupervisorResourceKey and
// constructed by process.NewSupervisorResource — not a private type of this
// package's own. This module's root definitions.go resolves the identical
// symbols for its three companion definitions, so any of the four
// process-backed definitions may win tool.SessionResourceRegistry's
// get-or-create race and every later caller still type-asserts the result
// to the same concrete type.
