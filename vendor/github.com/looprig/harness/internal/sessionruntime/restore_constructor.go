package sessionruntime

import (
	"context"
	"errors"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/internal/loopruntime"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/foreign"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/hub"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/harness/pkg/workspacestore"
	inferencemodel "github.com/looprig/inference/model"
)

// RestoreTopology reconstructs the durable loop topology and brings it up idle. It does
// not publish SessionStarted.
//
// Order (per the design — RestoreStarted is the FIRST restore mutation, after the lease
// fence the journal writes at construction):
//
//  1. Acquire the single-writer lease, construct the SessionJournal (writes the opening
//     LeaseFence — the handover boundary) and the EventReplayer.
//  2. Replay the stream; read the persisted config fingerprint and compare to the live
//     config. On mismatch → *ConfigMismatchError UNLESS WithAllowConfigMismatch is set.
//  3. Append RestoreStarted (the first restore mutation).
//  4. Validate, bind, and independently fold every durable declared loop.
//  5. Append checked crash closures for every open turn.
//  6. Materialize the workspace, build all loops, and validate the active loop.
//  7. Append RestoreDone as the final commit point and return the controller.
//
// On ANY failure in steps 2–7 (a config mismatch, a discovery/replay/decode/object
// failure, or an append failure) Restore durably records a RestoreErrored and returns a
// typed error; the session does NOT come up (fail-secure / no silent drift). On a FAILED
// restore the acquired lease is released here before returning (so a successor can
// re-acquire). On SUCCESS the journal HOLDS the lease for the live session's lifetime;
// releasing it at session teardown is the Phase-10 composition root's wiring (mirroring
// how the lease is composition-root-owned in the durable-tap path), out of scope here.
// RestoreTopology reconstructs the entire topology in one leased replay transaction.
func RestoreTopology(ctx context.Context, topology Topology, sessionID uuid.UUID, store *sessionstore.Store, opts ...Option) (*Session, error) {
	return restoreTopologySession(ctx, topology, sessionID, store, uuid.New, time.Now, opts...)
}

func (s *Session) attachRestoredLoop(started event.LoopStarted, parent loop.Provenance, bound loop.BoundDefinition, bindings tool.Bindings, folded foldResult, ri restoredInference, notifications []tool.ProcessCompletionNotification, foreignSID string) error {
	loopCtx, cancel := context.WithCancel(s.sessionCtx)
	var constructionHook *foreignDeliveryHook
	loopCommitted := false
	defer func() {
		if !loopCommitted && constructionHook != nil {
			s.revokeCollabLoop(constructionHook.loopID)
			s.unregisterForeignDeliveryHook(constructionHook)
		}
	}()
	var backend loop.Backend
	var err error
	switch bound.Engine() {
	case loop.EngineNative:
		var compactor loopruntime.Compactor
		compactor, err = s.compactorFor(bound, started.LoopID)
		if err == nil {
			backend, err = loopruntime.NewRestoredWithRuntime(
				loopCtx,
				s.sessionID,
				started.LoopID,
				parent,
				s,
				bound,
				restoredStateFrom(folded, ri, notifications),
				loopruntime.RuntimeDependencies{Compactor: compactor, Hooks: s.hooks, ReviewContext: s.loopReviewContext()},
			)
		}
	default:
		agentSessionID := ri.AgentSessionID
		if agentSessionID == "" {
			agentSessionID = foreignSID
		}
		if agentSessionID == "" {
			cancel()
			return &RestoreError{Kind: RestoreForeignSIDMissing}
		}
		seed := foreign.RestoredForeign{ForeignSID: foreignSID, AgentSessionID: agentSessionID, TurnIndex: folded.TurnIndex, Msgs: folded.Msgs}
		if s.usesServicesRestoredBuilder(bound) {
			servicesBuilder, builderErr := s.servicesRestoredBuilder(bound)
			if builderErr != nil {
				cancel()
				return builderErr
			}
			if err := s.startCollabBroker(); err != nil {
				cancel()
				return &RestoreError{Kind: RestoreLoopFailed, Cause: err}
			}
			services, hook := s.foreignServicesForTrackedWithController(started.LoopID, bindings.Delegate)
			constructionHook = hook
			backend, err = servicesBuilder(loopCtx, s.sessionID, started.LoopID, parent, s, bound, func() (uuid.UUID, error) { return s.newID() }, s.factory, seed, services)
			if err == nil && constructionHook != nil {
				err = constructionHook.brokerError()
			}
		} else {
			restoredBuilder, builderErr := s.restoredBuilder(bound)
			if builderErr != nil {
				cancel()
				return builderErr
			}
			backend, err = restoredBuilder(loopCtx, s.sessionID, started.LoopID, parent, s, bound, func() (uuid.UUID, error) { return s.newID() }, s.factory, seed)
		}
	}
	if err != nil {
		cancel()
		return &RestoreError{Kind: RestoreLoopFailed, Cause: err}
	}
	liveMode, liveModel := liveViewFor(bound, ri)
	s.loopsMu.Lock()
	s.loops[started.LoopID] = &loopHandle{id: started.LoopID, owner: s, bound: bound, bindings: bindings, backend: backend, parent: parent, cancel: cancel, liveMode: liveMode, liveModel: liveModel, state: tool.DelegateStatusIdle, agentName: started.DisplayName, agentMode: loop.ModeName(started.InitialMode), selectedHarness: restoredSelectedHarness(started.AgentRuntime)}
	s.loopsMu.Unlock()
	loopCommitted = true
	return nil
}

func (s *Session) restoredBuilder(bound loop.BoundDefinition) (foreign.RestoredBuilder, error) {
	if s.foreignRegistry != nil && bound.Engine() == loop.EngineAdapter && bound.RuntimeProfile() != "" {
		_, restored, err := s.foreignRegistry.Builder(bound.RuntimeProfile())
		if err != nil {
			return nil, &RestoreError{Kind: RestoreForeignBuilderMissing, Cause: err}
		}
		return restored, nil
	}
	if s.foreignBuildRestored == nil {
		return nil, &RestoreError{Kind: RestoreForeignBuilderMissing}
	}
	return s.foreignBuildRestored, nil
}

func (s *Session) usesServicesRestoredBuilder(bound loop.BoundDefinition) bool {
	return s.foreignBuildRestoredServices != nil ||
		(s.foreignRegistry != nil && bound.Engine() == loop.EngineAdapter && bound.RuntimeProfile() != "" &&
			s.foreignRegistry.HasServicesBuilder(bound.RuntimeProfile()))
}

func (s *Session) servicesRestoredBuilder(bound loop.BoundDefinition) (foreign.ServicesRestoredBuilder, error) {
	if s.foreignRegistry != nil && bound.Engine() == loop.EngineAdapter && bound.RuntimeProfile() != "" {
		_, restored, err := s.foreignRegistry.ServicesBuilder(bound.RuntimeProfile())
		if err != nil {
			return nil, &RestoreError{Kind: RestoreForeignBuilderMissing, Cause: err}
		}
		return restored, nil
	}
	if s.foreignBuildRestoredServices == nil {
		return nil, &RestoreError{Kind: RestoreForeignBuilderMissing}
	}
	return s.foreignBuildRestoredServices, nil
}

func restoreTopologySession(
	ctx context.Context,
	topology Topology,
	sessionID uuid.UUID,
	store *sessionstore.Store,
	newID idGenerator,
	now event.Clock,
	opts ...Option,
) (*Session, error) {
	activeDefinition, topologyOK := topology.definition(topology.ActivePrimer)
	if !topologyOK {
		return nil, &RestoreError{Kind: RestoreLoopFailed, Cause: &MissingTopologyError{}}
	}
	select {
	case <-ctx.Done():
		return nil, &RestoreError{Kind: RestoreContextDone, Cause: ctx.Err()}
	default:
	}
	if err := validateProcessServiceEngines(topology); err != nil {
		return nil, &RestoreError{Kind: RestoreLoopFailed, Cause: err}
	}
	// Allocate the one restored-session lifetime up front. Until successful transfer
	// into Session, every exit below owns cancellation (lease/replay/bind/check/append/
	// build failures included). Bind and the live Session receive this exact context.
	sessionCtx, sessionCancel := context.WithCancel(ctx)
	contextTransferred := false
	defer func() {
		if !contextTransferred {
			sessionCancel()
		}
	}()

	// Resolve restore-time options (the allow-mismatch flag) on a probe session so the
	// fingerprint decision can read it BEFORE the real session is built. The same opts
	// are applied to the real session in step 8.
	probe := &Session{}
	for _, opt := range opts {
		opt(probe)
	}
	constructionAbortTimeout := probe.constructionAbortTimeout
	if constructionAbortTimeout <= 0 {
		constructionAbortTimeout = defaultConstructionAbortTimeout
	}
	allowMismatch := probe.allowConfigMismatch
	// Default the restore-drift decider to the fail-secure policy when the composition root
	// injected none via WithRestoreDecider, so the NEW-PATH decision is never a nil-deref.
	if probe.restoreDecider == nil {
		probe.restoreDecider = DefaultPolicyDecider{}
	}
	// The compatibility path is chosen by whether a LIVE manifest is configured: a
	// SchemaVersion>=1 candidate (rig sessions) takes the NEW drift-assessed path; a zero
	// (SchemaVersion 0) candidate — existing tests and non-rig callers — takes the LEGACY
	// fingerprint path unchanged.
	candidate := probe.projectManifest()
	newPath := candidate.SchemaVersion >= 1
	if probe.fingerprint == nil && probe.frozenFingerprint == nil {
		return nil, &RestoreError{Kind: RestoreLoopFailed, Cause: &MissingFingerprintProviderError{}}
	}

	// (1) Acquire the single-writer lease, then construct the journal (which writes the
	// opening LeaseFence as its first append — the handover boundary) and the replayer,
	// all through the sessionstore facade over the composition-root-wired backend.
	lease, err := store.AcquireLease(ctx, sessionID)
	if err != nil {
		return nil, &RestoreError{Kind: RestoreLeaseFailed, Cause: err}
	}
	j, err := store.OpenJournalWithOpeningAppend(
		ctx,
		sessionID,
		lease,
		journal.HookMiddleware(probe.hooks, sessionID),
	)
	if err != nil {
		releaseLease(lease)
		return nil, &RestoreError{Kind: RestoreJournalFailed, Cause: err}
	}
	// Offload GC (symmetric with NewSession): when the restore-forwarded policy is armed,
	// wrap the journal with the admission gate BEFORE any appender (restore-lifecycle appends
	// or the rebuilt session's appenders) is built over it. The runner is started only after
	// the session is rebuilt.
	j, gcRunner, err := wrapJournalWithOffloadGC(store, sessionID, lease, j, probe.offloadGCPolicy)
	if err != nil {
		releaseLease(lease)
		return nil, &RestoreError{Kind: RestoreJournalFailed, Cause: err}
	}
	j = journal.WithHooks(j, probe.hooks, sessionID)
	// The replayer is bound to the stream BEGINNING (FromSeq 0). Restore intentionally
	// drains RECORDS, not events, so the private GatePreparedRecord is visible while the
	// normal event-based folds are derived from EventRecord payloads. Opening it is a
	// step-1 setup step (parallel to the journal): a failure releases the lease and
	// returns without a RestoreErrored, exactly like the journal-setup failure above (the
	// first restore MUTATION, RestoreStarted, has not been written yet).
	replayer, err := store.OpenInternalRecordReplayer(sessionID, sessionstore.ReplayRequest{FromSeq: 0})
	if err != nil {
		releaseLease(lease)
		return nil, &RestoreError{Kind: RestoreReplayFailed, Cause: err}
	}

	// The restore-lifecycle events (RestoreStarted/RestoreDone/RestoreErrored and the
	// crash-seam TurnInterrupted) are stamped from the SAME id-gen + clock seam the
	// session's own events use, then appended directly through the journal (the hub does
	// not exist yet). A failed mint fails the restore closed.
	factory := event.NewFactory(func() (uuid.UUID, error) { return newID() }, func() time.Time { return now() })

	// recordErrored durably appends a RestoreErrored carrying err, then releases the
	// lease and returns (nil session, the typed restore error). It is the single
	// fail-secure exit for every failure after the journal exists: the session never
	// comes up, and the failure is recorded in the durable log (a best-effort append — a
	// failure to record the failure never masks the original cause).
	// resolved holds the per-session managed-workspace placement (exclusive root lease +
	// coordinator) once resolved below. recordErrored releases the root lease BEFORE the
	// session lease (LIFO) so a failed restore never strands root-lease ownership.
	var resolved *resolvedPlacement
	var resources *sessionResources
	recordErrored := func(restoreErr error) (*Session, error) {
		runRestoreFailureCleanup(constructionAbortTimeout, func(appendCtx context.Context) {
			_ = appendRestoreEvent(appendCtx, j, factory, event.RestoreErrored{
				Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID}},
				Err:    restoreErr,
			})
		}, func() {
			if resources != nil {
				_ = resources.Shutdown(context.Background())
			}
			releaseResolvedRoot(context.Background(), resolved)
			releaseLease(lease)
		})
		return nil, restoreErr
	}
	abortAccepted := func(s *Session, restoreErr error) (*Session, error) {
		s.abortConstructionAfter(restoreErr, func(appendCtx context.Context) {
			_ = appendRestoreEvent(appendCtx, j, factory, event.RestoreErrored{
				Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID}},
				Err:    restoreErr,
			})
		})
		return nil, restoreErr
	}

	// (2) Replay the whole stream once for discovery (the persisted fingerprint + the
	// root loop id + the agent spawn count) and gate recovery. A ZERO LoopID leaves
	// the event projection UNNARROWED — every loop's events — so findRootLoopStarted and
	// countSpawnedLoops see the agent LoopStarted events, not just the root's. Fail
	// closed on any error.
	allRecords, err := drainRecordReplay(ctx, replayer, journal.ReplayRequest{Follow: false})
	if err != nil {
		return recordErrored(&RestoreError{Kind: RestoreReplayFailed, Cause: err})
	}
	all := eventsFromRecords(allRecords, uuid.UUID{})
	// Privileged lifecycle records are interpreted only for crash-consistency
	// validation. Unmatched starts remain in the journal as interrupted audit
	// evidence; no queue, worker, request, finalizer, activity, or synthetic
	// terminal is reconstructed.
	if _, auditErr := foldRestoredHustleAudit(all); auditErr != nil {
		return recordErrored(&RestoreError{Kind: RestoreReplayFailed, Cause: auditErr})
	}
	compactWaiterRepairs, repairErr := planCompactWaiterRepairs(all)
	if repairErr != nil {
		return recordErrored(&RestoreError{Kind: RestoreReplayFailed, Cause: repairErr})
	}

	// NEW PATH: the drift baseline is the LATEST adopted manifest (or a legacy projection of
	// the first SessionStarted); LEGACY PATH: the persisted config fingerprint from the first
	// SessionStarted governs the byte-for-byte-unchanged compatibility check.
	var baseline adoptedBaseline
	var persisted event.ConfigFingerprint
	var frozenContextStale bool
	if newPath {
		baseline, err = latestAdoptedBaseline(all)
		if err != nil {
			return recordErrored(err)
		}
	} else {
		persisted, err = firstConfigFingerprint(all)
		if err != nil {
			return recordErrored(err)
		}
		// A rig supplies a frozen fingerprint, allowing the mismatch decision to happen
		// immediately after replay and before root acquisition, binding, or reconstruction.
		if probe.frozenFingerprint != nil {
			frozenContextStale, err = restoredContextDisposition(persisted, *probe.frozenFingerprint, allowMismatch)
			if err != nil {
				return recordErrored(err)
			}
		}
	}
	// (4a) Discover every durable root (zero-Cause LoopStarted) and the ordered starts, and
	// validate that every configured primer maps to a root (single-primer recovery honored
	// under allowMismatch). (4b) Bind + independently fold every durable loop into a plan
	// and pick the active primer's plan. Both fail closed through recordErrored.
	roots, starts, err := discoverRoots(all, topology, allowMismatch)
	if err != nil {
		return recordErrored(err)
	}
	// NEW-PATH drift decision — placed HERE, right after root discovery and BEFORE workspace
	// placement (the root lease) and loop binding, so a rejection has ZERO side effects
	// (mirroring where the frozen-fingerprint mismatch rejects on the legacy path). The
	// assessment compares the live candidate against the adopted baseline and folds the
	// root-loop AgentName check in: a persisted-vs-configured name difference broadens what
	// the session answers to, so it classifies Warn and is seen by the decider. The
	// configured name is read off the definition (identical to the bound name, but available
	// before binding). A decider error or a rejection routes through the SAME fail-secure
	// exit a fingerprint mismatch uses (recordErrored: append RestoreErrored, release the
	// leases, return the typed error). On accept, an adoption is durably recorded after
	// RestoreStarted below when the config actually changed or the schema was upgraded.
	var assessment event.DriftAssessment
	var decision RestoreDecision
	newPathStale := false
	adoptOnAccept := false
	if newPath {
		assessment = event.AssessDrift(baseline.Manifest, candidate)
		persistedName := roots[topology.ActivePrimer].AgentName
		if configuredName := activeDefinition.Name(); persistedName != configuredName {
			// A persisted-vs-configured root-loop NAME difference has its OWN category
			// (DriftAgentName) — a broader "what does the session answer to" change,
			// distinct from the agent KIND (DriftAgentKind). Still Warn, so the default
			// policy decider's severity-keyed behavior is unchanged.
			assessment.Changes = append(assessment.Changes, event.DriftChange{
				Category: event.DriftAgentName,
				Old:      string(persistedName),
				New:      string(configuredName),
				Severity: event.DriftWarn,
			})
		}
		dec, derr := probe.restoreDecider.DecideRestore(ctx, assessment)
		if derr != nil {
			// A decider FAILURE (a returned error or a timeout — a timeout is a rejection)
			// fails secure AS a rejection, but stays classifiable: RestoreRejectedError
			// carries the assessment and wraps the cause (Unwrap), so a caller keeps the
			// structured drift AND can errors.As/errors.Is through to the underlying error
			// (e.g. context.DeadlineExceeded). Source is left zero — Error() defaults it to
			// "policy".
			return recordErrored(&RestoreRejectedError{Assessment: assessment, Cause: derr})
		}
		if !dec.Accept {
			return recordErrored(&RestoreRejectedError{Assessment: assessment, Source: dec.Source})
		}
		// Normalize the ACCEPTING decision so a custom RestoreDecider can never brick a
		// restore with an invalid durable ConfigurationAdopted (it would fail validation on
		// EVERY subsequent restore → permanently unrestorable). An accepting decision that
		// omitted a Source is, by default, a policy decision; an over-long Message/Actor is
		// TRUNCATED, never rejected — a long audit note must not make the session
		// unrestorable. Only the accept path builds a durable event; a reject uses dec solely
		// for RestoreRejectedError. A decider-supplied `migration` source is ALSO normalized to
		// policy: DecisionSourceMigration is Valid() but reserved for Harness itself (a Phase-2
		// migration) — a RestoreDecider must never stamp a false migration adoption.
		if !dec.Source.Valid() || dec.Source == event.DecisionSourceMigration {
			dec.Source = event.DecisionSourcePolicy
		}
		if len(dec.Message) > event.MaxConfigMessageLen {
			dec.Message = dec.Message[:event.MaxConfigMessageLen]
		}
		if len(dec.Actor) > event.MaxConfigActorLen {
			dec.Actor = dec.Actor[:event.MaxConfigActorLen]
		}
		decision = dec
		// Context is stale iff a real config difference exists — a pure baseline upgrade with
		// no changes keeps the durable context measurement.
		newPathStale = len(assessment.Changes) > 0
		adoptOnAccept = len(assessment.Changes) > 0 || assessment.BaselineUpgrade
	}
	// Stand up the delegation manager BEFORE binding any loop so each restored loop's
	// agent collaboration tools are bound against a parent-scoped controller; they are attached to the
	// live session once buildRestoredSession creates it. Seed its durable request→terminal
	// index from the full stream so a wait for a wait:false request submitted before the
	// restart resolves after restore.
	// Resolve the managed-workspace placement AFTER the session lease (acquired in step 1),
	// honoring the session-lease-before-root-lease ordering on the restore path. It
	// populates probe.ws/wsRoot/wsCoordinator so the loop bind below serializes the
	// restored session's tools through the live coordinator, and re-fences an exclusive
	// root. A root-lease contention fails the restore closed.
	if probe.placementSpec.Configured() {
		r, placeErr := probe.placementSpec.resolveForNew(ctx, sessionID)
		if placeErr != nil {
			return recordErrored(&RestoreError{Kind: RestoreLeaseFailed, Cause: placeErr})
		}
		resolved = r
		withResolvedPlacement(resolved)(probe)
	}
	if topologyRequiresProcessServices(topology) {
		workspaceRoot := ""
		if resolved != nil {
			workspaceRoot = resolved.root
		}
		resources, err = resolveSessionResources(
			ctx,
			sessionID,
			probe.resourceStorageResolver,
			workspaceRoot,
			true,
		)
		if err != nil {
			return recordErrored(&RestoreError{Kind: RestoreLoopFailed, Cause: err})
		}
		withSessionResources(resources)(probe)
	}

	var manager *delegationManager
	if probe.runtimeCatalogProvider != nil {
		manager = newDelegationManagerWithCatalogProvider(topology, probe.runtimeCatalogProvider)
	} else if probe.hasRuntimeCatalog {
		manager = newDelegationManager(topology, probe.runtimeCatalog)
	} else {
		manager = newDelegationManager(topology)
	}
	contextDisposition := func(bound loop.BoundDefinition) (bool, error) {
		if newPath {
			// The NEW-path decision (above) already assessed drift; context staleness reuses
			// that result so the fold never re-derives it.
			return newPathStale, nil
		}
		if probe.frozenFingerprint != nil {
			return frozenContextStale, nil
		}
		return restoredContextDisposition(persisted, probe.projectFingerprint(bound), allowMismatch)
	}
	// On the NEW path the AgentName difference is decided by the decider (folded into the
	// assessment above), never hard-failed at bind time, so planLoops must not reject it —
	// it relaxes the same name check WithAllowConfigMismatch relaxes.
	planAllowMismatch := allowMismatch
	if newPath {
		planAllowMismatch = true
	}
	plans, activePlan, err := planLoops(sessionCtx, sessionID, topology, activeDefinition, roots, starts, allRecords, planAllowMismatch, contextDisposition, manager, probe.newWorkspaceBinding, resources)
	if err != nil {
		return recordErrored(err)
	}
	rootLoopID := activePlan.started.LoopID
	bound := activePlan.bound

	// Re-seed the cumulative spawn counter from the durable log so the quota SURVIVES the
	// restart: count the non-root LoopStarted events (agent spawns). `all` is the
	// full-stream replay (every loop's events, not loop-scoped), so it already carries
	// every agent LoopStarted — no extra read. Without this, `spawned` would reset to 0
	// and a restart would grant a fresh quota (a trivial cap bypass, design §16.3).
	spawnedCount := countSpawnedLoops(all)

	// The effective durable workspace pointer to materialize on resume (if any). Scanned over
	// the SAME unnarrowed discovery drain; both checkpoint and restore transitions are
	// session-scoped. Consumed at the pre-RestoreDone seam below.
	wsRef, hasWorkspacePointer := effectiveCurrentWorkspace(all)

	// Gate recovery folds the same record replay so private GatePreparedRecord payloads are
	// visible. Unsupported or payload-less open gates are durably closed below, after
	// RestoreStarted, before RestoreDone makes the restored session reachable.
	restoredGates := foldRestoredGates(allRecords)

	// (3) RestoreStarted — the FIRST restore mutation (after the lease fence).
	if err := appendRestoreEvent(ctx, j, factory, event.RestoreStarted{
		Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID}},
	}); err != nil {
		return recordErrored(&RestoreError{Kind: RestoreAppendFailed, Cause: err})
	}
	// Durable adoption (NEW path, on accept, when the configuration actually changed or the
	// baseline schema was upgraded): appended AFTER RestoreStarted and BEFORE RestoreDone
	// through the SAME lease-checked journal that appends RestoreStarted, so the session
	// never comes up under an unrecorded configuration. A failed append aborts the restore
	// through the same fail-secure exit.
	if newPath && adoptOnAccept {
		prevFingerprint := ""
		if baseline.Manifest.SchemaVersion >= 1 {
			prevFingerprint = baseline.Manifest.Fingerprint()
		}
		adopted := event.ConfigurationAdopted{
			Header:              event.Header{Coordinates: identity.Coordinates{SessionID: sessionID}},
			Epoch:               baseline.Epoch + 1,
			PreviousFingerprint: prevFingerprint,
			AdoptedFingerprint:  candidate.Fingerprint(),
			Manifest:            candidate,
			Drift:               assessment.Changes,
			Source:              decision.Source,
			Actor:               decision.Actor,
			Message:             decision.Message,
		}
		if err := appendConfigurationAdopted(ctx, j, factory, adopted); err != nil {
			return recordErrored(err)
		}
	}
	if err := appendCompactWaiterRepairs(ctx, j, factory, compactWaiterRepairs); err != nil {
		return recordErrored(&RestoreError{Kind: RestoreAppendFailed, Cause: err})
	}

	if err := appendRestoreUnavailableGates(ctx, j, factory, restoredGates.unavailable); err != nil {
		return recordErrored(&RestoreError{Kind: RestoreAppendFailed, Cause: err})
	}

	// Every loop was independently folded from its loop-scoped projection above.
	rootEvents := activePlan.events
	folded := activePlan.folded
	// Fold the root loop's mode + direct inference changes so it resumes under the
	// effective config it crashed under (last write wins, live precedence).
	activeInference := foldLoopInference(rootEvents)

	// (6) Crash-seam: an open turn (a TurnStarted with no terminal) is closed durably
	// with a TurnInterrupted carrying the open turn's id + index, so the resumed loop
	// never observes a half-open turn.
	var crashClosures []event.Event
	for _, plan := range plans {
		if !plan.folded.OpenTurn {
			continue
		}
		turnID, turnIdx := openTurnCoords(plan.events)
		closure := event.TurnInterrupted{
			Header: event.Header{Coordinates: identity.Coordinates{
				SessionID: sessionID, LoopID: plan.started.LoopID, TurnID: turnID,
			}},
			TurnIndex: turnIdx,
		}
		if err := appendRestoreEvent(ctx, j, factory, closure); err != nil {
			return recordErrored(&RestoreError{Kind: RestoreAppendFailed, Cause: err})
		}
		crashClosures = append(crashClosures, closure)
	}
	// (6b) Materialize the workspace ref selected by the latest durable transition BEFORE
	// declaring the restore done, so
	// RestoreDone is appended only if the workspace is also restored (fail closed — the
	// session never comes up on a workspace that does not match the durable pointer). It
	// uses probe.ws/probe.wsRoot because the live Session is not built yet. Skipped
	// when no workspace store is wired (a conversation-only restore —
	// the composition root opted out) or the journal carries no checkpoint or restore
	// transition: the root is left untouched in both cases. The journal-sourced
	// ref is validated through ParseRef (a trust boundary — a corrupt log fails closed) and
	// any Materialize failure routes through the SAME recordErrored exit as every other
	// restore failure.
	if probe.ws != nil && hasWorkspacePointer {
		ref, err := workspacestore.ParseRef(wsRef)
		if err != nil {
			return recordErrored(&RestoreError{Kind: RestoreMaterializeFailed, Cause: err})
		}
		if err := probe.ws.Materialize(ctx, ref, probe.wsRoot); err != nil {
			return recordErrored(&RestoreError{Kind: RestoreMaterializeFailed, Cause: err})
		}
	}

	// Build the Session, reusing sessionID + the active primer loop id (identity stable).
	// It mirrors newSession's wiring EXCEPT: no SessionStarted is published (the start
	// was recorded on the original run), and the root loop is SEEDED via NewRestored
	// rather than spawned empty. The lease Restore acquired is handed to the session as its
	// release-on-Shutdown hook (the Phase-10 composition wiring): the journal holds the
	// lease for the live lifetime, and a clean Shutdown releases it so a successor can
	// re-acquire without waiting out the TTL. We append WithLeaseRelease AFTER the caller's
	// opts so the restore owns the lease lifecycle (a caller cannot accidentally override
	// the releaser with a stale one).
	leaseOpts := append(append([]Option(nil), opts...), WithLeaseRelease(lease.Release))
	if resolved != nil {
		// Hand the restored session the coordinator + exclusive root-lease release so its
		// Shutdown releases the root lease before the session lease (LIFO), and so its loops'
		// tools serialize through the live coordinator.
		leaseOpts = append(leaseOpts, withResolvedPlacement(resolved))
	}
	if gcRunner != nil {
		// Hand the restored session its offload-GC runner (started after the hub exists,
		// below) so its Shutdown stops+joins it before SessionStopped and lease release.
		leaseOpts = append(leaseOpts, withOffloadGCRunner(gcRunner))
	}
	if resources != nil {
		leaseOpts = append(leaseOpts, withSessionResources(resources))
	}
	// Recover the foreign session id from the root loop's events. Prebound adapters
	// stamped it on LoopStarted; late-bound adapters record it with ForeignSessionBound.
	// buildRestoredSession fails closed on an empty sid for a foreign engine.
	foreignSID := findForeignSID(rootEvents)
	s, err := buildRestoredSession(sessionCtx, sessionCancel, bound, activePlan.bindings, sessionID, rootLoopID, foreignSID, spawnedCount, folded, activeInference, activePlan.notifications, restoredGates.open, j, factory, newID, now, leaseOpts...)
	if err != nil {
		if constructionCleanupOwned(err) {
			return nil, err
		}
		return recordErrored(err)
	}
	// Attach the delegation manager the loop tools were bound against, so the restored
	// session's scoped controllers can spawn + address children. Ownership survives
	// restore through the loop registry's parent links (re-seeded by attachAndActivate).
	s.topology = cloneTopology(topology)
	s.delegation = manager
	manager.attach(s)
	// (7) Post-build wiring: register every non-root restored loop, resolve + validate the
	// durable active selection, and install it as the session's active loop. Any failure cancels the
	// session context (tearing down the seeded loops) before recording a RestoreErrored.
	if err := attachAndActivate(s, all, plans, rootLoopID); err != nil {
		return abortAccepted(s, err)
	}
	// Restored children are registered after the delegation manager is attached.
	// Build the direct-child ownership index only after every live or tombstoned
	// handle has been installed, before the restored session can escape.
	s.seedDirectChildren()
	// Correlation is seeded only after every checked crash closure committed AND every
	// restored loop was attached. A child can fail during restored backend construction
	// after the initial fold (and be tombstoned by attachAndActivate), so seeding before
	// attachment would leave that child's durable requests with stale Interrupted or
	// Completed results instead of Failed.
	tombstoned := make(map[uuid.UUID]struct{})
	for _, plan := range plans {
		if plan.tombstoned || plan.runtimeMismatch != nil {
			tombstoned[plan.started.LoopID] = struct{}{}
		}
	}
	deliveryRepairs, err := persistUnresolvedDelegateDeliveryStates(ctx, j, factory, sessionID, allRecords, all, crashClosures)
	if err != nil {
		return abortAccepted(s, &RestoreError{Kind: RestoreAppendFailed, Cause: err})
	}
	all = append(all, deliveryRepairs...)
	if err := seedResolvedDelegateRecords(manager, allRecords, all, crashClosures, tombstoned); err != nil {
		return abortAccepted(s, &RestoreError{Kind: RestoreReplayFailed, Cause: err})
	}
	backgroundPlan, err := manager.planRestoredBackgroundRequests(s, allRecords, all, crashClosures)
	if err != nil {
		return abortAccepted(s, &RestoreError{Kind: RestoreReplayFailed, Cause: err})
	}
	if resources != nil {
		// Attach the checked process-service delegates BEFORE Activate: a process
		// resource's own Activate may need to durably publish a lifecycle record
		// (for example, Task 4's ProcessLifecycleLostOnRestore for a manifest it
		// cannot otherwise confirm), and it must observe the checked path on its
		// very first call, never the bridge's pre-attachment "services unavailable"
		// stub. See activateProcessServiceBridge's doc for the full contract.
		if err := s.activateProcessServiceBridge(); err != nil {
			return abortAccepted(s, &RestoreError{Kind: RestoreLoopFailed, Cause: err})
		}
		if err := resources.Activate(ctx); err != nil {
			return abortAccepted(s, &RestoreError{Kind: RestoreLoopFailed, Cause: err})
		}
		// Harness's OWN durable trail (never Tools' on-disk manifest, and never an OS
		// process identifier — tool.ProcessLifecycleMetadata carries none) is the only
		// record the session itself can consult for "was a process running when we
		// crashed, with no way to confirm its actual fate". Any (LoopID, ProcessHandle)
		// whose last known lifecycle record never reached a terminal (Completed/Lost)
		// is marked lost here, through the SAME checked publication path Task 24B/24C
		// built (the bridge just attached above) — never a separate, unchecked write —
		// mirroring the crash-seam TurnInterrupted closure above, but for process
		// lifecycles instead of turns.
		if err := publishOrphanedProcessLifecycles(ctx, s, orphanedProcessLifecycles(all)); err != nil {
			return abortAccepted(s, &RestoreError{Kind: RestoreAppendFailed, Cause: err})
		}
	}
	// RestoreDone is the commit point: every loop is bound, crash-closed, built,
	// attached, and the active selection has been validated before this append.
	if err := appendRestoreEvent(ctx, j, factory, event.RestoreDone{
		Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID}},
	}); err != nil {
		return abortAccepted(s, &RestoreError{Kind: RestoreAppendFailed, Cause: err})
	}
	// Start the exclusive root-lease loss watcher now that the restored session owns the
	// root lease (via leaseOpts). Nil-safe for per-session/shared/no placement. Arm the
	// offload-GC runner now the hub exists (bound to hub.IsIdle).
	s.watchRootLease()
	s.startOffloadGC()
	manager.reconcileRestoredBackgroundRequests(s, backgroundPlan)
	contextTransferred = true
	return s, nil
}

func runRestoreFailureCleanup(timeout time.Duration, appendErrored func(context.Context), release func()) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer cancel()
		appendErrored(ctx)
		release()
	}()
	select {
	case <-done:
	case <-ctx.Done():
		// The sole cleanup owner goroutine retains raw ownership while a
		// context-ignoring append remains inside the journal.
	}
}

// loopPlan is a single durable loop staged for restore: its root LoopStarted, its bound
// definition, its loop-scoped events, and the fold of those events (committed msgs +
// turnIndex + open-turn flag). planLoops builds one per durable loop; the restore
// constructor crash-closes, seeds, and attaches from these.
type loopPlan struct {
	started    event.LoopStarted
	definition loop.Definition
	bound      loop.BoundDefinition
	// bindings is the EXACT tool.Bindings this loop was bound with. It is retained so a
	// later external-toolset replacement builds its tools with the same capabilities (and
	// the same WorkspaceObservations instance) the declared tools got — rebuilding a fresh
	// binding would hand external tools a separate observation set and break TOCTOU
	// tracking across the toolset.
	bindings        tool.Bindings
	events          []event.Event
	folded          foldResult
	runtimeMismatch *RestoreRuntimeMismatchError
	tombstoned      bool
	// notifications are this loop's undelivered process completion
	// notifications (Task 24C), computed by planLoops from allRecords and
	// this plan's own events (see undeliveredProcessNotifications).
	notifications []tool.ProcessCompletionNotification
}

// discoverRoots scans the full UNNARROWED replay for every LoopStarted, collecting the
// ordered `starts` and the root (zero-Cause) LoopStarted per AgentName. For a single-primer
// topology it maps the sole configured primer onto the first root (name-agnostic
// compatibility), and — under WithAllowConfigMismatch — recovers a single primer whose name
// does not match the persisted root. It fails closed if the stream carries no loop
// (RestoreNoPrimerLoop) or a configured primer has no matching root in a topology that
// cannot be recovered, returning a typed error the caller records as a RestoreErrored.
func discoverRoots(all []event.Event, topology Topology, allowMismatch bool) (map[identity.AgentName]event.LoopStarted, []event.LoopStarted, error) {
	roots := make(map[identity.AgentName]event.LoopStarted)
	starts := make([]event.LoopStarted, 0)
	for _, ev := range all {
		if started, ok := ev.(event.LoopStarted); ok {
			starts = append(starts, started)
			if started.Cause.Coordinates == (identity.Coordinates{}) {
				if _, duplicate := roots[started.AgentName]; !duplicate {
					roots[started.AgentName] = started
				}
			}
		}
	}
	if len(topology.Primers) == 1 {
		for _, started := range starts {
			if started.Cause.Coordinates == (identity.Coordinates{}) {
				roots[topology.Primers[0]] = started
				break
			}
		}
	}
	if len(starts) == 0 {
		return nil, nil, &RestoreDiscoveryError{Kind: RestoreNoPrimerLoop}
	}
	for _, primer := range topology.Primers {
		if _, ok := roots[primer]; !ok {
			if allowMismatch && len(topology.Primers) == 1 {
				for _, started := range starts {
					if started.Cause.Coordinates == (identity.Coordinates{}) {
						roots[primer] = started
						break
					}
				}
				if _, recovered := roots[primer]; recovered {
					continue
				}
			}
			return nil, nil, &RestoreError{Kind: RestoreLoopFailed, Cause: &AgentNameMismatchError{Persisted: "", Configured: primer}}
		}
	}
	return roots, starts, nil
}

// planLoops binds and independently folds every durable loop into a loopPlan (in stream
// order) and identifies the active primer's plan. For a single-definition topology it maps
// the configured root onto the sole definition and skips non-root loops whose AgentName is
// unknown (children of a single-definition run). It is the single Bind of each loop,
// performed inside the restore lease. It returns the ordered plans and the active plan, or a
// typed error the caller records as a RestoreErrored.
func planLoops(sessionCtx context.Context, sessionID uuid.UUID, topology Topology, activeDefinition loop.Definition, roots map[identity.AgentName]event.LoopStarted, starts []event.LoopStarted, allRecords []journal.JournalRecord, allowMismatch bool, contextDisposition func(loop.BoundDefinition) (bool, error), manager *delegationManager, wsBind func() *tool.WorkspaceBinding, resources *sessionResources) ([]loopPlan, loopPlan, error) {
	plans := make([]loopPlan, 0, len(starts))
	activeIndex := -1
	for _, started := range starts {
		definition, ok := topology.definition(started.AgentName)
		isConfiguredRoot := started.LoopID == roots[topology.ActivePrimer].LoopID
		if !ok && isConfiguredRoot && len(topology.Definitions) == 1 {
			definition, ok = activeDefinition, true
		}
		if !ok && !isConfiguredRoot && len(topology.Definitions) == 1 {
			continue
		}
		if !ok {
			// WithAllowConfigMismatch recovery is intentionally single-primer/fail-secure: a
			// multi-definition topology hard-fails on ANY unknown loop AgentName (allowMismatch
			// only relaxes the name check for the sole root above via checkAgentName — it never
			// invents a missing definition here).
			return nil, loopPlan{}, &RestoreError{Kind: RestoreLoopFailed, Cause: &AgentNameMismatchError{Persisted: started.AgentName}}
		}
		processBinding, processErr := processBindingFor(definition, resources)
		if processErr != nil {
			return nil, loopPlan{}, &RestoreError{Kind: RestoreLoopFailed, Cause: processErr}
		}
		bindings := tool.Bindings{SessionID: sessionID, LoopID: started.LoopID, Workspace: wsBind(), Delegate: manager.controllerFor(started.LoopID, definition), Process: processBinding, ExtraTools: delegateExtraTools(definition, manager)}
		bound, bindErr := definition.Bind(sessionCtx, bindings)
		if bindErr != nil {
			return nil, loopPlan{}, &RestoreError{Kind: RestoreLoopFailed, Cause: bindErr}
		}
		if nameErr := checkAgentName(started.AgentName, bound.Name(), allowMismatch); nameErr != nil {
			return nil, loopPlan{}, nameErr
		}
		// A restored child agent keeps its OWN definition's access gate: authority
		// differences between loops are expressed by the consumer configuring
		// different gates per definition (or OverrideBoundAccess at a binding
		// seam), never by harness-side attenuation against the parent.
		loopEvents := eventsFromRecords(allRecords, started.LoopID)
		plans = append(plans, loopPlan{started: started, definition: definition, bound: bound, bindings: bindings, events: loopEvents})
		if started.LoopID == roots[topology.ActivePrimer].LoopID {
			activeIndex = len(plans) - 1
		}
	}
	if activeIndex < 0 {
		return nil, loopPlan{}, &RestoreError{Kind: RestoreLoopFailed, Cause: &SessionError{Kind: SessionLoopNotFound}}
	}
	discardContext, dispositionErr := contextDisposition(plans[activeIndex].bound)
	if dispositionErr != nil {
		return nil, loopPlan{}, dispositionErr
	}
	for i := range plans {
		folded, foldErr := foldLoopForRestore(plans[i].bound, plans[i].events, discardContext)
		if foldErr != nil {
			return nil, loopPlan{}, foldErr
		}
		plans[i].folded = folded
		ri := foldLoopInference(plans[i].events)
		catalog, hasCatalog := manager.catalogFor(plans[i].definition)
		bound, runtimeErr := restoreRuntimeBinding(plans[i].started, plans[i].bound, ri, catalog, hasCatalog, findForeignSID(plans[i].events) != "")
		if runtimeErr != nil {
			var mismatch *RestoreRuntimeMismatchError
			if !errors.As(runtimeErr, &mismatch) || i == activeIndex {
				return nil, loopPlan{}, runtimeErr
			}
			plans[i].runtimeMismatch = mismatch
		} else {
			plans[i].bound = bound
		}
		plans[i].tombstoned = hasLoopRestoreTombstone(plans[i].events)
		if plans[i].tombstoned && i == activeIndex {
			return nil, loopPlan{}, &RestoreError{Kind: RestoreLoopFailed, Cause: &SessionError{Kind: SessionLoopExited}}
		}
		plans[i].notifications = undeliveredProcessNotifications(allRecords, plans[i].started.LoopID, causedCommandIDs(plans[i].events))
	}
	return plans, plans[activeIndex], nil
}

func hasLoopRestoreTombstone(events []event.Event) bool {
	for _, ev := range events {
		if _, ok := ev.(event.LoopRestoreTombstoned); ok {
			return true
		}
	}
	return false
}

// restoreRuntimeBinding re-authorizes a durable adapter selection against the
// current parent-scoped catalog before any backend constructor observes Engine.
// A journaled AgentRuntime is authoritative even when the current definition
// initially binds as native; OverrideBoundRuntimeSelection turns that view into
// the adapter view only after every durable selector and target identity matches.
func restoreRuntimeBinding(started event.LoopStarted, bound loop.BoundDefinition, ri restoredInference, catalog loop.RuntimeCatalog, hasCatalog bool, hasForeignSID bool) (loop.BoundDefinition, error) {
	if ri.AgentRuntime == nil {
		if bound.Engine() != loop.EngineNative || hasForeignSID {
			return nil, &RestoreRuntimeMismatchError{Kind: RestoreRuntimeMissing}
		}
		return bound, nil
	}
	if !hasCatalog {
		return nil, &RestoreRuntimeMismatchError{Kind: RestoreRuntimeUnavailable}
	}
	runtime := ri.AgentRuntime
	harnessManaged := runtime.SelectionKind == string(loop.RuntimeSelectionHarnessManaged)
	if harnessManaged && ri.HasRuntime {
		return nil, &RestoreRuntimeMismatchError{Kind: RestoreRuntimeTargetMismatch}
	}
	if !ri.HasRuntime && !harnessManaged {
		return nil, &RestoreRuntimeMismatchError{Kind: RestoreRuntimeTargetMismatch}
	}
	if harnessManaged {
		if runtime.CredentialMode != string(loop.CredentialNativeAuth) {
			return nil, &RestoreRuntimeMismatchError{Kind: RestoreRuntimeCredentialMismatch}
		}
		if runtime.Source != string(loop.RuntimeSourceNative) || runtime.ModelAlias != "" || runtime.SmallModelAlias != "" {
			return nil, &RestoreRuntimeMismatchError{Kind: RestoreRuntimeUnavailable}
		}
		resolved, err := catalog.ResolveTargetAliasWithSource(
			started.AgentName,
			loop.AgentHarnessName(runtime.Harness),
			loop.RuntimeSourceName(runtime.Source),
			"",
			inferencemodel.EffortNone,
		)
		if err != nil || resolved.Profile != loop.RuntimeProfileName(runtime.Profile) || resolved.AgentHarness != loop.AgentHarnessName(runtime.Harness) || resolved.Source != loop.RuntimeSourceNative || resolved.SelectionKind != loop.RuntimeSelectionHarnessManaged || resolved.Credential != loop.CredentialNativeAuth {
			return nil, &RestoreRuntimeMismatchError{Kind: RestoreRuntimeUnavailable, Cause: err}
		}
		bound, err = loop.OverrideBoundRuntimeManaged(bound, resolved.Profile)
		if err != nil {
			return nil, &RestoreRuntimeMismatchError{Kind: RestoreRuntimeUnavailable}
		}
		bound, err = loop.OverrideBoundRuntimeCatalog(bound, catalog)
		if err != nil {
			return nil, &RestoreRuntimeMismatchError{Kind: RestoreRuntimeUnavailable}
		}
		return bound, nil
	}
	resolved, err := catalog.ResolveTargetAliasWithSource(
		started.AgentName,
		loop.AgentHarnessName(runtime.Harness),
		loop.RuntimeSourceName(runtime.Source),
		loop.ModelAlias(runtime.ModelAlias),
		ri.Runtime.Effort,
	)
	if err != nil {
		return nil, &RestoreRuntimeMismatchError{Kind: RestoreRuntimeUnavailable, Cause: err}
	}
	if resolved.Profile != loop.RuntimeProfileName(runtime.Profile) ||
		resolved.AgentHarness != loop.AgentHarnessName(runtime.Harness) ||
		resolved.SmallModel != loop.ModelAlias(runtime.SmallModelAlias) {
		return nil, &RestoreRuntimeMismatchError{Kind: RestoreRuntimeUnavailable}
	}
	if string(resolved.Credential) != runtime.CredentialMode {
		return nil, &RestoreRuntimeMismatchError{Kind: RestoreRuntimeCredentialMismatch}
	}
	if runtime.Source != "" && string(resolved.Source) != runtime.Source {
		return nil, &RestoreRuntimeMismatchError{Kind: RestoreRuntimeCredentialMismatch}
	}
	if runtime.SelectionKind != "" && string(resolved.SelectionKind) != runtime.SelectionKind {
		return nil, &RestoreRuntimeMismatchError{Kind: RestoreRuntimeUnavailable}
	}
	if resolved.Target.Key() != ri.Runtime.Key {
		return nil, &RestoreRuntimeMismatchError{Kind: RestoreRuntimeTargetMismatch}
	}
	if resolved.Effort != ri.Runtime.Effort {
		return nil, &RestoreRuntimeMismatchError{Kind: RestoreRuntimeEffortMismatch}
	}
	runtimeAlias := resolved.TargetAlias
	if runtimeAlias == "" {
		runtimeAlias = resolved.ModelAlias
	}
	bound, err = loop.OverrideBoundRuntimeSelectionWithIdentity(bound, resolved.Profile, runtimeAlias, resolved.Target, resolved.Effort, resolved.Source, resolved.SelectionKind)
	if err != nil {
		return nil, &RestoreRuntimeMismatchError{Kind: RestoreRuntimeUnavailable}
	}
	bound, err = loop.OverrideBoundRuntimeCatalog(bound, catalog)
	if err != nil {
		return nil, &RestoreRuntimeMismatchError{Kind: RestoreRuntimeUnavailable}
	}
	return bound, nil
}

// attachAndActivate registers every non-root restored loop, resolves the durable active
// selection (the last ActiveLoopChanged, else the initially active root), validates it is
// registered, and sets it as the session's active loop under loopsMu — the post-build wiring performed after the
// root loop is seeded and before RestoreDone. It returns a typed *RestoreError on any
// failure (an attach failure, an unregistered active target, or a latched persistence
// fault); the caller cancels the session context and records a RestoreErrored. On success
// s.activeLoopID reflects the durable active loop.
func attachAndActivate(s *Session, all []event.Event, plans []loopPlan, rootLoopID uuid.UUID) error {
	activeID := rootLoopID
	for _, ev := range all {
		if changed, ok := ev.(event.ActiveLoopChanged); ok {
			activeID = changed.ActiveLoopID
		}
	}
	for i := range plans {
		plan := &plans[i]
		if plan.started.LoopID == rootLoopID {
			continue
		}
		parent := loop.Provenance{LoopID: plan.started.Cause.Coordinates.LoopID, TurnID: plan.started.Cause.Coordinates.TurnID, StepID: plan.started.Cause.Coordinates.StepID}
		if plan.runtimeMismatch != nil || plan.tombstoned {
			if plan.started.LoopID == activeID {
				return &RestoreError{Kind: RestoreLoopFailed, Cause: &SessionError{Kind: SessionLoopExited}}
			}
			if err := s.attachRestoredTombstonedLoop(*plan, parent); err != nil {
				return err
			}
			continue
		}
		if err := s.attachRestoredLoop(plan.started, parent, plan.bound, plan.bindings, plan.folded, foldLoopInference(plan.events), plan.notifications, findForeignSID(plan.events)); err != nil {
			mismatch, runtimeFailure := classifyRestoredChildRuntimeFailure(plan.bound, err)
			if !runtimeFailure {
				return err
			}
			if plan.started.LoopID == activeID {
				return &RestoreError{Kind: RestoreLoopFailed, Cause: &SessionError{Kind: SessionLoopExited}}
			}
			// A runtime-classified failure can arise after the preflight fold (for
			// example, an adapter session/load failure). Keep the same per-child
			// degraded-restore rule as catalog drift: preserve the folded child
			// identity, publish one bounded tombstone, and continue restoring
			// siblings. The active-loop check below still makes an active child
			// failure session-fatal.
			plan.runtimeMismatch = mismatch
			if tombstoneErr := s.attachRestoredTombstonedLoop(*plan, parent); tombstoneErr != nil {
				return tombstoneErr
			}
			plan.tombstoned = true
		}
	}
	for _, plan := range plans {
		if (plan.tombstoned || plan.runtimeMismatch != nil) && plan.started.LoopID == activeID {
			return &RestoreError{Kind: RestoreLoopFailed, Cause: &SessionError{Kind: SessionLoopExited}}
		}
	}
	if _, ok := s.Loop(activeID); !ok {
		return &RestoreError{Kind: RestoreLoopFailed, Cause: &SessionError{Kind: SessionLoopNotFound}}
	}
	s.loopsMu.Lock()
	s.activeLoopID = activeID
	if s.faulted {
		fault := s.faultErr
		s.loopsMu.Unlock()
		return &RestoreError{Kind: RestoreAppendFailed, Cause: fault}
	}
	s.loopsMu.Unlock()
	return nil
}

func classifyRestoredChildRuntimeFailure(bound loop.BoundDefinition, err error) (*RestoreRuntimeMismatchError, bool) {
	var mismatch *RestoreRuntimeMismatchError
	if errors.As(err, &mismatch) {
		return mismatch, true
	}
	var restoreErr *RestoreError
	if !errors.As(err, &restoreErr) {
		return nil, false
	}
	switch restoreErr.Kind {
	case RestoreForeignBuilderMissing, RestoreForeignSIDMissing:
		return &RestoreRuntimeMismatchError{Kind: RestoreRuntimeUnavailable}, true
	case RestoreLoopFailed:
		if bound != nil && bound.Engine() == loop.EngineAdapter && bound.RuntimeIdentity().Source == loop.RuntimeSourceNative {
			return &RestoreRuntimeMismatchError{Kind: RestoreRuntimeUnavailable}, true
		}
	default:
	}
	return nil, false
}

func (s *Session) attachRestoredTombstonedLoop(plan loopPlan, parent loop.Provenance) error {
	var tombstone event.LoopRestoreTombstoned
	if !plan.tombstoned {
		hdr, err := s.factory.Stamp(event.Header{Coordinates: identity.Coordinates{SessionID: s.sessionID, LoopID: plan.started.LoopID}})
		if err != nil {
			return &RestoreError{Kind: RestoreIDGenerationFailed, Cause: err}
		}
		tombstone = event.LoopRestoreTombstoned{Header: hdr, Category: restoreTombstoneCategory(plan.runtimeMismatch)}
	}
	_, cancel := context.WithCancel(s.sessionCtx)
	liveMode, liveModel := liveViewFor(plan.bound, foldLoopInference(plan.events))
	handle := &loopHandle{
		id: plan.started.LoopID, owner: s, bound: plan.bound, bindings: plan.bindings,
		parent: parent, cancel: cancel, liveMode: liveMode, liveModel: liveModel,
		state: tool.DelegateStatusFailed, tombstoned: true,
		agentName: plan.started.DisplayName, agentMode: loop.ModeName(plan.started.InitialMode),
		selectedHarness: restoredSelectedHarness(plan.started.AgentRuntime),
	}
	s.loopsMu.Lock()
	if existing, ok := s.loops[plan.started.LoopID]; ok && existing.tombstoned {
		s.loopsMu.Unlock()
		cancel()
		return nil
	}
	s.loops[plan.started.LoopID] = handle
	s.loopsMu.Unlock()
	if plan.tombstoned {
		return nil
	}
	// Register the failed handle before fanout. Subscribers may synchronously inspect
	// ownership/status after receiving the durable tombstone, and must never observe the
	// event before the loop is addressable.
	if err := s.PublishEventChecked(context.Background(), tombstone); err != nil {
		s.loopsMu.Lock()
		if current, ok := s.loops[plan.started.LoopID]; ok && current == handle {
			delete(s.loops, plan.started.LoopID)
		}
		s.loopsMu.Unlock()
		cancel()
		return &RestoreError{Kind: RestoreAppendFailed, Cause: err}
	}
	return nil
}

func restoreTombstoneCategory(mismatch *RestoreRuntimeMismatchError) string {
	if mismatch != nil && mismatch.Kind == RestoreRuntimeUnavailable {
		return event.LoopRestoreTombstoneRuntimeUnavailable
	}
	return event.LoopRestoreTombstoneRuntimeMismatch
}

// buildRestoredSession assembles the live Session for a successful restore: the hub
// wired with the journal-backed event appender, the shared Factory, and the session as
// the hub's FaultReporter; the journal-backed command appender; and the root loop
// seeded (NewRestored) with the folded committed state under its ORIGINAL id, idle. It
// publishes NO SessionStarted and spawns NO empty loop — both are the deliberate
// difference from New.
//
// bindings MUST be the tool.Bindings cfg was bound with (activePlan.bindings); it is
// retained on the root's handle so a later external-toolset replacement builds against the
// same capabilities the declared tools were given.
func buildRestoredSession(
	sessionCtx context.Context,
	sessionCancel context.CancelFunc,
	cfg loop.BoundDefinition,
	bindings tool.Bindings,
	sessionID, rootLoopID uuid.UUID,
	foreignSID string,
	spawnedCount int,
	folded foldResult,
	ri restoredInference,
	notifications []tool.ProcessCompletionNotification,
	restoredGates map[gate.ID]gateEntry,
	j journal.SessionJournal,
	factory *event.Factory,
	newID idGenerator,
	now event.Clock,
	opts ...Option,
) (*Session, error) {
	commandAppender, err := journal.NewJournalCommandAppenderChecked(j)
	if err != nil {
		return nil, &RestoreError{Kind: RestoreJournalFailed, Cause: err}
	}
	s := &Session{
		sessionID:                sessionID,
		sessionCtx:               sessionCtx,
		sessionCancel:            sessionCancel,
		constructionAbortTimeout: defaultConstructionAbortTimeout,
		loops:                    make(map[uuid.UUID]*loopHandle),
		newID:                    newID,
		now:                      now,
		cmdAppender:              commandAppender,
		factory:                  factory,
		// Re-seed the cumulative spawn counter from the durable non-root LoopStarted count
		// so the quota survives restart (§16.3). Set before the session is reachable, so no
		// lock is needed yet; once up, NewLoop enforces the quota against this restored base.
		spawned: spawnedCount,
		gates:   cloneGateEntries(restoredGates),
		// Gate directory: restored sessions own a journal-backed appender by default.
		// Caller opts may replace it below for same-package tests.
		gateTimers: map[gate.ID]*time.Timer{},
		// Restored gates never get an answer slot: a host-owned gate is not
		// restorable (its opener's blocked call did not survive the process), so
		// foldRestoredGates resolves it unavailable rather than reinstalling it.
		gateAnswers:         map[gate.ID]chan gate.Answer{},
		gateAppender:        nopGateAppender{},
		checkpointAdmission: newCheckpointAdmissionGate(),
	}
	var constructionHook *foreignDeliveryHook
	loopCommitted := false
	defer func() {
		if !loopCommitted && constructionHook != nil {
			s.revokeCollabLoop(constructionHook.loopID)
			s.unregisterForeignDeliveryHook(constructionHook)
		}
	}()
	// Apply the same opts the probe read (WithCommandAppender may replace the durable
	// journal adapter for a direct/test caller; WithAllowConfigMismatch is a no-op here —
	// already consumed; WithLimits sets the spawn caps the restored session enforces
	// against the re-seeded counter). A nil appender option leaves the journal adapter.
	for _, opt := range opts {
		opt(s)
	}
	abort := func(restoreErr error) (*Session, error) {
		s.abortConstructionAfter(restoreErr, func(appendCtx context.Context) {
			_ = appendRestoreEvent(appendCtx, j, factory, event.RestoreErrored{
				Header: event.Header{Coordinates: identity.Coordinates{SessionID: sessionID}},
				Err:    restoreErr,
			})
		})
		return nil, &constructionCleanupOwnedError{cause: restoreErr}
	}
	gateAppender, err := journal.NewJournalGateAppenderChecked(j)
	if err != nil {
		return abort(&RestoreError{Kind: RestoreJournalFailed, Cause: err})
	}
	s.gateAppender = gateAppender
	// Resolve the spawn-cap defaults AFTER the options, mirroring newSession, so a restore
	// (with or without WithLimits) has positive depth/quota caps from the first NewLoop.
	s.limits = s.limits.withDefaults()

	// The hub uses the journal-backed REQUIRED durable tap and the session as its
	// FaultReporter (fail-secure on a required-append failure), sharing the Factory so a
	// hub-synthesized session event is stamped from the same seam.
	appender, err := journal.NewJournalEventAppenderChecked(j)
	if err != nil {
		return abort(&RestoreError{Kind: RestoreJournalFailed, Cause: err})
	}
	hubOpts := []hub.Option{
		hub.WithAppender(appender),
		hub.WithFactory(factory),
		hub.WithFaultReporter(s),
		hub.WithCommitObserver(s.recordLoopMechanicalState),
	}
	s.hub = hub.New(sessionID, hubOpts...)
	s.gateAppender = &liveGateAppender{prepared: gateAppender, publisher: s}
	if err := s.bindSessionHustles(); err != nil {
		return abort(&RestoreError{Kind: RestoreLoopFailed, Cause: err})
	}
	if s.snapshotPolicy != nil && s.ws != nil && s.wsCoordinator != nil {
		s.checkpoints = newCheckpointController(checkpointControllerConfig{
			SessionID: sessionID, Policy: *s.snapshotPolicy, Store: s.ws, Root: s.wsRoot,
			Mode: s.wsMode, Coordinator: s.wsCoordinator, Publisher: s, Factory: s.factory,
			Idle: s.hub.IsIdle, Fault: s.latchWorkspaceCheckpointFault,
			Recover: s.recoverWorkspaceCheckpointFault, Faulted: s.faultIfFaulted,
			Admission:    s.checkpointAdmission.enterCheckpoint,
			ObserveError: s.observeBestEffortCheckpointError,
		})
	}
	if s.collabBrokerRequired(cfg) {
		if err := s.startCollabBroker(); err != nil {
			return abort(&RestoreError{Kind: RestoreLoopFailed, Cause: err})
		}
	}

	// Seed the root loop under its ORIGINAL id (identity stable), coming up idle with
	// the folded committed history + turnIndex. No empty loop is spawned and no
	// LoopStarted is published — the loop already exists in the durable record. The Engine
	// switch mirrors newLoop's: a native bound definition seeds through
	// loopruntime.NewRestored; a foreign engine reconstructs through the injected RestoredBuilder,
	// carrying the recovered foreign session id. It fails CLOSED on an empty recovered sid
	// (the foreign session could not be --resumed) or a missing builder (never silently
	// rebuild the root as a native loop). Every error path cancels the loopCtx and the
	// session backstop, exactly like the native path.
	loopCtx, cancel := context.WithCancel(sessionCtx)
	var l loop.Backend
	switch cfg.Engine() {
	case loop.EngineNative:
		var compactor loopruntime.Compactor
		compactor, err = s.compactorFor(cfg, rootLoopID)
		if err == nil {
			l, err = loopruntime.NewRestoredWithRuntime(
				loopCtx,
				sessionID,
				rootLoopID,
				loop.Provenance{},
				s,
				cfg,
				restoredStateFrom(folded, ri, notifications),
				loopruntime.RuntimeDependencies{Compactor: compactor, Hooks: s.hooks, ReviewContext: s.loopReviewContext()},
			)
		}
	default:
		agentSessionID := ri.AgentSessionID
		if agentSessionID == "" {
			agentSessionID = foreignSID
		}
		if agentSessionID == "" {
			cancel()
			restoreErr := &RestoreError{Kind: RestoreForeignSIDMissing}
			return abort(restoreErr)
		}
		// Legacy journals expose one foreign SID. Phase 6 dedicated journal wiring may
		// replace the agent-side value with a separately journaled AgentSessionID.
		seed := foreign.RestoredForeign{ForeignSID: foreignSID, AgentSessionID: agentSessionID, TurnIndex: folded.TurnIndex, Msgs: folded.Msgs}
		if s.usesServicesRestoredBuilder(cfg) {
			servicesBuilder, builderErr := s.servicesRestoredBuilder(cfg)
			if builderErr != nil {
				cancel()
				return abort(builderErr)
			}
			services, hook := s.foreignServicesForTrackedWithController(rootLoopID, bindings.Delegate)
			constructionHook = hook
			l, err = servicesBuilder(loopCtx, sessionID, rootLoopID, loop.Provenance{}, s, cfg,
				func() (uuid.UUID, error) { return newID() }, factory, seed, services)
			if err == nil && constructionHook != nil {
				err = constructionHook.brokerError()
			}
		} else {
			restoredBuilder, builderErr := s.restoredBuilder(cfg)
			if builderErr != nil {
				cancel()
				return abort(builderErr)
			}
			l, err = restoredBuilder(loopCtx, sessionID, rootLoopID, loop.Provenance{}, s, cfg,
				func() (uuid.UUID, error) { return newID() }, factory, seed)
		}
	}
	if err != nil {
		cancel()
		restoreErr := &RestoreError{Kind: RestoreLoopFailed, Cause: err}
		return abort(restoreErr)
	}
	liveMode, liveModel := liveViewFor(cfg, ri)
	// bindings is activePlan.bindings — the EXACT tool.Bindings cfg was bound with in
	// planLoops (the same WorkspaceBinding instance and this
	// loop's scoped delegate controller). Retaining it here is what lets a later
	// ReplaceExternalTools build its tools with the capabilities the declared tools got,
	// exactly as the live path (session.go) and attachRestoredLoop do for every other loop.
	// Dropping it would leave a restored root unable to build ANY external tool that
	// declares a requirement, and would hand the rest a separate observation set.
	s.loops[rootLoopID] = &loopHandle{id: rootLoopID, owner: s, bound: cfg, bindings: bindings, backend: l, parent: loop.Provenance{}, cancel: cancel, liveMode: liveMode, liveModel: liveModel, state: tool.DelegateStatusIdle, selectedHarness: restoredSelectedHarness(ri.AgentRuntime)}
	s.activeLoopID = rootLoopID
	loopCommitted = true
	return s, nil
}

func restoredSelectedHarness(runtime *event.AgentRuntime) loop.AgentHarnessName {
	if runtime == nil {
		return ""
	}
	return loop.AgentHarnessName(runtime.Harness)
}

// appendRestoreEvent stamps ev (EventID + CreatedAt) via the Factory and appends it to
// the journal as an EventRecord. A mint failure surfaces as a typed restore error; an
// append failure returns the journal's typed error unchanged (the caller wraps it). It
// is the single chokepoint for the restore-lifecycle events written before the hub
// exists.
func appendRestoreEvent(ctx context.Context, j journal.SessionJournal, factory *event.Factory, ev event.Event) error {
	stamped, err := factory.Stamp(ev.EventHeader())
	if err != nil {
		return &RestoreError{Kind: RestoreIDGenerationFailed, Cause: err}
	}
	if _, err := j.Append(ctx, journal.NewEventRecord(withRestoreHeader(ev, stamped))); err != nil {
		return err
	}
	return nil
}

// appendConfigurationAdopted stamps (fresh EventID + CreatedAt, preserving the
// session-scoped Coordinates) and validates a ConfigurationAdopted, then appends it through
// the lease-checked journal. It is the restore-lifecycle counterpart to appendRestoreEvent
// for the one event appendRestoreEvent's withRestoreHeader does not handle; a mint or
// validation failure surfaces as a typed restore error the caller records as a
// RestoreErrored. A build/validation failure and a journal-append failure are named
// distinctly (RestoreAdoptionInvalid vs RestoreAppendFailed) so a caller can tell a
// malformed adoption apart from a lost lease or storage error.
func appendConfigurationAdopted(ctx context.Context, j journal.SessionJournal, factory *event.Factory, adopted event.ConfigurationAdopted) error {
	hdr, err := factory.Stamp(adopted.EventHeader())
	if err != nil {
		return &RestoreError{Kind: RestoreIDGenerationFailed, Cause: err}
	}
	adopted.Header = hdr
	if err := event.ValidateEvent(adopted); err != nil {
		return &RestoreError{Kind: RestoreAdoptionInvalid, Cause: err}
	}
	if _, err := j.Append(ctx, journal.NewEventRecord(adopted)); err != nil {
		return &RestoreError{Kind: RestoreAppendFailed, Cause: err}
	}
	return nil
}

// appendCompactWaiterRepairs durably fills terminal membership replies without
// minting new identities. The content-addressed EventID is retained; only
// CreatedAt is stamped at the trusted restore boundary.
func appendCompactWaiterRepairs(ctx context.Context, j journal.SessionJournal, factory *event.Factory, repairs []event.Event) error {
	for _, repair := range repairs {
		var stamped event.Event
		switch typed := repair.(type) {
		case event.CompactWaiterResolved:
			header, err := factory.StampCompactWaiterResolved(typed)
			if err != nil {
				return err
			}
			typed.Header = header
			stamped = typed
		case event.CompactWaiterRejected:
			header, err := factory.StampCompactWaiterRejected(typed)
			if err != nil {
				return err
			}
			typed.Header = header
			stamped = typed
		default:
			return &restoredCompactionError{Kind: restoredCompactionWaiterMismatch}
		}
		if err := event.ValidateEvent(stamped); err != nil {
			return err
		}
		if _, err := j.Append(ctx, journal.NewEventRecord(stamped)); err != nil {
			return err
		}
	}
	return nil
}

// withRestoreHeader returns a copy of a restore-lifecycle event with hdr substituted for
// its Header. The set is exhaustive over the events appendRestoreEvent stamps; an
// unexpected type panics (a programming error — no other event is appended here).
func withRestoreHeader(ev event.Event, hdr event.Header) event.Event {
	switch e := ev.(type) {
	case event.RestoreStarted:
		e.Header = hdr
		return e
	case event.RestoreDone:
		e.Header = hdr
		return e
	case event.RestoreErrored:
		e.Header = hdr
		return e
	case event.TurnInterrupted:
		e.Header = hdr
		return e
	default:
		panic("session: withRestoreHeader called on a non-restore event type")
	}
}

// processLifecycleKey identifies one supervised process across its durable
// lifecycle record sequence: the loop that owns it plus its opaque,
// non-OS-identifying handle (tool.ProcessLifecycleMetadata never carries an OS
// process identifier — see Task 4's DTO shape).
type processLifecycleKey struct {
	loopID uuid.UUID
	handle string
}

// orphanedProcessLifecycles scans the full unnarrowed restore replay (every
// loop's events) and returns the last-known tool.ProcessLifecycleMetadata for
// every (LoopID, ProcessHandle) whose durable record sequence never reached a
// terminal kind (ProcessLifecycleCompleted or ProcessLifecycleLost) — a
// process that, per Harness's OWN durable trail, was still Started,
// Backgrounded, or had only a StopRequested recorded when the session
// crashed, with no way to confirm what actually happened to it afterward.
// Order is first-seen (stream order), so a caller that publishes one
// synthesized ProcessLost per orphan does so deterministically.
//
// This is a pure fold over already-durable Harness events; it never reads an
// OS-level manifest and never touches an OS process identifier (there is none
// on the DTO to misuse).
func orphanedProcessLifecycles(events []event.Event) []tool.ProcessLifecycleMetadata {
	last := make(map[processLifecycleKey]tool.ProcessLifecycleMetadata)
	terminal := make(map[processLifecycleKey]bool)
	order := make([]processLifecycleKey, 0)
	for _, ev := range events {
		var metadata tool.ProcessLifecycleMetadata
		switch typed := ev.(type) {
		case event.ProcessStarted:
			metadata = typed.Process
		case event.ProcessBackgrounded:
			metadata = typed.Process
		case event.ProcessStopRequested:
			metadata = typed.Process
		case event.ProcessCompleted:
			metadata = typed.Process
		case event.ProcessLost:
			metadata = typed.Process
		default:
			continue
		}
		key := processLifecycleKey{loopID: metadata.LoopID, handle: metadata.ProcessHandle}
		if _, seen := last[key]; !seen {
			order = append(order, key)
		}
		last[key] = metadata
		terminal[key] = metadata.Kind == tool.ProcessLifecycleCompleted || metadata.Kind == tool.ProcessLifecycleLost
	}
	orphans := make([]tool.ProcessLifecycleMetadata, 0, len(order))
	for _, key := range order {
		if !terminal[key] {
			orphans = append(orphans, last[key])
		}
	}
	return orphans
}

// publishOrphanedProcessLifecycles durably marks every orphan (see
// orphanedProcessLifecycles) lost, through the session's already-attached
// checked process-service bridge — the SAME idempotent publication path
// Task 24B's live lifecycle events and Task 24C's completion notifications
// use, never a separate direct journal write. Each record mints a FRESH
// EventID (the orphan's original metadata already durably owns its own
// Started/Backgrounded EventID) and sets Kind/State/Reason to Task 4's
// ProcessLifecycleLost / ProcessLifecycleLostOnRestore / ProcessTerminalLostOnRestore
// triple, carrying every other field (SessionID, LoopID, ProcessHandle,
// OriginExecutionID, ProcessCreatedAt/StartedAt) forward unchanged from the
// last-known record — it never signals or queries the OS, and never invents
// an OS-identifying field the DTO does not have.
func publishOrphanedProcessLifecycles(ctx context.Context, s *Session, orphans []tool.ProcessLifecycleMetadata) error {
	for _, orphan := range orphans {
		eventID, err := s.newID()
		if err != nil {
			return &RestoreError{Kind: RestoreIDGenerationFailed, Cause: err}
		}
		lost := orphan
		lost.EventID = eventID
		lost.Kind = tool.ProcessLifecycleLost
		lost.State = tool.ProcessLifecycleLostOnRestore
		lost.Reason = tool.ProcessTerminalLostOnRestore
		lost.ProcessFinishedAt = s.now()
		lost.HasExitCode = false
		lost.ExitCode = 0
		lost.Diagnostic = ""
		if err := s.resources.processServiceBridge.PublishProcessLifecycle(ctx, lost); err != nil {
			return err
		}
	}
	return nil
}

// openTurnCoords returns the TurnID + TurnIndex of the last TurnStarted in one loop's
// events — the open (unterminated) turn when foldLoop reports OpenTurn.
// The crash-seam TurnInterrupted is stamped with these so it closes the exact turn that
// crashed. It is only called when the fold detected an open turn, so a last TurnStarted
// always exists; the zero return is the defensive fall-through.
func openTurnCoords(events []event.Event) (uuid.UUID, event.TurnIndex) {
	for i := len(events) - 1; i >= 0; i-- {
		if ts, ok := events[i].(event.TurnStarted); ok {
			return ts.TurnID, ts.TurnIndex
		}
	}
	return uuid.UUID{}, 0
}

// releaseLease releases the lease on a bounded context, best-effort. On a failed restore
// the lease must not be held (a successor must be able to re-acquire); a release failure
// is swallowed (the bucket TTL is the backstop) since the restore is already failing.
func releaseLease(lease journal.Lease) {
	rctx, cancel := context.WithTimeout(context.Background(), leaseReleaseTimeout)
	defer cancel()
	_ = lease.Release(rctx)
}
