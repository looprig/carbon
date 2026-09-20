package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/looprig/carbon/browser"
	carbon "github.com/looprig/carbon/internal/app"
	"io"
	"time"
)

var ErrServeFactoryVerifierRequired = browser.ErrVerifierRequired

// browserStartConfig holds the choices an embedding application must make.
// The stock binary supplies no verifier and is refused before opening storage.
type browserStartConfig = browser.Config

// runBrowserLifecycle owns the successful composition stages in start order.
// Failed Host construction/start cleanup waits for Host v0.6's unstarted-close
// contract; until then those failures return without closing the borrowed
// storage provider, which the process must discard on exit.
func runBrowserLifecycle(ctx context.Context, appCfg carbon.Config, cfg browserStartConfig, out, errOut io.Writer) int {
	cfg.Runtime = appCfg
	server, err := browser.Start(ctx, cfg)
	if err != nil {
		fmt.Fprintln(errOut, "serve:", err)
		if server != nil {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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
	waitCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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
