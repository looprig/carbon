package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/fsstore"
)

// seedPreV060Root plants what fsstore <= v0.5.x left behind: a KV value stored
// as a plain, unsuffixed file under kv/. fsstore v0.6.0 refuses such a root.
func seedPreV060Root(t *testing.T, root string) {
	t.Helper()
	dir := filepath.Join(root, "kv", "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "0f1e2d3c-4b5a-4968-8776-655443322110"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// assertLegacyDataRoot requires the one actionable refusal: typed, naming the
// root, telling the user to move or delete it, and still classifiable as both
// Carbon's store-init failure and fsstore's legacy-layout sentinel.
func assertLegacyDataRoot(t *testing.T, err error, root, stage string) {
	t.Helper()
	var legacy *LegacyDataRootError
	if !errors.As(err, &legacy) {
		t.Fatalf("err = %v, want a *LegacyDataRootError", err)
	}
	if legacy.Root != root {
		t.Fatalf("LegacyDataRootError.Root = %q, want %q", legacy.Root, root)
	}
	message := err.Error()
	for _, want := range []string{root, "move or delete this directory", "pre-v0.6.0 data is not migrated"} {
		if !strings.Contains(message, want) {
			t.Fatalf("message %q does not contain %q", message, want)
		}
	}
	if !errors.Is(err, fsstore.ErrLegacyLayout) {
		t.Fatalf("err = %v does not match fsstore.ErrLegacyLayout", err)
	}
	var initErr *StoreInitError
	if !errors.As(err, &initErr) || initErr.Stage != stage {
		t.Fatalf("err = %v, want a StoreInitError at stage %q", err, stage)
	}
}

func TestSessionStoreFactoryRefusesAPreV060DataDir(t *testing.T) {
	root := t.TempDir()
	seedPreV060Root(t, root)
	factory, err := NewSessionStoreFactory(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = factory.Close() })
	_, err = factory.List(context.Background())
	assertLegacyDataRoot(t, err, root, "fsstore")
}

func TestOpenServeStorageRefusesAPreV060DataDir(t *testing.T) {
	root := t.TempDir()
	seedPreV060Root(t, root)
	stores, err := OpenServeStorage(context.Background(), Config{HomeDir: t.TempDir()}, ServeStorageConfig{DataDir: root, DefaultTenant: "local"})
	if stores != nil {
		_ = stores.Close(context.Background())
	}
	assertLegacyDataRoot(t, err, root, "control-fsstore")
	// Not conflated with Carbon's own SessionStore-layout refusal.
	if errors.As(err, new(*ServeLegacyCompatibilityError)) {
		t.Fatalf("an fsstore layout refusal reads as the store-layout refusal: %v", err)
	}
}

// A tenant root written before fsstore v0.6.0 is refused PERMANENTLY: the
// failed bundle is kept, so the refusal is never retried as a transient fault
// (the next call does not reopen the root even once its contents change).
func TestPooledLauncherRefusesAPreV060TenantRootPermanently(t *testing.T) {
	ctx := context.Background()
	data := t.TempDir()
	launcher, err := OpenPooledLauncher(ctx, Config{HomeDir: t.TempDir()}, data)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = launcher.Close(ctx) })
	const tenant = sessionwire.TenantID("legacy-tenant")
	sum := sha256.Sum256([]byte(tenantJournalDigestDomain + "\x00" + string(tenant)))
	root := filepath.Join(data, "tenant-journals", hex.EncodeToString(sum[:]))
	seedPreV060Root(t, root)

	_, err = launcher.JournalStoreForTenant(tenant)
	assertLegacyDataRoot(t, err, root, "tenant-fsstore")

	// Clearing the root would let a retry succeed; a permanent refusal does
	// not retry.
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	_, again := launcher.JournalStoreForTenant(tenant)
	assertLegacyDataRoot(t, again, root, "tenant-fsstore")
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("the refused tenant root was reopened (stat err %v)", err)
	}

	// Another tenant is unaffected.
	if _, err := launcher.JournalStoreForTenant("fresh-tenant"); err != nil {
		t.Fatalf("a fresh tenant: %v", err)
	}
}

// An ordinary open failure stays transient: the bundle is dropped and the
// next call reopens.
func TestPooledLauncherRetriesATransientTenantOpenFailure(t *testing.T) {
	ctx := context.Background()
	data := t.TempDir()
	launcher, err := OpenPooledLauncher(ctx, Config{HomeDir: t.TempDir()}, data)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = launcher.Close(ctx) })
	const tenant = sessionwire.TenantID("flaky-tenant")
	journals := filepath.Join(data, "tenant-journals")
	// A regular file where the tenant-journals directory belongs makes the
	// open fail for a reason that is not the legacy layout.
	if err := os.WriteFile(journals, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = launcher.JournalStoreForTenant(tenant)
	if err == nil || errors.As(err, new(*LegacyDataRootError)) {
		t.Fatalf("blocked open = %v, want an ordinary open failure", err)
	}
	if err := os.Remove(journals); err != nil {
		t.Fatal(err)
	}
	if _, err := launcher.JournalStoreForTenant(tenant); err != nil {
		t.Fatalf("retry after the transient cause cleared: %v", err)
	}
}
