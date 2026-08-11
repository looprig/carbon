// Package nofollow provides one small, platform-portable primitive for
// opening a file while refusing to traverse a symlink (POSIX) or reparse
// point (Windows) at the final path component. It centralizes the
// FILE_FLAG_OPEN_REPARSE_POINT workaround permission/store_windows.go's
// openNoFollow pioneered for Windows (CreateFile has no direct
// O_NOFOLLOW-at-open-time equivalent, so the reparse point is opened as
// itself and refused after the fact rather than transparently followed),
// generalized to the fuller os.OpenFile flag vocabulary (read, write,
// create, exclusive-create, truncate) so grep, readfile, and filemutation
// do not each reimplement the platform split.
//
// Open is a drop-in analogue of os.OpenFile(path, flag|syscall.O_NOFOLLOW,
// perm) on POSIX targets; see open_windows.go for the Windows mechanism and
// its documented faithfulness gaps against that POSIX behavior.
package nofollow

import "errors"

// ErrSymlinkNotAllowed is wrapped into the error Open returns when path's
// final component is a symlink (POSIX) or reparse point (Windows) — the
// caller asked for a no-follow open and the target could not be opened
// without traversing one. Callers classify this with errors.Is rather than
// a platform errno (POSIX ELOOP has no Windows analogue). The permission
// package wraps this again behind its own private errSymlinkNotAllowed
// sentinel so its internal classification in store.go stays independent of
// this package's exported error identity.
var ErrSymlinkNotAllowed = errors.New("nofollow: refusing to open a symlinked/reparse-point path")
