package app

import (
	"context"
	"encoding/xml"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/sandbox"
	"github.com/looprig/tools/skill"
)

const (
	// runtimeGitTimeout bounds each git invocation so a hung/slow repo never
	// stalls the turn. Runtime context is best-effort: it must be cheap.
	runtimeGitTimeout = 2 * time.Second
	// maxRuntimeGitBytes caps the bytes read from a single git command, so a
	// pathological `git status` (huge untracked tree) cannot blow the buffer.
	maxRuntimeGitBytes int64 = 16 << 10 // 16 KiB
	// maxRuntimeStatusFiles caps the per-file lines we enumerate from status
	// before collapsing to a count, keeping the block compact.
	maxRuntimeStatusFiles = 20
	// maxRuntimeSkillEntries caps the number of metadata records rendered even
	// when an injected catalog returns more than workspace discovery normally can.
	maxRuntimeSkillEntries = 32
	// maxRuntimeSkillNameBytes and maxRuntimeSkillDescriptionBytes bound cleaned,
	// unescaped UTF-8 metadata. Rendering is rune-safe; escaping may expand it,
	// and the aggregate runtime-context ceiling remains authoritative.
	maxRuntimeSkillNameBytes        = 128
	maxRuntimeSkillDescriptionBytes = 512
	// maxRuntimeContextBytes is the hard ceiling on the rendered block text, a
	// final guard so the volatile tail can never bloat the context window.
	maxRuntimeContextBytes = 4 << 10 // 4 KiB
	// runtimeDateLayout is the date format injected into the block (UTC date only;
	// the wall-clock time is intentionally omitted as needless churn).
	runtimeDateLayout = "2006-01-02"
)

// runtimeCommandRunner is the command-execution seam. It runs a fixed binary with
// an argv list (never a shell string) and returns its stdout. Production wires
// runSandboxedGit, which runs git inside the session's sandbox; tests replace it
// with a fake so the provider never depends on a real git repo. A nil runner
// omits every git line.
type runtimeCommandRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

// defaultRuntimeContextProvider builds the volatile per-turn runtime block
// (date/cwd/git) from injected seams. Every field is a seam so tests are
// deterministic and the impl never touches the real clock, cwd, or git directly.
type defaultRuntimeContextProvider struct {
	clock   func() time.Time
	getwd   func() (string, error)
	run     runtimeCommandRunner
	catalog func() []skill.SkillMeta
}

// NewRuntimeContextProvider returns a RuntimeContextProvider wired to the real
// clock (time.Now) and the real working directory (os.Getwd). It has no session
// sandbox, so it reports NO git state: Carbon never executes git with its own
// process authority (see runSandboxedGit). Session composition uses
// newRuntimeContextProvider with the session's executor set instead.
func NewRuntimeContextProvider() loop.RuntimeContextProvider {
	return newRuntimeContextProvider(nil, nil)
}

// newRuntimeContextProvider is the TUI/headless provider: the model is told the
// process working directory, and git state for it (including an enclosing
// repository) is read by git running INSIDE the session sandbox set. A nil set
// omits git.
func newRuntimeContextProvider(set *sandbox.ExecutorSet, catalog func() []skill.SkillMeta) loop.RuntimeContextProvider {
	p := &defaultRuntimeContextProvider{
		clock:   time.Now,
		getwd:   os.Getwd,
		catalog: catalog,
	}
	if set != nil {
		p.run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			dir, err := os.Getwd()
			if err != nil {
				return nil, &runtimeGitError{cmd: name, cause: err}
			}
			return runSandboxedGit(ctx, set, dir, "", name, args...)
		}
	}
	return p
}

// Blocks renders exactly one <runtime_context> TextBlock. It is non-fatal by
// contract: the date is always present; cwd and git degrade silently (omitted)
// when their seam fails — Blocks never returns an error and never panics.
func (p *defaultRuntimeContextProvider) Blocks(ctx context.Context) []content.Block {
	const closeTag = "</runtime_context>"

	var b strings.Builder
	b.WriteString("<runtime_context>\n")
	b.WriteString("date: ")
	b.WriteString(p.clock().UTC().Format(runtimeDateLayout))
	b.WriteByte('\n')

	if cwd, err := p.getwd(); err == nil && cwd != "" {
		b.WriteString("cwd: ")
		b.WriteString(cwd)
		b.WriteByte('\n')
	}

	p.writeGit(ctx, &b)

	// Bound the pre-catalog body first, on a UTF-8 boundary. Catalog records are
	// then admitted only as whole escaped entries with room for both close tags.
	bodyLimit := maxRuntimeContextBytes - len(closeTag)
	body := truncateUTF8Bytes(b.String(), bodyLimit)
	b.Reset()
	b.WriteString(body)
	p.writeSkillCatalog(&b, bodyLimit)

	return []content.Block{&content.TextBlock{Text: b.String() + closeTag}}
}

type runtimeSkillMeta struct {
	name        string
	description string
}

func (p *defaultRuntimeContextProvider) writeSkillCatalog(b *strings.Builder, bodyLimit int) {
	if p.catalog == nil {
		return
	}

	raw := p.catalog()
	if len(raw) == 0 {
		return
	}
	metas := make([]runtimeSkillMeta, 0, len(raw))
	for _, meta := range raw {
		name := strings.TrimSpace(normalizeXMLText(meta.Name))
		description := strings.TrimSpace(normalizeXMLText(meta.Description))
		if name == "" || description == "" {
			continue
		}
		metas = append(metas, runtimeSkillMeta{name: name, description: description})
	}
	if len(metas) == 0 {
		return
	}
	sort.Slice(metas, func(i, j int) bool {
		if metas[i].name != metas[j].name {
			return metas[i].name < metas[j].name
		}
		return metas[i].description < metas[j].description
	})

	const sectionOpen = "\n<available_skills>\n"
	const sectionClose = "</available_skills>\n"
	remaining := bodyLimit - b.Len()
	if remaining < len(sectionOpen)+len(sectionClose) {
		return
	}
	var section strings.Builder
	section.WriteString(sectionOpen)
	rendered := 0
	lastName := ""
	for _, meta := range metas {
		if meta.name == lastName {
			continue
		}
		lastName = meta.name
		if rendered == maxRuntimeSkillEntries {
			break
		}
		entry := renderRuntimeSkill(meta)
		if section.Len()+len(entry)+len(sectionClose) > remaining {
			continue
		}
		section.WriteString(entry)
		rendered++
	}
	if rendered == 0 {
		return
	}
	section.WriteString(sectionClose)
	b.WriteString(section.String())
}

func renderRuntimeSkill(meta runtimeSkillMeta) string {
	name := escapeXMLText(truncateUTF8Bytes(meta.name, maxRuntimeSkillNameBytes))
	description := escapeXMLText(truncateUTF8Bytes(meta.description, maxRuntimeSkillDescriptionBytes))
	return "<skill>\n<name>" + name + "</name>\n<description>" + description + "</description>\n</skill>\n"
}

func normalizeXMLText(s string) string {
	s = strings.ToValidUTF8(s, "")
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == '\t' || r == '\n' || r == '\r' ||
			r >= 0x20 && r <= 0xD7FF ||
			r >= 0xE000 && r <= 0xFFFD ||
			r >= 0x10000 && r <= 0x10FFFF {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func truncateUTF8Bytes(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(s) <= limit {
		return s
	}
	for limit > 0 && !utf8.RuneStart(s[limit]) {
		limit--
	}
	return s[:limit]
}

func escapeXMLText(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// writeGit appends the git branch + status summary, degrading silently: a failed
// branch lookup (not a repo) omits all git lines; a failed status omits only the
// status line. A nil runner (no session sandbox) omits every git line. No git
// error is ever surfaced or logged (it may contain paths).
func (p *defaultRuntimeContextProvider) writeGit(ctx context.Context, b *strings.Builder) {
	if p.run == nil {
		return
	}
	out, err := p.run(ctx, "git", "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return
	}
	branch := strings.TrimSpace(string(out))
	if branch == "" {
		return
	}
	b.WriteString("git branch: ")
	b.WriteString(branch)
	b.WriteByte('\n')

	status, err := p.run(ctx, "git", "status", "--porcelain")
	if err != nil {
		return
	}
	b.WriteString(summarizeStatus(string(status)))
	b.WriteByte('\n')
}

// summarizeStatus collapses `git status --porcelain` into a compact line: the
// count of changed files, plus up to maxRuntimeStatusFiles of the names. A clean
// tree (no output) reads as "clean".
func summarizeStatus(porcelain string) string {
	lines := splitNonEmptyLines(porcelain)
	if len(lines) == 0 {
		return "git status: clean"
	}
	var b strings.Builder
	b.WriteString("git status: ")
	b.WriteString(strconv.Itoa(len(lines)))
	b.WriteString(" changed")
	shown := lines
	if len(shown) > maxRuntimeStatusFiles {
		shown = shown[:maxRuntimeStatusFiles]
	}
	b.WriteString(" (")
	b.WriteString(strings.Join(shown, ", "))
	if len(lines) > len(shown) {
		b.WriteString(", …")
	}
	b.WriteByte(')')
	return b.String()
}

// splitNonEmptyLines splits on newlines and drops blank lines (a trailing newline
// from git's output would otherwise count as a phantom change).
func splitNonEmptyLines(s string) []string {
	raw := strings.Split(s, "\n")
	out := make([]string, 0, len(raw))
	for _, line := range raw {
		if strings.TrimSpace(line) != "" {
			out = append(out, strings.TrimSpace(line))
		}
	}
	return out
}

// newSessionRuntimeContextProvider is the runtime-context provider for a
// session whose workspace is a per-session root rather than the process working
// directory (the pooled browser launcher). The model is told the session root —
// the directory its tools actually serve — and git runs inside the session
// sandbox in that root with repository discovery stopped at the root, so a
// pooled session never reports the server process's own directory or a
// repository enclosing the data root. A nil set omits git.
func newSessionRuntimeContextProvider(set *sandbox.ExecutorSet, root string, catalog func() []skill.SkillMeta) loop.RuntimeContextProvider {
	p := &defaultRuntimeContextProvider{
		clock:   time.Now,
		getwd:   func() (string, error) { return root, nil },
		catalog: catalog,
	}
	if set != nil {
		p.run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return runSandboxedGit(ctx, set, root, filepath.Dir(root), name, args...)
		}
	}
	return p
}

// runtimeContextExecutorKey names the session executor that runs the runtime
// context's git probe. It is distinct from every Loop's executor (those are keyed
// by Loop UUID), so the probe has its own scratch HOME and TMPDIR and never
// shares a grant key with a tool call.
const runtimeContextExecutorKey = "carbon-runtime-context"

// runtimeGitShim runs "$0" "$@" with stderr discarded. It is a FIXED string:
// the binary and its arguments are passed as positional parameters, never
// interpolated, so no path or argument is ever parsed by the shell.
const runtimeGitShim = `exec "$0" "$@" 2>/dev/null`

// runSandboxedGit runs git in dir INSIDE the session's sandbox, through the
// session executor set, never as a child of the Carbon process.
//
// This is a security boundary, not a convenience. The model can write its
// workspace (Trusted profile), and git honours repository-local configuration
// that executes commands — core.fsmonitor, .gitattributes clean/smudge filters,
// and other config-driven exec paths that cannot be enumerated or reliably
// disabled with -c flags. Run with Carbon's own authority, a planted
// configuration would execute unconfined on the next turn. Inside the sandbox,
// anything git runs has exactly the authority the model's own Bash command has
// (same profile, workspace, isolated HOME and scrubbed environment), so a plant
// gains nothing. A profile whose commands are gated (ReadOnly) refuses the
// ungranted run and git state is omitted.
//
// A non-empty ceiling becomes GIT_CEILING_DIRECTORIES. Inherited GIT_DIR-style
// variables are removed so discovery always starts at dir. GIT_OPTIONAL_LOCKS=0
// keeps `git status` from writing the index. Output is capped at
// maxRuntimeGitBytes; an over-long output is returned truncated.
func runSandboxedGit(ctx context.Context, set *sandbox.ExecutorSet, dir, ceiling, name string, args ...string) ([]byte, error) {
	executor, err := set.For(runtimeContextExecutorKey)
	if err != nil {
		return nil, &runtimeGitError{cmd: name, cause: err}
	}
	ctx, cancel := context.WithTimeout(ctx, runtimeGitTimeout)
	defer cancel()

	argv := []string{"env",
		"-u", "GIT_DIR", "-u", "GIT_WORK_TREE", "-u", "GIT_INDEX_FILE", "-u", "GIT_COMMON_DIR",
		"-u", "GIT_OBJECT_DIRECTORY", "-u", "GIT_CONFIG", "-u", "GIT_CONFIG_PARAMETERS",
		"GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0",
	}
	if ceiling != "" {
		argv = append(argv, "GIT_CEILING_DIRECTORIES="+ceiling)
	}
	argv = append(argv, "sh", "-c", runtimeGitShim, name)
	argv = append(argv, args...)
	out, code, err := executor.RunArgvLimited(ctx, dir, argv, maxRuntimeGitBytes)
	if errors.Is(err, sandbox.ErrOutputLimit) {
		return out, nil
	}
	if err != nil {
		return nil, &runtimeGitError{cmd: name, cause: err}
	}
	if code != 0 {
		return nil, &runtimeGitError{cmd: name, cause: &runtimeGitExitError{code: code}}
	}
	return out, nil
}

// runtimeGitExitError reports a sandboxed git run that exited non-zero (for
// example, not a repository).
type runtimeGitExitError struct{ code int }

func (e *runtimeGitExitError) Error() string { return "exit status " + strconv.Itoa(e.code) }

// runtimeGitError wraps a failed git invocation. It carries the command name and
// cause for errors.As inspection, but the provider deliberately never surfaces it
// to the model (git errors may embed filesystem paths).
type runtimeGitError struct {
	cmd   string
	cause error
}

func (e *runtimeGitError) Error() string {
	return "runtime context: " + e.cmd + " failed: " + e.cause.Error()
}

func (e *runtimeGitError) Unwrap() error { return e.cause }
