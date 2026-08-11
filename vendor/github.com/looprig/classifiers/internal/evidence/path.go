// Package evidence implements the command-safety classifier's read-only
// evidence-tool pack (design doc §13.2): bounded filesystem inspection
// (path.go), bounded Git repository inspection (git.go), an injectable
// repository-visibility seam (visibility.go), and the catalog that wires
// them into Harness's tool.Definition/hustle.EvidenceToolPolicy contracts
// (catalog.go).
//
// Every tool in this package is built exclusively from
// tool.EvidenceFactoryBindings.ReadWorkspace — a single canonical,
// consumer-supplied root directory. Harness's own evidence runner
// (internal/hustleruntime/evidence_runner.go, not part of this module)
// independently re-verifies containment via a consumer-supplied
// EvidenceContainmentVerifier before any call executes; that is the
// authoritative boundary. This package's own containment logic exists
// anyway, as defense in depth against TOCTOU between that check and this
// tool's actual filesystem access (design §13.4): every access to the
// filesystem funnels through *os.Root (Go's syscall-level, symlink-aware
// root confinement primitive), never through unconfined os/filepath calls
// on attacker-influenced paths.
package evidence

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/tool"
)

// Requirement.Kind constants for the filesystem evidence pack. These are
// exported and stable: a consumer's evidence-tool access allowlist
// (tool.EvidenceContainmentVerifier's caller, per design §13.1) configures
// trust by exact kind string, so renaming these is a breaking change for
// every consumer.
const (
	// KindFilesystemStat covers canonical path resolution, lstat-style
	// metadata (not following the final symlink), and resolved-target
	// metadata. It never reads file contents.
	KindFilesystemStat = "evidence.filesystem.stat"
	// KindFilesystemList covers bounded, single-level directory listing.
	KindFilesystemList = "evidence.filesystem.list"
	// KindFilesystemRead covers bounded file content reading.
	KindFilesystemRead = "evidence.filesystem.read"
	// KindFilesystemGlob covers bounded filename pattern matching. It
	// reports paths only, never file contents.
	KindFilesystemGlob = "evidence.filesystem.glob"
	// KindFilesystemGrep covers bounded file-content pattern search.
	KindFilesystemGrep = "evidence.filesystem.grep"
)

// Hard ceilings no configured Limits value may exceed, regardless of
// consumer configuration. These bound worst-case tool-result size and walk
// cost independently of hustle.ToolLoopLimits (which the runtime enforces
// on the OTHER side of this boundary): a tool that only ever truncates at
// the source can never rely on a caller-side limit to save it.
const (
	hardMaxReadBytes     = 1 << 20 // 1 MiB
	hardMaxListEntries   = 10_000
	hardMaxGlobMatches   = 2_000
	hardMaxGrepMatches   = 1_000
	hardMaxGrepFileBytes = 2 << 20 // 2 MiB

	// maxPathArgBytes bounds any single model-supplied path argument before
	// it is even lexically inspected.
	maxPathArgBytes = 4096
	// maxPatternArgBytes bounds a glob/grep pattern argument.
	maxPatternArgBytes = 1024
	// maxArgsJSONBytes bounds the raw argument JSON this package will
	// attempt to decode at all.
	maxArgsJSONBytes = 16 << 10
)

// hardMaxGlobScanned and hardMaxGrepScanned bound the number of filesystem
// nodes walkRoot will visit for one glob/grep call. They are `var`, not
// `const`, solely so this package's own tests can lower them for the
// duration of one test to directly exercise the scan-budget-exhausted path
// without constructing a tree of hundreds of thousands of entries; no
// production code ever assigns to them.
var (
	hardMaxGlobScanned = 200_000
	hardMaxGrepScanned = 200_000
)

var (
	errMalformedArguments   = errors.New("evidence: malformed arguments")
	errPathOutsideRoot      = errors.New("evidence: path resolves outside the review workspace")
	errWorkspaceUnavailable = errors.New("evidence: review workspace is unavailable")
)

// ---- shared argument decoding -------------------------------------------------

// decodeArgs strictly decodes argsJSON into v: exactly the keys in
// requiredKeys (no fewer — a JSON `null` never masquerades as an omitted,
// zero-valued field, matching this module's internal/wire discipline — and
// no more — an unknown field is rejected, not silently ignored), no
// duplicate top-level key, no trailing content, and a hard byte ceiling
// before any parsing is attempted. v must be a pointer to a flat struct (no
// nested objects) — every evidence tool's argument shape is intentionally
// flat. requiredKeys may be empty (a zero-argument tool), in which case
// argsJSON must decode to exactly `{}`.
func decodeArgs(argsJSON string, requiredKeys []string, v any) error {
	if len(argsJSON) == 0 || len(argsJSON) > maxArgsJSONBytes {
		return errMalformedArguments
	}
	if !utf8.ValidString(argsJSON) {
		return errMalformedArguments
	}
	object, err := decodeTopLevelObject([]byte(argsJSON))
	if err != nil {
		return errMalformedArguments
	}
	if len(object) != len(requiredKeys) {
		return errMalformedArguments
	}
	for _, key := range requiredKeys {
		raw, present := object[key]
		if !present || isExplicitJSONNull(raw) {
			return errMalformedArguments
		}
	}
	decoder := json.NewDecoder(strings.NewReader(argsJSON))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(v); err != nil {
		return errMalformedArguments
	}
	if err := decoder.Decode(new(struct{})); err != io.EOF {
		return errMalformedArguments
	}
	return nil
}

func isExplicitJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// decodeTopLevelObject requires data to be exactly one flat JSON object with
// no repeated key at its top level, returning its members. encoding/json's
// ordinary map decode silently accepts a duplicate key (last write wins),
// which would let a prompt-influenced caller smuggle a second value past a
// decoder that only inspects the final collapsed map/struct value.
func decodeTopLevelObject(data []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := token.(json.Delim)
	if !ok || delim != '{' {
		return nil, errMalformedArguments
	}
	object := make(map[string]json.RawMessage)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, errMalformedArguments
		}
		if _, duplicate := object[key]; duplicate {
			return nil, errMalformedArguments
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, err
		}
		object[key] = raw
	}
	if _, err := decoder.Token(); err != nil { // closing '}'
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errMalformedArguments
	}
	return object, nil
}

// ---- path validation and containment ------------------------------------------

// resolveRootRelative performs LEXICAL containment validation of a
// model-supplied path argument: it never touches the filesystem, and
// therefore cannot see an intermediate symlink. It exists to reject
// obviously-malicious input cheaply (absolute paths, `..` escapes, control
// bytes) before any syscall. Every actual filesystem access must ADDITIONALLY
// go through an *os.Root rooted at the canonical read root, which is the
// authoritative, symlink-aware, TOCTOU-resistant boundary.
//
// An empty string and "." both mean "the workspace root itself". A leading
// "/" (or any absolute path) is always rejected — evidence tools accept only
// workspace-relative paths, never host-absolute ones, so there is never a
// question of whether an absolute path "happens to" land inside the root.
func resolveRootRelative(raw string) (string, error) {
	if !utf8.ValidString(raw) || strings.ContainsRune(raw, '\x00') || len(raw) > maxPathArgBytes {
		return "", errMalformedArguments
	}
	if raw == "" || raw == "." {
		return ".", nil
	}
	if filepath.IsAbs(raw) {
		return "", errPathOutsideRoot
	}
	cleaned := filepath.Clean(raw)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) || filepath.IsAbs(cleaned) {
		return "", errPathOutsideRoot
	}
	return cleaned, nil
}

// precheckContainment performs a REAL, filesystem-aware containment check
// against root via *os.Root: this is what actually detects a symlink whose
// target escapes root, which a lexical check cannot see. followFinal
// selects Stat (follows the final path component, for tools that will
// dereference it) vs Lstat (does not, for the stat tool's own semantics). A
// missing target is not a containment failure — it is reported to the
// caller as a normal "not found" outcome, never a security rejection.
func precheckContainment(root, relPath string, followFinal bool) error {
	r, err := os.OpenRoot(root)
	if err != nil {
		return errWorkspaceUnavailable
	}
	defer r.Close()

	var statErr error
	if followFinal {
		_, statErr = r.Stat(relPath)
	} else {
		_, statErr = r.Lstat(relPath)
	}
	if statErr == nil || errors.Is(statErr, fs.ErrNotExist) {
		return nil
	}
	return errPathOutsideRoot
}

// clampLimit normalizes a caller-requested bound: <= 0 means "use def"; any
// value above hardMax is silently clamped to hardMax. It is used both for
// the consumer-configured Limits (catalog.go) and for a model-supplied
// per-call "limit" argument.
func clampLimit(requested, def, hardMax int) int {
	if def <= 0 || def > hardMax {
		def = hardMax
	}
	if requested <= 0 {
		return def
	}
	if requested > hardMax {
		return hardMax
	}
	return requested
}

func truncateForDisplay(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// ---- tool.ToolInfo schema helpers ---------------------------------------------

// Every evidence-tool schema below is a flat, strict object: "required"
// lists every declared property (inference.ValidateOutputSchema's strict
// mode rejects a schema whose "required" set differs from its property
// set), and "additionalProperties" is always false. There is no optional
// argument in this pack — a sentinel zero value (empty string, 0, false)
// stands in for "caller did not specify" wherever that distinction matters.

const pathStatSchema = `{
  "type": "object",
  "description": "Path argument, relative to the review workspace root ('' or '.' means the root itself).",
  "properties": {"path": {"type": "string"}},
  "required": ["path"],
  "additionalProperties": false
}`

const listDirectorySchema = `{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Directory, relative to the review workspace root ('' or '.' means the root itself)."},
    "limit": {"type": "integer", "description": "Maximum entries to return. 0 means use the tool's default."}
  },
  "required": ["path", "limit"],
  "additionalProperties": false
}`

const readFileSchema = `{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "File, relative to the review workspace root."},
    "offset": {"type": "integer", "description": "Zero-based byte offset to start reading from."},
    "limit": {"type": "integer", "description": "Maximum bytes to read. 0 means use the tool's default."}
  },
  "required": ["path", "offset", "limit"],
  "additionalProperties": false
}`

const globFilesSchema = `{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Directory to search under, relative to the review workspace root ('' or '.' means the root itself)."},
    "pattern": {"type": "string", "description": "Shell-style glob (*, ?, [...]). A pattern with no '/' also matches at any depth by basename."},
    "limit": {"type": "integer", "description": "Maximum matches to return. 0 means use the tool's default."}
  },
  "required": ["path", "pattern", "limit"],
  "additionalProperties": false
}`

const grepFilesSchema = `{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Directory to search under, relative to the review workspace root ('' or '.' means the root itself)."},
    "pattern": {"type": "string", "description": "Literal substring, or a regular expression when regex is true."},
    "regex": {"type": "boolean", "description": "Interpret pattern as an RE2 regular expression."},
    "case_sensitive": {"type": "boolean", "description": "Match case-sensitively."},
    "limit": {"type": "integer", "description": "Maximum matching lines to return. 0 means use the tool's default."}
  },
  "required": ["path", "pattern", "regex", "case_sensitive", "limit"],
  "additionalProperties": false
}`

// ---- evidence_filesystem_stat ---------------------------------------------

const toolNameFilesystemStat = "evidence_filesystem_stat"

type pathStatTool struct {
	root string

	// observations carries the target-sensitive observation InvokableRun
	// captured for the most recent call against each rel it stat'd, forward
	// to the matching later ObservedRequirement call — see observation.go's
	// observationCapture doc comment and this tool's own ObservedRequirement
	// doc comment for why.
	observations observationCapture
}

func newPathStatTool(root string) *pathStatTool { return &pathStatTool{root: root} }

func (t *pathStatTool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{
		Name:   toolNameFilesystemStat,
		Desc:   filesystemStatDesc,
		Schema: json.RawMessage(pathStatSchema),
	}, nil
}

type pathArgsV1 struct {
	Path string `json:"path"`
}

var pathArgsKeys = []string{"path"}

func (t *pathStatTool) PrepareCall(_ context.Context, executionID uuid.UUID, argsJSON string) (tool.Request, tool.PreparedArtifact, error) {
	var args pathArgsV1
	if err := decodeArgs(argsJSON, pathArgsKeys, &args); err != nil {
		return tool.Request{}, nil, err
	}
	rel, err := resolveRootRelative(args.Path)
	if err != nil {
		return tool.Request{}, nil, err
	}
	if err := precheckContainment(t.root, rel, false); err != nil {
		return tool.Request{}, nil, err
	}
	return tool.Request{
		ToolName:    toolNameFilesystemStat,
		ExecutionID: executionID.String(),
		Requirements: []tool.Requirement{{
			Kind:        KindFilesystemStat,
			Scope:       t.root,
			Match:       rel,
			Description: truncateForDisplay(fmt.Sprintf("stat %s", rel), 512),
		}},
	}, nil, nil
}

func (t *pathStatTool) InvokableRun(_ context.Context, argsJSON string) (*tool.ToolResult, error) {
	var args pathArgsV1
	if err := decodeArgs(argsJSON, pathArgsKeys, &args); err != nil {
		return tool.TextResult("error: malformed arguments"), nil
	}
	rel, err := resolveRootRelative(args.Path)
	if err != nil {
		return tool.TextResult("error: path resolves outside the review workspace"), nil
	}
	r, err := os.OpenRoot(t.root)
	if err != nil {
		return tool.TextResult("error: review workspace is unavailable"), nil
	}
	defer r.Close()

	var b strings.Builder
	fmt.Fprintf(&b, "path: %s\n", rel)

	info, err := r.Lstat(rel)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		b.WriteString("exists: false\n")
		// Absence is itself a real, TOCTOU-relevant observation (design
		// §13.4's own worked example) — record it, captured from this exact
		// Lstat call, not a later independent one. See
		// observation.go's observationCapture and pathStatTool's own
		// ObservedRequirement doc comment.
		t.observations.record(rel, canonicalObservationTarget(t.root, rel), observationDigest(fingerprintAbsentFields()...))
		return tool.TextResult(b.String()), nil
	case err != nil:
		// The target's state genuinely could not be established: no
		// observation is recorded, matching this tool's pre-existing
		// "ok=false on error" contract.
		b.WriteString("exists: unknown\nerror: path could not be resolved within the review workspace\n")
		return tool.TextResult(b.String()), nil
	}

	b.WriteString("exists: true\n")
	writeFileInfo(&b, "lstat_", info)

	if info.Mode()&fs.ModeSymlink == 0 {
		t.observations.record(rel, canonicalObservationTarget(t.root, rel), observationDigest(fingerprintFields(info, "", nil, nil, nil)...))
		return tool.TextResult(b.String()), nil
	}

	target, linkErr := r.Readlink(rel)
	if linkErr == nil {
		fmt.Fprintf(&b, "symlink_target: %s\n", truncateForDisplay(target, 1024))
	}
	resolved, statErr := r.Stat(rel)
	switch {
	case statErr == nil:
		b.WriteString("target_within_root: true\n")
		writeFileInfo(&b, "target_", resolved)
	case errors.Is(statErr, fs.ErrNotExist):
		b.WriteString("target_within_root: true\ntarget_exists: false\n")
	default:
		b.WriteString("target_within_root: false\n")
	}
	// The observation's fingerprint is built from the SAME info/target/
	// resolved/statErr values already computed above to construct the
	// model-visible text — zero additional filesystem calls, and the token
	// is guaranteed to attest to the exact state that text describes.
	t.observations.record(rel, canonicalObservationTarget(t.root, rel), observationDigest(fingerprintFields(info, target, linkErr, resolved, statErr)...))
	return tool.TextResult(b.String()), nil
}

func writeFileInfo(b *strings.Builder, prefix string, info fs.FileInfo) {
	fmt.Fprintf(b, "%stype: %s\n", prefix, entryKind(info))
	fmt.Fprintf(b, "%ssize: %d\n", prefix, info.Size())
	fmt.Fprintf(b, "%smode: %s\n", prefix, info.Mode().String())
	fmt.Fprintf(b, "%smod_time: %s\n", prefix, info.ModTime().UTC().Format("2006-01-02T15:04:05Z"))
}

func entryKind(info fs.FileInfo) string {
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		return "symlink"
	case info.IsDir():
		return "dir"
	case info.Mode().IsRegular():
		return "file"
	default:
		return "other"
	}
}

var _ tool.InvokableTool = (*pathStatTool)(nil)
var _ tool.CallPreparer = (*pathStatTool)(nil)

// ---- evidence_filesystem_list ----------------------------------------------

const toolNameFilesystemList = "evidence_filesystem_list"

type listDirectoryTool struct {
	root       string
	defEntries int
}

func newListDirectoryTool(root string, defEntries int) *listDirectoryTool {
	return &listDirectoryTool{root: root, defEntries: defEntries}
}

func (t *listDirectoryTool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{
		Name:   toolNameFilesystemList,
		Desc:   filesystemListDesc,
		Schema: json.RawMessage(listDirectorySchema),
	}, nil
}

type listArgsV1 struct {
	Path  string `json:"path"`
	Limit int    `json:"limit"`
}

var listArgsKeys = []string{"path", "limit"}

func (t *listDirectoryTool) PrepareCall(_ context.Context, executionID uuid.UUID, argsJSON string) (tool.Request, tool.PreparedArtifact, error) {
	var args listArgsV1
	if err := decodeArgs(argsJSON, listArgsKeys, &args); err != nil {
		return tool.Request{}, nil, err
	}
	rel, err := resolveRootRelative(args.Path)
	if err != nil {
		return tool.Request{}, nil, err
	}
	if err := precheckContainment(t.root, rel, true); err != nil {
		return tool.Request{}, nil, err
	}
	return tool.Request{
		ToolName:    toolNameFilesystemList,
		ExecutionID: executionID.String(),
		Requirements: []tool.Requirement{{
			Kind:        KindFilesystemList,
			Scope:       t.root,
			Match:       rel,
			Description: truncateForDisplay(fmt.Sprintf("list %s", rel), 512),
		}},
	}, nil, nil
}

func (t *listDirectoryTool) InvokableRun(_ context.Context, argsJSON string) (*tool.ToolResult, error) {
	var args listArgsV1
	if err := decodeArgs(argsJSON, listArgsKeys, &args); err != nil {
		return tool.TextResult("error: malformed arguments"), nil
	}
	rel, err := resolveRootRelative(args.Path)
	if err != nil {
		return tool.TextResult("error: path resolves outside the review workspace"), nil
	}
	limit := clampLimit(args.Limit, t.defEntries, hardMaxListEntries)

	r, err := os.OpenRoot(t.root)
	if err != nil {
		return tool.TextResult("error: review workspace is unavailable"), nil
	}
	defer r.Close()

	f, err := r.Open(rel)
	if errors.Is(err, fs.ErrNotExist) {
		return tool.TextResult(fmt.Sprintf("path: %s\nexists: false\n", rel)), nil
	}
	if err != nil {
		return tool.TextResult(fmt.Sprintf("path: %s\nerror: %s\n", rel, classifyOSError(err))), nil
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return tool.TextResult(fmt.Sprintf("path: %s\nerror: unable to stat\n", rel)), nil
	}
	if !info.IsDir() {
		return tool.TextResult(fmt.Sprintf("path: %s\nerror: not a directory\n", rel)), nil
	}

	entries, err := f.ReadDir(-1)
	if err != nil {
		return tool.TextResult(fmt.Sprintf("path: %s\nerror: %s\n", rel, classifyOSError(err))), nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	var b strings.Builder
	fmt.Fprintf(&b, "path: %s\n", rel)
	truncated := len(entries) > limit
	if truncated {
		entries = entries[:limit]
	}
	fmt.Fprintf(&b, "entries: %d\n", len(entries))
	fmt.Fprintf(&b, "truncated: %t\n", truncated)
	for _, entry := range entries {
		entryInfo, infoErr := entry.Info()
		if infoErr != nil {
			fmt.Fprintf(&b, "- %s ?\n", entry.Name())
			continue
		}
		fmt.Fprintf(&b, "- %s %s %d %s\n", entry.Name(), entryKind(entryInfo), entryInfo.Size(), entryInfo.Mode().String())
	}
	return tool.TextResult(b.String()), nil
}

func classifyOSError(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "not found"
	case errors.Is(err, fs.ErrPermission):
		return "permission denied"
	default:
		return "unavailable"
	}
}

var _ tool.InvokableTool = (*listDirectoryTool)(nil)
var _ tool.CallPreparer = (*listDirectoryTool)(nil)

// ---- evidence_filesystem_read ----------------------------------------------

const toolNameFilesystemRead = "evidence_filesystem_read"

type readFileTool struct {
	root    string
	defRead int

	// observations carries the target-sensitive observation InvokableRun
	// captured for the most recent call against each rel it read, forward to
	// the matching later ObservedRequirement call — see observation.go's
	// observationCapture doc comment and this tool's own ObservedRequirement
	// doc comment for why.
	observations observationCapture
}

func newReadFileTool(root string, defRead int) *readFileTool {
	return &readFileTool{root: root, defRead: defRead}
}

func (t *readFileTool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{
		Name:   toolNameFilesystemRead,
		Desc:   filesystemReadDesc,
		Schema: json.RawMessage(readFileSchema),
	}, nil
}

type readArgsV1 struct {
	Path   string `json:"path"`
	Offset int64  `json:"offset"`
	Limit  int    `json:"limit"`
}

var readArgsKeys = []string{"path", "offset", "limit"}

func (t *readFileTool) PrepareCall(_ context.Context, executionID uuid.UUID, argsJSON string) (tool.Request, tool.PreparedArtifact, error) {
	var args readArgsV1
	if err := decodeArgs(argsJSON, readArgsKeys, &args); err != nil {
		return tool.Request{}, nil, err
	}
	if args.Offset < 0 || args.Limit < 0 {
		return tool.Request{}, nil, errMalformedArguments
	}
	rel, err := resolveRootRelative(args.Path)
	if err != nil {
		return tool.Request{}, nil, err
	}
	if rel == "." {
		// A read always names a file; the workspace root itself is not one.
		return tool.Request{}, nil, errMalformedArguments
	}
	if err := precheckContainment(t.root, rel, true); err != nil {
		return tool.Request{}, nil, err
	}
	return tool.Request{
		ToolName:    toolNameFilesystemRead,
		ExecutionID: executionID.String(),
		Requirements: []tool.Requirement{{
			Kind:        KindFilesystemRead,
			Scope:       t.root,
			Match:       rel,
			Description: truncateForDisplay(fmt.Sprintf("read %s", rel), 512),
		}},
	}, nil, nil
}

func (t *readFileTool) InvokableRun(_ context.Context, argsJSON string) (*tool.ToolResult, error) {
	var args readArgsV1
	if err := decodeArgs(argsJSON, readArgsKeys, &args); err != nil || args.Offset < 0 || args.Limit < 0 {
		return tool.TextResult("error: malformed arguments"), nil
	}
	rel, err := resolveRootRelative(args.Path)
	if err != nil || rel == "." {
		return tool.TextResult("error: path resolves outside the review workspace"), nil
	}
	// captureObservation runs synchronously, immediately before this
	// function returns its result — as late as possible while still being
	// inside InvokableRun's own synchronous execution — rather than being
	// deferred to a later, separate ObservedRequirement call after the
	// result has already left this function. See observation.go's
	// observationCapture doc comment and this tool's own ObservedRequirement
	// doc comment for why that timing is the actual fix.
	defer t.captureObservation(rel)
	limit := clampLimit(args.Limit, t.defRead, hardMaxReadBytes)

	r, err := os.OpenRoot(t.root)
	if err != nil {
		return tool.TextResult("error: review workspace is unavailable"), nil
	}
	defer r.Close()

	f, err := r.Open(rel)
	if err != nil {
		return tool.TextResult(fmt.Sprintf("path: %s\nerror: %s\n", rel, classifyOSError(err))), nil
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return tool.TextResult(fmt.Sprintf("path: %s\nerror: unable to stat\n", rel)), nil
	}
	if info.IsDir() {
		return tool.TextResult(fmt.Sprintf("path: %s\nerror: is a directory\n", rel)), nil
	}
	// os.Root's Open already followed a within-root symlink to reach a
	// regular file here; a non-regular final target (device, socket) is
	// refused explicitly rather than read.
	if !info.Mode().IsRegular() {
		return tool.TextResult(fmt.Sprintf("path: %s\nerror: not a regular file\n", rel)), nil
	}

	if args.Offset > 0 {
		if _, err := f.Seek(args.Offset, io.SeekStart); err != nil {
			return tool.TextResult(fmt.Sprintf("path: %s\nerror: offset beyond file\n", rel)), nil
		}
	}

	buf := make([]byte, limit+1)
	n, readErr := io.ReadFull(f, buf)
	if readErr != nil && !errors.Is(readErr, io.ErrUnexpectedEOF) && !errors.Is(readErr, io.EOF) {
		return tool.TextResult(fmt.Sprintf("path: %s\nerror: %s\n", rel, classifyOSError(readErr))), nil
	}
	truncated := n > limit
	if truncated {
		n = limit
	}
	content := buf[:n]

	var b strings.Builder
	fmt.Fprintf(&b, "path: %s\n", rel)
	fmt.Fprintf(&b, "offset: %d\n", args.Offset)
	fmt.Fprintf(&b, "bytes_read: %d\n", n)
	fmt.Fprintf(&b, "truncated: %t\n", truncated)
	b.WriteString("---\n")
	b.Write(content)
	return tool.TextResult(b.String()), nil
}

// captureObservation records rel's target-sensitive observation for the
// matching, later ObservedRequirement call to pick up (see
// observation.go's observationCapture doc comment and readFileTool's own
// ObservedRequirement doc comment for the full reasoning). Called via defer
// from InvokableRun, so it always runs — regardless of which return branch
// fired — synchronously, immediately before InvokableRun's result leaves
// this function. A fingerprint error (e.g. the workspace root itself
// becoming unavailable) simply records nothing, matching this file's
// existing "ok=false, dropped, not fatal" contract for a missing
// observation.
func (t *readFileTool) captureObservation(rel string) {
	fingerprint, err := filesystemObservationFingerprint(t.root, rel)
	if err != nil {
		return
	}
	t.observations.record(rel, canonicalObservationTarget(t.root, rel), fingerprint)
}

var _ tool.InvokableTool = (*readFileTool)(nil)
var _ tool.CallPreparer = (*readFileTool)(nil)

// ---- evidence_filesystem_glob ----------------------------------------------

const toolNameFilesystemGlob = "evidence_filesystem_glob"

type globFilesTool struct {
	root       string
	defMatches int
}

func newGlobFilesTool(root string, defMatches int) *globFilesTool {
	return &globFilesTool{root: root, defMatches: defMatches}
}

func (t *globFilesTool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{
		Name:   toolNameFilesystemGlob,
		Desc:   filesystemGlobDesc,
		Schema: json.RawMessage(globFilesSchema),
	}, nil
}

type globArgsV1 struct {
	Path    string `json:"path"`
	Pattern string `json:"pattern"`
	Limit   int    `json:"limit"`
}

var globArgsKeys = []string{"path", "pattern", "limit"}

func (t *globFilesTool) PrepareCall(_ context.Context, executionID uuid.UUID, argsJSON string) (tool.Request, tool.PreparedArtifact, error) {
	var args globArgsV1
	if err := decodeArgs(argsJSON, globArgsKeys, &args); err != nil {
		return tool.Request{}, nil, err
	}
	if err := validPattern(args.Pattern); err != nil {
		return tool.Request{}, nil, err
	}
	rel, err := resolveRootRelative(args.Path)
	if err != nil {
		return tool.Request{}, nil, err
	}
	if err := precheckContainment(t.root, rel, true); err != nil {
		return tool.Request{}, nil, err
	}
	return tool.Request{
		ToolName:    toolNameFilesystemGlob,
		ExecutionID: executionID.String(),
		Requirements: []tool.Requirement{{
			Kind:        KindFilesystemGlob,
			Scope:       t.root,
			Match:       rel,
			Description: truncateForDisplay(fmt.Sprintf("glob %s under %s", args.Pattern, rel), 512),
		}},
	}, nil, nil
}

func validPattern(pattern string) error {
	if pattern == "" || !utf8.ValidString(pattern) || strings.ContainsRune(pattern, '\x00') || len(pattern) > maxPatternArgBytes {
		return errMalformedArguments
	}
	if _, err := pathpkg.Match(pattern, "probe"); err != nil {
		return errMalformedArguments
	}
	return nil
}

func (t *globFilesTool) InvokableRun(ctx context.Context, argsJSON string) (*tool.ToolResult, error) {
	var args globArgsV1
	if err := decodeArgs(argsJSON, globArgsKeys, &args); err != nil || validPattern(args.Pattern) != nil {
		return tool.TextResult("error: malformed arguments"), nil
	}
	rel, err := resolveRootRelative(args.Path)
	if err != nil {
		return tool.TextResult("error: path resolves outside the review workspace"), nil
	}
	limit := clampLimit(args.Limit, t.defMatches, hardMaxGlobMatches)

	r, err := os.OpenRoot(t.root)
	if err != nil {
		return tool.TextResult("error: review workspace is unavailable"), nil
	}
	defer r.Close()

	matches := make([]string, 0, limit)
	scanned := 0
	matchCapped := false
	basenameOnly := !strings.ContainsRune(args.Pattern, '/')

	budgetExhausted, walkErr := walkRoot(ctx, r, rel, hardMaxGlobScanned, func(relEntry string, d fs.DirEntry) (bool, error) {
		scanned++
		if d.IsDir() {
			return true, nil
		}
		name := relEntry
		matched, _ := pathpkg.Match(args.Pattern, name)
		if !matched && basenameOnly {
			matched, _ = pathpkg.Match(args.Pattern, pathpkg.Base(name))
		}
		if matched {
			if len(matches) >= limit {
				matchCapped = true
				return false, nil
			}
			matches = append(matches, name)
		}
		return true, nil
	})

	var b strings.Builder
	fmt.Fprintf(&b, "path: %s\n", rel)
	fmt.Fprintf(&b, "pattern: %s\n", args.Pattern)
	if walkErr != nil {
		if errors.Is(walkErr, context.Canceled) || errors.Is(walkErr, context.DeadlineExceeded) {
			return nil, walkErr
		}
		b.WriteString("error: unable to search this path\n")
		return tool.TextResult(b.String()), nil
	}
	// truncated is true whenever the reported match set is known to be
	// incomplete, for either reason: the match cap was reached (more
	// matches existed beyond limit) or the scan's own node-visit budget was
	// exhausted before the walk could finish (part of the tree was never
	// even examined). scan_budget_exhausted disambiguates the two: a
	// consumer that specifically needs to know whether the whole tree was
	// looked at must not have to infer that from truncated alone.
	truncated := matchCapped || budgetExhausted
	fmt.Fprintf(&b, "matches: %d\n", len(matches))
	fmt.Fprintf(&b, "truncated: %t\n", truncated)
	fmt.Fprintf(&b, "scan_budget_exhausted: %t\n", budgetExhausted)
	fmt.Fprintf(&b, "scanned: %d\n", scanned)
	for _, m := range matches {
		fmt.Fprintf(&b, "- %s\n", m)
	}
	return tool.TextResult(b.String()), nil
}

var _ tool.InvokableTool = (*globFilesTool)(nil)
var _ tool.CallPreparer = (*globFilesTool)(nil)

// ---- evidence_filesystem_grep ----------------------------------------------

const toolNameFilesystemGrep = "evidence_filesystem_grep"

type grepFilesTool struct {
	root         string
	defMatches   int
	maxFileBytes int
}

func newGrepFilesTool(root string, defMatches, maxFileBytes int) *grepFilesTool {
	return &grepFilesTool{root: root, defMatches: defMatches, maxFileBytes: maxFileBytes}
}

func (t *grepFilesTool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{
		Name:   toolNameFilesystemGrep,
		Desc:   filesystemGrepDesc,
		Schema: json.RawMessage(grepFilesSchema),
	}, nil
}

type grepArgsV1 struct {
	Path          string `json:"path"`
	Pattern       string `json:"pattern"`
	Regex         bool   `json:"regex"`
	CaseSensitive bool   `json:"case_sensitive"`
	Limit         int    `json:"limit"`
}

var grepArgsKeys = []string{"path", "pattern", "regex", "case_sensitive", "limit"}

func (t *grepFilesTool) PrepareCall(_ context.Context, executionID uuid.UUID, argsJSON string) (tool.Request, tool.PreparedArtifact, error) {
	var args grepArgsV1
	if err := decodeArgs(argsJSON, grepArgsKeys, &args); err != nil {
		return tool.Request{}, nil, err
	}
	if _, err := compileGrepMatcher(args); err != nil {
		return tool.Request{}, nil, err
	}
	rel, err := resolveRootRelative(args.Path)
	if err != nil {
		return tool.Request{}, nil, err
	}
	if err := precheckContainment(t.root, rel, true); err != nil {
		return tool.Request{}, nil, err
	}
	return tool.Request{
		ToolName:    toolNameFilesystemGrep,
		ExecutionID: executionID.String(),
		Requirements: []tool.Requirement{{
			Kind:        KindFilesystemGrep,
			Scope:       t.root,
			Match:       rel,
			Description: truncateForDisplay(fmt.Sprintf("grep %s under %s", args.Pattern, rel), 512),
		}},
	}, nil, nil
}

func (t *grepFilesTool) InvokableRun(ctx context.Context, argsJSON string) (*tool.ToolResult, error) {
	var args grepArgsV1
	if err := decodeArgs(argsJSON, grepArgsKeys, &args); err != nil {
		return tool.TextResult("error: malformed arguments"), nil
	}
	matcher, err := compileGrepMatcher(args)
	if err != nil {
		return tool.TextResult("error: malformed arguments"), nil
	}
	rel, err := resolveRootRelative(args.Path)
	if err != nil {
		return tool.TextResult("error: path resolves outside the review workspace"), nil
	}
	limit := clampLimit(args.Limit, t.defMatches, hardMaxGrepMatches)
	maxFileBytes := clampLimit(0, t.maxFileBytes, hardMaxGrepFileBytes)

	r, err := os.OpenRoot(t.root)
	if err != nil {
		return tool.TextResult("error: review workspace is unavailable"), nil
	}
	defer r.Close()

	type matchLine struct {
		path string
		line int
		text string
	}
	var matches []matchLine
	filesScanned, bytesScanned := 0, 0
	matchCapped := false

	budgetExhausted, walkErr := walkRoot(ctx, r, rel, hardMaxGrepScanned, func(relEntry string, d fs.DirEntry) (bool, error) {
		if d.IsDir() {
			return true, nil
		}
		data, readErr := readBoundedFile(r, relEntry, maxFileBytes)
		if readErr != nil {
			return true, nil // unreadable file: skip silently, not a hard failure
		}
		filesScanned++
		bytesScanned += len(data)
		if looksBinary(data) {
			return true, nil
		}
		lineNum := 0
		for _, line := range bytes.Split(data, []byte("\n")) {
			lineNum++
			if !matcher(line) {
				continue
			}
			// Only NOW, upon finding evidence of one match beyond the cap,
			// do we know the match set is capped — matching exactly limit
			// matches (with nothing further) must report matchCapped: false.
			if len(matches) >= limit {
				matchCapped = true
				return false, nil
			}
			matches = append(matches, matchLine{
				path: relEntry, line: lineNum,
				text: truncateForDisplay(string(line), 512),
			})
		}
		return true, nil
	})

	var b strings.Builder
	fmt.Fprintf(&b, "path: %s\n", rel)
	fmt.Fprintf(&b, "pattern: %s\n", args.Pattern)
	fmt.Fprintf(&b, "regex: %t\n", args.Regex)
	fmt.Fprintf(&b, "case_sensitive: %t\n", args.CaseSensitive)
	if walkErr != nil {
		if errors.Is(walkErr, context.Canceled) || errors.Is(walkErr, context.DeadlineExceeded) {
			return nil, walkErr
		}
		b.WriteString("error: unable to search this path\n")
		return tool.TextResult(b.String()), nil
	}
	// truncated is true whenever the reported match set is known to be
	// incomplete, for either reason: the match cap was reached (more
	// matches existed beyond limit) or the scan's own node-visit budget was
	// exhausted before the walk could finish (part of the tree was never
	// even examined). scan_budget_exhausted disambiguates the two: a
	// consumer that specifically needs to know whether the whole tree was
	// looked at must not have to infer that from truncated alone.
	truncated := matchCapped || budgetExhausted
	fmt.Fprintf(&b, "matches: %d\n", len(matches))
	fmt.Fprintf(&b, "truncated: %t\n", truncated)
	fmt.Fprintf(&b, "scan_budget_exhausted: %t\n", budgetExhausted)
	fmt.Fprintf(&b, "files_scanned: %d\n", filesScanned)
	fmt.Fprintf(&b, "bytes_scanned: %d\n", bytesScanned)
	for _, m := range matches {
		fmt.Fprintf(&b, "- %s:%d: %s\n", m.path, m.line, m.text)
	}
	return tool.TextResult(b.String()), nil
}

func compileGrepMatcher(args grepArgsV1) (func(line []byte) bool, error) {
	if args.Pattern == "" || !utf8.ValidString(args.Pattern) || strings.ContainsRune(args.Pattern, '\x00') || len(args.Pattern) > maxPatternArgBytes {
		return nil, errMalformedArguments
	}
	if args.Regex {
		expr := args.Pattern
		if !args.CaseSensitive {
			expr = "(?i)" + expr
		}
		re, err := compileBoundedRegexp(expr)
		if err != nil {
			return nil, errMalformedArguments
		}
		return func(line []byte) bool { return re.Match(line) }, nil
	}
	needle := args.Pattern
	if !args.CaseSensitive {
		needle = strings.ToLower(needle)
	}
	return func(line []byte) bool {
		hay := string(line)
		if !args.CaseSensitive {
			hay = strings.ToLower(hay)
		}
		return strings.Contains(hay, needle)
	}, nil
}

// compileBoundedRegexp compiles a model-supplied regular expression. Go's
// regexp package is RE2-based (linear-time matching, no catastrophic
// backtracking), so there is no ReDoS class of attack here; the byte-length
// cap already applied by the caller (maxPatternArgBytes) is the only bound
// needed against a pathologically large program.
func compileBoundedRegexp(expr string) (*regexp.Regexp, error) {
	if len(expr) > maxPatternArgBytes {
		return nil, errMalformedArguments
	}
	return regexp.Compile(expr)
}

func looksBinary(data []byte) bool {
	probe := data
	if len(probe) > 8000 {
		probe = probe[:8000]
	}
	return bytes.ContainsRune(probe, 0)
}

// readBoundedFile reads at most maxBytes from relPath through r, refusing
// non-regular files (including a symlink whose target escapes root, which
// r.Open already refuses at the syscall level).
func readBoundedFile(r *os.Root, relPath string, maxBytes int) ([]byte, error) {
	f, err := r.Open(relPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("evidence: not a regular file")
	}
	limited := io.LimitReader(f, int64(maxBytes))
	return io.ReadAll(limited)
}

var _ tool.InvokableTool = (*grepFilesTool)(nil)
var _ tool.CallPreparer = (*grepFilesTool)(nil)

// ---- shared bounded walk ----------------------------------------------------

// walkRoot performs a bounded, iterative, non-recursive-into-symlinks walk
// of the subtree rooted at start (a root-relative path already validated by
// resolveRootRelative/precheckContainment). It visits regular files and
// directories reached via ordinary (non-symlink) entries; a symlink is
// followed at most once, through r.Stat (confined — refuses an escaping
// target), and only when it resolves to a regular file, which is then
// visited as a leaf. A symlink that is broken, escapes root, or resolves to
// a directory is skipped entirely: directory symlinks are never traversed,
// which deliberately forecloses any symlink-cycle walk risk. fn returning
// (false, nil) stops the walk early without error (e.g. once a result cap
// is reached); ctx cancellation aborts promptly between entries.
//
// The returned budgetExhausted reports whether maxNodes was reached before
// the walk would otherwise have finished naturally — a caller's only way to
// distinguish "the tree was fully examined and nothing more was found" from
// "the scan gave up partway through, so absence of a result here is not
// proof of absence in the tree." budgetExhausted is always false when err
// is non-nil or when fn itself stops the walk early via (false, nil): those
// are different reasons for stopping and must not be conflated with running
// out of scan budget.
func walkRoot(ctx context.Context, r *os.Root, start string, maxNodes int, fn func(relPath string, d fs.DirEntry) (bool, error)) (budgetExhausted bool, err error) {
	type pending struct{ dir string }
	stack := []pending{{dir: start}}
	visited := 0

	for len(stack) > 0 {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		top := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		f, err := r.Open(top.dir)
		if err != nil {
			continue // unreadable/vanished directory: skip, not fatal
		}
		entries, readErr := f.ReadDir(-1)
		_ = f.Close() // read-only handle; a close failure here changes nothing observable
		if readErr != nil {
			continue
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			visited++
			if visited > maxNodes {
				return true, nil
			}
			childRel := entry.Name()
			if top.dir != "." {
				childRel = top.dir + "/" + entry.Name()
			}

			if entry.Type()&fs.ModeSymlink != 0 {
				resolved, statErr := r.Stat(childRel)
				if statErr != nil || resolved.IsDir() {
					continue // broken, escaping, or a directory: never traversed
				}
				keepGoing, err := fn(childRel, fs.FileInfoToDirEntry(resolved))
				if err != nil {
					return false, err
				}
				if !keepGoing {
					return false, nil
				}
				continue
			}

			if entry.IsDir() {
				keepGoing, err := fn(childRel, entry)
				if err != nil {
					return false, err
				}
				if !keepGoing {
					return false, nil
				}
				stack = append(stack, pending{dir: childRel})
				continue
			}

			keepGoing, err := fn(childRel, entry)
			if err != nil {
				return false, err
			}
			if !keepGoing {
				return false, nil
			}
		}
	}
	return false, nil
}
