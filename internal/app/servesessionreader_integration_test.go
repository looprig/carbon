//go:build integration

package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/harness/pkg/journal"
	"github.com/looprig/sessionstore"
)

func TestServeSessionReaderReadsBoundHarnessJournal(t *testing.T) {
	ctx := context.Background()
	stores, err := OpenServeStorage(ctx, Config{}, ServeStorageConfig{DataDir: t.TempDir(), DefaultTenant: "tenant-a"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stores.Close(ctx) }()
	reader, err := NewServeSessionReader(stores.ControlStore(), stores.Launcher(), "carbon-local-v1", "v1")
	if err != nil {
		t.Fatal(err)
	}
	runtimeID, loopID := mustUUID(t), mustUUID(t)
	const publicID sessionwire.SessionID = "public-session"
	entry := sessionstore.CreateCatalogEntryRequest{
		TenantID: "tenant-a", SessionID: publicID, AgentID: "carbon", RuntimeCompatibilityID: "carbon-v1",
		Binding:   sessionstore.SessionBinding{StorageBindingID: "carbon-local-v1", BindingVersion: "v1", RuntimeSessionID: runtimeID.String(), ProtocolMode: sessionstore.ProtocolModeDisposition},
		CreatedAt: time.Now().UTC(), LastActiveAt: time.Now().UTC(), State: sessionwire.SessionStateIdle,
		Residency: sessionwire.SessionResidencyCold, DesiredPlacement: sessionwire.HostPlacementPooled,
	}
	if _, _, err := stores.ControlStore().CreateCatalogEntry(ctx, entry); err != nil {
		t.Fatal(err)
	}
	harnessJournal, err := stores.Launcher().JournalStoreForTenant("tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	lease, err := harnessJournal.AcquireLease(ctx, runtimeID)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := harnessJournal.OpenJournal(ctx, runtimeID, lease)
	if err != nil {
		t.Fatal(err)
	}
	events := persistedVisibilityEvents(t, runtimeID, loopID)
	nextPublic := persistedVisibilityEvents(t, runtimeID, loopID)[3]
	for _, ev := range events {
		if _, err := writer.Append(ctx, journal.NewEventRecord(ev)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := writer.Append(ctx, journal.NewEventRecord(nextPublic)); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(ctx); err != nil {
		t.Fatal(err)
	}
	page, err := reader.ReadPublicJournal(ctx, sessionstore.ReadPublicJournalRequest{TenantID: "tenant-a", SessionID: publicID, Limit: 1, ScanLimit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 || page.Events[0].EventID != sessionwire.EventID(events[3].EventHeader().EventID.String()) {
		t.Fatalf("public page = %+v", page)
	}
	if page.NextCursor == "" {
		t.Fatal("expected continuation cursor")
	}
	last, err := reader.ReadPublicJournal(ctx, sessionstore.ReadPublicJournalRequest{TenantID: "tenant-a", SessionID: publicID, Tail: true, Limit: 1})
	if err != nil || len(last.Events) != 1 || last.Events[0].EventID != sessionwire.EventID(nextPublic.EventHeader().EventID.String()) {
		t.Fatalf("tail page = %+v, err %v", last, err)
	}
	other := entry
	other.SessionID = "other"
	if _, _, err := stores.ControlStore().CreateCatalogEntry(ctx, other); err != nil {
		t.Fatal(err)
	}
	otherTenant := entry
	otherTenant.TenantID = "tenant-b"
	if _, _, err := stores.ControlStore().CreateCatalogEntry(ctx, otherTenant); err != nil {
		t.Fatal(err)
	}
	_, err = reader.ReadPublicJournal(ctx, sessionstore.ReadPublicJournalRequest{TenantID: "tenant-b", SessionID: publicID, Cursor: page.NextCursor})
	assertCursorError(t, err)
	_, err = reader.ReadPublicJournal(ctx, sessionstore.ReadPublicJournalRequest{TenantID: "tenant-a", SessionID: "other", Cursor: page.NextCursor})
	assertCursorError(t, err)
	for _, tc := range []struct {
		id     sessionwire.SessionID
		change func(*sessionstore.SessionBinding)
	}{
		{"wrong-binding-id", func(b *sessionstore.SessionBinding) { b.StorageBindingID = "other-binding" }},
		{"wrong-binding-version", func(b *sessionstore.SessionBinding) { b.BindingVersion = "v2" }},
		{"wrong-binding-protocol", func(b *sessionstore.SessionBinding) { b.ProtocolMode = sessionstore.ProtocolModeLegacy }},
		{"zero-runtime-id", func(b *sessionstore.SessionBinding) { b.RuntimeSessionID = "00000000-0000-0000-0000-000000000000" }},
		{"malformed-runtime-id", func(b *sessionstore.SessionBinding) { b.RuntimeSessionID = "not-a-uuid" }},
		{"noncanonical-runtime-id", func(b *sessionstore.SessionBinding) { b.RuntimeSessionID = strings.ToUpper(runtimeID.String()) }},
	} {
		wrong := entry
		wrong.SessionID = tc.id
		tc.change(&wrong.Binding)
		if _, _, err := stores.ControlStore().CreateCatalogEntry(ctx, wrong); err != nil {
			t.Fatal(err)
		}
		_, err = reader.ReadPublicJournal(ctx, sessionstore.ReadPublicJournalRequest{TenantID: "tenant-a", SessionID: wrong.SessionID})
		var bindingErr *ServeJournalBindingError
		if !errors.As(err, &bindingErr) {
			t.Fatalf("%s: %v", tc.id, err)
		}
	}
	if err := stores.Close(ctx); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenServeStorage(ctx, Config{}, ServeStorageConfig{DataDir: stores.Launcher().dataDir, DefaultTenant: "tenant-a"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close(ctx) }()
	reopenedReader, err := NewServeSessionReader(reopened.ControlStore(), reopened.Launcher(), "carbon-local-v1", "v1")
	if err != nil {
		t.Fatal(err)
	}
	continued, err := reopenedReader.ReadPublicJournal(ctx,
		sessionstore.ReadPublicJournalRequest{TenantID: "tenant-a", SessionID: publicID, Cursor: page.NextCursor, Limit: 1, ScanLimit: 5})
	if err != nil || len(continued.Events) != 1 || continued.Events[0].EventID != sessionwire.EventID(nextPublic.EventHeader().EventID.String()) {
		t.Fatalf("continuation after reopen = %+v, err %v", continued, err)
	}
}

func assertCursorError(t *testing.T, err error) {
	t.Helper()
	var journalErr *sessionstore.JournalError
	if !errors.As(err, &journalErr) || journalErr.Code != sessionstore.JournalErrorCursor {
		t.Fatalf("cursor refusal = %v, want JournalErrorCursor", err)
	}
}
