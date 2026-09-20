package browser

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/looprig/factory"
	"github.com/looprig/sessionstore"
)

// ShutdownPolicy bounds Factory quiescence and command observation. CleanupTimeout
// reserves time for Host drain and final cleanup inside ForcedCeiling; it is not
// a timer on owned cleanup. Zero fields derive conservative values from Factory's
// effective limits and this Host's drain options. Budgets are ceilings, not proof
// that a blocked applying command settled.
type ShutdownPolicy struct {
	QuiesceTimeout    time.Duration
	SettlementTimeout time.Duration
	CleanupTimeout    time.Duration
	ForcedCeiling     time.Duration
}

// EffectiveShutdownPolicy computes the bounds an embedding application should
// use for its signal supervisor. Start validates and stores the same result.
func (cfg Config) EffectiveShutdownPolicy() (ShutdownPolicy, error) {
	return effectiveShutdownPolicy(cfg)
}

func effectiveShutdownPolicy(cfg Config) (ShutdownPolicy, error) {
	limits := cfg.Factory.ReconcileLimits
	if limits == (factory.ReconcileLimits{}) {
		limits = factory.DefaultReconcileLimits()
	}
	if err := limits.Validate(); err != nil {
		return ShutdownPolicy{}, err
	}
	clientLinks := cfg.Factory.ClientLinkLimits
	if clientLinks == (factory.ClientLinkLimits{}) {
		clientLinks = factory.DefaultClientLinkLimits()
	}
	if err := clientLinks.Validate(); err != nil {
		return ShutdownPolicy{}, err
	}
	p := cfg.Shutdown
	if p.QuiesceTimeout == 0 {
		admission := clientLinks.CommandTimeout
		if admission < 30*time.Second {
			admission = 30 * time.Second
		}
		var err error
		// Factory first joins durable admissions, then gives ClientLink
		// shutdown a separate CommandTimeout+DemandTimeout window.
		p.QuiesceTimeout, err = sumDurations(admission, clientLinks.CommandTimeout, clientLinks.DemandTimeout, 5*time.Second)
		if err != nil {
			return ShutdownPolicy{}, err
		}
	}
	if p.CleanupTimeout == 0 {
		var err error
		p.CleanupTimeout, err = sumDurations(cfg.Host.Drain.Grace, cfg.Host.Drain.PublishBound, 5*time.Second)
		if err != nil {
			return ShutdownPolicy{}, err
		}
	}
	if p.SettlementTimeout == 0 {
		claim := limits.ClaimTTL
		if cfg.Host.Options.ClaimTTL > claim {
			claim = cfg.Host.Options.ClaimTTL
		}
		sweep, err := multiplyDuration(limits.Interval, 2*sessionstore.DefaultControlShards)
		if err != nil {
			return ShutdownPolicy{}, err
		}
		p.SettlementTimeout, err = sumDurations(limits.ApplyDeadline, claim, sweep)
		if err != nil {
			return ShutdownPolicy{}, err
		}
	}
	if p.QuiesceTimeout <= 0 || p.SettlementTimeout <= 0 || p.CleanupTimeout <= 0 || cfg.Host.Drain.Grace < 0 || cfg.Host.Drain.PublishBound < 0 {
		return ShutdownPolicy{}, errors.New("carbon: shutdown durations must be positive and Host drain bounds nonnegative")
	}
	minimum, err := sumDurations(p.QuiesceTimeout, p.SettlementTimeout, p.CleanupTimeout)
	if err != nil {
		return ShutdownPolicy{}, err
	}
	if p.ForcedCeiling == 0 {
		p.ForcedCeiling, err = sumDurations(minimum, 5*time.Second)
		if err != nil {
			return ShutdownPolicy{}, err
		}
	}
	if p.ForcedCeiling < minimum {
		return ShutdownPolicy{}, fmt.Errorf("carbon: forced shutdown ceiling %s is below phase budget %s", p.ForcedCeiling, minimum)
	}
	return p, nil
}

func multiplyDuration(d time.Duration, n int) (time.Duration, error) {
	if d < 0 || n < 0 || n != 0 && d > time.Duration(math.MaxInt64/int64(n)) {
		return 0, errors.New("carbon: shutdown duration overflow")
	}
	return d * time.Duration(n), nil
}

func sumDurations(values ...time.Duration) (time.Duration, error) {
	var sum time.Duration
	for _, value := range values {
		if value < 0 || sum > time.Duration(math.MaxInt64)-value {
			return 0, errors.New("carbon: shutdown duration overflow")
		}
		sum += value
	}
	return sum, nil
}
