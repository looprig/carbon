package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/looprig/host"
)

func TestBrowserDisposalStopsFactoryHostThenStorage(t *testing.T) {
	var order []string
	disposal := browserDisposal{
		stopFactory: func(context.Context) error { order = append(order, "factory"); return nil },
		stopHost: func(context.Context) (host.DrainReport, error) {
			order = append(order, "host")
			return host.DrainReport{}, nil
		},
		closeStorage: func(context.Context) error { order = append(order, "storage"); return nil },
	}
	if err := disposal.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(order, ","); got != "factory,host,storage" {
		t.Fatalf("disposal order = %q", got)
	}
}

func TestBrowserDisposalKeepsProviderOpenAfterHostStopFailure(t *testing.T) {
	var order []string
	failed := true
	disposal := browserDisposal{
		stopFactory: func(context.Context) error { order = append(order, "factory"); return nil },
		stopHost: func(context.Context) (host.DrainReport, error) {
			order = append(order, "host")
			if failed {
				return host.DrainReport{}, context.DeadlineExceeded
			}
			return host.DrainReport{}, nil
		},
		closeStorage: func(context.Context) error { order = append(order, "storage"); return nil },
	}
	if err := disposal.Close(context.Background()); err == nil || strings.Contains(strings.Join(order, ","), "storage") {
		t.Fatalf("failed Host stop closed storage: order %v, error %v", order, err)
	}
	failed = false
	if err := disposal.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(order, ","); got != "factory,host,host,storage" {
		t.Fatalf("retry order = %q", got)
	}
}

func TestBrowserShutdownCallerTimeoutDoesNotAdvanceCleanup(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	stages := make(chan string, 3)
	disposal := browserDisposal{
		stopFactory: func(context.Context) error {
			close(entered)
			<-release
			stages <- "factory"
			return nil
		},
		stopHost:     func(context.Context) (host.DrainReport, error) { stages <- "host"; return host.DrainReport{}, nil },
		closeStorage: func(context.Context) error { stages <- "storage"; return nil },
	}
	shutdown := startBrowserShutdown(&disposal)
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := shutdown.Wait(ctx); err != context.Canceled {
		t.Fatalf("cancelled caller wait = %v", err)
	}
	select {
	case stage := <-stages:
		t.Fatalf("cleanup advanced to %s while Factory still stopping", stage)
	default:
	}
	close(release)
	if err := shutdown.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"factory", "host", "storage"} {
		if got := <-stages; got != want {
			t.Fatalf("cleanup stage = %s, want %s", got, want)
		}
	}
}

func TestStockServeRefusesWithoutConfiguredVerifier(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"serve", "--data-dir", t.TempDir()}, &out, &errOut)
	if code != exitFailed || !strings.Contains(errOut.String(), ErrServeFactoryVerifierRequired.Error()) {
		t.Fatalf("stock serve = code %d stderr %q, want verifier-required refusal", code, errOut.String())
	}
}
