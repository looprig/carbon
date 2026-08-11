// This file implements RFC 7591 dynamic client registration and defines the
// credentials it produces.
//
// # Why ClientCredentials is a type and not two string fields
//
// The obvious API is `OAuthConfig{ClientID, ClientSecret string}`. It is also a
// hole straight through this package's redaction design: an OAuthConfig is
// exactly the kind of value an application builds at startup, keeps in its own
// config struct, and logs when someone turns on debug. A bare ClientSecret
// string field would be printed by every one of those paths, and none of this
// package's machinery could stop it — Formatter cannot help a string.
//
// So the secret goes where every other secret in this package goes: behind a
// closure, inside a type that renders redacted for every verb and refuses to
// marshal. The client ID stays exported through an accessor because it is not
// secret — RFC 6749 §2.2 is explicit that a client identifier "is not a secret;
// it is exposed to the resource owner" — and because it is half of a Key.
//
// # Why registration persists nothing
//
// RegisterClient returns credentials and stores them nowhere. A registered
// client is durable state: registering again on the next run leaks a client
// record on the authorization server every time, and some servers rate-limit or
// refuse repeat registration. But where that state belongs is an application
// question — the same question TokenStore answers for tokens — and this package
// does not get to decide it. OAuthProvider.Credentials is how a caller reads
// what registration produced, so it can persist it and pass it back next time.

package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// maxRegistrationRequestBytes bounds the registration document we send. It is a
// sanity bound on our own output — a caller supplying a pathological client name
// should get an error, not a surprising request.
const maxRegistrationRequestBytes = 1 << 16

// ClientCredentials is an OAuth client's identity: a public client identifier
// and, for a confidential client, the secret that authenticates it.
//
// The secret is reachable only through Secret(). Construct with
// NewClientCredentials. The zero value is an unregistered client — Valid
// reports false — which is what an OAuthConfig carries when it wants dynamic
// registration.
//
// A ClientCredentials is a value and is safe to copy; it is immutable after
// construction.
type ClientCredentials struct {
	id     string
	secret *secret // SECRET — reachable only via Secret()
}

// NewClientCredentials builds a ClientCredentials. A public client — which is
// what a native application using PKCE is — has no secret, so an empty secret
// is normal and valid.
func NewClientCredentials(id, secret string) ClientCredentials {
	return ClientCredentials{id: id, secret: newSecret(secret)}
}

// ID returns the client identifier, which is not secret.
func (c ClientCredentials) ID() string { return c.id }

// Secret returns the client secret, which is empty for a public client. This is
// secret material: it goes in a token request's Authorization header and
// nowhere else.
func (c ClientCredentials) Secret() string { return c.secret.value() }

// Valid reports whether these credentials name a client at all. It says nothing
// about whether the authorization server still knows that client.
func (c ClientCredentials) Valid() bool { return c.id != "" }

// Confidential reports whether the client authenticates with a secret.
func (c ClientCredentials) Confidential() bool { return c.secret != nil }

// String renders the credentials with the secret redacted and the ID shown; see
// Key.String for why showing a client ID is correct.
func (c ClientCredentials) String() string {
	id := c.id
	if id == "" {
		id = "<unregistered>"
	}
	return fmt.Sprintf("auth.ClientCredentials{id:%s, secret:%s}", id, presence(c.secret))
}

// GoString renders redacted text for a direct caller; Format is what serves
// %#v. See TokenSet.GoString.
func (c ClientCredentials) GoString() string { return c.String() }

// Format routes every fmt verb through the redacted rendering; see
// TokenSet.Format for why Stringer alone is insufficient.
func (c ClientCredentials) Format(f fmt.State, verb rune) {
	// fmt.Formatter cannot report a write error and fmt ignores them anyway;
	// discarding is the contract here, not an oversight.
	_, _ = io.WriteString(f, c.String())
}

// MarshalJSON always fails; see TokenSet.MarshalJSON for why refusing beats
// redacting. A caller persisting credentials uses ID() and Secret().
func (c ClientCredentials) MarshalJSON() ([]byte, error) {
	return nil, fmt.Errorf("auth.ClientCredentials: %w", ErrMarshalRefused)
}

// UnmarshalJSON always fails; see TokenSet.UnmarshalJSON.
func (c *ClientCredentials) UnmarshalJSON([]byte) error {
	return fmt.Errorf("auth.ClientCredentials: %w", ErrMarshalRefused)
}

// registrationRequest is the RFC 7591 §2 client metadata this client sends.
//
// The values are fixed rather than configurable, because every one of them is a
// statement about what this code does, not a preference:
//
//   - grant_types: this package implements exactly authorization_code and
//     refresh_token.
//   - response_types: PKCE authorization-code flow means "code". Never "token"
//     — that is the implicit flow, which OAuth 2.1 removes.
//   - token_endpoint_auth_method "none": we are a public client. A native
//     application cannot keep a secret (RFC 8252 §8.5), so claiming to be
//     confidential would be a lie that buys nothing; PKCE is what actually
//     authenticates the exchange. A server may still issue a secret, and we
//     honor it if it does — see registrationResponse.
type registrationRequest struct {
	ClientName              string   `json:"client_name,omitempty"`
	RedirectURIs            []string `json:"redirect_uris"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	Scope                   string   `json:"scope,omitempty"`
}

// registrationResponse is the RFC 7591 §3.2.1 response, reduced to what we act
// on.
type registrationResponse struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	// Error and ErrorDescription carry an RFC 7591 §3.2.2 failure, which is
	// returned with a 400 rather than a 200.
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// registerClient performs dynamic client registration against endpoint and
// returns the credentials the server issued.
//
// redirectURI is the real, already-bound loopback URI — see newRedirectServer
// for why the port is known before this runs.
func (p *OAuthProvider) registerClient(ctx context.Context, endpoint, redirectURI string, scopes []string) (ClientCredentials, error) {
	fail := func(msg string, err error) (ClientCredentials, error) {
		return ClientCredentials{}, NewError(ClassFailed, "register", msg, err)
	}

	if err := checkEndpointURL(endpoint); err != nil {
		return ClientCredentials{}, NewError(ClassInvalidConfig, "register",
			fmt.Sprintf("registration endpoint: %s", err.Error()), nil)
	}

	body, err := json.Marshal(registrationRequest{
		ClientName:              p.clientName,
		RedirectURIs:            []string{redirectURI},
		GrantTypes:              []string{grantAuthorizationCode, grantRefreshToken},
		ResponseTypes:           []string{"code"},
		TokenEndpointAuthMethod: "none",
		Scope:                   strings.Join(scopes, " "),
	})
	if err != nil {
		return fail("could not build the registration request", err)
	}
	if len(body) > maxRegistrationRequestBytes {
		return fail(fmt.Sprintf("registration request is %d bytes, max %d", len(body), maxRegistrationRequestBytes), nil)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fail("could not build the registration request", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return fail(fmt.Sprintf("could not reach the registration endpoint %q", redactURL(endpoint)), err)
	}
	defer func() { _ = resp.Body.Close() }()

	// RFC 7591 §3.2.1 specifies 201; servers return 200 too, and the difference
	// carries no meaning we act on.
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		var failure registrationResponse
		// The error body is best-effort: a server that failed to register us
		// may well not explain itself in JSON, and the status is already a
		// complete story.
		if err := decodeBoundedJSON(resp.Body, &failure); err == nil && failure.Error != "" {
			return fail(fmt.Sprintf("registration was refused: %s", failure.Error), nil)
		}
		return fail(fmt.Sprintf("registration endpoint returned HTTP %d", resp.StatusCode), nil)
	}
	if err := checkJSONContentType(resp.Header.Get("Content-Type")); err != nil {
		return fail(fmt.Sprintf("registration response: %s", err.Error()), nil)
	}

	var registered registrationResponse
	if err := decodeBoundedJSON(resp.Body, &registered); err != nil {
		return fail(fmt.Sprintf("registration response: %s", err.Error()), err)
	}
	if registered.ClientID == "" {
		return fail("registration response has no client_id", nil)
	}
	return NewClientCredentials(registered.ClientID, registered.ClientSecret), nil
}
