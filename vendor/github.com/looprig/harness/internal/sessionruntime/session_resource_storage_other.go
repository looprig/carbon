//go:build !windows

package sessionruntime

import (
	"os"
)

func createPrivateSessionResourceRoot(root string) error {
	return os.MkdirAll(root, 0o700)
}

func sessionResourcePathIsPrivate(_ string, info os.FileInfo, directory bool) (bool, error) {
	if info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return false, nil
	}
	if directory {
		return info.IsDir(), nil
	}
	return info.Mode().IsRegular(), nil
}

func protectSessionResourceFile(_ string, file *os.File) error {
	return file.Chmod(0o600)
}

func commitSessionResourceAnchor(temporaryPath, anchorPath string) error {
	return os.Link(temporaryPath, anchorPath)
}

func syncSessionResourceDirectory(root *os.Root) error {
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}
