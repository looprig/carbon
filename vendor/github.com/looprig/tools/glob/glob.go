// Package glob implements the Glob tool: a workspace-contained,
// denied-path-excluding filename search over WalkDir-discovered entries.
// Preparation emits one direct filesystem.read requirement for the canonical
// walked root using the tree match encoding, so a durable tree rule covers it.
package glob

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/tools/internal/prepared"
	"github.com/looprig/tools/internal/workspace"
)

// glob.go implements the Glob tool: a workspace-contained, denied-path-excluding
// filename search using the shared `**`-aware matchGlob over WalkDir-discovered
// entries (design §4b). It is a CallPreparer (PrepareCall emits ONE direct
// filesystem.read requirement for the canonical walked root, with the tree
// Match encoding) and Auditable (pattern/root only).

// maxGlobResults caps the number of paths Glob returns so a broad pattern cannot
// flood the model context. A larger match set is truncated with a notice.
const maxGlobResults = 500

// globNoiseDirs are directory names Glob prunes at the directory level (it does
// not descend into them) by DEFAULT, so `**/*` over a workspace never floods the
// model with VCS internals or large generated trees that are never useful to a
// coding agent (a real provider context-window overflow motivated this). `.git`
// is the must-have — its hundreds of objects/refs are pure noise; the rest are
// well-known heavy/generated trees (dependencies, build output, editor metadata).
// Pruning is bypassed when the search ROOT is itself one of these dirs (the user
// explicitly targeted it), mirroring grep's grepNoiseDirs behaviour. The set is
// kept intentionally parallel to grep's grepNoiseDirs (same purpose, two callers);
// duplicating this short, stable list keeps each read tool self-contained.
var globNoiseDirs = map[string]bool{
	".git":         true, // VCS internals: objects/refs/logs — never useful to an agent.
	".hg":          true, // Mercurial internals.
	".svn":         true, // Subversion internals.
	"node_modules": true, // JS dependencies — huge, generated.
	"vendor":       true, // vendored deps (Go and others).
	"dist":         true, // build/distribution output.
	"build":        true, // build output.
	"target":       true, // Rust/Java (cargo/maven) build output.
	".next":        true, // Next.js build cache.
	"__pycache__":  true, // Python bytecode cache.
}

// globToolName is the EXACT tool name — it MUST equal "Glob".
const globToolName = "Glob"

const defaultSearchDir = "."

// globSchema is the JSON Schema for Glob's args.
const globSchema = `{
  "type": "object",
  "properties": {
    "pattern": {"type": "string", "description": "Glob pattern matched against workspace-relative paths. '**' matches across directories; '*'/'?'/'[...]' match within a single path segment."},
    "root": {"type": "string", "description": "Workspace-relative directory to search under (optional; defaults to the workspace root)."}
  },
  "required": ["pattern"]
}`

const globDesc = "List workspace files whose path matches a glob pattern. '**' spans directories; other wildcards stay within one segment. Results are confined to the workspace, exclude denied (secret) paths, and are capped."

const globHostReadsDesc = "List files whose path matches a glob pattern. '**' spans directories; other wildcards stay within one segment. An absolute root may resolve outside the workspace, subject to the caller's read authority; results exclude denied (secret) paths and are capped."

// globArgs is the typed decode of Glob's untrusted argsJSON.
type globArgs struct {
	Pattern string `json:"pattern"`
	Root    string `json:"root"`
}

// Glob searches workspace filenames against a glob pattern. It depends only on
// the workspace root and the narrow loop.ReadGuard (least privilege).
type Glob struct {
	root      string
	guard     loop.ReadGuard
	hostReads bool
}

// GlobOption configures a Glob at construction (functional-options pattern,
// matching grep.GrepOption / readfile.ReadFileOption).
type GlobOption func(*Glob)

// WithHostReads lets an absolute search root resolve OUTSIDE the workspace
// instead of being rejected at prepare time -- the Glob counterpart to
// readfile.WithHostReads() / grep.WithHostReads(). It does not itself grant
// anything: an uncontained resolved search root still emits the same
// filesystem.read requirement PrepareCall always has, so the consumer's bound
// access source makes the actual Allow/Deny/Gated decision. A RELATIVE "../"
// traversal is never widened by this option -- only a literal absolute path
// argument can resolve outside the workspace. Matched paths in an uncontained
// (host) walk are displayed relative to the SEARCHED directory itself, not
// the workspace root.
func WithHostReads() GlobOption {
	return func(g *Glob) { g.hostReads = true }
}

// NewGlob constructs a Glob bound to the workspace root and read guard.
func NewGlob(root string, guard loop.ReadGuard, opts ...GlobOption) *Glob {
	g := &Glob{root: root, guard: guard}
	for _, opt := range opts {
		opt(g)
	}
	return g
}

// Info returns Glob's self-description. Name MUST equal "Glob".
func (g *Glob) Info(context.Context) (*tool.ToolInfo, error) {
	desc := globDesc
	if g.hostReads {
		desc = globHostReadsDesc
	}
	return &tool.ToolInfo{
		Name:   globToolName,
		Desc:   desc,
		Schema: json.RawMessage(globSchema),
	}, nil
}

// AuditSummary returns a redacted one-line summary: the pattern (and root if
// present) only — never any matched path contents.
func (g *Glob) AuditSummary(argsJSON string) string {
	var a globArgs
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil || a.Pattern == "" {
		return "Glob (unparsable args)"
	}
	if a.Root != "" {
		return "Glob " + a.Pattern + " in " + a.Root
	}
	return "Glob " + a.Pattern
}

// globArtifact is Glob's typed prepared artifact: the validated pattern and
// the canonicalized walk root, bound to one call by PrepareCall and consumed
// by InvokableRun without reparsing the raw JSON. It deliberately embeds
// tool.TokenArtifact to satisfy the sealed tool.PreparedArtifact marker.
type globArtifact struct {
	tool.TokenArtifact
	pattern   string
	searchRel string // model-supplied search root (display + run-time re-check)
	searchAbs string // canonical resolved walk root (requirement Scope)
	// displayRoot is what matched paths are relativized against. For a
	// workspace-contained search it is the canonical workspace root (matches
	// unchanged). For an UNCONTAINED host search (WithHostReads() only), it is
	// searchAbs itself, so results display relative to the searched host
	// directory (see grep.go's identical displayRoot for the shared rationale).
	displayRoot string
}

// prepareGlob is the SINGLE parse-validate-canonicalize step for a Glob call.
func (g *Glob) prepareGlob(argsJSON string) (*globArtifact, error) {
	var a globArgs
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return nil, &globError{reason: "invalid arguments: not a JSON object", cause: err}
	}
	if a.Pattern == "" {
		return nil, &globError{reason: "a non-empty 'pattern' is required"}
	}
	searchRel := a.Root
	if searchRel == "" {
		searchRel = defaultSearchDir
	}
	searchAbs, displayRoot, err := g.resolveSearch(searchRel)
	if err != nil {
		return nil, err
	}
	return &globArtifact{pattern: a.Pattern, searchRel: searchRel, searchAbs: searchAbs, displayRoot: displayRoot}, nil
}

// resolveSearch is the SINGLE resolution step both prepareGlob and
// InvokableRun's stage-1 recheck use (mirrors grep.Grep.resolveSearch; see
// its doc comment for the full rationale on message stability and the
// uncontained displayRoot choice).
func (g *Glob) resolveSearch(input string) (searchAbs, displayRoot string, err error) {
	if !g.hostReads {
		searchAbs, err = workspace.ContainedPath(g.root, input)
		if err != nil {
			return "", "", &globError{reason: "search root is outside the workspace: " + input, cause: err}
		}
		root, rerr := workspace.ResolveRoot(g.root)
		if rerr != nil {
			return "", "", &globError{reason: "workspace root could not be resolved", cause: rerr}
		}
		return searchAbs, root, nil
	}
	searchAbs, contained, err := workspace.ResolvedPath(g.root, input)
	if err != nil {
		reason := "search root is outside the workspace: " + input
		if filepath.IsAbs(input) {
			reason = "search root could not be resolved: " + input
		}
		return "", "", &globError{reason: reason, cause: err}
	}
	if !contained {
		return searchAbs, searchAbs, nil
	}
	root, rerr := workspace.ResolveRoot(g.root)
	if rerr != nil {
		return "", "", &globError{reason: "workspace root could not be resolved", cause: rerr}
	}
	return searchAbs, root, nil
}

// PrepareCall decodes and validates the untrusted arguments ONCE, resolves the
// canonical walk root ONCE, and returns the typed request — ONE filesystem.read
// requirement for the walked root (plain canonical path as Scope for profile
// routing, canonical tree encoding as Match for durable tree rules, empty
// grant pair) — plus the typed artifact InvokableRun executes. Invalid input
// fails here and never reaches the permission gate.
func (g *Glob) PrepareCall(_ context.Context, executionID uuid.UUID, argsJSON string) (tool.Request, tool.PreparedArtifact, error) {
	art, err := g.prepareGlob(argsJSON)
	if err != nil {
		return tool.Request{}, nil, err
	}
	return tool.Request{
		ToolName:     globToolName,
		ExecutionID:  executionID.String(),
		Requirements: []tool.Requirement{prepared.TreeReadRequirement(art.searchAbs)},
	}, art, nil
}

// InvokableRun executes the PREPARED artifact bound to this call — the raw
// argsJSON is never reparsed, so mutating it after preparation changes
// nothing; without its artifact the tool fails closed. It walks the approved
// root, matches each workspace-relative path against the prepared pattern,
// EXCLUDES any path DeniedRead reports, caps results, and returns a
// newline-separated list. Every failure is a tool-result string.
func (g *Glob) InvokableRun(ctx context.Context, _ string) (*tool.ToolResult, error) {
	art, ok := prepared.FromContext[*globArtifact](ctx)
	if !ok || art == nil {
		return tool.TextResult("error: permission denied: Glob requires its prepared call artifact"), nil
	}

	// Enforce the APPROVED resolved walk root: a resolution changed between
	// prepare and run (a symlink swap) refuses the walk fail-closed.
	searchAbs, _, rerr := g.resolveSearch(art.searchRel)
	if rerr != nil {
		return tool.TextResult("error: " + rerr.Error()), nil
	}
	if searchAbs != art.searchAbs {
		return tool.TextResult("error: search root resolution changed since approval: " + art.searchRel), nil
	}

	matches, truncated, expired := g.walk(ctx, art.searchAbs, art.displayRoot, art.pattern)
	if expired {
		return tool.TextResult("error: glob timed out"), nil
	}
	return tool.TextResult(renderGlobResults(matches, truncated)), nil
}

// globError is the typed preparation failure for a Glob call: a non-secret
// reason plus an optional cause.
type globError struct {
	reason string
	cause  error
}

func (e *globError) Error() string { return e.reason }

func (e *globError) Unwrap() error { return e.cause }

// walk traverses searchAbs, returning the workspace-relative (slash) paths whose
// relPath matches pattern and that the shared deny-filter does NOT exclude, sorted
// and capped at maxGlobResults. truncated reports whether the cap was hit; expired
// reports that ctx was cancelled/expired mid-walk (the caller renders a timeout
// instead of a partial listing). A WalkDir error on a single entry is skipped
// (best-effort listing), never fatal.
func (g *Glob) walk(ctx context.Context, searchAbs, resolvedRoot, pattern string) (matches []string, truncated, expired bool) {
	walkErr := filepath.WalkDir(searchAbs, func(abs string, d fs.DirEntry, err error) error {
		// Cheap cancellability: abort before touching this entry if ctx is done so a
		// huge tree cannot block past cancellation.
		if ctx.Err() != nil {
			return errCtxCancelled
		}
		if err != nil {
			// Unreadable entry (permissions, races): skip it, keep walking.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			// Prune noise/VCS dirs at the directory level (don't descend) so a broad
			// pattern cannot flood the model context. The search root itself is never
			// pruned, so an explicit `root: ".git"` is still honoured.
			if abs != searchAbs && globNoiseDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		// Authoritative denied-path exclusion (shared helper): never leak a secret's
		// name. A denied or non-relativisable path is excluded.
		relSlash, denied := workspace.DenyFilteredRel(g.guard, resolvedRoot, abs)
		if denied {
			return nil
		}
		if !workspace.MatchGlob(pattern, relSlash) {
			return nil
		}
		matches = append(matches, relSlash)
		if len(matches) > maxGlobResults {
			truncated = true
			return errStopWalk
		}
		return nil
	})
	if errors.Is(walkErr, errCtxCancelled) {
		return nil, false, true
	}
	if len(matches) > maxGlobResults {
		matches = matches[:maxGlobResults]
		truncated = true
	}
	sort.Strings(matches)
	return matches, truncated, false
}

// errStopWalk is the shared sentinel that short-circuits a WalkDir traversal
// (Glob and Grep) once the result cap is reached. It is a leaf control-flow
// sentinel, never surfaced to a caller.
var errStopWalk = stopWalkError{}

// stopWalkError is the typed sentinel returned to abort WalkDir at the cap.
type stopWalkError struct{}

func (stopWalkError) Error() string { return "tools: walk result cap reached" }

// errCtxCancelled is the shared sentinel a WalkDir callback returns to abort a
// traversal when its context is cancelled or its deadline has expired (Glob and
// Grep's fallback), so a huge tree cannot block past cancellation. Like
// errStopWalk it is a leaf control-flow sentinel, never surfaced to a caller.
var errCtxCancelled = ctxCancelledError{}

// ctxCancelledError is the typed sentinel returned to abort WalkDir on context
// cancellation/expiry.
type ctxCancelledError struct{}

func (ctxCancelledError) Error() string { return "tools: walk aborted; context cancelled" }

// renderGlobResults formats the sorted match list, appending a truncation notice
// when the cap was hit. An empty list reports "no matches".
func renderGlobResults(matches []string, truncated bool) string {
	if len(matches) == 0 {
		return "no matches"
	}
	out := strings.Join(matches, "\n")
	if truncated {
		out += "\n[truncated: more than " + strconv.Itoa(maxGlobResults) + " matches; refine the pattern]"
	}
	return out
}

// compile-time assertions.
var (
	_ tool.InvokableTool = (*Glob)(nil)
	_ tool.CallPreparer  = (*Glob)(nil)
	_ tool.Auditable     = (*Glob)(nil)
)
