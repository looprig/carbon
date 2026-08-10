package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/loop"
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
