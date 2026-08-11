//go:build darwin || (linux && !android)

package stdio

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

func platformSupported() error { return nil }

// configureProcessGroup marks cmd so that, once started, its process becomes
// the leader of a new process group (pgid == pid). This is what lets
// interruptProcessGroup/killProcessGroup target the whole group — including
// any descendants the child spawns — with a single signal, rather than only
// the direct leader.
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func interruptProcessGroup(pgid int) error { return syscall.Kill(-pgid, syscall.SIGINT) }
func killProcessGroup(pgid int) error      { return syscall.Kill(-pgid, syscall.SIGKILL) }

func signalProcessGroup(pgid int, sig os.Signal) error {
	s, ok := sig.(syscall.Signal)
	if !ok {
		return fmt.Errorf("stdio: signal %v is not a syscall.Signal", sig)
	}
	return syscall.Kill(-pgid, s)
}
