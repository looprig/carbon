//go:build (!darwin && !linux) || android || ios

package backend

import (
	"errors"
	"os"
)

var errLockUnsupportedPlatform = errors.New("foreignloop: process locking unsupported on this platform")

func acquirePlatformForeignLock(path, _, _ string) (*foreignLock, error) {
	return nil, &LockError{Op: "platform", Path: path, Cause: errLockUnsupportedPlatform}
}

func releasePlatformForeignLock(file *os.File) {
	if file != nil {
		_ = file.Close()
	}
}
