package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/looprig/fsstore"
	"github.com/looprig/storage"
	"github.com/looprig/storage/memstore"
	"github.com/looprig/storage/storetest"
)

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
		return newBoundedBlobs(fs.Backend().Blobs)
	})
}

// A provider that already conforms must be handed back untouched, so a conforming
// backend pays neither the extra copy nor the pump goroutine.
func TestNewBoundedBlobsPassesThroughConformingProvider(t *testing.T) {
	t.Parallel()
	fs, err := fsstore.Open(fsstore.Options{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("fsstore.Open: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })

	wrapped := newBoundedBlobs(fs.Backend().Blobs)
	if _, isWrapper := wrapped.(*boundedBlobs); !isWrapper {
		t.Fatal("precondition: fsstore Blobs was expected to need wrapping")
	}
	if again := newBoundedBlobs(wrapped); again != wrapped {
		t.Errorf("re-wrapping an already-conforming provider returned %T, want the same value", again)
	}
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
	bounded := newBoundedBlobs(provider)

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
		if !errors.Is(err, errBlobReaderClosed) {
			t.Errorf("blocked Read error = %v, want errBlobReaderClosed", err)
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
	bounded := newBoundedBlobs(fs.Backend().Blobs)

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
	bounded := newBoundedBlobs(fs.Backend().Blobs)
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

// Delete and List must reach the wrapped provider unchanged.
func TestBoundedBlobsDelegatesDeleteAndList(t *testing.T) {
	t.Parallel()
	fs, err := fsstore.Open(fsstore.Options{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("fsstore.Open: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })
	bounded := newBoundedBlobs(fs.Backend().Blobs)

	ctx := context.Background()
	for _, key := range []string{"blobs/a", "blobs/b"} {
		if err := bounded.Put(ctx, key, bytes.NewReader([]byte(key))); err != nil {
			t.Fatalf("Put(%q): %v", key, err)
		}
	}
	keys, err := bounded.List(ctx, "blobs/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("List returned %v, want 2 keys", keys)
	}
	if err := bounded.Delete(ctx, "blobs/a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	keys, err = bounded.List(ctx, "blobs/")
	if err != nil {
		t.Fatalf("List after Delete: %v", err)
	}
	if len(keys) != 1 || keys[0] != "blobs/b" {
		t.Errorf("List after Delete = %v, want [blobs/b]", keys)
	}
}

// A missing Blobs provider must keep sessionstore's typed "missing Blobs" diagnosis
// rather than being wrapped into a value that passes the nil check and panics later.
func TestOpenStoresReportsMissingBlobsRatherThanWrappingNil(t *testing.T) {
	t.Parallel()
	if got := newBoundedBlobs(nil); got != nil {
		t.Fatalf("newBoundedBlobs(nil) = %#v, want nil", got)
	}

	base := memstore.New()
	backend := &storage.Composite{Ledger: base.Ledger, Leaser: base.Leaser, KV: base.KV, OrderedIndex: base.OrderedIndex}
	_, err := openStores(backend)
	if err == nil {
		t.Fatal("openStores() with no Blobs provider succeeded, want a typed backend error")
	}
	if !strings.Contains(err.Error(), "missing Blobs") {
		t.Errorf("openStores() error = %v, want it to name the missing Blobs component", err)
	}
}
