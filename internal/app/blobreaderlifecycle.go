package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/looprig/storage"
)

// sessionstore.Open requires its Blobs provider to implement
// storage.BlobReaderLifecycle: Close must bound both itself and any active Read, because
// a session object stream is closed while a provider Read may still be in flight and the
// store then waits for that read to land. fsstore deliberately declines the capability —
// portable regular-file I/O has no deadline support, so a kernel, network-filesystem or
// FUSE Read cannot be force-cancelled, and fsstore will not promise a bound it cannot
// keep for every deployment.
//
// boundedBlobs supplies that bound for THIS deployment by changing who owns the blocking
// read. Get hands the caller the read half of an io.Pipe and copies the provider's reader
// into the write half on an owned goroutine. Close closes the pipe, which unblocks the
// caller's Read immediately; the provider read is abandoned rather than waited on, and
// the pump goroutine releases the provider reader whenever that read eventually returns.
//
// The honest cost is that abandonment: on a wedged mount, one goroutine and one file
// descriptor may outlive Close. Carbon runs against a local directory it owns, where a
// regular-file read completes in microseconds, so trading an unbounded Close for a
// bounded one plus a bounded-in-practice leak is the right call HERE. It is a deployment
// decision, which is why it lives in carbon rather than being pushed into fsstore, whose
// refusal is correct for the general case.
//
// The wrapper adds one copy through the pipe. Session objects are spilled tool results,
// not bulk data, and the copy is amortized over 32KiB chunks.

// blobReaderCloseBound is the declared ceiling for Close and for an active Read to
// return once Close begins. The real cost is a pipe close plus a mutex, i.e. microseconds
// and no I/O; a full second is a deliberately generous ceiling rather than an estimate,
// since the contract asks for a bound that always holds and never for a tight one.
const blobReaderCloseBound = time.Second

// errBlobReaderClosed is the terminal error a Read observes after Close. The contract
// requires a non-nil, non-io.EOF error so a caller can distinguish a truncated stream
// from a complete one — sessionstore establishes object integrity only by reading
// through to a genuine EOF, so this error must never be mistakable for one.
//
// It wraps io.ErrClosedPipe because that is what io.PipeReader actually reports to a
// blocked Read (PipeReader.CloseWithError sets the error seen by the WRITER, not the
// reader). Wrapping keeps both predicates true — a caller may test either — while giving
// logs a message that names carbon and the cause rather than a bare "io: read/write on
// closed pipe".
var errBlobReaderClosed = fmt.Errorf("carbon: blob reader closed: %w", io.ErrClosedPipe)

// boundedBlobs wraps a Blobs provider and adds storage.BlobReaderLifecycle. Every method
// other than Get delegates unchanged.
type boundedBlobs struct {
	inner storage.Blobs
}

// newBoundedBlobs adapts inner. A provider that already implements the capability is
// returned unchanged, so wrapping a conforming backend (memstore, s3store, natsstore)
// costs nothing and does not insert a needless copy.
func newBoundedBlobs(inner storage.Blobs) storage.BlobReaderLifecycle {
	// A nil provider stays nil rather than becoming a wrapper around nothing: wrapping it
	// would satisfy sessionstore.Open's non-nil Blobs check and turn its typed
	// InvalidBackendError{Component: "Blobs"} into a nil dereference at first use.
	if inner == nil {
		return nil
	}
	if lifecycle, ok := inner.(storage.BlobReaderLifecycle); ok {
		return lifecycle
	}
	return &boundedBlobs{inner: inner}
}

var _ storage.BlobReaderLifecycle = (*boundedBlobs)(nil)

func (b *boundedBlobs) Put(ctx context.Context, key string, r io.Reader) error {
	return b.inner.Put(ctx, key, r)
}

func (b *boundedBlobs) Delete(ctx context.Context, key string) error {
	return b.inner.Delete(ctx, key)
}

func (b *boundedBlobs) List(ctx context.Context, prefix string) ([]string, error) {
	return b.inner.List(ctx, prefix)
}

// BlobReaderCloseBound reports the declared ceiling. It is a constant: the bound is a
// property of the pipe handoff, not of the wrapped provider, which is precisely why
// wrapping can promise one where the provider could not.
func (b *boundedBlobs) BlobReaderCloseBound() time.Duration { return blobReaderCloseBound }

// StoragePaths forwards the wrapped provider's local roots when it reports any.
// harness's Store.PersistencePaths type-asserts storage.PathReporter on the Blobs field,
// so a wrapper that did not forward it would silently drop the blob root from carbon's
// reported persistence paths — a wrapper must not narrow the capabilities of what it
// wraps.
func (b *boundedBlobs) StoragePaths() []string {
	reporter, ok := b.inner.(storage.PathReporter)
	if !ok {
		return nil
	}
	return reporter.StoragePaths()
}

// Get returns a reader whose Close is bounded. The provider reader is opened eagerly, so
// a missing key or a permission failure is still reported by Get rather than deferred
// into the first Read.
func (b *boundedBlobs) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	src, err := b.inner.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	go pumpBlob(src, pw)
	return &boundedBlobReader{pipe: pr}, nil
}

// pumpBlob copies src into pw and then releases both ends.
//
// A normal completion closes pw with a nil error, so the consumer sees a genuine io.EOF
// and integrity verification through EOF still works. A copy failure — including the
// io.ErrClosedPipe raised when the consumer closed early — is published to the consumer
// through CloseWithError.
//
// src.Close runs after the copy returns, so it is the ABANDONED path: when the consumer
// closes first, this goroutine stays parked in src.Read until that read completes on its
// own, then unwinds. That is the leak the wrapper trades for a bounded Close, and it is
// bounded in practice by local-disk read latency.
func pumpBlob(src io.ReadCloser, pw *io.PipeWriter) {
	_, copyErr := io.Copy(pw, src)
	_ = pw.CloseWithError(copyErr)
	_ = src.Close()
}

// boundedBlobReader is the consumer-facing stream: the read half of the pipe plus a
// latched Close.
//
// io.PipeReader already makes Read and Close safe to call concurrently and guarantees
// that a Read blocked at the moment of Close returns immediately afterwards, which is the
// property the whole wrapper exists to obtain.
type boundedBlobReader struct {
	pipe *io.PipeReader

	// once and closeErr latch the first Close's outcome. The contract requires a stable
	// success/failure classification across repeated calls, so every later Close must
	// report exactly what the first one did rather than re-running the teardown.
	once     sync.Once
	closeErr error
}

// Read translates the pipe's post-close signal into the named terminal error. io.Pipe
// reports io.ErrClosedPipe to a Read once the read half is closed; errBlobReaderClosed
// wraps it, so this narrows nothing and only makes the cause legible. A complete stream
// still ends in a genuine io.EOF, which is untouched here.
func (r *boundedBlobReader) Read(p []byte) (int, error) {
	n, err := r.pipe.Read(p)
	if errors.Is(err, io.ErrClosedPipe) {
		return n, errBlobReaderClosed
	}
	return n, err
}

// Close unblocks the consumer and publishes errBlobReaderClosed to any Read that is
// blocked now or attempted later. It does NOT wait for the pump goroutine: waiting is
// exactly the unbounded step this wrapper removes.
func (r *boundedBlobReader) Close() error {
	r.once.Do(func() {
		r.closeErr = r.pipe.CloseWithError(errBlobReaderClosed)
	})
	return r.closeErr
}
