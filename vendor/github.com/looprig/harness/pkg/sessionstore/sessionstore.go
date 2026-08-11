package sessionstore

import (
	"errors"
	"strconv"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/internal/pathutil"
	"github.com/looprig/storage"
)

// defaultOffloadThreshold is the payload size, in bytes, above which a record
// is offloaded to the blob store rather than inlined in the ledger (512 KiB). It sits
// comfortably under storage's 1 MiB per-record ceiling, leaving headroom for the
// envelope framing around the payload.
const defaultOffloadThreshold = 512 * 1024

// sessionsPrefix is the leading name segment every session's backend locations share:
// its ledger is "sessions/<uuid>", its blobs live under "sessions/<uuid>/blobs/...".
const sessionsPrefix = "sessions/"

// Options are the resolved knobs a Store operates under. It is populated by Open from
// the defaults plus any Option overrides; callers never construct it directly.
type Options struct {
	// OffloadThreshold is the payload size (bytes) above which a record is
	// stored as an out-of-line blob instead of inline in the ledger.
	OffloadThreshold int
}

// Option overrides a single field of Options at Open time. Options are applied in
// order over the defaults, so a later Option wins over an earlier one.
type Option func(*Options)

// WithOffloadThreshold sets the large-record offload threshold in bytes. A
// non-positive value is ignored and the default is kept, so the option owns its
// invariant (a threshold must be positive) rather than trusting the caller.
func WithOffloadThreshold(n int) Option {
	return func(o *Options) {
		if n > 0 {
			o.OffloadThreshold = n
		}
	}
}

// InvalidBackendError reports that Open was handed a nil composite, or a composite
// with a nil primitive field. Missing names the absent piece ("composite", or one of
// "Ledger"/"Leaser"/"KV"/"Blobs") so the composition root knows exactly what was not
// wired. Open fails closed on it rather than dereferencing a nil primitive later.
type InvalidBackendError struct {
	Missing string
}

func (e *InvalidBackendError) Error() string {
	return "sessionstore: invalid backend: missing " + e.Missing
}

// Store is the session-scoped facade over a storage backend. It holds the assembled
// *storage.Composite (whose four primitives it addresses by field — they have
// colliding method names, so there is no flattened backend interface) plus the
// resolved Options. Construct it only via Open.
type Store struct {
	backend *storage.Composite
	opts    Options
}

// Open validates the backend and returns a Store over it. A nil composite or any nil
// primitive field is rejected with a typed *InvalidBackendError (fail closed, never a
// panic). Options are resolved from the 512 KiB default plus any overrides.
func Open(b *storage.Composite, opts ...Option) (*Store, error) {
	if b == nil {
		return nil, &InvalidBackendError{Missing: "composite"}
	}
	if b.Ledger == nil {
		return nil, &InvalidBackendError{Missing: "Ledger"}
	}
	if b.Leaser == nil {
		return nil, &InvalidBackendError{Missing: "Leaser"}
	}
	if b.KV == nil {
		return nil, &InvalidBackendError{Missing: "KV"}
	}
	if b.Blobs == nil {
		return nil, &InvalidBackendError{Missing: "Blobs"}
	}

	resolved := Options{OffloadThreshold: defaultOffloadThreshold}
	for _, opt := range opts {
		opt(&resolved)
	}
	return &Store{backend: b, opts: resolved}, nil
}

// PersistencePaths returns the canonical local roots reported by the Store's
// configured primitives. Providers without the optional storage.PathReporter
// capability contribute no paths. It fails closed with *PersistencePathError
// when a reported path cannot be resolved without ambiguity.
func (s *Store) PersistencePaths() ([]string, error) {
	var reported []string
	if reporter, ok := s.backend.Ledger.(storage.PathReporter); ok {
		reported = append(reported, reporter.StoragePaths()...)
	}
	if reporter, ok := s.backend.Leaser.(storage.PathReporter); ok {
		reported = append(reported, reporter.StoragePaths()...)
	}
	if reporter, ok := s.backend.KV.(storage.PathReporter); ok {
		reported = append(reported, reporter.StoragePaths()...)
	}
	if reporter, ok := s.backend.Blobs.(storage.PathReporter); ok {
		reported = append(reported, reporter.StoragePaths()...)
	}
	paths, err := pathutil.Canonicalize(reported)
	if err != nil {
		return nil, newPersistencePathError(err)
	}
	return paths, nil
}

// PersistencePathError reports a local persistence path that could not be
// canonicalized without ambiguity.
type PersistencePathError struct {
	Path  string
	Cause error
}

func (e *PersistencePathError) Error() string {
	return "sessionstore: resolve persistence path " + strconv.Quote(e.Path) + ": " + e.Cause.Error()
}

func (e *PersistencePathError) Unwrap() error { return e.Cause }

func newPersistencePathError(err error) *PersistencePathError {
	var pathErr *pathutil.CanonicalPathError
	if errors.As(err, &pathErr) {
		return &PersistencePathError{Path: pathErr.Path, Cause: err}
	}
	return &PersistencePathError{Cause: err}
}

// ledgerName derives the storage ledger name for a session: "sessions/<uuid>". The
// uuid renders as lowercase hex with hyphens, which is a canonical storage name.
func ledgerName(id uuid.UUID) string {
	return sessionsPrefix + id.String()
}

// sessionName derives the ledger name for id and confirms it is a valid storage
// name. A uuid always yields a canonical name, so the error is defensive — returning
// it keeps the derivation honest rather than silently assuming validity.
func sessionName(id uuid.UUID) (string, error) {
	name := ledgerName(id)
	if err := storage.ValidateName(name); err != nil {
		return "", err
	}
	return name, nil
}
