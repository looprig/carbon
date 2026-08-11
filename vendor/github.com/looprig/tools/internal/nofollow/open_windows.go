//go:build windows

package nofollow

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"golang.org/x/sys/windows"
)

// Open opens path with the given flag/perm, refusing to traverse a reparse
// point (Windows's analogue of a symlink) at the final path component —
// the closest available equivalent of Unix's O_NOFOLLOW. This is the same
// mechanism permission/store_windows.go's openNoFollow pioneered, factored
// out and generalized to the fuller os.OpenFile flag vocabulary (O_EXCL,
// O_TRUNC) the grep/readfile/filemutation call sites need in addition to
// the plain read/open-or-create case the permission store itself uses (and
// now delegates to this function).
//
// Faithfulness gap: unlike O_NOFOLLOW, CreateFile has no flag that fails
// the open outright when the target is a reparse point.
// FILE_FLAG_OPEN_REPARSE_POINT instead succeeds and hands back a handle to
// the reparse point object itself (rather than transparently following it
// to its target, which is the unsafe behavior being defended against). This
// function closes that gap itself: it inspects the resulting handle's
// FILE_ATTRIBUTE_REPARSE_POINT bit via GetFileInformationByHandle and
// refuses (wrapping ErrSymlinkNotAllowed) before the handle is ever
// returned to a caller, so the net effect matches O_NOFOLLOW — traversal is
// refused, not silently allowed — even though the mechanism differs. One
// known imprecision: this refuses *any* reparse point (symlink, junction,
// mount point, or a third-party filesystem filter's reparse tag) rather
// than symlinks specifically; that is deliberately conservative (fail
// closed) rather than under-protective.
//
// perm is accepted for API symmetry with os.OpenFile/POSIX Open but is
// otherwise unused: Windows ACLs are not the POSIX permission-bits model,
// and CreateFile has no direct equivalent parameter for a plain file
// create (the file inherits its parent directory's ACL).
//
// None of this has been exercised against a real Windows filesystem — see
// permission/store_windows.go's package comment for that same caveat,
// which applies equally here; real-machine validation belongs to whichever
// phase first runs this codebase's Windows-hardening pass.
func Open(path string, flag int, perm os.FileMode) (*os.File, error) {
	var access uint32
	switch flag & (os.O_RDONLY | os.O_WRONLY | os.O_RDWR) {
	case os.O_WRONLY:
		access = windows.GENERIC_WRITE
	case os.O_RDWR:
		access = windows.GENERIC_READ | windows.GENERIC_WRITE
	default:
		access = windows.GENERIC_READ
	}
	if flag&os.O_CREATE != 0 {
		access |= windows.GENERIC_WRITE
	}

	// createDisposition mirrors the standard POSIX-open-flag -> Windows
	// CreateFile-disposition mapping (the same mapping Go's own os/syscall
	// package uses internally on Windows): O_CREATE|O_EXCL requires a
	// brand-new file (CREATE_NEW fails if one already exists — the
	// no-clobber guarantee filemutation's stageTempFile depends on),
	// O_CREATE|O_TRUNC creates-or-truncates, plain O_CREATE may
	// open-or-create without truncating (OPEN_ALWAYS), O_TRUNC alone
	// truncates an existing file, and no creation flags requires the file
	// to already exist (OPEN_EXISTING).
	var createDisposition uint32
	switch {
	case flag&os.O_CREATE != 0 && flag&os.O_EXCL != 0:
		createDisposition = windows.CREATE_NEW
	case flag&os.O_CREATE != 0 && flag&os.O_TRUNC != 0:
		createDisposition = windows.CREATE_ALWAYS
	case flag&os.O_CREATE != 0:
		createDisposition = windows.OPEN_ALWAYS
	case flag&os.O_TRUNC != 0:
		createDisposition = windows.TRUNCATE_EXISTING
	default:
		createDisposition = windows.OPEN_EXISTING
	}

	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	shareMode := uint32(windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE)
	// FILE_FLAG_OPEN_REPARSE_POINT: open the reparse point object itself
	// instead of transparently following it to its target.
	attrs := uint32(windows.FILE_ATTRIBUTE_NORMAL | windows.FILE_FLAG_OPEN_REPARSE_POINT)
	handle, err := windows.CreateFile(pathPtr, access, shareMode, nil, createDisposition, attrs, 0)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return nil, fmt.Errorf("%w: %w", fs.ErrNotExist, err)
		}
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)

	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		_ = file.Close()
		return nil, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		_ = file.Close()
		return nil, fmt.Errorf("%w: refusing reparse point at %s", ErrSymlinkNotAllowed, path)
	}
	return file, nil
}
