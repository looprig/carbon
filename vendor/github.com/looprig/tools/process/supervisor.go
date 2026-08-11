package process

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/tool"
)

// Lease is Task 8's minimal placeholder for the Harness lifetime workspace
// lease a caller acquires before calling Start (spec "Workspace
// coordination": "Tools acquires the matching Harness lifetime workspace
// lease" between PrepareProcess and Start). Task 8 does not wire Harness
// end-to-end and does not import any concrete Harness lease type; Start only
// needs "the resource the caller already acquired that must be released on
// every exit path". The method is named Release, not Close, so a call site
// reads unambiguously against PreparedProcess.Close (which releases the
// preparation, not the lease) -- Task 15/19 replaces this interface with (or
// adapts it to) the real Harness lease type.
type Lease interface {
	Release() error
}

// StorageCeiling bounds one supervised process's in-memory rolling window
// and disk spool retention window (mirrors Config's
// MaxProcessInMemoryBytes/MaxProcessSpoolBytes pair, and buffer.go's
// NewBuffer / spool.go's OpenSpool ceiling parameters). It is supplied per
// Start call rather than read from the Supervisor's Config, so a caller
// (Bash) can request a smaller window for one process; a non-positive field
// falls back to the Supervisor's configured per-process default
// (reserveQuota). Reserving InMemoryBytes/SpoolBytes against the
// Supervisor's aggregate ceilings before Start is this task's realization of
// the combined-acceptance text's "memory" and "spool" quotas.
type StorageCeiling struct {
	InMemoryBytes int64
	SpoolBytes    int64
}

// YieldSettings carries one Start call's explicit backgrounding preference
// (spec "Bash API": a foreground call may explicitly yield to the
// background rather than staying attached and returning inline). Task 8
// does not implement yield/backgrounded-handoff behavior yet -- that is
// part of Task 8B/8C's lifecycle-event work and, ultimately, the Bash tool
// built in Phase 4; this struct only reserves the field Start's signature
// commits to.
type YieldSettings struct {
	// Yield requests that the process detach to the background immediately
	// rather than staying attached to the foreground Bash call.
	Yield bool
}

// lifecycleSink is the narrow lifecycle-publication capability the spec's
// "Supervisor lifetime" section describes as half of Harness's
// SessionResourceServices ("stable lifecycle EventIDs ... allocated and
// persisted before the corresponding Harness lifecycle event ... may be
// published"). 8A only accepted and stored an implementation (both at
// construction and, per admission, from Start -- see NewSupervisor and
// Start's doc comments for why both exist), never called one. Task 8C added
// publish, the method entry.terminalize calls exactly once per process, at
// its one-shot terminal transition, always with the manifest's already-
// persisted stable EventID for event.Kind (manifest.go's
// LifecycleEventIDs.Completed or .Lost) -- never a freshly minted one.
//
// Phase Gate 2's Task 8 combined-acceptance follow-up adds publishStart, the
// method Start calls exactly once per process, immediately after the
// manifest's StateRunning Save succeeds: Started if the caller did not
// request immediate backgrounding (YieldSettings.Yield == false), or
// Backgrounded if it did (Yield == true) -- see lifecycleStartEvent's doc
// comment (entry.go) for the exact one-of-{Started,Backgrounded} discipline.
// A foreground-attached process later explicitly yielded to background
// mid-flight is a harder case Task 8 does not implement -- there is no
// caller for it yet (Phase 4's Bash tool) -- and is out of this follow-up's
// scope.
type lifecycleSink interface {
	publish(ctx context.Context, event lifecycleTerminalEvent) error
	publishStart(ctx context.Context, event lifecycleStartEvent) error
}

// completionNotifier is the other half of SessionResourceServices: the
// bounded completion notification submitted to the owning loop (spec
// "Supervisor lifetime"; mirrors harness/pkg/tool's
// ProcessCompletionNotification). Like lifecycleSink, 8A accepted and
// stored an implementation but never called one. Task 8C adds notify, the
// method entry.terminalize calls exactly once per process, at the same
// one-shot terminal transition, always with the manifest's already-
// persisted stable LifecycleEventIDs.CommandID -- never a freshly minted
// one.
type completionNotifier interface {
	notify(ctx context.Context, event completionEvent) error
}

// observationInvalidator is the originating loop's observation-cache
// invalidation capability (spec "Workspace coordination": "Observation
// caches are invalidated at process spawn, on reported filesystem activity
// ..., and at process completion"). 8A accepted and stored one per
// admission but never called it; Task 8D adds the invalidate method and
// wires the spawn/activity/completion call sites in entry.go's run.
//
// Every activity invalidates the complete observation cache bound to a
// process (harness tool.ProcessActivity's doc comment: "Every activity
// invalidates the complete bound observation cache; scoped observation
// paths are intentionally not represented"), so invalidate carries no
// scoping beyond which process (handle) triggered it.
type observationInvalidator interface {
	invalidate(ctx context.Context, handle Handle) error
}

// reservation records the exact quota amounts one admission attempt
// reserved (reserveQuota), so releaseQuota always reverses precisely what
// was reserved -- independent of which Config default filled in a
// non-positive StorageCeiling field, and independent of any later Config
// change.
type reservation struct {
	loopID      uuid.UUID
	sessionID   uuid.UUID
	memoryBytes int64
	spoolBytes  int64
}

// terminalEntry is the Supervisor's per-terminal-entry retention bookkeeping
// (Task 8D: terminal LRU retention), recorded by recordTerminal the instant
// an entry terminalizes and consulted by lruVictimLocked to choose which
// entry to evict once a session's terminal count exceeds
// Config.MaxRetainedCompletedProcessesPerSession.
//
// completionSeq stands in for a genuine "last accessed" timestamp:
// ProcessOutput and every other lookup/read path are Phase 4's job, not
// Task 8's, so there is no caller yet that could touch a real
// last-accessed marker on read. Until that path exists, "least recently
// used" here means "least recently completed" -- effectively
// FIFO-by-completion-order -- using a monotonically increasing sequence
// number (not wall-clock time) so ordering is exact and immune to clock
// resolution even when two entries terminalize within the same instant.
// Once a Phase 4 lookup path exists, it should bump this same bookkeeping
// (or a renamed lastAccessSeq) on every successful read, so eviction order
// reflects genuine access recency rather than only completion recency.
type terminalEntry struct {
	sessionID     uuid.UUID
	completionSeq int64
}

// Supervisor is the runner-free process supervisor described by the spec's
// "Supervisor lifetime" and "Workspace coordination" sections: it never
// calls tool.AsyncProcessRunner.PrepareProcess itself, and NewSupervisor
// deliberately does not accept a tool.AsyncProcessRunner at all. A caller
// (Task 8 does not implement this caller; it is the Bash tool built in
// Phase 4, per the spec's "Only Bash owns execution authority") resolves a
// runner, calls PrepareProcess, acquires the matching workspace Lease, and
// hands the resulting tool.PreparedProcess to Start. Supervisor then owns
// admission -- reserving quotas and consuming the single-use preparation
// (this microtask, 8A) -- and, starting in Task 8B, durable handoff, stream
// drain, terminal arbitration, and retention.
//
// A Supervisor is safe for concurrent use by multiple goroutines.
type Supervisor struct {
	cfg       Config
	manifests *ManifestStore
	// spoolRoot is the private resource root every entry's Spool is opened
	// beneath (spool.go's OpenSpool root parameter). Start opens one Spool
	// per admitted process here, keyed by that process's Handle.
	spoolRoot string

	// lifecycle and notifications are the Supervisor's session-scoped
	// SessionResourceServices capabilities, accepted at construction per
	// the combined-acceptance text ("its factory accepts lifecycle ...
	// notification ... dependencies"). Start also accepts a lifecycleSink
	// per call (recorded on that process's entry): the per-call sink is
	// what a later microtask actually publishes a given process's events
	// through (mirroring how "each admission captures the originating
	// loop's observation capability" in the spec), while these
	// construction-time fields are the Supervisor's own session-wide
	// service handles for supervisor-level events. Neither is called
	// anywhere in 8A; both are empty interfaces (see their doc comments)
	// until a later microtask adds methods.
	lifecycle     lifecycleSink
	notifications completionNotifier

	mu sync.Mutex

	// runningByLoop and runningBySession track the per-loop/per-session
	// running-process counts reserveQuota enforces against
	// cfg.MaxRunningProcessesPerLoop/MaxRunningProcessesPerSession. A
	// missing key means zero; reserveQuota/releaseQuota keep both maps
	// exact mirror images of every currently-reserved entry.
	runningByLoop    map[uuid.UUID]int
	runningBySession map[uuid.UUID]int
	// reservedMemoryBytes and reservedSpoolBytes are the running totals
	// reserveQuota enforces against cfg.MaxAggregateInMemoryBytes/
	// MaxAggregateSpoolBytes.
	reservedMemoryBytes int64
	reservedSpoolBytes  int64

	// entries is the registry of admitted processes, keyed by their
	// Handle. Start adds to it on every successful admission; recordTerminal
	// (Task 8D) is the only thing that ever removes an entry, when terminal
	// LRU retention evicts it.
	entries map[Handle]*entry

	// terminal tracks retention bookkeeping for every currently-registered
	// terminal entry, keyed by Handle (Task 8D: terminal LRU retention).
	// recordTerminal adds a record the instant an entry terminalizes;
	// evicting an entry removes its record from both terminal and entries
	// together, so the two maps are always exact mirror images for the
	// terminal subset -- every key in terminal is also a key in entries,
	// and every terminal entries[h] has a matching terminal[h]. A running
	// entry never appears here, which is what makes a running entry
	// unevictable regardless of the configured retention limit.
	terminal map[Handle]terminalEntry
	// nextTerminalSeq is the monotonically increasing counter recordTerminal
	// assigns to each newly terminalized entry's completionSeq (see
	// terminalEntry's doc comment for what this stands in for).
	nextTerminalSeq int64

	// pendingWaitersBySession tracks, per session, how many blocking (any
	// or all mode) wait.go Wait calls are currently outstanding (Task 9A).
	// A missing key means zero. acquireWaiterSlot/releaseWaiterSlot
	// (wait.go) keep it an exact mirror of every currently-blocked waiter,
	// enforcing cfg.MaxPendingWaiters -- "the outstanding ProcessOutput
	// wait: any|all waiters a session admits concurrently" (config.go) --
	// the same way runningByLoop/runningBySession enforce their own
	// quotas. Poll-mode Wait calls never touch this map: they return
	// immediately and never block.
	pendingWaitersBySession map[uuid.UUID]int

	// shuttingDown is set once by closeAdmission; Start checks it first and
	// rejects admission immediately once true (Task 8D: "shutting down
	// rejects admission and input" -- this flag covers admission only; see
	// closeAdmission's doc comment for scope).
	shuttingDown bool

	// shutdownOnce guards Shutdown's actual coordinated-termination sequence
	// (Task 9C) so concurrent and later callers all share exactly one
	// shutdown attempt and its exact result -- combined-acceptance:
	// "concurrent shutdown callers receive the same result". shutdownErr is
	// written exactly once, by whichever caller's Do closure executes
	// doShutdown, and is safe for every other caller to read immediately
	// after its own Do call returns without any additional lock: sync.Once's
	// own memory-model guarantee ("the completion of a single call of f() is
	// synchronized before the return of any call of Do(f)") makes that read
	// safe.
	shutdownOnce sync.Once
	shutdownErr  error
}

// NewSupervisor returns a Supervisor governed by cfg (normalized; see
// Config.Normalize), persisting manifests to manifests and, from Task 8B
// on, opening spools beneath spoolRoot. lifecycle and notifications are the
// two narrow SessionResourceServices capabilities described by the spec's
// "Supervisor lifetime" section; see the Supervisor.lifecycle/notifications
// field doc for why they are accepted here even though 8A never calls
// either. NewSupervisor deliberately does not accept a
// tool.AsyncProcessRunner: the supervisor is runner-free by construction,
// unable to call PrepareProcess even if some future caller wanted it to.
func NewSupervisor(cfg Config, manifests *ManifestStore, spoolRoot string, lifecycle lifecycleSink, notifications completionNotifier) (*Supervisor, error) {
	normalized, err := cfg.Normalize()
	if err != nil {
		return nil, err
	}
	if manifests == nil {
		return nil, Wrap(CodeInvalidSettings, errors.New("manifest store is required"))
	}
	return &Supervisor{
		cfg:                     normalized,
		manifests:               manifests,
		spoolRoot:               spoolRoot,
		lifecycle:               lifecycle,
		notifications:           notifications,
		runningByLoop:           make(map[uuid.UUID]int),
		runningBySession:        make(map[uuid.UUID]int),
		entries:                 make(map[Handle]*entry),
		terminal:                make(map[Handle]terminalEntry),
		pendingWaitersBySession: make(map[uuid.UUID]int),
	}, nil
}

// closeAdmission marks the supervisor as shutting down: every subsequent
// Start call is rejected immediately with CodeSupervisorShuttingDown,
// before reserving any quota or touching the caller-supplied
// tool.PreparedProcess at all (TestSupervisorShutdownRejectsAdmission).
// closeAdmission is idempotent and safe to call concurrently with Start or
// with itself.
//
// closeAdmission only closes admission of new processes -- it never stops,
// signals, or waits for any already-running entry. Coordinated shutdown
// (tree-termination, escalation, waiting for every running entry to reach a
// terminal state) is Task 9C's job, not this microtask's; this flag is only
// the narrow "no new admission" prerequisite it will build on. It also does
// not yet reject ProcessInput -- "shutting down rejects admission and
// input" (combined-acceptance text) is only half-implemented here: Task
// 9/17's ProcessInput call path does not exist yet, and when it is built it
// should reuse this exact same shuttingDown flag rather than adding a
// second one.
func (s *Supervisor) closeAdmission() {
	s.mu.Lock()
	s.shuttingDown = true
	s.mu.Unlock()
}

// activateServices late-binds the Supervisor's session-wide lifecycle
// publisher and completion notifier after construction, under s.mu so a
// concurrent Start (which reads both fields, also under s.mu -- see
// servicesLocked) can never observe a torn or stale value. It is the
// counterpart NewSupervisorResource's own doc comment predicted ("a future
// task that wires services's real capabilities through to a live Supervisor
// is what would give Activate real work to do"): SupervisorResource.Activate
// is that future task's caller, adapting the real, validated
// tool.SessionResourceServices Harness's own session construction supplies
// (session_resource.go) into this package's private lifecycleSink/
// completionNotifier shapes and calling this method exactly once, before any
// bound process tool can be invoked. It is intentionally a plain,
// non-defensive setter: Harness's own SessionResource.Activate contract
// guarantees at-most-once activation per session resource
// (TestSessionResourcesActivateAndShutdownOnce, harness
// internal/sessionruntime), so this method adds no re-activation guard of
// its own.
func (s *Supervisor) activateServices(lifecycle lifecycleSink, notifications completionNotifier) {
	s.mu.Lock()
	s.lifecycle = lifecycle
	s.notifications = notifications
	s.mu.Unlock()
}

// servicesLocked returns the Supervisor's current session-wide lifecycle/
// notifications capabilities under s.mu, so Start's own read can never race
// a concurrent activateServices call. See activateServices' doc comment for
// why a lock is required here even though every field write happens exactly
// once, before any live tool call: the race detector has no way to see that
// happens-before relationship from field access alone.
func (s *Supervisor) servicesLocked() (lifecycleSink, completionNotifier) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lifecycle, s.notifications
}

// Shutdown implements the spec's "Supervisor lifetime" coordinated shutdown
// sequence: close admission to new processes, concurrently request graceful
// termination of every currently running process tree, escalate to a
// forceful kill for any tree still running once cfg.GracefulShutdownPeriod
// elapses, confirm every tree has exited, and return any teardown failure to
// the caller.
//
// Shutdown closes admission (closeAdmission) as its very first step --
// before requesting termination of a single process -- so a concurrent Start
// call is rejected immediately with CodeSupervisorShuttingDown even while
// Shutdown's later termination/escalation steps are still in flight
// (TestShutdownClosesAdmissionBeforeStop).
//
// Every currently running entry (snapshotRunningEntries) is signaled
// tool.ProcessSignalTerminate concurrently, one goroutine per entry; an
// entry that has not confirmed exit within cfg.GracefulShutdownPeriod of
// that request is then signaled tool.ProcessSignalKill
// (TestShutdownEscalatesAndConfirmsTrees). Shutdown waits on each entry's
// exited channel -- not done -- for that confirmation: see entry.go's
// exited field doc comment for why a slow or backpressured completion
// notification must never delay this step (combined-acceptance:
// "notification backpressure cannot block terminalization"). A signaled
// entry's actual terminal-state computation and manifest write are never
// bypassed or duplicated here -- run's existing natural Wait()-return path
// (entry.terminalize) is what every signal ultimately drives, exactly as it
// already does for an unforced exit.
//
// Shutdown is idempotent and safe to call concurrently: only the first
// call's invocation actually runs the shutdown sequence (sync.Once); every
// concurrent or later caller receives that exact same result
// (TestShutdownConcurrentCallersShareResult). Only the first caller's ctx is
// ever used -- every other caller's ctx argument is ignored, exactly like
// every other input to a no-op Do call.
//
// A teardown failure -- a Signal call that itself returns an error --
// doesn't stop the affected entry from reaching a terminal state: its run
// goroutine still classifies whatever outcome its live tool.Process.Wait
// ultimately reports and terminalizes normally. Shutdown only aggregates
// every such Signal failure into its own returned error, a *Error wrapping
// CodeTeardownFailed (TestShutdownTeardownFailureRetainsAuthority) --
// Shutdown retains authority over every process regardless of a teardown
// hiccup; it never loses track of one.
//
// Steps the ordered sequence describes but that need no new code here:
// terminal manifests are already flushed and workspace leases are already
// released by entry.doTerminalize (Task 8C), which every signaled entry
// still reaches through its own run goroutine; any caller still blocked in
// Wait is already released once an entry's done channel closes (Task 9A's
// existing generation/done wake mechanism), no new mechanism required.
// Closing storage handles is a deliberate no-op: neither ManifestStore nor
// Spool has (or should have) a Supervisor-wide Close of its own -- a
// process's completed output must remain readable through its Spool after
// Shutdown returns, exactly as Restore already relies on for a clean
// (non-lost) reconciliation of a shutdown-terminated process
// (TestSupervisorIntegrationShutdownAndRestore).
func (s *Supervisor) Shutdown(ctx context.Context) error {
	s.shutdownOnce.Do(func() {
		s.shutdownErr = s.doShutdown(ctx)
	})
	return s.shutdownErr
}

// shutdownTarget is one running entry doShutdown must terminate, paired with
// its Handle for potential future diagnostics.
type shutdownTarget struct {
	handle Handle
	entry  *entry
}

// doShutdown is Shutdown's guarded body; shutdownOnce.Do ensures it runs at
// most once. See Shutdown's doc comment for the full ordered sequence this
// implements.
func (s *Supervisor) doShutdown(ctx context.Context) error {
	s.closeAdmission()

	targets := s.snapshotRunningEntries()
	if len(targets) == 0 {
		return nil
	}

	if errs := s.terminateEntries(ctx, targets); len(errs) > 0 {
		return Wrap(CodeTeardownFailed, errors.Join(errs...))
	}
	return nil
}

// snapshotRunningEntries returns every currently registered entry that has
// not yet reached a terminal state (entry.done not yet closed) -- the
// "snapshot every currently running handle" step of the ordered shutdown
// sequence. An entry already terminal -- including one Restore reopened,
// which is terminal by construction and was never live in this Supervisor
// instance at all -- is never signaled, and its exited channel (possibly
// nil for a Restore-reopened entry) is never touched.
func (s *Supervisor) snapshotRunningEntries() []shutdownTarget {
	s.mu.Lock()
	defer s.mu.Unlock()

	var targets []shutdownTarget
	for h, e := range s.entries {
		if closed(e.done) {
			continue
		}
		targets = append(targets, shutdownTarget{handle: h, entry: e})
	}
	return targets
}

// terminateEntries concurrently terminates every target -- one goroutine per
// entry, fanned into a plain sync.WaitGroup with a mutex-guarded error slice
// (golang.org/x/sync is only an indirect, unapproved dependency for this
// module; see CLAUDE.md's Dependencies section) -- and collects every
// non-nil error terminateOneEntry returns. A slow or failing entry never
// blocks or masks any other entry's own termination.
func (s *Supervisor) terminateEntries(ctx context.Context, targets []shutdownTarget) []error {
	var (
		mu   sync.Mutex
		errs []error
		wg   sync.WaitGroup
	)

	wg.Add(len(targets))
	for _, target := range targets {
		go func(target shutdownTarget) {
			defer wg.Done()
			if err := s.terminateOneEntry(ctx, target.entry); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}(target)
	}
	wg.Wait()

	return errs
}

// terminateOneEntry requests graceful termination of one running entry's
// process tree (tool.ProcessSignalTerminate), escalating to a forceful kill
// (tool.ProcessSignalKill) once cfg.GracefulShutdownPeriod elapses without
// e.exited having closed, and unconditionally waits for that confirmation
// before returning -- on every return path, including after a Signal error,
// so this call can never report success (or, for that matter, return at
// all) for an entry it has not actually confirmed exited
// (TestShutdownTeardownFailureRetainsAuthority). It deliberately does not
// select on ctx while waiting for e.exited: tool.ProcessSignalKill is not
// interceptable by a well-behaved process tree, so this wait is expected to
// complete on its own once the kill escalation above has been issued: an
// unconditional wait here can never regress into a permanent hang for a
// correctly implemented Process, and racing it against ctx would only trade
// one failure mode (a slow real teardown) for a strictly worse one (walking
// away from an entry Shutdown can no longer confirm exited at all). Every
// Signal call's error is joined into the returned error, reporting a
// teardown failure to the caller without ever narrowing that guarantee.
//
// e is always an entry snapshotRunningEntries returned, which only ever
// selects entries Supervisor.Start registered -- e.process and e.exited are
// therefore always non-nil here.
func (s *Supervisor) terminateOneEntry(ctx context.Context, e *entry) error {
	var errs []error

	if err := e.process.Signal(ctx, tool.ProcessSignalTerminate); err != nil {
		errs = append(errs, err)
	}

	grace := s.cfg.GracefulShutdownPeriod
	timer := time.NewTimer(grace)
	defer timer.Stop()

	select {
	case <-e.exited:
		return joinTeardownErrors(errs)
	case <-timer.C:
	}

	if err := e.process.Signal(ctx, tool.ProcessSignalKill); err != nil {
		errs = append(errs, err)
	}

	<-e.exited
	return joinTeardownErrors(errs)
}

// joinTeardownErrors returns nil for an empty errs slice (the common case:
// no teardown failure at all) and errors.Join(errs...) otherwise.
func joinTeardownErrors(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	return errors.Join(errs...)
}

// recordTerminal is entry.doTerminalize's post-terminal-Save retention hook
// (wired as onTerminal in Start), called at most once per entry --
// terminalize's own terminalOnce already guarantees that. It records handle
// as newly terminal for owner's session and, if that session's terminal
// count now exceeds cfg.MaxRetainedCompletedProcessesPerSession, evicts
// that session's least-recently-used terminal entry (lruVictimLocked):
// removes it from both s.terminal and s.entries, then best-effort deletes
// its durable manifest and spool (evictResources) -- "Eviction deletes the
// manifest and spool atomically from the supervisor's perspective" (spec).
//
// recordTerminal never touches a running entry: only a Handle that has
// already terminalized is ever added to s.terminal (Start never adds a
// running entry's Handle there), so a running entry can never be chosen as
// lruVictimLocked's victim regardless of how low the retention limit is
// configured (TestSupervisorNeverEvictsRunning).
func (s *Supervisor) recordTerminal(handle Handle, owner Owner) {
	s.mu.Lock()
	s.nextTerminalSeq++
	s.terminal[handle] = terminalEntry{sessionID: owner.SessionID, completionSeq: s.nextTerminalSeq}

	victim, evict := s.lruVictimLocked(owner.SessionID)
	var victimEntry *entry
	if evict {
		victimEntry = s.entries[victim]
		delete(s.terminal, victim)
		delete(s.entries, victim)
	}
	s.mu.Unlock()

	if evict {
		s.evictResources(victim, victimEntry)
	}
}

// lruVictimLocked reports the least-recently-used terminal entry (see
// terminalEntry's doc comment) belonging to sessionID, and whether that
// session's terminal count currently exceeds
// cfg.MaxRetainedCompletedProcessesPerSession and therefore has a victim to
// evict at all. Callers must hold s.mu.
func (s *Supervisor) lruVictimLocked(sessionID uuid.UUID) (Handle, bool) {
	var (
		victim    Handle
		victimSeq int64
		found     bool
		count     int
	)
	for h, rec := range s.terminal {
		if rec.sessionID != sessionID {
			continue
		}
		count++
		if !found || rec.completionSeq < victimSeq {
			victim, victimSeq, found = h, rec.completionSeq, true
		}
	}
	if count <= s.cfg.MaxRetainedCompletedProcessesPerSession {
		return "", false
	}
	return victim, found
}

// evictResources best-effort deletes the durable manifest and spool for an
// evicted terminal entry. It runs outside s.mu -- disk I/O never happens
// while holding the registry lock -- and never fails loudly: a failed
// delete simply leaves an orphaned file behind rather than blocking or
// panicking retention, mirroring drain/appendChunk's existing "storage
// failures are never fatal to supervision" pattern (entry.go). e is nil
// only if the registry was already inconsistent, which recordTerminal's own
// bookkeeping never produces; the nil check is defensive, not expected.
func (s *Supervisor) evictResources(handle Handle, e *entry) {
	if e != nil && e.spool != nil {
		_ = e.spool.Remove()
	}
	_ = s.manifests.Delete(handle)
}

// quotaExceeded builds the typed, model-facing quota-rejection error
// (spec "Stable errors"): reason is retained only as Cause for trusted
// diagnostics (logs, tests) and is never itself model-facing -- callers
// must render only Code (errors.go's doc comment).
func quotaExceeded(reason string) error {
	return Wrap(CodeProcessQuotaExceeded, errors.New(reason))
}

// reserveQuota reserves, in one atomic step, every quota the spec's
// "Quotas and retention" section requires before a process may be spawned:
// the owning loop's and session's running-process counts, and this
// process's share of the aggregate in-memory and spool byte ceilings
// (ceiling's fields, defaulted from cfg.MaxProcess{InMemory,Spool}Bytes
// when non-positive). All four checks are evaluated before any counter is
// mutated, so a rejected admission leaves every counter exactly as it was
// -- nothing to roll back (TestSupervisorRejectsSessionAndLoopQuota) --
// and Start must never call PreparedProcess.Start until this returns
// successfully (TestSupervisorReservesQuotaBeforeStart).
func (s *Supervisor) reserveQuota(owner Owner, ceiling StorageCeiling) (reservation, error) {
	memoryBytes := ceiling.InMemoryBytes
	if memoryBytes <= 0 {
		memoryBytes = s.cfg.MaxProcessInMemoryBytes
	}
	spoolBytes := ceiling.SpoolBytes
	if spoolBytes <= 0 {
		spoolBytes = s.cfg.MaxProcessSpoolBytes
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	switch {
	case s.runningByLoop[owner.LoopID] >= s.cfg.MaxRunningProcessesPerLoop:
		return reservation{}, quotaExceeded("max running processes per loop reached")
	case s.runningBySession[owner.SessionID] >= s.cfg.MaxRunningProcessesPerSession:
		return reservation{}, quotaExceeded("max running processes per session reached")
	case s.reservedMemoryBytes+memoryBytes > s.cfg.MaxAggregateInMemoryBytes:
		return reservation{}, quotaExceeded("max aggregate in-memory bytes reached")
	case s.reservedSpoolBytes+spoolBytes > s.cfg.MaxAggregateSpoolBytes:
		return reservation{}, quotaExceeded("max aggregate spool bytes reached")
	}

	s.runningByLoop[owner.LoopID]++
	s.runningBySession[owner.SessionID]++
	s.reservedMemoryBytes += memoryBytes
	s.reservedSpoolBytes += spoolBytes

	return reservation{
		loopID:      owner.LoopID,
		sessionID:   owner.SessionID,
		memoryBytes: memoryBytes,
		spoolBytes:  spoolBytes,
	}, nil
}

// releaseQuota exactly reverses a reservation reserveQuota returned. Start
// only ever calls it with a reservation reserveQuota actually produced (a
// failed reserveQuota call has nothing to release), so it does not need to
// guard against an unreserved zero value.
func (s *Supervisor) releaseQuota(r reservation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runningByLoop[r.loopID] > 0 {
		s.runningByLoop[r.loopID]--
	}
	if s.runningBySession[r.sessionID] > 0 {
		s.runningBySession[r.sessionID]--
	}
	s.reservedMemoryBytes -= r.memoryBytes
	s.reservedSpoolBytes -= r.spoolBytes
}

// releaseLease releases lease, tolerating a nil lease (a caller that has no
// lease to release, e.g. a read-only or test scenario).
func (s *Supervisor) releaseLease(lease Lease) {
	if lease == nil {
		return
	}
	_ = lease.Release()
}

// accessMode maps a Harness WorkspaceAccessKind (tool.WorkspaceAccess,
// PreparedProcess.EffectiveWorkspaceAccess) to this package's manifest-level
// AccessMode (manifest.go). Both are closed, three-value domains in the same
// read-only/scoped-write/broad-write order. An unrecognized kind -- which
// tool.WorkspaceAccessKind.Valid() should never let through, but this
// function does not assume that -- conservatively maps to the narrowest,
// AccessReadOnly, rather than producing an invalid AccessMode that would
// fail Manifest.Validate.
func accessMode(kind tool.WorkspaceAccessKind) AccessMode {
	switch kind {
	case tool.WorkspaceAccessScopedWrite:
		return AccessScopedWrite
	case tool.WorkspaceAccessBroadWrite:
		return AccessBroadWrite
	default:
		return AccessReadOnly
	}
}

// newLifecycleEventIDs allocates a fresh stable EventID for each lifecycle
// kind a process could eventually emit -- started, backgrounded, completed,
// lost -- plus a completion CommandID, all in one call so Start can persist
// every one of them in the very first manifest it ever saves (spec
// "Manifests and durability": "stable lifecycle EventIDs and completion-
// notification CommandID allocated and persisted before publication").
// Allocating all five up front, rather than minting each lazily at its own
// publish time, is what lets a later publish attempt -- including a
// hypothetical retry, or one made after a crash-recovery reload of the
// manifest from disk -- always reuse the exact same ID:
// ManifestStore.Save's validateLifecycleEventIDsStable (manifest.go)
// rejects any later attempt to reassign an already-non-zero field.
func newLifecycleEventIDs() (LifecycleEventIDs, error) {
	var ids [5]uuid.UUID
	for i := range ids {
		id, err := uuid.New()
		if err != nil {
			return LifecycleEventIDs{}, err
		}
		ids[i] = id
	}
	return LifecycleEventIDs{
		Started:      ids[0],
		Backgrounded: ids[1],
		Completed:    ids[2],
		Lost:         ids[3],
		CommandID:    ids[4],
	}, nil
}

// handleExists reports whether h already names an entry in the registry; it
// is NewHandle's collision check (identity.go's HandleExists), so a minted
// Handle can never collide with one already admitted.
func (s *Supervisor) handleExists(h Handle) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.entries[h]
	return ok
}

// Start begins supervising one already-prepared process. The caller has
// already resolved an AsyncProcessRunner, called PrepareProcess, and
// acquired the matching workspace lease (spec "Workspace coordination");
// Start only ever consumes what it is handed -- it never calls
// PrepareProcess itself. owner and origin are recorded verbatim as the new
// process's Identity (identity.go); prepared is the single-use preparation
// this call consumes; lease is released on every path that does not end
// with a registered entry owning it; sink, observations, ceiling, and yield
// are recorded on the entry for later microtasks to use.
//
// Start reserves every quota reserveQuota enforces before calling
// prepared.Start (TestSupervisorReservesQuotaBeforeStart), at most once,
// exactly matching PreparedProcess.Start's own single-use contract. It then
// atomically persists this process's manifest in state StateStarting --
// via s.manifests, a real durable ManifestStore -- before ever calling
// prepared.Start, so a Handle can never be returned for a process that has
// no durable record (TestSupervisorPersistsBeforeReturningHandle; spec
// "Manifests and durability": "Before returning a process handle, Tools
// atomically persists a manifest"). This requires minting the Handle (and
// therefore checking handleExists) before prepared.Start is called, not
// after, unlike a Handle-agnostic admission flow would.
//
// If prepared.Start fails, Start releases every reservation it made,
// releases lease, closes prepared -- idempotent per the Harness
// PreparedProcess contract -- and transitions the already-persisted
// StateStarting manifest directly to StateFailed (there is no live entry
// on this path, so this is a plain synchronous Save, not
// entry.terminalize's compare-and-set: Start's own call sequence is
// already the only writer of this manifest at this point, and no
// concurrent caller can race it) before returning a *Error wrapping
// CodeSpawnFailed (TestSupervisorStartFailureReleasesQuota). If a quota is
// already exhausted, Start returns a *Error wrapping
// CodeProcessQuotaExceeded without ever calling prepared.Start or
// prepared.Close, and without minting a Handle or writing any manifest
// (TestSupervisorRejectsSessionAndLoopQuota).
//
// Once prepared.Start succeeds, Start persists the manifest's StateRunning
// transition, opens this process's in-memory Buffer and durable Spool
// (both sized from the exact quota reservation -- see reservation's doc
// comment), registers the entry, and starts the entry's single
// wait/activity/drain goroutine (entry.go's run) on a lifetime-scoped
// context that is deliberately independent of ctx (see run's doc comment
// for why). Start returns the freshly minted Handle only after the entry
// is registered and its goroutine has been started
// (TestSupervisorDrainsOrderedStreams, TestSupervisorSpoolCeilingDropsOldest).
//
// Start allocates this process's stable LifecycleEventIDs
// (newLifecycleEventIDs) and records them on the very first manifest it
// persists, so they are already durably in place before the process can
// ever reach a terminal state (TestSupervisorPublishesStableLifecycleIDs;
// spec "Manifests and durability"). Start itself does not arbitrate a
// terminal state for a process that reaches one after prepared.Start
// succeeds -- that is entry.terminalize's one-shot compare-and-set
// (entry.go), driven by run's natural Wait() return today and, starting in
// Task 9C, also by an explicit stop request, a deadline timeout, or
// supervisor shutdown.
//
// If closeAdmission has already been called, Start rejects admission
// immediately with a *Error wrapping CodeSupervisorShuttingDown, before
// reserving any quota or touching prepared at all
// (TestSupervisorShutdownRejectsAdmission).
//
// A prepared.Start failure is classified by classifyStartError, below,
// rather than unconditionally reported as CodeSpawnFailed: a real
// AsyncProcessRunner adapter (e.g. the product composition root's process_adapter.go) reports a
// more specific tool.ProcessError classification for a handful of
// documented, distinct failure modes (lifetime containment unavailable, PTY
// unavailable), and that specific reason must survive through this layer
// for bash/supervised.go's own classifyProcessError to see it rather than a
// generic spawn failure that already lost it here.
func (s *Supervisor) Start(
	ctx context.Context,
	owner Owner,
	origin Origin,
	prepared tool.PreparedProcess,
	lease Lease,
	sink lifecycleSink,
	observations observationInvalidator,
	ceiling StorageCeiling,
	yield YieldSettings,
) (Handle, error) {
	s.mu.Lock()
	shuttingDown := s.shuttingDown
	s.mu.Unlock()
	if shuttingDown {
		return "", New(CodeSupervisorShuttingDown)
	}

	// sessionLifecycle/notifications are the Supervisor's own session-wide
	// services (activateServices, above), read once here under lock. sink,
	// this call's own explicit per-call parameter, wins when the caller
	// supplies one (TestSupervisorStartPublishesStartedOrBackgrounded
	// exercises exactly that override); otherwise every lifecycle publish
	// this process ever makes -- Start-time AND, via the entry it
	// registers below, its eventual terminal publish -- falls back to the
	// session-wide value a real caller (Bash, via the product composition root's composed
	// session) never supplies directly. notifications has no per-call
	// counterpart at all: it is always the session-wide value.
	sessionLifecycle, notifications := s.servicesLocked()
	effectiveSink := sink
	if effectiveSink == nil {
		effectiveSink = sessionLifecycle
	}

	if prepared == nil {
		return "", Wrap(CodeInvalidArguments, errors.New("prepared process is required"))
	}

	res, err := s.reserveQuota(owner, ceiling)
	if err != nil {
		return "", err
	}

	handle, err := NewHandle(s.handleExists)
	if err != nil {
		s.releaseQuota(res)
		s.releaseLease(lease)
		_ = prepared.Close()
		return "", err
	}

	identity := Identity{Handle: handle, Owner: owner, Origin: origin}

	// events are this process's stable lifecycle EventIDs and completion
	// CommandID (manifest.go's LifecycleEventIDs), allocated once here and
	// recorded on every manifest Save from this point on. See
	// newLifecycleEventIDs' doc comment for why allocating all five up
	// front -- rather than lazily at each one's own publish time -- is what
	// lets entry.terminalize's eventual completed/lost publish always reuse
	// these exact IDs.
	events, err := newLifecycleEventIDs()
	if err != nil {
		s.releaseQuota(res)
		s.releaseLease(lease)
		_ = prepared.Close()
		return "", Wrap(CodeProcessSetupFailed, err)
	}

	// CommandMetadata is left at its zero value: Start's signature (fixed
	// by Task 8's combined-acceptance text) carries no sanitized command
	// description for this microtask to record, and Manifest.Validate does
	// not require one. A later phase (Bash, Phase 4) is the caller that
	// actually knows the command line; wiring it through is out of this
	// task's scope. TTY is likewise left false here: it is only knowable
	// from the live tool.Process's StreamMode after prepared.Start
	// succeeds, and this manifest write must happen strictly before that
	// call.
	access := accessMode(prepared.EffectiveWorkspaceAccess().Kind)
	createdAt := time.Now().UTC()
	manifest := NewManifest(identity, CommandMetadata{}, access, false, createdAt, nil)
	manifest.Events = events
	if err := s.manifests.Save(manifest); err != nil {
		s.releaseQuota(res)
		s.releaseLease(lease)
		_ = prepared.Close()
		return "", Wrap(CodeProcessSetupFailed, err)
	}

	proc, err := prepared.Start(ctx)
	if err != nil {
		s.releaseQuota(res)
		s.releaseLease(lease)
		_ = prepared.Close()

		finishedAt := time.Now().UTC()
		manifest.State = StateFailed
		manifest.FinishedAt = &finishedAt
		manifest.Result = Result{Reason: reasonString(tool.ProcessTerminalFailed)}
		// Best-effort: Start already reports its own classification of err
		// (classifyStartError, below) regardless of whether this terminal
		// Save itself succeeds, and there is no entry/completion-notification
		// path to retry it through on this synchronous, single-writer path.
		_ = s.manifests.Save(manifest)

		return "", Wrap(classifyStartError(err), err)
	}

	startedAt := time.Now().UTC()
	manifest.State = StateRunning
	manifest.StartedAt = &startedAt
	if err := s.manifests.Save(manifest); err != nil {
		s.releaseQuota(res)
		s.releaseLease(lease)
		_ = proc.Close(ctx)
		return "", Wrap(CodeProcessSetupFailed, err)
	}

	// base is entry.base (entry.go's manifestBase): the same Start-time-only
	// manifest fields just persisted above, captured onto the entry so a
	// later terminal-transition reload failure never has to depend on
	// reloading them (see manifestBase's and doTerminalize's doc comments).
	base := manifestBase{
		command:   manifest.Command,
		access:    access,
		tty:       manifest.TTY,
		createdAt: createdAt,
		startedAt: startedAt,
		deadline:  manifest.Deadline,
		events:    events,
	}

	spool, err := OpenSpool(s.spoolRoot, handle, res.spoolBytes)
	if err != nil {
		s.releaseQuota(res)
		s.releaseLease(lease)
		_ = proc.Close(ctx)

		// Bug fix (flagged during 8C's review, fixed here in 8D): by this
		// point prepared.Start already succeeded and returned a live
		// tool.Process (now best-effort closed above), so without this
		// terminal Save the manifest this call already persisted in
		// StateRunning above would be left stuck there forever -- no
		// terminal transition, and (before this fix) wrongly protected from
		// Task 8D's terminal-LRU eviction forever, since eviction only ever
		// considers entries State.Terminal() has actually reached. There is
		// no live entry on this path (one is never registered when setup
		// fails after spawn), so this is a plain synchronous Save, not
		// entry.terminalize's compare-and-set -- exactly like the
		// PreparedProcess.Start failure branch above.
		finishedAt := time.Now().UTC()
		manifest.State = StateFailed
		manifest.FinishedAt = &finishedAt
		manifest.Result = Result{Reason: reasonString(tool.ProcessTerminalFailed)}
		// Best-effort: Start already reports CodeProcessSetupFailed
		// regardless of whether this terminal Save itself succeeds, and
		// there is no entry/completion-notification path to retry it
		// through on this synchronous, single-writer path.
		_ = s.manifests.Save(manifest)

		return "", Wrap(CodeProcessSetupFailed, err)
	}

	// Publish this process's Start-time lifecycle record: Started if the
	// caller did not request immediate backgrounding, or Backgrounded if it
	// did (YieldSettings.Yield == true) -- exactly one of the two, using the
	// manifest's own pre-persisted stable EventID for whichever kind this
	// is, never a freshly minted one (see lifecycleStartEvent's doc comment,
	// entry.go). This is synchronous, not deferred into entry.run's
	// lifetime-scoped goroutine: it is tied to the StateRunning transition
	// made above, which already happened synchronously. Best-effort and
	// nil-tolerant, like every other publish/notify call in this package: a
	// slow or failing sink must never fail Start or delay returning the
	// Handle -- the manifest itself, already durably saved above, is the
	// source of truth regardless of whether this notification succeeds.
	//
	// Deliberately placed here, after OpenSpool has already succeeded,
	// rather than immediately after the StateRunning Save above (Phase Gate
	// 2 re-review finding): publishing any earlier would let a subsequent
	// OpenSpool failure emit a Started/Backgrounded event for a process that
	// never gets a registered entry and therefore never reaches
	// entry.terminalize's terminal publish -- a nonterminal event with no
	// terminal pair. By this point every failure branch that could still
	// abort Start is behind us, so a Started/Backgrounded publish here is
	// always eventually followed by exactly one terminal publish once this
	// entry's own lifetime ends.
	startKind := tool.ProcessLifecycleStarted
	startEventID := events.Started
	if yield.Yield {
		startKind = tool.ProcessLifecycleBackgrounded
		startEventID = events.Backgrounded
	}
	if effectiveSink != nil {
		_ = effectiveSink.publishStart(ctx, lifecycleStartEvent{
			EventID:   startEventID,
			Kind:      startKind,
			Identity:  identity,
			CreatedAt: createdAt,
			StartedAt: startedAt,
		})
	}

	// lifetimeCtx is deliberately derived from context.Background(), not the
	// request-scoped ctx parameter above: per tool.PreparedProcess's
	// documented contract, "the returned Process lives until Wait, Close,
	// its deadline, or runner shutdown independently of the Start context,"
	// and Task 8's combined-acceptance text requires "invocation
	// cancellation after handoff does not cancel lifetime." A supervised
	// background process must outlive the Start call that spawned it. cancel
	// is stored as lifetimeCancel and called by entry.run's own deferred
	// cleanup once that goroutine returns (entry.go), so the two gosec G118
	// findings this used to produce here (background-derived context;
	// cancel func apparently never called) are both addressed, the second
	// one in a different file gosec's same-function check cannot see --
	// hence the explicit #nosec below on the one line gosec still flags.
	lifetimeCtx, cancel := context.WithCancel(context.Background()) // #nosec G118 -- cancel is called by entry.run's deferred cleanup (entry.go), see comment above

	e := &entry{
		identity:       identity,
		lease:          lease,
		lifecycle:      effectiveSink,
		observations:   observations,
		ceiling:        ceiling,
		yield:          yield,
		base:           base,
		process:        proc,
		reservation:    res,
		manifests:      s.manifests,
		notifications:  notifications,
		releaseQuota:   s.releaseQuota,
		onTerminal:     s.recordTerminal,
		buffer:         NewBuffer(res.memoryBytes),
		spool:          spool,
		lifetimeCancel: cancel,
		done:           make(chan struct{}),
		exited:         make(chan struct{}),
		wake:           make(chan struct{}),
	}

	s.mu.Lock()
	s.entries[handle] = e
	s.mu.Unlock()

	go e.run(lifetimeCtx) // #nosec G118 -- lifetimeCtx is deliberately independent of ctx; see the comment where it's constructed above

	return handle, nil
}

// classifyStartError classifies a prepared.Start failure into this
// package's own stable Code domain. A runner that classifies its own
// failure through Harness's typed tool.ProcessError -- e.g. the product composition root's
// process_adapter.go mapStartError, for
// tool.ProcessErrorLifetimeEnforcementUnavailable when Sandbox's own
// lifetime-containment proof is unavailable (today, every real
// Seatbelt-confined Darwin spawn), or tool.ProcessErrorPTYUnavailable when
// a requested PTY could not be honored at Start -- reports that exact, more
// specific reason; every other Start failure (untyped, or a
// tool.ProcessErrorCode this package's own domain has no direct
// counterpart for) conservatively reports CodeSpawnFailed, exactly matching
// this package's behavior before this classification existed
// (TestSupervisorStartFailureReleasesQuota).
func classifyStartError(err error) Code {
	var perr *tool.ProcessError
	if errors.As(err, &perr) {
		switch perr.Code {
		case tool.ProcessErrorLifetimeEnforcementUnavailable:
			return CodeLifetimeEnforcementUnavailable
		case tool.ProcessErrorPTYUnavailable:
			return CodePTYUnavailable
		}
	}
	return CodeSpawnFailed
}
