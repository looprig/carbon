package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/fsstore"
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

type heldControlCloser struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (c *heldControlCloser) Close(context.Context) error {
	c.once.Do(func() { close(c.entered) })
	<-c.release
	return nil
}

func TestServeStorageCancelledCloseKeepsProviderUntilControlCompletes(t *testing.T) {
	ctx := context.Background()
	s, err := OpenServeStorage(ctx, Config{}, ServeStorageConfig{DataDir: t.TempDir(), DefaultTenant: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.control.Close(ctx); err != nil {
		t.Fatal(err)
	}
	backend := *s.controlFS.Backend()
	backend.Blobs = newBoundedBlobs(backend.Blobs)
	held := &heldControlCloser{entered: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(held.release) }) })
	s.control, err = sessionstore.Open(ctx, &backend, sessionstore.WithProviderOwnership(held))
	if err != nil {
		t.Fatal(err)
	}
	providerClosed := make(chan struct{})
	s.closeProvider = func() error { close(providerClosed); return s.controlFS.Close() }
	closeCtx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if err := s.Close(closeCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timed Close=%v, want caller deadline", err)
	}
	select {
	case <-held.entered:
	case <-time.After(time.Second):
		t.Fatal("control close did not start")
	}
	select {
	case <-providerClosed:
		t.Fatal("provider closed before control completed")
	default:
	}
	second := make(chan error, 1)
	go func() { second <- s.Close(ctx) }()
	select {
	case err := <-second:
		t.Fatalf("later Close returned before cleanup: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(held.release) })
	select {
	case err := <-second:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("later Close did not finish")
	}
	select {
	case <-providerClosed:
	default:
		t.Fatal("provider not closed after control")
	}
}

func TestServeStorageFailedInitWaitsForControlBeforeProviderClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	fs, err := fsstore.Open(fsstore.Options{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	backend := *fs.Backend()
	backend.Blobs = newBoundedBlobs(backend.Blobs)
	held := &heldControlCloser{entered: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(held.release) }) })
	control, err := sessionstore.Open(ctx, &backend, sessionstore.WithProviderOwnership(held))
	if err != nil {
		t.Fatal(err)
	}
	providerClosed := make(chan struct{})
	cancel() // Initialization failed after control opened.
	done := make(chan error, 1)
	go func() {
		done <- closeServeStorageResources(nil, control, func() error { close(providerClosed); return fs.Close() })
	}()
	select {
	case <-held.entered:
	case <-time.After(time.Second):
		t.Fatal("control cleanup did not start")
	}
	select {
	case <-providerClosed:
		t.Fatal("provider closed before control cleanup")
	default:
	}
	select {
	case err := <-done:
		t.Fatalf("failed init cleanup returned early: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(held.release) })
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("failed init cleanup did not finish")
	}
	select {
	case <-providerClosed:
	default:
		t.Fatal("provider was not closed")
	}
}

func TestServeStorageCloseReturnsStableCleanupError(t *testing.T) {
	ctx := context.Background()
	s, err := OpenServeStorage(ctx, Config{}, ServeStorageConfig{DataDir: t.TempDir(), DefaultTenant: "local"})
	if err != nil {
		t.Fatal(err)
	}
	providerClose := s.closeProvider
	want := errors.New("provider close failed")
	s.closeProvider = func() error { return errors.Join(providerClose(), want) }
	for i := 0; i < 2; i++ {
		if err := s.Close(ctx); !errors.Is(err, want) {
			t.Fatalf("Close %d = %v, want stable provider error", i, err)
		}
	}
}

func TestServeStorageJournalFailureAfterInitCancellationCanReopen(t *testing.T) {
	root := t.TempDir()
	obstacle := filepath.Join(root, "tenant-journals")
	if err := os.WriteFile(obstacle, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	selected := ServeStorageConfig{DataDir: root, DefaultTenant: "local"}
	_, err := OpenServeStorage(ctx, Config{}, selected, func(*serveHostConfig) { cancel() })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled initialization: %v, want context cancellation", err)
	}
	if err := os.Remove(obstacle); err != nil {
		t.Fatal(err)
	}
	s, err := OpenServeStorage(context.Background(), Config{}, selected)
	if err != nil {
		t.Fatalf("reopen after cancelled initialization: %v", err)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestServeStorageNeverPublishesCancelledControlStore(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	selected := ServeStorageConfig{DataDir: root, DefaultTenant: "local"}
	s, err := OpenServeStorage(ctx, Config{}, selected, func(*serveHostConfig) { cancel() })
	if s != nil || !errors.Is(err, context.Canceled) {
		if s != nil {
			_ = s.Close(context.Background())
		}
		t.Fatalf("cancelled bootstrap returned owner=%v err=%v", s != nil, err)
	}
	reopened, err := OpenServeStorage(context.Background(), Config{}, selected)
	if err != nil {
		t.Fatalf("reopen after cancellation: %v", err)
	}
	if err := reopened.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}
