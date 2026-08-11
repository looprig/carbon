package process

// stop_tool.go implements the ProcessStop model-facing tool (design spec
// docs/specs/long-running-command-supervision.md, "ProcessStop API"):
// signaling the whole owned process tree -- interrupt, graceful
// terminate-then-kill, or immediate kill -- and confirming Sandbox's own
// termination before ever reporting success.
//
// Like ProcessOutput and ProcessInput, ProcessStop never re-runs the
// originating Bash gate: its Request carries no Requirements. Owner
// authorization is identical -- a missing handle and a cross-owner handle
// are indistinguishable, both rendering "not_found" -- achieved by calling
// the exact same Supervisor.resolveEntry helper output_tool.go defines
// (same package). ProcessStop "may signal only the owned process tree"
// (spec "Identity and authorization").
//
// This file follows the same PrepareCall/InvokableRun shape as
// output_tool.go/input_tool.go: PrepareCall owns the whole untrusted-
// argument boundary; InvokableRun never reparses argsJSON, it only reads
// the sealed processStopArtifact PrepareCall already produced.
//
// The three modes map directly onto the same tool.ProcessSignal values and
// terminate-then-kill escalation shape supervisor.go's Supervisor.Shutdown
// already established through terminateOneEntry: interrupt sends
// tool.ProcessSignalInterrupt and never escalates (spec: "It does not
// terminalize the supervisor state unless the process exits"); terminate
// sends tool.ProcessSignalTerminate and escalates to
// tool.ProcessSignalKill exactly once if grace_ms elapses without a
// confirmed exit; kill sends tool.ProcessSignalKill immediately. Every
// mode's final confirmation wait blocks on the entry's own exited channel
// (entry.go: closed by doTerminalize once the terminal manifest is
// durably persisted, strictly before this call could ever read a terminal
// snapshot) rather than on a fixed timeout, so InvokableRun never renders
// a terminal status the supervisor has not itself already confirmed and
// persisted (spec: "A stop result is not successful until Sandbox
// confirms that the owned process tree has exited or returns a typed
// teardown failure"). Unlike terminateOneEntry's own unconditional final
// wait (appropriate for a whole-session Shutdown that must not return
// early), every wait here is also bounded by ctx, so a canceled tool
// invocation can never hang this call forever; a not-yet-confirmed exit is
// rendered as this process's current (non-terminal) status, never as a
// call failure.
//
// A stop request against an already-terminal process is idempotent (spec:
// "Repeating a stop operation against a terminal process is successful
// and returns the existing terminal result"): InvokableRun checks the
// entry's terminal state BEFORE ever calling Signal, so a repeated stop
// never re-signals a process that has already exited.
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

// processStopToolName is the EXACT tool name carried by every prepared
// request and shown to the model -- it MUST stay "ProcessStop" (spec
// "Ownership boundaries": Tools owns "ProcessStop").
const processStopToolName = "ProcessStop"

// The closed set of `mode` values ProcessStop accepts (spec "ProcessStop
// API": `"mode": "interrupt | terminate | kill"`).
const (
	stopModeInterrupt = "interrupt"
	stopModeTerminate = "terminate"
	stopModeKill      = "kill"
)

// validStopMode reports whether mode belongs to the closed mode domain.
func validStopMode(mode string) bool {
	switch mode {
	case stopModeInterrupt, stopModeTerminate, stopModeKill:
		return true
	default:
		return false
	}
}

const processStopSchema = `{
  "type": "object",
  "properties": {
    "process_id": {"type": "string", "description": "The owned, supervised process tree to stop."},
    "mode": {"type": "string", "enum": ["interrupt", "terminate", "kill"], "description": "interrupt sends the platform interactive interrupt and does not terminalize the process unless it exits; terminate requests graceful termination and escalates to kill once grace_ms elapses without a confirmed exit; kill immediately force-terminates the process tree."},
    "grace_ms": {"type": "integer", "minimum": 0, "description": "Bounds the grace period before terminate escalates to kill, and how long interrupt waits for a confirmed exit before returning a non-terminal status (optional; defaults to the supervisor's configured graceful-shutdown period). Ignored for kill."}
  },
  "required": ["process_id", "mode"]
}`

const processStopDesc = "Stop a supervised process tree by interrupt, graceful terminate-then-kill, or immediate kill. Waits for Sandbox to confirm the process tree has exited before reporting success; repeating a stop against an already-terminal process is a no-op that returns its existing terminal result."

// processStopArgs is the typed decode of ProcessStop's untrusted argsJSON.
// GraceMS is presence-aware (a pointer) so an explicit 0 -- "escalate/give
// up immediately, no grace period at all" -- is distinguishable from an
// omitted field, which instead defaults to the supervisor's own configured
// Config.GracefulShutdownPeriod (see PrepareCall).
type processStopArgs struct {
	ProcessID string `json:"process_id"`
	Mode      string `json:"mode"`
	GraceMS   *int   `json:"grace_ms"`
}

// processStopArtifact binds PrepareCall's validated, normalized decode of
// one ProcessStop call. InvokableRun consumes it verbatim -- the raw args
// are never reparsed.
type processStopArtifact struct {
	tool.TokenArtifact

	handle Handle
	mode   string
	grace  time.Duration
}

// processStopPrepareError is the typed preparation failure; its message is
// model-safe (mirrors output_tool.go's processOutputPrepareError).
type processStopPrepareError struct{ reason string }

func (e *processStopPrepareError) Error() string { return e.reason }

func prepareStopFail(format string, args ...any) error {
	return &processStopPrepareError{reason: fmt.Sprintf(format, args...)}
}

// ProcessStopTool implements the mutating ProcessStop tool over a
// session's shared Supervisor. supervisor and owner are resolved once, by
// the caller that constructs this value (mirrors ProcessOutputTool/
// ProcessInputTool) -- ProcessStopTool itself never touches a
// tool.SessionResourceRegistry.
type ProcessStopTool struct {
	supervisor *Supervisor
	owner      Owner
	initErr    error
}

// NewProcessStop constructs a ProcessStopTool bound to the session's
// shared supervisor and this tool's immutable process-authority owner. A
// nil supervisor is retained as a construction error and fails every call
// closed, mirroring NewProcessOutput/NewProcessInput's initErr
// convention.
func NewProcessStop(supervisor *Supervisor, owner Owner) *ProcessStopTool {
	t := &ProcessStopTool{supervisor: supervisor, owner: owner}
	if supervisor == nil {
		t.initErr = errors.New("supervisor is required")
	}
	return t
}

// Info returns ProcessStop's self-description. Name MUST equal
// "ProcessStop".
func (t *ProcessStopTool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{
		Name:   processStopToolName,
		Desc:   processStopDesc,
		Schema: json.RawMessage(processStopSchema),
	}, nil
}

// PrepareCall decodes, validates, and normalizes one ProcessStop call and
// freezes the result into a sealed processStopArtifact. Every argument is
// validated HERE; InvokableRun never re-parses argsJSON. The emitted
// Request carries no Requirements: stopping a process the caller already
// owns needs no new gate decision (spec "Identity and authorization":
// follow-up operations do not re-run the original Bash gate).
//
// An omitted grace_ms defaults to t.supervisor.cfg.GracefulShutdownPeriod
// -- the exact same duration Supervisor.Shutdown's own escalation uses
// (config.go's GracefulShutdownPeriod doc comment: "how long supervisor
// shutdown and ProcessStop's terminate mode wait before escalating to
// kill") -- rather than a fixed constant, so a session configured with a
// different grace period gets a consistent default across both paths.
func (t *ProcessStopTool) PrepareCall(_ context.Context, _ uuid.UUID, argsJSON string) (tool.Request, tool.PreparedArtifact, error) {
	if t.initErr != nil {
		return tool.Request{}, nil, t.initErr
	}

	var a processStopArgs
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return tool.Request{}, nil, prepareStopFail("invalid arguments: not a JSON object")
	}

	if a.ProcessID == "" {
		return tool.Request{}, nil, prepareStopFail("process_id is required")
	}
	handle := Handle(a.ProcessID)
	if !handle.Valid() {
		return tool.Request{}, nil, prepareStopFail("invalid process id: %q", a.ProcessID)
	}

	if !validStopMode(a.Mode) {
		return tool.Request{}, nil, prepareStopFail("mode must be interrupt, terminate, or kill, got %q", a.Mode)
	}

	grace := t.supervisor.cfg.GracefulShutdownPeriod
	if a.GraceMS != nil {
		if *a.GraceMS < 0 {
			return tool.Request{}, nil, prepareStopFail("grace_ms must be >= 0")
		}
		grace = time.Duration(*a.GraceMS) * time.Millisecond
	}

	artifact := &processStopArtifact{handle: handle, mode: a.Mode, grace: grace}
	return tool.Request{ToolName: processStopToolName}, artifact, nil
}

// InvokableRun executes the PREPARED artifact bound to this call. It never
// reparses argsJSON. A missing/cross-owner handle or a nil live process on
// a non-terminal entry both render the single bare-error shape (ProcessID
// + Error) without ever calling Signal. An already-terminal entry is
// rendered from its existing manifest without ever calling Signal
// (idempotence). Otherwise the requested mode's signal/escalation/confirm
// sequence runs, and the result is either a bare teardown_failed error (a
// Signal call itself failed) or this process's current snapshot -- terminal
// only if this call's own wait actually observed the confirmed terminal
// manifest.
func (t *ProcessStopTool) InvokableRun(ctx context.Context, _ string) (*tool.ToolResult, error) {
	if t.initErr != nil {
		return renderProcessOutputCallError(string(CodeLifetimeEnforcementUnavailable)), nil
	}
	call, ok := loop.PreparedCallFromContext(ctx)
	if !ok {
		return renderProcessOutputCallError(string(CodeInvalidArguments)), nil
	}
	art, ok := call.Artifact.(*processStopArtifact)
	if !ok || art == nil {
		return renderProcessOutputCallError(string(CodeInvalidArguments)), nil
	}

	e, found := t.supervisor.resolveEntry(t.owner, art.handle)
	if !found {
		return renderStopResult(processStopResult{ProcessID: string(art.handle), Error: string(CodeNotFound)}), nil
	}

	if closed(e.done) {
		return renderStopResult(t.snapshot(e, art.handle)), nil
	}

	if e.process == nil {
		return renderStopResult(processStopResult{ProcessID: string(art.handle), Error: string(CodeProcessSetupFailed)}), nil
	}

	var signalErr error
	switch art.mode {
	case stopModeInterrupt:
		_, signalErr = t.doInterrupt(ctx, e, art.grace)
	case stopModeTerminate:
		_, signalErr = t.doTerminate(ctx, e, art.grace)
	case stopModeKill:
		_, signalErr = t.doKill(ctx, e)
	}

	if signalErr != nil {
		return renderStopResult(processStopResult{ProcessID: string(art.handle), Error: string(CodeTeardownFailed)}), nil
	}
	return renderStopResult(t.snapshot(e, art.handle)), nil
}

// doInterrupt sends the platform interactive interrupt and waits up to
// grace for e's confirmed exit, never escalating (spec "ProcessStop API":
// "interrupt ... does not terminalize the supervisor state unless the
// process exits"). If the interrupt itself could not be delivered, this
// returns immediately without waiting -- there is nothing to wait for the
// effect of a signal that was never sent.
func (t *ProcessStopTool) doInterrupt(ctx context.Context, e *entry, grace time.Duration) (confirmed bool, signalErr error) {
	if err := e.process.Signal(ctx, tool.ProcessSignalInterrupt); err != nil {
		return false, err
	}
	return awaitExit(ctx, e.exited, grace), nil
}

// doTerminate requests graceful termination and, if e has not confirmed
// exit within grace, escalates exactly once to a forceful kill (spec:
// "terminate requests graceful termination and escalates to kill after
// grace_ms"). This mirrors supervisor.go's terminateOneEntry --
// Supervisor.Shutdown's own terminate-then-kill escalation -- but is
// scoped to this one call's own grace (not the supervisor-wide
// GracefulShutdownPeriod terminateOneEntry uses) and bounds its final
// confirmation wait by ctx rather than waiting unconditionally, so a
// canceled tool invocation can never hang this call. Every Signal error is
// collected rather than aborting the sequence early -- matching
// terminateOneEntry's own "retain authority regardless of a teardown
// hiccup" discipline -- and joined into the returned error.
func (t *ProcessStopTool) doTerminate(ctx context.Context, e *entry, grace time.Duration) (confirmed bool, signalErr error) {
	var errs []error
	if err := e.process.Signal(ctx, tool.ProcessSignalTerminate); err != nil {
		errs = append(errs, err)
	}
	if awaitExit(ctx, e.exited, grace) {
		return true, joinTeardownErrors(errs)
	}

	if err := e.process.Signal(ctx, tool.ProcessSignalKill); err != nil {
		errs = append(errs, err)
	}
	return awaitExit(ctx, e.exited, 0), joinTeardownErrors(errs)
}

// doKill immediately force-terminates the process tree and waits, bounded
// only by ctx (never by an artificial timeout: kill is not itself
// escalatable further), for Sandbox to confirm the tree has exited (spec:
// "kill immediately force-terminates the process tree"; "A stop result is
// not successful until Sandbox confirms ... exited").
func (t *ProcessStopTool) doKill(ctx context.Context, e *entry) (confirmed bool, signalErr error) {
	err := e.process.Signal(ctx, tool.ProcessSignalKill)
	return awaitExit(ctx, e.exited, 0), err
}

// awaitExit blocks until e's exited channel closes (entry.go: closed by
// doTerminalize once this process's terminal manifest is durably
// persisted -- see entry.go's exited field doc comment), ctx is done, or
// -- when bound > 0 -- bound elapses, whichever happens first. It reports
// whether exit was actually confirmed (true only for the exited case); a
// timeout or ctx cancellation is never itself surfaced as an error,
// mirroring output_tool.go's awaitTargets and input_tool.go's awaitYield:
// the caller always renders the best available (possibly still-running)
// snapshot afterward regardless of how this returns. bound <= 0 means "no
// additional bound beyond ctx" -- used for kill's and terminate's
// escalated-kill confirmation wait, where an artificial timeout would
// contradict "a stop result is not successful until Sandbox confirms
// exit" (spec).
func awaitExit(ctx context.Context, exited <-chan struct{}, bound time.Duration) bool {
	var timerC <-chan time.Time
	if bound > 0 {
		timer := time.NewTimer(bound)
		defer timer.Stop()
		timerC = timer.C
	}
	select {
	case <-exited:
		return true
	case <-ctx.Done():
		return false
	case <-timerC:
		return false
	}
}

// snapshot renders handle's current terminal-or-not status straight from
// its durable Manifest -- never from a value this call computed or
// guessed itself, exactly mirroring output_tool.go's
// applyManifestMetadata discipline. A reload failure (no manifests
// dependency, or nothing yet persisted for a bare test-built entry) leaves
// every field but ProcessID at its zero value, which json's omitempty then
// omits entirely, rather than failing this call.
func (t *ProcessStopTool) snapshot(e *entry, handle Handle) processStopResult {
	result := processStopResult{ProcessID: string(handle)}
	if t.supervisor.manifests == nil {
		return result
	}
	m, err := t.supervisor.manifests.Load(handle)
	if err != nil {
		return result
	}
	result.Status = string(m.State)
	result.ExitCode = m.Result.ExitCode
	result.Reason = m.Result.Reason
	result.StartedAt = formatManifestTime(m.StartedAt)
	result.FinishedAt = formatManifestTime(m.FinishedAt)
	return result
}

// processStopResult is the JSON shape ProcessStop renders (spec
// "ProcessStop API"): the process's current terminal-or-not status, using
// the same manifest-derived fields ProcessOutput's own result carries
// (status/exit_code/reason/started_at/finished_at), but never any
// output/cursor field -- ProcessStop never reads output. Every field but
// ProcessID is optional/omittable: a bare not_found/process_setup_failed/
// teardown_failed error carries only ProcessID and Error, and a
// non-terminal (still running, e.g. an unescalated interrupt) status
// naturally omits ExitCode/Reason/FinishedAt.
type processStopResult struct {
	ProcessID  string `json:"process_id"`
	Status     string `json:"status,omitempty"`
	ExitCode   *int   `json:"exit_code,omitempty"`
	Reason     string `json:"reason,omitempty"`
	StartedAt  string `json:"started_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
	Error      string `json:"error,omitempty"`
}

// renderStopResult marshals result into ProcessStop's model-facing JSON.
// json.Marshal can fail only for a cyclic value or an unsupported type,
// neither of which this plain, flat, all-value struct can ever contain;
// the fallback exists only to keep InvokableRun's "never returns a Go
// error" contract airtight even against that theoretical case (mirrors
// output_tool.go's renderProcessOutputResults).
func renderStopResult(result processStopResult) *tool.ToolResult {
	data, err := json.Marshal(result)
	if err != nil {
		return renderProcessOutputCallError(string(CodeProcessSetupFailed))
	}
	return tool.TextResult(string(data))
}

// compile-time assertions: ProcessStopTool is an InvokableTool and a
// CallPreparer. It is deliberately NOT a WriteTarget (it signals a process
// tree, not a filesystem path) and NOT Auditable beyond the runner's
// generic fallback (a bare tool name plus mode is a sufficient audit
// trail for a stop request).
var (
	_ tool.InvokableTool = (*ProcessStopTool)(nil)
	_ tool.CallPreparer  = (*ProcessStopTool)(nil)
)
