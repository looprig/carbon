package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	carbon "github.com/looprig/carbon/internal/app"
)

func TestBrowserServeBranchRequiresInjectedVerifierBeforeLegacyOpen(t *testing.T) {
	var out, errOut bytes.Buffer
	legacyOpened := false
	legacy := func(context.Context, carbon.Config, string) (serveHostAPI, error) {
		legacyOpened = true
		return nil, nil
	}
	code := runServeCommand(context.Background(), cliFlags{serve: true, serveAddr: "127.0.0.1:0"},
		carbon.Config{}, t.TempDir(), legacy, &out, &errOut, browserComposition{})
	if code != exitFailed || legacyOpened || !strings.Contains(errOut.String(), ErrServeFactoryVerifierRequired.Error()) {
		t.Fatalf("browser branch = code %d legacy opened %t stderr %q", code, legacyOpened, errOut.String())
	}
}
