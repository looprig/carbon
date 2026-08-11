package hustleruntime

import (
	"context"
	"errors"
	"math"
	"sync"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/hustle"
)

// Controller owns the two independent participation lanes. Task 19 layers the
// inference and audit state machine over the ownership seam in this file.
type Controller struct {
	sessionCtx context.Context
	blocking   *lane
	background *lane
	newRunID   RunIDFactory
	runtime    *runtimeController

	admissionMu sync.Mutex
	closeOnce   sync.Once
	closeErr    error
	poisoned    bool // guarded by admissionMu

	drainMu      sync.Mutex
	drainOwned   int
	drainClosing bool
	drained      chan struct{}
	drainedOnce  sync.Once
}

// New validates and constructs the two bounded lanes without starting workers.
func New(sessionCtx context.Context, config Config) (*Controller, error) {
	if sessionCtx == nil || sessionCtx.Err() != nil {
		return nil, &ConfigError{Reason: ConfigInvalidContext, Field: "context"}
	}
	if err := validateLaneLimits("blocking", config.Blocking); err != nil {
		return nil, err
	}
	if err := validateLaneLimits("background", config.Background); err != nil {
		return nil, err
	}
	newRunID := config.NewRunID
	if newRunID == nil {
		newRunID = uuid.New
	}
	controller := &Controller{
		sessionCtx: sessionCtx,
		blocking:   newLane(hustle.ParticipationBlocking, config.Blocking),
		background: newLane(hustle.ParticipationBackground, config.Background),
		newRunID:   newRunID,
		drained:    make(chan struct{}),
	}
	if config.Runtime != nil {
		var err error
		controller.runtime, err = newRuntimeController(sessionCtx, *config.Runtime)
		if err != nil {
			return nil, err
		}
		controller.runtime.owner = controller
	}
	return controller, nil
}

func validateLaneLimits(name string, limits LaneLimits) error {
	if limits.Concurrent <= 0 {
		return &ConfigError{Reason: ConfigInvalidConcurrent, Field: name + ".concurrent"}
	}
	if limits.Queued < 0 || limits.Queued > maxLaneQueued {
		return &ConfigError{Reason: ConfigInvalidQueued, Field: name + ".queued"}
	}
	if limits.Concurrent > math.MaxInt-limits.Queued {
		return &ConfigError{Reason: ConfigCapacityOverflow, Field: name}
	}
	return nil
}

// ownedRun is the Task 18 extension seam: Task 19 owns one, waits for its FIFO
// execution grant, performs audit/execution, then finalizes it exactly once.
type ownedRun struct {
	controller    *Controller
	lane          *lane
	id            hustle.RunID
	participation hustle.Participation
	callerCtx     context.Context
	finalizer     Finalizer
	sequence      uint64
	state         runState // guarded by lane.mu
	eligible      bool     // guarded by lane.mu
	granted       chan struct{}
	done          chan struct{}
	setupDone     chan struct{}
	setupErr      error
	cleanup       func() error
	queueAudit    func(*QueueFailureError) error

	finalizeOnce sync.Once
	finalizeErr  error
}

func (c *Controller) own(ctx context.Context, participation hustle.Participation, finalizer Finalizer) (*ownedRun, error) {
	return c.ownWithEligibility(ctx, participation, finalizer, true)
}

func (c *Controller) ownWithEligibility(ctx context.Context, participation hustle.Participation, finalizer Finalizer, eligible bool) (*ownedRun, error) {
	if ctx == nil {
		return nil, &AdmissionError{Reason: AdmissionInvalidContext, Participation: participation}
	}
	lane := c.laneFor(participation)
	if lane == nil {
		return nil, &AdmissionError{Reason: AdmissionInvalidParticipation, Participation: participation}
	}
	if finalizer == nil {
		return nil, &AdmissionError{Reason: AdmissionNilFinalizer, Participation: participation}
	}
	if err := ctx.Err(); err != nil {
		return nil, &AdmissionError{Reason: AdmissionInvalidContext, Participation: participation, Cause: err}
	}
	id, err := c.newRunID()
	if err != nil || id.IsZero() {
		return nil, &AdmissionError{Reason: AdmissionRunID, Participation: participation, Cause: err}
	}
	setupDone := make(chan struct{})
	run := &ownedRun{
		controller: c, lane: lane, id: hustle.RunID(id), participation: participation,
		callerCtx: ctx, finalizer: finalizer, eligible: eligible, granted: make(chan struct{}), done: make(chan struct{}), setupDone: setupDone,
	}
	if eligible {
		close(setupDone)
	}
	c.admissionMu.Lock()
	defer c.admissionMu.Unlock()
	if c.poisoned {
		return nil, &AdmissionError{Reason: AdmissionPoisoned, Participation: participation}
	}
	if err := lane.enqueue(run); err != nil {
		return nil, err
	}
	c.addDrainOwnership()
	return run, nil
}

func (c *Controller) addDrainOwnership() {
	c.drainMu.Lock()
	c.drainOwned++
	c.drainMu.Unlock()
}

func (c *Controller) isPoisoned() bool {
	c.admissionMu.Lock()
	defer c.admissionMu.Unlock()
	return c.poisoned
}

func (c *Controller) releaseDrainOwnership() {
	c.drainMu.Lock()
	c.drainOwned--
	shouldClose := c.drainClosing && c.drainOwned == 0
	c.drainMu.Unlock()
	if shouldClose {
		c.drainedOnce.Do(func() { close(c.drained) })
	}
}

func (c *Controller) beginDrainClose() {
	c.drainMu.Lock()
	c.drainClosing = true
	shouldClose := c.drainOwned == 0
	c.drainMu.Unlock()
	if shouldClose {
		c.drainedOnce.Do(func() { close(c.drained) })
	}
}

// Drained closes after admission is closed or poisoned and every owned run has
// completed terminal audit, finalization, and activity cleanup.
func (c *Controller) Drained() <-chan struct{} { return c.drained }

func (c *Controller) laneFor(participation hustle.Participation) *lane {
	switch participation {
	case hustle.ParticipationBlocking:
		return c.blocking
	case hustle.ParticipationBackground:
		return c.background
	default:
		return nil
	}
}

func (r *ownedRun) awaitExecution() error {
	for {
		switch r.lane.stateOf(r) {
		case runExecuting:
			return nil
		case runFinalizing, runDone:
			<-r.done
			return r.finalizeErr
		}
		select {
		case <-r.granted:
			return nil
		case <-r.callerCtx.Done():
			if r.lane.cancelQueued(r) {
				return r.finishQueued(context.WithoutCancel(r.controller.sessionCtx), queueFailureReason(r.callerCtx.Err()), r.callerCtx.Err())
			}
		case <-r.controller.sessionCtx.Done():
			if r.lane.cancelQueued(r) {
				return r.finishQueued(context.WithoutCancel(r.controller.sessionCtx), QueueFailureCanceled, r.controller.sessionCtx.Err())
			}
		case <-r.done:
			return r.finalizeErr
		}
	}
}

func queueFailureReason(err error) QueueFailureReason {
	if errors.Is(err, context.DeadlineExceeded) {
		return QueueFailureTimeout
	}
	return QueueFailureCanceled
}

func (r *ownedRun) finalize(ctx context.Context, outcome hustle.Outcome) error {
	return r.finish(ctx, outcome, nil, false)
}

func (r *ownedRun) finishQueued(ctx context.Context, reason QueueFailureReason, cause error) error {
	<-r.setupDone
	if r.setupErr != nil {
		return r.finish(ctx, hustle.Outcome{Err: r.setupErr}, nil, true)
	}
	failure := &QueueFailureError{
		RunID: r.id, Participation: r.participation, Stage: hustle.StageQueue, Reason: reason, Cause: cause,
	}
	if r.queueAudit != nil {
		failure.TerminalErr = r.queueAudit(failure)
		if failure.TerminalErr != nil {
			r.controller.reportFault(failure.TerminalErr)
		}
	}
	return r.finish(ctx, hustle.Outcome{Err: failure}, failure, true)
}

func (r *ownedRun) finish(ctx context.Context, outcome hustle.Outcome, queueFailure *QueueFailureError, preExecution bool) error {
	r.finalizeOnce.Do(func() {
		defer close(r.done)
		defer r.controller.releaseDrainOwnership()
		if !preExecution {
			if err := r.lane.beginFinalizing(r); err != nil {
				r.finalizeErr = err
				return
			}
		}
		finalizerCtx, finalizerCancel := r.controller.finalizationContext(ctx)
		defer finalizerCancel()
		if err := callFinalizer(finalizerCtx, r.finalizer, outcome); err != nil {
			finalizerErr := &FinalizerError{RunID: r.id, Cause: err}
			r.controller.reportFault(finalizerErr)
			if queueFailure != nil {
				queueFailure.FinalizerErr = finalizerErr
			} else if runErr, ok := outcome.Err.(*RunError); ok {
				runErr.FinalizerErr = finalizerErr
			} else {
				r.finalizeErr = finalizerErr
			}
		}
		if queueFailure != nil {
			r.finalizeErr = queueFailure
		} else if outcome.Err != nil {
			r.finalizeErr = outcome.Err
		}
		if r.cleanup != nil {
			if err := r.cleanup(); err != nil {
				r.controller.reportFault(err)
				if runErr, ok := outcome.Err.(*RunError); ok {
					runErr.CleanupErr = err
				} else if queueFailure != nil {
					queueFailure.CleanupErr = err
				} else if finalizerErr, ok := r.finalizeErr.(*FinalizerError); ok {
					finalizerErr.CleanupErr = err
				} else if r.finalizeErr == nil {
					r.finalizeErr = err
				}
			}
		}
		r.lane.complete(r)
	})
	<-r.done
	return r.finalizeErr
}

func callFinalizer(ctx context.Context, finalizer Finalizer, outcome hustle.Outcome) (err error) {
	defer func() {
		if recover() != nil {
			err = &CallbackPanicError{Stage: hustle.StageFinalization}
		}
	}()
	return finalizer(ctx, outcome)
}

func (c *Controller) finalizationContext(fallback context.Context) (context.Context, context.CancelFunc) {
	if c.runtime != nil {
		return c.runtime.newFinalizationContext()
	}
	if fallback == nil {
		fallback = context.Background()
	}
	return fallback, func() {}
}

func (c *Controller) reportFault(err error) {
	if err != nil && c.runtime != nil {
		c.runtime.reportFault(err)
	}
}

func (r *ownedRun) completeSetup(err error, cleanup func() error, queueAudit func(*QueueFailureError) error) {
	r.setupErr = err
	r.cleanup = cleanup
	r.queueAudit = queueAudit
	close(r.setupDone)
}

// Close atomically closes both admissions, cancels execution, and resolves
// every queued-owned node. Drained joins executing finalizers and cleanup.
func (c *Controller) Close(ctx context.Context) error {
	c.closeOnce.Do(func() {
		if ctx == nil {
			ctx = context.Background()
		}
		c.admissionMu.Lock()
		blocking := c.blocking.closeQueued()
		background := c.background.closeQueued()
		c.beginDrainClose()
		if c.runtime != nil {
			c.runtime.cancelExecutions()
		}
		c.admissionMu.Unlock()
		failures := make([]*FinalizerError, 0)
		for _, run := range append(blocking, background...) {
			err := run.finishQueued(ctx, QueueFailureClosed, nil)
			var queueFailure *QueueFailureError
			if errorsAsQueueFailure(err, &queueFailure) && queueFailure.FinalizerErr != nil {
				failures = append(failures, queueFailure.FinalizerErr)
			}
		}
		if len(failures) > 0 {
			c.closeErr = &CloseError{Failures: failures}
		}
	})
	return c.closeErr
}

func (c *Controller) poison(cause *WorkerPoisonError) {
	c.admissionMu.Lock()
	c.poisoned = true
	blocking := c.blocking.closeQueued()
	background := c.background.closeQueued()
	c.beginDrainClose()
	if c.runtime != nil {
		c.runtime.cancelExecutions()
	}
	c.admissionMu.Unlock()
	c.reportFault(cause)
	for _, run := range append(blocking, background...) {
		_ = run.finishQueued(context.Background(), QueueFailurePoisoned, cause)
	}
}

func errorsAsQueueFailure(err error, target **QueueFailureError) bool {
	queueFailure, ok := err.(*QueueFailureError)
	if ok {
		*target = queueFailure
	}
	return ok
}
