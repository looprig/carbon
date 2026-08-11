//go:build unix

// This file is the Unix half of process ownership: the child leads its own
// process group, and termination addresses the group rather than the process.
//
// A server that spawns helpers (a language runtime's worker, a wrapper script's
// real payload) leaves them behind when only the leader is signalled, and an
// orphaned helper still holds the pipes it inherited. Killing the group is what
// makes "the server is gone" true rather than likely.

package stdio

import (
	"errors"
	"os"
	"syscall"
)

// sysProcAttr puts the child in a new process group whose id is its own pid, so
// that -pid addresses the server and every descendant that has not deliberately
// left the group.
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// terminateGroup asks the child's group to shut down.
func terminateGroup(p *os.Process) error {
	return signalGroup(p, syscall.SIGTERM)
}

// killGroup destroys the child's group.
func killGroup(p *os.Process) error {
	return signalGroup(p, syscall.SIGKILL)
}

// signalGroup sends sig to the child's process group.
//
// A group that is already gone is not a failure: termination races the
// process's own exit by nature, and a caller that treated "it was already dead"
// as an error would report a failure every time shutdown was fast. Anything
// else — a permission error, an unknown pid we should still own — is returned,
// because it means the process may still be running.
//
// The negative pid is the group: the child was made a group leader at Start, so
// its pgid is its pid. If setting the group failed, the kernel would have
// failed the fork, not silently left the child in ours.
func signalGroup(p *os.Process, sig syscall.Signal) error {
	if p == nil {
		return errors.New("no process to signal")
	}
	err := syscall.Kill(-p.Pid, sig)
	if err == nil || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

// exitSignal names the signal that ended ps, or returns "" if it exited on its
// own.
func exitSignal(ps *os.ProcessState) string {
	status, ok := ps.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() {
		return ""
	}
	return status.Signal().String()
}
