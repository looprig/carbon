package app

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
)

// readHygienicConfigFile applies the file-hygiene discipline shared by every
// secret-bearing looprig config file loaded from disk at the composition
// boundary (models.json, mcp.json, ...): an absent file is not an error
// (exists=false, nil), the final path component is never followed if it is a
// symlink, the file must be a regular file, on Unix it must be owner-only
// (mode & 0o077 == 0) both before and after opening, the opened handle is
// re-checked against the pre-open Lstat to defeat a TOCTOU replacement, and
// the content is capped at maxBytes.
//
// openFile performs the actual, no-follow-safe open (see
// openModelConfigNoFollow and its platform variants -- despite the name,
// they are generic and used by every caller of this function). wrapErr
// builds the caller's own typed error (*ModelConfigError, *MCPConfigError,
// ...) from a short operation label (e.g. "inspect "+path) and the
// underlying cause, so each config family keeps its own error type while
// sharing this exact hygiene logic.
func readHygienicConfigFile(path string, maxBytes int64, openFile func(string) (*os.File, error), wrapErr func(op string, cause error) error) ([]byte, bool, error) {
	beforeOpen, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, wrapErr("inspect "+path, err)
	}
	if beforeOpen.Mode()&os.ModeSymlink != 0 {
		return nil, true, wrapErr("validate "+path, errors.New("symbolic links are not allowed"))
	}
	if !beforeOpen.Mode().IsRegular() {
		return nil, true, wrapErr("validate "+path, errors.New("file is not regular"))
	}
	if modelConfigIsUnix() && beforeOpen.Mode().Perm()&0o077 != 0 {
		return nil, true, wrapErr("validate "+path, fmt.Errorf("permissions %04o allow group or other access", beforeOpen.Mode().Perm()))
	}

	file, err := openFile(path)
	if err != nil {
		return nil, true, wrapErr("open "+path, err)
	}
	defer file.Close()

	afterOpen, err := file.Stat()
	if err != nil {
		return nil, true, wrapErr("inspect opened file "+path, err)
	}
	if !afterOpen.Mode().IsRegular() || !os.SameFile(beforeOpen, afterOpen) {
		return nil, true, wrapErr("validate opened file "+path, errors.New("file type or identity changed while opening"))
	}
	if modelConfigIsUnix() && afterOpen.Mode().Perm()&0o077 != 0 {
		return nil, true, wrapErr("validate opened file "+path, fmt.Errorf("permissions %04o allow group or other access", afterOpen.Mode().Perm()))
	}

	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, true, wrapErr("read "+path, err)
	}
	if int64(len(data)) > maxBytes {
		return nil, true, wrapErr("read "+path, fmt.Errorf("file exceeds %d-byte limit", maxBytes))
	}
	return data, true, nil
}
