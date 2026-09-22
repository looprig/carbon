package browser

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/looprig/host"
)

// failStart waits for cleanup within the resolved shutdown policy, not a fixed
// five seconds: a shorter policy returns the retained Server promptly, and a
// longer one gives a slow Host drain the time the policy granted it.
func TestFailStartWaitsWithinShutdownPolicy(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	s := &Server{done: make(chan struct{}), failureDone: make(chan struct{}),
		shutdownPolicy: ShutdownPolicy{QuiesceTimeout: 10 * time.Millisecond, SettlementTimeout: 10 * time.Millisecond,
			CleanupTimeout: 10 * time.Millisecond, ForcedCeiling: 100 * time.Millisecond},
		stopHost: func(context.Context) (host.DrainReport, error) {
			<-release
			return host.DrainReport{}, nil
		},
		closeStorage: func(context.Context) error { return nil },
	}
	cause := errors.New("start failed")
	began := time.Now()
	owner, err := failStart(s, cause)
	if elapsed := time.Since(began); elapsed > 2*time.Second {
		t.Fatalf("failStart waited %s, beyond the %s policy ceiling", elapsed, s.shutdownPolicy.ForcedCeiling)
	}
	if owner != s || !errors.Is(err, cause) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("failStart = (%v, %v), want retained Server and deadline", owner, err)
	}
}

// failStart's budget is the whole ForcedCeiling, not CleanupTimeout: a start
// failure after Factory started runs quiesce, settlement, and cleanup, so a
// healthy drain that outlasts CleanupTimeout but finishes inside the ceiling
// must complete, closing Done and returning no retained Server.
func TestFailStartCompletesADrainLongerThanCleanupTimeoutWithinTheCeiling(t *testing.T) {
	s := &Server{done: make(chan struct{}), failureDone: make(chan struct{}),
		shutdownPolicy: ShutdownPolicy{QuiesceTimeout: 10 * time.Millisecond, SettlementTimeout: 10 * time.Millisecond,
			CleanupTimeout: 20 * time.Millisecond, ForcedCeiling: 5 * time.Second},
		stopHost: func(context.Context) (host.DrainReport, error) {
			time.Sleep(200 * time.Millisecond) // > CleanupTimeout, << ForcedCeiling
			return host.DrainReport{}, nil
		},
		closeStorage: func(context.Context) error { return nil },
	}
	cause := errors.New("start failed")
	owner, err := failStart(s, cause)
	if owner != nil {
		t.Fatalf("failStart retained the Server (err %v); want the drain completed within ForcedCeiling", err)
	}
	if !errors.Is(err, cause) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("failStart error = %v, want only the start cause", err)
	}
	select {
	case <-s.Done():
	default:
		t.Fatal("Done not closed after a completed failStart drain")
	}
}
