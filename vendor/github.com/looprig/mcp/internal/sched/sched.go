// Package sched provides the per-binding request scheduler: the admission
// control that decides which of a binding's requests may be in flight on its
// connection at once.
//
// One MCP connection may be shared — a Session-scoped binding serves every Loop
// allowed to use it — so the requests on it come from callers that do not know
// about each other. This package is what makes that safe without making it
// serial by accident.
//
// # What it guarantees
//
//   - Tool calls are serialized by default. A tool call has effects, and a
//     server that was never written to expect two at once is the common case,
//     not the exception. Parallelism is opt-in per binding (Config.AllowParallel)
//     because only the application knows whether a particular server tolerates
//     it. A server's own tool annotations may inform that decision, but they can
//     never make it: annotations are untrusted input, and a server that could
//     widen its own concurrency by claiming to be idempotent would be deciding
//     the host's policy for it.
//   - Everything is bounded. MaxConcurrent caps in-flight requests of every
//     class, whether or not parallel calls are allowed — enabling parallelism
//     raises a serialization constraint, it does not remove a budget.
//   - Ordering-sensitive work is serialized among itself. Lifecycle, auth and
//     catalog-refresh operations take one class and run one at a time, so two
//     refreshes can never interleave into one candidate.
//   - Cancelling one request cancels one request. Each Begin returns its own
//     derived context and its own cancel; nothing here is shared between two
//     live requests but the counter.
//   - Shutdown rejects, then cancels. New work is refused before in-flight work
//     is torn down, so nothing can slip in behind the teardown.
//
// # What it is not
//
// It is not a queue and not a fair scheduler. Waiters take their turn in
// whatever order the runtime hands them the channel, and nothing here promises
// FIFO. The bounds are the contract; the order in which two callers pass a
// bound is not one, because no caller can depend on it — a shared connection has
// no principled ordering between the requests of two unrelated Loops.
package sched

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// ErrShutdown reports that the scheduler is shut down: no further work is
// admitted. Callers classify it into their own taxonomy; this package has none.
var ErrShutdown = errors.New("sched: scheduler is shut down")

// Class is the kind of work a request is, which is what decides its
// serialization. The zero value is not a valid class.
type Class uint8

// The request classes.
const (
	// ClassCall is a tool call: it may have effects, so it is serialized
	// against other tool calls unless the binding allows parallelism.
	ClassCall Class = iota + 1
	// ClassRequest is any other server request — reading a resource, getting a
	// prompt. These are reads: they are bounded by the concurrency budget, but
	// not serialized against each other or against tool calls, because nothing
	// about their ordering is load-bearing.
	ClassRequest
	// ClassControl is an operation whose state transitions require ordering:
	// lifecycle, auth, catalog refresh. Control operations run one at a time.
	ClassControl
	// ClassReentrantCall is a tool call issued from within a sampling handler,
	// while the tool call that provoked the sampling is still in flight and still
	// holding the ClassCall permit. It is a call — it may have effects — but it
	// must NOT take that permit: the outer call it descends from holds it and will
	// not release it until the sampling round-trip this call is part of returns,
	// so queueing behind it is a self-deadlock that only the deadline breaks.
	//
	// It is therefore admitted without the tool-call serializer, and bounded
	// instead by the sampling depth and concurrency caps that already govern the
	// chain (the client's sampleGate). It still counts against the concurrency
	// budget like every other in-flight request: raising a serialization
	// constraint is not the same as removing a budget.
	ClassReentrantCall

	classSentinel // must remain last; tests derive the declared range from it
)

// classNames maps each class to its stable lowercase identifier.
var classNames = [classSentinel]string{
	ClassCall:          "call",
	ClassRequest:       "request",
	ClassControl:       "control",
	ClassReentrantCall: "reentrant_call",
}

// String returns the class's stable identifier, or "unknown".
func (c Class) String() string {
	if !c.valid() {
		return "unknown"
	}
	return classNames[c]
}

// valid reports whether c is a declared class.
func (c Class) valid() bool { return c >= ClassCall && c < classSentinel }

// Config is a scheduler's policy.
type Config struct {
	// MaxConcurrent caps in-flight requests of every class combined. Values
	// below 1 are clamped to 1; see New.
	MaxConcurrent int
	// AllowParallel opts in to parallel tool calls, up to MaxConcurrent. It is
	// the application's decision and never a server's.
	AllowParallel bool
}

// Scheduler admits a binding's requests. It is safe for concurrent use, and its
// zero value is not usable — call New.
type Scheduler struct {
	// sem is the concurrency budget: one buffer slot per permitted in-flight
	// request, of any class.
	sem chan struct{}
	// callLock serializes tool calls. It is nil when the binding allows
	// parallel calls, which is the whole of that opt-in: there is then no lock
	// to take, rather than a lock that is taken and ignored.
	callLock chan struct{}
	// controlLock serializes the ordering-sensitive operations.
	controlLock chan struct{}

	// closed is closed by Shutdown. It is what makes a waiter abandon a bound
	// it will never be admitted through.
	closed chan struct{}

	// mu guards the in-flight registry and the shutdown flag. The flag is
	// duplicated by the closed channel because the two answer different
	// questions: the channel is what a select waits on, and the flag is what
	// makes admission and shutdown decide the same way when they race (see
	// register).
	mu       sync.Mutex
	shutdown bool
	inflight map[uint64]context.CancelFunc
	nextID   uint64
}

// New returns a Scheduler for cfg.
//
// A MaxConcurrent below 1 is clamped to 1 rather than rejected or honored. This
// module's rule is that a non-positive bound fails closed rather than meaning
// "unbounded" — but a budget of zero does not refuse work, it blocks forever,
// which is a deadlock wearing a bound's clothes. One is the most restrictive
// budget that can still make progress, so it is the fail-closed value here. The
// client normalizes its limits before this is reached, so the clamp is defence
// in depth rather than a configuration path.
func New(cfg Config) *Scheduler {
	n := cfg.MaxConcurrent
	if n < 1 {
		n = 1
	}
	s := &Scheduler{
		sem:         make(chan struct{}, n),
		controlLock: make(chan struct{}, 1),
		closed:      make(chan struct{}),
		inflight:    make(map[uint64]context.CancelFunc),
	}
	if !cfg.AllowParallel {
		s.callLock = make(chan struct{}, 1)
	}
	return s
}

// Begin admits one request of the given class, blocking until the binding's
// bounds allow it.
//
// It returns a context for the request and a release func the caller must call
// exactly once, whatever the outcome of the request — the canonical use is a
// defer on the line after the error check. Releasing is what returns the budget
// and the serialization lock; a caller that forgets one wedges the binding.
//
// The returned context is derived from ctx and is additionally cancelled by
// Shutdown. It is per-request: cancelling it, or cancelling the ctx behind it,
// affects this request and no other. That is the design's isolation rule, and it
// is structural here — there is no shared cancel for a caller to reach.
//
// Waiting respects ctx: a caller that gives up while queued gets ctx's error
// and consumes nothing. Time spent waiting counts against the caller's own
// deadline, which is the point of passing an already-bounded context — a request
// that queues past its deadline has missed it, whether it waited on a bound or
// on a server.
//
// It fails closed: an undeclared class is refused rather than admitted
// unserialized, and a shutdown scheduler admits nothing.
func (s *Scheduler) Begin(ctx context.Context, class Class) (context.Context, func(), error) {
	if !class.valid() {
		return nil, nil, fmt.Errorf("sched: undeclared request class %d", class)
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	// Serialization lock first, budget second. The other order would work — no
	// holder here ever waits on something a waiter holds, so neither can
	// deadlock — but it would have every call queued behind the call lock
	// sitting on a slot of the concurrency budget it is not using, and a budget
	// spent on waiting is a budget that bounds nothing.
	var releases []func()
	undo := func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}

	if lock := s.lockFor(class); lock != nil {
		if err := s.take(ctx, lock); err != nil {
			return nil, nil, err
		}
		releases = append(releases, func() { <-lock })
	}
	if err := s.take(ctx, s.sem); err != nil {
		undo()
		return nil, nil, err
	}
	releases = append(releases, func() { <-s.sem })

	reqCtx, cancel := context.WithCancel(ctx)
	id, ok := s.register(cancel)
	if !ok {
		// Shutdown won the race for this request: it had already taken the
		// registry when this one arrived, so this cancel would never be called
		// by it. Refusing here is what keeps "shutdown rejects new work" true
		// even for work that was mid-admission when shutdown began.
		cancel()
		undo()
		return nil, nil, ErrShutdown
	}

	var once sync.Once
	release := func() {
		once.Do(func() {
			s.unregister(id)
			cancel()
			undo()
		})
	}
	return reqCtx, release, nil
}

// lockFor returns the serialization lock a class takes, or nil when the class
// takes none.
func (s *Scheduler) lockFor(class Class) chan struct{} {
	switch class {
	case ClassCall:
		// Nil when the binding allows parallel calls.
		return s.callLock
	case ClassControl:
		return s.controlLock
	case ClassReentrantCall:
		// No serialization lock, deliberately: a re-entrant call must not queue
		// behind the ClassCall permit its outer call is still holding. Its bound
		// is the concurrency budget alone, plus the sampling caps upstream.
		return nil
	default:
		return nil
	}
}

// take acquires one slot of ch, abandoning the wait if ctx ends or the
// scheduler shuts down.
func (s *Scheduler) take(ctx context.Context, ch chan struct{}) error {
	select {
	case ch <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-s.closed:
		return ErrShutdown
	}
}

// register records a request's cancel func so Shutdown can reach it, reporting
// false if the scheduler is already shut down.
func (s *Scheduler) register(cancel context.CancelFunc) (uint64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.shutdown {
		return 0, false
	}
	s.nextID++
	s.inflight[s.nextID] = cancel
	return s.nextID, true
}

// unregister drops a finished request from the registry.
func (s *Scheduler) unregister(id uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.inflight, id)
}

// Shutdown refuses all further work and cancels everything in flight, in that
// order. It is idempotent and safe to call concurrently.
//
// The order is the point. Cancelling first and refusing second would leave a gap
// in which a request admitted between the two steps survives the shutdown that
// was supposed to cancel it — on a connection that is about to close.
//
// It does not wait for the cancelled requests to return. What a cancel
// guarantees is that they stop soon and on their own terms; the caller (the
// client's Close) is what bounds the wait for them, because only it knows what
// it is willing to wait for.
func (s *Scheduler) Shutdown() {
	s.mu.Lock()
	if s.shutdown {
		s.mu.Unlock()
		return
	}
	s.shutdown = true
	close(s.closed)

	// Take the cancels out of the registry under the lock, and call them
	// outside it: a cancel runs the context's own callbacks, which is foreign
	// code from this package's point of view.
	cancels := make([]context.CancelFunc, 0, len(s.inflight))
	for _, cancel := range s.inflight {
		cancels = append(cancels, cancel)
	}
	clear(s.inflight)
	s.mu.Unlock()

	for _, cancel := range cancels {
		cancel()
	}
}

// InFlight reports how many admitted requests have not been released.
//
// It exists for tests and diagnostics: the count is a fact about a moment that
// has passed by the time a caller reads it, so nothing may branch on it. It is
// not a substitute for release.
func (s *Scheduler) InFlight() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.inflight)
}
