package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/host"
	"github.com/looprig/sessionstore"
)

// TestServeJournalResolverRefusesAnUnservedBinding pins the resolver's refusal:
// Factory hands it every disposition-bound catalog binding, and a binding this
// deployment does not serve must be refused, never defaulted to the tenant's
// journal — that would serve one deployment's journal under another's
// configuration. The legacy half refuses every read, because Carbon creates no
// legacy session and the control store's journal for a Host session is empty.
func TestServeJournalResolverRefusesAnUnservedBinding(t *testing.T) {
	const runtimeID = "2db05411-d065-4bdc-9d19-a23d1284ffb1"
	served := sessionstore.SessionBinding{StorageBindingID: "carbon-local-v1", BindingVersion: "v1", RuntimeSessionID: runtimeID, ProtocolMode: sessionstore.ProtocolModeDisposition}
	reader, err := NewServeSessionReader(&sessionstore.Store{}, &PooledLauncher{}, "tenant-a", "carbon-local-v1", "v1")
	if err != nil {
		t.Fatal(err)
	}
	journal, err := reader.ResolveJournal(context.Background(), "tenant-a", "public-a", served)
	if err != nil || journal == nil {
		t.Fatalf("served binding = %v, %v", journal, err)
	}
	// Only the one served tenant resolves: PooledLauncher would otherwise open
	// a backend for any valid tenant it is asked about.
	if other, err := reader.ResolveJournal(context.Background(), "tenant-b", "public-a", served); other != nil || !errors.As(err, new(*ServeJournalBindingError)) {
		t.Fatalf("another tenant = %v, %v; want a binding refusal", other, err)
	}
	// The resolved (projecting) reader is bound to its runtime session and
	// tenant, and so is the raw runtime reader beneath it.
	raw := &serveRuntimeJournal{launcher: &PooledLauncher{}, tenant: "tenant-a", runtimeID: runtimeID}
	for _, req := range []sessionstore.ReadPublicJournalRequest{
		{TenantID: "tenant-a", SessionID: "public-a"},
		{TenantID: "tenant-b", SessionID: runtimeID},
	} {
		if _, err := journal.ReadPublicJournal(context.Background(), req); !errors.Is(err, host.ErrPublicJournalScope) {
			t.Fatalf("%+v: %v, want host.ErrPublicJournalScope", req, err)
		}
		var bindingErr *ServeJournalBindingError
		if _, err := raw.ReadPublicJournal(context.Background(), req); !errors.As(err, &bindingErr) {
			t.Fatalf("raw public %+v: %v, want a binding refusal", req, err)
		}
		if _, err := raw.ReadRuntimeJournal(context.Background(), sessionstore.ReadRuntimeJournalRequest{TenantID: req.TenantID, SessionID: req.SessionID}); !errors.As(err, &bindingErr) {
			t.Fatalf("raw runtime %+v: %v, want a binding refusal", req, err)
		}
		if strings.Contains(bindingErr.Error(), runtimeID) {
			t.Fatalf("a refusal names the private runtime session id: %v", bindingErr)
		}
	}
	for _, tc := range []struct {
		name   string
		change func(*sessionstore.SessionBinding)
	}{
		{"other binding id", func(b *sessionstore.SessionBinding) { b.StorageBindingID = "other" }},
		{"other binding version", func(b *sessionstore.SessionBinding) { b.BindingVersion = "v2" }},
		{"legacy protocol", func(b *sessionstore.SessionBinding) { b.ProtocolMode = sessionstore.ProtocolModeLegacy }},
		{"zero runtime id", func(b *sessionstore.SessionBinding) { b.RuntimeSessionID = "00000000-0000-0000-0000-000000000000" }},
		{"malformed runtime id", func(b *sessionstore.SessionBinding) { b.RuntimeSessionID = "not-a-uuid" }},
		{"noncanonical runtime id", func(b *sessionstore.SessionBinding) { b.RuntimeSessionID = strings.ToUpper(runtimeID) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := served
			tc.change(&b)
			got, err := reader.ResolveJournal(context.Background(), "tenant-a", "public-a", b)
			var bindingErr *ServeJournalBindingError
			if !errors.As(err, &bindingErr) || got != nil {
				t.Fatalf("resolve = %v, %v; want a binding refusal", got, err)
			}
			if b.RuntimeSessionID != "" && strings.Contains(err.Error(), b.RuntimeSessionID) {
				t.Fatalf("a refusal names the private runtime session id: %v", err)
			}
		})
	}
	var bindingErr *ServeJournalBindingError
	if _, err := reader.ReadPublicJournal(context.Background(), sessionstore.ReadPublicJournalRequest{TenantID: "tenant-a", SessionID: "public-a"}); !errors.As(err, &bindingErr) {
		t.Fatalf("legacy journal half = %v, want a binding refusal", err)
	}
}

func TestServeSessionReaderRejectsMissingDependenciesAndObjects(t *testing.T) {
	for _, tc := range []struct {
		name        string
		control     *sessionstore.Store
		launcher    *PooledLauncher
		tenant      sessionwire.TenantID
		id, version string
	}{
		{"nil control", nil, &PooledLauncher{}, "local", "binding", "v1"},
		{"nil launcher", &sessionstore.Store{}, nil, "local", "binding", "v1"},
		{"invalid served tenant", &sessionstore.Store{}, &PooledLauncher{}, "", "binding", "v1"},
		{"empty binding ID", &sessionstore.Store{}, &PooledLauncher{}, "local", "", "v1"},
		{"empty binding version", &sessionstore.Store{}, &PooledLauncher{}, "local", "binding", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewServeSessionReader(tc.control, tc.launcher, tc.tenant, tc.id, tc.version)
			var configErr *ServeSessionReaderConfigError
			if !errors.As(err, &configErr) {
				t.Fatalf("constructor error = %v", err)
			}
		})
	}
	storage, err := OpenServeStorage(context.Background(), Config{}, ServeStorageConfig{DataDir: t.TempDir(), DefaultTenant: "local"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = storage.Close(context.Background()) }()
	r, err := NewServeSessionReader(storage.ControlStore(), storage.Launcher(), "local", "binding", "v1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.GetObjectMetadata(context.Background(), sessionstore.GetObjectMetadataRequest{}); !errors.Is(err, ErrServeObjectUnavailable) {
		t.Fatalf("object metadata = %v, want unavailable", err)
	} else {
		var objectErr *sessionstore.ObjectError
		if !errors.As(err, &objectErr) || objectErr.Code != sessionstore.ObjectErrorBackend {
			t.Fatalf("object metadata mapping = %v, want backend/unavailable", err)
		}
	}
	if _, err := r.GetObject(context.Background(), sessionstore.GetObjectRequest{}); !errors.Is(err, ErrServeObjectUnavailable) {
		t.Fatalf("object = %v, want unavailable", err)
	}
}
