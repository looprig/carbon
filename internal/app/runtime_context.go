package app

import (
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/tools/skill"
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
// an argv list (never a shell string) and returns its stdout. Defaulted to a
// bounded exec.CommandContext wrapper; replaced by a fake in tests so the provider
// never depends on a real git repo.
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

// NewRuntimeContextProvider returns the default RuntimeContextProvider wired to the
// real clock (time.Now), the real working directory (os.Getwd), and a bounded,
// timeout-guarded git runner. The carbon composition root constructs it so
// the engine-generic loop package stays free of os/exec.
func NewRuntimeContextProvider() loop.RuntimeContextProvider {
	return newRuntimeContextProvider(nil)
}

func newRuntimeContextProvider(catalog func() []skill.SkillMeta) loop.RuntimeContextProvider {
	return &defaultRuntimeContextProvider{
		clock:   time.Now,
		getwd:   os.Getwd,
		run:     runGitCommand,
		catalog: catalog,
	}
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
