package main

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestServeSignalSupervisorCancelsOnFirstSignalAndForcesOnSecond(t *testing.T) {
	for _, first := range []os.Signal{os.Interrupt, syscall.SIGTERM} {
		t.Run(first.String(), func(t *testing.T) {
			signals := make(chan os.Signal, 2)
			started := make(chan struct{})
			cancelled := make(chan struct{})
			finish := make(chan struct{})
			forced := make(chan struct{}, 1)
			armed := make(chan struct{})
			done := make(chan int, 1)
			go func() {
				done <- runWithTerminationSignals(context.Background(), signals, func(time.Duration) <-chan time.Time {
					close(armed)
					return make(chan time.Time)
				}, func() { forced <- struct{}{} }, func(ctx context.Context) int {
					close(started)
					<-ctx.Done()
					close(cancelled)
					<-finish
					return exitOK
				})
			}()
			<-started
			signals <- first
			<-cancelled
			<-armed
			select {
			case <-forced:
				t.Fatal("first signal forced exit")
			default:
			}
			signals <- syscall.SIGTERM
			select {
			case <-forced:
			case <-time.After(time.Second):
				t.Fatal("second signal did not force exit")
			}
			close(finish)
			if code := <-done; code != exitOK {
				t.Fatalf("run exit = %d", code)
			}
		})
	}
}

func TestServeSignalSupervisorForcesAtInjectedCeiling(t *testing.T) {
	signals := make(chan os.Signal, 1)
	ceiling := make(chan time.Time, 1)
	forced := make(chan struct{}, 1)
	cancelled := make(chan struct{})
	finish := make(chan struct{})
	done := make(chan int, 1)
	go func() {
		done <- runWithTerminationSignals(context.Background(), signals, func(d time.Duration) <-chan time.Time {
			if d <= 0 {
				t.Errorf("ceiling = %s", d)
			}
			return ceiling
		}, func() { forced <- struct{}{} }, func(ctx context.Context) int {
			<-ctx.Done()
			close(cancelled)
			<-finish
			return exitOK
		})
	}()
	signals <- os.Interrupt
	<-cancelled
	ceiling <- time.Now()
	select {
	case <-forced:
	case <-time.After(time.Second):
		t.Fatal("ceiling did not force exit")
	}
	close(finish)
	<-done
}
