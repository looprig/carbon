package app

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"

	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/harness/pkg/session"
	"github.com/looprig/host/department"
	"github.com/looprig/host/harnessruntime"
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

// TestCarbonRuntimeForwardsTheFaultSignalAndTheAbandon pins what is forwarded
// through Carbon's target: the SAME channel the session answers, the latched
// fault, and the abandon reaching the session.
func TestCarbonRuntimeForwardsTheFaultSignalAndTheAbandon(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newRealRigFixture(t)
	var probe *probeController
	target := mustCarbonTargetOver(t, fixture.wrappedLauncher(func(real session.SessionController) session.SessionController {
		probe = newProbe(real)
		probe.faulted = make(chan struct{})
		return probe
	}), CarbonCompatibilityID(fixture.cfg))
	runtime, err := target.Create(ctx, department.CreateRequest{
		TenantID: "tenant-a", SessionID: "session-a", AgentID: CarbonAgentID,
		Placement: sessionwire.HostPlacementDedicated, RigSessionID: mustUUIDForTest(t),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	faults := forwardedFaults(t, runtime)
	if faults.PersistenceFaulted() != probe.PersistenceFaulted() {
		t.Fatal("PersistenceFaulted did not forward the session's own channel")
	}
	probe.inject()
	select {
	case <-faults.PersistenceFaulted():
	default:
		t.Fatal("the forwarded fault channel did not close when the session latched")
	}
	if got := faults.PersistenceFault(); !errors.Is(got, errInjectedPersistenceFault) {
		t.Fatalf("PersistenceFault = %v, want the latched cause", got)
	}
	if err := faults.AbandonResidency(ctx); err != nil {
		t.Fatalf("AbandonResidency: %v", err)
	}
	if probe.abandons.Load() != 1 {
		t.Fatalf("AbandonResidency reached the session %d times, want 1", probe.abandons.Load())
	}
}

// TestCarbonTargetRefusesASessionMissingEitherPersistenceFaultsHalf is SF3,
// now enforced by harnessruntime at bind: a session lacking either half of
// harness's durable-health capability is refused at launch, where the absence
// can be seen, rather than silently unsupervised — and the refused session is
// given back without being ended (a nonterminal release when it has one,
// otherwise a crash-equivalent abandon; never Shutdown, which would make the
// conversation terminal).
func TestCarbonTargetRefusesASessionMissingEitherPersistenceFaultsHalf(t *testing.T) {
	t.Parallel()
	abandonOnly, releaseOnly := &abandonerOnlyController{}, &releaserOnlyController{}
	for _, tc := range []struct {
		name       string
		controller session.SessionController
		missing    []string
		disposed   func() bool
	}{
		{name: "neither half", controller: &closerlessController{},
			missing: []string{"session.PersistenceFaultReporter", "session.ResidencyAbandoner"}},
		{name: "reporter without abandoner", controller: &reporterOnlyController{faulted: make(chan struct{})},
			missing: []string{"session.ResidencyAbandoner"}},
		{name: "abandoner without reporter", controller: abandonOnly,
			missing:  []string{"session.PersistenceFaultReporter"},
			disposed: func() bool { return abandonOnly.abandons.Load() == 1 }},
		{name: "releasable but neither half", controller: releaseOnly,
			missing:  []string{"session.PersistenceFaultReporter", "session.ResidencyAbandoner"},
			disposed: func() bool { return releaseOnly.releases.Load() == 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := mustCarbonTargetOver(t, launcherFunc(func(context.Context, LaunchScope) (session.SessionController, error) {
				return tc.controller, nil
			}), department.CompatibilityID("test-build"))
			got, err := target.Create(context.Background(), department.CreateRequest{
				TenantID: "tenant-a", SessionID: "session-a", AgentID: CarbonAgentID,
				Placement: sessionwire.HostPlacementDedicated, RigSessionID: mustUUIDForTest(t),
			})
			if got != nil {
				t.Fatal("a session lacking a PersistenceFaults half was launched")
			}
			var incapable *harnessruntime.IncapableSessionError
			if !errors.As(err, &incapable) {
				t.Fatalf("launch = %v, want *harnessruntime.IncapableSessionError", err)
			}
			for _, want := range tc.missing {
				if !slices.Contains(incapable.Missing, want) {
					t.Errorf("refusal names %v, want it to include %q", incapable.Missing, want)
				}
			}
			if tc.disposed != nil && !tc.disposed() {
				t.Fatal("the refused session was not released")
			}
		})
	}
}

// closerlessController is a session.SessionController stand-in that offers no
// segregated capability at all. It is a DISTINCT TYPE rather than a flag on a
// fake, because Go method sets are not conditional.
type closerlessController struct{ session.SessionController }

// reporterOnlyController offers the fault signal but no abandon.
type reporterOnlyController struct {
	session.SessionController
	faulted chan struct{}
}

func (c *reporterOnlyController) PersistenceFaulted() <-chan struct{} { return c.faulted }
func (c *reporterOnlyController) PersistenceFault() error             { return nil }

// abandonerOnlyController offers the abandon but no fault signal.
type abandonerOnlyController struct {
	session.SessionController
	abandons atomic.Int32
}

func (c *abandonerOnlyController) AbandonResidency(context.Context) error {
	c.abandons.Add(1)
	return nil
}

// releaserOnlyController offers only a graceful release.
type releaserOnlyController struct {
	session.SessionController
	releases atomic.Int32
}

func (c *releaserOnlyController) ReleaseResidency(context.Context) error {
	c.releases.Add(1)
	return nil
}
