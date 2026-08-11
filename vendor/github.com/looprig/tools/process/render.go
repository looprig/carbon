package process

import (
	"encoding/base64"

	"github.com/looprig/tools/internal/safetext"
)

// Reader is the minimal cursor-addressed read surface render.go needs from
// a byte store. Both *Buffer (buffer.go) and *Spool (spool.go) already
// implement this exact method signature, so render.go depends on neither
// concretely: a caller passes whichever store currently holds the
// requested cursor range. Routing between the in-memory window and the
// disk spool (and any owner authorization) is Task 8's supervisor's job,
// not this package's rendering logic.
type Reader interface {
	Read(cursor int64, maxBytes int) (data []byte, nextCursor int64, gap bool, err error)
}

// ArtifactEncodingBase64 is the only Encoding value Artifact currently
// carries: raw bytes retrieved through a base64 ProcessOutput read (spec
// "ProcessOutput API": `"artifact": {"id": "opaque", "encoding": "base64"}`).
const ArtifactEncodingBase64 = "base64"

// Artifact is an opaque, path-free reference to a bounded window of a
// process's raw output (spec "Output capture and storage": "raw bytes
// remain only in the bounded spool and are exposed only through an opaque
// artifact descriptor plus owner-authorized ProcessOutput base64 reads";
// "No filesystem path to a spool or manifest is returned to a model").
// Every field is independently already safe to hand to a model:
// ProcessID is a Handle, which by construction carries no filesystem path,
// owner identifier, or OS PID (see identity.go's Handle doc); StartCursor
// and EndCursor are plain integers. There is no path, file descriptor, or
// other host detail anywhere in this type. A caller retrieves the bytes an
// Artifact describes with a base64 ProcessOutput read against ProcessID at
// StartCursor (spec: "callers retrieve its bytes with ProcessOutput and the
// original process handle, cursor, and base64 encoding").
type Artifact struct {
	ProcessID   Handle
	StartCursor int64
	EndCursor   int64
	Encoding    string
}

// NewArtifact builds the opaque descriptor for the raw byte range
// [startCursor, endCursor) of handle's output.
func NewArtifact(handle Handle, startCursor, endCursor int64) Artifact {
	return Artifact{
		ProcessID:   handle,
		StartCursor: startCursor,
		EndCursor:   endCursor,
		Encoding:    ArtifactEncodingBase64,
	}
}

// SafeTextResult is the safe-text render outcome for one bounded,
// cursor-addressed read (spec "ProcessOutput API" result shape, the subset
// render.go owns: output/start_cursor/next_cursor/gap/normalized/binary/
// artifact). Manifest-derived fields such as total_bytes, status, and
// exit_code belong to the future supervisor-facing ProcessOutput tool
// (Task 16), not to this task's render/encode logic.
type SafeTextResult struct {
	// Output is the normalized, capped, model-visible text.
	Output string
	// StartCursor is the cursor Read was called with (before any
	// gap adjustment).
	StartCursor int64
	// NextCursor is the exclusive cursor immediately after the bytes Read
	// actually returned (Reader's own nextCursor, unaffected by the
	// safe-text cap below).
	NextCursor int64
	// Gap reports whether StartCursor fell before the earliest retained
	// byte; when true, Output begins at the earliest retained byte rather
	// than at StartCursor, exactly as Reader.Read documents.
	Gap bool
	// Normalized reports whether normalization changed anything: invalid
	// UTF-8 was replaced, or a control/escape sequence was removed. Safe
	// pass-through text (Step 2's "safe text unchanged" case) reports
	// false.
	Normalized bool
	// Binary reports whether the pre-normalization bytes looked like
	// binary data (safetext.LooksBinary), so a caller can prefer routing
	// this read through base64/Artifact instead of showing Output inline.
	Binary bool
	// Artifact is always populated, independent of Binary, so a caller can
	// retrieve the exact raw bytes of this same cursor range later via a
	// base64 read, even for output that rendered safely as inline text.
	Artifact Artifact
}

// RenderSafeText reads up to maxBytes of retained output from r starting at
// cursor -- sharing Reader's exact gap and cursor-ahead Read semantics --
// normalizes it through a one-shot safetext.Normalizer, and caps the
// normalized text at capBytes (a non-positive capBytes defaults to
// DefaultMaxInlineResultBytes) without splitting a replacement sequence
// (safetext.Truncate).
//
// RenderSafeText normalizes each call's byte window independently: a
// terminal escape sequence that happens to be split exactly at this read's
// byte boundary is safely dropped (never leaked as raw bytes) but not
// reconstructed across separate RenderSafeText calls, unlike
// safetext.Normalizer's own cross-call carry-over (normalize_test.go),
// which this function does not use across calls. Reconstructing a sequence
// split across two different ProcessOutput polls of the same process would
// require a per-process *safetext.Normalizer threaded through the
// supervisor across polls; that is out of this task's scope (see the task
// doc: render.go accepts already-resolved byte sources and does not wire
// supervisor plumbing) and is flagged here for the phase-gate reviewer to
// weigh in on for Task 8/16.
func RenderSafeText(r Reader, handle Handle, cursor int64, maxBytes int, capBytes int64) (SafeTextResult, error) {
	data, next, gap, err := r.Read(cursor, maxBytes)
	if err != nil {
		return SafeTextResult{}, err
	}
	if capBytes <= 0 {
		capBytes = DefaultMaxInlineResultBytes
	}

	binary := safetext.LooksBinary(data)

	var norm safetext.Normalizer
	normalized := norm.Normalize(data)
	changed := string(normalized) != string(data)
	capped := safetext.Truncate(normalized, int(capBytes))

	return SafeTextResult{
		Output:      string(capped),
		StartCursor: cursor,
		NextCursor:  next,
		Gap:         gap,
		Normalized:  changed,
		Binary:      binary,
		Artifact:    NewArtifact(handle, cursor, next),
	}, nil
}

// Base64Result is the base64 render outcome for one bounded,
// cursor-addressed read: the exact raw bytes from r, unmodified and
// unnormalized, base64-encoded (spec "ProcessOutput API": "`base64` reads
// the same owner-authorized raw spool bytes without exposing a host path").
type Base64Result struct {
	// Data is the raw bytes Read returned, base64-encoded byte for byte
	// with no normalization applied.
	Data string
	// StartCursor is the cursor Read was called with (before any
	// gap adjustment).
	StartCursor int64
	// NextCursor is the exclusive cursor immediately after the bytes Read
	// actually returned.
	NextCursor int64
	// Gap reports whether StartCursor fell before the earliest retained
	// byte, exactly as Reader.Read documents.
	Gap bool
}

// RenderBase64 reads up to maxBytes of retained output from r starting at
// cursor and returns it base64-encoded, byte for byte, with no
// normalization applied. It reuses exactly the same Reader/cursor/maxBytes
// plumbing as RenderSafeText -- "the same owner check and byte limits as
// safe text" from the task doc means both render modes share this one
// read path; only RenderSafeText additionally normalizes and caps.
// RenderBase64 performs no owner authorization itself (see Reader's doc);
// that is Task 8's supervisor's responsibility before either render
// function is called.
func RenderBase64(r Reader, cursor int64, maxBytes int) (Base64Result, error) {
	data, next, gap, err := r.Read(cursor, maxBytes)
	if err != nil {
		return Base64Result{}, err
	}
	return Base64Result{
		Data:        base64.StdEncoding.EncodeToString(data),
		StartCursor: cursor,
		NextCursor:  next,
		Gap:         gap,
	}, nil
}
