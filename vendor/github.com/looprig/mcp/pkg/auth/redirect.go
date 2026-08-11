// This file implements the loopback redirect listener: the thing that catches
// the authorization code the browser is redirected to, per RFC 8252 §7.3.
//
// # Why loopback rather than a custom scheme or an out-of-band code
//
// A native app has three options for receiving a code. A custom URI scheme
// ("myapp://cb") can be claimed by any other program on the machine, so it
// hands the code to whoever registered it last. An out-of-band code asks the
// user to copy a string from a browser into a terminal, which RFC 8252 §8.4 and
// the OAuth 2.1 draft both deprecate — it trains users to paste credentials and
// it defeats PKCE's binding to the client. A loopback listener on an ephemeral
// port is what remains, and it is what the spec recommends: the OS guarantees
// only one process holds the port, and the code never leaves the machine.
//
// # What this thing is careful about
//
//   - It binds 127.0.0.1 explicitly, never ":0" or "0.0.0.0". A redirect
//     listener reachable from the network is a code interceptor for anyone on
//     that network.
//   - It is one-shot. The first callback resolves the flow and no further
//     request can; a listener that keeps answering is one an attacker can keep
//     guessing at.
//   - It validates state before it does anything with the code, and it never
//     echoes any request parameter into its response. The response is a static
//     page: reflecting the server's query string into HTML on a page the user's
//     browser renders would be a self-inflicted XSS, on localhost, in a context
//     where the attacker controls the query string by construction.
//   - It closes deterministically, so a cancelled or timed-out flow does not
//     leave a port held for the process's lifetime.

package auth

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// redirectPath is the single path the listener answers on. Everything else gets
// a 404, so a stray browser request — a favicon probe, a prefetch — cannot
// consume the one-shot callback.
const redirectPath = "/callback"

// Timeouts for the redirect listener's HTTP server. The callback is a GET from
// a browser on loopback with no body: it is the smallest request in the module,
// and none of these bounds are near anything legitimate. They exist because an
// unbounded server is a slowloris target, and this one is reachable by any
// local process.
const (
	redirectReadTimeout   = 5 * time.Second
	redirectWriteTimeout  = 5 * time.Second
	redirectIdleTimeout   = 5 * time.Second
	redirectMaxHeaderByte = 1 << 16
)

// callbackPage is what the user sees when the browser lands on the redirect. It
// is a constant, so nothing from the request can reach it.
const callbackPage = `<!DOCTYPE html>
<html lang="en">
<head><meta charset="utf-8"><title>Authorization complete</title></head>
<body style="font-family:system-ui,sans-serif;text-align:center;padding-top:4rem">
<h1>Authorization complete</h1>
<p>You can close this window and return to the application.</p>
</body>
</html>
`

// callbackResult is the outcome of the one callback the listener accepts.
type callbackResult struct {
	code authCode
	err  error
}

// redirectServer is a one-shot loopback listener for an authorization callback.
//
// Construct with newRedirectServer, which binds the port so that the redirect
// URI is known before it is needed — the URI has to go into the registration
// request and the authorization request, both of which happen before anyone
// waits here. Always Close it.
type redirectServer struct {
	listener net.Listener
	server   *http.Server
	results  chan callbackResult
	once     sync.Once // the one-shot guarantee: exactly one result is ever sent
	uri      string

	// mu guards expected. The handler runs on the server's goroutine while the
	// flow publishes the state from its own, so this is a genuine race rather
	// than a formality: -race catches it immediately without the lock.
	mu       sync.RWMutex
	expected state
}

// newRedirectServer binds an ephemeral loopback port and starts serving.
//
// Binding happens here rather than at wait time on purpose: the port is part of
// the redirect URI, the redirect URI is part of the dynamic client registration
// request (RFC 7591) and of the authorization request, and both of those must
// name the port we will actually be listening on. Registering a placeholder
// port and hoping the server honors RFC 8252 §7.3's "allow any port for
// loopback" is a real source of redirect_uri_mismatch failures; knowing the
// port first makes the question moot.
func newRedirectServer() (*redirectServer, error) {
	// 127.0.0.1 explicitly, not "localhost": what localhost resolves to is
	// local configuration, and it can resolve to a non-loopback address or to
	// an IPv6 address the authorization server will not accept in a redirect
	// URI. The literal is the thing we can promise.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, NewError(ClassFailed, "authorize", "could not bind a loopback redirect listener", err)
	}

	rs := &redirectServer{
		listener: listener,
		results:  make(chan callbackResult, 1),
		uri:      (&url.URL{Scheme: "http", Host: listener.Addr().String(), Path: redirectPath}).String(),
	}
	mux := http.NewServeMux()
	mux.HandleFunc(redirectPath, rs.handle)
	rs.server = &http.Server{
		Handler:        mux,
		ReadTimeout:    redirectReadTimeout,
		WriteTimeout:   redirectWriteTimeout,
		IdleTimeout:    redirectIdleTimeout,
		MaxHeaderBytes: redirectMaxHeaderByte,
	}
	go func() {
		// Serve always returns a non-nil error; on our own Close it is
		// ErrServerClosed, and there is no other outcome worth reporting —
		// wait() is what reports a flow that never completed, with a reason a
		// caller can act on.
		_ = rs.server.Serve(listener)
	}()
	return rs, nil
}

// URI returns the redirect URI to send to the authorization server.
func (r *redirectServer) URI() string { return r.uri }

// wait blocks for the callback and returns the authorization code it carried.
//
// arm must have been called first; a listener that has not been armed rejects
// every callback, because a zero state matches nothing.
//
// It returns a typed error when the flow is refused, when the state does not
// match, or when ctx ends first — the last covering both the caller's
// cancellation and the authorization timeout, which is just a deadline on ctx.
func (r *redirectServer) wait(ctx context.Context) (authCode, error) {
	select {
	case result := <-r.results:
		if result.err != nil {
			return authCode{}, result.err
		}
		return result.code, nil
	case <-ctx.Done():
		return authCode{}, NewError(ClassFailed, "authorize",
			"timed out waiting for the authorization callback", ctx.Err())
	}
}

// arm publishes the state the handler must match, and must be called before the
// browser is opened.
//
// That ordering is the whole reason this is a separate method rather than an
// argument to wait, where it started. With it inside wait, the sequence was
// "open the browser, then arm the listener" — and the browser can win that
// race. A callback arriving before the listener is armed is matched against the
// zero state, which matches nothing, so the legitimate callback is rejected at
// the door and the flow blocks until it times out. It survives a test suite
// because a browser launch plus a network round trip is slower than reaching
// the next statement, which is exactly the kind of race that waits to happen on
// a loaded machine against a fast local server.
//
// Arming is separate from construction because the port must be known before
// the flow starts — registration needs it — while the state belongs to the
// flow.
func (r *redirectServer) arm(st state) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expected = st
}

// loadState reads the expected state.
func (r *redirectServer) loadState() state {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.expected
}

// handle serves the one callback.
//
// The order is the security-relevant part: state is checked before the code is
// touched. A callback that fails the check is answered with a flat 400 and does
// NOT resolve the flow, so an attacker who can reach the listener cannot use a
// forged callback either to inject a code or to deny the real one.
func (r *redirectServer) handle(w http.ResponseWriter, req *http.Request) {
	query := req.URL.Query()

	if !r.loadState().Matches(query.Get("state")) {
		// Nothing from the request goes into this response or into any error:
		// the whole request is attacker-supplied.
		http.Error(w, "invalid state", http.StatusBadRequest)
		return
	}

	// Every branch below answers the browser BEFORE resolving, because
	// resolving is what unblocks the flow, and the flow closes this server the
	// moment it is unblocked. Resolve first and the user's browser gets a
	// connection reset instead of the message explaining what went wrong.

	// RFC 6749 §4.1.2.1: the server reports a refusal in the query string
	// rather than by failing the redirect.
	if errCode := query.Get("error"); errCode != "" {
		http.Error(w, "authorization failed", http.StatusBadRequest)
		r.resolve(callbackResult{err: authorizationError(errCode)})
		return
	}

	code := newAuthCode(query.Get("code"))
	if !code.Valid() {
		http.Error(w, "invalid callback", http.StatusBadRequest)
		r.resolve(callbackResult{err: NewError(ClassFailed, "authorize",
			"authorization callback carried neither a code nor an error", nil)})
		return
	}

	// Answer the browser before resolving, and flush, so the user sees the page
	// even though Close is moments away. The flush is what makes that ordering
	// real rather than hopeful: without it the bytes sit in the server's buffer
	// until the handler returns, which may be after the socket is gone.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte(callbackPage)); err == nil {
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}
	r.resolve(callbackResult{code: code})
}

// resolve delivers the flow's one result, discarding any later one.
//
// sync.Once rather than a non-blocking send: the buffered channel would also
// avoid a block, but it would let a second callback overwrite nothing while
// still doing the work, and the intent here is "exactly one outcome, decided by
// the first caller through the door".
func (r *redirectServer) resolve(result callbackResult) {
	r.once.Do(func() { r.results <- result })
}

// authorizationError maps an RFC 6749 §4.1.2.1 authorization error code to a
// typed failure.
//
// The distinction that matters to a caller is denied versus failed: access_denied
// means the user said no and retrying will produce the same answer, while the
// rest are configuration or server problems. Anything unrecognized is failed,
// because an error we do not understand is not one we may treat as a refusal.
func authorizationError(code string) error {
	class := ClassFailed
	if code == "access_denied" {
		class = ClassDenied
	}
	// The code is from a fixed vocabulary but arrives over the wire, so it is
	// bounded and normalized by NewError before it can reach a log.
	return NewError(class, "authorize", fmt.Sprintf("authorization server refused the request: %s", code), nil)
}

// Close stops the listener and releases the port. It is safe to call more than
// once and is what makes a cancelled flow leave nothing behind.
func (r *redirectServer) Close() error {
	// Close rather than Shutdown: Shutdown waits for in-flight requests, and
	// the only in-flight request this server can have is one whose response we
	// have already flushed. Waiting would mean a cancelled flow blocks on a
	// browser connection that may be keep-alive idle for as long as the idle
	// timeout allows.
	err := r.server.Close()
	// Serve closes the listener itself; this is for the paths where Serve never
	// got that far. A double close is an error we do not care about.
	_ = r.listener.Close()
	if err != nil {
		return NewError(ClassFailed, "authorize", "could not close the redirect listener", err)
	}
	return nil
}
