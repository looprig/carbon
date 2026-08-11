// Package safetext converts raw process output bytes into model-safe text
// (spec "docs/specs/long-running-command-supervision.md", "Output capture
// and storage": "Model-visible text passes through safe-text normalization:
// invalid UTF-8 is replaced deterministically; disallowed terminal control
// sequences are escaped or removed; binary detection is reported;
// normalization is reported").
//
// This package is pure text processing: it has no knowledge of processes,
// owners, handles, cursors, or storage, and it must stay that way -- the
// process package (process/render.go) is the only place that wires this
// package's output to a process's identity and cursor-addressed reads.
// safetext is stdlib-only.
package safetext

import (
	"math"
	"unicode/utf8"
)

// scanState is Normalizer's carry-over state for the terminal-control-
// sequence scanner. It survives across Normalize calls so a sequence split
// exactly at a chunk boundary (e.g. the ESC byte at the end of one chunk,
// the sequence's introducer byte at the start of the next) is still
// recognized as a single unit and no partial escape byte ever reaches the
// output.
type scanState int

const (
	stateText       scanState = iota // ordinary text: decode runes, filter controls
	stateEsc                         // just consumed ESC (0x1B), waiting to classify
	stateCSI                         // inside a CSI sequence, waiting for its final byte
	stateOSCBody                     // inside an OSC string, waiting for BEL or ESC \
	stateOSCBodyEsc                  // inside an OSC string, just saw ESC, waiting for \
	stateDCSBody                     // inside a DCS string, waiting for ESC \
	stateDCSBodyEsc                  // inside a DCS string, just saw ESC, waiting for \
)

// Normalizer incrementally converts a raw byte stream, arriving in
// arbitrary chunks, into model-safe text. It:
//
//   - replaces invalid UTF-8 byte sequences deterministically with the
//     standard U+FFFD replacement character;
//   - strips C0 control bytes (0x00-0x1F) and C1 control codepoints
//     (U+0080-U+009F, however encoded) except approved whitespace (space,
//     tab, newline, and carriage return -- see the isApprovedWhitespace doc
//     comment for why \r is included);
//   - recognizes and removes ANSI/terminal CSI (`ESC [ ... final-byte`),
//     OSC (`ESC ] ... BEL` or `ESC ] ... ESC \`), and DCS (`ESC P ...
//     ESC \`) sequences as complete units, even when a sequence is split
//     across two separate Normalize calls.
//
// The zero Normalizer is ready to use. A Normalizer carries state between
// calls (in-progress escape sequence, a truncated trailing UTF-8 byte
// sequence) and assumes its Normalize calls are fed strictly in stream
// order with no gaps or rewinds; it is NOT safe for concurrent use, and
// re-feeding an overlapping or out-of-order chunk produces undefined
// (though never unsafe -- no raw control byte can leak) results. Each
// distinct byte stream (e.g. one process's combined output) needs its own
// Normalizer.
type Normalizer struct {
	state   scanState
	pending []byte // a trailing byte sequence that might be the truncated prefix of a valid multi-byte UTF-8 rune, held for the next call
}

// isStrippedControl reports whether r is a C0 control (0x00-0x1F), C1
// control (U+0080-U+009F), or DEL (0x7F). DEL is not part of either the C0
// or C1 range by the formal ISO 6429 boundaries the task text cites, but it
// is a non-printable control byte with no legitimate place in model-visible
// text, so this implementation strips it alongside C0/C1 as a documented
// extension beyond the task's literal range list; flag this at the phase
// gate if that over-reaches the intended scope.
func isStrippedControl(r rune) bool {
	return (r >= 0x00 && r <= 0x1F) || r == 0x7F || (r >= 0x80 && r <= 0x9F)
}

// isApprovedWhitespace reports whether r is one of the C0 whitespace
// controls this package deliberately keeps: space is not a C0 control at
// all (0x20) and always passes through regardless of this function. Tab and
// newline are the two the task text names explicitly. Carriage return is
// added as a documented judgment call: captured terminal output routinely
// contains bare \r (progress bars, \r\n line endings), and stripping it
// would silently corrupt otherwise-ordinary text, so it is treated as
// approved whitespace alongside \t and \n.
func isApprovedWhitespace(r rune) bool {
	return r == '\t' || r == '\n' || r == '\r'
}

// Normalize consumes chunk -- the next slice of a byte stream, in stream
// order -- and returns the model-safe text produced from it. Bytes that
// belong to an in-progress escape sequence or a truncated UTF-8 rune
// carried over from a previous call (or held back for a future one) are
// never included in the returned slice's bytes as raw/partial data; they
// are either completed and dropped (escape sequences are removed entirely)
// or reconstructed and decoded (a completed multi-byte rune) on a later
// call.
func (n *Normalizer) Normalize(chunk []byte) []byte {
	data := chunk
	if len(n.pending) > 0 {
		data = make([]byte, 0, len(n.pending)+len(chunk))
		data = append(data, n.pending...)
		data = append(data, chunk...)
		n.pending = nil
	}

	out := make([]byte, 0, len(data))
	i := 0
	for i < len(data) {
		switch n.state {
		case stateText:
			consumed := n.stepText(data[i:], &out)
			if consumed == 0 {
				// The remaining bytes are a possibly-valid but truncated
				// UTF-8 prefix: hold them for the next call instead of
				// emitting a premature replacement character.
				n.pending = append([]byte(nil), data[i:]...)
				i = len(data)
				continue
			}
			i += consumed
		case stateEsc:
			b := data[i]
			i++
			switch b {
			case '[':
				n.state = stateCSI
			case ']':
				n.state = stateOSCBody
			case 'P':
				n.state = stateDCSBody
			default:
				// A simple two-byte escape sequence (e.g. ESC c, ESC =):
				// complete and dropped as a unit.
				n.state = stateText
			}
		case stateCSI:
			b := data[i]
			i++
			if b >= 0x40 && b <= 0x7E {
				n.state = stateText
			}
			// Parameter/intermediate bytes (0x20-0x3F) stay in stateCSI.
		case stateOSCBody:
			b := data[i]
			i++
			switch b {
			case 0x07: // BEL terminator
				n.state = stateText
			case 0x1B: // possible ST (ESC \) terminator
				n.state = stateOSCBodyEsc
			}
		case stateOSCBodyEsc:
			b := data[i]
			i++
			if b == '\\' {
				n.state = stateText
			} else {
				// Not a valid ST after all; resume consuming the OSC body.
				// This byte is still swallowed either way -- OSC content is
				// never emitted -- so silently returning to stateOSCBody is
				// a safe, if imprecise, simplification for the malformed
				// case (not exercised by this task's required coverage).
				n.state = stateOSCBody
			}
		case stateDCSBody:
			b := data[i]
			i++
			if b == 0x1B {
				n.state = stateDCSBodyEsc
			}
		case stateDCSBodyEsc:
			b := data[i]
			i++
			if b == '\\' {
				n.state = stateText
			} else {
				n.state = stateDCSBody
			}
		}
	}
	return out
}

// stepText processes stateText's next unit of work: either the single ESC
// byte that starts an escape sequence, or one decoded rune. It appends any
// model-safe text to out and returns the number of input bytes consumed. A
// return of 0 means data begins with a truncated-but-possibly-valid UTF-8
// prefix that must wait for more bytes (see the caller's pending handling).
func (n *Normalizer) stepText(data []byte, out *[]byte) (consumed int) {
	if data[0] == 0x1B {
		n.state = stateEsc
		return 1
	}

	r, size := utf8.DecodeRune(data)
	if r == utf8.RuneError && size <= 1 {
		if !utf8.FullRune(data) {
			return 0
		}
		*out = utf8.AppendRune(*out, utf8.RuneError)
		return 1
	}

	if isStrippedControl(r) {
		if isApprovedWhitespace(r) {
			*out = append(*out, data[:size]...)
		}
		return size
	}
	*out = append(*out, data[:size]...)
	return size
}

// Truncate trims data to at most limit bytes without splitting a multi-byte
// UTF-8 sequence in half. data is assumed to already be valid UTF-8 (as
// Normalize's own output always is), so any RuneError observed while
// backing off from the cut point can only mean the cut fell inside a
// multi-byte sequence, never a genuinely invalid byte -- backing off by at
// most utf8.UTFMax-1 bytes always reaches a valid boundary. A non-positive
// limit returns data unchanged (no cap requested); limit == 0 returns an
// empty slice.
func Truncate(data []byte, limit int) []byte {
	if limit < 0 || len(data) <= limit {
		return data
	}
	if limit == 0 {
		return data[:0]
	}
	data = data[:limit]
	for i := 0; i < utf8.UTFMax && len(data) > 0 && !utf8.Valid(data); i++ {
		data = data[:len(data)-1]
	}
	return data
}

// Binary detection tunables. Kept deliberately simple and testable (task
// text: "keep it simple and testable"): a NUL-density check catches
// obviously-binary data cheaply, and a Shannon-entropy check over a large
// enough sample catches high-entropy binary/compressed data that happens to
// contain no NUL bytes at all.
const (
	// binaryNULThresholdPercent: more than this percentage of NUL bytes in
	// the sample looks binary. Legitimate text output essentially never
	// contains NUL bytes at all, so this is deliberately sensitive.
	binaryNULThresholdPercent = 1
	// binaryMinSampleBytes is the minimum sample size before the entropy
	// check runs at all. Entropy is bounded above by log2(min(256, N)) for
	// N bytes, so a sample much smaller than 256 bytes can never reach
	// binaryEntropyThreshold regardless of content; below this size, only
	// the NUL-density check applies.
	binaryMinSampleBytes = 256
	// binaryEntropyThreshold is the Shannon entropy, in bits per byte (max
	// 8, for a perfectly uniform byte distribution), above which a sample
	// is reported as binary.
	binaryEntropyThreshold = 7.5
)

// LooksBinary reports whether data looks like binary data rather than text,
// so a caller (process/render.go) can route it through base64 instead of
// inline safe text. This is a heuristic, not a proof: it combines a NUL-byte
// density check with a Shannon-entropy check over the byte-value
// distribution (task text: "NUL-byte density and/or Shannon-entropy-style
// byte-distribution check").
func LooksBinary(data []byte) bool {
	if len(data) == 0 {
		return false
	}

	var hist [256]int
	nul := 0
	for _, b := range data {
		hist[b]++
		if b == 0 {
			nul++
		}
	}
	if nul*100 > len(data)*binaryNULThresholdPercent {
		return true
	}
	if len(data) < binaryMinSampleBytes {
		return false
	}
	return shannonEntropy(hist[:], len(data)) >= binaryEntropyThreshold
}

// shannonEntropy computes the Shannon entropy, in bits per symbol, of the
// byte-value histogram hist over total samples.
func shannonEntropy(hist []int, total int) float64 {
	if total == 0 {
		return 0
	}
	var entropy float64
	for _, count := range hist {
		if count == 0 {
			continue
		}
		p := float64(count) / float64(total)
		entropy -= p * math.Log2(p)
	}
	return entropy
}
