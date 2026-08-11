package sessionruntime

import (
	"context"
	"errors"
	"reflect"
	"sync"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/foreign"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/tool"
)

// foreignDeliveryPhase is the in-memory state machine for one foreign delivery
// request. The durable command/event records remain the source of truth across
// restore; this state only prevents an actor from issuing contradictory live
// transitions through its loop-scoped capability.
type foreignDeliveryPhase uint8

const (
	foreignDeliveryAbsent foreignDeliveryPhase = iota
	foreignDeliveryIntent
	foreignDeliveryReserved
	foreignDeliveryRestored
	foreignDeliveryFallback
	foreignDeliveryInjected
	foreignDeliveryUnknown
	foreignDeliveryUntrackable
	foreignDeliveryRestoredDone
	foreignDeliveryAbandoned
)

// Tombstones preserve fail-closed duplicate handling without retaining every
// completed request for the lifetime of a session. Eviction only changes a
// duplicate into the same invalid-transition result produced for an unknown ID.
const foreignDeliveryTombstoneLimit = 256

var (
	errForeignDeliveryInvalidCoordinate  = errors.New("foreign delivery: invalid coordinate")
	errForeignDeliveryInvalidTransition  = errors.New("foreign delivery: invalid transition")
	errForeignDeliveryCommandUnavailable = errors.New("foreign delivery: command unavailable")
	errForeignDeliveryPublicationFailed  = errors.New("foreign delivery: durable publication failed")
)

// foreignDeliveryPublicationError keeps the public diagnostic fixed and
// bounded. Journal and adapter errors are not safe to expose: they may contain
// storage paths, bearer material, or provider response text.
type foreignDeliveryPublicationError struct {
}

func (e *foreignDeliveryPublicationError) Error() string {
	return errForeignDeliveryPublicationFailed.Error()
}

func (e *foreignDeliveryPublicationError) Unwrap() []error {
	return []error{errForeignDeliveryPublicationFailed}
}

func foreignDeliveryPublicationErrorFrom(_ error) error {
	return &foreignDeliveryPublicationError{}
}

// foreignDeliveryWaiter retains one request's terminal phase while the
// session-owned coordinator is handed off from actor admission. The handoff
// reference is created with the durable intent and released after the
// coordinator samples and owns the request.
type foreignDeliveryWaiter struct {
	changed chan struct{}
	phase   foreignDeliveryPhase
	refs    int
	claimed bool
}

// foreignDeliveryHook is the private session-owned implementation of the
// public foreign.DeliveryHook. Its only authority is the exact loop selected
// when it is constructed. Command payloads are registered by the session before
// actor admission; the actor receives only IDs through the public hook and can
// never obtain a session, journal, or controller from it.
type foreignDeliveryHook struct {
	session   *Session
	sessionID uuid.UUID
	loopID    uuid.UUID

	brokerMu  sync.RWMutex
	broker    foreign.BrokerDescriptor
	brokerSet bool
	brokerErr error

	mu             sync.Mutex
	commands       map[uuid.UUID]command.UserInput
	phases         map[uuid.UUID]foreignDeliveryPhase
	restored       map[uuid.UUID]bool
	folds          map[uuid.UUID]uuid.UUID
	waiters        map[uuid.UUID]*foreignDeliveryWaiter
	tombstones     map[uuid.UUID]foreignDeliveryPhase
	tombstoneOrder []uuid.UUID
	// changed is closed and replaced whenever a delivery reaches an externally
	// observable terminal phase. Session orchestration waits on this channel
	// instead of polling the actor or sleeping, so the internal admission clock
	// remains deterministic under tests and bounded in production.
	changed chan struct{}
}

var _ foreign.DeliveryHook = (*foreignDeliveryHook)(nil)

func newForeignDeliveryHook(session *Session, loopID uuid.UUID) *foreignDeliveryHook {
	hook := &foreignDeliveryHook{
		session:    session,
		commands:   make(map[uuid.UUID]command.UserInput),
		phases:     make(map[uuid.UUID]foreignDeliveryPhase),
		restored:   make(map[uuid.UUID]bool),
		folds:      make(map[uuid.UUID]uuid.UUID),
		waiters:    make(map[uuid.UUID]*foreignDeliveryWaiter),
		tombstones: make(map[uuid.UUID]foreignDeliveryPhase),
		changed:    make(chan struct{}),
		loopID:     loopID,
	}
	if session != nil {
		hook.sessionID = session.sessionID
		session.registerForeignDeliveryHook(hook)
	}
	return hook
}

func (h *foreignDeliveryHook) setBrokerDescriptor(descriptor foreign.BrokerDescriptor) {
	if h == nil {
		return
	}
	h.brokerMu.Lock()
	h.broker = descriptor
	h.brokerSet = true
	h.brokerErr = nil
	h.brokerMu.Unlock()
}

func (h *foreignDeliveryHook) setBrokerError(err error) {
	if h == nil || err == nil {
		return
	}
	h.brokerMu.Lock()
	h.brokerErr = err
	h.brokerMu.Unlock()
}

func (h *foreignDeliveryHook) brokerError() error {
	if h == nil {
		return nil
	}
	h.brokerMu.RLock()
	err := h.brokerErr
	h.brokerMu.RUnlock()
	return err
}

func (s *Session) registerForeignDeliveryHook(hook *foreignDeliveryHook) {
	if s == nil || hook == nil || hook.loopID.IsZero() {
		return
	}
	s.foreignDeliveryMu.Lock()
	if s.foreignDeliveryHooks == nil {
		s.foreignDeliveryHooks = make(map[uuid.UUID]*foreignDeliveryHook)
	}
	s.foreignDeliveryHooks[hook.loopID] = hook
	s.foreignDeliveryMu.Unlock()
	s.ensureForeignDeliveryWatcher()
}

// ensureForeignDeliveryWatcher starts one session-owned cancellation watcher.
// The watcher captures the Session rather than an individual hook, so failed
// loop construction can unregister a hook without leaving a goroutine holding
// that hook alive until session shutdown.
func (s *Session) ensureForeignDeliveryWatcher() {
	if s == nil || s.sessionCtx == nil {
		return
	}
	s.foreignDeliveryWatcherOnce.Do(func() {
		done := s.sessionCtx.Done()
		if done == nil {
			return
		}
		go func() {
			<-done
			s.abandonForeignDeliveryHooks()
		}()
	})
}

func (s *Session) unregisterForeignDeliveryHook(hook *foreignDeliveryHook) {
	if s == nil || hook == nil {
		return
	}
	s.foreignDeliveryMu.Lock()
	if current := s.foreignDeliveryHooks[hook.loopID]; current == hook {
		delete(s.foreignDeliveryHooks, hook.loopID)
	}
	s.foreignDeliveryMu.Unlock()
}

// abandonForeignDeliveryHooks synchronously clears process-local delivery
// payloads after the session context has been cancelled. The session-owned
// watcher handles direct context cancellation; Shutdown also calls this method
// so its return establishes a deterministic cleanup boundary for callers
// inspecting session-owned state.
func (s *Session) abandonForeignDeliveryHooks() {
	if s == nil {
		return
	}
	s.foreignDeliveryMu.RLock()
	hooks := make([]*foreignDeliveryHook, 0, len(s.foreignDeliveryHooks))
	for _, hook := range s.foreignDeliveryHooks {
		hooks = append(hooks, hook)
	}
	s.foreignDeliveryMu.RUnlock()
	for _, hook := range hooks {
		hook.abandonAll()
	}
}

func (s *Session) recordForeignDeliveryFold(ev event.Event) {
	var loopID, requestID uuid.UUID
	switch typed := ev.(type) {
	case event.TurnFoldedInto:
		loopID, requestID = typed.LoopID, typed.Cause.CommandID
	case event.TurnStarted:
		loopID, requestID = typed.LoopID, typed.Cause.CommandID
	case event.InputCancelled:
		loopID, requestID = typed.LoopID, typed.Cause.CommandID
	case event.TurnRejected:
		loopID, requestID = typed.LoopID, typed.Cause.CommandID
	default:
		return
	}
	if loopID.IsZero() || requestID.IsZero() {
		return
	}
	s.foreignDeliveryMu.RLock()
	hook := s.foreignDeliveryHooks[loopID]
	s.foreignDeliveryMu.RUnlock()
	if hook == nil {
		return
	}
	switch typed := ev.(type) {
	case event.TurnFoldedInto:
		hook.observeFold(typed)
	case event.TurnStarted:
		hook.observeStart(typed)
	case event.InputCancelled:
		hook.observeRestoredTerminal(typed.SessionID, loopID, requestID)
	case event.TurnRejected:
		hook.observeRestoredTerminal(typed.SessionID, loopID, requestID)
	}
}

// bindCommand installs the exact command that carries the actor payload. It is
// intentionally private: only Session creates the command and only the hook
// uses it to write the durable intent/fallback records.
func (h *foreignDeliveryHook) bindCommand(cmd command.UserInput) error {
	if h == nil || cmd.CommandID.IsZero() || cmd.TargetLoopID.IsZero() || cmd.TargetLoopID != h.loopID {
		return errForeignDeliveryInvalidCoordinate
	}
	if err := command.ValidateCommand(cmd); err != nil {
		return err
	}
	// Snapshot the durable command representation so later actor-side mutation
	// of a shared Blocks slice cannot alter the fallback payload. Accepted is
	// restored below because it is process-local and intentionally not durable.
	body, err := command.MarshalCommand(cmd)
	if err != nil {
		return err
	}
	decoded, err := command.UnmarshalCommand(body)
	if err != nil {
		return err
	}
	bound, ok := decoded.(command.UserInput)
	if !ok {
		return errForeignDeliveryInvalidTransition
	}
	bound.Accepted = cmd.Accepted
	h.mu.Lock()
	defer h.mu.Unlock()
	// A terminal/fallback callback has already consumed the durable payload.
	// Do not let a late restore bind or callback recreate an unbounded command
	// entry for a request whose phase is no longer admitting delivery.
	switch h.phaseLocked(cmd.CommandID) {
	case foreignDeliveryFallback, foreignDeliveryInjected, foreignDeliveryUnknown, foreignDeliveryUntrackable, foreignDeliveryRestoredDone, foreignDeliveryAbandoned:
		return errForeignDeliveryInvalidTransition
	}
	if prior, exists := h.commands[cmd.CommandID]; exists {
		// Accepted is a process-local admission channel recreated for every
		// restore dispatch. It is deliberately outside the durable command
		// identity, just like the journal fingerprint, so a rebound command can
		// be idempotent across restore attempts without weakening payload checks.
		prior.Accepted = nil
		candidate := bound
		candidate.Accepted = nil
		if !reflect.DeepEqual(prior, candidate) {
			return errForeignDeliveryInvalidTransition
		}
	}
	h.commands[cmd.CommandID] = bound
	return nil
}

// observeRestoredCommand binds a command being replayed by Restore and marks it
// separately from the live intent/reservation protocol. Its payload is released
// at the first authoritative actor boundary (TurnStarted/TurnFoldedInto).
func (h *foreignDeliveryHook) observeRestoredCommand(cmd command.UserInput) error {
	if err := h.bindCommand(cmd); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	phase := h.phaseLocked(cmd.CommandID)
	if phase != foreignDeliveryAbsent && !h.restored[cmd.CommandID] {
		return errForeignDeliveryInvalidTransition
	}
	// Restore ownership is orthogonal to the durable delivery phase. A rebound
	// actor continues through intent/reservation/fallback, while the marker
	// allows authoritative lifecycle evidence to release local payload state.
	h.phases[cmd.CommandID] = foreignDeliveryIntent
	h.restored[cmd.CommandID] = true
	return nil
}

func (h *foreignDeliveryHook) phaseLocked(requestID uuid.UUID) foreignDeliveryPhase {
	if waiter, ok := h.waiters[requestID]; ok && waiter.phase != foreignDeliveryAbsent {
		return waiter.phase
	}
	if phase, ok := h.phases[requestID]; ok {
		return phase
	}
	if phase, ok := h.tombstones[requestID]; ok {
		return phase
	}
	return foreignDeliveryAbsent
}

func (h *foreignDeliveryHook) finishLocked(requestID uuid.UUID, phase foreignDeliveryPhase) {
	delete(h.commands, requestID)
	delete(h.folds, requestID)
	delete(h.phases, requestID)
	delete(h.restored, requestID)
	if waiter, ok := h.waiters[requestID]; ok {
		if waiter.phase == foreignDeliveryAbsent {
			waiter.phase = phase
			if waiter.changed != nil {
				close(waiter.changed)
			}
		}
		if waiter.refs == 0 {
			delete(h.waiters, requestID)
		}
	}
	if _, exists := h.tombstones[requestID]; !exists {
		h.tombstoneOrder = append(h.tombstoneOrder, requestID)
	}
	h.tombstones[requestID] = phase
	for len(h.tombstoneOrder) > foreignDeliveryTombstoneLimit {
		oldest := h.tombstoneOrder[0]
		h.tombstoneOrder = h.tombstoneOrder[1:]
		delete(h.tombstones, oldest)
	}
	if h.changed != nil {
		close(h.changed)
		h.changed = make(chan struct{})
	}
}

// registerDeliveryWaiter installs the admission handoff reference for one
// live request. Session admission calls this after CreateIntent and before the
// command enters the actor, so a terminal phase cannot be evicted while
// coordinator construction is delayed. Keeping this separate from CreateIntent
// avoids retaining waiter state for restore-only or direct hook transitions.
func (h *foreignDeliveryHook) registerDeliveryWaiter(requestID uuid.UUID) bool {
	if h == nil || requestID.IsZero() {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.phaseLocked(requestID) == foreignDeliveryAbsent {
		return false
	}
	if _, exists := h.waiters[requestID]; exists {
		return false
	}
	h.waiters[requestID] = &foreignDeliveryWaiter{changed: make(chan struct{}), refs: 1}
	return true
}

// claimDeliveryWaiter transfers the admission handoff reference to the one
// session-owned coordinator for this request. The handoff reference is held
// until coordinator cleanup acknowledges the terminal result.
func (h *foreignDeliveryHook) claimDeliveryWaiter(requestID uuid.UUID) bool {
	if h == nil || requestID.IsZero() {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	waiter, ok := h.waiters[requestID]
	if !ok {
		return false
	}
	if waiter.claimed {
		return false
	}
	waiter.claimed = true
	return true
}

func (h *foreignDeliveryHook) releaseDeliveryWaiterLocked(requestID uuid.UUID) {
	waiter, ok := h.waiters[requestID]
	if !ok || waiter.refs == 0 {
		return
	}
	waiter.refs--
	if waiter.refs == 0 && waiter.phase != foreignDeliveryAbsent {
		delete(h.waiters, requestID)
	}
}

// deliveryStatus returns the public disposition once the actor has committed a
// terminal delivery transition. Intermediate intent/reservation phases are
// deliberately reported as pending so callers cannot mistake an actor receipt
// for provider delivery.
func (h *foreignDeliveryHook) deliveryStatus(requestID uuid.UUID) tool.DelegateDeliveryStatus {
	if h == nil || requestID.IsZero() {
		return ""
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return foreignDeliveryStatusForPhase(h.phaseLocked(requestID))
}

// deliveryWaitState returns the current concrete disposition together with the
// generation channel that is replaced whenever the hook reaches a terminal
// phase. The pair is sampled under one lock so an observer cannot miss a
// transition between reading the status and subscribing to the notification.
func (h *foreignDeliveryHook) deliveryWaitState(requestID uuid.UUID) (tool.DelegateDeliveryStatus, <-chan struct{}) {
	if h == nil || requestID.IsZero() {
		return "", nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	changed := h.changed
	if waiter, ok := h.waiters[requestID]; ok && waiter.changed != nil {
		changed = waiter.changed
	}
	return foreignDeliveryStatusForPhase(h.phaseLocked(requestID)), changed
}

func foreignDeliveryStatusForPhase(phase foreignDeliveryPhase) tool.DelegateDeliveryStatus {
	switch phase {
	case foreignDeliveryFallback:
		return tool.DelegateDeliveryQueued
	case foreignDeliveryInjected:
		return tool.DelegateDeliveryInjected
	case foreignDeliveryUnknown:
		return tool.DelegateDeliveryUnknown
	case foreignDeliveryUntrackable:
		return tool.DelegateDeliveryUntrackable
	default:
		return ""
	}
}

// waitDeliveryStatus waits for the actor's durable delivery transition. The
// caller owns the admission deadline; this method only provides an event-driven
// wait and never re-admits or retracts the request.
func (h *foreignDeliveryHook) waitDeliveryStatus(ctx context.Context, requestID uuid.UUID) tool.DelegateDeliveryStatus {
	if h == nil {
		return ""
	}
	for {
		h.mu.Lock()
		status := foreignDeliveryStatusForPhase(h.phaseLocked(requestID))
		changed := h.changed
		if waiter, ok := h.waiters[requestID]; ok && waiter.changed != nil {
			changed = waiter.changed
		}
		h.mu.Unlock()
		if status != "" {
			return status
		}
		if changed == nil {
			return ""
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ""
		}
	}
}

// abandon drops only process-local admission state after a durable intent could
// not be handed to the backend. The intent remains in the journal for restore,
// where the normal unresolved-intent repair closes it durably as unknown.
func (h *foreignDeliveryHook) abandon(requestID uuid.UUID) {
	if h == nil || requestID.IsZero() {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.abandonLocked(requestID)
}

// abandonAll releases every process-local delivery payload owned by this hook.
// It is invoked by the session context cancellation watcher to cover the gap
// before a coordinator claims its waiter as well as coordinators that exit on
// session shutdown. Durable intent/fallback records remain untouched; restore
// remains the authority for any later delivery decision.
func (h *foreignDeliveryHook) abandonAll() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	ids := make(map[uuid.UUID]struct{}, len(h.commands)+len(h.phases)+len(h.folds)+len(h.restored)+len(h.waiters))
	for requestID := range h.commands {
		ids[requestID] = struct{}{}
	}
	for requestID := range h.phases {
		ids[requestID] = struct{}{}
	}
	for requestID := range h.folds {
		ids[requestID] = struct{}{}
	}
	for requestID := range h.restored {
		ids[requestID] = struct{}{}
	}
	for requestID := range h.waiters {
		ids[requestID] = struct{}{}
	}
	for requestID := range ids {
		h.abandonLocked(requestID)
	}
}

func (h *foreignDeliveryHook) abandonLocked(requestID uuid.UUID) {
	if requestID.IsZero() {
		return
	}
	_, hasCommand := h.commands[requestID]
	_, hasPhase := h.phases[requestID]
	_, hasFold := h.folds[requestID]
	_, hasRestored := h.restored[requestID]
	waiter, hasWaiter := h.waiters[requestID]
	// A completed request may retain only a waiter reference while its
	// coordinator is unwinding. Its terminal signal was already closed by
	// finishLocked; release that reference without rewriting the tombstone.
	if !hasCommand && !hasPhase && !hasFold && !hasRestored && (!hasWaiter || waiter.phase != foreignDeliveryAbsent) {
		if hasWaiter {
			h.releaseDeliveryWaiterLocked(requestID)
		}
		return
	}
	phase := h.phaseLocked(requestID)
	if phase == foreignDeliveryAbsent {
		// A command can be bound in the tiny pre-intent window. Treat it as
		// abandoned local payload, while retaining no public delivery evidence.
		h.finishLocked(requestID, foreignDeliveryAbandoned)
		h.releaseDeliveryWaiterLocked(requestID)
		return
	}
	h.finishLocked(requestID, foreignDeliveryAbandoned)
	h.releaseDeliveryWaiterLocked(requestID)
}

func (h *foreignDeliveryHook) observeFold(ev event.TurnFoldedInto) {
	if h == nil || ev.SessionID != h.sessionID || ev.LoopID != h.loopID ||
		(!ev.Cause.LoopID.IsZero() && ev.Cause.LoopID != h.loopID) ||
		ev.Cause.CommandID.IsZero() || ev.TurnID.IsZero() {
		return
	}
	h.mu.Lock()
	phase := h.phaseLocked(ev.Cause.CommandID)
	if h.restored[ev.Cause.CommandID] && phase != foreignDeliveryReserved {
		h.finishLocked(ev.Cause.CommandID, foreignDeliveryRestoredDone)
		h.mu.Unlock()
		return
	}
	if phase != foreignDeliveryReserved {
		h.mu.Unlock()
		return
	}
	if _, exists := h.folds[ev.Cause.CommandID]; !exists {
		h.folds[ev.Cause.CommandID] = ev.TurnID
	}
	h.mu.Unlock()
}

func (h *foreignDeliveryHook) observeStart(ev event.TurnStarted) {
	if h == nil || ev.SessionID != h.sessionID || ev.LoopID != h.loopID || ev.Cause.CommandID.IsZero() {
		return
	}
	h.mu.Lock()
	if h.restored[ev.Cause.CommandID] {
		h.finishLocked(ev.Cause.CommandID, foreignDeliveryRestoredDone)
	} else if h.phaseLocked(ev.Cause.CommandID) == foreignDeliveryIntent {
		// An idle foreign actor admits the intent directly through its normal
		// TurnStarted path; no steering machine is present to call QueueFallback.
		// The opening event is authoritative queued delivery for this live request.
		// Restore remains distinct above and never steers or rewrites the command.
		h.finishLocked(ev.Cause.CommandID, foreignDeliveryFallback)
	}
	h.mu.Unlock()
}

func (h *foreignDeliveryHook) observeRestoredTerminal(sessionID, loopID, requestID uuid.UUID) {
	if h == nil || sessionID != h.sessionID || loopID != h.loopID || requestID.IsZero() {
		return
	}
	h.mu.Lock()
	if h.restored[requestID] {
		h.finishLocked(requestID, foreignDeliveryRestoredDone)
	}
	h.mu.Unlock()
}

func (h *foreignDeliveryHook) validateIntent(intent foreign.DeliveryIntent) error {
	if h == nil || h.session == nil || h.sessionID.IsZero() || h.loopID.IsZero() || intent.LoopID.IsZero() || intent.RequestID.IsZero() {
		return errForeignDeliveryInvalidCoordinate
	}
	if intent.LoopID != h.loopID {
		return &journal.CommandRouteMismatchError{RecordLoopID: intent.LoopID, TargetLoopID: h.loopID}
	}
	return nil
}

func (h *foreignDeliveryHook) validateResolution(resolution foreign.DeliveryResolution) error {
	if err := h.validateIntent(foreign.DeliveryIntent{LoopID: resolution.LoopID, RequestID: resolution.RequestID}); err != nil {
		return err
	}
	switch resolution.State {
	case foreign.DeliveryResolutionInjected:
		if resolution.TurnID.IsZero() {
			return errForeignDeliveryInvalidCoordinate
		}
	case foreign.DeliveryResolutionUnknown, foreign.DeliveryResolutionUntrackable:
		if !resolution.TurnID.IsZero() {
			return errForeignDeliveryInvalidCoordinate
		}
	default:
		return errForeignDeliveryInvalidTransition
	}
	return nil
}

// CreateIntent durably writes the exact machine command with the intent phase.
// The in-memory phase advances only after the checked command append succeeds.
func (h *foreignDeliveryHook) CreateIntent(ctx context.Context, intent foreign.DeliveryIntent) error {
	if err := h.validateIntent(intent); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.phases[intent.RequestID] == foreignDeliveryIntent && h.restored[intent.RequestID] {
		return nil
	}
	if h.phaseLocked(intent.RequestID) != foreignDeliveryAbsent {
		return errForeignDeliveryInvalidTransition
	}
	cmd, ok := h.commands[intent.RequestID]
	if !ok || cmd.CommandID != intent.RequestID || cmd.TargetLoopID != h.loopID {
		return errForeignDeliveryCommandUnavailable
	}
	cmd.DelegateDeliveryPhase = command.DelegateDeliveryPhaseIntent
	record := journal.NewCommandRecord(h.sessionID, h.loopID, cmd)
	if err := journal.ValidateCommandRecordRoute(record); err != nil {
		return err
	}
	if _, err := h.session.appendCommandResultChecked(ctx, record); err != nil {
		delete(h.commands, intent.RequestID)
		delete(h.folds, intent.RequestID)
		return foreignDeliveryPublicationErrorFrom(err)
	}
	h.commands[intent.RequestID] = cmd
	h.phases[intent.RequestID] = foreignDeliveryIntent
	return nil
}

// Reserve appends the session-scoped reservation event before a foreign actor
// can authorize a provider writer. The checked event path prevents a failed
// durable append from becoming an in-memory admission decision.
func (h *foreignDeliveryHook) Reserve(ctx context.Context, reservation foreign.DeliveryReservation) error {
	if err := h.validateIntent(foreign.DeliveryIntent(reservation)); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.phases[reservation.RequestID] != foreignDeliveryIntent {
		return errForeignDeliveryInvalidTransition
	}
	if err := h.publishStateChecked(ctx, reservation.RequestID, event.DelegateDeliverySteerAttemptReserved); err != nil {
		return err
	}
	h.phases[reservation.RequestID] = foreignDeliveryReserved
	return nil
}

// QueueFallback appends the exact command payload with the fallback phase. The
// append completes before this method returns, so the actor may safely perform
// normal admission afterward. The one command ID is reused; no second request
// identity can be minted by a retry.
func (h *foreignDeliveryHook) QueueFallback(ctx context.Context, fallback foreign.DeliveryFallback) error {
	if err := h.validateIntent(foreign.DeliveryIntent(fallback)); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	phase := h.phases[fallback.RequestID]
	if phase != foreignDeliveryIntent && phase != foreignDeliveryReserved {
		return errForeignDeliveryInvalidTransition
	}
	cmd, ok := h.commands[fallback.RequestID]
	if !ok || cmd.CommandID != fallback.RequestID || cmd.TargetLoopID != h.loopID {
		return errForeignDeliveryCommandUnavailable
	}
	cmd.DelegateDeliveryPhase = command.DelegateDeliveryPhaseFallbackQueued
	record := journal.NewCommandRecord(h.sessionID, h.loopID, cmd)
	if err := journal.ValidateCommandRecordRoute(record); err != nil {
		return err
	}
	if _, err := h.session.appendCommandResultChecked(ctx, record); err != nil {
		return foreignDeliveryPublicationErrorFrom(err)
	}
	h.finishLocked(fallback.RequestID, foreignDeliveryFallback)
	return nil
}

// Resolve commits terminal foreign delivery evidence. Injected is represented
// by the already-committed authoritative TurnFoldedInto; only ambiguous states
// append a DelegateDeliveryStateChanged event.
func (h *foreignDeliveryHook) Resolve(ctx context.Context, resolution foreign.DeliveryResolution) error {
	if err := h.validateResolution(resolution); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.phaseLocked(resolution.RequestID) != foreignDeliveryReserved {
		return errForeignDeliveryInvalidTransition
	}
	switch resolution.State {
	case foreign.DeliveryResolutionInjected:
		if turnID, ok := h.folds[resolution.RequestID]; !ok || turnID != resolution.TurnID {
			return errForeignDeliveryInvalidTransition
		}
		h.finishLocked(resolution.RequestID, foreignDeliveryInjected)
		return nil
	case foreign.DeliveryResolutionUnknown:
		if err := h.publishStateChecked(ctx, resolution.RequestID, event.DelegateDeliveryResolvedUnknown); err != nil {
			return err
		}
		h.finishLocked(resolution.RequestID, foreignDeliveryUnknown)
		return nil
	case foreign.DeliveryResolutionUntrackable:
		if err := h.publishStateChecked(ctx, resolution.RequestID, event.DelegateDeliveryResolvedUntrackable); err != nil {
			return err
		}
		h.finishLocked(resolution.RequestID, foreignDeliveryUntrackable)
		return nil
	default:
		return errForeignDeliveryInvalidTransition
	}
}

func (h *foreignDeliveryHook) publishStateChecked(ctx context.Context, requestID uuid.UUID, state event.DelegateDeliveryState) error {
	if h == nil || h.session == nil || h.session.factory == nil || h.session.hub == nil {
		return errForeignDeliveryPublicationFailed
	}
	stateEvent := event.DelegateDeliveryStateChanged{
		Header:       event.Header{Coordinates: identity.Coordinates{SessionID: h.sessionID}},
		RequestID:    requestID,
		TargetLoopID: h.loopID,
		State:        state,
	}
	header, err := h.session.factory.Stamp(stateEvent.EventHeader())
	if err != nil {
		return foreignDeliveryPublicationErrorFrom(err)
	}
	stateEvent.Header = header
	if err := event.ValidateEvent(stateEvent); err != nil {
		return foreignDeliveryPublicationErrorFrom(err)
	}
	if err := h.session.PublishEventChecked(ctx, stateEvent); err != nil {
		return foreignDeliveryPublicationErrorFrom(err)
	}
	return nil
}
