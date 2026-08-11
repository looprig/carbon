package sessionruntime

import (
	"context"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/internal/hustleruntime"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/tool"
)

// HustleModelResolveReason classifies an exact current-loop lookup failure.
type HustleModelResolveReason string

const (
	HustleModelResolveInvalidContext HustleModelResolveReason = "invalid_context"
	HustleModelResolveInvalidLoopID  HustleModelResolveReason = "invalid_loop_id"
	HustleModelResolveLoopNotFound   HustleModelResolveReason = "loop_not_found"
	HustleModelResolveForeignLoop    HustleModelResolveReason = "foreign_loop"
	HustleModelResolveLoopExited     HustleModelResolveReason = "loop_exited"
)

// HustleModelResolveError reports why the originating loop cannot supply its
// current inference binding.
type HustleModelResolveError struct {
	Reason HustleModelResolveReason
	LoopID uuid.UUID
	Cause  error
}

func (e *HustleModelResolveError) Error() string {
	message := "session: hustle model resolution failed (" + string(e.Reason) + ")"
	if e.Cause != nil {
		return message + ": " + e.Cause.Error()
	}
	return message
}

func (e *HustleModelResolveError) Unwrap() error { return e.Cause }

// HustleConstructionReason classifies the frozen-definition binding stage.
type HustleConstructionReason string

const (
	HustleConstructionBindFailed          HustleConstructionReason = "bind_failed"
	HustleConstructionRuntimeFailed       HustleConstructionReason = "runtime_failed"
	HustleConstructionMissingCollaborator HustleConstructionReason = "missing_collaborator"
	HustleConstructionAlreadyBound        HustleConstructionReason = "already_bound"
)

// HustleConstructionError reports a transactional hustle composition failure.
type HustleConstructionError struct {
	Reason HustleConstructionReason
	Name   hustle.Name
	Index  int
	Field  string
	Cause  error
}

func (e *HustleConstructionError) Error() string {
	message := "session: hustle construction failed (" + string(e.Reason) + ")"
	if e.Field != "" {
		message += ": " + e.Field
	}
	if e.Cause != nil {
		message += ": " + e.Cause.Error()
	}
	return message
}

func (e *HustleConstructionError) Unwrap() error { return e.Cause }

type sessionHustleModelResolver struct{ session *Session }

func (r sessionHustleModelResolver) ResolveHustleModel(ctx context.Context, loopID uuid.UUID) (hustle.InferenceBinding, error) {
	if err := validateHustleModelRequest(ctx, loopID); err != nil {
		return hustle.InferenceBinding{}, err
	}
	handle, err := r.exactLoop(loopID)
	if err != nil {
		return hustle.InferenceBinding{}, err
	}
	return resolveLiveHustleBinding(handle, loopID)
}

func validateHustleModelRequest(ctx context.Context, loopID uuid.UUID) error {
	if ctx == nil {
		return &HustleModelResolveError{Reason: HustleModelResolveInvalidContext, LoopID: loopID}
	}
	if err := ctx.Err(); err != nil {
		return &HustleModelResolveError{Reason: HustleModelResolveInvalidContext, LoopID: loopID, Cause: err}
	}
	if loopID.IsZero() {
		return &HustleModelResolveError{Reason: HustleModelResolveInvalidLoopID}
	}
	return nil
}

func (r sessionHustleModelResolver) exactLoop(loopID uuid.UUID) (*loopHandle, error) {
	if r.session == nil {
		return nil, &HustleModelResolveError{Reason: HustleModelResolveLoopNotFound, LoopID: loopID}
	}
	r.session.loopsMu.RLock()
	defer r.session.loopsMu.RUnlock()
	handle, exists := r.session.loops[loopID]
	if !exists || handle == nil || handle.backend == nil || handle.bound == nil {
		return nil, &HustleModelResolveError{Reason: HustleModelResolveLoopNotFound, LoopID: loopID}
	}
	if handle.owner != r.session {
		return nil, &HustleModelResolveError{Reason: HustleModelResolveForeignLoop, LoopID: loopID}
	}
	return handle, nil
}

func resolveLiveHustleBinding(handle *loopHandle, loopID uuid.UUID) (hustle.InferenceBinding, error) {
	backend := handle.backend
	if loopExited(backend.DoneChan()) {
		return hustle.InferenceBinding{}, &HustleModelResolveError{Reason: HustleModelResolveLoopExited, LoopID: loopID}
	}
	handle.liveMu.RLock()
	binding := hustle.InferenceBinding{Client: handle.bound.Client(), Model: handle.liveModel.Clone()}
	handle.liveMu.RUnlock()
	if loopExited(backend.DoneChan()) {
		return hustle.InferenceBinding{}, &HustleModelResolveError{Reason: HustleModelResolveLoopExited, LoopID: loopID}
	}
	return binding, nil
}

func loopExited(done <-chan struct{}) bool {
	if done == nil {
		return true
	}
	select {
	case <-done:
		return true
	default:
		return false
	}
}

type sessionHustleFaultReporter struct{ session *Session }

func (r sessionHustleFaultReporter) ReportFault(_ context.Context, cause error) {
	r.session.latchSessionFault(cause)
}

type hustleFinalizerContextKey struct{}

type sessionHustleFinalizerContext struct{ session *Session }

func (d sessionHustleFinalizerContext) DecorateFinalizerContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, hustleFinalizerContextKey{}, d.session)
}

// HustleShutdownReentryError refuses the structurally circular operation in
// which a session-owned finalizer passes its supplied trusted context to the
// shutdown that is itself waiting for that finalizer. The private marker is
// exact-context defense in depth, not goroutine identity: a callback that drops
// the context cannot be detected. Production adapters therefore receive focused
// product capability only, never Session/Shutdown, and must preserve the callback
// context just as they must honor its deadline.
type HustleShutdownReentryError struct{}

func (*HustleShutdownReentryError) Error() string {
	return "session: hustle finalizer cannot synchronously shut down its owning session"
}

func hustleFinalizerOwnsSession(ctx context.Context, session *Session) bool {
	if ctx == nil || session == nil {
		return false
	}
	owner, ok := ctx.Value(hustleFinalizerContextKey{}).(*Session)
	return ok && owner == session
}

func withSessionHustles(definitions []hustle.Definition, limits HustleLimits) Option {
	captured := append([]hustle.Definition(nil), definitions...)
	return func(s *Session) {
		s.hustleDefinitions = append([]hustle.Definition(nil), captured...)
		s.hustleLimits = limits
	}
}

func (s *Session) bindSessionHustles() error {
	if err := s.beginHustleBinding(); err != nil || len(s.hustleDefinitions) == 0 {
		return err
	}
	bound, err := s.bindHustleDefinitions()
	if err != nil {
		return err
	}
	controller, err := s.newHustleController(bound)
	if err != nil {
		return &HustleConstructionError{Reason: HustleConstructionRuntimeFailed, Cause: err}
	}
	s.hustleController = controller
	return nil
}

func (s *Session) beginHustleBinding() error {
	if s.hustlesBound {
		return &HustleConstructionError{Reason: HustleConstructionAlreadyBound}
	}
	s.hustlesBound = true
	if len(s.hustleDefinitions) == 0 {
		return nil
	}
	if s.factory == nil {
		return &HustleConstructionError{Reason: HustleConstructionMissingCollaborator, Field: "factory"}
	}
	if s.hub == nil {
		return &HustleConstructionError{Reason: HustleConstructionMissingCollaborator, Field: "hub"}
	}
	return nil
}

func (s *Session) bindHustleDefinitions() ([]hustle.BoundDefinition, error) {
	resolver := sessionHustleModelResolver{session: s}
	bound := make([]hustle.BoundDefinition, 0, len(s.hustleDefinitions))
	for index, definition := range s.hustleDefinitions {
		candidate, err := definition.Bind(s.sessionCtx, hustle.Bindings{Models: resolver})
		if err != nil {
			return nil, &HustleConstructionError{
				Reason: HustleConstructionBindFailed, Name: definition.Name(), Index: index, Cause: err,
			}
		}
		bound = append(bound, candidate)
	}
	return bound, nil
}

func (s *Session) newHustleController(bound []hustle.BoundDefinition) (*hustleruntime.Controller, error) {
	return hustleruntime.New(s.sessionCtx, hustleruntime.Config{
		Blocking:   hustleruntime.LaneLimits{Concurrent: s.hustleLimits.BlockingConcurrent, Queued: s.hustleLimits.BlockingQueued},
		Background: hustleruntime.LaneLimits{Concurrent: s.hustleLimits.BackgroundConcurrent, Queued: s.hustleLimits.BackgroundQueued},
		Runtime: &hustleruntime.RuntimeConfig{
			SessionID: s.sessionID, Definitions: bound,
			AuditTimeout: s.hustleLimits.AuditTimeout, FinalizationTimeout: s.hustleLimits.FinalizationTimeout,
			WorkerDrainTimeout: s.hustleLimits.WorkerDrainTimeout,
			Stamper:            s.factory, Audit: s.hub, Faults: sessionHustleFaultReporter{session: s},
			Activity:         newHubHustleActivityTracker(s.hub),
			FinalizerContext: sessionHustleFinalizerContext{session: s},
			Evidence:         s.hustleEvidenceRuntimeConfig(),
		},
	})
}

// hustleEvidenceRuntimeConfig builds the evidence-tool runtime config every
// registered hustle's (opt-in) evidence catalog binds against. ReadWorkspace
// is auto-derived from the session's OWN existing workspace-root tracking
// (s.wsRoot, populated by withResolvedPlacement/WithWorkspaceCheckpointing —
// see command_journal.go) — the consumer is never asked for it a second
// time. NewExecutionID is Harness's own trivial internal factory (uuid.New);
// it is never consumer-supplied. Access/Containment/AllowedKinds come
// entirely from the consumer-supplied withPermissionReviewEvidence option
// (gates.go); when that option was never applied they stay at their zero
// value, and newRuntimeController (internal/hustleruntime) fails
// CONSTRUCTION closed — never silently permissive — for any registered
// hustle whose definition actually needs evidence tools. This value is
// always non-nil so that fail-closed check is reachable regardless of
// whether this particular session opted into evidence review at all.
func (s *Session) hustleEvidenceRuntimeConfig() *hustleruntime.EvidenceRuntimeConfig {
	evidence := &hustleruntime.EvidenceRuntimeConfig{
		Access:         s.permissionReviewEvidenceAccess,
		Containment:    s.permissionReviewEvidenceContainment,
		AllowedKinds:   append([]string(nil), s.permissionReviewEvidenceAllowedKinds...),
		NewExecutionID: uuid.New,
	}
	if s.wsRoot != "" {
		evidence.ReadWorkspace = &tool.ReadWorkspaceBinding{Root: s.wsRoot}
	}
	return evidence
}
