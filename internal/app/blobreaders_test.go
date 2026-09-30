package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/looprig/fsstore"
	"github.com/looprig/storage"
	"github.com/looprig/storage/memstore"
	"github.com/looprig/storage/storetest"
)

// Carbon backs every SessionStore it opens with an fsstore root adapted by
// storage.WithBoundedBlobReaders (storage v0.9.0), which replaced Carbon's private
// wrapper of the same design. The adapter is storage's and is tested there; these
// cases hold the properties CARBON's deployment relies on, over the fsstore Blobs it
// actually adapts: the bound is real while a provider read is stuck, a complete
// stream still ends in io.EOF, truncation never passes for completion, and the blob
// root stays in carbon's reported persistence paths.

// boundedFor adapts b the way Carbon's composition roots do.
func boundedFor(t *testing.T, b storage.Blobs) storage.BlobReaderLifecycle {
	t.Helper()
	bounded, err := storage.WithBoundedBlobReaders(b)
	if err != nil {
		t.Fatalf("storage.WithBoundedBlobReaders: %v", err)
	}
	return bounded
}

// The shared suite is the contract check. It exercises concurrency but, as its own doc
// notes, cannot create genuinely blocked provider I/O — that is what
// TestBoundedBlobsCloseIsBoundedWhileProviderReadIsBlocked below supplies.
func TestBoundedBlobsConformance(t *testing.T) {
	storetest.TestBlobReaderLifecycle(t, func(t *testing.T) storage.BlobReaderLifecycle {
		t.Helper()
		fs, err := fsstore.Open(fsstore.Options{Root: t.TempDir()})
		if err != nil {
			t.Fatalf("fsstore.Open: %v", err)
		}
		t.Cleanup(func() { _ = fs.Close() })
		return boundedFor(t, fs.Backend().Blobs)
	})
}

// blockingBlobs is a Blobs provider whose Get reader parks in Read until released — the
// deterministic stand-in for the uncancellable filesystem read (a wedged FUSE or NFS
// mount) that motivates the whole wrapper. It is the case fsstore refuses to make a
// promise about.
type blockingBlobs struct {
	storage.Blobs
	release  chan struct{}
	readDone chan struct{}
}

func (b *blockingBlobs) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	return &blockingReader{release: b.release, readDone: b.readDone}, nil
}

type blockingReader struct {
	release  chan struct{}
	readDone chan struct{}
	served   bool
}

// Read serves one byte first — so the consumer's pipe has data and the pump advances —
// then parks forever on the second call until release is closed.
func (r *blockingReader) Read(p []byte) (int, error) {
	if !r.served {
		r.served = true
		if len(p) > 0 {
			p[0] = 'x'
			return 1, nil
		}
		return 0, nil
	}
	<-r.release
	close(r.readDone)
	return 0, io.EOF
}

func (r *blockingReader) Close() error { return nil }

// The load-bearing property: with a provider read genuinely stuck, Close must still
// return within the declared bound and the consumer's blocked Read must terminate with a
// non-EOF error. Before the wrapper this is precisely what could not be guaranteed.
func TestBoundedBlobsCloseIsBoundedWhileProviderReadIsBlocked(t *testing.T) {
	t.Parallel()
	fs, err := fsstore.Open(fsstore.Options{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("fsstore.Open: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })

	release := make(chan struct{})
	readDone := make(chan struct{})
	provider := &blockingBlobs{Blobs: fs.Backend().Blobs, release: release, readDone: readDone}
	bounded := boundedFor(t, provider)

	rc, err := bounded.Get(context.Background(), "blobs/stuck")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	// Drain the one served byte so the pump is parked inside the provider's second Read.
	one := make([]byte, 1)
	if n, err := rc.Read(one); n != 1 || err != nil {
		t.Fatalf("first Read = (%d, %v), want (1, nil)", n, err)
	}

	readErr := make(chan error, 1)
	go func() {
		_, err := rc.Read(make([]byte, 8))
		readErr <- err
	}()

	closed := make(chan error, 1)
	start := time.Now()
	go func() { closed <- rc.Close() }()

	select {
	case err := <-closed:
		if elapsed := time.Since(start); elapsed > bounded.BlobReaderCloseBound() {
			t.Errorf("Close took %v, exceeding the declared bound %v", elapsed, bounded.BlobReaderCloseBound())
		}
		if err != nil {
			t.Errorf("Close = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return while a provider Read was blocked — the bound is not real")
	}

	select {
	case err := <-readErr:
		if err == nil || errors.Is(err, io.EOF) {
			t.Errorf("blocked Read terminated with %v, want a non-EOF error", err)
		}
		var closedErr *storage.BlobReaderClosedError
		if !errors.As(err, &closedErr) {
			t.Errorf("blocked Read error = %v, want *storage.BlobReaderClosedError", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("blocked Read did not terminate after Close returned")
	}

	// The abandoned pump must unwind on its own once the provider read finally completes,
	// rather than staying parked forever.
	close(release)
	select {
	case <-readDone:
	case <-time.After(5 * time.Second):
		t.Fatal("provider read never completed after release")
	}
}

// A stream read through to EOF must still see a genuine io.EOF, because sessionstore
// establishes object integrity only by reaching one — the wrapper must not convert a
// complete stream into an error or truncate it.
func TestBoundedBlobsRoundTripsThroughEOF(t *testing.T) {
	t.Parallel()
	fs, err := fsstore.Open(fsstore.Options{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("fsstore.Open: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })
	bounded := boundedFor(t, fs.Backend().Blobs)

	payload := bytes.Repeat([]byte("carbon blob payload "), 5000)
	ctx := context.Background()
	if err := bounded.Put(ctx, "blobs/roundtrip", bytes.NewReader(payload)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	rc, err := bounded.Get(ctx, "blobs/roundtrip")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Errorf("Close after full read = %v, want nil", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("round trip returned %d bytes, want %d", len(got), len(payload))
	}
}

type readFailureBlobs struct {
	storage.Blobs
	readErr error
}

func (b readFailureBlobs) Get(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(&readFailureReader{err: b.readErr}), nil
}

type readFailureReader struct {
	err  error
	sent bool
}

func (r *readFailureReader) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(p, "partial blob"), nil
	}
	return 0, r.err
}

func TestBoundedBlobsPropagatesProviderReadError(t *testing.T) {
	t.Parallel()
	providerErr := errors.New("provider read failed")
	bounded := boundedFor(t, readFailureBlobs{readErr: providerErr})
	rc, err := bounded.Get(context.Background(), "blobs/broken")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if string(data) != "partial blob" || !errors.Is(err, providerErr) {
		t.Fatalf("ReadAll = (%q, %v), want partial bytes and provider error", data, err)
	}
}

// The bound sessionstore.Open reads is storage's declared ceiling for the adapter,
// not a number Carbon chooses; it was Carbon's own 1s before storage published it.
func TestBoundedBlobsDeclareStoragesCloseBound(t *testing.T) {
	t.Parallel()
	if got := boundedFor(t, readFailureBlobs{}).BlobReaderCloseBound(); got != storage.BoundedBlobReaderCloseBound {
		t.Fatalf("BlobReaderCloseBound = %v, want storage.BoundedBlobReaderCloseBound (%v)", got, storage.BoundedBlobReaderCloseBound)
	}
	if storage.BoundedBlobReaderCloseBound > time.Second {
		t.Errorf("storage.BoundedBlobReaderCloseBound = %v; Carbon's wrapper promised 1s and nothing here was re-measured against a looser bound", storage.BoundedBlobReaderCloseBound)
	}
}

// closeFailureBlobs serves a complete stream whose reader then fails to Close.
type closeFailureBlobs struct {
	storage.Blobs
	closeErr error
}

func (b closeFailureBlobs) Get(context.Context, string) (io.ReadCloser, error) {
	return closeFailureReader{Reader: strings.NewReader("complete blob"), err: b.closeErr}, nil
}

type closeFailureReader struct {
	io.Reader
	err error
}

func (r closeFailureReader) Close() error { return r.err }

// BEHAVIOUR CHANGE with storage's adapter: a provider reader whose Close fails after
// a complete read now fails the stream (the error is delivered in place of io.EOF).
// Carbon's private wrapper dropped that error and reported a clean EOF, so a stream
// whose release failed passed for complete.
func TestBoundedBlobsSurfaceACloseErrorAfterACompleteRead(t *testing.T) {
	t.Parallel()
	closeErr := errors.New("provider close failed")
	rc, err := boundedFor(t, closeFailureBlobs{closeErr: closeErr}).Get(context.Background(), "blobs/close-fails")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if string(data) != "complete blob" || !errors.Is(err, closeErr) {
		t.Fatalf("ReadAll = (%q, %v), want every byte and then the provider's Close error, not io.EOF", data, err)
	}
}

// A wrapper must not narrow what it wraps: harness's PersistencePaths type-asserts
// storage.PathReporter on the Blobs field, so losing it would silently drop the blob root
// from carbon's reported persistence paths.
func TestBoundedBlobsForwardsPathReporter(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	fs, err := fsstore.Open(fsstore.Options{Root: root})
	if err != nil {
		t.Fatalf("fsstore.Open: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })

	inner, ok := fs.Backend().Blobs.(storage.PathReporter)
	if !ok {
		t.Skip("fsstore Blobs no longer reports paths; nothing to forward")
	}
	bounded := boundedFor(t, fs.Backend().Blobs)
	reporter, ok := bounded.(storage.PathReporter)
	if !ok {
		t.Fatal("wrapped Blobs no longer implements storage.PathReporter")
	}
	want, got := inner.StoragePaths(), reporter.StoragePaths()
	if len(got) != len(want) {
		t.Fatalf("StoragePaths = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("StoragePaths[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// A missing Blobs provider must be reported as missing rather than wrapped into a
// value that passes the nil check and panics later. storage's adapter names it with
// *storage.IncompleteCompositeError, where sessionstore.Open used to.
func TestOpenStoresReportsMissingBlobsRatherThanWrappingNil(t *testing.T) {
	t.Parallel()
	if got, err := storage.WithBoundedBlobReaders(nil); got != nil || err == nil {
		t.Fatalf("storage.WithBoundedBlobReaders(nil) = (%#v, %v), want a refusal", got, err)
	}

	base := memstore.New()
	backend := &storage.Composite{Ledger: base.Ledger, Leaser: base.Leaser, KV: base.KV, OrderedIndex: base.OrderedIndex}
	_, err := openStores(backend)
	if err == nil {
		t.Fatal("openStores() with no Blobs provider succeeded, want a typed backend error")
	}
	var incomplete *storage.IncompleteCompositeError
	if !errors.As(err, &incomplete) || !slices.Contains(incomplete.Missing, "Blobs") {
		t.Errorf("openStores() error = %v, want *storage.IncompleteCompositeError naming Blobs", err)
	}
}
