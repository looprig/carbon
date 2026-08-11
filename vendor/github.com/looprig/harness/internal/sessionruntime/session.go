package sessionruntime

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/internal/hustleruntime"
	"github.com/looprig/harness/internal/loopruntime"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/foreign"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/hook"
	"github.com/looprig/harness/pkg/hub"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/harness/pkg/workspacestore"
	model "github.com/looprig/inference/model"
)

type idGenerator func() (uuid.UUID, error)

const (
	defaultConstructionAbortTimeout = 5 * time.Second
	leaseReleaseTimeout             = 5 * time.Second
)

func withConstructionAbortTimeout(timeout time.Duration) Option {
	return func(s *Session) { s.constructionAbortTimeout = timeout }
}

type Session struct {
	// SessionID is shared by every loop participating in this session.
	sessionID uuid.UUID

	// hub is the session-level event fan-in. Loops publish through it (via the
	// session's PublishEvent, which delegates here); consumers subscribe via
	// SubscribeEvents. The hub also owns the federated-quiescence model that
	// WaitIdle reads. It is constructed in New before any loop, so a loop
	// never publishes into a nil hub.
	hub *hub.Hub

	// sessionCtx is the shared lifetime root for the session; every loop gets a
	// loopCtx derived from it. sessionCancel is the final backstop, cancelled by
	// the construction context (today) or future explicit teardown.
	sessionCtx               context.Context
	sessionCancel            context.CancelFunc
	constructionAbortTimeout time.Duration
	hooks                    *hook.Runner

	// loopsMu protects loops and activeLoopID. There is no session goroutine, so
	// session methods serialize registry access with a normal RWMutex.
	loopsMu  sync.RWMutex
	activeMu sync.Mutex

	// loops are the loop handles in this session, keyed by loop id. Each entry
	// pairs the loop handle with the provenance of whatever spawned it (zero for
	// a root loop). Today this map holds one entry; agent orchestration adds
	// child loops with a non-zero parent.
	loops          map[uuid.UUID]*loopHandle
	directChildren map[uuid.UUID]map[uuid.UUID]struct{}
	constructing   bool

	// activeLoopID is the mutable default target for Submit.
	activeLoopID uuid.UUID

	// closing is the fail-secure latch: once set, NewLoop refuses to create or
	// register any further loop. It is guarded by loopsMu — set by Shutdown
	// (which flips it and snapshots the loops under the same lock), read by
	// NewLoop's registration critical section. Because both the set and the
	// authoritative NewLoop check happen under loopsMu, a loop can never be
	// registered after Shutdown's snapshot has been taken.
	closing bool

	// shutdownMu makes teardown single-owner. Every concurrent or repeated caller
	// joins shutdownDone and observes the same cleanup result; its own context error
	// is added only after the shared, session-bounded teardown has completed.
	shutdownMu      sync.Mutex
	shutdownStarted bool
	shutdownDone    chan struct{}
	shutdownErr     error
	// shutdownTimeouts is a package-private test seam. Production leaves it zero
	// and derives every phase budget from the session's already-validated loop,
	// hustle, checkpoint, and durable-I/O bounds.
	shutdownTimeouts shutdownCleanupTimeouts

	// faulted is the terminal fail-secure latch: set by ReportFault when the hub
	// raises a SessionPersistenceFault, or by an internal hustle/workspace fault. Once set,
	// every new Submit/NewLoop is refused with SessionFaulted, so no further work is
	// admitted to a session whose durable log is no longer trustworthy. faultErr is
	// the fault that latched it (chained as the refusal's Cause). Both are guarded by
	// loopsMu — the same lock that gates closing and the NewLoop registration check —
	// so a fault and a NewLoop can never interleave incorrectly.
	faulted           bool
	faultErr          error
	workspaceFaulted  bool
	workspaceFaultErr error
	// workspaceWaiterFailureToken is the sticky Hub waiter-failure generation
	// owned by the recoverable required-checkpoint latch. Manual recovery may clear
	// only this token; a newer terminal fault has a different generation.
	workspaceWaiterFailureToken uint64

	// hustleDefinitions and hustleLimits are immutable construction inputs. The
	// single controller is bound before the session or any loop is reachable.
	hustleDefinitions []hustle.Definition
	hustleLimits      HustleLimits
	hustleController  *hustleruntime.Controller
	hustlesBound      bool

	// permissionClassifiers and permissionReviewPolicy are immutable
	// construction inputs set by withPermissionReview (internal/sessionruntime/
	// gates.go). A zero PermissionClassifierSet (the default: no classifiers
	// configured) means StartPermissionReview starts no review — the exact
	// pre-Task-14 gate lifecycle is unchanged for any session that does not opt
	// in. Unlike hustleDefinitions/hustleController, there is no separate
	// "bind" step: StartPermissionReview builds a permissionReviewAdapter
	// on demand from these fields plus the already-bound s.hustleController,
	// mirroring compactorFor's on-demand construction (internal/sessionruntime/
	// compaction_adapter.go) rather than hustleController's eager one, because
	// review is session-wide (one registry for every loop) rather than
	// per-loop.
	permissionClassifiers  gate.PermissionClassifierSet
	permissionReviewPolicy gate.PermissionReviewPolicy

	// permissionReviewEvidenceAccess, permissionReviewEvidenceContainment, and
	// permissionReviewEvidenceAllowedKinds are the immutable construction
	// inputs set by withPermissionReviewEvidence (internal/sessionruntime/
	// gates.go), forwarded into hustleruntime.RuntimeConfig.Evidence at
	// hustle-controller construction (hustle.go's newHustleController). They
	// install the consumer-supplied read-only evidence-tool access boundary
	// every registered permission classifier's evidence tools authorize
	// against (design §13.1). All three stay at their zero value for a
	// session that never opts in — newHustleController then fails
	// CONSTRUCTION closed (never silently permissive) for any registered
	// classifier whose definition actually needs evidence tools.
	permissionReviewEvidenceAccess       gate.EvidenceAccessEvaluator
	permissionReviewEvidenceContainment  gate.EvidenceContainmentVerifier
	permissionReviewEvidenceAllowedKinds []string

	// permissionReviewSecurityCeiling is the immutable, consumer-supplied
	// effective security posture (design §13.1/§21) set by
	// withPermissionReviewSecurityCeiling (internal/sessionruntime/gates.go)
	// and threaded into every ReviewContext loopReviewContext builds. It is a
	// consumer-owned concept — the SAME kind of value as
	// permissionReviewEvidenceContainment/permissionReviewEvidenceAllowedKinds
	// above — that Harness has no way to originate itself (this module has no
	// first-class "effective access posture" concept; the product composition
	// root binds its own AccessProfile name here). Staying empty for a session that
	// never opts
	// in makes loopReviewContext return nil (defense in depth; the LOUD
	// failure is rig.Define()-time validation pairing this with
	// WithPermissionClassifiers, mirroring WithPermissionReviewEvidence's
	// pairing) rather than silently stamping a placeholder value that can
	// never equal a real consumer's own ceiling.
	permissionReviewSecurityCeiling string

	// permissionReviewObservationVerifier is the immutable, OPTIONAL
	// consumer-supplied recheck seam (design §13.4, TOCTOU) set by
	// withPermissionReviewObservationVerifier (internal/sessionruntime/
	// gates.go). Unlike permissionReviewEvidence*/permissionReviewSecurityCeiling
	// above, staying nil is a fully supported, non-degraded configuration: a
	// session whose registered classifiers' evidence tools never record any
	// ObservationRequirement has nothing for this seam to ever recheck.
	// respondFromClassifier's verifyPermissionReviewObservations treats the
	// two "nil verifier" cases very differently, though — see its own doc
	// comment: zero recorded observations is a genuine no-op, but ONE OR MORE
	// recorded observations with this still nil is exactly the "consumer
	// wired an observation-recording evidence tool but forgot
	// rig.WithPermissionReviewObservations" misconfiguration, and fails
	// closed (treated as stale) rather than silently skipping the recheck.
	permissionReviewObservationVerifier gate.EvidenceObservationVerifier

	// review is the session's bounded, PURELY in-memory permission-review
	// cancellation-group + circuit-breaker bookkeeping (design §15, §18;
	// internal/sessionruntime/review_state.go). It is never persisted and
	// never restored: a restored session is built as a fresh struct literal
	// (restore_constructor.go's buildRestoredSession) that never sets this
	// field, so it always starts from the zero reviewLifecycle — design
	// §22.4's "stale live cancellation handles do not survive restore".
	review reviewLifecycle

	// resources is the one session-private registry shared by every process-enabled
	// loop binding in this live construction.
	resources               *sessionResources
	resourceStorageResolver SessionResourceStorageResolver

	// limits are the in-session agent-spawn safety caps NewLoop enforces (depth +
	// quota). Defaulted in newSession (withDefaults) so the live values are always
	// positive caps, and overridable via WithLimits. Read under loopsMu inside NewLoop's
	// authoritative critical section (the same lock that gates closing/faulted), so the
	// caps are evaluated atomically with the reservation. It is set once at construction
	// and never mutated, so the lock is only for read-coherence with the spawned counter.
	limits Limits

	// spawned is the running count of sub-loops this session has spawned via the
	// quota-counted NewLoop path (the zero-parent loop built by New does not count). It is
	// the quota's reservation counter: NewLoop reserves a slot (spawned++) under loopsMu
	// once the depth + quota + closing/faulted checks pass, and ROLLS BACK (spawned--)
	// under the same lock on every later failure (id-mint, loopruntime.New, the registration-time
	// closing re-check, publish failure). Restore re-seeds it by counting the durable
	// non-root LoopStarted events, so the quota survives a restart. Guarded by loopsMu.
	spawned int

	// newID mints command-Header IDs and loop ids. It defaults to uuid.New; kept
	// as a field only so tests can inject failure and prove the session never
	// sends zero-id commands and never registers a zero-id loop.
	newID idGenerator

	// now is the clock the session's event Factory mints CreatedAt from. It
	// defaults to time.Now; kept as a field so tests can pin it deterministically
	// (mirrors newID).
	now event.Clock

	// factory mints the EventID + CreatedAt stamped onto the session-scoped Enduring
	// events the session itself produces — SessionStarted (in New) and the loop-tree
	// record LoopStarted (in NewLoop). It is built in New from closures over the live
	// newID + now fields, so a test that swaps either after construction pins the
	// stamp too. The LOOP owns minting for its own loop events (loopConfig.eventFactory);
	// this Factory is only for the session's own creation sites.
	factory *event.Factory

	// cmdAppender is the intent-log seam: every command the session
	// dispatches to a loop is appended here BEFORE the send (appendCommand). It defaults
	// to the nop appender (no-persistence/headless mode); the composition root (Phase 10)
	// injects the real journal.JournalCommandAppender via WithCommandAppender. Unlike the
	// hub's required durable tap, ordinary command failures are logged-and-swallowed;
	// machine NoFold delegate intent is required and refuses dispatch on failure.
	cmdAppender commandAppender

	// allowConfigMismatch is the restore-only opt-in (set by WithAllowConfigMismatch)
	// to resume a session whose persisted config fingerprint no longer matches the live
	// config. It is read ONLY by Restore (before the session comes up); New never
	// consults it. Default false = fail-secure (a mismatch rejects the restore).
	allowConfigMismatch bool

	// restoreDecider is the restore-only application policy (set by WithRestoreDecider)
	// that answers a configuration-drift assessment. It defaults to the fail-secure
	// DefaultPolicyDecider{} (reject on any Warn) when the composition root supplies
	// none. A later task consumes it in the restore path; today it is stored only.
	restoreDecider RestoreDecider

	// fingerprint is required composition wiring supplied by rig and is the single
	// projection used by both new and restored sessions.
	fingerprint FingerprintProvider
	// frozenFingerprint is the rig-resolved compatibility identity used before any
	// restore-time workspace resolution or loop/tool binding.
	frozenFingerprint *event.ConfigFingerprint
	// frozenManifest is the rig-assembled ConfigManifest counterpart to
	// frozenFingerprint, stamped onto the construction-time SessionStarted's additive
	// Manifest field. Nil leaves Manifest at its zero value.
	frozenManifest *event.ConfigManifest

	// injectedSessionID is the externally-minted sessionID the composition root supplies
	// via WithSessionID, read ONLY by newSession to resolve the journal chicken-and-egg:
	// the journal needs the sid before session construction, so the composition root mints
	// it first, builds the journal/lease/appenders from it, then passes it in here. Zero
	// (the default, no option) means New mints its own. It is never consulted by Restore
	// (which already takes the sessionID as a positional argument).
	injectedSessionID uuid.UUID

	// injectedEventAppender is the hub's REQUIRED durable tap the composition root supplies
	// via WithEventAppender, read ONLY by newSession when it builds the hub: the journal's
	// JournalEventAppender wired as hub.WithAppender so every Enduring event is appended
	// before fan-out (fail-secure). Nil (the default, no option) leaves the hub's nop
	// appender installed — headless/no-persistence mode is unchanged. Restore builds its
	// own appender from the journal it constructs, so it does not read this.
	injectedEventAppender eventAppender

	// leaseRelease is the single-writer-lease release hook the composition root installs
	// via WithLeaseRelease (New) or that Restore installs from the lease it acquires. It is
	// called ONCE at the END of Shutdown (releaseOnce) — after the loops have drained, so
	// the journal's last append precedes the release — so a clean exit relinquishes
	// ownership and a successor can re-acquire without waiting out the TTL. Nil (headless /
	// no-persistence) is a no-op. The session owns this seam (DIP): it never holds the
	// concrete lease, only the narrow release closure.
	leaseRelease func(context.Context) error
	releaseOnce  sync.Once

	// foreignBuild and foreignBuildRestored are the composition-root seams newLoop and
	// the restore path use to construct a foreign-engine loop (live + restored). They are
	// wired via WithForeignBuilders; nil (the default) means foreign engines are not
	// supported, so a foreign-engine definition fails closed (SessionForeignBuilderMissing /
	// RestoreForeignBuilderMissing). The session depends only on these narrow function
	// seams, never on the foreignloop concrete loop (Dependency Inversion): loopruntime.New
	// itself only ever builds native, and the foreign backend is injected here.
	foreignBuild                 foreign.Builder
	foreignBuildRestored         foreign.RestoredBuilder
	foreignBuildServices         foreign.ServicesBuilder
	foreignBuildRestoredServices foreign.ServicesRestoredBuilder
	foreignRegistry              *foreign.BuilderRegistry
	foreignDeliveryMu            sync.RWMutex
	foreignDeliveryHooks         map[uuid.UUID]*foreignDeliveryHook
	foreignDeliveryWatcherOnce   sync.Once
	// collabBroker is created lazily when a services-aware foreign loop is
	// constructed. Native and legacy foreign loops never receive a broker
	// descriptor. The broker owns the endpoint; raw capabilities remain only in
	// the immutable descriptor handed to the matching builder.
	collabBrokerMu         sync.Mutex
	collabBroker           *collabBroker
	runtimeCatalog         loop.RuntimeCatalog
	hasRuntimeCatalog      bool
	runtimeCatalogProvider RuntimeCatalogProvider
	delegateSubscribe      func(event.EventFilter) (event.Subscription, error)
	delegateEnqueue        func(context.Context, loop.Backend, command.UserInput) error

	// ws is the workspace snapshot store CheckpointWorkspace archives the session's
	// working tree into, and wsRoot is the directory it archives. Both are wired
	// together by WithWorkspaceCheckpointing; nil ws (the default, no option) leaves the
	// capability unconfigured, so CheckpointWorkspace fails closed with a typed
	// *WorkspaceNotConfiguredError. The session depends only on the narrow *Store
	// (Dependency Inversion): it never sees the Blobs backend beneath it.
	ws                         *workspacestore.Store // nil unless WithWorkspaceCheckpointing wired it; gates CheckpointWorkspace
	wsRoot                     string                // the workspace directory Snapshot archives
	initialWorkspaceCheckpoint workspacestore.Ref

	// placementSpec is the UNRESOLVED managed-workspace placement carried into the restore
	// path (withPlacementSpec); restoreTopologySession resolves it after the session lease.
	// Zero (PlacementNone) for NewSession (which resolves in the Lifecycle) and no-workspace.
	placementSpec WorkspacePlacement

	// wsMode records the managed-workspace placement mode (exclusive/per-session/shared)
	// so RestoreWorkspace can select the whole-root swap (per-session) versus the
	// manifest reconcile (fixed exclusive/shared). PlacementNone when unmanaged.
	wsMode WorkspacePlacementMode

	// wsCoordinator is the ONE session-scoped workspace mutation coordinator every
	// primer/delegate loop's file + Bash tools serialize through (design §"Native
	// checkpoint boundary and workspace gate"). It is populated by the placement work
	// via WithWorkspacePlacement; nil means no managed workspace, so the three bind
	// sites leave tool.Bindings.Workspace nil and workspace-requiring tools are refused
	// (already guaranteed invalid at rig.Define). Its Healthy() reflects exclusive
	// root-lease loss, so a structured mutator fails closed after ownership is lost.
	wsCoordinator tool.WorkspaceCoordinator

	// wsRootRelease releases the EXCLUSIVE root lease. Shutdown calls it (releaseRootOnce)
	// BEFORE the session lease release (LIFO teardown): work/checkpoints stop, the root
	// lease is released, then the session lease. Nil for per-session/shared/no placement.
	wsRootRelease   func(context.Context) error
	releaseRootOnce sync.Once

	// wsLeaseLost is the exclusive root lease's loss channel. A watcher goroutine
	// (watchRootLease) faults the session and interrupts live loops when it closes, so
	// admission closes and in-flight loops/checkpoints are torn down on ownership loss.
	wsLeaseLost <-chan struct{}

	// snapshotPolicy is the validated rig policy captured before construction;
	// checkpoints is the per-session native boundary controller built after the hub.
	snapshotPolicy      *checkpointPolicy
	checkpoints         *checkpointController
	checkpointAdmission *checkpointAdmissionGate

	// gatesMu protects gates and the per-entry state transitions. The gate
	// directory is session-owned (the session is the source of truth for which
	// gates are preparing/open/claiming/closed), so all directory mutations
	// serialize behind this mutex.
	gatesMu sync.Mutex

	// gates is the authoritative gate directory: gate.ID -> gateEntry. An entry
	// exists from PrepareGateOpen (state=preparing) through ActivateGate (open)
	// and RespondGate/CloseGate (claiming -> closed/removed). ListGates returns
	// only open entries.
	gates map[gate.ID]gateEntry

	// gateAppender is the STRICT durable append seam for gate prepare/open/resolve.
	// Unlike the hub's PublishEvent (which faults and returns nil on append
	// failure), this seam returns the append error so PrepareGateOpen/ActivateGate
	// can fail closed — a failed prepare installs no directory entry, a failed
	// activate leaves the gate preparing. The nop default (nopGateAppender) keeps
	// headless/no-persistence mode unchanged; the composition root wires the real
	// journal+hub adapter via WithGateAppender.
	gateAppender gateAppender

	// gateCaps bounds the live gate directory. The cap counts preparing + open +
	// claiming so failed activations cannot accumulate invisible prepared entries.
	// Zero (the default) means no cap — the session accepts unlimited gates.
	gateCaps GateCaps

	// gateTimers holds activation-time response-policy timers keyed by GateID.
	// Protected by gatesMu alongside gates.
	gateTimers map[gate.ID]*time.Timer

	// gateAnswers holds the live delivery slot for each HOST-OWNED gate, keyed by
	// GateID. A loop-owned gate's answer becomes a command the loop consumes; a
	// host-owned gate (see hostOwnedGate) has no loop to command, so its answer is
	// handed back to the opener blocked in AwaitGateAnswer through this slot.
	//
	// Each channel is buffered with capacity one and written at most once, so
	// RespondGate never blocks on a slow or absent opener. The map is protected by
	// gatesMu alongside gates; the send happens after the durable append, outside
	// the lock. A slot is created by PrepareGateOpen and removed by
	// AwaitGateAnswer or CloseGate.
	gateAnswers map[gate.ID]chan gate.Answer

	// topology is the immutable set of loop definitions this session may instantiate,
	// keyed by agent name. The delegation manager resolves a parent's requested delegate
	// name to its child definition through it. It is set once at construction (New /
	// Restore) and never mutated, so it is read without a lock.
	topology Topology

	// delegation is the parent-to-child delegation mediator: it vends the parent-scoped
	// tool.DelegateController injected into each loop's agent collaboration bundle, and owns the
	// in-flight delegate-request map. It is created before the loops (so restore can bind
	// loop tools against it) and attached to this session once the session exists. Never
	// nil for a constructed session.
	delegation *delegationManager

	// interruptPending REFCOUNTS the loop ids currently under an interrupt admission barrier:
	// a marked loop's current turn was interrupted and its NEW machine-delegate admission
	// (agent start/send) is refused while the count is positive. The count (not a set) lets
	// overlapping interrupt scopes each hold a shared loop independently — the mark clears only
	// when the last holding barrier releases. It is guarded by loopsMu — the SAME lock that gates
	// the registry, closing, and faulted — so the mark-before-fanout select-and-mark is atomic
	// w.r.t. a concurrent NewLoop / Interrupt. Human input is still accepted into the actor
	// inbox, but its loop-scoped execution admission waits until every applicable ref clears.
	// Lazily allocated (interrupt.go), so a struct-literal test session is safe. See interrupt.go.
	interruptPending map[uuid.UUID]int
	// interruptChanged is closed and replaced whenever an interrupt barrier refcount
	// changes. Loop-scoped execution admission waits on it while its target remains
	// pending, preserving accepted user input in the actor inbox without starting a turn.
	interruptChanged chan struct{}

	// interruptRelease is the pluggable admission-barrier release policy (Dependency Inversion
	// seam): after an interrupt fan-out cancels a running turn, the session holds the
	// interrupt-pending marks until this policy's AwaitRelease returns, then clears them. Nil
	// (the default) resolves to sessionIdleRelease — release once the session next reaches idle
	// (SessionIdle durably appended). Workspace-backed sessions bypass this seam and await their
	// checkpoint controller's generation-specific accepted/committed/faulted outcome. See interrupt.go.
	interruptRelease InterruptReleasePolicy

	// offloadGCPolicy is the restore-path carrier for the offload-GC cadence (set by
	// withOffloadGCPolicy, read by restoreTopologySession so it can build the runner from
	// the lease it acquires). NewSession builds the runner in the Lifecycle instead. Zero
	// (unconfigured) means no offload GC.
	offloadGCPolicy OffloadGCPolicy

	// offloadGC is the session's offload-blob GC runner (nil unless WithOffloadGC armed it).
	// It is installed by the composition root via withOffloadGCRunner, STARTED after the hub
	// exists (startOffloadGC, bound to hub.IsIdle), and STOPPED+joined at the very start of
	// Shutdown's teardown — before SessionStopped is appended and before the lease is
	// released. Its own admission gate serializes every durable append against a GC pass.
	offloadGC *offloadGCRunner
}

// constructionCleanupOwnedError marks a construction failure after a Session has
// accepted the lease hooks and become their sole synchronous/background cleanup owner.
// It unwraps transparently so public typed error matching is unchanged.
type constructionCleanupOwnedError struct{ cause error }

func (e *constructionCleanupOwnedError) Error() string { return e.cause.Error() }
func (e *constructionCleanupOwnedError) Unwrap() error { return e.cause }

func constructionCleanupOwned(err error) bool {
	var owned *constructionCleanupOwnedError
	return errors.As(err, &owned)
}

// abortConstruction unwinds collaborators created by a session that will never become
// reachable. It deliberately emits no normal SessionStopped event. Once lease hooks have
// been accepted, the Session owns root-then-session release synchronously or through one
// background cleanup owner after every collaborator drains.
func (s *Session) abortConstruction(cause error) {
	s.abortConstructionAfter(cause, nil)
}

// abortConstructionAfter seals hub admission first, cancels every collaborator, and
// gives a restore failure one terminal direct-journal append after all already-admitted
// hub publishes drain. One cleanup owner preserves that ordering beyond the caller's
// deadline and retains both leases until checkpoints and loops also drain.
func (s *Session) abortConstructionAfter(cause error, appendTerminal func(context.Context)) {
	if cause == nil {
		cause = context.Canceled
	}
	abortTimeout := s.constructionAbortTimeout
	if abortTimeout <= 0 {
		abortTimeout = defaultConstructionAbortTimeout
	}
	abortCtx, cancelAbort := context.WithTimeout(context.Background(), abortTimeout)
	defer cancelAbort()
	// Stop the offload-GC runner (nil/no-op unless it was installed and started). On a
	// construction abort it is typically unstarted, so this only stops the ticker.
	s.stopOffloadGC()
	// Seal durable publication before cancellation can make a backend emit a late
	// terminal. Already-admitted publishes are the first cleanup phase.
	var hubDrain <-chan struct{}
	if s.hub != nil {
		hubDrain = s.hub.AbortSession(cause)
	}
	if s.checkpointAdmission != nil {
		s.checkpointAdmission.latch(cause)
	}
	s.gatesMu.Lock()
	for id, timer := range s.gateTimers {
		timer.Stop()
		delete(s.gateTimers, id)
	}
	for id := range s.gates {
		delete(s.gates, id)
	}
	s.gatesMu.Unlock()
	var checkpointDrain <-chan struct{}
	if s.checkpoints != nil {
		checkpointDrain = s.checkpoints.beginShutdown()
	}
	if s.sessionCancel != nil {
		s.sessionCancel()
	}
	s.loopsMu.RLock()
	loops := make([]*loopHandle, 0, len(s.loops))
	for _, handle := range s.loops {
		loops = append(loops, handle)
	}
	s.loopsMu.RUnlock()
	for _, handle := range loops {
		if handle.cancel != nil {
			handle.cancel()
		}
	}
	// A construction failure must not leave a foreign adapter holding a live
	// capability or a broker goroutine after the session's context is canceled.
	_ = s.closeCollabBroker(context.Background())
	loopDrains := make([]<-chan struct{}, 0, len(loops))
	for _, handle := range loops {
		if handle.backend != nil && handle.backend.DoneChan() != nil {
			loopDrains = append(loopDrains, handle.backend.DoneChan())
		}
	}
	needsOwner := appendTerminal != nil || s.resources != nil || s.wsRootRelease != nil || s.leaseRelease != nil
	if !needsOwner {
		waitConstructionPhases(abortCtx, hubDrain, checkpointDrain, loopDrains)
		return
	}
	cleanupDone := make(chan struct{})
	cleanup := func() {
		defer close(cleanupDone)
		if hubDrain != nil {
			<-hubDrain
		}
		if appendTerminal != nil {
			terminalCtx, cancelTerminal := context.WithTimeout(context.Background(), abortTimeout)
			appendTerminal(terminalCtx)
			cancelTerminal()
		}
		if checkpointDrain != nil {
			<-checkpointDrain
		}
		waitConstructionDrains(context.Background(), loopDrains)
		if s.resources != nil {
			_ = s.resources.Shutdown(context.Background())
		}
		// Lease hooks are accepted by the Session before construction starts. Once
		// accepted, this is their sole teardown owner: root then session, exactly once.
		s.releaseRootLease(context.Background())
		s.releaseLease(context.Background())
	}
	go cleanup()
	select {
	case <-cleanupDone:
	case <-abortCtx.Done():
		// The one owner continues the exact phase order and retains both leases.
	}
}

func waitConstructionPhases(ctx context.Context, hubDrain, checkpointDrain <-chan struct{}, loopDrains []<-chan struct{}) bool {
	if hubDrain != nil && !waitConstructionDrains(ctx, []<-chan struct{}{hubDrain}) {
		return false
	}
	if checkpointDrain != nil && !waitConstructionDrains(ctx, []<-chan struct{}{checkpointDrain}) {
		return false
	}
	return waitConstructionDrains(ctx, loopDrains)
}

func waitConstructionDrains(ctx context.Context, drains []<-chan struct{}) bool {
	for _, drained := range drains {
		select {
		case <-drained:
		case <-ctx.Done():
			return false
		}
	}
	return true
}

// eventAppender is the session's narrow view of the hub's REQUIRED durable event tap:
// append one Enduring event to the durable journal, returning the assigned durable
// sequence and a typed error if it did not commit. The session holds it only to FORWARD
// it into the hub at construction (hub.WithAppender); the session never calls AppendEvent
// itself (the hub owns the durable tap). It mirrors the hub's own unexported eventAppender
// method-set, so the concrete journal.JournalEventAppender satisfies both structurally and
// the session never imports the journal's appender type. Defined here (where it is
// consumed) per Dependency Inversion, exactly like commandAppender.
type eventAppender interface {
	AppendEvent(ctx context.Context, ev event.Event) (uint64, error)
}

// loopHandle is the session's registry entry: the loop's channel handle, the
// provenance of the turn/step that spawned it (zero for a root loop), and
// the cancel for this loop's loopCtx (a session-owned backstop).
type loopHandle struct {
	id    uuid.UUID
	owner *Session
	bound loop.BoundDefinition
	// bindings is the EXACT tool.Bindings this loop was bound with, retained so
	// ReplaceExternalTools builds external tools with the same capabilities (and the same
	// WorkspaceObservations instance) the declared tools received.
	//
	// It is populated for EVERY engine, foreign included: both construction paths build the
	// full binding set before dispatching on Engine. It is therefore NOT a signal for
	// whether a loop can host harness tools — use bound.Engine() for that. (A foreign loop
	// is refused by ReplaceExternalTools on its engine, not on this field.)
	bindings   tool.Bindings
	backend    loop.Backend
	tombstoned bool
	parent     loop.Provenance
	cancel     context.CancelFunc

	// liveMu guards the live view (liveMode/liveModel) — the CURRENT selection
	// Handle.Mode()/Model() report. The loop actor is the authoritative owner of the
	// effective config; SetMode/Change update this view ONLY from the actor's committed
	// reply, so the read never runs ahead of the durable/applied change. Reads take the
	// read lock. It is seeded at construction with the loop's starting selection.
	liveMu    sync.RWMutex
	liveMode  loop.ModeName
	liveModel model.Model
	stateMu   sync.RWMutex
	state     tool.DelegateStatusValue
	agentName string
	agentMode loop.ModeName
	// selectedHarness is the immutable public harness selected for this loop's
	// durable runtime identity. Harness-managed RuntimeIdentity intentionally omits
	// it, so ListAgents cannot reconstruct it from a shared profile.
	selectedHarness loop.AgentHarnessName
}

// runtimeForModel is the SOLE builder of the durable event.LoopStarted.Runtime — the
// session-level counterpart to loopruntime's modelRuntime (loop.go), which builds the
// same shape for LoopModeChanged/LoopInferenceChanged. Dropping a field here silently
// breaks restore in production: a field missing from the durable LoopStarted record can
// never be folded back on restore no matter how correct the restore-fold read side is —
// exactly the bug fixed by e7da984a (APIFormat/BaseURL were grafted at the restore-fold
// sites before this builder ever populated them on write). See the sibling restore-fold
// sites this function's output ultimately feeds: internal/loopruntime/restored.go's
// NewRestoredWithRuntime and loop_change.go's applyModelRuntime.
func runtimeForModel(model model.Model) event.ModelRuntime {
	return event.ModelRuntime{
		Key:       model.Key(),
		Limits:    model.Limits,
		Effort:    model.Sampling.Effort,
		APIFormat: model.APIFormat,
		BaseURL:   model.BaseURL,
	}
}

func (h *loopHandle) ID() uuid.UUID { return h.id }
func (h *loopHandle) Mode() loop.ModeName {
	h.liveMu.RLock()
	defer h.liveMu.RUnlock()
	return h.liveMode
}
func (h *loopHandle) Model() model.Model {
	h.liveMu.RLock()
	defer h.liveMu.RUnlock()
	return h.liveModel
}

func (h *loopHandle) Modes() []loop.ModeName {
	boundModes := h.bound.Modes()
	modes := make([]loop.ModeName, len(boundModes))
	for i := range boundModes {
		modes[i] = boundModes[i].Name
	}
	return modes
}

// setLiveView records the mode/model the loop actor committed, so Handle.Mode()/Model()
// reflect the current selection after a successful change.
func (h *loopHandle) setLiveView(mode loop.ModeName, model model.Model) {
	h.liveMu.Lock()
	h.liveMode = mode
	h.liveModel = model
	h.liveMu.Unlock()
}

func (h *loopHandle) setMechanicalState(status tool.DelegateStatusValue) {
	h.stateMu.Lock()
	h.state = status
	h.stateMu.Unlock()
}

func (h *loopHandle) mechanicalState() tool.DelegateStatusValue {
	h.stateMu.RLock()
	defer h.stateMu.RUnlock()
	if h.state == tool.DelegateStatusUnknown {
		return tool.DelegateStatusIdle
	}
	return h.state
}

// Interrupt is the loop.Controller interrupt: it cancels this loop's current turn AND every
// loop below it in the delegate subtree (design: "loop.Controller.Interrupt marks one loop and
// its delegate subtree"), marking the whole subtree interrupt-pending before fan-out. It is the
// trusted controller mutation surface; the subtree selection + barrier live in interrupt.go.
func (h *loopHandle) Interrupt(ctx context.Context) error { return h.owner.interruptSubtree(ctx, h.id) }

type preparedLoop struct {
	id       uuid.UUID
	bound    loop.BoundDefinition
	bindings tool.Bindings
}

type loopEventPublisher interface {
	PublishEvent(context.Context, event.Event) error
	PublishEventChecked(context.Context, event.Event) error
}

// eventSubscriber is the consumer-facing half of the session fan-in: a TUI/CLI (or
// later a durable journal) attaches here to receive filtered events. It is defined
// where it is consumed (the session), per Dependency Inversion. *Session
// satisfies it by delegating to the hub.
type eventSubscriber interface {
	SubscribeEvents(event.EventFilter) (event.Subscription, error)
}

// Compile-time proof that *Session is the consumer-facing eventSubscriber.
// Its publisher half (PublishEvent) is asserted by loopruntime.New accepting s as its
// eventPublisher at the NewLoop call site.
var _ eventSubscriber = (*Session)(nil)

// Compile-time proof that *Session is the hub's FaultReporter: on a required durable
// append failure the hub calls ReportFault, and the session fails secure.
var _ hub.FaultReporter = (*Session)(nil)

// ReportFault is the session's fail-secure response to a hub persistence fault (a
// required durable append failed): it latches the faulted state (so every new
// Submit/NewLoop is refused) and wakes every blocked WaitIdle waiter with the fault.
// It is the hub's FaultReporter — the hub calls it inline (outside the hub lock) when
// AppendEvent fails the durable tap. It is idempotent: the FIRST fault latches and
// records the cause; a later fault still wakes any new waiters but keeps the first
// recorded cause (the root failure). The session is NOT torn down here — restore /
// operator action owns recovery; this only stops admitting new work and unblocks
// callers stuck waiting on a session that can no longer reach idle durably.
func (s *Session) ReportFault(_ context.Context, fault *hub.SessionPersistenceFault) {
	s.latchSessionFault(fault)
}

func (s *Session) latchSessionFault(fault error) {
	s.loopsMu.Lock()
	if !s.faulted {
		s.faulted = true
		s.faultErr = fault
	}
	s.loopsMu.Unlock()

	// Wake blocked WaitIdle waiters with the fault (outside loopsMu — FailWaiters
	// takes the hub lock; loopsMu and the hub lock are never held together).
	s.hub.FailWaiters(fault)
}

// faultIfFaulted returns a typed SessionFaulted error (chaining the latched fault) if
// the session has faulted, else nil. It is the fail-secure gate every new-work entry
// point (Submit/submitToLoop/NewLoop) checks before admitting work. Read under
// loopsMu — the same lock ReportFault latches under.
func (s *Session) faultIfFaulted() error {
	s.loopsMu.RLock()
	defer s.loopsMu.RUnlock()
	if s.faulted {
		return &SessionError{Kind: SessionFaulted, Cause: s.faultErr}
	}
	if s.workspaceFaulted {
		return &SessionError{Kind: SessionFaulted, Cause: s.workspaceFaultErr}
	}
	return nil
}

func (s *Session) latchWorkspaceCheckpointFault(err error) {
	s.loopsMu.Lock()
	s.workspaceFaulted = true
	s.workspaceFaultErr = err
	s.loopsMu.Unlock()
	if s.checkpointAdmission != nil {
		s.checkpointAdmission.latch(err)
	}
	token := s.hub.FailWaiters(err)
	s.loopsMu.Lock()
	s.workspaceWaiterFailureToken = token
	s.loopsMu.Unlock()
}

func (s *Session) recoverWorkspaceCheckpointFault() {
	// The manual checkpoint still holds the writer admission here. Clear the gate
	// latch first (readers remain blocked by the writer), then clear the public
	// workspace-fault rejection, and only afterward does the controller release writer.
	if s.checkpointAdmission != nil {
		s.checkpointAdmission.recover()
	}
	s.loopsMu.Lock()
	token := s.workspaceWaiterFailureToken
	s.workspaceFaulted = false
	s.workspaceFaultErr = nil
	s.workspaceWaiterFailureToken = 0
	s.loopsMu.Unlock()
	s.hub.ClearWaiterFailure(token)
}

// observeBestEffortCheckpointError reports an automatic checkpoint failure without
// latching the session. Best-effort policy explicitly lets execution continue and
// retries on the next eligible boundary; the failure must still remain observable.
func (s *Session) observeBestEffortCheckpointError(err error) {
	slog.ErrorContext(s.sessionCtx, "session: best-effort workspace checkpoint failed", "error", err)
}

// seedDirectChildren builds the ownership index once loops have been restored.
func (s *Session) seedDirectChildren() {
	s.loopsMu.Lock()
	defer s.loopsMu.Unlock()
	if s.directChildren == nil {
		s.directChildren = make(map[uuid.UUID]map[uuid.UUID]struct{})
	}
	for id, handle := range s.loops {
		if handle == nil || handle.parent.LoopID.IsZero() {
			continue
		}
		s.addDirectChildUnderLock(handle.parent.LoopID, id)
	}
}

func (s *Session) addDirectChildUnderLock(parentID, childID uuid.UUID) {
	if s.directChildren == nil {
		s.directChildren = make(map[uuid.UUID]map[uuid.UUID]struct{})
	}
	children := s.directChildren[parentID]
	if children == nil {
		children = make(map[uuid.UUID]struct{})
		s.directChildren[parentID] = children
	}
	children[childID] = struct{}{}
}

func (s *Session) removeDirectChildUnderLock(parentID, childID uuid.UUID) {
	children := s.directChildren[parentID]
	delete(children, childID)
	if len(children) == 0 {
		delete(s.directChildren, parentID)
	}
}

// FaultErr is the loop actor's post-emit durable-fault probe (loopruntime type-asserts the
// session for it). It returns the latched persistence fault (a required Enduring append
// failed) or nil. After emitting a mode/inference change event, the actor calls it: because
// the hub raises the fault INLINE (synchronously on the same actor goroutine, via
// ReportFault) when the append fails, a non-nil result here means the change event did not
// persist, so the actor declines to apply the change (fail-secure, no partial apply).
func (s *Session) FaultErr() error {
	return s.faultIfFaulted()
}

// AdmissionFaultErr is the loop actor's last-moment admission probe, closing the
// Session submit check→send race without changing the narrower change-event FaultErr seam.
func (s *Session) AdmissionFaultErr() error { return s.faultIfFaulted() }

// PublishEvent is the session's eventPublisher implementation passed to loopruntime.New.
// It delegates to the hub, which fans the event out to matching subscribers and
// applies any quiescence transition the event implies. The loop depends only on
// the narrow eventPublisher interface; it never sees the hub, its subscriber set,
// or its shutdown state (Interface Segregation / least privilege).
func (s *Session) PublishEvent(ctx context.Context, ev event.Event) error {
	if err := s.hub.PublishEvent(ctx, ev); err != nil {
		return err
	}
	s.recordLoopMechanicalState(ev)
	s.clearReviewTurnState(ev)
	return nil
}

// PublishEventChecked is the transactional publication path for state transitions
// whose caller must not mutate live state unless the required append commits. Delegate
// acceptance, public gate open/resolve transitions, and native checkpoint boundaries
// use it to receive append failures directly while retaining durable-first fan-out.
func (s *Session) PublishEventChecked(ctx context.Context, ev event.Event) error {
	if err := s.hub.PublishEventChecked(ctx, ev); err != nil {
		return err
	}
	s.recordForeignDeliveryFold(ev)
	s.recordLoopMechanicalState(ev)
	s.clearReviewTurnState(ev)
	return nil
}

func (s *Session) foreignServicesForTrackedWithController(loopID uuid.UUID, controller tool.DelegateController) (foreign.Services, *foreignDeliveryHook) {
	hook := s.foreignDeliveryHookFor(loopID)
	if hook == nil {
		hook = newForeignDeliveryHook(s, loopID)
	}
	if s == nil || controller == nil {
		return foreign.NewServices(foreign.BrokerDescriptor{}, hook), hook
	}
	hook.brokerMu.RLock()
	if hook.brokerSet {
		descriptor := hook.broker
		hook.brokerMu.RUnlock()
		return foreign.NewServices(descriptor, hook), hook
	}
	hook.brokerMu.RUnlock()
	s.collabBrokerMu.Lock()
	broker := s.collabBroker
	s.collabBrokerMu.Unlock()
	if broker == nil {
		// Services-aware foreign construction is the opt-in boundary for the
		// collaboration capability. Start exactly one broker for the session
		// before invoking the builder; legacy builders and native loops never
		// enter this path and therefore never receive a descriptor.
		var err error
		broker, err = s.ensureCollabBroker()
		if err != nil {
			hook.setBrokerError(err)
			return foreign.NewServices(foreign.BrokerDescriptor{}, hook), hook
		}
	}
	if controller == nil {
		return foreign.NewServices(foreign.BrokerDescriptor{}, hook), hook
	}
	descriptor, err := broker.Mint(loopID, controller)
	if err != nil {
		hook.setBrokerError(err)
		return foreign.NewServices(foreign.BrokerDescriptor{}, hook), hook
	}
	hook.setBrokerDescriptor(descriptor)
	return foreign.NewServices(descriptor, hook), hook
}

// collabBrokerRequired reports whether a bound loop will use the additive
// services-aware foreign construction seam. Native loops and the legacy
// Builder/RestoredBuilder seams deliberately do not start a broker or receive
// a broker descriptor. Services-aware adapter profiles routed through a
// registry use the services lookup path and therefore opt in as well.
func (s *Session) collabBrokerRequired(bound loop.BoundDefinition) bool {
	if s == nil || bound == nil || bound.Engine() == loop.EngineNative {
		return false
	}
	if s.foreignBuildServices != nil {
		return true
	}
	return s.foreignRegistry != nil && bound.Engine() == loop.EngineAdapter && bound.RuntimeProfile() != "" &&
		s.foreignRegistry.HasServicesBuilder(bound.RuntimeProfile())
}

func (s *Session) foreignDeliveryHookFor(loopID uuid.UUID) *foreignDeliveryHook {
	if s == nil {
		return nil
	}
	s.foreignDeliveryMu.RLock()
	hook := s.foreignDeliveryHooks[loopID]
	s.foreignDeliveryMu.RUnlock()
	return hook
}

// CommitBoundary is the native loop-actor boundary seam. Without a configured
// workspace policy it preserves ordinary publication; with one it delegates the
// already-stamped StepDone/turn terminal to the session checkpoint controller.
func (s *Session) CommitBoundary(ctx context.Context, ev event.Event) error {
	if s.checkpoints == nil {
		return s.PublishEventChecked(ctx, ev)
	}
	return s.checkpoints.boundary(ctx, ev)
}

// CommitContextBoundary is the context-mutating boundary seam. The committed
// result distinguishes an event append failure from a later checkpoint failure,
// so the loop actor can keep live history aligned with durable restore state.
func (s *Session) CommitContextBoundary(ctx context.Context, ev event.Event) (bool, error) {
	if s.checkpoints == nil {
		err := s.PublishEventChecked(ctx, ev)
		return err == nil, err
	}
	return s.checkpoints.boundaryResult(ctx, ev)
}

// CommitSessionIdle is the hub's narrow derived-idle collaborator. The hub retains
// append/fanout ownership in commit; the controller brackets it with the workspace
// permit and accepted checkpoint walk when idle is the configured trigger.
func (s *Session) CommitSessionIdle(ctx context.Context, idle event.SessionIdle, commit func() error) error {
	if s.checkpoints == nil {
		return commit()
	}
	return s.checkpoints.sessionIdle(ctx, idle, commit)
}

// SessionActivated lets the hub cancel an active best-effort quiescent walk before
// newly active work proceeds. Required and shared-fuzzy policies ignore activation.
func (s *Session) SessionActivated() {
	if s.checkpoints != nil {
		s.checkpoints.activated()
	}
}

type sessionTurnStartCapability struct {
	session          *Session
	reservation      *hub.TurnStartReservation
	releaseExecution func()
	releaseOnce      sync.Once
}

func (c *sessionTurnStartCapability) PublishTurnStarted(ctx context.Context, started event.TurnStarted) (bool, error) {
	committed, err := c.reservation.PublishTurnStartedChecked(ctx, started)
	if committed {
		c.session.recordLoopMechanicalState(started)
	}
	return committed, err
}

func (c *sessionTurnStartCapability) Release() {
	if c == nil {
		return
	}
	c.releaseOnce.Do(func() {
		c.releaseExecution()
		c.reservation.Release()
	})
}

// EnterExecution is the inference-step session-wide checkpoint and loop-scoped
// interrupt admission seam.
func (s *Session) EnterExecution(ctx context.Context, loopID uuid.UUID) (func(), error) {
	release, _, err := s.enterExecution(ctx, loopID, false)
	return release, err
}

// EnterTurnStart reserves the Hub activity transition before acquiring the first
// checkpoint reader. Its returned capability publishes the exact opening event and
// releases the checkpoint reader when the first inference step ends.
func (s *Session) EnterTurnStart(ctx context.Context, loopID uuid.UUID) (loopruntime.TurnStartCapability, error) {
	release, reservation, err := s.enterExecution(ctx, loopID, true)
	if err != nil {
		return nil, err
	}
	return &sessionTurnStartCapability{session: s, reservation: reservation, releaseExecution: release}, nil
}

func (s *Session) enterExecution(ctx context.Context, loopID uuid.UUID, reserve bool) (func(), *hub.TurnStartReservation, error) {
	for {
		for {
			s.loopsMu.Lock()
			if s.interruptPending[loopID] == 0 {
				s.loopsMu.Unlock()
				break
			}
			if s.interruptChanged == nil {
				s.interruptChanged = make(chan struct{})
			}
			changed := s.interruptChanged
			s.loopsMu.Unlock()
			select {
			case <-changed:
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			case <-s.sessionCtx.Done():
				return nil, nil, s.sessionCtx.Err()
			}
		}
		var reservation *hub.TurnStartReservation
		if reserve {
			var err error
			reservation, err = s.hub.ReserveTurnStart(loopID)
			if err != nil {
				return nil, nil, err
			}
		}
		if s.checkpointAdmission == nil {
			return func() {}, reservation, nil
		}
		release, err := s.checkpointAdmission.enterExecution(ctx)
		if err != nil {
			if reservation != nil {
				reservation.Release()
			}
			return nil, nil, err
		}
		// Close the interrupt mark/checkpoint-acquire race: a sweep may mark
		// this loop while it waited for the checkpoint reader permit. In that
		// case release and retry instead of returning an admission that can start.
		s.loopsMu.RLock()
		pending := s.interruptPending[loopID] > 0
		s.loopsMu.RUnlock()
		if !pending {
			return release, reservation, nil
		}
		release()
		if reservation != nil {
			reservation.Release()
		}
	}
}

func (s *Session) recordLoopMechanicalState(ev event.Event) {
	loopID := ev.EventHeader().Coordinates.LoopID
	if loopID.IsZero() {
		return
	}
	if started, ok := ev.(event.TurnStarted); ok {
		s.delegation.markRequestActive(started.Cause.CommandID, loopID)
	}
	var status tool.DelegateStatusValue
	switch ev.(type) {
	case event.TurnStarted:
		status = tool.DelegateStatusRunning
	case event.TurnDone, event.LoopIdle:
		status = tool.DelegateStatusIdle
	case event.TurnFailed:
		status = tool.DelegateStatusFailed
	case event.TurnInterrupted:
		status = tool.DelegateStatusInterrupted
	default:
		return
	}
	s.loopsMu.RLock()
	h := s.loops[loopID]
	s.loopsMu.RUnlock()
	if h != nil {
		h.setMechanicalState(status)
	}
}

// SubscribeEvents attaches a consumer to the session fan-in with the given filter.
// The returned subscription's Events() channel yields the filtered stream; the
// caller must Close it when done. It delegates to the hub.
func (s *Session) SubscribeEvents(filter event.EventFilter) (event.Subscription, error) {
	return s.hub.SubscribeEvents(filter)
}

func (s *Session) SessionID() uuid.UUID { return s.sessionID }

func (s *Session) projectFingerprint(definition loop.BoundDefinition) event.ConfigFingerprint {
	if s.frozenFingerprint != nil {
		return *s.frozenFingerprint
	}
	return s.fingerprint(definition)
}

// projectManifest returns the rig-assembled manifest baseline stamped onto
// SessionStarted. The zero ConfigManifest stands in when no manifest was supplied
// (the deprecation-window default), keeping the Manifest field additive.
func (s *Session) projectManifest() event.ConfigManifest {
	if s.frozenManifest != nil {
		return *s.frozenManifest
	}
	return event.ConfigManifest{}
}

func (s *Session) ActiveLoop() loop.Handle {
	s.loopsMu.RLock()
	defer s.loopsMu.RUnlock()
	return s.loops[s.activeLoopID]
}

func (s *Session) Loop(id uuid.UUID) (loop.Handle, bool) {
	s.loopsMu.RLock()
	defer s.loopsMu.RUnlock()
	h, ok := s.loops[id]
	return h, ok
}

func (s *Session) LoopController(id uuid.UUID) (loop.Controller, bool) {
	s.loopsMu.RLock()
	defer s.loopsMu.RUnlock()
	h, ok := s.loops[id]
	return h, ok
}

func (s *Session) SetActiveLoop(ctx context.Context, id uuid.UUID) error {
	s.activeMu.Lock()
	defer s.activeMu.Unlock()
	if err := s.faultIfFaulted(); err != nil {
		return err
	}
	s.loopsMu.RLock()
	target, ok := s.loops[id]
	previous := s.activeLoopID
	closing := s.closing
	s.loopsMu.RUnlock()
	if !ok {
		return &SessionError{Kind: SessionLoopNotFound}
	}
	if closing {
		return &SessionError{Kind: SessionClosing}
	}
	if target.backend == nil {
		return &SessionError{Kind: SessionLoopExited}
	}
	select {
	case <-target.backend.DoneChan():
		return &SessionError{Kind: SessionLoopExited}
	default:
	}
	if id == previous {
		return nil
	}
	header, err := s.factory.Stamp(event.Header{Coordinates: identity.Coordinates{SessionID: s.sessionID}})
	if err != nil {
		return &SessionError{Kind: SessionIDGenerationFailed, Cause: err}
	}
	changed := event.ActiveLoopChanged{Header: header, PreviousLoopID: previous, ActiveLoopID: id}
	if err := s.PublishEvent(ctx, changed); err != nil {
		return err
	}
	s.loopsMu.Lock()
	if s.faulted {
		var persistenceFault *hub.SessionPersistenceFault
		if errors.As(s.faultErr, &persistenceFault) && persistenceFault.Event.EventHeader().EventID == header.EventID {
			cause := s.faultErr
			s.loopsMu.Unlock()
			return &SessionError{Kind: SessionFaulted, Cause: cause}
		}
	}
	s.activeLoopID = id
	s.loopsMu.Unlock()
	return nil
}

// ActiveLoopID returns the session's mutable active loop id, the default target for
// Submit. It is safe to call concurrently.
func (s *Session) ActiveLoopID() uuid.UUID {
	s.loopsMu.RLock()
	defer s.loopsMu.RUnlock()
	return s.activeLoopID
}

// WaitIdle blocks until the session is quiescent, ctx is done, or the session has
// stopped (hub.ErrSessionStopped). It is the headless caller's "is the whole
// interaction at rest?" primitive; it delegates to the hub's quiescence model.
func (s *Session) WaitIdle(ctx context.Context) error {
	return s.hub.WaitIdle(ctx)
}

// expectTurn takes a hand-back wake token for an agent loop at spawn so its
// in-flight result cannot empty the quiescence set and fire a false idle. It is
// session-internal — loops never call it (they hold only the narrow eventPublisher);
// only the session's agent orchestration does.
//
// TODO(Open Items A): async agent spawn must call expectTurn(agentLoopID)
// before the child can complete its first turn, so the {wake} token guards the
// quiescence set across the hand-back. That async-spawn orchestration is deferred;
// when it lands, NewLoop's async-spawn path is where this call wires in. Today no loop
// spawns an async agent, so this method has no production caller yet — it is
// exercised by the round-trip and the session+hub quiescence tests.
func (s *Session) expectTurn(ctx context.Context, agentLoopID uuid.UUID) {
	s.hub.ExpectTurn(ctx, agentLoopID)
}

// cancelExpectTurn releases an agent's wake token off the publish path. It is
// session-internal (loops never call it) and is NO LONGER on the SubagentResult
// hand-back path: a SubagentResult is never rejected, so its {wake} token always
// releases on the publish path via the resulting TurnStarted/TurnFoldedInto/
// InputCancelled carrying Cause.LoopID. cancelExpectTurn remains for the future
// async-spawn DISCARD path (a child spawned but abandoned before it ever hands back,
// so no event ever carries its Cause.LoopID). Today it has no production caller;
// it is exercised by the session+hub quiescence tests.
func (s *Session) cancelExpectTurn(ctx context.Context, agentLoopID uuid.UUID) {
	s.hub.CancelExpectTurn(ctx, agentLoopID)
}

// deliverSubagentResult is the session-owned SubagentResult hand-back: it routes a
// finished child agent's output (blocks) to its parent loop as a command.SubagentResult
// and returns only a transport error (the loop is gone, or ctx is done). It is
// FIRE-AND-FORGET: a SubagentResult is NEVER rejected, so there is no outcome to wait
// for off the publish path. The parent loop always starts (idle) or queues
// (running/shutting-down) the hand-back, and its quiescence {wake, fromLoopID} token
// is ALWAYS released on the publish path by the resulting Enduring event — a
// TurnStarted/TurnFoldedInto carrying Cause.LoopID == fromLoopID, or an
// InputCancelled (also carrying it) if the loop ends before the hand-back commits (the
// shutdown terminal's returnQueuedInbox, or an idle-time id-gen failure to start). The
// session no longer reads a disposition and no longer releases the token off the
// publish path.
//
// parentLoopID selects the parent loop's command channel — it rides as the command's
// embedded Coordinates.LoopID (the delivery target). fromLoopID is the producing
// child agent; it rides as Header.Cause.LoopID and is stamped onto the events
// the hand-back causes. The submit carries no per-turn stream — the parent's events flow
// to the session fan-in. ctx governs the send only (the loop derives the turn ctx from
// its own loopCtx).
func (s *Session) deliverSubagentResult(ctx context.Context, parentLoopID, fromLoopID uuid.UUID, blocks []content.Block) error {
	l, ok := s.loopFor(parentLoopID)
	if !ok {
		return &SessionError{Kind: SessionLoopNotFound}
	}
	id, err := s.newCommandID()
	if err != nil {
		return err
	}
	cmd := command.SubagentResult{
		Coordinates: identity.Coordinates{LoopID: parentLoopID}, // delivery target (PARENT)
		Header: command.Header{
			CommandID: id,
			Cause:     identity.Cause{Coordinates: identity.Coordinates{LoopID: fromLoopID}}, // CHILD (wake token)
			CreatedAt: s.stampNow(),
		},
		Blocks: blocks,
	} // Agency left default AgencyMachine — a hand-back is machine-originated
	// Intent log (audit-only): append BEFORE dispatch; an append failure is logged and
	// the hand-back proceeds. Targets the PARENT loop (the command's delivery target).
	s.appendCommand(ctx, parentLoopID, cmd)
	select {
	case commandSinkFor(l, cmd) <- cmd:
		return nil
	case <-l.DoneChan():
		return &SessionError{Kind: SessionLoopExited}
	case <-ctx.Done():
		return &SessionError{Kind: SessionContextDone, Cause: ctx.Err()}
	}
}

// replaySubagentResult re-admits an already durable hand-back command without
// appending a second intent record. Restore uses this when the prior parent actor
// accepted the command but no durable turn/fold event proves that it processed the
// completion. The restored actor has no ephemeral inbox to rebuild, so the original
// command crosses the actor boundary again with its original id.
func (s *Session) replaySubagentResult(ctx context.Context, cmd command.SubagentResult) error {
	l, ok := s.loopFor(cmd.Coordinates.LoopID)
	if !ok {
		return &SessionError{Kind: SessionLoopNotFound}
	}
	if l == nil {
		return &SessionError{Kind: SessionLoopExited}
	}
	select {
	case commandSinkFor(l, cmd) <- cmd:
		return nil
	case <-l.DoneChan():
		return &SessionError{Kind: SessionLoopExited}
	case <-ctx.Done():
		return &SessionError{Kind: SessionContextDone, Cause: ctx.Err()}
	}
}

// NewLoop creates another loop inside this session. The new loop shares
// SessionID but receives its own loop id and loop goroutine. parent is the
// provenance of the spawning turn/step (zero for a root loop); the session
// records it in the registry and passes it to loopruntime.New. The session stores the
// loop handle and returns only the loop id, because callers route through
// session methods rather than writing to a loop command channel directly.
func (s *Session) NewLoop(parent loop.Provenance, cfg loop.Definition) (uuid.UUID, error) {
	// A plain NewLoop is never spawned by an agent collaboration tool call, so it carries no
	// parent tool-use id; the private newLoop does the real work. RunAgent is the
	// only path that threads a non-empty id (its child's LoopStarted correlates back
	// to the spawning tool call).
	return s.newLoop(parent, cfg, "", "", nil)
}

// newLoop is the private loop-creation core behind NewLoop and RunAgent. It is
// identical to the public NewLoop except that parentToolUseID is stamped onto the new
// loop's LoopStarted (event.LoopStarted.ParentToolUseID): the durable carrier that
// correlates a tool-spawned child loop back to its parent agent tool call. NewLoop
// passes ""; RunAgent passes the provider tool-use id of the spawning call. The id
// rides as a plain parameter into the LoopStarted build only — it touches no identity /
// Provenance / Header struct, so it never perturbs the loop tree or the quota/depth math.
func (s *Session) newLoop(parent loop.Provenance, cfg loop.Definition, parentToolUseID string, selectedMode loop.ModeName, prepared *preparedLoop) (uuid.UUID, error) {
	return s.newLoopWithAdmission(parent, cfg, parentToolUseID, selectedMode, prepared, nil, nil, loop.RuntimeCatalog{}, false)
}

func (s *Session) newLoopWithAdmission(parent loop.Provenance, cfg loop.Definition, parentToolUseID string, selectedMode loop.ModeName, prepared *preparedLoop, admission *delegateAdmission, runtime *loop.Resolved, runtimeCatalog loop.RuntimeCatalog, hasRuntimeCatalog bool) (uuid.UUID, error) {
	// Whether this spawn counts toward the cumulative spawn quota. The initial root loop is
	// built by newSession via NewLoop with zero provenance and must not consume a quota slot;
	// every agent spawn
	// carries a non-zero parent.LoopID. This is the SAME root/non-root discriminator the
	// durable recount uses on restore (a non-root LoopStarted has a non-zero Header.Cause),
	// so the live counter and the restored counter count exactly the same set.
	counts := !parent.LoopID.IsZero()

	// One authoritative critical section folds the closing/faulted gate, the PURE depth
	// check, and the quota RESERVATION under a single loopsMu.Lock — so the cap decisions
	// and the spawned++ reservation are atomic with respect to a concurrent NewLoop,
	// Shutdown (sets closing), or ReportFault (sets faulted). On success it reserves a
	// quota slot (spawned++) and unlocks; that reservation is ROLLED BACK by release()
	// (below) on every later failure. Defaults are resolved here (withDefaults) so the
	// caps are always positive even on a struct-literal Session that bypassed
	// newSession/restore (test seams) — a zero-limits session behaves with the production
	// defaults, never rejecting every spawn. Depth is final once it passes: it is a
	// function of the FIXED parent chain (already-registered ancestors never change), so
	// there is no later depth race to re-check.
	s.loopsMu.Lock()
	if s.faulted {
		fe := s.faultErr
		s.loopsMu.Unlock()
		return uuid.UUID{}, &SessionError{Kind: SessionFaulted, Cause: fe}
	}
	if s.workspaceFaulted {
		fe := s.workspaceFaultErr
		s.loopsMu.Unlock()
		return uuid.UUID{}, &SessionError{Kind: SessionFaulted, Cause: fe}
	}
	if s.closing {
		s.loopsMu.Unlock()
		return uuid.UUID{}, &SessionError{Kind: SessionClosing}
	}
	limits := s.limits.withDefaults()
	if s.depthUnderLock(parent.LoopID) >= limits.Depth {
		s.loopsMu.Unlock()
		return uuid.UUID{}, &SessionError{Kind: SessionLoopDepthExceeded}
	}
	if counts && s.spawned >= limits.Quota {
		s.loopsMu.Unlock()
		return uuid.UUID{}, &SessionError{Kind: SessionLoopQuotaExceeded}
	}
	if counts {
		s.spawned++ // reserve the quota slot; release() rolls it back on any later failure
	}
	s.loopsMu.Unlock()

	// release rolls back the quota reservation made above. It is called on EVERY failure
	// path after the reservation (id mint, loopruntime.New, the registration-time closing/faulted
	// re-check, and publish failure) ALONGSIDE the loop's cancel(), so a spawn that does
	// not complete never permanently consumes a slot. It is a no-op when this spawn did not
	// reserve (a zero-parent spawn). A SUCCESSFUL spawn never calls it, so
	// the reservation stands permanently (loops are retained — the cumulative budget never
	// decrements on idle, only on a rolled-back spawn).
	release := func() {
		if !counts {
			return
		}
		s.loopsMu.Lock()
		s.spawned--
		s.loopsMu.Unlock()
	}

	var loopID uuid.UUID
	var bound loop.BoundDefinition
	var bindings tool.Bindings
	var err error
	if prepared != nil {
		loopID, bound, bindings = prepared.id, prepared.bound, prepared.bindings
	} else {
		loopID, err = s.newID()
		if err != nil {
			release()
			return uuid.UUID{}, &SessionError{Kind: SessionLoopIDGenerationFailed, Cause: err}
		}
		processBinding, bindingErr := processBindingFor(cfg, s.resources)
		if bindingErr != nil {
			release()
			return uuid.UUID{}, bindingErr
		}
		bindings = tool.Bindings{SessionID: s.sessionID, LoopID: loopID, Workspace: s.newWorkspaceBinding(), Delegate: s.delegation.controllerFor(loopID, cfg), Process: processBinding, ExtraTools: delegateExtraTools(cfg, s.delegation)}
		bound, err = cfg.Bind(s.sessionCtx, bindings)
		if err != nil {
			release()
			return uuid.UUID{}, err
		}
		if runtime != nil {
			if runtime.SelectionKind == loop.RuntimeSelectionHarnessManaged {
				bound, err = loop.OverrideBoundRuntimeManaged(bound, runtime.Profile)
			} else {
				runtimeAlias := runtime.TargetAlias
				if runtimeAlias == "" {
					// Resolved values created before concrete target aliases were
					// introduced remain valid and use the model-facing alias as the
					// compatible durable identity.
					runtimeAlias = runtime.ModelAlias
				}
				bound, err = loop.OverrideBoundRuntimeSelectionWithIdentity(bound, runtime.Profile, runtimeAlias, runtime.Target, runtime.Effort, runtime.Source, runtime.SelectionKind)
			}
			if err == nil && hasRuntimeCatalog {
				bound, err = loop.OverrideBoundRuntimeCatalog(bound, runtimeCatalog)
			}
			if err != nil {
				release()
				return uuid.UUID{}, err
			}
		}
	}
	// A spawned child agent keeps its OWN definition's access gate — never the
	// parent's. Authority differences between loops are expressed by the
	// consumer configuring different gates per definition (or OverrideBoundAccess
	// at a binding seam); harness performs no cross-loop attenuation.
	var eventTarget loopEventPublisher = s
	admissionSubscriptionOwned := false
	if admission != nil {
		admission.sub, err = s.subscribeLoop(loopID)
		if err != nil {
			release()
			return uuid.UUID{}, err
		}
		admissionSubscriptionOwned = true
		defer func() {
			if admissionSubscriptionOwned {
				_ = admission.sub.Close()
			}
		}()
		admission.requestID, err = s.newCommandID()
		if err != nil {
			release()
			return uuid.UUID{}, err
		}
		admission.command = command.UserInput{
			Header:             command.Header{CommandID: admission.requestID, Agency: identity.AgencyMachine, CreatedAt: s.stampNow()},
			Blocks:             delegateBlocks(admission.message),
			NoFold:             true,
			TargetLoopID:       loopID,
			BackgroundHandBack: admission.background,
		}
		admission.publisher = newDelegateAdmissionPublisher(s)
		eventTarget = admission.publisher
	}
	// Resolve the effective starting mode: a non-empty selectedMode (the delegation
	// mode-selective spawn) starts the loop directly in that predeclared mode; an empty
	// selection uses the definition's initial mode. It is recorded on LoopStarted so
	// replay/restore reconstruct the child deterministically, without a synthetic
	// LoopModeChanged. A selected mode unknown to the bound definition fails closed below
	// (loopruntime.NewInMode returns a typed BindError).
	startedMode := selectedMode
	if startedMode == "" {
		startedMode = bound.InitialMode()
	}

	// Stamp the LoopStarted header (minting its EventID + CreatedAt via the Factory)
	// BEFORE building or registering the loop, so a crypto/rand failure fails NewLoop
	// cleanly (typed error) before any loop exists — we never leave a registered loop
	// behind a returned error. This is the 2nd mint of NewLoop (the loop id was the
	// 1st), so an id-gen failure here is SessionIDGenerationFailed (the loop id was
	// already minted). Coordinates/Cause are the loop-tree record the Factory
	// preserves; only EventID + CreatedAt are added.
	startedHeader, err := s.factory.Stamp(event.Header{
		Coordinates: identity.Coordinates{SessionID: s.sessionID, LoopID: loopID},
		// AgentName is the loop's immutable attribution name, stamped from its definition so
		// the durable LoopStarted records which agent drove this loop. Empty for a plain
		// loop; the zero-parent root carries its configured name through this same path.
		AgentName: bound.Name(),
		Cause: identity.Cause{
			Coordinates: identity.Coordinates{LoopID: parent.LoopID, TurnID: parent.TurnID, StepID: parent.StepID},
			Agency:      identity.AgencyMachine,
		},
	})
	if err != nil {
		release()
		return uuid.UUID{}, &SessionError{Kind: SessionIDGenerationFailed, Cause: err}
	}

	// Engine switch — the single loop-construction chokepoint for both the zero-parent root
	// (built via newSession → NewLoop) and every descendant. A
	// definition bound to EngineNative builds through loopruntime.New. A foreign engine
	// routes to the injected foreign Builder seam and fails CLOSED if none is
	// wired (a foreign engine must never silently resolve to a native loop). The minted
	// foreign sid the builder returns is stamped onto the published LoopStarted below;
	// it is "" for native (omitzero drops it). Every failure path rolls back the quota
	// reservation (release()) and cancels the loopCtx, exactly like the surrounding code.
	loopCtx, cancel := context.WithCancel(s.sessionCtx)
	var constructionHook *foreignDeliveryHook
	loopCommitted := false
	defer func() {
		if !loopCommitted && constructionHook != nil {
			s.revokeCollabLoop(constructionHook.loopID)
			s.unregisterForeignDeliveryHook(constructionHook)
		}
	}()
	servicesFor := func() foreign.Services {
		services, hook := s.foreignServicesForTrackedWithController(loopID, bindings.Delegate)
		constructionHook = hook
		return services
	}
	var b loop.Backend
	var foreignSID string
	var agentSessionHeader event.Header
	switch bound.Engine() {
	case loop.EngineNative:
		var compactor loopruntime.Compactor
		compactor, err = s.compactorFor(bound, loopID)
		if err == nil {
			b, err = loopruntime.NewInModeWithRuntime(
				loopCtx,
				s.sessionID,
				loopID,
				parent,
				eventTarget,
				bound,
				startedMode,
				loopruntime.RuntimeDependencies{Compactor: compactor, Hooks: s.hooks, ReviewContext: s.loopReviewContext()},
			)
		}
	case loop.EngineAdapter:
		selectedBound, selectErr := loop.SelectBoundMode(bound, startedMode)
		if selectErr != nil {
			release()
			cancel()
			return uuid.UUID{}, selectErr
		}
		bound = selectedBound
		if s.foreignRegistry != nil {
			if s.foreignRegistry.HasServicesBuilder(bound.RuntimeProfile()) {
				builder, _, lookupErr := s.foreignRegistry.ServicesBuilder(bound.RuntimeProfile())
				if lookupErr != nil {
					release()
					cancel()
					return uuid.UUID{}, &SessionError{Kind: SessionForeignBuilderMissing, Cause: lookupErr}
				}
				b, foreignSID, err = builder(loopCtx, s.sessionID, loopID, parent, eventTarget, selectedBound,
					func() (uuid.UUID, error) { return s.newID() }, s.factory, servicesFor())
				if err == nil && constructionHook != nil {
					err = constructionHook.brokerError()
				}
			} else {
				builder, _, lookupErr := s.foreignRegistry.Builder(bound.RuntimeProfile())
				if lookupErr != nil {
					release()
					cancel()
					return uuid.UUID{}, &SessionError{Kind: SessionForeignBuilderMissing, Cause: lookupErr}
				}
				b, foreignSID, err = builder(loopCtx, s.sessionID, loopID, parent, eventTarget, selectedBound,
					func() (uuid.UUID, error) { return s.newID() }, s.factory)
			}
		} else if s.foreignBuildServices != nil {
			b, foreignSID, err = s.foreignBuildServices(loopCtx, s.sessionID, loopID, parent, eventTarget, selectedBound,
				func() (uuid.UUID, error) { return s.newID() }, s.factory, servicesFor())
			if err == nil && constructionHook != nil {
				err = constructionHook.brokerError()
			}
		} else if s.foreignBuild != nil {
			// The legacy function-pair seam remains a valid composition path for
			// adapter definitions. A profile-aware dispatcher supplied through
			// WithForeignBuilders still performs its own bounded lookup; the
			// session must not reject it merely because no registry option exists.
			b, foreignSID, err = s.foreignBuild(loopCtx, s.sessionID, loopID, parent, eventTarget, selectedBound,
				func() (uuid.UUID, error) { return s.newID() }, s.factory)
		} else {
			release()
			cancel()
			return uuid.UUID{}, &SessionError{Kind: SessionForeignBuilderMissing}
		}
	default:
		if s.foreignBuildServices == nil && s.foreignBuild == nil {
			release()
			cancel()
			return uuid.UUID{}, &SessionError{Kind: SessionForeignBuilderMissing}
		}
		selectedBound, selectErr := loop.SelectBoundMode(bound, startedMode)
		if selectErr != nil {
			release()
			cancel()
			return uuid.UUID{}, selectErr
		}
		bound = selectedBound
		if s.foreignBuildServices != nil {
			b, foreignSID, err = s.foreignBuildServices(loopCtx, s.sessionID, loopID, parent, eventTarget, selectedBound,
				func() (uuid.UUID, error) { return s.newID() }, s.factory, servicesFor())
			if err == nil && constructionHook != nil {
				err = constructionHook.brokerError()
			}
		} else {
			b, foreignSID, err = s.foreignBuild(loopCtx, s.sessionID, loopID, parent, eventTarget, selectedBound,
				func() (uuid.UUID, error) { return s.newID() }, s.factory)
		}
	}
	if err != nil {
		release()
		cancel()
		return uuid.UUID{}, err
	}
	if foreignSID != "" && runtime != nil && runtime.Profile != "" {
		agentSessionHeader, err = s.factory.Stamp(event.Header{
			Coordinates: identity.Coordinates{SessionID: s.sessionID, LoopID: loopID},
		})
		if err != nil {
			release()
			cancel()
			return uuid.UUID{}, &SessionError{Kind: SessionIDGenerationFailed, Cause: err}
		}
	}
	if admission != nil {
		if err := s.appendDelegateCommand(admission.ctx, loopID, admission.command); err != nil {
			release()
			cancel()
			return uuid.UUID{}, err
		}
		if err := s.enqueuePreparedDelegate(admission.ctx, b, admission.command); err != nil {
			release()
			cancel()
			return uuid.UUID{}, err
		}
	}

	s.loopsMu.Lock()
	// Authoritative fail-secure check: re-test faulted AND closing under the SAME
	// lock acquire that registers the loop. Shutdown sets closing and ReportFault
	// sets faulted under this lock, so checking-then-registering atomically here
	// guarantees a loop is never registered after a fault or after Shutdown's
	// snapshot — it either makes it into the snapshot (registered before the latch)
	// or is refused here. On refusal: register nothing, release the lock, roll back the
	// quota reservation (release()), tear down the already-built loop with cancel(), and
	// return before the publish so no LoopStarted is ever emitted. Faulted is checked first
	// (the stronger condition). release() takes loopsMu, so it is called AFTER Unlock (the
	// mutex is not reentrant).
	if s.faulted {
		fe := s.faultErr
		s.loopsMu.Unlock()
		release()
		cancel()
		return uuid.UUID{}, &SessionError{Kind: SessionFaulted, Cause: fe}
	}
	if s.workspaceFaulted {
		fe := s.workspaceFaultErr
		s.loopsMu.Unlock()
		release()
		cancel()
		return uuid.UUID{}, &SessionError{Kind: SessionFaulted, Cause: fe}
	}
	if s.closing {
		s.loopsMu.Unlock()
		release()
		cancel()
		return uuid.UUID{}, &SessionError{Kind: SessionClosing}
	}
	liveModel := bound.Model()
	if selected, ok := bound.Mode(startedMode); ok {
		liveModel = selected.Model
	}
	initialState := tool.DelegateStatusIdle
	displayName := bound.DisplayName()
	if admission != nil {
		// The initial command has already been accepted behind the publication barrier;
		// the child is mechanically running even before TurnStarted is released.
		initialState = tool.DelegateStatusRunning
		displayName = admission.name
		if displayName == "" {
			displayName = string(bound.Name()) + "-" + loopID.String()
		}
	}
	var selectedHarness loop.AgentHarnessName
	if runtime != nil {
		selectedHarness = runtime.AgentHarness
	}
	s.loops[loopID] = &loopHandle{id: loopID, owner: s, bound: bound, bindings: bindings, backend: b, parent: parent, cancel: cancel, liveMode: startedMode, liveModel: liveModel, state: initialState, agentName: displayName, agentMode: startedMode, selectedHarness: selectedHarness}
	if !parent.LoopID.IsZero() {
		s.addDirectChildUnderLock(parent.LoopID, loopID)
	}
	s.loopsMu.Unlock()

	// Announce the new loop to subscribers active at creation time. Published AFTER
	// releasing loopsMu — never under the registry lock — because a hub publish fans
	// out and must not hold the registry lock. LoopStarted is a pure announcement: it
	// is not one of the active-mutating events (TurnStarted/LoopIdle/TurnFoldedInto/
	// InputCancelled), so it never perturbs session quiescence. Header.Coordinates is
	// the NEW loop (SessionID+LoopID; Turn/Step zero); Header.Cause is the spawning
	// loop/turn/step (zero for a root), machine-originated. There is no
	// ctx param, so it publishes on the session lifetime (s.sessionCtx). The header
	// (Coordinates/Cause + minted EventID/CreatedAt) was stamped above before the loop
	// was built.
	var agentRuntime *event.AgentRuntime
	if runtime != nil && runtime.Profile != "" {
		runtimeAlias := runtime.TargetAlias
		if runtimeAlias == "" {
			runtimeAlias = runtime.ModelAlias
		}
		smallModelAlias := runtime.SmallModel
		if runtime.SelectionKind == loop.RuntimeSelectionHarnessManaged {
			runtimeAlias = ""
			smallModelAlias = ""
		}
		agentRuntime = &event.AgentRuntime{
			Harness:         string(runtime.AgentHarness),
			Profile:         string(runtime.Profile),
			CredentialMode:  string(runtime.Credential),
			Source:          string(runtime.Source),
			SelectionKind:   string(runtime.SelectionKind),
			ModelAlias:      string(runtimeAlias),
			SmallModelAlias: string(smallModelAlias),
		}
	}
	startedRuntime := runtimeForModel(liveModel)
	if runtime != nil && runtime.SelectionKind == loop.RuntimeSelectionHarnessManaged {
		startedRuntime = event.ModelRuntime{}
	}
	ev := event.LoopStarted{Header: startedHeader, Runtime: startedRuntime, AgentRuntime: agentRuntime, ParentToolUseID: parentToolUseID, ForeignSID: foreignSID, InitialMode: string(startedMode), DisplayName: displayName, Description: bound.Description()}
	if admission != nil {
		ev.InitialRequestID = admission.requestID
	}
	publish := s.PublishEventChecked
	if err := publish(s.sessionCtx, ev); err != nil {
		// Mirror New's cleanup-on-publish-failure: the loop is already registered and
		// its loopCtx cancel is live, so a bare return would leak a cancel-orphaned
		// loop. Preserve it in the registry while initial construction is active so the
		// one bounded abort joins it; dynamic failures unregister it immediately.
		s.loopsMu.Lock()
		if !s.constructing {
			delete(s.loops, loopID)
			s.removeDirectChildUnderLock(parent.LoopID, loopID)
		}
		s.loopsMu.Unlock()
		release()
		cancel()
		return uuid.UUID{}, &SessionError{Kind: SessionContextDone, Cause: err}
	}
	if foreignSID != "" && runtime != nil && runtime.Profile != "" {
		if err := publish(s.sessionCtx, event.LoopAgentSessionBound{Header: agentSessionHeader, ACPSessionID: foreignSID}); err != nil {
			s.loopsMu.Lock()
			if !s.constructing {
				delete(s.loops, loopID)
				s.removeDirectChildUnderLock(parent.LoopID, loopID)
			}
			s.loopsMu.Unlock()
			release()
			cancel()
			return uuid.UUID{}, &SessionError{Kind: SessionContextDone, Cause: err}
		}
	}
	if admission != nil {
		if admission.background {
			// The publisher barrier still prevents the child from emitting TurnStarted,
			// so acquiring the wake token here is early enough to cover even an
			// immediately-terminal first turn.
			s.expectTurn(admission.ctx, loopID)
		}
		admission.tracked = admission.registerRequest(admission.requestID, loopID)
		admission.publisher.release()
		admissionSubscriptionOwned = false
	}
	loopCommitted = true
	return loopID, nil
}

func (s *Session) enqueuePreparedDelegate(ctx context.Context, backend loop.Backend, cmd command.UserInput) error {
	if s.delegateEnqueue != nil {
		return s.delegateEnqueue(ctx, backend, cmd)
	}
	select {
	case backend.CommandSink() <- cmd:
		return nil
	case <-ctx.Done():
		return &SessionError{Kind: SessionContextDone, Cause: ctx.Err()}
	case <-backend.DoneChan():
		return &SessionError{Kind: SessionLoopExited}
	}
}

// depthUnderLock returns the ancestor-chain length a NEW loop spawned under parentLoopID
// would have: it walks parentLoopID up the registry's parent links (loopHandle.parent.LoopID)
// and counts each registered ancestor. A zero parentLoopID (the root built by New) has no
// ancestors and returns 0; a child of that root returns 1, its child
// 2, and so on. NewLoop rejects when this count >= limits.Depth, so the deepest spawnable
// loop has an ancestor chain of limits.Depth-1.
//
// It is a PURE read over the FIXED parent chain (ancestors never change), so the caller's
// depth decision needs no later re-check. The caller MUST hold loopsMu (the registry read
// must be coherent); the cycle guard (capping the walk at len(s.loops)) is defensive — the
// registry is a tree (each loop has at most one parent, set once at creation), so a cycle
// is impossible, but an unbounded walk must never be possible from a corrupt registry.
func (s *Session) depthUnderLock(parentLoopID uuid.UUID) int {
	depth := 0
	cur := parentLoopID
	max := len(s.loops)
	for !cur.IsZero() && depth <= max {
		h, ok := s.loops[cur]
		if !ok {
			break
		}
		depth++
		cur = h.parent.LoopID
	}
	return depth
}

// loopFor returns the loop's channel handle for command routing. The registry
// stores *loopHandle; this derefs to the handle's loop. The parent provenance is
// read only by future tree walks, which read s.loops directly.
func (s *Session) loopFor(loopID uuid.UUID) (loop.Backend, bool) {
	s.loopsMu.RLock()
	defer s.loopsMu.RUnlock()
	h, ok := s.loops[loopID]
	if !ok {
		return nil, false
	}
	return h.backend, true
}

// newCommandID mints a fresh correlation ID for a command Header. Any
// crypto/rand failure is mapped onto the session's typed error path rather than
// swallowed, so callers never send an unidentifiable (zero-ID) command.
func (s *Session) newCommandID() (uuid.UUID, error) {
	id, err := s.newID()
	if err != nil {
		return uuid.UUID{}, &SessionError{Kind: SessionIDGenerationFailed, Cause: err}
	}
	return id, nil
}

// New constructs a Session and starts its zero-parent root loop's actor
// goroutine. It owns the session fan-in hub and emits the session-scoped
// SessionStarted through it.
//
// This SessionStarted (the s.hub.PublishEvent below) is the SOLE SessionStarted:
// the session publishes it through the HUB to its SUBSCRIBERS (TUI/CLI fan-in),
// and the loop never emits one. It is published before any subscriber attaches,
// so a subscriber that connects later does not observe it; reliable delivery of
// the session start to late subscribers is a separate future follow-on.
func New(ctx context.Context, cfg loop.Definition, opts ...Option) (*Session, error) {
	// Production seams: crypto/rand id-gen + the wall clock. newSession is the
	// unexported core that lets a same-package test inject a failing newID (or a
	// pinned now) that is IN EFFECT during the construction-time SessionStarted
	// stamp — the only way to exercise New's mint-error failure branch — mirroring
	// how the loop injects idGen/now into its private runtime state before construction. opts are the optional
	// dependency injections (e.g. WithCommandAppender) the composition root supplies.
	return newSession(ctx, cfg, uuid.New, time.Now, opts...)
}

// NewTopology constructs every configured primer as an independent root loop.
func NewTopology(ctx context.Context, topology Topology, opts ...Option) (*Session, error) {
	return newSessionTopology(ctx, topology, uuid.New, time.Now, opts...)
}

// newSession is the construction core of New with the id-gen and clock seams made
// explicit. New calls it with the production defaults (uuid.New, time.Now); a
// same-package test calls it with a failing newID to drive the SessionStarted
// mint-error branch (no zero-EventID SessionStarted is ever published; New returns
// nil + a typed *SessionError). newID also mints the session id itself, so a
// generator that fails on its FIRST call aborts before any event is stamped.
func newSession(ctx context.Context, cfg loop.Definition, newID idGenerator, now event.Clock, opts ...Option) (*Session, error) {
	return newSessionTopology(ctx, Topology{Definitions: []loop.Definition{cfg}, Primers: []identity.AgentName{cfg.Name()}, ActivePrimer: cfg.Name()}, newID, now, opts...)
}

func newSessionTopology(ctx context.Context, topology Topology, newID idGenerator, now event.Clock, opts ...Option) (*Session, error) {
	select {
	case <-ctx.Done():
		return nil, &SessionError{Kind: SessionContextDone, Cause: ctx.Err()}
	default:
	}

	// Resolve the composition-root options on a probe BEFORE minting the session id, so
	// WithSessionID can be consulted to adopt an externally-minted id (the journal
	// chicken-and-egg) instead of minting one here. The same opts are re-applied to the
	// real session below so the other injections (WithCommandAppender/WithEventAppender)
	// take effect on it. A probe (not the real session) is used because the id and the
	// hub must be built from the resolved values, before the struct is finalized.
	probe := &Session{}
	for _, opt := range opts {
		opt(probe)
	}

	// SessionID: adopt the externally-minted id when WithSessionID supplied a non-zero
	// one (so the composition root's journal and this session share the same id); else
	// mint one. The id-gen path is preserved for the no-injection case so a crypto/rand
	// failure still fails New closed.
	id := probe.injectedSessionID
	if id.IsZero() {
		minted, err := newID()
		if err != nil {
			return nil, &SessionError{Kind: SessionIDGenerationFailed, Cause: err}
		}
		id = minted
	}

	sessionCtx, sessionCancel := context.WithCancel(ctx)
	s := &Session{
		sessionID:                id,
		sessionCtx:               sessionCtx,
		sessionCancel:            sessionCancel,
		constructionAbortTimeout: defaultConstructionAbortTimeout,
		loops:                    make(map[uuid.UUID]*loopHandle),
		directChildren:           make(map[uuid.UUID]map[uuid.UUID]struct{}),
		constructing:             true,
		newID:                    newID,
		now:                      now,
		// Audit-only intent-log appender: nop by default (no-persistence/headless mode).
		// The composition root (Phase 10) overrides it via WithCommandAppender below.
		cmdAppender: nopCommandAppender{},
		// Gate directory: nop appender + empty directory by default; the composition
		// root wires the real journal+hub adapter via WithGateAppender.
		gates:               map[gate.ID]gateEntry{},
		gateTimers:          map[gate.ID]*time.Timer{},
		gateAnswers:         map[gate.ID]chan gate.Answer{},
		gateAppender:        nopGateAppender{},
		checkpointAdmission: newCheckpointAdmissionGate(),
	}
	// Apply optional dependency injections (e.g. WithCommandAppender, WithEventAppender)
	// before any command can be dispatched or the hub is built, so an injected appender is
	// in effect from the first dispatch/publish. A nil appender option is ignored (the nop
	// default stays installed). WithSessionID is a no-op here (already consumed above).
	for _, opt := range opts {
		opt(s)
	}
	abort := func(err error) (*Session, error) {
		s.abortConstruction(err)
		return nil, &constructionCleanupOwnedError{cause: err}
	}
	if s.fingerprint == nil && s.frozenFingerprint == nil {
		return abort(&MissingFingerprintProviderError{})
	}
	// Default the restore-drift decider to the fail-secure policy when the composition
	// root injected none via WithRestoreDecider, so the stored decider is never nil.
	if s.restoreDecider == nil {
		s.restoreDecider = DefaultPolicyDecider{}
	}
	// Apply the spawn-cap defaults AFTER the options so an unset (or WithLimits-supplied)
	// Limits resolves to positive caps before the first NewLoop — a zero or negative
	// configured value never silently disables the depth/quota backstop.
	s.limits = s.limits.withDefaults()
	// Store the immutable topology and stand up the delegation manager BEFORE any loop is
	// bound, so each loop's agent collaboration tools are built against a parent-scoped controller. The
	// manager is attached to this session so its scoped controllers can spawn + address
	// children through it.
	s.topology = cloneTopology(topology)
	if s.runtimeCatalogProvider != nil {
		s.delegation = newDelegationManagerWithCatalogProvider(s.topology, s.runtimeCatalogProvider)
	} else if s.hasRuntimeCatalog {
		s.delegation = newDelegationManager(s.topology, s.runtimeCatalog)
	} else {
		s.delegation = newDelegationManager(s.topology)
	}
	s.delegation.attach(s)
	// The Factory mints from closures over the LIVE newID + now fields, so a test
	// that swaps either after construction pins the stamp too (the same seam the
	// session's command-id minting uses).
	s.factory = event.NewFactory(func() (uuid.UUID, error) { return s.newID() }, func() time.Time { return s.now() })

	// The hub is built AFTER s so the session can wire itself as the hub's
	// FaultReporter (fail-secure: on a required-durable-append failure the hub calls
	// s.ReportFault). The hub shares the session's Factory so a hub-synthesized
	// session event (SessionActive/Idle/Stopped) is stamped from the same pinned
	// newID/now seam as the session's own events. The durable event tap is the injected
	// appender (WithEventAppender) when the composition root supplied one — wrapping the
	// SessionJournal so every Enduring event is appended before fan-out — else the hub's
	// nop default (headless/no-persistence). A nil appender is passed through to
	// hub.WithAppender, which ignores it (the nop default stays), so the no-injection
	// path is unchanged.
	hubOpts := []hub.Option{
		hub.WithFactory(s.factory),
		hub.WithFaultReporter(s),
		hub.WithCommitObserver(s.recordLoopMechanicalState),
	}
	if s.injectedEventAppender != nil {
		hubOpts = append(hubOpts, hub.WithAppender(s.injectedEventAppender))
	}
	s.hub = hub.New(id, hubOpts...)
	// Attach the checked process-service delegates now that the hub exists, so any
	// process resource this session activates below (via the prepared loops' Bind,
	// which may bind a ProcessBinding over s.resources) observes the checked publish
	// path from its very first call — never the bridge's pre-attachment stub. See
	// activateProcessServiceBridge's doc for the ordering contract this preserves.
	if err := s.activateProcessServiceBridge(); err != nil {
		return abort(err)
	}
	if err := s.bindSessionHustles(); err != nil {
		return abort(err)
	}
	if s.snapshotPolicy != nil && s.ws != nil && s.wsCoordinator != nil {
		s.checkpoints = newCheckpointController(checkpointControllerConfig{
			SessionID: id, Policy: *s.snapshotPolicy, Store: s.ws, Root: s.wsRoot,
			Mode: s.wsMode, Coordinator: s.wsCoordinator, Publisher: s, Factory: s.factory,
			Idle:  s.hub.IsIdle,
			Fault: s.latchWorkspaceCheckpointFault, Recover: s.recoverWorkspaceCheckpointFault,
			Faulted:      s.faultIfFaulted,
			Admission:    s.checkpointAdmission.enterCheckpoint,
			ObserveError: s.observeBestEffortCheckpointError,
		})
	}

	// SessionStarted is an Enduring, session-scoped event: stamp it with a minted
	// EventID + CreatedAt so the journal sees a stable idempotency key and creation
	// time. A crypto/rand failure aborts construction (fail-secure) rather than
	// publishing a zero-EventID SessionStarted.
	startedHeader, err := s.factory.Stamp(event.Header{Coordinates: identity.Coordinates{SessionID: id}})
	if err != nil {
		return abort(&SessionError{Kind: SessionIDGenerationFailed, Cause: err})
	}
	type preparedPrimer struct {
		name identity.AgentName
		def  loop.Definition
		loop preparedLoop
	}
	prepared := make([]preparedPrimer, 0, len(topology.Primers))
	orderedPrimers := make([]identity.AgentName, 0, len(topology.Primers))
	orderedPrimers = append(orderedPrimers, topology.ActivePrimer)
	for _, name := range topology.Primers {
		if name != topology.ActivePrimer {
			orderedPrimers = append(orderedPrimers, name)
		}
	}
	for _, name := range orderedPrimers {
		definition, ok := topology.definition(name)
		if !ok {
			return abort(&SessionError{Kind: SessionLoopNotFound})
		}
		loopID, mintErr := s.newID()
		if mintErr != nil {
			return abort(&SessionError{Kind: SessionLoopIDGenerationFailed, Cause: mintErr})
		}
		processBinding, bindingErr := processBindingFor(definition, s.resources)
		if bindingErr != nil {
			return abort(bindingErr)
		}
		bindings := tool.Bindings{SessionID: id, LoopID: loopID, Workspace: s.newWorkspaceBinding(), Delegate: s.delegation.controllerFor(loopID, definition), Process: processBinding, ExtraTools: delegateExtraTools(definition, s.delegation)}
		bound, bindErr := definition.Bind(sessionCtx, bindings)
		if bindErr != nil {
			return abort(bindErr)
		}
		prepared = append(prepared, preparedPrimer{name: name, def: definition, loop: preparedLoop{id: loopID, bound: bound, bindings: bindings}})
	}
	if len(prepared) == 0 {
		return abort(&SessionError{Kind: SessionLoopNotFound})
	}
	for _, primer := range prepared {
		if s.collabBrokerRequired(primer.loop.bound) {
			if err := s.startCollabBroker(); err != nil {
				return abort(err)
			}
			break
		}
	}

	// The hub is built before any loop, so a loop publishing through the session's
	// PublishEvent never sees a nil hub. With no subscribers yet, this delivers to
	// nobody (a no-op), but it is the session's authoritative session-scoped start.
	// Config is the fingerprint of the agent configuration this session started
	// under, stamped here so a durable journal can detect a config change on restore.
	// Rig supplies the SAME projection the restore comparison uses, so the stamped and
	// compared-against fingerprints cannot drift.
	// The rig fingerprint provider returns the same topology projection for every
	// primer; calling it with the active primer keeps the compatibility contract.
	// orderedPrimers placed topology.ActivePrimer first (unconditionally) and prepared was
	// built in that order, so prepared[0] is always the active primer — no rescan needed.
	activePrepared := prepared[0]
	if err := s.hub.PublishEventChecked(sessionCtx, event.SessionStarted{Header: startedHeader, Config: s.projectFingerprint(activePrepared.loop.bound), Manifest: s.projectManifest()}); err != nil {
		return abort(&SessionError{Kind: SessionContextDone, Cause: err})
	}
	if s.initialWorkspaceCheckpoint != "" {
		if err := s.recordSeedCheckpoint(sessionCtx, s.initialWorkspaceCheckpoint); err != nil {
			return abort(err)
		}
	}

	for _, primer := range prepared {
		loopID, createErr := s.newLoop(loop.Provenance{}, primer.def, "", "", &primer.loop)
		if createErr != nil {
			return abort(createErr)
		}
		if primer.name == topology.ActivePrimer {
			s.activeLoopID = loopID
		}
	}
	s.loopsMu.Lock()
	s.constructing = false
	s.loopsMu.Unlock()
	return s, nil
}

// interruptLoop sends a best-effort Interrupt to the loop to cancel its active
// turn, escaping on the loop's Done so a stopped loop never wedges the send. It is
// the loop-targeted cancel primitive interruptLoopID delegates to (the agent
// drain's ctx-cancel fail-safe). The ack is buffered(1) and unread here: the
// caller observes the cancellation through the resulting TurnInterrupted terminal,
// not this command's reply. An id-gen failure is swallowed (best-effort): the worst
// case is the turn runs to its natural terminal instead of being interrupted.
//
// loopID is the dispatch target, passed in for the intent-log append (the Interrupt
// itself carries no addressing); it is appended audit-only before the send on the
// session lifetime ctx (interruptLoop has no ctx of its own — it is best-effort).
func (s *Session) interruptLoop(loopID uuid.UUID, l loop.Backend) {
	id, err := s.newID()
	if err != nil {
		return
	}
	ack := make(chan bool, 1)
	cmd := command.Interrupt{Header: command.Header{CommandID: id, CreatedAt: s.stampNow()}, Ack: ack}
	// Intent log (audit-only): append before dispatch; failure is logged and the
	// best-effort interrupt proceeds.
	s.appendCommand(s.sessionCtx, loopID, cmd)
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	select {
	case commandSinkFor(l, cmd) <- cmd:
	case <-l.DoneChan():
	case <-timer.C:
	}
}

// interruptLoopID interrupts a SPECIFIC loop's active turn by id (machine-originated,
// fire-and-forget). It resolves loopID then delegates to interruptLoop. Returns
// SessionLoopNotFound if no such loop is registered. It is the per-loop lever the
// agent drain uses as a fail-safe (drainToFinalText, later task), and the
// loop-targeted counterpart to the distributed Session.Interrupt.
//
// There is deliberately no ctx parameter: interruptLoop is best-effort and already
// escapes on the loop's Done, and the drain calls this on its own ctx.Done(), so a
// ctx here would arrive already-cancelled and useless. Like interruptLoop the send
// stays Agency=AgencyMachine (the zero value) — a programmatic per-loop interrupt is
// a machine action, never falsely attributed to a human.
func (s *Session) interruptLoopID(loopID uuid.UUID) error {
	l, ok := s.loopFor(loopID)
	if !ok {
		return &SessionError{Kind: SessionLoopNotFound}
	}
	if l == nil {
		return &SessionError{Kind: SessionLoopExited}
	}
	s.interruptLoop(loopID, l)
	return nil
}

// cancelDelegateRequest asks one loop actor to atomically cancel exactly one
// managed request. The actor decides queued/active/terminal without a session-side
// TOCTOU and never affects a different request on the same loop.
func (s *Session) cancelDelegateRequest(loopID, requestID uuid.UUID) (command.DelegateCancelResult, error) {
	// Refuse the cancel BEFORE its durable intent-log append once the session context is
	// cancelled (teardown/crash). A wait:false request's background drain runs on
	// s.sessionCtx and fires this cancel from drainCorrelated's ctx.Done() branch — i.e.
	// exactly when the session is going away. At that point there is nothing to cancel (the
	// loop is being torn down and no waiter will collect the request; restore classifies the
	// never-terminated request as Interrupted from the durable log without this record), and
	// appendCommand would otherwise write a late CancelDelegateRequest to a journal whose
	// single-writer lease is being released — racing a successor's opening LeaseFence and
	// spuriously failing its append. Fail closed here (SessionContextDone) so the append
	// never escapes teardown; callers already ignore this cancel's error.
	if err := s.sessionCtx.Err(); err != nil {
		return command.DelegateCancelNoop, &SessionError{Kind: SessionContextDone, Cause: err}
	}
	l, ok := s.loopFor(loopID)
	if !ok {
		return command.DelegateCancelNoop, &SessionError{Kind: SessionLoopNotFound}
	}
	if l == nil {
		return command.DelegateCancelNoop, &SessionError{Kind: SessionLoopExited}
	}
	id, err := s.newID()
	if err != nil {
		return command.DelegateCancelNoop, err
	}
	ack := make(chan command.DelegateCancelResult, 1)
	cmd := command.CancelDelegateRequest{
		Header:          command.Header{CommandID: id, CreatedAt: s.stampNow()},
		Coordinates:     identity.Coordinates{SessionID: s.sessionID, LoopID: loopID},
		TargetCommandID: requestID,
		Ack:             ack,
	}
	s.appendCommand(s.sessionCtx, loopID, cmd)
	select {
	case l.CommandSink() <- cmd:
	case <-s.sessionCtx.Done():
		return command.DelegateCancelNoop, &SessionError{Kind: SessionContextDone, Cause: s.sessionCtx.Err()}
	case <-l.DoneChan():
		return command.DelegateCancelNoop, &SessionError{Kind: SessionLoopExited}
	}
	select {
	case result := <-ack:
		return result, nil
	case <-s.sessionCtx.Done():
		return command.DelegateCancelNoop, &SessionError{Kind: SessionContextDone, Cause: s.sessionCtx.Err()}
	case <-l.DoneChan():
		return command.DelegateCancelNoop, &SessionError{Kind: SessionLoopExited}
	}
}

// Submit is the HUMAN-ONLY submit entry point: it stamps Agency=AgencyUser (a
// person authored this input). Programmatic/machine callers go through
// submitToLoop with Agency=AgencyMachine (the agent path).
//
// Submit sends input as a queueable UserInput to the active loop,
// FIRE-AND-FORGET: it returns the InputID (the submit command's id, == the
// Cause.CommandID on the resulting Reply events) and a transport error only if the
// command could not be handed to the loop. The outcome — InputQueued /
// TurnStarted / TurnFoldedInto / TurnRejected / InputCancelled — is observed on
// the event fan-in (each Reply carries Cause.CommandID == this returned id), NOT
// returned here.
//
// A submit while a turn is running QUEUES rather than rejecting; a submit while idle
// starts a turn. Submit never reads a reply, so it returns the instant the command
// is accepted by the loop.
//
// The send carries the standard escapes: ctx.Done() →
// SessionContextDone, the loop's Done → SessionLoopExited, and a missing active
// loop → SessionLoopNotFound. On any of those the returned id is the zero UUID,
// because nothing was sent and there is no correlation to hand back.
func (s *Session) Submit(ctx context.Context, input []content.Block) (uuid.UUID, error) {
	// Submit is the active-loop, human-authored (AgencyUser) case of submitToLoop:
	// the interactive submit targets the active loop and stamps user agency. The
	// loop-targeted core (a sub-loop, machine agency) is the agent path.
	s.loopsMu.RLock()
	active := s.activeLoopID
	s.loopsMu.RUnlock()
	return s.submitToLoop(ctx, active, input, identity.AgencyUser, false)
}

// SubmitToLoop is the loop-targeted counterpart of Submit: it sends human-authored
// (AgencyUser) input to a SPECIFIC loop's CommandSink rather than the active selection. It is the
// modern viewport's "submit to the FOCUSED loop" primitive — a submit while focused on a
// agent runs a NEW turn on THAT loop (accepted: a submit to an idle-but-tracked
// agent starts a fresh turn on it), while a submit to the active loop id behaves
// exactly like Submit.
//
// Like Submit it stamps command.UserInput with Agency=AgencyUser and is FIRE-AND-FORGET:
// it returns the minted InputID (the Cause.CommandID the resulting Reply events carry on
// the session fan-in) and a transport error only; the turn outcome —
// InputQueued / TurnStarted / TurnFoldedInto / TurnRejected / InputCancelled — is observed
// on the event fan-in, never returned here. The send carries the same escapes as Submit:
// ctx.Done() → SessionContextDone, the loop's Done → SessionLoopExited, and an unknown
// loop id → SessionLoopNotFound. On any of those the returned id is the zero UUID, because
// nothing was sent and there is no correlation to hand back. It delegates to the shared
// loop-targeted core submitToLoop with AgencyUser, exactly as Submit does for the active loop.
func (s *Session) SubmitToLoop(ctx context.Context, loopID uuid.UUID, blocks []content.Block) (uuid.UUID, error) {
	return s.submitToLoop(ctx, loopID, blocks, identity.AgencyUser, false)
}

// Compact requests manual compaction of the currently active loop. The active
// id is sampled once, then routed through the exact-target implementation.
func (s *Session) Compact(ctx context.Context) (uuid.UUID, error) {
	s.loopsMu.RLock()
	active := s.activeLoopID
	s.loopsMu.RUnlock()
	return s.CompactToLoop(ctx, active)
}

// CompactToLoop requests manual compaction of one exact live native loop. The
// trusted session boundary owns user agency and command coordinates; callers
// receive only the correlation id used by durable waiter outcomes.
func (s *Session) CompactToLoop(ctx context.Context, loopID uuid.UUID) (uuid.UUID, error) {
	if err := s.faultIfFaulted(); err != nil {
		return uuid.UUID{}, err
	}
	s.loopsMu.RLock()
	h, ok := s.loops[loopID]
	s.loopsMu.RUnlock()
	if !ok {
		return uuid.UUID{}, &SessionError{Kind: SessionLoopNotFound}
	}
	if h.bound.Engine() != loop.EngineNative {
		return uuid.UUID{}, &SessionError{Kind: SessionCompactionUnsupported}
	}
	if _, configured := h.bound.CompactionPolicy(); !configured {
		return uuid.UUID{}, &SessionError{Kind: SessionCompactionUnsupported}
	}
	select {
	case <-h.backend.DoneChan():
		return uuid.UUID{}, &SessionError{Kind: SessionLoopExited}
	default:
	}
	id, err := s.newCommandID()
	if err != nil {
		return uuid.UUID{}, err
	}
	cmd := command.Compact{
		Header:      command.Header{CommandID: id, Agency: identity.AgencyUser, CreatedAt: s.stampNow()},
		Coordinates: identity.Coordinates{SessionID: s.sessionID, LoopID: loopID},
	}
	s.appendCommand(ctx, loopID, cmd)
	select {
	case h.backend.CommandSink() <- cmd:
		return id, nil
	case <-ctx.Done():
		return uuid.UUID{}, &SessionError{Kind: SessionContextDone, Cause: ctx.Err()}
	case <-h.backend.DoneChan():
		return uuid.UUID{}, &SessionError{Kind: SessionLoopExited}
	}
}

// submitToLoop submits a UserInput to a SPECIFIC loop with the given Agency,
// returning the minted CommandID (correlate Reply events via Cause.CommandID).
// It is the loop-targeted core of Submit: public Submit is the active-loop,
// AgencyUser case; the agent path targets a sub-loop with AgencyMachine.
//
// Like Submit it is FIRE-AND-FORGET: the outcome —
// InputQueued / TurnStarted / TurnFoldedInto / TurnRejected / InputCancelled — is
// observed on the event fan-in (each Reply carries Cause.CommandID == the returned
// id), not returned here. The send carries the same escapes: ctx.Done() →
// SessionContextDone, the loop's Done → SessionLoopExited, and an unknown loop id →
// SessionLoopNotFound. On any of those the returned id is the zero UUID, because
// nothing was sent and there is no correlation to hand back.
func (s *Session) submitToLoop(ctx context.Context, loopID uuid.UUID, blocks []content.Block, agency identity.Agency, noFold bool) (uuid.UUID, error) {
	// Fail-secure: a faulted session (a required durable append failed) admits no new
	// work. Checked before any loop lookup or id mint so nothing is sent.
	if err := s.faultIfFaulted(); err != nil {
		return uuid.UUID{}, err
	}
	l, ok := s.loopFor(loopID)
	if !ok {
		return uuid.UUID{}, &SessionError{Kind: SessionLoopNotFound}
	}
	if l == nil {
		return uuid.UUID{}, &SessionError{Kind: SessionLoopExited}
	}
	id, err := s.newCommandID()
	if err != nil {
		return uuid.UUID{}, err
	}
	// Queueable submit: Cause.CommandID is zero (root); the outcome is observed on the
	// session fan-in. Agency is caller-chosen — AgencyUser for the interactive human
	// Submit, AgencyMachine for the agent task submit — so a machine path never
	// claims user agency. noFold is true only for the delegate follow-up path, which must
	// start a distinct correlated turn rather than fold into the child's running turn.
	cmd := command.UserInput{Header: command.Header{CommandID: id, Agency: agency, CreatedAt: s.stampNow()}, Blocks: blocks, NoFold: noFold}
	// Intent log (audit-only): append BEFORE dispatch; an append failure is logged and
	// the submit proceeds (a lost record must never block the user's input).
	s.appendCommand(ctx, loopID, cmd)
	select {
	case l.CommandSink() <- cmd:
		return id, nil
	case <-ctx.Done():
		return uuid.UUID{}, &SessionError{Kind: SessionContextDone, Cause: ctx.Err()}
	case <-l.DoneChan():
		return uuid.UUID{}, &SessionError{Kind: SessionLoopExited}
	}
}

// RunAgent creates an in-session sub-loop, runs one turn on it for the given
// blocks (machine-originated), and returns the sub-loop's final assistant text. It
// is the SOLE exported entry point of the agent composition: it wires together
// the unexported building blocks (NewLoop + SubscribeEvents + submitToLoop +
// drainToFinalText + interruptLoopID) so the agent collaboration tools' injected capability
// (a later task) has one method to call and the blocks stay package-private.
//
// cfg is the sub-loop's loop.Definition — the CALLER builds a FRESH cfg per call (its
// own ToolSet and access gate) so each sub-loop has independent approval state;
// RunAgent never reuses a shared ToolSet. parent is the spawning loop/turn/step
// provenance (recorded on the sub-loop's registry entry and stamped on its
// LoopStarted). The submit is stamped Agency=AgencyMachine — an agent turn is a
// machine action, never falsely attributed to a human.
//
// The sub-loop PERSISTS idle after the turn (loops are never deleted, design §8):
// RunAgent closes only the SUBSCRIPTION, never the loop. The ordering is
// load-bearing: it subscribes (scoped to the new sub-loop) BEFORE submitting, so the
// opening TurnStarted the drain correlates on cannot be missed — the hub has no
// replay (design §4).
//
// Errors propagate from the first block that fails: NewLoop (e.g.
// SessionClosing while shutting down, or id-gen failure), SubscribeEvents, the
// submit, or the drain's typed §5 failures (*drainFailedError / *TurnRejectedError /
// *drainInterruptedError / *drainLostError). ctx is the calling turn's context;
// because submits carry no ctx, a ctx cancel cannot reach the sub-loop's turn — the
// drain translates it into a single loop-targeted Interrupt (the closure below) and
// drains to the resulting TurnInterrupted terminal.
func (s *Session) RunAgent(ctx context.Context, parent loop.Provenance, cfg loop.Definition, blocks []content.Block, parentToolUseID string) (string, error) {
	// newLoop publishes LoopStarted and fails SessionClosing if the session is
	// shutting down; either way no sub-loop is left behind a returned error.
	// parentToolUseID is stamped onto that LoopStarted so the sub-loop correlates back
	// to the spawning agent tool call across persist/restore.
	subLoopID, err := s.newLoop(parent, cfg, parentToolUseID, "", nil)
	if err != nil {
		return "", err
	}

	// Subscribe BEFORE submitting (design §4): the hub has no replay, so a
	// subscription created after the submit could miss the opening TurnStarted and the
	// drain would then block until ctx-cancel or subscription loss. The filter is
	// scoped to JUST this sub-loop (Enduring carries StepDone + terminals the drain
	// needs); Ephemeral is left empty so the sub-loop's token firehose never enters
	// this subscription's egress buffer.
	sub, err := s.SubscribeEvents(event.EventFilter{
		Enduring: event.LoopScope{Loops: map[uuid.UUID]struct{}{subLoopID: {}}},
	})
	if err != nil {
		return "", err
	}
	// Close the SUBSCRIPTION (not the sub-loop — it persists idle, design §8).
	// EventSubscription.Close is documented to always return nil (idempotent,
	// records no error), so there is nothing to handle; this mirrors every other
	// sub.Close() call site in the package.
	defer func() { _ = sub.Close() }()

	// Machine-originated submit: an agent turn is our code's action, so it stamps
	// AgencyMachine (the submitToLoop core, not the human-only Submit).
	cmdID, err := s.submitToLoop(ctx, subLoopID, blocks, identity.AgencyMachine, false)
	if err != nil {
		return "", err
	}

	// Drain to the sub-loop's terminal. The interrupt closure is the drain's
	// ctx-cancel fail-safe: it is loop-targeted to THIS sub-loop. interruptLoopID only
	// errors SessionLoopNotFound, which is impossible for a sub-loop we just created
	// and never delete; so a non-nil error here is best-effort-logged (never swallowed
	// with _) rather than failing the drain.
	return drainToFinalText(ctx, sub, cmdID, func() {
		if ierr := s.interruptLoopID(subLoopID); ierr != nil {
			slog.WarnContext(ctx, "agent interrupt failed", "loop", subLoopID, "err", ierr)
		}
	})
}

// loopSnapshot pairs a loop id with its handle, captured under loopsMu by the
// Interrupt/Shutdown fan-outs. The loop id is carried alongside the handle (the map
// key is otherwise lost on snapshot) so the per-loop intent-log append can target the
// right loop's command subject — Interrupt/Shutdown carry no addressing of their own.
type loopSnapshot struct {
	loopID uuid.UUID
	handle *loopHandle
}

// Interrupt is the human "stop everything": it cancels the running turn of EVERY live loop
// in the session — every registered loop — marking each interrupt-pending before
// fanning the command.Interrupt out to all of them CONCURRENTLY (design: "Session.Interrupt marks
// every live loop interrupt-pending before concurrently sending commands"). Idle loops ack false
// and are harmless; Interrupt returns true iff ANY loop reported it cancelled a running turn. A
// fully-idle interrupt is fail-quiet: it returns false and appends no events. ctx bounds the
// fan-out so a slow actor cannot wedge it.
//
// Unlike Shutdown, Interrupt does NOT latch closing and does NOT tear loops down. It stamps
// Agency=AgencyUser (a human pressed interrupt). Selection + marking + concurrent delivery +
// the admission barrier live in interrupt.go (runInterrupt); this is the session-wide scope.
func (s *Session) Interrupt(ctx context.Context) (bool, error) {
	any, _, err := s.runInterrupt(ctx, func() ([]loopSnapshot, bool) {
		return s.liveLoopSnapshotLocked(), true
	}, identity.AgencyUser)
	return any, err
}

// releaseLease invokes the lease-release hook EXACTLY ONCE (releaseOnce) on a fresh,
// bounded background context, swallowing the error (the bucket TTL is the backstop and Shutdown's own error is
// the caller-facing one). It is nil-safe: a headless session (no WithLeaseRelease, no
// Restore-installed releaser) has no hook and this is a no-op. Idempotent so a second
// Shutdown never double-releases.
func (s *Session) releaseLease(_ context.Context) {
	s.releaseOnce.Do(func() {
		if s.leaseRelease == nil {
			return
		}
		releaseCtx, cancel := context.WithTimeout(context.Background(), leaseReleaseTimeout)
		defer cancel()
		if err := s.leaseRelease(releaseCtx); err != nil {
			slog.WarnContext(releaseCtx, "session: lease release on shutdown failed (TTL is the backstop)",
				"session", s.sessionID, "err", err)
		}
	})
}

// releaseRootLease releases the EXCLUSIVE workspace root lease EXACTLY ONCE
// (releaseRootOnce), on a fresh bounded background context, swallowing the error (the lease TTL is the
// backstop). Nil-safe: per-session, shared, and no-placement sessions have no root lease.
// Shutdown calls this BEFORE releaseLease so the root lease is relinquished before the
// session lease (LIFO teardown), and after work/checkpoints have stopped.
func (s *Session) releaseRootLease(_ context.Context) {
	s.releaseRootOnce.Do(func() {
		if s.wsRootRelease == nil {
			return
		}
		releaseCtx, cancel := context.WithTimeout(context.Background(), leaseReleaseTimeout)
		defer cancel()
		if err := s.wsRootRelease(releaseCtx); err != nil {
			slog.WarnContext(releaseCtx, "session: workspace root lease release on shutdown failed (TTL is the backstop)",
				"session", s.sessionID, "err", err)
		}
	})
}

// watchRootLease starts the exclusive root-lease loss watcher (a no-op unless an exclusive
// placement wired wsLeaseLost). On loss it FAULTS the session — latches faulted with a
// typed *WorkspaceRootLeaseLostError so admission closes — and cancels the session context
// so live loops are interrupted and any session-scoped checkpoint is cancelled. The
// coordinator's Healthy() independently fences cooperative structured mutators after loss.
func (s *Session) watchRootLease() {
	if s.wsLeaseLost == nil {
		return
	}
	go func() {
		select {
		case <-s.sessionCtx.Done():
			return
		case <-s.wsLeaseLost:
			// Latch the fault + wake WaitIdle waiters (symmetry with ReportFault), THEN
			// cancel the session context so live loops are interrupted and any
			// session-scoped checkpoint is cancelled.
			s.faultWorkspaceInconsistent(&WorkspaceRootLeaseLostError{})
			s.sessionCancel()
		}
	}()
}

// startOffloadGC arms the offload-GC runner (bound to the hub's native-idle probe) once the
// hub exists. It mirrors watchRootLease: called by the composition root after construction.
// A no-op unless WithOffloadGC installed a runner.
func (s *Session) startOffloadGC() {
	if s.offloadGC != nil {
		s.offloadGC.start(s.hub.IsIdle)
	}
}

// stopOffloadGC stops and joins the offload-GC runner. It is called at the very start of
// Shutdown's teardown — before SessionStopped is appended and before the lease is released
// — and on construction abort. Idempotent and nil-safe.
func (s *Session) stopOffloadGC() {
	if s.offloadGC != nil {
		s.offloadGC.Stop()
	}
}

// faultWorkspaceInconsistent latches the session's fault state with a workspace-integrity
// cause (root-lease loss, or a restore whose rollback itself failed) and wakes every
// blocked WaitIdle waiter — mirroring ReportFault, but for failures the hub's durable tap
// never sees (there is no failed enduring append). It is idempotent: the FIRST fault
// records the cause. FailWaiters is called OUTSIDE loopsMu (it takes the hub lock, which is
// never held together with loopsMu).
func (s *Session) faultWorkspaceInconsistent(cause error) {
	s.latchSessionFault(cause)
}

// newWorkspaceBinding returns the tool.WorkspaceBinding to populate at a loop bind site, or
// nil when the session has no managed workspace (leaving tool.Bindings.Workspace nil, the
// no-placement default). Each loop gets a FRESH per-loop observation set so a Bash run
// invalidates exactly its own loop's file-tool observations, while all loops share the ONE
// session coordinator (cross-loop mutation serialization).
func (s *Session) newWorkspaceBinding() *tool.WorkspaceBinding {
	if s.wsCoordinator == nil {
		return nil
	}
	return &tool.WorkspaceBinding{Root: s.wsRoot, Coordinator: s.wsCoordinator, Observations: tool.NewWorkspaceObservations()}
}

// activateProcessServiceBridge attaches Task 24B's checked durable lifecycle
// publisher and this Session's own Task 24C completion notifier
// (NotifyProcessCompletion) behind the session's process-service bridge
// (process_services.go). It is nil-safe: a session with no process-enabled
// loop never resolved a resources registry, so this is then a no-op.
//
// It MUST be called after s.hub exists (the checked publisher publishes
// through it) and BEFORE the resources registry's one-time Activate runs: a
// process resource that decides during its own Activate to publish a
// lifecycle record (for example marking a manifest it cannot otherwise
// confirm as lost) must observe the checked path immediately, never the
// bridge's pre-attachment "services unavailable" stub. The live construction
// path (newSessionTopology) calls this right after building the hub; the
// restore path (restoreTopologySession) calls it after buildRestoredSession
// returns and before resources.Activate — both strictly before the session
// (or restore) is reachable/committed.
func (s *Session) activateProcessServiceBridge() error {
	if s.resources == nil {
		return nil
	}
	publisher, err := newCheckedProcessLifecyclePublisher(s.sessionID, s.hub, s.now)
	if err != nil {
		return err
	}
	workflowPublisher, err := newCheckedWorkflowActivityPublisher(s.sessionID, s.hub, s.now)
	if err != nil {
		return err
	}
	s.resources.processServiceBridge.attachProcessLifecyclePublisher(publisher)
	s.resources.processServiceBridge.attachProcessCompletionNotifier(s)
	s.resources.processServiceBridge.attachWorkflowActivityPublisher(workflowPublisher)
	return nil
}

// shutdownTarget pairs a loop with the Ack channel of the command.Shutdown the
// session sent it, so the ack-wait phase can drain each loop's reply in turn. It
// is Shutdown-internal: the send phase records one per loop actually reached, and
// the wait phase reads them back.
type shutdownTarget struct {
	loop loop.Backend
	ack  chan error
}

// Shutdown drives the WHOLE session to its stopped phase and blocks until every
// loop's actor exits. Sub-loops are retained (each runs its own goroutine and
// loopCtx), so Shutdown must reach every loop. The order is
// deliberate:
//
//  1. Latch closing AND snapshot the loops in ONE loopsMu critical section. This
//     is the atomicity NewLoop's registration check pairs with (it re-tests
//     closing under the same lock): a loop is either already in this snapshot or
//     refused by NewLoop — never registered after the snapshot is taken.
//  2. Close hustle admission and cancel queued/executing inference. Keep the
//     checkpoint controller, hub, and session context open for owned cleanup.
//  3. Send command.Shutdown to EVERY loop in the snapshot, recording each reached
//     loop's (loop, ack) pair. Per loop: mint a CommandID; on id-gen failure SKIP
//     that loop's graceful shutdown (the final sessionCancel hard-cancels it)
//     rather than aborting the whole Shutdown. A loop already exited is skipped.
//  4. Wait for every recorded ack, then join hustle terminal audit, finalizers,
//     and blocking activity release through Controller.Drained.
//  5. Stop/join checkpoints and offload GC, append SessionStopped/stop the hub,
//     release root/session leases, and cancel sessionCtx last.
//  6. Loop/checkpoint/hub phases have private deadlines derived from validated
//     component bounds. Hustle audit, finalization, and worker drain use their own
//     trusted inner bounds and are always joined; an outer deadline never detaches
//     owned cleanup. Caller cancellation is diagnostic only.
//
// Concurrent and repeated calls join one teardown owner and receive the same cleanup
// result, augmented with each caller's own context error after cleanup completes.
func (s *Session) Shutdown(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if hustleFinalizerOwnsSession(ctx, s) {
		return &HustleShutdownReentryError{}
	}
	s.shutdownMu.Lock()
	if s.shutdownStarted {
		done := s.shutdownDone
		s.shutdownMu.Unlock()
		<-done
		s.shutdownMu.Lock()
		cleanupErr := s.shutdownErr
		s.shutdownMu.Unlock()
		return shutdownResult(cleanupErr, ctx.Err())
	}
	s.shutdownStarted = true
	s.shutdownDone = make(chan struct{})
	s.shutdownMu.Unlock()

	cleanupErr := s.shutdown()
	s.shutdownMu.Lock()
	s.shutdownErr = cleanupErr
	close(s.shutdownDone)
	s.shutdownMu.Unlock()
	return shutdownResult(cleanupErr, ctx.Err())
}

func (s *Session) shutdown() error {
	shutdownRoot := context.Background()
	if s.sessionCtx != nil {
		shutdownRoot = context.WithoutCancel(s.sessionCtx)
	}
	// Serialize the closing latch with SetActiveLoop's durable append→visibility
	// transaction. Once closing is visible, no active-loop change may start.
	s.activeMu.Lock()
	// (1) Latch closing and snapshot the loops atomically under loopsMu — the same
	// lock NewLoop's registration check re-tests closing under, so no loop can be
	// registered after this snapshot is taken.
	s.loopsMu.Lock()
	s.closing = true
	snapshot := make([]loopSnapshot, 0, len(s.loops))
	for lid, h := range s.loops {
		snapshot = append(snapshot, loopSnapshot{loopID: lid, handle: h})
	}
	s.loopsMu.Unlock()
	s.activeMu.Unlock()
	timeouts := s.resolveShutdownTimeouts(snapshot)
	// Revoke every origin before any target loop receives Shutdown. This closes
	// the authority race: a foreign adapter can no longer admit MessageAgent
	// work while its loop is draining.
	for _, item := range snapshot {
		s.revokeCollabLoop(item.loopID)
	}
	// design §15's fourth cancellation trigger: the session shuts down. Every
	// active permission review is signalled to stop before the shared Hustle
	// runtime begins its own drain, so in-flight classifier calls observe
	// cancellation as early as possible.
	s.shutdownPermissionReviews()
	failures := make([]error, 0, 7)
	failures = append(failures, s.closeHustles(shutdownRoot, timeouts.hustle))
	targets, sendErr := s.sendLoopShutdowns(shutdownRoot, snapshot, timeouts.loopSend)
	failures = append(failures, sendErr)
	failures = append(failures, s.waitLoopShutdowns(shutdownRoot, snapshot, targets, timeouts.loopDrain))
	s.waitHustlesDrained()
	// The broker waits for already-admitted calls to release before its endpoint
	// is removed. Its admission/I/O deadlines are separate from MessageAgent's
	// response observation deadline.
	failures = append(failures, s.closeCollabBrokerWithTimeout(shutdownRoot, timeouts.collabBroker))

	// From here onward every phase gets a fresh private deadline. A timeout in one
	// component therefore cannot suppress checkpoint, durable-stop, or lease cleanup.
	s.stopOffloadGC()
	failures = append(failures, s.stopCheckpoints(shutdownRoot, timeouts.checkpoint))
	// Session resources (including the process registry) must fully terminate and
	// confirm before the hub stops and the leases/session context release: their own
	// Shutdown is where a live supervised process is actually stopped and its final
	// lifecycle/completion record durably published, and that publication still needs
	// a live hub to reach durably (see stopHub below, which only runs after this
	// returns).
	failures = append(failures, s.stopSessionResources(shutdownRoot, timeouts.sessionResources))
	failures = append(failures, s.stopHub(shutdownRoot, timeouts.hub))
	s.releaseRootLease(shutdownRoot)
	s.releaseLease(shutdownRoot)
	if s.sessionCancel != nil {
		s.sessionCancel()
	}
	s.abandonForeignDeliveryHooks()
	return combineShutdownErrors(failures...)
}

type shutdownErrorSet struct{ Causes []error }

func (e *shutdownErrorSet) Error() string   { return "session: shutdown cleanup failed" }
func (e *shutdownErrorSet) Unwrap() []error { return e.Causes }

func combineShutdownErrors(causes ...error) error {
	filtered := make([]error, 0, len(causes))
	for _, cause := range causes {
		if cause != nil {
			filtered = append(filtered, cause)
		}
	}
	switch len(filtered) {
	case 0:
		return nil
	case 1:
		return filtered[0]
	default:
		return &shutdownErrorSet{Causes: filtered}
	}
}

func shutdownResult(cleanupErr, callerErr error) error {
	cause := combineShutdownErrors(cleanupErr, callerErr)
	if cause == nil {
		return nil
	}
	return &SessionError{Kind: SessionContextDone, Cause: cause}
}

// routeGate sends a fire-and-route gate command to the resolved target loop. These
// commands carry no Ack, so routeGate returns nil as soon as the send completes
// and never waits for a reply. It selects on ctx.Done() and the loop's Done
// channel alongside the unbuffered send so the call can never block forever when
// the actor is busy (ctx times out) or has already exited (Done is closed).
//
// loopID is the resolved dispatch target (route.LoopID), passed in for the intent-log
// append — appended audit-only BEFORE the send; an append failure is logged and the
// gate reply proceeds (a lost record must never block a human's approval/denial).
func (s *Session) routeGate(ctx context.Context, loopID uuid.UUID, l loop.Backend, cmd command.Command) error {
	s.appendCommand(ctx, loopID, cmd)
	select {
	case l.CommandSink() <- cmd:
		return nil
	case <-l.DoneChan():
		return &SessionError{Kind: SessionLoopExited}
	case <-ctx.Done():
		return &SessionError{Kind: SessionContextDone, Cause: ctx.Err()}
	}
}
