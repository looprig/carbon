package presentation

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// clipboardCopiedMsg reports the outcome of a local clipboard write so the model
// can surface a transient "copied N chars" notice, or an error notice on failure.
// n counts runes (not bytes); err is nil on success. It is a tea.Msg.
type clipboardCopiedMsg struct {
	n   int
	err error
}

// clipboardError — a local clipboard write failed. Argv is the clipboard command
// that was run; Cause wraps the underlying os/exec error. It is typed so callers
// can errors.As it, and never swallowed with _.
type clipboardError struct {
	Argv  []string
	Cause error
}

func (e *clipboardError) Error() string {
	return fmt.Sprintf("tui: clipboard write via %q failed: %v", strings.Join(e.Argv, " "), e.Cause)
}

func (e *clipboardError) Unwrap() error { return e.Cause }

// copyCmd puts text on the system clipboard two ways so it works everywhere:
// tea.SetClipboard emits OSC 52 through the program's OWN writer (reaching the
// terminal even though process stdout is redirected to the log, and covering
// SSH where only OSC 52 works), and a local clipboard binary via os/exec covers
// Apple Terminal and tmux, which ignore OSC 52. An empty string is a no-op:
// it returns a cmd but writes nothing, so an empty selection never clears the
// user's clipboard. Local-write errors surface via clipboardCopiedMsg, never
// swallowed.
func copyCmd(text string) tea.Cmd {
	if text == "" {
		return func() tea.Msg { return clipboardCopiedMsg{} }
	}
	return tea.Batch(tea.SetClipboard(text), localWriteCmd(text))
}

// localWriteCmd writes text to the local platform clipboard and reports the
// result (rune count + any error) as a clipboardCopiedMsg.
func localWriteCmd(text string) tea.Cmd {
	return func() tea.Msg {
		return clipboardCopiedMsg{n: len([]rune(text)), err: writeLocalClipboard(text)}
	}
}

// writeLocalClipboard pipes text to the platform clipboard binary. On an
// unsupported OS there is no local binary, so it is a no-op (nil) — OSC 52 via
// SetClipboard is the fallback. An exec failure is wrapped in a typed
// clipboardError.
func writeLocalClipboard(text string) error {
	argv := localClipboardArgv(runtime.GOOS, os.Getenv("WAYLAND_DISPLAY") != "")
	if argv == nil {
		return nil
	}
	if err := runClipboardCmd(argv, text); err != nil {
		return &clipboardError{Argv: argv, Cause: err}
	}
	return nil
}

// localClipboardArgv returns the clipboard-write command (as an argv list) for
// the given OS, or nil when no local clipboard binary is known. wayland selects
// wl-copy over xclip on linux.
func localClipboardArgv(goos string, wayland bool) []string {
	switch goos {
	case "darwin":
		return []string{"pbcopy"}
	case "linux":
		if wayland {
			return []string{"wl-copy"}
		}
		return []string{"xclip", "-selection", "clipboard"}
	default:
		return nil
	}
}

// runClipboardCmd runs argv with stdin piped to it. It is a package-level var so
// tests inject a fake without spawning a process; the default is the real
// os/exec write.
var runClipboardCmd = func(argv []string, stdin string) error {
	// #nosec G204 -- argv is not user input: it comes only from
	// localClipboardArgv, a fixed allow-list of clipboard binaries. text is piped
	// to stdin as data, never parsed as a shell string, so there is no injection.
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = strings.NewReader(stdin)
	return cmd.Run()
}
