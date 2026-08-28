package main

// serve.go is the ONE place in this module that imports harness's generic HTTP
// layer. cmd/carbon/main_test.go's TestRigPackagesHaveNoServeAdapter forbids the
// import everywhere under internal/, and TestServeCompositionLivesInCommand
// requires it here — the composition is a process-root concern, exactly as the
// generic serve.Handler[S, O] shape intends.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"

	carbon "github.com/looprig/carbon/internal/app"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/rig"
	"github.com/looprig/harness/pkg/serve"
	"github.com/looprig/harness/pkg/session"
	"github.com/looprig/wui"
)

// serveHost is the narrow view serveRig needs of internal/app's *ServeHost. It is
// declared HERE, at the consumer, so the wrapper is unit-testable against a stub and
// so internal/app never has to name a serve type.
type serveHost interface {
	NewSession(ctx context.Context) (session.SessionController, error)
	RestoreSession(ctx context.Context, id uuid.UUID) (session.SessionController, error)
	HasSession(ctx context.Context, id uuid.UUID) (bool, error)
}

// serveRig adapts carbon's session host to serve.Rig. It exists for exactly one
// behaviour the host cannot provide itself: minting a serve.SessionNotFoundError for
// an id that was never persisted, so handleRestore answers 404 instead of the bare
// 500 every other RestoreSession failure maps to.
//
// It is NOT a "Runner" adapter of the kind harness's pkg/serve/deps_test.go forbids
// and the rig-migration plan ruled out: it adds no HTTP behaviour, holds no request
// state, and its NewSession is a pass-through.
//
// It returns the host's controller UNWRAPPED. That is load-bearing: harness's live
// registry evicts a dead session by watching an optional Done() channel on the value
// it was handed (serve.SessionDone), and a wrapper that did not forward Done would
// silently opt every session out of eviction, pinning a subscription and a goroutine
// per dead session.
type serveRig struct{ host serveHost }

// NewSession is a pass-through. The variadic rig.SessionOption is part of serve.Rig's
// generic contract; carbon supplies none (there is no seed snapshot).
func (r *serveRig) NewSession(ctx context.Context, _ ...rig.SessionOption) (session.SessionController, error) {
	return r.host.NewSession(ctx)
}

// RestoreSession checks existence BEFORE touching the rig. A clean "no such session"
// becomes serve.SessionNotFoundError (404); a FAILED check propagates unchanged
// (500), because a broken catalog must never be reported as a missing session; and a
// restore that fails for a session that DOES exist also propagates unchanged, because
// rewriting a busy-workspace or config-drift failure into a 404 would hide a real
// fault behind a benign status.
func (r *serveRig) RestoreSession(ctx context.Context, id uuid.UUID) (session.SessionController, error) {
	exists, err := r.host.HasSession(ctx, id)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, serve.SessionNotFoundError{SessionID: id}
	}
	return r.host.RestoreSession(ctx, id)
}

// The instantiation serve.Handler is called with. Asserted here as well as in the
// test so a shape drift breaks the build, not just the suite.
var _ serve.Rig[session.SessionController, rig.SessionOption] = (*serveRig)(nil)

// --- HTTP composition -------------------------------------------------------

// buildServeHandler composes carbon's complete browser-facing surface:
//
//	serve.Handler(rig, reads)   -> the /v1 session API (lifecycle, control, SSE, reads)
//	wui.Handler(api)            -> /v1 forwarded, / served as the SPA, GET /v1/csrf-token
//	                               minted, per-route CSRF on the five control routes,
//	                               Host/Origin guard over that subtree
//	/ui/                        -> carbon's OWN pre-flight routes (see uiHandler)
//	wui.Guard(...)              -> the SAME Host/Origin guard, over EVERYTHING
//
// WHERE /ui/* SITS, and why. It sits INSIDE the Host/Origin guard and OUTSIDE wui's
// CSRF guard, and both halves of that are deliberate:
//
//   - Inside the origin guard, because loopback binding alone does not stop DNS
//     rebinding and /ui/handoff can end a running session. Guarding only wui's own
//     subtree would leave carbon's routes reachable from a rebound page. wui.Guard is
//     therefore applied over the whole mux; wui.Handler applies it again over its own
//     subtree, which is harmless (the check is pure) and keeps wui's guarantee
//     self-contained rather than dependent on what wrapped it.
//   - Outside wui's CSRF guard, because it is unreachable: wui.Handler creates its
//     CSRFGuard internally and registers GET /v1/csrf-token against THAT instance, so
//     a second guard built here would reject every token the SPA holds. /ui/handoff
//     instead requires a JSON content type (see handleUIHandoff), which an HTML form
//     cannot produce — a cross-site POST carrying it is preflighted, and the preflight
//     is refused by the origin guard above.
//
// The result is passed to serve.Server DIRECTLY so carbon keeps Server's hardening
// (ReadHeaderTimeout 5s, IdleTimeout 60s, MaxHeaderBytes 1 MiB, WriteTimeout 0 for
// SSE, TLS floor 1.2) instead of re-deriving five constants by hand. It does NOT
// carry harness's unexported auth-installed proof and cannot be made to — Go
// qualifies an unexported method name by its declaring package, so a wui or carbon
// authInstalled method is a different identifier from serve's. The consequence is
// bounded: Server refuses a bind only when it is non-loopback AND unproven, so it can
// only ever cause a false-negative refusal of a public bind, never a false-positive
// exposure. carbon binds loopback.
func buildServeHandler(r serve.Rig[session.SessionController, rig.SessionOption], reads serve.Reader, host handoffHost) http.Handler {
	root := http.NewServeMux()
	root.Handle(uiPrefix, uiHandler(host))
	root.Handle("/", wui.Handler(serve.Handler(r, reads)))
	return wui.Guard(root)
}

// uiPrefix is the subtree carbon owns. It is deliberately OUTSIDE /v1: /v1 is
// harness's wire contract, whose schemas are additionalProperties:false and whose
// error mapping turns every RestoreSession failure that is not a
// serve.SessionNotFoundError into a bare 500. A client that cannot distinguish "busy
// with another session" from "broken" cannot offer the handoff confirmation, so the
// distinction is published here instead of smuggled into harness's DTOs.
const uiPrefix = "/ui/"

// maxUIRequestBytes caps a /ui request body. The bodies are a single JSON object with
// one field; anything larger is a client error, not a large legitimate request.
const maxUIRequestBytes = 4 << 10

// handoffHost is the narrow view the /ui routes need of internal/app's *ServeHost. It
// is separate from serveHost because the two consumers are separate: serveRig needs a
// session factory, the handoff routes need the live-session seam, and neither should
// be forced to grow the other's methods.
type handoffHost interface {
	// LiveSession reports which session currently holds the workspace root.
	LiveSession() (uuid.UUID, bool)
	// CloseLive ends the live session only if it is the one named, so a confirmation
	// that raced another tab never kills the wrong session.
	CloseLive(ctx context.Context, expected uuid.UUID) error
}

// uiHandler routes carbon's own pre-flight surface. It is a dedicated mux rather than
// two patterns on the root mux because the root's "/" SPA catch-all matches every
// method: a POST to /ui/live registered only as "GET /ui/live" would fall through to
// the SPA and answer 200 with an HTML page instead of 405. Confined to its own mux,
// an unknown /ui path is a 404 and a wrong method is a 405.
func uiHandler(host handoffHost) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /ui/live", handleUILive(host))
	mux.Handle("POST /ui/handoff", handleUIHandoff(host))
	return mux
}

// uiLiveResponse is GET /ui/live's body. session_id is omitted when nothing is live,
// so a client cannot mistake the zero UUID for a session.
type uiLiveResponse struct {
	Live      bool   `json:"live"`
	SessionID string `json:"session_id,omitempty"`
}

// uiHandoffRequest is POST /ui/handoff's body. The id is REQUIRED: the whole point of
// the route is that the caller names the session it is ending.
type uiHandoffRequest struct {
	SessionID string `json:"session_id"`
}

// handleUILive answers which session holds the workspace root. It is the read half of
// the confirmed handoff: a client asks what is live BEFORE it asks a human whether to
// end it.
func handleUILive(host handoffHost) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body := uiLiveResponse{}
		if id, ok := host.LiveSession(); ok {
			body.Live, body.SessionID = true, id.String()
		}
		writeUIJSON(w, http.StatusOK, body)
	}
}

// handleUIHandoff performs a CONFIRMED handoff. A body naming a different session than
// the one actually live answers 409 rather than closing it, so a stale confirmation
// re-asks about the right session instead of killing work mid-turn.
//
// The JSON content-type requirement is this route's CSRF defence and is checked BEFORE
// anything else: it is not a convenience, so it must not be inferable or skippable.
func handleUIHandoff(host handoffHost) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !hasJSONContentType(r) {
			writeUIError(w, http.StatusUnsupportedMediaType, "unsupported_media_type",
				"handoff requires a Content-Type of application/json", false)
			return
		}
		var req uiHandoffRequest
		dec := json.NewDecoder(io.LimitReader(r.Body, maxUIRequestBytes))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeUIError(w, http.StatusBadRequest, "invalid_request", "malformed handoff request body", false)
			return
		}
		var id uuid.UUID
		if err := id.UnmarshalText([]byte(req.SessionID)); err != nil {
			writeUIError(w, http.StatusBadRequest, "invalid_request", "handoff requires a canonical session_id", false)
			return
		}

		err := host.CloseLive(r.Context(), id)
		var mismatch *carbon.LiveSessionHandoffError
		if errors.As(err, &mismatch) {
			// 409, never 500: the client's confirmation was for a session that is no
			// longer the live one. It re-reads GET /ui/live and re-asks. The id that
			// IS live is deliberately NOT echoed here — the client refetches it
			// through the one route that publishes it.
			writeUIError(w, http.StatusConflict, "live_session_mismatch",
				"a different session is live; re-read /ui/live and confirm again", true)
			return
		}
		if err != nil {
			slog.Error("carbon: handoff failed", "err", err)
			writeUIError(w, http.StatusInternalServerError, "handoff_failed", "the live session could not be closed", false)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// hasJSONContentType reports whether r declares a JSON body. Parameters (charset) are
// tolerated; a missing or non-JSON type is not. The three media types an HTML form can
// produce — application/x-www-form-urlencoded, multipart/form-data, text/plain — all
// fail this check, which is exactly why it stands in for the CSRF token wui's own
// guard cannot lend this route.
func hasJSONContentType(r *http.Request) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && mediaType == "application/json"
}

// writeUIJSON encodes a /ui success body.
func writeUIJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("carbon: encode ui response", "err", err)
	}
}

// uiErrorResponse mirrors the nested envelope harness's pkg/serve and wui both write
// ({"error":{"code","message","retryable"}}), so a client decodes every non-2xx
// response from this server — serve's, wui's, or carbon's — through one shape.
type uiErrorResponse struct {
	Error uiErrorBody `json:"error"`
}

// uiErrorBody carries a stable machine-readable code, a generic client-safe message
// that NEVER contains internal cause text, and whether the identical request may be
// retried.
type uiErrorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

func writeUIError(w http.ResponseWriter, status int, code, message string, retryable bool) {
	writeUIJSON(w, status, uiErrorResponse{Error: uiErrorBody{Code: code, Message: message, Retryable: retryable}})
}
