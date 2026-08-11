//go:build darwin || linux

package local

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
	rootFD          int
	lockFD          int
	lockID          fileIdentity
	rootID          fileIdentity
	rootPath        string
	beforeReadFstat func(int) error
}

type fileIdentity struct {
	dev uint64
	ino uint64
}

func openPlatform(root string) (platform, error) {
	clean := filepath.Clean(root)
	if root == "" || !filepath.IsAbs(root) || root != clean {
		return nil, errInsecure
	}
	walkRoot := rootWalkPath(clean)
	fd, err := openRootDescriptor(walkRoot)
	if err != nil {
		return nil, err
	}
	rootStat := &unix.Stat_t{}
	if err := unix.Fstat(fd, rootStat); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	if !isOwnerOnlyDirectory(rootStat) {
		_ = unix.Close(fd)
		return nil, errInsecure
	}
	lockFD, err := unix.Openat(fd, lockName, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		_ = unix.Close(fd)
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.EISDIR) {
			return nil, errInsecure
		}
		return nil, err
	}
	lockStat := &unix.Stat_t{}
	if err := unix.Fstat(lockFD, lockStat); err != nil {
		_ = unix.Close(lockFD)
		_ = unix.Close(fd)
		return nil, err
	}
	if !isOwnerOnlyRegular(lockStat) {
		_ = unix.Close(lockFD)
		_ = unix.Close(fd)
		return nil, errInsecure
	}
	if lockStat.Size != 0 {
		_ = unix.Close(lockFD)
		_ = unix.Close(fd)
		return nil, errInsecure
	}
	return &unixPlatform{
		rootFD: fd, lockFD: lockFD,
		lockID: identity(lockStat), rootID: identity(rootStat), rootPath: clean,
	}, nil
}

func rootWalkPath(root string) string {
	// macOS exposes /var as a compatibility symlink to /private/var. Resolve
	// that platform-owned alias before the no-follow descriptor walk; all
	// caller-controlled components remain subject to O_NOFOLLOW.
	if runtime.GOOS == "darwin" && (root == "/var" || strings.HasPrefix(root, "/var/")) {
		return "/private" + root
	}
	return root
}

func openRootDescriptor(root string) (int, error) {
	return openRootDescriptorMode(root, true)
}

func openRootDescriptorNoCreate(root string) (int, error) {
	return openRootDescriptorMode(root, false)
}

func openRootDescriptorMode(root string, create bool) (int, error) {
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, err
	}
	components := strings.Split(strings.TrimPrefix(root, "/"), "/")
	if root == "/" {
		components = nil
	}
	for index, component := range components {
		if component == "" || component == "." || component == ".." {
			_ = unix.Close(fd)
			return -1, errInsecure
		}
		next, openErr := unix.Openat(fd, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if create && errors.Is(openErr, unix.ENOENT) {
			if mkdirErr := unix.Mkdirat(fd, component, 0o700); mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
				_ = unix.Close(fd)
				return -1, mkdirErr
			}
			next, openErr = unix.Openat(fd, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		}
		if openErr != nil {
			_ = unix.Close(fd)
			if errors.Is(openErr, unix.ELOOP) || errors.Is(openErr, unix.ENOTDIR) {
				return -1, errInsecure
			}
			return -1, openErr
		}
		if index == len(components)-1 {
			stat := &unix.Stat_t{}
			if statErr := unix.Fstat(next, stat); statErr != nil {
				_ = unix.Close(next)
				_ = unix.Close(fd)
				return -1, statErr
			}
			if !isOwnerOnlyDirectory(stat) {
				_ = unix.Close(next)
				_ = unix.Close(fd)
				return -1, errInsecure
			}
		}
		_ = unix.Close(fd)
		fd = next
	}
	if len(components) == 0 {
		_ = unix.Close(fd)
		return -1, errInsecure
	}
	return fd, nil
}

func (u *unixPlatform) close() error {
	var first error
	if u.lockFD >= 0 {
		if err := unix.Close(u.lockFD); err != nil && first == nil {
			first = err
		}
		u.lockFD = -1
	}
	if u.rootFD >= 0 {
		if err := unix.Close(u.rootFD); err != nil && first == nil {
			first = err
		}
		u.rootFD = -1
	}
	return first
}

func (u *unixPlatform) validateRoot() error {
	if u.rootFD < 0 {
		return errInsecure
	}
	st := &unix.Stat_t{}
	if err := unix.Fstat(u.rootFD, st); err != nil {
		return err
	}
	if !isOwnerOnlyDirectory(st) || identity(st) != u.rootID {
		return errInsecure
	}
	pathFD, err := openRootDescriptorNoCreate(rootWalkPath(u.rootPath))
	if err != nil {
		if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) {
			return errInsecure
		}
		return err
	}
	pathStat := &unix.Stat_t{}
	statErr := unix.Fstat(pathFD, pathStat)
	closeErr := unix.Close(pathFD)
	if statErr != nil {
		return statErr
	}
	if closeErr != nil {
		return closeErr
	}
	if !isOwnerOnlyDirectory(pathStat) || identity(pathStat) != u.rootID {
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

func (u *unixPlatform) validateLock() error {
	if u.lockFD < 0 || u.rootFD < 0 {
		return errInsecure
	}
	st := &unix.Stat_t{}
	if err := unix.Fstatat(u.rootFD, lockName, st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ELOOP) {
			return errInsecure
		}
		return err
	}
	if !isOwnerOnlyRegular(st) || st.Size != 0 || identity(st) != u.lockID {
		return errInsecure
	}
	return nil
}

func (u *unixPlatform) entry(name string) (fileState, error) {
	if !singleComponent(name) {
		return fileState{}, errInsecure
	}
	st := &unix.Stat_t{}
	if err := unix.Fstatat(u.rootFD, name, st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return fileState{}, nil
		}
		if errors.Is(err, unix.ELOOP) {
			return fileState{insecure: true}, nil
		}
		return fileState{}, err
	}
	if !isOwnerOnlyRegular(st) {
		return fileState{exists: true, insecure: true}, nil
	}
	return fileState{exists: true}, nil
}

func (u *unixPlatform) read(name string) ([]byte, error) {
	state, err := u.entry(name)
	if err != nil {
		return nil, err
	}
	if !state.exists {
		return nil, errAbsent
	}
	if state.insecure {
		return nil, errInsecure
	}
	fd, err := unix.Openat(u.rootFD, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, errAbsent
		}
		if errors.Is(err, unix.ELOOP) {
			return nil, errInsecure
		}
		return nil, err
	}
	closed := false
	defer func() {
		if !closed {
			_ = unix.Close(fd)
		}
	}()
	if u.beforeReadFstat != nil {
		if err := u.beforeReadFstat(fd); err != nil {
			return nil, err
		}
	}
	st := &unix.Stat_t{}
	if err := unix.Fstat(fd, st); err != nil {
		return nil, err
	}
	if !isOwnerOnlyRegular(st) {
		return nil, errInsecure
	}
	f := os.NewFile(uintptr(fd), name)
	if f == nil {
		return nil, errors.New("file handle unavailable")
	}
	defer func() {
		_ = f.Close()
		closed = true
	}()
	// LimitReader ensures an attacker cannot make a malformed record consume
	// unbounded memory before JSON validation.
	data, readErr := io.ReadAll(io.LimitReader(f, maxEnvelopeBytes+1))
	if readErr != nil {
		return nil, readErr
	}
	if len(data) > maxEnvelopeBytes {
		return nil, errOversized
	}
	return data, nil
}

func (u *unixPlatform) readPrefix(name string, limit int) ([]byte, error) {
	if limit <= 0 {
		return nil, errOversized
	}
	state, err := u.entry(name)
	if err != nil {
		return nil, err
	}
	if !state.exists {
		return nil, errAbsent
	}
	if state.insecure {
		return nil, errInsecure
	}
	fd, err := unix.Openat(u.rootFD, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, errAbsent
		}
		if errors.Is(err, unix.ELOOP) {
			return nil, errInsecure
		}
		return nil, err
	}
	closed := false
	defer func() {
		if !closed {
			_ = unix.Close(fd)
		}
	}()
	if u.beforeReadFstat != nil {
		if err := u.beforeReadFstat(fd); err != nil {
			return nil, err
		}
	}
	st := &unix.Stat_t{}
	if err := unix.Fstat(fd, st); err != nil {
		return nil, err
	}
	if !isOwnerOnlyRegular(st) {
		return nil, errInsecure
	}
	f := os.NewFile(uintptr(fd), name)
	if f == nil {
		return nil, errors.New("file handle unavailable")
	}
	defer func() {
		_ = f.Close()
		closed = true
	}()
	return io.ReadAll(io.LimitReader(f, int64(limit)))
}

func (u *unixPlatform) writeTemp(data []byte) (string, error) {
	for attempt := 0; attempt < 8; attempt++ {
		var suffix [12]byte
		if _, err := io.ReadFull(rand.Reader, suffix[:]); err != nil {
			return "", err
		}
		name := tempPrefix + hex.EncodeToString(suffix[:])
		if !singleComponent(name) {
			return "", errInsecure
		}
		fd, err := unix.Openat(u.rootFD, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			if errors.Is(err, unix.ELOOP) {
				return "", errInsecure
			}
			return "", err
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
		state, stateErr := u.entry(name)
		if stateErr != nil {
			_, _ = u.remove(name)
			return "", stateErr
		}
		if !state.exists || state.insecure {
			_, _ = u.remove(name)
			return "", errInsecure
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
	tempState, err := u.entry(temp)
	if err != nil {
		return err
	}
	if !tempState.exists || tempState.insecure {
		return errInsecure
	}
	targetState, err := u.entry(target)
	if err != nil {
		return err
	}
	if targetState.insecure {
		return errInsecure
	}
	return unix.Renameat(u.rootFD, temp, u.rootFD, target)
}

func (u *unixPlatform) remove(name string) (bool, error) {
	if !singleComponent(name) {
		return false, errInsecure
	}
	state, err := u.entry(name)
	if err != nil {
		return false, err
	}
	if !state.exists {
		return false, nil
	}
	if state.insecure {
		return false, errInsecure
	}
	if err := unix.Unlinkat(u.rootFD, name, 0); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (u *unixPlatform) names(limit int) ([]string, error) {
	// Open a fresh descriptor rather than duping rootFD: dup shares the
	// directory stream offset, so concurrent/paginated listings would lose
	// entries after the first read.
	fd, err := unix.Openat(u.rootFD, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), u.rootPath)
	if f == nil {
		_ = unix.Close(fd)
		return nil, errors.New("directory handle unavailable")
	}
	names, err := f.Readdirnames(limit + 1)
	_ = f.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(names) > limit {
		return nil, ErrListTooLarge
	}
	return names, nil
}

func (u *unixPlatform) syncDir() error { return unix.Fsync(u.rootFD) }

func identity(st *unix.Stat_t) fileIdentity {
	return fileIdentity{dev: uint64(st.Dev), ino: uint64(st.Ino)}
}

func isOwnerOnlyRegular(st *unix.Stat_t) bool {
	return st.Mode&unix.S_IFMT == unix.S_IFREG && uint32(st.Mode&0o7777) == 0o600 && uint32(st.Uid) == uint32(os.Getuid()) && uint64(st.Nlink) == 1
}

func isOwnerOnlyDirectory(st *unix.Stat_t) bool {
	return st.Mode&unix.S_IFMT == unix.S_IFDIR && uint32(st.Mode&0o7777) == 0o700 && uint32(st.Uid) == uint32(os.Getuid())
}
