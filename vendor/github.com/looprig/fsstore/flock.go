package fsstore

import (
	"errors"
	"strconv"
)

// This file declares the cross-process advisory-lock helper shared by fsstore's
// backends (the ledger here; later the leaser). The lock is an OS advisory file
// lock taken on the backend's own file, so a crashed holder's lock is released
// automatically by the kernel — no stale-lock cleanup is ever required. The
// platform mechanism lives in the build-tagged flock_unix.go / flock_unsupported.go;
// a platform without advisory locking fails closed with a *FlockError rather than
// degrading to a silent no-op (which would let two processes corrupt a file).

// errFlockUnsupported is the leaf cause reported on a platform that provides no
// advisory file locking. It is a sentinel with no context fields.
var errFlockUnsupported = errors.New("fsstore: advisory file locking is not supported on this platform")

// FlockError reports that acquiring or releasing a cross-process advisory lock
// failed. Op is "lock" or "unlock"; Cause carries the underlying syscall error
// (or errFlockUnsupported on a platform without advisory locking). Callers
// classify with errors.As(&FlockError), never by string.
type FlockError struct {
	Op    string
	Path  string
	Cause error
}

func (e *FlockError) Error() string {
	return "fsstore: flock " + e.Op + " " + strconv.Quote(e.Path) + ": " + e.Cause.Error()
}

// Unwrap exposes the underlying syscall error (or errFlockUnsupported).
func (e *FlockError) Unwrap() error { return e.Cause }
