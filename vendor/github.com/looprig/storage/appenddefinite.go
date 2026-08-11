package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strconv"
)

// AppendVerifyError reports that AppendDefinite could not read the record at
// Seq to resolve an ambiguous/conflicting append. Fail closed: the caller must
// treat the append outcome as unknown. Cause carries the underlying read
// failure (the Read error or the cursor's non-EOF Next error) and is exposed
// via Unwrap.
type AppendVerifyError struct {
	Name  string
	Seq   uint64
	Cause error
}

func (e *AppendVerifyError) Error() string {
	return "storage: ledger " + strconv.Quote(e.Name) + " append verify failed: cannot read record at seq " + strconv.FormatUint(e.Seq, 10)
}

// Unwrap returns the underlying read failure so callers can errors.Is/As it.
func (e *AppendVerifyError) Unwrap() error { return e.Cause }

// errNilCursor is the leaf cause when a backend's Read returns a nil cursor with
// a nil error — a contract violation. verifyAppend is the fail-closed fencing
// core, so it treats this as an unknown outcome rather than panicking.
var errNilCursor = errors.New("storage: ledger Read returned nil cursor without error")

// AppendDefinite turns any Append into a definite outcome. On AmbiguousError it
// retries the identical append once; on conflict (from either attempt) it reads
// the record at expected+1 and byte-compares: equal payload means the original
// landed (success); a foreign payload means this writer has been fenced
// (ConflictError). A second ambiguous outcome surfaces AmbiguousError unresolved.
func AppendDefinite(ctx context.Context, l Ledger, name string, expected uint64, payload []byte) error {
	err := l.Append(ctx, name, expected, payload)
	if err == nil {
		return nil
	}

	var conflict *ConflictError
	if errors.As(err, &conflict) {
		return verifyAppend(ctx, l, name, expected, payload, err)
	}

	var ambiguous *AmbiguousError
	if !errors.As(err, &ambiguous) {
		// Any other error is a definite failure: fail closed with the raw error.
		return err
	}

	// Ambiguous: the first append's outcome is unknown. Retry the identical
	// append exactly once.
	err2 := l.Append(ctx, name, expected, payload)
	if err2 == nil {
		return nil
	}

	var conflict2 *ConflictError
	if errors.As(err2, &conflict2) {
		return verifyAppend(ctx, l, name, expected, payload, err2)
	}

	var ambiguous2 *AmbiguousError
	if errors.As(err2, &ambiguous2) {
		// Still ambiguous after the retry: surface an unresolved outcome,
		// preserving the FIRST ambiguous error as the cause.
		return &AmbiguousError{Name: name, Expected: expected, Cause: err}
	}

	// A definite non-conflict failure on the retry: fail closed.
	return err2
}

// verifyAppend resolves a conflict by inspecting the record now occupying
// expected+1. If that record carries our payload the original append landed
// (nil); a foreign payload — or nothing there at all — means we were fenced, so
// the definite conflictErr is returned. Any failure to read the record is a
// typed AppendVerifyError (fail closed: the outcome is unknown).
func verifyAppend(ctx context.Context, l Ledger, name string, expected uint64, payload []byte, conflictErr error) error {
	seq := expected + 1

	cur, rerr := l.Read(ctx, name, seq)
	if rerr != nil {
		return &AppendVerifyError{Name: name, Seq: seq, Cause: rerr}
	}
	if cur == nil {
		// Contract violation: Read must return a non-nil cursor when err == nil.
		// Fail closed rather than panic at defer cur.Close().
		return &AppendVerifyError{Name: name, Seq: seq, Cause: errNilCursor}
	}
	defer cur.Close()

	rec, nerr := cur.Next(ctx)
	if nerr != nil {
		if errors.Is(nerr, io.EOF) {
			// Nothing at expected+1: our record is not there. Definite conflict.
			return conflictErr
		}
		return &AppendVerifyError{Name: name, Seq: seq, Cause: nerr}
	}

	// Fencing assumption: session records are effectively unique per writer
	// (unique IDs/timestamps in real payloads), so byte-identical content at
	// expected+1 means OUR record landed. A foreign writer committing
	// byte-identical bytes would be indistinguishable — acceptable in practice.
	if bytes.Equal(rec.Payload, payload) {
		// Our record landed despite the conflict/ambiguous ack.
		return nil
	}
	// A foreign record occupies expected+1: this writer was fenced.
	return conflictErr
}
