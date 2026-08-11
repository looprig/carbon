package sessionruntime

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/harness/pkg/workspacestore"
)

type checkpointTrigger uint8

const (
	checkpointManual checkpointTrigger = iota + 1
	checkpointOnIdle
	checkpointOnTurnDone
	checkpointOnStepDone
)

type checkpointPriority uint8

const (
	checkpointBestEffort checkpointPriority = iota
	checkpointRequired
)

var errCheckpointActivated = errors.New("sessionruntime: checkpoint canceled by session activation")

// bestEffortAcquireTimeout bounds how long a best-effort checkpoint's workspace
// permit Acquire may run while a live caller is synchronously waiting on it (see
// runBestEffort's accepted channel). That caller is the loop actor's own
// goroutine -- sessionIdle and the native StepDone/TurnDone boundary are both
// invoked synchronously from the same call stack that publishes the actor's own
// terminal events (pkg/hub's commit/derive -> CommitSessionIdle/CommitBoundary) --
// so it must stay free to keep reading the actor's own command channel. A
// long-running background command can hold the workspace's exclusive lifetime
// lease well past its own turn's synchronous end, which makes Acquire block for
// as long as that command runs. Bounding Acquire itself, rather than leaving it to
// the full snapshot-policy timeout in the product composition root's real
// assembly (60s), lets a stalled permit fall through to the existing
// "publish the trigger without a checkpoint, retry on the next eligible boundary"
// fallback fast enough to matter
// to a live caller. checkpointRequired/manual checkpoints never pass a non-nil
// accepted callback into commit, so they are unaffected by this bound.
const bestEffortAcquireTimeout = 200 * time.Millisecond

type checkpointPolicy struct {
	Trigger  checkpointTrigger
	Priority checkpointPriority
	Timeout  time.Duration
}

// SnapshotPolicy is the internal composition form of rig.SnapshotPolicy. pkg/rig owns
// the public API and converts its validated values into this dependency-only shape.
type SnapshotPolicy struct {
	Trigger  checkpointTrigger
	Priority checkpointPriority
	Timeout  time.Duration
}

const (
	SnapshotManual     = checkpointManual
	SnapshotOnIdle     = checkpointOnIdle
	SnapshotOnTurnDone = checkpointOnTurnDone
	SnapshotOnStepDone = checkpointOnStepDone
	SnapshotBestEffort = checkpointBestEffort
	SnapshotRequired   = checkpointRequired
)

func (p SnapshotPolicy) internal() checkpointPolicy {
	return checkpointPolicy(p)
}

type checkedCheckpointPublisher interface {
	PublishEventChecked(context.Context, event.Event) error
}

type checkpointControllerConfig struct {
	SessionID      uuid.UUID
	Policy         checkpointPolicy
	Store          *workspacestore.Store
	Root           string
	Mode           WorkspacePlacementMode
	Coordinator    tool.WorkspaceCoordinator
	Publisher      checkedCheckpointPublisher
	Factory        *event.Factory
	Idle           func() bool
	Fault          func(error)
	Faulted        func() error
	Recover        func()
	Admission      func(context.Context) (func(), error)
	RequiredQueued func(event.Event)
	ManualQueued   func()
	ObservePending func()
	ObserveError   func(error)
}

type checkpointController struct {
	cfg checkpointControllerConfig

	ctx    context.Context
	cancel context.CancelFunc
	once   sync.Once

	mu                sync.Mutex
	active            bool
	pending           *checkpointRequest
	manualWaiting     int
	closed            bool
	workers           int
	drained           chan struct{}
	drainedOpen       bool
	activeCancel      context.CancelCauseFunc
	activationPending bool
	requiredQueue     []*requiredRequest
	requiredRunner    bool
	requiredNext      uint64
	requiredCompleted uint64
	requiredFinished  map[uint64]error
	interruptDeferred map[uint64]*interruptCheckpointSweep
	interruptNext     uint64
	interruptSweeps   map[uint64]*interruptCheckpointSweep
	// interruptTransferred is a deterministic package-test seam invoked after
	// the atomic live-to-ready/deferred handoff. Production leaves it nil.
	interruptTransferred func()
}

type checkpointRequest struct {
	ctx              context.Context
	trigger          event.Event
	alreadyPublished bool
	kind             event.SnapshotTriggerKind
}

type checkpointResult struct {
	ref workspacestore.Ref
	err error
}

type interruptCheckpointDisposition uint8

const (
	interruptCheckpointAccepted interruptCheckpointDisposition = iota + 1
	interruptCheckpointCommitted
	interruptCheckpointFaulted
)

type interruptCheckpointOutcome struct {
	Disposition interruptCheckpointDisposition
	Err         error
}

// interruptCheckpointSweep is one interrupt generation's native checkpoint
// barrier. Registration happens before command fan-out, so only a later
// SessionIdle edge can resolve it. The buffered result lets the idle publication
// path finish without depending on the interrupt caller being scheduled.
type interruptCheckpointSweep struct {
	controller     *checkpointController
	id             uint64
	result         chan interruptCheckpointOutcome
	once           sync.Once
	cause          event.Event
	requiredAfter  uint64
	requiredCutoff uint64
	requiredErr    error
	idleSeen       bool
	idleOutcome    interruptCheckpointOutcome
}

func (s *interruptCheckpointSweep) await(ctx context.Context) (interruptCheckpointOutcome, error) {
	select {
	case outcome := <-s.result:
		return outcome, nil
	case <-ctx.Done():
		s.cancel()
		return interruptCheckpointOutcome{}, ctx.Err()
	}
}

func (s *interruptCheckpointSweep) cancel() {
	if s == nil || s.controller == nil {
		return
	}
	s.once.Do(func() {
		s.controller.mu.Lock()
		delete(s.controller.interruptSweeps, s.id)
		delete(s.controller.interruptDeferred, s.id)
		s.controller.mu.Unlock()
	})
}

type requiredRequest struct {
	ctx     context.Context
	trigger event.Event
	run     func(context.Context) (workspacestore.Ref, error)
	result  chan checkpointResult
	started bool
	seq     uint64
}

func newCheckpointController(cfg checkpointControllerConfig) *checkpointController {
	ctx, cancel := context.WithCancel(context.Background())
	drained := make(chan struct{})
	close(drained)
	return &checkpointController{
		cfg:               cfg,
		ctx:               ctx,
		cancel:            cancel,
		interruptSweeps:   make(map[uint64]*interruptCheckpointSweep),
		interruptDeferred: make(map[uint64]*interruptCheckpointSweep),
		requiredFinished:  make(map[uint64]error),
		drained:           drained,
	}
}

func (c *checkpointController) beginInterruptSweep() *interruptCheckpointSweep {
	c.mu.Lock()
	c.interruptNext++
	if c.interruptNext == 0 {
		c.interruptNext++
	}
	sweep := &interruptCheckpointSweep{
		controller:    c,
		id:            c.interruptNext,
		result:        make(chan interruptCheckpointOutcome, 1),
		requiredAfter: c.requiredNext,
	}
	if c.closed {
		c.mu.Unlock()
		closedErr := &CheckpointError{Kind: CheckpointCanceled, Cause: context.Canceled}
		c.resolveInterruptSweeps([]*interruptCheckpointSweep{sweep}, interruptCheckpointOutcome{Disposition: interruptCheckpointFaulted, Err: closedErr})
		return sweep
	}
	c.interruptSweeps[sweep.id] = sweep
	c.mu.Unlock()
	return sweep
}

func (c *checkpointController) observeInterruptCause(trigger event.Event) {
	if _, ok := trigger.(event.TurnInterrupted); !ok {
		return
	}
	c.mu.Lock()
	for _, sweep := range c.interruptSweeps {
		if sweep.cause == nil {
			sweep.cause = trigger
		}
	}
	c.mu.Unlock()
}

// transferInterruptSweeps atomically removes every generation owned by this
// SessionIdle edge, captures the exact required-request cutoff, and places each
// generation in either the ready result or the deferred map. A required completion
// therefore always observes the sweep in one map or the other, while a request queued
// after this handoff is strictly beyond the captured cutoff.
func (c *checkpointController) transferInterruptSweeps() (ready, deferred []*interruptCheckpointSweep, cause event.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.interruptSweeps) == 0 {
		return nil, nil, nil
	}
	ids := make([]uint64, 0, len(c.interruptSweeps))
	for id := range c.interruptSweeps {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	ready = make([]*interruptCheckpointSweep, 0, len(ids))
	deferRequired := c.cfg.Policy.Priority == checkpointRequired &&
		c.cfg.Policy.Trigger != checkpointOnIdle && c.cfg.Policy.Trigger != checkpointManual
	cutoff := c.requiredNext
	for _, id := range ids {
		sweep := c.interruptSweeps[id]
		delete(c.interruptSweeps, id)
		if cause == nil && sweep.cause != nil {
			cause = sweep.cause
		}
		if deferRequired {
			sweep.requiredCutoff = cutoff
			if c.requiredCompleted < cutoff {
				c.interruptDeferred[id] = sweep
				deferred = append(deferred, sweep)
				continue
			}
		}
		ready = append(ready, sweep)
	}
	return ready, deferred, cause
}

func (c *checkpointController) resolveInterruptSweeps(sweeps []*interruptCheckpointSweep, outcome interruptCheckpointOutcome) {
	for _, sweep := range sweeps {
		resolved := outcome
		if sweep.requiredErr != nil {
			resolved = interruptCheckpointOutcome{Disposition: interruptCheckpointFaulted, Err: sweep.requiredErr}
		}
		sweep.once.Do(func() { sweep.result <- resolved })
	}
}

func (c *checkpointController) completeDeferredInterruptIdle(sweeps []*interruptCheckpointSweep, outcome interruptCheckpointOutcome) {
	c.mu.Lock()
	ready := make([]*interruptCheckpointSweep, 0, len(sweeps))
	for _, sweep := range sweeps {
		sweep.idleSeen = true
		sweep.idleOutcome = outcome
		if c.requiredCompleted >= sweep.requiredCutoff {
			delete(c.interruptDeferred, sweep.id)
			ready = append(ready, sweep)
		}
	}
	c.mu.Unlock()
	c.resolveInterruptSweeps(ready, outcome)
}

func (c *checkpointController) interruptSuccessDisposition() interruptCheckpointDisposition {
	if c.cfg.Policy.Priority == checkpointRequired && c.cfg.Policy.Trigger != checkpointManual {
		return interruptCheckpointCommitted
	}
	return interruptCheckpointAccepted
}

func (c *checkpointController) beginShutdown() <-chan struct{} {
	c.once.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.pending = nil
		interrupts := make([]*interruptCheckpointSweep, 0, len(c.interruptSweeps))
		for id, sweep := range c.interruptSweeps {
			interrupts = append(interrupts, sweep)
			delete(c.interruptSweeps, id)
		}
		for id, sweep := range c.interruptDeferred {
			interrupts = append(interrupts, sweep)
			delete(c.interruptDeferred, id)
		}
		c.mu.Unlock()
		shutdownErr := &CheckpointError{Kind: CheckpointCanceled, Cause: context.Canceled}
		c.resolveInterruptSweeps(interrupts, interruptCheckpointOutcome{Disposition: interruptCheckpointFaulted, Err: shutdownErr})
		c.cancel()
	})
	c.mu.Lock()
	drained := c.drained
	c.mu.Unlock()
	return drained
}

func (c *checkpointController) shutdown() {
	<-c.beginShutdown()
}

func (c *checkpointController) shutdownUntil(ctx context.Context) {
	select {
	case <-c.beginShutdown():
	case <-ctx.Done():
	}
}

// workerAddLocked reserves one controller-owned worker while c.mu is held.
func (c *checkpointController) workerAddLocked() {
	if c.workers == 0 {
		c.drained = make(chan struct{})
		c.drainedOpen = true
	}
	c.workers++
}

func (c *checkpointController) workerDone() {
	c.mu.Lock()
	c.workers--
	if c.workers == 0 && c.drainedOpen {
		close(c.drained)
		c.drainedOpen = false
	}
	c.mu.Unlock()
}

func (c *checkpointController) waitDrained() {
	c.mu.Lock()
	drained := c.drained
	c.mu.Unlock()
	<-drained
}

func (c *checkpointController) boundary(ctx context.Context, trigger event.Event) error {
	_, err := c.boundaryResult(ctx, trigger)
	return err
}

func (c *checkpointController) boundaryResult(ctx context.Context, trigger event.Event) (bool, error) {
	if c == nil || c.cfg.Publisher == nil {
		return false, &CheckpointError{Kind: CheckpointUnavailable}
	}
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		// Shutdown never manufactures a snapshot trigger. A real terminal already
		// produced by a draining turn still remains durable/live.
		err := c.cfg.Publisher.PublishEventChecked(ctx, trigger)
		return err == nil, err
	}
	c.observeInterruptCause(trigger)
	if !c.matches(trigger) {
		err := c.cfg.Publisher.PublishEventChecked(ctx, trigger)
		return err == nil, err
	}
	if c.cfg.Policy.Priority == checkpointBestEffort {
		err := c.bestEffortBoundary(ctx, trigger)
		return err == nil, err
	}
	published := false
	_, err := c.runRequired(ctx, trigger, func(runCtx context.Context) (workspacestore.Ref, error) {
		publish := func() error {
			err := c.cfg.Publisher.PublishEventChecked(runCtx, trigger)
			if err == nil {
				published = true
			}
			return err
		}
		ref, commitErr := c.commit(runCtx, trigger, checkpointTriggerKind(trigger), publish, nil)
		if commitErr != nil && !published {
			_ = publish()
		}
		return ref, commitErr
	})
	return published, err
}

func (c *checkpointController) sessionIdle(ctx context.Context, idle event.SessionIdle, publish func() error) error {
	if c == nil {
		return publish()
	}
	sweeps, deferredSweeps, interruptCause := c.transferInterruptSweeps()
	if c.interruptTransferred != nil {
		c.interruptTransferred()
	}
	disposition := c.interruptSuccessDisposition()
	var err error
	switch {
	case !c.matches(idle):
		err = publish()
	case c.cfg.Policy.Priority == checkpointBestEffort:
		if interruptCause != nil {
			err = c.bestEffortInterruptIdle(ctx, interruptCause, publish)
		} else {
			err = c.bestEffortBoundaryWithCommit(ctx, idle, publish)
		}
	default:
		_, err = c.runRequired(ctx, idle, func(runCtx context.Context) (workspacestore.Ref, error) {
			published := false
			trackedPublish := func() error {
				err := publish()
				if err == nil {
					published = true
				}
				return err
			}
			cause := event.Event(idle)
			kind := event.SnapshotTriggerIdle
			if interruptCause != nil {
				cause = interruptCause
				kind = event.SnapshotTriggerInterrupt
			}
			ref, commitErr := c.commit(runCtx, cause, kind, trackedPublish, nil)
			if commitErr != nil && !published {
				_ = trackedPublish()
			}
			return ref, commitErr
		})
	}
	if err != nil {
		disposition = interruptCheckpointFaulted
	}
	c.resolveInterruptSweeps(sweeps, interruptCheckpointOutcome{Disposition: disposition, Err: err})
	c.completeDeferredInterruptIdle(deferredSweeps, interruptCheckpointOutcome{Disposition: disposition, Err: err})
	return err
}

func (c *checkpointController) bestEffortInterruptIdle(ctx context.Context, cause event.Event, publish func() error) error {
	req := checkpointRequest{ctx: ctx, trigger: cause, kind: event.SnapshotTriggerInterrupt}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return publish()
	}
	if !c.active {
		c.active = true
		c.workerAddLocked()
		accepted := make(chan error, 1)
		c.mu.Unlock()
		c.runBestEffort(req, publish, accepted)
		return <-accepted
	}
	c.mu.Unlock()

	if err := publish(); err != nil {
		return err
	}
	req.alreadyPublished = true
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	if c.active || c.manualWaiting > 0 {
		copyReq := req
		c.pending = &copyReq
		c.mu.Unlock()
		return nil
	}
	c.active = true
	c.pending = nil
	c.workerAddLocked()
	c.mu.Unlock()
	c.runBestEffort(req, nil, nil)
	return nil
}

func (c *checkpointController) manual(ctx context.Context) (workspacestore.Ref, error) {
	if c == nil {
		return "", &CheckpointError{Kind: CheckpointUnavailable}
	}
	if c.cfg.Policy.Priority == checkpointRequired {
		if c.cfg.ManualQueued != nil {
			c.cfg.ManualQueued()
		}
		return c.runRequired(ctx, nil, func(runCtx context.Context) (workspacestore.Ref, error) {
			return c.commit(runCtx, nil, event.SnapshotTriggerManual, nil, nil)
		})
	}
	if !c.beginManualOperation() {
		return "", &CheckpointError{Kind: CheckpointCanceled, Cause: context.Canceled}
	}
	if c.cfg.ManualQueued != nil {
		c.cfg.ManualQueued()
	}
	defer c.workerDone()
	ref, err := c.commit(ctx, nil, event.SnapshotTriggerManual, nil, nil)
	c.mu.Lock()
	c.manualWaiting--
	start := c.takePendingLocked()
	if start != nil {
		c.workerAddLocked()
	}
	c.mu.Unlock()
	if start != nil {
		c.runBestEffort(*start, nil, nil)
	}
	return ref, err
}

func (c *checkpointController) bestEffortBoundary(ctx context.Context, trigger event.Event) error {
	return c.bestEffortBoundaryWithCommit(ctx, trigger, nil)
}

func (c *checkpointController) bestEffortBoundaryWithCommit(ctx context.Context, trigger event.Event, triggerCommit func() error) error {
	req := checkpointRequest{ctx: ctx, trigger: trigger}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		if triggerCommit != nil {
			return triggerCommit()
		}
		return c.cfg.Publisher.PublishEventChecked(ctx, trigger)
	}
	if !c.active {
		c.active = true
		c.workerAddLocked()
		accepted := make(chan error, 1)
		c.mu.Unlock()
		c.runBestEffort(req, triggerCommit, accepted)
		return <-accepted
	}
	c.mu.Unlock()

	// An active walk means this edge is coalescible. Persist/emit the execution edge
	// now, then retain only the latest already-published trigger for the next walk.
	var err error
	if triggerCommit != nil {
		err = triggerCommit()
	} else {
		err = c.cfg.Publisher.PublishEventChecked(ctx, trigger)
	}
	if err != nil {
		return err
	}
	req.alreadyPublished = true
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	if c.active || c.manualWaiting > 0 {
		copyReq := req
		c.pending = &copyReq
		c.mu.Unlock()
		return nil
	}
	c.active = true
	c.pending = nil
	c.workerAddLocked()
	c.mu.Unlock()
	c.runBestEffort(req, nil, nil)
	return nil
}

// runBestEffort starts a request whose wait-group slot was reserved while holding c.mu.
// Reserving before unlock prevents Shutdown's closed→Wait transition from missing it.
func (c *checkpointController) runBestEffort(req checkpointRequest, triggerCommit func() error, accepted chan error) {
	go func() {
		defer c.workerDone()
		workerCtx, cancel := context.WithCancelCause(req.ctx)
		defer cancel(context.Canceled)
		if c.cfg.Mode != PlacementShared {
			c.mu.Lock()
			c.activeCancel = cancel
			cancelNow := c.activationPending
			c.activationPending = false
			c.mu.Unlock()
			if cancelNow {
				cancel(errCheckpointActivated)
			}
		}
		signaled := false
		onAccepted := func() {
			if accepted != nil && !signaled {
				accepted <- nil
				signaled = true
			}
		}
		commit := triggerCommit
		if req.alreadyPublished {
			commit = func() error { return nil }
		}
		kind := req.kind
		if kind == event.SnapshotTriggerKindUnknown {
			kind = checkpointTriggerKind(req.trigger)
		}
		// commit only needs to know onAccepted when a live caller is actually
		// waiting on it (accepted != nil): that is precisely the signal it uses to
		// bound its own permit Acquire (see bestEffortAcquireTimeout). A fire-and-
		// forget walk (queued-pending drain, post-activation resume) has no such
		// caller and keeps the full policy timeout to acquire its permit.
		var commitAccepted func()
		if accepted != nil {
			commitAccepted = onAccepted
		}
		_, commitErr := c.commit(workerCtx, req.trigger, kind, commit, commitAccepted)
		observe := accepted == nil || signaled
		if accepted != nil && !signaled {
			// Best-effort progress still requires the execution trigger to survive a
			// permit/snapshot setup failure. Publish it without a checkpoint and retry
			// on the next eligible boundary.
			acceptErr := commitErr
			if !req.alreadyPublished {
				if triggerCommit != nil {
					acceptErr = triggerCommit()
				} else {
					acceptErr = c.cfg.Publisher.PublishEventChecked(req.ctx, req.trigger)
				}
			}
			accepted <- acceptErr
			// Once the trigger survives, the checkpoint failure is asynchronous from
			// the caller's perspective and must be observed internally.
			observe = acceptErr == nil
		}
		if observe {
			c.observeBestEffortError(workerCtx, commitErr)
		}
		c.mu.Lock()
		c.activeCancel = nil
		c.mu.Unlock()
		c.finishBestEffort()
	}()
}

func (c *checkpointController) runRequired(
	ctx context.Context,
	trigger event.Event,
	run func(context.Context) (workspacestore.Ref, error),
) (workspacestore.Ref, error) {
	req := &requiredRequest{ctx: ctx, trigger: trigger, run: run, result: make(chan checkpointResult, 1)}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return "", &CheckpointError{Kind: CheckpointCanceled, Cause: context.Canceled}
	}
	c.workerAddLocked()
	c.requiredNext++
	req.seq = c.requiredNext
	c.requiredQueue = append(c.requiredQueue, req)
	startRunner := !c.requiredRunner
	if startRunner {
		c.requiredRunner = true
	}
	c.mu.Unlock()
	if c.cfg.RequiredQueued != nil {
		c.cfg.RequiredQueued(trigger)
	}
	if startRunner {
		go c.runRequiredQueue()
	}

	select {
	case result := <-req.result:
		return result.ref, result.err
	case <-ctx.Done():
		c.mu.Lock()
		if !req.started {
			removed := false
			for i, queued := range c.requiredQueue {
				if queued == req {
					c.requiredQueue = append(c.requiredQueue[:i], c.requiredQueue[i+1:]...)
					removed = true
					break
				}
			}
			c.mu.Unlock()
			if removed {
				c.recordRequiredCompletion(req.seq, &CheckpointError{Kind: CheckpointCanceled, Cause: ctx.Err()})
				c.workerDone()
				return "", &CheckpointError{Kind: CheckpointCanceled, Cause: ctx.Err()}
			}
		} else {
			c.mu.Unlock()
		}
		// Once selected by the FIFO runner, a request owns completion; caller
		// cancellation cannot let a successor overtake its durable transaction.
		result := <-req.result
		return result.ref, result.err
	}
}

func (c *checkpointController) runRequiredQueue() {
	for {
		c.mu.Lock()
		if len(c.requiredQueue) == 0 {
			c.requiredRunner = false
			c.mu.Unlock()
			return
		}
		req := c.requiredQueue[0]
		c.requiredQueue = c.requiredQueue[1:]
		req.started = true
		c.mu.Unlock()

		ref, err := req.run(c.ctx)
		c.recordRequiredCompletion(req.seq, err)
		req.result <- checkpointResult{ref: ref, err: err}
		c.workerDone()
	}
}

func (c *checkpointController) recordRequiredCompletion(seq uint64, err error) {
	c.mu.Lock()
	c.requiredFinished[seq] = err
	if err != nil {
		for _, sweep := range c.interruptSweeps {
			if seq > sweep.requiredAfter {
				sweep.requiredErr = err
			}
		}
		for _, sweep := range c.interruptDeferred {
			if seq > sweep.requiredAfter && seq <= sweep.requiredCutoff {
				sweep.requiredErr = err
			}
		}
	}
	for {
		next := c.requiredCompleted + 1
		if _, ok := c.requiredFinished[next]; !ok {
			break
		}
		delete(c.requiredFinished, next)
		c.requiredCompleted = next
	}
	ready := make([]*interruptCheckpointSweep, 0)
	for id, sweep := range c.interruptDeferred {
		if sweep.idleSeen && c.requiredCompleted >= sweep.requiredCutoff {
			delete(c.interruptDeferred, id)
			ready = append(ready, sweep)
		}
	}
	c.mu.Unlock()
	for _, sweep := range ready {
		c.resolveInterruptSweeps([]*interruptCheckpointSweep{sweep}, sweep.idleOutcome)
	}
}

func (c *checkpointController) beginManualOperation() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false
	}
	c.workerAddLocked()
	c.manualWaiting++
	return true
}

// activated cancels an active best-effort quiescent walk when session progress resumes.
// Shared fuzzy walks deliberately continue because they make no quiescence claim.
func (c *checkpointController) activated() {
	if c == nil || c.cfg.Policy.Priority != checkpointBestEffort || c.cfg.Mode == PlacementShared {
		return
	}
	c.mu.Lock()
	cancel := c.activeCancel
	c.pending = nil
	if cancel == nil && c.active {
		c.activationPending = true
	}
	c.mu.Unlock()
	if cancel != nil {
		cancel(errCheckpointActivated)
	}
}

func (c *checkpointController) observeBestEffortError(workerCtx context.Context, err error) {
	if err == nil || c.cfg.ObserveError == nil {
		return
	}
	if c.cfg.ObservePending != nil {
		c.cfg.ObservePending()
	}
	// Activation and controller shutdown deliberately abandon a best-effort walk;
	// they are expected control flow, not operational checkpoint failures.
	if errors.Is(err, context.Canceled) &&
		(errors.Is(context.Cause(workerCtx), errCheckpointActivated) || c.ctx.Err() != nil) {
		return
	}
	c.cfg.ObserveError(err)
}

func (c *checkpointController) finishBestEffort() {
	c.mu.Lock()
	c.active = false
	c.activationPending = false
	start := c.takePendingLocked()
	if start != nil {
		c.workerAddLocked()
	}
	c.mu.Unlock()
	if start != nil {
		c.runBestEffort(*start, nil, nil)
	}
}

func (c *checkpointController) takePendingLocked() *checkpointRequest {
	if c.closed || c.active || c.manualWaiting > 0 || c.pending == nil {
		return nil
	}
	req := c.pending
	c.pending = nil
	c.active = true
	return req
}

func (c *checkpointController) matches(trigger event.Event) bool {
	switch c.cfg.Policy.Trigger {
	case checkpointOnIdle:
		_, ok := trigger.(event.SessionIdle)
		return ok
	case checkpointOnTurnDone:
		switch trigger.(type) {
		case event.TurnDone, event.TurnFailed, event.TurnInterrupted:
			return true
		}
	case checkpointOnStepDone:
		_, ok := trigger.(event.StepDone)
		return ok
	}
	return false
}

func (c *checkpointController) commit(caller context.Context, trigger event.Event, kind event.SnapshotTriggerKind, triggerCommit func() error, accepted func()) (ref workspacestore.Ref, err error) {
	if c.cfg.Store == nil || c.cfg.Coordinator == nil || c.cfg.Factory == nil {
		err = &CheckpointError{Kind: CheckpointUnavailable}
		if trigger != nil {
			c.latchRequired(err)
		}
		return "", err
	}
	ctx, cancel := c.operationContext(caller)
	defer cancel()
	admissionHeld := false
	durabilityAttempted := false
	defer func() {
		if admissionHeld {
			return
		}
		if err != nil && (trigger != nil || durabilityAttempted) {
			c.latchRequired(err)
		} else if err == nil && kind == event.SnapshotTriggerManual && c.cfg.Recover != nil {
			c.cfg.Recover()
		}
	}()
	if c.cfg.Admission != nil && (c.cfg.Policy.Priority == checkpointRequired || kind == event.SnapshotTriggerManual) {
		releaseAdmission, admissionErr := c.cfg.Admission(ctx)
		if admissionErr != nil {
			err = c.classifyError(admissionErr)
			return "", err
		}
		admissionHeld = true
		defer func() {
			if err != nil && (trigger != nil || durabilityAttempted) {
				c.latchRequired(err)
			} else if err == nil && kind == event.SnapshotTriggerManual && c.cfg.Recover != nil {
				// Recovery clears the workspace fault and admission latch while the
				// writer is still held; readers wake only after releaseAdmission.
				c.cfg.Recover()
			}
			releaseAdmission()
		}()
	}
	if trigger != nil && c.cfg.Policy.Priority == checkpointRequired && c.cfg.Faulted != nil {
		if fault := c.cfg.Faulted(); fault != nil {
			publish := triggerCommit
			if publish == nil {
				publish = func() error { return c.cfg.Publisher.PublishEventChecked(ctx, trigger) }
			}
			if publishErr := publish(); publishErr != nil {
				return "", &CheckpointError{Kind: CheckpointTriggerAppendFailed, Cause: publishErr}
			}
			return "", &CheckpointError{Kind: CheckpointFaulted, Cause: fault}
		}
	}
	if kind == event.SnapshotTriggerManual && (c.cfg.Idle == nil || !c.cfg.Idle()) {
		return "", &CheckpointError{Kind: CheckpointNotIdle}
	}
	durabilityAttempted = true
	acquireCtx := ctx
	if accepted != nil && c.cfg.Policy.Priority == checkpointBestEffort {
		// A live caller (the loop actor's own goroutine) is synchronously waiting
		// on accepted(); never let it wait anywhere near the full policy timeout
		// for a permit that a background process may hold for a long time. This
		// context.WithTimeout is naturally capped by ctx's own (policy) deadline
		// when that deadline is already shorter, so it never LENGTHENS the wait.
		var acquireCancel context.CancelFunc
		acquireCtx, acquireCancel = context.WithTimeout(ctx, bestEffortAcquireTimeout)
		defer acquireCancel()
	}
	permit, err := c.cfg.Coordinator.Acquire(acquireCtx, tool.WorkspaceOperationCheckpoint, "")
	if err != nil {
		return "", c.classifyError(err)
	}
	defer permit.Release()
	if err := c.cfg.Coordinator.Healthy(); err != nil {
		return "", c.classifyError(err)
	}
	if trigger != nil {
		publish := triggerCommit
		if publish == nil {
			publish = func() error { return c.cfg.Publisher.PublishEventChecked(ctx, trigger) }
		}
		if err := publish(); err != nil {
			return "", &CheckpointError{Kind: CheckpointTriggerAppendFailed, Cause: err}
		}
	}
	if accepted != nil {
		accepted()
	}
	ref, err = c.cfg.Store.Snapshot(ctx, c.cfg.Root)
	if err != nil {
		return "", c.classifyError(err)
	}
	var cause identity.Cause
	if trigger != nil {
		cause = checkpointCause(trigger.EventHeader())
	}
	h, err := c.cfg.Factory.Stamp(event.Header{Coordinates: identity.Coordinates{SessionID: c.cfg.SessionID}, Cause: cause})
	if err != nil {
		return "", &CheckpointError{Kind: CheckpointIDGenerationFailed, Cause: err}
	}
	cp := event.WorkspaceCheckpointed{Header: h, Ref: string(ref), Consistency: c.consistency(), Trigger: kind}
	if err := c.cfg.Publisher.PublishEventChecked(ctx, cp); err != nil {
		return "", &CheckpointError{Kind: CheckpointAppendFailed, Cause: err}
	}
	return ref, nil
}

func (c *checkpointController) latchRequired(err error) {
	if err != nil && c.cfg.Policy.Priority == checkpointRequired && c.cfg.Fault != nil {
		c.cfg.Fault(err)
	}
}

func (c *checkpointController) operationContext(caller context.Context) (context.Context, context.CancelFunc) {
	ctx, cancelCause := context.WithCancelCause(caller)
	stop := context.AfterFunc(c.ctx, func() { cancelCause(context.Canceled) })
	timeout := c.cfg.Policy.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	timed, cancelTimeout := context.WithTimeout(ctx, timeout)
	return timed, func() {
		stop()
		cancelTimeout()
		cancelCause(context.Canceled)
	}
}

func (c *checkpointController) classifyError(err error) error {
	kind := CheckpointFailed
	if errors.Is(err, context.DeadlineExceeded) {
		kind = CheckpointTimeout
	} else if errors.Is(err, context.Canceled) {
		kind = CheckpointCanceled
	}
	return &CheckpointError{Kind: kind, Cause: err}
}

func (c *checkpointController) consistency() event.SnapshotConsistency {
	if c.cfg.Mode == PlacementShared {
		return event.SnapshotFuzzy
	}
	return event.SnapshotQuiescent
}

func checkpointTriggerKind(trigger event.Event) event.SnapshotTriggerKind {
	switch trigger.(type) {
	case event.SessionIdle:
		return event.SnapshotTriggerIdle
	case event.TurnDone, event.TurnFailed, event.TurnInterrupted:
		return event.SnapshotTriggerTurnDone
	case event.StepDone:
		return event.SnapshotTriggerStepDone
	default:
		return event.SnapshotTriggerKindUnknown
	}
}

func checkpointCause(h event.Header) identity.Cause {
	return identity.Cause{Coordinates: h.Coordinates, EventID: h.EventID, Agency: identity.AgencyMachine}
}

type CheckpointErrorKind string

const (
	CheckpointUnavailable         CheckpointErrorKind = "unavailable"
	CheckpointNotIdle             CheckpointErrorKind = "not_idle"
	CheckpointFaulted             CheckpointErrorKind = "faulted"
	CheckpointFailed              CheckpointErrorKind = "failed"
	CheckpointTimeout             CheckpointErrorKind = "timeout"
	CheckpointCanceled            CheckpointErrorKind = "canceled"
	CheckpointTriggerAppendFailed CheckpointErrorKind = "trigger_append_failed"
	CheckpointAppendFailed        CheckpointErrorKind = "append_failed"
	CheckpointIDGenerationFailed  CheckpointErrorKind = "id_generation_failed"
)

type CheckpointError struct {
	Kind  CheckpointErrorKind
	Cause error
}

func (e *CheckpointError) Error() string {
	msg := "sessionruntime: workspace checkpoint failed (" + string(e.Kind) + ")"
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}
	return msg
}

func (e *CheckpointError) Unwrap() error { return e.Cause }
