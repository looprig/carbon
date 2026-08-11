//go:build darwin || linux

package catalog

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

type unixPlatform struct {
	rootFD       int
	lockFD       int
	rootPath     string
	rootWalkPath string
	rootID       identity
	lockID       identity
	// beforeReadOpen is a test-only race-injection seam. It runs after the
	// pre-open identity check and before Openat so read can prove that the
	// descriptor it actually opened is still the checked file.
	beforeReadOpen func() error
}

type identity struct{ dev, ino uint64 }

func openPlatform(root string) (platform, error) {
	clean := filepath.Clean(root)
	if root == "" || !filepath.IsAbs(root) || root != clean {
		return nil, errInsecure
	}
	walkRoot := clean
	if runtime.GOOS == "darwin" && (walkRoot == "/var" || strings.HasPrefix(walkRoot, "/var/")) {
		walkRoot = "/private" + walkRoot
	}
	rootFD, err := openRoot(walkRoot)
	if err != nil {
		return nil, err
	}
	rootStat := &unix.Stat_t{}
	if err := unix.Fstat(rootFD, rootStat); err != nil {
		_ = unix.Close(rootFD)
		return nil, err
	}
	if !ownerDir(rootStat) {
		_ = unix.Close(rootFD)
		return nil, errInsecure
	}
	lockFD, err := unix.Openat(rootFD, LockFilename, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		_ = unix.Close(rootFD)
		return nil, mapOpenErr(err)
	}
	lockStat := &unix.Stat_t{}
	if err := unix.Fstat(lockFD, lockStat); err != nil {
		_ = unix.Close(lockFD)
		_ = unix.Close(rootFD)
		return nil, err
	}
	if !ownerRegular(lockStat) || lockStat.Size != 0 {
		_ = unix.Close(lockFD)
		_ = unix.Close(rootFD)
		return nil, errInsecure
	}
	return &unixPlatform{rootFD: rootFD, lockFD: lockFD, rootPath: clean, rootWalkPath: walkRoot, rootID: fileID(rootStat), lockID: fileID(lockStat)}, nil
}

func openRoot(root string) (int, error) {
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, err
	}
	parts := strings.Split(strings.TrimPrefix(root, "/"), "/")
	if root == "/" || len(parts) == 0 {
		_ = unix.Close(fd)
		return -1, errInsecure
	}
	for index, part := range parts {
		if !singleComponent(part) {
			_ = unix.Close(fd)
			return -1, errInsecure
		}
		next, openErr := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if errors.Is(openErr, unix.ENOENT) {
			if mkdirErr := unix.Mkdirat(fd, part, 0o700); mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
				_ = unix.Close(fd)
				return -1, mkdirErr
			}
			next, openErr = unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		}
		if openErr != nil {
			_ = unix.Close(fd)
			return -1, mapOpenErr(openErr)
		}
		if index == len(parts)-1 {
			st := &unix.Stat_t{}
			if err := unix.Fstat(next, st); err != nil {
				_ = unix.Close(next)
				_ = unix.Close(fd)
				return -1, err
			}
			if !ownerDir(st) {
				_ = unix.Close(next)
				_ = unix.Close(fd)
				return -1, errInsecure
			}
		}
		_ = unix.Close(fd)
		fd = next
	}
	return fd, nil
}

func (u *unixPlatform) root() string { return u.rootPath }

// ensureData must run while the catalog lock is held. It reports true only
// when this initializer won O_EXCL creation; an existing zero-length file is
// therefore distinguishable and remains corruption.
func (u *unixPlatform) ensureData() (bool, error) {
	if u == nil || u.rootFD < 0 {
		return false, errInsecure
	}
	dataFD, err := unix.Openat(u.rootFD, Filename, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	created := err == nil
	if errors.Is(err, unix.EEXIST) {
		dataFD, err = unix.Openat(u.rootFD, Filename, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	}
	if err != nil {
		return false, mapOpenErr(err)
	}
	dataStat := &unix.Stat_t{}
	if err := unix.Fstat(dataFD, dataStat); err != nil {
		_ = unix.Close(dataFD)
		return false, err
	}
	if !ownerRegular(dataStat) {
		_ = unix.Close(dataFD)
		return false, errInsecure
	}
	if err := unix.Close(dataFD); err != nil {
		return false, err
	}
	return created, nil
}

func (u *unixPlatform) close() error {
	var first error
	if u.lockFD >= 0 {
		if err := unix.Close(u.lockFD); err != nil {
			first = err
		}
		u.lockFD = -1
	}
	if u.rootFD >= 0 {
		if err := unix.Close(u.rootFD); first == nil && err != nil {
			first = err
		}
		u.rootFD = -1
	}
	return first
}

func (u *unixPlatform) validateRoot() error {
	if u == nil || u.rootFD < 0 {
		return errInsecure
	}
	st := &unix.Stat_t{}
	if err := unix.Fstat(u.rootFD, st); err != nil {
		return err
	}
	if !ownerDir(st) || fileID(st) != u.rootID {
		return errInsecure
	}
	reopened, err := openExistingRoot(u.rootWalkPath)
	if err != nil {
		return errInsecure
	}
	defer unix.Close(reopened)
	pathStat := &unix.Stat_t{}
	if err := unix.Fstat(reopened, pathStat); err != nil {
		return errInsecure
	}
	if !ownerDir(pathStat) || fileID(pathStat) != u.rootID {
		return errInsecure
	}
	return nil
}

// openExistingRoot reopens every configured path component with O_NOFOLLOW.
// It never creates missing components, so validation cannot silently switch
// to a recreated path or manufacture a new directory hierarchy.
func openExistingRoot(root string) (int, error) {
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, err
	}
	parts := strings.Split(strings.TrimPrefix(root, "/"), "/")
	if root == "/" || len(parts) == 0 {
		_ = unix.Close(fd)
		return -1, errInsecure
	}
	for _, part := range parts {
		if !singleComponent(part) {
			_ = unix.Close(fd)
			return -1, errInsecure
		}
		next, openErr := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if openErr != nil {
			_ = unix.Close(fd)
			return -1, mapOpenErr(openErr)
		}
		_ = unix.Close(fd)
		fd = next
	}
	return fd, nil
}

func (u *unixPlatform) validateLock() error {
	if u == nil || u.rootFD < 0 || u.lockFD < 0 {
		return errInsecure
	}
	st := &unix.Stat_t{}
	if err := unix.Fstatat(u.rootFD, LockFilename, st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return mapOpenErr(err)
	}
	if !ownerRegular(st) || st.Size != 0 || fileID(st) != u.lockID {
		return errInsecure
	}
	return nil
}

func (u *unixPlatform) lock(ctx context.Context) (func(), error) {
	if err := u.validateLock(); err != nil {
		return nil, err
	}
	for {
		err := unix.Flock(u.lockFD, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			if err := u.validateLock(); err != nil {
				_ = unix.Flock(u.lockFD, unix.LOCK_UN)
				return nil, err
			}
			return func() { _ = unix.Flock(u.lockFD, unix.LOCK_UN) }, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return nil, err
		}
		if ctx == nil {
			return nil, context.Canceled
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (u *unixPlatform) read(name string) ([]byte, error) {
	if !singleComponent(name) {
		return nil, errInsecure
	}
	st := &unix.Stat_t{}
	if err := unix.Fstatat(u.rootFD, name, st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, errAbsent
		}
		return nil, mapOpenErr(err)
	}
	if !ownerRegular(st) {
		return nil, errInsecure
	}
	if u.beforeReadOpen != nil {
		if err := u.beforeReadOpen(); err != nil {
			return nil, err
		}
	}
	fd, err := unix.Openat(u.rootFD, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, mapOpenErr(err)
	}
	f := os.NewFile(uintptr(fd), name)
	if f == nil {
		_ = unix.Close(fd)
		return nil, errors.New("file handle unavailable")
	}
	defer f.Close()
	opened := &unix.Stat_t{}
	if err := unix.Fstat(fd, opened); err != nil {
		return nil, err
	}
	if !ownerRegular(opened) || fileID(opened) != fileID(st) {
		return nil, errInsecure
	}
	data, err := io.ReadAll(io.LimitReader(f, maxCatalogBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxCatalogBytes {
		return nil, errOversize
	}
	return data, nil
}

func (u *unixPlatform) writeTemp(data []byte) (string, error) {
	for attempt := 0; attempt < 8; attempt++ {
		var suffix [12]byte
		if _, err := io.ReadFull(rand.Reader, suffix[:]); err != nil {
			return "", err
		}
		name := tempPrefix + hex.EncodeToString(suffix[:])
		fd, err := unix.Openat(u.rootFD, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return "", mapOpenErr(err)
		}
		writeErr := writeAll(fd, data)
		if writeErr == nil {
			writeErr = unix.Fsync(fd)
		}
		closeErr := unix.Close(fd)
		if writeErr != nil {
			_, _ = u.remove(name)
			return "", writeErr
		}
		if closeErr != nil {
			_, _ = u.remove(name)
			return "", closeErr
		}
		return name, nil
	}
	return "", errors.New("temporary name collision")
}

func writeAll(fd int, data []byte) error {
	for len(data) > 0 {
		n, err := unix.Write(fd, data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func (u *unixPlatform) rename(temp, target string) error {
	if !singleComponent(temp) || !singleComponent(target) {
		return errInsecure
	}
	for _, name := range []string{temp, target} {
		st := &unix.Stat_t{}
		err := unix.Fstatat(u.rootFD, name, st, unix.AT_SYMLINK_NOFOLLOW)
		if errors.Is(err, unix.ENOENT) && name == target {
			continue
		}
		if err != nil {
			return mapOpenErr(err)
		}
		if !ownerRegular(st) {
			return errInsecure
		}
	}
	if err := unix.Renameat(u.rootFD, temp, u.rootFD, target); err != nil {
		return mapOpenErr(err)
	}
	return nil
}

func (u *unixPlatform) remove(name string) (bool, error) {
	if !singleComponent(name) {
		return false, errInsecure
	}
	st := &unix.Stat_t{}
	if err := unix.Fstatat(u.rootFD, name, st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return false, nil
		}
		return false, mapOpenErr(err)
	}
	if !ownerRegular(st) {
		return false, errInsecure
	}
	if err := unix.Unlinkat(u.rootFD, name, 0); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return false, nil
		}
		return false, mapOpenErr(err)
	}
	return true, nil
}

func (u *unixPlatform) syncDir() error { return unix.Fsync(u.rootFD) }

func mapOpenErr(err error) error {
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.EISDIR) {
		return errInsecure
	}
	return err
}

func fileID(st *unix.Stat_t) identity { return identity{dev: uint64(st.Dev), ino: uint64(st.Ino)} }

func ownerRegular(st *unix.Stat_t) bool {
	return st.Mode&unix.S_IFMT == unix.S_IFREG && uint32(st.Mode&0o7777) == 0o600 && uint32(st.Uid) == uint32(os.Getuid()) && uint64(st.Nlink) == 1
}

func ownerDir(st *unix.Stat_t) bool {
	return st.Mode&unix.S_IFMT == unix.S_IFDIR && uint32(st.Mode&0o7777) == 0o700 && uint32(st.Uid) == uint32(os.Getuid())
}

func singleComponent(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\\")
}
