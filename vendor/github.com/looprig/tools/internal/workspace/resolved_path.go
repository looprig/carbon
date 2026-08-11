package workspace

import "path/filepath"

// ResolvedPath resolves a caller-supplied path against a workspace root and
// reports whether the result is CONTAINED within that root, instead of
// rejecting an uncontained result the way ContainedPath does.
//
// A RELATIVE input is resolved exactly as containedPath does -- anchored under
// root, symlinks resolved, any lexical or symlink escape above root rejected
// with an error. This preserves containedPath's existing escape-prevention
// unchanged: a relative "../" climb is still hard-rejected, never silently
// widened.
//
// An ABSOLUTE input is honoured AS ABSOLUTE (not anchored under root): its
// existing prefix is symlink-resolved the same fail-secure way, and the result
// is reported contained=true or contained=false depending on whether it lands
// under the resolved root. This is the one new capability ResolvedPath adds
// over ContainedPath: a literal absolute path can now resolve to a real,
// reported location outside the workspace instead of being rejected outright.
//
// ResolvedPath itself grants nothing: contained=false is not an authorization
// decision. Callers MUST route an uncontained result through their own
// authorization (e.g. the filesystem.read capability gate) before treating it
// as approved -- exactly as an uncontained Bash command target is authorized
// by the OS sandbox profile, never by this function.
func ResolvedPath(root, input string) (abs string, contained bool, err error) {
	if !filepath.IsAbs(input) {
		resolved, cerr := containedPath(root, input)
		if cerr != nil {
			return "", false, cerr
		}
		return resolved, true, nil
	}

	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", false, &ContainmentError{
			Root:   root,
			Input:  input,
			Reason: "workspace root could not be resolved",
			Err:    err,
		}
	}
	resolvedRoot, err = filepath.Abs(resolvedRoot)
	if err != nil {
		return "", false, &ContainmentError{
			Root:     root,
			Input:    input,
			Resolved: resolvedRoot,
			Reason:   "workspace root could not be made absolute",
			Err:      err,
		}
	}

	resolved, err := resolveExistingPrefix(filepath.Clean(input))
	if err != nil {
		return "", false, &ContainmentError{
			Root:     root,
			Input:    input,
			Resolved: filepath.Clean(input),
			Reason:   "path could not be resolved",
			Err:      err,
		}
	}

	rel, err := filepath.Rel(resolvedRoot, resolved)
	if err != nil {
		return "", false, &ContainmentError{
			Root:     root,
			Input:    input,
			Resolved: resolved,
			Reason:   "resolved path is not relative to root",
			Err:      err,
		}
	}
	return resolved, !hasParentEscape(rel), nil
}
