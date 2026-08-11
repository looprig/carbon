package storage

import "strconv"

// This file defines the storage-canonical error taxonomy: the concrete error
// types a backend returns for each distinct failure mode. Backends wrap their
// own cause with fmt.Errorf("...: %w", &storage.XxxError{...}); callers
// classify by recovering the concrete type with errors.As, never by string.
//
// Every Error() is prefixed "storage: " and names its subject — the Name/Key
// and any relevant number — with the string subject strconv.Quote'd so an
// untrusted key cannot inject newlines or control bytes into a log line.

// ConflictError reports a ledger compare-and-swap that failed because the caller
// appended at the wrong expected sequence (the head had moved).
type ConflictError struct {
	Name     string
	Expected uint64
}

func (e *ConflictError) Error() string {
	return "storage: ledger " + strconv.Quote(e.Name) + " conflict: wrong expected seq " + strconv.FormatUint(e.Expected, 10)
}

// AmbiguousError reports a ledger append whose acknowledgement was lost or timed
// out: the record may or may not have been committed at Expected. Cause carries
// the underlying transport/timeout error and may be nil.
type AmbiguousError struct {
	Name     string
	Expected uint64
	Cause    error
}

func (e *AmbiguousError) Error() string {
	return "storage: ledger " + strconv.Quote(e.Name) + " append ambiguous at expected seq " + strconv.FormatUint(e.Expected, 10)
}

// Unwrap returns the underlying cause (possibly nil). AmbiguousError is the only
// error in the taxonomy that carries and exposes a cause.
func (e *AmbiguousError) Unwrap() error {
	return e.Cause
}

// RecordNotFoundError reports that a ledger has no record at the requested Seq.
type RecordNotFoundError struct {
	Name string
	Seq  uint64
}

func (e *RecordNotFoundError) Error() string {
	return "storage: ledger " + strconv.Quote(e.Name) + " has no record at seq " + strconv.FormatUint(e.Seq, 10)
}

// KeyNotFoundError reports that a KV key is absent.
type KeyNotFoundError struct {
	Key string
}

func (e *KeyNotFoundError) Error() string {
	return "storage: kv key " + strconv.Quote(e.Key) + " not found"
}

// BlobNotFoundError reports that a blob is absent at Key.
type BlobNotFoundError struct {
	Key string
}

func (e *BlobNotFoundError) Error() string {
	return "storage: blob " + strconv.Quote(e.Key) + " not found"
}

// BlobConflictError reports a blob Put where Key already exists with different
// content (blob writes are content-addressed and immutable per key).
type BlobConflictError struct {
	Key string
}

func (e *BlobConflictError) Error() string {
	return "storage: blob " + strconv.Quote(e.Key) + " already exists with different content"
}

// LeaseHeldError reports an Acquire that was refused because another holder owns
// the lease at HolderEpoch.
type LeaseHeldError struct {
	Name        string
	HolderEpoch uint64
}

func (e *LeaseHeldError) Error() string {
	return "storage: lease " + strconv.Quote(e.Name) + " held by epoch " + strconv.FormatUint(e.HolderEpoch, 10)
}

// LeaseLostError reports a write attempted after the caller's lease at Epoch was
// lost or expired (fenced by a newer holder).
type LeaseLostError struct {
	Name  string
	Epoch uint64
}

func (e *LeaseLostError) Error() string {
	return "storage: lease " + strconv.Quote(e.Name) + " lost at epoch " + strconv.FormatUint(e.Epoch, 10)
}
