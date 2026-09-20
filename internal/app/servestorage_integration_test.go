//go:build integration

package app

import (
	"context"
	"errors"
	"testing"

	"github.com/looprig/core/content"
)

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
}
