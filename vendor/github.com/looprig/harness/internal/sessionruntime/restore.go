package sessionruntime

import (
	"sort"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/internal/loopruntime"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	model "github.com/looprig/inference/model"
)

type restoredHustleAuditErrorKind string

const (
	restoredHustleAuditDuplicateStart       restoredHustleAuditErrorKind = "duplicate_start"
	restoredHustleAuditTerminalWithoutStart restoredHustleAuditErrorKind = "terminal_without_start"
	restoredHustleAuditAttributionMismatch  restoredHustleAuditErrorKind = "attribution_mismatch"
	restoredHustleAuditInvalidStart         restoredHustleAuditErrorKind = "invalid_start"
	restoredHustleAuditCountOverflow        restoredHustleAuditErrorKind = "count_overflow"
)

type restoredHustleAuditError struct {
	Kind  restoredHustleAuditErrorKind
	RunID hustle.RunID
}

func (e *restoredHustleAuditError) Error() string {
	return "sessionruntime: invalid restored hustle audit: " + string(e.Kind)
}

// restoredHustleInterrupted is a bounded diagnostic classification. The
// unmatched HustleStarted remains the durable audit record; restore neither
// appends a synthetic terminal nor reconstructs runtime work.
type restoredHustleInterrupted struct {
	Name          hustle.Name
	ModelSource   hustle.ModelSource
	NamedModelKey model.ModelKey
	Runs          uint64
}

type restoredHustleAudit struct {
	Interrupted []restoredHustleInterrupted
}

type restoredHustleStart struct {
	descriptor hustle.DefinitionDescriptor
	sessionID  uuid.UUID
	cause      identity.Cause
}

// foldRestoredHustleAudit validates exact one-start/one-terminal lifecycle
// attribution by RunID. Only unmatched starts survive as canonically sorted
// counts keyed by immutable definition identity; no per-run restore state is
// returned.
func foldRestoredHustleAudit(events []event.Event) (restoredHustleAudit, error) {
	starts := make(map[hustle.RunID]restoredHustleStart)
	seen := make(map[hustle.RunID]struct{})
	for _, ev := range events {
		switch typed := ev.(type) {
		case event.HustleStarted:
			if typed.Visibility() != event.Internal || typed.SessionID.IsZero() || uuid.UUID(typed.Run.RunID).IsZero() || typed.Run.Runtime != (event.ModelRuntime{}) || typed.Run.Definition.Validate() != nil {
				return restoredHustleAudit{}, &restoredHustleAuditError{Kind: restoredHustleAuditInvalidStart, RunID: typed.Run.RunID}
			}
			if _, exists := seen[typed.Run.RunID]; exists {
				return restoredHustleAudit{}, &restoredHustleAuditError{Kind: restoredHustleAuditDuplicateStart, RunID: typed.Run.RunID}
			}
			seen[typed.Run.RunID] = struct{}{}
			starts[typed.Run.RunID] = restoredHustleStart{descriptor: typed.Run.Definition, sessionID: typed.SessionID, cause: typed.Cause}
		case event.HustleCompleted:
			if err := consumeRestoredHustleTerminal(starts, typed.Run, typed.SessionID, typed.Cause); err != nil {
				return restoredHustleAudit{}, err
			}
		case event.HustleFailed:
			if err := consumeRestoredHustleTerminal(starts, typed.Run, typed.SessionID, typed.Cause); err != nil {
				return restoredHustleAudit{}, err
			}
		}
	}
	return projectRestoredHustleInterruptions(starts)
}

func projectRestoredHustleInterruptions(starts map[hustle.RunID]restoredHustleStart) (restoredHustleAudit, error) {
	type interruptedKey struct {
		name          hustle.Name
		modelSource   hustle.ModelSource
		namedModelKey model.ModelKey
	}
	counts := make(map[interruptedKey]uint64)
	for _, start := range starts {
		key := interruptedKey{name: start.descriptor.Name, modelSource: start.descriptor.ModelSource}
		if start.descriptor.ModelSource == hustle.ModelSourceNamed {
			key.namedModelKey = start.descriptor.NamedModelKey
		}
		var err error
		counts[key], err = incrementRestoredHustleCount(counts[key])
		if err != nil {
			return restoredHustleAudit{}, err
		}
	}
	interrupted := make([]restoredHustleInterrupted, 0, len(counts))
	for key, runs := range counts {
		interrupted = append(interrupted, restoredHustleInterrupted{Name: key.name, ModelSource: key.modelSource, NamedModelKey: key.namedModelKey, Runs: runs})
	}
	sort.Slice(interrupted, func(i, j int) bool {
		left, right := interrupted[i], interrupted[j]
		if left.Name != right.Name {
			return left.Name < right.Name
		}
		if left.ModelSource != right.ModelSource {
			return left.ModelSource < right.ModelSource
		}
		if left.NamedModelKey.Provider != right.NamedModelKey.Provider {
			return left.NamedModelKey.Provider < right.NamedModelKey.Provider
		}
		return left.NamedModelKey.Model < right.NamedModelKey.Model
	})
	return restoredHustleAudit{Interrupted: interrupted}, nil
}

func incrementRestoredHustleCount(value uint64) (uint64, error) {
	if value == ^uint64(0) {
		return 0, &restoredHustleAuditError{Kind: restoredHustleAuditCountOverflow}
	}
	return value + 1, nil
}

func consumeRestoredHustleTerminal(starts map[hustle.RunID]restoredHustleStart, run event.HustleRunDescriptor, sessionID uuid.UUID, cause identity.Cause) error {
	start, exists := starts[run.RunID]
	if !exists {
		return &restoredHustleAuditError{Kind: restoredHustleAuditTerminalWithoutStart, RunID: run.RunID}
	}
	if start.descriptor != run.Definition || start.sessionID != sessionID || start.cause != cause {
		return &restoredHustleAuditError{Kind: restoredHustleAuditAttributionMismatch, RunID: run.RunID}
	}
	if run.Definition.ModelSource == hustle.ModelSourceNamed && run.Runtime != (event.ModelRuntime{}) && run.Runtime.Key != run.Definition.NamedModelKey {
		return &restoredHustleAuditError{Kind: restoredHustleAuditAttributionMismatch, RunID: run.RunID}
	}
	delete(starts, run.RunID)
	return nil
}

func checkFingerprint(persisted, live event.ConfigFingerprint, allowMismatch bool) error {
	if persisted.Equal(live) {
		return nil
	}
	if allowMismatch {
		return nil
	}
	return &ConfigMismatchError{Persisted: persisted, Live: live}
}

// restoredContextDisposition validates restore compatibility and reports whether
// an explicitly overridden, actual config mismatch makes a durable context
// measurement stale. Merely enabling the override does not discard matching state.
func restoredContextDisposition(persisted, live event.ConfigFingerprint, allowMismatch bool) (bool, error) {
	if err := checkFingerprint(persisted, live, allowMismatch); err != nil {
		return false, err
	}
	return !persisted.Equal(live), nil
}

// checkAgentName is the restore root-loop AgentName decision: it returns nil when the
// persisted (root LoopStarted) name and the configured (root loop.Definition) name are
// equal, a typed *AgentNameMismatchError when they differ, and — when allowMismatch is
// set — nil even on a difference (the operator's explicit opt-in, shared with the
// fingerprint override). It is fail-secure by default and treats an EMPTY persisted name
// vs a non-empty configured one as a mismatch: a legacy/pre-AgentName record is never
// silently accepted as a match (plain string inequality covers this — "" != "operator").
func checkAgentName(persisted, configured identity.AgentName, allowMismatch bool) error {
	if persisted == configured {
		return nil
	}
	if allowMismatch {
		return nil
	}
	return &AgentNameMismatchError{Persisted: persisted, Configured: configured}
}

// firstConfigFingerprint extracts the persisted config fingerprint from the FIRST
// SessionStarted in the replayed event slice (stream-sequence order). A stream with no
// SessionStarted fails closed with a typed *RestoreDiscoveryError — there is nothing to
// check the live config against, so the restore must not proceed.
func firstConfigFingerprint(events []event.Event) (event.ConfigFingerprint, error) {
	for _, ev := range events {
		if ss, ok := ev.(event.SessionStarted); ok {
			return ss.Config, nil
		}
	}
	return event.ConfigFingerprint{}, &RestoreDiscoveryError{Kind: RestoreNoSessionStarted}
}

// adoptedBaseline is the restore comparison baseline: the manifest and epoch of
// the LATEST committed SessionStarted or ConfigurationAdopted. A legacy
// SessionStarted (no manifest) yields a SchemaVersion-0 projection, which limits
// drift assessment and forces a baseline upgrade on acceptance (Task 10).
type adoptedBaseline struct {
	Manifest event.ConfigManifest
	Epoch    event.ConfigEpoch
}

// latestAdoptedBaseline scans the replayed stream for the baseline the next restore
// assesses drift against: the manifest + epoch of the LATEST committed SessionStarted
// or ConfigurationAdopted. Only the FIRST SessionStarted seeds epoch 1 (a legacy start
// without a manifest projects via ManifestFromLegacy at SchemaVersion 0); a later
// ConfigurationAdopted overrides both manifest and epoch, last-write-wins. A stream with
// no SessionStarted fails closed with a typed *RestoreDiscoveryError — the same discovery
// error firstConfigFingerprint returns.
func latestAdoptedBaseline(events []event.Event) (adoptedBaseline, error) {
	var baseline adoptedBaseline
	found := false
	for _, ev := range events {
		switch typed := ev.(type) {
		case event.SessionStarted:
			if !found { // only the first SessionStarted seeds epoch 1
				found = true
				baseline.Epoch = 1
				if typed.Manifest.SchemaVersion != 0 {
					baseline.Manifest = typed.Manifest
				} else {
					baseline.Manifest = event.ManifestFromLegacy(typed.Config)
				}
			}
		case event.ConfigurationAdopted:
			found = true
			baseline.Manifest = typed.Manifest
			baseline.Epoch = typed.Epoch
		}
	}
	if !found {
		return adoptedBaseline{}, &RestoreDiscoveryError{Kind: RestoreNoSessionStarted}
	}
	return baseline, nil
}

// findRootLoopStarted locates the session's root LoopStarted: the one whose
// Cause.Coordinates is zero (a root loop has no spawning loop/turn/step). Restore reads
// two facts off it — the root loop's stable id (the recovered loop comes up under THIS
// id, keeping session identity stable) and its immutable stamped AgentName — so the "what
// is the root loop" rule lives in one place. A stream with no root LoopStarted fails closed
// with a typed *RestoreDiscoveryError.
func findRootLoopStarted(events []event.Event) (event.LoopStarted, error) {
	for _, ev := range events {
		ls, ok := ev.(event.LoopStarted)
		if !ok {
			continue
		}
		if ls.Cause.Coordinates == (identity.Coordinates{}) {
			return ls, nil
		}
	}
	return event.LoopStarted{}, &RestoreDiscoveryError{Kind: RestoreNoPrimerLoop}
}

func findForeignSID(events []event.Event) string {
	for _, ev := range events {
		if ls, ok := ev.(event.LoopStarted); ok && ls.ForeignSID != "" {
			return ls.ForeignSID
		}
	}
	for _, ev := range events {
		if fb, ok := ev.(event.ForeignSessionBound); ok && fb.ForeignSID != "" {
			return fb.ForeignSID
		}
	}
	return ""
}

// countSpawnedLoops counts the durable NON-ROOT LoopStarted events in the replayed
// stream — those whose Header.Cause.Coordinates is non-zero (an agent spawn carries the
// spawning loop/turn/step in its Cause). It is the restore-time re-seed of the session's
// cumulative spawn counter: the live counter increments only on a successful NewLoop spawn
// (a rejected or rolled-back spawn emits NO LoopStarted, §6d), so counting the durable
// non-root LoopStarted events reproduces exactly the live `spawned` value at crash time.
// The root LoopStarted (Cause zero) is excluded — the initial loop never counts
// toward the quota — using the SAME root/non-root discriminator findRootLoopStarted and the
// live NewLoop counter use, so the restored quota matches the live one and a restart cannot
// grant a fresh budget.
//
// It scans the full-stream replay (the `all` slice restore already drains for discovery,
// which spans EVERY loop's events — it is not loop-scoped), so no extra read is needed.
func countSpawnedLoops(events []event.Event) int {
	n := 0
	for _, ev := range events {
		ls, ok := ev.(event.LoopStarted)
		if !ok {
			continue
		}
		if ls.Cause.Coordinates != (identity.Coordinates{}) {
			n++
		}
	}
	return n
}

// effectiveCurrentWorkspace returns the Ref selected by the LAST durable workspace
// transition, whether checkpoint or restore. It is scanned from the unnarrowed discovery
// drain because both event types are session-scoped. A restore therefore changes the resume
// point without erasing the independently projected LastCheckpoint history.
func effectiveCurrentWorkspace(events []event.Event) (string, bool) {
	ref, ok := "", false
	for _, ev := range events {
		switch e := ev.(type) {
		case event.WorkspaceCheckpointed:
			ref, ok = e.Ref, true
		case event.WorkspaceRestored:
			ref, ok = e.Ref, true
		}
	}
	return ref, ok
}

// restoredInference is the fold of one loop's selected mode and latest resolved
// runtime. Every lifecycle event replaces Runtime, matching the actor's live
// precedence, so restore does not consult a mutable model catalog for identity,
// limits, or effort.
type restoredInference struct {
	Mode           loop.ModeName
	HasMode        bool
	Runtime        event.ModelRuntime
	HasRuntime     bool
	AgentRuntime   *event.AgentRuntime
	AgentSessionID string
}

// foldLoopInference folds a loop's ordered lifecycle events into its durable mode
// and resolved runtime, last-write-wins. Legacy LoopStarted records without the
// additive runtime retain the live bound definition fallback.
func foldLoopInference(events []event.Event) restoredInference {
	var ri restoredInference
	var loopID uuid.UUID
	for _, ev := range events {
		switch e := ev.(type) {
		case event.LoopStarted:
			loopID = e.LoopID
			// Seed the mode BASELINE from the durable start mode: a mode-selective spawn
			// records the selected mode on LoopStarted and emits NO LoopModeChanged, so
			// without this a child spawned in a non-default mode would resume at the
			// definition's initial mode (wrong model/effort/tools/instructions). An empty
			// InitialMode means the definition default; a later LoopModeChanged overrides
			// this baseline (a mode change also resets any inference override, handled below).
			if e.InitialMode != "" {
				ri.Mode = loop.ModeName(e.InitialMode)
				ri.HasMode = true
			}
			if e.Runtime != (event.ModelRuntime{}) {
				ri.Runtime = e.Runtime
				ri.HasRuntime = true
			}
			if e.AgentRuntime != nil {
				copy := *e.AgentRuntime
				ri.AgentRuntime = &copy
				if copy.ACPSessionID != "" {
					ri.AgentSessionID = copy.ACPSessionID
				}
			}
		case event.LoopModeChanged:
			ri.Mode = loop.ModeName(e.Mode)
			ri.HasMode = true
			// A pre-runtime mode change still authoritatively selects its named
			// mode, but its model must be re-resolved from the bound definition.
			// Clearing a prior direct inference override is therefore intentional.
			ri.Runtime = e.Runtime
			ri.HasRuntime = e.Runtime != (event.ModelRuntime{})
		case event.LoopInferenceChanged:
			ri.Runtime = e.Runtime
			ri.HasRuntime = e.Runtime != (event.ModelRuntime{})
		case event.LoopAgentSessionBound:
			if loopID.IsZero() || e.LoopID == loopID {
				ri.AgentSessionID = e.ACPSessionID
			}
		}
	}
	return ri
}

// restoredStateFrom builds the loopruntime restore seed from a loop's committed-conversation
// fold, its mode/inference fold, and its undelivered process notifications (Task 24C), so a
// re-created loop comes up with its history, the effective config it crashed under, AND the
// live de-dup guard already reconciled against durable causality.
func restoredStateFrom(folded foldResult, ri restoredInference, notifications []tool.ProcessCompletionNotification) loopruntime.RestoredState {
	return loopruntime.RestoredState{
		Msgs:          folded.Msgs,
		DerivedPrefix: folded.DerivedPrefix,
		TurnIndex:     folded.TurnIndex,
		Mode:          ri.Mode,
		HasMode:       ri.HasMode,
		Runtime:       ri.Runtime,
		HasRuntime:    ri.HasRuntime,
		Context:       folded.Context,
		HasContext:    folded.HasContext,
		Basis:         folded.Basis,
		HasBasis:      folded.HasBasis,

		AutomaticBasis:    folded.AutomaticBasis,
		HasAutomaticBasis: folded.HasAutomaticBasis,

		PendingProcessNotifications: notifications,
	}
}

// causedCommandIDs folds a durable Enduring event sequence into the set of
// non-zero Header.Cause.CommandID values it carries — generically, over
// WHATEVER event a command's live processing durably emits, with no
// dependency on any specific event type. Task 24C's restore-side notification
// subtraction consumes it: a process-notification CommandID present in this
// set already caused a durable event before the crash (already consumed) and
// is therefore excluded from the redelivered remainder.
func causedCommandIDs(events []event.Event) map[uuid.UUID]struct{} {
	caused := make(map[uuid.UUID]struct{})
	for _, ev := range events {
		if id := ev.EventHeader().Cause.CommandID; !id.IsZero() {
			caused[id] = struct{}{}
		}
	}
	return caused
}

// undeliveredProcessNotifications replays loopID's durable command intent log
// (records, in ledger order — the SAME []journal.JournalRecord slice restore
// already drains for gate reconstruction) for command.ProcessNotification
// entries addressed to loopID, and subtracts those whose CommandID already
// appears in caused (see causedCommandIDs) — already consumed before the
// crash. The remainder, preserving ledger order, is what NewRestored
// re-enqueues directly into the loop's live de-dup guard. It is naturally
// bounded by however many completion commands the session's intent log
// retains; no separate cap is needed.
func undeliveredProcessNotifications(records []journal.JournalRecord, loopID uuid.UUID, caused map[uuid.UUID]struct{}) []tool.ProcessCompletionNotification {
	var pending []tool.ProcessCompletionNotification
	for _, rec := range records {
		cmdRec, ok := rec.(journal.CommandRecord)
		if !ok {
			continue
		}
		notification, ok := cmdRec.Command().(command.ProcessNotification)
		if !ok || notification.Notification.LoopID != loopID {
			continue
		}
		if _, consumed := caused[notification.Notification.CommandID]; consumed {
			continue
		}
		pending = append(pending, notification.Notification)
	}
	return pending
}

// foldResult is the reconstruction of one loop's committed conversation from its
// ordered Enduring event sequence. It is what the Restore constructor (Task 8.3)
// seeds a re-created loop with: the committed message history, the next live
// turn's index, and whether the history ends on a crash-seam (a turn that was
// started but never terminated).
//
// Msgs is the committed conversation only — it does NOT include a SystemMessage.
// The constructor re-seeds the system prompt from the live config, so the fold
// produces solely what the loop committed (TurnStarted/StepDone/TurnFoldedInto).
//
// TurnIndex is the count of TurnStarted events, which equals loopState.turnIndex
// after the last started turn — the loop increments turnIndex as it installs each
// turn, so the next live turn numbers correctly when the constructor resumes.
//
// OpenTurn reports the crash seam: true when the sequence ends with a turn that
// was started but never reached a terminal (TurnDone/TurnFailed/TurnInterrupted).
// The Task 8.3 constructor closes such a turn by synthesizing a TurnInterrupted
// before resuming, so a resumed loop never observes a half-open turn.
type foldResult struct {
	Msgs content.AgenticMessages
	// DerivedPrefix counts the leading messages in Msgs that are a
	// compaction-generated summary rather than genuine human-authored
	// conversation. It is 1 immediately after the last CompactionCommitted
	// in the folded sequence replaces Msgs with [Summary, Retained...] (where
	// Summary is a model-generated *content.UserMessage), and 0 if no compaction has ever
	// committed — see loopruntime.RestoredState.DerivedPrefix's doc comment
	// for why this must be threaded through to a restored loop.
	DerivedPrefix int
	TurnIndex     event.TurnIndex
	OpenTurn      bool
	Runtime       event.ModelRuntime
	HasRuntime    bool
	Context       event.ContextMeasurement
	HasContext    bool
	Basis         event.ContextBasis
	HasBasis      bool

	AutomaticBasis    event.ContextBasis
	HasAutomaticBasis bool
	Err               error
}

type restoredContextRevisionOverflowError struct{}

func (*restoredContextRevisionOverflowError) Error() string {
	return "sessionruntime: restored context revision overflow"
}

type restoredCompactionErrorKind string

const (
	restoredCompactionDuplicateTerminal restoredCompactionErrorKind = "duplicate_terminal"
	restoredCompactionWaiterMismatch    restoredCompactionErrorKind = "waiter_mismatch"
)

// restoredCompactionError reports contradictory durable compaction membership.
// Restore wraps it as RestoreReplayFailed before any repaired outcome is appended.
type restoredCompactionError struct {
	Kind      restoredCompactionErrorKind
	AttemptID event.CompactAttemptID
	CommandID uuid.UUID
}

func (e *restoredCompactionError) Error() string {
	return "sessionruntime: invalid restored compaction: " + string(e.Kind)
}

type restoredCompactionTerminal struct {
	event    event.Event
	attempt  event.CompactAttemptID
	waiters  []uuid.UUID
	resolved bool
}

type restoredCompactionWaiterKey struct {
	attempt event.CompactAttemptID
	command uuid.UUID
}

type restoredCompactionOutcome struct {
	key   restoredCompactionWaiterKey
	event event.Event
}

type restoredCompactionOrphanOwnership struct {
	sessionID   uuid.UUID
	loopID      uuid.UUID
	disposition event.CompactRejectReason
}

// planCompactWaiterRepairs validates canonical terminal uniqueness and returns
// only terminal members whose exact durable reply is absent. It is a pure fold:
// the authoritative replay remains untouched and in its original order.
func planCompactWaiterRepairs(events []event.Event) ([]event.Event, error) {
	terminals := make(map[event.CompactAttemptID]restoredCompactionTerminal)
	ordered := make([]restoredCompactionTerminal, 0)
	outcomes := make(map[restoredCompactionWaiterKey][]event.Event)
	outcomeOrder := make([]restoredCompactionOutcome, 0)
	orphanOwnership := make(map[event.CompactAttemptID]restoredCompactionOrphanOwnership)
	for _, ev := range events {
		switch typed := ev.(type) {
		case event.CompactionCommitted:
			if _, exists := terminals[typed.AttemptID]; exists {
				return nil, &restoredCompactionError{Kind: restoredCompactionDuplicateTerminal, AttemptID: typed.AttemptID}
			}
			terminal := restoredCompactionTerminal{
				event: typed, attempt: typed.AttemptID, waiters: typed.WaiterCommandIDs, resolved: true,
			}
			terminals[typed.AttemptID] = terminal
			ordered = append(ordered, terminal)
		case event.CompactionRejected:
			if _, exists := terminals[typed.AttemptID]; exists {
				return nil, &restoredCompactionError{Kind: restoredCompactionDuplicateTerminal, AttemptID: typed.AttemptID}
			}
			terminal := restoredCompactionTerminal{
				event: typed, attempt: typed.AttemptID, waiters: typed.WaiterCommandIDs,
			}
			terminals[typed.AttemptID] = terminal
			ordered = append(ordered, terminal)
		case event.CompactWaiterResolved:
			key := restoredCompactionWaiterKey{attempt: typed.AttemptID, command: typed.Cause.CommandID}
			outcomes[key] = append(outcomes[key], typed)
			outcomeOrder = append(outcomeOrder, restoredCompactionOutcome{key: key, event: typed})
		case event.CompactWaiterRejected:
			key := restoredCompactionWaiterKey{attempt: typed.AttemptID, command: typed.Cause.CommandID}
			outcomes[key] = append(outcomes[key], typed)
			outcomeOrder = append(outcomeOrder, restoredCompactionOutcome{key: key, event: typed})
		}
	}

	var repairs []event.Event
	for _, terminal := range ordered {
		for _, commandID := range terminal.waiters {
			key := restoredCompactionWaiterKey{attempt: terminal.attempt, command: commandID}
			existing := outcomes[key]
			if len(existing) == 0 {
				repairs = append(repairs, restoredCompactionRepair(terminal, commandID))
				continue
			}
			for _, outcome := range existing {
				if !restoredCompactionOutcomeMatches(terminal, commandID, outcome) {
					return nil, &restoredCompactionError{
						Kind: restoredCompactionWaiterMismatch, AttemptID: terminal.attempt, CommandID: commandID,
					}
				}
			}
		}
	}
	for _, outcome := range outcomeOrder {
		key := outcome.key
		terminal, hasTerminal := terminals[key.attempt]
		if !hasTerminal {
			current, seen := orphanOwnership[key.attempt]
			next, allowed := advanceRestoredCompactionOrphanOwnership(current, seen, outcome.event)
			if !allowed {
				return nil, &restoredCompactionError{
					Kind: restoredCompactionWaiterMismatch, AttemptID: key.attempt, CommandID: key.command,
				}
			}
			orphanOwnership[key.attempt] = next
			continue
		}
		if restoredCompactionHasWaiter(terminal.waiters, key.command) {
			continue
		}
		if !restoredCompactionOverflowMatches(terminal, key.command, outcome.event) {
			return nil, &restoredCompactionError{
				Kind: restoredCompactionWaiterMismatch, AttemptID: key.attempt, CommandID: key.command,
			}
		}
	}
	return repairs, nil
}

func advanceRestoredCompactionOrphanOwnership(
	current restoredCompactionOrphanOwnership,
	hasCurrent bool,
	outcome event.Event,
) (restoredCompactionOrphanOwnership, bool) {
	rejected, ok := outcome.(event.CompactWaiterRejected)
	if !ok {
		return current, false
	}
	switch rejected.Reason {
	case event.CompactRejectInterrupted, event.CompactRejectShuttingDown, event.CompactRejectControlLaneFull:
	default:
		return current, false
	}
	if !hasCurrent {
		current.sessionID = rejected.SessionID
		current.loopID = rejected.LoopID
	} else if current.sessionID != rejected.SessionID || current.loopID != rejected.LoopID {
		return current, false
	}
	if rejected.Reason == event.CompactRejectControlLaneFull {
		return current, true
	}
	if current.disposition != event.CompactRejectUnspecified && current.disposition != rejected.Reason {
		return current, false
	}
	current.disposition = rejected.Reason
	return current, true
}

func restoredCompactionHasWaiter(waiters []uuid.UUID, commandID uuid.UUID) bool {
	for _, waiter := range waiters {
		if waiter == commandID {
			return true
		}
	}
	return false
}

func restoredCompactionOverflowMatches(terminal restoredCompactionTerminal, commandID uuid.UUID, outcome event.Event) bool {
	rejected, ok := outcome.(event.CompactWaiterRejected)
	if !ok {
		return false
	}
	return rejected.AttemptID == terminal.attempt && rejected.Cause.CommandID == commandID &&
		rejected.Coordinates == terminal.event.EventHeader().Coordinates &&
		rejected.Reason == event.CompactRejectControlLaneFull &&
		rejected.EventID == event.CompactWaiterReplyID(terminal.attempt, commandID, false)
}

func restoredCompactionRepair(terminal restoredCompactionTerminal, commandID uuid.UUID) event.Event {
	header := event.Header{
		Coordinates: terminal.event.EventHeader().Coordinates,
		Cause:       identity.Cause{CommandID: commandID},
		EventID:     event.CompactWaiterReplyID(terminal.attempt, commandID, terminal.resolved),
	}
	if terminal.resolved {
		committed := terminal.event.(event.CompactionCommitted)
		return event.CompactWaiterResolved{
			Header: header, AttemptID: terminal.attempt, CommittedEventID: committed.EventID,
		}
	}
	rejected := terminal.event.(event.CompactionRejected)
	return event.CompactWaiterRejected{
		Header: header, AttemptID: terminal.attempt, Reason: rejected.RejectReason,
	}
}

func restoredCompactionOutcomeMatches(terminal restoredCompactionTerminal, commandID uuid.UUID, outcome event.Event) bool {
	header := outcome.EventHeader()
	if header.Coordinates != terminal.event.EventHeader().Coordinates || header.Cause.CommandID != commandID {
		return false
	}
	if terminal.resolved {
		resolved, ok := outcome.(event.CompactWaiterResolved)
		committed := terminal.event.(event.CompactionCommitted)
		return ok && resolved.AttemptID == terminal.attempt && resolved.CommittedEventID == committed.EventID &&
			resolved.EventID == event.CompactWaiterReplyID(terminal.attempt, commandID, true)
	}
	rejectedOutcome, ok := outcome.(event.CompactWaiterRejected)
	rejectedTerminal := terminal.event.(event.CompactionRejected)
	return ok && rejectedOutcome.AttemptID == terminal.attempt && rejectedOutcome.Reason == rejectedTerminal.RejectReason &&
		rejectedOutcome.EventID == event.CompactWaiterReplyID(terminal.attempt, commandID, false)
}

func advanceFoldedContextBasis(current event.ContextBasis, hasCurrent bool, eventID uuid.UUID) (event.ContextBasis, bool, error) {
	if eventID.IsZero() {
		return current, hasCurrent, nil
	}
	if hasCurrent && current.Revision == ^event.ContextRevision(0) {
		return event.ContextBasis{}, false, &restoredContextRevisionOverflowError{}
	}
	revision := event.ContextRevision(1)
	if hasCurrent {
		revision = current.Revision + 1
	}
	return event.ContextBasis{Revision: revision, ThroughEventID: eventID}, true, nil
}

// RestoredContextModelMismatchError reports a replay projection that would
// seed a current measurement under a different restored runtime model.
type RestoredContextModelMismatchError struct {
	Runtime     model.ModelKey
	Measurement model.ModelKey
}

func (e *RestoredContextModelMismatchError) Error() string {
	return "sessionruntime: restored context measurement model does not match runtime"
}

// foldLoop reconstructs a loop's committed msgs + turnIndex from an ordered
// slice of its Enduring events, mirroring runLoop's commit arm EXACTLY so a fold
// reproduces loopState.msgs byte-for-byte:
//
//   - TurnStarted    -> append its UserMessage (the loop commits qi.msg as the
//     turn's first message), and increment the turn counter
//     (the loop bumps turnIndex as it installs the turn).
//   - StepDone       -> append its Messages, the finalized step group in order
//     (the single AIMessage followed by its ToolResultMessages),
//     exactly the slice the loop appends at the commit point.
//   - TurnFoldedInto -> append its UserMessage at the fold point (the loop commits
//     the folded user message after that step's tool results).
//   - Everything else (the terminals TurnDone/TurnFailed/TurnInterrupted, and the
//     lifecycle/queue events LoopStarted/LoopIdle/Session*/Restore*/
//     InputQueued/InputCancelled/TurnRejected/TokenDelta) does NOT
//     contribute to msgs — the terminal's AIMessage was already
//     committed via its StepDone, and lifecycle/queue events never
//     mutate loopState.msgs.
//
// It is a PURE function: no I/O. The events are already-typed, journaled
// payloads (each TurnStarted/StepDone/TurnFoldedInto carries its committed
// message[s] verbatim), so there is no malformed-group failure mode to surface — a
// nil Message or empty Messages folds to the same nil/empty the loop itself
// committed. Cross-event runtime/context consistency is reported through Err so
// the restore constructor can reject the replay before seeding a loop.
//
// Open-turn (crash-seam) detection rides the same single pass: a TurnStarted opens
// the turn (openTurn = true) and a terminal closes it (openTurn = false). After the
// last event, openTurn is true iff the loop crashed mid-turn — a TurnStarted with no
// later terminal. The interrupted-turn contract follows directly from mirroring the
// commit arm: only StepDone groups (completed steps) and TurnFoldedInto user
// messages reach msgs, so a turn that crashed after some completed steps yields the
// committed UserMessage + those completed step groups and NO partial assistant step
// (the in-flight step never emitted a StepDone, so it never entered the slice); a
// turn that crashed before its first step yields just the committed UserMessage.
func foldLoop(events []event.Event) foldResult {
	// Start non-nil so an empty sequence reconstructs as an empty (not nil) thread,
	// matching content.AgenticMessages' documented empty zero value and the loop's
	// own freshly-seeded msgs.
	msgs := content.AgenticMessages{}
	derivedPrefix := 0
	var turnIndex event.TurnIndex
	openTurn := false
	var runtime event.ModelRuntime
	hasRuntime := false
	var contextMeasurement event.ContextMeasurement
	hasContext := false
	var basis event.ContextBasis
	hasBasis := false
	var automaticBasis event.ContextBasis
	hasAutomaticBasis := false
	var foldErr error
	advanceBasis := func(header event.Header) {
		if foldErr != nil {
			return
		}
		basis, hasBasis, foldErr = advanceFoldedContextBasis(basis, hasBasis, header.EventID)
	}

	for _, ev := range events {
		switch e := ev.(type) {
		case event.TurnStarted:
			// The loop increments turnIndex then commits the initial UserMessage. A turn
			// is now open until a terminal closes it.
			turnIndex++
			msgs = append(msgs, e.Message)
			openTurn = true
			contextMeasurement = event.ContextMeasurement{}
			hasContext = false
			advanceBasis(e.Header)
		case event.StepDone:
			// The loop appends the finalized step group (AIMessage + ToolResultMessages).
			msgs = append(msgs, e.Messages...)
			contextMeasurement = event.ContextMeasurement{}
			hasContext = false
			advanceBasis(e.Header)
		case event.TurnFoldedInto:
			// The loop commits the folded user message at the tool-continuation point.
			msgs = append(msgs, e.Message)
			contextMeasurement = event.ContextMeasurement{}
			hasContext = false
			advanceBasis(e.Header)
		case event.LoopStarted:
			runtime = e.Runtime
			hasRuntime = e.Runtime != (event.ModelRuntime{})
			contextMeasurement = event.ContextMeasurement{}
			hasContext = false
		case event.LoopInferenceChanged:
			runtime = e.Runtime
			hasRuntime = e.Runtime != (event.ModelRuntime{})
			contextMeasurement = event.ContextMeasurement{}
			hasContext = false
			advanceBasis(e.Header)
		case event.LoopModeChanged:
			runtime = e.Runtime
			hasRuntime = e.Runtime != (event.ModelRuntime{})
			contextMeasurement = event.ContextMeasurement{}
			hasContext = false
			advanceBasis(e.Header)
		case event.LoopExternalToolsetChanged:
			// The tools ride in the inference request, so replacing them invalidates any
			// cached context measurement — the live actor commits this event through the
			// same context-mutation path as a mode change. The fold MUST mirror that or a
			// restored loop's context basis would disagree with the one the live loop had.
			// Deliberately no runtime/mode change: an external replacement touches neither.
			// The toolset itself is NOT restored — external tools are live resources, so the
			// slot comes up empty and the application re-installs.
			contextMeasurement = event.ContextMeasurement{}
			hasContext = false
			advanceBasis(e.Header)
		case event.ContextMeasured:
			contextMeasurement = e.Measurement
			hasContext = true
			basis = e.Measurement.Basis
			hasBasis = true
		case event.CompactionRejected:
			if !hasBasis {
				basis = e.Basis
				hasBasis = true
			}
			if e.Reason == event.CompactionReasonAutomatic {
				automaticBasis = e.Basis
				hasAutomaticBasis = true
			}
		case event.CompactionCommitted:
			replacement := make(content.AgenticMessages, 0, 1+len(e.Retained))
			replacement = append(replacement, e.Summary)
			replacement = append(replacement, e.Retained...)
			msgs = cloneCompactionMessages(replacement)
			// e.Summary is the compaction Hustle's model-generated summary
			// (wrapped as a *content.UserMessage), not genuine human input. The
			// retained suffix follows as genuine conversation; see
			// foldResult.DerivedPrefix's doc comment.
			derivedPrefix = 1
			contextMeasurement = e.PostContext
			hasContext = true
			basis = e.PostContext.Basis
			hasBasis = true
		case event.TurnDone, event.TurnFailed, event.TurnInterrupted:
			// A terminal closes the open turn. Its AIMessage (for TurnDone) was already
			// committed via that step's StepDone, so the terminal adds nothing to msgs.
			openTurn = false
		default:
			// Lifecycle/queue/ephemeral events (LoopStarted/LoopIdle/Session*/Restore*/
			// InputQueued/InputCancelled/TurnRejected/TokenDelta) never mutate msgs and
			// never open or close a turn.
		}
	}

	if foldErr == nil && hasContext && hasRuntime && contextMeasurement.Model != runtime.Key {
		foldErr = &RestoredContextModelMismatchError{Runtime: runtime.Key, Measurement: contextMeasurement.Model}
		contextMeasurement = event.ContextMeasurement{}
		hasContext = false
	}
	return foldResult{
		Msgs: msgs, DerivedPrefix: derivedPrefix, TurnIndex: turnIndex, OpenTurn: openTurn,
		Runtime: runtime, HasRuntime: hasRuntime,
		Context: contextMeasurement, HasContext: hasContext, Err: foldErr,
		Basis: basis, HasBasis: hasBasis,
		AutomaticBasis: automaticBasis, HasAutomaticBasis: hasAutomaticBasis,
	}
}

func foldLoopForRestore(bound loop.BoundDefinition, events []event.Event, discardContext bool) (foldResult, error) {
	folded := foldLoop(events)
	if folded.Err != nil {
		return foldResult{}, &RestoreError{Kind: RestoreReplayFailed, Cause: folded.Err}
	}
	if discardContext {
		folded.Context = event.ContextMeasurement{}
		folded.HasContext = false
		return folded, nil
	}
	if folded.HasContext {
		_, effectiveModel := liveViewFor(bound, foldLoopInference(events))
		if folded.Context.Model != effectiveModel.Key() {
			folded.Err = &RestoredContextModelMismatchError{
				Runtime: effectiveModel.Key(), Measurement: folded.Context.Model,
			}
		}
	}
	if folded.Err != nil {
		return foldResult{}, &RestoreError{Kind: RestoreReplayFailed, Cause: folded.Err}
	}
	return folded, nil
}
