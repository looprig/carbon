package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ContainmentError is the single typed error returned by containedPath whenever
// a path cannot be proven to live inside the workspace root, or the resolution
// itself fails. It is errors.As-able so callers can inspect every dimension of
// the rejection. A non-nil ContainmentError ALWAYS means "deny": containedPath
// is fail-secure and never returns a path alongside an error.
type ContainmentError struct {
	Root     string // the workspace root as supplied to containedPath
	Input    string // the caller-supplied (untrusted) path
	Resolved string // the best resolved path we computed before rejecting ("" if none)
	Reason   string // human-readable, non-secret reason for the denial
	Err      error  // underlying cause (e.g. an os/filepath error), may be nil
}

func (e *ContainmentError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("path containment denied: %s (root=%q input=%q resolved=%q): %v",
			e.Reason, e.Root, e.Input, e.Resolved, e.Err)
	}
	return fmt.Sprintf("path containment denied: %s (root=%q input=%q resolved=%q)",
		e.Reason, e.Root, e.Input, e.Resolved)
}

// Unwrap exposes the underlying cause for errors.Is / errors.As chaining.
func (e *ContainmentError) Unwrap() error { return e.Err }

// errNoExistingAncestor is the leaf cause returned by resolveExistingPrefix when
// it walks all the way to the volume/filesystem root without finding any
// existing ancestor. This is (near-)unreachable for an absolute joined path
// anchored under a root that already resolved, but we fail secure rather than
// loop forever. It carries no Root/Input context: containedPath wraps it into a
// full *ContainmentError at the call site, exactly as it wraps every other
// resolution failure, so EVERY *ContainmentError it returns has Root and Input
// populated.
var errNoExistingAncestor = errors.New("no existing ancestor found for path")

// containedPath resolves a caller-supplied path against a workspace root and
// returns the cleaned, symlink-resolved, ABSOLUTE path proven to be inside the
// root — or a *ContainmentError. It implements §3c steps 1–4 of the tools
// design (step 5, the O_NOFOLLOW open, is the calling tool's concern and is
// intentionally NOT done here — this function performs no open()).
//
// Resolution (fail-secure at every step):
//  1. EvalSymlinks(root) once -> the canonical root. A root that does not exist
//     or cannot be resolved is rejected (we will not trust an unresolved root).
//  2. Clean the input and Join it UNDER the resolved root. The input is treated
//     as workspace-relative. An absolute input (e.g. "/etc/passwd") is NOT
//     honoured as absolute: filepath.Join(root, "/etc/passwd") anchors it under
//     root, and any "../" that tries to climb out is caught in step 4. So an
//     absolute path outside the root is rejected rather than escaping.
//  3. For an EXISTING target, EvalSymlinks the full joined path. For a
//     NOT-YET-EXISTING write target, EvalSymlinks the DEEPEST EXISTING ancestor
//     and re-append the non-existent remainder (so we resolve symlinks in every
//     real component, then keep the would-be tail verbatim).
//  4. Verify the resolved path is still under the resolved root with
//     filepath.Rel: reject if the relative path is ".." or begins with
//     ".." + separator. This catches a symlink inside the workspace that points
//     outside it, as well as any "../" escape that survived Join.
//
// The returned path is absolute and contains no remaining symlinks in its
// existing components.
func containedPath(root, input string) (string, error) {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", &ContainmentError{
			Root:   root,
			Input:  input,
			Reason: "workspace root could not be resolved",
			Err:    err,
		}
	}
	// EvalSymlinks does not guarantee an absolute path if root was relative;
	// make the canonical root absolute so the final Rel check is meaningful.
	resolvedRoot, err = filepath.Abs(resolvedRoot)
	if err != nil {
		return "", &ContainmentError{
			Root:     root,
			Input:    input,
			Resolved: resolvedRoot,
			Reason:   "workspace root could not be made absolute",
			Err:      err,
		}
	}

	// Step 2: anchor the cleaned input under the resolved root. filepath.Join
	// cleans the result, collapsing "." and resolving lexical "..". A leading
	// "/" or "../" in input is neutralised here (Join anchors under root) but a
	// path that still climbs above root is rejected in step 4.
	joined := filepath.Join(resolvedRoot, filepath.Clean(input))

	// Step 3: resolve symlinks. EvalSymlinks requires the path (and its parents)
	// to exist; for a not-yet-existing write target we resolve the deepest
	// existing ancestor and re-append the remainder.
	resolved, err := resolveExistingPrefix(joined)
	if err != nil {
		return "", &ContainmentError{
			Root:     root,
			Input:    input,
			Resolved: joined,
			Reason:   "path could not be resolved",
			Err:      err,
		}
	}

	// Step 4: containment check via Rel against the resolved root.
	rel, err := filepath.Rel(resolvedRoot, resolved)
	if err != nil {
		return "", &ContainmentError{
			Root:     root,
			Input:    input,
			Resolved: resolved,
			Reason:   "resolved path is not relative to root",
			Err:      err,
		}
	}
	// rel == "." means the path IS the root itself, which is contained (allowed).
	// rel == ".." or a "../"-prefixed rel means it climbed above root: reject.
	if hasParentEscape(rel) {
		return "", &ContainmentError{
			Root:     root,
			Input:    input,
			Resolved: resolved,
			Reason:   "resolved path escapes the workspace root",
		}
	}

	return resolved, nil
}

// ContainedPath resolves input beneath root and fails closed when containment
// cannot be proven.
func ContainedPath(root, input string) (string, error) {
	return containedPath(root, input)
}

// hasParentEscape reports whether the Rel result climbs above the root, i.e. it
// is exactly ".." or starts with a ".." path component (".." + separator). A
// path that merely contains ".." deeper inside (which Rel never produces for a
// contained path anyway) is not treated as an escape.
func hasParentEscape(rel string) bool {
	if rel == ".." {
		return true
	}
	return len(rel) >= 3 && rel[0] == '.' && rel[1] == '.' && os.IsPathSeparator(rel[2])
}

// resolveExistingPrefix resolves symlinks for the longest existing prefix of
// target and re-appends the non-existent remainder verbatim. If the full target
// exists, it is fully EvalSymlinks-resolved. The returned path is absolute and
// has all symlinks in its existing components resolved.
//
// It walks up from the full target to the root-most ancestor, stopping at the
// first ancestor that exists (via Lstat). EvalSymlinks is then applied to that
// existing ancestor, and the trailing components are joined back on. This is
// fail-secure: if even the volume root does not resolve, the error propagates.
// On the (near-)unreachable volume-root fixed-point branch it returns the leaf
// errNoExistingAncestor cause, which containedPath wraps with full Root/Input
// context — this function deliberately adds no Root/Input itself.
func resolveExistingPrefix(target string) (string, error) {
	// Fast path: the whole target exists -> resolve it directly.
	if _, err := os.Lstat(target); err == nil {
		return filepath.EvalSymlinks(target)
	} else if !errors.Is(err, os.ErrNotExist) {
		// A non-"not exist" Lstat error (e.g. ELOOP, EACCES on a component) is
		// fail-secure: we cannot prove containment, so reject.
		return "", err
	}

	// Walk up collecting non-existent trailing components until we find an
	// existing ancestor. The loop is bounded by the number of path separators.
	var tail []string
	cur := target
	for {
		parent := filepath.Dir(cur)
		if parent == cur {
			// Reached the volume/filesystem root and nothing existed. This
			// should be unreachable for an absolute joined path (root exists),
			// but fail secure rather than loop forever. Return the leaf cause;
			// containedPath wraps it with Root/Input at the call site.
			return "", errNoExistingAncestor
		}
		tail = append([]string{filepath.Base(cur)}, tail...)
		_, err := os.Lstat(parent)
		if err == nil {
			// parent exists. If it is a symlink, EvalSymlinks resolves it; if it
			// is not a directory we still resolve and let the containment check
			// (and the later tool open) deal with the type.
			resolvedParent, evErr := filepath.EvalSymlinks(parent)
			if evErr != nil {
				return "", evErr
			}
			return filepath.Join(append([]string{resolvedParent}, tail...)...), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		cur = parent
	}
}
