package hustleruntime

import (
	"context"
	"reflect"
	"sync"

	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/tool"
)

// MaxObservationsPerRun caps how many ObservationRequirement values a single
// ObservationCollector retains across one Hustle run's whole evidence
// catalog, independent of gate.MaxObservationRequirementsPerAssessment (which
// bounds the same slice again at the outcome-validation boundary in
// pkg/gate). Both bounds exist for the same reason at two different layers —
// defense in depth, never trust a single call site.
const MaxObservationsPerRun = gate.MaxObservationRequirementsPerAssessment

// ObservationCollector accumulates the ObservationRequirement values
// target-sensitive evidence tools record while executing during ONE
// classifier's Hustle run (design §13.4, TOCTOU). It is created fresh per
// run by the review adapter (internal/sessionruntime/review_adapter.go),
// attached to that run's context via WithObservationCollector, and read back
// after RunAndFinalize returns — never shared across runs, never persisted.
// A nil *ObservationCollector is a safe no-op receiver everywhere below, so a
// Hustle run with no collector attached (every non-review Hustle, and any
// review whose evidence tools recorded nothing) never allocates one.
type ObservationCollector struct {
	mu           sync.Mutex
	observations []gate.ObservationRequirement
}

// NewObservationCollector returns an empty collector ready to attach to one
// run's context.
func NewObservationCollector() *ObservationCollector {
	return &ObservationCollector{}
}

// Observations returns an independent copy of every requirement recorded so
// far. Safe to call at any time, including concurrently with recording.
func (c *ObservationCollector) Observations() []gate.ObservationRequirement {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]gate.ObservationRequirement(nil), c.observations...)
}

// record appends requirement, silently dropping it once MaxObservationsPerRun
// is reached rather than growing without bound — a run that hits the cap
// simply stops gaining new TOCTOU protection for further calls; it never
// fails the evidence call that triggered the overflow (recording is a purely
// additive safety net, never load-bearing for evidence execution itself).
func (c *ObservationCollector) record(requirement gate.ObservationRequirement) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.observations) >= MaxObservationsPerRun {
		return
	}
	c.observations = append(c.observations, requirement)
}

type observationCollectorContextKey struct{}

// WithObservationCollector attaches collector to ctx so evidence execution
// deep inside RunAndFinalize (evidenceRunner.run, several call frames below
// where this context originates) can record onto it without RunAndFinalize's
// own signature changing — the same context-capability convention
// withPreparedEvidenceCall/loop.WithPreparedCall already uses for per-call
// prepared-artifact access. A nil collector leaves ctx unchanged.
func WithObservationCollector(ctx context.Context, collector *ObservationCollector) context.Context {
	if collector == nil {
		return ctx
	}
	return context.WithValue(ctx, observationCollectorContextKey{}, collector)
}

// observationCollectorFromContext reads back a collector WithObservationCollector
// attached, or nil if none was (every non-review Hustle run, and any review
// call whose review adapter predates this addendum's wiring — both are
// ordinary no-observation-recorded outcomes, never an error).
func observationCollectorFromContext(ctx context.Context) *ObservationCollector {
	collector, _ := ctx.Value(observationCollectorContextKey{}).(*ObservationCollector)
	return collector
}

// recordEvidenceObservation probes concrete for the optional
// tool.EvidenceObservation capability and, when implemented and it reports an
// observation for THIS call, converts and validates it before appending to
// collector. It is called exactly once per successfully executed evidence
// call (evidenceRunner.run, immediately after executeEvidenceCall succeeds) —
// never for a call that failed preparation, authorization, or execution.
//
// A tool implementation is trusted but fallible, exactly like a permission
// classifier (pkg/gate/reviewer.go's PermissionClassifierPanicError doc
// comment): ObservedRequirement is therefore called under a local recover,
// so a buggy capability implementation can cost this one call its
// observation but can never abort evidence gathering itself — recording is a
// purely additive safety net, never load-bearing for the underlying evidence
// call that already succeeded.
func recordEvidenceObservation(
	collector *ObservationCollector,
	concrete tool.InvokableTool,
	request tool.Request,
	result *tool.ToolResult,
) {
	if collector == nil {
		return
	}
	reporter, ok := concrete.(tool.EvidenceObservation)
	if !ok || nilEvidenceRuntimeValue(reflect.ValueOf(reporter)) {
		return
	}
	target, token, reported := observeSafely(reporter, request, result)
	if !reported {
		return
	}
	requirement := gate.ObservationRequirement{Target: target, Token: token}
	if !requirement.Valid() {
		return
	}
	collector.record(requirement)
}

func observeSafely(
	reporter tool.EvidenceObservation,
	request tool.Request,
	result *tool.ToolResult,
) (target string, token string, reported bool) {
	defer func() {
		if recover() != nil {
			target, token, reported = "", "", false
		}
	}()
	return reporter.ObservedRequirement(request, result)
}
