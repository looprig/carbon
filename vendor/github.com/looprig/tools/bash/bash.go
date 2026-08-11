// Package bash implements the Bash tool: single-command shell execution inside
// a workspace-contained working directory, with a bounded timeout and a capped
// combined-output capture. Preparation (prepare.go) owns the whole argument
// boundary and emits one typed, command-backed access request — the exact
// normalized command plus one requirement per explicitly declared
// filesystem/network delta (a request for authority, never a grant). Execution
// runs directly via `sh -c` or through an optional injected confined runner.
package bash

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/tools/internal/definition"
	"github.com/looprig/tools/internal/workspace"
	"github.com/looprig/tools/permission"
	"github.com/looprig/tools/process"
)

// bash.go implements the Bash tool: it runs a single shell command via `sh -c`
// inside a workspace-contained working directory, with a bounded timeout and a
// capped combined-output capture (design §4b, "Bash security model").
//
// DELIBERATE, DOCUMENTED EXCEPTION to CLAUDE.md's "never pass user input to
// exec.Command as a shell string" rule: a coding agent genuinely needs shell
// features (pipes, globs, &&, redirects) that an argv list cannot express, so the
// command is handed to `sh -c`. The boundary is NOT this argv shape — it is the
// PERMISSION GATE plus the injected confined runner: PrepareCall (prepare.go)
// emits the typed command-backed access request the gate decides, and the
// resulting single-spawn grant tokens travel through the PreparedCall to the
// runner (e.g. the sandbox Executor), which MAC-verifies and enforces them.
// Direct `sh -c` execution remains the bare-harness default for consumers that
// accept unconfined execution. This exception is recorded in CLAUDE.md.
//
// Failure model: a non-zero EXIT CODE is a NORMAL tool result (the model reads
// stderr + the code), NOT a Go error. A timeout is a tool-result "error: command
// timed out". Only a structural surprise (impossible to start sh) is reported as a
// tool-result error; the tool never returns a Go error.

// bashToolName is the EXACT tool name carried by every prepared request
// (tool.Request.ToolName) and shown at the gate — it MUST stay "Bash".
const bashToolName = "Bash"

// maxBashTimeout is the hard ceiling on a Bash command's wall-clock runtime. A
// caller-supplied timeout is clamped to this; it bounds resource use so a runaway
// command cannot block the agent indefinitely (CLAUDE.md: no unbounded I/O).
const maxBashTimeout = 120 * time.Second

// defaultBashTimeout is used when the caller omits (or supplies a non-positive)
// timeout. It is generous enough for typical build/test commands but bounded.
const defaultBashTimeout = 30 * time.Second

// maxBashOutputBytes caps the COMBINED stdout+stderr capture so a chatty command
// cannot exhaust memory or flood the model context. Output beyond this is dropped
// and a truncation notice is appended.
const maxBashOutputBytes = 32 * 1024 // 32 KiB

// bashShell and bashShellFlag are the interpreter and flag for the documented
// `sh -c <command>` exception. `sh` is the POSIX shell present on the host.
const (
	bashShell     = "sh"
	bashShellFlag = "-c"
)

// bashSchema is the JSON Schema for Bash's argument object. The field names
// (command/workdir/timeout/background/yield_time_ms/tty/max_output_bytes/
// access) are the model-facing contract PrepareCall decodes; nothing else
// ever parses these arguments.
const bashSchema = `{
  "type": "object",
  "properties": {
    "command": {"type": "string", "description": "The shell command to run via 'sh -c'. May use pipes, globs, redirects, and '&&'."},
    "workdir": {"type": "string", "description": "Working directory for the command, relative to the workspace or an absolute path contained within it (optional; defaults to the workspace root)."},
    "timeout": {"type": "integer", "minimum": 0, "description": "Maximum runtime in seconds (optional; default 30, hard cap 120 for a plain foreground call). 0 is valid only together with 'background' or 'yield_time_ms' and means 'run until session shutdown'."},
    "background": {"type": "boolean", "description": "Start the command under session supervision and return as soon as it is durably registered, without waiting for it to finish (optional, default false)."},
    "yield_time_ms": {"type": "integer", "minimum": 0, "description": "Optional initial wait budget in milliseconds under session supervision. If the command exits within the budget its terminal result is returned; otherwise a process handle plus the output observed so far is returned."},
    "tty": {"type": "boolean", "description": "Request a real PTY/ConPTY for the command (optional, default false). Requires 'background' or 'yield_time_ms'; failure to allocate one never falls back to pipes."},
    "max_output_bytes": {"type": "integer", "minimum": 1, "description": "Optional per-process disk retention ceiling in bytes for a supervised command; may not exceed the configured supervisor ceiling."},
    "access": {
      "type": "object",
      "description": "Optional structured access declaration for this command. It REQUESTS authority (each declared delta joins the same approval as the command); it never grants it. An omitted gated delta stays OS-blocked; after such a block, retry with a new call that declares the needed capability.",
      "properties": {
        "network": {"type": "array", "items": {"type": "object", "properties": {"transport": {"type": "string", "enum": ["tcp"], "description": "Transport (optional; tcp is the only supported value)."}, "host": {"type": "string", "description": "Exact destination hostname or IP. Omit to request a truthfully broad, exact-command-bound egress delta for the port."}, "port": {"type": "integer", "minimum": 1, "maximum": 65535}}, "required": ["port"]}},
        "read": {"type": "array", "items": {"type": "object", "properties": {"scope": {"type": "string", "enum": ["path", "tree", "host"]}, "path": {"type": "string", "description": "Path or tree root (relative paths resolve against the command's working directory). Not allowed for scope 'host', which is explicitly broad."}}, "required": ["scope"]}},
        "write": {"type": "array", "items": {"type": "object", "properties": {"scope": {"type": "string", "enum": ["path", "tree", "host"]}, "path": {"type": "string"}}, "required": ["scope"]}}
      }
    }
  },
  "required": ["command"]
}`

const bashDesc = "Run a single shell command via 'sh -c' inside the workspace. Supports pipes, globs, redirects, and '&&'. Combined stdout+stderr is captured (capped at 32 KiB) and the exit code is reported. The working directory is confined to the workspace; runtime is bounded (default 30s, max 120s). Requires approval before each command."

// bashArgs is the typed decode of Bash's untrusted argsJSON. Grant tokens are
// deliberately NOT a model-facing argument: they travel only in the
// tool.PreparedCall the runner binds to the call after the gate decision.
//
// Timeout, YieldTimeMS, and MaxOutputBytes are PRESENCE-aware (pointers): a
// call using only the pre-existing fields decodes them as nil, exactly
// preserving legacy behavior, and an explicit `timeout: 0` (only meaningful
// together with Background or a present YieldTimeMS) is distinguishable from
// an omitted timeout. Background, YieldTimeMS, and TTY are the new
// supervision-facing arguments (spec, "Bash API"); normalizeSupervision
// (prepare.go) is their single validation/normalization point.
type bashArgs struct {
	Command        string      `json:"command"`
	Workdir        string      `json:"workdir"`
	Timeout        *int        `json:"timeout"`
	Background     bool        `json:"background"`
	YieldTimeMS    *int        `json:"yield_time_ms"`
	TTY            bool        `json:"tty"`
	MaxOutputBytes *int64      `json:"max_output_bytes"`
	Access         *accessDecl `json:"access,omitempty"`
}

// BashTool runs a single shell command in a workspace-contained directory. It
// depends on the workspace root and an optional confined-execution runner;
// command policy is decided by the harness gate over the prepared request,
// against rules stored in the permission package's workspace store. A nil runner
// means direct `sh -c` execution (the bare-harness default), while an invalid
// option or typed-nil runner fails closed through a model-safe error.
type BashTool struct {
	root           string
	runner         tool.CommandRunner
	coord          tool.WorkspaceCoordinator
	obs            tool.WorkspaceObservations
	familyEligible permission.FamilyEligibility
	initErr        error

	// asyncRunner, registry, and owner are supervised.go's supervision-only
	// fields: NewSupervisedFactory is the only constructor that ever sets
	// them (a plain NewBash/NewFactory tool leaves all three at their zero
	// value, so any supervised call it receives fails closed with
	// lifetime_enforcement_unavailable — see runSupervised). asyncRunner is
	// resolved exactly once, at Build; InvokableRun never re-resolves it
	// from ctx or invocation provenance.
	asyncRunner tool.AsyncProcessRunner
	registry    tool.SessionResourceRegistry
	owner       process.Owner
}

// BashOption configures a BashTool at construction (functional-options pattern).
type BashOption func(*BashTool)

// WithRunner injects a confined command runner. When set, InvokableRun routes the
// command through r.RunCommand instead of the direct `sh -c` path. A nil runner
// (the default) preserves the exact bare-harness direct-execution behavior.
func WithRunner(r tool.CommandRunner) BashOption {
	return func(b *BashTool) { b.runner = r }
}

// WithWorkspaceCoordinator binds the session workspace coordinator so a command run
// holds the EXCLUSIVE whole-workspace mutation permit (design §"File-tool optimistic
// concurrency and binding"). A nil or typed-nil coordinator is ignored (the tool runs
// coordinator-free — the standalone/bare path).
func WithWorkspaceCoordinator(coord tool.WorkspaceCoordinator) BashOption {
	return func(b *BashTool) {
		if !workspace.IsNil(coord) {
			b.coord = coord
		}
	}
}

// WithFamilyCatalog injects the consumer's explicit eligible-prefix catalog for
// AUTOMATIC family candidate proposal (spec: unknown prefixes fail closed to an
// exact proposal). The catalog affects only which reusable candidate is
// DISPLAYED; it never widens the requirement or the issued exact-command grant.
// Nil (the default) proposes exact candidates only.
func WithFamilyCatalog(eligible permission.FamilyEligibility) BashOption {
	return func(b *BashTool) { b.familyEligible = eligible }
}

// WithObservations binds the loop's shared file-observation set so a command run
// invalidates it wholesale afterward (the changed paths are unknowable). A nil or
// typed-nil set is ignored (no invalidation).
func WithObservations(obs tool.WorkspaceObservations) BashOption {
	return func(b *BashTool) {
		if !workspace.IsNil(obs) {
			b.obs = obs
		}
	}
}

// NewBash constructs a BashTool bound to the workspace root. With no options, or
// WithRunner(nil), the tool uses direct execution. Invalid options and typed-nil
// runners are retained as initialization errors and fail closed when invoked.
func NewBash(root string, opts ...BashOption) *BashTool {
	config, initErr := resolveBashOptions(opts)
	b := newBash(root, config)
	b.initErr = initErr
	return b
}

type bashConfig struct {
	runner         tool.CommandRunner
	coord          tool.WorkspaceCoordinator
	obs            tool.WorkspaceObservations
	familyEligible permission.FamilyEligibility
}

// Factory is an immutable Bash construction blueprint. It resolves options once
// and binds per-Loop workspace services without reapplying caller closures.
type Factory func(root string, coordinator tool.WorkspaceCoordinator, observations tool.WorkspaceObservations) *BashTool

// NewFactory validates and seals Bash options for use by a definition builder.
func NewFactory(options ...BashOption) (Factory, error) {
	config, err := resolveBashOptions(options)
	if err != nil {
		return nil, err
	}
	return func(root string, coordinator tool.WorkspaceCoordinator, observations tool.WorkspaceObservations) *BashTool {
		bound := config
		bound.coord = coordinator
		bound.obs = observations
		return newBash(root, bound)
	}, nil
}

type noPermit struct{}

func (noPermit) Release() {}

func resolveBashOptions(opts []BashOption) (bashConfig, error) {
	resolved := &BashTool{}
	for _, opt := range opts {
		if opt == nil {
			return bashConfig{}, &definition.BuildError{Definition: bashToolName, Dependency: "option"}
		}
		opt(resolved)
	}
	if resolved.runner != nil && workspace.IsNil(resolved.runner) {
		return bashConfig{}, &definition.BuildError{Definition: bashToolName, Dependency: "runner"}
	}
	return bashConfig{runner: resolved.runner, coord: resolved.coord, obs: resolved.obs, familyEligible: resolved.familyEligible}, nil
}

func newBash(root string, config bashConfig) *BashTool {
	return &BashTool{root: root, runner: config.runner, coord: config.coord, obs: config.obs, familyEligible: config.familyEligible}
}

// Info returns Bash's self-description. Name MUST equal "Bash".
func (b *BashTool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{
		Name:   bashToolName,
		Desc:   bashDesc,
		Schema: json.RawMessage(bashSchema),
	}, nil
}

// AuditSummary returns the command itself — it is exactly what the user approves
// at the gate, so it is the right (and only) redacted summary. No secrets are
// added beyond the command the user already sees. An unparseable args document
// yields a generic summary.
func (b *BashTool) AuditSummary(argsJSON string) string {
	var a bashArgs
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil || a.Command == "" {
		return "Bash (unparsable args)"
	}
	return "Bash: " + a.Command
}

// resolveSpawnDir maps a relative or contained absolute workdir to the confined
// directory a Bash command runs in. Absolute inputs are first made relative to
// root, then workspace.ContainedPath remains the single symlink-aware authority
// that rejects lexical and resolved escapes. PrepareCall and InvokableRun both
// use this function so a resolution change after approval fails closed.
func resolveSpawnDir(root, workdir string) (string, error) {
	if workdir == "" {
		return root, nil
	}
	if filepath.IsAbs(workdir) {
		rel, err := filepath.Rel(root, workdir)
		if err != nil {
			return "", err
		}
		workdir = rel
	}
	return workspace.ContainedPath(root, workdir)
}

// InvokableRun executes the PREPARED artifact bound to this call — the raw
// argsJSON is never reparsed, so mutating it after preparation changes nothing;
// without its artifact the tool fails closed. The command runs through the
// bound runner with the PreparedCall's issued grant tokens (the runner MAC
// verifies them; Bash only carries the opaque strings). A non-zero exit is a
// normal result; a timeout or start failure is a tool-result error string. It
// never returns a Go error.
func (b *BashTool) InvokableRun(ctx context.Context, _ string) (*tool.ToolResult, error) {
	if b.initErr != nil {
		return tool.TextResult("error: Bash is unavailable: " + b.initErr.Error()), nil
	}
	call, ok := loop.PreparedCallFromContext(ctx)
	if !ok {
		return tool.TextResult("error: permission denied: Bash requires its prepared call artifact"), nil
	}
	art, ok := call.Artifact.(*bashArtifact)
	if !ok || art == nil {
		return tool.TextResult("error: permission denied: Bash requires its prepared call artifact"), nil
	}

	// Enforce the APPROVED spawn directory: a resolution changed between
	// prepare and run (a symlink swap) refuses the run fail-closed.
	dir, err := resolveSpawnDir(b.root, art.workdirRaw)
	if err != nil {
		return tool.TextResult("error: workdir is outside the workspace: " + art.workdirRaw), nil
	}
	if dir != art.dirAbs {
		return tool.TextResult("error: workdir resolution changed since approval: " + art.workdirRaw), nil
	}

	// A SUPERVISED call (background or yield_time_ms, frozen at preparation
	// by normalizeSupervision) routes through the shared, runner-free
	// process.Supervisor instead of the synchronous path below — see
	// supervised.go's runSupervised. Every other (legacy) call falls
	// through to the unchanged synchronous `sh -c`/injected-runner path.
	if art.supervised {
		return b.runSupervised(ctx, call, art, dir)
	}

	// Take the EXCLUSIVE whole-workspace mutation permit for the run: Bash may change
	// unknowable paths, so while it runs it excludes ALL structured path mutations and
	// every other whole/checkpoint permit session-wide. Acquire on the OUTER ctx (not
	// the command-timeout ctx below) so a slow command's timeout can't cancel an
	// already-held permit; a ctx-canceled acquire returns WITHOUT running. A nil
	// coordinator (bare path) yields a no-op permit.
	permit, err := b.acquireWhole(ctx)
	if err != nil {
		return tool.TextResult("error: " + err.Error()), nil
	}
	defer permit.Release()
	// Whichever way the run ends (success, non-zero exit, timeout, or start error) the
	// loop's ENTIRE file-observation set is invalidated, because the changed paths are
	// unknowable — Bash gains no file-level compare-and-swap. This defer fires only
	// once the command has been attempted (after a successful acquire).
	defer b.invalidateObservations()

	// Post-decision grants travel ONLY in the PreparedCall the runner bound to
	// this ctx — never in an ambient grant context or a model-facing argument.
	grants := call.Grants

	// Bound the command's runtime with the timeout validated at preparation.
	timeout := art.timeout
	ctx2, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var (
		out      string
		exitCode int
		timedOut bool
		runErr   error
	)
	// Grant-aware dispatch: when issued grants are present AND the injected
	// runner supports them, run via RunCommandWithGrants; otherwise RunCommand
	// if a runner is set, else direct sh -c. Grants present but no GrantedRunner
	// (nil runner, or a RunCommand-only runner) falls through — the tokens are
	// ignored at the exec layer (the gate already resolved the decision).
	if gr, ok := b.runner.(tool.GrantedRunner); ok && len(grants) > 0 {
		// Confined + escalated path: the injected runner (e.g. the sandbox Executor)
		// folds a timeout/cancel into err (it returns ctx.Err()); adapt its byte
		// output + error into the (output, exitCode, timedOut, startErr) shape.
		outBytes, ec, err := gr.RunCommandWithGrants(ctx2, dir, art.command, grants)
		out, exitCode, timedOut, runErr = adaptRunnerResult(ctx2, outBytes, ec, err)
	} else if b.runner != nil {
		// Confined path: same adaptation as the grants path, without the tokens.
		outBytes, ec, err := b.runner.RunCommand(ctx2, dir, art.command)
		out, exitCode, timedOut, runErr = adaptRunnerResult(ctx2, outBytes, ec, err)
	} else {
		out, exitCode, timedOut, runErr = runShellCommand(ctx2, dir, art.command)
	}
	if timedOut {
		return tool.TextResult("error: command timed out after " + timeout.String()), nil
	}
	if runErr != nil {
		// sh could not be started (not an exit-code situation). Surface as a
		// tool-result error, not a Go error.
		return tool.TextResult("error: could not run command: " + runErr.Error()), nil
	}
	return tool.TextResult(formatBashResult(out, exitCode)), nil
}

// acquireWhole takes the exclusive whole-workspace mutation permit for a command run.
// A nil coordinator (the bare/standalone path) yields a no-op permit so InvokableRun
// runs the command coordinator-free. ctx is the OUTER per-call ctx; a canceled acquire
// returns the coordinator's typed error and no permit.
func (b *BashTool) acquireWhole(ctx context.Context) (tool.WorkspacePermit, error) {
	if workspace.IsNil(b.coord) {
		return noPermit{}, nil
	}
	return b.coord.Acquire(ctx, tool.WorkspaceOperationWholeMutation, "")
}

// invalidateObservations drops the loop's entire file-observation set after a Bash
// run (a no-op when no observation set is bound).
func (b *BashTool) invalidateObservations() {
	if !workspace.IsNil(b.obs) {
		b.obs.InvalidateAll()
	}
}

// clampBashTimeout maps a caller-supplied timeout (seconds) into a bounded
// time.Duration: ≤0 → defaultBashTimeout; otherwise min(timeout, maxBashTimeout).
func clampBashTimeout(seconds int) time.Duration {
	if seconds <= 0 {
		return defaultBashTimeout
	}
	d := time.Duration(seconds) * time.Second
	if d > maxBashTimeout {
		return maxBashTimeout
	}
	return d
}

// adaptRunnerResult maps an injected runner's (output, exitCode, err) return into
// the (out, exitCode, timedOut, startErr) shape InvokableRun consumes, applying
// the DeadlineExceeded→timedOut rule: the sandbox executor folds a timeout/cancel
// into its returned err (ctx.Err()), so a deadline-exceeded err (directly or via
// the expired ctx) is a timeout, not a start error. It is shared by the plain and
// grant-carrying runner dispatch paths so both adapt identically.
func adaptRunnerResult(ctx context.Context, outBytes []byte, exitCode int, err error) (out string, code int, timedOut bool, startErr error) {
	out, code = string(outBytes), exitCode
	switch {
	case err == nil:
		// success: timedOut/startErr stay zero.
	case errors.Is(err, context.DeadlineExceeded) || ctx.Err() == context.DeadlineExceeded:
		timedOut = true
	default:
		startErr = err
	}
	return out, code, timedOut, startErr
}

// runShellCommand runs `sh -c command` in dir, capturing COMBINED stdout+stderr
// capped at maxBashOutputBytes (the cappedBuffer drops bytes past the cap and
// records the overflow). It returns (output, exitCode, timedOut, startErr):
//   - timedOut is true when ctx's deadline fired (the process was killed);
//   - startErr is non-nil only when sh could not be started (structural);
//   - a non-zero exit code is returned WITHOUT a startErr (a normal result).
func runShellCommand(ctx context.Context, dir, command string) (output string, exitCode int, timedOut bool, startErr error) {
	// #nosec G204 -- DELIBERATE, documented exception (see file header & CLAUDE.md):
	// the Bash tool runs a single human-approved command via `sh -c`; the security
	// boundary is the permission gate, not this argv shape. exec.CommandContext
	// bounds the runtime so the process is killed on timeout.
	cmd := exec.CommandContext(ctx, bashShell, bashShellFlag, command)
	cmd.Dir = dir

	var buf cappedBuffer
	buf.limit = maxBashOutputBytes
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	err := cmd.Run()
	out := buf.cappedString()

	// A deadline-exceeded context means the process was killed by the timeout.
	if ctx.Err() == context.DeadlineExceeded {
		return out, 0, true, nil
	}
	if err == nil {
		return out, 0, false, nil
	}
	// A non-zero exit is a normal result, not a start error.
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return out, exitErr.ExitCode(), false, nil
	}
	// Anything else (sh not found, permission denied to exec) is a start error.
	return out, 0, false, err
}

// formatBashResult renders the combined output and exit code into the tool-result
// text. The exit code line is always present so the model can branch on it.
func formatBashResult(output string, exitCode int) string {
	body := output
	if body != "" && body[len(body)-1] != '\n' {
		body += "\n"
	}
	return body + "[exit code: " + strconv.Itoa(exitCode) + "]"
}

// cappedBuffer is an io.Writer that accumulates up to limit bytes and then drops
// the rest, remembering whether anything was dropped. It is used as BOTH the
// stdout and stderr sink so the two streams are interleaved into one capped
// combined buffer (a write past the cap is silently truncated, never panics).
type cappedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

// Write appends as many bytes of p as fit under limit, marking truncated if any
// were dropped. It always reports len(p) written (the producer must not see a
// short write / error) so the command keeps running to completion.
func (c *cappedBuffer) Write(p []byte) (int, error) {
	remaining := c.limit - c.buf.Len()
	if remaining <= 0 {
		if len(p) > 0 {
			c.truncated = true
		}
		return len(p), nil
	}
	if len(p) > remaining {
		c.buf.Write(p[:remaining])
		c.truncated = true
		return len(p), nil
	}
	c.buf.Write(p)
	return len(p), nil
}

// cappedString returns the captured output, appending a single truncation notice
// line when bytes were dropped past the cap.
func (c *cappedBuffer) cappedString() string {
	s := c.buf.String()
	if c.truncated {
		if s != "" && s[len(s)-1] != '\n' {
			s += "\n"
		}
		s += "[output truncated at " + strconv.Itoa(c.limit) + " bytes]"
	}
	return s
}

// compile-time assertions: BashTool is an InvokableTool and Auditable. It is
// NOT a WriteTarget (it is not a path-write tool).
var (
	_ tool.InvokableTool = (*BashTool)(nil)
	_ tool.Auditable     = (*BashTool)(nil)
)
