package main

import (
	"context"
	"errors"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/rig"
	"github.com/looprig/harness/pkg/serve"
	"github.com/looprig/harness/pkg/session"
)

// serveRigContract is the compile-time proof that carbon's wrapper is exactly what
// serve.Handler wants — the same instantiation harness/pkg/serve/deps_test.go
// asserts for *rig.Rig itself. A drift in serve.Rig's shape breaks this file before
// it can break the composition in serve.go.
var serveRigContract serve.Rig[session.SessionController, rig.SessionOption] = (*serveRig)(nil)

// stubServeHost is the narrow serveHost double. It records what was asked of it so a
// test can prove serveRig did NOT reach the rig on the not-found path.
type stubServeHost struct {
	exists     bool
	readErr    error
	session    session.SessionController
	restoreErr error

	restored []uuid.UUID
	created  int
}

func (s *stubServeHost) NewSession(context.Context) (session.SessionController, error) {
	s.created++
	return s.session, nil
}

func (s *stubServeHost) RestoreSession(_ context.Context, id uuid.UUID) (session.SessionController, error) {
	s.restored = append(s.restored, id)
	if s.restoreErr != nil {
		return nil, s.restoreErr
	}
	return s.session, nil
}

func (s *stubServeHost) HasSession(context.Context, uuid.UUID) (bool, error) {
	if s.readErr != nil {
		return false, s.readErr
	}
	return s.exists, nil
}

// TestServeRigMapsAbsentSessionToNotFound proves a never-persisted id becomes a 404
// rather than a 500. handleRestore maps EVERY RestoreSession error except
// serve.SessionNotFoundError to 500, and rig.RestoreSession has no not-found class,
// so the mapping has to be minted here — before the rig is touched at all.
func TestServeRigMapsAbsentSessionToNotFound(t *testing.T) {
	t.Parallel()

	absent, err := uuid.New()
	if err != nil {
		t.Fatal(err)
	}
	host := &stubServeHost{exists: false}
	r := &serveRig{host: host}

	_, gotErr := r.RestoreSession(context.Background(), absent)
	var notFound serve.SessionNotFoundError
	if !errors.As(gotErr, &notFound) {
		t.Fatalf("RestoreSession(absent) err = %v, want serve.SessionNotFoundError", gotErr)
	}
	if notFound.SessionID != absent {
		t.Errorf("SessionID = %v, want %v", notFound.SessionID, absent)
	}
	if len(host.restored) != 0 {
		t.Errorf("rig was asked to restore %v; an absent session must never reach it", host.restored)
	}
}

// TestServeRigPropagatesCatalogFailure proves a FAILED existence check is not
// silently reported as "not found" — a broken store must stay a 500, and it must not
// be papered over by attempting the restore anyway.
func TestServeRigPropagatesCatalogFailure(t *testing.T) {
	t.Parallel()

	boom := errors.New("catalog unavailable")
	host := &stubServeHost{readErr: boom}
	r := &serveRig{host: host}

	_, gotErr := r.RestoreSession(context.Background(), uuid.UUID{0x1})
	var notFound serve.SessionNotFoundError
	if errors.As(gotErr, &notFound) {
		t.Fatal("a catalog read failure was reported as session-not-found")
	}
	if !errors.Is(gotErr, boom) {
		t.Fatalf("err = %v, want %v", gotErr, boom)
	}
	if len(host.restored) != 0 {
		t.Errorf("rig was asked to restore %v after a failed existence check", host.restored)
	}
}

// TestServeRigRestoresAnExistingSession proves the happy path reaches the host with
// the id it was given: the not-found mapping is a pre-flight check, not a
// replacement for the restore.
func TestServeRigRestoresAnExistingSession(t *testing.T) {
	t.Parallel()

	id, err := uuid.New()
	if err != nil {
		t.Fatal(err)
	}
	host := &stubServeHost{exists: true}
	r := &serveRig{host: host}

	if _, err := r.RestoreSession(context.Background(), id); err != nil {
		t.Fatalf("RestoreSession(existing) err = %v", err)
	}
	if len(host.restored) != 1 || host.restored[0] != id {
		t.Fatalf("host.restored = %v, want exactly [%v]", host.restored, id)
	}
}

// TestServeRigPropagatesRestoreFailure proves a genuine restore failure is NOT
// rewritten into a 404: the session exists, so "not found" would be a lie and would
// hide a real fault (a config-fingerprint mismatch, a busy workspace lease) behind a
// benign-looking status.
func TestServeRigPropagatesRestoreFailure(t *testing.T) {
	t.Parallel()

	boom := errors.New("workspace root busy")
	r := &serveRig{host: &stubServeHost{exists: true, restoreErr: boom}}

	_, gotErr := r.RestoreSession(context.Background(), uuid.UUID{0x2})
	var notFound serve.SessionNotFoundError
	if errors.As(gotErr, &notFound) {
		t.Fatal("a restore failure on an EXISTING session was reported as session-not-found")
	}
	if !errors.Is(gotErr, boom) {
		t.Fatalf("err = %v, want %v", gotErr, boom)
	}
}

// TestServeRigNewSessionIsAPassThrough proves the adapter adds nothing on the create
// path. The variadic rig.SessionOption is part of serve.Rig's generic contract;
// carbon supplies none, and the adapter must not invent any.
func TestServeRigNewSessionIsAPassThrough(t *testing.T) {
	t.Parallel()

	host := &stubServeHost{}
	r := &serveRig{host: host}

	if _, err := r.NewSession(context.Background()); err != nil {
		t.Fatalf("NewSession err = %v", err)
	}
	if host.created != 1 {
		t.Fatalf("host.created = %d, want 1", host.created)
	}
}

// stubSession is a session.SessionController double that also satisfies
// serve.SessionDone. Embedding the interface supplies the dozen methods this test
// never calls; only Done() carries meaning here.
type stubSession struct {
	session.SessionController
	done chan struct{}
}

func (s *stubSession) Done() <-chan struct{} { return s.done }

// TestServeRigReturnsTheHostSessionUnwrapped is the eviction canary. harness's live
// registry evicts a dead session by type-asserting the DYNAMIC value it was handed to
// serve.SessionDone and watching Done(). A wrapper inserted anywhere in this adapter
// that failed to forward Done would silently opt every session out of eviction,
// pinning a subscription and a goroutine per dead session — and nothing else in the
// suite would notice. So this asserts identity, not merely behaviour.
func TestServeRigReturnsTheHostSessionUnwrapped(t *testing.T) {
	t.Parallel()

	live := &stubSession{done: make(chan struct{})}
	host := &stubServeHost{exists: true, session: live}
	r := &serveRig{host: host}

	created, err := r.NewSession(context.Background())
	if err != nil {
		t.Fatalf("NewSession err = %v", err)
	}
	if created != session.SessionController(live) {
		t.Errorf("NewSession returned %T, not the host's own controller", created)
	}
	restored, err := r.RestoreSession(context.Background(), uuid.UUID{0x3})
	if err != nil {
		t.Fatalf("RestoreSession err = %v", err)
	}
	if restored != session.SessionController(live) {
		t.Errorf("RestoreSession returned %T, not the host's own controller", restored)
	}
	for name, got := range map[string]session.SessionController{"NewSession": created, "RestoreSession": restored} {
		if _, ok := got.(serve.SessionDone); !ok {
			t.Errorf("%s result does not satisfy serve.SessionDone; the registry will never evict it", name)
		}
	}
}
