package process

// output_tool.go implements the ProcessOutput model-facing tool (design spec
// docs/specs/long-running-command-supervision.md, "ProcessOutput API"):
// non-mutating, cursor-addressed inspection of one or more supervised
// processes' combined output plus their manifest-derived terminal metadata.
//
// ProcessOutput is READ-ONLY (spec "Identity and authorization":
// "ProcessOutput is read-only"). It never re-runs the originating Bash gate;
// authorization is the immutable process Owner (SessionID + LoopID) compared
// against the live Supervisor registry -- a missing handle and a
// cross-owner handle are deliberately indistinguishable, both rendering
// "not_found" for that one entry, never failing the whole call.
//
// This file follows the same shape bash/bash.go and bash/prepare.go
// establish for a tool.Definition-backed tool: PrepareCall owns the WHOLE
// untrusted-argument boundary (decode, validate, normalize) and freezes the
// validated result into a sealed tool.PreparedArtifact; InvokableRun never
// reparses argsJSON, it only reads the artifact PrepareCall already
// produced back off the ctx. Because ProcessOutput has no capability to
// gate (spec: follow-up operations do not re-run the original Bash gate),
// PrepareCall's Request carries no Requirements -- an "empty effect
// request" exactly like todo.Todo's -- but, unlike Todo, ProcessOutput DOES
// require its sealed artifact at InvokableRun time (CLAUDE.md: "a prepared
// tool without its typed artifact refuses to run"), because that artifact
// is where every argument validation result already lives; InvokableRun
// must never re-decode or re-validate argsJSON itself.
//
// Every rendered field is drawn from process's own bounded, safe-by-
// construction types (Handle, State, Result.Reason -- see bash/result.go's
// identical note) or from a bounded Spool read. No spool or manifest
// FILESYSTEM PATH is ever read, held, or rendered by this file (spec:
// "No filesystem path to a spool or manifest is returned to a model").
//
// Wiring ProcessOutputTool to the session's ONE shared, runner-free
// Supervisor (spec: "any of the four [process] definitions may win
// get-or-create") is a later task's job (Task 19, "Export four
// independently selectable definitions"): this file accepts an
// already-resolved *Supervisor and Owner directly, exactly the shape a
// root Definition's Build closure can hand it after resolving the shared
// supervisor session resource through the harness registry.

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

// processOutputToolName is the EXACT tool name carried by every prepared
// request and shown to the model -- it MUST stay "ProcessOutput" (spec
// "Ownership boundaries": Tools owns "ProcessOutput").
const processOutputToolName = "ProcessOutput"

// encodingSafeText and encodingBase64 are the closed set of `encoding`
// values ProcessOutput accepts (spec "ProcessOutput API":
// `"encoding": "safe_text | base64"`). encodingBase64 is deliberately the
// same string as render.go's ArtifactEncodingBase64 -- both name the
// identical wire value -- so this file reuses that constant directly rather
// than declaring a second one.
const encodingSafeText = "safe_text"

const processOutputSchema = `{
  "type": "object",
  "properties": {
    "process_id": {"type": "string", "description": "The single process to inspect. Mutually exclusive with process_ids; exactly one of the two is required."},
    "process_ids": {"type": "array", "items": {"type": "string"}, "minItems": 1, "description": "Multiple processes to inspect in one call, each opaque and distinct. Mutually exclusive with process_id; exactly one of the two is required."},
    "cursor": {"type": "integer", "minimum": 0, "description": "Byte offset into each process's combined output stream to read from (optional; default 0)."},
    "limit_bytes": {"type": "integer", "minimum": 1, "description": "Maximum output bytes to read per process (optional; default 32768)."},
    "encoding": {"type": "string", "enum": ["safe_text", "base64"], "description": "Output encoding (optional; default safe_text). base64 returns the same owner-authorized raw bytes without normalization."},
    "wait": {"type": "string", "enum": ["poll", "any", "all"], "description": "poll (default) returns immediately. any/all block until, respectively, at least one or every selected process has new output past cursor or becomes terminal."},
    "timeout_ms": {"type": "integer", "minimum": 0, "description": "Bounds an any/all wait, in milliseconds (optional; 0 or omitted waits with no additional bound beyond the call's own cancellation). Ignored for poll."}
  }
}`

const processOutputDesc = "Read a supervised process's combined stdout+stderr output by cursor, or wait for new output or completion, without replaying the whole transcript. Supply exactly one of process_id or process_ids. Read-only: never mutates or stops a process."

// processOutputArgs is the typed decode of ProcessOutput's untrusted
// argsJSON. Cursor/LimitBytes/TimeoutMS are presence-aware (pointers) so an
// explicit 0 is distinguishable from an omitted field, mirroring
// bash/prepare.go's bashArgs convention for its own presence-sensitive
// fields.
type processOutputArgs struct {
	ProcessID  string   `json:"process_id"`
	ProcessIDs []string `json:"process_ids"`
	Cursor     *int64   `json:"cursor"`
	LimitBytes *int     `json:"limit_bytes"`
	Encoding   string   `json:"encoding"`
	Wait       string   `json:"wait"`
	TimeoutMS  *int     `json:"timeout_ms"`
}

// processOutputArtifact binds PrepareCall's validated, normalized decode of
// one ProcessOutput call. InvokableRun consumes it verbatim -- the raw args
// are never reparsed. handles preserves the caller's exact input order
// (spec: "Multi-process results preserve input order"); multi records
// whether the call used the plural process_ids (array response) or the
// singular process_id (flat single-object response).
type processOutputArtifact struct {
	tool.TokenArtifact

	handles []Handle
	multi   bool

	cursor     int64
	limitBytes int
	encoding   string
	wait       WaitKind
	timeoutMS  int
}

// processOutputPrepareError is the typed preparation failure; its message
// is model-safe (mirrors bash/prepare.go's bashPrepareError).
type processOutputPrepareError struct{ reason string }

func (e *processOutputPrepareError) Error() string { return e.reason }

func prepareOutputFail(format string, args ...any) error {
	return &processOutputPrepareError{reason: fmt.Sprintf(format, args...)}
}

// ProcessOutputTool implements the read-only ProcessOutput tool over a
// session's shared Supervisor. supervisor and owner are resolved once, by
// the caller that constructs this value (see this file's package doc
// comment) -- ProcessOutputTool itself never touches a
// tool.SessionResourceRegistry.
type ProcessOutputTool struct {
	supervisor *Supervisor
	owner      Owner
	initErr    error
}

// NewProcessOutput constructs a ProcessOutputTool bound to the session's
// shared supervisor and this tool's immutable process-authority owner. A
// nil supervisor is retained as a construction error and fails every call
// closed, mirroring bash.BashTool's initErr convention -- ProcessOutput
// never panics on a missing dependency.
func NewProcessOutput(supervisor *Supervisor, owner Owner) *ProcessOutputTool {
	t := &ProcessOutputTool{supervisor: supervisor, owner: owner}
	if supervisor == nil {
		t.initErr = errors.New("supervisor is required")
	}
	return t
}

// Info returns ProcessOutput's self-description. Name MUST equal
// "ProcessOutput".
func (t *ProcessOutputTool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{
		Name:   processOutputToolName,
		Desc:   processOutputDesc,
		Schema: json.RawMessage(processOutputSchema),
	}, nil
}

// PrepareCall decodes, validates, and normalizes one ProcessOutput call and
// freezes the result into a sealed processOutputArtifact. Every argument
// -- process_id/process_ids exclusivity and shape, cursor, limit_bytes,
// encoding, wait, and timeout_ms -- is validated HERE; InvokableRun never
// re-parses argsJSON. The emitted Request carries no Requirements: reading
// a process the caller already owns needs no new gate decision (spec
// "Identity and authorization": follow-up operations do not re-run the
// original Bash gate).
func (t *ProcessOutputTool) PrepareCall(_ context.Context, _ uuid.UUID, argsJSON string) (tool.Request, tool.PreparedArtifact, error) {
	if t.initErr != nil {
		return tool.Request{}, nil, t.initErr
	}

	var a processOutputArgs
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return tool.Request{}, nil, prepareOutputFail("invalid arguments: not a JSON object")
	}

	ids, multi, err := resolveProcessIDs(a)
	if err != nil {
		return tool.Request{}, nil, err
	}
	handles, err := parseProcessHandles(ids)
	if err != nil {
		return tool.Request{}, nil, err
	}

	cursor := int64(0)
	if a.Cursor != nil {
		if *a.Cursor < 0 {
			return tool.Request{}, nil, prepareOutputFail("cursor must be >= 0")
		}
		cursor = *a.Cursor
	}

	limitBytes := int(DefaultMaxInlineResultBytes)
	if a.LimitBytes != nil {
		if *a.LimitBytes <= 0 {
			return tool.Request{}, nil, prepareOutputFail("limit_bytes must be > 0")
		}
		limitBytes = *a.LimitBytes
	}

	encoding := a.Encoding
	if encoding == "" {
		encoding = encodingSafeText
	}
	if encoding != encodingSafeText && encoding != ArtifactEncodingBase64 {
		return tool.Request{}, nil, prepareOutputFail("encoding must be safe_text or base64, got %q", a.Encoding)
	}

	wait := WaitKind(a.Wait)
	if wait == "" {
		wait = WaitPoll
	}
	if !wait.Valid() {
		return tool.Request{}, nil, prepareOutputFail("wait must be poll, any, or all, got %q", a.Wait)
	}

	timeoutMS := 0
	if a.TimeoutMS != nil {
		if *a.TimeoutMS < 0 {
			return tool.Request{}, nil, prepareOutputFail("timeout_ms must be >= 0")
		}
		timeoutMS = *a.TimeoutMS
	}

	artifact := &processOutputArtifact{
		handles:    handles,
		multi:      multi,
		cursor:     cursor,
		limitBytes: limitBytes,
		encoding:   encoding,
		wait:       wait,
		timeoutMS:  timeoutMS,
	}
	return tool.Request{ToolName: processOutputToolName}, artifact, nil
}

// resolveProcessIDs enforces single-vs-multi-ID exclusivity (spec: "One
// process may be supplied as process_id; multiple processes use
// process_ids. Supplying neither, both, duplicates, or an empty list is
// invalid.") and returns the caller's ids in their original order. multi
// reports whether the plural process_ids form was used (an empty
// process_ids array is indistinguishable from an omitted one -- both
// naturally fall into the "neither supplied" branch below).
func resolveProcessIDs(a processOutputArgs) (ids []string, multi bool, err error) {
	hasSingle := a.ProcessID != ""
	hasMulti := len(a.ProcessIDs) > 0

	switch {
	case hasSingle && hasMulti:
		return nil, false, prepareOutputFail("supply exactly one of process_id or process_ids, not both")
	case !hasSingle && !hasMulti:
		return nil, false, prepareOutputFail("process_id or process_ids is required")
	case hasSingle:
		return []string{a.ProcessID}, false, nil
	default:
		seen := make(map[string]struct{}, len(a.ProcessIDs))
		for _, id := range a.ProcessIDs {
			if id == "" {
				return nil, false, prepareOutputFail("process_ids entries must not be empty")
			}
			if _, dup := seen[id]; dup {
				return nil, false, prepareOutputFail("process_ids must not contain duplicates: %q", id)
			}
			seen[id] = struct{}{}
		}
		return append([]string(nil), a.ProcessIDs...), true, nil
	}
}

// parseProcessHandles validates that every id is a well-formed Handle
// (identity.go's Handle.Valid()) -- a purely structural check, independent
// of whether the handle currently names a live or owned process (that is a
// runtime check InvokableRun performs, rendering "not_found" per entry
// rather than failing preparation, since a stale or foreign handle is not,
// itself, malformed input).
func parseProcessHandles(ids []string) ([]Handle, error) {
	handles := make([]Handle, len(ids))
	for i, id := range ids {
		h := Handle(id)
		if !h.Valid() {
			return nil, prepareOutputFail("invalid process id: %q", id)
		}
		handles[i] = h
	}
	return handles, nil
}

// InvokableRun executes the PREPARED artifact bound to this call. It never
// reparses argsJSON. A wait: any|all call first blocks (bounded by
// timeout_ms, if any, and always by ctx) until its combinator's condition
// is already satisfied or becomes satisfied; the wait's own outcome
// (including a timeout or ctx cancellation) is never surfaced as a call
// failure -- "the wait timeout affects only the output call, never the
// process" (spec) -- InvokableRun always proceeds to render whatever is
// available once the wait attempt returns. Every per-process failure (a
// missing/cross-owner handle, a cursor beyond total_bytes) renders as that
// one entry's own "error" field, never as a whole-call failure or a Go
// error.
func (t *ProcessOutputTool) InvokableRun(ctx context.Context, _ string) (*tool.ToolResult, error) {
	if t.initErr != nil {
		return renderProcessOutputCallError(string(CodeLifetimeEnforcementUnavailable)), nil
	}
	call, ok := loop.PreparedCallFromContext(ctx)
	if !ok {
		return renderProcessOutputCallError(string(CodeInvalidArguments)), nil
	}
	art, ok := call.Artifact.(*processOutputArtifact)
	if !ok || art == nil {
		return renderProcessOutputCallError(string(CodeInvalidArguments)), nil
	}

	if art.wait != WaitPoll {
		t.awaitTargets(ctx, art)
	}

	results := make([]processOutputResult, len(art.handles))
	for i, h := range art.handles {
		results[i] = t.readOne(h, art)
	}
	return renderProcessOutputResults(results, art.multi), nil
}

// awaitTargets blocks until art.wait's combinator condition is satisfied,
// or art.timeoutMS elapses (0 means no additional bound beyond ctx), or ctx
// itself ends -- whichever happens first. Every target's OWN condition is
// "already has output past art.cursor, or is terminal" (spec: "any waits
// until any selected process has new output after its supplied cursor or
// becomes terminal"), decided from a fresh snapshot of the live Supervisor
// state, never from Supervisor.Wait's own generation-only satisfaction
// rule (which knows nothing about the caller's cursor). When the
// combinator is not yet satisfied, awaitTargets calls Supervisor.Wait with
// each target's CURRENTLY OBSERVED generation, so Wait blocks only until
// something genuinely new happens beyond what this snapshot already
// examined -- never re-triggering immediately on already-seen output. A
// missing or cross-owner handle can never block the wait (mirrors
// wait.go's own snapshotTargets rule): it is treated as immediately
// satisfied here too, so the eventual not_found render still happens, no
// differently than if wait had never been requested.
func (t *ProcessOutputTool) awaitTargets(ctx context.Context, art *processOutputArtifact) {
	targets := make([]WaitTarget, len(art.handles))
	satisfiedCount := 0
	for i, h := range art.handles {
		generation, terminal, totalBytes, found := t.supervisor.snapshotHandle(t.owner, h)
		targets[i] = WaitTarget{Handle: h, Generation: generation}
		if !found || terminal || art.cursor < totalBytes {
			satisfiedCount++
		}
	}

	satisfied := false
	switch art.wait {
	case WaitAny:
		satisfied = satisfiedCount > 0
	case WaitAll:
		satisfied = satisfiedCount == len(targets)
	}
	if satisfied {
		return
	}

	waitCtx := ctx
	if art.timeoutMS > 0 {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, time.Duration(art.timeoutMS)*time.Millisecond)
		defer cancel()
	}
	// The result and any error (deadline exceeded, ctx canceled, or a
	// quota rejection) are both intentionally discarded: whichever way
	// this wait attempt ends, InvokableRun's caller still renders the
	// best available snapshot of every requested process below.
	_, _ = t.supervisor.Wait(waitCtx, t.owner, art.wait, targets)
}

// snapshotHandle resolves handle under owner's authorization and reports
// its current append-generation, terminal state, and live combined-output
// total_bytes -- everything awaitTargets needs to decide whether a target
// is already satisfied before ever blocking. found is false for a missing
// OR cross-owner handle, indistinguishably (spec "Identity and
// authorization"), in which case every other return value is the zero
// value and the caller must treat this target as trivially satisfied
// (it can never advance).
func (s *Supervisor) snapshotHandle(owner Owner, handle Handle) (generation uint64, terminal bool, totalBytes int64, found bool) {
	e, ok := s.resolveEntry(owner, handle)
	if !ok {
		return 0, false, 0, false
	}
	generation, _ = e.generationSnapshot()
	return generation, closed(e.done), e.spool.TotalBytes(), true
}

// resolveEntry looks up handle in the registry and returns it only if
// owner is authorized to see it -- a missing Handle and one owned by a
// different Owner are indistinguishable to every caller (spec "Identity
// and authorization"; mirrors wait.go's snapshotTargets inline check).
func (s *Supervisor) resolveEntry(owner Owner, handle Handle) (*entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[handle]
	if !ok || !e.identity.Owner.Equal(owner) {
		return nil, false
	}
	return e, true
}

// readOne renders one requested process's current output window plus its
// manifest-derived terminal metadata. It ALWAYS reads through the process's
// Spool (never its in-memory Buffer): the spool is configured with a
// ceiling at least as large as the buffer's by every documented default
// (spec "Output capture and storage" defaults: 64 MiB spool vs. 1 MiB
// buffer) and, for a restored/terminal process, is the ONLY store that
// still holds any retained bytes at all (restore.go's reopenEntry gives a
// restored entry a fresh, empty Buffer) -- "The spool is the bounded source
// of truth for completed output and cursor recovery" (spec). Never returns
// a spool or manifest FILESYSTEM PATH: only Handle, cursors, byte counts,
// and manifest-derived closed-enum/timestamp fields ever reach the result.
func (t *ProcessOutputTool) readOne(handle Handle, art *processOutputArtifact) processOutputResult {
	e, ok := t.supervisor.resolveEntry(t.owner, handle)
	if !ok {
		return processOutputResult{ProcessID: string(handle), Error: string(CodeNotFound)}
	}

	result := processOutputResult{ProcessID: string(handle)}
	if art.encoding == ArtifactEncodingBase64 {
		rendered, err := RenderBase64(e.spool, art.cursor, art.limitBytes)
		if err != nil {
			return processOutputResult{ProcessID: string(handle), Error: renderErrorCode(err)}
		}
		result.Output = rendered.Data
		result.StartCursor = rendered.StartCursor
		result.NextCursor = rendered.NextCursor
		result.Gap = rendered.Gap
		result.Artifact = &processOutputArtifactRef{ID: string(handle), Encoding: ArtifactEncodingBase64}
	} else {
		rendered, err := RenderSafeText(e.spool, handle, art.cursor, art.limitBytes, int64(art.limitBytes))
		if err != nil {
			return processOutputResult{ProcessID: string(handle), Error: renderErrorCode(err)}
		}
		result.Output = rendered.Output
		result.StartCursor = rendered.StartCursor
		result.NextCursor = rendered.NextCursor
		result.Gap = rendered.Gap
		result.Normalized = rendered.Normalized
		result.Binary = rendered.Binary
		result.Artifact = &processOutputArtifactRef{ID: string(rendered.Artifact.ProcessID), Encoding: rendered.Artifact.Encoding}
	}
	result.TotalBytes = e.spool.TotalBytes()
	t.applyManifestMetadata(&result, handle)
	return result
}

// applyManifestMetadata fills result's status/exit_code/reason/started_at/
// finished_at fields from handle's durable Manifest -- never from a value
// this file computed or guessed itself, exactly mirroring bash/
// supervised.go's readTerminalOutcome discipline. A reload failure (e.g. no
// manifest was ever written for a bare test-built entry) leaves those
// fields at their zero value, which json's omitempty then omits entirely,
// rather than failing this entry's whole result: the output/cursor fields
// readOne already populated remain valid and useful on their own.
func (t *ProcessOutputTool) applyManifestMetadata(result *processOutputResult, handle Handle) {
	if t.supervisor.manifests == nil {
		return
	}
	m, err := t.supervisor.manifests.Load(handle)
	if err != nil {
		return
	}
	result.Status = string(m.State)
	result.ExitCode = m.Result.ExitCode
	result.Reason = m.Result.Reason
	result.StartedAt = formatManifestTime(m.StartedAt)
	result.FinishedAt = formatManifestTime(m.FinishedAt)
}

// renderErrorCode extracts a *Error's stable Code for model-facing
// rendering (CLAUDE.md: "render only Code -- never Cause"), or
// CodeInvalidArguments for any other error shape (Buffer.Read/Spool.Read
// only ever return *Error today, but this fallback keeps the contract
// closed even against a future change).
func renderErrorCode(err error) string {
	var perr *Error
	if errors.As(err, &perr) {
		return string(perr.Code)
	}
	return string(CodeInvalidArguments)
}

// formatManifestTime renders t as RFC3339Nano, or the empty string
// (omitted by processOutputResult's omitempty tags) for a nil or zero
// Time -- mirrors bash/result.go's formatTime for the identical
// Manifest-timestamp-to-wire-string conversion.
func formatManifestTime(t *time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339Nano)
}

// processOutputResult is the per-process JSON shape ProcessOutput renders
// (spec "ProcessOutput API", "Each result includes:"). Every field but
// ProcessID is optional/omittable: a "not_found" or cursor_ahead entry
// carries only ProcessID and Error, and a non-terminal process naturally
// omits ExitCode/Reason/FinishedAt (Manifest.Result is the zero value until
// a process reaches a terminal state).
type processOutputResult struct {
	ProcessID   string                    `json:"process_id"`
	Status      string                    `json:"status,omitempty"`
	Output      string                    `json:"output,omitempty"`
	StartCursor int64                     `json:"start_cursor"`
	NextCursor  int64                     `json:"next_cursor"`
	TotalBytes  int64                     `json:"total_bytes"`
	Gap         bool                      `json:"gap,omitempty"`
	Normalized  bool                      `json:"normalized,omitempty"`
	Binary      bool                      `json:"binary,omitempty"`
	Artifact    *processOutputArtifactRef `json:"artifact,omitempty"`
	ExitCode    *int                      `json:"exit_code,omitempty"`
	Reason      string                    `json:"reason,omitempty"`
	StartedAt   string                    `json:"started_at,omitempty"`
	FinishedAt  string                    `json:"finished_at,omitempty"`
	Error       string                    `json:"error,omitempty"`
}

// processOutputArtifactRef is the opaque, path-free artifact descriptor
// (spec: `"artifact": {"id": "opaque", "encoding": "base64"}`). ID is the
// process's own Handle -- the SAME value a caller already has as
// process_id -- because a raw-byte re-read of this exact cursor range is
// just another ProcessOutput call against that same handle with
// encoding: base64 (spec: "callers retrieve its bytes with ProcessOutput
// and the original process handle, cursor, and base64 encoding"). There is
// no separate artifact-only identifier and no filesystem path anywhere in
// this type.
type processOutputArtifactRef struct {
	ID       string `json:"id"`
	Encoding string `json:"encoding"`
}

// renderProcessOutputResults marshals the per-process results into
// ProcessOutput's model-facing JSON: a flat single object for a
// process_id call (multi false), or {"results": [...]} preserving input
// order for a process_ids call (multi true) -- see resolveProcessIDs' doc
// comment for why the two request shapes map onto these two response
// shapes. json.Marshal can fail only for a cyclic value or an unsupported
// type, neither of which this plain, flat, all-value struct can ever
// contain, so the fallback branch exists only to keep InvokableRun's
// "never returns a Go error" contract airtight even against that
// theoretical case (mirrors bash/result.go's renderSupervisedResult).
func renderProcessOutputResults(results []processOutputResult, multi bool) *tool.ToolResult {
	if !multi {
		data, err := json.Marshal(results[0])
		if err != nil {
			return renderProcessOutputCallError(string(CodeProcessSetupFailed))
		}
		return tool.TextResult(string(data))
	}
	data, err := json.Marshal(struct {
		Results []processOutputResult `json:"results"`
	}{Results: results})
	if err != nil {
		return renderProcessOutputCallError(string(CodeProcessSetupFailed))
	}
	return tool.TextResult(string(data))
}

// renderProcessOutputCallError renders a stable, model-facing error code
// for a WHOLE-call structural failure (missing prepared artifact,
// unavailable supervisor) -- as opposed to a per-process error, which
// lives inside that one processOutputResult's own Error field.
func renderProcessOutputCallError(code string) *tool.ToolResult {
	data, err := json.Marshal(struct {
		Error string `json:"error"`
	}{Error: code})
	if err != nil {
		return tool.TextResult(`{"error":"` + code + `"}`)
	}
	return tool.TextResult(string(data))
}

// compile-time assertions: ProcessOutputTool is an InvokableTool and a
// CallPreparer. It is deliberately NOT a WriteTarget (it has no path-write
// effect) and NOT Auditable beyond the runner's generic fallback (its
// arguments carry no secret; a bare tool name is a sufficient audit trail
// for a read-only inspection call).
var (
	_ tool.InvokableTool = (*ProcessOutputTool)(nil)
	_ tool.CallPreparer  = (*ProcessOutputTool)(nil)
)
