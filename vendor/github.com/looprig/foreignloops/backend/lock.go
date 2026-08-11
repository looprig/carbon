package backend

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	lockDirectoryName          = "looprig-foreign"
	durableForeignLockPrefix   = "durable-"
	temporaryForeignLockPrefix = "temporary-"
)

// foreignLock owns one kernel lock through its open file description. The
// stable lock file is never unlinked during normal operation: ownership ends
// only when release closes the locked descriptor or the process exits.
type foreignLock struct {
	path        string
	file        *os.File
	releaseOnce sync.Once
}

func lockRootPath() string {
	return filepath.Clean(filepath.Join(os.TempDir(), lockDirectoryName))
}

func temporaryForeignLockPath(loopID, cwd string) string {
	return foreignLockPathFor(temporaryForeignLockPrefix, loopID, cwd)
}

func foreignLockPath(sid, cwd string) string {
	return foreignLockPathFor(durableForeignLockPrefix, sid, cwd)
}

// foreignLockPathFor hashes the complete opaque identifier and cleaned
// workspace path. Neither value can add a path separator or grow the filename.
func foreignLockPathFor(namespace, identifier, cwd string) string {
	filename := namespace + digest(filepath.Clean(cwd)) + "-" + digest(identifier) + ".lock"
	return filepath.Join(lockRootPath(), filename)
}

func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func validateForeignLockPath(path string) error {
	root := lockRootPath()
	cleaned := filepath.Clean(path)
	relative, err := filepath.Rel(root, cleaned)
	if err != nil {
		return err
	}
	if relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.Dir(cleaned) != root {
		return fmt.Errorf("path %q is outside lock root %q", cleaned, root)
	}
	if filepath.Base(cleaned) != relative {
		return fmt.Errorf("path %q is not an immediate lock-root child", cleaned)
	}
	return nil
}

func acquireForeignLock(sid, cwd string) (*foreignLock, error) {
	return acquireForeignLockPath(foreignLockPath(sid, cwd), sid, cwd)
}

func acquireTemporaryForeignLock(loopID, cwd string) (*foreignLock, error) {
	return acquireForeignLockPath(
		temporaryForeignLockPath(loopID, cwd),
		temporaryForeignLockPrefix+loopID,
		cwd,
	)
}

func acquireForeignLockPath(path, sid, cwd string) (*foreignLock, error) {
	if err := validateForeignLockPath(path); err != nil {
		return nil, &LockError{Op: "validate", Path: path, Cause: err}
	}
	return acquirePlatformForeignLock(path, sid, cwd)
}

func (lock *foreignLock) release() {
	if lock == nil {
		return
	}
	lock.releaseOnce.Do(func() {
		releasePlatformForeignLock(lock.file)
	})
}
