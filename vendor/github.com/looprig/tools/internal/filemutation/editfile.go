package filemutation

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/tools/internal/nofollow"
	"github.com/looprig/tools/internal/prepared"
)

// editfile.go implements the EditFile tool: an exact-string-replace editor over a
// workspace-contained file (design §4b). It proves containment (containedPath,
// symlink-resolved) then reads and writes the LEXICAL joined path — the read is
// O_RDONLY|O_NOFOLLOW so a final-component symlink is REJECTED (consistent with
// ReadFile), and the atomic write targets the same lexical name so it REPLACES a
// final-component symlink rather than following it. It replaces `old` with
// `replacement` under strict occurrence rules, writes back atomically (the shared
// atomicWriteFile temp+Rename), and returns a diff preview. Like WriteFile it
// is a CallPreparer (direct filesystem.write requirement + typed artifact), is
// Auditable (no content), and is a WriteTarget.
//
// Occurrence rules (the §4b contract):
//   - 0 matches of `old`          → tool-result error ("not found")
//   - ≥2 matches && !replace_all  → tool-result error ("ambiguous: N matches…")
//   - exactly 1, or replace_all   → perform the replacement (all if replace_all)

// editFileToolName is the EXACT tool name — it MUST equal "EditFile".
const editFileToolName = "EditFile"

// maxEditFileBytes caps the file EditFile will read so a pathological target
// cannot exhaust memory. It matches the 1 MiB ceiling used elsewhere in the
// package for human-edited/source files.
const maxEditFileBytes int64 = 1 << 20

// diffPreviewContextLines is how many unchanged lines of context the diff preview
// shows on each side of a changed region (keeps the preview compact but readable).
const diffPreviewContextLines = 2

// editFileSchema is the JSON Schema for EditFile's argument object.
const editFileSchema = `{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Workspace-relative path of the file to edit."},
    "old": {"type": "string", "description": "The exact substring to find. Must match exactly once unless replace_all is true."},
    "new": {"type": "string", "description": "The replacement substring."},
    "replace_all": {"type": "boolean", "description": "Replace every occurrence of 'old' instead of requiring a single unique match."}
  },
  "required": ["path", "old", "new"]
}`

const editFileDesc = "Edit a UTF-8 text file in the workspace by replacing an exact substring. By default 'old' must occur exactly once (a unique edit); set replace_all to replace every occurrence. Returns a diff preview. Edits are confined to the workspace and never follow a final-component symlink. Requires approval before each edit."

// editFileHostWritesDesc is EditFile's Info() description when constructed
// WithHostWrites(): an absolute path may resolve outside the workspace
// (subject to the caller's write authority), and — critically — such edits
// are NOT covered by session checkpoint/undo, which only snapshots the
// workspace. Mirrors writefile.go's writeFileDesc/writeFileHostWritesDesc
// split.
const editFileHostWritesDesc = "Edit a UTF-8 text file by replacing an exact substring. By default 'old' must occur exactly once (a unique edit); set replace_all to replace every occurrence. Returns a diff preview. An absolute path may resolve outside the workspace, subject to the caller's write authority; edits never follow a final-component symlink. Edits outside the workspace are NOT covered by session checkpoint/undo. Requires approval before each edit."

// editFileArgs is the typed decode of EditFile's untrusted argsJSON.
type editFileArgs struct {
	Path       string `json:"path"`
	Old        string `json:"old"`
	New        string `json:"new"`
	ReplaceAll bool   `json:"replace_all"`
}

// EditFile edits a workspace-contained file by exact-string replacement under the
// loop's optimistic-concurrency policy. It depends only on the workspace root
// (least privilege), the loop's shared observation map, and an OPTIONAL session
// workspace coordinator: an edit requires a complete prior read of this path whose
// hash still equals the file's current on-disk hash. When a coordinator is bound the
// commit runs under a SHARED session-mutation + canonical-PATH permit (serializing
// same-real-file edits across loops, excluded by a Bash/checkpoint permit).
type EditFile struct {
	root       string
	obs        tool.WorkspaceObservations
	coord      tool.WorkspaceCoordinator
	hostWrites bool
}

// NewEditFile constructs an EditFile bound to the workspace root and the loop's
// shared observation map (supplied by Files, one per loop binding). A
// WithMutationCoordinator option binds the session workspace coordinator; without it
// the tool runs coordinator-free (the standalone/bare path). A WithHostWrites option
// lets an absolute target resolve outside the workspace instead of being rejected.
func NewEditFile(root string, obs tool.WorkspaceObservations, opts ...FileMutatorOption) *EditFile {
	cfg := resolveFileMutatorConfig(opts)
	return &EditFile{root: root, obs: obs, coord: cfg.coord, hostWrites: cfg.hostWrites}
}

// Info returns EditFile's self-description. Name MUST equal "EditFile". The
// description swaps to editFileHostWritesDesc when hostWrites is set,
// mirroring writefile.go's writeFileDesc/writeFileHostWritesDesc swap.
func (e *EditFile) Info(context.Context) (*tool.ToolInfo, error) {
	desc := editFileDesc
	if e.hostWrites {
		desc = editFileHostWritesDesc
	}
	return &tool.ToolInfo{
		Name:   editFileToolName,
		Desc:   desc,
		Schema: json.RawMessage(editFileSchema),
	}, nil
}

// AuditSummary returns a redacted, content-free one-line summary: the path only
// (never the old/new substrings, which can carry secrets). An unparseable args
// document yields a generic summary.
func (e *EditFile) AuditSummary(argsJSON string) string {
	var a editFileArgs
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil || a.Path == "" {
		return "EditFile (unparsable args)"
	}
	return "EditFile " + a.Path
}

// editFileArtifact is EditFile's typed prepared artifact: the validated,
// canonicalized target plus the exact replacement, bound to one call by
// PrepareCall and consumed by InvokableRun without reparsing the raw JSON. It
// deliberately embeds tool.TokenArtifact to satisfy the sealed
// tool.PreparedArtifact marker; the typed fields stay tool-private.
type editFileArtifact struct {
	tool.TokenArtifact
	target      mutationTarget
	old         string
	replacement string
	replaceAll  bool
}

// prepareEdit is the SINGLE parse-validate-canonicalize step for an EditFile
// call, shared by PrepareCall and WriteTarget so the scheduling key and the
// requirement Scope can never diverge.
func (e *EditFile) prepareEdit(argsJSON string) (*editFileArtifact, error) {
	var a editFileArgs
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return nil, &writeFileError{reason: "invalid arguments: not a JSON object", cause: err}
	}
	if a.Path == "" {
		return nil, &writeFileError{reason: "a non-empty 'path' is required"}
	}
	if a.Old == "" {
		return nil, &writeFileError{reason: "'old' must be a non-empty substring to find"}
	}
	target, err := resolveMutationTarget(e.root, a.Path, e.hostWrites)
	if err != nil {
		return nil, err
	}
	return &editFileArtifact{target: target, old: a.Old, replacement: a.New, replaceAll: a.ReplaceAll}, nil
}

// PrepareCall decodes and validates the untrusted arguments ONCE, resolves the
// canonical edit target ONCE, and returns the typed request — a direct
// filesystem.write requirement whose Scope and Match are the canonical resolved
// path, empty grant pair — plus the typed artifact InvokableRun executes.
// Invalid input fails here and never reaches the permission gate.
//
// For an UNCONTAINED target ONLY, the request also carries a paired
// filesystem.read requirement for the SAME canonical path (see
// pairedReadRequirement): EditFile always performs an in-process read via
// readForEdit before writing back, and a prior host read/write must never
// silently authorize this one — every host read tied to an edit gets its own
// fresh gate decision. A contained target is unchanged: exactly one
// requirement (write only).
func (e *EditFile) PrepareCall(_ context.Context, executionID uuid.UUID, argsJSON string) (tool.Request, tool.PreparedArtifact, error) {
	art, err := e.prepareEdit(argsJSON)
	if err != nil {
		return tool.Request{}, nil, err
	}
	if !art.target.contained {
		return mutationRequest(editFileToolName, executionID.String(), art.target, pairedReadRequirement(art.target.abs)), art, nil
	}
	return mutationRequest(editFileToolName, executionID.String(), art.target), art, nil
}

// WriteTarget returns the CANONICAL prepared edit path as the serialization key
// (an edit is a write), derived by the same preparation step that emits the
// requirement Scope — preparation is the single source of the scheduling key.
// ok is true for a well-formed call; a non-nil err (bad args/escape) tells the
// runner to treat the call as invalid.
func (e *EditFile) WriteTarget(argsJSON string) (string, bool, error) {
	art, err := e.prepareEdit(argsJSON)
	if err != nil {
		return "", false, err
	}
	return art.target.abs, true, nil
}

// InvokableRun executes the PREPARED artifact bound to this call — the raw
// argsJSON is never reparsed, so mutating it after preparation changes
// nothing. Without its typed artifact the effectful tool fails closed. It
// applies the edit and returns a diff preview, or a tool-result error string
// for every failure mode. Never a Go error, never echoing the full file body.
func (e *EditFile) InvokableRun(ctx context.Context, _ string) (*tool.ToolResult, error) {
	art, ok := prepared.FromContext[*editFileArtifact](ctx)
	if !ok || art == nil {
		return tool.TextResult("error: permission denied: EditFile requires its prepared call artifact"), nil
	}

	// Stage 1: enforce the APPROVED resolved path (see WriteFile.InvokableRun):
	// a resolution changed since preparation refuses the edit fail-closed.
	if err := enforceApprovedResolution(e.root, art.target, e.hostWrites); err != nil {
		return tool.TextResult("error: " + err.Error()), nil
	}

	// Stage 2: take the SHARED session-mutation + canonical-PATH permit (and verify
	// lease health) BEFORE the commit critical section — the OUTER lock over commit's
	// per-path st.mu (consistent ordering). A ctx-canceled acquire or an unhealthy
	// lease returns WITHOUT editing.
	key := canonicalObservationKey(art.target.abs)
	permit, err := acquirePathMutation(ctx, e.coord, key)
	if err != nil {
		return tool.TextResult("error: " + err.Error()), nil
	}
	defer permit.Release()

	// Stage 3: commit under the path's optimistic-concurrency critical section (for a
	// CONTAINED target) or bypassing the observation map entirely (for an UNCONTAINED
	// host target — see commitUncontained's doc comment). The read/write operate on
	// the LEXICAL joined path (NOT the symlink-resolved form), mirroring ReadFile: the
	// O_NOFOLLOW read rejects a final-component symlink rather than following it, and
	// the atomic write targets the same lexical name so it REPLACES a final-component
	// symlink rather than following it.
	preview, err := e.commit(key, art.target, art.old, art.replacement, art.replaceAll)
	if err != nil {
		return tool.TextResult("error: " + err.Error()), nil
	}
	return tool.TextResult(preview), nil
}

// commit performs the edit for one target while (for a CONTAINED target) holding
// that path's optimistic-concurrency critical section, or (for an UNCONTAINED host
// target) bypassing the observation map entirely — see commitUncontained's doc
// comment for why. key is the observation-map key (meaningful only for a contained
// target); target carries the lexical on-disk name, the display path used in error
// messages/the diff header, and the contained flag that selects which policy
// applies; old/replacement/replaceAll are the requested edit.
func (e *EditFile) commit(key canonicalObservationKey, target mutationTarget, old, replacement string, replaceAll bool) (string, error) {
	if !target.contained {
		return e.commitUncontained(target, old, replacement, replaceAll)
	}
	var preview string
	err := e.obs.WithPath(string(key), func(obs *tool.FileObservation) error {
		// Cheap classify first (a single Lstat, no content read). An absent target is an
		// honest "file not found" (there is nothing to edit, and "read again" would
		// dead-end); a symlink/non-regular target is refused with the DISTINCT
		// IrregularFileError (re-reading it via ReadFile also fails O_NOFOLLOW, so
		// "read again" would dead-end there too).
		switch classifyWriteTarget(target.lexical) {
		case writeTargetAbsent:
			return &writeFileError{reason: "file not found"}
		case writeTargetIrregular:
			*obs = tool.FileObservation{}
			return &IrregularFileError{Path: target.display}
		}

		// writeTargetRegular. Read the file ONCE (that same read yields both the bytes to
		// edit and the hash to compare); the read is bounded by maxEditFileBytes, and a
		// read failure (too-large, or a race to symlink/absent since the classify) is
		// returned as-is — it is not an optimistic-concurrency conflict, so it must not
		// masquerade as a StaleFileError telling the model to "read again".
		original, rerr := e.readForEdit(target.lexical)
		if rerr != nil {
			return rerr
		}

		// Freshness: the edit is authorized only if this loop completely observed the
		// file and its recorded hash still equals the current on-disk content.
		if curHash := sha256.Sum256([]byte(original)); !obs.Observed || !obs.Present || obs.Hash != curHash {
			*obs = tool.FileObservation{}
			return &StaleFileError{Path: target.display}
		}

		updated, errMsg := applyReplacement(original, old, replacement, replaceAll)
		if errMsg != "" {
			return &editAnchorError{message: errMsg}
		}
		if err := atomicWriteFile(target.lexical, []byte(updated)); err != nil {
			return err
		}
		*obs = tool.FileObservation{Observed: true, Present: true, Hash: sha256.Sum256([]byte(updated))}
		preview = diffPreview(target.display, original, updated)
		return nil
	})
	return preview, err
}

// commitUncontained edits an UNCONTAINED (host) target WITHOUT ever touching the
// observation map, in either direction: it does not require a prior observation
// before editing an existing file, and it does not record one after a successful
// edit. This is a deliberate product decision, not an oversight — a prior host
// read/write must never authorize a later host edit, and vice versa (the same
// decision WithHostReads() enforces on the read side, and commitUncontained
// enforces on WriteFile's write side).
//
// EditFile's freshness mechanism differs from WriteFile's in a way that matters
// here: EditFile ALREADY reads the current file fresh at commit time
// (readForEdit), and the recorded observation hash is used ONLY as a comparator
// against that fresh read — never as the source of the bytes being edited. So for
// an uncontained target, this method simply SKIPS the
// obs.Observed/obs.Present/obs.Hash comparison; everything else (the fresh read,
// the exact-`old`-substring anchor match, the atomic write-back, the diff preview)
// stays IDENTICAL to the contained path. The anchor match in applyReplacement is
// itself real freshness protection, but its strength depends on replace_all:
//   - replace_all=false: it degrades gracefully. applyReplacement requires
//     EXACTLY ONE occurrence of `old`, so if the file changed underneath since
//     the model last saw it in a way that alters the occurrence count (zero, or
//     two-or-more), the edit refuses loudly with a distinct editAnchorError —
//     never a silent wrong-edit.
//   - replace_all=true: the check is weaker. applyReplacement only requires AT
//     LEAST ONE occurrence and then replaces ALL of them, so it confirms `old`
//     still exists somewhere, not that the occurrence SET is unchanged. If the
//     file drifts between the model's read and this commit such that a NEW
//     occurrence of `old` appears somewhere the model never saw, that occurrence
//     is silently replaced too — a contained target does not have this gap,
//     because its CAS check requires a fresh full-file read matching the exact
//     current on-disk hash before any edit is authorized, so ANY drift (not just
//     a changed occurrence count) is caught before applyReplacement ever runs.
//     This is an accepted, intentional residual risk of skipping the CAS check
//     for host targets (see TestEditFileHostWritesReplaceAllOverReplacesDriftedOccurrences),
//     not an oversight.
//
// This is why no NEW freshness mechanism is needed for the non-replace_all
// uncontained case — the anchor match already does freshness-adjacent work, and
// the CAS check on top of it was specifically about not editing based on a stale
// cached hash the model never actually re-verified, which does not apply once
// host edits get no cached authority at all.
//
// Unlike WriteFile, EditFile never creates a file (an absent target is already an
// honest "file not found" — there is nothing to edit), so no parent-directory
// pre-flight check is needed here.
//
// Like WriteFile's commitUncontained, bypassing e.obs.WithPath here also bypasses
// its internal per-canonical-path lock: cross-loop serialization for the SAME
// real file still happens one layer up via the coordinator's PathMutation permit
// (InvokableRun acquires it before commit is ever called) so long as a
// coordinator is bound, but a coordinator-free EditFile gives an uncontained
// target NO same-loop serialization at all — unlike a contained target, which
// still serializes on the WithPath lock even coordinator-free. See WriteFile's
// commitUncontained doc comment for the full discussion.
func (e *EditFile) commitUncontained(target mutationTarget, old, replacement string, replaceAll bool) (string, error) {
	switch classifyWriteTarget(target.lexical) {
	case writeTargetAbsent:
		return "", &writeFileError{reason: "file not found"}
	case writeTargetIrregular:
		// A final-component symlink or other non-regular node: refused exactly as for
		// a contained target.
		return "", &IrregularFileError{Path: target.display}
	}

	// writeTargetRegular. Read the file ONCE, exactly as the contained path does; the
	// only thing skipped for an uncontained target is the obs comparison below.
	original, rerr := e.readForEdit(target.lexical)
	if rerr != nil {
		return "", rerr
	}

	// No observation check, no hash comparison: an uncontained edit is authorized by
	// the fresh approval alone, never by a same-loop observation (in either
	// direction). The anchor match below is the freshness protection that remains —
	// full strength for replace_all=false, weaker for replace_all=true (see this
	// method's doc comment).
	updated, errMsg := applyReplacement(original, old, replacement, replaceAll)
	if errMsg != "" {
		return "", &editAnchorError{message: errMsg}
	}
	if err := atomicWriteFile(target.lexical, []byte(updated)); err != nil {
		return "", err
	}
	return diffPreview(target.display, original, updated), nil
}

// readForEdit opens path with a no-follow open (a final-component symlink or
// reparse point fails to open — see internal/nofollow), confirms a regular file
// via the fd stat, and reads up to maxEditFileBytes. path is the LEXICAL joined
// path (joinedUnderRoot); the caller has already proven the symlink-resolved
// form is contained. Errors are typed writeFileError (non-secret reason, never
// contents).
func (e *EditFile) readForEdit(path string) (string, error) {
	// #nosec G304 -- path is workspace.JoinedPath(root, input): the workspace root +
	// the lexically-cleaned, contained input (containedPath already proved the
	// symlink-resolved target is inside the workspace). The no-follow open rejects a
	// FINAL-COMPONENT symlink/reparse point (consistent with ReadFile); it does NOT
	// by itself close the broader parent-dir resolve→open TOCTOU window, which §3c
	// (write-side threat model) explicitly accepts as out of scope for this local
	// single-user tool.
	f, err := nofollow.Open(path, os.O_RDONLY, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", &writeFileError{reason: "file not found", cause: err}
		}
		if isSymlinkLoop(err) {
			return "", &writeFileError{reason: "refusing to follow a symlinked path", cause: err}
		}
		return "", &writeFileError{reason: "could not open the file", cause: err}
	}
	defer func() { _ = f.Close() }()

	fi, err := f.Stat()
	if err != nil {
		return "", &writeFileError{reason: "could not stat the file", cause: err}
	}
	if !fi.Mode().IsRegular() {
		return "", &writeFileError{reason: "not a regular file"}
	}

	data, err := io.ReadAll(io.LimitReader(f, maxEditFileBytes+1))
	if err != nil {
		return "", &writeFileError{reason: "could not read the file", cause: err}
	}
	if int64(len(data)) > maxEditFileBytes {
		return "", &writeFileError{reason: "file is too large to edit (exceeds the " + strconv.FormatInt(maxEditFileBytes, 10) + "-byte cap)"}
	}
	return string(data), nil
}

// applyReplacement enforces the occurrence rules and returns the updated content.
// `replacement` is the new substring (the param is named replacement, not `new`,
// to avoid shadowing the builtin). On a rule violation it returns ("", errMsg) —
// a non-secret message naming the match count, never the file body. On success it
// returns (updated, "").
func applyReplacement(original, old, replacement string, replaceAll bool) (string, string) {
	n := strings.Count(original, old)
	switch {
	case n == 0:
		return "", "'old' substring not found in the file"
	case n >= 2 && !replaceAll:
		return "", "ambiguous: 'old' matches " + strconv.Itoa(n) + " times; set replace_all to replace every occurrence"
	case replaceAll:
		return strings.ReplaceAll(original, old, replacement), ""
	default: // exactly 1 match
		return strings.Replace(original, old, replacement, 1), ""
	}
}

// isSymlinkLoop reports whether err is a no-follow refusal (a final-component
// symlink on POSIX, or a reparse point on Windows — see internal/nofollow).
func isSymlinkLoop(err error) bool {
	return errors.Is(err, nofollow.ErrSymlinkNotAllowed)
}

// compile-time assertions: EditFile is an InvokableTool, a CallPreparer,
// Auditable, and a WriteTarget.
var (
	_ tool.InvokableTool = (*EditFile)(nil)
	_ tool.CallPreparer  = (*EditFile)(nil)
	_ tool.Auditable     = (*EditFile)(nil)
	_ tool.WriteTarget   = (*EditFile)(nil)
)
