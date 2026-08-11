package workspace

import "path/filepath"

// ResolveRoot returns the canonical absolute workspace root.
func ResolveRoot(root string) (string, error) {
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	return filepath.Abs(resolved)
}

// ResolveSpawnDir resolves an optional workspace-relative command directory.
func ResolveSpawnDir(root, workdir string) (string, error) {
	if workdir == "" {
		return root, nil
	}
	return ContainedPath(root, workdir)
}

// JoinedPath returns the lexically cleaned workspace path without resolving a
// final-component symlink. Callers must prove containment separately.
func JoinedPath(root, input string) string {
	return filepath.Join(root, filepath.Clean(input))
}
