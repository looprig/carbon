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
	"github.com/looprig/host"
	"github.com/looprig/sessionstore"
)

func TestServeSessionReaderReadsBoundHarnessJournal(t *testing.T) {
	ctx := context.Background()
	stores, err := OpenServeStorage(ctx, Config{}, ServeStorageConfig{DataDir: t.TempDir(), DefaultTenant: "tenant-a"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stores.Close(ctx) }()
	reader, err := NewServeSessionReader(stores.ControlStore(), stores.Launcher(), "tenant-a", "carbon-local-v1", "v1")
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
	// Factory reads the catalog under the public id and hands the resolver the
	// binding; the resolved reader is addressed by the runtime id.
	journal, err := reader.ResolveJournal(ctx, "tenant-a", publicID, entry.Binding)
	if err != nil {
		t.Fatal(err)
	}
	runtimeSession := sessionwire.SessionID(runtimeID.String())
	page, err := journal.ReadPublicJournal(ctx, sessionstore.ReadPublicJournalRequest{TenantID: "tenant-a", SessionID: runtimeSession, Limit: 1, ScanLimit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 || page.Events[0].EventID != sessionwire.EventID(events[3].EventHeader().EventID.String()) {
		t.Fatalf("public page = %+v", page)
	}
	if page.NextCursor == "" {
		t.Fatal("expected continuation cursor")
	}
	// PROJECTED (release audit R5.2 H1): the body names the public session id,
	// never the runtime one the harness journal was written under.
	if body := string(page.Events[0].Body); strings.Contains(body, runtimeID.String()) || !strings.Contains(body, string(publicID)) {
		t.Fatalf("resolved journal body is not projected to the public session: %s", body)
	}
	last, err := journal.ReadPublicJournal(ctx, sessionstore.ReadPublicJournalRequest{TenantID: "tenant-a", SessionID: runtimeSession, Tail: true, Limit: 1})
	if err != nil || len(last.Events) != 1 || last.Events[0].EventID != sessionwire.EventID(nextPublic.EventHeader().EventID.String()) {
		t.Fatalf("tail page = %+v, err %v", last, err)
	}
	// The public id is never a runtime journal address, and the legacy half
	// refuses a disposition-bound session outright.
	var bindingErr *ServeJournalBindingError
	if _, err := journal.ReadPublicJournal(ctx, sessionstore.ReadPublicJournalRequest{TenantID: "tenant-a", SessionID: publicID}); !errors.Is(err, host.ErrPublicJournalScope) {
		t.Fatalf("public id through the runtime reader = %v", err)
	}
	if _, err := reader.ReadPublicJournal(ctx, sessionstore.ReadPublicJournalRequest{TenantID: "tenant-a", SessionID: publicID}); !errors.As(err, &bindingErr) {
		t.Fatalf("legacy half = %v", err)
	}
	if err := stores.Close(ctx); err != nil {
		t.Fatal(err)
	}
	// The runtime cursor survives a reopen: it names the runtime journal, not a
	// process. (Factory wraps it as j1. and binds it to the public session.)
	reopened, err := OpenServeStorage(ctx, Config{}, ServeStorageConfig{DataDir: stores.Launcher().dataDir, DefaultTenant: "tenant-a"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close(ctx) }()
	reopenedReader, err := NewServeSessionReader(reopened.ControlStore(), reopened.Launcher(), "tenant-a", "carbon-local-v1", "v1")
	if err != nil {
		t.Fatal(err)
	}
	reopenedJournal, err := reopenedReader.ResolveJournal(ctx, "tenant-a", publicID, entry.Binding)
	if err != nil {
		t.Fatal(err)
	}
	continued, err := reopenedJournal.ReadPublicJournal(ctx,
		sessionstore.ReadPublicJournalRequest{TenantID: "tenant-a", SessionID: runtimeSession, Cursor: page.NextCursor, Limit: 1, ScanLimit: 5})
	if err != nil || len(continued.Events) != 1 || continued.Events[0].EventID != sessionwire.EventID(nextPublic.EventHeader().EventID.String()) {
		t.Fatalf("continuation after reopen = %+v, err %v", continued, err)
	}
}
