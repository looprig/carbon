//go:build !aix && !android && !darwin && !dragonfly && !freebsd && !hurd && !illumos && !ios && !linux && !netbsd && !openbsd && !solaris && !windows

package app

import "os"

// Platforms without a native no-follow primitive retain the Lstat/opened-Stat
// identity checks in readModelConfigFileWithOpen.
func openModelConfigNoFollow(path string) (*os.File, error) {
	return os.Open(path)
}
