package browser

import (
	"context"
	"testing"

	carbon "github.com/looprig/carbon/internal/app"
	"github.com/looprig/factory"
	"github.com/looprig/sessionstore"
)

// The object route's evidence and objects are the SERVED tenant's runtime
// stores and nobody else's, and the composition carries Carbon's own capture
// ceiling, Factory's default limits and the evidence-scan limiter.
func TestToolResultObjectConfigServesTheServedTenantOnly(t *testing.T) {
	ctx := context.Background()
	launcher, err := carbon.OpenPooledLauncher(ctx, carbon.Config{HomeDir: t.TempDir()}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = launcher.Close(ctx) })
	reader, err := carbon.NewServeSessionReader(&sessionstore.Store{}, launcher, "local", "carbon-local-v1", "v1")
	if err != nil {
		t.Fatal(err)
	}
	cfg := toolResultObjectConfig(reader, FactoryConfig{StorageBindingID: "carbon-local-v1", BindingVersion: "v1"})
	if cfg.Binding.StorageBindingID != "carbon-local-v1" || cfg.Binding.BindingVersion != "v1" {
		t.Fatalf("binding = %+v", cfg.Binding)
	}
	if cfg.CaptureBytes != carbon.ServeToolResultCaptureBytes() || cfg.Limits != factory.DefaultObjectLimits() || cfg.RateLimit.Validate() != nil {
		t.Fatalf("config = %+v", cfg)
	}
	if evidence, ok := cfg.Evidence("other"); ok || evidence != nil {
		t.Fatalf("Evidence(other) = (%v, %v), want a refusal", evidence, ok)
	}
	if objects, ok := cfg.Objects("other"); ok || objects != nil {
		t.Fatalf("Objects(other) = (%v, %v), want a refusal", objects, ok)
	}
	journal, err := launcher.JournalStoreForTenant("local")
	if err != nil {
		t.Fatal(err)
	}
	if evidence, ok := cfg.Evidence("local"); !ok || evidence != journal {
		t.Fatalf("Evidence(local) = (%v, %v), want the served tenant's journal store", evidence, ok)
	}
	if objects, ok := cfg.Objects("local"); !ok || objects == nil {
		t.Fatalf("Objects(local) = (%v, %v), want the served tenant's reader", objects, ok)
	}
	if _, err := toolResultObjectOptions(reader, FactoryConfig{StorageBindingID: "carbon-local-v1", BindingVersion: "v1"}); err != nil {
		t.Fatalf("toolResultObjectOptions: %v", err)
	}
}
