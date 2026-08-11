// Package atomicfile provides durable, crash-safe atomic replacement of one
// file's contents (spec "docs/specs/long-running-command-supervision.md",
// "Manifests and durability": "Manifest updates use write-new, sync, and
// atomic replace semantics"). Replace writes a temporary file in the
// destination's own directory, syncs it, renames it over the destination,
// and syncs the containing directory where the platform supports it. The
// file that exists at the destination path, if any, remains fully readable
// and unmodified at every point before the rename commits.
//
// This package is stdlib-only and has no dependency on the process package
// or any other part of this module, so it can be reused by any future
// durable-storage need without pulling in process-supervision domain types.
package atomicfile

import (
	"fmt"
	"os"
	"path/filepath"
)

// Stage identifies one durability boundary inside Replace: which step a
// failure occurred at.
type Stage int

// The five durability boundaries Replace passes through, in order.
const (
	StageCreate Stage = iota
	StageWrite
	StageSync
	StageRename
	StageDirSync
)

// String renders a Stage for error messages and test failure output.
func (s Stage) String() string {
	switch s {
	case StageCreate:
		return "create"
	case StageWrite:
		return "write"
	case StageSync:
		return "sync"
	case StageRename:
		return "rename"
	case StageDirSync:
		return "dirsync"
	default:
		return fmt.Sprintf("stage(%d)", int(s))
	}
}

// Error reports a failure at a specific Replace durability boundary,
// wrapping the underlying OS error so callers can errors.As to a *Error to
// inspect exactly which stage failed and errors.Is/Unwrap to inspect the
// underlying cause.
type Error struct {
	Stage Stage
	Path  string
	Err   error
}

func (e *Error) Error() string {
	return fmt.Sprintf("atomicfile: %s %s: %v", e.Stage, e.Path, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// The following package-level function variables are the fault-injection
// seam behind Replace's five durability boundaries. Each defaults to real
// OS/stdlib behavior; replace_test.go (white-box, same package) substitutes
// a fake for the duration of one test to prove Replace fails safely and
// leaves the previous destination content untouched. They are deliberately
// unexported so no public API exposes fault injection.
var (
	createTempFunc = os.CreateTemp
	writeFunc      = writeAll
	syncFunc       = func(f *os.File) error { return f.Sync() }
	renameFunc     = os.Rename
	syncDirFunc    = syncDir // platform-specific: syncdir_unix.go / syncdir_windows.go
)

// Replace atomically overwrites (or creates) the file at path with data.
// perm sets the owner-only permission bits of the resulting file. The
// destination's directory must already exist; Replace never creates it and
// never writes anywhere outside that directory.
//
// On success, path durably contains exactly data. On a failure at any stage
// through StageRename, path is left exactly as it was before the call
// (unchanged if it already existed, absent if it did not); no partial or
// temporary file is left behind. The one exception is a StageDirSync
// failure: by that point the rename has already committed, so path already
// contains data -- the failure only means the containing directory's own
// entry for that rename could not be confirmed durable (e.g. a crash before
// the next directory-metadata flush could, in principle, still lose the
// rename itself on some filesystems). Callers that must distinguish this
// case from an earlier-stage failure can inspect the returned *Error's
// Stage field.
func Replace(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	base := filepath.Base(path)

	tmp, err := createTempFunc(dir, base+".tmp-*")
	if err != nil {
		return &Error{Stage: StageCreate, Path: path, Err: err}
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()

	if err := os.Chmod(tmpPath, perm); err != nil {
		_ = tmp.Close()
		return &Error{Stage: StageCreate, Path: path, Err: err}
	}

	if err := writeFunc(tmp, data); err != nil {
		_ = tmp.Close()
		return &Error{Stage: StageWrite, Path: path, Err: err}
	}

	if err := syncFunc(tmp); err != nil {
		_ = tmp.Close()
		return &Error{Stage: StageSync, Path: path, Err: err}
	}
	if err := tmp.Close(); err != nil {
		return &Error{Stage: StageSync, Path: path, Err: err}
	}

	if err := renameFunc(tmpPath, path); err != nil {
		return &Error{Stage: StageRename, Path: path, Err: err}
	}
	// The rename has committed: path now durably (modulo directory-entry
	// fsync, checked next) contains the new content, and tmpPath no longer
	// exists under its old name. Nothing remains for the deferred cleanup
	// to remove.
	committed = true

	if err := syncDirFunc(dir); err != nil {
		return &Error{Stage: StageDirSync, Path: path, Err: err}
	}
	return nil
}

// writeAll loops Write until all of data has been written or an error
// occurs, since a single os.File.Write is not guaranteed to consume its
// entire argument.
func writeAll(f *os.File, data []byte) error {
	for len(data) > 0 {
		n, err := f.Write(data)
		if err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}
