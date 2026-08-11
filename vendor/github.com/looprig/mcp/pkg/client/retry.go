// This file defines the bounded retry policy the client applies to the two
// background activities that may legitimately fail and be worth trying again: a
// catalog refresh (see refresh.go) and a reconnect (see reconnect.go).
//
// Every dimension is bounded, and the bounds are conjunctive: a retry loop stops
// at whichever of attempts, delay or total elapsed time it reaches first. There
// is deliberately no "retry forever" setting. A background loop that never gives
// up against a server that is never coming back is not resilience — it is a
// goroutine, a process, and a reconnect storm that outlive the reason for them,
// and it hides the failure from the operator instead of reporting it.

package client

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"time"
)

// Default retry bounds, applied when the corresponding RetryPolicy field is
// zero.
const (
	// DefaultRetryAttempts is how many times an operation is tried in total
	// (the first try plus its retries).
	DefaultRetryAttempts = 5
	// DefaultRetryBaseDelay is the delay before the second attempt; each
	// subsequent delay doubles it.
	DefaultRetryBaseDelay = 200 * time.Millisecond
	// DefaultRetryMaxDelay caps one delay however far the backoff has doubled.
	DefaultRetryMaxDelay = 30 * time.Second
	// DefaultRetryMaxTotal caps the wall-clock time one retry loop may span,
	// including the attempts themselves.
	DefaultRetryMaxTotal = 2 * time.Minute
)

// MaxRetryAttempts caps RetryPolicy.Attempts. The bound exists so that a
// configured policy cannot express an effectively unbounded loop by way of a
// very large count; MaxTotal would still stop it, but a policy whose attempts
// are absurd is a configuration error worth reporting at Validate rather than
// silently ignoring at runtime.
const MaxRetryAttempts = 64

// RetryPolicy bounds a background retry loop. The zero value of any field
// selects its default; negative values fail validation.
type RetryPolicy struct {
	// Attempts is the total number of tries, including the first. 1 means try
	// once and do not retry. Zero means DefaultRetryAttempts.
	Attempts int
	// BaseDelay is the delay before the second attempt. Zero means
	// DefaultRetryBaseDelay.
	BaseDelay time.Duration
	// MaxDelay caps one delay. Zero means DefaultRetryMaxDelay.
	MaxDelay time.Duration
	// MaxTotal caps the loop's total wall-clock span. Zero means
	// DefaultRetryMaxTotal.
	MaxTotal time.Duration
}

// validate reports the first invalid field, naming it. Zero is valid: it selects
// the default.
func (p RetryPolicy) validate(field string) error {
	if p.Attempts < 0 {
		return fmt.Errorf("%s.Attempts: negative count %d", field, p.Attempts)
	}
	if p.Attempts > MaxRetryAttempts {
		return fmt.Errorf("%s.Attempts: %d exceeds the maximum of %d", field, p.Attempts, MaxRetryAttempts)
	}
	for _, f := range []struct {
		name  string
		value time.Duration
	}{
		{"BaseDelay", p.BaseDelay},
		{"MaxDelay", p.MaxDelay},
		{"MaxTotal", p.MaxTotal},
	} {
		if f.value < 0 {
			return fmt.Errorf("%s.%s: negative duration %v", field, f.name, f.value)
		}
	}
	return nil
}

// withDefaults returns a copy with every zero field replaced by its default.
func (p RetryPolicy) withDefaults() RetryPolicy {
	if p.Attempts == 0 {
		p.Attempts = DefaultRetryAttempts
	}
	if p.BaseDelay == 0 {
		p.BaseDelay = DefaultRetryBaseDelay
	}
	if p.MaxDelay == 0 {
		p.MaxDelay = DefaultRetryMaxDelay
	}
	if p.MaxTotal == 0 {
		p.MaxTotal = DefaultRetryMaxTotal
	}
	return p
}

// delay returns how long to wait before attempt n (1-based), which is zero for
// the first attempt.
//
// The backoff is exponential from BaseDelay, capped at MaxDelay, with full
// jitter: the returned delay is uniform in [0, cap]. Jitter matters because the
// bindings of one host fail together — a server restarts, a network partitions —
// and an unjittered backoff would have every binding retry in the same instant,
// repeatedly, which is how a recovering server gets knocked back down.
//
// The randomness comes from crypto/rand. Jitter is not security-sensitive and
// math/rand would be sound for it, but crypto/rand costs nothing at this call
// rate (a handful of draws per failure, not per request), and it keeps the
// module free of math/rand entirely — which is worth more than the nanoseconds:
// a later reader looking for "is this draw security-sensitive?" never has to
// answer the question, and gosec's G404 never has to be suppressed here.
//
// A failed draw is not fatal. It falls back to the full, unjittered cap: the
// conservative direction, since the alternative — retrying immediately — is the
// one behavior jitter exists to prevent.
func (p RetryPolicy) delay(n int) time.Duration {
	if n <= 1 {
		return 0
	}
	ceiling := p.BaseDelay
	// Double once per attempt past the second, stopping at MaxDelay so the
	// doubling can never overflow however many attempts a policy allows.
	for i := 2; i < n && ceiling < p.MaxDelay; i++ {
		ceiling *= 2
	}
	if ceiling > p.MaxDelay || ceiling <= 0 {
		ceiling = p.MaxDelay
	}
	return jitter(ceiling)
}

// jitter returns a duration uniform in [0, d].
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(d)))
	if err != nil {
		return d
	}
	return time.Duration(n.Int64())
}

// ReconnectPolicy governs whether and how hard a binding tries to rebuild a
// connection that failed transiently. The zero value reconnects under the
// default bounds, which is the useful default: a binding that gave up on the
// first dropped socket would make every server restart an operator's problem.
type ReconnectPolicy struct {
	// Disabled refuses reconnection entirely. A binding whose connection is
	// lost then stays degraded until its owner closes it.
	//
	// It is a negative flag because the zero value must mean "the default", as
	// it does everywhere else in a Definition, and the default is to reconnect.
	// An application disables this when re-establishing the connection costs
	// something it would rather decide about itself — a subprocess with side
	// effects at startup, a metered endpoint, an auth flow that prompts a human.
	Disabled bool

	// RetryPolicy bounds the attempts. Zero fields select their defaults.
	//
	// It is embedded rather than named because a reconnect policy *is* a retry
	// policy plus the decision to retry at all; there is no second dimension for
	// a field name to distinguish.
	RetryPolicy
}

// validate reports the first invalid field.
func (p ReconnectPolicy) validate(field string) error {
	return p.RetryPolicy.validate(field)
}

// withDefaults returns a copy with every zero bound replaced by its default.
// Disabled is a decision, not a bound, and is left as the application set it.
func (p ReconnectPolicy) withDefaults() ReconnectPolicy {
	p.RetryPolicy = p.RetryPolicy.withDefaults()
	return p
}

// retrySchedule tracks one bounded retry loop's budget.
//
// It is a value with an explicit start time rather than a closure over
// time.Now, so a test can drive the whole budget without sleeping through it.
type retrySchedule struct {
	policy  RetryPolicy
	started time.Time
	attempt int
}

// newRetrySchedule starts a loop's budget at now.
func newRetrySchedule(p RetryPolicy, now time.Time) retrySchedule {
	return retrySchedule{policy: p, started: now}
}

// next reports the delay before the next attempt and whether the budget allows
// it at all. It consumes one attempt from the budget.
//
// The total bound is applied to the moment the attempt would *start*, not to the
// moment the previous one failed: a loop whose next delay would carry it past
// MaxTotal stops now rather than sleeping first and giving up afterwards.
func (s *retrySchedule) next(now time.Time) (time.Duration, bool) {
	if s.attempt >= s.policy.Attempts {
		return 0, false
	}
	s.attempt++
	d := s.policy.delay(s.attempt)
	if now.Add(d).Sub(s.started) > s.policy.MaxTotal {
		return 0, false
	}
	return d, true
}
