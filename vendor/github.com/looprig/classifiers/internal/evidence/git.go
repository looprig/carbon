package evidence

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/tool"
)

// KindGitRead is the single Requirement.Kind shared by every Git evidence
// tool in this pack (repository/worktree state, status, diff metadata,
// remotes, branch/upstream/default-branch, and remote-visibility hints).
// They share one kind deliberately: from an access-control perspective they
// are uniformly read-only metadata queries scoped to the review workspace's
// repository, with no differentiated risk tier that would justify a finer
// split (unlike the filesystem pack, where stat/list/read/glob/grep expose
// meaningfully different amounts of information). It is exported and
// stable: a consumer's evidence access allowlist configures trust by this
// exact string (design §13.1).
const KindGitRead = "evidence.git.read"

// hardMaxGitOutputBytes bounds any single git subprocess's captured output,
// independent of consumer-configured Limits, so a pathologically large
// diff or status cannot be used to exhaust memory even if misconfigured.
const hardMaxGitOutputBytes = 4 << 20 // 4 MiB

// gitProcessTimeout is an absolute ceiling on one git subprocess invocation,
// independent of ctx's own deadline: it guards against a git subprocess
// that, despite the sanitized environment, ends up blocking (e.g. on an
// unexpected prompt) longer than any sane evidence query should take.
const gitProcessTimeout = 20 * time.Second

// refPattern is the only shape a model-supplied Git ref name may take. It
// deliberately excludes a leading '-' (flag injection: a value like
// "--upload-pack=/bin/sh" must never reach git as a bare positional
// argument) and restricts the character set to what ordinary branch/tag/SHA
// names use, so it can never be mistaken for a git option or for range
// syntax ("..", "...") this package does not implement.
func validRef(ref string) bool {
	if ref == "" {
		return true // "" means "no explicit ref" — a valid, common case.
	}
	if len(ref) > 200 || !utf8.ValidString(ref) {
		return false
	}
	first := ref[0]
	if !isRefChar(first) || first == '-' {
		return false
	}
	for i := 0; i < len(ref); i++ {
		if !isRefChar(ref[i]) {
			return false
		}
	}
	return !strings.Contains(ref, "..")
}

func isRefChar(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	case b == '.' || b == '_' || b == '/' || b == '-':
		return true
	default:
		return false
	}
}

// gitSafeConfig is applied as a fixed set of `-c key=value` overrides before
// EVERY git invocation in this package, regardless of which subcommand
// runs. Most entries exist to neutralize repository-local configuration
// that could otherwise turn a read-only git invocation into arbitrary local
// code execution: core.fsmonitor (executes an arbitrary hook program on
// `status`), diff.external / a pathspec's diff driver textconv (executes an
// arbitrary program on `diff`), and credential helpers (would otherwise run
// even though this package never performs a network operation).
// core.quotePath is a correctness override, not a safety one: git's
// default C-quotes any path byte outside the printable ASCII range (e.g. a
// literal non-ASCII filename is rendered as `"unicode-h\303\251llo.txt"`),
// which would silently corrupt every non-ASCII evidence path this pack
// reports. Every value here is a deliberate, documented override — never
// derived from model or repository input.
var gitSafeConfig = []string{
	"core.fsmonitor=",
	"core.pager=cat",
	"core.editor=true",
	"core.quotePath=false",
	"color.ui=false",
	"advice.detachedHead=false",
	"diff.external=",
	"credential.helper=",
	"protocol.ext.allow=never",
	"protocol.file.allow=never",
}

// sanitizedGitEnv returns the ENTIRE environment for a git subprocess: it is
// never os.Environ() (which could leak ambient credentials/tokens into a
// subprocess a model's tool call ultimately triggers) and never grown by
// appending — it is one fixed, minimal, explicit set. HOME and
// XDG_CONFIG_HOME point at a nonexistent path so no real per-user gitconfig
// is read; GIT_CONFIG_NOSYSTEM disables /etc/gitconfig.
func sanitizedGitEnv(path string) []string {
	const inertHome = "/nonexistent-evidence-tool-home"
	return []string{
		"PATH=" + path,
		"HOME=" + inertHome,
		"XDG_CONFIG_HOME=" + inertHome + "/.config",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_PAGER=cat",
		"GIT_OPTIONAL_LOCKS=0",
		"LC_ALL=C",
		"LANG=C",
		"TZ=UTC",
	}
}

// runGit invokes exactly one fixed, allowlisted git subcommand (args) with
// no shell interpretation, scoped to dir, under a sanitized environment and
// gitSafeConfig. It NEVER builds args from untrusted string concatenation —
// every element of args is either a fixed literal or a value already
// validated by its caller (validRef, resolveRootRelative) and always placed
// as its own argv element. Cancelling ctx kills the child process (and any
// process it spawned) via exec.CommandContext + WaitDelay/Cancel, not just
// the Go-side wait.
func runGit(ctx context.Context, gitPath, envPath, dir string, args []string) (stdout []byte, exitCode int, truncated bool, err error) {
	full := make([]string, 0, 2+2*len(gitSafeConfig)+len(args))
	full = append(full, "-C", dir, "--no-optional-locks")
	for _, kv := range gitSafeConfig {
		full = append(full, "-c", kv)
	}
	full = append(full, args...)

	runCtx, cancel := context.WithTimeout(ctx, gitProcessTimeout)
	defer cancel()

	// #nosec G204 -- gitPath is resolved once from the real PATH by
	// resolveGitBinary (catalog.go), never from tool arguments; full's
	// elements are exclusively fixed literals ("-C", dir, "--no-optional-locks",
	// gitSafeConfig's "-c key=value" pairs, a fixed per-operation subcommand
	// argv) plus values every caller ALREADY validated before appending them
	// (validRef's charset check, resolveRootRelative's containment check).
	// There is no shell (CommandContext never invokes one), and no argv
	// element is ever built by concatenating untrusted text into a single
	// string — see runGit's doc comment.
	cmd := exec.CommandContext(runCtx, gitPath, full...)
	cmd.Env = sanitizedGitEnv(envPath)
	cmd.Dir = dir
	cmd.Cancel = func() error { return cmd.Process.Kill() }
	cmd.WaitDelay = 5 * time.Second

	var out boundedWriter
	out.limit = hardMaxGitOutputBytes
	cmd.Stdout = &out
	cmd.Stderr = nil // never captured: stderr may echo untrusted argv/config text

	runErr := cmd.Run()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, 0, false, ctxErr
	}
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			return out.buf.Bytes(), exitErr.ExitCode(), out.Truncated, nil
		}
		return nil, -1, false, fmt.Errorf("evidence: git invocation failed: %w", classifyExecError(runErr))
	}
	return out.buf.Bytes(), 0, out.Truncated, nil
}

// classifyExecError narrows an exec-layer failure to a bounded, secret-free
// sentinel; the raw error (which can embed argv/env-derived text on some
// platforms) is never retained past this call.
func classifyExecError(err error) error {
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return errGitUnavailable
	default:
		return errGitUnavailable
	}
}

var errGitUnavailable = errors.New("evidence: git is unavailable")

// boundedWriter caps how many bytes it retains; bytes beyond limit are
// accepted (so the writer never blocks or errors the writing process) but
// discarded, and Truncated is set. Bounding happens here, at the point
// bytes leave the subprocess pipe — never after slurping the full output
// into memory first.
type boundedWriter struct {
	buf       bytes.Buffer
	limit     int
	Truncated bool
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	if w.buf.Len() >= w.limit {
		w.Truncated = true
		return len(p), nil
	}
	remaining := w.limit - w.buf.Len()
	if len(p) > remaining {
		w.buf.Write(p[:remaining])
		w.Truncated = true
		return len(p), nil
	}
	w.buf.Write(p)
	return len(p), nil
}

// gitBinary resolves and caches the absolute path to the git executable
// found on the sanitized PATH. It never resolves a path supplied by tool
// arguments.
type gitBinary struct {
	path    string
	envPath string
}

func newGitBinary(path, envPath string) *gitBinary { return &gitBinary{path: path, envPath: envPath} }

// ---- shared git tool plumbing ------------------------------------------------

// gitRun is a small convenience bound to one workspace root and resolved
// git binary, shared by every concrete git tool below.
type gitRun struct {
	root   string
	binary *gitBinary
}

func (g gitRun) run(ctx context.Context, args ...string) (output string, exitCode int, truncated bool, err error) {
	if g.binary == nil || g.binary.path == "" {
		return "", -1, false, errGitUnavailable
	}
	out, code, truncated, err := runGit(ctx, g.binary.path, g.binary.envPath, g.root, args)
	return string(out), code, truncated, err
}

func isGitRepository(ctx context.Context, g gitRun) bool {
	_, code, _, err := g.run(ctx, "rev-parse", "--is-inside-work-tree")
	return err == nil && code == 0
}

// ---- evidence_git_repository_status ------------------------------------------

const toolNameGitRepositoryStatus = "evidence_git_repository_status"

const gitRepositoryStatusSchema = `{"type":"object","additionalProperties":false}`

type gitRepositoryStatusTool struct {
	g            gitRun
	maxStatusLen int
}

func newGitRepositoryStatusTool(root string, binary *gitBinary, maxStatusLen int) *gitRepositoryStatusTool {
	return &gitRepositoryStatusTool{g: gitRun{root: root, binary: binary}, maxStatusLen: maxStatusLen}
}

func (t *gitRepositoryStatusTool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{
		Name:   toolNameGitRepositoryStatus,
		Desc:   gitRepositoryStatusDesc,
		Schema: json.RawMessage(gitRepositoryStatusSchema),
	}, nil
}

func (t *gitRepositoryStatusTool) PrepareCall(_ context.Context, executionID uuid.UUID, argsJSON string) (tool.Request, tool.PreparedArtifact, error) {
	if err := decodeArgs(argsJSON, nil, &struct{}{}); err != nil {
		return tool.Request{}, nil, err
	}
	return tool.Request{
		ToolName:    toolNameGitRepositoryStatus,
		ExecutionID: executionID.String(),
		Requirements: []tool.Requirement{{
			Kind: KindGitRead, Scope: t.g.root, Match: "repository-status",
			Description: "git repository root, worktree state, and workspace-scoped status",
		}},
	}, nil, nil
}

func (t *gitRepositoryStatusTool) InvokableRun(ctx context.Context, argsJSON string) (*tool.ToolResult, error) {
	if err := decodeArgs(argsJSON, nil, &struct{}{}); err != nil {
		return tool.TextResult("error: malformed arguments"), nil
	}
	if !isGitRepository(ctx, t.g) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return tool.TextResult("git_repository: false\n"), nil
	}

	var b strings.Builder
	b.WriteString("git_repository: true\n")

	if top, _, _, err := t.g.run(ctx, "rev-parse", "--show-toplevel"); err == nil {
		fmt.Fprintf(&b, "toplevel: %s\n", strings.TrimSpace(top))
	} else if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}

	bareOut, _, _, err := t.g.run(ctx, "rev-parse", "--is-bare-repository")
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
	}
	fmt.Fprintf(&b, "is_bare: %s\n", strings.TrimSpace(bareOut))

	branchOut, branchCode, _, err := t.g.run(ctx, "symbolic-ref", "-q", "--short", "HEAD")
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
	}
	detached := branchCode != 0
	fmt.Fprintf(&b, "detached_head: %t\n", detached)
	if !detached {
		fmt.Fprintf(&b, "current_branch: %s\n", strings.TrimSpace(branchOut))
	}

	_, verifyCode, _, err := t.g.run(ctx, "rev-parse", "--verify", "-q", "HEAD")
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
	}
	fmt.Fprintf(&b, "has_commits: %t\n", verifyCode == 0)

	statusOut, _, captureTruncated, err := t.g.run(ctx, "status", "--porcelain=v2", "--untracked-files=all", "--", ".")
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		b.WriteString("status: unavailable\n")
		return tool.TextResult(b.String()), nil
	}
	trimmed, formatTruncated := truncateBytes(statusOut, t.maxStatusLen)
	b.WriteString("status:\n")
	b.WriteString(trimmed)
	fmt.Fprintf(&b, "status_truncated: %t\n", captureTruncated || formatTruncated)
	return tool.TextResult(b.String()), nil
}

func truncateBytes(s string, max int) (string, bool) {
	if max <= 0 {
		max = hardMaxGitOutputBytes
	}
	if len(s) <= max {
		return s, false
	}
	return s[:max], true
}

var _ tool.InvokableTool = (*gitRepositoryStatusTool)(nil)
var _ tool.CallPreparer = (*gitRepositoryStatusTool)(nil)

// ---- evidence_git_diff -------------------------------------------------------

const toolNameGitDiff = "evidence_git_diff"

const gitDiffSchema = `{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Restrict the diff to this path, relative to the review workspace root. '' or '.' means the whole workspace."},
    "ref": {"type": "string", "description": "Compare against this ref instead of the default. '' means the default comparison."},
    "staged": {"type": "boolean", "description": "Compare the index against HEAD (true) instead of the working tree against the index (false)."}
  },
  "required": ["path", "ref", "staged"],
  "additionalProperties": false
}`

type gitDiffTool struct {
	g          gitRun
	maxDiffLen int
}

func newGitDiffTool(root string, binary *gitBinary, maxDiffLen int) *gitDiffTool {
	return &gitDiffTool{g: gitRun{root: root, binary: binary}, maxDiffLen: maxDiffLen}
}

func (t *gitDiffTool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{
		Name:   toolNameGitDiff,
		Desc:   gitDiffDesc,
		Schema: json.RawMessage(gitDiffSchema),
	}, nil
}

type gitDiffArgsV1 struct {
	Path   string `json:"path"`
	Ref    string `json:"ref"`
	Staged bool   `json:"staged"`
}

var gitDiffArgsKeys = []string{"path", "ref", "staged"}

func (t *gitDiffTool) PrepareCall(_ context.Context, executionID uuid.UUID, argsJSON string) (tool.Request, tool.PreparedArtifact, error) {
	var args gitDiffArgsV1
	if err := decodeArgs(argsJSON, gitDiffArgsKeys, &args); err != nil {
		return tool.Request{}, nil, err
	}
	if !validRef(args.Ref) {
		return tool.Request{}, nil, errMalformedArguments
	}
	rel, err := resolveRootRelative(args.Path)
	if err != nil {
		return tool.Request{}, nil, err
	}
	if err := precheckContainment(t.g.root, rel, true); err != nil {
		return tool.Request{}, nil, err
	}
	return tool.Request{
		ToolName:    toolNameGitDiff,
		ExecutionID: executionID.String(),
		Requirements: []tool.Requirement{{
			Kind: KindGitRead, Scope: t.g.root, Match: rel,
			Description: truncateForDisplay(fmt.Sprintf("git diff %s (staged=%t)", rel, args.Staged), 512),
		}},
	}, nil, nil
}

func (t *gitDiffTool) InvokableRun(ctx context.Context, argsJSON string) (*tool.ToolResult, error) {
	var args gitDiffArgsV1
	if err := decodeArgs(argsJSON, gitDiffArgsKeys, &args); err != nil || !validRef(args.Ref) {
		return tool.TextResult("error: malformed arguments"), nil
	}
	rel, err := resolveRootRelative(args.Path)
	if err != nil {
		return tool.TextResult("error: path resolves outside the review workspace"), nil
	}
	if !isGitRepository(ctx, t.g) {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return tool.TextResult("git_repository: false\n"), nil
	}

	// Fixed argv shape; both variable elements are independently validated
	// above. ref (git diff [<ref>] [--] <path> positional syntax) is placed
	// as a bare revision argument — safe from flag injection because
	// validRef already rejects a leading '-' and any character outside its
	// fixed allowlist, so it can never be mistaken for an option. path
	// ALWAYS follows a literal "--" separator, so a leading-dash file name
	// can never be interpreted as a flag regardless of its content.
	diffArgs := []string{"diff", "--no-color", "--no-ext-diff", "--no-textconv"}
	if args.Staged {
		diffArgs = append(diffArgs, "--cached")
	}
	if args.Ref != "" {
		diffArgs = append(diffArgs, args.Ref)
	}
	diffArgs = append(diffArgs, "--", rel)

	out, _, captureTruncated, err := t.g.run(ctx, diffArgs...)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return tool.TextResult("error: diff unavailable\n"), nil
	}
	trimmed, formatTruncated := truncateBytes(out, t.maxDiffLen)

	var b strings.Builder
	fmt.Fprintf(&b, "path: %s\n", rel)
	fmt.Fprintf(&b, "ref: %s\n", args.Ref)
	fmt.Fprintf(&b, "staged: %t\n", args.Staged)
	fmt.Fprintf(&b, "bytes: %d\n", len(trimmed))
	fmt.Fprintf(&b, "truncated: %t\n", captureTruncated || formatTruncated)
	b.WriteString("---\n")
	b.WriteString(trimmed)
	return tool.TextResult(b.String()), nil
}

var _ tool.InvokableTool = (*gitDiffTool)(nil)
var _ tool.CallPreparer = (*gitDiffTool)(nil)

// ---- evidence_git_remotes ----------------------------------------------------

const toolNameGitRemotes = "evidence_git_remotes"

const gitRemotesSchema = `{"type":"object","additionalProperties":false}`

type gitRemotesTool struct {
	g          gitRun
	visibility VisibilityResolver
}

func newGitRemotesTool(root string, binary *gitBinary, visibility VisibilityResolver) *gitRemotesTool {
	return &gitRemotesTool{g: gitRun{root: root, binary: binary}, visibility: visibility}
}

func (t *gitRemotesTool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{
		Name:   toolNameGitRemotes,
		Desc:   gitRemotesDesc,
		Schema: json.RawMessage(gitRemotesSchema),
	}, nil
}

func (t *gitRemotesTool) PrepareCall(_ context.Context, executionID uuid.UUID, argsJSON string) (tool.Request, tool.PreparedArtifact, error) {
	if err := decodeArgs(argsJSON, nil, &struct{}{}); err != nil {
		return tool.Request{}, nil, err
	}
	return tool.Request{
		ToolName:    toolNameGitRemotes,
		ExecutionID: executionID.String(),
		Requirements: []tool.Requirement{{
			Kind: KindGitRead, Scope: t.g.root, Match: "remotes",
			Description: "configured git remotes and visibility hint",
		}},
	}, nil, nil
}

func (t *gitRemotesTool) InvokableRun(ctx context.Context, argsJSON string) (*tool.ToolResult, error) {
	if err := decodeArgs(argsJSON, nil, &struct{}{}); err != nil {
		return tool.TextResult("error: malformed arguments"), nil
	}
	if !isGitRepository(ctx, t.g) {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return tool.TextResult("git_repository: false\n"), nil
	}
	out, _, _, err := t.g.run(ctx, "remote", "-v")
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return tool.TextResult("error: remotes unavailable\n"), nil
	}

	remotes := parseGitRemotes(out)
	var b strings.Builder
	fmt.Fprintf(&b, "remotes: %d\n", len(remotes))
	for _, remote := range remotes {
		// The VisibilityResolver is a consumer-supplied, explicitly
		// configured, governed lookup (see VisibilityResolver's own doc
		// comment) — it receives the exact, unmodified fetch URL, since a
		// real implementation may legitimately need any embedded
		// credential to authenticate its own read-only lookup. Everything
		// derived from remote.fetchURL that becomes MODEL-VISIBLE below
		// (the reported fetch URL and the hint) instead uses
		// sanitizedFetchURL: never the raw value.
		visibility := resolveVisibility(ctx, t.visibility, remote.fetchURL)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		sanitizedFetchURL := sanitizeRemoteURL(remote.fetchURL)
		hint := RemoteVisibilityHint(sanitizedFetchURL)
		fmt.Fprintf(&b, "- %s fetch=%s hint=%s visibility=%s\n",
			remote.name, sanitizedFetchURL, hint, visibility)
	}
	return tool.TextResult(b.String()), nil
}

type gitRemote struct {
	name     string
	fetchURL string
}

// parseGitRemotes parses `git remote -v` output ("name\turl (fetch|push)"
// lines), keeping only fetch URLs and deduplicating by name. It only ever
// consumes text this package's own runGit produced from a fixed,
// argument-free subcommand.
func parseGitRemotes(out string) []gitRemote {
	seen := make(map[string]struct{})
	var remotes []gitRemote
	scanner := bufio.NewScanner(strings.NewReader(out))
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasSuffix(line, "(fetch)") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name, url := fields[0], fields[1]
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		remotes = append(remotes, gitRemote{name: name, fetchURL: url})
	}
	return remotes
}

var _ tool.InvokableTool = (*gitRemotesTool)(nil)
var _ tool.CallPreparer = (*gitRemotesTool)(nil)

// ---- evidence_git_branch -----------------------------------------------------

const toolNameGitBranch = "evidence_git_branch"

const gitBranchSchema = `{"type":"object","additionalProperties":false}`

type gitBranchTool struct{ g gitRun }

func newGitBranchTool(root string, binary *gitBinary) *gitBranchTool {
	return &gitBranchTool{g: gitRun{root: root, binary: binary}}
}

func (t *gitBranchTool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{
		Name:   toolNameGitBranch,
		Desc:   gitBranchDesc,
		Schema: json.RawMessage(gitBranchSchema),
	}, nil
}

func (t *gitBranchTool) PrepareCall(_ context.Context, executionID uuid.UUID, argsJSON string) (tool.Request, tool.PreparedArtifact, error) {
	if err := decodeArgs(argsJSON, nil, &struct{}{}); err != nil {
		return tool.Request{}, nil, err
	}
	return tool.Request{
		ToolName:    toolNameGitBranch,
		ExecutionID: executionID.String(),
		Requirements: []tool.Requirement{{
			Kind: KindGitRead, Scope: t.g.root, Match: "branch",
			Description: "current branch, upstream, and default branch",
		}},
	}, nil, nil
}

func (t *gitBranchTool) InvokableRun(ctx context.Context, argsJSON string) (*tool.ToolResult, error) {
	if err := decodeArgs(argsJSON, nil, &struct{}{}); err != nil {
		return tool.TextResult("error: malformed arguments"), nil
	}
	if !isGitRepository(ctx, t.g) {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return tool.TextResult("git_repository: false\n"), nil
	}

	var b strings.Builder

	branchOut, branchCode, _, err := t.g.run(ctx, "symbolic-ref", "-q", "--short", "HEAD")
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
	}
	detached := branchCode != 0
	fmt.Fprintf(&b, "detached_head: %t\n", detached)
	if !detached {
		fmt.Fprintf(&b, "current_branch: %s\n", strings.TrimSpace(branchOut))
	}

	_, verifyCode, _, err := t.g.run(ctx, "rev-parse", "--verify", "-q", "HEAD")
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
	}
	fmt.Fprintf(&b, "has_commits: %t\n", verifyCode == 0)

	upstreamOut, upstreamCode, _, err := t.g.run(ctx, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}")
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
	}
	if upstreamCode == 0 {
		fmt.Fprintf(&b, "upstream: %s\n", strings.TrimSpace(upstreamOut))
	} else {
		b.WriteString("upstream: none\n")
	}

	defaultBranch, source := t.resolveDefaultBranch(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fmt.Fprintf(&b, "default_branch: %s\n", defaultBranch)
	fmt.Fprintf(&b, "default_branch_source: %s\n", source)

	return tool.TextResult(b.String()), nil
}

// resolveDefaultBranch never uses a model-supplied value; every candidate
// ref queried below is a fixed literal.
func (t *gitBranchTool) resolveDefaultBranch(ctx context.Context) (branch, source string) {
	if out, code, _, err := t.g.run(ctx, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil && code == 0 {
		if name := strings.TrimSpace(out); name != "" {
			return strings.TrimPrefix(name, "origin/"), "origin/HEAD"
		}
	}
	for _, candidate := range []string{"main", "master"} {
		if _, code, _, err := t.g.run(ctx, "show-ref", "--verify", "--quiet", "refs/remotes/origin/"+candidate); err == nil && code == 0 {
			return candidate, "origin/" + candidate + " heuristic"
		}
		if _, code, _, err := t.g.run(ctx, "show-ref", "--verify", "--quiet", "refs/heads/"+candidate); err == nil && code == 0 {
			return candidate, "local " + candidate + " heuristic"
		}
	}
	return "unknown", "undetermined"
}

var _ tool.InvokableTool = (*gitBranchTool)(nil)
var _ tool.CallPreparer = (*gitBranchTool)(nil)
