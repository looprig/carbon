// spawn.go is the client side of the stdio transport: it starts a child
// process whose stdin/stdout carry the other end of a protocol.Conn, and
// supervises its teardown. The process-group handling here mirrors the
// proven pattern in foreignloops/driver/claude/claude.go: configure the child
// as its own process-group leader, escalate SIGINT -> grace period ->
// SIGKILL(group) on teardown, and reap it exactly once. Unsupported platforms
// (see process_unsupported.go) fail before any child is started; they never
// fall back to weaker supervision.
package stdio

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// closeGrace is how long Kill waits for a SIGINT'd process group to exit on
// its own before escalating to SIGKILL.
const closeGrace = 2 * time.Second

// Command describes a child process to spawn. Every field is validated at
// the boundary: Path must be an absolute, cleaned path (never resolved via
// PATH lookup or a shell); Env is the child's complete environment (an
// explicit whitelist assembled by the caller) and is never merged with this
// process's ambient environment, even when nil or empty.
type Command struct {
	// Path is the absolute, cleaned path to the executable.
	Path string
	// Args is the argument list (excluding argv[0]).
	Args []string
	// Env is the child's complete environment. A nil or empty Env starts the
	// child with an empty environment, never this process's ambient one.
	Env []string
	// Dir is the child's working directory. Empty means "inherit this
	// process's current working directory" (as os/exec does by default); a
	// non-empty Dir must be an absolute, cleaned path.
	Dir string
}

// CommandError reports an invalid Command discovered before any child is
// started.
type CommandError struct{ Field, Reason string }

func (e *CommandError) Error() string {
	return "stdio: command: " + e.Field + ": " + e.Reason
}

// PlatformError reports an OS without the process-group supervision Spawn
// requires. Spawn returns this before starting a child; it never degrades to
// weaker supervision.
type PlatformError struct{ GOOS string }

func (e *PlatformError) Error() string {
	return "stdio: process supervision is unsupported on " + e.GOOS
}

// ExitError reports a child's abnormal exit together with the last
// stderrRingCapacity bytes of its stderr, kept only for diagnosis and never
// parsed for protocol meaning.
type ExitError struct {
	// Err is the underlying error from the child's Wait (typically an
	// *exec.ExitError, or the escalated-kill outcome).
	Err error
	// Stderr is the bounded tail of the child's stderr at the time it exited.
	Stderr []byte
}

func (e *ExitError) Error() string {
	if len(e.Stderr) == 0 {
		return fmt.Sprintf("stdio: process exited: %v", e.Err)
	}
	return fmt.Sprintf("stdio: process exited: %v; stderr tail: %s", e.Err, e.Stderr)
}

func (e *ExitError) Unwrap() error { return e.Err }

// Proc is a supervised child process. Stdin and Stdout carry the raw
// transport bytes for a protocol.Conn built on top of them; Stderr is never
// exposed as a stream — it is drained internally to a bounded ring and
// surfaced only via ExitError.
type Proc struct {
	Stdin  io.WriteCloser
	Stdout io.Reader

	cmd       *exec.Cmd
	processMu sync.Mutex
	startDone chan struct{}
	started   bool
	startErr  error
	pgid      int
	stderr    *stderrRing

	waitOnce sync.Once
	waitErr  error

	killOnce sync.Once
	killErr  error
}

// Spawn starts cmd as the leader of its own process group and returns a Proc
// supervising it. It validates cmd, checks platform support, and honors ctx:
// a context already canceled (or one that is canceled before the child
// exits) triggers the same SIGINT -> grace -> SIGKILL(group) teardown that
// Kill performs, run asynchronously so ctx cancellation never blocks on it.
//
// The returned Proc's Stdin/Stdout are pipes to the child; the child's
// stderr is drained internally (never exposed) to a bounded ring surfaced
// via ExitError on abnormal exit.
func Spawn(ctx context.Context, cmd Command) (*Proc, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateCommand(cmd); err != nil {
		return nil, err
	}
	if err := platformSupported(); err != nil {
		return nil, err
	}

	// #nosec G204 -- cmd.Path is caller-validated above (clean, absolute, no
	// shell/PATH lookup) and cmd.Args is a fixed argv list passed
	// positionally; there is no shell and no string splitting.
	ecmd := exec.CommandContext(ctx, cmd.Path, cmd.Args...)
	ecmd.Dir = cmd.Dir
	// Never nil: exec.Cmd treats a nil Env as "inherit the ambient
	// environment," which is exactly what must never happen here.
	ecmd.Env = append(make([]string, 0, len(cmd.Env)), cmd.Env...)
	ring := newStderrRing()
	ecmd.Stderr = ring
	configureProcessGroup(ecmd)

	stdin, err := ecmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := ecmd.StdoutPipe()
	if err != nil {
		return nil, err
	}

	p := &Proc{
		Stdin:     stdin,
		Stdout:    stdout,
		cmd:       ecmd,
		startDone: make(chan struct{}),
		stderr:    ring,
	}

	// The default CommandContext Cancel (Process.Kill) only signals the
	// leader, which can orphan the rest of the process group. Replace it with
	// our own group-aware teardown, run asynchronously: Cancel must not block
	// waiting for the child, and Kill already performs its own single reap.
	// Leaving WaitDelay at zero disables exec's own force-kill escalation, so
	// this is the only escalation path.
	ecmd.Cancel = func() error {
		go func() { _ = p.Kill() }()
		return nil
	}

	err = ecmd.Start()
	p.processMu.Lock()
	if err != nil {
		p.startErr = err
		close(p.startDone)
		p.processMu.Unlock()
		return nil, err
	}
	p.pgid = ecmd.Process.Pid
	p.started = true
	close(p.startDone)
	p.processMu.Unlock()
	return p, nil
}

func validateCommand(cmd Command) error {
	if cmd.Path == "" {
		return &CommandError{Field: "Path", Reason: "required"}
	}
	if !cleanAbsolutePath(cmd.Path) {
		return &CommandError{Field: "Path", Reason: "must be a clean absolute path"}
	}
	if cmd.Dir != "" && !cleanAbsolutePath(cmd.Dir) {
		return &CommandError{Field: "Dir", Reason: "must be empty or a clean absolute path"}
	}
	return nil
}

func cleanAbsolutePath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path
}

// Wait blocks until the child exits and reaps it. This is the single call to
// the underlying *exec.Cmd.Wait — safe to call concurrently and any number of
// times, including after Kill (which calls it internally as its own final
// step): every caller observes the same cached result. A non-nil result is
// always an *ExitError carrying the stderr tail captured at exit.
func (p *Proc) Wait() error {
	p.waitOnce.Do(func() {
		err := p.cmd.Wait()
		if err != nil {
			err = &ExitError{Err: err, Stderr: p.stderr.Bytes()}
		}
		p.waitErr = err
	})
	return p.waitErr
}

// Signal sends sig to the entire process group (the leader and every
// descendant that has not changed its own group).
func (p *Proc) Signal(sig os.Signal) error {
	pgid, err := p.processGroupID()
	if err != nil {
		return err
	}
	return signalProcessGroup(pgid, sig)
}

// Kill tears the process group down: SIGINT, a closeGrace grace period for a
// cooperative exit, then SIGKILL to the group, followed by the single Wait
// reap. It is idempotent — concurrent or repeated calls all observe the same
// result — and always returns after the child has been reaped, so it never
// leaves a zombie behind.
func (p *Proc) Kill() error {
	p.killOnce.Do(func() {
		p.killErr = p.teardown()
	})
	return p.killErr
}

func (p *Proc) teardown() error {
	pgid, err := p.processGroupID()
	if err != nil {
		return err
	}

	_ = interruptProcessGroup(pgid)

	exited := make(chan struct{})
	go func() {
		// A failed observer cannot safely distinguish a running leader from
		// an exited one, so treat failure as a request for immediate
		// escalation.
		_ = waitProcessExit(pgid)
		close(exited)
	}()

	timer := time.NewTimer(closeGrace)
	select {
	case <-exited:
		if !timer.Stop() {
			<-timer.C
		}
		// The leader has exited but remains unreaped, so its pid still
		// reserves the process-group id while any surviving descendants are
		// killed.
		_ = killProcessGroup(pgid)
	case <-timer.C:
		// Escalate before waiting: the live leader still owns the numeric
		// pgid.
		_ = killProcessGroup(pgid)
		<-exited
	}

	return p.Wait()
}

func (p *Proc) processGroupID() (int, error) {
	<-p.startDone
	p.processMu.Lock()
	defer p.processMu.Unlock()
	if !p.started {
		return 0, p.startErr
	}
	return p.pgid, nil
}
