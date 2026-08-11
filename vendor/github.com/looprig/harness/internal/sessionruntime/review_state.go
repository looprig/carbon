package sessionruntime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"log/slog"
	"sync"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/tool"
)

// review_state.go owns the session's bounded, PURELY in-memory permission-review
// lifecycle bookkeeping: one cancellation handle per active gate review (design
// §15) and per-turn AND per-session circuit-breaker counters (design §18).
// Nothing declared here
// is ever durable and nothing here is restored — a restored *Session is built as
// a fresh struct literal (restore_constructor.go's buildRestoredSession) that
// never sets the review field, so it always starts from the zero reviewLifecycle.
// That is precisely design §22.4's "stale live cancellation handles do not
// survive restore": there is no code path by which a restored session could
// inherit a cancellation handle or breaker counter from before the restart.

// reviewCircuitBreakerLimits are the bounded, immutable per-turn thresholds
// design §18 requires. A zero limit disables that specific counter (it never
// trips on its own), so a Session that never applies withPermissionReviewBreaker
// (the default, zero reviewCircuitBreakerLimits) never trips — matching every
// other permission-review Option's "zero configuration preserves current
// behavior" rule.
type reviewCircuitBreakerLimits struct {
	// MaxConsecutiveNeedsHuman bounds back-to-back reviews whose combined
	// decision was ineligible for a reason that means "the classifier(s) ran
	// cleanly but this particular action needs a human" (design
	// ReviewDecisionRecommendation/RiskCeiling/Authorization/AbsoluteHuman/
	// MaterialTruncation).
	MaxConsecutiveNeedsHuman int
	// MaxInvalidOrFailed bounds recent reviews whose combined decision was
	// ineligible because the review mechanism itself broke (classifier
	// status, invalid assessment, invalid policy, or basis mismatch) rather
	// than because policy legitimately said "ask a human".
	MaxInvalidOrFailed int
	// MaxIdenticalSubjects bounds how many times, within one turn, review is
	// attempted against a materially identical prepared request (same tool
	// name and requirement descriptions) — a stuck retry loop.
	MaxIdenticalSubjects int
	// MaxStaleResponses bounds classifier-originated responses that arrive
	// after the gate they targeted has already moved on (respondFromClassifier's
	// drift detection).
	MaxStaleResponses int
	// InterruptOnTrip additionally interrupts the tripping turn's loop subtree
	// when the breaker trips (design §18: "consumers may configure a
	// stricter interrupt action, matching Codex Guardian's repeated-denial
	// behavior"). The default (false) only disables automatic review and
	// leaves the loop running — an open human gate already prevents rapid
	// autonomous retry.
	InterruptOnTrip bool
	// Session configures the session-scoped counterpart of the four
	// thresholds above (design §18: "per-turn AND per-session"). It is a
	// SEPARATE, independently-configured threshold set, not a shared reuse of
	// the turn-scoped fields above: the session-scoped counters never reset
	// at a turn boundary (see reviewSessionCounters), so reusing the same
	// numeric thresholds would mean any turn-scoped threshold low enough to
	// trip in a single turn (e.g. 1) also permanently trips the session on
	// that same first observation — defeating clearReviewTurnState's
	// "cleared when the turn completes" contract for the turn-scoped breaker
	// entirely. A zero Session (the default) disables every session-scoped
	// counter, exactly like a zero top-level field disables its turn-scoped
	// counterpart: a consumer that only ever configured the turn-scoped
	// fields above keeps the exact pre-Finding-2 behavior unchanged.
	Session reviewSessionCircuitBreakerLimits
}

// reviewSessionCircuitBreakerLimits are the bounded, immutable per-session
// thresholds design §18 requires, mirroring reviewCircuitBreakerLimits'
// four counter kinds (InterruptOnTrip has no session-scoped counterpart: it
// is documented, and tested, as interrupting the tripping TURN's loop
// subtree specifically — extending it to "interrupt the whole session" is a
// materially different, undocumented action outside this fix's scope).
type reviewSessionCircuitBreakerLimits struct {
	// MaxConsecutiveNeedsHuman is reviewCircuitBreakerLimits.MaxConsecutiveNeedsHuman's
	// session-scoped counterpart.
	MaxConsecutiveNeedsHuman int
	// MaxInvalidOrFailed is reviewCircuitBreakerLimits.MaxInvalidOrFailed's
	// session-scoped counterpart.
	MaxInvalidOrFailed int
	// MaxIdenticalSubjects is reviewCircuitBreakerLimits.MaxIdenticalSubjects's
	// session-scoped counterpart, bounding a materially identical gated
	// subject recurring across the WHOLE session rather than one turn.
	MaxIdenticalSubjects int
	// MaxStaleResponses is reviewCircuitBreakerLimits.MaxStaleResponses's
	// session-scoped counterpart.
	MaxStaleResponses int
}

// withPermissionReviewBreaker installs the circuit-breaker thresholds a
// consumer configures, mirroring withPermissionReview's private,
// zero-preserves-behavior shape. Wiring a public consumer-facing option (e.g.
// a future rig.WithPermissionReviewBreakerLimits) into this seam is later
// composition-root work, not this seam's.
func withPermissionReviewBreaker(limits reviewCircuitBreakerLimits) Option {
	return func(s *Session) { s.review.limits = limits }
}

// reviewBreakerOutcome is the terminal, secret-free summary
// permissionReviewAdapter.review reports for ONE completed review attempt
// (regardless of whether it reached an eligible decision), so the session's
// circuit breaker can count outcomes without ever seeing subject content —
// design §18: "they do not contain subject contents". SubjectDigest is a
// content-only digest (subjectContentDigest) deliberately independent of
// GateID/ToolExecutionID, which are unique per call and would defeat
// "materially identical" detection entirely.
type reviewBreakerOutcome struct {
	SubjectDigest [32]byte
	Reason        gate.ReviewDecisionReason
	Eligible      bool
}

// permissionReviewOutcomeObserver is the narrow, optional seam
// permissionReviewAdapter.review reports every completed review's
// circuit-breaker-relevant summary through. It is satisfied by *Session
// (observePermissionReviewOutcome) and is deliberately NOT a
// newPermissionReviewAdapter constructor parameter: it is optional
// bookkeeping whose absence must never fail adapter construction the way a
// missing runner/classifiers/policy/responder does. Session.StartPermissionReview
// assigns it directly onto the adapter after construction.
type permissionReviewOutcomeObserver interface {
	observePermissionReviewOutcome(coords identity.Coordinates, outcome reviewBreakerOutcome)
}

// reviewNeedsHumanReasons classifies a combined-decision reason as "the review
// ran cleanly but policy/the classifier legitimately wants a human" rather
// than a mechanical failure.
var reviewNeedsHumanReasons = map[gate.ReviewDecisionReason]bool{
	gate.ReviewDecisionRecommendation:     true,
	gate.ReviewDecisionRiskCeiling:        true,
	gate.ReviewDecisionAuthorization:      true,
	gate.ReviewDecisionAbsoluteHuman:      true,
	gate.ReviewDecisionMaterialTruncation: true,
}

// reviewInvalidOrFailedReasons classifies a combined-decision reason as a
// mechanical failure of the review machinery itself.
var reviewInvalidOrFailedReasons = map[gate.ReviewDecisionReason]bool{
	gate.ReviewDecisionClassifierStatus:  true,
	gate.ReviewDecisionInvalidAssessment: true,
	gate.ReviewDecisionInvalidPolicy:     true,
	gate.ReviewDecisionBasisMismatch:     true,
}

// maxTrackedReviewSubjectDigests bounds the distinct subject digests one
// turn's breaker tracks. Beyond this many DISTINCT digests, a newly seen
// digest is simply not tracked (fail-open on tracking only — it never widens
// auto-approval, it only means a very high-cardinality turn stops
// contributing new identical-subject evidence, which is an acceptable bound
// trade-off for a purely defensive, non-durable counter).
const maxTrackedReviewSubjectDigests = 64

// maxTrackedReviewTurns is a defense-in-depth cap on the number of turns the
// breaker simultaneously tracks. Per-turn state is the ordinary lifecycle
// (installed at first outcome, removed at TurnDone/TurnFailed/TurnInterrupted
// via clearReviewTurnState), so this is never expected to bind in practice;
// it exists so a turn-completion event that is somehow never observed cannot
// grow this map without bound over a very long session.
const maxTrackedReviewTurns = 1024

// reviewTurnCounters is the bounded, in-memory-only per-turn circuit-breaker
// state (design §18). It never contains subject content — only counts and a
// bounded set of content-only digests.
type reviewTurnCounters struct {
	consecutiveNeedsHuman int
	invalidOrFailed       int
	staleResponses        int
	subjectCounts         map[[32]byte]int
	tripped               bool
	warned                bool
}

// reviewSessionCounters is the session-scoped counterpart of
// reviewTurnCounters (design §18: "track per-turn AND per-session bounded
// counters"). It mirrors the exact same four counter kinds, but unlike
// reviewTurnCounters it is never cleared by clearReviewTurnState — there is
// exactly one reviewSessionCounters value for the session's entire lifetime.
// That is the whole point: a pattern of repeated failures/needs_human spread
// across many DIFFERENT turns, each individually below the per-turn
// threshold, is invisible to the turn-scoped counters (cleared at every
// TurnDone/TurnFailed/TurnInterrupted) but accumulates here.
//
// Design §18 describes threshold crossing and a per-turn clear-on-completion
// rule, but does not separately describe a reset trigger for the
// session-scoped counters. The safe, fail-closed reading is that they never
// reset — only grow, bounded — until the session itself ends (a restored
// *Session always starts a fresh, zero reviewLifecycle; see this file's
// top-of-file doc comment), matching every other unresolved-ambiguity
// default in this design: narrowing (more human review), never widening.
//
// It never contains subject content — only counts and a bounded set of
// content-only digests, exactly like reviewTurnCounters.
type reviewSessionCounters struct {
	consecutiveNeedsHuman int
	invalidOrFailed       int
	staleResponses        int
	subjectCounts         map[[32]byte]int
	tripped               bool
	warned                bool
}

// reviewLifecycle is the session's bounded, PURELY in-memory permission-review
// lifecycle state: one cancellation handle per active gate review (design §15)
// and per-turn AND per-session circuit-breaker counters (design §18). It is
// its own type —
// not folded into gateEntry — because its invariants and lifetime differ:
// gateEntry is part of the durable-adjacent gate directory (rebuilt on
// restore); reviewLifecycle is purely live and always starts zero, including
// after a restore.
type reviewLifecycle struct {
	mu sync.Mutex

	// cancellations holds ONE cancellation handle per gate with an active
	// classifier review. beginPermissionReviewCancellation installs an entry
	// before the review goroutine starts; that SAME goroutine's deferred
	// cleanup is the only place an entry is ever deleted. Every other
	// trigger (gate resolve, owner close, loop/turn interrupt, session
	// shutdown) only ever CALLS the stored cancel func — never deletes —
	// so there is exactly one writer for deletion and no ordering race
	// between two triggers over map mutation.
	cancellations map[gate.ID]context.CancelFunc

	limits reviewCircuitBreakerLimits
	turns  map[uuid.UUID]*reviewTurnCounters

	// session is the single, session-lifetime reviewSessionCounters value
	// (design §18's session-scoped counters). Lazily allocated by
	// reviewSessionCountersLocked on first observation; nil (never trips)
	// for a session that has never observed a review outcome.
	session *reviewSessionCounters
}

// beginPermissionReviewCancellation installs a fresh cancellation handle for
// gateID derived from parent and returns the context review must run under,
// plus a cleanup the caller MUST invoke exactly once when its review
// goroutine returns, regardless of outcome. Cleanup is the ONLY place an
// entry is ever removed from the cancellation map (see reviewLifecycle's doc
// comment); it also cancels the context so a caller that exits WITHOUT any
// external trigger having cancelled it still releases the derived context's
// resources.
func (s *Session) beginPermissionReviewCancellation(parent context.Context, gateID gate.ID) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	s.review.mu.Lock()
	if s.review.cancellations == nil {
		s.review.cancellations = make(map[gate.ID]context.CancelFunc)
	}
	s.review.cancellations[gateID] = cancel
	s.review.mu.Unlock()
	return ctx, func() {
		s.review.mu.Lock()
		delete(s.review.cancellations, gateID)
		s.review.mu.Unlock()
		cancel()
	}
}

// cancelPermissionReview cancels gateID's active review context, if any. It
// is idempotent and a safe no-op for a gate with no active review (the
// common case — most gates never have automatic review configured). It is
// the direct trigger point for design §15's "the gate resolves", "the owner
// closes the gate", and (via cancelPermissionReviewsForLoop /
// shutdownPermissionReviews) "the loop or turn is interrupted" and "the
// session shuts down".
func (s *Session) cancelPermissionReview(gateID gate.ID) {
	s.review.mu.Lock()
	cancel := s.review.cancellations[gateID]
	s.review.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// cancelPermissionReviewsForLoop cancels every active review whose gate is
// routed to loopID (design §15: "the loop or turn is interrupted"). It is
// O(open gates), bounded by the same live gate directory GateCaps already
// bounds.
func (s *Session) cancelPermissionReviewsForLoop(loopID uuid.UUID) {
	if loopID.IsZero() {
		return
	}
	s.gatesMu.Lock()
	var targets []gate.ID
	for id, entry := range s.gates {
		if entry.route.LoopID == loopID {
			targets = append(targets, id)
		}
	}
	s.gatesMu.Unlock()
	for _, id := range targets {
		s.cancelPermissionReview(id)
	}
}

// shutdownPermissionReviews cancels every currently active review (design
// §15: "the session shuts down"). Each cancellation-group entry removes
// itself as its owning goroutine returns (beginPermissionReviewCancellation's
// cleanup); this only needs to signal them.
func (s *Session) shutdownPermissionReviews() {
	s.review.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(s.review.cancellations))
	for _, cancel := range s.review.cancellations {
		cancels = append(cancels, cancel)
	}
	s.review.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

// reviewBreakerAllows reports whether automatic review may start for the turn
// coords identifies. It consults BOTH breaker scopes (design §18): a tripped
// SESSION-scoped breaker blocks every gate in the session — including one
// belonging to a turn that never itself contributed a single observation —
// because a session-level trip is a stronger signal than a turn-level one.
// Absent a session-scoped trip, it returns true (allow) whenever coords
// carries no TurnID to scope a turn-level breaker against — that can only
// happen for a degenerate/test caller, and failing open here only ever means
// "the pre-Task-16 no-breaker behavior applies", never a widened approval,
// since the breaker can only ever narrow (send more to human), never approve
// anything itself.
func (s *Session) reviewBreakerAllows(coords identity.Coordinates) bool {
	s.review.mu.Lock()
	defer s.review.mu.Unlock()
	if s.review.session != nil && s.review.session.tripped {
		return false
	}
	if coords.TurnID.IsZero() {
		return true
	}
	counters := s.review.turns[coords.TurnID]
	return counters == nil || !counters.tripped
}

// observePermissionReviewOutcome implements permissionReviewOutcomeObserver.
// It is called once per completed review() attempt (never for a review that
// found no applicable classifier at all — design §18's counters are about
// REVIEWED-but-rejected/failed attempts, not no-op turns) and updates BOTH
// the turn-scoped and the session-scoped circuit-breaker counters, tripping
// either breaker and emitting its own exactly-one bounded warning the moment
// its threshold is first crossed. The two scopes are otherwise entirely
// independent: a turn-scoped trip never marks the session tripped, and a
// session-scoped trip is checked on every future gate regardless of turn
// (reviewBreakerAllows).
func (s *Session) observePermissionReviewOutcome(coords identity.Coordinates, outcome reviewBreakerOutcome) {
	if coords.TurnID.IsZero() {
		return
	}
	s.review.mu.Lock()
	turnCounters := s.reviewTurnCountersLocked(coords.TurnID)
	sessionCounters := s.reviewSessionCountersLocked()
	if outcome.Eligible {
		turnCounters.consecutiveNeedsHuman = 0
		turnCounters.invalidOrFailed = 0
		sessionCounters.consecutiveNeedsHuman = 0
		sessionCounters.invalidOrFailed = 0
		s.review.mu.Unlock()
		return
	}
	switch {
	case reviewNeedsHumanReasons[outcome.Reason]:
		turnCounters.consecutiveNeedsHuman++
		sessionCounters.consecutiveNeedsHuman++
	case reviewInvalidOrFailedReasons[outcome.Reason]:
		turnCounters.invalidOrFailed++
		sessionCounters.invalidOrFailed++
	}
	s.trackReviewSubjectLocked(turnCounters, outcome.SubjectDigest)
	s.trackReviewSessionSubjectLocked(sessionCounters, outcome.SubjectDigest)
	s.maybeTripReviewBreakerLocked(coords, turnCounters)
	s.maybeTripReviewSessionBreakerLocked(coords, sessionCounters)
	s.review.mu.Unlock()
}

// recordPermissionReviewStale increments coords' turn's AND the session's
// stale-response counter (design §18's "classifier-originated stale
// responses"). It is called from respondFromClassifier's drift closure the
// moment a classifier-originated response is discovered to be stale — the
// exact moment design §18 wants counted, replacing the pre-Task-16 silent
// no-op with a silent no-op THAT ALSO counts (the response itself is still
// dropped exactly as before; only the bookkeeping is new). Since Addendum 4
// (design §13.4, TOCTOU) it is also called from
// verifyPermissionReviewObservations on an observation mismatch or an
// unverifiable/unconfigured recheck — the same "stale, silently dropped, but
// counted" treatment as the four original drift dimensions.
func (s *Session) recordPermissionReviewStale(coords identity.Coordinates) {
	if coords.TurnID.IsZero() {
		return
	}
	s.review.mu.Lock()
	turnCounters := s.reviewTurnCountersLocked(coords.TurnID)
	turnCounters.staleResponses++
	s.maybeTripReviewBreakerLocked(coords, turnCounters)
	sessionCounters := s.reviewSessionCountersLocked()
	sessionCounters.staleResponses++
	s.maybeTripReviewSessionBreakerLocked(coords, sessionCounters)
	s.review.mu.Unlock()
}

// reviewTurnCountersLocked returns turnID's counters, creating them if this
// is the first observation for the turn. The caller MUST hold s.review.mu.
// If the tracked-turn count is already at the defensive cap, a fresh
// zero-value counters value is returned WITHOUT being stored — the turn is
// simply not tracked rather than growing the map further; see
// maxTrackedReviewTurns.
func (s *Session) reviewTurnCountersLocked(turnID uuid.UUID) *reviewTurnCounters {
	if s.review.turns == nil {
		s.review.turns = make(map[uuid.UUID]*reviewTurnCounters)
	}
	if counters, ok := s.review.turns[turnID]; ok {
		return counters
	}
	counters := &reviewTurnCounters{}
	if len(s.review.turns) < maxTrackedReviewTurns {
		s.review.turns[turnID] = counters
	}
	return counters
}

// reviewSessionCountersLocked returns the session's single
// reviewSessionCounters value, allocating it on first observation. The
// caller MUST hold s.review.mu. Unlike reviewTurnCountersLocked there is no
// per-session cap to enforce (there is only ever one session-scoped counters
// value per Session, never a map keyed by session).
func (s *Session) reviewSessionCountersLocked() *reviewSessionCounters {
	if s.review.session == nil {
		s.review.session = &reviewSessionCounters{}
	}
	return s.review.session
}

// trackReviewSubjectLocked increments digest's occurrence count for counters,
// bounded to maxTrackedReviewSubjectDigests distinct digests. The caller MUST
// hold s.review.mu.
func (s *Session) trackReviewSubjectLocked(counters *reviewTurnCounters, digest [32]byte) {
	if counters.subjectCounts == nil {
		counters.subjectCounts = make(map[[32]byte]int)
	}
	if _, seen := counters.subjectCounts[digest]; !seen && len(counters.subjectCounts) >= maxTrackedReviewSubjectDigests {
		return
	}
	counters.subjectCounts[digest]++
}

// trackReviewSessionSubjectLocked is trackReviewSubjectLocked's
// session-scoped counterpart: the same bounded-digest-set discipline
// (maxTrackedReviewSubjectDigests), applied to counters that live for the
// session's whole lifetime rather than one turn. The caller MUST hold
// s.review.mu.
func (s *Session) trackReviewSessionSubjectLocked(counters *reviewSessionCounters, digest [32]byte) {
	if counters.subjectCounts == nil {
		counters.subjectCounts = make(map[[32]byte]int)
	}
	if _, seen := counters.subjectCounts[digest]; !seen && len(counters.subjectCounts) >= maxTrackedReviewSubjectDigests {
		return
	}
	counters.subjectCounts[digest]++
}

// maybeTripReviewBreakerLocked trips the breaker the first time any
// configured threshold is crossed, emitting exactly one bounded slog warning
// (design §18: "emits one bounded warning") and, when the consumer has
// configured InterruptOnTrip, asynchronously interrupting the tripping
// turn's loop subtree (design §18's optional stricter action). The caller
// MUST hold s.review.mu; the interrupt is dispatched on its own goroutine so
// it never blocks a caller holding the lock or the review goroutine that
// triggered the trip.
func (s *Session) maybeTripReviewBreakerLocked(coords identity.Coordinates, counters *reviewTurnCounters) {
	if counters.tripped {
		// Already tripped for this turn: nothing new to warn about or act on.
		return
	}
	limits := s.review.limits
	tripped := (limits.MaxConsecutiveNeedsHuman > 0 && counters.consecutiveNeedsHuman >= limits.MaxConsecutiveNeedsHuman) ||
		(limits.MaxInvalidOrFailed > 0 && counters.invalidOrFailed >= limits.MaxInvalidOrFailed) ||
		(limits.MaxStaleResponses > 0 && counters.staleResponses >= limits.MaxStaleResponses) ||
		s.reviewSubjectThresholdCrossedLocked(counters, limits)
	if !tripped {
		return
	}
	counters.tripped = true
	if !counters.warned {
		counters.warned = true
		slog.WarnContext(s.sessionCtx, "sessionruntime: permission-review circuit breaker tripped; automatic review disabled for this turn",
			"turn_id", coords.TurnID, "loop_id", coords.LoopID)
	}
	if limits.InterruptOnTrip && !coords.LoopID.IsZero() {
		loopID := coords.LoopID
		go func() { _ = s.interruptSubtree(s.sessionCtx, loopID) }()
	}
}

// reviewSubjectThresholdCrossedLocked reports whether any tracked subject
// digest has reached the configured identical-subject limit. The caller MUST
// hold s.review.mu.
func (s *Session) reviewSubjectThresholdCrossedLocked(counters *reviewTurnCounters, limits reviewCircuitBreakerLimits) bool {
	if limits.MaxIdenticalSubjects <= 0 {
		return false
	}
	for _, count := range counters.subjectCounts {
		if count >= limits.MaxIdenticalSubjects {
			return true
		}
	}
	return false
}

// maybeTripReviewSessionBreakerLocked is maybeTripReviewBreakerLocked's
// session-scoped counterpart: it trips the SESSION breaker the first time any
// configured SESSION threshold (s.review.limits.Session — a separate
// configuration surface from the turn-scoped thresholds; see
// reviewCircuitBreakerLimits.Session's doc comment for why they must not be
// shared) is crossed by the session-lifetime counters, emitting its own
// exactly-one bounded warning (a distinct warned flag from the turn-scoped
// counters' own, and a distinct log message) the moment the session breaker
// is first tripped. A session-scoped trip deliberately does NOT apply
// InterruptOnTrip: that option is documented (and tested) as interrupting the
// tripping TURN's loop subtree, and extending it to "interrupt everything in
// the session" is a materially different, undocumented action outside this
// fix's scope. The caller MUST hold s.review.mu.
func (s *Session) maybeTripReviewSessionBreakerLocked(coords identity.Coordinates, counters *reviewSessionCounters) {
	if counters.tripped {
		// Already tripped for the session: nothing new to warn about.
		return
	}
	limits := s.review.limits.Session
	tripped := (limits.MaxConsecutiveNeedsHuman > 0 && counters.consecutiveNeedsHuman >= limits.MaxConsecutiveNeedsHuman) ||
		(limits.MaxInvalidOrFailed > 0 && counters.invalidOrFailed >= limits.MaxInvalidOrFailed) ||
		(limits.MaxStaleResponses > 0 && counters.staleResponses >= limits.MaxStaleResponses) ||
		s.reviewSessionSubjectThresholdCrossedLocked(counters, limits)
	if !tripped {
		return
	}
	counters.tripped = true
	if !counters.warned {
		counters.warned = true
		slog.WarnContext(s.sessionCtx, "sessionruntime: permission-review circuit breaker tripped; automatic review disabled for the session",
			"session_id", coords.SessionID)
	}
}

// reviewSessionSubjectThresholdCrossedLocked is
// reviewSubjectThresholdCrossedLocked's session-scoped counterpart. The
// caller MUST hold s.review.mu.
func (s *Session) reviewSessionSubjectThresholdCrossedLocked(counters *reviewSessionCounters, limits reviewSessionCircuitBreakerLimits) bool {
	if limits.MaxIdenticalSubjects <= 0 {
		return false
	}
	for _, count := range counters.subjectCounts {
		if count >= limits.MaxIdenticalSubjects {
			return true
		}
	}
	return false
}

// clearReviewTurnState removes ev's turn from the breaker's tracked set
// (design §18: "counters are bounded and cleared when the turn completes").
// It is a no-op for any event that is not one of the three turn-terminal
// events, mirroring recordLoopMechanicalState's own event-type switch. It is
// called from PublishEvent/PublishEventChecked alongside
// recordLoopMechanicalState.
func (s *Session) clearReviewTurnState(ev event.Event) {
	switch ev.(type) {
	case event.TurnDone, event.TurnFailed, event.TurnInterrupted:
	default:
		return
	}
	turnID := ev.EventHeader().Coordinates.TurnID
	if turnID.IsZero() {
		return
	}
	s.review.mu.Lock()
	delete(s.review.turns, turnID)
	s.review.mu.Unlock()
}

// subjectContentDigest is a content-only digest of req: the tool name plus
// every requirement's description, in order. It is deliberately independent
// of GateID/ToolExecutionID (which are unique per call by construction and
// would defeat "materially identical" detection entirely — design §18) and
// deliberately NOT gate.SubjectDigest, whose canonical wire projection is
// scoped per-classifier and always includes the unique gate/tool-execution
// identity (design §8.2). It is used only as an in-memory, non-durable,
// purely defensive dedupe signal: a collision or an imperfect canonicalization
// can only ever cause MORE human review, never less, so json.Marshal's
// ordinary struct/slice-order determinism is sufficient here without the
// stricter canonicalization pkg/gate's durable digest requires.
func subjectContentDigest(req tool.Request) [32]byte {
	type digestRequirement struct {
		Description string `json:"description"`
	}
	type digestRequest struct {
		ToolName     string              `json:"tool_name"`
		Requirements []digestRequirement `json:"requirements"`
	}
	projected := digestRequest{ToolName: req.ToolName}
	for _, requirement := range req.Requirements {
		projected.Requirements = append(projected.Requirements, digestRequirement{Description: requirement.Description})
	}
	encoded, err := json.Marshal(projected)
	if err != nil {
		// encoding a struct of plain strings cannot fail; fall back to a
		// fixed sentinel rather than propagating an error from a purely
		// advisory digest.
		return sha256.Sum256([]byte(req.ToolName))
	}
	return sha256.Sum256(encoded)
}
