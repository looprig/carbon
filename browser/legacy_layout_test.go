package browser_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/looprig/carbon/browser"
)

// Browser serve refuses the legacy single-tenant layout with a typed error an
// embedder can name, and the message says why and where legacy sessions remain
// readable, before any runtime is built.
func TestStartRefusesLegacyLayoutWithTypedExplainedError(t *testing.T) {
	cfg := browserFixture(t)
	cfg.Storage.Layout = browser.StoreLayoutLegacySingleTenant
	s, err := browser.Start(context.Background(), cfg)
	if s != nil {
		_ = s.Stop(context.Background())
		t.Fatal("legacy layout started a browser server")
	}
	var refused *browser.LegacyLayoutRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("Start with legacy layout = %v, want *browser.LegacyLayoutRefusedError", err)
	}
	for _, want := range []string{"legacy-single-tenant-v1", "tenant-v1", "Factory", "TUI", "headless"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal %q does not mention %q", err, want)
		}
	}
}
