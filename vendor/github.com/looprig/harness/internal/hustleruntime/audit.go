package hustleruntime

import (
	"context"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/identity"
)

type auditRun struct {
	descriptor hustle.DefinitionDescriptor
	runID      hustle.RunID
	cause      identity.Cause
}

func (r *runtimeController) publishStarted(run auditRun) error {
	return classifyAuditError(r.publishAudit(event.HustleStarted{
		Header: r.auditHeader(run.cause),
		Run: event.HustleRunDescriptor{
			Definition: run.descriptor,
			RunID:      run.runID,
		},
	}), hustle.StageQueue, hustle.ReasonInternal)
}

func (r *runtimeController) publishCompleted(run auditRun, runtime event.ModelRuntime, result hustle.Result, duration time.Duration) error {
	return classifyAuditError(r.publishAudit(event.HustleCompleted{
		Header: r.auditHeader(run.cause),
		Run: event.HustleRunDescriptor{
			Definition: run.descriptor,
			RunID:      run.runID,
			Runtime:    runtime,
		},
		Duration: duration,
		Usage:    cloneUsage(result.Usage),
	}), hustle.StageTerminal, hustle.ReasonTerminal)
}

func (r *runtimeController) publishFailed(run auditRun, runtime event.ModelRuntime, usage *content.Usage, runErr *RunError, duration time.Duration) error {
	return classifyAuditError(r.publishAudit(event.HustleFailed{
		Header: r.auditHeader(run.cause),
		Run: event.HustleRunDescriptor{
			Definition: run.descriptor,
			RunID:      run.runID,
			Runtime:    runtime,
		},
		Duration:   duration,
		Stage:      runErr.Stage,
		ReasonCode: runErr.ReasonCode,
		Usage:      cloneUsage(usage),
	}), hustle.StageTerminal, hustle.ReasonTerminal)
}

func (r *runtimeController) publishQueueFailure(run auditRun, failure *QueueFailureError) error {
	reason := hustle.ReasonCanceled
	if failure.Reason == QueueFailureTimeout {
		reason = hustle.ReasonTimeout
	} else if failure.Reason == QueueFailurePoisoned {
		reason = hustle.ReasonInternal
	}
	return classifyAuditError(r.publishAudit(event.HustleFailed{
		Header: r.auditHeader(run.cause),
		Run: event.HustleRunDescriptor{
			Definition: run.descriptor,
			RunID:      run.runID,
		},
		Stage:      hustle.StageQueue,
		ReasonCode: reason,
	}), hustle.StageTerminal, hustle.ReasonTerminal)
}

func (r *runtimeController) auditHeader(cause identity.Cause) event.Header {
	return event.Header{
		Coordinates:     identity.Coordinates{SessionID: r.sessionID},
		Cause:           cause,
		EventVisibility: event.Internal,
	}
}

func (r *runtimeController) publishAudit(ev event.Event) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.sessionCtx), r.auditTimeout)
	defer cancel()
	stamped, err := r.stamp(ev)
	if err != nil {
		return &AuditError{Operation: AuditStamp, Cause: err}
	}
	if err := r.audit.PublishInternalEventChecked(ctx, stamped); err != nil {
		return &AuditError{Operation: AuditPublish, Cause: err}
	}
	return nil
}

func classifyAuditError(err error, stage hustle.Stage, reason hustle.ReasonCode) error {
	if err == nil {
		return nil
	}
	auditErr, ok := err.(*AuditError)
	if !ok {
		return err
	}
	auditErr.Stage = stage
	auditErr.ReasonCode = reason
	return auditErr
}

func (r *runtimeController) stamp(ev event.Event) (event.Event, error) {
	header, err := r.stamper.Stamp(ev.EventHeader())
	if err != nil {
		return nil, err
	}
	switch typed := ev.(type) {
	case event.HustleStarted:
		typed.Header = header
		return typed, nil
	case event.HustleCompleted:
		typed.Header = header
		return typed, nil
	case event.HustleFailed:
		typed.Header = header
		return typed, nil
	default:
		return nil, &RunStateError{}
	}
}
