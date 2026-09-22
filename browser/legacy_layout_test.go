package browser_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/looprig/carbon/browser"
	carbon "github.com/looprig/carbon/internal/app"
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

// R1.5 step 1 (as amended by the D2 ruling). A data root the TUI/headless path
// has already used carries SessionStore's legacy marker. Browser serve's
// default tenant-v1 layout must not auto-detect it, adopt it, or present it as
// an empty catalog: Start refuses with a type an embedder can name, writes
// nothing, and the TUI/headless store still opens that root afterwards.
func TestStartRefusesExistingTUIRootWithoutAdoptingIt(t *testing.T) {
	ctx := context.Background()
	cfg := browserFixture(t)
	tui, err := carbon.NewSessionStoreFactory(cfg.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tui.List(ctx); err != nil {
		t.Fatalf("seed TUI store: %v", err)
	}
	if err := tui.Close(); err != nil {
		t.Fatal(err)
	}
	for attempt := range 2 {
		s, err := browser.Start(ctx, cfg)
		if s != nil {
			_ = s.Stop(ctx)
			t.Fatalf("attempt %d: browser serve adopted a TUI store root", attempt)
		}
		var mismatch *browser.StoreLayoutMismatchError
		if !errors.As(err, &mismatch) {
			t.Fatalf("attempt %d: Start over TUI root = %v, want *browser.StoreLayoutMismatchError", attempt, err)
		}
		if mismatch.Layout != browser.StoreLayoutTenantV1 {
			t.Fatalf("attempt %d: mismatch layout = %q, want tenant-v1", attempt, mismatch.Layout)
		}
	}
	for _, created := range []string{"tenant-journals", "session-workspaces"} {
		if _, err := os.Stat(filepath.Join(cfg.Storage.DataDir, created)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("refused Start created %s in the TUI root: %v", created, err)
		}
	}
	reopened, err := carbon.NewSessionStoreFactory(cfg.Storage.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if _, err := reopened.List(ctx); err != nil {
		t.Fatalf("TUI store after refused browser serve: %v", err)
	}
}
