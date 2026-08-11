package process

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/tool"
)

// drainChunkBytes bounds each single Read call inside drain. It is large
// enough that ordinary test fixtures and small real command output land in
// one Append per Read, while still bounding worst-case per-call memory for
// a genuinely large single read.
const drainChunkBytes = 32 << 10 // 32 KiB

// manifestSaver is the narrow persistence capability entry.terminalize
// needs from a ManifestStore: reload the manifest a live entry's Start call
// already durably persisted, and persist the terminal update. *ManifestStore
// satisfies it directly; entry depends on the interface, not the concrete
// type, purely so a test can substitute a call-counting fake in place of a
// real ManifestStore to observe terminalize's one-shot Save guarantee
// (TestSupervisorTerminalRaceChoosesOnce) without touching real disk I/O
// semantics.
//
// A successful Load is doTerminalize's preferred source of the fields it
// must carry forward unchanged into the terminal Save, but it is not the
// only source: entry.base (manifestBase) already carries every one of those
// same fields, captured once by Supervisor.Start, so a Load failure never
// leaves doTerminalize without them -- see manifestBase's doc comment and
// doTerminalize's synthesizeManifest fallback.
type manifestSaver interface {
	Load(Handle) (Manifest, error)
	Save(Manifest) error
}

var _ manifestSaver = (*ManifestStore)(nil)

// lifecycleTerminalEvent is the payload lifecycleSink.publish receives at a
// process's one-shot terminal transition (entry.terminalize). EventID is
// always one of the manifest's pre-persisted, already-persisted
// LifecycleEventIDs (manifest.go) -- Completed for an ordinary terminal
// outcome, Lost for a restore-reconciliation outcome (Task 9; State ==
// StateLostOnRestore) -- never a freshly minted ID.
type lifecycleTerminalEvent struct {
	EventID uuid.UUID
	Kind    tool.ProcessLifecycleKind

	Identity Identity
	State    State
	Result   Result

	// CreatedAt and StartedAt mirror this process's manifest.CreatedAt/
	// StartedAt (entry.base's own already-captured values -- see
	// manifestBase's doc comment), never freshly derived here. A REAL
	// tool.ProcessLifecyclePublisher requires ProcessCreatedAt non-zero for
	// EVERY kind, and ProcessStartedAt non-zero for a Completed/Exited
	// record (tool.ProcessLifecycleMetadata.Validate/validProcessLifecycleShape),
	// so a lifecycleSink adapter that publishes a real
	// tool.ProcessLifecycleMetadata (session_resource.go's
	// lifecyclePublisherAdapter) cannot construct a valid DTO without them.
	// Added alongside session_resource.go's Activate wiring -- see that
	// file's doc comment for why the pre-fix package-private fakes never
	// caught this: they never validated against the real Harness DTO at
	// all.
	CreatedAt time.Time
	StartedAt time.Time

	FinishedAt time.Time
}

// lifecycleStartEvent is the payload lifecycleSink.publishStart receives at
// a process's Start-time lifecycle emission (Supervisor.Start), the moment
// the manifest transitions to StateRunning: Started, if the caller did not
// request immediate backgrounding, or Backgrounded, if it did
// (YieldSettings.Yield == true) -- Task 8's combined-acceptance text
// ("lifecycle sink receives the pre-persisted started EventID exactly once"
// and "explicit/yield handoff emits backgrounded with its stable EventID").
// Exactly one of {Started, Backgrounded} is ever published per process, at
// Start, never both, and EventID is always one of the manifest's
// pre-persisted LifecycleEventIDs.Started/.Backgrounded (manifest.go),
// mirroring lifecycleTerminalEvent's own pre-persisted-ID discipline --
// never a freshly minted one.
//
// CreatedAt/StartedAt mirror the manifest's own already-set CreatedAt/
// StartedAt at the point Start calls publishStart (both are set by then,
// since this happens strictly after the StateRunning Save that sets
// StartedAt). The harness ProcessLifecycleMetadata validation matrix
// requires both non-zero for kind Started/Backgrounded
// (validProcessLifecycleTuple), and forbids a finished timestamp or exit
// code for either -- this type carries no such fields at all, so a caller
// building the eventual harness DTO from it cannot populate them by
// mistake.
type lifecycleStartEvent struct {
	EventID   uuid.UUID
	Kind      tool.ProcessLifecycleKind
	Identity  Identity
	CreatedAt time.Time
	StartedAt time.Time
}

// manifestBase is the small set of Manifest fields that are set exactly
// once, at Start, and never change again for the rest of a process's
// lifetime: the sanitized command description, the effective workspace
// access mode, whether the process runs under a PTY, the manifest's
// creation/start/deadline timestamps, and this process's stable
// LifecycleEventIDs. Supervisor.Start captures all of them onto the entry
// it registers (entry.base) at the exact same point it persists them into
// the manifest itself, so entry.doTerminalize's terminal Save never has to
// depend on successfully reloading the manifest just to recover values it
// already has in memory -- only Cursors (from e.spool, already
// reload-independent) and the terminal State/Result/FinishedAt (this call's
// own parameters) are genuinely NOT part of manifestBase, because they are
// exactly the fields a terminal transition is setting for the first time.
//
// See doTerminalize's synthesizeManifest for the fallback path this makes
// possible: when e.manifests.Load fails, doTerminalize still assembles a
// complete, valid terminal Manifest from e.identity + e.base + this call's
// own parameters, rather than silently skipping the terminal Save (Phase
// Gate 2 finding).
type manifestBase struct {
	command   CommandMetadata
	access    AccessMode
	tty       bool
	createdAt time.Time
	startedAt time.Time
	deadline  *time.Time
	events    LifecycleEventIDs
}

// completionEvent is the payload completionNotifier.notify receives at a
// process's one-shot terminal transition. CommandID is always the
// manifest's pre-persisted LifecycleEventIDs.CommandID, never a freshly
// minted ID (mirrors harness/pkg/tool's ProcessCompletionNotification.CommandID).
type completionEvent struct {
	CommandID uuid.UUID
	Owner     Owner
	Handle    Handle
	State     State
	Result    Result
}

// entry is the supervisor's per-process registry record (supervisor.go's
// Supervisor.entries, keyed by Handle). It carries the process's immutable
// Identity, the caller-supplied dependencies Start received (lease,
// lifecycle sink, observation capability, storage ceiling, yield
// settings), the live tool.Process PreparedProcess.Start returned, and the
// exact quota reservation amounts (so release always reverses precisely
// what admission reserved -- see supervisor.go's reservation/reserveQuota/
// releaseQuota).
//
// Task 8B added the entry's output storage (buffer/spool) and its one
// lifetime-owning goroutine (run/drain, started by Supervisor.Start after a
// successful spawn). Task 8C adds terminal-state arbitration: terminalOnce
// guards the one-shot compare-and-set every path that can end this
// process's lifetime -- today only run's natural Wait() return, and
// starting in Task 9C/18 also an explicit stop request, a deadline
// timeout, and supervisor shutdown -- must go through
// (entry.terminalize), and manifests/notifications/releaseQuota are the
// dependencies that one-shot path needs to write the terminal manifest,
// notify completion, and release this process's quota reservation. Task 8D
// adds the observation-invalidation call sites (run calls
// invalidateObservations at spawn, at each received tool.ProcessActivity,
// and at completion) and onTerminal, the Supervisor's terminal-LRU
// retention hook.
type entry struct {
	identity Identity

	lease        Lease
	lifecycle    lifecycleSink
	observations observationInvalidator
	ceiling      StorageCeiling
	yield        YieldSettings

	// base carries the small set of Manifest fields Supervisor.Start sets
	// once and that never change again -- see manifestBase's doc comment.
	// It is what lets doTerminalize's terminal Save survive a reload
	// failure (Phase Gate 2 finding) without losing fidelity: a bare entry
	// built directly by an older test fixture (predating this field) simply
	// leaves it at its zero value, which doTerminalize's synthesizeManifest
	// fallback path only reaches at all when e.manifests.Load has failed --
	// exactly the same nil/zero-tolerant convention as every other entry
	// dependency in this file.
	base manifestBase

	// process is the live tool.Process PreparedProcess.Start returned.
	// run's drain goroutines and Wait call are its first consumers.
	process tool.Process

	// reservation is the exact quota amounts admission reserved for this
	// process (supervisor.go's reserveQuota). It is retained on the entry
	// so terminalize's release path (natural exit, and -- starting in Task
	// 9C -- stop/timeout/shutdown) can call releaseQuota with precisely
	// what was reserved, without re-deriving it from Config and risking
	// drift if Config changes after admission.
	reservation reservation

	// manifests, notifications, and releaseQuota are terminalize's
	// dependencies: reload/persist the terminal manifest, notify
	// completion, and reverse this entry's quota reservation. All three
	// are nil-tolerant (terminalize skips whichever step has a nil
	// dependency) so a test can construct a bare entry (as
	// TestEntryRunClosesDoneAfterDrainingBothStreams already does) without
	// wiring every dependency.
	manifests     manifestSaver
	notifications completionNotifier
	releaseQuota  func(reservation)

	// onTerminal is the Supervisor's terminal-LRU retention-bookkeeping hook
	// (Task 8D, supervisor.go's recordTerminal). entry has no reference to
	// the Supervisor's registry, so doTerminalize calls this once -- inside
	// terminalOnce's guarded body, alongside releaseQuota/lifecycle/
	// notifications -- to let the Supervisor record this entry as newly
	// terminal for its owning session and evict that session's
	// least-recently-used terminal entry if the retention limit
	// (Config.MaxRetainedCompletedProcessesPerSession) is now exceeded.
	// Nil-tolerant, like every other terminalize dependency, so a bare
	// test-built entry (no Supervisor) still terminalizes cleanly.
	onTerminal func(Handle, Owner)

	// buffer is the in-memory rolling window (buffer.go) over this
	// process's combined stdout+stderr stream, sized from
	// reservation.memoryBytes.
	buffer *Buffer
	// spool is the durable disk retention window (spool.go) over the same
	// combined stream, sized from reservation.spoolBytes and opened
	// beneath the Supervisor's spoolRoot, keyed by this process's Handle.
	spool *Spool

	// appendMu serializes every Buffer/Spool append so concurrent stdout
	// and stderr reads can never interleave mid-chunk. This is the single
	// per-process append sequence the spec's "Output capture and storage"
	// section requires: "Writes use a single per-process append sequence
	// so cursor order is deterministic even when stdout and stderr are
	// read concurrently." Both stores are appended to, in the same order,
	// while appendMu is held, so Buffer and Spool always agree on append
	// order for a given process even though they are otherwise
	// independent stores.
	appendMu sync.Mutex

	// waitMu guards generation and wake, the append-driven wake mechanism
	// Task 9A's wait.go builds on (bumpGeneration, generationSnapshot). It
	// is a separate lock from appendMu -- disjoint from the Buffer/Spool
	// append sequence -- so a blocked waiter's read of (generation, wake)
	// never has to contend with, or wait behind, an in-flight append.
	waitMu sync.Mutex
	// generation counts how many times appendChunk has appended a
	// non-empty chunk to this entry, starting at zero. wait.go's waiters
	// compare a caller-supplied "last observed" generation against the
	// current value to decide whether new output has arrived since they
	// last looked.
	generation uint64
	// wake is closed, then immediately replaced with a fresh channel, by
	// every bumpGeneration call (the classic Go "close to broadcast every
	// current waiter, replace to re-arm for the next one" pattern). A
	// waiter that captures (generation, wake) together, atomically, under
	// waitMu (generationSnapshot) can safely decide "already advanced" vs.
	// "block on wake" with no lost-wakeup window: any bump after the
	// snapshot is read is guaranteed to close exactly the channel that
	// snapshot returned. A process's terminal transition is deliberately
	// not wired through this same mechanism -- see wait.go, which selects
	// on the entry's existing done channel (closed exactly once, by run,
	// after terminalize returns) as the independent "or the entry
	// terminalizes" wake condition instead.
	wake chan struct{}

	// lifetimeCancel cancels the entry's own lifetime-scoped context (see
	// run's doc comment for why this is never the ctx a caller passed into
	// Supervisor.Start). 8B stores it purely so a later microtask (Stop,
	// coordinated shutdown -- Task 9C) can cancel the ongoing Wait/drain
	// without needing any new plumbing; nothing calls it yet.
	lifetimeCancel context.CancelFunc

	// terminalOnce guards terminalize's one-shot compare-and-set: whichever
	// caller's terminalize invocation sync.Once selects as the first is the
	// only one that computes and persists the terminal manifest, publishes
	// the completed/lost lifecycle event, notifies completion, releases the
	// quota reservation, and releases the lease -- see terminalize's doc
	// comment.
	terminalOnce sync.Once

	// done closes once run's Wait call has returned, both drain goroutines
	// have finished reading their stream to completion, and terminalize has
	// returned, so a caller (test, or a later microtask) can observe the
	// entry's complete terminal transition without polling.
	done chan struct{}

	// exited closes inside doTerminalize as soon as this process's terminal
	// manifest is durably persisted (or, for a dependency-free entry, as
	// soon as that attempt has been made) -- strictly before the lifecycle
	// publish and completion notify calls that follow it. Task 9C:
	// Supervisor.Shutdown's "confirm every tree has exited" step waits on
	// this channel, deliberately not on done, so a slow or backpressured
	// lifecycleSink.publish/completionNotifier.notify call can never delay
	// Shutdown's own completion (combined-acceptance: "notification
	// backpressure cannot block terminalization"). Every other caller
	// (wait.go's Wait, via the entry's done/generation wake mechanism) is
	// unaffected and continues to depend on done exactly as it did before
	// 9C: it needs the fully completed terminal transition, not merely
	// confirmed-exited.
	//
	// Supervisor.Start populates this field for every entry it registers;
	// restore.go's reopenEntry sets it too (reusing its already-closed done
	// channel, since a restored entry is terminal by construction), purely
	// for hygiene. Every entry Shutdown ever actually waits on comes from
	// Start -- Restore's reopened entries are already terminal (done
	// pre-closed) and are filtered out of Shutdown's snapshot before exited
	// is ever touched. This field is nil-tolerant like every other entry
	// dependency (see bumpGeneration's identical nil-guard on wake) so a
	// bare test-built entry that predates 9C still terminalizes cleanly
	// without it.
	exited chan struct{}
}

// run is the single goroutine an entry owns for the rest of its lifetime,
// matching the spec's "One entry goroutine owns wait, activity, and stream
// drain" (Task 8's combined-acceptance text). Supervisor.Start spawns run
// exactly once, immediately after registering the entry.
//
// ctx is the entry's own lifetime-scoped context -- Supervisor.Start builds
// it with context.WithCancel(context.Background()), deliberately NOT the
// ctx a caller passed into Supervisor.Start. Per the Harness
// PreparedProcess/Process contract (harness/pkg/tool/process.go):
// "The Start context governs setup through process handoff only. Once
// Start returns a Process, that process lives ... independently of the
// Start context." Cancelling a caller's original Start context after Start
// has already returned a Handle must never cancel this process's ongoing
// Wait or drain; using a separate background-derived context here is what
// makes that true.
//
// run drains Stdout and Stderr concurrently via two drain goroutines that
// both funnel through appendChunk's single per-process append sequence.
// This is harmless in PTY mode too: tool.Process's doc comment guarantees
// Stderr "remains non-nil but is closed and empty" for a PTY-mode process,
// so its drain goroutine simply observes EOF immediately and contributes no
// bytes -- no StreamMode branch is needed here.
//
// run also drains a tool.ProcessActivitySource activity channel, if the
// live tool.Process optionally implements one (drainActivity), invalidating
// this entry's bound observation cache once per received tool.ProcessActivity.
// run additionally invalidates observations once at the very top (spawn)
// and once more after every drain goroutine -- stdout, stderr, and
// activity, if any -- has finished and Wait has returned (completion),
// matching the spec's "Workspace coordination" section: "Observation caches
// are invalidated at process spawn, on reported filesystem activity ...,
// and at process completion."
//
// Once every drain goroutine has finished and Wait has returned, run
// classifies the ProcessResult/error Wait returned into this package's
// State/Result domain and calls terminalize with it (classifyWaitOutcome,
// terminalize). run always calls terminalize with context.Background(),
// never ctx: ctx is this entry's own lifetime-scoped context, and a future
// caller that terminates the process by canceling it (e.g. a stop request
// cancelling lifetimeCancel to interrupt an in-flight Wait) must not also
// cause the resulting terminal manifest Save, lifecycle publish, and
// completion notify to be skipped or fail because the same context is
// already canceled by the time run reaches them. The completion
// invalidateObservations call is made with context.Background() for the
// same reason.
func (e *entry) run(ctx context.Context) {
	// ctx is lifetimeCtx (supervisor.go's go e.run(lifetimeCtx)), and
	// lifetimeCancel is its paired cancel func. Deferring the call here
	// releases the context's resources once run -- the sole owner of this
	// context's lifetime -- returns, regardless of whether anything actually
	// observes ctx.Done() beforehand. context.CancelFunc is idempotent, so
	// this is safe even after a future stop-request path (see this
	// function's own doc comment above) calls lifetimeCancel early to
	// interrupt an in-flight Wait. Nil-tolerant like every other entry
	// dependency (see this file's other "if e.xxx != nil" guards): a bare
	// *entry built directly by a unit test, rather than through
	// Supervisor.Start, never sets it.
	if e.lifetimeCancel != nil {
		defer e.lifetimeCancel()
	}

	e.invalidateObservations(ctx)

	var drainWG sync.WaitGroup
	drainWG.Add(2)
	go e.drain(&drainWG, e.process.Stdout())
	go e.drain(&drainWG, e.process.Stderr())

	// Activities is an optional capability: only start a third drain
	// goroutine when the live tool.Process actually implements
	// tool.ProcessActivitySource and returns a non-nil channel. Every
	// drainWG.Add call happens here, strictly before Wait is called below,
	// so there is no race with drainWG.Wait().
	if source, ok := e.process.(tool.ProcessActivitySource); ok {
		if activities := source.Activities(); activities != nil {
			drainWG.Add(1)
			go e.drainActivity(ctx, &drainWG, activities)
		}
	}

	// Draining to EOF must happen before Wait, not after: EOF on
	// stdout/stderr is driven by the child process itself closing those
	// descriptors (which happens independently of whether the parent has
	// called Wait), but a real os/exec-backed Process's Wait closes the
	// underlying pipe files once the child exits. Calling Wait first would
	// race that close against these drain goroutines' still-in-progress
	// reads on the same pipes and can silently truncate or lose real
	// output -- exactly the ordering the Go exec package's docs warn
	// against ("it is incorrect to call Wait before all reads from the
	// pipe have completed"). tool.ProcessActivitySource's own contract
	// ("the activity channel must close before Process.Wait returns") is
	// unaffected either way: draining it here, before Wait is even called,
	// still satisfies that guarantee.
	drainWG.Wait()

	result, waitErr := e.process.Wait(ctx)

	e.invalidateObservations(context.Background())

	state, outcome := classifyWaitOutcome(result, waitErr)
	e.terminalize(context.Background(), state, outcome, time.Now().UTC())

	close(e.done)
}

// invalidateObservations calls e.observations.invalidate for this entry's
// Handle, tolerating a nil observations dependency exactly like every other
// entry dependency (manifests, lifecycle, notifications, releaseQuota,
// lease, onTerminal). See run's doc comment for the three points in a
// process's lifetime it is called from.
func (e *entry) invalidateObservations(ctx context.Context) {
	if e.observations == nil {
		return
	}
	_ = e.observations.invalidate(ctx, e.identity.Handle)
}

// drainActivity ranges over activities, calling invalidateObservations once
// per received tool.ProcessActivity, until activities closes. Per the
// Harness contract (tool.ProcessActivitySource's doc comment): "The
// activity channel must close before Process.Wait returns" -- so this
// goroutine always finishes on its own before run's Wait call above
// returns, and run's drainWG.Wait() call waits for it exactly like it waits
// for the stdout/stderr drain goroutines. If activities closes having never
// sent anything, `for range` returns immediately: this goroutine never
// blocks forever on a channel that only closes.
func (e *entry) drainActivity(ctx context.Context, wg *sync.WaitGroup, activities <-chan tool.ProcessActivity) {
	defer wg.Done()
	for range activities {
		e.invalidateObservations(ctx)
	}
}

// drain reads r to EOF in bounded chunks, appending every non-empty chunk
// to the entry's Buffer and Spool through appendChunk. It never returns an
// error to its caller: a read or append failure only stops that one
// stream's draining early (the other stream and Wait are unaffected). This
// mirrors the spec's "a process is never terminated for producing too much
// output" -- a read or append failure here is similarly non-fatal to the
// process; it is simply the point at which that one stream stops
// contributing further bytes.
func (e *entry) drain(wg *sync.WaitGroup, r io.ReadCloser) {
	defer wg.Done()
	defer r.Close()

	buf := make([]byte, drainChunkBytes)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			e.appendChunk(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// appendChunk appends chunk to both the in-memory Buffer and the durable
// Spool while holding appendMu, so a chunk read from Stdout and a chunk
// read from Stderr can never interleave mid-write; see appendMu's doc
// comment for why this is the entry's single per-process append sequence.
// Append errors from either store are swallowed here for the same reason
// drain's are: 8B's drain path must never abort or block the process over
// an output-storage failure. Surfacing a persistent Spool append failure
// (e.g. disk full) more visibly is out of 8B's scope.
//
// A non-empty chunk also bumps this entry's generation (Task 9A), waking
// every waiter currently blocked in wait.go's blocking Wait modes. The
// bump happens after appendMu is released: it uses its own lock (waitMu),
// and there is no ordering requirement between releasing appendMu and
// bumping generation beyond "after this chunk is durably appended", which
// is already true once appendMu.Unlock returns.
func (e *entry) appendChunk(chunk []byte) {
	if len(chunk) == 0 {
		return
	}

	e.appendMu.Lock()
	_, _ = e.buffer.Append(chunk)
	_, _ = e.spool.Append(chunk)
	e.appendMu.Unlock()

	e.bumpGeneration()
}

// bumpGeneration advances the entry's generation counter by one and wakes
// every waiter currently blocked on the previous generation's wake
// channel, by closing it and replacing it with a fresh one -- see wake's
// doc comment for the exact "close to broadcast, replace to re-arm"
// pattern and why it is race-safe with generationSnapshot. wake may still
// be nil here for an entry built directly by an older test fixture that
// predates Task 9A (entry_test.go's TestEntryRunClosesDoneAfterDrainingBothStreams,
// newRaceEntry); closing a nil channel would panic, so the first bump for
// such an entry only allocates wake, skipping the close (there is nothing
// listening on a channel that was never handed out).
func (e *entry) bumpGeneration() {
	e.waitMu.Lock()
	e.generation++
	if e.wake != nil {
		close(e.wake)
	}
	e.wake = make(chan struct{})
	e.waitMu.Unlock()
}

// generationSnapshot atomically returns this entry's current generation
// together with the wake channel that will close on the very next
// bumpGeneration call. Reading both values together, under the same lock
// acquisition, is what lets a caller compare generation against its own
// last-observed value and then select on wake with no lost-wakeup window
// (see wake's doc comment).
func (e *entry) generationSnapshot() (uint64, <-chan struct{}) {
	e.waitMu.Lock()
	defer e.waitMu.Unlock()
	return e.generation, e.wake
}

// terminalize is the one-shot compare-and-set terminal arbiter every path
// that can end this process's lifetime must call: today only run's natural
// Wait() return (this file's only caller), and -- starting in Task 9C/18 --
// also an explicit stop request, a deadline timeout, and supervisor
// shutdown, none of which exist yet. terminalOnce (sync.Once) guarantees
// that whichever concurrent caller's invocation is selected first is the
// only one whose state/result/finishedAt actually take effect: it is the
// only call that computes and persists the terminal manifest, publishes the
// completed/lost lifecycle event, notifies completion, releases the quota
// reservation, and releases the lease. Every other concurrent (or later)
// caller's terminalize call returns having done nothing -- its own
// state/result/finishedAt are silently discarded -- which is exactly the
// combined-acceptance text's "one terminal manifest, one completion
// callback, one lease release" for a process that could otherwise reach its
// terminal state via more than one racing path at once.
//
// ctx is used only for the lifecycle-publish and completion-notify calls
// doTerminalize makes; see run's doc comment for why run always passes
// context.Background() rather than its own (possibly already-canceled)
// lifetime context.
func (e *entry) terminalize(ctx context.Context, state State, result Result, finishedAt time.Time) {
	e.terminalOnce.Do(func() {
		e.doTerminalize(ctx, state, result, finishedAt)
	})
}

// doTerminalize is terminalize's guarded body; terminalOnce.Do ensures it
// runs at most once per entry. It prefers to reload the manifest Start
// already durably persisted (rather than reconstructing one from scratch)
// so the terminal Save carries forward every already-persisted field
// unchanged. If that reload fails, it falls back to synthesizeManifest,
// which assembles an equally complete terminal Manifest from e.identity and
// e.base (manifestBase) -- the same fields Supervisor.Start already
// captured onto the entry when it first persisted them -- rather than
// silently skipping the terminal Save entirely (Phase Gate 2 finding: a
// reload failure must never leave a process's manifest stuck at a
// nonterminal state, nor fall through to publish/notify with a zero-value
// manifest and a nil EventID/CommandID the real Harness event codec would
// reject). Either way, the resulting manifest's Events -- the manifest's
// stable LifecycleEventIDs (manifest.go) -- is what makes the completed/lost
// lifecycle event and the completion notification reuse the exact
// pre-persisted EventID/CommandID values rather than minting fresh ones, on
// this call and on any hypothetical future retry of the same publish step
// (spec "Manifests and durability": "stable lifecycle EventIDs ...
// allocated and persisted before publication"). publish/notify are only
// ever called with a non-zero EventID/CommandID (see the IsZero guards
// below); if neither the reload nor the synthesis fallback can produce one
// -- only possible for a bare entry a test built directly, with no
// manifests/base wired at all -- they are skipped rather than invoked with
// an invalid ID. Every dependency is nil-tolerant: a bare entry built
// directly by a test (no manifests/lifecycle/notifications/releaseQuota/
// lease wired) still terminalizes cleanly, simply skipping whichever step
// has no dependency to call.
func (e *entry) doTerminalize(ctx context.Context, state State, result Result, finishedAt time.Time) {
	cursors := SpoolCursors{
		TotalBytes:   e.spool.TotalBytes(),
		RetainedFrom: e.spool.RetainedFrom(),
	}

	events, haveEvents := e.terminalManifest(state, result, finishedAt, cursors)

	// exited closes here -- see its doc comment for why this must happen
	// strictly before the lifecycle publish/completion notify calls below,
	// rather than after them (which is where done closes, in run, once this
	// whole method returns).
	if e.exited != nil {
		close(e.exited)
	}

	// The quota reservation and the workspace lifetime lease are released
	// here -- BEFORE lifecycle publication and completion notification,
	// below -- deliberately reordered from this package's original
	// publish-then-notify-then-release sequence. By the time doTerminalize
	// ever runs, the process has ALREADY fully exited (this method is only
	// reached after Process.Wait has returned), so it can no longer touch
	// the workspace regardless of when its lease is released: deferring
	// release behind two best-effort notifications served no safety
	// purpose. It did, however, create a real deadlock risk once a real
	// completion notifier needs to reach its owning loop (the product composition root/Harness's
	// production wiring dispatches into that loop's own command channel):
	// if that loop's actor is itself synchronously blocked trying to
	// acquire a workspace lease this exact still-held lease conflicts with
	// (e.g. a SessionIdle-triggered checkpoint racing a just-backgrounded
	// process), the old ordering meant the lease could never be released
	// until notify() reached an actor that could only become reachable
	// AFTER that same lease was released -- a genuine circular wait,
	// reproduced end to end by the product composition root's Task 28 integration tests as a
	// ~10 second stall (bounded only by an unrelated shutdown drain
	// timeout, not by anything in this package). Releasing first breaks
	// that cycle unconditionally: a slow or unreachable notifier can no
	// longer hold the lease hostage.
	if e.releaseQuota != nil {
		e.releaseQuota(e.reservation)
	}
	if e.lease != nil {
		_ = e.lease.Release()
	}

	kind := tool.ProcessLifecycleCompleted
	eventID := events.Completed
	if state == StateLostOnRestore {
		kind = tool.ProcessLifecycleLost
		eventID = events.Lost
	}

	if e.lifecycle != nil && haveEvents && !eventID.IsZero() {
		_ = e.lifecycle.publish(ctx, lifecycleTerminalEvent{
			EventID:    eventID,
			Kind:       kind,
			Identity:   e.identity,
			State:      state,
			Result:     result,
			CreatedAt:  e.base.createdAt,
			StartedAt:  e.base.startedAt,
			FinishedAt: finishedAt,
		})
	}

	if e.notifications != nil && haveEvents && !events.CommandID.IsZero() {
		_ = e.notifications.notify(ctx, completionEvent{
			CommandID: events.CommandID,
			Owner:     e.identity.Owner,
			Handle:    e.identity.Handle,
			State:     state,
			Result:    result,
		})
	}

	if e.onTerminal != nil {
		e.onTerminal(e.identity.Handle, e.identity.Owner)
	}
}

// terminalManifest produces and persists the terminal Manifest for this
// entry, preferring a reload of the manifest Start already durably
// persisted and falling back to synthesizeManifest when that reload fails.
// It returns the resulting manifest's Events (the values doTerminalize's
// publish/notify calls must use) and whether a manifest was actually
// produced at all -- false only for a bare test-built entry with neither a
// reloadable manifest nor a populated e.base to synthesize from.
//
// Either path's Save call is best-effort, matching the rest of this file's
// established convention (drain/appendChunk's storage-failure handling,
// Supervisor.Start's own synchronous terminal-Save branches): a Save
// failure here does not prevent doTerminalize from still publishing/
// notifying with the stable IDs it already knows in memory, and does not
// abort quota/lease release or the onTerminal retention hook -- exactly the
// fault-injection contract the Phase Gate 2 review asked for ("quota
// release / lease release / onTerminal retention hook still run correctly"
// even when persistence itself is degraded).
func (e *entry) terminalManifest(state State, result Result, finishedAt time.Time, cursors SpoolCursors) (LifecycleEventIDs, bool) {
	if e.manifests != nil {
		if loaded, err := e.manifests.Load(e.identity.Handle); err == nil {
			loaded.State = state
			loaded.FinishedAt = &finishedAt
			loaded.Result = result
			loaded.Cursors = cursors
			loaded.CompletionPublished++
			_ = e.manifests.Save(loaded)
			return loaded.Events, true
		}
	}

	// The reload failed (or there is no manifests dependency at all):
	// synthesize as complete a terminal manifest as this entry's own
	// in-memory state allows, rather than silently abandoning the terminal
	// Save.
	synthesized, ok := e.synthesizeManifest(state, result, finishedAt, cursors)
	if !ok {
		return LifecycleEventIDs{}, false
	}
	if e.manifests != nil {
		_ = e.manifests.Save(synthesized)
	}
	return synthesized.Events, true
}

// synthesizeManifest builds a best-effort terminal Manifest entirely from
// this entry's own in-memory state -- e.identity and e.base (both set once,
// at Start, and immutable for the rest of a process's lifetime) plus this
// call's own state/result/finishedAt/cursors parameters -- for
// terminalManifest to persist when e.manifests.Load has failed to reload
// the manifest Start already durably persisted. ok is false only when
// e.identity.Handle is itself invalid: a bare entry a test built directly
// that was never registered by Supervisor.Start at all, and therefore has
// nothing meaningful to persist regardless of which path is taken.
//
// Every field this method sets comes from e.identity/e.base, never from the
// failed reload: CommandMetadata, Access, TTY, CreatedAt, Deadline, and
// Events are all captured once by Supervisor.Start (see manifestBase's doc
// comment) specifically so a later reload failure here never has to guess
// at them. CompletionPublished is set directly to 1 rather than
// incremented: terminalOnce guarantees doTerminalize runs at most once per
// entry, and Supervisor.Start's own manifest writes never touch
// CompletionPublished, so the true prior value this terminal transition is
// replacing is always 0 -- the same effective result the loaded-manifest
// path's current.CompletionPublished++ produces.
func (e *entry) synthesizeManifest(state State, result Result, finishedAt time.Time, cursors SpoolCursors) (Manifest, bool) {
	if !e.identity.Handle.Valid() {
		return Manifest{}, false
	}
	startedAt := e.base.startedAt
	return Manifest{
		Identity:            e.identity,
		Command:             e.base.command,
		Access:              e.base.access,
		TTY:                 e.base.tty,
		State:               state,
		CreatedAt:           e.base.createdAt,
		StartedAt:           &startedAt,
		FinishedAt:          &finishedAt,
		Deadline:            e.base.deadline,
		Cursors:             cursors,
		Result:              result,
		Events:              e.base.events,
		CompletionPublished: 1,
	}, true
}

// classifyWaitOutcome maps the tool.ProcessResult/error pair
// tool.Process.Wait returns into this package's terminal State and Result,
// which terminalize's manifest write requires. A non-nil waitErr -- the
// runner itself failed to observe the process's own exit, as distinct from
// the process exiting on its own -- has no dedicated ProcessTerminalReason
// and conservatively maps to StateFailed with Result.Reason "failed",
// mirroring reasonString's own fallback for any ProcessTerminalReason this
// package's closed State domain has no direct counterpart for.
func classifyWaitOutcome(res tool.ProcessResult, waitErr error) (State, Result) {
	if waitErr != nil {
		return StateFailed, Result{Reason: "failed"}
	}
	state := terminalStateForReason(res.Reason)
	result := Result{Reason: reasonString(res.Reason)}
	if state == StateExited {
		code := res.ExitCode
		result.ExitCode = &code
	}
	return state, result
}

// terminalStateForReason maps a harness tool.ProcessTerminalReason to this
// package's closed State domain (state.go). tool.ProcessTerminalRunnerShutdown
// and tool.ProcessTerminalOutputLimit have no state of their own in the
// harness lifecycle tuple table either (both pair with either Terminated or
// Killed depending on which signal was actually used) and conservatively
// map to StateTerminated here; neither reason is reachable through 8C's
// only terminalize caller (run's natural Wait() return), so this mapping
// exists for completeness rather than current exercise. An unrecognized
// reason (including the zero value) conservatively maps to StateFailed.
func terminalStateForReason(reason tool.ProcessTerminalReason) State {
	switch reason {
	case tool.ProcessTerminalExited:
		return StateExited
	case tool.ProcessTerminalTimedOut:
		return StateTimedOut
	case tool.ProcessTerminalInterrupted:
		return StateInterrupted
	case tool.ProcessTerminalTerminated, tool.ProcessTerminalRunnerShutdown, tool.ProcessTerminalOutputLimit:
		return StateTerminated
	case tool.ProcessTerminalKilled:
		return StateKilled
	case tool.ProcessTerminalLostOnRestore:
		return StateLostOnRestore
	default:
		return StateFailed
	}
}

// reasonString maps a harness tool.ProcessTerminalReason to Manifest.Result's
// documented closed set of reason strings (manifest.go's Result.Reason doc
// comment: "exited", "failed", "timed-out", "interrupted", "terminated",
// "killed", "lost-on-restore"). An unrecognized reason (including the zero
// value) conservatively maps to "failed".
func reasonString(reason tool.ProcessTerminalReason) string {
	switch reason {
	case tool.ProcessTerminalExited:
		return "exited"
	case tool.ProcessTerminalTimedOut:
		return "timed-out"
	case tool.ProcessTerminalInterrupted:
		return "interrupted"
	case tool.ProcessTerminalTerminated, tool.ProcessTerminalRunnerShutdown, tool.ProcessTerminalOutputLimit:
		return "terminated"
	case tool.ProcessTerminalKilled:
		return "killed"
	case tool.ProcessTerminalLostOnRestore:
		return "lost-on-restore"
	default:
		return "failed"
	}
}
