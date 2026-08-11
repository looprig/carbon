// This file implements PKCE (RFC 7636) and the CSRF state parameter.
//
// Both are secret-bearing, and both follow the discipline the rest of the
// package sets: the material sits in a closure (see secret in tokenstore.go),
// the type renders redacted through fmt.Formatter for every verb, and reading
// the value is a named, greppable act. They are unexported because neither is
// part of any contract a caller can usefully hold — they are born, used once,
// and dropped inside a single call to OAuthProvider.Token.
//
// # Why S256 only
//
// RFC 7636 defines two challenge methods. "plain" sends the verifier itself as
// the challenge, which means an attacker who can read the authorization request
// — the very thing PKCE exists to defend against, since that request travels
// through the user's browser and the system URL handler — learns the verifier
// and can complete the exchange. It defends against nothing. RFC 7636 §7.2 says
// clients MUST use S256 if they can, and a Go client always can.
//
// So there is no "plain" branch here, and no configuration to add one. A server
// that advertises only "plain" is refused (see selectChallengeMethod): the
// failure mode of a downgrade is silent and total, and a client that negotiates
// its own security down on a server's say-so is not a security feature.
//
// # Why the verifier is a secret
//
// The verifier is the proof that whoever redeems the code is whoever requested
// it. Leak it alongside a code and the code is redeemable by the leaker. It is
// as sensitive as the code it protects, and is treated as such: it never enters
// an error, a log, or the authorization URL — only the token request.
//
// The state is CSRF material rather than a credential, but the same treatment
// costs nothing and closes the same class: state does reach a URL (it must —
// the server echoes it back), but a state in a log is a state an attacker can
// forge a callback with.

package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"io"
	"slices"
)

// PKCE parameters. The names are the wire values from RFC 7636 §4.3.
const (
	// challengeMethodS256 is the only method this client will use.
	challengeMethodS256 = "S256"
	// challengeMethodPlain is never used. It is named so that the refusal can
	// say what it refused, and so the constant is greppable to anyone
	// wondering whether we support it. We do not.
	challengeMethodPlain = "plain"
)

// Verifier length bounds from RFC 7636 §4.1. The verifier this package
// generates is always verifierBytes of entropy rendered as unpadded base64url,
// which is 43 characters — the minimum, and 256 bits, which is the maximum
// entropy the minimum length can carry.
const (
	minVerifierLen = 43
	maxVerifierLen = 128
	// verifierBytes is the raw entropy behind a generated verifier. 32 bytes
	// of base64url is exactly 43 characters, so this hits the RFC's floor with
	// no padding and no waste. RFC 7636 §7.1 asks for at least 256 bits.
	verifierBytes = 32
	// stateBytes is the entropy behind the CSRF state. It has the same job as
	// the verifier — be unguessable — and gets the same budget.
	stateBytes = 32
)

// pkce is one authorization request's PKCE material.
//
// It is created per flow by newPKCE and lives no longer than the flow. The
// verifier is secret; the challenge derived from it is not — it is a hash, it
// goes in the authorization URL by design, and it is safe to log (we do not,
// because there is no reason to).
type pkce struct {
	verifier  *secret // SECRET — reachable only via Verifier()
	challenge string
}

// newPKCE generates a fresh verifier from crypto/rand and derives its S256
// challenge.
//
// The verifier is base64url of 32 random bytes, which lands in the RFC's
// unreserved charset ([A-Za-z0-9-._~]) by construction: base64url's alphabet is
// letters, digits, '-' and '_', all of which are unreserved, and the encoding
// is unpadded so no '=' appears. That is why there is no "now sanitize it" step
// — the charset is a property of the encoding, not something checked after the
// fact.
func newPKCE() (pkce, error) {
	raw := make([]byte, verifierBytes)
	// crypto/rand.Read is documented never to return a short read without an
	// error, and on modern Go it cannot fail at all; the check stays because
	// "cannot fail" is not a thing to assume about entropy.
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		return pkce{}, NewError(ClassFailed, "pkce", "could not read random bytes for the PKCE verifier", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(raw)
	return pkce{
		verifier:  newSecret(verifier),
		challenge: s256Challenge(verifier),
	}, nil
}

// Verifier returns the code verifier. This is secret material: it goes in the
// token request and nowhere else.
func (p pkce) Verifier() string { return p.verifier.value() }

// Challenge returns the S256 code challenge, which is not secret.
func (p pkce) Challenge() string { return p.challenge }

// s256Challenge computes BASE64URL(SHA256(ASCII(verifier))), the S256
// transformation of RFC 7636 §4.2. It is a plain function so the RFC's own test
// vector (§B.1) can be run against it directly.
func s256Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// String renders the material with the verifier redacted.
func (p pkce) String() string {
	return fmt.Sprintf("auth.pkce{verifier:%s, method:%s}", presence(p.verifier), challengeMethodS256)
}

// GoString renders redacted text for a direct caller; Format is what serves
// %#v. See TokenSet.GoString.
func (p pkce) GoString() string { return p.String() }

// Format routes every fmt verb through the redacted rendering; see
// TokenSet.Format for why Stringer alone is insufficient.
func (p pkce) Format(f fmt.State, verb rune) {
	// fmt.Formatter cannot report a write error and fmt ignores them anyway;
	// discarding is the contract here, not an oversight.
	_, _ = io.WriteString(f, p.String())
}

// selectChallengeMethod picks the PKCE method to use against a server
// advertising supported, and refuses anything but S256.
//
// An empty supported is accepted as S256. This is the one piece of tolerance
// here and it is deliberate: code_challenge_methods_supported is RECOMMENDED,
// not REQUIRED, by RFC 8414, so a server omitting it is common and says nothing
// about what it implements. Sending an S256 challenge to a server that ignores
// PKCE entirely costs nothing — the parameter is simply unused — whereas
// refusing every server that omits an optional metadata field would make the
// client useless against a large part of the real world. What we never do is
// the opposite trade: downgrade because a server asked us to.
func selectChallengeMethod(supported []string) (string, error) {
	if len(supported) == 0 {
		return challengeMethodS256, nil
	}
	if slices.Contains(supported, challengeMethodS256) {
		return challengeMethodS256, nil
	}
	// The server named its methods and S256 was not among them. The only
	// alternative it can be offering is one we will not use.
	return "", NewError(ClassFailed, "pkce", fmt.Sprintf(
		"authorization server does not support the %s code challenge method; refusing to downgrade to %s",
		challengeMethodS256, challengeMethodPlain), nil)
}

// state is the CSRF state parameter: an unguessable value sent on the
// authorization request and required to come back unchanged on the callback.
//
// Without it, an attacker who can reach the redirect listener can feed it an
// authorization code of the attacker's own, and the client will exchange it and
// store the attacker's tokens — the user is then silently operating as the
// attacker (RFC 6749 §10.12). The listener is on loopback, which raises the bar
// but does not close it: any local process, and any web page the user visits
// that can make a request to 127.0.0.1, can try.
type state struct {
	value *secret // SECRET — reachable only via Value()
}

// newState generates a state from crypto/rand.
func newState() (state, error) {
	raw := make([]byte, stateBytes)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		return state{}, NewError(ClassFailed, "state", "could not read random bytes for the CSRF state", err)
	}
	return state{value: newSecret(base64.RawURLEncoding.EncodeToString(raw))}, nil
}

// Value returns the state. It goes in the authorization URL and is compared
// against the callback's; it does not belong in a log.
func (s state) Value() string { return s.value.value() }

// Matches reports whether got is the state this flow sent.
//
// The comparison is constant-time. The timing channel in a naive == is narrow —
// an attacker would need many callback attempts against a listener that answers
// exactly one — but "narrow" is an argument for not relying on the analysis
// being right, and subtle.ConstantTimeCompare costs nothing here.
//
// A zero state matches nothing: fail closed, so a flow that somehow reached the
// callback without generating a state cannot be talked into accepting one.
func (s state) Matches(got string) bool {
	want := s.value.value()
	if want == "" || got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}

// String renders the state redacted.
func (s state) String() string {
	return fmt.Sprintf("auth.state{value:%s}", presence(s.value))
}

// GoString renders redacted text for a direct caller; Format is what serves
// %#v. See TokenSet.GoString.
func (s state) GoString() string { return s.String() }

// Format routes every fmt verb through the redacted rendering; see
// TokenSet.Format for why Stringer alone is insufficient.
func (s state) Format(f fmt.State, verb rune) {
	// fmt.Formatter cannot report a write error and fmt ignores them anyway;
	// discarding is the contract here, not an oversight.
	_, _ = io.WriteString(f, s.String())
}

// authCode is an authorization code returned on the callback.
//
// It is short-lived, single-use material that is nevertheless a credential:
// combined with the verifier it mints tokens. It gets the same treatment as
// everything else here, which matters most for errors — a failed exchange is
// exactly the moment someone reaches for "log the code so we can see what
// happened".
type authCode struct {
	value *secret // SECRET — reachable only via Value()
}

// newAuthCode wraps a code from a callback.
func newAuthCode(v string) authCode { return authCode{value: newSecret(v)} }

// Value returns the code. It goes in the token request and nowhere else.
func (c authCode) Value() string { return c.value.value() }

// Valid reports whether a code is present.
func (c authCode) Valid() bool { return c.value != nil }

// String renders the code redacted.
func (c authCode) String() string {
	return fmt.Sprintf("auth.authCode{value:%s}", presence(c.value))
}

// GoString renders redacted text for a direct caller; Format is what serves
// %#v. See TokenSet.GoString.
func (c authCode) GoString() string { return c.String() }

// Format routes every fmt verb through the redacted rendering; see
// TokenSet.Format for why Stringer alone is insufficient.
func (c authCode) Format(f fmt.State, verb rune) {
	// fmt.Formatter cannot report a write error and fmt ignores them anyway;
	// discarding is the contract here, not an oversight.
	_, _ = io.WriteString(f, c.String())
}
