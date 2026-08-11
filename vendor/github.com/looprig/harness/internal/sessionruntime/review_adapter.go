package sessionruntime

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"time"

	"github.com/looprig/harness/internal/hustleruntime"
	"github.com/looprig/harness/internal/loopruntime"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/identity"
)

// permissionReviewHustleRunner is the narrow seam the review adapter uses to
// run one classifier's bound Hustle. It has the exact same method shape as
// compactionHustleRunner (internal/sessionruntime/compaction_adapter.go) —
// both are satisfied by *hustleruntime.Controller — so review and compaction
// share the one session-wide Hustle runtime without either adapter depending
// on a generic session runner. It is declared again here, rather than reused,
// so the two call sites stay free to diverge (Interface Segregation): a
// future requirement on one must not force a method onto the other.
type permissionReviewHustleRunner interface {
	RunAndFinalize(context.Context, hustle.Request, hustleruntime.ValidateResult, hustleruntime.Finalizer) error
}

// permissionReviewResponder is the narrow seam through which an eligible
// combined review decision reaches gate state. It is satisfied by
// *Session.respondFromClassifier — the ONLY method in the codebase permitted
// to stamp gate.ResponseFromClassifier provenance — and is declared here,
// package-private, so nothing outside sessionruntime can supply a
// substitute: a consumer cannot construct an adapter whose "responder" skips
// the private claim/drift-check path (design §25.2).
//
// The bool result distinguishes "the classifier-originated response actually
// resolved the gate" (true) from every expected no-op — the gate already
// resolved/closed, or the response arrived stale against the live basis
// (false, nil error) — from a genuine append failure (false, non-nil error).
// review() needs this distinction to audit AutoApproved/stale correctly
// (design §16.2): the GATE's fate decides AutoApproved, never the
// classifier's own opinion.
//
// observations is classifierApprovalBasis' aggregated set of
// gate.ObservationRequirement values every contributing classifier's own
// evidence gathering recorded (design §13.4, TOCTOU). respondFromClassifier
// treats a mismatch or unverifiable observation among them exactly like a
// basis-drift mismatch: the response is dropped as stale, folded into the
// same (false, nil) no-op outcome above, never a distinct return shape.
type permissionReviewResponder interface {
	respondFromClassifier(ctx context.Context, basis gate.ReviewBasis, observations []gate.ObservationRequirement, reason string) (bool, error)
}

// permissionReviewAuditPublisher is the checked durable-append seam
// PermissionReviewStarted/PermissionReviewCompleted flow through (design
// §16). It is satisfied directly by *hub.Hub (PublishInternalEventChecked) —
// the SAME private audit-record path internal/hustleruntime/audit.go already
// uses for HustleStarted/Completed/Failed (its Audit collaborator is wired
// from s.hub directly, not through *Session; StartPermissionReview mirrors
// that exact wiring for adapter.publisher).
//
// This is deliberately NOT PublishEventChecked (the PUBLIC-only path
// Hub.validatePublicPublication enforces): PermissionReviewStarted/Completed
// are BOTH Internal-visibility, so publishing them through the public path
// unconditionally fails at the boundary — which is exactly the Phase 6
// spec-compliance bug this comment now documents the fix for. Internal
// publication is safe here for the same reason it is safe for Hustle
// lifecycle events (Hub.PublishInternalEventChecked's own doc comment): it
// deliberately bypasses quiescence mutation and subscriber delivery, relying
// on the event's own activity/blocking state being owned elsewhere. A
// permission review always runs inside gate evaluation / tool execution,
// which the session already tracks as active through the ordinary gate and
// loop lifecycle — there is no quiescence hole, and nothing needs to
// subscribe to a private audit-only event.
type permissionReviewAuditPublisher interface {
	PublishInternalEventChecked(ctx context.Context, ev event.Event) error
}

// permissionReviewAuditStamper mints the EventID/CreatedAt for a review
// audit event's Header, mirroring every other durable event's Factory.Stamp
// usage (gates.go's buildGateResolved). Satisfied directly by *event.Factory.
type permissionReviewAuditStamper interface {
	Stamp(h event.Header) (event.Header, error)
}

// permissionReviewFaultReporter is the review adapter's audit-append fault
// seam. It reuses the EXACT session fault path Task 14's ordinary Hustle
// faults already use (sessionHustleFaultReporter, internal/sessionruntime/
// hustle.go) rather than inventing a parallel one: design §17 buckets
// "durable-event append failure" as an Integrity failure with existing fault
// semantics. An ordinary expected classifier outcome (needs_human/failed/
// timed_out/cancelled/stale/not_applicable/allowed) never reaches this seam
// — only a failure to durably record THAT outcome's own audit event does.
type permissionReviewFaultReporter interface {
	ReportFault(ctx context.Context, cause error)
}

// permissionReviewAuditError wraps an audit-append failure with the gate
// identity it was auditing, without retaining classifier input, evidence,
// output, or rationale — the underlying cause is whatever the durable
// publish path itself already produced (never raw provider/tool content).
type permissionReviewAuditError struct {
	GateID gate.ID
	Cause  error
}

func (e *permissionReviewAuditError) Error() string {
	return "sessionruntime: permission review audit append failed"
}

func (e *permissionReviewAuditError) Unwrap() error { return e.Cause }

// permissionReviewAdapter is the session-wide (not per-loop, unlike
// compaction) facility that begins classifier review once a permission gate
// has activated and the loop actor has handed off a
// loopruntime.PermissionReviewRequest via Session.StartPermissionReview.
//
// It implements the whole of design §14.3 ("Start review"): evaluate
// classifier applicability, build one subject+basis per applicable
// classifier, schedule that classifier's Hustle run (steps 1-2+4);
// validate every result and combine decisions via
// gate.CombinePermissionAssessments (steps 5-6); and, only when the combined
// decision is eligible, attempt a classifier-originated response through
// permissionReviewResponder, which itself re-reads live gate state and
// recomputes the entire basis immediately before claiming (step 7-8). Every
// other outcome — not applicable, any classifier status other than allowed,
// an invalid/mismatched assessment, or an ineligible combined decision —
// reaches no responder call at all, so design §25.4 ("every ambiguous,
// invalid, missing, stale, cancelled, or unknown condition preserves the
// human gate") holds by construction, not by error handling.
//
// It also publishes secret-free PermissionReviewStarted/PermissionReviewCompleted
// audit events (design §14.3 step 3, §16): one Started per applicable
// classifier right before its Hustle is scheduled, and one Completed per
// classifier covering every terminal status reviewOne/review can produce.
// Publication uses the SAME optional, assigned-after-construction pattern as
// observer (see below) — publisher/stamper/faults are all nil-safe no-ops
// when unset, so every pre-Task-17 direct newPermissionReviewAdapter caller
// (and every test constructing one without them) keeps working unchanged;
// only Session.StartPermissionReview, the sole production call site, wires
// all three.
type permissionReviewAdapter struct {
	runner      permissionReviewHustleRunner
	classifiers gate.PermissionClassifierSet
	policy      gate.PermissionReviewPolicy
	responder   permissionReviewResponder
	// observer, when non-nil, receives one terminal circuit-breaker-relevant
	// summary of each review() call (design §18). It is assigned directly by
	// Session.StartPermissionReview after construction — see
	// permissionReviewOutcomeObserver's doc comment (review_state.go) for why
	// it is not a newPermissionReviewAdapter parameter.
	observer permissionReviewOutcomeObserver
	// publisher/stamper, when both non-nil, are used to durably (checked)
	// append every PermissionReviewStarted/PermissionReviewCompleted audit
	// event this adapter produces. Either being nil is a fail-open no-op on
	// AUDIT PUBLICATION ONLY — it never changes review's gate-response
	// decisions, matching observer's own optionality rationale.
	publisher permissionReviewAuditPublisher
	stamper   permissionReviewAuditStamper
	// faults, when non-nil, receives ONLY audit-append failures (a Stamp or
	// PublishEventChecked error) — never an ordinary expected classifier
	// outcome. See permissionReviewFaultReporter's doc comment.
	faults permissionReviewFaultReporter
	// auditTimeout bounds each audit publish call, decoupled from review's
	// own cancellation (see publish) so a cancelled/timed-out review can
	// still durably record its OWN "cancelled"/"timed_out" completion event.
	// Zero (the default for a directly constructed adapter that never wires
	// it) means unbounded — Session.StartPermissionReview always sets it
	// from the same s.hustleLimits.AuditTimeout the session's Hustle
	// controller itself required to be positive at construction.
	auditTimeout time.Duration
}

// permissionReviewAdapterField names the collaborator newPermissionReviewAdapter
// rejected, mirroring compactionAdapterField's shape.
type permissionReviewAdapterField string

const (
	permissionReviewAdapterFieldRunner      permissionReviewAdapterField = "runner"
	permissionReviewAdapterFieldClassifiers permissionReviewAdapterField = "classifiers"
	permissionReviewAdapterFieldPolicy      permissionReviewAdapterField = "policy"
	permissionReviewAdapterFieldResponder   permissionReviewAdapterField = "responder"
)

type permissionReviewAdapterError struct{ Field permissionReviewAdapterField }

func (e *permissionReviewAdapterError) Error() string {
	return "sessionruntime: invalid permission review adapter field " + string(e.Field)
}

// newPermissionReviewAdapter validates its collaborators up front so a
// misconfigured adapter fails before the first gate rather than deep inside a
// fire-and-forget goroutine where nothing observes the error.
func newPermissionReviewAdapter(
	runner permissionReviewHustleRunner,
	classifiers gate.PermissionClassifierSet,
	policy gate.PermissionReviewPolicy,
	responder permissionReviewResponder,
) (*permissionReviewAdapter, error) {
	if nilPermissionReviewRunner(runner) {
		return nil, &permissionReviewAdapterError{Field: permissionReviewAdapterFieldRunner}
	}
	if len(classifiers.Classifiers()) == 0 {
		return nil, &permissionReviewAdapterError{Field: permissionReviewAdapterFieldClassifiers}
	}
	if policy.Revision == "" {
		return nil, &permissionReviewAdapterError{Field: permissionReviewAdapterFieldPolicy}
	}
	if nilPermissionReviewResponder(responder) {
		return nil, &permissionReviewAdapterError{Field: permissionReviewAdapterFieldResponder}
	}
	return &permissionReviewAdapter{runner: runner, classifiers: classifiers, policy: policy, responder: responder}, nil
}

func nilPermissionReviewRunner(value permissionReviewHustleRunner) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	return reflected.Kind() == reflect.Pointer && reflected.IsNil()
}

func nilPermissionReviewResponder(value permissionReviewResponder) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	return reflected.Kind() == reflect.Pointer && reflected.IsNil()
}

// review evaluates every registered classifier's applicability against req,
// schedules a Hustle run for each applicable one (in registration order),
// combines every classifier's terminal outcome via
// gate.CombinePermissionAssessments, and — only when that combined decision
// is eligible — attempts a classifier-originated gate response through
// a.responder.
//
// The caller (Session.StartPermissionReview) MUST invoke this on its own
// goroutine: review blocks for as long as each scheduled RunAndFinalize call
// takes, and doing that inline would block whatever called it. That
// obligation is exactly the one the loop actor places on
// StartPermissionReview itself (gate.go's permissionReviewStarter) — review
// simply keeps it.
//
// A live-only ReviewContext is required. req.ReviewContext.ContextRevision ==
// "" means no review context was captured for this turn (turnConfig.
// reviewContext was nil — internal/loopruntime/review_context.go), which
// fails closed as "nothing to review" rather than guessing at defaults.
// Every classifier's Hustle is scheduled and awaited IN FULL, even once an
// earlier classifier's outcome has already made the combined decision
// impossible to reverse (design §15's sixth cancellation trigger: "a
// conjunction member makes auto-approval impossible and remaining results
// are not needed for configured audit"). Skipping the remaining classifiers
// once eligibility is already lost would change gate.CombinePermissionAssessments'
// outcome-attribution (it requires exactly one outcome per registered
// classifier to distinguish ReviewDecisionClassifierStatus from
// ReviewDecisionInvalidAssessment) and there is currently no "configured
// audit" concept that would tell this adapter whether the remaining results
// are actually needed. Skipping never widens approval — the gate stays
// human either way — so this is a deliberately deferred efficiency
// optimization, not a correctness or security gap; reviewCtx (armed by
// Session.StartPermissionReview) still lets every OTHER trigger (gate
// resolve, owner close, loop/turn interrupt, session shutdown) stop a
// still-running classifier immediately.
func (a *permissionReviewAdapter) review(ctx context.Context, req loopruntime.PermissionReviewRequest) {
	if req.ReviewContext.ContextRevision == "" {
		return
	}
	classifiers := a.classifiers.Classifiers()
	outcomes := make([]gate.PermissionAssessmentOutcome, 0, len(classifiers))
	for _, classifier := range classifiers {
		outcomes = append(outcomes, a.reviewOne(ctx, req, classifier))
	}
	decision := gate.CombinePermissionAssessments(a.policy, a.classifiers, outcomes)
	a.reportBreakerOutcome(req, decision)

	applied := false
	if decision.Eligible {
		if basis, observations, reason, ok := classifierApprovalBasis(classifiers, outcomes); ok {
			responded, err := a.responder.respondFromClassifier(ctx, basis, observations, reason)
			if err != nil {
				slog.WarnContext(ctx, "sessionruntime: classifier-originated gate response failed",
					"gate_id", req.GateID, "error", err)
			}
			applied = responded
		}
		// !ok is structurally unreachable given decision.Eligible == true
		// (eligibility requires at least one applicable+allowed outcome), but
		// fails closed rather than attempting a response with no evidence to
		// justify it; applied stays false, so every Allowed classifier below
		// is still audited (as "stale", never "allowed").
	}
	a.publishDeferredCompletions(ctx, req, classifiers, outcomes, decision.Eligible, applied)
}

// publishDeferredCompletions audits every classifier whose OWN Hustle
// validated successfully (outcome.Status == gate.ReviewStatusAllowed).
// reviewOne already audited every other terminal status (not_applicable,
// failed, timed_out, cancelled) as a side effect of producing that outcome,
// since those are fully determined per classifier without the combined
// decision. An Allowed classifier's audit status, by contrast, can only be
// resolved here (design §16.2):
//
//   - "allowed" (AutoApproved=true) only when the whole combined decision was
//     eligible AND the classifier-originated gate response actually applied.
//   - "stale" (AutoApproved=false, no risk/authorization/categories) when the
//     combined decision was eligible but the response was silently dropped —
//     the GATE's fate decides AutoApproved, never the classifier's own
//     opinion, even though this classifier's own assessment was itself
//     eligible.
//   - "needs_human" whenever the combined decision was NOT eligible,
//     regardless of which classifier's own assessment (or which OTHER
//     classifier's non-allowed status) caused that:
//     gate.CombinePermissionAssessments reports one combined reason, not a
//     per-classifier attribution, so every individually-allowed classifier
//     shares the same "the review, collectively, needs a human" audit
//     status — carrying its OWN risk/authorization/categories, exactly as
//     design §16.2 permits for needs_human.
func (a *permissionReviewAdapter) publishDeferredCompletions(
	ctx context.Context,
	req loopruntime.PermissionReviewRequest,
	classifiers []gate.PermissionClassifier,
	outcomes []gate.PermissionAssessmentOutcome,
	eligible bool,
	applied bool,
) {
	for index, outcome := range outcomes {
		if !outcome.Applicable || outcome.Status != gate.ReviewStatusAllowed || index >= len(classifiers) {
			continue
		}
		switch {
		case eligible && applied:
			a.publishReviewCompleted(ctx, req, classifiers[index], gate.ReviewStatusAllowed, outcome.Assessment, true)
		case eligible:
			a.publishReviewCompleted(ctx, req, classifiers[index], gate.ReviewStatusStale, gate.PermissionAssessment{}, false)
		default:
			a.publishReviewCompleted(ctx, req, classifiers[index], gate.ReviewStatusNeedsHuman, outcome.Assessment, false)
		}
	}
}

// reportBreakerOutcome reports one review() call's terminal
// circuit-breaker-relevant summary to a.observer (design §18), when one is
// configured. It deliberately skips ReviewDecisionNoApplicableClassifier: no
// classifier applying at all is not a reviewed-and-rejected/failed attempt —
// counting it would trip the breaker on ordinary gates that no classifier was
// ever meant to look at.
func (a *permissionReviewAdapter) reportBreakerOutcome(req loopruntime.PermissionReviewRequest, decision gate.ReviewDecision) {
	if a.observer == nil || decision.Reason == gate.ReviewDecisionNoApplicableClassifier {
		return
	}
	a.observer.observePermissionReviewOutcome(req.ReviewContext.Coordinates, reviewBreakerOutcome{
		SubjectDigest: subjectContentDigest(req.Request),
		Reason:        decision.Reason,
		Eligible:      decision.Eligible,
	})
}

// reviewOne builds ONE classifier's subject+basis (design §14.3 step 2),
// checks applicability (step 1), and — only when applicable — marshals the
// input, schedules the classifier's Hustle, and validates its result (steps
// 4-5). It ALWAYS returns exactly one gate.PermissionAssessmentOutcome for
// this classifier — never skips — because
// gate.CombinePermissionAssessments requires exactly one outcome per
// registered classifier, in registration order, to even structurally
// validate.
//
// Every failure mode (invalid subject, marshal error, a non-allowed
// terminal Hustle outcome, a validator that rejects the result) is logged
// and reported as a non-allowed outcome rather than propagated: there is no
// caller waiting on this fire-and-forget goroutine, and a non-allowed
// outcome can never itself contribute to eligibility (see the type doc) —
// CombinePermissionAssessments fails the WHOLE combined decision closed the
// moment any applicable classifier's status is not allowed.
func (a *permissionReviewAdapter) reviewOne(ctx context.Context, req loopruntime.PermissionReviewRequest, classifier gate.PermissionClassifier) gate.PermissionAssessmentOutcome {
	basis := gate.ReviewBasis{
		GateID:             req.GateID,
		ToolExecutionID:    req.ToolExecutionID,
		ContextRevision:    req.ReviewContext.ContextRevision,
		GatePolicyRevision: req.ReviewContext.GatePolicyRevision,
		ClassifierRevision: classifier.Revision(),
		SecurityCeiling:    req.ReviewContext.SecurityCeiling,
	}
	subject, err := gate.NewPermissionReviewSubject(basis, req.Request, req.ReviewContext)
	if err != nil {
		slog.WarnContext(ctx, "sessionruntime: permission review subject invalid; failing classifier closed",
			"classifier", classifier.Name(), "gate_id", req.GateID, "error", err)
		// No valid subject can be constructed for this classifier's slot at
		// all: leave the outcome's ClassifierRevision empty so it can never
		// match this classifier's registered revision, which makes
		// CombinePermissionAssessments' very first structural check fail the
		// whole decision closed. No PermissionReviewStarted was ever
		// published (applicability was never even confirmed), but the
		// attempt is still audited as failed — see the type doc's "no
		// silent gap" requirement.
		a.publishReviewCompleted(ctx, req, classifier, gate.ReviewStatusFailed, gate.PermissionAssessment{}, false)
		return gate.PermissionAssessmentOutcome{Applicable: true, Status: gate.ReviewStatusFailed}
	}
	applies, paniced := callClassifierApplies(classifier, subject)
	if paniced {
		// classifier.Applies returns a bare bool, so — unlike MarshalInput/
		// ValidateResult, whose (..., error) signatures let pkg/gate's
		// frozenPermissionClassifier absorb a panic at the registry boundary
		// without touching the public PermissionClassifier contract — a panic
		// here has no return-value channel to travel through except this call
		// site. Treat it as "applicable but failed", never as an ordinary
		// not-applicable result: an ambiguous applicability determination must
		// not let a DIFFERENT classifier's allow silently decide the whole
		// gate (design §11 "a non-applicable classifier contributes nothing"
		// only holds for a genuine, confidently-determined non-applicability).
		slog.WarnContext(ctx, "sessionruntime: permission review applicability check panicked; failing classifier closed",
			"classifier", classifier.Name(), "classifier_revision", classifier.Revision(), "gate_id", req.GateID)
		a.publishReviewCompleted(ctx, req, classifier, gate.ReviewStatusFailed, gate.PermissionAssessment{}, false)
		return gate.PermissionAssessmentOutcome{Subject: subject, Applicable: true, Status: gate.ReviewStatusFailed}
	}
	if !applies {
		a.publishReviewCompleted(ctx, req, classifier, gate.ReviewStatusNotApplicable, gate.PermissionAssessment{}, false)
		return gate.PermissionAssessmentOutcome{Subject: subject, Status: gate.ReviewStatusNotApplicable}
	}
	input, err := classifier.MarshalInput(subject)
	if err != nil {
		// classifier.MarshalInput is entirely classifier-owned and returns an
		// unconstrained error (unlike NewPermissionReviewSubject's bounded,
		// content-free typed error above) — a classifier implementation could
		// embed raw subject/command/context content in its error text. Log
		// only the classifier identity, never err itself, matching the
		// content-free discipline classifyReviewFailureStatus already applies
		// to every other failure path in this file.
		slog.WarnContext(ctx, "sessionruntime: permission review input marshal failed; failing classifier closed",
			"classifier", classifier.Name(), "classifier_revision", classifier.Revision(), "gate_id", req.GateID)
		a.publishReviewCompleted(ctx, req, classifier, gate.ReviewStatusFailed, gate.PermissionAssessment{}, false)
		return gate.PermissionAssessmentOutcome{Subject: subject, Applicable: true, Status: gate.ReviewStatusFailed}
	}

	// design §14.3 step 3: the start event is published once applicability is
	// confirmed and the input is ready, strictly before step 4 schedules the
	// Hustle below.
	a.publishReviewStarted(ctx, req, classifier)

	var assessment gate.PermissionAssessment
	var runErr error
	allowed := false
	validate := func(_ context.Context, result hustle.Result) error {
		parsed, validationErr := classifier.ValidateResult(subject, result)
		if validationErr != nil {
			return validationErr
		}
		assessment = parsed
		return nil
	}
	finish := func(_ context.Context, outcome hustle.Outcome) error {
		allowed = outcome.Err == nil && outcome.Result != nil
		runErr = outcome.Err
		return nil
	}
	runRequest := hustle.Request{
		Name:  classifier.Name(),
		Cause: identity.Cause{Coordinates: req.ReviewContext.Coordinates},
		Input: input,
		// basis.SecurityCeiling is this review's frozen basis value, captured
		// once above from req.ReviewContext.SecurityCeiling (design §21) — the
		// exact per-review ceiling any evidence tools this classifier's Hustle
		// binds must be authorized against. It is never a session-wide
		// constant (hustle.Request.SecurityCeiling's doc comment), so a
		// classifier with no evidence-tool concept simply never reads it.
		SecurityCeiling: basis.SecurityCeiling,
	}
	// design §13.4 (TOCTOU): a fresh collector per classifier run, attached
	// only to the context THIS RunAndFinalize call receives (never req's or
	// review()'s own ctx directly) — see hustleruntime.ObservationCollector's
	// doc comment. Any target-sensitive evidence tool this classifier's
	// Hustle invokes records into it via the evidence runtime; nothing reads
	// it until after RunAndFinalize returns below.
	observations := hustleruntime.NewObservationCollector()
	runCtx := hustleruntime.WithObservationCollector(ctx, observations)
	if err := a.runner.RunAndFinalize(runCtx, runRequest, validate, finish); err != nil {
		if runErr == nil {
			// The finalizer above never ran (a pre-ownership admission/preflight
			// rejection) — RunAndFinalize's own returned error is the only
			// signal available for classification.
			runErr = err
		}
		slog.WarnContext(ctx, "sessionruntime: permission review classifier run failed",
			"classifier", classifier.Name(), "gate_id", req.GateID, "error", err)
	}
	if !allowed {
		// The combined-decision input (gate.PermissionAssessmentOutcome.Status)
		// stays the coarse ReviewStatusFailed unconditionally — unchanged from
		// Task 15/16 — regardless of the finer audit classification below; only
		// the AUDITED status distinguishes cancelled/timed_out/failed. Any
		// observations gathered before this classifier's run ultimately failed
		// are discarded (never carried onto the outcome) — see
		// PermissionAssessmentOutcome's own doc comment for why that is exactly
		// right, not a gap.
		a.publishReviewCompleted(ctx, req, classifier, classifyReviewFailureStatus(ctx, runErr), gate.PermissionAssessment{}, false)
		return gate.PermissionAssessmentOutcome{Subject: subject, Applicable: true, Status: gate.ReviewStatusFailed}
	}
	return gate.PermissionAssessmentOutcome{
		Subject: subject, Applicable: true, Status: gate.ReviewStatusAllowed, Assessment: assessment,
		Observations: observations.Observations(),
	}
}

// callClassifierApplies invokes classifier.Applies(subject) with a local
// recover so a panicking, trusted-but-fallible classifier implementation
// cannot crash the goroutine review() runs on (Session.StartPermissionReview
// spawns review() fire-and-forget, with nothing else on that goroutine to
// catch an unrecovered panic — an unrecovered panic on ANY goroutine
// terminates the whole process, not just this one gate's review). paniced=true
// must be treated by the caller as "applicable but failed", never as an
// ordinary not-applicable result — see the call site's comment.
func callClassifierApplies(classifier gate.PermissionClassifier, subject gate.PermissionReviewSubject) (applies bool, paniced bool) {
	defer func() {
		if recover() != nil {
			applies = false
			paniced = true
		}
	}()
	return classifier.Applies(subject), false
}

// classifyReviewFailureStatus derives the finer-grained audit status for a
// classifier whose Hustle run did not produce an allowed assessment,
// distinguishing an outer review-lifecycle cancellation/timeout (design §15
// — this classifier's own reviewCtx, cancelled by gate resolve/owner close/
// loop interrupt/session shutdown/policy timeout) from the Hustle runtime's
// own typed classification (hustleruntime.RunError.ReasonCode /
// QueueFailureError.Reason) from a generic mechanical failure. It never
// inspects err's message text — only typed reason codes — so no provider or
// tool content can leak through this classification.
func classifyReviewFailureStatus(ctx context.Context, err error) gate.ReviewStatus {
	if ctx != nil {
		switch {
		case errors.Is(ctx.Err(), context.Canceled):
			return gate.ReviewStatusCancelled
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			return gate.ReviewStatusTimedOut
		}
	}
	var runErr *hustleruntime.RunError
	if errors.As(err, &runErr) {
		switch runErr.ReasonCode {
		case hustle.ReasonCanceled:
			return gate.ReviewStatusCancelled
		case hustle.ReasonTimeout:
			return gate.ReviewStatusTimedOut
		}
	}
	var queueErr *hustleruntime.QueueFailureError
	if errors.As(err, &queueErr) {
		switch queueErr.Reason {
		case hustleruntime.QueueFailureCanceled:
			return gate.ReviewStatusCancelled
		case hustleruntime.QueueFailureTimeout:
			return gate.ReviewStatusTimedOut
		}
	}
	return gate.ReviewStatusFailed
}

// publishReviewStarted publishes a PermissionReviewStarted for classifier if
// a.publisher/a.stamper are both configured (see permissionReviewAdapter's
// type doc); otherwise it is a no-op.
func (a *permissionReviewAdapter) publishReviewStarted(
	ctx context.Context,
	req loopruntime.PermissionReviewRequest,
	classifier gate.PermissionClassifier,
) {
	if a.publisher == nil || a.stamper == nil {
		return
	}
	header, err := a.stamper.Stamp(event.Header{
		Coordinates:     req.ReviewContext.Coordinates,
		EventVisibility: event.Internal,
	})
	if err != nil {
		a.reportAuditFault(ctx, req.GateID, err)
		return
	}
	a.publish(ctx, req.GateID, event.PermissionReviewStarted{
		Header:             header,
		GateID:             req.GateID,
		ToolExecutionID:    req.ToolExecutionID,
		Classifier:         classifier.Name(),
		ClassifierRevision: classifier.Revision(),
	})
}

// publishReviewCompleted publishes a PermissionReviewCompleted for
// classifier's terminal status if a.publisher/a.stamper are both configured;
// otherwise it is a no-op. Risk/Authorization/Categories are populated ONLY
// for allowed/needs_human (design §16.2's validation rules reject them on
// every other status) — assessment is otherwise ignored by callers passing
// the zero value for every other status.
func (a *permissionReviewAdapter) publishReviewCompleted(
	ctx context.Context,
	req loopruntime.PermissionReviewRequest,
	classifier gate.PermissionClassifier,
	status gate.ReviewStatus,
	assessment gate.PermissionAssessment,
	autoApproved bool,
) {
	if a.publisher == nil || a.stamper == nil {
		return
	}
	header, err := a.stamper.Stamp(event.Header{
		Coordinates:     req.ReviewContext.Coordinates,
		EventVisibility: event.Internal,
	})
	if err != nil {
		a.reportAuditFault(ctx, req.GateID, err)
		return
	}
	completed := event.PermissionReviewCompleted{
		Header:             header,
		GateID:             req.GateID,
		ToolExecutionID:    req.ToolExecutionID,
		Classifier:         classifier.Name(),
		ClassifierRevision: classifier.Revision(),
		Status:             status,
		AutoApproved:       autoApproved,
	}
	if status == gate.ReviewStatusAllowed || status == gate.ReviewStatusNeedsHuman {
		completed.Risk = assessment.Risk
		completed.Authorization = assessment.Authorization
		completed.Categories = append([]gate.ReviewRiskCategory(nil), assessment.Categories...)
	}
	a.publish(ctx, req.GateID, completed)
}

// publish durably (checked) appends ev using a context decoupled from
// review's own cancellation — context.WithoutCancel, bounded by
// a.auditTimeout when configured — mirroring hustleruntime's own audit
// publication (internal/hustleruntime/audit.go's publishAudit): a
// cancelled/timed-out review must still be able to record ITS OWN audit
// trail, exactly as respondFromClassifier already races the same
// reviewCtx for the gate response itself.
func (a *permissionReviewAdapter) publish(ctx context.Context, gateID gate.ID, ev event.Event) {
	publishCtx := context.WithoutCancel(ctx)
	if a.auditTimeout > 0 {
		var cancel context.CancelFunc
		publishCtx, cancel = context.WithTimeout(publishCtx, a.auditTimeout)
		defer cancel()
	}
	if err := a.publisher.PublishInternalEventChecked(publishCtx, ev); err != nil {
		a.reportAuditFault(ctx, gateID, err)
	}
}

// reportAuditFault routes an audit-append failure through a.faults (design
// §17: an Integrity failure keeps existing session fault semantics), when
// configured. It uses a context decoupled from review's own cancellation for
// the same reason publish does.
func (a *permissionReviewAdapter) reportAuditFault(ctx context.Context, gateID gate.ID, cause error) {
	if a.faults == nil {
		return
	}
	a.faults.ReportFault(context.WithoutCancel(ctx), &permissionReviewAuditError{GateID: gateID, Cause: cause})
}

// classifierApprovalBasis derives the common ReviewBasis and a stable,
// human-readable classifier-identity reason string from an eligible combined
// decision's outcomes. It requires at least one applicable+allowed outcome
// (guaranteed whenever gate.CombinePermissionAssessments reports Eligible)
// and returns ok=false otherwise, so a caller cannot accidentally attempt a
// classifier response backed by no real evidence.
//
// classifiers and outcomes MUST be the same length and in the same order —
// review always builds them that way, one outcome per registered classifier.
// Every applicable classifier's subject shares the same basis fields other
// than ClassifierRevision/SubjectDigest (CombinePermissionAssessments enforces
// this via its common-subject-digest check), so taking the basis from the
// first applicable outcome and zeroing its classifier-specific fields yields
// the one shared ReviewBasis every contributing classifier agreed on.
// classifierApprovalBasis additionally aggregates every contributing
// classifier's OWN Observations (design §13.4, TOCTOU) into one combined
// slice — concatenated, not deduplicated: two classifiers observing the same
// target independently is a stronger, not weaker, signal for the recheck,
// and deduplicating would need an equality notion this function has no
// reason to invent. Every applicable outcome here is guaranteed
// ReviewStatusAllowed (CombinePermissionAssessments only reports Eligible
// when every applicable outcome is Allowed), so gate.PermissionAssessmentOutcome's
// own "Observations only meaningful when Allowed" invariant already holds by
// construction — nothing here re-checks it.
func classifierApprovalBasis(
	classifiers []gate.PermissionClassifier,
	outcomes []gate.PermissionAssessmentOutcome,
) (gate.ReviewBasis, []gate.ObservationRequirement, string, bool) {
	var basis gate.ReviewBasis
	haveBasis := false
	var reasons []string
	var observations []gate.ObservationRequirement
	for index, outcome := range outcomes {
		if !outcome.Applicable {
			continue
		}
		if !haveBasis {
			basis = outcome.Subject.Basis
			basis.ClassifierRevision = ""
			basis.SubjectDigest = [32]byte{}
			haveBasis = true
		}
		observations = append(observations, outcome.Observations...)
		if index < len(classifiers) {
			reasons = append(reasons, string(classifiers[index].Name())+"@"+classifiers[index].Revision())
		}
	}
	if !haveBasis || len(reasons) == 0 {
		return gate.ReviewBasis{}, nil, "", false
	}
	return basis, observations, strings.Join(reasons, ";"), true
}
