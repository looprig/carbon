//go:build linux && !android

package stdio

import (
	"bytes"
	"errors"
	"os"
	"strconv"
	"syscall"
	"time"
)

// waitProcessExit observes process-group leader termination without reaping
// it. A pidfd stays tied to this exact process even if its numeric pid is
// later reused, so it is safe to use as a non-reaping exit signal while
// Kill's grace-period escalation decides whether the leader is already gone.
func waitProcessExit(pid int) error {
	rawFD, _, errno := syscall.Syscall(sysPIDFDOpen, uintptr(pid), 0, 0)
	if errno != 0 {
		return waitProcessExitProc(pid)
	}
	fd := int(rawFD)
	defer syscall.Close(fd)

	var capacity syscall.FdSet
	if fd < 0 || fd >= len(capacity.Bits)*strconv.IntSize {
		return waitProcessExitProc(pid)
	}
	for {
		var read syscall.FdSet
		read.Bits[fd/strconv.IntSize] |= 1 << uint(fd%strconv.IntSize)
		_, err := syscall.Select(fd+1, &read, nil, nil, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return waitProcessExitProc(pid)
		}
		return nil
	}
}

// waitProcessExitProc is the non-reaping fallback for kernels or sandboxes
// without pidfd_open, and for pidfds too large for select's fixed fd_set.
func waitProcessExitProc(pid int) error {
	proc, err := os.OpenRoot("/proc")
	if err != nil {
		return err
	}
	defer proc.Close()
	path := strconv.Itoa(pid) + "/stat"
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		stat, err := proc.ReadFile(path)
		if err != nil {
			return err
		}
		// The command name is parenthesized and may contain spaces or ')', so
		// locate its final delimiter before reading the following state byte.
		nameEnd := bytes.LastIndexByte(stat, ')')
		if nameEnd < 0 || nameEnd+2 >= len(stat) {
			return syscall.EINVAL
		}
		switch stat[nameEnd+2] {
		case 'Z', 'X', 'x':
			return nil
		}
		<-ticker.C
	}
}
