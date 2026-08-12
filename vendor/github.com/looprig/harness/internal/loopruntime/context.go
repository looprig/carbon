package loopruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/inference"
	contextcount "github.com/looprig/inference/contextcount"
	model "github.com/looprig/inference/model"
)

type contextCompactionAwaitDisposition uint8

const (
	contextCompactionAwaitUnknown contextCompactionAwaitDisposition = iota
	contextCompactionAwaitRejected
	contextCompactionAwaitCommitted
)

type contextCompactionAwaitResult struct {
	Disposition contextCompactionAwaitDisposition
	// Proposal is prepared outside the actor but has no durable authority. The
	// actor validates it and owns the one canonical terminal append.
	Proposal compactionFinalizationProposal
	// ContinuationError is private in-memory evidence for deciding whether a
	// future primary request may proceed after the canonical rejection.
	ContinuationError error
}

type contextCompactionAwaiter interface {
	AwaitCompaction(context.Context, event.CompactAttemptID) (contextCompactionAwaitResult, error)
}

type contextReplacementDirective struct {
	AttemptID   event.CompactAttemptID
	Replacement turnContextReplacement
}

type terminalCompactionCancellationError struct {
	Reason event.CompactRejectReason
}

func (e *terminalCompactionCancellationError) Error() string {
	return "loopruntime: terminal compaction wait canceled"
}

func (e *contextReplacementDirective) Error() string {
	return "loopruntime: compacted context replacement is ready for continuation"
}

type contextCompactionAwaitError struct {
	AttemptID event.CompactAttemptID
	Cause     error
}

func (e *contextCompactionAwaitError) Error() string {
	if e.Cause != nil {
		return "loopruntime: automatic compaction await failed: " + e.Cause.Error()
	}
	return "loopruntime: automatic compaction returned an unknown disposition"
}

func (e *contextCompactionAwaitError) Unwrap() error { return e.Cause }

type contextCompactionOutcomeError struct {
	AttemptID event.CompactAttemptID
	Cause     error
}

func (*contextCompactionOutcomeError) Error() string {
	return "loopruntime: invalid canonical compaction outcome identity"
}

func (e *contextCompactionOutcomeError) Unwrap() error { return e.Cause }

func validateContextCompactionProposal(attempt *compactionAttempt, result contextCompactionAwaitResult) error {
	if attempt == nil {
		return &contextCompactionOutcomeError{}
	}
	if err := result.Proposal.validate(); err != nil {
		return &contextCompactionOutcomeError{AttemptID: attempt.AttemptID, Cause: err}
	}
	switch result.Disposition {
	case contextCompactionAwaitRejected:
		if result.Proposal.Success != nil {
			return &contextCompactionOutcomeError{AttemptID: attempt.AttemptID}
		}
	case contextCompactionAwaitCommitted:
		if result.Proposal.Success == nil {
			return &contextCompactionOutcomeError{AttemptID: attempt.AttemptID}
		}
	default:
		return &contextCompactionOutcomeError{AttemptID: attempt.AttemptID}
	}
	return nil
}

type contextRevisionOverflowError struct{}

func (*contextRevisionOverflowError) Error() string { return "loopruntime: context revision overflow" }

type contextGenerationOverflowError struct{}

func (*contextGenerationOverflowError) Error() string {
	return "loopruntime: request configuration generation overflow"
}

type contextMutationKind uint8

const (
	contextMutationHistory contextMutationKind = iota
	contextMutationRequestShape
)

type contextMutation struct {
	basis      event.ContextBasis
	generation uint64
	kind       contextMutationKind
}

func preflightContextMutation(tracker contextTracker, generation uint64, eventID uuid.UUID, kind contextMutationKind) (contextMutation, error) {
	basis, err := tracker.nextBasis(eventID)
	if err != nil {
		return contextMutation{}, err
	}
	nextGeneration := generation
	if kind == contextMutationRequestShape {
		if generation == ^uint64(0) {
			return contextMutation{}, &contextGenerationOverflowError{}
		}
		nextGeneration++
	}
	return contextMutation{basis: basis, generation: nextGeneration, kind: kind}, nil
}

func (m contextMutation) commit(tracker *contextTracker, generation *uint64) {
	tracker.basis = m.basis
	if m.kind == contextMutationRequestShape {
		*generation = m.generation
	}
}

type staleContextMeasurementError struct {
	Measured event.ContextBasis
	Current  event.ContextBasis
}

type contextConfigurationStateError struct{ Detail string }

func (e *contextConfigurationStateError) Error() string {
	return "loopruntime: invalid context configuration state: " + e.Detail
}

func (*staleContextMeasurementError) Error() string {
	return "loopruntime: context changed while request measurement was in flight"
}

type contextMeasureRequest struct {
	ctx                    context.Context
	request                inference.Request
	runtimeTail            *content.UserMessage
	runtimeContextRevision string
	reply                  chan<- contextMeasureReply
}

type contextCountResult struct {
	request     contextMeasureRequest
	measurement event.ContextMeasurement
	generation  uint64
	err         error
}

// idleCompactionPreparation freezes the stable committed request inputs while an
// idle manual attempt is counted off-actor. Tool definitions are resolved from
// the frozen registry by the worker because Tool.Info may block.
type idleCompactionPreparation struct {
	attemptID     event.CompactAttemptID
	basis         event.ContextBasis
	generation    uint64
	derivedPrefix int
	cancel        context.CancelFunc
	request       inference.Request
	tools         ToolSet
	transcript    content.AgenticMessages

	// inferenceCapability is snapshotted from state.effective.inferenceCapability on the
	// actor goroutine, synchronously, at the same point model/system are captured into
	// request above — never read from state.effective inside the off-actor preparation
	// goroutine, which would race the actor's own concurrent writes (SetMode/
	// ChangeLoopInference can mutate state.effective while this preparation runs).
	inferenceCapability contextcount.InferenceCapability
}

type idleCompactionCountResult struct {
	preparation idleCompactionPreparation
	candidate   compactionExecutionCandidate
	err         error
}

type contextMeasureReply struct {
	measurement event.ContextMeasurement
	attemptID   event.CompactAttemptID
	awaiter     contextCompactionAwaiter
	err         error
}

type contextCompactionOutcomeRequest struct {
	attemptID event.CompactAttemptID
	result    contextCompactionAwaitResult
	reply     chan<- contextCompactionOutcomeReply
}

type contextCompactionOutcomeReply struct {
	disposition       contextCompactionAwaitDisposition
	replacement       *turnContextReplacement
	continuationError error
	rejectReason      event.CompactRejectReason
	retry             bool
	err               error
}

type contextAdmissionSettings struct {
	ReservedOutput content.TokenCount
	SafetyMargin   content.TokenCount
	CountTimeout   time.Duration
	Automatic      bool
	CompactAt      event.BasisPoints
	RearmBelow     event.BasisPoints
}

func observationAdmissionSettings(policy loop.ContextObservationPolicy) contextAdmissionSettings {
	return contextAdmissionSettings{ReservedOutput: policy.ReservedOutput, SafetyMargin: policy.SafetyMargin, CountTimeout: policy.CountTimeout}
}

func compactionAdmissionSettings(policy loop.CompactionPolicy) contextAdmissionSettings {
	return contextAdmissionSettings{
		ReservedOutput: policy.ReservedOutput, SafetyMargin: policy.SafetyMargin, CountTimeout: policy.CountTimeout,
		Automatic: policy.Automatic, CompactAt: policy.CompactAt, RearmBelow: policy.RearmBelow,
	}
}

func contextSettings(config runtimeConfig) (contextAdmissionSettings, bool) {
	if config.ContextObservation != nil {
		return observationAdmissionSettings(*config.ContextObservation), true
	}
	if config.Compaction != nil {
		return compactionAdmissionSettings(*config.Compaction), true
	}
	return contextAdmissionSettings{}, false
}

func measureRequestContext(
	ctx context.Context,
	counter contextcount.ContextCounter,
	counterCapability contextcount.CounterCapability,
	inferenceCapability contextcount.InferenceCapability,
	settings contextAdmissionSettings,
	basis event.ContextBasis,
	request inference.Request,
	runtimeContextRevision string,
) (event.ContextMeasurement, error) {
	limits, err := loop.ResolveContextLimits(request.Model.Key(), request.Model.Limits, settings.ReservedOutput, settings.SafetyMargin)
	if err != nil {
		return event.ContextMeasurement{}, err
	}
	fingerprint, err := contextRequestFingerprint(request, basis, runtimeContextRevision, counterCapability, inferenceCapability)
	if err != nil {
		return event.ContextMeasurement{}, err
	}
	countCtx, cancel := context.WithTimeout(ctx, settings.CountTimeout)
	defer cancel()
	count, err := counter.CountContext(countCtx, request)
	if err != nil {
		return event.ContextMeasurement{}, normalizeContextCountError(request.Model.Key(), counterCapability.Quality, err)
	}
	if err := countCtx.Err(); err != nil {
		return event.ContextMeasurement{}, normalizeContextCountError(request.Model.Key(), counterCapability.Quality, err)
	}
	if err := validateContextCount(request.Model.Key(), counterCapability.Quality, count); err != nil {
		return event.ContextMeasurement{}, err
	}
	measurement := event.ContextMeasurement{
		Basis: basis, Model: count.Model, RequestFingerprint: fingerprint,
		InputTokens: count.InputTokens, InputLimit: limits.InputLimit, Quality: count.Quality,
	}
	if err := measurement.Validate(); err != nil {
		return event.ContextMeasurement{}, err
	}
	return measurement, nil
}

func contextRequestFingerprint(
	request inference.Request,
	basis event.ContextBasis,
	runtimeContextRevision string,
	counterCapability contextcount.CounterCapability,
	inferenceCapability contextcount.InferenceCapability,
) ([32]byte, error) {
	template, err := contextFingerprintTemplateForRequest(request, runtimeContextRevision, counterCapability, inferenceCapability)
	if err != nil {
		return [32]byte{}, err
	}
	return template.Fingerprint(basis)
}

// contextFingerprintTemplate is the least-privilege request-shape projection
// safe to carry across compaction finalization. It contains only irreversible
// revisions and public model/capability descriptors; Basis is supplied only
// after the actor mints the canonical CompactionCommitted EventID.
type contextFingerprintTemplate struct {
	SystemRevision         string
	ToolPolicyRevision     string
	Model                  model.Model
	RuntimeContextRevision string
	CounterCapability      contextcount.CounterCapability
	InferenceCapability    contextcount.InferenceCapability
}

func contextFingerprintTemplateForRequest(
	request inference.Request,
	runtimeContextRevision string,
	counterCapability contextcount.CounterCapability,
	inferenceCapability contextcount.InferenceCapability,
) (contextFingerprintTemplate, error) {
	toolShape, err := json.Marshal(struct {
		Tools    []inference.Tool
		Override *model.Sampling
	}{Tools: request.Tools, Override: request.Override})
	if err != nil {
		return contextFingerprintTemplate{}, &loop.RequestFingerprintError{Field: "ToolPolicyRevision", Cause: err}
	}
	template := contextFingerprintTemplate{
		SystemRevision: revisionDigest([]byte(request.System)), ToolPolicyRevision: revisionDigest(toolShape),
		Model: request.Model.Clone(), RuntimeContextRevision: runtimeContextRevision,
		CounterCapability: counterCapability, InferenceCapability: inferenceCapability,
	}
	if _, err := template.Fingerprint(event.ContextBasis{Revision: 1, ThroughEventID: uuid.UUID{1}}); err != nil {
		return contextFingerprintTemplate{}, err
	}
	return template, nil
}

func (t contextFingerprintTemplate) Fingerprint(basis event.ContextBasis) ([32]byte, error) {
	return loop.RequestFingerprint(loop.RequestFingerprintInput{
		SystemRevision: t.SystemRevision, ToolPolicyRevision: t.ToolPolicyRevision,
		Model: t.Model, Basis: basis, RuntimeContextRevision: t.RuntimeContextRevision,
		CounterCapability: t.CounterCapability, InferenceCapability: t.InferenceCapability,
	})
}

func revisionDigest(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func runtimeContextRevision(message *content.UserMessage) (string, error) {
	if message == nil {
		return revisionDigest(nil), nil
	}
	encoded, err := json.Marshal(message)
	if err != nil {
		return "", &loop.RequestFingerprintError{Field: "RuntimeContextRevision", Cause: err}
	}
	return revisionDigest(encoded), nil
}

func normalizeContextCountError(model model.ModelKey, quality contextcount.CountQuality, err error) error {
	var typed *contextcount.ContextCountError
	if errors.As(err, &typed) {
		return err
	}
	return &contextcount.ContextCountError{Model: model, Quality: quality, Cause: err}
}

func validateContextCount(model model.ModelKey, quality contextcount.CountQuality, count contextcount.ContextCount) error {
	if err := count.Model.Validate(); err != nil {
		return &contextcount.ContextCountError{Model: count.Model, Quality: count.Quality, Cause: err}
	}
	if count.Model != model {
		return &contextcount.ContextCountError{Model: count.Model, Quality: count.Quality, Cause: contextcount.ErrContextCountModelMismatch}
	}
	if count.Quality != contextcount.CountQualityExactProvider && count.Quality != contextcount.CountQualityExactLocal && count.Quality != contextcount.CountQualityHeuristicEstimate {
		return &contextcount.ContextCountError{Model: count.Model, Quality: count.Quality, Cause: contextcount.ErrContextCountQualityInvalid}
	}
	if count.Quality != quality {
		return &contextcount.ContextCountError{Model: count.Model, Quality: count.Quality, Cause: contextcount.ErrContextCountCapabilityQualityMismatch}
	}
	return nil
}

type contextTrackingResult struct {
	MeasurementChanged bool
	PressureChanged    bool
	Occupancy          event.BasisPoints
	Previous           event.PressureLevel
	Current            event.PressureLevel
	TriggerAutomatic   bool
	AdmissionError     error
}

type contextTracker struct {
	basis          event.ContextBasis
	measurement    event.ContextMeasurement
	hasMeasurement bool
	pressure       event.PressureLevel
	automaticBasis event.ContextBasis
	hasAutomatic   bool
}

func (t *contextTracker) restore(
	basis event.ContextBasis,
	hasBasis bool,
	measurement event.ContextMeasurement,
	hasMeasurement bool,
	automaticBasis event.ContextBasis,
	hasAutomatic bool,
	settings contextAdmissionSettings,
) error {
	if hasBasis {
		t.basis = basis
	}
	if hasMeasurement {
		if err := measurement.Validate(); err != nil {
			return err
		}
		if !hasBasis {
			t.basis = measurement.Basis
		}
		occupancy, err := loop.OccupancyBasisPoints(measurement.InputTokens, measurement.InputLimit)
		if err != nil {
			return err
		}
		t.measurement = measurement
		t.hasMeasurement = true
		t.pressure = t.pressureLevel(occupancy, settings)
	}
	if hasAutomatic {
		t.automaticBasis = automaticBasis
		t.hasAutomatic = true
	}
	return nil
}

func (t contextTracker) nextBasis(eventID uuid.UUID) (event.ContextBasis, error) {
	if eventID.IsZero() {
		return event.ContextBasis{}, &event.ContextValidationError{Field: event.ContextFieldThroughEventID}
	}
	if t.basis.Revision == ^event.ContextRevision(0) {
		return event.ContextBasis{}, &contextRevisionOverflowError{}
	}
	t.basis.Revision++
	t.basis.ThroughEventID = eventID
	return t.basis, nil
}

func (t *contextTracker) currentBasis() event.ContextBasis { return t.basis }

func (t *contextTracker) apply(measurement event.ContextMeasurement, settings contextAdmissionSettings) (contextTrackingResult, error) {
	if err := measurement.Validate(); err != nil {
		return contextTrackingResult{}, err
	}
	occupancy, err := loop.OccupancyBasisPoints(measurement.InputTokens, measurement.InputLimit)
	if err != nil {
		return contextTrackingResult{}, err
	}
	level := t.pressureLevel(occupancy, settings)
	result := contextTrackingResult{
		MeasurementChanged: !t.hasMeasurement || t.measurement != measurement,
		PressureChanged:    t.pressure != level, Occupancy: occupancy,
		Previous: t.pressure, Current: level,
	}
	if settings.Automatic && (level == event.PressureCompact || level == event.PressureHardLimit) && (!t.hasAutomatic || t.automaticBasis != measurement.Basis) {
		result.TriggerAutomatic = true
	}
	if level == event.PressureHardLimit && !result.TriggerAutomatic {
		result.AdmissionError = &loop.ContextLimitError{Measurement: measurement}
	}
	t.measurement = measurement
	t.hasMeasurement = true
	t.pressure = level
	return result, nil
}

func (t *contextTracker) exhaustAutomatic(basis event.ContextBasis, reason event.CompactionReason, durable bool) {
	if !durable || reason != event.CompactionReasonAutomatic {
		return
	}
	t.automaticBasis = basis
	t.hasAutomatic = true
}

func (t *contextTracker) pressureLevel(occupancy event.BasisPoints, settings contextAdmissionSettings) event.PressureLevel {
	if occupancy >= event.FullScaleBasisPoints {
		return event.PressureHardLimit
	}
	if !settings.Automatic {
		return event.PressureNormal
	}
	if (t.pressure == event.PressureCompact || t.pressure == event.PressureHardLimit) && occupancy >= settings.RearmBelow {
		return event.PressureCompact
	}
	if occupancy >= settings.CompactAt {
		return event.PressureCompact
	}
	return event.PressureNormal
}
