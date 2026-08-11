package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/tools/skill"
)

// fakeGitError is a typed error for the runner seam's failure paths in tests.
type fakeGitError struct{ msg string }

func (e fakeGitError) Error() string { return e.msg }

// fixedClock returns a deterministic time so the date assertion is stable.
func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

// gitRunner builds a runner seam that answers `git rev-parse` / `git status`
// from canned values and returns runErr for any other (or all, when set) calls.
func gitRunner(branch, status string, branchErr, statusErr error) runtimeCommandRunner {
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "git" || len(args) == 0 {
			return nil, fakeGitError{msg: "unexpected command"}
		}
		switch args[0] {
		case "rev-parse":
			if branchErr != nil {
				return nil, branchErr
			}
			return []byte(branch), nil
		case "status":
			if statusErr != nil {
				return nil, statusErr
			}
			return []byte(status), nil
		default:
			return nil, fakeGitError{msg: "unexpected git subcommand"}
		}
	}
}

func TestDefaultRuntimeContextProviderRendersSkillCatalog(t *testing.T) {
	t.Parallel()

	p := &defaultRuntimeContextProvider{
		clock: fixedClock(time.Date(2026, time.August, 11, 0, 0, 0, 0, time.UTC)),
		getwd: func() (string, error) { return "/work/repo", nil },
		run:   gitRunner("main\n", "", nil, nil),
		catalog: func() []skill.SkillMeta {
			return []skill.SkillMeta{
				{Name: "zeta", Description: "Zeta description."},
				{Name: "alpha", Description: "Alpha description."},
			}
		},
	}

	text := soleText(t, p.Blocks(context.Background()))
	want := `<runtime_context>
date: 2026-08-11
cwd: /work/repo
git branch: main
git status: clean

<available_skills>
<skill>
<name>alpha</name>
<description>Alpha description.</description>
</skill>
<skill>
<name>zeta</name>
<description>Zeta description.</description>
</skill>
</available_skills>
</runtime_context>`
	if text != want {
		t.Fatalf("Blocks() text =\n%s\nwant:\n%s", text, want)
	}
	if strings.Contains(text, "representative secret skill body") {
		t.Fatal("Blocks() leaked a skill body")
	}
}

func TestDefaultRuntimeContextProviderSkillCatalogEscapesAndDeduplicates(t *testing.T) {
	t.Parallel()

	p := &defaultRuntimeContextProvider{
		clock: fixedClock(time.Date(2026, time.August, 11, 0, 0, 0, 0, time.UTC)),
		getwd: func() (string, error) { return "", errors.New("no cwd") },
		run:   gitRunner("", "", fakeGitError{msg: "no git"}, nil),
		catalog: func() []skill.SkillMeta {
			return []skill.SkillMeta{
				{Name: "duplicate", Description: "Zeta winner must lose."},
				{Name: "escape</name><forged>", Description: "use <care> & caution\x00\x01\xff"},
				{Name: "duplicate", Description: "Alpha winner."},
				{Name: "", Description: "missing name"},
				{Name: "missing-description", Description: ""},
			}
		},
	}

	text := soleText(t, p.Blocks(context.Background()))
	if got := strings.Count(text, "<name>duplicate</name>"); got != 1 {
		t.Fatalf("duplicate rendered %d times, want once:\n%s", got, text)
	}
	for _, want := range []string{
		"<description>Alpha winner.</description>",
		"<name>escape&lt;/name&gt;&lt;forged&gt;</name>",
		"<description>use &lt;care&gt; &amp; caution</description>",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("catalog missing escaped deterministic content %q:\n%s", want, text)
		}
	}
	for _, absent := range []string{"Zeta winner must lose.", "<forged>", "\x00", "\x01", "\xff", "missing name", "missing-description"} {
		if strings.Contains(text, absent) {
			t.Errorf("catalog contains unsafe or invalid content %q:\n%s", absent, text)
		}
	}
	if !utf8.ValidString(text) {
		t.Fatal("catalog output is not valid UTF-8")
	}
}

func TestDefaultRuntimeContextProviderSkillCatalogBoundsAreAtomic(t *testing.T) {
	t.Parallel()

	metas := make([]skill.SkillMeta, maxRuntimeSkillEntries+10)
	for i := range metas {
		metas[i] = skill.SkillMeta{
			Name:        fmt.Sprintf("%03d-%s", i, strings.Repeat("n", maxRuntimeSkillNameBytes)),
			Description: strings.Repeat("é", maxRuntimeSkillDescriptionBytes),
		}
	}
	p := &defaultRuntimeContextProvider{
		clock:   fixedClock(time.Date(2026, time.August, 11, 0, 0, 0, 0, time.UTC)),
		getwd:   func() (string, error) { return "/work/repo", nil },
		run:     gitRunner("main\n", "", nil, nil),
		catalog: func() []skill.SkillMeta { return metas },
	}

	text := soleText(t, p.Blocks(context.Background()))
	entries := strings.Count(text, "<skill>")
	if entries == 0 || entries > maxRuntimeSkillEntries || entries >= len(metas) {
		t.Fatalf("rendered entries = %d, want 1..%d and fewer than %d", entries, maxRuntimeSkillEntries, len(metas))
	}
	if strings.Count(text, "<available_skills>") != 1 || strings.Count(text, "</available_skills>") != 1 {
		t.Fatalf("catalog section is not whole and singular:\n%s", text)
	}
	if len(text) > maxRuntimeContextBytes || !strings.HasSuffix(text, "</available_skills>\n</runtime_context>") {
		t.Fatalf("bounded output lost a closing tag (len %d):\n%s", len(text), text)
	}
	if !utf8.ValidString(text) {
		t.Fatal("bounded catalog output is not valid UTF-8")
	}
	firstNameStart := strings.Index(text, "<name>") + len("<name>")
	firstNameEnd := strings.Index(text[firstNameStart:], "</name>") + firstNameStart
	if got := len(text[firstNameStart:firstNameEnd]); got != maxRuntimeSkillNameBytes {
		t.Errorf("rendered name bytes = %d, want %d", got, maxRuntimeSkillNameBytes)
	}
	firstDescriptionStart := strings.Index(text, "<description>") + len("<description>")
	firstDescriptionEnd := strings.Index(text[firstDescriptionStart:], "</description>") + firstDescriptionStart
	if got := len(text[firstDescriptionStart:firstDescriptionEnd]); got > maxRuntimeSkillDescriptionBytes {
		t.Errorf("rendered description bytes = %d, want <= %d", got, maxRuntimeSkillDescriptionBytes)
	}
}

func TestDefaultRuntimeContextProviderSkillCatalogCapsEntryCount(t *testing.T) {
	t.Parallel()

	metas := make([]skill.SkillMeta, maxRuntimeSkillEntries+10)
	for i := range metas {
		metas[i] = skill.SkillMeta{Name: fmt.Sprintf("skill-%03d", i), Description: "d"}
	}
	p := &defaultRuntimeContextProvider{
		clock:   fixedClock(time.Date(2026, time.August, 11, 0, 0, 0, 0, time.UTC)),
		getwd:   func() (string, error) { return "", errors.New("no cwd") },
		run:     gitRunner("", "", fakeGitError{msg: "no git"}, nil),
		catalog: func() []skill.SkillMeta { return metas },
	}

	text := soleText(t, p.Blocks(context.Background()))
	if got := strings.Count(text, "<skill>"); got != maxRuntimeSkillEntries {
		t.Fatalf("rendered entries = %d, want explicit cap %d", got, maxRuntimeSkillEntries)
	}
}

func TestDefaultRuntimeContextProviderSkillCatalogPreservesUTF8AndClosesRuntimeAtTotalLimit(t *testing.T) {
	t.Parallel()

	p := &defaultRuntimeContextProvider{
		clock: fixedClock(time.Date(2026, time.August, 11, 0, 0, 0, 0, time.UTC)),
		getwd: func() (string, error) { return "", errors.New("no cwd") },
		run:   gitRunner(strings.Repeat("é", maxRuntimeContextBytes), "", nil, nil),
		catalog: func() []skill.SkillMeta {
			return []skill.SkillMeta{{Name: "omitted", Description: "no partial section"}}
		},
	}

	text := soleText(t, p.Blocks(context.Background()))
	if len(text) > maxRuntimeContextBytes || !utf8.ValidString(text) || !strings.HasSuffix(text, "</runtime_context>") {
		t.Fatalf("total bound produced invalid output: len=%d utf8=%v suffix=%v", len(text), utf8.ValidString(text), strings.HasSuffix(text, "</runtime_context>"))
	}
	if strings.Contains(text, "<available_skills>") || strings.Contains(text, "</available_skills>") {
		t.Fatalf("catalog was partially rendered when it did not fit:\n%s", text)
	}
}

func TestDefaultRuntimeContextProviderSkillCatalogIsFreshAndNilSafe(t *testing.T) {
	t.Parallel()

	base := defaultRuntimeContextProvider{
		clock: fixedClock(time.Date(2026, time.August, 11, 0, 0, 0, 0, time.UTC)),
		getwd: func() (string, error) { return "", errors.New("no cwd") },
		run:   gitRunner("", "", fakeGitError{msg: "no git"}, nil),
	}
	if text := soleText(t, base.Blocks(context.Background())); strings.Contains(text, "<available_skills>") {
		t.Fatalf("nil catalog seam rendered a section:\n%s", text)
	}

	calls := 0
	base.catalog = func() []skill.SkillMeta {
		calls++
		return []skill.SkillMeta{{Name: fmt.Sprintf("fresh-%d", calls), Description: "current metadata"}}
	}
	first := soleText(t, base.Blocks(context.Background()))
	second := soleText(t, base.Blocks(context.Background()))
	if calls != 2 || !strings.Contains(first, "fresh-1") || strings.Contains(first, "fresh-2") || !strings.Contains(second, "fresh-2") || strings.Contains(second, "fresh-1") {
		t.Fatalf("catalog seam was not queried fresh per Blocks call: calls=%d\nfirst=%s\nsecond=%s", calls, first, second)
	}
}

func TestDefaultRuntimeContextProviderMissingSkillsDirectoryIsOrdinaryRuntimeContext(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir() // deliberately has no .skills directory
	p := &defaultRuntimeContextProvider{
		clock: fixedClock(time.Date(2026, time.August, 11, 0, 0, 0, 0, time.UTC)),
		getwd: func() (string, error) { return workspace, nil },
		run:   gitRunner("", "", fakeGitError{msg: "not a repository"}, nil),
		catalog: func() []skill.SkillMeta {
			return skill.DiscoverWorkspaceSkills(workspace)
		},
	}

	text := soleText(t, p.Blocks(context.Background()))
	want := "<runtime_context>\ndate: 2026-08-11\ncwd: " + workspace + "\n</runtime_context>"
	if text != want {
		t.Fatalf("Blocks() with missing .skills = %q, want ordinary runtime block %q", text, want)
	}
	if strings.Contains(text, "<available_skills>") {
		t.Fatalf("Blocks() with missing .skills rendered a catalog: %q", text)
	}
}

// soleText extracts the single TextBlock the provider must return, failing the
// test otherwise. It centralizes the "exactly one TextBlock" contract.
func soleText(t *testing.T, blocks []content.Block) string {
	t.Helper()
	if len(blocks) != 1 {
		t.Fatalf("Blocks() returned %d blocks, want exactly 1", len(blocks))
	}
	tb, ok := blocks[0].(*content.TextBlock)
	if !ok {
		t.Fatalf("Blocks()[0] type = %T, want *content.TextBlock", blocks[0])
	}
	return tb.Text
}

func TestDefaultRuntimeContextProviderImplementsInterface(t *testing.T) {
	t.Parallel()
	var _ loop.RuntimeContextProvider = NewRuntimeContextProvider()
}

func TestDefaultRuntimeContextProviderBlocks(t *testing.T) {
	t.Parallel()

	fixed := time.Date(2026, time.June, 22, 9, 30, 0, 0, time.UTC)

	tests := []struct {
		name        string
		clock       func() time.Time
		getwd       func() (string, error)
		run         runtimeCommandRunner
		wantContain []string
		wantAbsent  []string
	}{
		{
			name:  "happy path: date, cwd, branch and status all present",
			clock: fixedClock(fixed),
			getwd: func() (string, error) { return "/work/repo", nil },
			run:   gitRunner("feature/carbon\n", " M a.go\n?? b.go\n", nil, nil),
			wantContain: []string{
				"<runtime_context>",
				"</runtime_context>",
				"2026-06-22",
				"/work/repo",
				"feature/carbon",
				"2", // changed-file count
			},
		},
		{
			name:  "clean tree reports no changes",
			clock: fixedClock(fixed),
			getwd: func() (string, error) { return "/work/repo", nil },
			run:   gitRunner("main\n", "", nil, nil),
			wantContain: []string{
				"main",
				"clean",
			},
		},
		{
			name:        "git branch failure degrades: date present, no branch, no error",
			clock:       fixedClock(fixed),
			getwd:       func() (string, error) { return "/work/repo", nil },
			run:         gitRunner("", "", fakeGitError{msg: "not a git repository"}, nil),
			wantContain: []string{"2026-06-22", "/work/repo"},
			wantAbsent:  []string{"branch", "not a git repository"},
		},
		{
			name:        "git status failure degrades: branch present, no status",
			clock:       fixedClock(fixed),
			getwd:       func() (string, error) { return "/work/repo", nil },
			run:         gitRunner("main\n", "", nil, fakeGitError{msg: "status boom"}),
			wantContain: []string{"2026-06-22", "main"},
			wantAbsent:  []string{"status boom"},
		},
		{
			name:        "cwd error degrades: date present, no cwd",
			clock:       fixedClock(fixed),
			getwd:       func() (string, error) { return "", errors.New("getwd boom") },
			run:         gitRunner("main\n", "", nil, nil),
			wantContain: []string{"2026-06-22"},
			wantAbsent:  []string{"getwd boom"},
		},
		{
			name:        "total git failure: only date and cwd, never errors",
			clock:       fixedClock(fixed),
			getwd:       func() (string, error) { return "/work/repo", nil },
			run:         func(context.Context, string, ...string) ([]byte, error) { return nil, fakeGitError{msg: "git missing"} },
			wantContain: []string{"2026-06-22", "/work/repo"},
			wantAbsent:  []string{"git missing", "branch"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := &defaultRuntimeContextProvider{
				clock: tt.clock,
				getwd: tt.getwd,
				run:   tt.run,
			}
			text := soleText(t, p.Blocks(context.Background()))
			for _, want := range tt.wantContain {
				if !strings.Contains(text, want) {
					t.Errorf("block text missing %q\n---\n%s", want, text)
				}
			}
			for _, absent := range tt.wantAbsent {
				if strings.Contains(text, absent) {
					t.Errorf("block text unexpectedly contains %q\n---\n%s", absent, text)
				}
			}
		})
	}
}

// TestDefaultRuntimeContextProviderClockControlsDate proves the date is taken from
// the injected clock seam (not the real wall clock).
func TestDefaultRuntimeContextProviderClockControlsDate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		when time.Time
		want string
	}{
		{name: "leap day", when: time.Date(2024, time.February, 29, 0, 0, 0, 0, time.UTC), want: "2024-02-29"},
		{name: "year boundary", when: time.Date(1999, time.December, 31, 23, 59, 0, 0, time.UTC), want: "1999-12-31"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := &defaultRuntimeContextProvider{
				clock: fixedClock(tt.when),
				getwd: func() (string, error) { return "", errors.New("no cwd") },
				run:   func(context.Context, string, ...string) ([]byte, error) { return nil, fakeGitError{msg: "no git"} },
			}
			text := soleText(t, p.Blocks(context.Background()))
			if !strings.Contains(text, tt.want) {
				t.Errorf("date %q not in block\n---\n%s", tt.want, text)
			}
		})
	}
}

// TestDefaultRuntimeContextProviderBoundsOutput proves runaway git output is
// truncated so a huge status can never bloat the turn/context window.
func TestDefaultRuntimeContextProviderBoundsOutput(t *testing.T) {
	t.Parallel()

	huge := strings.Repeat(" M file.go\n", 100000) // ~1MB of status lines
	p := &defaultRuntimeContextProvider{
		clock: fixedClock(time.Date(2026, time.June, 22, 0, 0, 0, 0, time.UTC)),
		getwd: func() (string, error) { return "/work/repo", nil },
		run:   gitRunner("main\n", huge, nil, nil),
	}
	text := soleText(t, p.Blocks(context.Background()))
	if len(text) > maxRuntimeContextBytes {
		t.Errorf("block text len = %d, want <= %d (output not bounded)", len(text), maxRuntimeContextBytes)
	}
}

// TestNewRuntimeContextProviderDefaults proves the public constructor wires real
// seams and never panics / errors against the live environment (it degrades).
func TestNewRuntimeContextProviderDefaults(t *testing.T) {
	t.Parallel()
	p := NewRuntimeContextProvider()
	blocks := p.Blocks(context.Background())
	if len(blocks) != 1 {
		t.Fatalf("Blocks() len = %d, want 1", len(blocks))
	}
	if _, ok := blocks[0].(*content.TextBlock); !ok {
		t.Fatalf("Blocks()[0] type = %T, want *content.TextBlock", blocks[0])
	}
}
