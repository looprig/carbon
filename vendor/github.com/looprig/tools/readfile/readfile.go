// Package readfile implements the ReadFile tool: a workspace-contained,
// denied-path-aware, symlink-rejecting file reader returning line-numbered
// text capped by the injected ReadGuard. Preparation emits the direct
// filesystem.read requirement for the canonical resolved path; the tool
// enforces the approved resolved resource itself at run time.
package readfile

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/tools/internal/nofollow"
	"github.com/looprig/tools/internal/prepared"
	"github.com/looprig/tools/internal/workspace"
)

// readfile.go implements the ReadFile tool: a workspace-contained, denied-path-
// aware, symlink-rejecting file reader that returns line-numbered text capped by
// the ReadGuard's per-file byte limit (design §4b). It is a CallPreparer
// (PrepareCall emits the direct filesystem.read requirement for the canonical
// resolved path) and Auditable (a path-only, content-free summary); the
// ReadGuard stays the in-process denied-path enforcement.

// lineNumberWidth is the minimum column width for the 1-based line number prefix
// in ReadFile output ("   1\t<line>"). It keeps short files readably aligned;
// longer line numbers simply widen past it.
const lineNumberWidth = 4

// readFileToolName is the EXACT tool name — it MUST equal "ReadFile".
const readFileToolName = "ReadFile"

// readFileSchema is the JSON Schema for ReadFile's argument object.
const readFileSchema = `{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Workspace-relative path of the file to read."},
    "start_line": {"type": "integer", "minimum": 1, "description": "1-based first line to include (optional; default 1)."},
    "end_line": {"type": "integer", "minimum": 1, "description": "1-based last line to include (optional; default end of file)."}
  },
  "required": ["path"]
}`

const readFileDesc = "Read a UTF-8 text file from the workspace and return its contents as line-numbered text. Supports an optional 1-based line range. Reads are confined to the workspace, never follow a final-component symlink, and are capped at a per-file byte limit."

const readFileHostReadsDesc = "Read a UTF-8 text file and return its contents as line-numbered text. Supports an optional 1-based line range. An absolute path may resolve outside the workspace, subject to the caller's read authority; reads never follow a final-component symlink and are capped at a per-file byte limit."

// readFileArgs is the typed decode of ReadFile's untrusted argsJSON.
type readFileArgs struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}

// ReadFile reads a workspace-contained text file. It depends only on the
// workspace root, a narrow loop.ReadGuard (least privilege / interface
// segregation), and the loop's shared observation map: it never sees the full
// permission gate.
type ReadFile struct {
	root      string
	guard     loop.ReadGuard
	obs       tool.WorkspaceObservations
	hostReads bool
}

// ReadFileOption configures a ReadFile at construction (functional-options
// pattern, matching grep.GrepOption).
type ReadFileOption func(*ReadFile)

// WithHostReads lets an absolute path resolve OUTSIDE the workspace instead of
// being rejected at prepare time. It does not itself grant anything: an
// uncontained resolved path still emits the same filesystem.read requirement
// PrepareCall always has, so the consumer's bound access source (the selected
// sandbox.Profile in the product composition root) makes the actual
// Allow/Deny/Gated decision --
// the same authority a spawned Bash command's host reads already go through.
// Without this option, ReadFile's behavior is unchanged: any path outside the
// workspace is rejected lexically before a requirement is ever built. A
// RELATIVE "../" traversal is NEVER widened by this option -- only a literal
// absolute path argument can resolve outside the workspace (workspace.ResolvedPath).
// A successful uncontained read never records an observation (see
// InvokableRun): a host read must not feed the same-loop write-authorization
// map, since WriteFile/EditFile remain workspace-confined regardless of this
// option.
func WithHostReads() ReadFileOption {
	return func(r *ReadFile) { r.hostReads = true }
}

// NewReadFile constructs a ReadFile bound to the workspace root, read guard, and
// the loop's shared observation map. A complete read records the raw-content hash
// into obs so a subsequent same-loop WriteFile/EditFile of the same path is
// authorized; obs is supplied by Files (one per loop binding).
func NewReadFile(root string, guard loop.ReadGuard, obs tool.WorkspaceObservations, opts ...ReadFileOption) *ReadFile {
	r := &ReadFile{root: root, guard: guard, obs: obs}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Info returns ReadFile's self-description. Name MUST equal "ReadFile".
func (r *ReadFile) Info(context.Context) (*tool.ToolInfo, error) {
	desc := readFileDesc
	if r.hostReads {
		desc = readFileHostReadsDesc
	}
	return &tool.ToolInfo{
		Name:   readFileToolName,
		Desc:   desc,
		Schema: json.RawMessage(readFileSchema),
	}, nil
}

// AuditSummary returns a redacted, content-free one-line summary: the path only
// (never file contents). An unparseable args document yields a generic summary.
func (r *ReadFile) AuditSummary(argsJSON string) string {
	var a readFileArgs
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil || a.Path == "" {
		return "ReadFile (unparsable args)"
	}
	return "ReadFile " + a.Path
}

// readFileArtifact is ReadFile's typed prepared artifact: the validated,
// canonicalized target plus the displayed line range, bound to one call by
// PrepareCall and consumed by InvokableRun without reparsing the raw JSON. It
// deliberately embeds tool.TokenArtifact to satisfy the sealed
// tool.PreparedArtifact marker; the typed fields stay tool-private.
type readFileArtifact struct {
	tool.TokenArtifact
	abs       string // canonical symlink-resolved path (requirement Scope/Match, observation key)
	lexical   string // lexically-joined on-disk name the O_NOFOLLOW open targets
	display   string // model-supplied path, used only in messages
	contained bool   // whether abs is inside the workspace root (false only possible under WithHostReads)
	startLine int
	endLine   int
}

// prepareRead is the SINGLE parse-validate-canonicalize step for a ReadFile
// call.
func (r *ReadFile) prepareRead(argsJSON string) (*readFileArtifact, error) {
	var a readFileArgs
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return nil, &readFileError{reason: "invalid arguments: not a JSON object", cause: err}
	}
	if a.Path == "" {
		return nil, &readFileError{reason: "a non-empty 'path' is required"}
	}
	if err := validateLineRange(a.StartLine, a.EndLine); err != nil {
		return nil, err
	}
	abs, contained, lexical, err := r.resolveRead(a.Path)
	if err != nil {
		return nil, err
	}
	return &readFileArtifact{
		abs:       abs,
		lexical:   lexical,
		display:   a.Path,
		contained: contained,
		startLine: a.StartLine,
		endLine:   a.EndLine,
	}, nil
}

// resolveRead is the SINGLE resolution step both PrepareCall (via prepareRead)
// and InvokableRun's stage-1 recheck use, so the two can never drift on how a
// path resolves between approval and execution. Without WithHostReads(), it is
// exactly workspace.ContainedPath -- any non-nil error means "outside the
// workspace", matching the historical error message. With WithHostReads(), it
// is workspace.ResolvedPath -- a non-nil error here means resolution itself
// failed (e.g. an unresolvable symlink chain), never merely "outside the
// workspace", since an uncontained-but-resolved path is not an error at this
// layer; the filesystem.read gate decides it. lexical is the ON-DISK name
// (never symlink-resolved) the caller's O_NOFOLLOW open must target: joined
// under root for a contained result, or the cleaned input itself for an
// uncontained absolute one, so a symlinked final component is rejected by
// O_NOFOLLOW either way, never silently followed.
func (r *ReadFile) resolveRead(input string) (abs string, contained bool, lexical string, err error) {
	if !r.hostReads {
		abs, err = workspace.ContainedPath(r.root, input)
		if err != nil {
			return "", false, "", &readFileError{reason: "path is outside the workspace", cause: err}
		}
		return abs, true, joinedUnderRoot(r.root, input), nil
	}
	abs, contained, err = workspace.ResolvedPath(r.root, input)
	if err != nil {
		// A RELATIVE input's error always comes from the exact same containedPath
		// call the non-widened branch above makes (workspace.ResolvedPath forwards
		// it verbatim for relative input) -- keep its historical message. Only an
		// ABSOLUTE input can fail for the NEW reason (its existing-prefix chain
		// itself could not be resolved), which is a materially different cause and
		// gets its own message.
		reason := "path is outside the workspace"
		if filepath.IsAbs(input) {
			reason = "path could not be resolved"
		}
		return "", false, "", &readFileError{reason: reason, cause: err}
	}
	if filepath.IsAbs(input) {
		// input is already the correct on-disk name; joinedUnderRoot would
		// wrongly re-anchor an absolute path underneath root.
		return abs, contained, filepath.Clean(input), nil
	}
	return abs, contained, joinedUnderRoot(r.root, input), nil
}

// PrepareCall decodes and validates the untrusted arguments ONCE, resolves the
// canonical read target ONCE, and returns the typed request — one direct
// filesystem.read requirement whose Scope and Match are the canonical resolved
// path, empty grant pair — plus the typed artifact InvokableRun executes.
// Invalid input fails here and never reaches the permission gate.
func (r *ReadFile) PrepareCall(_ context.Context, executionID uuid.UUID, argsJSON string) (tool.Request, tool.PreparedArtifact, error) {
	art, err := r.prepareRead(argsJSON)
	if err != nil {
		return tool.Request{}, nil, err
	}
	return tool.Request{
		ToolName:     readFileToolName,
		ExecutionID:  executionID.String(),
		Requirements: []tool.Requirement{prepared.PathReadRequirement(art.abs)},
	}, art, nil
}

// InvokableRun executes the PREPARED artifact bound to this call — the raw
// argsJSON is never reparsed, so mutating it after preparation changes
// nothing; without its artifact the tool fails closed. Every failure mode
// (denied path, changed resolution, symlink, not-found, not-regular, read
// error) is returned as a tool-result error string — never a Go error and
// never echoing a secret's contents.
func (r *ReadFile) InvokableRun(ctx context.Context, _ string) (*tool.ToolResult, error) {
	art, ok := prepared.FromContext[*readFileArtifact](ctx)
	if !ok || art == nil {
		return tool.TextResult("error: permission denied: ReadFile requires its prepared call artifact"), nil
	}

	// Stage 1: enforce the APPROVED resolved path. Preparation proved
	// containment once for the permission decision; re-proving the resolution
	// still equals the approved canonical target closes the prepare→run window
	// (a parent-dir symlink swap must not redirect the read).
	abs, _, _, rerr := r.resolveRead(art.display)
	if rerr != nil {
		return tool.TextResult("error: " + rerr.Error() + ": " + art.display), nil
	}
	if abs != art.abs {
		return tool.TextResult("error: path resolution changed since approval: " + art.display), nil
	}
	key := art.abs

	// Stage 2: denied-path enforcement (secrets). DeniedRead's contract wants the
	// containedPath-resolved ABSOLUTE path. Echo the requested path only, never
	// the file body. A denied read authorizes no mutation: we record nothing.
	if r.guard.DeniedRead(art.abs) {
		return tool.TextResult("error: read denied: " + art.display), nil
	}

	// Stage 3 (step 5): open the LEXICALLY-joined path (not the symlink-resolved
	// abs) with O_NOFOLLOW so a final-component symlink fails to open rather than
	// being followed — both rejecting a static symlinked file AND closing the
	// resolve→open TOCTOU window. Containment above already guaranteed the target
	// is inside the workspace; the fd stat below confirms a regular file.
	body, truncated, err := r.readCapped(art.lexical)
	if err != nil {
		// A DEFINITIVE not-found records absence (authorizing a later create of a
		// genuinely new file). Every other failure — symlink (ELOOP), not-regular,
		// or a read error — is ambiguous and records NO usable observation. An
		// UNCONTAINED (host) target records nothing either way: WriteFile/EditFile
		// stay workspace-confined regardless of WithHostReads(), so a host read
		// authorizing a future write there is never meaningful, and a host read
		// must not feed the same-loop write-authorization map at all.
		if art.contained && errors.Is(err, os.ErrNotExist) {
			_ = r.obs.WithPath(string(key), func(obs *tool.FileObservation) error {
				*obs = tool.FileObservation{Observed: true}
				return nil
			})
		}
		return tool.TextResult("error: " + err.Error()), nil
	}

	// A COMPLETE (non-truncated) read records the raw-content SHA-256 so a later
	// same-loop overwrite/edit of this path is authorized. `body` is the FULL raw
	// file content here — a start_line/end_line range only narrows the DISPLAYED
	// lines (numberLines below), it does not change the bytes read or hashed. So the
	// observation means "this loop knows the file's CURRENT on-disk bytes" (the token
	// for external-change detection), NOT "the model saw every line". That is the
	// correct optimistic-concurrency contract: the compare-and-swap only needs the
	// hash to reflect current on-disk state. It is a DIFFERENT concern from the
	// truncation guard below — truncation is about hash CORRECTNESS: a truncated read
	// saw only a prefix, so its hash would not cover the whole file and MUST NOT be
	// recorded (else a partial read could authorize clobbering the unseen remainder).
	// The hash is private — it never reaches the model.
	if !truncated && art.contained {
		_ = r.obs.WithPath(string(key), func(obs *tool.FileObservation) error {
			*obs = tool.FileObservation{Observed: true, Present: true, Hash: sha256.Sum256([]byte(body))}
			return nil
		})
	}

	out := numberLines(body, art.startLine, art.endLine)
	if truncated {
		out += "\n[truncated: file exceeds the " + strconv.FormatInt(r.guard.MaxReadBytes(), 10) + "-byte read cap]"
	}
	return tool.TextResult(out), nil
}

// readCapped opens abs with a no-follow read open (a symlinked/reparse-point
// final component then fails to open — see internal/nofollow), confirms via fd
// stat that it is a regular file, and reads up to MaxReadBytes via
// io.LimitReader. truncated reports whether the file exceeds the cap (one extra
// byte is read to detect this). Errors are typed via readFileError.
func (r *ReadFile) readCapped(abs string) (body string, truncated bool, err error) {
	// #nosec G304 -- abs is the containedPath-resolved, workspace-confined path;
	// the no-follow open + fd stat below close the resolve→open TOCTOU window.
	f, oerr := nofollow.Open(abs, os.O_RDONLY, 0)
	if oerr != nil {
		return "", false, openErrorToReadFileError(abs, oerr)
	}
	defer func() { _ = f.Close() }()

	fi, serr := f.Stat()
	if serr != nil {
		return "", false, &readFileError{reason: "could not stat the file"}
	}
	if !fi.Mode().IsRegular() {
		return "", false, &readFileError{reason: "not a regular file"}
	}

	limit := r.guard.MaxReadBytes()
	// Read limit+1 so a file exactly at the cap is not falsely flagged truncated.
	data, rerr := io.ReadAll(io.LimitReader(f, limit+1))
	if rerr != nil {
		return "", false, &readFileError{reason: "could not read the file"}
	}
	if int64(len(data)) > limit {
		return string(data[:limit]), true, nil
	}
	return string(data), false, nil
}

// joinedUnderRoot returns the lexically-cleaned path of input anchored under
// root WITHOUT resolving symlinks — the path whose final component O_NOFOLLOW
// must inspect. containedPath has already proven the symlink-resolved form is
// inside the workspace; this lexical join (a leading "/" or "../" in input is
// neutralised by Join, exactly as in containedPath step 2) is the on-disk name
// to open so a final-component symlink is rejected, not followed.
func joinedUnderRoot(root, input string) string {
	return filepath.Join(root, filepath.Clean(input))
}

// openErrorToReadFileError maps a nofollow.Open error to a non-secret typed
// readFileError. A symlinked/reparse-point final component surfaces as an error
// wrapping nofollow.ErrSymlinkNotAllowed. errors.Is (rather than os.IsNotExist,
// which only unwraps a fixed set of stdlib error types) is used for the
// not-exist check because Windows's not-found case is a plain wrapped error
// (fmt.Errorf("%w: %w", fs.ErrNotExist, ...) in internal/nofollow), not one of
// those recognized types.
func openErrorToReadFileError(path string, err error) *readFileError {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return &readFileError{reason: "file not found", cause: err}
	case errors.Is(err, nofollow.ErrSymlinkNotAllowed):
		return &readFileError{reason: "refusing to follow a symlinked path", cause: err}
	default:
		return &readFileError{reason: "could not open the file", cause: err}
	}
}

// validateLineRange rejects a malformed 1-based [start,end] range. A zero value
// means "unset". start<0/end<0 or end<start (both set) is invalid.
func validateLineRange(start, end int) error {
	if start < 0 || end < 0 {
		return &readFileError{reason: "line numbers must be positive"}
	}
	if start > 0 && end > 0 && end < start {
		return &readFileError{reason: "end_line must be >= start_line"}
	}
	return nil
}

// numberLines renders body as 1-based line-numbered text, restricted to the
// [start,end] range (1-based, inclusive). A zero start defaults to 1; a zero end
// defaults to the last line. The line-number column is right-aligned to at least
// lineNumberWidth. A trailing newline in body does not add a spurious empty line.
func numberLines(body string, start, end int) string {
	if start <= 0 {
		start = 1
	}
	var b strings.Builder
	sc := bufio.NewScanner(strings.NewReader(body))
	// Allow long lines (default Scanner token is 64 KiB; widen to the read cap
	// ceiling so a single very long line is not split unexpectedly).
	sc.Buffer(make([]byte, 0, 64*1024), maxScanLineBytes)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		if lineNo < start {
			continue
		}
		if end > 0 && lineNo > end {
			break
		}
		fmt.Fprintf(&b, "%*d\t%s\n", lineNumberWidth, lineNo, sc.Text())
	}
	return strings.TrimRight(b.String(), "\n")
}

// maxScanLineBytes caps a single scanned line so a pathological file cannot
// exhaust memory inside the scanner. It is generous (matches the default read
// cap) but bounded.
const maxScanLineBytes = 1 << 20

// readFileError is the typed failure for a ReadFile read attempt. It carries a
// non-secret reason and an optional underlying cause; its message NEVER includes
// file contents.
type readFileError struct {
	reason string
	cause  error
}

func (e *readFileError) Error() string { return e.reason }

func (e *readFileError) Unwrap() error { return e.cause }

// compile-time assertions: ReadFile is an InvokableTool, a CallPreparer, and
// Auditable.
var (
	_ tool.InvokableTool = (*ReadFile)(nil)
	_ tool.CallPreparer  = (*ReadFile)(nil)
	_ tool.Auditable     = (*ReadFile)(nil)
)
