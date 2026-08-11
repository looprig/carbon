package workspace

import (
	"path/filepath"

	"github.com/looprig/harness/pkg/loop"
)

// DenyFilteredRel applies the read guard to a canonical path and returns its
// workspace-relative slash form only when it remains inside the workspace.
func DenyFilteredRel(guard loop.ReadGuard, resolvedRoot, absolutePath string) (string, bool) {
	denyPath := absolutePath
	if resolved, err := filepath.EvalSymlinks(absolutePath); err == nil {
		denyPath = resolved
	}
	if guard.DeniedRead(denyPath) {
		return "", true
	}
	relativePath, err := filepath.Rel(resolvedRoot, denyPath)
	if err != nil || hasParentEscape(relativePath) {
		return "", true
	}
	return filepath.ToSlash(relativePath), false
}
