package process

// input_tool.go implements the ProcessInput model-facing tool (design spec
// docs/specs/long-running-command-supervision.md, "ProcessInput API"):
// serialized, bounded writes to an owned live process's standard input,
// optional EOF/terminal-resize, and a post-operation cursor-addressed
// output snapshot in the same shape ProcessOutput renders.
//
// ProcessInput "may write only to the process input/terminal" (spec
// "Identity and authorization"). Like ProcessOutput, it never re-runs the
// originating Bash gate: its Request carries no Requirements. Owner
// authorization is identical to ProcessOutput's -- a missing handle and a
// cross-owner handle are indistinguishable, both rendering "not_found" --
// achieved by calling the exact same Supervisor.resolveEntry helper
// output_tool.go defines (same package).
//
// After a successful operation, InvokableRun builds its snapshot by
// constructing a throwaway *ProcessOutputTool bound to this call's own
// supervisor/owner and calling its unexported readOne with a
// safe_text-encoded, single-handle processOutputArtifact. This is
// deliberate reuse, not duplication: "returns the same snapshot shape as
// ProcessOutput" (spec) is true by construction because it is the exact
// same rendering code path, including manifest-derived terminal metadata
// and cursor_ahead/gap handling -- output_tool.go is read-only, so
// borrowing its render step here adds no coupling.
//
// Writes are serialized per process (serializeHandle) and bounded two ways
// (spec: "Writes are serialized per process and bounded; the tool may not
// block indefinitely behind a process that does not consume input"): a
// single call's data must fit under the Supervisor's configured
// MaxPendingInputBytes ceiling, checked before any write is attempted, and
// the underlying Stdin().Write call itself is wrapped in a
// maxInputWriteDuration bound (writeBounded) so a process that never reads
// its stdin can never hang this call forever -- only the abandoned
// goroutine's blocked Write leaks, never the caller.
//
// Resize is applied only through a live PTY-mode process (StreamMode ==
// tool.ProcessStreamModePTY, read directly off the live tool.Process, never
// from durable manifest.TTY, which Supervisor.Start does not yet populate
// accurately); a resize request against a pipe-mode process fails closed
// with CodePTYUnavailable, before any data/EOF in the same call is ever
// attempted. EOF closes Stdin() for a pipe-mode process (idempotent per the
// Harness tool.Process contract) and instead writes a single ASCII
// End-of-Transmission byte (Ctrl-D) for a PTY-mode process -- "maps to
// terminal input semantics appropriate to the platform for PTYs" (spec) --
// rather than closing the terminal's writer outright.
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

// processInputToolName is the EXACT tool name carried by every prepared
// request and shown to the model -- it MUST stay "ProcessInput" (spec
// "Ownership boundaries": Tools owns "ProcessInput").
const processInputToolName = "ProcessInput"

// maxInputWriteDuration bounds a single Stdin().Write attempt (spec
// "ProcessInput API": "the tool may not block indefinitely behind a
// process that does not consume input"). There is no configured Config
// field for this: MaxPendingInputBytes (config.go) bounds queued bytes, not
// write duration, and this task's contract is the smallest addition that
// satisfies both halves of the spec sentence without widening Config. A
// write that has not completed within this bound is treated identically to
// a queued-bytes rejection: CodeInputBackpressure.
const maxInputWriteDuration = 500 * time.Millisecond

// eofEOT is the ASCII End-of-Transmission control byte (Ctrl-D), the
// canonical Unix terminal end-of-input signal. sendEOF writes this single
// byte to a PTY-mode process's Stdin instead of closing it -- closing a
// PTY's write side tears down the terminal outright, which is not what "an
// idempotent, resumable EOF" means for an interactive session.
const eofEOT byte = 0x04

const processInputSchema = `{
  "type": "object",
  "properties": {
    "process_id": {"type": "string", "description": "The single owned, supervised process to write to."},
    "data": {"type": "string", "description": "Text to write to the process's standard input (optional)."},
    "cursor": {"type": "integer", "minimum": 0, "description": "Byte offset to begin the post-operation output snapshot from (optional; defaults to the process's combined output end offset captured before this call's own operation)."},
    "eof": {"type": "boolean", "description": "Close standard input (pipe-backed processes) or send the platform terminal end-of-input sequence (PTY processes). Idempotent for pipe-backed processes."},
    "rows": {"type": "integer", "minimum": 1, "maximum": 65535, "description": "New terminal row count. Requires cols. Valid only for PTY processes."},
    "cols": {"type": "integer", "minimum": 1, "maximum": 65535, "description": "New terminal column count. Requires rows. Valid only for PTY processes."},
    "yield_time_ms": {"type": "integer", "minimum": 0, "description": "After the input operation, wait up to this many milliseconds for new output or process termination before returning the snapshot (optional; omit for an immediate snapshot)."}
  },
  "required": ["process_id"]
}`

const processInputDesc = "Write to a supervised process's standard input, send EOF, or resize its terminal, then return the same cursor-addressed combined-output snapshot shape as ProcessOutput. Supply at least one of data, eof, or rows and cols together."

// processInputArgs is the typed decode of ProcessInput's untrusted
// argsJSON. Data/Cursor/Rows/Cols/YieldTimeMS are presence-aware (pointers)
// so an explicit value is distinguishable from an omitted field --
// required for the "at least one of data, EOF, or resize" rule and for the
// omitted-cursor/omitted-yield defaults, mirroring output_tool.go's
// processOutputArgs convention. EOF has no presence-sensitive meaning
// (explicit false and omitted are the same "not requested"), so it stays a
// plain bool.
type processInputArgs struct {
	ProcessID   string  `json:"process_id"`
	Data        *string `json:"data"`
	Cursor      *int64  `json:"cursor"`
	EOF         bool    `json:"eof"`
	Rows        *int    `json:"rows"`
	Cols        *int    `json:"cols"`
	YieldTimeMS *int    `json:"yield_time_ms"`
}

// processInputArtifact binds PrepareCall's validated, normalized decode of
// one ProcessInput call. InvokableRun consumes it verbatim -- the raw args
// are never reparsed.
type processInputArtifact struct {
	tool.TokenArtifact

	handle Handle

	hasData bool
	data    []byte

	eof bool

	resize bool
	rows   uint16
	cols   uint16

	cursor *int64

	hasYield bool
	yieldMS  int
}

// processInputPrepareError is the typed preparation failure; its message
// is model-safe (mirrors output_tool.go's processOutputPrepareError).
type processInputPrepareError struct{ reason string }

func (e *processInputPrepareError) Error() string { return e.reason }

func prepareInputFail(format string, args ...any) error {
	return &processInputPrepareError{reason: fmt.Sprintf(format, args...)}
}

// ProcessInputTool implements the mutating ProcessInput tool over a
// session's shared Supervisor. supervisor and owner are resolved once, by
// the caller that constructs this value (mirrors ProcessOutputTool) --
// ProcessInputTool itself never touches a tool.SessionResourceRegistry.
type ProcessInputTool struct {
	supervisor *Supervisor
	owner      Owner
	initErr    error

	// mu guards locks, the lazily populated per-handle serialization
	// registry (serializeHandle). Never touches Supervisor state directly.
	mu    sync.Mutex
	locks map[Handle]*sync.Mutex
}

// NewProcessInput constructs a ProcessInputTool bound to the session's
// shared supervisor and this tool's immutable process-authority owner. A
// nil supervisor is retained as a construction error and fails every call
// closed, mirroring NewProcessOutput's initErr convention.
func NewProcessInput(supervisor *Supervisor, owner Owner) *ProcessInputTool {
	t := &ProcessInputTool{supervisor: supervisor, owner: owner}
	if supervisor == nil {
		t.initErr = errors.New("supervisor is required")
	}
	return t
}

// Info returns ProcessInput's self-description. Name MUST equal
// "ProcessInput".
func (t *ProcessInputTool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{
		Name:   processInputToolName,
		Desc:   processInputDesc,
		Schema: json.RawMessage(processInputSchema),
	}, nil
}

// PrepareCall decodes, validates, and normalizes one ProcessInput call and
// freezes the result into a sealed processInputArtifact. Every argument is
// validated HERE; InvokableRun never re-parses argsJSON. The emitted
// Request carries no Requirements: writing to a process the caller already
// owns needs no new gate decision (spec "Identity and authorization":
// follow-up operations do not re-run the original Bash gate).
func (t *ProcessInputTool) PrepareCall(_ context.Context, _ uuid.UUID, argsJSON string) (tool.Request, tool.PreparedArtifact, error) {
	if t.initErr != nil {
		return tool.Request{}, nil, t.initErr
	}

	var a processInputArgs
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return tool.Request{}, nil, prepareInputFail("invalid arguments: not a JSON object")
	}

	if a.ProcessID == "" {
		return tool.Request{}, nil, prepareInputFail("process_id is required")
	}
	handle := Handle(a.ProcessID)
	if !handle.Valid() {
		return tool.Request{}, nil, prepareInputFail("invalid process id: %q", a.ProcessID)
	}

	hasData := a.Data != nil
	var data []byte
	if hasData {
		data = []byte(*a.Data)
	}

	hasRows := a.Rows != nil
	hasCols := a.Cols != nil
	if hasRows != hasCols {
		return tool.Request{}, nil, prepareInputFail("rows and cols must be supplied together")
	}
	resize := hasRows && hasCols
	var rows, cols uint16
	if resize {
		if *a.Rows < 1 || *a.Rows > 65535 {
			return tool.Request{}, nil, prepareInputFail("rows must be between 1 and 65535")
		}
		if *a.Cols < 1 || *a.Cols > 65535 {
			return tool.Request{}, nil, prepareInputFail("cols must be between 1 and 65535")
		}
		rows = uint16(*a.Rows)
		cols = uint16(*a.Cols)
	}

	if !hasData && !a.EOF && !resize {
		return tool.Request{}, nil, prepareInputFail("at least one of data, eof, or rows/cols is required")
	}

	var cursor *int64
	if a.Cursor != nil {
		if *a.Cursor < 0 {
			return tool.Request{}, nil, prepareInputFail("cursor must be >= 0")
		}
		c := *a.Cursor
		cursor = &c
	}

	hasYield := a.YieldTimeMS != nil
	yieldMS := 0
	if hasYield {
		if *a.YieldTimeMS < 0 {
			return tool.Request{}, nil, prepareInputFail("yield_time_ms must be >= 0")
		}
		yieldMS = *a.YieldTimeMS
	}

	artifact := &processInputArtifact{
		handle:   handle,
		hasData:  hasData,
		data:     data,
		eof:      a.EOF,
		resize:   resize,
		rows:     rows,
		cols:     cols,
		cursor:   cursor,
		hasYield: hasYield,
		yieldMS:  yieldMS,
	}
	return tool.Request{ToolName: processInputToolName}, artifact, nil
}

// InvokableRun executes the PREPARED artifact bound to this call. It never
// reparses argsJSON. A missing/cross-owner handle, a terminal target
// process, or a nil live process all render the single bare-error shape
// (ProcessID + Error) without ever reaching applyOperations; every other
// outcome -- success or a mid-operation failure -- goes through
// applyOperations under this handle's serialization lock and then renders
// the same cursor-addressed snapshot shape ProcessOutput renders (readOne,
// reused verbatim from output_tool.go).
func (t *ProcessInputTool) InvokableRun(ctx context.Context, _ string) (*tool.ToolResult, error) {
	if t.initErr != nil {
		return renderProcessOutputCallError(string(CodeLifetimeEnforcementUnavailable)), nil
	}
	call, ok := loop.PreparedCallFromContext(ctx)
	if !ok {
		return renderProcessOutputCallError(string(CodeInvalidArguments)), nil
	}
	art, ok := call.Artifact.(*processInputArtifact)
	if !ok || art == nil {
		return renderProcessOutputCallError(string(CodeInvalidArguments)), nil
	}

	e, found := t.supervisor.resolveEntry(t.owner, art.handle)
	if !found {
		return renderInputResult(art.handle, CodeNotFound), nil
	}
	if closed(e.done) {
		return renderInputResult(art.handle, CodeStdinClosed), nil
	}
	if e.process == nil {
		return renderInputResult(art.handle, CodeProcessSetupFailed), nil
	}

	preWriteCursor, failCode, failed := t.applyOperations(ctx, e, art)
	if failed {
		return renderInputResult(art.handle, failCode), nil
	}

	cursor := preWriteCursor
	if art.cursor != nil {
		cursor = *art.cursor
	}

	t.awaitYield(ctx, e, art.handle, cursor, art.hasYield, art.yieldMS)

	ot := &ProcessOutputTool{supervisor: t.supervisor, owner: t.owner}
	snapshotArt := &processOutputArtifact{
		handles:    []Handle{art.handle},
		cursor:     cursor,
		limitBytes: int(DefaultMaxInlineResultBytes),
		encoding:   encodingSafeText,
	}
	return renderInputSnapshot(ot.readOne(art.handle, snapshotArt)), nil
}

// applyOperations performs every operation art requested -- resize, then
// data, then EOF -- against e's live process, serialized against every
// other ProcessInput call for the same handle (serializeHandle). It fails
// fast and never partially recovers: the first failing sub-operation stops
// the rest of this call's own operations from being attempted at all
// (spec's "the tool may not block indefinitely" is about one write, not
// about salvaging a partially invalid multi-operation request). preWriteCursor
// is always returned, even on failure, so a caller could in principle use
// it for diagnostics, though InvokableRun's current failure path renders a
// bare error and does not.
func (t *ProcessInputTool) applyOperations(ctx context.Context, e *entry, art *processInputArtifact) (preWriteCursor int64, failCode Code, failed bool) {
	lock := t.serializeHandle(art.handle)
	lock.Lock()
	defer lock.Unlock()

	preWriteCursor = e.spool.TotalBytes()

	if art.resize {
		if e.process.StreamMode() != tool.ProcessStreamModePTY {
			return preWriteCursor, CodePTYUnavailable, true
		}
		if err := e.process.Resize(ctx, art.rows, art.cols); err != nil {
			return preWriteCursor, CodeProcessSetupFailed, true
		}
	}

	if art.hasData {
		if int64(len(art.data)) > t.supervisor.cfg.MaxPendingInputBytes {
			return preWriteCursor, CodeInputBackpressure, true
		}
		if err := writeBounded(ctx, e.process.Stdin(), art.data); err != nil {
			return preWriteCursor, classifyWriteError(err), true
		}
	}

	if art.eof {
		if err := sendEOF(ctx, e.process); err != nil {
			return preWriteCursor, classifyWriteError(err), true
		}
	}

	return preWriteCursor, "", false
}

// serializeHandle returns the mutex that serializes every ProcessInput
// operation against handle for the lifetime of this tool instance (spec
// "ProcessInput API": "Writes are serialized per process"). Lazily
// created and never removed: this tool's Handle space is bounded by
// however many distinct processes it has ever addressed, not by request
// volume, so the map cannot grow per call.
func (t *ProcessInputTool) serializeHandle(handle Handle) *sync.Mutex {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.locks == nil {
		t.locks = make(map[Handle]*sync.Mutex)
	}
	l, ok := t.locks[handle]
	if !ok {
		l = &sync.Mutex{}
		t.locks[handle] = l
	}
	return l
}

// awaitYield optionally blocks after art's operations have already been
// applied, mirroring output_tool.go's awaitTargets for a single WaitAny
// target: already-satisfied (terminal, or the entry has already advanced
// past cursor) never blocks at all, and any outcome of the wait attempt
// itself -- timeout or ctx cancellation -- is discarded, exactly like
// ProcessOutput's own wait: any|all. InvokableRun always renders the best
// available snapshot afterward regardless of how this returns.
func (t *ProcessInputTool) awaitYield(ctx context.Context, e *entry, handle Handle, cursor int64, hasYield bool, yieldMS int) {
	if !hasYield {
		return
	}
	generation, _ := e.generationSnapshot()
	if closed(e.done) || cursor < e.spool.TotalBytes() {
		return
	}

	waitCtx := ctx
	if yieldMS > 0 {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, time.Duration(yieldMS)*time.Millisecond)
		defer cancel()
	}
	_, _ = t.supervisor.Wait(waitCtx, t.owner, WaitAny, []WaitTarget{{Handle: handle, Generation: generation}})
}

// writeBounded writes data to w, bounded by maxInputWriteDuration
// regardless of ctx's own deadline (see maxInputWriteDuration's doc
// comment). A zero-length data short-circuits before spawning anything:
// there is nothing to bound. The write itself always runs in its own
// goroutine; if the bound elapses first, that goroutine is abandoned
// (there is no portable way to cancel an in-flight io.Writer.Write) and
// writeBounded returns the context's own deadline/cancellation error, which
// classifyWriteError maps to CodeInputBackpressure.
func writeBounded(ctx context.Context, w io.Writer, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	writeCtx, cancel := context.WithTimeout(ctx, maxInputWriteDuration)
	defer cancel()

	result := make(chan error, 1)
	go func() {
		_, err := w.Write(data)
		result <- err
	}()

	select {
	case err := <-result:
		return err
	case <-writeCtx.Done():
		return writeCtx.Err()
	}
}

// classifyWriteError renders a writeBounded/sendEOF failure as a stable
// Code: a bound-exceeded (deadline or cancellation) error is
// CodeInputBackpressure -- the write never completed, not that the process
// refused it -- and every other error (a closed pipe's io.ErrClosedPipe or
// equivalent) is CodeStdinClosed, the process's own input is no longer
// writable.
func classifyWriteError(err error) Code {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return CodeInputBackpressure
	}
	return CodeStdinClosed
}

// sendEOF implements EOF's platform-appropriate semantics (spec
// "ProcessInput API": "EOF is idempotent for pipe-backed processes and maps
// to terminal input semantics appropriate to the platform for PTYs"): a
// pipe-mode process has its Stdin closed outright (idempotent per the
// Harness tool.Process contract, so a repeated EOF request is never an
// error); a PTY-mode process instead has a single ASCII EOT byte written to
// its Stdin, leaving the terminal itself open.
func sendEOF(ctx context.Context, p tool.Process) error {
	if p.StreamMode() == tool.ProcessStreamModePTY {
		return writeBounded(ctx, p.Stdin(), []byte{eofEOT})
	}
	return p.Stdin().Close()
}

// renderInputResult renders a bare per-call error (ProcessID + Error, every
// other field at its zero value) -- the not_found/stdin_closed/
// pty_unavailable/etc. shape a call never reaching a successful snapshot
// produces.
func renderInputResult(handle Handle, code Code) *tool.ToolResult {
	return renderInputSnapshot(processOutputResult{ProcessID: string(handle), Error: string(code)})
}

// renderInputSnapshot marshals result -- reusing output_tool.go's own
// processOutputResult type, since "returns the same snapshot shape as
// ProcessOutput" (spec) -- into ProcessInput's model-facing JSON.
// json.Marshal can fail only for a cyclic value or an unsupported type,
// neither of which this plain, flat, all-value struct can ever contain;
// the fallback exists only to keep InvokableRun's "never returns a Go
// error" contract airtight even against that theoretical case (mirrors
// output_tool.go's renderProcessOutputResults).
func renderInputSnapshot(result processOutputResult) *tool.ToolResult {
	data, err := json.Marshal(result)
	if err != nil {
		return renderProcessOutputCallError(string(CodeProcessSetupFailed))
	}
	return tool.TextResult(string(data))
}

// compile-time assertions: ProcessInputTool is an InvokableTool and a
// CallPreparer. It is deliberately NOT a WriteTarget (it writes to a
// process's stdin, not a filesystem path) and NOT Auditable beyond the
// runner's generic fallback (its data payload must never be echoed into an
// audit summary).
var (
	_ tool.InvokableTool = (*ProcessInputTool)(nil)
	_ tool.CallPreparer  = (*ProcessInputTool)(nil)
)
