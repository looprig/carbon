//go:build (darwin || linux) && !android && !ios

package backend

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func acquirePlatformForeignLock(path, sid, cwd string) (*foreignLock, error) {
	rootFD, err := openLockRoot()
	if err != nil {
		return nil, err
	}
	defer unix.Close(rootFD)

	filename := filepath.Base(path)
	fd, err := unix.Openat(rootFD, filename, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if errors.Is(err, unix.EEXIST) {
		fd, err = unix.Openat(rootFD, filename, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	}
	if err != nil {
		return nil, &LockError{Op: "open", Path: path, Cause: err}
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, &LockError{Op: "open", Path: path, Cause: errors.New("invalid lock file descriptor")}
	}
	closeWithError := func(op string, cause error) (*foreignLock, error) {
		_ = file.Close()
		return nil, &LockError{Op: op, Path: path, Cause: cause}
	}
	if err := validateLockFile(file); err != nil {
		return closeWithError("validate", err)
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			pid := readLockPID(file)
			_ = file.Close()
			return nil, &ForeignSessionBusyError{SID: sid, Cwd: cwd, PID: pid}
		}
		return closeWithError("acquire", err)
	}
	lock := &foreignLock{path: path, file: file}
	if err := writeLockPID(file); err != nil {
		lock.release()
		return nil, err
	}
	return lock, nil
}

func openLockRoot() (int, error) {
	root := lockRootPath()
	if err := os.Mkdir(root, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return -1, &LockError{Op: "mkdir", Path: root, Cause: err}
	}
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, &LockError{Op: "open_root", Path: root, Cause: err}
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = unix.Close(fd)
		return -1, &LockError{Op: "stat_root", Path: root, Cause: err}
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Uid != effectiveUID() || stat.Mode&0o077 != 0 {
		_ = unix.Close(fd)
		return -1, &LockError{Op: "validate_root", Path: root, Cause: errors.New("lock root must be an owner-only directory")}
	}
	return fd, nil
}

func validateLockFile(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("lock path is not a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return errors.New("lock file must be owner-only")
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return err
	}
	if stat.Uid != effectiveUID() || stat.Nlink != 1 {
		return errors.New("lock file must be owner-owned with one link")
	}
	return nil
}

func effectiveUID() uint32 {
	// #nosec G115 -- darwin/linux geteuid is a non-negative uid_t represented
	// by int in Go; both supported kernels expose uid_t as uint32 in Stat_t.
	return uint32(os.Geteuid())
}

func writeLockPID(file *os.File) error {
	path := file.Name()
	if err := file.Truncate(0); err != nil {
		return &LockError{Op: "truncate", Path: path, Cause: err}
	}
	if _, err := file.WriteAt([]byte(strconv.Itoa(os.Getpid())), 0); err != nil {
		return &LockError{Op: "write", Path: path, Cause: err}
	}
	if err := file.Sync(); err != nil {
		return &LockError{Op: "sync", Path: path, Cause: err}
	}
	return nil
}

func readLockPID(file *os.File) int {
	buffer := make([]byte, 32)
	read, err := file.ReadAt(buffer, 0)
	if err != nil && read == 0 {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(buffer[:read])))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

func releasePlatformForeignLock(file *os.File) {
	if file == nil {
		return
	}
	_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
	_ = file.Close()
}
