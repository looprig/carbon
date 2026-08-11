package sessionruntime

import (
	"context"
	"errors"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/tool"
)

// foreignDeliveryResponsivenessBound is a private observer-only bound used for
// coordinator bookkeeping. Reaching it never reports a model-visible delivery
// result, changes the hook, or interrupts the target actor; callers continue
// waiting for the actor's concrete delivery phase unless an explicit response
// observer deadline/cancellation ends their wait.
const foreignDeliveryResponsivenessBound = 250 * time.Millisecond

type foreignDeliveryObserverTimer interface {
	Chan() <-chan time.Time
	Stop() bool
}

type runtimeForeignDeliveryObserverTimer struct{ timer *time.Timer }

func (t *runtimeForeignDeliveryObserverTimer) Chan() <-chan time.Time {
	if t == nil || t.timer == nil {
		return nil
	}
	return t.timer.C
}

func (t *runtimeForeignDeliveryObserverTimer) Stop() bool {
	return t != nil && t.timer != nil && t.timer.Stop()
}

func newRuntimeForeignDeliveryObserverTimer(timeout time.Duration) foreignDeliveryObserverTimer {
	return &runtimeForeignDeliveryObserverTimer{timer: time.NewTimer(timeout)}
}

type foreignDeliveryCoordinatorAction uint8

const (
	foreignDeliveryCoordinatorFinish foreignDeliveryCoordinatorAction = iota + 1
	foreignDeliveryCoordinatorDetach
)

type foreignDeliveryCoordinatorUpdate struct {
	status tool.DelegateDeliveryStatus
}

// foreignDeliveryCoordinator is the one session-owned lifecycle owner for a
// foreign accepted request. The Execute caller only observes updates and may
// relinquish its response wait; all later hook/target cleanup and background
// handback decisions stay on this goroutine.
type foreignDeliveryCoordinator struct {
	session    *Session
	manager    *delegationManager
	parentID   uuid.UUID
	childID    uuid.UUID
	requestID  uuid.UUID
	name       string
	hook       *foreignDeliveryHook
	tracked    *requestTracker
	sub        event.Subscription
	drainState *drainCorrelationState
	background bool
	timeout    *int

	updates chan foreignDeliveryCoordinatorUpdate
	actions chan foreignDeliveryCoordinatorAction
	done    chan struct{}
	// pendingSeen and pendingNotified are owned by the coordinator goroutine.
	// The private responsiveness observation must not consume the one-shot
	// notification needed when an explicit response deadline later fires.
	pendingSeen     bool
	pendingNotified bool
}

func newForeignDeliveryCoordinator(c *scopedController, s *Session, childID, requestID uuid.UUID,
	name string, hook *foreignDeliveryHook, tracked *requestTracker, sub event.Subscription,
	background bool, timeout *int,
) *foreignDeliveryCoordinator {
	coordinator := &foreignDeliveryCoordinator{
		session:    s,
		manager:    c.manager,
		parentID:   c.parentLoopID,
		childID:    childID,
		requestID:  requestID,
		name:       name,
		hook:       hook,
		tracked:    tracked,
		sub:        sub,
		drainState: &drainCorrelationState{},
		background: background,
		timeout:    timeout,
		updates:    make(chan foreignDeliveryCoordinatorUpdate, 2),
		actions:    make(chan foreignDeliveryCoordinatorAction, 1),
		done:       make(chan struct{}),
	}
	if hook != nil {
		hook.claimDeliveryWaiter(requestID)
	}
	go coordinator.run()
	return coordinator
}

func (c *foreignDeliveryCoordinator) observerTimer(timeout time.Duration) foreignDeliveryObserverTimer {
	if c != nil && c.manager != nil && c.manager.foreignDeliveryTimerFactory != nil {
		return c.manager.foreignDeliveryTimerFactory(timeout)
	}
	return newRuntimeForeignDeliveryObserverTimer(timeout)
}

func (c *foreignDeliveryCoordinator) detach() {
	c.action(foreignDeliveryCoordinatorDetach)
}

func (c *foreignDeliveryCoordinator) finish() {
	c.action(foreignDeliveryCoordinatorFinish)
}

func (c *foreignDeliveryCoordinator) action(action foreignDeliveryCoordinatorAction) {
	if c == nil {
		return
	}
	select {
	case <-c.done:
		return
	case c.actions <- action:
	}
}

func (c *foreignDeliveryCoordinator) notify(status tool.DelegateDeliveryStatus) {
	if c == nil || status == "" {
		return
	}
	select {
	case c.updates <- foreignDeliveryCoordinatorUpdate{status: status}:
	default:
		// A caller that has already transferred ownership cannot consume a
		// later concrete update. The coordinator still performs cleanup from
		// the hook state; dropping this observer-only notification is safe.
	}
}

func (c *foreignDeliveryCoordinator) markPending(notify bool) {
	if c == nil {
		return
	}
	if !c.pendingSeen {
		c.pendingSeen = true
		c.tracked.markDelivery(tool.DelegateDeliveryAcceptedPending)
	}
	if !notify || c.pendingNotified {
		return
	}
	c.pendingNotified = true
	c.notify(tool.DelegateDeliveryAcceptedPending)
}

func (c *foreignDeliveryCoordinator) run() {
	defer close(c.done)
	defer close(c.updates)
	defer c.cleanup()

	if c.hook == nil || c.session == nil || c.tracked == nil || c.sub == nil {
		return
	}
	observer := c.observerTimer(c.responsiveness())
	if observer != nil {
		defer observer.Stop()
	}
	responseCtx, responseCancel := waitContext(c.session.sessionCtx, c.timeout)
	defer responseCancel()
	responseDone := responseCtx.Done()

	concreteSeen := false
	handBackSent := false
	detached := c.background

	for {
		status, changed := c.hook.deliveryWaitState(c.requestID)
		if status != "" {
			if !concreteSeen {
				concreteSeen = true
				c.tracked.markDelivery(status)
				c.notify(status)
				if status == tool.DelegateDeliveryUnknown || status == tool.DelegateDeliveryUntrackable {
					if c.background && !handBackSent {
						c.sendCategoricalHandBack(status)
						handBackSent = true
					}
					return
				}
				if !c.trackable(status) {
					return
				}
				if c.background {
					if handBackSent {
						c.drainCleanupOnly()
					} else {
						c.sendTargetHandBack(status)
					}
				} else if detached {
					// A detached foreground observer transfers target lifecycle
					// ownership here, but never gains background handback rights.
					c.drainCleanupOnly()
				} else {
					c.waitForegroundAction()
				}
				return
			}
		}

		select {
		case action := <-c.actions:
			switch action {
			case foreignDeliveryCoordinatorFinish:
				return
			case foreignDeliveryCoordinatorDetach:
				detached = true
				if concreteSeen {
					c.drainCleanupOnly()
					return
				}
			}
		case <-changed:
			continue
		case <-observerChan(observer):
			// The responsiveness bound is private coordinator bookkeeping. It
			// must not synthesize a model-visible delivery result; the actor's
			// authoritative phase is the only concrete foreground/background
			// admission result.
			c.markPending(false)
			observer = nil
		case <-responseDone:
			if errors.Is(responseCtx.Err(), context.DeadlineExceeded) {
				// An explicit response observer deadline is allowed to return
				// accepted-pending while the coordinator retains ownership. A
				// foreground caller observes its own wait context instead, so only
				// background admission needs this notification.
				c.markPending(c.background)
				if c.background && !handBackSent {
					c.sendTimedOutHandBack()
					handBackSent = true
				}
				responseDone = nil
				continue
			}
			// Session cancellation is ownership shutdown, not delivery
			// evidence. The deferred cleanup removes only local tracking.
			return
		case <-c.session.sessionCtx.Done():
			return
		}
	}
}

func (c *foreignDeliveryCoordinator) waitForegroundAction() {
	for {
		select {
		case action := <-c.actions:
			switch action {
			case foreignDeliveryCoordinatorFinish:
				return
			case foreignDeliveryCoordinatorDetach:
				c.drainCleanupOnly()
				return
			}
		case <-c.session.sessionCtx.Done():
			return
		}
	}
}

func (c *foreignDeliveryCoordinator) responsiveness() time.Duration {
	if c != nil && c.manager != nil && c.manager.foreignDeliveryResponsiveness > 0 {
		return c.manager.foreignDeliveryResponsiveness
	}
	return foreignDeliveryResponsivenessBound
}

func observerChan(timer foreignDeliveryObserverTimer) <-chan time.Time {
	if timer == nil {
		return nil
	}
	return timer.Chan()
}

func (c *foreignDeliveryCoordinator) trackable(status tool.DelegateDeliveryStatus) bool {
	return status == tool.DelegateDeliveryQueued || status == tool.DelegateDeliveryInjected
}

func (c *foreignDeliveryCoordinator) cleanup() {
	if c == nil {
		return
	}
	if c.hook != nil {
		// Session cancellation is ownership abandonment, not delivery evidence.
		// Clear any still-live process-local command/phase payload before releasing
		// the coordinator's waiter reference; the durable intent remains for restore.
		c.hook.abandon(c.requestID)
	}
	if c.tracked != nil {
		c.tracked.markTerminal()
	}
	if c.sub != nil {
		_ = c.sub.Close()
	}
	if c.manager != nil {
		c.manager.removeRequest(c.requestID, c.tracked)
	}
}

func (c *foreignDeliveryCoordinator) sendCategoricalHandBack(status tool.DelegateDeliveryStatus) {
	if c.session.sessionCtx.Err() != nil {
		return
	}
	blocks := backgroundCompletionBlocksWithState(c.childID, c.name, c.requestID,
		tool.DelegateStatusUnknown, "", status, true)
	if err := c.session.deliverSubagentResult(c.session.sessionCtx, c.parentID, c.childID, blocks); err != nil {
		c.session.cancelExpectTurn(context.Background(), c.childID)
	}
}

func (c *foreignDeliveryCoordinator) sendTimedOutHandBack() {
	if c.session.sessionCtx.Err() != nil {
		return
	}
	blocks := backgroundCompletionBlocksWithState(c.childID, c.name, c.requestID,
		tool.DelegateStatusTimedOut, "", tool.DelegateDeliveryAcceptedPending, true)
	if err := c.session.deliverSubagentResult(c.session.sessionCtx, c.parentID, c.childID, blocks); err != nil {
		c.session.cancelExpectTurn(context.Background(), c.childID)
	}
}

func (c *foreignDeliveryCoordinator) drainCleanupOnly() {
	if c.session.sessionCtx.Err() != nil {
		return
	}
	_, _ = drainDelegateAnswerObservedWithState(c.session.sessionCtx, c.sub, c.requestID, nil, c.tracked.markOpening, c.drainState)
}

func (c *foreignDeliveryCoordinator) sendTargetHandBack(status tool.DelegateDeliveryStatus) {
	if c.session.sessionCtx.Err() != nil {
		return
	}
	waitCtx, cancel := waitContext(c.session.sessionCtx, c.timeout)
	defer cancel()
	text, err := drainDelegateAnswerObservedWithState(waitCtx, c.sub, c.requestID, nil, c.tracked.markOpening, c.drainState)
	observerExpired := drainObserverExpired(err)
	responseStatus := statusFromDrain(err)
	if responseStatus == tool.DelegateStatusInterrupted && didTimeout(c.timeout, waitCtx) {
		responseStatus = tool.DelegateStatusTimedOut
	}
	if responseStatus == tool.DelegateStatusFailed && text == "" {
		text = delegateFailureDetail(err)
	}
	if c.session.sessionCtx.Err() != nil {
		return
	}
	blocks := backgroundCompletionBlocksWithState(c.childID, c.name, c.requestID,
		responseStatus, text, status, observerExpired)
	if err := c.session.deliverSubagentResult(c.session.sessionCtx, c.parentID, c.childID, blocks); err != nil {
		c.session.cancelExpectTurn(context.Background(), c.childID)
	}
	if observerExpired && c.session.sessionCtx.Err() == nil {
		// A response timeout is only an observer outcome. Keep the tracker and
		// subscription owned by this coordinator until the eventual correlated
		// terminal arrives; no second handback is emitted.
		c.drainCleanupOnly()
	}
}

func (c *foreignDeliveryCoordinator) waitUpdate(ctx context.Context) (tool.DelegateDeliveryStatus, bool) {
	if c == nil {
		return "", false
	}
	select {
	case update, ok := <-c.updates:
		if !ok {
			return "", false
		}
		return update.status, true
	case <-c.done:
		// The update channel is closed after the final run step. Drain a
		// buffered concrete notification before reporting completion.
		select {
		case update, ok := <-c.updates:
			if ok {
				return update.status, true
			}
		default:
		}
		return "", false
	case <-ctx.Done():
		return "", false
	}
}

func (c *foreignDeliveryCoordinator) tryUpdate() (tool.DelegateDeliveryStatus, bool) {
	if c == nil {
		return "", false
	}
	select {
	case update, ok := <-c.updates:
		if !ok {
			return "", false
		}
		return update.status, true
	default:
		return "", false
	}
}

func (c *scopedController) resolveForeignForeground(ctx context.Context, s *Session, childID, requestID uuid.UUID,
	tracked *requestTracker, sub event.Subscription, req tool.DelegateRequest, hook *foreignDeliveryHook,
) (tool.DelegateResult, error) {
	coordinator := newForeignDeliveryCoordinator(c, s, childID, requestID, req.Name, hook, tracked, sub, false, req.TimeoutSeconds)
	waitCtx, cancel := waitContext(ctx, req.TimeoutSeconds)
	defer cancel()
	{
		status, ok := coordinator.waitUpdate(waitCtx)
		if !ok {
			// A caller deadline and the hook transition can be concurrent. Give
			// already-committed concrete evidence precedence over the observer
			// timeout so the response remains queued/injected while its target
			// drain reports the caller interruption.
			status, ok = hook.deliveryStatus(requestID), true
			if status == "" {
				status, ok = coordinator.tryUpdate()
			}
		}
		if !ok || status == "" {
			if s.sessionCtx.Err() != nil {
				coordinator.detach()
				return c.foreignObserverResult(s, childID, requestID, tool.DelegateDeliveryAcceptedPending, tool.DelegateResponseUnknown, req.Name), nil
			}
			if waitCtx.Err() != nil {
				coordinator.detach()
				response := tool.DelegateResponseInterrupted
				if didTimeout(req.TimeoutSeconds, waitCtx) {
					response = tool.DelegateResponseTimedOut
				}
				return c.foreignObserverResult(s, childID, requestID, tool.DelegateDeliveryAcceptedPending, response, req.Name), nil
			}
			coordinator.detach()
			return c.foreignObserverResult(s, childID, requestID, tool.DelegateDeliveryAcceptedPending, tool.DelegateResponseUnknown, req.Name), nil
		}
		tracked.markDelivery(status)
		if status == tool.DelegateDeliveryAcceptedPending {
			coordinator.detach()
			return c.foreignObserverResult(s, childID, requestID, status, tool.DelegateResponseUnknown, req.Name), nil
		}
		if status == tool.DelegateDeliveryUnknown || status == tool.DelegateDeliveryUntrackable {
			return c.foreignObserverResult(s, childID, requestID, status, tool.DelegateResponseUnknown, req.Name), nil
		}
		if !coordinator.trackable(status) {
			return c.foreignObserverResult(s, childID, requestID, status, tool.DelegateResponseUnknown, req.Name), nil
		}

		text, err := drainDelegateAnswerObservedWithState(waitCtx, sub, requestID, nil, tracked.markOpening, coordinator.drainState)
		responseStatus := statusFromDrain(err)
		if responseStatus == tool.DelegateStatusInterrupted && didTimeout(req.TimeoutSeconds, waitCtx) {
			responseStatus = tool.DelegateStatusTimedOut
		}
		if responseStatus == tool.DelegateStatusFailed && text == "" {
			text = delegateFailureDetail(err)
		}
		if drainObserverExpired(err) {
			coordinator.detach()
		} else {
			coordinator.finish()
		}
		result := c.responseResult(s, childID, requestID, responseStatus, text)
		result.DeliveryStatus = tracked.openingStatus()
		if drainObserverExpired(err) {
			result.State = tool.AgentStateWorking
		}
		if req.Name != "" {
			result.Name = req.Name
		}
		return result, nil
	}
}

func (c *scopedController) resolveForeignBackground(s *Session, childID, requestID uuid.UUID,
	tracked *requestTracker, sub event.Subscription, req tool.DelegateRequest, hook *foreignDeliveryHook,
) (tool.DelegateResult, error) {
	name := req.Name
	if name == "" {
		name = c.agentSnapshot(s, childID).Name
	}
	coordinator := newForeignDeliveryCoordinator(c, s, childID, requestID, name, hook, tracked, sub, true, req.TimeoutSeconds)
	status, ok := coordinator.waitUpdate(s.sessionCtx)
	if !ok {
		status = tool.DelegateDeliveryAcceptedPending
	}
	if status == "" {
		status = tool.DelegateDeliveryAcceptedPending
	}
	result := c.agentResult(s, childID)
	result.CorrelationID = requestID
	result.State = tool.AgentStateWorking
	result.DeliveryStatus = status
	if req.Name != "" {
		result.Name = req.Name
	}
	return result, nil
}

func (c *scopedController) foreignObserverResult(s *Session, childID, requestID uuid.UUID,
	delivery tool.DelegateDeliveryStatus, response tool.DelegateResponseStatus, name string,
) tool.DelegateResult {
	result := c.agentResult(s, childID)
	result.CorrelationID = requestID
	result.State = tool.AgentStateWorking
	result.DeliveryStatus = delivery
	result.ResponseStatus = response
	if name != "" {
		result.Name = name
	}
	return result
}
