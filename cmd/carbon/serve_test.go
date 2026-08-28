package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	carbon "github.com/looprig/carbon/internal/app"
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

// stubHandoffHost is the handoffHost double backing the /ui/* route tests.
type stubHandoffHost struct {
	live     uuid.UUID
	hasLive  bool
	closeErr error

	closed []uuid.UUID
}

func (s *stubHandoffHost) LiveSession() (uuid.UUID, bool) { return s.live, s.hasLive }

func (s *stubHandoffHost) CloseLive(_ context.Context, expected uuid.UUID) error {
	s.closed = append(s.closed, expected)
	if s.closeErr != nil {
		return s.closeErr
	}
	if s.hasLive && s.live != expected {
		return &carbon.LiveSessionHandoffError{LiveID: s.live}
	}
	s.hasLive = false
	return nil
}

// testServeHandler builds the production composition over stubs.
func testServeHandler(t *testing.T, host *stubHandoffHost) http.Handler {
	t.Helper()
	return buildServeHandler(&serveRig{host: &stubServeHost{}}, nil, host)
}

// doServe issues req against h and returns the recorded response. Host defaults to a
// loopback value because every request the guard admits must carry one.
func doServe(t *testing.T, h http.Handler, method, target string, body io.Reader, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, body)
	req.Host = "127.0.0.1:8722"
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestServeHandlerBindsLoopback covers exactly one thing: serve.Server accepts the
// composed handler for a loopback address, so `carbon serve`'s default bind is not
// refused by the fail-secure guard.
//
// It deliberately does NOT claim to prove authAware forwarding. That is impossible to
// assert from this package and impossible to satisfy from wui: Go qualifies an
// unexported method name by its DECLARING package, so a wui.authInstalled method is a
// different identifier from serve.authInstalled and serve's assertion would still
// fail (harness's comment at server.go:116 is wrong on this point, and wui's
// Handler doc records the empirical verification). The consequence is bounded and
// benign — the wrapper can only ever cause a false-negative refusal of a PUBLIC bind,
// never a false-positive exposure — and carbon binds loopback.
func TestServeHandlerBindsLoopback(t *testing.T) {
	t.Parallel()

	h := testServeHandler(t, &stubHandoffHost{})
	if _, err := serve.Server("127.0.0.1:0", h); err != nil {
		t.Fatalf("loopback bind refused: %v", err)
	}
}

// TestServeServerRefusesPublicBindWithoutAuth pins the guard carbon inherits by
// passing the composed handler to serve.Server directly rather than building an
// http.Server by hand. Carbon installs no authenticator, so every non-loopback bind
// must be refused.
func TestServeServerRefusesPublicBindWithoutAuth(t *testing.T) {
	t.Parallel()

	h := testServeHandler(t, &stubHandoffHost{})
	for _, addr := range []string{"0.0.0.0:8722", ":8722", "192.0.2.1:8722"} {
		if _, err := serve.Server(addr, h); err == nil {
			t.Errorf("serve.Server(%q) accepted a public bind with no auth; the fail-secure guard is not in play", addr)
		}
	}
}

// TestServeHandlerGuardsEveryRouteAgainstDNSRebinding is the load-bearing assertion
// of the composition. Loopback binding alone does not stop DNS rebinding, and behind
// this endpoint sits a fully-permissioned coding agent — so the Host/Origin guard has
// to cover the SPA, the forwarded /v1 API AND carbon's own /ui/* routes. Guarding
// only the wui subtree would leave /ui/handoff reachable from a rebound page.
func TestServeHandlerGuardsEveryRouteAgainstDNSRebinding(t *testing.T) {
	t.Parallel()

	h := testServeHandler(t, &stubHandoffHost{})
	for _, target := range []string{"/", "/v1/sessions", "/ui/live", "/ui/handoff"} {
		rec := doServe(t, h, http.MethodGet, target, nil, func(r *http.Request) { r.Host = "attacker.example" })
		if rec.Code != http.StatusForbidden {
			t.Errorf("GET %s with a foreign Host = %d, want %d", target, rec.Code, http.StatusForbidden)
		}
		rec = doServe(t, h, http.MethodGet, target, nil, func(r *http.Request) {
			r.Header.Set("Origin", "https://attacker.example")
		})
		if rec.Code != http.StatusForbidden {
			t.Errorf("GET %s with a foreign Origin = %d, want %d", target, rec.Code, http.StatusForbidden)
		}
	}
}

// TestServeHandlerServesTheBuiltSPA proves / reaches wui's embedded bundle and that
// the bundle is the REAL one. wui v0.1.0 was tagged from a development tree and ships
// only the placeholder; go.mod's pin is checked statically elsewhere, and this is the
// runtime half of the same guarantee.
func TestServeHandlerServesTheBuiltSPA(t *testing.T) {
	t.Parallel()

	rec := doServe(t, testServeHandler(t, &stubHandoffHost{}), http.MethodGet, "/", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(strings.ToLower(body), "<!doctype html") {
		t.Errorf("GET / body is not an HTML document: %.120q", body)
	}
	if strings.Contains(body, "build the app to replace this placeholder") {
		t.Error("GET / served wui's placeholder; the pinned wui release does not carry the built bundle")
	}
}

// TestServeHandlerForwardsTheHarnessAPI proves /v1 reaches serve.Handler rather than
// falling through to the SPA catch-all, and that wui's own CSRF token route is
// mounted — without it every state-changing request from the SPA is a permanent 403.
func TestServeHandlerForwardsTheHarnessAPI(t *testing.T) {
	t.Parallel()

	h := testServeHandler(t, &stubHandoffHost{})

	rec := doServe(t, h, http.MethodGet, "/v1/capabilities", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/capabilities = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("GET /v1/capabilities Content-Type = %q, want JSON; the SPA catch-all swallowed the route", ct)
	}

	rec = doServe(t, h, http.MethodGet, "/v1/csrf-token", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/csrf-token = %d, want 200", rec.Code)
	}
	var token struct {
		Token string `json:"csrf_token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &token); err != nil {
		t.Fatalf("decode csrf token: %v (body %q)", err, rec.Body.String())
	}
	if token.Token == "" {
		t.Errorf("csrf token route returned no token (body %q)", rec.Body.String())
	}
}

// TestServeHandlerCSRFProtectsTheHarnessControlRoutes proves the composition keeps
// wui's per-route CSRF rather than mounting serve.Handler raw. The origin guard fails
// OPEN on a missing Origin header and harness enforces no content type, so without
// this a bodiless cross-site form POST would reach POST /v1/sessions.
func TestServeHandlerCSRFProtectsTheHarnessControlRoutes(t *testing.T) {
	t.Parallel()

	rec := doServe(t, testServeHandler(t, &stubHandoffHost{}), http.MethodPost, "/v1/sessions", nil, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST /v1/sessions without a CSRF token = %d, want 403", rec.Code)
	}
	if code := errorCode(t, rec); code != "csrf_invalid" {
		t.Errorf("error code = %q, want csrf_invalid", code)
	}
}

// TestUILiveReportsTheLiveSession covers the read half of the confirmed handoff.
// It is a CARBON-owned route outside /v1 on purpose: harness maps every
// RestoreSession error that is not serve.SessionNotFoundError to a bare 500, so
// without this route a client cannot tell "busy with another session" from "broken".
func TestUILiveReportsTheLiveSession(t *testing.T) {
	t.Parallel()

	id, err := uuid.New()
	if err != nil {
		t.Fatal(err)
	}
	live := struct {
		Live      bool   `json:"live"`
		SessionID string `json:"session_id,omitempty"`
	}{}

	rec := doServe(t, testServeHandler(t, &stubHandoffHost{live: id, hasLive: true}), http.MethodGet, "/ui/live", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /ui/live = %d, want 200", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &live); err != nil {
		t.Fatalf("decode: %v (body %q)", err, rec.Body.String())
	}
	if !live.Live || live.SessionID != id.String() {
		t.Fatalf("live = %+v, want live=true session_id=%s", live, id)
	}

	live = struct {
		Live      bool   `json:"live"`
		SessionID string `json:"session_id,omitempty"`
	}{}
	rec = doServe(t, testServeHandler(t, &stubHandoffHost{}), http.MethodGet, "/ui/live", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /ui/live (idle) = %d, want 200", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &live); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if live.Live || live.SessionID != "" {
		t.Fatalf("idle host reported %+v, want live=false and no session id", live)
	}
}

// TestUIHandoffIsConfirmedNotSilent covers the write half. The handoff must name the
// session it is ending: a bare "close whatever is live" would let a confirmation that
// raced another tab kill the wrong session mid-turn.
func TestUIHandoffIsConfirmedNotSilent(t *testing.T) {
	t.Parallel()

	id, err := uuid.New()
	if err != nil {
		t.Fatal(err)
	}
	other, err := uuid.New()
	if err != nil {
		t.Fatal(err)
	}

	host := &stubHandoffHost{live: id, hasLive: true}
	h := testServeHandler(t, host)

	// A confirmation naming a DIFFERENT session is refused, and is refused
	// distinguishably: 409, not the bare 500 harness would produce.
	rec := doServe(t, h, http.MethodPost, "/ui/handoff", strings.NewReader(`{"session_id":"`+other.String()+`"}`), jsonBody)
	if rec.Code != http.StatusConflict {
		t.Fatalf("mismatched handoff = %d, want 409 (body %q)", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "live_session_mismatch" {
		t.Errorf("error code = %q, want live_session_mismatch", code)
	}
	if host.hasLive != true {
		t.Error("a mismatched confirmation closed the live session anyway")
	}

	// The confirmation that names the live session closes it.
	rec = doServe(t, h, http.MethodPost, "/ui/handoff", strings.NewReader(`{"session_id":"`+id.String()+`"}`), jsonBody)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("confirmed handoff = %d, want 204 (body %q)", rec.Code, rec.Body.String())
	}
	if len(host.closed) != 2 || host.closed[1] != id {
		t.Fatalf("host.closed = %v, want the second entry to be %v", host.closed, id)
	}
}

// TestUIHandoffRequiresANonSimpleContentType is carbon's CSRF defence for its own
// control route. wui's CSRFGuard is created INSIDE wui.Handler and its token store is
// unreachable from here, so /ui/handoff cannot join the five routes it protects.
// Requiring application/json closes the same hole a different way: an HTML form can
// only send the three "simple" content types, so a cross-site POST carrying JSON
// triggers a CORS preflight the Host/Origin guard then rejects.
func TestUIHandoffRequiresANonSimpleContentType(t *testing.T) {
	t.Parallel()

	id, err := uuid.New()
	if err != nil {
		t.Fatal(err)
	}
	host := &stubHandoffHost{live: id, hasLive: true}
	h := testServeHandler(t, host)

	for _, ct := range []string{"", "text/plain", "application/x-www-form-urlencoded", "multipart/form-data"} {
		rec := doServe(t, h, http.MethodPost, "/ui/handoff", strings.NewReader(`{"session_id":"`+id.String()+`"}`), func(r *http.Request) {
			if ct != "" {
				r.Header.Set("Content-Type", ct)
			}
		})
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Errorf("handoff with Content-Type %q = %d, want 415", ct, rec.Code)
		}
	}
	if len(host.closed) != 0 {
		t.Fatalf("a form-shaped cross-site POST reached CloseLive: %v", host.closed)
	}
}

// TestUIHandoffRejectsAMalformedBody keeps a bad request a 400 rather than an
// accidental close of whatever happens to be live.
func TestUIHandoffRejectsAMalformedBody(t *testing.T) {
	t.Parallel()

	host := &stubHandoffHost{live: uuid.UUID{0x7}, hasLive: true}
	h := testServeHandler(t, host)

	for name, body := range map[string]string{
		"not json":      `{`,
		"no id":         `{}`,
		"not a uuid":    `{"session_id":"nope"}`,
		"unknown field": `{"session_id":"00000000-0000-4000-8000-000000000000","force":true}`,
	} {
		rec := doServe(t, h, http.MethodPost, "/ui/handoff", strings.NewReader(body), jsonBody)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: handoff = %d, want 400 (body %q)", name, rec.Code, rec.Body.String())
		}
	}
	if len(host.closed) != 0 {
		t.Fatalf("a malformed confirmation reached CloseLive: %v", host.closed)
	}
}

// TestUIRoutesRejectTheWrongMethod proves the /ui surface is method-pinned, so a
// state-changing route can never be reached by a GET (which carries no CSRF-relevant
// protection at all).
func TestUIRoutesRejectTheWrongMethod(t *testing.T) {
	t.Parallel()

	h := testServeHandler(t, &stubHandoffHost{})
	if rec := doServe(t, h, http.MethodPost, "/ui/live", nil, jsonBody); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /ui/live = %d, want 405", rec.Code)
	}
	if rec := doServe(t, h, http.MethodGet, "/ui/handoff", nil, nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /ui/handoff = %d, want 405", rec.Code)
	}
}

// jsonBody marks a request as carrying a JSON body.
func jsonBody(r *http.Request) { r.Header.Set("Content-Type", "application/json") }

// errorCode decodes the shared {"error":{"code",...}} envelope wui and harness both
// write, so a test can assert WHICH rejection happened rather than merely that one did.
func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v (body %q)", err, rec.Body.String())
	}
	return env.Error.Code
}
