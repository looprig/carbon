package stream

import (
	"errors"
	"io"
	"sync"
)

// StreamReader is a pull-based iterator over streaming values of type T.
// Call Next to advance; it returns (zero, io.EOF) when the stream is exhausted.
// Always call Close when done — even after io.EOF — to release the underlying connection.
type StreamReader[T any] struct {
	next        func() (T, error)
	close       func() error
	producer    StreamResultProducer
	nextMu      sync.Mutex
	stateMu     sync.RWMutex
	state       streamState
	terminalErr error
	result      StreamResult
	hasResult   bool
	closeOnce   sync.Once
	closeErr    error
}

type streamState uint8

const (
	streamStateActive streamState = iota
	streamStateCleanEOF
	streamStateFailed
)

// NewStreamReader constructs a StreamReader from a next function and an optional
// closer. If closer is nil, Close is a no-op.
// next must return (zero, io.EOF) when the stream is exhausted.
func NewStreamReader[T any](next func() (T, error), closer func() error) *StreamReader[T] {
	return NewStreamReaderWithResult(next, closer, nil)
}

// NewStreamReaderWithResult constructs a reader with a narrow terminal-result
// producer. The producer is not consulted until Next observes clean EOF. Its
// state may therefore accumulate while the stream is active; the reader takes
// an immutable snapshot at EOF and returns a fresh Usage copy on every Result.
func NewStreamReaderWithResult[T any](next func() (T, error), closer func() error, producer StreamResultProducer) *StreamReader[T] {
	if closer == nil {
		closer = func() error { return nil }
	}
	return &StreamReader[T]{next: next, close: closer, producer: producer}
}

// Next returns the next value. Returns (zero, io.EOF) when exhausted. Calls to
// Next are serialized; the underlying next function is never invoked
// concurrently by StreamReader. Close is deliberately not serialized behind a
// blocking Next so it can interrupt I/O. An underlying next/closer pair used
// concurrently must honor that contract itself.
func (r *StreamReader[T]) Next() (T, error) {
	var zero T
	if r == nil {
		return zero, &StreamReaderError{Operation: StreamOperationNext, Failure: StreamReaderFailureNilReceiver}
	}
	r.nextMu.Lock()
	defer r.nextMu.Unlock()

	if terminalErr, done := r.terminalOutcome(); done {
		return zero, terminalErr
	}
	if r.next == nil {
		err := &StreamReaderError{Operation: StreamOperationNext, Failure: StreamReaderFailureMissingNext}
		r.fail(err)
		return zero, err
	}
	value, err := r.next()
	if err == nil {
		return value, nil
	}
	if !errors.Is(err, io.EOF) {
		r.fail(err)
		return zero, err
	}
	return zero, r.complete()
}

func (r *StreamReader[T]) terminalOutcome() (error, bool) {
	r.stateMu.RLock()
	defer r.stateMu.RUnlock()
	switch r.state {
	case streamStateCleanEOF:
		return io.EOF, true
	case streamStateFailed:
		return r.terminalErr, true
	default:
		return nil, false
	}
}

func (r *StreamReader[T]) complete() error {
	result, ok, resultErr := readStreamResult(r.producer)
	if resultErr != nil {
		r.fail(resultErr)
		return resultErr
	}
	r.stateMu.Lock()
	r.state = streamStateCleanEOF
	r.result = result
	r.hasResult = ok
	r.stateMu.Unlock()
	return io.EOF
}

func (r *StreamReader[T]) fail(err error) {
	r.stateMu.Lock()
	r.state = streamStateFailed
	r.terminalErr = err
	r.result = StreamResult{}
	r.hasResult = false
	r.stateMu.Unlock()
}

// Result returns a fresh copy of authoritative terminal metadata. It is false
// before clean EOF, after any non-EOF failure, or when no producer result exists.
func (r *StreamReader[T]) Result() (StreamResult, bool) {
	if r == nil {
		return StreamResult{}, false
	}
	r.stateMu.RLock()
	defer r.stateMu.RUnlock()
	if r.state != streamStateCleanEOF || !r.hasResult {
		return StreamResult{}, false
	}
	return cloneStreamResult(r.result), true
}

// Close releases the underlying connection. It is idempotent: the wrapped close
// func runs at most once (guarded by a sync.Once), so a double Close never runs the
// closer twice; every call returns the first call's result.
func (r *StreamReader[T]) Close() error {
	if r == nil {
		return &StreamReaderError{Operation: StreamOperationClose, Failure: StreamReaderFailureNilReceiver}
	}
	r.closeOnce.Do(func() {
		if r.close != nil {
			r.closeErr = r.close()
		}
	})
	return r.closeErr
}
