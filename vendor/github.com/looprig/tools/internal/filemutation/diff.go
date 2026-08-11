package filemutation

import (
	"strings"
)

// diff.go renders a compact, unified-style preview of an EditFile change. It is a
// human-readable summary for the tool result and the approval transcript — NOT a
// machine-applicable patch — so it favours readability (a header + the changed
// region with a few lines of context) over exactness.

// diffPreview returns a unified-ish preview of the change from before→after for
// the file at path. It emits a "--- a/<path>" / "+++ b/<path>" header followed by
// the contiguous changed line region (lines only in `before` prefixed "-", lines
// only in `after` prefixed "+") with diffPreviewContextLines of unchanged context
// ("  ") on each side. Identical content yields a "(no changes)" note.
func diffPreview(path, before, after string) string {
	if before == after {
		return "edited " + path + " (no changes)"
	}
	bl := splitLines(before)
	al := splitLines(after)

	// Find the first and last line indices that differ, walking in from both ends.
	prefix := commonPrefixLen(bl, al)
	suffix := commonSuffixLen(bl, al, prefix)

	var b strings.Builder
	b.WriteString("edited " + path + "\n")
	b.WriteString("--- a/" + path + "\n")
	b.WriteString("+++ b/" + path + "\n")

	ctxStart := prefix - diffPreviewContextLines
	if ctxStart < 0 {
		ctxStart = 0
	}
	// Leading context (unchanged lines just before the change).
	for i := ctxStart; i < prefix; i++ {
		b.WriteString("  " + bl[i] + "\n")
	}
	// Removed lines (in before, not in the unchanged prefix/suffix).
	for i := prefix; i < len(bl)-suffix; i++ {
		b.WriteString("- " + bl[i] + "\n")
	}
	// Added lines (in after).
	for i := prefix; i < len(al)-suffix; i++ {
		b.WriteString("+ " + al[i] + "\n")
	}
	// Trailing context (unchanged lines just after the change), drawn from the
	// shared suffix region of `before`.
	trailStart := len(bl) - suffix
	trailEnd := trailStart + diffPreviewContextLines
	if trailEnd > len(bl) {
		trailEnd = len(bl)
	}
	for i := trailStart; i < trailEnd; i++ {
		b.WriteString("  " + bl[i] + "\n")
	}

	return strings.TrimRight(b.String(), "\n")
}

// splitLines splits s into lines WITHOUT a trailing empty element for a final
// newline (so "a\nb\n" -> ["a","b"], and "" -> []), which keeps the diff line
// math clean.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	// Drop a single trailing empty element produced by a terminating newline.
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	return lines
}

// commonPrefixLen returns the number of leading lines a and b share.
func commonPrefixLen(a, b []string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

// commonSuffixLen returns the number of trailing lines a and b share, without
// overlapping the already-counted common prefix (so a change is never
// double-counted on both ends).
func commonSuffixLen(a, b []string, prefix int) int {
	n := 0
	for n < len(a)-prefix && n < len(b)-prefix && a[len(a)-1-n] == b[len(b)-1-n] {
		n++
	}
	return n
}
