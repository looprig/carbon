package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/looprig/carbon/browser"
	carbon "github.com/looprig/carbon/internal/app"
	"io"
	"os"
	"time"
)

var ErrServeFactoryVerifierRequired = browser.ErrVerifierRequired

// browserStartConfig holds the choices an embedding application must make.
// The stock binary supplies no verifier and is refused before opening storage.
type browserStartConfig = browser.Config

// runBrowserLifecycle owns successful composition stages in start order.
// Host v0.6 CloseUnstarted releases a failed pre-publication composition;
// the borrowed storage provider is closed only after Host has released it.
func runBrowserLifecycle(ctx context.Context, appCfg carbon.Config, cfg browserStartConfig, out, errOut io.Writer) int {
	cfg.Runtime = appCfg
	policy, err := cfg.EffectiveShutdownPolicy()
	if err != nil {
		fmt.Fprintln(errOut, "serve:", err)
		return exitFailed
	}
	server, err := browser.Start(ctx, cfg)
	if err != nil {
		fmt.Fprintln(errOut, "serve:", err)
		if server != nil {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), policy.ForcedCeiling)
			defer cancel()
			if cleanupErr := server.Stop(cleanupCtx); cleanupErr != nil {
				fmt.Fprintln(errOut, "serve: cleanup:", cleanupErr)
			}
		}
		return exitFailed
	}
	fmt.Fprintf(out, "carbon serve listening on http://%s\n", server.Addr())
	waitErr := server.Wait(ctx)
	if waitErr != nil && !errors.Is(waitErr, ctx.Err()) {
		fmt.Fprintln(errOut, "serve: listener:", waitErr)
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), policy.ForcedCeiling)
	defer cancel()
	if err := server.Stop(waitCtx); err != nil {
		fmt.Fprintln(errOut, "serve: cleanup:", err)
		return exitFailed
	}
	if waitErr != nil && !errors.Is(waitErr, ctx.Err()) {
		return exitFailed
	}
	return exitOK
}

// runWithTerminationSignals gives the first termination signal to the owned
// lifecycle. A second signal or the ceiling invokes force while cleanup still
// owns its resources; it never closes a provider out from under Host.
func runWithTerminationSignals(parent context.Context, signals <-chan os.Signal, ceilingDuration time.Duration, after func(time.Duration) <-chan time.Time, force func(), run func(context.Context) int) int {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-signals:
			cancel()
		case <-done:
			return
		}
		ceiling := after(ceilingDuration)
		select {
		case <-signals:
			force()
		case <-ceiling:
			force()
		case <-done:
		}
	}()
	return run(ctx)
}
