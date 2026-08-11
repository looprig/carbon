//go:build darwin

package stdio

import (
	"errors"
	"strconv"
	"syscall"
)

// waitProcessExit observes process-group leader termination without reaping
// it. Kill's grace-period escalation needs to know the leader has already
// exited (so it can skip straight to reclaiming the process-group id) without
// ever calling Proc.Wait itself — that single reap is reserved for the
// caller, exactly once, after teardown decides the leader is gone.
func waitProcessExit(pid int) error {
	ident, err := strconv.ParseUint(strconv.Itoa(pid), 10, 64)
	if err != nil {
		return syscall.EINVAL
	}
	kq, err := syscall.Kqueue()
	if err != nil {
		return err
	}
	defer syscall.Close(kq)

	changes := []syscall.Kevent_t{{
		Ident:  ident,
		Filter: syscall.EVFILT_PROC,
		Flags:  syscall.EV_ADD | syscall.EV_ENABLE | syscall.EV_ONESHOT,
		Fflags: syscall.NOTE_EXIT,
	}}
	events := make([]syscall.Kevent_t, 1)
	for {
		n, waitErr := syscall.Kevent(kq, changes, events, nil)
		if errors.Is(waitErr, syscall.EINTR) {
			continue
		}
		if waitErr != nil {
			return waitErr
		}
		changes = nil
		if n == 0 {
			continue
		}
		if events[0].Flags&syscall.EV_ERROR != 0 && events[0].Data != 0 {
			return syscall.EINVAL
		}
		return nil
	}
}
