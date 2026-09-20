package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/sessionstore"
)

func TestServeCursorPreservesInnerTokenAndRejectsMalformedOrWrongScope(t *testing.T) {
	binding := sessionstore.SessionBinding{StorageBindingID: "carbon-local-v1", BindingVersion: "v1", RuntimeSessionID: "2db05411-d065-4bdc-9d19-a23d1284ffb1", ProtocolMode: sessionstore.ProtocolModeDisposition}
	inner := sessionwire.Cursor("opaque.inner+bytes/unchanged")
	wrapped, err := wrapServeCursor(inner, "tenant-a", "public-a", binding)
	if err != nil {
		t.Fatal(err)
	}
	got, err := unwrapServeCursor(wrapped, "tenant-a", "public-a", binding)
	if err != nil || got != inner {
		t.Fatalf("inner = %q, err %v; want %q", got, err, inner)
	}
	for _, tc := range []struct {
		name     string
		token    sessionwire.Cursor
		tenant   sessionwire.TenantID
		publicID sessionwire.SessionID
		binding  sessionstore.SessionBinding
	}{
		{name: "other public session", token: wrapped, tenant: "tenant-a", publicID: "public-b", binding: binding},
		{name: "other tenant", token: wrapped, tenant: "tenant-b", publicID: "public-a", binding: binding},
		{name: "other binding version", token: wrapped, tenant: "tenant-a", publicID: "public-a", binding: func() sessionstore.SessionBinding { b := binding; b.BindingVersion = "v2"; return b }()},
		{name: "malformed", token: "c1.not-a-digest.!", tenant: "tenant-a", publicID: "public-a", binding: binding},
		{name: "oversized", token: sessionwire.Cursor(strings.Repeat("x", maxServeCursorBytes+1)), tenant: "tenant-a", publicID: "public-a", binding: binding},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := unwrapServeCursor(tc.token, tc.tenant, tc.publicID, tc.binding)
			var journalErr *sessionstore.JournalError
			if !errors.As(err, &journalErr) || journalErr.Code != sessionstore.JournalErrorCursor {
				t.Fatalf("cursor refusal = %v, want JournalErrorCursor", err)
			}
		})
	}
}

func TestServeSessionReaderRejectsMissingDependenciesAndObjects(t *testing.T) {
	for _, tc := range []struct {
		name        string
		control     *sessionstore.Store
		launcher    *PooledLauncher
		id, version string
	}{
		{"nil control", nil, &PooledLauncher{}, "binding", "v1"},
		{"nil launcher", &sessionstore.Store{}, nil, "binding", "v1"},
		{"empty binding ID", &sessionstore.Store{}, &PooledLauncher{}, "", "v1"},
		{"empty binding version", &sessionstore.Store{}, &PooledLauncher{}, "binding", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewServeSessionReader(tc.control, tc.launcher, tc.id, tc.version)
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
	r, err := NewServeSessionReader(storage.ControlStore(), storage.Launcher(), "binding", "v1")
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
