//go:build integration

package app

import (
	"context"
	"errors"
	"testing"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/journal"
	harnessstore "github.com/looprig/harness/pkg/sessionstore"
)

func TestServeStorageRestartReplaysDefaultTenantJournal(t *testing.T) {
	ctx := context.Background()
	selected := ServeStorageConfig{DataDir: t.TempDir(), DefaultTenant: "local"}
	first, err := OpenServeStorage(ctx, Config{}, selected)
	if err != nil {
		t.Fatal(err)
	}
	sessionID, loopID := mustUUID(t), mustUUID(t)
	events := persistedVisibilityEvents(t, sessionID, loopID)
	lease, err := first.DefaultJournalStore().AcquireLease(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := first.DefaultJournalStore().OpenJournal(ctx, sessionID, lease)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Append(ctx, journal.NewEventRecord(events[0])); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(ctx); err != nil {
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
	replayer, err := second.DefaultJournalStore().OpenInternalEventReplayer(sessionID, harnessstore.ReplayRequest{})
	if err != nil {
		t.Fatal(err)
	}
	got := drainEventReplay(t, replayer)
	if len(got) != 1 || got[0].EventHeader().EventID != events[0].EventHeader().EventID {
		t.Fatalf("journal replay after restart: %#v, want event %s", got, events[0].EventHeader().EventID)
	}
}

// A real historical Carbon root is a Harness journal and a legacy
// SessionStore marker at the configured data root. Refusing tenant-v1 here is
// what prevents a silently empty catalog during browser cutover.
func TestServeStorageRefusesRealLegacyCarbonRootWithoutLosingItsSession(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	t.Chdir(t.TempDir())
	old, err := NewSessionStoreFactory(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.List(ctx); err != nil {
		t.Fatal(err)
	}
	opened, err := old.openWithClient(ctx, &fakeLLM{chunks: []content.Chunk{textChunk("legacy")}}, newModelFactory(), SessionSelector{}, Config{})
	if err != nil {
		t.Fatal(err)
	}
	id := opened.SessionID()
	if err := opened.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = OpenServeStorage(ctx, Config{}, ServeStorageConfig{DataDir: root, DefaultTenant: "local"})
	var mismatch *ServeStoreLayoutMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("tenant-v1 opened real legacy root: %v", err)
	}
	_, err = OpenServeStorage(ctx, Config{}, ServeStorageConfig{DataDir: root, DefaultTenant: "local", Layout: ServeStoreLayoutLegacySingleTenant})
	var refused *ServeLegacyCompatibilityError
	if !errors.As(err, &refused) {
		t.Fatalf("legacy browser composition was accepted: %v", err)
	}

	reopened, err := NewSessionStoreFactory(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	metas, err := reopened.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 1 || metas[0].SessionID != id {
		t.Fatalf("legacy session after refused migration: %+v, want %s", metas, id)
	}
	// Listing reads only the catalog index. The TUI/headless path must still
	// RESTORE the session, which replays its journal under a fresh lease.
	resumed, err := reopened.openWithClient(ctx, &fakeLLM{}, newModelFactory(), SessionSelector{Resume: id}, Config{})
	if err != nil {
		t.Fatalf("resume legacy session after refused browser serve: %v", err)
	}
	if resumed.SessionID() != id {
		t.Fatalf("resumed session %s, want %s", resumed.SessionID(), id)
	}
	if err := resumed.Close(ctx); err != nil {
		t.Fatal(err)
	}
}
