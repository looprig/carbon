package sessionruntime

import (
	"context"
	"errors"
	"sync"

	"github.com/looprig/harness/pkg/tool"
)

var errSessionProcessServicesUnavailable = errors.New("session: process services unavailable")

// sessionProcessServiceBridge is the stable, session-owned indirection captured
// by process resources during activation. Task 24 attaches checked durable
// publication and completion delivery behind this object without replacing it:
// the bridge's pointer identity never changes across probe/restore/live
// activation, only its unexported delegate fields do, under mu. Until a delegate
// is attached, every call keeps returning the explicit unavailable error a
// resource sees before Task 24 lands.
type sessionProcessServiceBridge struct {
	mu                        sync.Mutex
	lifecyclePublisher        tool.ProcessLifecyclePublisher // nil until Task 24B attaches it
	completionNotifier        tool.ProcessCompletionNotifier // nil until Task 24C attaches it
	workflowActivityPublisher tool.WorkflowActivityPublisher // nil until Task 5 attaches it
	workflowActivityClosed    bool
	workflowActivityInFlight  int
	workflowActivityDrained   chan struct{}
	workflowActivityDrainOnce bool
}

func newSessionProcessServices() (
	*sessionProcessServiceBridge,
	tool.SessionResourceServices,
	error,
) {
	bridge := &sessionProcessServiceBridge{workflowActivityDrained: make(chan struct{})}
	services, err := tool.NewSessionResourceServices(bridge, bridge, bridge)
	if err != nil {
		return nil, tool.SessionResourceServices{}, err
	}
	return bridge, services, nil
}

// attachProcessLifecyclePublisher installs the checked durable publisher (Task
// 24B's implementation) behind the bridge WITHOUT replacing the bridge value
// itself: every resource that already captured this *sessionProcessServiceBridge
// through SessionResourceServices observes the checked behavior on its very next
// call, because the bridge's identity never changes. A nil publisher is ignored
// so an accidental nil injection can never erase an already-attached delegate and
// silently fall back to the unavailable stub.
func (b *sessionProcessServiceBridge) attachProcessLifecyclePublisher(p tool.ProcessLifecyclePublisher) {
	if p == nil {
		return
	}
	b.mu.Lock()
	b.lifecyclePublisher = p
	b.mu.Unlock()
}

// attachProcessCompletionNotifier installs the checked completion notifier
// (Task 24C's implementation, *Session.NotifyProcessCompletion) behind the
// bridge WITHOUT replacing the bridge value itself — the exact same pattern
// attachProcessLifecyclePublisher established for 24B: every resource that
// already captured this *sessionProcessServiceBridge through
// SessionResourceServices observes the checked behavior on its very next
// call, because the bridge's identity never changes. A nil notifier is
// ignored so an accidental nil injection can never erase an already-attached
// delegate and silently fall back to the unavailable stub.
func (b *sessionProcessServiceBridge) attachProcessCompletionNotifier(n tool.ProcessCompletionNotifier) {
	if n == nil {
		return
	}
	b.mu.Lock()
	b.completionNotifier = n
	b.mu.Unlock()
}

// attachWorkflowActivityPublisher installs the checked, session-bound workflow
// publisher without replacing the stable bridge captured by already-activated
// resources. A nil value never erases a working delegate.
func (b *sessionProcessServiceBridge) attachWorkflowActivityPublisher(p tool.WorkflowActivityPublisher) {
	if p == nil {
		return
	}
	b.mu.Lock()
	if b.workflowActivityClosed {
		b.mu.Unlock()
		return
	}
	b.workflowActivityPublisher = p
	b.mu.Unlock()
}

// closeWorkflowActivityPublisher seals new workflow publications and waits for
// any publication already admitted through the bridge to finish. The session
// calls this after session resources have shut down (so their terminal records
// still have a live Hub) and immediately before the Hub's SessionStopped edge.
// A retained SessionResourceServices value therefore cannot publish after the
// session teardown boundary, while an in-flight call is never cut off halfway
// through its durable append.
func (b *sessionProcessServiceBridge) closeWorkflowActivityPublisher(ctx context.Context) error {
	b.mu.Lock()
	if b.workflowActivityDrained == nil {
		b.workflowActivityDrained = make(chan struct{})
	}
	b.workflowActivityClosed = true
	b.closeWorkflowActivityDrainLocked()
	drained := b.workflowActivityDrained
	b.mu.Unlock()

	select {
	case <-drained:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *sessionProcessServiceBridge) closeWorkflowActivityDrainLocked() {
	if b.workflowActivityClosed && b.workflowActivityInFlight == 0 && !b.workflowActivityDrainOnce {
		close(b.workflowActivityDrained)
		b.workflowActivityDrainOnce = true
	}
}

func (b *sessionProcessServiceBridge) PublishProcessLifecycle(
	ctx context.Context,
	metadata tool.ProcessLifecycleMetadata,
) error {
	b.mu.Lock()
	publisher := b.lifecyclePublisher
	b.mu.Unlock()
	if publisher == nil {
		return errSessionProcessServicesUnavailable
	}
	return publisher.PublishProcessLifecycle(ctx, metadata)
}

func (b *sessionProcessServiceBridge) NotifyProcessCompletion(
	ctx context.Context,
	notification tool.ProcessCompletionNotification,
) error {
	b.mu.Lock()
	notifier := b.completionNotifier
	b.mu.Unlock()
	if notifier == nil {
		return errSessionProcessServicesUnavailable
	}
	return notifier.NotifyProcessCompletion(ctx, notification)
}

func (b *sessionProcessServiceBridge) PublishWorkflowActivity(
	ctx context.Context,
	metadata tool.WorkflowActivityMetadata,
) error {
	b.mu.Lock()
	if b.workflowActivityClosed {
		b.mu.Unlock()
		return errSessionProcessServicesUnavailable
	}
	publisher := b.workflowActivityPublisher
	if publisher != nil {
		b.workflowActivityInFlight++
	}
	b.mu.Unlock()
	if publisher == nil {
		return errSessionProcessServicesUnavailable
	}
	defer func() {
		b.mu.Lock()
		b.workflowActivityInFlight--
		b.closeWorkflowActivityDrainLocked()
		b.mu.Unlock()
	}()
	return publisher.PublishWorkflowActivity(ctx, metadata)
}

var (
	_ tool.ProcessLifecyclePublisher = (*sessionProcessServiceBridge)(nil)
	_ tool.ProcessCompletionNotifier = (*sessionProcessServiceBridge)(nil)
	_ tool.WorkflowActivityPublisher = (*sessionProcessServiceBridge)(nil)
)
