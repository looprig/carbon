package sessionruntime

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/internal/loopruntime"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/harness/pkg/tool"
)

// gateState is the live state of a directory entry. It is never persisted — the
// durable state is reconstructed from GatePreparedRecord/GateOpened/GateResolved
// on restore. preparing and claiming are not client-visible.
type gateState uint8

const (
	gatePreparing gateState = iota // durable GatePreparedRecord, no public GateOpened
	gateOpen                       // durable GateOpened, listed and answerable
	gateClaiming                   // in-memory only, between lock-claim and durable GateResolved
	gateClosed                     // durable GateResolved, removed from directory
)

// gateEntry is the session-private directory entry: the public Gate envelope PLUS
// the internal route, the typed resolver payload (never shipped to clients), the
// event coordinates stamped at prepare time, and the live state. The route is set
// only after ActivateGate appends GateOpened.
type gateEntry struct {
	gate        gate.Gate
	route       gate.Route
	payload     gate.Payload
	coordinates identity.Coordinates
	state       gateState
	// reviewBasis is the trusted snapshot of the live permission-review basis
	// (GateID, ToolExecutionID, ContextRevision, GatePolicyRevision,
	// SecurityCeiling) captured by StartPermissionReview at the moment
	// classifier review began for this gate. It is populated independently of
	// anything a classifier or its Hustle result reports, so
	// respondFromClassifier can re-derive the CURRENT basis from data the
	// session itself owns and compare it against the basis a classifier's
	// eligible assessment was computed against — any divergence (a stale
	// review, a policy revision that has since moved on, a mismatched tool
	// execution) is detected here rather than trusted from the caller. Zero
	// for gates permission review never started on.
	reviewBasis gate.ReviewBasis
}

// gateAppender is the STRICT durable append seam for gate prepare/open/resolve.
// Unlike the hub's PublishEvent (which faults and returns nil on append failure),
// this seam returns the append error so PrepareGateOpen/ActivateGate can fail
// closed — a failed prepare installs no directory entry, a failed activate leaves
// the gate preparing. The nop default (nopGateAppender) keeps headless mode
// unchanged; the composition root wires the real journal+hub adapter.
type gateAppender interface {
	AppendGatePrepared(ctx context.Context, rec journal.GatePreparedRecord) error
	AppendGateOpened(ctx context.Context, ev event.GateOpened) error
	AppendGateResolved(ctx context.Context, ev event.GateResolved) error
}

// liveGateAppender keeps the private prepared payload on the gate journal seam, while
// routing public GateOpened/GateResolved events through the session's checked hub path.
// The latter is essential: public gate transitions must be both durably appended and
// delivered to live subscribers, and PublishEventChecked preserves durable-first failure
// semantics without double-appending them.
type liveGateAppender struct {
	prepared  gateAppender
	publisher interface {
		PublishEventChecked(context.Context, event.Event) error
	}
}

func (a *liveGateAppender) AppendGatePrepared(ctx context.Context, rec journal.GatePreparedRecord) error {
	return a.prepared.AppendGatePrepared(ctx, rec)
}

func (a *liveGateAppender) AppendGateOpened(ctx context.Context, ev event.GateOpened) error {
	return a.publisher.PublishEventChecked(ctx, ev)
}

func (a *liveGateAppender) AppendGateResolved(ctx context.Context, ev event.GateResolved) error {
	return a.publisher.PublishEventChecked(ctx, ev)
}

// nopGateAppender is the default gateAppender: all appends succeed without doing
// anything. It keeps headless/no-persistence mode unchanged.
type nopGateAppender struct{}

func (nopGateAppender) AppendGatePrepared(context.Context, journal.GatePreparedRecord) error {
	return nil
}
func (nopGateAppender) AppendGateOpened(context.Context, event.GateOpened) error {
	return nil
}
func (nopGateAppender) AppendGateResolved(context.Context, event.GateResolved) error {
	return nil
}

// hostOwnedGate reports whether a gate's answer belongs to the HOST that opened
// it rather than to a loop.
//
// This is the distinction that decides how an answer is delivered.
// translateGateResponse turns a permission or ask-user answer into a
// command.Command addressed to entry.route.LoopID, because those gates park a
// loop's turn and resuming it IS the answer. A form or open-url gate raised by an
// integration (an MCP server's elicitation) parks no turn: the caller waiting on
// it is the host's own blocked OpenGate call, which no loop command can reach.
//
// Both conditions are required, and the resolver is not redundant with the kind:
//
//   - The KIND must be one whose answer this session can validate against a
//     schema and hand back as gate.Values. Only form and open-url qualify.
//   - The RESOLVER must be gate.ResolverSession. ResolverSession has existed on
//     the envelope since the gate contract was written and no production code
//     ever produced one — it is the declared, unused name for exactly this case,
//     so this fills a vacant seam rather than inventing one.
//
// A form gate marked ResolverLoop is therefore NOT host-owned and is refused at
// answer time (GateKindMismatch) rather than silently delivered somewhere. There
// is no loop-side form resolver to route it to, so accepting it would mean
// dropping a human's answer on the floor.
func hostOwnedGate(g gate.Gate) bool {
	if g.Resolver != gate.ResolverSession {
		return false
	}
	switch g.Kind {
	case gate.KindForm, gate.KindOpenURL:
		return true
	case gate.KindPermission, gate.KindAskUser:
		return false
	default:
		return false
	}
}

// GateCaps bounds the live gate directory. The cap counts preparing + open +
// claiming so failed activations cannot accumulate invisible prepared entries.
// Zero means no cap.
type GateCaps struct {
	MaxOpen    int
	MaxTimeout time.Duration
}

// WithGateAppender injects the strict durable append seam for gate
// prepare/open/resolve. A nil appender is ignored (the nop default stays
// installed). It is the gate-directory counterpart to WithCommandAppender.
func WithGateAppender(a gateAppender) Option {
	return func(s *Session) {
		if a != nil {
			s.gateAppender = a
		}
	}
}

// WithGateCaps injects the live gate directory bounds. Zero (the default) means
// no cap. The cap counts preparing + open + claiming.
func WithGateCaps(caps GateCaps) Option {
	return func(s *Session) {
		s.gateCaps = caps
	}
}

// withPermissionReview configures the session-wide permission-classifier
// registry and review policy, mirroring withSessionHustles
// (internal/sessionruntime/hustle.go) for automatic permission review rather
// than compaction/summarization Hustles. It is private: wiring a real
// classifier set from a consumer's rig.WithPermissionClassifiers call into
// this option is Task 16/Phase 6 work, not this seam's.
//
// A zero classifiers set (the default when this option is never applied)
// leaves StartPermissionReview a no-op, so any session that does not opt in
// keeps the exact pre-Task-14 gate lifecycle.
func withPermissionReview(classifiers gate.PermissionClassifierSet, policy gate.PermissionReviewPolicy) Option {
	return func(s *Session) {
		s.permissionClassifiers = classifiers
		s.permissionReviewPolicy = policy
	}
}

// withPermissionReviewEvidence configures the session-wide, consumer-supplied
// evidence-tool access boundary every registered permission classifier's
// evidence tools authorize against (design §13.1), mirroring
// withPermissionReview's capture-only shape. It is private: a real
// access/verifier pair wired from a consumer's rig.WithPermissionReviewEvidence
// call is Task 24 work, not this seam's. newHustleController (hustle.go)
// fails construction closed if a registered classifier needs evidence tools
// but this option was never applied.
func withPermissionReviewEvidence(access gate.EvidenceAccessEvaluator, containment gate.EvidenceContainmentVerifier, allowedKinds []string) Option {
	return func(s *Session) {
		s.permissionReviewEvidenceAccess = access
		s.permissionReviewEvidenceContainment = containment
		s.permissionReviewEvidenceAllowedKinds = append([]string(nil), allowedKinds...)
	}
}

// withPermissionReviewSecurityCeiling configures the session-wide,
// consumer-supplied effective security posture (design §13.1/§21) every
// ReviewContext loopReviewContext builds carries as SecurityCeiling, and
// every classifier's ReviewBasis/ReviewBasis-derived hustle.Request
// therefore inherits. It is private: the real value, wired from a
// consumer's rig.WithPermissionReviewSecurityCeiling call, is
// rig.Define()-time validated (mirroring withPermissionReviewEvidence's own
// "config X requires config Y" pairing) to be non-empty whenever classifiers
// are configured — this option's own zero value simply leaves
// s.permissionReviewSecurityCeiling empty, which loopReviewContext then
// treats as "review context capture stays off" (defense in depth; the loud
// failure lives at Define()-time, not here).
//
// This deliberately replaces a former Harness-side sentinel constant
// ("harness-session; ceiling-not-classified") that every session used to
// stamp unconditionally: SecurityCeiling is architecturally the SAME KIND OF
// VALUE as permissionReviewEvidenceContainment/permissionReviewEvidenceAllowedKinds
// above — a consumer-owned concept Harness structurally cannot originate
// (this module has no first-class "effective access posture" notion) — so a
// fixed placeholder could never equal a real consumer's own ceiling string,
// making every evidence-tool containment check that compared against it fail
// closed 100% of the time (the Phase 6 spec-compliance review's Finding 2).
func withPermissionReviewSecurityCeiling(ceiling string) Option {
	return func(s *Session) {
		s.permissionReviewSecurityCeiling = ceiling
	}
}

// withPermissionReviewObservationVerifier configures the session-wide,
// OPTIONAL consumer-supplied recheck seam (design §13.4, TOCTOU) every
// classifier-originated auto-approval's recorded observations are verified
// against immediately before respondFromClassifier claims the gate,
// mirroring withPermissionReviewEvidence's capture-only shape. It is
// private: the real value, wired from a consumer's
// rig.WithPermissionReviewObservations call, is Addendum 4 work, not this
// seam's. Unlike withPermissionReviewEvidence/withPermissionReviewSecurityCeiling,
// omitting this option is a fully supported terminal configuration, not a
// construction-time gap papered over by a later fail-closed check — see
// permissionReviewObservationVerifier's doc comment (session.go) and
// verifyPermissionReviewObservations for exactly how a nil verifier is
// still handled safely (and when it is NOT: one or more recorded
// observations reaching a nil verifier fails closed).
func withPermissionReviewObservationVerifier(verifier gate.EvidenceObservationVerifier) Option {
	return func(s *Session) {
		s.permissionReviewObservationVerifier = verifier
	}
}

// defaultReviewContextPolicy is the fixed default gate.ReviewContextPolicy
// loopReviewContext installs. No consumer-facing surface carries a
// context-snapshot budget today (this addendum deliberately adds no new
// public option — see loopReviewContext's doc comment), so these bounds are a
// conservative, documented default: every field sits well inside its
// corresponding hard cap (pkg/gate.MaxReviewContextInputEntries /
// MaxReviewContextInputBytes / MaxReviewContextEntryInputBytes) with margin
// under the 1 MiB permission-review subject-wire ceiling
// (gate.MaxPermissionReviewSubjectWireBytes).
var defaultReviewContextPolicy = gate.ReviewContextPolicy{
	Revision:             "harness-default-review-context-policy/v1",
	MaxBytes:             512 << 10,
	MaxEstimatedTokens:   128 << 10,
	MaxEntries:           512,
	MaxUserEntryBytes:    64 << 10,
	MaxAgentEntryBytes:   64 << 10,
	MaxToolEntryBytes:    64 << 10,
	MaxBlockBytes:        64 << 10,
	MaxActiveActionBytes: 64 << 10,
}

// loopReviewContext builds the auto-derived, Harness-internal review-context
// capture configuration every Loop this session constructs receives, once
// registered permission classifiers make automatic review reachable at all
// (design §14.1, this addendum). It returns nil (capture stays OFF, exactly
// as before this addendum) whenever no classifier is configured — a session
// that never opts into permission review keeps precisely the pre-existing
// behavior regardless of anything else on the session.
//
// A non-empty PermissionClassifierSet forces this ON with NO further consumer
// input: "classifiers registered but capture off" is not a legitimate
// selective-disable state — it is exactly the bug this addendum closes (a
// configured reviewer that silently never reviews). If a consumer ever wants
// selective review-routing, that belongs in the review POLICY layer
// (gate.ReviewContextPolicy / gate.PermissionReviewPolicy, both already
// consumer-configurable), not a capture on/off switch — so this addendum
// adds NO new public rig/consumer-facing option.
//
//   - WorkspaceRoot/WorkingDirectory are auto-derived from the session's own
//     existing workspace-root tracking (s.wsRoot), the SAME source
//     hustleEvidenceRuntimeConfig already uses for ReadWorkspace.Root — no
//     new consumer input. Every classifier's hustle.Definition requires
//     evidence tools (gate.NewPermissionClassifierSet's own descriptor
//     check), so newHustleController's pre-existing fail-closed check
//     already refuses session construction when classifiers are configured
//     without a populated ReadWorkspace — meaning s.wsRoot is guaranteed
//     non-empty here whenever classifiers are configured and construction
//     succeeded; the emptiness guard below is defense in depth, not
//     load-bearing. Harness tracks no per-loop working directory narrower
//     than the workspace root, so WorkingDirectory is set to the same value
//     (a documented simplification, not a fabricated distinction).
//   - GatePolicyRevision is s.permissionReviewPolicy.Revision — the SAME
//     value the first addendum already wires into the session's fingerprint
//     and review policy, not a new source.
//   - Policy has NO existing Harness-level source (see
//     defaultReviewContextPolicy above for why it is a documented, bounded,
//     honest default rather than an invented classification scheme).
//   - SecurityCeiling is s.permissionReviewSecurityCeiling — a genuinely
//     consumer-supplied value (withPermissionReviewSecurityCeiling's doc
//     comment), NOT a Harness-invented default: unlike Policy, Harness must
//     never guess at this one, because a wrong guess widens (or falsely
//     narrows) what a consumer's own evidence-containment check compares
//     against. An empty value here — a session that never opted into
//     WithPermissionReviewSecurityCeiling — makes this whole method return
//     nil below rather than stamp a placeholder (defense in depth;
//     rig.Define() is the loud, primary enforcement for any rig-constructed
//     session).
func (s *Session) loopReviewContext() *loopruntime.ReviewContext {
	if len(s.permissionClassifiers.Classifiers()) == 0 || s.wsRoot == "" || s.permissionReviewSecurityCeiling == "" {
		return nil
	}
	return &loopruntime.ReviewContext{
		WorkspaceRoot:      s.wsRoot,
		WorkingDirectory:   s.wsRoot,
		SecurityCeiling:    s.permissionReviewSecurityCeiling,
		GatePolicyRevision: s.permissionReviewPolicy.Revision,
		Policy:             defaultReviewContextPolicy,
	}
}

// StartPermissionReview implements the loop actor's private permissionReviewStarter
// seam (internal/loopruntime/gate.go): the ONLY thing the actor calls, inline
// and fire-and-forget, after a gatePermission registration activates and the
// runner has been acked (design §14.2/§14.3). It MUST return promptly — the
// actor never wraps this call in a goroutine of its own, so classifier
// inference is moved onto a fresh goroutine here (`go adapter.review(...)`)
// before this method returns.
//
// StartPermissionReview no-ops (starts no review) whenever:
//   - no permission classifiers are configured for this session
//     (s.permissionClassifiers is the zero PermissionClassifierSet); or
//   - the session's shared Hustle runtime is not yet bound
//     (s.hustleController == nil — cannot happen once a session has finished
//     construction, but is checked rather than assumed); or
//   - the classifier/policy configuration itself is invalid
//     (newPermissionReviewAdapter's own validation).
//
// Every one of those is a "nothing to review" outcome, never a denial or an
// approval: this method has no path to RespondGate at all (see
// permissionReviewAdapter's type doc), so the human gate this handoff
// accompanies is always preserved regardless of what happens here.
func (s *Session) StartPermissionReview(ctx context.Context, req loopruntime.PermissionReviewRequest) {
	if len(s.permissionClassifiers.Classifiers()) == 0 {
		return
	}
	if s.hustleController == nil {
		return
	}
	// Circuit breaker (design §18): a tripped turn OR a tripped session
	// (reviewBreakerAllows consults both scopes) starts no further automatic
	// review at all — no adapter, no cancellation handle, no reviewBasis
	// stamp — leaving every current and future gate human-only, exactly like
	// the "no classifiers configured" no-op above. A session-scoped trip
	// blocks EVERY turn in the session, not just the one that tripped it.
	if !s.reviewBreakerAllows(req.ReviewContext.Coordinates) {
		return
	}
	adapter, err := newPermissionReviewAdapter(s.hustleController, s.permissionClassifiers, s.permissionReviewPolicy, s)
	if err != nil {
		slog.WarnContext(ctx, "sessionruntime: permission review adapter misconfigured; starting no review",
			"gate_id", req.GateID, "error", err)
		return
	}
	// Optional: the adapter reports one terminal circuit-breaker summary per
	// review() call through this seam (design §18). It is not a
	// newPermissionReviewAdapter collaborator because its absence must never
	// fail construction.
	adapter.observer = s
	// Wire durable (checked) audit publication (design §16, Task 17, fixed by
	// the Phase 6 spec-compliance review): s.hub implements
	// PublishInternalEventChecked — the SAME private audit-record path
	// s.newHustleController already wires as its Audit collaborator
	// (hustle.go) for ordinary Hustle lifecycle events — not *Session's
	// PublishEventChecked, which is the PUBLIC-only path and unconditionally
	// rejects these Internal-visibility events (permissionReviewAuditPublisher's
	// doc comment). s.factory already mints every other durable event's
	// Header, and sessionHustleFaultReporter is the SAME session fault path
	// Task 14's ordinary Hustle faults use — reused here rather than
	// duplicated. s.hustleLimits.AuditTimeout is guaranteed positive by the
	// time s.hustleController is non-nil (hustleruntime.New rejects a
	// non-positive AuditTimeout at construction), matching the
	// nil-hustleController guard above.
	adapter.publisher = s.hub
	adapter.stamper = s.factory
	adapter.faults = sessionHustleFaultReporter{session: s}
	adapter.auditTimeout = s.hustleLimits.AuditTimeout
	s.recordPermissionReviewBasis(req)
	// One cancellation-group context per active gate review (design §15).
	// reviewCtx's cleanup is the ONLY place its cancellations entry is ever
	// removed; every other trigger (gate resolve, owner close, loop/turn
	// interrupt, session shutdown) only ever calls the stored cancel func.
	reviewCtx, done := s.beginPermissionReviewCancellation(ctx, req.GateID)
	go func() {
		defer done()
		adapter.review(reviewCtx, req)
	}()
}

// recordPermissionReviewBasis stamps the gate directory entry with the
// trusted, session-owned snapshot of the live review basis at the moment
// review starts. It is a no-op for a gate that is not currently in the
// directory (already closed, or never prepared) — StartPermissionReview is
// fire-and-forget, so there is nothing to record a basis against and nothing
// downstream that would ever read it.
func (s *Session) recordPermissionReviewBasis(req loopruntime.PermissionReviewRequest) {
	s.gatesMu.Lock()
	defer s.gatesMu.Unlock()
	entry, ok := s.gates[req.GateID]
	if !ok {
		return
	}
	entry.reviewBasis = gate.ReviewBasis{
		GateID:             req.GateID,
		ToolExecutionID:    req.ToolExecutionID,
		ContextRevision:    req.ReviewContext.ContextRevision,
		GatePolicyRevision: req.ReviewContext.GatePolicyRevision,
		SecurityCeiling:    req.ReviewContext.SecurityCeiling,
	}
	s.gates[req.GateID] = entry
}

// PrepareGateOpen durably commits the public envelope plus private payload as a
// private GatePreparedRecord. It mints the GateID, stamps the GatePrepared event
// with the session's coordinates and the caller's loopID, appends the record via
// the strict gateAppender, and — only on success — inserts a non-listable
// preparing entry. A failed append returns a typed *GateError{GateAppendFailed}
// and does not mutate the directory. loopID is the producing loop's id (the
// GatePrepared event is loopScoped); TurnID/StepID are read from the gate's
// Subject.
func (s *Session) PrepareGateOpen(ctx context.Context, loopID uuid.UUID, g gate.Gate, payload gate.Payload) (gate.ID, error) {
	s.gatesMu.Lock()
	defer s.gatesMu.Unlock()

	if err := s.checkGateCap(); err != nil {
		return gate.ID{}, err
	}
	// Kind-implied envelope invariants (e.g. an open-url gate may not be
	// Restorable). Every pre-existing kind validates clean, so this rejects only
	// envelopes that were already incoherent.
	if err := gate.ValidateGate(g); err != nil {
		return gate.ID{}, &GateError{Kind: GateKindMismatch, Cause: err}
	}
	policy, err := s.resolveGatePolicy(g)
	if err != nil {
		return gate.ID{}, err
	}

	gateID, err := s.mintGateID()
	if err != nil {
		return gate.ID{}, err
	}
	g.ID = gateID
	g.ResponsePolicy = policy

	coords := identity.Coordinates{
		SessionID: s.sessionID,
		LoopID:    loopID,
		TurnID:    uuid.UUID(g.Subject.TurnID),
		StepID:    uuid.UUID(g.Subject.StepID),
	}
	prepared, err := s.stampGateEvent(coords, g)
	if err != nil {
		return gate.ID{}, err
	}

	openPayload := gate.OpenPayload{GateID: gateID, Payload: payload}
	rec := journal.NewGatePreparedRecord(prepared, openPayload)
	if err := s.gateAppender.AppendGatePrepared(ctx, rec); err != nil {
		return gate.ID{}, &GateError{GateID: gateID, Kind: GateAppendFailed, Cause: err}
	}

	s.gates[gateID] = gateEntry{gate: g, payload: payload, coordinates: coords, state: gatePreparing}
	// The answer slot is installed at PREPARE time, before the gate is public and
	// therefore before anything can answer it. That ordering is what makes
	// AwaitGateAnswer race-free: the opener learns its GateID here, calls
	// ActivateGate, and only then blocks — by which point the slot that a
	// concurrent RespondGate would write to already exists.
	if hostOwnedGate(g) {
		if s.gateAnswers == nil {
			s.gateAnswers = make(map[gate.ID]chan gate.Answer)
		}
		s.gateAnswers[gateID] = make(chan gate.Answer, 1)
	}
	return gateID, nil
}

// OpenHostGate opens a HOST-OWNED gate and returns its id, ready to be awaited.
//
// It is the ONLY gate-opening entry point on the published session.GateHost
// contract, and it is deliberately not PrepareGateOpen. PrepareGateOpen takes an
// arbitrary kind, resolver, payload, and — through the ActivateGate route that
// must follow it — an arbitrary target loop, which together are exactly the
// loop-owned command path (dispatchGateCommand). A host must not be able to
// reach that: minting an "approve this tool call" gate against someone else's
// loop is not a capability an integration should acquire by being able to ask a
// human a question. This entry point takes the same arguments minus the route
// and refuses everything that is not host-owned, so the loop path is unreachable
// through it by construction rather than by convention.
//
// It collapses prepare and activate into one call, which is safe precisely
// because the gate is host-owned. The two phases exist so a LOOP can install its
// blocker while the gate is still private; a host's blocker is the answer slot,
// and PrepareGateOpen installs that itself. There is no window here for the
// opener to miss.
//
// Every rejection is a *GateError{GateKindMismatch}, checked BEFORE anything is
// journaled:
//
//   - hostOwnedGate — the same predicate that governs answer time, so open time
//     and answer time cannot drift apart. A form gate declaring ResolverLoop is
//     refused here rather than accepted and then refused after a human has
//     already answered it.
//   - the payload must match the kind, so a form gate cannot be opened with an
//     open-url payload (which would strand its answer at ParseFormAnswers).
//   - the payload's own invariants (ValidateFormSchema / ValidateOpenURLPayload)
//     must hold, because an unanswerable or targetless prompt should never reach
//     a human.
//
// It then DERIVES the trusted half of the public projection from the validated
// payload (projectHostGatePrompt), so the envelope a renderer sees cannot
// disagree with the payload a response is validated against.
//
// The caller MUST either AwaitGateAnswer or CloseGate; both free the slot.
func (s *Session) OpenHostGate(ctx context.Context, loopID uuid.UUID, g gate.Gate, payload gate.Payload) (gate.ID, error) {
	if !hostOwnedGate(g) {
		return gate.ID{}, &GateError{Kind: GateKindMismatch}
	}
	if err := validateHostGatePayload(g.Kind, payload); err != nil {
		return gate.ID{}, &GateError{Kind: GateKindMismatch, Cause: err}
	}
	g = projectHostGatePrompt(g, payload)

	id, err := s.PrepareGateOpen(ctx, loopID, g, payload)
	if err != nil {
		return gate.ID{}, err
	}
	// The route carries only the gate id. A host-owned answer is delivered to the
	// opener's slot and never translated into a command, so there is no loop to
	// address — and leaving LoopID zero means that even a future miswiring would
	// fail to find a loop rather than reach the wrong one.
	if err := s.ActivateGate(ctx, id, gate.Route{GateID: id}); err != nil {
		// The gate never became public, so nothing can answer it. Drop it so a
		// failed open leaves neither a directory entry nor an answer slot behind.
		_ = s.CloseGate(ctx, id, gate.CloseAbandoned)
		return gate.ID{}, err
	}
	return id, nil
}

// projectHostGatePrompt derives the trusted half of a host gate's PUBLIC prompt
// from its already-validated PRIVATE payload, and returns the corrected gate.
//
// The payload is the authoritative record of what was asked; Prompt is the
// projection of it that a renderer sees. Nothing structural made the two agree:
// an opener could pass one schema to be answered against and project a
// different one, or name one origin in the payload and another on the envelope.
// Rather than CHECK for that divergence and refuse it, this makes it
// unrepresentable — the projected fields are overwritten from the payload, so
// there is no value an opener can pass that produces a divergent envelope, and
// no third state to test for. It is the same move PrepareGateOpen already makes
// with Gate.ID and Gate.ResponsePolicy: fields the session owns are derived,
// not accepted.
//
// Two fields, for two different reasons:
//
//   - Origin (open-url) is a SECURITY projection. It is validated as a bare
//     origin by ValidateOpenURLPayload above and by gate.ValidateGate below, and
//     a human's trust decision rests on it. Deriving it means the origin a human
//     reads is necessarily the origin the opener declared and the journal
//     recorded — an envelope naming "https://github.com" over a payload naming
//     an attacker's host cannot be constructed.
//   - Schema (form) is a UX projection. Divergence there was never a security
//     hole — ParseFormAnswers validates against the payload, so a bogus
//     projection costs a REJECTED answer, never an unvalidated one — but a
//     rejected answer means a human filled in fields that were never going to be
//     accepted. Deriving retires that bug class at the same seam.
//
// Title and Body are deliberately left alone: they are prose for a human, carry
// no invariant a renderer or validator relies on, and an opener may legitimately
// word the projection differently from the record.
func projectHostGatePrompt(g gate.Gate, payload gate.Payload) gate.Gate {
	switch g.Kind {
	case gate.KindForm:
		if form, ok := formPayloadFromGatePayload(payload); ok {
			g.Prompt.Schema = form.Schema
		}
	case gate.KindOpenURL:
		if openURL, ok := openURLPayloadFromGatePayload(payload); ok {
			g.Prompt.Origin = openURL.DisplayOrigin
		}
	}
	return g
}

// validateHostGatePayload reports whether payload is the right shape for kind and
// satisfies that shape's own invariants. It fails closed: a kind with no
// host-owned payload is rejected rather than opened unvalidated.
func validateHostGatePayload(kind gate.Kind, payload gate.Payload) error {
	switch kind {
	case gate.KindForm:
		form, ok := formPayloadFromGatePayload(payload)
		if !ok {
			return errHostGatePayloadMismatch
		}
		return gate.ValidateFormSchema(form.Schema)
	case gate.KindOpenURL:
		openURL, ok := openURLPayloadFromGatePayload(payload)
		if !ok {
			return errHostGatePayloadMismatch
		}
		return gate.ValidateOpenURLPayload(openURL)
	default:
		return errHostGatePayloadMismatch
	}
}

// errHostGatePayloadMismatch reports a payload whose type does not match the
// gate kind it was offered for.
var errHostGatePayloadMismatch = errors.New("sessionruntime: gate payload does not match gate kind")

func openURLPayloadFromGatePayload(payload gate.Payload) (gate.OpenURLPayload, bool) {
	switch v := payload.(type) {
	case gate.OpenURLPayload:
		return v, true
	case *gate.OpenURLPayload:
		if v == nil {
			return gate.OpenURLPayload{}, false
		}
		return *v, true
	default:
		return gate.OpenURLPayload{}, false
	}
}

// AwaitGateAnswer blocks until a HOST-OWNED gate is answered and returns the
// validated answer, including the form values that are deliberately absent from
// every durable record.
//
// It is the host's half of the loop's command dispatch: the opener of a form or
// open-url gate calls PrepareGateOpen, ActivateGate, then this. It returns a
// typed *GateError{GateNotFound} for a gate that is not host-owned, was never
// prepared, or whose answer was already taken — an answer is delivered exactly
// once.
//
// The opener MUST either await or CloseGate, which is the same obligation
// ActivateGate already places on it. Both paths free the slot.
//
// A ctx cancellation abandons the wait and frees the slot; it does NOT close the
// gate, because the gate is durable state and ctx is the caller's. An opener that
// gives up should CloseGate.
func (s *Session) AwaitGateAnswer(ctx context.Context, id gate.ID) (gate.Answer, error) {
	s.gatesMu.Lock()
	slot, ok := s.gateAnswers[id]
	s.gatesMu.Unlock()
	if !ok {
		return gate.Answer{}, &GateError{GateID: id, Kind: GateNotFound}
	}

	select {
	case answer, ok := <-slot:
		s.releaseGateAnswerSlot(id)
		if !ok {
			// CloseGate closed the slot: the gate was abandoned or withdrawn and
			// will never be answered. Report it as gone rather than returning a
			// zero Answer an opener could mistake for a real one.
			return gate.Answer{}, &GateError{GateID: id, Kind: GateNotFound}
		}
		return answer, nil
	case <-ctx.Done():
		s.releaseGateAnswerSlot(id)
		return gate.Answer{}, ctx.Err()
	}
}

// releaseGateAnswerSlot drops a host-owned gate's delivery slot once its opener
// is done with it. It does NOT close the channel — the opener is the reader, and
// only CloseGate (which knows no answer is coming) closes. It is idempotent.
func (s *Session) releaseGateAnswerSlot(id gate.ID) {
	s.gatesMu.Lock()
	defer s.gatesMu.Unlock()
	delete(s.gateAnswers, id)
}

// closeGateAnswerSlotLocked closes and drops a host-owned gate's delivery slot,
// waking an opener blocked in AwaitGateAnswer with GateNotFound.
//
// Closing is safe against a concurrent send for two independent reasons, and the
// second is what this relies on. First, CloseGate and RespondGate can never both
// act on one gate: both claim the entry by flipping it to gateClaiming under
// gatesMu, and the loser returns GateNotReady. Second — and this is the guarantee
// that does not depend on that protocol — deliverGateAnswer performs its send
// while holding gatesMu, so once this has deleted the slot no sender can still
// find it. The caller MUST hold gatesMu.
func (s *Session) closeGateAnswerSlotLocked(id gate.ID) {
	slot, ok := s.gateAnswers[id]
	if !ok {
		return
	}
	close(slot)
	delete(s.gateAnswers, id)
}

// deliverGateAnswer hands a host-owned gate's answer to its opener.
//
// The send holds gatesMu, which is what makes closing a slot safe: a closer
// (CloseGate or shutdown) removes the slot from the map under the same lock, so
// this either finds it and sends, or does not find it and does nothing. There is
// no interleaving in which it sends on a closed channel.
//
// Holding the lock across a channel send is safe here only because the send
// cannot block: the slot is buffered with capacity one and written at most once
// (RespondGate removes the directory entry before delivering, so no second
// response can reach the same gate). The select/default is belt-and-braces — an
// opener that has already given up costs a discarded value, not a stuck session.
func (s *Session) deliverGateAnswer(id gate.ID, answer gate.Answer) {
	s.gatesMu.Lock()
	defer s.gatesMu.Unlock()
	slot, ok := s.gateAnswers[id]
	if !ok {
		return
	}
	select {
	case slot <- answer:
	default:
	}
}

// ActivateGate is called by the owner after its local blocker/continuation exists.
// It requires a preparing gate, appends the public GateOpened event via the strict
// gateAppender, stores the private route, and flips the entry to open so
// ListGates returns it. A failed append leaves the gate preparing. An unknown or
// non-preparing gate returns a typed *GateError.
func (s *Session) ActivateGate(ctx context.Context, id gate.ID, route gate.Route) error {
	s.gatesMu.Lock()
	defer s.gatesMu.Unlock()

	entry, ok := s.gates[id]
	if !ok {
		return &GateError{GateID: id, Kind: GateNotFound}
	}
	if entry.state != gatePreparing {
		return &GateError{GateID: id, Kind: GateNotReady}
	}

	stamped, err := s.factory.Stamp(event.Header{Coordinates: entry.coordinates})
	if err != nil {
		return &GateError{GateID: id, Kind: GateAppendFailed, Cause: err}
	}
	opened := event.GateOpened{Header: stamped, Gate: entry.gate}
	if err := s.gateAppender.AppendGateOpened(ctx, opened); err != nil {
		return &GateError{GateID: id, Kind: GateAppendFailed, Cause: err}
	}

	entry.route = route
	entry.state = gateOpen
	s.gates[id] = entry
	s.startGatePolicyTimerLocked(id, entry)
	return nil
}

// ListGates returns the public envelopes of all open gates — preparing, claiming,
// and closed entries are excluded. The returned slice is a snapshot; mutating it
// does not affect the directory.
func (s *Session) ListGates(context.Context) []gate.Gate {
	s.gatesMu.Lock()
	defer s.gatesMu.Unlock()
	out := make([]gate.Gate, 0, len(s.gates))
	for _, entry := range s.gates {
		if entry.state == gateOpen {
			out = append(out, entry.gate)
		}
	}
	return out
}

// CloseGate closes or abandons a session-owned gate without dispatching an
// answer to the resolver. Preparing gates were never public, so they are removed
// without a public GateResolved. Open gates are durably resolved before removal.
func (s *Session) CloseGate(ctx context.Context, id gate.ID, reason gate.CloseReason) error {
	s.gatesMu.Lock()
	entry, ok := s.gates[id]
	if !ok {
		s.gatesMu.Unlock()
		return &GateError{GateID: id, Kind: GateNotFound}
	}
	switch entry.state {
	case gatePreparing:
		s.stopGateTimerLocked(id)
		delete(s.gates, id)
		s.closeGateAnswerSlotLocked(id)
		s.gatesMu.Unlock()
		s.cancelPermissionReview(id)
		return nil
	case gateOpen:
		entry.state = gateClaiming
		s.gates[id] = entry
		s.gatesMu.Unlock()
	case gateClaiming, gateClosed:
		s.gatesMu.Unlock()
		return &GateError{GateID: id, Kind: GateNotReady}
	default:
		s.gatesMu.Unlock()
		return &GateError{GateID: id, Kind: GateNotReady}
	}

	resolved, err := s.buildGateClosed(entry, id, reason)
	if err != nil {
		s.revertClaiming(id)
		return err
	}
	if err := s.gateAppender.AppendGateResolved(ctx, resolved); err != nil {
		s.revertClaiming(id)
		return &GateError{GateID: id, Kind: GateAppendFailed, Cause: err}
	}

	s.gatesMu.Lock()
	s.stopGateTimerLocked(id)
	delete(s.gates, id)
	// An owner-closed gate is never answered, so its opener must not keep waiting
	// on a slot nothing will ever write to.
	s.closeGateAnswerSlotLocked(id)
	s.gatesMu.Unlock()
	// The owner closed the gate (design §15's second cancellation trigger). A
	// no-op for a gate with no active review.
	s.cancelPermissionReview(id)
	return nil
}

// checkGateCap returns a typed *GateError{GateCapacity} if the directory is at or
// above the configured cap (counting preparing + open + claiming). A zero cap
// means unlimited. The caller MUST hold gatesMu.
func (s *Session) checkGateCap() error {
	if s.gateCaps.MaxOpen <= 0 {
		return nil
	}
	count := 0
	for _, entry := range s.gates {
		if entry.state == gatePreparing || entry.state == gateOpen || entry.state == gateClaiming {
			count++
		}
	}
	if count >= s.gateCaps.MaxOpen {
		return &GateError{Kind: GateCapacity}
	}
	return nil
}

func (s *Session) resolveGatePolicy(g gate.Gate) (gate.ResponsePolicy, error) {
	policy := g.ResponsePolicy
	if policy.Timeout == 0 && policy.OnTimeout == "" && g.Kind == gate.KindPermission {
		policy.Timeout = 5 * time.Minute
		policy.OnTimeout = gate.PolicyRespond
		policy.Response = gate.ResponseTemplate{Action: string(gate.ApprovalDeny)}
	}
	if s.gateCaps.MaxTimeout > 0 && policy.Timeout > s.gateCaps.MaxTimeout {
		return gate.ResponsePolicy{}, &GateError{GateID: g.ID, Kind: GateCapacity}
	}
	switch policy.EffectiveAction() {
	case gate.PolicyWait:
		return policy, nil
	case gate.PolicyRespond:
		if policy.Timeout <= 0 || policy.Response.Action == "" {
			return gate.ResponsePolicy{}, &GateError{GateID: g.ID, Kind: GateActionInvalid}
		}
		return policy, nil
	case gate.PolicyModelDecide, gate.PolicySuspendSession:
		return gate.ResponsePolicy{}, &GateError{GateID: g.ID, Kind: GateActionInvalid}
	default:
		return gate.ResponsePolicy{}, &GateError{GateID: g.ID, Kind: GateActionInvalid}
	}
}

// startGatePolicyTimerLocked starts the activation-time timer for PolicyRespond.
// The caller MUST hold gatesMu.
func (s *Session) startGatePolicyTimerLocked(id gate.ID, entry gateEntry) {
	policy := entry.gate.ResponsePolicy
	if policy.EffectiveAction() != gate.PolicyRespond || policy.Timeout <= 0 {
		return
	}
	if s.gateTimers == nil {
		s.gateTimers = make(map[gate.ID]*time.Timer)
	}
	s.stopGateTimerLocked(id)
	template := policy.Response
	s.gateTimers[id] = time.AfterFunc(policy.Timeout, func() {
		_ = s.RespondGate(s.sessionCtx, gate.GateResponse{
			GateID: id,
			Action: template.Action,
			Values: cloneRawValues(template.Values),
			Source: gate.ResponseSource{Kind: gate.ResponseFromPolicy, Reason: "timeout"},
		})
	})
}

// stopGateTimerLocked stops and removes a policy timer. The caller MUST hold
// gatesMu.
func (s *Session) stopGateTimerLocked(id gate.ID) {
	if s.gateTimers == nil {
		return
	}
	if timer := s.gateTimers[id]; timer != nil {
		timer.Stop()
	}
	delete(s.gateTimers, id)
}

func cloneRawValues(in map[string]json.RawMessage) map[string]json.RawMessage {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]json.RawMessage, len(in))
	for k, v := range in {
		out[k] = append(json.RawMessage(nil), v...)
	}
	return out
}

// mintGateID mints a fresh gate.ID via the session's id generator.
func (s *Session) mintGateID() (gate.ID, error) {
	id, err := s.newID()
	if err != nil {
		return gate.ID{}, &GateError{Kind: GateAppendFailed, Cause: err}
	}
	return gate.ID(id), nil
}

// stampGateEvent stamps a GatePrepared event with the given coordinates and gate.
// The factory mints a fresh EventID and CreatedAt; the coordinates carry the
// session, loop, turn, and step identity the event's stepProfile requires.
func (s *Session) stampGateEvent(coords identity.Coordinates, g gate.Gate) (event.GatePrepared, error) {
	stamped, err := s.factory.Stamp(event.Header{Coordinates: coords})
	if err != nil {
		return event.GatePrepared{}, &GateError{Kind: GateAppendFailed, Cause: err}
	}
	return event.GatePrepared{Header: stamped, Gate: g}, nil
}

// RespondGate claims an open gate, durably appends GateResolved, and dispatches
// the translated command to the owning loop. It is durable-first: the GateResolved
// append happens BEFORE the command dispatch, so a crash after the append leaves
// the gate closed (not re-answerable) even if the command was not yet consumed.
// A failed append reverts the in-memory claim and leaves the gate answerable.
// Command dispatch uses s.sessionCtx (not the caller's ctx) so a client
// disconnect after the durable commit does not cancel delivery.
//
// A caller-supplied response whose Source.Kind is gate.ResponseFromClassifier
// is rejected outright, before any locking or validation: only the private
// respondFromClassifier method (called exclusively from the permission-review
// adapter once gate.CombinePermissionAssessments reports eligibility) may
// produce that provenance. gate.ResponseSource's fields are exported — a
// public caller CAN construct one with Kind set to ResponseFromClassifier —
// so this is a runtime, defense-in-depth check rather than a type-system
// guarantee; respondFromClassifier reaches the shared core below directly and
// is not subject to it.
func (s *Session) RespondGate(ctx context.Context, response gate.GateResponse) error {
	if response.Source.Kind == gate.ResponseFromClassifier {
		return &GateError{GateID: response.GateID, Kind: GateActionInvalid}
	}
	return s.respondGateCore(ctx, response, nil)
}

// respondGateCore performs the exactly-once gate claim, durable resolve, and
// command/answer delivery shared by every gate-response path (human,
// timeout-policy, and classifier). It is durable-first exactly as RespondGate
// documents.
//
// drift, when non-nil, is evaluated under gatesMu in the SAME critical
// section that checks the gate is open and claims it — there is no window
// between the drift check and the claim in which the world could move again.
// It reports whether the caller's evidence has gone stale; a true result
// silently no-ops (nil error) rather than reporting a fault, matching design
// §14.4's "a stale classifier response is an expected race, not a session
// fault."
func (s *Session) respondGateCore(ctx context.Context, response gate.GateResponse, drift func(gateEntry) bool) error {
	s.gatesMu.Lock()
	entry, ok := s.gates[response.GateID]
	if !ok {
		s.gatesMu.Unlock()
		return &GateError{GateID: response.GateID, Kind: GateNotFound}
	}
	if entry.state != gateOpen {
		s.gatesMu.Unlock()
		return &GateError{GateID: response.GateID, Kind: GateNotReady}
	}
	if drift != nil && drift(entry) {
		s.gatesMu.Unlock()
		return nil
	}
	if !validateGateAction(entry.gate, response.Action) {
		s.gatesMu.Unlock()
		return &GateError{GateID: response.GateID, Kind: GateActionInvalid}
	}
	translated, err := s.translateGateResponse(entry, response)
	if err != nil {
		s.gatesMu.Unlock()
		return err
	}
	entry.state = gateClaiming
	s.gates[response.GateID] = entry
	s.gatesMu.Unlock()

	resolved, err := s.buildGateResolved(entry, response, translated.audit)
	if err != nil {
		s.revertClaiming(response.GateID)
		return err
	}
	if err := s.gateAppender.AppendGateResolved(ctx, resolved); err != nil {
		s.revertClaiming(response.GateID)
		return &GateError{GateID: response.GateID, Kind: GateAppendFailed, Cause: err}
	}

	s.gatesMu.Lock()
	s.stopGateTimerLocked(response.GateID)
	delete(s.gates, response.GateID)
	s.gatesMu.Unlock()
	// The gate has resolved (design §15's first cancellation trigger, which
	// also covers a policy-timeout response — startGatePolicyTimerLocked
	// resolves through this exact path). A no-op for a gate with no active
	// review.
	s.cancelPermissionReview(response.GateID)

	// Delivery happens after the durable commit and after the entry is gone, so a
	// host-owned answer and a loop command are both unrepeatable.
	if translated.answer != nil {
		s.deliverGateAnswer(response.GateID, *translated.answer)
		return nil
	}
	_ = s.dispatchGateCommand(entry, translated.cmd)
	return nil
}

// respondFromClassifier is the ONLY place in the codebase that may construct
// a GateResponse carrying gate.ResponseFromClassifier provenance (design
// §14.5, §16.3, §25.2). It is unexported and called exclusively by the
// permission-review adapter after gate.CombinePermissionAssessments reports
// an eligible decision; there is no action parameter because Approve is the
// only action a classifier-originated response may ever carry — structurally,
// not by convention.
//
// basis is the common ReviewBasis (GateID/ToolExecutionID/ContextRevision/
// GatePolicyRevision/SecurityCeiling; ClassifierRevision and SubjectDigest are
// per-classifier and deliberately not part of the comparison) the eligible
// assessment was computed against. Immediately before claiming the gate —
// inside the SAME respondGateCore critical section that performs the claim,
// so there is no TOCTOU window — it is compared against the trusted
// entry.reviewBasis snapshot StartPermissionReview recorded, AND against the
// session's CURRENT s.permissionReviewPolicy.Revision (read fresh, not from
// the snapshot, so a policy revision changing under a long-running review is
// still caught). Any divergence — the gate having moved to a different tool
// execution, a context/policy/security-ceiling revision no longer matching,
// or the gate simply being gone/claimed/closed already — makes the response
// stale, which this method treats as an expected race rather than a fault:
// it returns (false, nil), exactly like every other non-fault stale-response
// outcome.
//
// The bool result is the review adapter's ONLY way to distinguish "the gate
// was actually resolved via this classifier-originated response" (true) from
// every no-op outcome — stale drift, or the gate already gone/claimed
// (false, nil) — versus a genuine append failure (false, non-nil error);
// design §16.2 requires this to audit AutoApproved/"stale" correctly, since
// the GATE's fate decides AutoApproved, never the classifier's own opinion.
// respondGateCore itself returns nil for BOTH the stale-drift no-op and a
// genuine success, so drift's closure captures which one happened locally —
// respondGateCore's own signature is untouched.
//
// observations (design §13.4, TOCTOU addendum) is checked BEFORE any of the
// above: verifyPermissionReviewObservations runs synchronously,
// DELIBERATELY OUTSIDE respondGateCore's gatesMu critical section, and a
// failed/mismatched/unverifiable observation returns (false, nil) — the same
// stale, silently-dropped contract as every drift outcome above — without
// ever calling respondGateCore at all. See verifyPermissionReviewObservations's
// own doc comment for why this one check cannot share drift's
// no-TOCTOU-window claim, and exactly what residual window it leaves.
func (s *Session) respondFromClassifier(ctx context.Context, basis gate.ReviewBasis, observations []gate.ObservationRequirement, reason string) (bool, error) {
	if basis.GateID.IsZero() || reason == "" {
		return false, nil
	}
	if !s.verifyPermissionReviewObservations(ctx, basis.GateID, observations) {
		return false, nil
	}
	response := gate.GateResponse{
		GateID: basis.GateID,
		Action: string(gate.ApprovalApprove),
		Source: gate.ResponseSource{Kind: gate.ResponseFromClassifier, Reason: reason},
	}
	stale := false
	drift := func(entry gateEntry) bool {
		live := entry.reviewBasis
		isStale := live.ToolExecutionID != basis.ToolExecutionID ||
			live.ContextRevision != basis.ContextRevision ||
			live.SecurityCeiling != basis.SecurityCeiling ||
			live.GatePolicyRevision != basis.GatePolicyRevision ||
			s.permissionReviewPolicy.Revision != basis.GatePolicyRevision
		if isStale {
			// design §18: a classifier-originated stale response feeds the
			// circuit breaker. The response itself is still silently
			// dropped exactly as before — only the bookkeeping is new.
			s.recordPermissionReviewStale(entry.coordinates)
			stale = true
		}
		return isStale
	}
	err := s.respondGateCore(ctx, response, drift)
	if err == nil {
		return !stale, nil
	}
	var gateErr *GateError
	if errors.As(err, &gateErr) && (gateErr.Kind == GateNotFound || gateErr.Kind == GateNotReady) {
		// The gate was already claimed, resolved, or never existed by the time
		// this classifier-originated response arrived: an expected race, not a
		// session fault.
		return false, nil
	}
	return false, err
}

// verifyPermissionReviewObservations rechecks every observation requirement
// (design §13.4, TOCTOU) an eligible classifier-originated response depends
// on, and reports whether respondFromClassifier may proceed to
// respondGateCore at all.
//
// Placement (the judgment call this addendum's plan explicitly calls out to
// document): the four PRE-EXISTING drift dimensions (ToolExecutionID/
// ContextRevision/SecurityCeiling/GatePolicyRevision) run INSIDE
// respondGateCore's gatesMu critical section because they are pure
// in-memory field comparisons — cheap, non-blocking, and therefore safe to
// perform while holding a lock every other gate response in the session
// also needs (respondGateCore's own doc comment: "so there is no TOCTOU
// window"). An observation recheck cannot share that placement: it calls a
// consumer-supplied EvidenceObservationVerifier that performs real I/O
// (re-stat a file, resolve a git ref against a live filesystem or VCS).
// Holding gatesMu for that I/O would trade a narrow, already-small TOCTOU
// window for a NEW, categorically worse problem — unbounded latency (or
// outright hang, if the verifier's own I/O blocks) while holding a lock
// every OTHER concurrent gate response in the session (human or classifier,
// for any gate) also needs to acquire, turning one slow/broken verifier
// into a session-wide stall. This method therefore runs synchronously but
// OUTSIDE the lock, immediately before respondFromClassifier calls
// s.respondGateCore — as late as reasonably possible before the claim
// without ever holding gatesMu across I/O.
//
// Residual TOCTOU window: the gap between this method returning true and
// respondGateCore's own lock-protected claim a few lines later — a handful
// of in-process instructions, not the "seconds of evidence-gathering plus
// classifier inference" window this whole mechanism exists to close. Design
// §13.4 requires the pre-existing symlink-swap, containment, grant-target,
// and sandbox checks to remain mandatory regardless — those, not this
// method, are what bound the residual window: a target that changes in the
// few instructions between this check and the claim is still caught by the
// SAME enforcement that already protects every non-classifier-approved tool
// call, exactly as design §13.4's closing sentence ("the eventual tool
// still consumes its original prepared artifact") requires.
//
// The nil-verifier cases are asymmetric BY DESIGN, not an oversight:
//   - len(observations) == 0: nothing was ever recorded (no target-sensitive
//     evidence tool ran, or none is registered at all for this session) —
//     there is nothing to recheck, so this returns true unconditionally,
//     with or without a configured verifier. Forcing every session with
//     classifiers configured to wire rig.WithPermissionReviewObservations
//     "just in case" would violate Interface Segregation for the common
//     case (a classifier whose evidence tools are all non-target-sensitive,
//     e.g. only glob/grep-style multi-match tools).
//   - len(observations) > 0 but s.permissionReviewObservationVerifier is
//     nil: a target-sensitive evidence tool DID run and DID record an
//     observation, but nothing is configured to recheck it. This can only
//     happen from a genuine consumer misconfiguration (an evidence tool
//     that records observations, wired without also calling
//     rig.WithPermissionReviewObservations) — see that option's own doc
//     comment for why Define()-time cannot catch this case today. Runtime
//     must never let this silently degrade to "recheck skipped, approval
//     proceeds" — that would be exactly the fail-open gap this whole
//     addendum exists to close. It is treated identically to a genuine
//     verifier-reported mismatch: stale, gate stays open, no session fault,
//     and (like every other stale outcome here) counted against the design
//     §18 circuit breaker so a persistent misconfiguration or attack is
//     eventually visible rather than silently absorbed forever.
func (s *Session) verifyPermissionReviewObservations(ctx context.Context, gateID gate.ID, observations []gate.ObservationRequirement) bool {
	if len(observations) == 0 {
		return true
	}
	if nilEvidenceObservationVerifier(s.permissionReviewObservationVerifier) {
		s.recordPermissionReviewStale(s.gateCoordinatesForStale(gateID))
		return false
	}
	// policy is assembled from the SAME two trusted, session-construction-time
	// sources hustleEvidenceRuntimeConfig already uses for the structurally
	// identical EvidenceContainmentPolicy (hustle.go): s.wsRoot (the
	// canonical workspace root) and s.permissionReviewSecurityCeiling (read
	// fresh off the session, exactly like the GatePolicyRevision comparison
	// above — never a value captured earlier in this specific review, so a
	// ceiling that has since moved on is still caught).
	policy := gate.EvidenceContainmentPolicy{ReadRoot: s.wsRoot, SecurityCeiling: s.permissionReviewSecurityCeiling}
	// Bound this specific I/O call (CLAUDE.md: "Every I/O call ... must use a
	// context.Context with a timeout or deadline. No unbounded blocking.").
	// ctx here traces back through respondFromClassifier ->
	// StartPermissionReview's beginPermissionReviewCancellation, which is a
	// bare context.WithCancel with no deadline anywhere in that chain — so
	// without this, a consumer-supplied verifier whose re-stat/git-ref-resolve
	// I/O hangs would hang this call forever. s.hustleLimits.AuditTimeout is
	// the same positive-guaranteed budget review_adapter.go's publish already
	// reuses for the adapter's own audit-publish call (StartPermissionReview
	// sets adapter.auditTimeout from this exact field, and only after
	// s.hustleController is confirmed non-nil, which hustleruntime.New
	// guarantees means AuditTimeout > 0) — mirrored here rather than adding a
	// new consumer-facing timeout knob for one recheck call. context.WithTimeout
	// still composes with ctx's own cancellation, so an outer review
	// cancellation (gate resolved, session shutdown) stops this just as
	// promptly as before.
	verifyCtx := ctx
	if s.hustleLimits.AuditTimeout > 0 {
		var cancel context.CancelFunc
		verifyCtx, cancel = context.WithTimeout(ctx, s.hustleLimits.AuditTimeout)
		defer cancel()
	}
	if err := s.permissionReviewObservationVerifier.VerifyEvidenceObservations(verifyCtx, policy, observations); err != nil {
		s.recordPermissionReviewStale(s.gateCoordinatesForStale(gateID))
		return false
	}
	return true
}

// gateCoordinatesForStale takes a short, non-blocking lock to read id's
// directory-entry coordinates for circuit-breaker bookkeeping ONLY — never
// while holding the lock across the observation verifier's own I/O (that
// call has already returned by the time this runs). A gate already gone by
// the time this is called (an ordinary race, not an error here) yields the
// zero identity.Coordinates, which recordPermissionReviewStale treats like
// any other coordinates value.
func (s *Session) gateCoordinatesForStale(id gate.ID) identity.Coordinates {
	s.gatesMu.Lock()
	defer s.gatesMu.Unlock()
	if entry, ok := s.gates[id]; ok {
		return entry.coordinates
	}
	return identity.Coordinates{}
}

func nilEvidenceObservationVerifier(value gate.EvidenceObservationVerifier) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	return reflected.Kind() == reflect.Pointer && reflected.IsNil()
}

// revertClaiming reverts a gate from claiming back to open after a failed
// durable append. It is safe to call after the entry was already removed.
func (s *Session) revertClaiming(id gate.ID) {
	s.gatesMu.Lock()
	defer s.gatesMu.Unlock()
	if entry, ok := s.gates[id]; ok && entry.state == gateClaiming {
		entry.state = gateOpen
		s.gates[id] = entry
	}
}

// validateGateAction reports whether action matches one of the gate's prompt
// controls. An empty action or a gate with no controls fails secure.
func validateGateAction(g gate.Gate, action string) bool {
	if action == "" || len(g.Prompt.Controls) == 0 {
		return false
	}
	for _, c := range g.Prompt.Controls {
		if c.Action == action {
			return true
		}
	}
	return false
}

// buildGateResolved stamps and builds the GateResolved event from the response
// and the resolved (already-redacted) audit.
func (s *Session) buildGateResolved(entry gateEntry, response gate.GateResponse, audit gate.ResponseAudit) (event.GateResolved, error) {
	stamped, err := s.factory.Stamp(event.Header{Coordinates: entry.coordinates})
	if err != nil {
		return event.GateResolved{}, &GateError{GateID: response.GateID, Kind: GateAppendFailed, Cause: err}
	}
	return event.GateResolved{
		Header:   stamped,
		GateID:   response.GateID,
		Resolver: entry.gate.Resolver,
		Reason:   gate.CloseAnswered,
		Action:   response.Action,
		Source:   response.Source,
		Audit:    audit,
	}, nil
}

func (s *Session) buildGateClosed(entry gateEntry, id gate.ID, reason gate.CloseReason) (event.GateResolved, error) {
	if reason == "" {
		reason = gate.CloseOwnerClosed
	}
	stamped, err := s.factory.Stamp(event.Header{Coordinates: entry.coordinates})
	if err != nil {
		return event.GateResolved{}, &GateError{GateID: id, Kind: GateAppendFailed, Cause: err}
	}
	return event.GateResolved{
		Header:   stamped,
		GateID:   id,
		Resolver: entry.gate.Resolver,
		Reason:   reason,
	}, nil
}

// dispatchGateCommand routes the translated command to the owning loop using
// s.sessionCtx (not the caller's ctx) so a client disconnect after the durable
// commit does not cancel delivery.
func (s *Session) dispatchGateCommand(entry gateEntry, cmd command.Command) error {
	l, ok := s.loopFor(entry.route.LoopID)
	if !ok {
		return &SessionError{Kind: SessionLoopNotFound}
	}
	return s.routeGate(s.sessionCtx, entry.route.LoopID, l, cmd)
}

// translatedGateResponse is the validated result of a response: what to deliver,
// and what to durably record.
//
// Exactly one of cmd and answer is set. A loop-owned gate yields a cmd addressed
// to the owning loop; a host-owned gate (hostOwnedGate) yields an answer for the
// opener blocked in AwaitGateAnswer. The two are separate fields rather than one
// interface because they are delivered through different seams and only the cmd
// has a loop to fail to find.
type translatedGateResponse struct {
	cmd    command.Command
	answer *gate.Answer
	audit  gate.ResponseAudit
}

// translateGateResponse validates the payload-specific parts of the response and
// builds the translated command, redacted audit, and exact approval action. It
// returns a typed *GateError on validation failure (invalid action, missing
// values, unknown kind).
func (s *Session) translateGateResponse(entry gateEntry, response gate.GateResponse) (translatedGateResponse, error) {
	cmdID, err := s.newCommandID()
	if err != nil {
		return translatedGateResponse{}, &GateError{GateID: response.GateID, Kind: GateAppendFailed, Cause: err}
	}
	hdr := command.Header{CommandID: cmdID, Agency: identity.AgencyUser, CreatedAt: s.stampNow()}
	route := command.GateRoute{
		Coordinates:     identity.Coordinates{SessionID: entry.coordinates.SessionID, LoopID: entry.coordinates.LoopID},
		GateID:          response.GateID,
		ToolExecutionID: uuid.UUID(entry.route.ToolExecutionID),
	}
	switch entry.gate.Kind {
	case gate.KindPermission:
		return s.translatePermissionResponse(hdr, route, entry.payload, response)
	case gate.KindAskUser:
		return s.translateAskUserResponse(hdr, route, response)
	case gate.KindForm, gate.KindOpenURL:
		// Both kinds are answerable only as host-owned gates. A form or open-url
		// gate declaring ResolverLoop has no loop-side resolver to route to, so it
		// is refused here rather than answered into a void.
		if !hostOwnedGate(entry.gate) {
			return translatedGateResponse{}, &GateError{GateID: response.GateID, Kind: GateKindMismatch}
		}
		if entry.gate.Kind == gate.KindForm {
			return translateFormResponse(entry.payload, response)
		}
		return translateOpenURLResponse(response)
	default:
		return translatedGateResponse{}, &GateError{GateID: response.GateID, Kind: GateKindMismatch}
	}
}

// translateFormResponse validates a form gate's response against the schema in
// its PAYLOAD — the authoritative record of what was asked — and builds the live
// answer plus the durable audit that records it.
//
// Accept is the only action that carries values. Decline and cancel are explicit
// non-answers: they record that a human refused or that the request was
// withdrawn, and any values submitted alongside them are ignored rather than
// validated, because there is no answer to validate. Both actions must still
// appear in the gate's Prompt.Controls (validateGateAction has already checked
// that), so an integration cannot be declined against its will by a control it
// never offered.
func translateFormResponse(payload gate.Payload, response gate.GateResponse) (translatedGateResponse, error) {
	formPayload, ok := formPayloadFromGatePayload(payload)
	if !ok {
		return translatedGateResponse{}, &GateError{GateID: response.GateID, Kind: GateKindMismatch}
	}

	switch response.Action {
	case gate.FormActionAccept:
		// ParseFormAnswers re-validates the schema before reading any value, so
		// bounds, field kinds (FieldMultiSelect is refused), required fields,
		// unknown fields, over-long values, and select options are all enforced
		// here and not trusted from the opener.
		answers, err := gate.ParseFormAnswers(formPayload.Schema, response.Values)
		if err != nil {
			return translatedGateResponse{}, &GateError{GateID: response.GateID, Kind: GateActionInvalid, Cause: err}
		}
		return translatedGateResponse{
			answer: &gate.Answer{
				GateID: response.GateID,
				Action: response.Action,
				Values: answers,
				Source: response.Source,
			},
			audit: gate.NewFormAudit(formPayload.Schema, answers),
		}, nil
	case gate.FormActionDecline, gate.FormActionCancel:
		return translatedGateResponse{
			answer: &gate.Answer{
				GateID: response.GateID,
				Action: response.Action,
				Source: response.Source,
			},
		}, nil
	default:
		return translatedGateResponse{}, &GateError{GateID: response.GateID, Kind: GateActionInvalid}
	}
}

// translateOpenURLResponse builds the live answer for an open-url gate.
//
// It carries NO audit, and that is a deliberate omission rather than a gap. An
// open-url answer has nothing to redact and nothing to add: the human either
// reported completion or did not, which GateResolved.Action already records in
// the clear, and the only other fact about the request — its DisplayOrigin — is
// already durable in the payload. The URL itself must never reach a durable
// record (gate.OpenURLPayload), and a decoded payload does not even have one. An
// audit member here would be an empty struct whose only effect would be to add a
// codec arm that could later be widened to hold the very thing that must not be
// stored. A nil audit is the same choice permission "deny" already makes.
func translateOpenURLResponse(response gate.GateResponse) (translatedGateResponse, error) {
	switch response.Action {
	case gate.FormActionAccept, gate.FormActionDecline, gate.FormActionCancel:
		return translatedGateResponse{
			answer: &gate.Answer{
				GateID: response.GateID,
				Action: response.Action,
				Source: response.Source,
			},
		}, nil
	default:
		return translatedGateResponse{}, &GateError{GateID: response.GateID, Kind: GateActionInvalid}
	}
}

func formPayloadFromGatePayload(payload gate.Payload) (gate.FormPayload, bool) {
	switch v := payload.(type) {
	case gate.FormPayload:
		return v, true
	case *gate.FormPayload:
		if v == nil {
			return gate.FormPayload{}, false
		}
		return *v, true
	default:
		return gate.FormPayload{}, false
	}
}

// translatePermissionResponse maps a permission gate response to the exact
// approval action and its command. The action string must be one of the three
// exact gate.ApprovalAction values (ParseApprovalAction is the same single
// validation source the strict wire decoder uses); anything else fails secure.
// Approve actions travel on ApproveToolCall, Deny on DenyToolCall. The durable
// audit stores the payload request's requirement descriptions and — for a
// workspace approval, which persists them — the displayed candidate
// descriptions. Descriptions only: no tokens, no raw arguments, and the
// response carries no values to smuggle either.
func (s *Session) translatePermissionResponse(hdr command.Header, route command.GateRoute, payload gate.Payload, response gate.GateResponse) (translatedGateResponse, error) {
	action, ok := gate.ParseApprovalAction(response.Action)
	if !ok {
		return translatedGateResponse{}, &GateError{GateID: response.GateID, Kind: GateActionInvalid}
	}
	permPayload, ok := permissionPayloadFromGatePayload(payload)
	if !ok {
		return translatedGateResponse{}, &GateError{GateID: response.GateID, Kind: GateActionInvalid}
	}
	switch action {
	case gate.ApprovalApprove, gate.ApprovalApproveAlwaysWorkspace:
		return translatedGateResponse{
			cmd:   command.ApproveToolCall{Header: hdr, GateRoute: route, Action: action},
			audit: permissionAuditFor(permPayload.Request, action),
		}, nil
	case gate.ApprovalDeny:
		return translatedGateResponse{cmd: command.DenyToolCall{Header: hdr, GateRoute: route}}, nil
	default:
		return translatedGateResponse{}, &GateError{GateID: response.GateID, Kind: GateActionInvalid}
	}
}

// permissionAuditFor builds the redacted durable audit for an approval: the
// displayed requirement descriptions always, and the displayed reusable
// candidate descriptions only for the workspace approval that persists them.
func permissionAuditFor(displayed tool.Request, action gate.ApprovalAction) gate.PermissionAudit {
	audit := gate.PermissionAudit{}
	for _, requirement := range displayed.Requirements {
		audit.RequirementDescriptions = append(audit.RequirementDescriptions, requirement.Description)
	}
	if action == gate.ApprovalApproveAlwaysWorkspace {
		for _, requirement := range displayed.Requirements {
			for _, candidate := range requirement.Candidates {
				audit.CandidateDescriptions = append(audit.CandidateDescriptions, candidate.Description)
			}
		}
	}
	return audit
}

// translateAskUserResponse builds a ProvideUserInput from an ask-user gate
// response. It extracts the answer from Values["answer"].
func (s *Session) translateAskUserResponse(hdr command.Header, route command.GateRoute, response gate.GateResponse) (translatedGateResponse, error) {
	var answer string
	if raw, ok := response.Values["answer"]; ok {
		if err := json.Unmarshal(raw, &answer); err != nil {
			return translatedGateResponse{}, &GateError{GateID: response.GateID, Kind: GateActionInvalid, Cause: err}
		}
	}
	preview := answer
	if len(preview) > 80 {
		preview = preview[:80]
	}
	return translatedGateResponse{
		cmd:   command.ProvideUserInput{Header: hdr, GateRoute: route, Answer: answer},
		audit: gate.AskUserAudit{AnswerPreview: preview},
	}, nil
}

func permissionPayloadFromGatePayload(payload gate.Payload) (gate.PermissionPayload, bool) {
	switch v := payload.(type) {
	case gate.PermissionPayload:
		return v, true
	case *gate.PermissionPayload:
		if v == nil {
			return gate.PermissionPayload{}, false
		}
		return *v, true
	default:
		return gate.PermissionPayload{}, false
	}
}
