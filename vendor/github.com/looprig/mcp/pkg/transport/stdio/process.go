// This file defines the process seam: how a child MCP server is created,
// observed and destroyed. It is separate from stdio.go because it is the one
// part of this transport an application is expected to replace — a host that
// confines its servers (a sandbox, a jail, a container) supplies its own
// ProcessLauncher, and nothing in this module has to know how it works.

package stdio

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
)

// ProcessSpec is the complete, argv-only description of a child process. It is
// a value: a launcher may read it, but nothing here is a live handle onto this
// transport's state except the three files, which the child inherits.
//
// There is no shell anywhere in this type, and no field a shell could be
// smuggled through: Path names an executable and Args are its arguments, passed
// to execve as separate strings.
type ProcessSpec struct {
	// Path is the absolute path of the executable to run. It has already been
	// resolved (via PATH if the configured command was a bare name).
	Path string
	// Args are the arguments after the program name, exactly as os/exec takes
	// them. They are untrusted display-wise — an argument may carry a secret —
	// so a launcher must not log them.
	Args []string
	// Dir is the child's working directory. Empty means the parent's.
	Dir string
	// Env is the child's complete environment, built from an allowlist. An
	// empty (but non-nil) Env means the child gets no environment at all; a
	// launcher must never substitute the parent's for it.
	Env []string
	// Stdin, Stdout and Stderr are the child's three streams. Stdin and Stdout
	// are the MCP transport and carry nothing else. The launcher passes them to
	// the child and does not read, write or close them.
	Stdin  *os.File
	Stdout *os.File
	Stderr *os.File
}

// ExitStatus is how a child process ended, in neutral terms. It never names a
// syscall type, so a launcher on any platform can report one.
type ExitStatus struct {
	// Code is the exit status, or -1 when a signal ended the process.
	Code int
	// Signal names the signal that ended the process ("terminated",
	// "killed", ...), and is empty when the process exited on its own.
	Signal string
}

// String renders the status for a diagnostic message.
func (s ExitStatus) String() string {
	if s.Signal != "" {
		return "signal: " + s.Signal
	}
	return fmt.Sprintf("exit status %d", s.Code)
}

// Process is a started child process, owned by whoever started it.
//
// It is an interface rather than a concrete handle because termination is part
// of confinement, not just of exec: a launcher that puts the server in a jail,
// a cgroup or a container is the only thing that knows how to tear that down,
// and forcing this module's Kill (a signal to a process group) onto it would be
// wrong for every launcher whose child is not in this process's group at all.
// The seam is therefore "start it and destroy it", not "start it and let us
// signal it".
//
// Every method must tolerate being called after the process has already exited:
// termination races the process's own death by nature. Terminate and Kill
// report an error only when the request could not be delivered for a reason
// other than the process being gone.
type Process interface {
	// Pid reports the process id, for diagnostics only.
	Pid() int
	// Terminate asks the process, and everything it spawned, to shut down.
	Terminate() error
	// Kill destroys the process, and everything it spawned, unconditionally.
	Kill() error
	// Wait blocks until the process has exited and been reaped, and reports how
	// it ended. It is called exactly once. A non-nil error means the process
	// could not be reaped — a non-zero exit is a status, not an error.
	Wait() (ExitStatus, error)
}

// ProcessLauncher creates child processes. The default launcher runs the
// command directly with os/exec; an application that confines its MCP servers
// supplies its own, which is why this interface names nothing but the stdlib:
// this module never imports a confinement mechanism, it delegates to one.
//
// Start must honor ctx for the start itself. It must not tie the child's
// lifetime to ctx: ctx bounds connecting, and the process outlives it. Killing
// the child when the connect context is cancelled is this transport's job, done
// through Process.
type ProcessLauncher interface {
	Start(ctx context.Context, spec ProcessSpec) (Process, error)
}

// osLauncher is the default launcher: a plain argv exec with no confinement
// beyond a process group of its own.
type osLauncher struct{}

// Start runs spec with os/exec.
//
// Deliberately exec.Command and not exec.CommandContext: CommandContext kills
// the child when ctx is done, and ctx here is the connect context, which is
// cancelled the moment connecting returns — successfully or not. Tying the
// server's life to it would kill every server at the end of its own startup.
func (osLauncher) Start(ctx context.Context, spec ProcessSpec) (Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// #nosec G204 -- argv exec, never a shell: Path is an executable resolved
	// and validated by New, and every argument is passed to execve as its own
	// string. There is no interpolation anywhere on this path.
	cmd := exec.Command(spec.Path, spec.Args...)
	cmd.Dir = spec.Dir
	// Fail closed on the environment: os/exec reads a nil Env as "inherit the
	// parent's", which is exactly the wholesale inheritance the allowlist
	// exists to prevent. An empty environment must stay empty.
	cmd.Env = spec.Env
	if cmd.Env == nil {
		cmd.Env = []string{}
	}
	cmd.Stdin = spec.Stdin
	cmd.Stdout = spec.Stdout
	cmd.Stderr = spec.Stderr
	cmd.SysProcAttr = sysProcAttr()

	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &osProcess{cmd: cmd}, nil
}

// osProcess is the default launcher's Process: an *exec.Cmd whose child leads
// its own process group, so termination reaps everything it spawned rather than
// just the server itself.
//
// The mutex guards signalling against pid reuse. A pid names a process only
// until it is reaped; after that the kernel is free to hand the number out
// again, and this type signals a process *group* by number (-pid) through a raw
// syscall, which has none of os.Process's post-reap ErrProcessDone guard. Wait
// records the reap the instant it returns, and Terminate and Kill take the same
// lock and refuse to signal a number that is no longer ours.
//
// What this does not do is hold the lock across cmd.Wait itself, which would
// make the guard total — the kernel cannot recycle a pid before it is reaped —
// but would also deadlock: the reap runs for the child's whole life, and
// Terminate, blocked on the lock, is what would have ended it. What is left
// unguarded is the interval between the kernel freeing the pid inside cmd.Wait
// and this method's next instruction. Closing that last gap needs a hook
// between "the child exited" and "the pid is freed" — waitid(WNOWAIT), which is
// how os.Process manages it — and no portable one is exported to us. A signal
// would have to land inside those few instructions, against a pid recycled in
// the same breath.
type osProcess struct {
	cmd *exec.Cmd

	// mu guards reaped, and makes the check-and-signal in Terminate and Kill
	// atomic with respect to Wait recording the reap.
	mu sync.Mutex
	// reaped is true once Wait has collected the child, from which moment the
	// pid is no longer ours to signal.
	reaped bool
}

func (p *osProcess) Pid() int { return p.cmd.Process.Pid }

// Terminate signals the child's whole group to shut down. A child that has
// already been reaped is not signalled — and is not an error: it is gone, which
// is what Terminate was asking for.
func (p *osProcess) Terminate() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.reaped {
		return nil
	}
	return terminateGroup(p.cmd.Process)
}

// Kill destroys the child's whole group, unless it has already been reaped —
// see Terminate.
func (p *osProcess) Kill() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.reaped {
		return nil
	}
	return killGroup(p.cmd.Process)
}

// Wait reaps the child. A non-zero exit — or a signal — is a status this
// transport reports, not an error it fails on, so only a genuine wait failure
// comes back as one.
//
// The reap is recorded before the status is even examined: from cmd.Wait's
// return the pid belongs to the kernel again, and the flag is what stops this
// process from ever signalling it. It is recorded whatever cmd.Wait reported,
// because every way it returns for a started child leaves the child reaped —
// and if that were somehow untrue, declining to signal is the safe way to be
// wrong.
func (p *osProcess) Wait() (ExitStatus, error) {
	err := p.cmd.Wait()
	p.mu.Lock()
	p.reaped = true
	p.mu.Unlock()

	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return ExitStatus{}, err
	}
	if p.cmd.ProcessState == nil {
		return ExitStatus{}, errors.New("process was not reaped")
	}
	return ExitStatus{
		Code:   p.cmd.ProcessState.ExitCode(),
		Signal: exitSignal(p.cmd.ProcessState),
	}, nil
}
