package browser

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/looprig/host"
)

// A store call wedged inside an admitted request keeps Factory's admission
// join from completing. The rejoin must end at the policy's forced ceiling so
// the owned cleanup attempt terminates and a later Stop can retry, retaining
// Host and storage because admission never fenced.
func TestWedgedAdmissionJoinIsBoundedByForcedCeilingAndRetryable(t *testing.T) {
	var order []string
	release := make(chan struct{})
	calls := 0
	s := &Server{done: make(chan struct{}),
		shutdownPolicy: ShutdownPolicy{QuiesceTimeout: 20 * time.Millisecond, SettlementTimeout: time.Second,
			CleanupTimeout: time.Second, ForcedCeiling: 200 * time.Millisecond},
		quiesceFactory: func(ctx context.Context) error {
			calls++
			select {
			case <-release:
				order = append(order, "quiesce-joined")
				return nil
			case <-ctx.Done():
				order = append(order, "quiesce-wait")
				return ctx.Err()
			}
		},
		stopFactory: func(context.Context) error { order = append(order, "factory"); return nil },
		stopHost: func(context.Context) (host.DrainReport, error) {
			order = append(order, "host")
			return host.DrainReport{}, nil
		},
		closeStorage: func(context.Context) error { order = append(order, "storage"); return nil },
	}
	stopped := make(chan error, 1)
	go func() { stopped <- s.Stop(context.Background()) }()
	var err error
	select {
	case err = <-stopped:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("owned cleanup never ended while the admission join was wedged")
	}
	var incomplete *QuiesceIncompleteError
	if !errors.As(err, &incomplete) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop = %v, want QuiesceIncompleteError", err)
	}
	if got := strings.Join(order, ","); got != "quiesce-wait,quiesce-wait" {
		t.Fatalf("cleanup past a wedged admission join = %s", got)
	}
	select {
	case <-s.Done():
		t.Fatal("Done closed while Host and storage are retained")
	default:
	}
	close(release)
	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("retry Stop = %v", err)
	}
	if got := strings.Join(order, ","); got != "quiesce-wait,quiesce-wait,quiesce-joined,host,factory,storage" {
		t.Fatalf("retry cleanup = %s", got)
	}
}
