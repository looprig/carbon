//go:build windows

package sessionruntime

import (
	"errors"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

func createPrivateSessionResourceRoot(root string) error {
	if _, err := os.Lstat(root); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	existing := filepath.Clean(root)
	var suffix []string
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return os.ErrNotExist
		}
		suffix = append(suffix, filepath.Base(existing))
		existing = parent
	}

	descriptor, err := privateWindowsSecurityDescriptor(true)
	if err != nil {
		return err
	}
	attributes := &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: descriptor,
	}
	for i := len(suffix) - 1; i >= 0; i-- {
		existing = filepath.Join(existing, suffix[i])
		path, err := windows.UTF16PtrFromString(existing)
		if err != nil {
			return err
		}
		if err := windows.CreateDirectory(path, attributes); err != nil &&
			!errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			return err
		}
	}
	return nil
}

func sessionResourcePathIsPrivate(path string, info os.FileInfo, directory bool) (bool, error) {
	if info.Mode()&os.ModeSymlink != 0 {
		return false, nil
	}
	if directory {
		if !info.IsDir() {
			return false, nil
		}
	} else if !info.Mode().IsRegular() {
		return false, nil
	}

	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return false, err
	}
	descriptor, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return false, err
	}
	if descriptor == nil {
		return false, nil
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return false, err
	}
	if owner == nil || !owner.Equals(user.User.Sid) {
		return false, nil
	}
	control, _, err := descriptor.Control()
	if err != nil {
		return false, err
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		return false, nil
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return false, err
	}
	if dacl == nil || dacl.AceCount != 1 {
		return false, nil
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, 0, &ace); err != nil {
		return false, err
	}
	if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE ||
		ace.Mask&windows.GENERIC_ALL == 0 ||
		!sessionResourceWindowsACEFlagsArePrivate(ace.Header.AceFlags, directory) {
		return false, nil
	}
	trustee := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	return trustee.Equals(user.User.Sid), nil
}

func protectSessionResourceFile(path string, _ *os.File) error {
	descriptor, err := privateWindowsSecurityDescriptor(false)
	if err != nil {
		return err
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|
			windows.DACL_SECURITY_INFORMATION|
			windows.PROTECTED_DACL_SECURITY_INFORMATION,
		user.User.Sid,
		nil,
		dacl,
		nil,
	)
}

func privateWindowsSecurityDescriptor(directory bool) (*windows.SECURITY_DESCRIPTOR, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	inheritance := ""
	if directory {
		inheritance = "OICI"
	}
	return windows.SecurityDescriptorFromString(
		"O:" + user.User.Sid.String() + "D:P(A;" + inheritance + ";GA;;;" + user.User.Sid.String() + ")",
	)
}

func commitSessionResourceAnchor(temporaryPath, anchorPath string) error {
	temporary, err := windows.UTF16PtrFromString(temporaryPath)
	if err != nil {
		return err
	}
	anchor, err := windows.UTF16PtrFromString(anchorPath)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(temporary, anchor, windows.MOVEFILE_WRITE_THROUGH)
}

func syncSessionResourceDirectory(root *os.Root) error {
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	// FlushFileBuffers is not supported for directory handles on every Windows
	// filesystem. The anchor move itself uses MOVEFILE_WRITE_THROUGH; these
	// documented directory-handle results therefore mean no additional flush
	// primitive is available.
	if syncErr != nil &&
		!errors.Is(syncErr, windows.ERROR_ACCESS_DENIED) &&
		!errors.Is(syncErr, windows.ERROR_INVALID_FUNCTION) &&
		!errors.Is(syncErr, windows.ERROR_INVALID_HANDLE) {
		return syncErr
	}
	return closeErr
}
