// Package pathutil provides shared filesystem-path normalization for harness
// components that must compare local persistence roots safely.
package pathutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
)

// CanonicalPathError reports that a path could not be canonicalized without
// ambiguity. Path is the original input, Op is the failed resolution step, and
// Cause is available through errors.Is/As.
type CanonicalPathError struct {
	Path  string
	Op    string
	Cause error
}

func (e *CanonicalPathError) Error() string {
	return "pathutil: canonicalize " + strconv.Quote(e.Path) + ": " + e.Op + ": " + e.Cause.Error()
}

func (e *CanonicalPathError) Unwrap() error { return e.Cause }

// Canonicalize returns sorted, duplicate-free canonical paths. It resolves the
// deepest existing prefix of each path through symlinks and then appends any
// missing suffix. Empty paths are ignored. Any ambiguous resolution fails closed
// with *CanonicalPathError and no partial result.
func Canonicalize(paths []string) ([]string, error) {
	set := make(map[string]struct{})
	for _, path := range paths {
		if path == "" {
			continue
		}
		canonical, err := canonicalPath(path)
		if err != nil {
			return nil, err
		}
		set[canonical] = struct{}{}
	}
	if len(set) == 0 {
		return nil, nil
	}

	result := make([]string, 0, len(set))
	for path := range set {
		result = append(result, path)
	}
	sort.Strings(result)
	return result, nil
}

func canonicalPath(path string) (string, error) {
	absolute, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", &CanonicalPathError{Path: path, Op: "resolve absolute path", Cause: err}
	}

	candidate := absolute
	var suffix []string
	for {
		resolved, resolveErr := filepath.EvalSymlinks(candidate)
		if resolveErr == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(resolveErr, fs.ErrNotExist) {
			return "", &CanonicalPathError{Path: path, Op: "resolve symlinks", Cause: resolveErr}
		}

		_, statErr := os.Lstat(candidate)
		switch {
		case statErr == nil:
			return "", &CanonicalPathError{Path: path, Op: "resolve symlinks", Cause: resolveErr}
		case !errors.Is(statErr, fs.ErrNotExist):
			return "", &CanonicalPathError{Path: path, Op: "inspect path", Cause: statErr}
		}

		parent := filepath.Dir(candidate)
		if parent == candidate {
			return "", &CanonicalPathError{Path: path, Op: "find existing prefix", Cause: resolveErr}
		}
		suffix = append(suffix, filepath.Base(candidate))
		candidate = parent
	}
}
