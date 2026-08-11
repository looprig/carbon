//go:build !windows

package atomicfile

import "os"

// syncDir durably syncs a directory's entries by opening it and calling
// Sync, so a preceding rename's directory-entry update survives a crash.
// POSIX filesystems require an explicit fsync of the containing directory
// for a rename to be crash-durable; syncing the renamed file itself is not
// sufficient.
func syncDir(dir string) error {
	// #nosec G304 -- dir is Replace's own filepath.Dir(path), i.e. the fixed
	// resource-root directory a caller passed to Replace; it carries no
	// caller/Handle-derived path component of its own (only path's base
	// name does, and that never reaches this function), so there is no
	// variable, untrusted segment for a directory-traversal payload to ride
	// on here.
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
