package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/sandbox"
	"github.com/looprig/tools"
)

// process_adapter.go is CodeRig's MECHANICAL Sandbox-to-Harness type mapping
// for asynchronous (supervised/background) processes. It follows the exact
// composition style toolsets.go's grantedExecutor already established for the
// synchronous command path: a thin value wraps a concrete Sandbox type and
// satisfies a Harness/tools interface by translating field-for-field and
// call-for-call. No buffering, authorization, event, or supervisor policy
// lives here — that is the tools package's process.Supervisor's job. This
// file only answers "what is the Sandbox equivalent of this Harness call,"
// never "should this call be allowed" or "what should happen next."
//
// newProcessRunnerResolver is the exposed seam: it captures one role's
// *sandbox.ExecutorSet and returns a tools.AsyncProcessRunnerResolver — a
// closure/factory, not a pre-built adapter. Each call resolves the executor
// bound to the LoopID Harness supplies at Bind time (via
// set.For(loopID.String()), the SAME per-Loop executor bashDefinition and
// roleGate already resolve for that Loop) and wraps it fresh. Task 27 is the
// first and only caller that threads this resolver into a role's Bash
// definition; this file deliberately does not touch toolsets.go's
// roster-building functions.

// newProcessRunnerResolver returns a tools.AsyncProcessRunnerResolver closed
// over set. Each call uses set.For(loopID.String()) exactly once and wraps
// the returned *sandbox.Executor as a tool.AsyncProcessRunner; a lookup
// failure (ErrExecutorSetClosed, ErrExecutorLimit, or an invalid key) is
// returned to the caller unwrapped — CodeRig adds no ProcessError translation
// at this seam, since no process admission has even begun.
func newProcessRunnerResolver(set *sandbox.ExecutorSet) tools.AsyncProcessRunnerResolver {
	return func(_ context.Context, loopID uuid.UUID) (tool.AsyncProcessRunner, error) {
		executor, err := set.For(loopID.String())
		if err != nil {
			return nil, err
		}
		return processRunnerAdapter{exec: executor}, nil
	}
}

// processRunnerAdapter adapts a *sandbox.Executor to tool.AsyncProcessRunner,
// mirroring grantedExecutor's composition exactly: a concrete Sandbox type
// wrapped by value, no interface indirection.
type processRunnerAdapter struct{ exec *sandbox.Executor }

// PrepareProcess maps every tool.ProcessRequest field exactly once onto
// sandbox.ProcessOptions:
//
//   - Command, Directory, Grants, and PTY (-> TTY) carry across unchanged.
//   - OriginExecutionID (uuid.UUID) becomes ExecutionID (string) via
//     .String(); Sandbox's own grant verification (verifyGrantBinding)
//     rejects a malformed or mismatched value, so this adapter does not
//     duplicate that validation.
//   - Deadline carries across unchanged; see processAdapter's doc comment
//     for why this adapter still never produces ProcessTerminalTimedOut.
//
// sandbox.ProcessOptions.TerminateGrace has no Harness source field and is
// left at its zero value, which Sandbox itself resolves to its own default
// (defaultProcessTerminateGrace) — this adapter invents no policy of its
// own for it.
//
// Grants and the origin execution ID are passed through as opaque values:
// this adapter never inspects, parses, or re-derives them.
func (a processRunnerAdapter) PrepareProcess(ctx context.Context, req tool.ProcessRequest) (tool.PreparedProcess, error) {
	opts := sandbox.ProcessOptions{
		Command:     req.Command,
		Directory:   req.Directory,
		ExecutionID: req.OriginExecutionID.String(),
		Grants:      req.Grants,
		TTY:         req.PTY,
		Deadline:    req.Deadline,
	}
	prepared, err := a.exec.PrepareProcess(ctx, opts)
	if err != nil {
		return nil, mapPrepareProcessError(err)
	}
	return preparedProcessAdapter{prepared: prepared, tty: req.PTY}, nil
}

var _ tool.AsyncProcessRunner = processRunnerAdapter{}

// preparedProcessAdapter adapts a *sandbox.PreparedProcess to
// tool.PreparedProcess. tty is the PTY bit this preparation was requested
// with, captured at PrepareProcess time so the eventual Process can report
// StreamMode without needing Sandbox's own (module-internal) stream-mode
// type — see processAdapter.StreamMode's doc comment for why.
type preparedProcessAdapter struct {
	prepared *sandbox.PreparedProcess
	tty      bool
}

// EffectiveWorkspaceAccess maps Sandbox's authoritative ProcessAccess onto
// Harness's WorkspaceAccess one-for-one: Kind translates via mapAccessKind,
// and WritePaths/WriteTrees (each already a defensive copy from Sandbox)
// pass through unchanged into tool.NewWorkspaceAccess, which itself takes
// its own defensive copy. This is available, and stable, before Start is
// ever called — the caller's lease/lifetime acquisition (Harness's
// WorkspaceLifetimeCoordinator, in the tools package) happens between this
// call and Start, exactly as the two-phase PreparedProcess contract intends;
// this adapter does nothing to prevent or shortcut that window.
func (p preparedProcessAdapter) EffectiveWorkspaceAccess() tool.WorkspaceAccess {
	access := p.prepared.EffectiveAccess()
	return tool.NewWorkspaceAccess(mapAccessKind(access.Kind), access.WritePaths(), access.WriteTrees())
}

// Start consumes the preparation exactly once, mirroring Sandbox's own
// single-use contract: a second Start, or a Start after Close, fails without
// spawning (proven by TestProcessAdapterPreparedSingleUse).
func (p preparedProcessAdapter) Start(ctx context.Context) (tool.Process, error) {
	proc, err := p.prepared.Start(ctx)
	if err != nil {
		return nil, mapStartError(err)
	}
	return newProcessAdapter(proc, p.tty), nil
}

// Close releases an unstarted preparation's reservations and is a harmless
// no-op once Start has consumed it, exactly mirroring
// sandbox.PreparedProcess.Close's own documented idempotence.
func (p preparedProcessAdapter) Close() error {
	if err := p.prepared.Close(); err != nil {
		return mapCloseError(err)
	}
	return nil
}

var _ tool.PreparedProcess = preparedProcessAdapter{}

// mapAccessKind translates Sandbox's ProcessAccessKind to Harness's
// WorkspaceAccessKind one-for-one by name (ReadOnly/ScopedWrite/BroadWrite on
// both sides). An unrecognized Sandbox kind — never produced by today's
// Sandbox implementation — fails closed to the most restrictive Harness kind
// rather than silently widening authority.
func mapAccessKind(kind sandbox.ProcessAccessKind) tool.WorkspaceAccessKind {
	switch kind {
	case sandbox.ProcessAccessReadOnly:
		return tool.WorkspaceAccessReadOnly
	case sandbox.ProcessAccessScopedWrite:
		return tool.WorkspaceAccessScopedWrite
	case sandbox.ProcessAccessBroadWrite:
		return tool.WorkspaceAccessBroadWrite
	default:
		return tool.WorkspaceAccessReadOnly
	}
}

// signalSeverity ranks the three portable process signals so processAdapter
// can track the highest-severity signal a caller has actually requested
// through this adapter, for use by deriveReason. Kill dominates Terminate
// dominates Interrupt: an escalating caller (or Sandbox's own
// terminate-then-kill grace escalation, driven by this same adapter's calls)
// always ends up attributing the eventual death to the more severe request.
type signalSeverity uint32

const (
	signalNone signalSeverity = iota
	signalInterrupted
	signalTerminated
	signalKilled
)

// processAdapter adapts a *sandbox.Process to tool.Process.
//
// # Stream mode
//
// StreamMode reports tty (the PTY bit this Process's preparation was started
// with) rather than calling Sandbox's own Process.StreamMode: Sandbox's
// facade package (github.com/looprig/sandbox) does not re-export a way to
// construct or compare against sandbox.ProcessStreamMode's values from
// outside the module in a way CodeRig could otherwise use without importing
// internal/exec directly, which the module boundary forbids. This is not a
// loss of fidelity: Sandbox's own PrepareProcess documents that a TTY
// request is "honored with a real platform PTY where one exists... and
// rejected outright everywhere else... rather than silently downgraded to
// pipes" — so for any Process this adapter's Start ever successfully
// returns, Sandbox's own StreamMode is PTY if and only if tty is true, by
// that contract alone.
//
// # Activities
//
// activities is a translated copy of Sandbox's own activity channel, pumped
// by pumpActivities: Sandbox's sandbox.ProcessActivity and Harness's
// tool.ProcessActivity are structurally similar but distinct Go types, so
// the channel itself cannot be returned directly. Sandbox's *sandbox.Process
// always reports real activity support (Activities() is unconditional on
// its side), so this adapter always implements tool.ProcessActivitySource —
// never conditionally — matching that.
//
// Wait blocks on activitiesEnd (closed by pumpActivities immediately after
// closing the translated activities channel) before returning a successful
// result, so "the activity channel must close before Process.Wait returns"
// (tool.ProcessActivitySource's documented contract) holds for THIS
// adapter's own channel, not only for Sandbox's internal one. This wait is
// never reached on the ctx-cancellation path (see Wait's own doc comment),
// so a caller's context timeout is never delayed by it.
//
// # Terminal reason derivation — see deriveReason's doc comment.
type processAdapter struct {
	proc *sandbox.Process
	tty  bool

	signalState atomic.Uint32

	activities    chan tool.ProcessActivity
	activitiesEnd chan struct{}
}

// newProcessAdapter wraps proc and starts its activity-translation pump.
func newProcessAdapter(proc *sandbox.Process, tty bool) *processAdapter {
	p := &processAdapter{
		proc:          proc,
		tty:           tty,
		activities:    make(chan tool.ProcessActivity),
		activitiesEnd: make(chan struct{}),
	}
	go p.pumpActivities()
	return p
}

// pumpActivities drains Sandbox's own activity channel, translating each
// value onto this adapter's own channel, and closes both — activities first,
// then activitiesEnd — exactly once, when Sandbox's channel closes (which
// Sandbox itself guarantees happens no later than its own Process reaching a
// terminal state, strictly before its internal Wait unblocks).
func (p *processAdapter) pumpActivities() {
	defer close(p.activitiesEnd)
	defer close(p.activities)
	src := p.proc.Activities()
	if src == nil {
		return
	}
	for activity := range src {
		p.activities <- translateActivity(activity)
	}
}

// translateActivity maps one Sandbox activity onto its Harness equivalent.
// It resolves Sandbox's own invalid-kind broadening (EffectiveKind) first,
// so this adapter's output is always one of the two valid Harness kinds —
// an invalid Sandbox value always broadens to BroadWrite, never propagates
// an unrecognized value for Harness's own EffectiveKind to have to catch.
func translateActivity(activity sandbox.ProcessActivity) tool.ProcessActivity {
	if activity.EffectiveKind() == sandbox.ProcessActivityBroadWrite {
		return tool.ProcessActivity{Kind: tool.WorkspaceActivityBroadWrite}
	}
	return tool.ProcessActivity{Kind: tool.WorkspaceActivityWrite}
}

func (p *processAdapter) Stdout() io.ReadCloser { return p.proc.Stdout() }
func (p *processAdapter) Stderr() io.ReadCloser { return p.proc.Stderr() }
func (p *processAdapter) Stdin() io.WriteCloser { return p.proc.Stdin() }

// StreamMode reports tty translated to Harness's vocabulary. See
// processAdapter's own doc comment for why this does not call Sandbox's
// Process.StreamMode.
func (p *processAdapter) StreamMode() tool.ProcessStreamMode {
	if p.tty {
		return tool.ProcessStreamModePTY
	}
	return tool.ProcessStreamModePipes
}

// Activities returns this adapter's own translated channel; see
// processAdapter's doc comment.
func (p *processAdapter) Activities() <-chan tool.ProcessActivity { return p.activities }

// Wait blocks on Sandbox's own Wait, translates a successful result (see
// deriveReason), and — only on that successful path — additionally blocks
// until this adapter's own translated activities channel has closed, so
// "the activity channel must close before Wait returns" holds for a caller
// that only ever sees this adapter's types. That second wait is skipped
// entirely when ctx ends the call early (Sandbox's own Wait contract: "a ctx
// that is done before the process exits does not stop or kill the process —
// it only stops this call from waiting for it"), since Sandbox's activity
// channel has no reason to have closed yet in that case and this adapter
// must not block past its own caller's deadline waiting for it to.
//
// A non-nil error from Sandbox's Wait is either the SAME ctx.Err() this
// call's own ctx produced (propagated unwrapped, so errors.Is(err,
// context.Canceled/DeadlineExceeded) keeps working for the caller) or a
// genuine post-spawn Sandbox failure, wrapped as
// &tool.ProcessError{Code: ProcessErrorWaitFailed, Cause: err} without
// synthesizing a terminal Reason — mirroring how a Harness-side supervisor
// is expected to treat a Wait error as distinct from any terminal
// ProcessResult (see this file's design-question doc comment on
// deriveReason for the evidence this mirrors).
func (p *processAdapter) Wait(ctx context.Context) (tool.ProcessResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	result, err := p.proc.Wait(ctx)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(err, ctxErr) {
			return tool.ProcessResult{}, err
		}
		return tool.ProcessResult{}, mapWaitError(err)
	}
	<-p.activitiesEnd
	return tool.ProcessResult{
		ExitCode:   result.ExitCode,
		Reason:     p.deriveReason(result.ExitCode),
		StartedAt:  result.StartedAt,
		FinishedAt: result.FinishedAt,
	}, nil
}

// deriveReason classifies a successfully-observed terminal ProcessResult
// into Harness's ProcessTerminalReason. Sandbox's own ProcessResult carries
// no Reason of its own (only ExitCode/StartedAt/FinishedAt), so this adapter
// derives one from two pieces of evidence it alone has access to: Go's own
// os/exec exit-code convention, and this adapter's own record of which
// signal (if any) IT dispatched through Signal, below.
//
// # Exited vs signal-death (ExitCode)
//
// Sandbox's Process.runWait reaps the child through the stdlib os/exec
// package (or, for a backend-owned launch, a backend Execution.Wait that
// mirrors the same convention). *os/exec.ExitError.ExitCode — and so
// sandbox.ProcessResult.ExitCode — is documented by the standard library to
// return -1 specifically when "the process hasn't exited or was terminated
// by a signal"; since Wait has already returned by the time deriveReason
// runs, "hasn't exited" is moot, so ExitCode == -1 here means, unambiguously,
// that the OS reports this process as killed by an uncaught signal rather
// than having called exit() itself. Any other ExitCode value (0 or a real
// positive/negative-but-not-(-1) exit status) means the process controlled
// its own exit, so this adapter reports ProcessTerminalExited regardless of
// whether a signal was ever requested through this adapter — a process that
// caught SIGTERM and chose to exit(0) exited; it was not killed.
//
// # Attributing a signal-death (adapter-tracked signal state)
//
// Sandbox's *sandbox.Process tracks internally whether Signal was called and
// with what escalation (terminateOnce/killOnce), but exposes none of that
// state publicly, and a Signal call that races an already-terminal process
// is silently a no-op with no distinguishing return value either way. This
// adapter therefore keeps its own record (signalState, updated only when
// Sandbox's own Signal call returns without error — see Signal, below) of
// the highest-severity signal a caller has actually, successfully requested
// through THIS adapter. When ExitCode == -1, that record is used to
// attribute the death:
//
//   - signalKilled   -> ProcessTerminalKilled
//   - signalTerminated -> ProcessTerminalTerminated
//   - signalInterrupted -> ProcessTerminalInterrupted
//   - signalNone (no adapter-issued signal explains this signal-death) ->
//     ProcessTerminalRunnerShutdown
//
// The signalNone case is a real, residual ambiguity: a signal-death with no
// adapter-tracked cause could in principle be an external actor signaling
// the OS process directly, outside this adapter's Signal method entirely.
// In practice this attributes to RunnerShutdown because Sandbox's own
// confined process-tree spawn is wired through exec.CommandContext(lease.ctx,
// ...) (process.go), and lease.ctx is canceled ONLY by explicit
// ExecutorSet/session Close today (ProcessOptions.Deadline is accepted and
// forwarded to Sandbox but is explicitly NOT YET enforced anywhere in the
// stack — see process.go's own "once a later task wires it" comment) — so a
// signal-death this adapter did not itself request is, overwhelmingly, the
// executor set (or session) shutting down out from under a still-running
// process, exactly matching tools/process's own supervisor, which signals
// shutdown-time termination through this same Signal method (so THAT path
// already attributes correctly as Terminated/Killed) but can also race
// Sandbox's own lower-level lease.ctx cancellation ahead of it.
//
// # Values this adapter never produces
//
//   - ProcessTerminalTimedOut: Deadline enforcement is not wired anywhere in
//     the stack yet (see above) — neither Sandbox's own lease.ctx nor any
//     independent watch in this adapter. Building an independent
//     deadline-race-and-kill loop into this adapter would itself be
//     supervisor policy, which this mechanical layer deliberately excludes;
//     once real Deadline enforcement lands (most likely as Sandbox's own
//     lease.ctx cancellation, indistinguishable from the RunnerShutdown case
//     above at this adapter's boundary), this mapping needs revisiting.
//   - ProcessTerminalFailed: confirmed, by reading
//     tools/process/entry.go's classifyWaitOutcome and
//     terminalStateForReason, to be the SUPERVISOR's own fallback — for a
//     Start-time failure, a genuine Wait error (this adapter's own
//     ProcessErrorWaitFailed path, above), or any unrecognized Reason value
//     — never something an adapter's own successful Wait/Reason is expected
//     to produce directly.
//   - ProcessTerminalOutputLimit: an output-buffering-limit concept owned by
//     the tools-package supervisor, which does not exist at this mechanical
//     layer.
//   - ProcessTerminalLostOnRestore: set directly by Harness's own restore
//     path (tools/process/restore.go) when reattaching to a process with no
//     live handle at all — never produced by a live Wait call on an actual
//     Process, which is the only thing this adapter ever has.
func (p *processAdapter) deriveReason(exitCode int) tool.ProcessTerminalReason {
	if exitCode != -1 {
		return tool.ProcessTerminalExited
	}
	switch signalSeverity(p.signalState.Load()) {
	case signalKilled:
		return tool.ProcessTerminalKilled
	case signalTerminated:
		return tool.ProcessTerminalTerminated
	case signalInterrupted:
		return tool.ProcessTerminalInterrupted
	default:
		return tool.ProcessTerminalRunnerShutdown
	}
}

// Resize passes rows/cols straight through to Sandbox's own Resize: both
// sides use the identical plain (uint16, uint16) shape, so no translation is
// needed beyond the call itself.
func (p *processAdapter) Resize(ctx context.Context, rows, cols uint16) error {
	if err := p.proc.Resize(ctx, rows, cols); err != nil {
		return mapResizeError(err)
	}
	return nil
}

// Signal translates kind to Sandbox's own ProcessSignal vocabulary BY NAME —
// never by relying on the two enums' ordinal values happening to match — and
// only records it into signalState (for deriveReason, above) once Sandbox's
// own call has returned without error, so a signal Sandbox never actually
// dispatched (ErrProcessSignalUnsupported, or any other failure) is never
// attributed as the cause of an eventual death.
func (p *processAdapter) Signal(ctx context.Context, kind tool.ProcessSignal) error {
	sandboxKind, severity, ok := mapProcessSignal(kind)
	if !ok {
		return &tool.ProcessError{Code: tool.ProcessErrorSignalFailed, Cause: fmt.Errorf("coderig: invalid process signal: %d", kind)}
	}
	if err := p.proc.Signal(ctx, sandboxKind); err != nil {
		return mapSignalError(err)
	}
	p.recordSignal(severity)
	return nil
}

// recordSignal raises signalState to sev if sev is more severe than
// whatever is already recorded, and otherwise leaves it untouched — so a
// Kill recorded before a later Interrupt (or a concurrent race between the
// two) can never be downgraded.
func (p *processAdapter) recordSignal(sev signalSeverity) {
	for {
		cur := signalSeverity(p.signalState.Load())
		if sev <= cur {
			return
		}
		if p.signalState.CompareAndSwap(uint32(cur), uint32(sev)) {
			return
		}
	}
}

// mapProcessSignal translates Harness's ProcessSignal to Sandbox's own,
// distinct Go type of the identical name, by explicit name-to-name switch —
// deliberately not a numeric conversion, even though the two enums' ordinal
// values happen to agree today.
func mapProcessSignal(kind tool.ProcessSignal) (sandbox.ProcessSignal, signalSeverity, bool) {
	switch kind {
	case tool.ProcessSignalInterrupt:
		return sandbox.ProcessSignalInterrupt, signalInterrupted, true
	case tool.ProcessSignalTerminate:
		return sandbox.ProcessSignalTerminate, signalTerminated, true
	case tool.ProcessSignalKill:
		return sandbox.ProcessSignalKill, signalKilled, true
	default:
		return 0, signalNone, false
	}
}

// Close translates Sandbox's own Close, which is idempotent and tolerates a
// nil receiver exactly like every other Process method here.
func (p *processAdapter) Close(ctx context.Context) error {
	if err := p.proc.Close(ctx); err != nil {
		return mapCloseError(err)
	}
	return nil
}

var (
	_ tool.Process               = (*processAdapter)(nil)
	_ tool.ProcessActivitySource = (*processAdapter)(nil)
)

// --- Sandbox error -> Harness ProcessErrorCode mapping ---------------------
//
// Each adapter method maps ANY non-nil Sandbox error it observes into a
// *tool.ProcessError carrying a fixed, call-site-appropriate Code and the
// original error, unmodified, as Cause — so a caller inspecting Cause (via
// errors.Is/errors.As against a Sandbox sentinel, e.g. sandbox.ErrGrantBadMAC
// or sandbox.ErrGrantExpired) still sees the exact underlying failure;
// nothing is lost, only classified. Two Sandbox failures have a specific,
// well-justified Harness code of their own (see mapPrepareProcessError and
// mapStartError below) because Harness's own vocabulary names them
// distinctly; every other Sandbox failure at a given call site — including
// every grant-verification sentinel (ErrGrantBadMAC, ErrGrantExpired,
// ErrGrantWrongCommand, ErrGrantWrongExecution,
// ErrGrantWrongWorkingDirectory, ErrGrantProfileMismatch,
// ErrGrantGuaranteeMismatch, ErrGrantRouteMismatch, ErrGrantReplay,
// ErrGrantRequired, ErrGrantDenied — all resolved entirely within
// PrepareProcess, before anything is spawned) and every executor-lifecycle
// sentinel (ErrExecutorClosed, ErrProcessClosed, ErrProcessAlreadyStarted,
// ErrProcessStdinClosed) — maps to that call site's own generic code. This
// is a deliberate, mechanical choice: Harness's ProcessErrorCode vocabulary
// is a small, closed set of BROAD classifications (setup/spawn/signal/wait/
// teardown-phase failures), not a place to re-encode Sandbox's own
// fine-grained sentinel taxonomy — a caller that needs that detail already
// has it, unlost, via Cause.

// mapPrepareProcessError classifies a PrepareProcess-phase failure.
// TTY/ConPTY unavailability is Harness's own distinctly-named
// ProcessErrorPTYUnavailable code (Sandbox's own PrepareProcess doc:
// "a prepare-time, platform-wide reason" is one of the two documented
// sources of ErrProcessTTYUnsupported); everything else at this call site —
// grant verification, malformed input, an executor already closed — is
// ProcessErrorSetupFailed: nothing has been spawned yet, so "setup," not
// "spawn," is the more faithful classification.
func mapPrepareProcessError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sandbox.ErrProcessTTYUnsupported) || errors.Is(err, sandbox.ErrProcessConPTYUnavailable) {
		return &tool.ProcessError{Code: tool.ProcessErrorPTYUnavailable, Cause: err}
	}
	return &tool.ProcessError{Code: tool.ProcessErrorSetupFailed, Cause: err}
}

// mapStartError classifies a Start-phase failure.
// sandbox.ErrLifetimeContainmentUnavailable reports exactly what Harness's
// ProcessErrorLifetimeEnforcementUnavailable names: a Supervised spawn
// rejected before any child exists because no kernel-enforced process-tree
// teardown proof is available on this platform/backend (Sandbox's own doc:
// today, every real Seatbelt-confined Darwin spawn). TTY/ConPTY
// unavailability is Sandbox's OTHER documented source of
// ErrProcessTTYUnsupported ("a Start-time, backend-specific reason," e.g.
// the Windows elevated/broker backend), mapped identically to
// PrepareProcess's own case. Everything else is ProcessErrorSpawnFailed.
func mapStartError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, sandbox.ErrLifetimeContainmentUnavailable):
		return &tool.ProcessError{Code: tool.ProcessErrorLifetimeEnforcementUnavailable, Cause: err}
	case errors.Is(err, sandbox.ErrProcessTTYUnsupported), errors.Is(err, sandbox.ErrProcessConPTYUnavailable):
		return &tool.ProcessError{Code: tool.ProcessErrorPTYUnavailable, Cause: err}
	default:
		return &tool.ProcessError{Code: tool.ProcessErrorSpawnFailed, Cause: err}
	}
}

// mapSignalError classifies any Signal-phase failure as
// ProcessErrorSignalFailed. Sandbox's own ErrProcessSignalUnsupported
// sentinel is not re-exported by the public sandbox package (it lives only
// in internal/exec, which this module boundary forbids importing), so this
// adapter cannot and does not special-case it by identity — it is still
// classified correctly (a signal failure) and its Cause is still preserved
// unmodified.
func mapSignalError(err error) error {
	if err == nil {
		return nil
	}
	return &tool.ProcessError{Code: tool.ProcessErrorSignalFailed, Cause: err}
}

// mapResizeError classifies any Resize-phase failure. Harness's
// ProcessErrorCode vocabulary has no dedicated "resize failed" code; Sandbox
// resize failures are fundamentally PTY-shaped (a pipe-mode Process, or a
// PTY-mode Process on a platform/backend with no resize primitive wired, per
// Sandbox's own Resize doc comment), so ProcessErrorPTYUnavailable is the
// closest defensible fit. Sandbox's own ErrProcessResizeUnsupported sentinel
// is likewise not re-exported by the public package, for the identical
// reason described on mapSignalError.
func mapResizeError(err error) error {
	if err == nil {
		return nil
	}
	return &tool.ProcessError{Code: tool.ProcessErrorPTYUnavailable, Cause: err}
}

// mapWaitError classifies a genuine (non-ctx-cancellation — see Wait's own
// doc comment for that split) Wait-phase failure as ProcessErrorWaitFailed.
func mapWaitError(err error) error {
	if err == nil {
		return nil
	}
	return &tool.ProcessError{Code: tool.ProcessErrorWaitFailed, Cause: err}
}

// mapCloseError classifies any Close-phase failure (PreparedProcess.Close or
// Process.Close) as ProcessErrorTeardownFailed.
func mapCloseError(err error) error {
	if err == nil {
		return nil
	}
	return &tool.ProcessError{Code: tool.ProcessErrorTeardownFailed, Cause: err}
}
