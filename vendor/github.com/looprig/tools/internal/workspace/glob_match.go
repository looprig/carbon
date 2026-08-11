// Package workspace holds the shared, security-sensitive primitives the
// standard tools depend on: `**`-aware glob matching, workspace path
// containment, and typed-nil detection for injected dependencies.
package workspace

import (
	"path"
	"strings"
)

// doubleStar is the pattern segment that matches zero or more path segments.
const doubleStar = "**"

// matchGlob reports whether the slash-separated relPath matches the glob
// pattern. It is the single matcher shared by the permission path-matchers and
// the Glob tool, so its semantics are deliberately fixed and documented here.
//
// Semantics:
//   - Both pattern and relPath are split on "/" into segments.
//   - A "**" pattern segment matches ZERO OR MORE path segments (doublestar).
//   - Every other pattern segment is matched against the single corresponding
//     path segment with stdlib path.Match, so "*", "?" and "[...]" work WITHIN a
//     segment but "*" never crosses a "/".
//   - A malformed pattern segment (path.Match returns ErrBadPattern) makes that
//     segment fail to match — it never panics. This is fail-secure: a bad
//     pattern matches nothing rather than erroring or matching everything.
//
// Edge-case decisions (deliberate):
//   - Inputs are normalised with path.Clean first. path.Clean("") == "." and
//     collapses "a//b" -> "a/b", "a/./b" -> "a/b", trailing "/" -> none. This
//     means a trailing slash on either side is irrelevant and "" behaves as ".".
//   - pattern "**" matches EVERYTHING, including the cleaned-empty path ".".
//     This is the only way an empty/"." path matches, since a "." path segment
//     does not match a literal "" or "*" pattern segment otherwise.
//   - An empty pattern (cleaned to ".") matches only the empty/"." path.
//   - Because callers feed an already-canonical, symlink-resolved
//     workspace-relative path (see containedPath), a ".." segment should never
//     appear; if one does it is treated as an ordinary literal segment and will
//     only match a pattern segment that also literally matches "..".
//
// matchGlob always terminates and never panics for any input.
func matchGlob(pattern, relPath string) bool {
	patSegs := splitClean(pattern)
	pathSegs := splitClean(relPath)
	return matchSegments(patSegs, pathSegs)
}

// MatchGlob matches a slash-separated workspace-relative path, including the
// recursive ** segment supported by the standard file and permission tools.
func MatchGlob(pattern, relPath string) bool {
	return matchGlob(pattern, relPath)
}

// splitClean cleans s with path.Clean and splits it into segments on "/".
// A cleaned "." (empty or dot path) yields an empty segment slice so that the
// matcher treats it as "no segments" — which only "**" matches.
func splitClean(s string) []string {
	cleaned := path.Clean(s)
	if cleaned == "." {
		return nil
	}
	// Leading "/" would yield a leading empty segment; the canonical relPath is
	// workspace-relative so an absolute pattern/path is unusual, but handle it
	// without surprises by trimming a single leading separator.
	cleaned = strings.TrimPrefix(cleaned, "/")
	if cleaned == "" {
		return nil
	}
	return strings.Split(cleaned, "/")
}

// matchSegments runs the classic two-pointer doublestar backtracking match over
// the pre-split segment slices. It is iterative and bounded by len(pat)*len(name)
// in the worst case, so it cannot catastrophically backtrack or hang.
func matchSegments(pat, name []string) bool {
	var (
		px, nx         int // current indices into pat and name
		starPx, starNx = -1, 0
		haveStar       = false
	)
	for nx < len(name) {
		switch {
		case px < len(pat) && pat[px] == doubleStar:
			// Record a backtrack point: "**" tentatively consumes zero segments.
			haveStar = true
			starPx = px
			starNx = nx
			px++
		case px < len(pat) && segmentMatch(pat[px], name[nx]):
			px++
			nx++
		case haveStar:
			// Let the last "**" consume one more name segment, then retry.
			px = starPx + 1
			starNx++
			nx = starNx
		default:
			return false
		}
	}
	// Remaining pattern segments must all be "**" (each matching zero segments).
	for px < len(pat) && pat[px] == doubleStar {
		px++
	}
	return px == len(pat)
}

// segmentMatch matches a single pattern segment against a single name segment
// using path.Match. A "**" never reaches here (handled in matchSegments). A
// malformed pattern (ErrBadPattern) yields false — never a panic or error.
func segmentMatch(patSeg, nameSeg string) bool {
	ok, err := path.Match(patSeg, nameSeg)
	if err != nil {
		return false
	}
	return ok
}
