package toolresultobjects

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/factory"
	"github.com/looprig/factory/identity"
	"github.com/looprig/sessionstore"
)

// countingPolicy is the policy a limiter guards: it counts how often it was
// reached and answers a fixed result, so a case can prove a throttled call
// never reached the evidence scan.
type countingPolicy struct {
	calls atomic.Int64
	err   error
}

func (p *countingPolicy) AuthorizeReference(context.Context, identity.Principal, sessionstore.CatalogEntry, sessionwire.ObjectReference) (sessionstore.ObjectKind, error) {
	p.calls.Add(1)
	if p.err != nil {
		return "", p.err
	}
	return sessionstore.ObjectKindToolResult, nil
}

// fakeClock is a manually advanced clock.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func newClock() *fakeClock { return &fakeClock{now: time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)} }

func limited(t *testing.T, next *countingPolicy, limit RateLimit, clock *fakeClock) *RateLimitedPolicy {
	t.Helper()
	policy, err := NewRateLimitedPolicy(next, limit, clock.Now)
	if err != nil {
		t.Fatalf("NewRateLimitedPolicy: %v", err)
	}
	return policy
}

func sessionEntry(tenant sessionwire.TenantID, session sessionwire.SessionID) sessionstore.CatalogEntry {
	e := entry(tenant, testBinding, testVersion, testRuntime.String())
	e.Record.SessionID = session
	return e
}

func subject(t *testing.T, tenant sessionwire.TenantID, name string) identity.Principal {
	t.Helper()
	p, err := identity.NewPrincipal(tenant, name, identity.KindActor)
	if err != nil {
		t.Fatalf("NewPrincipal: %v", err)
	}
	return p
}

func ask(p *RateLimitedPolicy, who identity.Principal, e sessionstore.CatalogEntry) (sessionstore.ObjectKind, error) {
	return p.AuthorizeReference(context.Background(), who, e, sessionwire.ObjectReference{ObjectID: testRef})
}

// assertThrottled requires the throttle's own answer: ErrRateLimited, and NOT
// a denial -- a throttled read is not evidence that the object is absent, so
// it must never become Factory's absent-object 404.
func assertThrottled(t *testing.T, kind sessionstore.ObjectKind, err error) {
	t.Helper()
	if kind != "" || !errors.Is(err, ErrRateLimited) || errors.Is(err, identity.ErrUnauthorized) {
		t.Fatalf("AuthorizeReference = (%q, %v), want ErrRateLimited and no denial", kind, err)
	}
}

func TestRateLimitedPolicyAdmitsABurstThenThrottlesWithoutScanning(t *testing.T) {
	next := &countingPolicy{}
	clock := newClock()
	policy := limited(t, next, RateLimit{Burst: 3, Interval: time.Second, MaxKeys: 64}, clock)
	who, e := subject(t, testTenant, "alice"), sessionEntry(testTenant, "s1")
	for i := range 3 {
		if kind, err := ask(policy, who, e); err != nil || kind != sessionstore.ObjectKindToolResult {
			t.Fatalf("call %d = (%q, %v), want the guarded policy's grant", i, kind, err)
		}
	}
	kind, err := ask(policy, who, e)
	assertThrottled(t, kind, err)
	if got := next.calls.Load(); got != 3 {
		t.Fatalf("guarded policy reached %d times, want 3: a throttled call must not scan", got)
	}
}

func TestRateLimitedPolicyRefillsOneTokenPerIntervalUpToTheBurst(t *testing.T) {
	next := &countingPolicy{}
	clock := newClock()
	policy := limited(t, next, RateLimit{Burst: 2, Interval: time.Second, MaxKeys: 64}, clock)
	who, e := subject(t, testTenant, "alice"), sessionEntry(testTenant, "s1")
	for range 2 {
		if _, err := ask(policy, who, e); err != nil {
			t.Fatalf("burst call: %v", err)
		}
	}
	// Just short of one interval refills nothing.
	clock.Advance(time.Second - time.Nanosecond)
	kind, err := ask(policy, who, e)
	assertThrottled(t, kind, err)
	// Crossing it refills exactly one.
	clock.Advance(time.Nanosecond)
	if _, err := ask(policy, who, e); err != nil {
		t.Fatalf("after one interval: %v", err)
	}
	kind, err = ask(policy, who, e)
	assertThrottled(t, kind, err)
	// An hour idle refills to the burst, never beyond it.
	clock.Advance(time.Hour)
	for range 2 {
		if _, err := ask(policy, who, e); err != nil {
			t.Fatalf("after a long idle: %v", err)
		}
	}
	kind, err = ask(policy, who, e)
	assertThrottled(t, kind, err)
	if got := next.calls.Load(); got != 5 {
		t.Fatalf("guarded policy reached %d times, want 5", got)
	}
}

// A partial interval is not lost: 1.5 intervals then another 0.5 is two
// refills, not one.
func TestRateLimitedPolicyCarriesAPartialInterval(t *testing.T) {
	next := &countingPolicy{}
	clock := newClock()
	policy := limited(t, next, RateLimit{Burst: 2, Interval: time.Second, MaxKeys: 64}, clock)
	who, e := subject(t, testTenant, "alice"), sessionEntry(testTenant, "s1")
	for range 2 {
		if _, err := ask(policy, who, e); err != nil {
			t.Fatalf("burst call: %v", err)
		}
	}
	clock.Advance(1500 * time.Millisecond)
	if _, err := ask(policy, who, e); err != nil {
		t.Fatalf("after 1.5 intervals: %v", err)
	}
	clock.Advance(500 * time.Millisecond)
	if _, err := ask(policy, who, e); err != nil {
		t.Fatalf("after 2 intervals in total: %v", err)
	}
	kind, err := ask(policy, who, e)
	assertThrottled(t, kind, err)
}

// Both budgets must admit: the SESSION's is shared by every principal reading
// it, and the PRINCIPAL's by every session it reads.
func TestRateLimitedPolicyChargesThePrincipalAndTheSession(t *testing.T) {
	next := &countingPolicy{}
	clock := newClock()
	policy := limited(t, next, RateLimit{Burst: 2, Interval: time.Minute, MaxKeys: 64}, clock)
	alice, bob := subject(t, testTenant, "alice"), subject(t, testTenant, "bob")
	s1, s2 := sessionEntry(testTenant, "s1"), sessionEntry(testTenant, "s2")
	for range 2 {
		if _, err := ask(policy, alice, s1); err != nil {
			t.Fatalf("alice s1: %v", err)
		}
	}
	// s1's budget is spent: bob is throttled on it although his own is full.
	kind, err := ask(policy, bob, s1)
	assertThrottled(t, kind, err)
	// alice's budget is spent: she is throttled on s2 although its is full.
	kind, err = ask(policy, alice, s2)
	assertThrottled(t, kind, err)
	// Neither spent budget touched bob-on-s2, and the refusals above charged
	// nothing: bob still has his whole burst there.
	for range 2 {
		if _, err := ask(policy, bob, s2); err != nil {
			t.Fatalf("bob s2: %v", err)
		}
	}
	if got := next.calls.Load(); got != 4 {
		t.Fatalf("guarded policy reached %d times, want 4", got)
	}
}

// Keys are tenant-scoped: the same subject and session id in another tenant
// is another budget.
func TestRateLimitedPolicyKeysAreTenantScoped(t *testing.T) {
	next := &countingPolicy{}
	clock := newClock()
	policy := limited(t, next, RateLimit{Burst: 1, Interval: time.Minute, MaxKeys: 64}, clock)
	if _, err := ask(policy, subject(t, testTenant, "alice"), sessionEntry(testTenant, "s1")); err != nil {
		t.Fatalf("tenant-a: %v", err)
	}
	if _, err := ask(policy, subject(t, otherTenant, "alice"), sessionEntry(otherTenant, "s1")); err != nil {
		t.Fatalf("tenant-b shares tenant-a's budget: %v", err)
	}
}

// The subject's KIND is part of its key: a service principal named like an
// actor does not spend the actor's budget.
func TestRateLimitedPolicyKeysNameThePrincipalKind(t *testing.T) {
	next := &countingPolicy{}
	clock := newClock()
	policy := limited(t, next, RateLimit{Burst: 1, Interval: time.Minute, MaxKeys: 64}, clock)
	actor := subject(t, testTenant, "alice")
	service, err := identity.NewPrincipal(testTenant, "alice", identity.KindService)
	if err != nil {
		t.Fatalf("NewPrincipal: %v", err)
	}
	if _, err := ask(policy, actor, sessionEntry(testTenant, "s1")); err != nil {
		t.Fatalf("actor: %v", err)
	}
	if _, err := ask(policy, service, sessionEntry(testTenant, "s2")); err != nil {
		t.Fatalf("a service principal shares the actor's budget: %v", err)
	}
}

// A DENIAL still costs a token: every forged reference is exactly the scan the
// limiter exists to bound (tests gate A4). The denial itself passes through
// unchanged, so Factory still answers it as absence.
func TestRateLimitedPolicyChargesDenialsAndPassesThemThrough(t *testing.T) {
	denial := deny(ErrNoEvidence, "forged")
	next := &countingPolicy{err: denial}
	clock := newClock()
	policy := limited(t, next, RateLimit{Burst: 2, Interval: time.Minute, MaxKeys: 64}, clock)
	who, e := subject(t, testTenant, "mallory"), sessionEntry(testTenant, "s1")
	for range 2 {
		if _, err := ask(policy, who, e); !errors.Is(err, identity.ErrUnauthorized) || !errors.Is(err, ErrNoEvidence) {
			t.Fatalf("denial = %v, want the guarded policy's denial unchanged", err)
		}
	}
	kind, err := ask(policy, who, e)
	assertThrottled(t, kind, err)
	if got := next.calls.Load(); got != 2 {
		t.Fatalf("guarded policy reached %d times, want 2", got)
	}
}

// The key table is bounded. When it is full of buckets still in use, a new
// key is throttled rather than the table growing; once those buckets have
// refilled they are idle, are pruned, and the new key is admitted.
func TestRateLimitedPolicyBoundsItsKeyTable(t *testing.T) {
	next := &countingPolicy{}
	clock := newClock()
	policy := limited(t, next, RateLimit{Burst: 2, Interval: time.Second, MaxKeys: 2}, clock)
	alice, bob := subject(t, testTenant, "alice"), subject(t, testTenant, "bob")
	if _, err := ask(policy, alice, sessionEntry(testTenant, "s1")); err != nil {
		t.Fatalf("first pair: %v", err)
	}
	kind, err := ask(policy, bob, sessionEntry(testTenant, "s2"))
	assertThrottled(t, kind, err)
	if got := policy.trackedKeys(); got != 2 {
		t.Fatalf("tracked keys = %d, want the table held at its bound of 2", got)
	}
	// A key already tracked is still served while the table is full.
	if _, err := ask(policy, alice, sessionEntry(testTenant, "s1")); err != nil {
		t.Fatalf("a tracked pair while full: %v", err)
	}
	clock.Advance(2 * time.Second)
	if _, err := ask(policy, bob, sessionEntry(testTenant, "s2")); err != nil {
		t.Fatalf("after the old buckets went idle: %v", err)
	}
	if got := policy.trackedKeys(); got != 2 {
		t.Fatalf("tracked keys = %d, want the idle pair pruned and the new one tracked", got)
	}
}

// A clock that steps backwards refills nothing and does not wedge the bucket.
func TestRateLimitedPolicyToleratesAClockStepBackwards(t *testing.T) {
	next := &countingPolicy{}
	clock := newClock()
	policy := limited(t, next, RateLimit{Burst: 1, Interval: time.Second, MaxKeys: 64}, clock)
	who, e := subject(t, testTenant, "alice"), sessionEntry(testTenant, "s1")
	if _, err := ask(policy, who, e); err != nil {
		t.Fatalf("first: %v", err)
	}
	clock.Advance(-time.Hour)
	kind, err := ask(policy, who, e)
	assertThrottled(t, kind, err)
	clock.Advance(time.Hour + time.Second)
	if _, err := ask(policy, who, e); err != nil {
		t.Fatalf("after the clock recovered: %v", err)
	}
}

// Concurrent callers cannot overspend a budget.
func TestRateLimitedPolicyIsSafeForConcurrentUse(t *testing.T) {
	next := &countingPolicy{}
	clock := newClock()
	policy := limited(t, next, RateLimit{Burst: 5, Interval: time.Hour, MaxKeys: 64}, clock)
	who, e := subject(t, testTenant, "alice"), sessionEntry(testTenant, "s1")
	var wg sync.WaitGroup
	var granted atomic.Int64
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := ask(policy, who, e); err == nil {
				granted.Add(1)
			}
		}()
	}
	wg.Wait()
	if granted.Load() != 5 || next.calls.Load() != 5 {
		t.Fatalf("granted %d, reached %d; want exactly the burst of 5", granted.Load(), next.calls.Load())
	}
}

func TestRateLimitValidation(t *testing.T) {
	if err := DefaultRateLimit().Validate(); err != nil {
		t.Fatalf("DefaultRateLimit is invalid: %v", err)
	}
	for name, limit := range map[string]RateLimit{
		"zero":             {},
		"zero burst":       {Burst: 0, Interval: time.Second, MaxKeys: 64},
		"negative burst":   {Burst: -1, Interval: time.Second, MaxKeys: 64},
		"zero interval":    {Burst: 1, Interval: 0, MaxKeys: 64},
		"negative period":  {Burst: 1, Interval: -time.Second, MaxKeys: 64},
		"one key":          {Burst: 1, Interval: time.Second, MaxKeys: 1},
		"zero keys":        {Burst: 1, Interval: time.Second, MaxKeys: 0},
		"negative keys":    {Burst: 1, Interval: time.Second, MaxKeys: -2},
		"interval too big": {Burst: 1, Interval: 25 * time.Hour, MaxKeys: 64},
	} {
		if err := limit.Validate(); !errors.Is(err, ErrIncompleteConfig) {
			t.Fatalf("%s: Validate(%+v) = %v, want ErrIncompleteConfig", name, limit, err)
		}
		if p, err := NewRateLimitedPolicy(&countingPolicy{}, limit, nil); p != nil || !errors.Is(err, ErrIncompleteConfig) {
			t.Fatalf("%s: NewRateLimitedPolicy = (%v, %v), want a refusal", name, p, err)
		}
	}
	// The smallest valid table holds one principal and one session.
	if err := (RateLimit{Burst: 1, Interval: time.Second, MaxKeys: 2}).Validate(); err != nil {
		t.Fatalf("a two-key table is refused: %v", err)
	}
	var typedNil *countingPolicy
	if p, err := NewRateLimitedPolicy(typedNil, DefaultRateLimit(), nil); p != nil || !errors.Is(err, ErrIncompleteConfig) {
		t.Fatalf("NewRateLimitedPolicy(typed nil) = (%v, %v), want a refusal", p, err)
	}
	if p, err := NewRateLimitedPolicy(nil, DefaultRateLimit(), nil); p != nil || !errors.Is(err, ErrIncompleteConfig) {
		t.Fatalf("NewRateLimitedPolicy(nil) = (%v, %v), want a refusal", p, err)
	}
	// A nil clock means the wall clock, not a panic.
	p, err := NewRateLimitedPolicy(&countingPolicy{}, DefaultRateLimit(), nil)
	if err != nil {
		t.Fatalf("NewRateLimitedPolicy(nil clock): %v", err)
	}
	if _, err := ask(p, subject(t, testTenant, "alice"), sessionEntry(testTenant, "s1")); err != nil {
		t.Fatalf("wall-clock policy: %v", err)
	}
}

// FactoryOptions composes the limiter IN FRONT of the evidence policy: past
// the burst, the evidence is no longer consulted.
func TestFactoryOptionsGuardTheEvidenceScanWithTheLimiter(t *testing.T) {
	evidence := captured(testRuntime, testRef)
	policy, err := composedPolicy(Config{
		Binding:  Binding{StorageBindingID: testBinding, BindingVersion: testVersion},
		Evidence: func(tenant sessionwire.TenantID) (Evidence, bool) { return evidence, tenant == testTenant },
		Objects: func(sessionwire.TenantID) (factory.ObjectReader, bool) {
			return nil, false
		},
		Limits:    factory.DefaultObjectLimits(),
		RateLimit: RateLimit{Burst: 2, Interval: time.Hour, MaxKeys: 64},
	})
	if err != nil {
		t.Fatalf("composedPolicy: %v", err)
	}
	who, e := subject(t, testTenant, "alice"), entry(testTenant, testBinding, testVersion, testRuntime.String())
	for range 2 {
		if kind, err := policy.AuthorizeReference(context.Background(), who, e, sessionwire.ObjectReference{ObjectID: testRef}); err != nil || kind != sessionstore.ObjectKindToolResult {
			t.Fatalf("grant = (%q, %v)", kind, err)
		}
	}
	kind, err := policy.AuthorizeReference(context.Background(), who, e, sessionwire.ObjectReference{ObjectID: testRef})
	assertThrottled(t, kind, err)
	if len(evidence.asked) != 2 {
		t.Fatalf("evidence consulted %d times, want 2", len(evidence.asked))
	}
}

// A full bucket accrues nothing while it waits: after an idle spell, the token
// spent next refills one whole interval after THAT spend, not earlier.
func TestRateLimitedPolicyFullBucketAccruesNothing(t *testing.T) {
	next := &countingPolicy{}
	clock := newClock()
	policy := limited(t, next, RateLimit{Burst: 1, Interval: time.Second, MaxKeys: 64}, clock)
	alice, bob := subject(t, testTenant, "alice"), subject(t, testTenant, "bob")
	// alice spends her token; bob empties s1.
	if _, err := ask(policy, alice, sessionEntry(testTenant, "s0")); err != nil {
		t.Fatal(err)
	}
	clock.Advance(900 * time.Millisecond)
	if _, err := ask(policy, bob, sessionEntry(testTenant, "s1")); err != nil {
		t.Fatal(err)
	}
	// At 1.0s alice is full again but s1 is empty: refused, nothing spent.
	clock.Advance(100 * time.Millisecond)
	kind, err := ask(policy, alice, sessionEntry(testTenant, "s1"))
	assertThrottled(t, kind, err)
	// At 1.5s she is still full and still refused on s1.
	clock.Advance(500 * time.Millisecond)
	kind, err = ask(policy, alice, sessionEntry(testTenant, "s1"))
	assertThrottled(t, kind, err)
	// At 1.9s she spends it on a fresh session.
	clock.Advance(400 * time.Millisecond)
	if _, err := ask(policy, alice, sessionEntry(testTenant, "s2")); err != nil {
		t.Fatalf("spend after idle: %v", err)
	}
	// At 2.0s -- a tenth of an interval after that spend -- she has nothing.
	clock.Advance(100 * time.Millisecond)
	kind, err = ask(policy, alice, sessionEntry(testTenant, "s3"))
	assertThrottled(t, kind, err)
	// A whole interval after the spend, she has one again.
	clock.Advance(900 * time.Millisecond)
	if _, err := ask(policy, alice, sessionEntry(testTenant, "s3")); err != nil {
		t.Fatalf("one interval after the spend: %v", err)
	}
}
