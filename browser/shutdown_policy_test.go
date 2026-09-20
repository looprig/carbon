package browser

import (
	"math"
	"testing"
	"time"

	"github.com/looprig/factory"
)

func TestDefaultShutdownPolicyCoversFactoryDeadlineAndHostDrain(t *testing.T) {
	cfg := Config{}
	cfg.Host.Drain.Grace = 30 * time.Second
	cfg.Host.Drain.PublishBound = 5 * time.Second
	cfg.Host.Options.ClaimTTL = 5 * time.Second
	p, err := effectiveShutdownPolicy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	limits := factory.DefaultReconcileLimits()
	if p.SettlementTimeout <= limits.ApplyDeadline {
		t.Fatalf("settlement %s shorter than deadline plus sweep opportunity %s", p.SettlementTimeout, limits.ApplyDeadline)
	}
	if p.ForcedCeiling < p.QuiesceTimeout+p.SettlementTimeout+p.CleanupTimeout {
		t.Fatalf("forced ceiling %s excludes phase budgets: %+v", p.ForcedCeiling, p)
	}
}

func TestShutdownPolicyRejectsShortCeilingAndDurationOverflow(t *testing.T) {
	cfg := Config{}
	cfg.Host.Drain.Grace = time.Second
	cfg.Shutdown = ShutdownPolicy{QuiesceTimeout: time.Second, SettlementTimeout: time.Second, CleanupTimeout: time.Second, ForcedCeiling: time.Second}
	if _, err := effectiveShutdownPolicy(cfg); err == nil {
		t.Fatal("accepted forced ceiling shorter than phases")
	}
	cfg.Shutdown = ShutdownPolicy{}
	cfg.Factory.ReconcileLimits = factory.DefaultReconcileLimits()
	cfg.Factory.ReconcileLimits.ApplyDeadline = time.Duration(math.MaxInt64)
	if _, err := effectiveShutdownPolicy(cfg); err == nil {
		t.Fatal("accepted overflowing settlement budget")
	}
}
