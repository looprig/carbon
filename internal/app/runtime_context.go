package app

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/loop"
)

const (
	// runtimeGitTimeout bounds each git invocation so a hung/slow repo never
	// stalls the turn. Runtime context is best-effort: it must be cheap.
	runtimeGitTimeout = 2 * time.Second
	// maxRuntimeGitBytes caps the bytes read from a single git command, so a
	// pathological `git status` (huge untracked tree) cannot blow the buffer.
	maxRuntimeGitBytes = 16 << 10 // 16 KiB
	// maxRuntimeStatusFiles caps the per-file lines we enumerate from status
	// before collapsing to a count, keeping the block compact.
	maxRuntimeStatusFiles = 20
	// maxRuntimeContextBytes is the hard ceiling on the rendered block text, a
	// final guard so the volatile tail can never bloat the context window.
	maxRuntimeContextBytes = 4 << 10 // 4 KiB
	// runtimeDateLayout is the date format injected into the block (UTC date only;
	// the wall-clock time is intentionally omitted as needless churn).
	runtimeDateLayout = "2006-01-02"
)

// runtimeCommandRunner is the command-execution seam. It runs a fixed binary with
// an argv list (never a shell string) and returns its stdout. Defaulted to a
// bounded exec.CommandContext wrapper; replaced by a fake in tests so the provider
// never depends on a real git repo.
type runtimeCommandRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

// defaultRuntimeContextProvider builds the volatile per-turn runtime block
// (date/cwd/git) from injected seams. Every field is a seam so tests are
// deterministic and the impl never touches the real clock, cwd, or git directly.
type defaultRuntimeContextProvider struct {
	clock func() time.Time
	getwd func() (string, error)
	run   runtimeCommandRunner
}

// NewRuntimeContextProvider returns the default RuntimeContextProvider wired to the
// real clock (time.Now), the real working directory (os.Getwd), and a bounded,
// timeout-guarded git runner. The carbon composition root constructs it so
// the engine-generic loop package stays free of os/exec.
func NewRuntimeContextProvider() loop.RuntimeContextProvider {
	return &defaultRuntimeContextProvider{
		clock: time.Now,
		getwd: os.Getwd,
		run:   runGitCommand,
	}
}

// Blocks renders exactly one <runtime_context> TextBlock. It is non-fatal by
// contract: the date is always present; cwd and git degrade silently (omitted)
// when their seam fails — Blocks never returns an error and never panics.
func (p *defaultRuntimeContextProvider) Blocks(ctx context.Context) []content.Block {
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

	// Bound the body BEFORE the close tag, reserving room for it, so even a
	// pathological body yields a well-formed <runtime_context>…</runtime_context>
	// Phase 2 can still recognize. The per-component caps above make this a
	// last-resort guard, not the primary bound.
	const closeTag = "</runtime_context>"
	body := b.String()
	if max := maxRuntimeContextBytes - len(closeTag); len(body) > max {
		body = body[:max]
	}
	return []content.Block{&content.TextBlock{Text: body + closeTag}}
}

// writeGit appends the git branch + status summary, degrading silently: a failed
// branch lookup (not a repo) omits all git lines; a failed status omits only the
// status line. No git error is ever surfaced or logged (it may contain paths).
func (p *defaultRuntimeContextProvider) writeGit(ctx context.Context, b *strings.Builder) {
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

// runGitCommand is the default runtimeCommandRunner: a bounded, timeout-guarded
// exec of a fixed binary with an argv list (no shell). stderr is discarded so a
// repo path or error string can never leak into the block.
func runGitCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, runtimeGitTimeout)
	defer cancel()

	// #nosec G204 -- name is a fixed binary ("git") chosen by this package, never
	// from user input; args are a static argv list (no shell, no interpolation).
	cmd := exec.CommandContext(ctx, name, args...)
	var out bytes.Buffer
	cmd.Stdout = &boundedWriter{buf: &out, limit: maxRuntimeGitBytes}
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return nil, &runtimeGitError{cmd: name, cause: err}
	}
	return out.Bytes(), nil
}

// boundedWriter caps how many bytes it accepts, discarding the rest, so a runaway
// git command cannot grow the buffer without bound. It never errors (so it does
// not abort the command); excess output is simply dropped.
type boundedWriter struct {
	buf   *bytes.Buffer
	limit int
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	if remaining := w.limit - w.buf.Len(); remaining > 0 {
		if len(p) > remaining {
			w.buf.Write(p[:remaining])
		} else {
			w.buf.Write(p)
		}
	}
	// Report the full length so the writer is never treated as short by exec.
	return len(p), nil
}

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
