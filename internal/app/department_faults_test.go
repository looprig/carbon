package app

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/harness/pkg/runtimecommand"
	"github.com/looprig/harness/pkg/session"
	"github.com/looprig/host/department"
)

// ---- host v0.8.0/v0.8.1: department.PersistenceFaults is forwarded ----------

// forwardedFaults reports whether department's wrapper around a Carbon runtime
// actually carries PersistenceFaults. An assertion on the wrapper cannot say:
// the wrapper declares the methods for every runtime, which is exactly why a
// product adapter that dropped the capability compiled and ran. Host asks the
// wrapper this question, so the test asks it too.
func forwardedFaults(t *testing.T, runtime any) department.PersistenceFaults {
	t.Helper()
	faults, ok := runtime.(department.PersistenceFaults)
	if !ok {
		t.Fatal("the adapted runtime does not declare department.PersistenceFaults")
	}
	reporter, ok := runtime.(interface{ PersistenceFaultsAvailable() bool })
	if !ok {
		t.Fatal("department's wrapper no longer reports PersistenceFaultsAvailable; re-derive how Host discovers the capability")
	}
	if !reporter.PersistenceFaultsAvailable() {
		t.Fatal("department's wrapper did not receive PersistenceFaults from the Carbon runtime: a storage outage would leave the session resident and every command behind it pending (host v0.8.1)")
	}
	return faults
}

// TestCarbonRuntimeForwardsPersistenceFaultsFromARealSession launches a REAL
// harness session through Carbon's department target and proves Host would
// supervise it: the wrapper carries the capability, the fault channel is live
// (non-nil, open) and no fault is latched. Then it abandons the session through
// the wrapper and proves the call reached harness: the session stops answering
// and its journal lease is handed back, so a successor can restore it.
func TestCarbonRuntimeForwardsPersistenceFaultsFromARealSession(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	fixture := newRealRigFixture(t)
	target := mustCarbonTarget(t, fixture)
	id := mustUUIDForTest(t)
	runtime, err := target.Create(ctx, department.CreateRequest{
		TenantID: "tenant-a", SessionID: "session-a", AgentID: CarbonAgentID,
		Placement: sessionwire.HostPlacementDedicated, RigSessionID: id,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	launched := fixture.lastLaunched(t)
	if _, ok := launched.(session.PersistenceFaultReporter); !ok {
		t.Fatal("the launched harness session offers no PersistenceFaultReporter; the harness pin is below v0.38.0")
	}
	if _, ok := launched.(session.ResidencyAbandoner); !ok {
		t.Fatal("the launched harness session offers no ResidencyAbandoner; the harness pin is below v0.38.0")
	}

	faults := forwardedFaults(t, runtime)
	faulted := faults.PersistenceFaulted()
	if faulted == nil {
		t.Fatal("PersistenceFaulted is nil for a real session: Host would never supervise it")
	}
	if faults.PersistenceFaulted() != faulted {
		t.Fatal("PersistenceFaulted returned a different channel on a second call; department requires the same one")
	}
	select {
	case <-faulted:
		t.Fatal("a freshly launched session reports a latched persistence fault")
	default:
	}
	if err := faults.PersistenceFault(); err != nil {
		t.Fatalf("PersistenceFault on a healthy session = %v", err)
	}

	if err := faults.AbandonResidency(ctx); err != nil {
		t.Fatalf("AbandonResidency: %v", err)
	}
	if _, held := runtime.LeaseEpoch(); held {
		t.Fatal("the journal lease is still held after AbandonResidency; the abandon did not reach harness")
	}
	// Crash-equivalent: no SessionStopped, so the session is restorable by a
	// successor under a strictly later grant.
	restored, err := target.Restore(ctx, department.RestoreRequest{
		TenantID: "tenant-a", SessionID: "session-a", AgentID: CarbonAgentID,
		Placement: sessionwire.HostPlacementDedicated, CompatibilityID: CarbonCompatibilityID(fixture.cfg), RigSessionID: id,
	})
	if err != nil {
		t.Fatalf("a successor could not restore an abandoned session: %v", err)
	}
	t.Cleanup(func() { _ = restored.ReleaseResidency(context.Background()) })
	if _, held := restored.LeaseEpoch(); !held {
		t.Fatal("the restored successor holds no journal lease")
	}
}

// TestCarbonRuntimeForwardsTheFaultSignalAndTheAbandon pins what is forwarded,
// against a controller that records: the SAME channel, the latched fault, and
// the abandon call with its context.
func TestCarbonRuntimeForwardsTheFaultSignalAndTheAbandon(t *testing.T) {
	t.Parallel()
	controller := newFaultingController()
	runtime := &carbonRuntime{controller: controller}

	if runtime.PersistenceFaulted() != controller.faulted {
		t.Fatal("PersistenceFaulted did not forward the session's own channel")
	}
	cause := errors.New("journal append failed")
	controller.latch(cause)
	select {
	case <-runtime.PersistenceFaulted():
	default:
		t.Fatal("the forwarded fault channel did not close when the session latched")
	}
	if got := runtime.PersistenceFault(); !errors.Is(got, cause) {
		t.Fatalf("PersistenceFault = %v, want the latched cause", got)
	}
	if err := runtime.AbandonResidency(context.Background()); err != nil {
		t.Fatalf("AbandonResidency: %v", err)
	}
	if controller.abandons.Load() != 1 {
		t.Fatalf("AbandonResidency reached the session %d times, want 1", controller.abandons.Load())
	}
}

// TestARuntimeWithoutPersistenceFaultsIsUnsupervisedAndRefusesToAbandon covers
// a session that offers the capability only in part, or not at all. It is never
// supervised (a nil channel never fires) and an abandon is REFUSED with an error
// that reaches department's sentinel, rather than silently succeeding: a
// runtime that claimed to have abandoned while still holding its lease would
// let a successor race it.
func TestARuntimeWithoutPersistenceFaultsIsUnsupervisedAndRefusesToAbandon(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		controller session.SessionController
	}{
		{"neither half", &closerlessController{}},
		{"reporter without abandoner", &reporterOnlyController{faulted: make(chan struct{})}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime := &carbonRuntime{controller: tc.controller}
			if runtime.PersistenceFaulted() != nil {
				t.Fatal("PersistenceFaulted answered a channel for a session that cannot be abandoned")
			}
			if err := runtime.PersistenceFault(); err != nil {
				t.Fatalf("PersistenceFault = %v, want nil", err)
			}
			err := runtime.AbandonResidency(context.Background())
			if !errors.Is(err, ErrCarbonRuntimeCannotAbandon) || !errors.Is(err, department.ErrNoPersistenceFaults) {
				t.Fatalf("AbandonResidency = %v, want ErrCarbonRuntimeCannotAbandon wrapping department.ErrNoPersistenceFaults", err)
			}
		})
	}
}

// faultingController is a session stand-in whose persistence fault a test
// latches by hand.
type faultingController struct {
	session.SessionController
	faulted  chan struct{}
	once     sync.Once
	mu       sync.Mutex
	cause    error
	abandons atomic.Int32
}

func newFaultingController() *faultingController {
	return &faultingController{faulted: make(chan struct{})}
}

func (c *faultingController) latch(cause error) {
	c.once.Do(func() {
		c.mu.Lock()
		c.cause = cause
		c.mu.Unlock()
		close(c.faulted)
	})
}

func (c *faultingController) PersistenceFaulted() <-chan struct{} { return c.faulted }
func (c *faultingController) PersistenceFault() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cause
}
func (c *faultingController) AbandonResidency(context.Context) error {
	c.abandons.Add(1)
	return nil
}

// reporterOnlyController offers the fault signal but no abandon.
type reporterOnlyController struct {
	session.SessionController
	faulted chan struct{}
}

func (c *reporterOnlyController) PersistenceFaulted() <-chan struct{} { return c.faulted }
func (c *reporterOnlyController) PersistenceFault() error             { return nil }

// faultInjectingController wraps a REAL launched harness session and replaces
// only its fault SIGNAL with one a test controls; every other capability,
// AbandonResidency included, is the real session's. It lets a test drive Host's
// fault supervision without a real storage outage while proving Host's response
// reaches harness.
type faultInjectingController struct {
	session.SessionController
	injected chan struct{}
	once     sync.Once
	abandons atomic.Int32
}

func newFaultInjectingController(real session.SessionController) *faultInjectingController {
	return &faultInjectingController{SessionController: real, injected: make(chan struct{})}
}

func (c *faultInjectingController) inject() { c.once.Do(func() { close(c.injected) }) }

func (c *faultInjectingController) PersistenceFaulted() <-chan struct{} { return c.injected }
func (c *faultInjectingController) PersistenceFault() error {
	select {
	case <-c.injected:
		return errInjectedPersistenceFault
	default:
		return nil
	}
}
func (c *faultInjectingController) AbandonResidency(ctx context.Context) error {
	c.abandons.Add(1)
	return c.SessionController.(session.ResidencyAbandoner).AbandonResidency(ctx)
}
func (c *faultInjectingController) WaitIdle(ctx context.Context) error {
	return c.SessionController.(session.IdleWaiter).WaitIdle(ctx)
}
func (c *faultInjectingController) Done() <-chan struct{} {
	return c.SessionController.(session.Liveness).Done()
}
func (c *faultInjectingController) ReleaseResidency(ctx context.Context) error {
	return c.SessionController.(session.Releaser).ReleaseResidency(ctx)
}
func (c *faultInjectingController) LeaseEpoch() (uint64, bool) {
	return c.SessionController.(session.LeaseEpochReporter).LeaseEpoch()
}
func (c *faultInjectingController) CommittedPublicEvents() (session.CommittedPublicEventSource, bool) {
	return c.SessionController.(session.CommittedPublicEventProvider).CommittedPublicEvents()
}
func (c *faultInjectingController) ApplyRuntimeCommand(ctx context.Context, admitted runtimecommand.Admitted) (runtimecommand.Disposition, error) {
	return c.SessionController.(runtimecommand.Applier).ApplyRuntimeCommand(ctx, admitted)
}
func (c *faultInjectingController) CloseAttempt(ctx context.Context, closure runtimecommand.Closure) (runtimecommand.ClosureResult, error) {
	return c.SessionController.(runtimecommand.AttemptCloser).CloseAttempt(ctx, closure)
}

var errInjectedPersistenceFault = errors.New("carbon test: injected persistence fault")
