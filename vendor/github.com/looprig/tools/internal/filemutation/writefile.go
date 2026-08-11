package filemutation

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/tools/internal/nofollow"
	"github.com/looprig/tools/internal/prepared"
)

// writefile.go implements the WriteFile tool: a workspace-contained atomic file
// writer (design §4b). WriteFile is a CallPreparer (PrepareCall emits the
// direct filesystem.write requirement for the canonical resolved target and
// binds the typed artifact InvokableRun executes), is Auditable with a
// content-free summary, and is a WriteTarget so the runner serializes
// same-resolved-path writes. Whether the resolved path is denied, gated, or
// allowed is the gate evaluator's decision; the tool still runs containment
// itself for correct path resolution and defense in depth.
//
// Path handling: containedPath proves the symlink-RESOLVED target is inside the
// workspace (an escape — including an in-workspace symlink pointing out — is
// rejected). The atomic write then targets the LEXICAL joined path, so a write to
// a path whose final component is an existing in-workspace symlink REPLACES the
// symlink with the new regular file rather than following it to clobber the
// symlink's target (consistent with ReadFile/EditFile not silently following a
// final-component symlink).
//
// Atomicity: parent dirs are created (MkdirAll on the lexical parent), then the
// content is written to a uniquely-named temp file in the SAME directory opened
// O_CREATE|O_EXCL|O_WRONLY|O_NOFOLLOW (refuses a pre-planted symlinked temp, no
// clobber), fsync'd, closed, and os.Rename'd over the target (atomic within a
// directory; rename replaces a final-component symlink rather than following it).
// The temp file is removed on any failure so no half-written litter is left
// behind. This mirrors store.go's writeApprovalsFileAtomically hardening.
//
// O_NOFOLLOW on the temp open rejects a pre-planted symlink AT THE TEMP NAME; it
// does NOT close the broader parent-dir resolve→open TOCTOU window (a parent dir
// swapped to a symlink between the containment check and the write). §3c
// (write-side threat model) explicitly accepts that residual window as out of
// scope for this local single-user tool acting with the user's own privileges;
// the O_EXCL|O_NOFOLLOW temp is cheap defence-in-depth, not a complete TOCTOU fix.

// writeFileToolName is the EXACT tool name — it MUST equal "WriteFile".
const writeFileToolName = "WriteFile"

// newFilePerm is the mode a freshly-written file ends up with. The temp file is
// created 0o600 (owner-only while being written), and that mode carries through
// the Rename — a written source file is owner read/write, no group/world bits.
const newFilePerm os.FileMode = 0o600

// newDirPerm is the mode for parent directories created by MkdirAll (owner rwx,
// group/other read+execute), the conventional directory mode for a workspace.
const newDirPerm os.FileMode = 0o755

// writeFileSchema is the JSON Schema for WriteFile's argument object.
const writeFileSchema = `{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Workspace-relative path of the file to write (parent directories are created as needed)."},
    "content": {"type": "string", "description": "The full file contents to write. The file is overwritten atomically."}
  },
  "required": ["path", "content"]
}`

const writeFileDesc = "Write a UTF-8 text file in the workspace, creating parent directories as needed and overwriting any existing file atomically. Writes are confined to the workspace and never follow a final-component symlink. Requires approval before each write."

// writeFileHostWritesDesc is WriteFile's Info() description when constructed
// WithHostWrites(): an absolute path may resolve outside the workspace
// (subject to the caller's write authority), and — critically — such writes
// are NOT covered by session checkpoint/undo, which only snapshots the
// workspace.
const writeFileHostWritesDesc = "Write a UTF-8 text file, creating parent directories as needed and overwriting any existing file atomically. An absolute path may resolve outside the workspace, subject to the caller's write authority; writes never follow a final-component symlink. Writes outside the workspace are NOT covered by session checkpoint/undo. Requires approval before each write."

// writeFileArgs is the typed decode of WriteFile's untrusted argsJSON.
type writeFileArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// WriteFile writes a workspace-contained file atomically under the loop's
// optimistic-concurrency policy. It depends only on the workspace root (least
// privilege — deny rules are evaluated by the gate over the prepared
// filesystem.write requirement), the loop's shared
// observation map, and an OPTIONAL session workspace coordinator: overwriting an
// EXISTING file requires a complete prior read of this path whose hash still equals
// the file's current on-disk hash; a genuinely ABSENT path may be created without any
// prior read via an atomic no-replace publication. When a coordinator is bound the
// commit runs under a SHARED session-mutation + canonical-PATH permit (design
// §"File-tool optimistic concurrency and binding"), which serializes same-real-file
// writes ACROSS loops (the private observation map only serializes within one loop).
//
// This optimistic-concurrency policy applies ONLY to a CONTAINED (in-workspace)
// target. An UNCONTAINED target (WithHostWrites(), an absolute path resolving
// outside the workspace) never consults or updates the observation map in either
// direction — see commitUncontained's doc comment for the product decision behind
// that split.
type WriteFile struct {
	root       string
	obs        tool.WorkspaceObservations
	coord      tool.WorkspaceCoordinator
	hostWrites bool
}

// NewWriteFile constructs a WriteFile bound to the workspace root and the loop's
// shared observation map (supplied by Files, one per loop binding). A
// WithMutationCoordinator option binds the session workspace coordinator; without it
// the tool runs coordinator-free (the standalone/bare path). A WithHostWrites option
// lets an absolute target resolve outside the workspace instead of being rejected.
func NewWriteFile(root string, obs tool.WorkspaceObservations, opts ...FileMutatorOption) *WriteFile {
	cfg := resolveFileMutatorConfig(opts)
	return &WriteFile{root: root, obs: obs, coord: cfg.coord, hostWrites: cfg.hostWrites}
}

// Info returns WriteFile's self-description. Name MUST equal "WriteFile". The
// description swaps to writeFileHostWritesDesc when hostWrites is set,
// mirroring readfile.go's readFileDesc/readFileHostReadsDesc swap.
func (w *WriteFile) Info(context.Context) (*tool.ToolInfo, error) {
	desc := writeFileDesc
	if w.hostWrites {
		desc = writeFileHostWritesDesc
	}
	return &tool.ToolInfo{
		Name:   writeFileToolName,
		Desc:   desc,
		Schema: json.RawMessage(writeFileSchema),
	}, nil
}

// AuditSummary returns a redacted, content-free one-line summary: the path and
// byte count only — NEVER the content. An unparseable args document yields a
// generic summary.
func (w *WriteFile) AuditSummary(argsJSON string) string {
	var a writeFileArgs
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil || a.Path == "" {
		return "WriteFile (unparsable args)"
	}
	return "WriteFile " + a.Path + " (" + strconv.Itoa(len(a.Content)) + " bytes)"
}

// writeFileArtifact is WriteFile's typed prepared artifact: the validated,
// canonicalized target plus the exact content to write, bound to one call by
// PrepareCall and consumed by InvokableRun without reparsing the raw JSON. It
// deliberately embeds tool.TokenArtifact to satisfy the sealed
// tool.PreparedArtifact marker; the typed fields stay tool-private.
type writeFileArtifact struct {
	tool.TokenArtifact
	target  mutationTarget
	content string
}

// prepareWrite is the SINGLE parse-validate-canonicalize step for a WriteFile
// call, shared by PrepareCall and WriteTarget so the scheduling key and the
// requirement Scope can never diverge.
func (w *WriteFile) prepareWrite(argsJSON string) (*writeFileArtifact, error) {
	var a writeFileArgs
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return nil, &writeFileError{reason: "invalid arguments: not a JSON object", cause: err}
	}
	if a.Path == "" {
		return nil, &writeFileError{reason: "a non-empty 'path' is required"}
	}
	target, err := resolveMutationTarget(w.root, a.Path, w.hostWrites)
	if err != nil {
		return nil, err
	}
	return &writeFileArtifact{target: target, content: a.Content}, nil
}

// PrepareCall decodes and validates the untrusted arguments ONCE, resolves the
// canonical write target ONCE, and returns the typed request — one direct
// filesystem.write requirement whose Scope and Match are the canonical resolved
// path, empty grant pair — plus the typed artifact InvokableRun executes.
// Invalid input fails here and never reaches the permission gate.
func (w *WriteFile) PrepareCall(_ context.Context, executionID uuid.UUID, argsJSON string) (tool.Request, tool.PreparedArtifact, error) {
	art, err := w.prepareWrite(argsJSON)
	if err != nil {
		return tool.Request{}, nil, err
	}
	return mutationRequest(writeFileToolName, executionID.String(), art.target), art, nil
}

// WriteTarget returns the CANONICAL prepared write path as the serialization
// key so the runner groups concurrent writes to the same on-disk file. It is
// derived by the same preparation step that emits the requirement Scope —
// preparation is the single source of the scheduling key. ok is true for every
// well-formed write; a non-nil err (unparseable args or an escape) tells the
// runner to treat the call as invalid rather than execute it ungrouped.
func (w *WriteFile) WriteTarget(argsJSON string) (string, bool, error) {
	art, err := w.prepareWrite(argsJSON)
	if err != nil {
		return "", false, err
	}
	return art.target.abs, true, nil
}

// InvokableRun executes the PREPARED artifact bound to this call — the raw
// argsJSON is never reparsed, so mutating it after preparation changes
// nothing. Without its typed artifact the effectful tool fails closed. Every
// failure mode is a tool-result error string — never a Go error and never
// echoing the content.
func (w *WriteFile) InvokableRun(ctx context.Context, _ string) (*tool.ToolResult, error) {
	art, ok := prepared.FromContext[*writeFileArtifact](ctx)
	if !ok || art == nil {
		return tool.TextResult("error: permission denied: WriteFile requires its prepared call artifact"), nil
	}

	// Stage 1: enforce the APPROVED resolved path. Preparation proved
	// containment for the permission decision; re-proving that the resolution
	// still equals the approved canonical target closes the prepare→run window
	// (a parent-dir symlink swap redirects the write nowhere: it is refused).
	if err := enforceApprovedResolution(w.root, art.target, w.hostWrites); err != nil {
		return tool.TextResult("error: " + err.Error()), nil
	}

	// Stage 2: take the SHARED session-mutation + canonical-PATH permit (and verify
	// lease health) BEFORE the commit critical section, so same-real-file writes
	// serialize across loops and an unhealthy lease blocks the write. The coordinator
	// permit is the OUTER lock; commit's per-path st.mu is the INNER lock (consistent
	// ordering, no inversion). A ctx-canceled acquire or an unhealthy lease returns
	// WITHOUT writing.
	key := canonicalObservationKey(art.target.abs)
	permit, err := acquirePathMutation(ctx, w.coord, key)
	if err != nil {
		return tool.TextResult("error: " + err.Error()), nil
	}
	defer permit.Release()

	// Stage 3: commit under the path's optimistic-concurrency critical section. The
	// on-disk write targets the LEXICAL joined path (NOT the symlink-resolved form),
	// mirroring ReadFile/EditFile: an atomic Rename/Link on this lexical name never
	// follows a final-component symlink.
	if err := w.commit(key, art.target, []byte(art.content)); err != nil {
		return tool.TextResult("error: " + err.Error()), nil
	}
	return tool.TextResult("wrote " + art.target.display + " (" + strconv.Itoa(len(art.content)) + " bytes)"), nil
}

// commit performs the write for one target while (for a CONTAINED target) holding
// that path's optimistic-concurrency critical section, or (for an UNCONTAINED host
// target) bypassing the observation map entirely — see commitUncontained's doc
// comment for why. key is the observation-map key (meaningful only for a contained
// target); target carries the lexical on-disk name, the display path used in error
// messages, and the contained flag that selects which policy applies; data is the
// content to write.
func (w *WriteFile) commit(key canonicalObservationKey, target mutationTarget, data []byte) error {
	if !target.contained {
		return w.commitUncontained(target, data)
	}
	return w.obs.WithPath(string(key), func(obs *tool.FileObservation) error {
		switch classifyWriteTarget(target.lexical) {
		case writeTargetAbsent:
			// Create a genuinely new file. No prior read is required. The no-replace
			// publication fails without clobbering if another writer wins the race
			// between this absence check and the link.
			if err := atomicCreateFile(target.lexical, data); err != nil {
				*obs = tool.FileObservation{}
				if errors.Is(err, errCreateConflict) {
					return &FileCreateConflictError{Path: target.display}
				}
				return err
			}
			*obs = tool.FileObservation{Observed: true, Present: true, Hash: sha256.Sum256(data)}
			return nil

		case writeTargetIrregular:
			// A final-component symlink or other non-regular node: it cannot be observed
			// (a ReadFile refuses it O_NOFOLLOW), so overwriting is refused with a
			// distinct error that does NOT tell the model to "read again".
			*obs = tool.FileObservation{}
			return &IrregularFileError{Path: target.display}
		}

		// writeTargetRegular. Without a complete observation the overwrite is doomed
		// regardless of the bytes, so refuse BEFORE hashing — avoiding an O(file-size)
		// read of a file we may not touch. Only when an observation exists do we hash
		// the current contents to complete the compare-and-swap.
		if !obs.Observed || !obs.Present {
			*obs = tool.FileObservation{}
			return &StaleFileError{Path: target.display}
		}
		curHash, present, err := hashFileOnDisk(target.lexical)
		if err != nil {
			// The target became irregular/unreadable since the classify (a race): fail
			// secure with the distinct irregular error.
			*obs = tool.FileObservation{}
			return &IrregularFileError{Path: target.display}
		}
		if !present || obs.Hash != curHash {
			// Vanished, or its content changed since our read: an optimistic-concurrency
			// conflict — read again.
			*obs = tool.FileObservation{}
			return &StaleFileError{Path: target.display}
		}
		if err := atomicWriteFile(target.lexical, data); err != nil {
			return err
		}
		*obs = tool.FileObservation{Observed: true, Present: true, Hash: sha256.Sum256(data)}
		return nil
	})
}

// commitUncontained writes an UNCONTAINED (host) target WITHOUT ever touching the
// observation map, in either direction: it does not require a prior observation
// before overwriting an existing file, and it does not record one after a
// successful write or create. This is a deliberate product decision, not an
// oversight — a prior host READ must never authorize a later host WRITE, and vice
// versa (the same decision WithHostReads() already enforces on the read side by
// never recording an observation for an uncontained read). Recording an
// observation here would recreate exactly the authority-inheritance channel that
// decision forbids. The cleanest shape for that is to bypass w.obs.WithPath (and
// thus commit's per-path critical section) entirely for an uncontained target —
// cross-loop serialization for the SAME real file still happens one layer up, via
// the coordinator's PathMutation permit InvokableRun acquires before commit is
// ever called, SO LONG AS a coordinator is bound (the runtime always binds one
// through the Files definition). A coordinator-free WriteFile (the standalone/bare
// unit-test path, where acquirePathMutation is a documented no-op) gives an
// uncontained target NO same-loop serialization at all — unlike a contained
// target, which still serializes on w.obs.WithPath's internal per-path lock even
// coordinator-free. This is not a corruption risk (os.Rename/os.Link stay atomic
// at the OS level; the worst case is last-write-wins), but it is a real asymmetry
// with the contained path.
func (w *WriteFile) commitUncontained(target mutationTarget, data []byte) error {
	switch classifyWriteTarget(target.lexical) {
	case writeTargetAbsent:
		// Unlike the contained path, stageTempFile's MkdirAll must NOT be allowed to
		// silently manufacture a directory chain the approval never named (a typo'd
		// host path). Require the parent to already exist before attempting the
		// create. This check has a TOCTOU gap of its own: atomicCreateFile ->
		// stageTempFile still unconditionally MkdirAlls this same parent a few lines
		// later, so a parent removed/swapped in the narrow window between this Stat
		// and that MkdirAll is silently recreated — the same class of parent-dir
		// resolve->open TOCTOU the package doc comment's §3c already accepts as out
		// of scope for this local single-user tool, not a new hole.
		parent := filepath.Dir(target.lexical)
		fi, err := os.Stat(parent)
		switch {
		case errors.Is(err, os.ErrNotExist):
			return &writeFileError{reason: "parent directory does not exist", cause: err}
		case err != nil:
			return &writeFileError{reason: "could not stat parent directory", cause: err}
		case !fi.IsDir():
			return &writeFileError{reason: "parent path exists but is not a directory"}
		}
		if err := atomicCreateFile(target.lexical, data); err != nil {
			if errors.Is(err, errCreateConflict) {
				return &FileCreateConflictError{Path: target.display}
			}
			return err
		}
		return nil

	case writeTargetIrregular:
		// A final-component symlink or other non-regular node: refused exactly as for
		// a contained target.
		return &IrregularFileError{Path: target.display}
	}

	// writeTargetRegular. No observation check, no hash comparison: an uncontained
	// overwrite is authorized by the fresh approval alone, never by a same-loop
	// observation (in either direction).
	return atomicWriteFile(target.lexical, data)
}

// writeTargetKind classifies a write/edit target's final component without reading
// its contents.
type writeTargetKind int

const (
	writeTargetAbsent    writeTargetKind = iota // no node at the path
	writeTargetRegular                          // a plain regular file
	writeTargetIrregular                        // a final-component symlink or other non-regular node
)

// classifyWriteTarget cheaply classifies the LEXICAL target via a single os.Lstat
// (no content read, and — being Lstat — a final-component symlink is detected, never
// followed). A stat error other than not-exist is treated as irregular so the caller
// fails secure (deny). This lets commit reject a doomed unobserved overwrite, or an
// irregular target, WITHOUT the O(file-size) hash read.
func classifyWriteTarget(lexical string) writeTargetKind {
	fi, err := os.Lstat(lexical)
	if err != nil {
		if os.IsNotExist(err) {
			return writeTargetAbsent
		}
		return writeTargetIrregular
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
		return writeTargetIrregular
	}
	return writeTargetRegular
}

// atomicWriteFile publishes data to an EXISTING (or to-be-replaced) target: it
// stages a sibling temp file and os.Rename's it over target. target is the LEXICAL
// joined path (the caller has proved its symlink-resolved form is contained);
// rename to a target that is a final-component symlink REPLACES the symlink rather
// than following it. The temp file is removed on any post-stage failure. All
// failures are typed writeFileError (non-secret reason, never contents).
func atomicWriteFile(target string, data []byte) error {
	tmp, err := stageTempFile(filepath.Dir(target), data)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return &writeFileError{reason: "could not rename temp file into place", cause: err}
	}
	return nil
}

// errCreateConflict is the leaf cause atomicCreateFile returns when the no-replace
// link fails because the destination already exists. It carries no context;
// WriteFile.commit wraps it into a *FileCreateConflictError with the display path.
var errCreateConflict = errors.New("create destination already exists")

// atomicCreateFile publishes data to a currently-ABSENT target via an atomic
// no-replace publication: it stages a sibling temp file, then os.Link's it into the
// destination. os.Link fails with EEXIST if target already exists — the conflict
// signal (surfaced as errCreateConflict) — so a create never clobbers an existing
// file or follows/replaces a final-component symlink. On success the temp link is
// removed, leaving target as the sole link to the written inode. target is the
// LEXICAL joined path (the caller proved its symlink-resolved form is contained).
func atomicCreateFile(target string, data []byte) error {
	tmp, err := stageTempFile(filepath.Dir(target), data)
	if err != nil {
		return err
	}
	if err := os.Link(tmp, target); err != nil {
		_ = os.Remove(tmp)
		if errors.Is(err, os.ErrExist) {
			return errCreateConflict
		}
		return &writeFileError{reason: "could not link new file into place", cause: err}
	}
	// The publication succeeded via the link; drop the redundant temp name.
	_ = os.Remove(tmp)
	return nil
}

// stageTempFile creates dir (and ancestors), writes data to a uniquely-named temp
// file in the SAME directory (O_CREATE|O_EXCL|O_WRONLY|O_NOFOLLOW @0600), fsyncs and
// closes it, and returns the temp path for the caller to publish (rename over, or
// link into, the final destination). Same-directory placement guarantees an atomic
// rename AND a same-filesystem link. All failures are typed writeFileError.
func stageTempFile(dir string, data []byte) (string, error) {
	if err := os.MkdirAll(dir, newDirPerm); err != nil {
		return "", &writeFileError{reason: "could not create parent directories", cause: err}
	}
	tmp, err := uniqueWriteTempPath(dir)
	if err != nil {
		return "", err
	}
	// #nosec G304 -- tmp = target's containment-proven parent dir + a crypto/rand
	// suffix. O_EXCL + the no-follow open refuse to clobber an existing name or to
	// follow a pre-planted symlink/reparse point AT THE TEMP NAME (cheap
	// defence-in-depth; see internal/nofollow). This does NOT close the broader
	// parent-dir resolve→open TOCTOU window, which §3c (write-side threat model)
	// explicitly accepts as out of scope for this local single-user tool. target's
	// resolved form was proven contained by the caller.
	f, err := nofollow.Open(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, newFilePerm)
	if err != nil {
		return "", &writeFileError{reason: "could not create temp file", cause: err}
	}
	if err := writeSyncClose(f, data); err != nil {
		_ = os.Remove(tmp)
		return "", &writeFileError{reason: "could not write temp file", cause: err}
	}
	return tmp, nil
}

// hashFileOnDisk computes the SHA-256 of target's COMPLETE current raw bytes for
// the optimistic-concurrency compare. It opens with a no-follow read open (a
// final-component symlink or reparse point fails to open — see
// internal/nofollow) and streams the file through the hash (O(1) memory, any
// size). present is false with a nil error ONLY for a definitive not-found; any
// other open/stat/read failure (symlink, non-regular, unreadable) returns a
// non-nil error so the caller fails secure (treats the state as unverifiable and
// refuses the mutation). The hash is never exposed to the model.
func hashFileOnDisk(target string) (hash [sha256.Size]byte, present bool, err error) {
	// #nosec G304 -- target is the containment-proven lexical joined path; the
	// no-follow open rejects a final-component symlink/reparse point and the fd
	// stat confirms a regular file before any bytes are read.
	f, oerr := nofollow.Open(target, os.O_RDONLY, 0)
	if oerr != nil {
		if errors.Is(oerr, os.ErrNotExist) {
			return hash, false, nil
		}
		return hash, false, &writeFileError{reason: "could not open the file to verify freshness", cause: oerr}
	}
	defer func() { _ = f.Close() }()

	fi, serr := f.Stat()
	if serr != nil {
		return hash, false, &writeFileError{reason: "could not stat the file to verify freshness", cause: serr}
	}
	if !fi.Mode().IsRegular() {
		return hash, false, &writeFileError{reason: "not a regular file"}
	}

	h := sha256.New()
	if _, cerr := io.Copy(h, f); cerr != nil {
		return hash, false, &writeFileError{reason: "could not read the file to verify freshness", cause: cerr}
	}
	h.Sum(hash[:0])
	return hash, true, nil
}

// uniqueWriteTempPath returns a never-before-used temp file path in dir using a
// crypto/rand suffix (collision-resistant; O_EXCL still guards the create). It
// does NOT create the file — the caller opens it O_EXCL|O_NOFOLLOW. This mirrors
// store.go's uniqueTempPath but for the write tools' temp files.
func uniqueWriteTempPath(dir string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", &writeFileError{reason: "could not generate temp file name", cause: err}
	}
	return filepath.Join(dir, ".looprig-write-"+hex.EncodeToString(b[:])+".tmp"), nil
}

// writeFileError is the typed failure for a WriteFile/EditFile write attempt. It
// carries a non-secret reason and an optional cause; its message NEVER includes
// file contents.
type writeFileError struct {
	reason string
	cause  error
}

func (e *writeFileError) Error() string { return e.reason }

func (e *writeFileError) Unwrap() error { return e.cause }

// compile-time assertions: WriteFile is an InvokableTool, a CallPreparer,
// Auditable, and a WriteTarget.
var (
	_ tool.InvokableTool = (*WriteFile)(nil)
	_ tool.CallPreparer  = (*WriteFile)(nil)
	_ tool.Auditable     = (*WriteFile)(nil)
	_ tool.WriteTarget   = (*WriteFile)(nil)
)
