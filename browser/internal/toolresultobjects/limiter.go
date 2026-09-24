package toolresultobjects

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/factory"
	"github.com/looprig/factory/identity"
	"github.com/looprig/sessionstore"
)

// limiter.go bounds the work Factory's object route can be made to do (I2.2
// tests gate A4).
//
// The evidence policy's first read of a reference scans the session's runtime
// journal backwards, up to harness's 65,536-record budget. Hits are cached;
// misses are not, so every forged or unknown reference from a principal
// authorized for the session costs a full scan. RateLimitedPolicy charges one
// token per authorization -- a grant, a denial or a fault alike, since each may
// have scanned -- from TWO token buckets that must both admit: the principal's
// (bounding what one caller can do across sessions) and the session's
// (bounding what every caller together can do to one journal).
//
// A throttled read is answered with ErrRateLimited, which is deliberately NOT
// a denial: it does not wrap identity.ErrUnauthorized, so Factory answers it as
// a policy fault (500) rather than the absent-object 404. Throttling says
// nothing about whether the object exists, and a 404 would tell a legitimate
// client its capture is gone.

// ErrRateLimited reports a throttled evidence lookup. It is not a denial.
var ErrRateLimited = errors.New("toolresultobjects: object evidence lookups are rate limited; retry later")

// maxRateInterval bounds the refill interval, so a mistyped configuration
// cannot throttle the route for good.
const maxRateInterval = 24 * time.Hour

// RateLimit configures RateLimitedPolicy. Every field is required.
type RateLimit struct {
	// Burst is how many lookups a key may make before it is throttled.
	Burst int
	// Interval is how long one token takes to refill.
	Interval time.Duration
	// MaxKeys bounds the tracked principals and sessions together. When the
	// table is full, keys whose buckets have refilled are forgotten; if none
	// has, a call naming a new key is throttled rather than the table growing.
	MaxKeys int
}

// DefaultRateLimit admits a burst of 32 lookups per principal and per session,
// refilling two a second, over at most 4096 tracked keys. A browser paging one
// 8 MiB capture in 1 MiB pages spends nine (metadata plus eight pages).
func DefaultRateLimit() RateLimit {
	return RateLimit{Burst: 32, Interval: 500 * time.Millisecond, MaxKeys: 4096}
}

// Validate refuses a limit that would admit nothing, refill never, or track
// fewer keys than one call needs.
func (r RateLimit) Validate() error {
	if r.Burst < 1 || r.Interval <= 0 || r.Interval > maxRateInterval || r.MaxKeys < 2 {
		return fmt.Errorf("%w: rate limit needs Burst >= 1, 0 < Interval <= %s and MaxKeys >= 2", ErrIncompleteConfig, maxRateInterval)
	}
	return nil
}

// RateLimitedPolicy is a factory.ObjectPolicy that admits a call to the policy
// it guards only while both the principal's and the session's budgets allow.
type RateLimitedPolicy struct {
	next  factory.ObjectPolicy
	limit RateLimit
	now   func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
}

var _ factory.ObjectPolicy = (*RateLimitedPolicy)(nil)

type bucket struct {
	tokens int
	last   time.Time
}

// NewRateLimitedPolicy guards next. A nil now means time.Now.
func NewRateLimitedPolicy(next factory.ObjectPolicy, limit RateLimit, now func() time.Time) (*RateLimitedPolicy, error) {
	if err := limit.Validate(); err != nil {
		return nil, err
	}
	if isNil(next) {
		return nil, fmt.Errorf("%w: NewRateLimitedPolicy needs a policy to guard", ErrIncompleteConfig)
	}
	if now == nil {
		now = time.Now
	}
	return &RateLimitedPolicy{next: next, limit: limit, now: now, buckets: make(map[string]*bucket)}, nil
}

// AuthorizeReference satisfies factory.ObjectPolicy.
func (p *RateLimitedPolicy) AuthorizeReference(ctx context.Context, principal identity.Principal, entry sessionstore.CatalogEntry, ref sessionwire.ObjectReference) (sessionstore.ObjectKind, error) {
	principalKey := "principal\x00" + string(principal.Tenant()) + "\x00" + string(principal.Kind()) + "\x00" + principal.Subject()
	sessionKey := "session\x00" + string(entry.Record.TenantID) + "\x00" + string(entry.Record.SessionID)
	if !p.take(principalKey, sessionKey) {
		return "", fmt.Errorf("%w: session %s", ErrRateLimited, entry.Record.SessionID)
	}
	return p.next.AuthorizeReference(ctx, principal, entry, ref)
}

// take spends one token from every key, or from none of them.
func (p *RateLimitedPolicy) take(keys ...string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	missing := 0
	for _, key := range keys {
		if b, ok := p.buckets[key]; ok {
			p.refill(b, now)
		} else {
			missing++
		}
	}
	if len(p.buckets)+missing > p.limit.MaxKeys {
		p.pruneIdle(now)
		if len(p.buckets)+missing > p.limit.MaxKeys {
			return false
		}
	}
	for _, key := range keys {
		if b, ok := p.buckets[key]; ok && b.tokens < 1 {
			return false
		}
	}
	for _, key := range keys {
		b, ok := p.buckets[key]
		if !ok {
			b = &bucket{tokens: p.limit.Burst, last: now}
			p.buckets[key] = b
		}
		b.tokens--
	}
	return true
}

// refill credits whole intervals since the bucket's last credit, keeping the
// remainder, and caps at the burst. A full bucket accrues nothing: its clock is
// moved to now, so the first token spent after an idle spell refills a whole
// interval later rather than early. A clock that stepped backwards credits
// nothing.
func (p *RateLimitedPolicy) refill(b *bucket, now time.Time) {
	if b.tokens >= p.limit.Burst {
		if now.After(b.last) {
			b.last = now
		}
		return
	}
	elapsed := now.Sub(b.last)
	if elapsed < p.limit.Interval {
		return
	}
	intervals := elapsed / p.limit.Interval
	if intervals >= time.Duration(p.limit.Burst-b.tokens) {
		b.tokens, b.last = p.limit.Burst, now
		return
	}
	b.tokens += int(intervals)
	b.last = b.last.Add(intervals * p.limit.Interval)
}

// pruneIdle forgets every bucket that has refilled to the burst: forgetting
// one is indistinguishable from keeping it.
func (p *RateLimitedPolicy) pruneIdle(now time.Time) {
	for key, b := range p.buckets {
		p.refill(b, now)
		if b.tokens >= p.limit.Burst {
			delete(p.buckets, key)
		}
	}
}

// trackedKeys reports the table's size, for tests.
func (p *RateLimitedPolicy) trackedKeys() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.buckets)
}
