// Package ttylog keeps dependency logging off a full-screen TUI. A Bubble Tea
// program renders to stdout; any library that writes to stdout or stderr corrupts
// the alt-screen display. For example github.com/google/go-tdx-guest calls
// logger.Init(os.Stdout) at package-init time, so its TDX attestation warnings print
// straight onto the screen.
//
// CaptureStdio hands the caller a dedicated handle to the real terminal (for the TUI
// to render to) and points the process's stdout and stderr at a log file, so library
// output is preserved for debugging without ever reaching the screen. It operates at
// the file-descriptor level (replacing fd 1 and fd 2), so it affects every writer
// that holds os.Stdout / os.Stderr — including loggers that captured them at
// package-init time and therefore cannot be retargeted by reassigning the variables.
package ttylog

import (
	"errors"
	"os"
)

// errNilDst is the leaf cause for a CaptureStdio called with a nil destination.
var errNilDst = errors.New("nil destination file")

// RedirectError reports a failure to redirect a standard stream. Op names the
// failing step ("capture", "dup", "dup2", or "dup3").
type RedirectError struct {
	Op  string
	Err error
}

// Error implements error.
func (e *RedirectError) Error() string { return "ttylog: " + e.Op + ": " + e.Err.Error() }

// Unwrap exposes the underlying cause for errors.Is/As.
func (e *RedirectError) Unwrap() error { return e.Err }

// Capture is the result of CaptureStdio.
type Capture struct {
	// TTY is a handle to the original terminal. Render the TUI here (e.g.
	// tea.WithOutput) because the process's stdout now points at the log file.
	TTY *os.File
	// Restore reinstates the original stdout and stderr and releases TTY. Call it
	// once, after the TUI exits.
	Restore func() error
}

// CaptureStdio points the process's stdout (fd 1) and stderr (fd 2) at w and returns
// a handle to the original terminal for the TUI to render to. On any failure it
// returns a *RedirectError and leaves the standard streams untouched.
//
// On platforms without Unix file descriptors (e.g. Windows) it returns a
// *RedirectError; the caller should fall back to leaving stdio as-is.
func CaptureStdio(w *os.File) (*Capture, error) {
	if w == nil {
		return nil, &RedirectError{Op: "capture", Err: errNilDst}
	}

	// Save the real terminal (original stdout) for the TUI before redirecting fd 1.
	savedOut, err := dupFD(int(os.Stdout.Fd()))
	if err != nil {
		return nil, &RedirectError{Op: "dup", Err: err}
	}
	tty := os.NewFile(uintptr(savedOut), "/dev/tty")

	restoreOut, err := redirectFD(int(os.Stdout.Fd()), w)
	if err != nil {
		_ = tty.Close()
		return nil, err
	}
	restoreErr, err := redirectFD(int(os.Stderr.Fd()), w)
	if err != nil {
		_ = restoreOut()
		_ = tty.Close()
		return nil, err
	}

	return &Capture{
		TTY: tty,
		Restore: func() error {
			e1 := restoreErr()
			e2 := restoreOut()
			_ = tty.Close()
			if e1 != nil {
				return e1
			}
			return e2
		},
	}, nil
}

// redirectFD points fd at dst and returns a restore func that reinstates the
// original target of fd. The returned restore is meant to be called once.
func redirectFD(fd int, dst *os.File) (func() error, error) {
	saved, err := dupFD(fd)
	if err != nil {
		return nil, &RedirectError{Op: "dup", Err: err}
	}
	if err := dup2FD(int(dst.Fd()), fd); err != nil {
		_ = closeFD(saved)
		return nil, &RedirectError{Op: "dup2", Err: err}
	}
	return func() error {
		defer func() { _ = closeFD(saved) }()
		if err := dup2FD(saved, fd); err != nil {
			return &RedirectError{Op: "dup2", Err: err}
		}
		return nil
	}, nil
}
