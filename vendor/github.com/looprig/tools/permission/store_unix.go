//go:build !windows

package permission

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"

	"github.com/looprig/tools/internal/nofollow"
)

// store_unix.go implements store.go's platform primitives for every
// non-Windows target using the POSIX syscall package: symlink-safe opens
// (delegated to the shared internal/nofollow primitive), advisory
// interprocess locking (flock), and the owner/link-count identity checks
// (Stat_t). See store_windows.go for the Windows counterparts and store.go
// for the shared logic that calls into these.

// openNoFollow opens path with flag, refusing to traverse a symlink at the
// final path component — the store's defense against a symlink swapped in
// by another user to redirect the store onto a file it does not expect to
// read or write. It delegates the actual open to the shared
// internal/nofollow primitive (also used by grep/readfile/filemutation) and
// translates its exported ErrSymlinkNotAllowed into this package's own
// errSymlinkNotAllowed so store.go's shared classification keeps using its
// private sentinel.
func openNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	file, err := nofollow.Open(path, flag, perm)
	if err != nil {
		if errors.Is(err, nofollow.ErrSymlinkNotAllowed) {
			return nil, fmt.Errorf("%w: %w", errSymlinkNotAllowed, err)
		}
		return nil, err
	}
	return file, nil
}

// lockExclusiveNonBlocking takes an exclusive, non-blocking flock on f:
// LOCK_EX|LOCK_NB returns immediately rather than blocking, and flock
// associates the lock with the open file description, so separate Store
// instances contend correctly whether they live in one process or many.
func lockExclusiveNonBlocking(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EINTR) {
		return errLockWouldBlock
	}
	return err
}

// unlockFile releases a lock taken by lockExclusiveNonBlocking. Callers
// treat it as best-effort: closing the descriptor also drops the flock even
// if the explicit unlock fails.
func unlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

// checkFileIdentity enforces the owner and hard-link-count hardening checks
// for the permission file: it must be owned by the store's effective user
// and have exactly one hard link, so a second name for the same inode
// cannot be used to smuggle writes past the store's atomic-rename update
// path. file is unused on this platform; the stat comes from the open
// descriptor's info, matching Windows's handle-based check.
func (s *Store) checkFileIdentity(file *os.File, info fs.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return &FileError{Path: s.path, Reason: FileIO, Err: errors.New("underlying stat unavailable")}
	}
	if int(stat.Uid) != s.euid {
		return &FileError{Path: s.path, Reason: FileOwnerUnexpected, Err: fmt.Errorf("owner uid %d, require %d", stat.Uid, s.euid)}
	}
	if stat.Nlink != 1 {
		return &FileError{Path: s.path, Reason: FileLinkCount, Err: fmt.Errorf("link count %d, require 1", stat.Nlink)}
	}
	return nil
}

// checkDirIdentity enforces the owner hardening check for the store
// directory. directory is unused on this platform; the stat comes from the
// already-taken os.Lstat info, matching Windows's path-based check.
func (s *Store) checkDirIdentity(directory string, info fs.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return &FileError{Path: s.path, Reason: FileIO, Err: errors.New("underlying stat unavailable")}
	}
	if int(stat.Uid) != s.euid {
		return &FileError{Path: s.path, Reason: FileOwnerUnexpected, Err: fmt.Errorf("store directory owner uid %d, require %d", stat.Uid, s.euid)}
	}
	return nil
}
