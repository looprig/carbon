package sessionruntime

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/foreign"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/harness/pkg/workspacestore"
)

// commandAppender is the session's narrow durable-write seam for the INTENT LOG:
// append one command (the session dispatched to a loop) to the session's durable
// journal. The session depends only on this one method (Interface Segregation) —
// never on the full SessionJournal, its stream management, or the record codec. The
// composition root (Phase 10) wires a real adapter over SessionJournal
// (journal.JournalCommandAppender); the default is the nop appender so existing tests
// and headless/no-persistence mode are unchanged.
//
// Ordinary interactive/control commands use this seam as AUDIT-ONLY: append errors are
// logged and dispatch proceeds. Machine NoFold delegate requests are the deliberate
// exception: their intent record is restore state, so append failure is propagated and
// dispatch is refused. The same narrow seam supports both policies at their call sites.
type commandAppender interface {
	AppendCommand(ctx context.Context, rec journal.CommandRecord) error
}

// nopCommandAppender is the default appender wired into a session built without an
// injected one. It persists nothing and never fails, so the audit-only append path is
// a pure no-op in no-persistence mode — every command is dispatched exactly as before
// the intent log landed. Headless runs and existing tests use this.
type nopCommandAppender struct{}

func (nopCommandAppender) AppendCommand(context.Context, journal.CommandRecord) error { return nil }

// Option configures an optional session dependency at construction. The bare
// New(ctx, cfg) installs the nop command appender; an Option overrides it. This mirrors
// the hub's Option pattern so the composition root injects the durable intent-log
// appender (Phase 10) without New growing a positional parameter.
type Option func(*Session)

// RuntimeCatalogProvider returns the immutable runtime catalog visible to one
// parent definition. Returning false preserves the native/no-choice surface
// for that parent. A provider is composition-root policy and must not derive
// entries from model-controlled request data.
type RuntimeCatalogProvider func(parent loop.Definition) (loop.RuntimeCatalog, bool)

// WithCommandAppender injects the intent-log appender (the composition
// root's adapter over SessionJournal). A nil appender is ignored (the nop default stays
// installed) so a caller can never accidentally null out the field and nil-deref the
// dispatch path.
func WithCommandAppender(a commandAppender) Option {
	return func(s *Session) {
		if a != nil {
			s.cmdAppender = a
		}
	}
}

// WithSessionID injects an externally-minted sessionID for New to adopt instead of
// minting its own. It resolves the journal chicken-and-egg: the durable journal needs
// the sessionID (to bind the per-session stream and write the opening LeaseFence)
// BEFORE the session exists, so the composition root mints the id first, builds the
// journal/lease/appenders from it, then hands the SAME id to New here. A zero id is
// ignored (New mints one) so a wiring slip can never produce a zero-id session. Restore
// takes the sessionID positionally and ignores this option.
func WithSessionID(id uuid.UUID) Option {
	return func(s *Session) {
		if !id.IsZero() {
			s.injectedSessionID = id
		}
	}
}

// WithEventAppender injects the hub's REQUIRED durable event tap (the composition
// root's adapter over SessionJournal — journal.JournalEventAppender). New forwards it
// into the hub (hub.WithAppender) so every Enduring event is durably appended before
// fan-out (fail-secure: an append failure faults the session). A nil appender is ignored
// (the hub's nop default stays installed) so a caller can never null out the tap and
// silently persist nothing. This is the event-side counterpart to WithCommandAppender
// (the audit-only intent log).
func WithEventAppender(a eventAppender) Option {
	return func(s *Session) {
		if a != nil {
			s.injectedEventAppender = a
		}
	}
}

// WithLeaseRelease installs the single-writer-lease release hook the session calls ONCE
// at the end of Shutdown (after the loops have drained, so the journal's last append is
// durable before ownership is relinquished). The composition root passes lease.Release
// for a NEW session; Restore installs it from the lease it acquired, so both paths free
// ownership on a clean exit and a successor can re-acquire without waiting out the TTL. A
// nil hook is ignored (headless mode stays a no-op). It takes a context so the release I/O
// is bounded by Shutdown's ctx.
func WithLeaseRelease(release func(context.Context) error) Option {
	return func(s *Session) {
		if release != nil {
			s.leaseRelease = release
		}
	}
}

// WithLimits sets the in-session agent-spawn safety caps (depth + quota) NewLoop
// enforces. A zero (or negative) field in the supplied Limits adopts the package default
// (Depth 3 / Quota 64) when newSession applies withDefaults, so a caller can never disable
// a cap with a missing or bad value. Without this option a session uses the defaults. It
// applies on both New and Restore (the restore path re-seeds the spawn counter from the
// durable log, then enforces these caps against it).
func WithLimits(l Limits) Option {
	return func(s *Session) {
		s.limits = l
	}
}

// WithAllowConfigMismatch is the restore-only opt-in to resume a session whose
// persisted config fingerprint no longer matches the live config (a different model,
// system prompt, or tool policy). Restore is fail-secure by DEFAULT — a mismatch
// rejects with *ConfigMismatchError so a conversation never silently resumes under
// behavior it never ran with — so this option exists for an operator who knowingly
// accepts the drift. New ignores it (only Restore checks fingerprints).
func WithAllowConfigMismatch() Option {
	return func(s *Session) {
		s.allowConfigMismatch = true
		// The deprecated shim also installs the accept-all decider so a manifest-carrying
		// caller that opts into mismatch accepts Warn drift through the NEW drift-assessed
		// path too (the legacy path still reads the bool). A later WithRestoreDecider on the
		// same session overrides this.
		s.restoreDecider = AcceptAllDecider{}
	}
}

// WithRestoreDecider installs the restore-only application policy that answers a
// configuration-drift assessment (the successor seam to WithAllowConfigMismatch).
// A nil decider is ignored so a wiring slip cannot null the field — the session
// then keeps its fail-secure DefaultPolicyDecider{} default. New ignores the
// decider (only Restore assesses drift); a later task consumes it in the restore path.
func WithRestoreDecider(decider RestoreDecider) Option {
	return func(s *Session) {
		if decider != nil {
			s.restoreDecider = decider
		}
	}
}

// WithInterruptReleasePolicy installs the pluggable admission-barrier release policy (the
// Dependency-Inversion seam of the interrupt machinery). After an interrupt cancels a running
// turn, the session holds the interrupt-pending marks until the policy's AwaitRelease returns,
// then clears them. Without this option the session uses the default (sessionIdleRelease):
// release once the session next reaches idle (SessionIdle durably appended). Task 16 injects a
// workspace-aware policy that may hold the barrier through a checkpoint. A nil policy is ignored
// (the default stays installed). See interrupt.go.
func WithInterruptReleasePolicy(p InterruptReleasePolicy) Option {
	return func(s *Session) {
		if p != nil {
			s.interruptRelease = p
		}
	}
}

// withOffloadGCRunner installs the session's offload-blob GC runner (built at the
// composition root over the session lease + the journal-admission gate). A nil runner is
// ignored so a wiring slip cannot null the field. The session STARTS it after the hub
// exists and STOPS it first on Shutdown.
func withOffloadGCRunner(runner *offloadGCRunner) Option {
	return func(s *Session) {
		if runner != nil {
			s.offloadGC = runner
		}
	}
}

// withOffloadGCPolicy carries the offload-GC cadence into the restore path so
// restoreTopologySession can build the runner from the lease it acquires. NewSession builds
// the runner in the Lifecycle and uses withOffloadGCRunner directly. An unconfigured policy
// is ignored.
func withOffloadGCPolicy(policy OffloadGCPolicy) Option {
	return func(s *Session) {
		if policy.Configured() {
			s.offloadGCPolicy = policy
		}
	}
}

// FingerprintProvider projects a bound loop into the immutable behavior fingerprint
// used for both SessionStarted and restore validation. It must be deterministic and safe
// for concurrent calls from separate sessions.
type FingerprintProvider func(loop.BoundDefinition) event.ConfigFingerprint

// WithFingerprintProvider installs the composition root's immutable projection.
func WithFingerprintProvider(provider FingerprintProvider) Option {
	return func(s *Session) {
		s.fingerprint = provider
	}
}

// WithFingerprint installs a definition-time frozen compatibility fingerprint. Unlike
// a provider, it is available before any loop definition is bound during restore.
func WithFingerprint(fingerprint event.ConfigFingerprint) Option {
	return func(s *Session) {
		copy := fingerprint
		s.frozenFingerprint = &copy
	}
}

// WithManifest installs the rig-assembled ConfigManifest counterpart to the frozen
// fingerprint. The session stamps it onto the construction-time SessionStarted's
// additive Manifest field.
func WithManifest(manifest event.ConfigManifest) Option {
	return func(s *Session) {
		copy := manifest
		s.frozenManifest = &copy
	}
}

// WithForeignBuilders wires the composition-root seam that constructs foreign-engine
// loops (live + restored). Without it, a foreign-engine definition fails closed at newLoop
// (SessionForeignBuilderMissing) and at restore (RestoreForeignBuilderMissing) — a
// foreign engine never silently resolves to a native loop. The two seams travel
// together (a live build and a restored build of the same agent), so they are wired as
// one option; either being nil leaves foreign engines unsupported for that path.
func WithForeignBuilders(b foreign.Builder, rb foreign.RestoredBuilder) Option {
	return func(s *Session) {
		s.foreignBuild = b
		s.foreignBuildRestored = rb
	}
}

// WithForeignServicesBuilders wires the additive foreign-engine seam. Runtime
// supplies a fresh Services snapshot for the exact session+loop construction:
// the broker descriptor is minted for that origin loop and Delivery remains a
// loop-scoped hook. Legacy builders remain on the zero-services adapter.
func WithForeignServicesBuilders(b foreign.ServicesBuilder, rb foreign.ServicesRestoredBuilder) Option {
	return func(s *Session) {
		s.foreignBuildServices = b
		s.foreignBuildRestoredServices = rb
	}
}

// WithForeignBuilderRegistry injects profile-keyed foreign construction while
// retaining the legacy function-pair seam for EngineForeignClaude/Codex.
func WithForeignBuilderRegistry(registry *foreign.BuilderRegistry) Option {
	return func(s *Session) { s.foreignRegistry = registry }
}

// WithRuntimeCatalog installs one immutable parent-scoped catalog snapshot. The
// same value feeds agent schema/preparation and controller revalidation.
func WithRuntimeCatalog(catalog loop.RuntimeCatalog) Option {
	return func(s *Session) {
		s.runtimeCatalog = catalog
		s.hasRuntimeCatalog = true
	}
}

// WithRuntimeCatalogProvider installs parent-specific catalog snapshots. It
// takes precedence over WithRuntimeCatalog when both are supplied.
func WithRuntimeCatalogProvider(provider RuntimeCatalogProvider) Option {
	return func(s *Session) {
		if provider != nil {
			s.runtimeCatalogProvider = provider
		}
	}
}

// WithWorkspaceCheckpointing wires the workspace snapshot store and the workspace root this
// session checkpoints. Both are required for CheckpointWorkspace; without this option the
// capability is unconfigured and CheckpointWorkspace fails closed with a typed
// *WorkspaceNotConfiguredError. The composition root decides WHEN to checkpoint (a
// quiescence point); looprig only exposes the capability. A nil store is ignored (the
// default unconfigured state stays), so a wiring slip can never install a store the
// capability would nil-deref on.
func WithWorkspaceCheckpointing(ws *workspacestore.Store, root string) Option {
	return func(s *Session) {
		if ws != nil {
			s.ws = ws
			s.wsRoot = root
		}
	}
}

// WithSnapshotPolicy carries the already-validated rig policy into one session.
// It is meaningful only with a managed placement; rig enforces that pairing.
func WithSnapshotPolicy(policy SnapshotPolicy) Option {
	resolved := policy.internal()
	return func(s *Session) { s.snapshotPolicy = &resolved }
}

// withResolvedPlacement installs a resolved managed-workspace placement: the workspace
// store + root, the ONE session-scoped mutation coordinator every loop's tools serialize
// through, the exclusive root-lease release hook (nil for non-leased modes), and the
// lease-loss channel the session watches to fault on ownership loss. It is the internal
// composition seam the Lifecycle populates after acquiring the root lease and materializing
// any seed. A nil argument is ignored (the no-placement default stays). This is the ONLY
// path that populates the coordinator, so a session without a placement leaves
// tool.Bindings.Workspace nil at every bind site.
func withResolvedPlacement(r *resolvedPlacement) Option {
	return func(s *Session) {
		if r == nil || r.coordinator == nil {
			return
		}
		s.ws = r.store
		s.wsRoot = r.root
		s.wsMode = r.mode
		s.wsCoordinator = r.coordinator
		s.wsRootRelease = r.rootRelease
		s.wsLeaseLost = r.leaseLost
	}
}

// withPlacementSpec carries the UNRESOLVED managed-workspace placement into RestoreTopology,
// which resolves it per-session after acquiring the session lease (so the exclusive root
// lease is acquired AFTER the session lease on the restore path too). NewSession resolves
// its placement in the Lifecycle instead (seeding must materialize before construction), so
// it uses withResolvedPlacement directly. A zero/unconfigured spec is ignored.
func withPlacementSpec(p WorkspacePlacement) Option {
	return func(s *Session) {
		if p.Configured() {
			s.placementSpec = p
		}
	}
}

// stampNow returns the session clock's current time, defaulting to the wall clock if
// the clock seam is unset (a struct-literal test session). The session stamps this onto
// every dispatched command's Header.CreatedAt at the dispatch boundary, so a journaled
// intent-log record carries its creation time minted from the SAME seam as the
// session's events.
func (s *Session) stampNow() time.Time {
	if s.now == nil {
		return time.Now()
	}
	return s.now()
}

// appendCommand is the session's DRY, AUDIT-ONLY intent-log write, called at every
// command-dispatch site BEFORE the command is sent to the loop. It wraps cmd in a
// journal.CommandRecord targeting (sessionID, loopID) — the dispatch target the command
// itself may not carry (Interrupt/Shutdown route per-loop) — and appends it.
//
// On a non-nil append error it LOGS LOUDLY and RETURNS (the caller proceeds with the
// dispatch): losing a command record must never block the user's action or fault the
// session. This is the single deliberate proceed-on-failure persistence path. The
// appender is nil-guarded (a struct-literal test session leaves it unset) so the path
// is a safe no-op in no-persistence mode.
func (s *Session) appendCommand(ctx context.Context, loopID uuid.UUID, cmd command.Command) {
	s.appendCommandWithPolicy(ctx, loopID, cmd, false)
}

// appendDelegateCommand is the load-bearing delegate request intent append. Unlike
// ordinary interactive audit records, failure prevents dispatch so restore can classify
// every accepted machine NoFold request deterministically.
func (s *Session) appendDelegateCommand(ctx context.Context, loopID uuid.UUID, cmd command.UserInput) error {
	if s.cmdAppender == nil {
		return nil
	}
	record := journal.NewCommandRecord(s.sessionID, loopID, cmd)
	if err := journal.ValidateCommandRecordRoute(record); err != nil {
		return &SessionError{Kind: SessionDelegateIntentAppendFailed, Cause: err}
	}
	if err := s.cmdAppender.AppendCommand(ctx, record); err != nil {
		return &SessionError{Kind: SessionDelegateIntentAppendFailed, Cause: err}
	}
	return nil
}

// appendShutdownCommand is the shutdown-path intent-log write. It is identical to
// appendCommand except that a TYPED lease-lost append failure is EXPECTED: Shutdown releases
// the single-writer lease as part of teardown (or the heartbeat already observed the loss),
// so a final shutdown-command append refused for lease loss is benign — not a fault. That
// one path is logged at debug; every OTHER append failure (and an ordinary, non-shutdown
// lease loss) still logs loudly at error. It does not change dispatch semantics.
func (s *Session) appendShutdownCommand(ctx context.Context, loopID uuid.UUID, cmd command.Command) {
	s.appendCommandWithPolicy(ctx, loopID, cmd, true)
}

// appendCommandWithPolicy performs the audit-only intent-log append and applies the
// failure-log policy: a lease-lost failure is downgraded to debug only when
// leaseLostExpected (the shutdown path); otherwise any failure logs loudly. It always
// proceeds — the append is never load-bearing for dispatch.
func (s *Session) appendCommandWithPolicy(ctx context.Context, loopID uuid.UUID, cmd command.Command, leaseLostExpected bool) {
	app := s.cmdAppender
	if app == nil {
		return
	}
	rec := journal.NewCommandRecord(s.sessionID, loopID, cmd)
	err := app.AppendCommand(ctx, rec)
	if err == nil {
		return
	}

	if leaseLostExpected && isJournalLeaseLost(err) {
		// Expected on a clean shutdown: ownership is relinquished during teardown, so a final
		// shutdown-command append refused for lease loss is benign. Log at debug, not error —
		// this is the incident's false-alarm path.
		slog.DebugContext(ctx, "session: shutdown intent-log append skipped after lease loss (expected)",
			"session", s.sessionID,
			"loop", loopID,
			"command_id", cmd.CommandHeader().CommandID,
			"err", err,
		)
		return
	}

	// Audit-only: log loudly and proceed. Never block the dispatch, never fault the
	// session — a lost intent-log record is recoverable; a blocked user action is not.
	slog.ErrorContext(ctx, "session: intent-log command append failed (audit-only, proceeding)",
		"session", s.sessionID,
		"loop", loopID,
		"command_id", cmd.CommandHeader().CommandID,
		"err", err,
	)
}

// isJournalLeaseLost reports whether err is (or wraps) a *journal.JournalLeaseLostError —
// the typed "append refused because ownership was lost" signal.
func isJournalLeaseLost(err error) bool {
	var leaseLost *journal.JournalLeaseLostError
	return errors.As(err, &leaseLost)
}

// commandAppenderResult is the OPTIONAL extension of commandAppender an
// injected appender may additionally satisfy to report whether ITS OWN
// durable append produced a NEW frame (Appended=true) or deduplicated an
// already-durable retry (Appended=false — the underlying durable journal
// recognized the command's idempotency id, its CommandID, as already
// indexed). It mirrors pkg/hub's eventAppenderResult exactly, one Task 24C
// process-notification delivery earlier used the SAME 24A idempotency
// primitive on the event side for. The plain commandAppender surface is
// never weakened (Interface Segregation): an appender written before this
// extension existed, or one that simply does not implement it, keeps
// satisfying commandAppender and is treated as if every successful append is
// new — exactly its pre-extension behavior.
type commandAppenderResult interface {
	commandAppender
	AppendCommandResult(ctx context.Context, rec journal.CommandRecord) (journal.AppendResult, error)
}

// appendCommandResultChecked is the process-notification path's STRICT append:
// unlike appendCommand/appendCommandWithPolicy (audit-only: log and proceed),
// a non-nil error here is returned to the caller unchanged — "Command append
// failure remains explicit for this process-notification path" — including a
// typed *journal.IdempotencyCollisionError when rec's CommandID already names
// a durable record with a DIFFERENT persisted payload. It reports whether
// THIS call durably persisted a NEW frame (true) or deduplicated an
// already-durable retry (false); a nil appender (no-persistence/headless
// mode) always reports a new frame, mirroring nopEventAppender's contract.
func (s *Session) appendCommandResultChecked(ctx context.Context, rec journal.CommandRecord) (bool, error) {
	app := s.cmdAppender
	if app == nil {
		return true, nil
	}
	if r, ok := app.(commandAppenderResult); ok {
		result, err := r.AppendCommandResult(ctx, rec)
		if err != nil {
			return false, err
		}
		return result.Appended, nil
	}
	if err := app.AppendCommand(ctx, rec); err != nil {
		return false, err
	}
	return true, nil
}

// boundLoopFor returns loopID's bound definition and channel handle under
// loopsMu — the two facts NotifyProcessCompletion needs (the engine, to
// refuse a foreign loop structurally, and the command sink, to dispatch).
func (s *Session) boundLoopFor(loopID uuid.UUID) (loop.BoundDefinition, loop.Backend, bool) {
	s.loopsMu.RLock()
	defer s.loopsMu.RUnlock()
	h, ok := s.loops[loopID]
	if !ok {
		return nil, nil, false
	}
	return h.bound, h.backend, true
}

// ProcessNotificationOwnerMismatchError reports that a Tools-supplied
// ProcessCompletionNotification named a session other than the one this
// Session was constructed for. A process resource is bound to exactly one
// session for its whole lifetime, so a notification naming a different
// session can only be a bug or a forged call — never a legitimate delivery —
// and is rejected fail-secure before it ever reaches the durable journal or a
// loop's command sink.
type ProcessNotificationOwnerMismatchError struct {
	Want uuid.UUID
	Got  uuid.UUID
}

func (e *ProcessNotificationOwnerMismatchError) Error() string {
	return "session: process notification session " + e.Got.String() +
		" does not match owning session " + e.Want.String()
}

// ProcessNotificationUnsupportedError reports that a process completion
// notification was addressed to a loop whose bound definition is not
// EngineNative. Foreign engines never receive process notifications — they
// have no backend arm to deliver one to, so accepting it would either block
// forever or silently drop it; this refuses structurally, on the ENGINE,
// mirroring loopHandle.ReplaceExternalTools' identical foreign-engine guard
// (see loop_tools.go) rather than testing bindings or backend shape.
type ProcessNotificationUnsupportedError struct {
	LoopID uuid.UUID
	Engine loop.Engine
}

func (e *ProcessNotificationUnsupportedError) Error() string {
	return "session: process notifications unsupported on foreign-engine loop " + e.LoopID.String()
}

// ProcessNotificationDeliveryStoppedError reports that a durably-appended (or
// deduplicated) ProcessNotification could not be delivered to its owning
// loop right now: the loop's bounded live notification set is full, or the
// loop exited concurrently with dispatch. The durable command remains
// authoritative — the caller (Tools' supervisor) retries dispatch later with
// the SAME CommandID; no re-append happens on that retry (24A's idempotency
// index already recognizes the id).
type ProcessNotificationDeliveryStoppedError struct {
	LoopID uuid.UUID
}

func (e *ProcessNotificationDeliveryStoppedError) Error() string {
	return "session: process notification delivery stopped for loop " + e.LoopID.String()
}

// NotifyProcessCompletion is Task 24C's real implementation of
// tool.ProcessCompletionNotifier, attached behind the session-owned
// sessionProcessServiceBridge exactly like 24B's checked lifecycle publisher
// (see process_services.go's attachProcessCompletionNotifier). It reports
// only the boundary contract's plain error (nil on ACCEPTED or DUPLICATE,
// the durable append's typed collision on COLLISION, and a typed reason on
// STOPPED); notifyProcessCompletion below carries the richer
// command.ProcessNotificationResult a caller inspecting the disposition
// (tests, and a future retry policy) needs.
func (s *Session) NotifyProcessCompletion(ctx context.Context, n tool.ProcessCompletionNotification) error {
	_, err := s.notifyProcessCompletion(ctx, n)
	return err
}

// notifyProcessCompletion appends the durable completion command FIRST using
// n's stable, pre-persisted CommandID (Harness installs it directly onto the
// command's Header — it never mints a replacement), then — only after a
// successful or deduplicated append — dispatches it to n's owning loop and
// returns whatever disposition the loop's own live de-dup guard reports.
// Command append failure is explicit here (unlike the audit-only
// appendCommand path every other command uses): a non-collision append error
// is returned unchanged and nothing is dispatched.
func (s *Session) notifyProcessCompletion(ctx context.Context, n tool.ProcessCompletionNotification) (command.ProcessNotificationResult, error) {
	if err := n.Validate(); err != nil {
		return 0, err
	}
	if n.SessionID != s.sessionID {
		return 0, &ProcessNotificationOwnerMismatchError{Want: s.sessionID, Got: n.SessionID}
	}
	bound, backend, ok := s.boundLoopFor(n.LoopID)
	if !ok {
		return command.ProcessNotificationStopped, &SessionError{Kind: SessionLoopNotFound}
	}
	if bound == nil {
		return command.ProcessNotificationStopped, &ProcessNotificationUnsupportedError{LoopID: n.LoopID}
	}
	if engine := bound.Engine(); engine != loop.EngineNative {
		return command.ProcessNotificationStopped, &ProcessNotificationUnsupportedError{LoopID: n.LoopID, Engine: engine}
	}

	result := make(chan command.ProcessNotificationResult, 1)
	// Header.CreatedAt is deliberately left zero (omitzero drops it — see
	// command.Header's doc) rather than stamped from s.stampNow(), UNLIKE every
	// other dispatched command. This path uses appendCommandResultChecked, whose
	// dedup is fingerprint-based over the full marshaled command bytes (24A:
	// journal.NewFingerprint hashes (kind, body)). A genuine retry of the same
	// notification — the supervisor redispatching with the SAME CommandID after a
	// Stopped disposition — carries an identical n but would otherwise be stamped
	// microseconds apart, producing two DIFFERENT fingerprints for the same
	// CommandID and misclassifying the retry as a collision instead of a
	// deduplicated duplicate. Nothing downstream reads ProcessNotification's
	// Header.CreatedAt (validateProcessNotification only checks Notification;
	// handleProcessNotification and undeliveredProcessNotifications only read
	// c.Notification/notification.Notification), so fixing its value to zero is
	// safe: it makes two calls carrying the same n byte-identical, which is
	// exactly what the strict checked-append path requires to dedup a real retry
	// while still failing closed on a genuine collision (a different Notification
	// reusing the same CommandID still produces a different fingerprint).
	cmd := command.ProcessNotification{
		Header:       command.Header{CommandID: n.CommandID, Agency: identity.AgencyMachine},
		Notification: n,
		Result:       result,
	}
	rec := journal.NewCommandRecord(s.sessionID, n.LoopID, cmd)
	if err := journal.ValidateCommandRecordRoute(rec); err != nil {
		// Defense in depth: cmd is built from n's already-validated fields
		// directly above, so this should never fire in production, but a
		// process-notification record — like a machine delegate request —
		// never reaches the durable log unvalidated.
		return 0, err
	}
	_, err := s.appendCommandResultChecked(ctx, rec)
	if err != nil {
		var collision *journal.IdempotencyCollisionError
		if errors.As(err, &collision) {
			return command.ProcessNotificationCollision, err
		}
		return 0, err
	}

	select {
	case backend.CommandSink() <- cmd:
	case <-backend.DoneChan():
		return command.ProcessNotificationStopped, &ProcessNotificationDeliveryStoppedError{LoopID: n.LoopID}
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	select {
	case disposition := <-result:
		if disposition == command.ProcessNotificationStopped {
			return disposition, &ProcessNotificationDeliveryStoppedError{LoopID: n.LoopID}
		}
		return disposition, nil
	case <-backend.DoneChan():
		return command.ProcessNotificationStopped, &ProcessNotificationDeliveryStoppedError{LoopID: n.LoopID}
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

var _ tool.ProcessCompletionNotifier = (*Session)(nil)
