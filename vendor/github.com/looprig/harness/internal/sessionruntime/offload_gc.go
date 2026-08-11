package sessionruntime

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/sessionstore"
)

// OffloadGCPolicy is the validated cadence for one session's offload-blob GC: how often a
// pass runs (Interval) and the per-pass deadline (Timeout). The rig validates the fields
// before constructing it; a zero value means "off". This is SESSION OFFLOAD GC only — it
// reaps orphaned content-addressed offload blobs (a crash gap of the writer's
// blob-durable-before-pointer discipline), never workspace snapshots.
type OffloadGCPolicy struct {
	Interval time.Duration
	Timeout  time.Duration
}

// Configured reports whether the policy is armed (both fields positive). An unarmed policy
// wires no gate and no runner, leaving the journal undecorated (unchanged behavior).
func (p OffloadGCPolicy) Configured() bool {
	return p.Interval > 0 && p.Timeout > 0
}

// journalAdmissionGate is the ONE reader/writer serialization point between durable
// appends and offload GC. Every SessionJournal.Append (event, command, gate-record, fence,
// restore-lifecycle) takes the READER for the whole delegated call — which spans an
// over-threshold offload's blob Put AND its blobptr AppendDefinite — so GC can never
// interleave between them. The GC scan/delete transaction takes the WRITER. This
// serialization is the whole of ObjectGC's concurrency safety (pkg/sessionstore/gc.go):
// without it, a blob whose pointer append is still in flight would be observed as an orphan
// and reaped.
type journalAdmissionGate struct{ mu sync.RWMutex }

func newJournalAdmissionGate() *journalAdmissionGate { return &journalAdmissionGate{} }

func (g *journalAdmissionGate) enterAppend() { g.mu.RLock() }
func (g *journalAdmissionGate) exitAppend()  { g.mu.RUnlock() }
func (g *journalAdmissionGate) enterGC()     { g.mu.Lock() }
func (g *journalAdmissionGate) exitGC()      { g.mu.Unlock() }

// gatedJournal decorates a journal.SessionJournal so every Append acquires the shared
// admission gate as a reader for the full delegated call. It is wired at the composition
// root over the one j returned by store.OpenJournal, BEFORE any appender (hub event tap,
// command intent log, gate-record appender) is built over it — so all of them funnel
// through this single reader admission (Open/Closed: the journal is not modified; it is
// wrapped).
type gatedJournal struct {
	inner journal.SessionJournal
	gate  *journalAdmissionGate
}

// Compile-time proof that the decorator honors the SessionJournal contract.
var _ journal.SessionJournal = (*gatedJournal)(nil)

func newGatedJournal(inner journal.SessionJournal, gate *journalAdmissionGate) *gatedJournal {
	return &gatedJournal{inner: inner, gate: gate}
}

func (g *gatedJournal) Append(ctx context.Context, rec journal.JournalRecord) (uint64, error) {
	g.gate.enterAppend()
	defer g.gate.exitAppend()
	return g.inner.Append(ctx, rec)
}

// offloadScanner is the runner's narrow view of the offload GC (Interface Segregation):
// one scan-and-sweep pass. *sessionstore.ObjectGC satisfies it.
type offloadScanner interface {
	GC(ctx context.Context) (sessionstore.GCResult, error)
}

// offloadGCTicker is the manual tick seam (Dependency Inversion): production wraps a
// time.Ticker over the policy Interval; tests inject a manual ticker so passes are driven
// deterministically with no wall-clock timer.
type offloadGCTicker interface {
	C() <-chan time.Time
	Stop()
}

// timeTicker is the production offloadGCTicker over a real time.Ticker.
type timeTicker struct{ t *time.Ticker }

func newTimeTicker(d time.Duration) *timeTicker { return &timeTicker{t: time.NewTicker(d)} }
func (t *timeTicker) C() <-chan time.Time       { return t.t.C }
func (t *timeTicker) Stop()                     { t.t.Stop() }

// offloadGCRunner drives periodic offload GC for one session while its lease is held. On
// each tick, if the session is at native SessionIdle, it takes the admission-gate WRITER
// and runs one bounded GC pass. Lease loss returns the goroutine (cancelling any in-flight
// pass); Stop joins it. Every dependency is injected at the composition root, never
// constructed here.
type offloadGCRunner struct {
	sessionID uuid.UUID // owning session, for best-effort pass-failure log context
	scanner   offloadScanner
	gate      *journalAdmissionGate
	newTicker func() offloadGCTicker
	lost      <-chan struct{}
	timeout   time.Duration

	idle   func() bool
	ticker offloadGCTicker

	started atomic.Bool
	stop    chan struct{}
	done    chan struct{}

	startOnce sync.Once
	stopOnce  sync.Once

	// onPass is an optional per-tick observer (a test seam; nil in production). It fires
	// once per processed tick, whether or not the idle gate admitted a scan.
	onPass func(sessionstore.GCResult, error)
}

// newOffloadGCRunner constructs an unstarted runner. sessionID is carried only for
// pass-failure log context. newTicker is invoked once at start so an aborted construction
// that never starts leaks no timer. lost may be nil (headless/no lease-loss signal).
func newOffloadGCRunner(sessionID uuid.UUID, scanner offloadScanner, gate *journalAdmissionGate, newTicker func() offloadGCTicker, lost <-chan struct{}, timeout time.Duration) *offloadGCRunner {
	return &offloadGCRunner{
		sessionID: sessionID,
		scanner:   scanner,
		gate:      gate,
		newTicker: newTicker,
		lost:      lost,
		timeout:   timeout,
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
}

// start launches the run loop exactly once, binding the native-idle probe (hub.IsIdle). It
// builds the ticker here so an unstarted runner holds no timer.
func (r *offloadGCRunner) start(idle func() bool) {
	r.startOnce.Do(func() {
		r.idle = idle
		r.ticker = r.newTicker()
		r.started.Store(true)
		go r.run()
	})
}

func (r *offloadGCRunner) run() {
	defer close(r.done)
	defer r.ticker.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-r.lost:
			return
		case <-r.ticker.C():
			r.tickOnce()
		}
	}
}

// tickOnce processes one tick: skip unless the session is at native SessionIdle, else take
// the writer and run one bounded pass. A pass failure (blob store down, ledger read, delete
// failures) is best-effort — GC retries on the next tick — so it is logged loudly rather than
// faulting the session, mirroring observeBestEffortCheckpointError.
//
// Note: Stop() cannot preempt a tick already blocked on the writer Lock() behind a wedged
// in-flight append — that wait is bounded only by the append's own per-append ctx, not by
// Stop. In practice the shutdown ordering makes this safe: Shutdown drains the loops (so no
// new appends are admitted) before it calls stopOffloadGC, so the writer Lock is uncontended
// by the time Stop runs.
func (r *offloadGCRunner) tickOnce() {
	if r.idle != nil && !r.idle() {
		if r.onPass != nil {
			r.onPass(sessionstore.GCResult{}, nil)
		}
		return
	}
	r.gate.enterGC()
	ctx, cancel := r.passContext()
	res, err := r.scanner.GC(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "sessionruntime: offload GC pass failed",
			"session", r.sessionID, "err", err)
	}
	cancel()
	r.gate.exitGC()
	if r.onPass != nil {
		r.onPass(res, err)
	}
}

// passContext derives the per-pass context: bounded by the policy Timeout and additionally
// cancelled on stop or lease loss so an in-flight pass unblocks promptly on teardown. The
// returned cancel joins the watcher goroutine.
func (r *offloadGCRunner) passContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
	watchDone := make(chan struct{})
	go func() {
		select {
		case <-r.stop:
			cancel()
		case <-r.lost:
			cancel()
		case <-watchDone:
		}
	}()
	return ctx, func() {
		close(watchDone)
		cancel()
	}
}

// Stop signals the loop to exit and joins the goroutine (idempotent). It is called on clean
// Shutdown BEFORE SessionStopped is appended and BEFORE the lease is released. If the runner
// was never started there is no goroutine to join.
func (r *offloadGCRunner) Stop() {
	r.stopOnce.Do(func() { close(r.stop) })
	if r.started.Load() {
		<-r.done
	}
}
