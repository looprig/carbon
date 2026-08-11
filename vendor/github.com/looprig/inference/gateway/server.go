// Package gateway (this file) implements Server: a loopback-only local HTTP
// listener that wraps an arbitrary http.Handler (typically, but not
// necessarily, a *Handler built by New) with its own per-instance,
// cryptographically random bearer token.
//
// # Token-generation architecture (read this before touching auth here)
//
// Config.Authenticate (config.go) is required and is baked into a *Handler
// at gateway.New time -- it authenticates whatever token the CALLER chose
// when it built that Config, before any Server exists. Server cannot
// retroactively change what an already-built http.Handler checks, and the
// design doc's own worked composition example configures a Handler with
// Authenticate omitted entirely from its sketch of the pairing.
//
// The design doc's security section instead describes the LOCAL SERVER --
// not the Handler -- as the thing that "generates a cryptographically
// random bearer token per server" and "reports readiness only after the
// listener is bound". This file implements that literally, at the Server
// level: Server generates its own token independently of whatever the
// wrapped Handler does, and wraps ServerConfig.Handler in its own
// constant-time bearer-check middleware (see authMiddleware) using that
// self-generated token -- completely independent of, and unaware of, any
// authentication the inner Handler itself performs.
//
// This makes Server self-sufficient and testable against ANY http.Handler
// (not just a *Handler from this package), matches "the local server
// generates a token" literally, and requires reopening neither the
// already-committed Config (whose Authenticate stays mandatory) nor
// auth.go. A caller that builds a real Handler with its own Authenticate
// gets defense-in-depth double-checking under this scheme -- harmless, not
// a bug, since both checks run against the same forwarded Authorization
// header and a request must satisfy whichever checks are actually wired in
// front of it.
package gateway

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

// tokenByteLength is the number of raw random bytes read from crypto/rand
// per generated Server token -- 256 bits of entropy, encoded below as a
// URL-safe string. This is unrelated to, and independent of, any
// Config.Authenticate token a wrapped *Handler might separately check.
const tokenByteLength = 32

// DefaultShutdownTimeout is the bounded graceful-shutdown window applied
// when ServerConfig.ShutdownTimeout is zero. 5 seconds is a conservative,
// finite default consistent with the design's "bounded server shutdown"
// security posture: long enough to drain a typical in-flight request,
// short enough that Close never appears to hang.
const DefaultShutdownTimeout = 5 * time.Second

// ServerConfig configures a Server. Handler is required; ShutdownTimeout is
// optional (zero means DefaultShutdownTimeout).
//
// There is deliberately no address/port field: Server always binds
// 127.0.0.1 on an ephemeral port (see Start). This is the design's fixed
// security posture, not an oversight -- a Server is a local trust-boundary
// primitive, not a configurable network listener.
type ServerConfig struct {
	// Handler is served behind Server's own generated-token auth
	// middleware. It is never mutated and may be shared by more than one
	// Server at once (see the package doc above and Server's doc comment).
	Handler http.Handler

	// ShutdownTimeout bounds Close's graceful-drain window. Zero means
	// DefaultShutdownTimeout. Must not be negative.
	ShutdownTimeout time.Duration
}

// Binding reports a bound Server's loopback base URL and bearer token. It
// is returned by value from Server.Binding -- see that method's doc comment
// for why Binding() itself returns a bare (string, string, bool) tuple
// instead of this struct.
type Binding struct {
	BaseURL string
	Token   string
}

// serverState is Server's internal lifecycle state, guarded by Server.mu.
// It is intentionally a small closed enum, not a pair of booleans, so every
// reachable (state, ready, listener-present) combination is representable
// exactly once.
type serverState int

const (
	serverStateNew serverState = iota
	serverStateRunning
	serverStateClosing
	serverStateClosed
)

func (s serverState) String() string {
	switch s {
	case serverStateNew:
		return "new"
	case serverStateRunning:
		return "running"
	case serverStateClosing:
		return "closing"
	case serverStateClosed:
		return "closed"
	default:
		return "unknown"
	}
}

// ServerStateError reports an invalid Server lifecycle transition -- in
// practice, Start called while the Server is already running, closing, or
// closed. Close is never the source of a ServerStateError: it is idempotent
// by design (see Close's doc comment) rather than erroring on repeated
// calls.
type ServerStateError struct {
	Op    string
	State string
}

func (e *ServerStateError) Error() string {
	return fmt.Sprintf("gateway: server: %s: invalid in state %q", e.Op, e.State)
}

// ShutdownTimeoutError reports that Close's bounded graceful shutdown did
// not complete before its effective deadline (the caller's context combined
// with ServerConfig.ShutdownTimeout -- see Close), so the listener and any
// still-in-flight connections were force-closed instead. This is one of the
// design doc's listed local-server error categories ("shutdown timeout");
// no earlier task owns it, so it is added here, in the one file that needs
// it.
type ShutdownTimeoutError struct {
	Timeout time.Duration
}

func (e *ShutdownTimeoutError) Error() string {
	return fmt.Sprintf("gateway: server: graceful shutdown exceeded %s: forced close", e.Timeout)
}

// Server runs one loopback-only HTTP listener wrapping a ServerConfig.Handler
// behind a per-instance, cryptographically random bearer token (see the
// package doc above for why token generation lives here and not in
// Config/Authenticate). It is safe for concurrent use by multiple
// goroutines, including concurrent Start/Binding/Close calls.
//
// Every Server built by NewServer owns an entirely independent listener,
// *http.Server, token, and lifecycle state -- constructing two Servers,
// even from the same ServerConfig.Handler value, never shares state between
// them (see server_test.go's isolation tests). Server never touches
// http.DefaultServeMux or any other package-level global.
type Server struct {
	handler         http.Handler
	shutdownTimeout time.Duration

	mu       sync.Mutex
	state    serverState
	listener net.Listener
	http     *http.Server
	baseURL  string
	token    string

	// closeDone and closeErr publish the outcome of the one real shutdown
	// attempt (triggered by whichever Close call first observes
	// serverStateRunning) to every concurrent or later Close caller, so
	// Close is idempotent -- including under concurrent calls -- without
	// ever invoking http.Server.Shutdown/Close more than once.
	closeDone chan struct{}
	closeErr  error
}

// NewServer validates config and builds a ready-to-Start Server. It rejects
// an invalid config with a *ConfigError -- the same type New (config.go)
// uses -- for a nil Handler or a negative ShutdownTimeout.
func NewServer(config ServerConfig) (*Server, error) {
	if config.Handler == nil {
		return nil, &ConfigError{Location: "ServerConfig.Handler", Reason: "must not be nil"}
	}
	if config.ShutdownTimeout < 0 {
		return nil, &ConfigError{Location: "ServerConfig.ShutdownTimeout", Reason: "must not be negative"}
	}

	timeout := config.ShutdownTimeout
	if timeout == 0 {
		timeout = DefaultShutdownTimeout
	}

	return &Server{
		handler:         config.Handler,
		shutdownTimeout: timeout,
		state:           serverStateNew,
	}, nil
}

// Start binds a loopback (127.0.0.1), ephemeral-port TCP listener and begins
// serving config.Handler behind Server's own generated-token auth
// middleware in a background goroutine. It returns once the listener is
// bound and serving has been dispatched; it does not block for the
// Server's lifetime.
//
// Start is idempotent in the sense that calling it more than once never
// binds a second listener or silently succeeds: any call after the first
// successful Start -- while running, closing, or already closed -- returns
// a *ServerStateError without attempting to bind. Concurrent Start calls
// race safely: exactly one wins and the rest observe the post-transition
// state and fail with *ServerStateError (see server_race_test.go).
func (s *Server) Start(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	token, err := generateToken()
	if err != nil {
		return fmt.Errorf("gateway: server: generate token: %w", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("gateway: server: listen: %w", err)
	}

	s.mu.Lock()
	if s.state != serverStateNew {
		state := s.state
		s.mu.Unlock()
		// Lost the race (or Start was already called, successfully or
		// not): this listener is spurious and must not leak. Best-effort
		// close: there is no further action to take on failure.
		_ = ln.Close()
		return &ServerStateError{Op: "Start", State: state.String()}
	}

	mux := http.NewServeMux()
	mux.Handle("/", authMiddleware(token, s.handler))
	httpServer := &http.Server{
		Handler: mux,
		// Bounds how long a client may take to send request headers, so a
		// slow-header client cannot hold a connection (and its goroutine)
		// open indefinitely -- defense in depth even though this server is
		// loopback-only.
		ReadHeaderTimeout: 10 * time.Second,
	}

	s.listener = ln
	s.http = httpServer
	s.token = token
	s.baseURL = "http://" + ln.Addr().String()
	s.state = serverStateRunning
	go func() {
		_ = httpServer.Serve(ln)
	}()
	s.mu.Unlock()

	return nil
}

// Binding reports this Server's loopback base URL and bearer token, and
// whether they are currently valid: ready is true only while the Server is
// in its running state -- i.e. strictly after a successful Start has bound
// the listener and dispatched the serving goroutine, and strictly before
// Close begins shutting the Server down. Binding returns ("", "", false)
// before the first successful Start and again once Close has been called
// (whether or not that Close has finished draining) -- ready flips to false
// at the start of shutdown, not only once it completes, so a caller can
// never observe a stale, about-to-be-invalid binding as ready.
//
// Binding returns a bare (string, string, bool) tuple rather than a
// *Binding value on purpose: this is the exact shape the ACP-owned
// ModelProxy contract's Binding method expects (see
// acp/docs/connectors/inference-gateway.md), and Go has no structural
// typing for named struct return types -- matching the tuple shape is what
// lets *Server satisfy that interface without inference/gateway importing
// anything ACP-related.
func (s *Server) Binding() (baseURL, token string, ready bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != serverStateRunning {
		return "", "", false
	}
	return s.baseURL, s.token, true
}

// Close idempotently shuts this Server down: the first call performs a
// bounded graceful drain (see below) and every call -- concurrent or later
// -- returns that same first outcome. Close on a Server that was never
// started is a valid no-op returning nil; it still marks the Server closed,
// so a subsequent Start correctly fails with *ServerStateError rather than
// starting a Server whose owner already gave it up.
//
// The graceful drain combines ctx with ServerConfig.ShutdownTimeout (or
// DefaultShutdownTimeout, if that was zero) via context.WithTimeout, then
// calls http.Server.Shutdown with the combined, bounded context -- normal
// context composition means an earlier deadline already carried by ctx
// naturally wins without any special-case logic here. If Shutdown does not
// complete before that bounded deadline (or ctx is otherwise canceled first),
// the listener and any still-in-flight connections are force-closed via
// http.Server.Close so Close never hangs past its bound; a deadline-exceeded
// timeout specifically is reported as a *ShutdownTimeoutError, while any
// other Shutdown error is returned as-is.
func (s *Server) Close(ctx context.Context) error {
	s.mu.Lock()
	switch s.state {
	case serverStateNew:
		s.state = serverStateClosed
		s.mu.Unlock()
		return nil

	case serverStateClosed:
		err := s.closeErr
		s.mu.Unlock()
		return err

	case serverStateClosing:
		done := s.closeDone
		s.mu.Unlock()
		<-done
		s.mu.Lock()
		err := s.closeErr
		s.mu.Unlock()
		return err

	default: // serverStateRunning
		s.state = serverStateClosing
		done := make(chan struct{})
		s.closeDone = done
		httpServer := s.http
		timeout := s.shutdownTimeout
		s.mu.Unlock()

		err := gracefulShutdown(ctx, httpServer, timeout)

		s.mu.Lock()
		s.closeErr = err
		s.state = serverStateClosed
		s.mu.Unlock()
		close(done)

		return err
	}
}

// gracefulShutdown performs the bounded-drain-then-force-close sequence
// documented on Close.
func gracefulShutdown(ctx context.Context, httpServer *http.Server, timeout time.Duration) error {
	shutdownCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		shutdownCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	err := httpServer.Shutdown(shutdownCtx)
	if err == nil {
		return nil
	}

	// Shutdown did not complete gracefully within the bounded window (or
	// the caller's own ctx expired/was canceled first). Force-close so
	// Close never hangs past the bound, regardless of which case this was.
	_ = httpServer.Close()

	if errors.Is(err, context.DeadlineExceeded) {
		return &ShutdownTimeoutError{Timeout: timeout}
	}
	return err
}

// generateToken returns a cryptographically random, URL-safe token: 256
// bits (tokenByteLength bytes) of crypto/rand output, base64.RawURLEncoding
// encoded.
func generateToken() (string, error) {
	buf := make([]byte, tokenByteLength)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// authMiddleware wraps next with a constant-time bearer-token check against
// token, sharing checkBearerToken's parsing/comparison mechanics with
// auth.go's staticTokenAuthenticator: every failure mode (missing header,
// wrong scheme, oversized header, wrong length, wrong bytes) is rejected
// identically, with no distinguishing detail in the response. This is
// entirely independent of, and unaware of, any Authenticator a wrapped
// *Handler itself might separately run (see the package doc above).
func authMiddleware(token string, next http.Handler) http.Handler {
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !checkBearerToken(r.Header.Get("Authorization"), want) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
