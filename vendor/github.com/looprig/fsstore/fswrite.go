package fsstore

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// This file holds the durable-write and tree-listing mechanics shared by the
// filesystem KV and Blobs backends. Both stage a new file's bytes in a sibling
// temp file, fsync it, and os.Rename it over the target (atomic on POSIX), and
// both list their key space by walking a root directory and mapping each committed
// file's path back to a '/'-joined key. The two primitives differ only in their
// typed IO error, which each injects via an ioErrFunc.

// fsTmpPrefix marks a temp file staged for an atomic rename. It starts with a '.',
// which the storage name grammar forbids as a segment's first byte, so a temp
// file can never collide with a committed key and is always skipped by walkKeys —
// a concurrent listing therefore never surfaces a half-written value as a key.
const fsTmpPrefix = ".fsstore-tmp-"

// ioErrFunc constructs a backend's typed filesystem IO error from an operation
// label, path, and cause. writeFileAtomic takes one so the shared write path can
// surface KV-typed or Blobs-typed errors without knowing which backend called it.
type ioErrFunc func(op, path string, cause error) error

// writeFileAtomic durably writes content to target via a sibling temp file: it
// creates the temp in dir (so the rename stays within one filesystem), writes and
// fsyncs it, closes it, and atomically renames it over target. On any failure
// before the rename it removes the stray temp file, joining a cleanup failure onto
// the primary error so neither is lost. The parent-directory fsync that makes the
// rename itself durable is the caller's responsibility. The temp file is created
// at 0600 (os.CreateTemp's default).
func writeFileAtomic(dir, target string, content []byte, ioErr ioErrFunc) (err error) {
	tmp, cerr := os.CreateTemp(dir, fsTmpPrefix+"*")
	if cerr != nil {
		return ioErr("create-temp", dir, cerr)
	}
	tmpName := tmp.Name()
	renamed := false
	// Runs last: unless the temp was renamed into place, remove the stray so a
	// failed write never leaves a partial file behind.
	defer func() {
		if !renamed {
			if rmErr := os.Remove(tmpName); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
				err = errors.Join(err, ioErr("remove-temp", tmpName, rmErr))
			}
		}
	}()

	if _, werr := tmp.Write(content); werr != nil {
		return closeOnError(tmp, tmpName, ioErr("write", tmpName, werr), ioErr)
	}
	if serr := tmp.Sync(); serr != nil {
		return closeOnError(tmp, tmpName, ioErr("fsync", tmpName, serr), ioErr)
	}
	// Close before rename: flush the fd and observe any deferred-write error, and
	// keep the rename valid on platforms that refuse to rename an open file.
	if clerr := tmp.Close(); clerr != nil {
		return ioErr("close", tmpName, clerr)
	}
	if rnerr := os.Rename(tmpName, target); rnerr != nil {
		return ioErr("rename", tmpName, rnerr)
	}
	renamed = true
	return nil
}

// closeOnError closes f on a write/fsync failure path and returns the primary
// error, joining any close failure onto it so neither is masked. It is only used
// after content staging has already failed, so the temp file is discarded by
// writeFileAtomic's deferred cleanup regardless.
func closeOnError(f *os.File, path string, primary error, ioErr ioErrFunc) error {
	if cerr := f.Close(); cerr != nil {
		return errors.Join(primary, ioErr("close", path, cerr))
	}
	return primary
}

// walkKeys lists the committed keys under root whose '/'-joined form has prefix,
// returned lexicographically ascending and duplicate-free. It walks the tree,
// skips directories and in-progress temp files (fsTmpPrefix), and maps each file's
// path back to a key by stripping the root prefix and converting to slash form.
// prefix is a raw substring filter, NOT a validated name (an empty prefix lists
// all keys). The returned error is the raw walk fault; the caller wraps it in its
// own typed IO error.
func walkKeys(root, prefix string) ([]string, error) {
	var out []string
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasPrefix(d.Name(), fsTmpPrefix) {
			return nil // a temp file mid-rename is not a committed key
		}
		key := filepath.ToSlash(strings.TrimPrefix(path, root+string(os.PathSeparator)))
		if strings.HasPrefix(key, prefix) {
			out = append(out, key)
		}
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	// Keys are unique by path, so no dedup is needed; sort makes the listing
	// canonical (WalkDir visits in lexical order, but sorting is a cheap guarantee).
	sort.Strings(out)
	return out, nil
}
