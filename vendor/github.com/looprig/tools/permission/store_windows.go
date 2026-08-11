//go:build windows

package permission

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"golang.org/x/sys/windows"

	"github.com/looprig/tools/internal/nofollow"
)

// store_windows.go implements store.go's platform primitives for Windows:
// LockFileEx/UnlockFileEx for the interprocess lock, a
// reparse-point-refusing open (delegated to the shared internal/nofollow
// primitive, this package's own former FILE_FLAG_OPEN_REPARSE_POINT
// implementation and its documented faithfulness gaps now live there), and
// a SID/token owner comparison plus NumberOfLinks for the Stat_t-based
// owner/link-count identity checks. See store_unix.go for the POSIX
// counterparts and store.go for the shared logic that calls into these.
//
// None of this has been exercised against a real Windows filesystem or
// process token — it compiles and is a good-faith, invariant-preserving
// port, but real-machine validation belongs to the plan's Phase 5
// ("Unix PTY and Windows ConPTY") Windows-hardening pass. Faithfulness
// gaps against the Unix behavior are called out at each function below (and,
// for openNoFollow's mechanism, in internal/nofollow's own doc comments).

// openNoFollow opens path with the given flag/perm, refusing to traverse a
// reparse point (Windows's analogue of a symlink) at the final path
// component — the closest available equivalent of Unix's O_NOFOLLOW. It
// delegates the actual open to the shared internal/nofollow primitive (also
// used by grep/readfile/filemutation) and translates its exported
// ErrSymlinkNotAllowed into this package's own errSymlinkNotAllowed so
// store.go's shared classification keeps using its private sentinel.
func openNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	file, err := nofollow.Open(path, flag, perm)
	if err != nil {
		if errors.Is(err, nofollow.ErrSymlinkNotAllowed) {
			return nil, fmt.Errorf("%w: %w", errSymlinkNotAllowed, err)
		}
		return nil, err
	}
	return file, nil
}

// lockExclusiveNonBlocking takes an exclusive, non-blocking byte-range lock
// on f via LockFileEx: LOCKFILE_EXCLUSIVE_LOCK requests exclusive
// (write) access and LOCKFILE_FAIL_IMMEDIATELY makes the call return at
// once rather than block, mirroring flock(LOCK_EX|LOCK_NB)'s semantics. The
// lock covers the file's first byte only; that is the standard Windows
// idiom for emulating a whole-file advisory lock (matching, e.g., the
// approach used by cmd/go's internal lockedfile package and the widely used
// gofrs/flock package) — all lockers in this codebase go through this same
// function, so the convention is self-consistent.
func lockExclusiveNonBlocking(f *os.File) error {
	overlapped := new(windows.Overlapped)
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, overlapped)
	if err == nil {
		return nil
	}
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return errLockWouldBlock
	}
	return err
}

// unlockFile releases a lock taken by lockExclusiveNonBlocking. Callers
// treat it as best-effort: closing the handle also drops the lock even if
// the explicit unlock fails.
func unlockFile(f *os.File) error {
	overlapped := new(windows.Overlapped)
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, overlapped)
}

// checkFileIdentity enforces the owner and hard-link-count hardening
// checks for the permission file using the open handle: NumberOfLinks is a
// direct, faithful analogue of Stat_t.Nlink (NTFS hard links are real, and
// BY_HANDLE_FILE_INFORMATION reports the true link count). info is unused
// on this platform — Go's os.FileInfo.Sys() on Windows does not expose
// link count or owner, so both checks go through the handle instead.
func (s *Store) checkFileIdentity(file *os.File, info fs.FileInfo) error {
	var winInfo windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &winInfo); err != nil {
		return &FileError{Path: s.path, Reason: FileIO, Err: err}
	}
	if winInfo.NumberOfLinks != 1 {
		return &FileError{Path: s.path, Reason: FileLinkCount, Err: fmt.Errorf("link count %d, require 1", winInfo.NumberOfLinks)}
	}
	return s.checkOwnerHandle(windows.Handle(file.Fd()), "owner")
}

// checkDirIdentity enforces the owner hardening check for the store
// directory. store.go's checkDirectory (the only caller) has already taken
// an os.Lstat of directory before calling here, but that Lstat result
// carries no SID -- Go's os.FileInfo.Sys() on Windows exposes neither owner
// nor reparse-point state -- so, unlike checkFileIdentity (which reuses the
// single handle loadFile already opened via the shared openNoFollow/
// nofollow.Open primitive), there is no already-open handle from that Lstat
// to thread through here.
//
// nofollow.Open itself is not reused for this: its Windows implementation
// opens with plain CreateFile (no FILE_FLAG_BACKUP_SEMANTICS), which
// real Windows rejects for a directory target, and none of nofollow.Open's
// other callers (grep, readfile, filemutation, and this package's own file
// checks) open directories, so extending it purely for this one directory
// check was judged out of proportion to a Minor finding.
//
// To still narrow the TOCTOU the previous by-path GetNamedSecurityInfo call
// had (it re-resolved the directory independently of the Lstat that already
// inspected it, so the object GetNamedSecurityInfo actually read could in
// principle have been swapped between the two calls), this function opens
// its own no-follow handle to directory -- CreateFile with
// FILE_FLAG_OPEN_REPARSE_POINT (refuse to transparently traverse a reparse
// point at the final component, exactly like nofollow.Open's file-open
// mechanism) and FILE_FLAG_BACKUP_SEMANTICS (required to open a directory at
// all) -- and re-verifies, from that SAME handle, that the object is still a
// plain directory and not a reparse point before fetching its owner SID
// from that SAME handle via GetSecurityInfo, rather than
// GetNamedSecurityInfo(path). This closes the second window (identity
// re-check vs. SID fetch: both now come from one handle, exactly like the
// file case). It does not close the first, inherent gap between store.go's
// own os.Lstat and this function's CreateFile call: no portable Windows API
// resolves identity and opens a handle atomically from a bare path, and this
// narrower remaining gap is symmetric with what the file path's own first
// open (loadFile's openNoFollow) already accepts as its starting point.
func (s *Store) checkDirIdentity(directory string, info fs.FileInfo) error {
	handle, err := openDirNoFollow(directory)
	if err != nil {
		return &FileError{Path: s.path, Reason: FileIO, Err: err}
	}
	defer func() { _ = windows.CloseHandle(handle) }()

	var winInfo windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &winInfo); err != nil {
		return &FileError{Path: s.path, Reason: FileIO, Err: err}
	}
	if winInfo.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return &FileError{Path: s.path, Reason: FileSymlink, Err: errors.New("store directory is a reparse point")}
	}
	if winInfo.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return &FileError{Path: s.path, Reason: FileNotRegular, Err: errors.New("store directory path is not a directory")}
	}

	return s.checkOwnerHandle(handle, "store directory owner")
}

// openDirNoFollow opens directory as a no-follow directory handle:
// FILE_FLAG_BACKUP_SEMANTICS is required by CreateFile to open a directory
// at all, and FILE_FLAG_OPEN_REPARSE_POINT refuses to transparently traverse
// a reparse point at the final path component -- the same
// refuse-rather-than-follow mechanism internal/nofollow.Open uses for files
// (see checkDirIdentity's doc comment for why that shared primitive is not
// reused directly here).
func openDirNoFollow(directory string) (windows.Handle, error) {
	pathPtr, err := windows.UTF16PtrFromString(directory)
	if err != nil {
		return 0, err
	}
	shareMode := uint32(windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE)
	attrs := uint32(windows.FILE_FLAG_BACKUP_SEMANTICS | windows.FILE_FLAG_OPEN_REPARSE_POINT)
	return windows.CreateFile(pathPtr, windows.GENERIC_READ, shareMode, nil, windows.OPEN_EXISTING, attrs, 0)
}

// checkOwnerHandle resolves the SID comparison from an already-open handle
// (used by both checkFileIdentity's file handle and checkDirIdentity's
// directory handle above).
func (s *Store) checkOwnerHandle(handle windows.Handle, label string) error {
	sd, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return &FileError{Path: s.path, Reason: FileIO, Err: err}
	}
	return s.compareOwnerSID(sd, label)
}

// compareOwnerSID is the Windows analogue of the Unix "stat.Uid != s.euid"
// check: Windows has no UID, so the equivalent identity comparison is
// between the object's owner SID (from its security descriptor) and the
// current process token's user SID.
//
// Faithfulness gap: Unix's euid is captured once at Store construction
// (os.Geteuid(), cached in s.euid) so a later privilege change on the
// process doesn't move the goalposts mid-comparison; this Windows check
// re-reads the current process token's user SID on every call instead,
// since os.Geteuid() always returns -1 on Windows and s.euid carries no
// meaningful value there. A process whose token user changes between
// checks would see different results than the cached-euid Unix behavior.
// This is expected to be effectively unobservable in practice (a process's
// primary token user does not normally change post-creation) but the
// Phase 5 pass should confirm that assumption holds for how this store is
// actually run on Windows.
func (s *Store) compareOwnerSID(sd *windows.SECURITY_DESCRIPTOR, label string) error {
	owner, _, err := sd.Owner()
	if err != nil {
		return &FileError{Path: s.path, Reason: FileIO, Err: err}
	}
	tokenUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return &FileError{Path: s.path, Reason: FileIO, Err: err}
	}
	if !owner.Equals(tokenUser.User.Sid) {
		return &FileError{Path: s.path, Reason: FileOwnerUnexpected, Err: fmt.Errorf("%s SID %s, require %s", label, owner.String(), tokenUser.User.Sid.String())}
	}
	return nil
}
