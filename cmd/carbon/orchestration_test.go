package main

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

func TestTerminationSignalChild(t *testing.T) {
	mode := os.Getenv("LOOPRIG_TERMINATION_CHILD")
	if mode == "" {
		return
	}
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	code := runWithTerminationSignals(context.Background(), signals, time.Minute, time.After,
		func() { os.Exit(23) }, func(ctx context.Context) int {
			_, _ = os.Stdout.WriteString("READY\n")
			<-ctx.Done()
			_, _ = os.Stdout.WriteString("CANCELLED\n")
			if mode == "second" {
				select {}
			}
			return exitOK
		})
	os.Exit(code)
}

func TestActualSIGINTAndSIGTERMSuperviseProcess(t *testing.T) {
	for _, signalValue := range []os.Signal{os.Interrupt, syscall.SIGTERM} {
		for _, mode := range []string{"first", "second"} {
			t.Run(signalValue.String()+"/"+mode, func(t *testing.T) {
				// No context timeout wraps Start(): re-exec and start-up of a
				// -race test binary is unpredictable under host load and has
				// nothing to do with what this test measures. A generous,
				// separate watchdog bounds only the wait for READY, and the
				// signal-round-trip budget below starts only once the child
				// has actually reported ready.
				cmd := exec.Command(os.Args[0], "-test.run=^TestTerminationSignalChild$")
				cmd.Env = append(os.Environ(), "LOOPRIG_TERMINATION_CHILD="+mode)
				stdout, err := cmd.StdoutPipe()
				if err != nil {
					t.Fatal(err)
				}
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
				lines := bufio.NewScanner(stdout)
				readyTimer := time.AfterFunc(30*time.Second, func() { _ = cmd.Process.Kill() })
				readyOK := lines.Scan() && lines.Text() == "READY"
				readyTimer.Stop()
				if !readyOK {
					t.Fatalf("child readiness: %q %v", lines.Text(), lines.Err())
				}
				// The signal round-trip budget starts here, after READY, so
				// it covers only the actual signal delivery and cancellation
				// this test measures.
				budgetTimer := time.AfterFunc(5*time.Second, func() { _ = cmd.Process.Kill() })
				defer budgetTimer.Stop()
				if err := cmd.Process.Signal(signalValue); err != nil {
					t.Fatal(err)
				}
				if !lines.Scan() || lines.Text() != "CANCELLED" {
					t.Fatalf("child cancellation: %q %v", lines.Text(), lines.Err())
				}
				if mode == "second" {
					if err := cmd.Process.Signal(signalValue); err != nil {
						t.Fatal(err)
					}
				}
				err = cmd.Wait()
				budgetTimer.Stop()
				want := 0
				if mode == "second" {
					want = 23
				}
				if got := cmd.ProcessState.ExitCode(); got != want {
					t.Fatalf("child exit = %d, want %d (Wait: %v)", got, want, err)
				}
			})
		}
	}
}

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
				done <- runWithTerminationSignals(context.Background(), signals, time.Minute, func(time.Duration) <-chan time.Time {
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
		done <- runWithTerminationSignals(context.Background(), signals, 3*time.Second, func(d time.Duration) <-chan time.Time {
			if d != 3*time.Second {
				t.Errorf("ceiling = %s, want 3s", d)
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
