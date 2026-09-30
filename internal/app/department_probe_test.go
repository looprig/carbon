package app

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/runtimecommand"
	"github.com/looprig/harness/pkg/session"
)

// probeController wraps a REAL launched harness session and forwards every
// capability harnessruntime binds, so Carbon's launch target accepts it like
// the session itself; a test then overrides ONE seam at a time.
//
// It exists because the adapter between Carbon and Host is now Host's
// harnessruntime, which refuses at bind a session missing any capability. A
// hand-built stub would have to re-derive all of them, and every assertion
// would become a statement about the stub; wrapping the real session keeps
// every seam the test does not name real.
//
// Every capability is a method on this type (Go method sets are not
// conditional), so a missing capability is expressed by a DIFFERENT wrapper,
// not a flag here: see closerlessProbe and uncommittedProbe.
type probeController struct {
	session.SessionController

	// faulted, when non-nil, replaces the session's persistence-fault signal.
	faulted     chan struct{}
	faultedOnce sync.Once
	abandons    atomic.Int32

	// events, when non-nil, is what SubscribeEvents answers (the live stream).
	events event.Subscription
	filter atomic.Pointer[event.EventFilter]

	// stubApply / stubClose record what the adapter built without reaching
	// harness, so a record harness would refuse is still observable.
	stubApply bool
	stubClose bool
	closeErr  error

	mu       sync.Mutex
	admitted []runtimecommand.Admitted
	closures []runtimecommand.Closure
}

func newProbe(real session.SessionController) *probeController {
	return &probeController{SessionController: real}
}

// inject latches the replaced fault signal.
func (p *probeController) inject() { p.faultedOnce.Do(func() { close(p.faulted) }) }

var errInjectedPersistenceFault = errors.New("carbon test: injected persistence fault")

func (p *probeController) WaitIdle(ctx context.Context) error {
	return p.SessionController.(session.IdleWaiter).WaitIdle(ctx)
}
func (p *probeController) Done() <-chan struct{} {
	return p.SessionController.(session.Liveness).Done()
}
func (p *probeController) ReleaseResidency(ctx context.Context) error {
	return p.SessionController.(session.Releaser).ReleaseResidency(ctx)
}
func (p *probeController) LeaseEpoch() (uint64, bool) {
	return p.SessionController.(session.LeaseEpochReporter).LeaseEpoch()
}
func (p *probeController) CommittedPublicEvents() (session.CommittedPublicEventSource, bool) {
	return p.SessionController.(session.CommittedPublicEventProvider).CommittedPublicEvents()
}
func (p *probeController) PersistenceFaulted() <-chan struct{} {
	if p.faulted != nil {
		return p.faulted
	}
	return p.SessionController.(session.PersistenceFaultReporter).PersistenceFaulted()
}
func (p *probeController) PersistenceFault() error {
	if p.faulted != nil {
		select {
		case <-p.faulted:
			return errInjectedPersistenceFault
		default:
			return nil
		}
	}
	return p.SessionController.(session.PersistenceFaultReporter).PersistenceFault()
}

// AbandonResidency counts an abandon only once the real one has returned, so a
// caller that sees the count also sees the lease hand-back it performed.
func (p *probeController) AbandonResidency(ctx context.Context) error {
	defer p.abandons.Add(1)
	return p.SessionController.(session.ResidencyAbandoner).AbandonResidency(ctx)
}

func (p *probeController) SubscribeEvents(filter event.EventFilter) (event.Subscription, error) {
	if p.events == nil {
		return p.SessionController.SubscribeEvents(filter)
	}
	p.filter.Store(&filter)
	return p.events, nil
}

// RuntimeCommands answers the probe itself as the applier, so every admitted
// command and closure the adapter builds passes through it.
func (p *probeController) RuntimeCommands() (runtimecommand.Applier, bool) {
	if _, ok := p.SessionController.(runtimecommand.Provider).RuntimeCommands(); !ok {
		return nil, false
	}
	return p, true
}

func (p *probeController) ApplyRuntimeCommand(ctx context.Context, admitted runtimecommand.Admitted) (runtimecommand.Disposition, error) {
	p.mu.Lock()
	p.admitted = append(p.admitted, admitted)
	p.mu.Unlock()
	if p.stubApply {
		return runtimecommand.Disposition{CommandID: admitted.CommandID, RuntimeCommandID: admitted.RuntimeCommandID}, nil
	}
	return p.realApplier().ApplyRuntimeCommand(ctx, admitted)
}

func (p *probeController) CloseAttempt(ctx context.Context, closure runtimecommand.Closure) (runtimecommand.ClosureResult, error) {
	p.mu.Lock()
	p.closures = append(p.closures, closure)
	calls := len(p.closures)
	p.mu.Unlock()
	if p.closeErr != nil {
		return runtimecommand.ClosureResult{}, p.closeErr
	}
	if p.stubClose {
		return runtimecommand.ClosureResult{Appended: true, Sequence: uint64(calls)}, nil
	}
	return p.realApplier().(runtimecommand.AttemptCloser).CloseAttempt(ctx, closure)
}

func (p *probeController) realApplier() runtimecommand.Applier {
	applier, _ := p.SessionController.(runtimecommand.Provider).RuntimeCommands()
	return applier
}

func (p *probeController) recordedAdmitted() []runtimecommand.Admitted {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]runtimecommand.Admitted(nil), p.admitted...)
}

func (p *probeController) recordedClosures() []runtimecommand.Closure {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]runtimecommand.Closure(nil), p.closures...)
}

// closerlessProbe's runtime-command applier applies but cannot close an
// attempt: the shape whose recovery declaration would be false.
type closerlessProbe struct{ *probeController }

func (p closerlessProbe) RuntimeCommands() (runtimecommand.Applier, bool) {
	return applyOnly{p.probeController}, true
}

type applyOnly struct{ probe *probeController }

func (a applyOnly) ApplyRuntimeCommand(ctx context.Context, admitted runtimecommand.Admitted) (runtimecommand.Disposition, error) {
	return a.probe.ApplyRuntimeCommand(ctx, admitted)
}

// uncommittedProbe reports no committed-bytes persistence, as a headless
// session does.
type uncommittedProbe struct{ *probeController }

func (uncommittedProbe) CommittedPublicEvents() (session.CommittedPublicEventSource, bool) {
	return nil, false
}

// scriptedSubscription is a live event stream a test feeds by hand.
type scriptedSubscription struct{ events chan event.Delivery }

func newScriptedSubscription(capacity int) *scriptedSubscription {
	return &scriptedSubscription{events: make(chan event.Delivery, capacity)}
}

func (s *scriptedSubscription) Events() <-chan event.Delivery { return s.events }
func (s *scriptedSubscription) Close() error                  { return nil }
func (s *scriptedSubscription) Err() error                    { return nil }
