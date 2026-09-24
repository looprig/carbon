package toolresultobjects

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/core/uuid"
	"github.com/looprig/factory"
	"github.com/looprig/factory/identity"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/loop"
	harnessstore "github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/sessionstore"
)

const (
	testTenant  = sessionwire.TenantID("tenant-a")
	otherTenant = sessionwire.TenantID("tenant-b")
	testBinding = "binding-1"
	testVersion = "v1"
	testRef     = "v1:tool-result:1:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

var testRuntime = uuid.MustParse("5f0c2f5e-8d0a-4c55-9d7b-3c1a0e4b2a11")

// fakeEvidence answers LookupToolResultCapture from a fixed table and records
// every question, so a case can prove WHICH session a policy asked about.
type fakeEvidence struct {
	captures map[string]event.ToolResultCapture // key: runtime/objectID
	err      error
	asked    []string
}

func (f *fakeEvidence) LookupToolResultCapture(_ context.Context, runtime uuid.UUID, ref sessionwire.ObjectReference) (event.ToolResultCapture, bool, error) {
	key := runtime.String() + "/" + ref.ObjectID
	f.asked = append(f.asked, key)
	if f.err != nil {
		return event.ToolResultCapture{}, false, f.err
	}
	capture, ok := f.captures[key]
	return capture, ok, nil
}

// notFoundWithCapture answers "not found" together with a populated capture.
type notFoundWithCapture struct{}

func (notFoundWithCapture) LookupToolResultCapture(context.Context, uuid.UUID, sessionwire.ObjectReference) (event.ToolResultCapture, bool, error) {
	return event.ToolResultCapture{Reference: &sessionwire.ObjectReference{ObjectID: testRef}}, false, nil
}

func captured(runtime uuid.UUID, objectID string) *fakeEvidence {
	return &fakeEvidence{captures: map[string]event.ToolResultCapture{
		runtime.String() + "/" + objectID: {Reference: &sessionwire.ObjectReference{ObjectID: objectID}, CapturedBytes: 10},
	}}
}

func principal(t *testing.T, tenant sessionwire.TenantID) identity.Principal {
	t.Helper()
	p, err := identity.NewPrincipal(tenant, "user", identity.KindActor)
	if err != nil {
		t.Fatalf("NewPrincipal: %v", err)
	}
	return p
}

func entry(tenant sessionwire.TenantID, binding, version string, runtime string) sessionstore.CatalogEntry {
	return sessionstore.CatalogEntry{Record: sessionstore.CatalogRecord{
		TenantID:  tenant,
		SessionID: "public-session",
		Binding: sessionstore.SessionBinding{
			StorageBindingID: binding,
			BindingVersion:   version,
			RuntimeSessionID: runtime,
			ProtocolMode:     sessionstore.ProtocolModeDisposition,
		},
	}}
}

func newTestPolicy(t *testing.T, evidence map[sessionwire.TenantID]Evidence) *Policy {
	t.Helper()
	policy, err := NewPolicy(Binding{StorageBindingID: testBinding, BindingVersion: testVersion}, func(tenant sessionwire.TenantID) (Evidence, bool) {
		e, ok := evidence[tenant]
		return e, ok
	})
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	return policy
}

func TestPolicyGrantsToolResultOnlyWithCommittedEvidence(t *testing.T) {
	evidence := captured(testRuntime, testRef)
	policy := newTestPolicy(t, map[sessionwire.TenantID]Evidence{testTenant: evidence})
	kind, err := policy.AuthorizeReference(context.Background(), principal(t, testTenant),
		entry(testTenant, testBinding, testVersion, testRuntime.String()), sessionwire.ObjectReference{ObjectID: testRef})
	if err != nil || kind != sessionstore.ObjectKindToolResult {
		t.Fatalf("AuthorizeReference = (%q, %v), want (%q, nil)", kind, err, sessionstore.ObjectKindToolResult)
	}
	// The evidence is asked about the RUNTIME session named by the catalog
	// binding -- never the public id.
	if want := testRuntime.String() + "/" + testRef; len(evidence.asked) != 1 || evidence.asked[0] != want {
		t.Fatalf("evidence was asked %v, want exactly [%s]", evidence.asked, want)
	}
}

// Every refusal below must be a DENIAL (identity.ErrUnauthorized, which
// Factory answers with the absent-object 404) and must name its reason.
func TestPolicyDenials(t *testing.T) {
	otherRuntime := uuid.MustParse("7a1e5b2c-0f9d-4b8e-a6c3-2d4f1e0b9c77")
	cases := []struct {
		name     string
		evidence map[sessionwire.TenantID]Evidence
		who      sessionwire.TenantID
		entry    sessionstore.CatalogEntry
		ref      string
		reason   error
		asked    bool
	}{
		{
			name:     "no committed capture names the reference (an orphan or another session's)",
			evidence: map[sessionwire.TenantID]Evidence{testTenant: captured(otherRuntime, testRef)},
			who:      testTenant, entry: entry(testTenant, testBinding, testVersion, testRuntime.String()),
			ref: testRef, reason: ErrNoEvidence, asked: true,
		},
		{
			name:     "principal's tenant is not the catalog's",
			evidence: map[sessionwire.TenantID]Evidence{testTenant: captured(testRuntime, testRef), otherTenant: captured(testRuntime, testRef)},
			who:      otherTenant, entry: entry(testTenant, testBinding, testVersion, testRuntime.String()),
			ref: testRef, reason: ErrForeignTenant,
		},
		{
			name:     "unknown storage binding",
			evidence: map[sessionwire.TenantID]Evidence{testTenant: captured(testRuntime, testRef)},
			who:      testTenant, entry: entry(testTenant, "binding-other", testVersion, testRuntime.String()),
			ref: testRef, reason: ErrUnknownBinding,
		},
		{
			name:     "unknown binding version",
			evidence: map[sessionwire.TenantID]Evidence{testTenant: captured(testRuntime, testRef)},
			who:      testTenant, entry: entry(testTenant, testBinding, "v2", testRuntime.String()),
			ref: testRef, reason: ErrUnknownBinding,
		},
		{
			name:     "legacy (zero) binding has no runtime journal",
			evidence: map[sessionwire.TenantID]Evidence{testTenant: captured(testRuntime, testRef)},
			who:      testTenant, entry: sessionstore.CatalogEntry{Record: sessionstore.CatalogRecord{TenantID: testTenant, SessionID: "s"}},
			ref: testRef, reason: ErrUnknownBinding,
		},
		{
			name:     "runtime session id is not a UUID",
			evidence: map[sessionwire.TenantID]Evidence{testTenant: captured(testRuntime, testRef)},
			who:      testTenant, entry: entry(testTenant, testBinding, testVersion, "not-a-uuid"),
			ref: testRef, reason: ErrNoRuntimeSession,
		},
		{
			name:     "runtime session id is the zero UUID",
			evidence: map[sessionwire.TenantID]Evidence{testTenant: captured(uuid.UUID{}, testRef)},
			who:      testTenant, entry: entry(testTenant, testBinding, testVersion, uuid.UUID{}.String()),
			ref: testRef, reason: ErrNoRuntimeSession,
		},
		{
			name:     "tenant has no runtime store in this deployment",
			evidence: map[sessionwire.TenantID]Evidence{otherTenant: captured(testRuntime, testRef)},
			who:      testTenant, entry: entry(testTenant, testBinding, testVersion, testRuntime.String()),
			ref: testRef, reason: ErrUnknownBinding,
		},
		{
			// Evidence that "names" the malformed id proves the refusal is the
			// policy's own validation, not a lookup miss.
			name:     "malformed reference",
			evidence: map[sessionwire.TenantID]Evidence{testTenant: captured(testRuntime, "")},
			who:      testTenant, entry: entry(testTenant, testBinding, testVersion, testRuntime.String()),
			ref: "", reason: ErrInvalidReference,
		},
		{
			name: "evidence scan budget exhausted is a refusal, never a grant",
			evidence: map[sessionwire.TenantID]Evidence{testTenant: &fakeEvidence{
				err: &harnessstore.ToolResultCaptureScanBudgetError{SessionID: testRuntime, Examined: harnessstore.ToolResultCaptureScanBudget},
			}},
			who: testTenant, entry: entry(testTenant, testBinding, testVersion, testRuntime.String()),
			ref: testRef, reason: ErrEvidenceBudget, asked: true,
		},
		{
			name: "evidence names another object than the one asked for",
			evidence: map[sessionwire.TenantID]Evidence{testTenant: &fakeEvidence{captures: map[string]event.ToolResultCapture{
				testRuntime.String() + "/" + testRef: {Reference: &sessionwire.ObjectReference{ObjectID: testRef + "0"}},
			}}},
			who: testTenant, entry: entry(testTenant, testBinding, testVersion, testRuntime.String()),
			ref: testRef, reason: ErrNoEvidence, asked: true,
		},
		{
			// A store that says "not found" is believed, whatever else it
			// returned alongside.
			name:     "evidence reporting not-found is not a grant",
			evidence: map[sessionwire.TenantID]Evidence{testTenant: notFoundWithCapture{}},
			who:      testTenant, entry: entry(testTenant, testBinding, testVersion, testRuntime.String()),
			ref: testRef, reason: ErrNoEvidence,
		},
		{
			name: "evidence is a reference-free (inline) capture",
			evidence: map[sessionwire.TenantID]Evidence{testTenant: &fakeEvidence{captures: map[string]event.ToolResultCapture{
				testRuntime.String() + "/" + testRef: {CapturedBytes: 3},
			}}},
			who: testTenant, entry: entry(testTenant, testBinding, testVersion, testRuntime.String()),
			ref: testRef, reason: ErrNoEvidence, asked: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy := newTestPolicy(t, tc.evidence)
			kind, err := policy.AuthorizeReference(context.Background(), principal(t, tc.who), tc.entry, sessionwire.ObjectReference{ObjectID: tc.ref})
			if kind != "" {
				t.Fatalf("a denial returned kind %q", kind)
			}
			if !errors.Is(err, identity.ErrUnauthorized) {
				t.Fatalf("err = %v, want a denial wrapping identity.ErrUnauthorized", err)
			}
			if !errors.Is(err, tc.reason) {
				t.Fatalf("err = %v, want reason %v", err, tc.reason)
			}
			asked := false
			for _, e := range tc.evidence {
				if f, ok := e.(*fakeEvidence); ok && len(f.asked) > 0 {
					asked = true
				}
			}
			if asked != tc.asked {
				t.Fatalf("evidence consulted = %v, want %v", asked, tc.asked)
			}
		})
	}
}

// A store fault is NOT a denial: answering 404 for an outage would tell a
// client its capture does not exist. It stays a policy fault (500).
func TestPolicyStoreFaultIsNotADenial(t *testing.T) {
	fault := errors.New("backend down")
	policy := newTestPolicy(t, map[sessionwire.TenantID]Evidence{testTenant: &fakeEvidence{err: fault}})
	kind, err := policy.AuthorizeReference(context.Background(), principal(t, testTenant),
		entry(testTenant, testBinding, testVersion, testRuntime.String()), sessionwire.ObjectReference{ObjectID: testRef})
	if kind != "" || !errors.Is(err, fault) || errors.Is(err, identity.ErrUnauthorized) {
		t.Fatalf("AuthorizeReference = (%q, %v), want the store fault and no denial", kind, err)
	}
}

func TestNewPolicyRefusesIncompleteConfiguration(t *testing.T) {
	lookup := func(sessionwire.TenantID) (Evidence, bool) { return nil, false }
	for name, build := range map[string]func() (*Policy, error){
		"no binding id":      func() (*Policy, error) { return NewPolicy(Binding{BindingVersion: "v1"}, lookup) },
		"no binding version": func() (*Policy, error) { return NewPolicy(Binding{StorageBindingID: "b"}, lookup) },
		"no evidence":        func() (*Policy, error) { return NewPolicy(Binding{StorageBindingID: "b", BindingVersion: "v1"}, nil) },
	} {
		if policy, err := build(); err == nil || policy != nil {
			t.Fatalf("%s: NewPolicy = (%v, %v), want a refusal", name, policy, err)
		}
	}
}

// A nil Evidence answered for a known tenant is a composition fault, not a
// grant and not a panic.
func TestPolicyTypedNilEvidenceIsRefused(t *testing.T) {
	var none *fakeEvidence
	policy := newTestPolicy(t, map[sessionwire.TenantID]Evidence{testTenant: none})
	kind, err := policy.AuthorizeReference(context.Background(), principal(t, testTenant),
		entry(testTenant, testBinding, testVersion, testRuntime.String()), sessionwire.ObjectReference{ObjectID: testRef})
	if kind != "" || !errors.Is(err, identity.ErrUnauthorized) || !errors.Is(err, ErrUnknownBinding) {
		t.Fatalf("AuthorizeReference = (%q, %v), want an unknown-binding denial", kind, err)
	}
}

// A lookup that answers (nil, true) -- a BARE nil Evidence interface, not a
// typed nil pointer wrapped in one -- exercises isNil's fast `v == nil` path
// rather than the reflect.Pointer path TestPolicyTypedNilEvidenceIsRefused
// covers. Without the nil guard this would call a method on a nil interface
// and panic rather than refuse.
func TestPolicyBareNilEvidenceLookupIsRefused(t *testing.T) {
	policy := newTestPolicy(t, map[sessionwire.TenantID]Evidence{testTenant: nil})
	kind, err := policy.AuthorizeReference(context.Background(), principal(t, testTenant),
		entry(testTenant, testBinding, testVersion, testRuntime.String()), sessionwire.ObjectReference{ObjectID: testRef})
	if kind != "" || !errors.Is(err, identity.ErrUnauthorized) || !errors.Is(err, ErrUnknownBinding) {
		t.Fatalf("AuthorizeReference = (%q, %v), want an unknown-binding denial", kind, err)
	}
}

type nopReader struct{ tenant sessionwire.TenantID }

func (nopReader) GetObjectMetadata(context.Context, sessionstore.GetObjectMetadataRequest) (sessionwire.ObjectMetadata, error) {
	return sessionwire.ObjectMetadata{}, nil
}

func (nopReader) GetObject(context.Context, sessionstore.GetObjectRequest) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

func TestResolverAnswersTheTenantsRuntimeStoreForTheKnownBindingOnly(t *testing.T) {
	stores := map[sessionwire.TenantID]factory.ObjectReader{testTenant: nopReader{tenant: testTenant}, otherTenant: nopReader{tenant: otherTenant}}
	resolve, err := NewResolver(Binding{StorageBindingID: testBinding, BindingVersion: testVersion}, func(tenant sessionwire.TenantID) (factory.ObjectReader, bool) {
		r, ok := stores[tenant]
		return r, ok
	})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	known := entry(testTenant, testBinding, testVersion, testRuntime.String()).Record.Binding
	reader, err := resolve(context.Background(), testTenant, "public-session", known)
	if err != nil || reader != (nopReader{tenant: testTenant}) {
		t.Fatalf("resolve(known) = (%v, %v), want tenant-a's store", reader, err)
	}
	for name, call := range map[string]func() (factory.ObjectReader, error){
		"unknown binding id": func() (factory.ObjectReader, error) {
			return resolve(context.Background(), testTenant, "s", entry(testTenant, "other", testVersion, testRuntime.String()).Record.Binding)
		},
		"unknown binding version": func() (factory.ObjectReader, error) {
			return resolve(context.Background(), testTenant, "s", entry(testTenant, testBinding, "v9", testRuntime.String()).Record.Binding)
		},
		"unknown tenant": func() (factory.ObjectReader, error) {
			return resolve(context.Background(), "tenant-z", "s", known)
		},
	} {
		if reader, err := call(); reader != nil || !errors.Is(err, ErrUnknownBinding) {
			t.Fatalf("%s: resolve = (%v, %v), want ErrUnknownBinding", name, reader, err)
		}
	}
	if _, err := NewResolver(Binding{StorageBindingID: testBinding}, func(sessionwire.TenantID) (factory.ObjectReader, bool) { return nil, false }); err == nil {
		t.Fatal("NewResolver accepted an incomplete binding")
	}
	if _, err := NewResolver(Binding{StorageBindingID: testBinding, BindingVersion: testVersion}, nil); err == nil {
		t.Fatal("NewResolver accepted a nil store lookup")
	}
}

// D7: a runtime's capture ceiling must fit inside Factory's whole-object
// verification ceiling, or every capture above it answers 413.
func TestCheckCaptureCeiling(t *testing.T) {
	limits := factory.DefaultObjectLimits()
	cases := []struct {
		name    string
		capture int
		limits  factory.ObjectLimits
		ok      bool
	}{
		{"harness default (8 MiB) within the default 64 MiB", 0, limits, true},
		{"exactly the verification ceiling", 64 << 20, limits, true},
		{"one byte over the verification ceiling", 64<<20 + 1, limits, false},
		{"within a lowered verification ceiling", 4 << 20, factory.ObjectLimits{MaxPageBytes: 1 << 20, MaxVerificationBytes: 4 << 20}, true},
		{"harness default over a lowered verification ceiling", 0, factory.ObjectLimits{MaxPageBytes: 1 << 20, MaxVerificationBytes: 4 << 20}, false},
		{"negative ceiling", -1, limits, false},
		{"invalid object limits", 1 << 20, factory.ObjectLimits{MaxPageBytes: 2 << 20, MaxVerificationBytes: 64 << 20}, false},
		{"zero object limits", 1 << 20, factory.ObjectLimits{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckCaptureCeiling(tc.capture, tc.limits)
			if tc.ok != (err == nil) {
				t.Fatalf("CheckCaptureCeiling(%d, %+v) = %v, want ok=%v", tc.capture, tc.limits, err, tc.ok)
			}
			if err != nil && !errors.Is(err, ErrCaptureCeiling) {
				t.Fatalf("err = %v, want ErrCaptureCeiling", err)
			}
		})
	}
	// A negative ceiling is named as such, not reported as an enormous one
	// (which is what it would wrap to as a byte count).
	if err := CheckCaptureCeiling(-1, limits); err == nil || !strings.Contains(err.Error(), "negative capture ceiling") {
		t.Fatalf("CheckCaptureCeiling(-1) = %v, want the negative-ceiling refusal", err)
	}
	if EffectiveCaptureBytes(0) != loop.DefaultToolResultCaptureBytes || EffectiveCaptureBytes(1234) != 1234 {
		t.Fatalf("EffectiveCaptureBytes does not mirror harness's zero-means-default rule")
	}
}

func TestFactoryOptionsComposeThePolicyResolverAndLimits(t *testing.T) {
	binding := Binding{StorageBindingID: testBinding, BindingVersion: testVersion}
	evidence := func(sessionwire.TenantID) (Evidence, bool) { return nil, false }
	objects := func(sessionwire.TenantID) (factory.ObjectReader, bool) { return nil, false }
	rate := DefaultRateLimit()
	options, err := FactoryOptions(Config{Binding: binding, Evidence: evidence, Objects: objects, CaptureBytes: 1 << 20, Limits: factory.DefaultObjectLimits(), RateLimit: rate})
	if err != nil || len(options) != 3 {
		t.Fatalf("FactoryOptions = (%d options, %v), want policy + resolver + limits", len(options), err)
	}
	if _, err := FactoryOptions(Config{Binding: binding, Evidence: evidence, Objects: objects, CaptureBytes: 65 << 20, Limits: factory.DefaultObjectLimits(), RateLimit: rate}); !errors.Is(err, ErrCaptureCeiling) {
		t.Fatalf("FactoryOptions accepted a capture ceiling over the verification ceiling: %v", err)
	}
	if _, err := FactoryOptions(Config{Binding: binding, Objects: objects, Limits: factory.DefaultObjectLimits(), RateLimit: rate}); err == nil {
		t.Fatal("FactoryOptions accepted no evidence")
	}
	if _, err := FactoryOptions(Config{Binding: binding, Evidence: evidence, Limits: factory.DefaultObjectLimits(), RateLimit: rate}); err == nil {
		t.Fatal("FactoryOptions accepted no object stores")
	}
	// The object route is never composed without its evidence-scan limiter
	// (tests gate A4): a zero rate limit is an incomplete configuration.
	if _, err := FactoryOptions(Config{Binding: binding, Evidence: evidence, Objects: objects, Limits: factory.DefaultObjectLimits()}); !errors.Is(err, ErrIncompleteConfig) {
		t.Fatalf("FactoryOptions accepted no rate limit: %v", err)
	}
}

// A lookup that answers false is a refusal even when it also hands back a
// value: the boolean, not the value, says whether the tenant is served.
func TestLookupsAnsweringFalseAreRefusalsWhateverTheyReturn(t *testing.T) {
	binding := Binding{StorageBindingID: testBinding, BindingVersion: testVersion}
	resolve, err := NewResolver(binding, func(sessionwire.TenantID) (factory.ObjectReader, bool) {
		return nopReader{tenant: testTenant}, false
	})
	if err != nil {
		t.Fatal(err)
	}
	known := entry(testTenant, testBinding, testVersion, testRuntime.String()).Record.Binding
	if reader, err := resolve(context.Background(), testTenant, "s", known); reader != nil || !errors.Is(err, ErrUnknownBinding) {
		t.Fatalf("resolve with an unserved lookup = (%v, %v), want ErrUnknownBinding", reader, err)
	}
	evidence := captured(testRuntime, testRef)
	policy, err := NewPolicy(binding, func(sessionwire.TenantID) (Evidence, bool) { return evidence, false })
	if err != nil {
		t.Fatal(err)
	}
	kind, err := policy.AuthorizeReference(context.Background(), principal(t, testTenant),
		entry(testTenant, testBinding, testVersion, testRuntime.String()), sessionwire.ObjectReference{ObjectID: testRef})
	if kind != "" || !errors.Is(err, ErrUnknownBinding) || len(evidence.asked) != 0 {
		t.Fatalf("policy with an unserved lookup = (%q, %v), evidence asked %v; want an unknown-binding denial and no scan", kind, err, evidence.asked)
	}
}
