package retry

import (
	"errors"
	"time"

	"github.com/looprig/inference/failure"
)

// retryAfterCap bounds a server-advertised Retry-After so a pathological
// header cannot park a turn for an hour; context cancellation remains the
// user's escape hatch below this bound.
const retryAfterCap = 5 * time.Minute

// baseDelay is the jitter-free schedule slot after the given 1-based failed
// attempt: StableDelay for the stable leg, then doubling from 2*StableDelay,
// capped at MaxDelay.
func baseDelay(p Policy, attempt int) time.Duration {
	if attempt <= p.StableRetries {
		return p.StableDelay
	}
	d := p.StableDelay
	for i := 0; i < attempt-p.StableRetries; i++ {
		d *= 2
		if d >= p.MaxDelay {
			return p.MaxDelay
		}
	}
	return d
}

// jittered spreads d by ±10% using r in [0,1).
func jittered(d time.Duration, r float64) time.Duration {
	return time.Duration(float64(d) * (0.9 + 0.2*r))
}

// nextDelay combines the jittered schedule slot with any server-advertised
// Retry-After on err: the larger wins (a server saying "60s" must not be
// hammered at 2s; a server saying "1s" doesn't shrink our own pacing).
func nextDelay(p Policy, attempt int, err error, r float64) time.Duration {
	d := jittered(baseDelay(p, attempt), r)
	var apiErr *failure.APIError
	if errors.As(err, &apiErr) && apiErr.RetryAfter > 0 {
		ra := min(apiErr.RetryAfter, retryAfterCap)
		if ra > d {
			return ra
		}
	}
	return d
}
