// This file implements the two discovery steps that turn "here is an MCP server
// URL" into "here is where to authorize and where to redeem a code":
//
//   - RFC 9728 protected-resource metadata, which asks the resource itself
//     which authorization servers it trusts;
//   - RFC 8414 authorization-server metadata, which asks that server where its
//     endpoints are, with the OpenID Connect discovery document as a fallback
//     because a large share of real deployments serve only that.
//
// # Everything here is untrusted input
//
// A discovery document is JSON fetched from a host named by another document
// fetched from a host the user typed. It is the least trustworthy input in the
// module, and it names URLs this client will then POST credentials to. So:
//
//   - bodies are bounded (a 1GB /.well-known must not OOM the process) and
//     depth-checked before they are parsed;
//   - every URL that comes out is re-validated for scheme before it is used,
//     because "the metadata said so" is not a reason to send a token over
//     cleartext or to hand a javascript: URL to a browser opener;
//   - the issuer is checked against the URL we fetched from (RFC 8414 §3.3).
//
// That last one is the load-bearing check and is worth stating plainly, because
// it looks like bookkeeping. Without it, a server that can influence which
// metadata URL we fetch can return a document whose issuer is some other
// authorization server and whose token_endpoint is the attacker's. The client
// would then send the user to the real server to authorize, and redeem the
// resulting code at the attacker's endpoint — a mix-up attack (RFC 9207). The
// check that the document's own issuer equals the identity we asked for is what
// makes the document self-certifying.
//
// # Why RFC 9207's `iss` is not validated on the callback
//
// RFC 9207 has the authorization server return an `iss` parameter on the
// redirect, so a client can tell WHICH server just answered. It is deliberately
// not implemented here, and the reason is a precondition rather than an
// oversight.
//
// A mix-up attack needs a client that is juggling more than one authorization
// server at once: the attack is to make a response from server A look like a
// response from server B, and `iss` is what distinguishes them. This client
// never has two in play. One provider binds to exactly one resource, discovery
// takes authorization_servers[0] and never falls back to another (see
// discoverResource — trying each in turn is refused precisely because it would
// blur which server we are talking to), the metadata is pinned to that issuer
// by the §3.3 check above, and the endpoints are pinned by that metadata. A
// callback can only be answering the single authorization request we sent to
// the single server we selected, and the state parameter already binds it to
// that request. There is no second server for `iss` to disambiguate from.
//
// What would change this: supporting several authorization servers per
// resource, or reusing one redirect listener across providers. Either
// reintroduces the precondition, and `iss` validation becomes load-bearing the
// day it lands.

package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/looprig/mcp/internal/limits"
)

// Bounds on discovery and token responses. These are the "hostile server"
// numbers: generous for any legitimate document, and small enough that a
// malicious one is a rounding error rather than an outage.
const (
	// maxDocumentBytes bounds any JSON body this package reads. Real discovery
	// documents are a couple of kilobytes; a megabyte is already absurd.
	maxDocumentBytes = 1 << 20
	// maxDocumentDepth bounds JSON nesting. These documents are flat — an
	// object of strings and string arrays — so anything deep is either broken
	// or an attempt to make a parser recurse.
	maxDocumentDepth = 16
)

// Well-known paths from RFC 9728 §3.1, RFC 8414 §3.1, and OpenID Connect
// Discovery §4.
const (
	wellKnownProtectedResource = "/.well-known/oauth-protected-resource"
	wellKnownAuthServer        = "/.well-known/oauth-authorization-server"
	wellKnownOpenIDConfig      = "/.well-known/openid-configuration"
)

// protectedResourceMetadata is the RFC 9728 document, reduced to the fields
// this client acts on.
//
// The unused fields of the RFC are deliberately absent rather than parsed and
// ignored: every field here is one this code reads, so there is nothing to
// audit for "is this validated?" beyond what is written. encoding/json drops
// unknown members, which is the correct behavior for a spec that is still
// growing members.
type protectedResourceMetadata struct {
	// Resource is the protected resource's own identifier. RFC 9728 §3.3
	// requires it to match the resource we asked about.
	Resource string `json:"resource"`
	// AuthorizationServers lists the issuers the resource accepts tokens from.
	AuthorizationServers []string `json:"authorization_servers"`
	// ScopesSupported is advisory; used only to default the requested scopes.
	ScopesSupported []string `json:"scopes_supported"`
}

// authServerMetadata is the RFC 8414 document (and the OpenID Connect discovery
// document, which is a superset for our purposes), reduced to the fields this
// client acts on.
type authServerMetadata struct {
	// Issuer is the server's identity. It must match the URL we derived it
	// from; see validate.
	Issuer string `json:"issuer"`
	// AuthorizationEndpoint is where the user is sent to authorize.
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	// TokenEndpoint is where a code or refresh token is redeemed.
	TokenEndpoint string `json:"token_endpoint"`
	// RegistrationEndpoint is where a client registers itself (RFC 7591). It
	// is optional: plenty of servers require an out-of-band client ID.
	RegistrationEndpoint string `json:"registration_endpoint"`
	// CodeChallengeMethodsSupported advertises PKCE support. See
	// selectChallengeMethod for how an empty value is treated.
	CodeChallengeMethodsSupported []string `json:"code_challenge_methods_supported"`
	// ScopesSupported is advisory.
	ScopesSupported []string `json:"scopes_supported"`
	// GrantTypesSupported is advisory; RFC 8414 defaults it to
	// authorization_code and implicit when absent.
	GrantTypesSupported []string `json:"grant_types_supported"`
}

// validate checks the document against the issuer it was fetched for.
//
// The issuer comparison is exact. RFC 8414 §3.3 specifies exactly that, and
// loosening it — trimming a trailing slash, comparing case-insensitively —
// would reintroduce the mix-up attack the check exists to stop, because "close
// enough" is precisely what an attacker needs.
func (m *authServerMetadata) validate(issuer string) error {
	fail := func(msg string) error {
		return NewError(ClassFailed, "discover", msg, nil)
	}

	if m.Issuer == "" {
		return fail("authorization server metadata has no issuer")
	}
	if m.Issuer != issuer {
		// Both values are non-secret identifiers, and naming them is the whole
		// value of this message: it is the one error where the operator needs
		// to see the mismatch to understand it. They are bounded by the
		// document bound above and normalized by NewError.
		return fail(fmt.Sprintf("authorization server metadata issuer %q does not match the issuer it was fetched for, %q", m.Issuer, issuer))
	}
	if m.AuthorizationEndpoint == "" {
		return fail("authorization server metadata has no authorization_endpoint")
	}
	if m.TokenEndpoint == "" {
		return fail("authorization server metadata has no token_endpoint")
	}

	// Re-validate every URL the flow will act on. The document is untrusted:
	// its endpoints are where credentials get sent and where the user's browser
	// gets pointed.
	endpoints := []struct {
		name  string
		value string
	}{
		{"authorization_endpoint", m.AuthorizationEndpoint},
		{"token_endpoint", m.TokenEndpoint},
		{"registration_endpoint", m.RegistrationEndpoint},
	}
	for _, endpoint := range endpoints {
		if endpoint.value == "" {
			continue
		}
		if err := checkEndpointURL(endpoint.value); err != nil {
			return fail(fmt.Sprintf("authorization server metadata %s: %s", endpoint.name, err.Error()))
		}
	}
	return nil
}

// checkEndpointURL reports whether an endpoint URL from a discovery document is
// one this client will use: bounded, control-character free, absolute, with a
// host, and https (or http on loopback).
//
// A path, query or fragment is allowed and left alone: an authorization
// endpoint legitimately carries a tenant hint in its query, and this function's
// job is to judge whether the URL is safe to act on, not to canonicalize it.
// CanonicalOrigin is the one that strips.
//
// The scheme rule is the same one Key.Validate and CanonicalOrigin apply, for
// the same reason — but it matters more here, because this URL did not come
// from the operator. It came from a document. A "javascript:" or "data:"
// authorization_endpoint handed to a BrowserOpener is script execution in the
// user's browser, and an "http://" token_endpoint is a credential on the wire.
// The allowlist below admits neither: only https, and http on loopback.
func checkEndpointURL(raw string) error {
	if len(raw) > MaxURLBytes {
		return fmt.Errorf("URL is %d bytes, max %d", len(raw), MaxURLBytes)
	}
	if i := indexOfControl(raw); i >= 0 {
		return fmt.Errorf("URL contains a control character at index %d", i)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("URL is not a valid URL")
	}
	if u.Opaque != "" {
		return fmt.Errorf("URL must be absolute, not opaque")
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("URL has no host")
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return nil
	case "http":
		if !isLoopbackHost(host) {
			return fmt.Errorf("scheme http is allowed only for loopback hosts")
		}
		return nil
	default:
		// The scheme is quoted; it is a short token from a bounded document and
		// NewError normalizes control characters out of the result.
		return fmt.Errorf("scheme must be https (or http for loopback), got %q", u.Scheme)
	}
}

// discovered is what the two discovery steps produce: everything the flow needs
// to authorize, redeem, and (maybe) register.
type discovered struct {
	meta   authServerMetadata
	scopes []string // scopes advertised by the resource, if any
}

// discover runs the full chain for serverURL: protected-resource metadata, then
// authorization-server metadata for whichever issuer that names.
func (p *OAuthProvider) discover(ctx context.Context) (discovered, error) {
	issuer, scopes, err := p.discoverResource(ctx)
	if err != nil {
		return discovered{}, err
	}
	meta, err := p.discoverAuthServer(ctx, issuer)
	if err != nil {
		return discovered{}, err
	}
	return discovered{meta: meta, scopes: scopes}, nil
}

// discoverResource fetches RFC 9728 protected-resource metadata and returns the
// issuer to use and any scopes the resource advertises.
//
// When the resource serves no such document, the server's own origin is used as
// the issuer. That fallback is not a shortcut: RFC 9728 is recent, most
// deployments predate it, and the pre-9728 convention — which the MCP spec
// itself started from — is that the resource and the authorization server are
// the same host. Refusing to proceed without a 9728 document would fail against
// most real servers, and the fallback gives up no security: an origin the user
// typed is a stronger statement of intent than a document that origin serves.
func (p *OAuthProvider) discoverResource(ctx context.Context) (issuer string, scopes []string, err error) {
	var meta protectedResourceMetadata
	found, err := p.fetchFirstDocument(ctx, wellKnownCandidates(p.serverURL, wellKnownProtectedResource), &meta)
	if err != nil {
		return "", nil, err
	}
	if !found {
		return p.origin, nil, nil
	}

	// RFC 9728 §3.3: the document must claim to be about the resource we asked
	// about. Compared as origins rather than as raw strings: the field is
	// specified as the resource identifier, and deployments disagree about
	// whether that includes the MCP path or a trailing slash, so a byte compare
	// rejects correct servers. The origin is the part that carries the security
	// meaning — it is what says "this document is about the host you are
	// talking to" — and it is what the token is keyed by.
	if meta.Resource != "" {
		got, err := CanonicalOrigin(meta.Resource)
		if err != nil {
			return "", nil, NewError(ClassFailed, "discover", "protected resource metadata has an invalid resource identifier", err)
		}
		if got != p.origin {
			return "", nil, NewError(ClassFailed, "discover", fmt.Sprintf(
				"protected resource metadata is for %q, but was served by %q", got, p.origin), nil)
		}
	}

	if len(meta.AuthorizationServers) == 0 {
		return "", nil, NewError(ClassFailed, "discover", "protected resource metadata names no authorization servers", nil)
	}
	// The first entry wins. The field is a preference-ordered list and this
	// client has no basis to prefer differently; trying each in turn would mean
	// a failed authorization against one server silently becoming an attempt
	// against another, which is a worse story than a clear failure.
	issuer = meta.AuthorizationServers[0]
	if err := checkEndpointURL(issuer); err != nil {
		return "", nil, NewError(ClassFailed, "discover", fmt.Sprintf("protected resource metadata authorization server: %s", err.Error()), nil)
	}
	return issuer, meta.ScopesSupported, nil
}

// discoverAuthServer fetches RFC 8414 metadata for issuer, falling back to the
// OpenID Connect discovery document, and validates the result against issuer.
func (p *OAuthProvider) discoverAuthServer(ctx context.Context, issuer string) (authServerMetadata, error) {
	candidates := append(
		wellKnownCandidates(issuer, wellKnownAuthServer),
		openIDCandidates(issuer)...,
	)

	var meta authServerMetadata
	found, err := p.fetchFirstDocument(ctx, candidates, &meta)
	if err != nil {
		return authServerMetadata{}, err
	}
	if !found {
		return authServerMetadata{}, NewError(ClassFailed, "discover", fmt.Sprintf(
			"no authorization server metadata found for issuer %q", issuer), nil)
	}
	if err := meta.validate(issuer); err != nil {
		return authServerMetadata{}, err
	}
	return meta, nil
}

// wellKnownCandidates builds the metadata URLs to try for base, in order.
//
// The path-insertion rule is the fiddly part of RFC 8414 §3.1 and RFC 9728
// §3.1, and it is easy to get backwards: the well-known segment goes between
// the host and the issuer's path, NOT at the end. For issuer
// "https://h/tenant/a" the URL is "https://h/.well-known/x/tenant/a", not
// "https://h/tenant/a/.well-known/x". That is what lets one host serve several
// issuers.
//
// Both are tried when the base has a path, because deployments get this wrong
// in both directions and a client that only implements the letter of the spec
// fails against servers that work fine in every other client.
func wellKnownCandidates(base, wellKnown string) []string {
	u, err := url.Parse(base)
	if err != nil {
		return nil
	}
	path := strings.Trim(u.Path, "/")

	root := *u
	root.Path = wellKnown
	root.RawQuery = ""
	root.Fragment = ""
	if path == "" {
		return []string{root.String()}
	}

	inserted := *u
	inserted.Path = wellKnown + "/" + path
	inserted.RawQuery = ""
	inserted.Fragment = ""
	// Insertion first: it is what the RFCs specify. The root form is the
	// fallback for servers that only ever serve one issuer and put the document
	// where it is easiest.
	return []string{inserted.String(), root.String()}
}

// openIDCandidates builds the OpenID Connect discovery URLs for issuer.
//
// OIDC Discovery §4 specifies the suffixed form — issuer + the well-known path
// — which is the opposite of RFC 8414's insertion. Both exist in the wild, so
// both are tried; the insertion form is included because RFC 8414 §3.1 defines
// it for the OIDC path too.
func openIDCandidates(issuer string) []string {
	u, err := url.Parse(issuer)
	if err != nil {
		return nil
	}
	path := strings.Trim(u.Path, "/")
	if path == "" {
		// With no path, the suffixed and inserted forms are the same URL.
		return wellKnownCandidates(issuer, wellKnownOpenIDConfig)
	}

	suffixed := *u
	suffixed.Path = "/" + path + wellKnownOpenIDConfig
	suffixed.RawQuery = ""
	suffixed.Fragment = ""
	return append([]string{suffixed.String()}, wellKnownCandidates(issuer, wellKnownOpenIDConfig)...)
}

// fetchFirstDocument tries each candidate in order and decodes the first one
// that exists into out. It reports whether any did.
//
// A 404 means "try the next"; anything else — a transport error, a 500, a body
// that will not parse — is fatal. That distinction is the point of the
// function: absence is a routing fact and is expected, while a server that
// answers badly is a server we must not quietly route around, because "route
// around the error" is how a client ends up authorizing against whatever
// responds.
func (p *OAuthProvider) fetchFirstDocument(ctx context.Context, candidates []string, out any) (bool, error) {
	for _, candidate := range candidates {
		if err := checkEndpointURL(candidate); err != nil {
			return false, NewError(ClassInvalidConfig, "discover", fmt.Sprintf("metadata URL: %s", err.Error()), nil)
		}
		found, err := p.fetchDocument(ctx, candidate, out)
		if err != nil {
			return false, err
		}
		if found {
			return true, nil
		}
	}
	return false, nil
}

// fetchDocument GETs one metadata URL and decodes it. It reports false, nil
// when the document is absent (404).
func (p *OAuthProvider) fetchDocument(ctx context.Context, endpoint string, out any) (bool, error) {
	fail := func(msg string, err error) error {
		return NewError(ClassFailed, "discover", msg, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false, fail("could not build the metadata request", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		// The transport error text routinely quotes the URL. It carries no
		// secret here — a metadata URL has no credentials in it — but Error
		// never renders a cause anyway, so this stays inspectable without being
		// printable.
		return false, fail(fmt.Sprintf("could not fetch metadata from %q", redactURL(endpoint)), err)
	}
	defer func() {
		// The body is drained by the bounded read below or abandoned here; the
		// close is what returns the connection either way.
		_ = resp.Body.Close()
	}()

	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return false, fail(fmt.Sprintf("metadata request to %q returned HTTP %d", redactURL(endpoint), resp.StatusCode), nil)
	}
	if err := checkJSONContentType(resp.Header.Get("Content-Type")); err != nil {
		return false, fail(fmt.Sprintf("metadata request to %q: %s", redactURL(endpoint), err.Error()), nil)
	}
	if err := decodeBoundedJSON(resp.Body, out); err != nil {
		return false, fail(fmt.Sprintf("metadata from %q: %s", redactURL(endpoint), err.Error()), err)
	}
	return true, nil
}

// checkJSONContentType reports whether a response claims to be JSON.
//
// Checked because a metadata endpoint that answers with HTML is a login page or
// a captive portal, not a document, and parsing it produces a confusing error
// three layers down instead of an accurate one here.
func checkJSONContentType(header string) error {
	if header == "" {
		return fmt.Errorf("response has no Content-Type; want application/json")
	}
	mediaType, _, err := mime.ParseMediaType(header)
	if err != nil {
		return fmt.Errorf("response has an unparseable Content-Type")
	}
	if mediaType != "application/json" {
		return fmt.Errorf("response Content-Type is %q; want application/json", mediaType)
	}
	return nil
}

// decodeBoundedJSON reads at most maxDocumentBytes from r, rejects
// over-nested JSON, and unmarshals into out.
//
// The order matters and is the whole reason this is a function rather than a
// json.NewDecoder call. Bound first: a json.Decoder streaming an unbounded body
// will happily allocate until the process dies, so the bound has to be on the
// reader, not on the parser. Then check depth on the bytes: encoding/json
// recurses on nesting, and a document of ten thousand open braces is a stack
// overflow, which is not a panic a caller can recover into a typed error.
// Only then parse.
func decodeBoundedJSON(r io.Reader, out any) error {
	raw, err := io.ReadAll(limits.BoundedReader(r, maxDocumentBytes))
	if err != nil {
		var overLimit *limits.OverLimitError
		if errors.As(err, &overLimit) {
			return fmt.Errorf("document exceeds the %d byte limit", maxDocumentBytes)
		}
		return fmt.Errorf("document could not be read")
	}
	if err := limits.CheckJSONDepth(raw, maxDocumentDepth); err != nil {
		return fmt.Errorf("document nesting exceeds the %d level limit", maxDocumentDepth)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		// The parse error quotes the offending input, which is attacker
		// controlled. Say what happened, not what it said.
		return fmt.Errorf("document is not valid JSON")
	}
	return nil
}

// redactURL reduces a URL to scheme://host/path for use in an error message,
// dropping query and userinfo.
//
// Discovery URLs carry no secrets today, so this is belt and braces — but the
// same helper is used by the token and registration paths, where a query string
// legitimately contains a code, and where an error message is exactly what gets
// pasted into a bug report. Making the safe rendering the only rendering means
// nobody has to remember which URLs are which.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<invalid url>"
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	return u.String()
}
