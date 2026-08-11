package loopruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	gatedomain "github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/hook"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

// runner.go is the heart of the agentic loop: RunBatch resolves permissions
// (prompting via gates), batches execution (serial then bounded-parallel with
// same-WriteTarget serialization), wraps each call in the middleware chain,
// recovers panics, and emits exactly one ToolCallStarted + one ToolCallCompleted
// per requested call whatever its fate. It is concurrency-correct and
// fail-secure: any permission ambiguity → the call is not executed.

// Pre-execution / failure tool-result strings. Every tool failure becomes a
// model-visible tool-result STRING (the contract) — never an aborted turn. The
// prefixes are named constants so they read consistently in the stream and in
// tests.
const (
	errToolPrefix        = "error: " // the common prefix every tool-error string carries
	errPrefixUnknownTool = "error: unknown tool: "
	errInvalidArgs       = "error: invalid tool arguments (not valid JSON)"
	errPermissionDenied  = "error: permission denied"
	errEmptyResult       = "error: empty result"
	errWriteTargetPrefix = "error: invalid tool arguments: "
	// errIDGenFailure is the fail-secure tool-result for a call whose ToolExecutionID could
	// not be minted (crypto/rand failure): the call is NOT executed and NO gate is
	// opened (a missing ToolExecutionID can't safely route a permission gate), but the model
	// still sees a paired error result.
	errIDGenFailure = "error: internal: could not generate call id"
	// errPreparePrefix is the fail-secure tool-result prefix for a call whose
	// CallPreparer.PrepareCall failed or returned an invalid request: the call is
	// NOT executed and NO gate is opened (a failed preparation can't safely gate
	// or run the call), but the model still sees a paired error result.
	errPreparePrefix = "error: tool preparation failed: "
	// errToolUnprepared is the fail-closed tool-result for an effectful tool that
	// implements no preparation step. Without a typed prepared request the gate
	// has nothing truthful to decide, so the call is never evaluated or executed.
	errToolUnprepared = "error: permission denied: tool has no call preparation"
	// errPrepareBinding is the fail-closed tool-result for a prepared request
	// bound to a different execution ID than the one the runner minted.
	errPrepareBinding       = "error: tool preparation failed: request is not bound to this execution"
	errToolHookDeniedPrefix = "error: tool call denied: "
	errToolHookFailure      = "error: internal: tool call blocked by hook"
	errToolCanceled         = "error: tool call canceled"
	errToolPanicRedacted    = "error: tool panicked" // #nosec G101 -- fixed redacted error text, not a credential
	errToolDependencyFailed = "error: internal: tool call failed"
)

// ResultPreview caps. ResultPreview may hold a slice of tool output, so it is
// capped by BOTH a byte budget and a line budget, with a visible truncation
// marker when exceeded.
const (
	previewMaxBytes  = 2 * 1024 // ~2 KiB
	previewMaxLines  = 20
	truncationMarker = "… [truncated]"
	// diagnosticMaxBytes bounds tool-controlled diagnostics before they reach
	// model-visible results, audit summaries, or structured logs.
	diagnosticMaxBytes = 1024
)

type permissionDenialCategory string

const (
	permissionDenialOutOfScope    permissionDenialCategory = "out of scope"
	permissionDenialNotAuthorized permissionDenialCategory = "not authorized"
	permissionDenialUnavailable   permissionDenialCategory = "unavailable"
)

// result is the package-private outcome of one tool call. Results are returned in
// the SAME ORDER as the requested calls (the model pairs tool_use↔tool_result by
// position/ID), and each carries its originating ToolUseBlock.ID so runTurn can
// build the paired ToolResultMessage.
type result struct {
	ToolExecutionID uuid.UUID
	ToolUseID       string
	Content         []content.Block
	IsError         bool
}

type toolExecutionHookError struct{}

func (*toolExecutionHookError) Error() string { return "loop: tool execution failed" }

type runtimeInvariantPanic struct{ value any }

// resolved is the runner's per-call working state, threaded from resolution
// through execution. It is built once per requested call in call order.
type resolved struct {
	ctx     context.Context
	callID  uuid.UUID
	block   content.ToolUseBlock
	argsstr string

	t       tool.InvokableTool // nil for an unknown tool
	summary string             // ToolCallStarted.Summary (redacted)

	// prepared is the per-call prepared execution contract: the minted execution
	// ID, the typed access Request and opaque artifact CallPreparer.PrepareCall
	// produced ONCE in newResolved, and — after resolveAccess — the fresh grant
	// tokens the combined gate issued for THIS call. Tokens travel only inside
	// this contract, never in an ambient grant context.
	prepared tool.PreparedCall

	// prompted records that resolveAccess opened an interactive gate for this
	// call (so the non-gated PermissionDecided audit is not emitted twice).
	prompted bool

	// failed marks a pre-execution failure (unknown tool, invalid args,
	// permission denied, WriteTarget error). Its result is fixed before any
	// execution begins; it never reaches runOne.
	failed    bool
	failedMsg string

	// writeKey groups calls that must serialize relative to each other (same
	// resolved write target). Empty when the call has no write target.
	writeKey string
	hasWrite bool

	sequential bool

	hookCall       hook.Call
	finishToolCall hook.FinishFunc
	terminalErr    error
	hooks          *hook.Runner

	permissionEffect event.PermissionDecisionEffect
	permissionReason string
	hookPreview      string
	finishOnce       sync.Once
	finished         atomic.Bool
}

// BatchRuntime carries the runtime-owned services and attribution shared by one
// batch. EmitContext is preferred when the caller can preserve operation
// context; Emit remains the context-free compatibility seam used by focused
// tests. Zero values are safe: UUID generation defaults to uuid.New, event
// emission is discarded, and a nil hook runner is a no-op.
type BatchRuntime struct {
	GateRegistrations chan<- gateRegistration
	IDGen             func() (uuid.UUID, error)
	Emit              func(event.Event)
	EmitContext       eventEmitter
	Hooks             *hook.Runner
	Coordinates       identity.Coordinates
	AgentName         identity.AgentName
	Cause             identity.Cause
}

// RunBatch executes a batch of tool calls. It mints a ToolExecutionID per call via runtime.IDGen
// (fail-secure: a call whose ToolExecutionID cannot be minted is NOT executed and NO gate
// is opened for it), resolves tools + permissions sequentially (so a session grant
// on call N is visible to call N+1's Check), emits ALL ToolCallStarted before
// executing any call, runs the executable calls (serial batch drained first, then
// bounded-parallel with same-WriteTarget serialization), and emits one
// ToolCallCompleted per call. The returned []result is in call order, each result
// owning its slot by index.
//
// RunBatch SERIALIZES its own event emission: it wraps emit in an internal mutex
// (safeEmit) and uses that for every Started and every (possibly concurrent)
// Completed. The caller's emit therefore need NOT be concurrent-safe.
func RunBatch(
	ctx context.Context,
	calls []content.ToolUseBlock,
	ts ToolSet,
	runtime BatchRuntime,
) []result {
	if runtime.IDGen == nil {
		runtime.IDGen = uuid.New
	}
	emitContext := runtime.EmitContext
	if emitContext == nil {
		emit := runtime.Emit
		if emit == nil {
			emit = func(event.Event) {}
		}
		emitContext = func(_ context.Context, value event.Event) { emit(value) }
	}
	// safeEmit serializes all event emission so the caller's emit need not be
	// concurrent-safe (the parallel executor calls Completed from many goroutines).
	var emitMu sync.Mutex
	safeEmit := func(ctx context.Context, ev event.Event) {
		emitMu.Lock()
		defer emitMu.Unlock()
		defer func() {
			if recovered := recover(); recovered != nil {
				panic(runtimeInvariantPanic{value: recovered})
			}
		}()
		emitContext(ctx, ev)
	}

	rs := make([]*resolved, len(calls))
	defer func() {
		if recovered := recover(); recovered != nil {
			for _, r := range rs {
				if r != nil {
					r.finishInvariantFailure()
				}
			}
			if invariant, ok := recovered.(runtimeInvariantPanic); ok {
				panic(invariant.value)
			}
			panic(recovered)
		}
	}()
	for i, c := range calls {
		callID, idErr := mintToolExecutionID(runtime.IDGen)
		rs[i] = newResolved(ctx, c, ts, callID, idErr, runtime)
	}

	// Sequential access resolution, in call order, BEFORE any execution, so a
	// workspace rule persisted by call N's Approve-always is visible to call
	// N+1's evaluation. A ctx cancel during a gate wait tears the whole batch
	// down: return what we have (runTurn's rollback discards a cancelled batch's
	// results).
	for _, r := range rs {
		if r.failed || r.t == nil {
			continue
		}
		if err := resolveAccessSafely(r.ctx, r, ts, runtime.GateRegistrations, safeEmit); err != nil {
			if r.ctx.Err() != nil {
				for _, pending := range rs {
					if !pending.failed {
						pending.failWith(errToolCanceled, r.ctx.Err())
					}
				}
				break
			}
			var captureErr *reviewContextCaptureError
			if errors.As(err, &captureErr) {
				// This batch's review-context capture (see review_context.go)
				// was attempted and failed closed while opening a permission
				// gate. Abort the WHOLE batch here, before the emit-Started /
				// execute phases below: any call already resolved Approved
				// earlier in this loop (e.g. a non-gated call ahead of the
				// failing one) must never actually run once the turn is about
				// to fail — a per-call denial alone would let it execute
				// despite the turn as a whole failing closed.
				return collectResults(rs)
			}
			// Any other non-ctx error is fail-closed: resolveAccess has
			// already marked r.failed (denied), so the call is never executed.
		}
	}

	// Emit ALL ToolCallStarted for the batch BEFORE executing any call — every
	// requested call (including pre-execution failures) gets a Started, and every
	// Started precedes every Completed so the TUI groups the batch race-free.
	for _, r := range rs {
		safeEmit(r.ctx, event.ToolCallStarted{ToolExecutionID: r.callID, ToolName: r.block.Name, Summary: r.summary})
	}

	// Each call owns final[i] by index: serial and parallel goroutines each write a
	// DISTINCT index, so result storage needs no shared map and no mutex, and the
	// outcome is already in call order. (A ToolExecutionID-keyed map could drop/duplicate on
	// a zero-key collision from a failed mint; indexing removes that hazard.)
	final := make([]result, len(rs))

	complete := func(i int, resolvedCall *resolved, callResult result) {
		final[i] = callResult
		preview, isErr := previewOf(callResult)
		safeEmit(resolvedCall.ctx, event.ToolCallCompleted{ToolExecutionID: callResult.ToolExecutionID, IsError: isErr, ResultPreview: preview})
		resolvedCall.finish(callResult)
	}

	// Pre-execution failures complete immediately, in the Started order. executable
	// carries each call's own index so its executor writes the right slot.
	var executable []indexedResolved
	for i, r := range rs {
		if r.failed {
			complete(i, r, failureResult(r))
			continue
		}
		executable = append(executable, indexedResolved{i: i, r: r})
	}

	execute(executable, ts, runtime, safeEmit, complete)

	return final
}

func mintToolExecutionID(idGen func() (uuid.UUID, error)) (id uuid.UUID, err error) {
	defer func() {
		if recover() != nil {
			id = uuid.UUID{}
			err = &operationHookPanicError{Operation: hook.OperationToolCall}
		}
	}()
	return idGen()
}

// indexedResolved pairs an executable call with the result slot it owns, so each
// executor (serial or parallel) writes a distinct final[i] with no shared state.
type indexedResolved struct {
	i int
	r *resolved
}

// newResolved builds the per-call working state around an already-minted
// ToolExecutionID (or its minting error), starts the ToolCall hook before any
// resolution side effect, looks up the tool, validates args JSON, runs
// CallPreparer.PrepareCall once, queries WriteTarget, and computes the redacted
// Summary. Pre-execution failures are recorded here; permission is resolved
// later (sequentially).
//
// An idGen error is fail-secure: the call is marked failed with errIDGenFailure
// (so it is NOT executed and NO gate is opened — a missing ToolExecutionID can't safely
// route a gate) and the error is NOT swallowed (it is surfaced as a model-visible
// tool-result and logged). The zero ToolExecutionID it then carries is harmless: a failed
// call neither opens a gate nor shares a result slot (results are indexed).
func newResolved(
	ctx context.Context,
	c content.ToolUseBlock,
	ts ToolSet,
	callID uuid.UUID,
	idErr error,
	runtime BatchRuntime,
) (r *resolved) {
	call := hook.Call{
		Operation:   hook.OperationToolCall,
		StartedAt:   time.Now(),
		Coordinates: runtime.Coordinates,
		AgentName:   runtime.AgentName,
		Cause:       runtime.Cause,
		ToolCall: &hook.ToolCallData{
			ToolExecutionID: callID,
			ToolUseID:       c.ID,
			ToolName:        c.Name,
			Summary:         boundedDiagnostic(c.Name),
			ArgsJSON:        append([]byte(nil), c.Input...),
		},
	}
	hookCtx, finish, hookErr := runtime.Hooks.Start(ctx, call)
	r = &resolved{
		ctx: hookCtx, callID: callID, block: c, argsstr: string(c.Input),
		summary: boundedDiagnostic(c.Name), hookCall: call, finishToolCall: finish, hooks: runtime.Hooks,
	}
	defer func() {
		if recover() != nil {
			r.failWith(errToolDependencyFailed, &operationHookPanicError{Operation: hook.OperationToolCall})
		}
	}()
	if hookErr != nil {
		if denial, denied := hook.AsDenial(hookErr); denied {
			r.terminalErr = hookErr
			r.fail(errToolHookDeniedPrefix + denial.Reason)
		} else {
			r.terminalErr = hookErr
			r.fail(errToolHookFailure)
		}
		return r
	}

	if idErr != nil {
		slog.Error("loop: tool-call id generation failed; failing call fail-secure (not executed, no gate)",
			"tool", boundedDiagnostic(c.Name), "error", boundedDiagnostic(safeErrorText(idErr)))
		r.summary = boundedDiagnostic(c.Name) // no ToolExecutionID → Summary is just the requested name
		r.failWith(errIDGenFailure, idErr)
		return r
	}

	r.t = lookupTool(r.ctx, ts.Registry, c.Name)
	if r.t == nil {
		r.summary = boundedDiagnostic(c.Name) // no tool → Summary is just the requested name
		r.fail(boundedDiagnostic(errPrefixUnknownTool + c.Name))
		return r
	}

	r.summary = summaryOf(r.t, c.Name, r.argsstr)

	if !json.Valid(c.Input) {
		r.fail(errInvalidArgs)
		return r
	}

	// Preparation happens ONCE here — after the callID is minted, the tool
	// resolved, and args validated — bound to the call by the minted execution
	// ID. The tool decodes/normalizes its own arguments and returns the typed
	// access Request plus its opaque per-call artifact; both are threaded to the
	// permission evaluation and to execution via the prepared execution
	// contract. An effectful tool WITHOUT a preparation step fails closed: the
	// gate has nothing truthful to decide, so the call is never evaluated or
	// executed. A PrepareCall error, an invalid request, or a request bound to a
	// different execution ID is equally fail-secure (not executed, no gate),
	// surfaced as a model-visible tool-result rather than swallowed.
	preparer, ok := r.t.(tool.CallPreparer)
	if !ok {
		slog.Warn("loop: tool has no call preparation; failing call fail-closed (not evaluated, not executed)",
			"tool", c.Name)
		r.fail(errToolUnprepared)
		return r
	}
	request, artifact, err := preparer.PrepareCall(r.ctx, r.callID, r.argsstr)
	if err != nil {
		detail := safeErrorText(err)
		slog.Warn("loop: tool PrepareCall failed; failing call fail-secure (not executed, no gate)",
			"tool", boundedDiagnostic(c.Name), "error", boundedDiagnostic(detail))
		r.fail(boundedDiagnostic(errPreparePrefix + detail))
		return r
	}
	if err := tool.ValidateRequest(request); err != nil {
		detail := safeErrorText(err)
		slog.Warn("loop: tool prepared an invalid request; failing call fail-secure (not executed, no gate)",
			"tool", boundedDiagnostic(c.Name), "error", boundedDiagnostic(detail))
		r.fail(boundedDiagnostic(errPreparePrefix + detail))
		return r
	}
	if request.ExecutionID != "" && request.ExecutionID != r.callID.String() {
		slog.Warn("loop: prepared request bound to a different execution id; failing call fail-secure",
			"tool", c.Name)
		r.fail(errPrepareBinding)
		return r
	}
	r.prepared = tool.PreparedCall{ExecutionID: r.callID, Request: request, Artifact: artifact}

	if wt, ok := r.t.(tool.WriteTarget); ok {
		key, has, err := wt.WriteTarget(r.argsstr)
		if err != nil {
			r.fail(boundedDiagnostic(errWriteTargetPrefix + safeErrorText(err)))
			return r
		}
		r.writeKey, r.hasWrite = key, has
	}

	if sq, ok := r.t.(tool.Sequential); ok {
		r.sequential = sq.Sequential()
	}
	return r
}

func resolveAccessSafely(
	ctx context.Context,
	r *resolved,
	ts ToolSet,
	gateReg chan<- gateRegistration,
	emit eventEmitter,
) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if invariant, ok := recovered.(runtimeInvariantPanic); ok {
				panic(invariant.value)
			}
			panicErr := &operationHookPanicError{Operation: hook.OperationToolCall}
			r.failWith(errToolDependencyFailed, panicErr)
			err = panicErr
		}
	}()
	return resolveAccess(ctx, r, ts, gateReg, emit)
}

// fail marks r as a pre-execution failure with the given model-visible message.
func (r *resolved) fail(msg string) {
	r.failed = true
	r.failedMsg = msg
}

func (r *resolved) failWith(msg string, err error) {
	r.fail(msg)
	r.terminalErr = err
}

func (r *resolved) finish(terminal result) {
	r.finishOnce.Do(func() {
		preview, isErr := previewOf(terminal)
		if r.hookPreview != "" {
			preview = r.hookPreview
		}
		call := r.hookCall
		call.ToolCall.Summary = r.summary
		call.ToolCall.PermissionEffect = r.permissionEffect
		call.ToolCall.PermissionReason = r.permissionReason
		call.ToolCall.Result = &tool.ToolResult{Content: append([]content.Block(nil), terminal.Content...)}
		call.ToolCall.ResultPreview = preview
		call.ToolCall.IsError = isErr
		outcome := hook.OutcomeCompleted
		if r.terminalErr != nil {
			outcome = hookOutcome(r.ctx, r.terminalErr)
		}
		finishHook(r.finishToolCall, call, outcome, r.terminalErr)
		r.finished.Store(true)
	})
}

func (r *resolved) finishInvariantFailure() {
	if r.finished.Load() {
		return
	}
	r.failWith(errToolDependencyFailed, &operationHookPanicError{Operation: hook.OperationToolCall})
	r.finish(errResult(r, errToolDependencyFailed))
}

// lookupTool resolves a tool by its Info(ctx).Name. Returns nil for an unknown
// name (so the caller produces a tool-result error, never a panic).
func lookupTool(ctx context.Context, registry []tool.InvokableTool, name string) tool.InvokableTool {
	for _, t := range registry {
		info, err := t.Info(ctx)
		if err != nil || info == nil {
			continue
		}
		if info.Name == name {
			return t
		}
	}
	return nil
}

// summaryOf returns the redacted ToolCallStarted.Summary: via Auditable when the
// tool implements it (it must tolerate invalid JSON, yielding just the name), else
// the tool name. Summary is NEVER built from raw args.
func summaryOf(t tool.InvokableTool, name, argsJSON string) string {
	if a, ok := t.(tool.Auditable); ok {
		return boundedDiagnostic(a.AuditSummary(argsJSON))
	}
	return boundedDiagnostic(name)
}

func safeErrorText(err error) (text string) {
	if err == nil {
		return ""
	}
	defer func() {
		if recover() != nil {
			text = "internal error"
		}
	}()
	return err.Error()
}

func boundedDiagnostic(value string) string {
	value = strings.ToValidUTF8(value, "�")
	var sanitized strings.Builder
	sanitized.Grow(min(len(value), diagnosticMaxBytes))
	for _, r := range value {
		if unicode.IsControl(r) {
			r = ' '
		}
		sanitized.WriteRune(r)
	}
	value = sanitized.String()
	if len(value) <= diagnosticMaxBytes {
		return value
	}

	budget := diagnosticMaxBytes - len(truncationMarker)
	cut := 0
	for cut < len(value) {
		_, size := utf8.DecodeRuneInString(value[cut:])
		if cut+size > budget {
			break
		}
		cut += size
	}
	return value[:cut] + truncationMarker
}

func permissionDeniedMessage(category permissionDenialCategory, description string) string {
	message := errPermissionDenied + " [" + string(category) + "]"
	if description != "" {
		message += ": " + description
	}
	return boundedDiagnostic(message)
}

func permissionDeniedForResolution(resolution gatedomain.Resolution) string {
	switch resolution.Denial {
	case gatedomain.DenialStructural:
		return permissionDeniedMessage(permissionDenialOutOfScope, resolution.DenialDescription)
	case gatedomain.DenialRefused:
		return permissionDeniedMessage(permissionDenialNotAuthorized, resolution.DenialDescription)
	default:
		return permissionDeniedMessage(permissionDenialNotAuthorized, "")
	}
}

func permissionDeniedForError(err error) string {
	var evaluationError *gatedomain.EvaluationError
	if errors.As(err, &evaluationError) && evaluationError.Kind == gatedomain.EvaluationApprovalRequired {
		return permissionDeniedMessage(permissionDenialNotAuthorized, "")
	}
	return permissionDeniedMessage(permissionDenialUnavailable, "")
}

// resolveAccess runs one (resolvable) call's prepared request through the
// combined access gate exactly once. The gate evaluates every requirement,
// opens at most ONE interactive approval (routed back through this runner's
// per-call approval capability, so the register→ack→emit→block gate plumbing is
// reused), resolves the chosen action, and issues fresh execution-bound grant
// tokens, which are recorded on the prepared execution contract for runOne.
//
// Fail-closed everywhere: no gate wired, an unapproved resolution, or any
// evaluator/approver error all mark the call permission-denied and it is never
// executed. A returned non-nil error is either ctx.Err() (batch torn down) or a
// fail-closed denial already recorded on r.
func resolveAccess(
	ctx context.Context,
	r *resolved,
	ts ToolSet,
	gateReg chan<- gateRegistration,
	emit eventEmitter,
) error {
	if ts.Access == nil {
		// No access gate wired → fail-secure: deny rather than fall through.
		emitAccessDecided(ctx, r, event.PermissionEffectDeny, "access_gate_missing", emit)
		r.fail(permissionDeniedMessage(permissionDenialUnavailable, ""))
		return nil
	}

	// Install the per-call approval capability so an INTERACTIVE evaluator can
	// open its (single) combined gate through this loop's gate machinery. A
	// headless evaluator never reads it.
	actx := WithApprovalRequester(ctx, approvalRequesterFor(r, gateReg, emit))
	resolution, err := ts.Access.Authorize(actx, r.prepared.Request)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		slog.Warn("loop: access authorization failed; failing call fail-closed (not executed)",
			"tool", boundedDiagnostic(r.block.Name), "error", boundedDiagnostic(safeErrorText(err)))
		if !r.prompted {
			emitAccessDecided(ctx, r, event.PermissionEffectDeny, "access_error", emit)
		}
		r.fail(permissionDeniedForError(err))
		return err
	}
	if !resolution.Approved {
		if !r.prompted {
			emitAccessDecided(ctx, r, event.PermissionEffectDeny, "access_denied", emit)
		}
		r.fail(permissionDeniedForResolution(resolution))
		return nil
	}
	if !r.prompted {
		emitAccessDecided(ctx, r, event.PermissionEffectApprove, "access_evaluated", emit)
	}
	// Fresh grants issued for THIS call travel on the prepared execution
	// contract (never an ambient ctx carrier, never a durable record).
	r.prepared.Grants = resolution.Grants
	return nil
}

// emitAccessDecided emits the redacted non-gated decision audit (an interactive
// prompt path emits PermissionRequested instead).
func emitAccessDecided(
	ctx context.Context,
	r *resolved,
	effect event.PermissionDecisionEffect,
	reason string,
	emit eventEmitter,
) {
	r.permissionEffect = effect
	r.permissionReason = reason
	emit(ctx, event.PermissionDecided{
		ToolExecutionID: r.callID,
		Effect:          effect,
		Reason:          reason,
		Subject:         r.block.Name,
		Audit:           r.summary,
	})
}

// approvalRequesterFor builds the per-call approval capability an interactive
// evaluator invokes at most once: it opens ONE combined permission gate
// (ctx-aware register → ack → emit → block, mirroring RequestUserInput),
// validates the reply's routing, and maps the durable command wire to the
// exact approval action.
func approvalRequesterFor(
	r *resolved,
	gateReg chan<- gateRegistration,
	emit eventEmitter,
) loop.ApprovalRequestFunc {
	return func(ctx context.Context, prompt gatedomain.ApprovalPrompt) (gatedomain.ApprovalAction, error) {
		// Triggered BEFORE r.prompted is set: this is the earliest point a
		// permission gate is genuinely about to open, so it is also the
		// earliest (and only) point this batch's lazy review-context capture
		// (see review_context.go) is attempted. A capture failure here must
		// refuse the gate exactly like any other fail-closed access error — and
		// because r.prompted stays false, resolveAccess's fail path still emits
		// a PermissionDecided audit for this call instead of silently dropping
		// it.
		reviewContext, err := reviewContextForApproval(ctx)
		if err != nil {
			return "", err
		}
		r.prompted = true
		displayed := displayedRequest(prompt)

		// reply is buffered(1) (runner is the sole reader, so the actor's routed
		// send never blocks). ack carries the session-minted GateID or the
		// prepare/activate error.
		reply := make(chan command.Command, 1)
		ack := make(chan gateInstallAck, 1)
		g := stampGateSubjectProvenance(ctx, permissionGate(r.callID, displayed))
		payload := gatedomain.PermissionPayload{Request: displayed}

		select {
		case gateReg <- gateRegistration{
			ctx: ctx, gate: g, payload: payload, reviewContext: reviewContext,
			callID: r.callID, reply: reply, kind: gatePermission, ack: ack,
		}:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		var installed gateInstallAck
		select {
		case installed = <-ack:
			if installed.err != nil {
				return "", installed.err
			}
		case <-ctx.Done():
			return "", ctx.Err()
		}

		// Install-before-emit: only now is the gate guaranteed installed, so the
		// matching Approve/Deny cannot be dropped on a race.
		emit(ctx, event.PermissionRequested{ToolExecutionID: r.callID, Request: displayed})

		g.ID = installed.gateID
		waitCtx, waitCall, finishWait, waitErr := startGateWaitWithRunner(ctx, r.hookCall, g, r.hooks)
		if waitErr != nil {
			return "", waitErr
		}
		select {
		case cmd := <-reply:
			action, err := approvalActionFromCommand(cmd, r.callID, installed.gateID)
			answer := &gatedomain.Answer{
				GateID: installed.gateID,
				Action: string(action),
				Source: gateResponseSource(cmd),
			}
			finishGateWait(finishWait, waitCall, answer, err)
			if err == nil {
				if action == gatedomain.ApprovalDeny {
					r.permissionEffect, r.permissionReason = event.PermissionEffectDeny, "permission_denied"
				} else {
					r.permissionEffect, r.permissionReason = event.PermissionEffectApprove, "permission_approved"
				}
			}
			return action, err
		case <-waitCtx.Done():
			waitErr := waitCtx.Err()
			if ctx.Err() == nil {
				if err := abandonInstalledGate(ctx, waitCtx, gateReg, installed.gateID); err != nil {
					finishGateWait(finishWait, waitCall, nil, err)
					return "", err
				}
			}
			finishGateWait(finishWait, waitCall, nil, waitErr)
			return "", waitErr
		}
	}
}

// approvalActionFromCommand maps a routed gate reply to the exact approval
// action. runLoop already matched by ToolExecutionID + kind; the routing is
// re-validated as cheap defence in depth. ApproveToolCall carries exactly one
// of the two approve actions (the codec validates that); anything else —
// including an unexpected command kind — is fail-closed Deny.
func approvalActionFromCommand(cmd command.Command, callID uuid.UUID, gateID gatedomain.ID) (gatedomain.ApprovalAction, error) {
	switch c := cmd.(type) {
	case command.ApproveToolCall:
		if !c.GateRoute.GateID.IsZero() && c.GateRoute.GateID != gateID {
			return "", &GateReplyMismatchError{ToolExecutionID: callID}
		}
		if c.GateToolExecutionID() != callID {
			return gatedomain.ApprovalDeny, nil
		}
		switch c.Action {
		case gatedomain.ApprovalApprove, gatedomain.ApprovalApproveAlwaysWorkspace:
			return c.Action, nil
		default:
			return gatedomain.ApprovalDeny, nil
		}
	case command.DenyToolCall:
		if !c.GateRoute.GateID.IsZero() && c.GateRoute.GateID != gateID {
			return "", &GateReplyMismatchError{ToolExecutionID: callID}
		}
		return gatedomain.ApprovalDeny, nil
	default:
		// Unexpected command kind on a permission gate — fail-secure.
		return gatedomain.ApprovalDeny, nil
	}
}

// displayedRequest narrows the prompt's typed request to exactly what the
// approval displays and the journal records: the unmet requirements (each
// carrying its reusable candidates) under the original execution binding. It
// never contains raw args, and tool.Request has no token field to leak.
func displayedRequest(prompt gatedomain.ApprovalPrompt) tool.Request {
	displayed := prompt.Request.Clone()
	displayed.Requirements = prompt.Unmet
	return displayed
}

// execute runs the executable calls: the serial batch (Sequential()==true) drains
// first, then the parallel batch runs bounded by a semaphore of width
// MaxParallelToolCalls, with same-WriteTarget calls serialized in call order. Each
// finished call is reported via complete(index, result) so it lands in its own
// result slot. execute does not return until every executable call has completed.
func execute(
	executable []indexedResolved,
	ts ToolSet,
	runtime BatchRuntime,
	emit eventEmitter,
	complete func(int, *resolved, result),
) {
	var serial, parallel []indexedResolved
	for _, ir := range executable {
		if ir.r.sequential {
			serial = append(serial, ir)
		} else {
			parallel = append(parallel, ir)
		}
	}

	// Serial batch drains fully first (in call order).
	for _, ir := range serial {
		complete(ir.i, ir.r, runOne(ir.r.ctx, ir.r, ts, runtime, emit))
	}

	if len(parallel) == 0 {
		return
	}

	// Per-WriteTarget-key mutex so same-key calls serialize (in call order)
	// even within the parallel batch.
	keyLocks := make(map[string]*sync.Mutex)
	for _, ir := range parallel {
		if ir.r.hasWrite {
			if _, ok := keyLocks[ir.r.writeKey]; !ok {
				keyLocks[ir.r.writeKey] = &sync.Mutex{}
			}
		}
	}

	// Semaphore bounds peak concurrency to MaxParallelToolCalls.
	sem := make(chan struct{}, resolveMaxParallelToolCalls(ts.MaxParallelToolCalls))
	panics := make(chan any, len(parallel))
	var wg sync.WaitGroup
	for _, ir := range parallel {
		wg.Add(1)
		go func(ir indexedResolved) {
			defer wg.Done()
			defer func() {
				if recovered := recover(); recovered != nil {
					panics <- recovered
				}
			}()
			sem <- struct{}{}
			defer func() { <-sem }()

			if ir.r.hasWrite {
				// Intentional: a same-key goroutine holds its semaphore slot while
				// blocked on the key mutex. Correctness is preserved (concurrency is
				// still capped and same-key calls still serialize); the only effect is
				// a transient throughput nuance (a blocked slot could otherwise admit
				// another call), not a bug.
				lk := keyLocks[ir.r.writeKey]
				lk.Lock()
				defer lk.Unlock()
			}
			complete(ir.i, ir.r, runOne(ir.r.ctx, ir.r, ts, runtime, emit))
		}(ir)
	}
	wg.Wait()
	select {
	case recovered := <-panics:
		panic(recovered)
	default:
	}
}

// runOne executes a single resolved call: builds the per-call ctx (ToolExecutionID + emit +
// gateReg + the per-call prepared artifact injected so the tool can emit / request
// user input / read back its prepared artifact), wraps InvokableRun in the middleware
// chain (first listed = outermost), recovers a panic into an error result, and
// normalizes the outcome to a result. It never aborts the batch.
func runOne(
	ctx context.Context,
	r *resolved,
	ts ToolSet,
	runtime BatchRuntime,
	emit eventEmitter,
) (res result) {
	ctx2 := WithPreparedCall(withGateReg(withContextEmit(withToolUseID(withCallID(ctx, r.callID), r.block.ID), emit), runtime.GateRegistrations), r.prepared)
	ctx2 = WithUserInputRequester(ctx2, RequestUserInput)
	ctx2 = withOperationHookRuntime(ctx2, operationHookRuntime{
		hooks: runtime.Hooks, coordinates: runtime.Coordinates,
		agentName: runtime.AgentName, cause: runtime.Cause,
	})

	executionCall := hook.Call{
		Operation:   hook.OperationToolExecution,
		StartedAt:   time.Now(),
		Coordinates: runtime.Coordinates,
		AgentName:   runtime.AgentName,
		Cause:       runtime.Cause,
		ToolExecution: &hook.ToolExecutionData{
			ToolExecutionID: r.callID,
			ToolUseID:       r.block.ID,
			ToolName:        r.block.Name,
			ArgsJSON:        append([]byte(nil), r.block.Input...),
		},
	}
	execCtx, finishExecution, startErr := runtime.Hooks.Start(ctx2, executionCall)
	if startErr != nil {
		res = errResult(r, errToolHookFailure)
		finishExecutionHook(finishExecution, executionCall, res, startErr)
		return res
	}

	var executionErr error
	defer func() {
		if rec := recover(); rec != nil {
			if invariant, ok := rec.(runtimeInvariantPanic); ok {
				res = errResult(r, errToolDependencyFailed)
				executionErr = &operationHookPanicError{Operation: hook.OperationToolExecution}
				finishExecutionHook(finishExecution, executionCall, res, executionErr)
				panic(invariant.value)
			}
			res = errResult(r, errToolPanicRedacted)
			executionErr = &operationHookPanicError{Operation: hook.OperationToolExecution}
			r.hookPreview = errToolPanicRedacted
		}
		finishExecutionHook(finishExecution, executionCall, res, executionErr)
	}()

	exec := chain(r.t, ts.Middlewares)
	tr, err := exec(execCtx, r.argsstr)
	if err != nil {
		executionErr = err
		if cancelErr := execCtx.Err(); cancelErr != nil {
			r.terminalErr = cancelErr
		}
		return errResult(r, boundedDiagnostic(errToolPrefix+safeErrorText(err)))
	}
	if tr == nil || len(tr.Content) == 0 {
		executionErr = fmt.Errorf("loop: tool returned an empty result")
		return errResult(r, errEmptyResult)
	}
	return result{
		ToolExecutionID: r.callID,
		ToolUseID:       r.block.ID,
		Content:         tr.Content,
		IsError:         isErrorResult(tr),
	}
}

func finishExecutionHook(finish hook.FinishFunc, call hook.Call, res result, err error) {
	preview, isErr := previewOf(res)
	if _, panicked := err.(*operationHookPanicError); panicked {
		preview = errToolPanicRedacted
		res.Content = []content.Block{&content.TextBlock{Text: errToolPanicRedacted}}
	}
	call.ToolExecution.Result = &tool.ToolResult{Content: append([]content.Block(nil), res.Content...)}
	call.ToolExecution.ResultPreview = preview
	call.ToolExecution.IsError = isErr
	outcome := hookOutcome(context.Background(), err)
	finishErr := err
	if err != nil {
		switch outcome {
		case hook.OutcomeCanceled:
			finishErr = context.Canceled
		default:
			finishErr = &toolExecutionHookError{}
		}
	}
	finishHook(finish, call, outcome, finishErr)
}

// isErrorResult reports whether a successful (non-nil, non-empty) ToolResult is an
// error result. A tool signals an error by setting IsError on a ToolResultBlock it
// returns; absent that, a value result is a success.
func isErrorResult(tr *tool.ToolResult) bool {
	for _, b := range tr.Content {
		if trb, ok := b.(*content.ToolResultBlock); ok && trb.IsError {
			return true
		}
	}
	return false
}

// chain composes the middleware chain around the tool's InvokableRun. The
// first-listed middleware is the OUTERMOST wrapper.
func chain(t tool.InvokableTool, mws []tool.ToolMiddleware) tool.ToolExecuteFunc {
	next := t.InvokableRun
	for i := len(mws) - 1; i >= 0; i-- {
		mw := mws[i]
		inner := next
		next = func(ctx context.Context, argsJSON string) (*tool.ToolResult, error) {
			return mw(ctx, t, argsJSON, inner)
		}
	}
	return next
}

// failureResult builds the fixed result for a pre-execution failure.
func failureResult(r *resolved) result {
	return errResult(r, r.failedMsg)
}

// errResult builds an error tool-result carrying the given message.
func errResult(r *resolved, msg string) result {
	return result{
		ToolExecutionID: r.callID,
		ToolUseID:       r.block.ID,
		Content:         []content.Block{&content.TextBlock{Text: msg}},
		IsError:         true,
	}
}

// collectResults gathers whatever results exist (used on the ctx-cancel teardown
// path). A torn-down batch's results are discarded by runTurn's rollback, so this
// just returns a best-effort, call-ordered slice with empty entries for
// unresolved calls.
func collectResults(rs []*resolved) []result {
	out := make([]result, len(rs))
	for i, r := range rs {
		out[i] = result{ToolExecutionID: r.callID, ToolUseID: r.block.ID}
	}
	return out
}

// previewOf returns the (capped) ResultPreview and the IsError flag for a result.
func previewOf(r result) (string, bool) {
	return capPreview(flattenToText(r.Content)), r.IsError
}

// flattenToText renders a block slice to text for the ResultPreview AND for the
// ToolResultMessage runTurn builds (runTurn REUSES this). Text/TextBlock content passes
// through (concatenated); any non-text block becomes a visible
// "[unsupported <type>]" placeholder — NEVER empty/silent, so a tool-result is
// always non-empty on the wire.
func flattenToText(blocks []content.Block) string {
	var sb strings.Builder
	for _, b := range blocks {
		switch v := b.(type) {
		case *content.TextBlock:
			sb.WriteString(v.Text)
		case *content.ToolResultBlock:
			sb.WriteString(flattenToText(v.Content))
		default:
			sb.WriteString("[unsupported " + string(blockTypeOf(b)) + "]")
		}
	}
	return sb.String()
}

// blockTypeOf returns the BlockType tag for a non-text block, used to build a
// visible placeholder in flattenToText.
func blockTypeOf(b content.Block) content.BlockType {
	switch b.(type) {
	case *content.ImageBlock:
		return content.TypeImage
	case *content.AudioBlock:
		return content.TypeAudio
	case *content.DocumentBlock:
		return content.TypeDocument
	case *content.ThinkingBlock:
		return content.TypeThinking
	case *content.ToolUseBlock:
		return content.TypeToolUse
	case *content.ToolResultBlock:
		return content.TypeToolResult
	default:
		return content.BlockType("unknown")
	}
}

// capPreview caps a preview string by BOTH a byte budget and a line budget, with a
// visible truncation marker appended when either is exceeded.
func capPreview(s string) string {
	truncated := false

	if lines := strings.Split(s, "\n"); len(lines) > previewMaxLines {
		s = strings.Join(lines[:previewMaxLines], "\n")
		truncated = true
	}
	if len(s) > previewMaxBytes {
		s = s[:previewMaxBytes]
		truncated = true
	}
	if truncated {
		return s + truncationMarker
	}
	return s
}
