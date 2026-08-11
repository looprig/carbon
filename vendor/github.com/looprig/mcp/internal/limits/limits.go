// Package limits provides small, dependency-free helpers for enforcing input
// bounds on untrusted data — server responses, wire frames, schemas — before
// it is buffered, parsed, or stored. Helpers report *OverLimitError; callers
// wrap it into their own error taxonomy (this package deliberately does not
// import pkg/client).
package limits

import (
	"fmt"
	"io"
	"unicode/utf8"
)

// Values for OverLimitError.What, identifying which bound was exceeded.
const (
	// WhatRead is reported by readers returned from BoundedReader.
	WhatRead = "read"
	// WhatJSONDepth is reported by CheckJSONDepth.
	WhatJSONDepth = "json_depth"
)

// OverLimitError reports that an input exceeded a configured bound. What
// identifies the bound (one of the What* constants) and Limit its value.
type OverLimitError struct {
	What  string
	Limit int
}

// Error renders "<what> exceeds limit <n>".
func (e *OverLimitError) Error() string {
	return fmt.Sprintf("%s exceeds limit %d", e.What, e.Limit)
}

// BoundedReader wraps r so that at most max bytes can be read through it.
// A source that ends at or before max bytes reads normally (EOF as usual).
// If the source yields more than max bytes, a Read that would cross the
// boundary first returns the allowed remainder with a nil error; the
// following Read returns (0, *OverLimitError). The error is sticky: every
// subsequent Read returns it again.
//
// max <= 0 is a programmer error and panics; use a positive bound.
func BoundedReader(r io.Reader, max int) io.Reader {
	if max <= 0 {
		panic(fmt.Sprintf("limits.BoundedReader: max must be positive, got %d", max))
	}
	return &boundedReader{r: r, remaining: max, limit: max}
}

type boundedReader struct {
	r         io.Reader
	remaining int
	limit     int
	err       error // sticky *OverLimitError once the bound is exceeded
}

func (b *boundedReader) Read(p []byte) (int, error) {
	if b.err != nil {
		return 0, b.err
	}
	if b.remaining <= 0 {
		// The budget is spent. Probe one byte to distinguish "source ended
		// exactly at the limit" (EOF, fine) from "source has more" (over
		// limit). The probed byte is discarded: once over the limit the
		// stream is poisoned and no further data is ever surfaced.
		var probe [1]byte
		n, err := b.r.Read(probe[:])
		if n > 0 {
			b.err = &OverLimitError{What: WhatRead, Limit: b.limit}
			return 0, b.err
		}
		return 0, err
	}
	if len(p) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.r.Read(p)
	b.remaining -= n
	return n, err
}

// CheckJSONDepth reports *OverLimitError if raw's JSON nesting depth (objects
// plus arrays) exceeds maxDepth. Depth of `{}` or `[]` is 1; a bare scalar is
// 0. Brackets inside strings (including after escaped quotes and escaped
// backslashes) do not count. The scan is a single O(1)-space byte pass — no
// unmarshalling, no allocation proportional to input size.
//
// maxDepth <= 0 is not "unbounded": it rejects any input containing a
// container (the first `{` or `[` takes depth to 1, already over the bound),
// while bare scalars — depth 0 — still pass. Callers wanting to admit
// containers must pass a positive bound.
//
// Validity is not this function's concern: malformed JSON never panics and
// is judged only by the bracket depth it exhibits (fail-closed: unmatched
// closers never reduce depth below zero, so stray opens still count).
func CheckJSONDepth(raw []byte, maxDepth int) error {
	depth := 0
	inString := false
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if inString {
			switch c {
			case '\\':
				i++ // skip the escaped byte, whatever it is
			case '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{', '[':
			depth++
			if depth > maxDepth {
				return &OverLimitError{What: WhatJSONDepth, Limit: maxDepth}
			}
		case '}', ']':
			if depth > 0 {
				depth--
			}
		}
	}
	return nil
}

// TruncationMarker terminates any text TruncateText cut short.
const TruncationMarker = "…[truncated]"

// TruncateText bounds s to at most max bytes, cutting at a rune boundary and
// appending TruncationMarker within the budget when truncation occurs. When
// max <= len(TruncationMarker) there is no room for the marker, so the result
// is a marker-less hard cut, still at a rune boundary. The result is valid
// UTF-8 whenever s is. It returns the (possibly truncated) string and whether
// truncation happened. max < 0 is treated as 0.
func TruncateText(s string, max int) (string, bool) {
	if max < 0 {
		max = 0
	}
	if len(s) <= max {
		return s, false
	}
	cut := max
	if max > len(TruncationMarker) {
		cut = max - len(TruncationMarker)
	}
	// cut < len(s) here (cut <= max < len(s)), so s[cut] is in range.
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	if max > len(TruncationMarker) {
		return s[:cut] + TruncationMarker, true
	}
	return s[:cut], true
}
