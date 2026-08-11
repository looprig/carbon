package loopruntime

import (
	"context"
	"errors"
	"reflect"
	"sync"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hook"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/inference"
	contextcount "github.com/looprig/inference/contextcount"
)

type compactionExecutorConfig struct {
	Compactor         Compactor
	Counter           contextcount.ContextCounter
	CounterCapability contextcount.CounterCapability
	Settings          contextAdmissionSettings
	MaxSummaryTokens  content.TokenCount
}

type compactionExecutionCandidate struct {
	Measurement         event.ContextMeasurement
	Request             inference.Request
	RuntimeTail         *content.UserMessage
	RuntimeRevision     string
	Transcript          content.AgenticMessages
	InferenceCapability contextcount.InferenceCapability
}

type compactionExecutorError struct{ Field string }

func (e *compactionExecutorError) Error() string {
	return "loopruntime: invalid compaction executor field " + e.Field
}

type compactionExecutionResult struct {
	outcome contextCompactionAwaitResult
	err     error
}

type compactionExecutionRun struct {
	result chan compactionExecutionResult
	cancel context.CancelFunc
	scope  *compactionHookScope
}

type compactionExecutor struct {
	ctx    context.Context
	config compactionExecutorConfig
	mu     sync.Mutex
	runs   map[event.CompactAttemptID]compactionExecutionRun
}

func newCompactionExecutor(ctx context.Context, config compactionExecutorConfig) (*compactionExecutor, error) {
	if ctx == nil {
		return nil, &compactionExecutorError{Field: "context"}
	}
	if nilCompactor(config.Compactor) {
		return nil, &compactionExecutorError{Field: "compactor"}
	}
	if nilContextCounter(config.Counter) {
		return nil, &compactionExecutorError{Field: "counter"}
	}
	if config.Settings.CountTimeout <= 0 || config.MaxSummaryTokens == 0 {
		return nil, &compactionExecutorError{Field: "policy"}
	}
	if err := config.CounterCapability.Validate(); err != nil {
		return nil, &compactionExecutorError{Field: "counter_capability"}
	}
	return &compactionExecutor{ctx: ctx, config: config, runs: make(map[event.CompactAttemptID]compactionExecutionRun)}, nil
}

func installCompactionExecutor(ctx context.Context, config *runtimeConfig, compactor Compactor) error {
	if compactor == nil {
		return nil
	}
	if nilCompactor(compactor) {
		return &compactionExecutorError{Field: "compactor"}
	}
	if config == nil || config.Compaction == nil {
		return &compactionExecutorError{Field: "policy"}
	}
	executor, err := newCompactionExecutor(ctx, compactionExecutorConfig{
		Compactor: compactor, Counter: config.ContextCounter,
		CounterCapability: config.CounterCapability,
		Settings:          compactionAdmissionSettings(*config.Compaction), MaxSummaryTokens: config.Compaction.MaxSummaryTokens,
	})
	if err != nil {
		return err
	}
	config.compactionSink = executor
	return nil
}

// Interface equality detects an untyped nil but not a typed nil pointer stored in
// an interface. Keep reflection at this construction boundary so the executor never
// starts with a collaborator that will panic on first use.
func nilCompactor(compactor Compactor) bool {
	return compactor == nil || nilInterfaceImplementation(reflect.ValueOf(compactor))
}

func nilContextCounter(counter contextcount.ContextCounter) bool {
	return counter == nil || nilInterfaceImplementation(reflect.ValueOf(counter))
}

func nilInterfaceImplementation(value reflect.Value) bool {
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func (e *compactionExecutor) CoordinateCompaction(context.Context, compactionDisposition) error {
	return &compactionExecutorError{Field: "candidate"}
}

func (e *compactionExecutor) CoordinateCompactionCandidate(
	ctx context.Context,
	disposition compactionDisposition,
	candidate compactionExecutionCandidate,
) error {
	if disposition.Kind != compactionDispositionStart || disposition.Attempt == nil {
		return &compactionExecutorError{Field: "disposition"}
	}
	attempt := *disposition.Attempt
	result := make(chan compactionExecutionResult, 1)
	if disposition.hookScope != nil {
		ctx = disposition.hookScope.ctx
	}
	if ctx == nil {
		ctx = e.ctx
	}
	runCtx, cancel := context.WithCancel(ctx)
	e.mu.Lock()
	if _, exists := e.runs[attempt.AttemptID]; exists {
		e.mu.Unlock()
		cancel()
		return &compactionExecutorError{Field: "attempt"}
	}
	e.runs[attempt.AttemptID] = compactionExecutionRun{result: result, cancel: cancel, scope: disposition.hookScope}
	e.mu.Unlock()
	if disposition.preRejected != nil {
		rejected := *disposition.preRejected
		rejected.Proposal.hookScope = disposition.hookScope
		result <- compactionExecutionResult{outcome: rejected}
		return nil
	}
	// Must run AFTER the preRejected branch above, not before: a preRejected disposition
	// carries this same candidate but has already decided its outcome (e.g. a hook denial
	// with its own specific CompactRejectReason) and returns before this point. Validating
	// candidate.InferenceCapability first would let a structurally invalid candidate silently
	// override that already-decided rejection reason with a generic "inference_capability"
	// error instead.
	if err := candidate.InferenceCapability.Validate(); err != nil {
		e.mu.Lock()
		delete(e.runs, attempt.AttemptID)
		e.mu.Unlock()
		cancel()
		return &compactionExecutorError{Field: "inference_capability"}
	}
	candidate.Request.Messages = cloneMessages(candidate.Request.Messages)
	candidate.RuntimeTail = cloneUserMessage(candidate.RuntimeTail)
	candidate.Transcript = cloneMessages(candidate.Transcript)
	input := cloneCompactionHookInput(disposition.input)
	go func() { result <- e.execute(runCtx, attempt, candidate, input, disposition.hookScope) }()
	return nil
}

func (e *compactionExecutor) AwaitCompaction(ctx context.Context, attemptID event.CompactAttemptID) (contextCompactionAwaitResult, error) {
	e.mu.Lock()
	run, exists := e.runs[attemptID]
	e.mu.Unlock()
	if !exists {
		return contextCompactionAwaitResult{}, &compactionExecutorError{Field: "attempt"}
	}
	select {
	case completed := <-run.result:
		run.cancel()
		e.mu.Lock()
		delete(e.runs, attemptID)
		e.mu.Unlock()
		return completed.outcome, completed.err
	case <-ctx.Done():
		run.cancel()
		e.mu.Lock()
		delete(e.runs, attemptID)
		e.mu.Unlock()
		rejected := rejectedCompactionResult(event.CompactRejectCanceled)
		rejected.Proposal.hookScope = run.scope
		run.scope.sealExecutorTerminal(hook.OutcomeCanceled, context.Canceled, nil)
		return rejected, nil
	}
}

func (e *compactionExecutor) execute(
	ctx context.Context,
	attempt compactionAttempt,
	candidate compactionExecutionCandidate,
	input *loop.CompactionInput,
	scope *compactionHookScope,
) (completed compactionExecutionResult) {
	defer func() {
		if recover() == nil {
			return
		}
		panicErr := &operationHookPanicError{Operation: hook.OperationCompaction}
		rejected := rejectedCompactionResultWithError(event.CompactRejectInternal, panicErr)
		rejected.Proposal.hookScope = scope
		scope.setExecutorTerminal(hook.OutcomeFailed, panicErr, nil)
		completed = compactionExecutionResult{outcome: rejected}
	}()
	if input == nil {
		input = &loop.CompactionInput{
			Basis: attempt.Basis, Model: candidate.Measurement.Model,
			RequestFingerprint: candidate.Measurement.RequestFingerprint,
			Transcript:         cloneMessages(candidate.Transcript), MaxSummaryTokens: e.config.MaxSummaryTokens,
		}
	}
	var prepared contextCompactionAwaitResult
	finalized := false
	err := e.config.Compactor.CompactAndFinalize(ctx, *input, func(finalizeCtx context.Context, outcome CompactionOutcome) error {
		finalized = true
		prepared = e.prepare(finalizeCtx, attempt, candidate, outcome)
		return nil
	})
	if !finalized {
		prepared = rejectedCompactionResultWithError(compactionRejectReason(err), err)
	}
	prepared.Proposal.hookScope = scope
	setCompactionScopeTerminal(scope, ctx, prepared)
	return compactionExecutionResult{outcome: prepared}
}

type compactionOperationError struct {
	Reason event.CompactRejectReason
}

func (e *compactionOperationError) Error() string {
	return "loopruntime: compaction operation rejected"
}

func setCompactionScopeTerminal(scope *compactionHookScope, ctx context.Context, result contextCompactionAwaitResult) {
	if scope == nil {
		return
	}
	if result.Disposition == contextCompactionAwaitCommitted && result.Proposal.Success != nil {
		success := result.Proposal.Success
		scope.setExecutorTerminal(hook.OutcomeCompleted, nil, &loop.CompactionOutput{
			Basis: scope.call.Compaction.Input.Basis, Model: success.Model,
			RequestFingerprint: success.RequestFingerprint, Summary: cloneUserMessage(success.Summary),
		})
		return
	}
	err := result.ContinuationError
	if result.Proposal.RejectReason == event.CompactRejectCanceled {
		if err == nil {
			err = context.Canceled
		}
		scope.setExecutorTerminal(hook.OutcomeCanceled, err, nil)
		return
	}
	if err == nil {
		err = &compactionOperationError{Reason: result.Proposal.RejectReason}
	}
	scope.setExecutorTerminal(hookOutcome(ctx, err), err, nil)
}

func cloneCompactionHookInput(value *loop.CompactionInput) *loop.CompactionInput {
	if value == nil {
		return nil
	}
	cloned := *value
	cloned.Transcript = cloneMessages(value.Transcript)
	return &cloned
}

func (e *compactionExecutor) prepare(
	ctx context.Context,
	attempt compactionAttempt,
	candidate compactionExecutionCandidate,
	outcome CompactionOutcome,
) contextCompactionAwaitResult {
	if err := outcome.Validate(); err != nil {
		return rejectedCompactionResult(event.CompactRejectInternal)
	}
	if outcome.Err != nil {
		return rejectedCompactionResultWithError(compactionRejectReason(outcome.Err), outcome.Err)
	}
	value := outcome.Value
	if value.Basis != attempt.Basis || value.Model != candidate.Measurement.Model ||
		value.RequestFingerprint != candidate.Measurement.RequestFingerprint {
		return rejectedCompactionResult(event.CompactRejectInvalidSummary)
	}
	request := candidate.Request
	request.Messages = content.AgenticMessages{cloneUserMessage(value.Summary)}
	if candidate.RuntimeTail != nil {
		request.Messages = append(request.Messages, cloneUserMessage(candidate.RuntimeTail))
	}
	measurement, err := measureRequestContext(
		ctx, e.config.Counter, e.config.CounterCapability, candidate.InferenceCapability,
		e.config.Settings, attempt.Basis, request, candidate.RuntimeRevision,
	)
	if err != nil {
		return rejectedCompactionResultWithError(compactionRejectReason(err), err)
	}
	if measurement.InputTokens >= measurement.InputLimit {
		return rejectedCompactionResultWithError(
			event.CompactRejectSummaryTooLarge,
			&loop.SummaryTooLargeError{Measurement: measurement},
		)
	}
	template, err := contextFingerprintTemplateForRequest(
		request, candidate.RuntimeRevision, e.config.CounterCapability, candidate.InferenceCapability,
	)
	if err != nil {
		return rejectedCompactionResult(event.CompactRejectInternal)
	}
	return contextCompactionAwaitResult{
		Disposition: contextCompactionAwaitCommitted,
		Proposal: compactionFinalizationProposal{Success: &compactionPreparedSuccess{
			Model: candidate.Measurement.Model, RequestFingerprint: candidate.Measurement.RequestFingerprint,
			Summary: cloneUserMessage(value.Summary),
			PostCount: compactionPostCount{
				Model: measurement.Model, InputTokens: measurement.InputTokens, InputLimit: measurement.InputLimit,
				Quality: measurement.Quality, Fingerprint: template,
			},
		}},
	}
}

func rejectedCompactionResult(reason event.CompactRejectReason) contextCompactionAwaitResult {
	return rejectedCompactionResultWithError(reason, nil)
}

func rejectedCompactionResultWithError(reason event.CompactRejectReason, continuationError error) contextCompactionAwaitResult {
	return contextCompactionAwaitResult{
		Disposition:       contextCompactionAwaitRejected,
		Proposal:          compactionFinalizationProposal{RejectReason: reason},
		ContinuationError: continuationError,
	}
}

func compactionRejectReason(err error) event.CompactRejectReason {
	if err == nil {
		return event.CompactRejectInternal
	}
	var invalid *loop.InvalidSummaryError
	if errors.As(err, &invalid) {
		return event.CompactRejectInvalidSummary
	}
	var unknown *loop.ContextLimitUnknownError
	if errors.As(err, &unknown) {
		return event.CompactRejectContextLimitUnknown
	}
	var count *contextcount.ContextCountError
	if errors.As(err, &count) {
		return event.CompactRejectContextCountFailed
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return event.CompactRejectCanceled
	}
	return event.CompactRejectExecutionFailed
}
