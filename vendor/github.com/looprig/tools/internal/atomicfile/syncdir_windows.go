//go:build windows

package atomicfile

// syncDir is a no-op on Windows. NTFS does not expose (or require) an
// explicit directory-entry fsync the way POSIX filesystems do for rename
// durability; directory-metadata durability is handled by NTFS's own
// transaction log.
func syncDir(dir string) error {
	return nil
}
