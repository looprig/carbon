//go:build unix

package fsstore

import (
	"errors"
	"os"
	"syscall"
)

// lockFileEx takes an exclusive (LOCK_EX) advisory lock on f, blocking until it
// is granted. The lock is associated with the open file description, so it is
// released by unlockFile or automatically when the process exits or the fd is
// closed — a crashed holder never leaves a stale lock behind.
func lockFileEx(f *os.File) error {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return &FlockError{Op: "lock", Path: f.Name(), Cause: err}
	}
	return nil
}

// tryLockFileEx attempts a NON-BLOCKING exclusive (LOCK_EX|LOCK_NB) advisory lock
// on f. It reports a tri-state, distinguishing "held elsewhere" from a real fault:
//   - (true, nil)  — the lock was granted.
//   - (false, nil) — the lock is already held on another open file description
//     (EWOULDBLOCK). This is an expected outcome (the Leaser maps it to
//     *LeaseHeldError), not a failure.
//   - (false, *FlockError) — a genuine syscall fault.
//
// Like lockFileEx, the lock binds to the open file description, so it is released
// by unlockFile, by closing f, or automatically when the process exits — a crashed
// holder's lock is reclaimed by the kernel with no stale-lock cleanup required.
func tryLockFileEx(f *os.File) (acquired bool, err error) {
	lerr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if lerr == nil {
		return true, nil
	}
	if errors.Is(lerr, syscall.EWOULDBLOCK) {
		return false, nil
	}
	return false, &FlockError{Op: "trylock", Path: f.Name(), Cause: lerr}
}

// unlockFile releases the advisory lock held on f (LOCK_UN).
func unlockFile(f *os.File) error {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
		return &FlockError{Op: "unlock", Path: f.Name(), Cause: err}
	}
	return nil
}
