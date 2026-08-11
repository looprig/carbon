// This file is the entry point that ties the pieces together: discovery
// (discovery.go), registration (register.go), PKCE and state (pkce.go), and the
// loopback listener (redirect.go) become one call, OAuthProvider.Token, that
// returns a usable access token or a typed reason why it cannot.
//
// # The shape of the thing
//
// Token is a ladder, and each rung is cheaper than the one below it:
//
//	stored and still valid  ->  return it
//	stored with a refresh   ->  refresh it            (no user involved)
//	anything else           ->  the full browser flow (a human must act)
//
// Falling to a lower rung is a decision, not a default. A refresh that fails
// because the grant is dead falls through to the browser, because that is
// genuinely the remedy; a refresh that fails because the network is down does
// NOT, because opening a browser at a user whose wifi dropped is a bad answer
// to a question they did not ask. That distinction is why refreshFailedTerminally
// exists and why it is written the strict way round: only errors we recognize
// as "this grant is over" earn the browser.
//
// # Concurrency
//
// Two mutexes, with different jobs. flowMu serializes the ladder, so that ten
// concurrent requests through Headers cause one browser flow rather than ten —
// it is held across the whole flow, which can be minutes. statusMu guards the
// status snapshot, and is separate precisely because Status must stay answerable
// while a flow is blocked on a human. One mutex here would make Status block for
// as long as the user takes to log in, which is the one time anybody wants to
// ask what the auth state is.

package auth

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// OAuth grant types (RFC 6749 §4.1.3, §6). These are the only two this package
// implements: the authorization-code grant, and the refresh that follows it.
const (
	grantAuthorizationCode = "authorization_code"
	grantRefreshToken      = "refresh_token"
)

// Defaults for an OAuthConfig that does not state otherwise.
const (
	// DefaultAuthorizationTimeout bounds how long the flow waits for the user
	// to finish in the browser. It is generous because the user may have to
	// find a password manager, complete an MFA challenge, or approve on a
	// phone; it is bounded because a flow that waits forever holds a loopback
	// port and a mutex forever.
	DefaultAuthorizationTimeout = 5 * time.Minute
	// DefaultHTTPTimeout bounds each individual HTTP request the flow makes.
	// Every one of them is a small JSON round trip to an endpoint that is
	// either healthy or not.
	DefaultHTTPTimeout = 30 * time.Second
	// defaultClientName is what an unnamed client registers itself as.
	defaultClientName = "mcp-client"
	// maxRedirects bounds redirect following on the flow's own requests.
	maxRedirects = 5
)

// OAuthConfig configures an OAuthProvider. Build the provider with
// NewOAuthProvider, which validates and applies defaults.
//
// Note that there is no ClientSecret field. A client secret lives inside
// Credentials, behind an accessor, so that an OAuthConfig — a value an
// application builds at startup and is entirely likely to log — carries no
// printable secret. See register.go.
type OAuthConfig struct {
	// ServerURL is the MCP server this provider gets tokens for: the protected
	// resource. It is canonicalized to an origin (see CanonicalOrigin) to key
	// the token store, and used as the RFC 8707 resource indicator.
	ServerURL string

	// Credentials identifies the OAuth client. A zero value means the client is
	// unregistered, and the provider will attempt dynamic client registration
	// (RFC 7591) against the authorization server if it offers it.
	//
	// Registration is not persisted — this package stores nothing but tokens.
	// Read OAuthProvider.Credentials after a successful flow and persist the
	// result if repeat registration is undesirable, which on most servers it is.
	Credentials ClientCredentials

	// Scopes are the scopes to request. When empty, the scopes the resource
	// advertises are requested, and when it advertises none, no scope parameter
	// is sent at all — which asks the server for its default.
	Scopes []string

	// Store is where tokens are kept. Required: a provider with nowhere to put
	// a token would run an interactive flow on every call, which is not a
	// degraded mode worth having. Use NewMemoryStore for tokens that should die
	// with the process.
	Store TokenStore

	// Browser opens the authorization URL. Required, because there is no
	// sensible default: see BrowserOpener.
	Browser BrowserOpener

	// HTTPClient is used for discovery, registration, and token requests. When
	// nil, a client with explicit timeouts and TLS 1.2 minimum is used.
	//
	// A supplied client is used as-is, timeouts and TLS settings included. That
	// is the caller's call to make: the reason to inject one is usually a
	// corporate proxy or a pinned root, and second-guessing it here would
	// defeat the point.
	HTTPClient *http.Client

	// ClientName is the human-readable name registered with the authorization
	// server, shown to the user on the consent screen. Defaults to
	// "mcp-client".
	ClientName string

	// AuthorizationTimeout bounds the wait for the user to complete the browser
	// flow. Defaults to DefaultAuthorizationTimeout.
	AuthorizationTimeout time.Duration
}

// OAuthProvider obtains and maintains OAuth tokens for one MCP server.
//
// It implements HeaderProvider, so an HTTP transport can ask it for an
// Authorization header per request and stay ignorant of OAuth entirely.
//
// It is safe for concurrent use, and concurrent callers cooperate: the first to
// need a token runs the flow and the rest wait for its result, rather than each
// opening a browser. Build one with NewOAuthProvider; the zero value is not
// usable.
type OAuthProvider struct {
	// Immutable after construction.
	serverURL   string
	origin      string
	scopes      []string
	store       TokenStore
	browser     BrowserOpener
	httpClient  *http.Client
	clientName  string
	authTimeout time.Duration

	// flowMu serializes the token ladder. Held across the browser flow.
	flowMu sync.Mutex
	// creds is mutable: dynamic registration assigns it. Guarded by flowMu.
	creds ClientCredentials
	// disco caches discovery for the process's lifetime. Guarded by flowMu.
	// Discovery is a property of the deployment, not of the moment, and
	// re-fetching it per token would triple the request count for nothing.
	disco *discovered

	// statusMu guards status, and is deliberately not flowMu. See the file
	// comment.
	statusMu sync.RWMutex
	status   Status
}

// Compile-time proof of the contract the HTTP transport depends on. If this
// stops holding, the transport's build breaks here instead of at its call site.
var _ HeaderProvider = (*OAuthProvider)(nil)

// NewOAuthProvider validates cfg and builds a provider.
//
// It does no I/O: nothing is discovered, registered, or fetched until Token is
// called. Construction failing means the configuration is wrong, which is a
// programmer or operator error and is reported as ClassInvalidConfig — never as
// an auth failure, because nothing has been attempted yet.
func NewOAuthProvider(cfg OAuthConfig) (*OAuthProvider, error) {
	fail := func(msg string) (*OAuthProvider, error) {
		return nil, NewError(ClassInvalidConfig, "new_oauth_provider", msg, nil)
	}

	origin, err := CanonicalOrigin(cfg.ServerURL)
	if err != nil {
		// err is already a ClassInvalidConfig *Error naming the problem.
		return nil, err
	}
	if cfg.Store == nil {
		return fail("Store is nil; supply a TokenStore (NewMemoryStore is the ephemeral one)")
	}
	if cfg.Browser == nil {
		return fail("Browser is nil; supply a BrowserOpener")
	}

	// The server URL is validated as a whole, not only as an origin: it is sent
	// as the RFC 8707 resource indicator, so it must be a URL the flow can name.
	if err := checkEndpointURL(cfg.ServerURL); err != nil {
		return fail(fmt.Sprintf("ServerURL: %s", err.Error()))
	}

	clientName := cfg.ClientName
	if clientName == "" {
		clientName = defaultClientName
	}
	if i := indexOfControl(clientName); i >= 0 {
		return fail(fmt.Sprintf("ClientName contains a control character at index %d", i))
	}
	authTimeout := cfg.AuthorizationTimeout
	if authTimeout == 0 {
		authTimeout = DefaultAuthorizationTimeout
	}
	if authTimeout < 0 {
		return fail("AuthorizationTimeout is negative")
	}
	for _, scope := range cfg.Scopes {
		// A scope reaches a URL query and a registration document. Space is the
		// scope-list separator (RFC 6749 §3.3), so a scope containing one is
		// two scopes wearing a trench coat.
		if scope == "" || strings.ContainsAny(scope, " \t\r\n") {
			return fail("Scopes contains an empty scope or one containing whitespace")
		}
	}

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = defaultHTTPClient()
	}

	provider := &OAuthProvider{
		serverURL:   cfg.ServerURL,
		origin:      origin,
		scopes:      append([]string(nil), cfg.Scopes...),
		store:       cfg.Store,
		browser:     cfg.Browser,
		httpClient:  httpClient,
		clientName:  clientName,
		authTimeout: authTimeout,
		creds:       cfg.Credentials,
		status:      NewStatus(StateRequired, time.Time{}, nil, ""),
	}
	// The key must be usable before anything is stored under it. CanonicalOrigin
	// guarantees the origin half; this catches an over-long client ID, which is
	// the caller's to fix and which would otherwise surface as a store error at
	// the end of a successful browser flow.
	if err := provider.key().Validate(); err != nil {
		return nil, err
	}
	return provider, nil
}

// defaultHTTPClient builds the client used when a caller supplies none.
//
// Every field here is a bound. The stock http.Client has no timeout at all, and
// http.Transport's defaults leave the response-header wait unbounded — against
// a hostile or wedged authorization server that is a goroutine parked forever
// holding flowMu, which is to say a deadlocked provider.
func defaultHTTPClient() *http.Client {
	return &http.Client{
		Timeout: DefaultHTTPTimeout,
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			// TLS 1.2 is the floor the module mandates. InsecureSkipVerify is
			// absent and must stay absent: these connections carry tokens.
			TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 15 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			IdleConnTimeout:       60 * time.Second,
			MaxIdleConns:          10,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("stopped after %d redirects", maxRedirects)
			}
			// A redirect that leaves https is a downgrade, and the Go client
			// would follow it carrying whatever headers it was given.
			return checkEndpointURL(req.URL.String())
		},
	}
}

// key returns the token-store key for the client currently in effect. Callers
// must hold flowMu: the key's ClientID changes when registration assigns one.
func (p *OAuthProvider) key() Key {
	return Key{ServerOrigin: p.origin, ClientID: p.creds.ID()}
}

// Credentials returns the OAuth client credentials in effect, including any
// assigned by dynamic registration.
//
// This is how a caller persists a registration: read it after a successful
// Token, keep the ID and Secret, and pass them back through OAuthConfig next
// run. Without that, every process start registers a new client on the
// authorization server.
func (p *OAuthProvider) Credentials() ClientCredentials {
	p.flowMu.Lock()
	defer p.flowMu.Unlock()
	return p.creds
}

// Status returns the provider's last known auth posture.
//
// It never blocks on a flow in progress and never performs I/O: it reports what
// the last completed operation established. A Status is designed to be logged
// as-is; see status.go.
func (p *OAuthProvider) Status() Status {
	p.statusMu.RLock()
	defer p.statusMu.RUnlock()
	return p.status
}

// setStatus publishes a new posture.
func (p *OAuthProvider) setStatus(status Status) {
	p.statusMu.Lock()
	defer p.statusMu.Unlock()
	p.status = status
}

// foreignFailure is the Status text for an error this package did not author.
//
// It names no detail on purpose. A Status is published — it reaches logs, UIs
// and telemetry — and the text of a foreign error is not this package's to
// vouch for. The error itself is still returned to the caller, who owns it and
// can render it wherever they judge safe; what is refused is republishing it.
const foreignFailure = "the operation failed with an error this package did not author; see the returned error"

// setFailure publishes a failure posture derived from err's class.
//
// Only an *Error's OWN rendering is ever published, and that is the whole point
// of this function's shape.
//
// The obvious version — classify with ClassOf, then publish err.Error() — is
// wrong twice, and both are the same mistake: taking a guarantee that holds for
// SOME of err's possible values and stating it for all of them.
//
//  1. A caller's TokenStore is application code and may return any error at
//     all. ClassOf's `ok` result is the admission: when it is false, err is
//     foreign, and err.Error() is arbitrary text of unknown provenance — a
//     store that puts a connection string or a key id in its message would land
//     it in a published Status.
//  2. Even when ClassOf succeeds, err.Error() is still not safe. ClassOf finds
//     an *Error ANYWHERE in the chain, including wrapped inside a foreign
//     error — and err.Error() renders the WHOLE chain, foreign text included.
//     "ok" says an auth Error is in there, never that err is one.
//
// So the *Error is extracted and asked to render ITSELF (class, op, and its own
// bounded, normalized, secret-free Msg — never its cause). An error with no
// *Error in it publishes a constant. The status is derived from what this
// package wrote, or from nothing.
func (p *OAuthProvider) setFailure(err error) {
	state := StateFailed
	message := foreignFailure

	var e *Error
	if errors.As(err, &e) {
		switch e.Class {
		case ClassDenied:
			state = StateDenied
		case ClassExpired:
			state = StateExpired
		case ClassRequired, ClassNoToken:
			state = StateRequired
		case ClassInvalidConfig, ClassFailed:
			state = StateFailed
		}
		message = e.Error()
	}
	p.setStatus(NewStatus(state, time.Time{}, nil, message))
}

// Headers implements HeaderProvider, returning the bearer Authorization header
// for a request.
//
// This is the whole interface between OAuth and the HTTP transport: the
// transport calls this per request and never learns that OAuth exists.
func (p *OAuthProvider) Headers(ctx context.Context) ([]Header, error) {
	set, err := p.Token(ctx)
	if err != nil {
		return nil, err
	}
	// "Bearer" per RFC 6750 §2.1. The token's own token_type is not consulted:
	// this client requests only bearer tokens, and honoring a server's claim of
	// some other type would mean implementing it.
	return []Header{NewHeader("Authorization", "Bearer "+set.Access())}, nil
}

// Token returns a valid access token, refreshing or running the full
// authorization flow as needed.
//
// It may block for a long time: the full flow waits for a human. Callers who
// cannot wait should pass a ctx with a deadline — the flow honors it at every
// step, including the wait for the browser.
func (p *OAuthProvider) Token(ctx context.Context) (TokenSet, error) {
	p.flowMu.Lock()
	defer p.flowMu.Unlock()

	set, err := p.token(ctx)
	if err != nil {
		p.setFailure(err)
		return TokenSet{}, err
	}
	p.setStatus(StatusOf(set, time.Now()))
	return set, nil
}

// token is Token's ladder, without the status bookkeeping. Callers hold flowMu.
func (p *OAuthProvider) token(ctx context.Context) (TokenSet, error) {
	if err := ctx.Err(); err != nil {
		return TokenSet{}, NewError(ClassFailed, "token", "context is done", err)
	}

	stored, err := p.store.Load(ctx, p.key())
	switch {
	case err == nil:
		if stored.Valid() && !stored.Expired(time.Now()) {
			return stored, nil
		}
	case errors.Is(err, ErrNoToken):
		// Absent, not broken: the flow below is exactly the remedy.
	default:
		// A store that is broken is not a store that is empty. Running an
		// interactive flow here would paper over a keyring failure with a
		// browser window, and then fail to persist the result anyway.
		return TokenSet{}, err
	}

	if err == nil && stored.Refresh() != "" {
		refreshed, refreshErr := p.refresh(ctx, stored)
		if refreshErr == nil {
			return refreshed, nil
		}
		if !refreshFailedTerminally(refreshErr) {
			return TokenSet{}, refreshErr
		}
		// The grant is over. The browser is the remedy; fall through.
	}
	return p.authorize(ctx)
}

// refreshFailedTerminally reports whether a failed refresh means the grant is
// finished, as opposed to something transient.
//
// Written as an allowlist rather than a denylist on purpose. The cost of
// getting this wrong in the permissive direction is a browser window opening in
// a user's face because a DNS lookup failed; in the strict direction it is a
// spurious error where a re-login would have worked, which the next call
// retries anyway. Only the classes that positively mean "this grant is dead"
// are terminal.
func refreshFailedTerminally(err error) bool {
	class, ok := ClassOf(err)
	if !ok {
		return false
	}
	return class == ClassExpired || class == ClassDenied
}

// authorize runs the full authorization-code flow with PKCE.
//
// The order is deliberate and is the reason this reads top to bottom rather
// than as a set of helpers: discovery decides whether the server is acceptable
// at all, the listener must exist before registration so registration can name
// its real port, and the browser must not open until everything that can fail
// without a human has already failed.
func (p *OAuthProvider) authorize(ctx context.Context) (TokenSet, error) {
	disco, err := p.discovery(ctx)
	if err != nil {
		return TokenSet{}, err
	}

	// Before anything else and before any user involvement: refuse a server
	// that cannot do S256. A downgrade to "plain" is not a fallback we have.
	method, err := selectChallengeMethod(disco.meta.CodeChallengeMethodsSupported)
	if err != nil {
		return TokenSet{}, err
	}

	redirect, err := newRedirectServer()
	if err != nil {
		return TokenSet{}, err
	}
	// The listener is one-shot and its lifetime is exactly this function's.
	defer func() { _ = redirect.Close() }()

	scopes := p.requestedScopes(disco)
	if !p.creds.Valid() {
		if disco.meta.RegistrationEndpoint == "" {
			return TokenSet{}, NewError(ClassInvalidConfig, "authorize",
				"no client ID was configured and the authorization server does not support dynamic client registration", nil)
		}
		creds, err := p.registerClient(ctx, disco.meta.RegistrationEndpoint, redirect.URI(), scopes)
		if err != nil {
			return TokenSet{}, err
		}
		// The key changes with the client ID: tokens are stored per client.
		p.creds = creds
		if err := p.key().Validate(); err != nil {
			return TokenSet{}, NewError(ClassFailed, "register",
				"the authorization server issued a client ID that cannot be used as a store key", err)
		}
	}

	verifier, err := newPKCE()
	if err != nil {
		return TokenSet{}, err
	}
	csrf, err := newState()
	if err != nil {
		return TokenSet{}, err
	}

	authURL, err := p.authorizationURL(disco.meta.AuthorizationEndpoint, redirect.URI(), verifier, csrf, method, scopes)
	if err != nil {
		return TokenSet{}, err
	}

	// The deadline covers the human. It is derived from ctx, so a caller's own
	// cancellation still wins if it is sooner.
	waitCtx, cancel := context.WithTimeout(ctx, p.authTimeout)
	defer cancel()

	// Arm before opening the browser, never after: the callback can arrive
	// before the next statement runs. See redirectServer.arm.
	redirect.arm(csrf)

	if err := p.browser.OpenURL(waitCtx, authURL); err != nil {
		// A headless BrowserOpener refusing is a legitimate, expected outcome
		// and must not read as a crash: the caller needs credentials and cannot
		// get them here.
		return TokenSet{}, NewError(ClassRequired, "authorize",
			"could not open a browser for the authorization request", err)
	}

	code, err := redirect.wait(waitCtx)
	if err != nil {
		return TokenSet{}, err
	}

	set, err := p.exchange(ctx, disco.meta.TokenEndpoint, code, verifier, redirect.URI(), scopes)
	if err != nil {
		return TokenSet{}, err
	}
	if err := p.store.Store(ctx, p.key(), set); err != nil {
		return TokenSet{}, err
	}
	return set, nil
}

// discovery returns the cached discovery result, fetching it on first use.
// Callers hold flowMu.
func (p *OAuthProvider) discovery(ctx context.Context) (discovered, error) {
	if p.disco != nil {
		return *p.disco, nil
	}
	disco, err := p.discover(ctx)
	if err != nil {
		return discovered{}, err
	}
	p.disco = &disco
	return disco, nil
}

// requestedScopes picks the scopes to ask for: the caller's if it stated any,
// otherwise whatever the resource advertises, otherwise none.
//
// Falling back to the resource's advertised scopes rather than to a guess is
// the point: asking for a scope the server does not know is an invalid_scope
// error, and asking for nothing gets the server's default, which is a defined
// behavior rather than a hopeful one.
func (p *OAuthProvider) requestedScopes(disco discovered) []string {
	if len(p.scopes) > 0 {
		return p.scopes
	}
	return disco.scopes
}

// authorizationURL builds the RFC 6749 §4.1.1 authorization request URL.
func (p *OAuthProvider) authorizationURL(endpoint, redirectURI string, verifier pkce, csrf state, method string, scopes []string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		// Already validated by authServerMetadata.validate; this is the
		// belt-and-braces branch.
		return "", NewError(ClassFailed, "authorize", "authorization endpoint is not a valid URL", err)
	}

	// The endpoint may legitimately carry its own query (a tenant hint, say),
	// so ours is merged into it rather than replacing it.
	query := u.Query()
	query.Set("response_type", "code")
	query.Set("client_id", p.creds.ID())
	query.Set("redirect_uri", redirectURI)
	query.Set("state", csrf.Value())
	query.Set("code_challenge", verifier.Challenge())
	query.Set("code_challenge_method", method)
	// RFC 8707 resource indicator. The MCP authorization spec requires it: it
	// tells the authorization server which resource the token is for, so a
	// token minted for one MCP server cannot be replayed against another that
	// trusts the same issuer.
	query.Set("resource", p.serverURL)
	if len(scopes) > 0 {
		query.Set("scope", strings.Join(scopes, " "))
	}
	u.RawQuery = query.Encode()

	return u.String(), nil
}

// tokenResponse is the RFC 6749 §5.1 success body and §5.2 error body.
//
// Its token fields are exported strings, which is the one place in this package
// that shape is allowed: this is a JSON serialization boundary, the type is
// unexported, and a value of it never escapes the function that decodes it —
// exchange and refresh convert it into a TokenSet, which is where the secret
// discipline resumes. Do not return one of these, store one, or put one in an
// error.
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	// ExpiresIn is a pointer so that ABSENT is distinguishable from zero, which
	// a plain int64 cannot do: both decode to 0, and the two mean opposite
	// things. An absent expires_in means "the server stated no expiry", which
	// TokenSet treats as never expiring; a zero one means "this token is
	// already dead". Conflating them yields a token that is never proactively
	// refreshed and instead degrades into 401s from the resource. See
	// expiryFrom.
	ExpiresIn    *int64 `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`

	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// maxExpiresIn caps expires_in at one year, in seconds, before it is turned
// into a duration.
//
// It exists for two reasons at once. The arithmetic one: time.Duration is an
// int64 of nanoseconds, so time.Duration(n) * time.Second silently WRAPS for n
// beyond ~292 years — expires_in of 1<<40 lands the expiry in 1795, and
// math.MaxInt64 lands it back at roughly now. Both happen to fail safe (an
// expiry in the past means we refresh), but by arithmetic luck rather than by
// design, and luck is not a property worth depending on. The semantic one: an
// access token good for longer than a year is not a thing, and a server
// claiming one is broken or lying.
//
// Clamping is safe in exactly one direction, which is why it is a clamp and not
// an error: capping expires_in DOWNWARDS can only make us refresh earlier than
// necessary, which costs one request. Rounding up, or trusting the number,
// could make us hold a token past its death.
const maxExpiresIn = 365 * 24 * 60 * 60

// exchange redeems an authorization code for tokens (RFC 6749 §4.1.3).
func (p *OAuthProvider) exchange(ctx context.Context, endpoint string, code authCode, verifier pkce, redirectURI string, scopes []string) (TokenSet, error) {
	form := url.Values{}
	form.Set("grant_type", grantAuthorizationCode)
	form.Set("code", code.Value())
	form.Set("redirect_uri", redirectURI)
	form.Set("client_id", p.creds.ID())
	// The PKCE proof. This is what makes a stolen code useless.
	form.Set("code_verifier", verifier.Verifier())
	form.Set("resource", p.serverURL)

	return p.postToken(ctx, endpoint, form, "exchange", scopes, TokenSet{})
}

// refresh exchanges a refresh token for a new access token (RFC 6749 §6).
func (p *OAuthProvider) refresh(ctx context.Context, previous TokenSet) (TokenSet, error) {
	disco, err := p.discovery(ctx)
	if err != nil {
		return TokenSet{}, err
	}

	form := url.Values{}
	form.Set("grant_type", grantRefreshToken)
	form.Set("refresh_token", previous.Refresh())
	form.Set("client_id", p.creds.ID())
	form.Set("resource", p.serverURL)
	// Scopes are deliberately not sent. RFC 6749 §6 allows a refresh to request
	// a narrower scope, and omitting the parameter means "the same scopes as
	// before" — which is what we want. Sending the originally requested scopes
	// would be wrong whenever the server granted fewer than we asked for: it
	// would read as a request to widen, which servers refuse.

	set, err := p.postToken(ctx, disco.meta.TokenEndpoint, form, "refresh", previous.Scopes(), previous)
	if err != nil {
		return TokenSet{}, err
	}
	if err := p.store.Store(ctx, p.key(), set); err != nil {
		return TokenSet{}, err
	}
	return set, nil
}

// postToken performs a token-endpoint request and converts the result.
//
// previous carries the token set being replaced, so that refresh-token rotation
// can be resolved here: see the fallback below.
//
// # This never retries
//
// A token request is not idempotent. An authorization code is single-use by
// specification (RFC 6749 §4.1.2), and a refresh token is single-use on any
// server that rotates them — which is the servers worth using. A retry after a
// response we did not see is therefore not a retry, it is a second, different
// request whose predecessor may well have succeeded: at best it fails with
// invalid_grant, at worst it burns a rotated refresh token and logs the user
// out. There is one Do call in this function and there must remain one.
func (p *OAuthProvider) postToken(ctx context.Context, endpoint string, form url.Values, op string, scopes []string, previous TokenSet) (TokenSet, error) {
	fail := func(class Class, msg string, err error) (TokenSet, error) {
		return TokenSet{}, NewError(class, op, msg, err)
	}

	if err := checkEndpointURL(endpoint); err != nil {
		return fail(ClassFailed, fmt.Sprintf("token endpoint: %s", err.Error()), nil)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return fail(ClassFailed, "could not build the token request", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if p.creds.Confidential() {
		// RFC 6749 §2.3.1 client_secret_basic. The values are form-urlencoded
		// before being base64'd, which is the part everyone forgets and which
		// matters for any secret containing a "+" or "%".
		req.SetBasicAuth(url.QueryEscape(p.creds.ID()), url.QueryEscape(p.creds.Secret()))
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return fail(ClassFailed, fmt.Sprintf("could not reach the token endpoint %q", redactURL(endpoint)), err)
	}
	defer func() { _ = resp.Body.Close() }()

	var body tokenResponse
	decodeErr := decodeBoundedJSON(resp.Body, &body)

	if resp.StatusCode != http.StatusOK {
		// RFC 6749 §5.2: a failed token request answers 400 (or 401) with a
		// JSON error body. The body is what classifies the failure; the status
		// alone cannot distinguish "the user revoked this" from "the server is
		// broken".
		if decodeErr == nil && body.Error != "" {
			return fail(tokenErrorClass(body.Error, resp.StatusCode),
				fmt.Sprintf("token request was refused: %s", body.Error), nil)
		}
		return fail(statusClass(resp.StatusCode),
			fmt.Sprintf("token endpoint returned HTTP %d", resp.StatusCode), decodeErr)
	}
	if decodeErr != nil {
		return fail(ClassFailed, fmt.Sprintf("token response: %s", decodeErr.Error()), decodeErr)
	}
	// A 200 carrying an error member is a server being sloppy, not a success.
	// Fail closed rather than treat an empty access token as a token.
	if body.Error != "" {
		return fail(tokenErrorClass(body.Error, resp.StatusCode),
			fmt.Sprintf("token request was refused: %s", body.Error), nil)
	}
	if body.AccessToken == "" {
		return fail(ClassFailed, "token response has no access_token", nil)
	}
	if body.TokenType != "" && !strings.EqualFold(body.TokenType, "bearer") {
		// We would send this as "Bearer" regardless; a server that means
		// something else is a server we cannot talk to correctly.
		return fail(ClassFailed, fmt.Sprintf("token response has unsupported token_type %q", body.TokenType), nil)
	}

	// now is read once, here, so that the expiry is computed against the same
	// instant the response arrived rather than against whenever the conversion
	// happens to run.
	set, err := newTokenSetFrom(body, scopes, previous, time.Now())
	if err != nil {
		return TokenSet{}, err
	}
	return set, nil
}

// expiryFrom turns a wire expires_in into an absolute expiry as of now.
//
// The four cases are genuinely four, and collapsing any pair of them is a bug:
//
//   - ABSENT (nil): the server stated no expiry. Returns the zero time, which
//     TokenSet reads as "never expires" — the correct reading, since some
//     servers legitimately issue tokens without one.
//   - NEGATIVE: not a thing. RFC 6749 §5.1 defines expires_in as "the lifetime
//     in seconds", and a negative lifetime is a malformed response, so it is a
//     typed error rather than a value to interpret. Guessing at what a broken
//     server meant is how a client ends up trusting a token it should not.
//   - ZERO: the server said this token is already dead. Returns now, which is
//     immediately expired (and ExpirySkew makes it expired regardless). This is
//     the case a plain int64 cannot see, because absent decodes to zero too.
//   - POSITIVE: now + n seconds, clamped to maxExpiresIn.
func expiryFrom(expiresIn *int64, now time.Time) (time.Time, error) {
	if expiresIn == nil {
		return time.Time{}, nil
	}
	seconds := *expiresIn
	if seconds < 0 {
		return time.Time{}, NewError(ClassFailed, "token",
			fmt.Sprintf("token response has a negative expires_in (%d)", seconds), nil)
	}
	if seconds > maxExpiresIn {
		seconds = maxExpiresIn
	}
	return now.Add(time.Duration(seconds) * time.Second), nil
}

// newTokenSetFrom converts a wire response into a TokenSet, resolving refresh
// rotation, expiry, and scope defaults.
func newTokenSetFrom(body tokenResponse, requested []string, previous TokenSet, now time.Time) (TokenSet, error) {
	// Rotation: a response carrying a refresh token replaces the old one — that
	// is what rotation means, and keeping the old one would mean using a token
	// the server has already invalidated. A response carrying none leaves the
	// old one in force (RFC 6749 §6 makes the member OPTIONAL precisely so a
	// non-rotating server can omit it), and dropping it there would silently
	// turn a refreshable grant into one that needs a browser next time.
	refresh := body.RefreshToken
	if refresh == "" {
		refresh = previous.Refresh()
	}

	expiry, err := expiryFrom(body.ExpiresIn, now)
	if err != nil {
		return TokenSet{}, err
	}

	// The granted scopes are what the server says they are, not what we asked
	// for; the two differ whenever the server grants a subset, and recording our
	// request rather than its answer would misreport what the token can do.
	scopes := requested
	if body.Scope != "" {
		scopes = strings.Fields(body.Scope)
	}
	return NewTokenSet(body.AccessToken, refresh, expiry, scopes), nil
}

// tokenErrorClass maps an RFC 6749 §5.2 token error code to a Class.
//
// The distinctions are the ones a caller acts on:
//
//   - expired means the grant is over but the user could get a new one, which
//     is what makes a refresh failure fall through to the browser;
//   - denied means someone said no — retrying or re-authorizing will not help,
//     so the browser must NOT open;
//   - failed is everything else, including every code we do not recognize,
//     because an unknown error is not one we may interpret as consent.
func tokenErrorClass(code string, status int) Class {
	switch code {
	case "invalid_grant":
		// The code or refresh token is expired, revoked, or already used. On a
		// refresh this is the signal to re-authorize.
		return ClassExpired
	case "access_denied":
		return ClassDenied
	case "invalid_client", "unauthorized_client":
		// The client itself is rejected. A new browser flow with the same
		// client would be rejected identically.
		return ClassDenied
	case "invalid_request", "invalid_scope", "unsupported_grant_type",
		"server_error", "temporarily_unavailable":
		return ClassFailed
	default:
		return statusClass(status)
	}
}

// statusClass maps an HTTP status to a Class, for responses that carry no
// usable OAuth error code.
func statusClass(status int) Class {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return ClassDenied
	default:
		return ClassFailed
	}
}
