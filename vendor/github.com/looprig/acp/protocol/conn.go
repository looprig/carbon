// conn.go implements Conn, the connection dispatcher that sits directly on
// top of the framing (framing.go) and envelope (jsonrpc.go) layers: it reads
// frames one at a time via FrameReader, classifies each via ParseEnvelope,
// and routes the result to the right place — a registered request handler,
// a registered notification handler, or the pending-call table for a
// correlated response. It writes outgoing requests/responses/notifications
// through the single-writer Writer.
//
// Conn knows nothing about ACP's specific methods or domain types: it routes
// purely by the JSON-RPC method string. The typed ACP surface built on top
// of this is Task 1.7's concern.
package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// MaxInFlightHandlers bounds how many request/notification handler callbacks
// may run concurrently on one Conn. Requests and notifications beyond this
// bound queue (as goroutines parked on a semaphore) until a slot frees.
const MaxInFlightHandlers = 64

// NotifyBufferDepth bounds how many notifications for one not-yet-registered
// method a Conn will buffer before HandleNotify is called for that method.
// Once the depth is exceeded, the oldest buffered notification is dropped to
// make room for the newest, and the drop is counted (see
// Conn.DroppedNotifications).
const NotifyBufferDepth = 512

// HandlerFunc handles one incoming JSON-RPC request and returns the value to
// send back as the response result, or an error. If err is (or wraps) a
// *Fault, its Code/Message/Data are sent to the peer via ToWireError;
// any other error is reported to the peer as InternalError, with the
// original error kept only for local diagnosis (never sent on the wire).
type HandlerFunc func(ctx context.Context, method string, params json.RawMessage) (any, error)

// NotifyFunc handles one incoming JSON-RPC notification. It has no result:
// notifications never receive a response.
type NotifyFunc func(ctx context.Context, method string, params json.RawMessage)

// NotifyWithSequenceFunc is the ordered form of NotifyFunc. The receive
// sequence is minted by Conn's single read loop before the notification is
// queued, so a handler can correlate its delivery with a response without
// creating another reader or racing a second dispatch path.
type NotifyWithSequenceFunc func(ctx context.Context, method string, params json.RawMessage, receiveSequence uint64)

// ConnOptions configures a Conn constructed by NewConn.
type ConnOptions struct {
	// ExtIDBase sets the starting id minted for extension-traffic calls (the
	// id space used internally for outgoing calls other than the ones Call
	// mints — see callExt). Zero means "use the default base of 1". This
	// exists so a later typed layer can mint extension-traffic ids from a
	// caller-chosen range without ever colliding with Call's id space, which
	// always starts at 1<<32 regardless of this option.
	ExtIDBase int64
}

// ConnClosedError is returned by Call and Notify once a Conn has been (or is
// being) closed, and delivered to every Call that was still in flight at the
// moment of closing. Close may have been explicit (Conn.Close) or implicit
// (the peer went away, or a read off the transport failed); Unwrap exposes
// that cause for local diagnosis, but it is never sent to a peer.
type ConnClosedError struct {
	cause error
}

func (e *ConnClosedError) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("protocol: connection closed: %v", e.cause)
	}
	return "protocol: connection closed"
}

// Unwrap exposes the cause that led to closing, if any.
func (e *ConnClosedError) Unwrap() error { return e.cause }

// callResult is what a pending Call is waiting to receive: either a decoded
// success (raw json result bytes) or a failure.
type callResult struct {
	raw             json.RawMessage
	err             error
	receiveSequence uint64
}

// CallResult contains ordered transport facts for one Conn call. The
// response sequence is zero only when no response was received (for example,
// cancellation or connection loss before the peer replied). WriteAdmitted is
// true once the request frame crossed Writer's admission boundary; it remains
// true for every later response, protocol error, timeout, or connection error.
type CallResult struct {
	WriteAdmitted    bool
	ResponseSequence uint64
	ReceiveSequence  uint64
}

// AsyncCallResult is the exactly-once completion of one asynchronous Call.
// Facts are retained even when Err is non-nil, including cancellation,
// connection closure, and peer faults.
type AsyncCallResult struct {
	Facts CallResult
	Err   error
}

// CallHandle is the minimal asynchronous request primitive used by typed ACP
// extension calls. Admission has capacity one and receives exactly one value
// (true at Writer queue admission, false when the request is rejected before
// admission) before closing. Result has capacity one and receives exactly one
// AsyncCallResult before closing. Cancel is idempotent and only cancels this
// call's observation; a request admitted to Writer remains eligible for its
// raw write.
type CallHandle struct {
	admission chan bool
	result    chan AsyncCallResult
	cancel    context.CancelFunc
}

// Admission reports the one writer-admission fact for this call.
func (h *CallHandle) Admission() <-chan bool {
	if h == nil {
		return nil
	}
	return h.admission
}

// Result reports the one final call completion.
func (h *CallHandle) Result() <-chan AsyncCallResult {
	if h == nil {
		return nil
	}
	return h.result
}

// Cancel stops waiting for this call's response. If Writer admission already
// happened, the resulting completion retains Facts.WriteAdmitted=true and the
// admitted frame is still drained by Writer.
func (h *CallHandle) Cancel() {
	if h != nil && h.cancel != nil {
		h.cancel()
	}
}

// ReceiveSequenceOverflowError reports that Conn exhausted its monotonic
// receive-sequence space. Conn fails closed before dispatching that inbound
// observation, so sequence zero remains reserved as the "not observed"
// sentinel and is never emitted on a response or notification.
type ReceiveSequenceOverflowError struct{}

func (e *ReceiveSequenceOverflowError) Error() string {
	return "protocol: receive sequence overflow"
}

type bufferedNotification struct {
	params          json.RawMessage
	receiveSequence uint64
}

// Conn is one bidirectional JSON-RPC connection. It routes incoming requests
// to registered handlers and correlates outgoing requests with their
// responses.
//
// A single internal goroutine reads frames off r and dispatches them; Send
// calls made by that dispatch (auto-responses, and any handler's eventual
// response) and by callers of Call/Notify all funnel through the shared
// Writer, which already serializes concurrent writers onto w.
type Conn struct {
	fr     *FrameReader
	writer *Writer
	r      io.Reader
	w      io.Writer

	// mu guards closed/closeErr/pending together so that "is this Conn
	// still open" and "register/remove a pending entry" are always
	// observed atomically with respect to shutdown's fail-all sweep: no
	// interleaving of Call and shutdown can produce a pending entry nobody
	// will ever resolve, nor a fail-all sweep that misses one.
	mu       sync.Mutex
	closed   bool
	closeErr *ConnClosedError
	pending  map[ID]chan callResult

	// handlersMu guards all handler registration state, including the
	// per-method notification buffers used before HandleNotify is called.
	handlersMu        sync.Mutex
	handlers          map[string]HandlerFunc
	notifyHandlers    map[string]NotifyFunc
	notifySeqHandlers map[string]NotifyWithSequenceFunc
	unknownRequest    HandlerFunc
	unknownNotify     NotifyFunc
	unknownNotifySeq  NotifyWithSequenceFunc
	notifyBuffers     map[string][]bufferedNotification

	droppedNotifications atomic.Uint64

	// nextCallID and nextExtID are disjoint id spaces for outgoing calls:
	// Call mints from nextCallID (starting at 1<<32), callExt mints from
	// nextExtID (starting at 1, or ConnOptions.ExtIDBase). Both are plain
	// incrementing counters; the actual collision-freedom comes from the
	// two ranges never overlapping, not from any coordination between them.
	nextCallID atomic.Int64
	nextExtID  atomic.Int64

	// sem is a counting semaphore (capacity MaxInFlightHandlers) that bounds
	// how many request handler callbacks run concurrently. A goroutine
	// spawned to run a handler blocks trying to send into sem until a slot
	// is free (or done closes), which is how "excess requests queue" is
	// implemented. Notification handlers do not use sem: see notifyMu et al.
	sem chan struct{}

	// notifyMu, notifyCond, notifyQueue, and notifyClosed implement an
	// unbounded FIFO job queue drained by a single dedicated goroutine
	// (notifyWorker), which is how notification handler *execution* (not
	// just invocation) is serialized in wire order. Both dispatchNotification
	// (live notifications) and HandleNotify (buffered-notification flush)
	// enqueue onto this same queue rather than running or spawning a handler
	// directly, so completion order always matches arrival order — including
	// across different notification methods, and including a buffered flush
	// relative to a live notification for the same method that arrives
	// concurrently with registration (see HandleNotify).
	notifyOrderMu sync.Mutex
	notifyMu      sync.Mutex
	notifyCond    *sync.Cond
	notifyQueue   []func()
	notifyClosed  bool

	notifyStateMu sync.Mutex
	notifyPending map[uint64]struct{}
	notifyChanged chan struct{}

	done         chan struct{}
	readLoopDone chan struct{}
	closeOnce    sync.Once

	receiveMu      sync.Mutex
	receiveThrough uint64
	// receiveDispatchPending covers the interval after an inbound
	// notification receives its sequence and before dispatchNotification has
	// recorded either a handler job or a buffer entry. WaitForNotificationsThrough
	// waits on this map so publication of receiveThrough cannot overtake that
	// registration boundary.
	receiveDispatchPending map[uint64]struct{}
	receiveChanged         chan struct{}
}

// NewConn wraps r and w as one JSON-RPC connection and starts its internal
// read loop. The returned Conn is ready for Handle/HandleNotify registration
// and for Call/Notify immediately.
func NewConn(r io.Reader, w io.Writer, opts ConnOptions) *Conn {
	c := &Conn{
		fr:                     NewFrameReader(r),
		writer:                 NewWriter(w),
		r:                      r,
		w:                      w,
		pending:                make(map[ID]chan callResult),
		handlers:               make(map[string]HandlerFunc),
		notifyHandlers:         make(map[string]NotifyFunc),
		notifySeqHandlers:      make(map[string]NotifyWithSequenceFunc),
		notifyBuffers:          make(map[string][]bufferedNotification),
		sem:                    make(chan struct{}, MaxInFlightHandlers),
		done:                   make(chan struct{}),
		readLoopDone:           make(chan struct{}),
		receiveChanged:         make(chan struct{}),
		notifyPending:          make(map[uint64]struct{}),
		notifyChanged:          make(chan struct{}),
		receiveDispatchPending: make(map[uint64]struct{}),
	}
	c.notifyCond = sync.NewCond(&c.notifyMu)

	// Call's id space starts at 1<<32 and counts up; storing base-1 lets
	// mintCallID uniformly use Add(1) to produce the first id.
	c.nextCallID.Store((int64(1) << 32) - 1)

	extBase := opts.ExtIDBase
	if extBase == 0 {
		extBase = 1
	}
	c.nextExtID.Store(extBase - 1)

	go c.readLoop()
	go c.notifyWorker()
	return c
}

// Done returns a channel that is closed once c has been (or is being)
// closed, whether by an explicit Close call or because reading from the
// peer failed.
func (c *Conn) Done() <-chan struct{} { return c.done }

// DroppedNotifications reports how many buffered early notifications have
// been dropped (oldest-first) across all methods due to NotifyBufferDepth
// overflow. Safe to call at any time, including after Close, and it reflects
// the final count once the Conn is closed.
func (c *Conn) DroppedNotifications() uint64 { return c.droppedNotifications.Load() }

// WaitForNotifications waits until all notification jobs enqueued before this
// call have finished executing in the Conn's ordered notification worker. It
// is cancellation-aware and returns *ConnClosedError if the Conn closes before
// the barrier reaches the worker. A caller should not invoke it from inside a
// notification handler, because that handler is itself ahead of the barrier.
func (c *Conn) WaitForNotifications(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.waitForNotificationMarker(ctx)
}

// WaitForNotificationsThrough waits until Conn has observed receiveSequence
// and every notification at or before that sequence has completed its
// registered handler. It never reads from a Session.Updates channel; the
// client layer's session handler has already placed the update in its own
// queue by the time this barrier completes.
func (c *Conn) WaitForNotificationsThrough(ctx context.Context, receiveSequence uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for {
		c.receiveMu.Lock()
		observed := c.receiveThrough >= receiveSequence
		dispatchPending := false
		if observed {
			for seq := range c.receiveDispatchPending {
				if seq <= receiveSequence {
					dispatchPending = true
					break
				}
			}
		}
		changed := c.receiveChanged
		c.receiveMu.Unlock()
		if observed && !dispatchPending {
			break
		}
		select {
		case <-changed:
		case <-c.done:
			return c.closedError()
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	// readLoop dispatches notifications under notifyOrderMu. Taking that
	// mutex here closes the small gap between observing receiveThrough and
	// recording the corresponding handler job in notifyPending. Recheck the
	// context after this potentially blocking ordering barrier so cancellation
	// remains authoritative even when no tracked job is pending.
	c.notifyOrderMu.Lock()
	ctxErr := ctx.Err()
	c.notifyOrderMu.Unlock()
	if ctxErr != nil {
		return ctxErr
	}
	for {
		c.notifyStateMu.Lock()
		pending := false
		for seq := range c.notifyPending {
			if seq <= receiveSequence {
				pending = true
				break
			}
		}
		changed := c.notifyChanged
		c.notifyStateMu.Unlock()
		if !pending {
			return nil
		}
		select {
		case <-changed:
		case <-c.done:
			return c.closedError()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// WaitForReceiveSequence is an additive name for
// WaitForNotificationsThrough. It is useful to callers that want to make
// clear that responses and notifications share one receive-order clock.
func (c *Conn) WaitForReceiveSequence(ctx context.Context, receiveSequence uint64) error {
	return c.WaitForNotificationsThrough(ctx, receiveSequence)
}

func (c *Conn) waitForNotificationMarker(ctx context.Context) error {

	markerDone := make(chan struct{})
	c.notifyOrderMu.Lock()
	c.notifyMu.Lock()
	if c.notifyClosed {
		c.notifyMu.Unlock()
		c.notifyOrderMu.Unlock()
		return c.closedError()
	}
	c.notifyQueue = append(c.notifyQueue, func() { close(markerDone) })
	c.notifyMu.Unlock()
	c.notifyOrderMu.Unlock()
	c.notifyCond.Signal()

	select {
	case <-markerDone:
		return nil
	case <-c.done:
		return c.closedError()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Conn) closedError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closeErr != nil {
		return c.closeErr
	}
	return &ConnClosedError{}
}

// Handle registers h as the handler for incoming requests with the given
// method. A later call with the same method replaces the previous handler.
func (c *Conn) Handle(method string, h HandlerFunc) {
	c.handlersMu.Lock()
	c.handlers[method] = h
	c.handlersMu.Unlock()
}

// HandleNotify registers h as the handler for incoming notifications with
// the given method, then enqueues — in arrival order — any notifications for
// that method that were buffered before this call (see NotifyBufferDepth)
// for ordered execution on the same dispatch worker that runs live
// notifications. HandleNotify does not block waiting for that flush to run;
// it only guarantees the flushed notifications are queued ahead of any live
// notification for method that could possibly be dispatched after this call
// returns.
//
// The registration and the flush-enqueue happen under the same handlersMu
// critical section, which is what guarantees the ordering: dispatchNotification
// cannot observe the new handler (and thus cannot enqueue a live notification
// for method) until this call releases handlersMu, by which point every
// buffered notification for method has already been pushed onto the shared
// queue ahead of it.
func (c *Conn) HandleNotify(method string, h NotifyFunc) {
	c.notifyOrderMu.Lock()
	defer c.notifyOrderMu.Unlock()
	c.handlersMu.Lock()
	c.notifyHandlers[method] = h
	delete(c.notifySeqHandlers, method)
	buffered := c.notifyBuffers[method]
	delete(c.notifyBuffers, method)
	for _, buffered := range buffered {
		buffered := buffered
		c.enqueueTrackedNotifyJob(buffered.receiveSequence, func() { h(context.Background(), method, buffered.params) })
	}
	c.handlersMu.Unlock()
}

// HandleNotifyWithSequence is HandleNotify's ordered form. The callback is
// invoked by the same single notification worker, with the receive sequence
// minted by Conn's read loop before enqueueing.
func (c *Conn) HandleNotifyWithSequence(method string, h NotifyWithSequenceFunc) {
	c.notifyOrderMu.Lock()
	defer c.notifyOrderMu.Unlock()
	c.handlersMu.Lock()
	c.notifySeqHandlers[method] = h
	delete(c.notifyHandlers, method)
	buffered := c.notifyBuffers[method]
	delete(c.notifyBuffers, method)
	for _, buffered := range buffered {
		buffered := buffered
		c.enqueueTrackedNotifyJob(buffered.receiveSequence, func() {
			h(context.Background(), method, buffered.params, buffered.receiveSequence)
		})
	}
	c.handlersMu.Unlock()
}

// HandleUnknownRequest registers a catch-all handler invoked for any
// incoming request whose method has no handler registered via Handle. This
// is meant for vendor/extension request methods (for example ACP's `_meta`
// passthrough) that this Conn does not know about by name.
func (c *Conn) HandleUnknownRequest(h HandlerFunc) {
	c.handlersMu.Lock()
	c.unknownRequest = h
	c.handlersMu.Unlock()
}

// HandleUnknownNotify registers a catch-all handler invoked for any incoming
// notification whose method has no handler registered via HandleNotify. Once
// this hook is set, it takes priority over buffering: an unrecognized
// notification method is routed to it immediately rather than being held for
// a HandleNotify registration that (by design, once this hook exists) is
// presumed never coming for that method.
func (c *Conn) HandleUnknownNotify(h NotifyFunc) {
	c.handlersMu.Lock()
	c.unknownNotify = h
	c.unknownNotifySeq = nil
	c.handlersMu.Unlock()
}

// HandleUnknownNotifyWithSequence registers the ordered catch-all variant of
// HandleUnknownNotify. It is primarily useful to protocol adapters that need
// to observe extension notifications without opening a second reader.
func (c *Conn) HandleUnknownNotifyWithSequence(h NotifyWithSequenceFunc) {
	c.handlersMu.Lock()
	c.unknownNotifySeq = h
	c.unknownNotify = nil
	c.handlersMu.Unlock()
}

// Call sends a JSON-RPC request for method with params (marshaled to JSON;
// may be nil), blocks until the correlated response arrives, decodes its
// result into result (if non-nil and the call succeeded), and returns any
// error. It mints the request id from Call's own id space (starting at
// 1<<32), disjoint from callExt's.
//
// Call returns *ConnClosedError immediately if c is already closed, and
// every Call still blocked when c closes (for any reason) receives the same
// typed error. A canceled ctx unblocks Call with ctx.Err() and removes the
// pending entry; it never leaks.
func (c *Conn) Call(ctx context.Context, method string, params, result any) error {
	_, err := c.call(ctx, method, params, result, c.mintCallID)
	return err
}

// CallWithResult is Call's additive ordered form. It preserves the generic
// method-string primitive inside protocol while returning write-admission and
// inbound response sequence facts needed by typed extension callers.
func (c *Conn) CallWithResult(ctx context.Context, method string, params, result any) (CallResult, error) {
	return c.call(ctx, method, params, result, c.mintCallID)
}

// StartCall is Call's asynchronous form. It is primarily consumed by typed
// ACP extension wrappers that need Writer admission before the eventual
// response. The returned handle owns one pending entry and resolves it exactly
// once, even when cancellation races a response or connection shutdown.
func (c *Conn) StartCall(ctx context.Context, method string, params, result any) (*CallHandle, error) {
	return c.startCall(ctx, method, params, result, c.mintCallID)
}

// callExt is Call's counterpart for extension traffic: it mints request ids
// from the ExtIDBase-configured id space instead of Call's. It is
// unexported for now — Task 1.7's typed surface is expected to expose it (or
// something built on it) for vendor/extension calls once that layer exists.
func (c *Conn) callExt(ctx context.Context, method string, params, result any) error {
	_, err := c.call(ctx, method, params, result, c.mintExtID)
	return err
}

func (c *Conn) mintCallID() int64 { return c.nextCallID.Add(1) }
func (c *Conn) mintExtID() int64  { return c.nextExtID.Add(1) }

func (c *Conn) startCall(ctx context.Context, method string, params, result any, mintID func() int64) (*CallHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var raw json.RawMessage
	if params != nil {
		r, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("protocol: marshal call params: %w", err)
		}
		raw = r
	}

	c.mu.Lock()
	if c.closed {
		err := error(c.closeErr)
		c.mu.Unlock()
		return nil, err
	}

	id := NewNumberID(mintID())
	pending := make(chan callResult, 1)
	c.pending[id] = pending
	c.mu.Unlock()

	callCtx, cancel := context.WithCancel(ctx)
	h := &CallHandle{
		admission: make(chan bool, 1),
		result:    make(chan AsyncCallResult, 1),
		cancel:    cancel,
	}
	req := &Request{ID: id, Method: method, Params: raw}
	go c.runAsyncCall(callCtx, id, req, result, pending, h)
	return h, nil
}

func (c *Conn) runAsyncCall(
	ctx context.Context,
	id ID,
	req *Request,
	result any,
	pending <-chan callResult,
	h *CallHandle,
) {
	defer h.cancel()
	defer close(h.result)

	send, sendErr := c.writer.startSendContextWithAdmission(ctx, req, h.admission)
	if sendErr != nil {
		c.publishAsyncAdmission(h, false)
		facts, err := c.abandonPendingCall(id, pending, result, CallResult{}, sendErr)
		c.publishAsyncResult(h, facts, err)
		return
	}

	admitted, ok := <-send.admissionDone
	if !ok {
		admitted = false
	}
	if !admitted {
		sendResult := <-send.done
		facts, err := c.abandonPendingCall(id, pending, result, CallResult{}, sendResult.err)
		c.publishAsyncResult(h, facts, err)
		return
	}

	var sendDone <-chan asyncSendResult = send.done
	var cancelCh <-chan struct{}
	for {
		select {
		case res := <-pending:
			// A correlated response itself proves that Writer admission
			// happened, even if an unusual transport reports its raw write
			// completion slightly later.
			facts, err := deliverCallResult(res, result, admitted)
			c.publishAsyncResult(h, facts, err)
			return
		case sendResult := <-sendDone:
			sendDone = nil
			admitted = sendResult.admitted
			cancelCh = ctx.Done()
			if !admitted {
				facts, err := c.abandonPendingCall(id, pending, result, CallResult{}, sendResult.err)
				c.publishAsyncResult(h, facts, err)
				return
			}
			if sendResult.err != nil {
				facts, err := c.abandonPendingCall(id, pending, result, CallResult{WriteAdmitted: admitted}, sendResult.err)
				c.publishAsyncResult(h, facts, err)
				return
			}
		case <-cancelCh:
			facts, err := c.abandonPendingCall(id, pending, result, CallResult{WriteAdmitted: admitted}, ctx.Err())
			c.publishAsyncResult(h, facts, err)
			return
		}
	}
}

func (c *Conn) publishAsyncAdmission(h *CallHandle, admitted bool) {
	h.admission <- admitted
	close(h.admission)
}

func (c *Conn) publishAsyncResult(h *CallHandle, facts CallResult, err error) {
	h.result <- AsyncCallResult{Facts: facts, Err: err}
}

// abandonPendingCall removes id if this async call still owns it. If a
// response or shutdown already won that ownership race, prefer the queued
// authoritative result over the caller's cancellation/send error.
func (c *Conn) abandonPendingCall(id ID, pending <-chan callResult, result any, fallback CallResult, fallbackErr error) (CallResult, error) {
	c.mu.Lock()
	_, stillPending := c.pending[id]
	if stillPending {
		delete(c.pending, id)
	}
	c.mu.Unlock()
	if stillPending {
		return fallback, fallbackErr
	}
	select {
	case res := <-pending:
		return deliverCallResult(res, result, fallback.WriteAdmitted)
	default:
		return fallback, fallbackErr
	}
}

func (c *Conn) call(ctx context.Context, method string, params, result any, mintID func() int64) (CallResult, error) {
	if err := ctx.Err(); err != nil {
		return CallResult{}, err
	}

	c.mu.Lock()
	if c.closed {
		err := error(c.closeErr)
		c.mu.Unlock()
		return CallResult{}, err
	}

	var raw json.RawMessage
	if params != nil {
		r, err := json.Marshal(params)
		if err != nil {
			c.mu.Unlock()
			return CallResult{}, fmt.Errorf("protocol: marshal call params: %w", err)
		}
		raw = r
	}

	id := NewNumberID(mintID())
	resultCh := make(chan callResult, 1)
	c.pending[id] = resultCh
	c.mu.Unlock()

	req := &Request{ID: id, Method: method, Params: raw}
	sendResult, sendErr := c.writer.SendContextResult(ctx, req)
	if sendErr != nil {
		// If our entry is still in the pending table, nobody else will ever
		// resolve it (a shutdown that already ran would have removed it as
		// part of its fail-all sweep before the writer could report
		// closed — see shutdown), so this call owns reporting sendErr and
		// must clean up its own entry. If the entry is already gone, some
		// concurrent event (shutdown's sweep, or in principle a response)
		// has already produced an authoritative result, so fall through and
		// prefer that instead of the raw send error.
		c.mu.Lock()
		_, stillPending := c.pending[id]
		if stillPending {
			delete(c.pending, id)
		}
		c.mu.Unlock()
		if stillPending {
			return CallResult{WriteAdmitted: sendResult.WriteAdmitted}, sendErr
		}
	}

	select {
	case res := <-resultCh:
		return deliverCallResult(res, result, sendResult.WriteAdmitted)
	case <-ctx.Done():
		c.mu.Lock()
		_, stillPending := c.pending[id]
		if stillPending {
			delete(c.pending, id)
		}
		c.mu.Unlock()
		if stillPending {
			return CallResult{WriteAdmitted: sendResult.WriteAdmitted}, ctx.Err()
		}
		// The entry was resolved concurrently between ctx firing and us
		// acquiring the lock (a response arrived, or shutdown swept it):
		// prefer that real result over ctx.Err() rather than discarding it.
		select {
		case res := <-resultCh:
			return deliverCallResult(res, result, sendResult.WriteAdmitted)
		default:
			return CallResult{WriteAdmitted: sendResult.WriteAdmitted}, ctx.Err()
		}
	}
}

func deliverCallResult(res callResult, result any, writeAdmitted bool) (CallResult, error) {
	facts := CallResult{
		WriteAdmitted:    writeAdmitted || res.receiveSequence != 0,
		ResponseSequence: res.receiveSequence,
		ReceiveSequence:  res.receiveSequence,
	}
	if res.err != nil {
		return facts, res.err
	}
	if result != nil && len(res.raw) > 0 {
		if err := json.Unmarshal(res.raw, result); err != nil {
			return facts, fmt.Errorf("protocol: unmarshal call result: %w", err)
		}
	}
	return facts, nil
}

// Notify sends a JSON-RPC notification for method with params (marshaled to
// JSON; may be nil). It does not wait for anything: notifications have no
// response. Notify returns *ConnClosedError immediately if c is already
// closed.
func (c *Conn) Notify(ctx context.Context, method string, params any) error {
	_, err := c.NotifyWithResult(ctx, method, params)
	return err
}

// NotifyWithResult is Notify's admission-aware form. Notifications have no
// inbound response sequence, so it returns only the writer fact.
func (c *Conn) NotifyWithResult(ctx context.Context, method string, params any) (WriteResult, error) {
	if err := ctx.Err(); err != nil {
		return WriteResult{}, err
	}

	c.mu.Lock()
	if c.closed {
		err := error(c.closeErr)
		c.mu.Unlock()
		return WriteResult{}, err
	}
	c.mu.Unlock()

	var raw json.RawMessage
	if params != nil {
		r, err := json.Marshal(params)
		if err != nil {
			return WriteResult{}, fmt.Errorf("protocol: marshal notify params: %w", err)
		}
		raw = r
	}

	n := &Notification{Method: method, Params: raw}
	return c.writer.SendContextResult(ctx, n)
}

// Close stops c from accepting further Call/Notify calls (which now fail
// fast with *ConnClosedError), fails every currently in-flight Call the same
// way, stops the internal writer, and closes the underlying reader and
// writer (whichever of them implement io.Closer) so the read loop's blocked
// Read unblocks and the peer observes our side going away. It waits for the
// read loop goroutine to fully exit before returning, but does not wait for
// any handler callback that is still running in user code — a slow or stuck
// handler cannot delay Close.
//
// Close is idempotent and safe to call concurrently with itself or with any
// other Conn method.
func (c *Conn) Close() error {
	c.shutdown(nil)
	<-c.readLoopDone
	return nil
}

// shutdown performs the one-time transition to closed, however it was
// triggered: an explicit Close (cause == nil), or the read loop observing a
// transport failure (cause == that error, including plain io.EOF for a
// clean peer disconnect). It never blocks on a handler callback.
func (c *Conn) shutdown(cause error) {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.closeErr = &ConnClosedError{cause: cause}
		pending := c.pending
		// Publish every shutdown result while still holding mu. A caller that
		// observes its pending entry removed must then be able to receive the
		// authoritative close result; it must never fall through to a racing
		// cancellation error while this publication is still outstanding.
		for _, ch := range pending {
			ch <- callResult{err: c.closeErr}
		}
		c.pending = make(map[ID]chan callResult)
		c.mu.Unlock()

		close(c.done)

		c.notifyMu.Lock()
		c.notifyClosed = true
		c.notifyMu.Unlock()
		c.notifyCond.Broadcast()

		// Interrupt raw I/O before waiting for admitted Writer sends. A
		// blocked Write must see the transport close or Writer.Close can wait
		// forever, preventing the transport and child process from being
		// interrupted at all.
		if closer, ok := c.r.(io.Closer); ok {
			_ = closer.Close()
		}
		if any(c.w) != any(c.r) {
			if closer, ok := c.w.(io.Closer); ok {
				_ = closer.Close()
			}
		}
		_ = c.writer.Close()
	})
}

// readLoop is the single goroutine that reads frames off the transport,
// classifies them, and dispatches them. It owns FrameReader exclusively (per
// FrameReader's single-reader-loop contract) and never blocks on a handler
// callback: request dispatch hands off to a spawned goroutine gated by sem,
// and notification dispatch enqueues onto the notify worker's unbounded
// queue (see enqueueNotifyJob), so a slow handler cannot stall reading
// subsequent frames (in particular, responses correlating other in-flight
// Calls).
func (c *Conn) readLoop() {
	defer close(c.readLoopDone)
	for {
		frame, err := c.fr.ReadFrame()
		if err != nil {
			// Any read failure — a clean io.EOF on peer disconnect, a
			// truncated frame, or a genuine transport error — means this
			// Conn can make no further progress: treat it exactly like an
			// explicit Close so every in-flight Call fails the same way.
			c.shutdown(err)
			return
		}

		env, err := ParseEnvelope(frame)
		if err != nil {
			// Malformed input from the peer. There is no reliable id to
			// respond to (ParseEnvelope failing means the shape itself,
			// possibly including the id, could not be trusted), so this
			// frame is dropped and reading continues rather than tearing
			// down the whole connection over one bad message.
			continue
		}

		switch env.Kind() {
		case KindRequest:
			c.dispatchRequest(env.Request)
		case KindNotification:
			seq := c.beginReceiveNotification()
			if seq == 0 {
				return
			}
			env.Notification.ReceiveSequence = seq
			c.dispatchNotification(env.Notification)
			c.finishReceiveNotification(seq)
		case KindResponse:
			seq := c.stampReceive()
			if seq == 0 {
				return
			}
			env.Response.ReceiveSequence = seq
			c.correlateResponse(env.Response)
		}
	}
}

// stampReceive advances the one receive-order clock owned by readLoop. It is
// intentionally called only for inbound responses and notifications: request
// handlers are peer-to-client calls, not observations that a client driver
// must merge with prompt/update/extension outcomes.
func (c *Conn) stampReceive() uint64 {
	return c.nextReceiveSequence(false)
}

// beginReceiveNotification advances the shared receive clock and records the
// notification as dispatch-pending before publishing the new sequence. This
// is the first half of the ordered notification admission boundary; the
// matching finishReceiveNotification runs only after dispatchNotification has
// recorded a registered handler job or buffered the notification.
func (c *Conn) beginReceiveNotification() uint64 {
	return c.nextReceiveSequence(true)
}

// nextReceiveSequence advances the shared receive clock and, for
// notifications, registers dispatch-pending under the same lock before any
// waiter can observe the new sequence. On exhaustion it closes the Conn and
// returns zero without publishing or dispatching an observation.
func (c *Conn) nextReceiveSequence(notification bool) uint64 {
	c.receiveMu.Lock()
	if c.receiveThrough == ^uint64(0) {
		c.receiveMu.Unlock()
		c.shutdown(&ReceiveSequenceOverflowError{})
		return 0
	}
	c.receiveThrough++
	seq := c.receiveThrough
	if notification {
		c.receiveDispatchPending[seq] = struct{}{}
	}
	close(c.receiveChanged)
	c.receiveChanged = make(chan struct{})
	c.receiveMu.Unlock()
	return seq
}

// finishReceiveNotification closes the dispatch-pending interval after the
// notification has been admitted to the registered handler queue or buffer.
func (c *Conn) finishReceiveNotification(sequence uint64) {
	c.receiveMu.Lock()
	delete(c.receiveDispatchPending, sequence)
	close(c.receiveChanged)
	c.receiveChanged = make(chan struct{})
	c.receiveMu.Unlock()
}

// correlateResponse delivers resp to the pending Call waiting on its id, if
// any. A response with no matching pending entry (already resolved by a
// concurrent cancel, or simply a bogus/late id from the peer) is dropped.
func (c *Conn) correlateResponse(resp *Response) {
	c.mu.Lock()
	ch, ok := c.pending[resp.ID]
	if ok {
		delete(c.pending, resp.ID)
		// Keep publication under the same lock as removal. Otherwise a
		// concurrent cancellation can observe the missing entry and return its
		// fallback before this authoritative response reaches the channel.
		if resp.Error != nil {
			ch <- callResult{err: FromWireError(resp.Error), receiveSequence: resp.ReceiveSequence}
		} else {
			ch <- callResult{raw: resp.Result, receiveSequence: resp.ReceiveSequence}
		}
	}
	c.mu.Unlock()
	if !ok {
		return
	}
}

// dispatchRequest routes an incoming request to its registered handler, the
// unknown-request catch-all, or (if neither exists) responds immediately
// with MethodNotFound. The handler itself always runs in a spawned,
// sem-gated goroutine so the read loop is never blocked by it.
func (c *Conn) dispatchRequest(req *Request) {
	c.handlersMu.Lock()
	h, ok := c.handlers[req.Method]
	if !ok {
		h = c.unknownRequest
	}
	c.handlersMu.Unlock()

	if h == nil {
		resp := &Response{
			ID:    req.ID,
			Error: ToWireError(MethodNotFound(fmt.Sprintf("method not found: %s", req.Method), nil)),
		}
		_ = c.writer.Send(resp)
		return
	}

	c.spawnHandler(func() {
		result, err := h(context.Background(), req.Method, req.Params)
		resp := c.buildResponse(req.ID, result, err)
		_ = c.writer.Send(resp)
	})
}

// buildResponse converts a handler's (result, error) pair into the Response
// to send back. A *Fault (directly returned, or reachable via errors.As)
// carries its Code/Message/whitelisted Data to the wire; any other error is
// reported as InternalError with the original error kept only in the local
// Fault for diagnosis, never serialized.
func (c *Conn) buildResponse(id ID, result any, err error) *Response {
	if err != nil {
		var f *Fault
		if !errors.As(err, &f) {
			f = InternalError(err.Error(), err)
		}
		return &Response{ID: id, Error: ToWireError(f)}
	}

	raw, merr := json.Marshal(result)
	if merr != nil {
		return &Response{ID: id, Error: ToWireError(InternalError("marshal handler result", merr))}
	}
	return &Response{ID: id, Result: raw}
}

// dispatchNotification routes an incoming notification to its registered
// handler or the unknown-notify catch-all, both enqueued onto the shared
// notify worker queue (see notifyWorker) so that handler *completion* order
// matches wire order across all notifications on this Conn, not just
// invocation order. If neither a handler nor the catch-all exists, the
// notification is buffered (bounded, drop-oldest-on-overflow) for a
// HandleNotify registration that may come later.
func (c *Conn) dispatchNotification(n *Notification) {
	c.notifyOrderMu.Lock()
	defer c.notifyOrderMu.Unlock()
	c.handlersMu.Lock()

	if h, ok := c.notifyHandlers[n.Method]; ok {
		c.handlersMu.Unlock()
		method, params := n.Method, n.Params
		c.enqueueTrackedNotifyJob(n.ReceiveSequence, func() { h(context.Background(), method, params) })
		return
	}

	if h, ok := c.notifySeqHandlers[n.Method]; ok {
		c.handlersMu.Unlock()
		method, params, seq := n.Method, n.Params, n.ReceiveSequence
		c.enqueueTrackedNotifyJob(seq, func() { h(context.Background(), method, params, seq) })
		return
	}

	if h := c.unknownNotify; h != nil {
		c.handlersMu.Unlock()
		method, params := n.Method, n.Params
		c.enqueueTrackedNotifyJob(n.ReceiveSequence, func() { h(context.Background(), method, params) })
		return
	}

	if h := c.unknownNotifySeq; h != nil {
		c.handlersMu.Unlock()
		method, params, seq := n.Method, n.Params, n.ReceiveSequence
		c.enqueueTrackedNotifyJob(seq, func() { h(context.Background(), method, params, seq) })
		return
	}

	buf := append(c.notifyBuffers[n.Method], bufferedNotification{
		params:          n.Params,
		receiveSequence: n.ReceiveSequence,
	})
	if drop := len(buf) - NotifyBufferDepth; drop > 0 {
		buf = buf[drop:]
		// drop is bounded above by len(buf) (an in-memory slice length,
		// never anywhere near the int64/uint64 boundary) and just proved
		// positive, so this conversion cannot wrap.
		c.droppedNotifications.Add(uint64(int64(drop)))
	}
	c.notifyBuffers[n.Method] = buf

	c.handlersMu.Unlock()
}

// enqueueNotifyJob appends job to the notify worker's queue and wakes it.
// job is abandoned (never run) if the Conn has already started shutting
// down, matching spawnHandler's "abandon rather than run" behavior for
// handler work that has not yet started. Never blocks: the queue is an
// unbounded slice guarded by notifyMu, not a fixed-capacity channel, so a
// slow (or stalled) notifyWorker can never make a caller — in particular the
// read loop, via dispatchNotification — block here.
func (c *Conn) enqueueNotifyJob(job func()) bool {
	c.notifyMu.Lock()
	if c.notifyClosed {
		c.notifyMu.Unlock()
		return false
	}
	c.notifyQueue = append(c.notifyQueue, job)
	c.notifyMu.Unlock()
	c.notifyCond.Signal()
	return true
}

// enqueueTrackedNotifyJob records a notification handler as pending by its
// receive sequence and clears that record only after the handler returns.
// Generic jobs (barriers and white-box test jobs) intentionally use the
// untracked enqueueNotifyJob path.
func (c *Conn) enqueueTrackedNotifyJob(sequence uint64, job func()) {
	if sequence == 0 {
		c.enqueueNotifyJob(job)
		return
	}
	c.notifyStateMu.Lock()
	c.notifyPending[sequence] = struct{}{}
	c.notifyStateMu.Unlock()
	accepted := c.enqueueNotifyJob(func() {
		defer c.completeTrackedNotify(sequence)
		job()
	})
	if !accepted {
		c.completeTrackedNotify(sequence)
	}
}

func (c *Conn) completeTrackedNotify(sequence uint64) {
	c.notifyStateMu.Lock()
	delete(c.notifyPending, sequence)
	close(c.notifyChanged)
	c.notifyChanged = make(chan struct{})
	c.notifyStateMu.Unlock()
}

// notifyWorker is the single goroutine that drains the notify queue,
// running each queued job to completion before starting the next: this
// strict one-at-a-time draining in enqueue order is what makes notification
// handler completion order match wire order. It exits once the Conn is
// shutting down, abandoning (not running) whatever is still queued at that
// point — it never waits for a job it has already started, matching Close's
// "does not wait for any handler callback still running" contract, and it
// never blocks Close from returning.
func (c *Conn) notifyWorker() {
	for {
		job, ok := c.nextNotifyJob()
		if !ok {
			return
		}
		job()
	}
}

// nextNotifyJob blocks until a job is available or the Conn starts shutting
// down. Once notifyClosed is set, it returns (nil, false) immediately even
// if jobs remain queued, abandoning them.
func (c *Conn) nextNotifyJob() (func(), bool) {
	c.notifyMu.Lock()
	defer c.notifyMu.Unlock()
	for len(c.notifyQueue) == 0 && !c.notifyClosed {
		c.notifyCond.Wait()
	}
	if c.notifyClosed {
		return nil, false
	}
	job := c.notifyQueue[0]
	c.notifyQueue[0] = nil
	c.notifyQueue = c.notifyQueue[1:]
	return job, true
}

// spawnHandler runs fn in a new goroutine once a slot in sem is available,
// or abandons the attempt (without running fn) if c closes first. This is
// how both the MaxInFlightHandlers concurrency cap and "excess requests
// queue" are implemented: the goroutine itself is cheap to leave parked on
// the semaphore send.
func (c *Conn) spawnHandler(fn func()) {
	go func() {
		select {
		case c.sem <- struct{}{}:
			defer func() { <-c.sem }()
			fn()
		case <-c.done:
		}
	}()
}

// pendingLen reports the number of in-flight Calls currently registered in
// the pending table. Unexported: a white-box test hook only (see
// conn_internal_test.go), not part of Conn's public contract.
func (c *Conn) pendingLen() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pending)
}
