// Package workspacestore captures a session's working directory as immutable,
// content-addressed snapshots so an agent's files survive the compute they ran on:
// snapshot a tree to a Ref, record the Ref in the session journal, and later
// materialize it on any host to resume. This file defines the Ref name and the
// package's typed error taxonomy; the snapshot/materialize machinery lands in
// later tasks.
package workspacestore

import (
	"strconv"
	"strings"
)

// refPrefix is the fixed, version-and-algorithm-bound prefix every v1 Ref carries:
// format version "v1" and content-hash algorithm "sha256". The version segment is
// the evolution seam — a future incremental format would parse under "v2:" without
// invalidating stored history — so ParseRef accepts this literal prefix and no other.
const refPrefix = "v1:sha256:"

// refHexLen is the exact number of lowercase hex characters a sha256 digest
// occupies (32 bytes rendered as hex). A v1 Ref is refPrefix followed by exactly
// this many hex characters, no more and no fewer.
const refHexLen = 64

// blobKeyPrefix namespaces snapshot archives within a storage Blobs store. A Ref's
// blob key is blobKeyPrefix followed by its 64-character digest hex, which is a
// valid storage name (its two segments each match [a-z0-9][a-z0-9_.-]*).
const blobKeyPrefix = "workspaces/"

// Ref names one immutable, content-addressed snapshot in the canonical form
// "v1:sha256:<64 lowercase hex>". It is opaque to callers: obtain one only from
// ParseRef or from the store, never by string surgery, so every Ref in circulation
// is grammar-valid and its blob key is derivable without re-validation.
type Ref string

// ParseRef validates s against the v1 Ref grammar — the literal refPrefix followed
// by exactly refHexLen lowercase hexadecimal characters — and returns it as a Ref.
// Any violation yields a *InvalidRefError naming the rejected value and the specific
// rule broken; on error the returned Ref is empty, never partially valid.
func ParseRef(s string) (Ref, error) {
	if len(s) == 0 {
		return "", &InvalidRefError{Value: s, Reason: "empty"}
	}
	rest, ok := strings.CutPrefix(s, refPrefix)
	if !ok {
		return "", &InvalidRefError{Value: s, Reason: `must begin with "` + refPrefix + `"`}
	}
	if len(rest) != refHexLen {
		return "", &InvalidRefError{Value: s, Reason: "digest must be exactly " + strconv.Itoa(refHexLen) + " hex characters"}
	}
	for i := 0; i < len(rest); i++ {
		if !isLowerHex(rest[i]) {
			return "", &InvalidRefError{Value: s, Reason: "digest must be lowercase hexadecimal"}
		}
	}
	return Ref(s), nil
}

// hex returns the 64-character lowercase digest of a grammar-valid Ref (one from
// ParseRef or the store). A Ref that does not carry refPrefix — only possible via
// a hand-forged conversion, which the package's own code never produces — yields
// the empty string rather than panicking.
func (r Ref) hex() string {
	rest, ok := strings.CutPrefix(string(r), refPrefix)
	if !ok {
		return ""
	}
	return rest
}

// blobKey returns the storage blob key under which r's snapshot archive is stored:
// blobKeyPrefix joined with r's digest hex. For any Ref obtained from ParseRef this
// is a valid storage name.
func (r Ref) blobKey() string {
	return blobKeyPrefix + r.hex()
}

// isLowerHex reports whether b is a lowercase hexadecimal digit: [0-9a-f]. Uppercase
// is deliberately rejected so a digest has exactly one canonical spelling.
func isLowerHex(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f')
}

// InvalidRefError reports a string that does not satisfy the v1 Ref grammar
// ("v1:sha256:<64 lowercase hex>"). Value is the rejected input and Reason names the
// specific rule it broke. A Ref carries no secret, so both fields are safe to log.
type InvalidRefError struct {
	Value  string
	Reason string
}

func (e *InvalidRefError) Error() string {
	return "workspacestore: invalid ref " + strconv.Quote(e.Value) + ": " + e.Reason
}

// DestNotEmptyError reports that Materialize was asked to restore Want into a
// non-empty Dest whose deterministic re-archive digest (GotDigest, a bare hex) does
// not match Want — a warm volume that has drifted from the checkpointed tree.
// Materialize never wipes a destination; it returns this and leaves the
// clear-and-retry decision to the caller. All fields are log-safe.
type DestNotEmptyError struct {
	Dest      string
	Want      Ref
	GotDigest string
}

func (e *DestNotEmptyError) Error() string {
	return "workspacestore: destination " + strconv.Quote(e.Dest) +
		" is not empty and its contents do not match ref " + string(e.Want) +
		" (re-archive digest " + strconv.Quote(e.GotDigest) + ")"
}

// SnapshotError wraps a failure while snapshotting the tree rooted at Root — a walk,
// archive, hash, or upload error. Root is the caller-supplied root path; Cause is the
// underlying error, reachable via errors.As and errors.Unwrap.
type SnapshotError struct {
	Root  string
	Cause error
}

func (e *SnapshotError) Error() string {
	return "workspacestore: snapshot " + strconv.Quote(e.Root) + ": " + e.Cause.Error()
}

func (e *SnapshotError) Unwrap() error { return e.Cause }

// MaterializeError wraps a failure while materializing Ref into Dest — a fetch,
// decompress, or extract error. Cause is the underlying error, reachable via
// errors.As and errors.Unwrap. A hostile-entry rejection surfaces as
// *ArchiveEntryError instead (typically as this error's Cause).
type MaterializeError struct {
	Ref   Ref
	Dest  string
	Cause error
}

func (e *MaterializeError) Error() string {
	return "workspacestore: materialize ref " + string(e.Ref) +
		" into " + strconv.Quote(e.Dest) + ": " + e.Cause.Error()
}

func (e *MaterializeError) Unwrap() error { return e.Cause }

// IntegrityError reports that an archive fetched from Blobs extracted cleanly but
// the sha256 of its bytes (Got, a bare hex) does not equal the digest Ref names —
// a tampered or corrupted blob whose content still decodes. Materialize wipes the
// partial destination and fails closed, surfacing this as a *MaterializeError's
// Cause. Ref is the expected content address; both fields are log-safe. A blob so
// corrupt it breaks gzip or tar surfaces earlier as an extract error instead.
type IntegrityError struct {
	Ref Ref
	Got string
}

func (e *IntegrityError) Error() string {
	return "workspacestore: archive for ref " + string(e.Ref) +
		" failed integrity check: computed digest " + strconv.Quote(e.Got) +
		" does not match"
}

// ArchiveEntryError reports that an archive entry was rejected during Materialize as
// hostile or unsupported: an absolute or ".."-bearing Name (zip-slip), a symlink
// escaping the destination, or a device/fifo/hardlink entry. Name is the offending
// entry name and Reason names the rule it broke; both are log-safe.
type ArchiveEntryError struct {
	Name   string
	Reason string
}

func (e *ArchiveEntryError) Error() string {
	return "workspacestore: rejected archive entry " + strconv.Quote(e.Name) + ": " + e.Reason
}

// ArchiveLimit names which decompression-bomb guard an archive tripped: the
// per-archive entry count or the cumulative extracted-byte count. It is a typed
// enum so callers match a specific breach without stringly-typed comparisons.
type ArchiveLimit string

const (
	// ArchiveLimitEntries marks a breach of the maximum archive entry count.
	ArchiveLimitEntries ArchiveLimit = "entries"
	// ArchiveLimitBytes marks a breach of the maximum cumulative extracted bytes.
	ArchiveLimitBytes ArchiveLimit = "bytes"
)

// ArchiveLimitError reports that a snapshot archive exceeded a decompression-bomb
// guard during Materialize and was rejected before finishing extraction. Limit
// names the guard (entries or bytes), Cap is the configured maximum, and Observed
// is the count that breached it. Observed is measured from bytes actually written
// or entries actually read — never a header's declared size — so a lying header
// cannot understate the breach. All fields are numeric or a fixed enum, log-safe.
type ArchiveLimitError struct {
	Limit    ArchiveLimit
	Cap      int64
	Observed int64
}

func (e *ArchiveLimitError) Error() string {
	return "workspacestore: archive exceeds " + string(e.Limit) + " limit: observed " +
		strconv.FormatInt(e.Observed, 10) + " > cap " + strconv.FormatInt(e.Cap, 10)
}

// gcOpList and gcOpDelete are the only two values GCError.Op ever carries: they
// name the backend Blobs call that failed during a sweep. They are named
// constants rather than inline literals so the operation is spelled exactly once.
const (
	gcOpList   = "list"
	gcOpDelete = "delete"
)

// GCError wraps a backend Blobs failure encountered during GC: either the List
// that enumerates snapshot blobs (Op == gcOpList, Ref empty) or a Delete that
// removes one unreferenced snapshot (Op == gcOpDelete, Ref naming the blob whose
// removal failed). Cause is the underlying error, reachable via errors.As and
// errors.Unwrap. Op is one of the two package constants; a Ref carries no secret;
// so every field is safe to log.
type GCError struct {
	Op    string
	Ref   Ref
	Cause error
}

func (e *GCError) Error() string {
	if e.Ref == "" {
		return "workspacestore: gc " + e.Op + ": " + e.Cause.Error()
	}
	return "workspacestore: gc " + e.Op + " ref " + string(e.Ref) + ": " + e.Cause.Error()
}

func (e *GCError) Unwrap() error { return e.Cause }
