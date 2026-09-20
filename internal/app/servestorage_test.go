package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/sessionstore"
)

func TestServeStorageDefaultsToTenantLayoutAndRegistersDefaultJournal(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	storage, err := OpenServeStorage(ctx, Config{}, ServeStorageConfig{DataDir: root, DefaultTenant: "local"})
	if err != nil {
		t.Fatal(err)
	}
	journal := storage.DefaultJournalStore()
	if journal == nil {
		t.Fatal("default tenant journal was not opened before Host composition")
	}
	if got, err := storage.Launcher().JournalStoreForTenant("local"); err != nil || got != journal {
		t.Fatalf("launcher journal=%p err=%v, want bootstrap journal=%p", got, err, journal)
	}
	if err := storage.Close(ctx); err != nil {
		t.Fatal(err)
	}
	_, err = OpenServeStorage(ctx, Config{}, ServeStorageConfig{DataDir: root, DefaultTenant: "local", Layout: ServeStoreLayoutLegacySingleTenant})
	var refused *ServeLegacyCompatibilityError
	if !errors.As(err, &refused) {
		t.Fatalf("legacy browser composition: %v, want typed refusal", err)
	}
}

func TestServeStorageLegacyIsRefusedBeforeMarkingFreshRoot(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	_, err := OpenServeStorage(ctx, Config{}, ServeStorageConfig{DataDir: root, DefaultTenant: "local", Layout: ServeStoreLayoutLegacySingleTenant})
	var refused *ServeLegacyCompatibilityError
	if !errors.As(err, &refused) {
		t.Fatalf("legacy browser composition: %v, want typed refusal", err)
	}
	storage, err := OpenServeStorage(ctx, Config{}, ServeStorageConfig{DataDir: root, DefaultTenant: "local"})
	if err != nil {
		t.Fatalf("legacy refusal wrote a marker into fresh root: %v", err)
	}
	if err := storage.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestServeStorageRejectsInvalidConfigBeforeMarkerWrite(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	for _, config := range []ServeStorageConfig{
		{DataDir: root, DefaultTenant: ""},
		{DataDir: root, DefaultTenant: "local", Layout: "typo"},
		{DataDir: "relative", DefaultTenant: "local"},
	} {
		if storage, err := OpenServeStorage(ctx, Config{}, config); err == nil {
			_ = storage.Close(ctx)
			t.Fatalf("invalid config %+v was admitted", config)
		}
	}
	// Invalid bootstrap attempts must not pin a legacy marker or produce an
	// empty-catalog migration when a valid tenant-v1 bootstrap follows.
	storage, err := OpenServeStorage(ctx, Config{}, ServeStorageConfig{DataDir: root, DefaultTenant: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestServeStorageClosesControlAndTenantResources(t *testing.T) {
	ctx := context.Background()
	storage, err := OpenServeStorage(ctx, Config{}, ServeStorageConfig{DataDir: t.TempDir(), DefaultTenant: sessionwire.TenantID("local")})
	if err != nil {
		t.Fatal(err)
	}
	control := storage.ControlStore()
	if err := storage.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := storage.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Launcher().JournalStoreForTenant("local"); err == nil {
		t.Fatal("closed launcher issued journal")
	}
	_, err = control.ListSessions(ctx, sessionstore.ListSessionsRequest{TenantID: "local", Limit: 1})
	if err == nil {
		t.Fatal("closed control store accepted read")
	}
}

func TestServeStorageRestartKeepsControlRecordAndTenantJournal(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	selected := ServeStorageConfig{DataDir: root, DefaultTenant: "local"}
	first, err := OpenServeStorage(ctx, Config{}, selected)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	_, _, err = first.ControlStore().CreateCatalogEntry(ctx, sessionstore.CreateCatalogEntryRequest{
		TenantID: "local", SessionID: "session-control-restart", AgentID: "carbon",
		RuntimeCompatibilityID: "carbon-v1", CreatedAt: now, LastActiveAt: now,
		State: sessionwire.SessionStateIdle, Residency: sessionwire.SessionResidencyCold,
		DesiredPlacement: sessionwire.HostPlacementPooled,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(ctx); err != nil {
		t.Fatal(err)
	}
	second, err := OpenServeStorage(ctx, Config{}, selected)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close(ctx) }()
	entry, err := second.ControlStore().GetCatalogEntry(ctx, sessionstore.GetCatalogEntryRequest{TenantID: "local", SessionID: "session-control-restart"})
	if err != nil || entry.Record.SessionID != "session-control-restart" {
		t.Fatalf("control record after restart: %+v %v", entry, err)
	}
	if second.DefaultJournalStore() == nil {
		t.Fatal("default tenant journal did not reopen")
	}
}

func TestServeStorageJournalFailureClosesEarlierStages(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	obstacle := filepath.Join(root, "tenant-journals")
	if err := os.WriteFile(obstacle, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	selected := ServeStorageConfig{DataDir: root, DefaultTenant: "local"}
	_, err := OpenServeStorage(ctx, Config{}, selected)
	var init *StoreInitError
	if !errors.As(err, &init) || init.Stage != "default-tenant-journal" {
		t.Fatalf("journal obstacle: %v, want default-tenant-journal init error", err)
	}
	if err := os.Remove(obstacle); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenServeStorage(ctx, Config{}, selected)
	if err != nil {
		t.Fatalf("reopen after journal failure: %v", err)
	}
	if err := reopened.Close(ctx); err != nil {
		t.Fatal(err)
	}
}
