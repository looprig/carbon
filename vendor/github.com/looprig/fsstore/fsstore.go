package fsstore

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/looprig/storage"
)

// This file is fsstore's composition root: it wires the four filesystem backends
// (ledger, leaser, KV, blobs) under one store root into a single storage
// field-bundle and exposes lifecycle (Open/Close).
//
// # Why a field-bundle, not an all-four interface
//
// storage's four primitives collide on method names — each of Ledger, KV, and
// Blobs has its own Delete, for instance — so NO single Go type can implement all
// four at once. storage solves this with Composite, a struct that EMBEDS one
// provider per primitive; a caller reaches each as a field (c.Ledger, c.KV, …).
// Store follows the same shape: it embeds *storage.Composite rather than
// pretending to satisfy Ledger+Leaser+KV+Blobs itself. A consumer that wants the
// whole bundle (sessionstore.Open, say) is handed store.Composite (or Backend()).

// errRootNotDir is the leaf cause recorded in an *OptionsError when Root already
// exists on disk but is not a directory, so the store cannot be laid out under it.
var errRootNotDir = errors.New("fsstore: store root exists but is not a directory")

// Options configures Open. Root is the single directory under which the store
// lays out its four backends (streams/, leases/, kv/, blobs/); it is required.
type Options struct {
	// Root is the store root directory. It is created at 0700 if absent. Required:
	// an empty Root is rejected with an *OptionsError.
	Root string
}

// OptionsError reports an invalid or unusable Open option. Field names the option
// (currently always "Root") and Reason explains the fault; Cause carries the
// underlying filesystem error (or errRootNotDir) when the fault was not a plain
// validation failure, and is exposed via Unwrap for errors.Is/As.
type OptionsError struct {
	Field  string
	Reason string
	Cause  error
}

func (e *OptionsError) Error() string {
	msg := "fsstore: invalid option " + strconv.Quote(e.Field) + ": " + e.Reason
	if e.Cause != nil {
		return msg + ": " + e.Cause.Error()
	}
	return msg
}

// Unwrap exposes the underlying cause (a filesystem error or errRootNotDir), or
// nil for a pure validation failure such as an empty Root.
func (e *OptionsError) Unwrap() error { return e.Cause }

// Store is an open fsstore rooted at a single directory on the local filesystem.
// It embeds *storage.Composite, so a caller reaches each primitive as a promoted
// field — store.Ledger, store.Leaser, store.KV, store.Blobs — and hands the whole
// bundle to a consumer as store.Composite or via Backend. The four primitives
// collide on method names, so no single type can implement all four (see the
// file-level comment); embedding the field-bundle is how Store sidesteps that.
type Store struct {
	*storage.Composite
	root string

	// ledger is retained as its concrete type solely so Close can release the
	// backend's in-process state (its cached name->file registry). The other three
	// backends hold no releasable long-lived state.
	ledger *ledgerStore

	mu     sync.Mutex
	closed bool
}

// Open assembles a filesystem-backed storage bundle under opts.Root. It requires
// a non-empty Root (empty -> *OptionsError), creates it at 0700, and verifies it
// resolves to a directory. It then wires the four backends under that root —
// <root>/streams, <root>/leases, <root>/kv, <root>/blobs, each created at 0700 by
// its constructor — and bundles them with storage.NewComposite.
//
// On any backend-construction failure Open returns that backend's typed error and
// no Store; because every fs backend only creates directories (none holds an open
// file descriptor until first use), a partial failure leaks no handles and needs
// no unwind.
func Open(opts Options) (*Store, error) {
	if opts.Root == "" {
		return nil, &OptionsError{Field: "Root", Reason: "must not be empty"}
	}
	root := filepath.Clean(opts.Root)
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, &OptionsError{Field: "Root", Reason: "resolve absolute store root", Cause: err}
	}
	switch info, serr := os.Stat(root); {
	case serr == nil:
		// Root already exists: it must be a directory to hold the backend layout.
		if !info.IsDir() {
			return nil, &OptionsError{Field: "Root", Reason: "store root is not a directory", Cause: errRootNotDir}
		}
	case errors.Is(serr, fs.ErrNotExist):
		// Root absent: create it (and any missing parents) at 0700.
		if mkErr := os.MkdirAll(root, 0o700); mkErr != nil {
			return nil, &OptionsError{Field: "Root", Reason: "create store root", Cause: mkErr}
		}
	default:
		return nil, &OptionsError{Field: "Root", Reason: "stat store root", Cause: serr}
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, &OptionsError{Field: "Root", Reason: "resolve store root symlinks", Cause: err}
	}

	ledger, err := newLedgerStore(root)
	if err != nil {
		return nil, err
	}
	leaser, err := newLeaserStore(root)
	if err != nil {
		return nil, err
	}
	kv, err := newKVStore(root)
	if err != nil {
		return nil, err
	}
	blobs, err := newBlobStore(root)
	if err != nil {
		return nil, err
	}

	composite, err := storage.NewComposite(ledger, leaser, kv, blobs)
	if err != nil {
		return nil, err
	}
	return &Store{Composite: composite, root: root, ledger: ledger}, nil
}

// Backend returns the assembled four-primitive bundle to hand to a consumer such
// as sessionstore.Open. It is the embedded *storage.Composite; callers may read
// store.Composite directly instead.
func (s *Store) Backend() *storage.Composite { return s.Composite }

// StoragePaths reports the canonical local root that contains every primitive.
// The result is newly allocated so callers cannot mutate subsequent reports.
func (s *Store) StoragePaths() []string { return providerPaths(s.root) }

func (s *ledgerStore) StoragePaths() []string { return providerPaths(s.root) }

func (s *leaserStore) StoragePaths() []string { return providerPaths(s.root) }

func (s *kvStore) StoragePaths() []string { return providerPaths(s.root) }

func (s *blobStore) StoragePaths() []string { return providerPaths(s.root) }

func providerPaths(root string) []string { return []string{root} }

// Close releases the store's in-process state. Concretely it drops the ledger
// backend's cached name->file registry (the tip/size metadata retained after the
// last write). The fs backends hold no long-lived file descriptors of their own —
// the ledger opens and closes its fd within each Append/Delete (the per-append
// advisory-lock model), the KV and Blobs writers do the same, and a Leaser grant's
// fd is owned and released by the caller via Lease.Release — so there are no open
// handles for Close to reclaim beyond that in-memory registry.
//
// Close is idempotent: a second call is a no-op returning nil. After Close the
// Store must not be reused; open a fresh Store on the same root to use it again.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.ledger.Close()
}
