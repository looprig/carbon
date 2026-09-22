package main

import (
	"context"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// Only browser serve owns a staged shutdown that a second signal may cut
// short. The TUI and headless paths restore the terminal in deferred teardown,
// so a second signal must not force-exit around it: the first cancels, later
// ones are absorbed, and the run returns its own code.
func TestSecondSignalForcesOnlyTheServePath(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      []string
		wantForce bool
	}{
		{"tui", nil, false},
		{"headless-list", []string{"--list"}, false},
		{"serve", []string{"serve"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			signals := make(chan os.Signal, 2)
			var forced atomic.Bool
			forcedCh := make(chan struct{})
			code := runProcess(context.Background(), tc.args, signals, time.Hour, time.After,
				func() {
					if forced.CompareAndSwap(false, true) {
						close(forcedCh)
					}
				},
				func(ctx context.Context, _ []string) int {
					signals <- os.Interrupt
					<-ctx.Done()
					// Teardown in progress: a second signal arrives.
					signals <- syscall.SIGTERM
					select {
					case <-forcedCh:
					case <-time.After(200 * time.Millisecond):
					}
					return 7
				})
			if code != 7 {
				t.Fatalf("exit = %d, want the run's own code", code)
			}
			if forced.Load() != tc.wantForce {
				t.Fatalf("forced = %t, want %t", forced.Load(), tc.wantForce)
			}
		})
	}
}
