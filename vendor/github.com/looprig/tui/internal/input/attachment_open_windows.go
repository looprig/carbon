//go:build windows

package input

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// openNoFollow opens clean read-only, refusing to traverse a reparse point
// (Windows's analogue of a symlink) at the final path component — the
// closest available equivalent of POSIX O_NOFOLLOW, which does not exist as
// a CreateFile flag. syscall.CreateFile has no option that fails the open
// outright when the target is a reparse point: FILE_FLAG_OPEN_REPARSE_POINT
// instead succeeds and hands back a handle to the reparse point object
// itself (rather than transparently following it to its target, which is
// the unsafe behavior being defended against). This function closes that
// gap itself: it inspects the resulting handle's FILE_ATTRIBUTE_REPARSE_POINT
// bit via GetFileInformationByHandle and refuses — wrapping
// errAttachmentSymlink, and closing the handle first — before it is ever
// returned to the caller, so the net effect matches O_NOFOLLOW (traversal
// refused, not silently allowed) even though the mechanism differs. This
// refuses *any* reparse point (symlink, junction, mount point, or a
// third-party filesystem filter's reparse tag) rather than symlinks
// specifically, which is deliberately conservative (fail closed) rather
// than under-protective.
//
// There is no Windows counterpart to the POSIX side's O_NONBLOCK: CreateFile
// against an ordinary filesystem path never blocks waiting for a reader/
// writer to connect the way opening a POSIX FIFO can. Windows named pipes
// that could block a connecting client live in a separate \\.\pipe\
// namespace, not the plain filesystem path space this attachment path is
// drawn from, so there is nothing to opt out of here. readAttachment's
// IsRegular check after this call remains the backstop against any
// non-regular file this open could otherwise return.
//
// This entire mechanism mirrors the one github.com/looprig/tools' internal
// nofollow package uses for the identical class of Windows build failure
// (syscall.O_NOFOLLOW undefined on Windows); it is reimplemented locally
// here, scoped to exactly the read-only open readAttachment needs, using
// only the standard library's syscall package (no golang.org/x/sys
// dependency, unlike that package's fuller read/write/create variant) so
// this module adds no new dependency to open one file.
func openNoFollow(clean string) (*os.File, error) {
	pathPtr, err := syscall.UTF16PtrFromString(clean)
	if err != nil {
		return nil, err
	}

	const shareMode = syscall.FILE_SHARE_READ | syscall.FILE_SHARE_WRITE
	// FILE_FLAG_OPEN_REPARSE_POINT: open the reparse point object itself
	// instead of transparently following it to its target.
	const attrs = syscall.FILE_ATTRIBUTE_NORMAL | syscall.FILE_FLAG_OPEN_REPARSE_POINT
	handle, err := syscall.CreateFile(pathPtr, syscall.GENERIC_READ, shareMode, nil, syscall.OPEN_EXISTING, attrs, 0)
	if err != nil {
		if errors.Is(err, syscall.ERROR_FILE_NOT_FOUND) || errors.Is(err, syscall.ERROR_PATH_NOT_FOUND) {
			return nil, fmt.Errorf("%w: %w", os.ErrNotExist, err)
		}
		return nil, err
	}
	f := os.NewFile(uintptr(handle), clean)

	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(handle, &info); err != nil {
		_ = f.Close()
		return nil, err
	}
	if info.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		_ = f.Close()
		return nil, fmt.Errorf("%w: refusing reparse point at %s", errAttachmentSymlink, clean)
	}
	return f, nil
}
