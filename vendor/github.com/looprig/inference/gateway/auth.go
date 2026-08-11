package gateway

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// maxAuthorizationHeaderBytes bounds the Authorization header value this
// package will inspect. It is a defensive header-size bound (the "header
// bounds" step of the request pipeline): a pathological, extremely long
// header is rejected as an authentication failure before any comparison work
// is done, rather than being handed to subtle.ConstantTimeCompare or
// otherwise processed.
const maxAuthorizationHeaderBytes = 4096

// bearerPrefix is the only Authorization scheme this package recognizes.
const bearerPrefix = "Bearer "

// Authenticator authenticates one inbound HTTP request against the
// gateway's own local, inbound credential. This is a distinct, separate
// concept from inference/auth's Authenticator: that package authorizes
// OUTBOUND requests to a provider with provider credentials; this interface
// authenticates INBOUND requests reaching the gateway's own HTTP surface,
// and is scoped to this package.
type Authenticator interface {
	// Authenticate reports whether req carries a valid credential. It must
	// not consume req.Body. A non-nil error is always an
	// *AuthenticationError: this interface deliberately has exactly one
	// failure mode so a caller never needs to distinguish "why" auth failed.
	Authenticate(req *http.Request) error
}

// AuthenticationError reports that a request failed gateway-local
// authentication: a missing Authorization header, a non-Bearer scheme, an
// oversized header, or a token that does not match. Every one of those
// causes is reported identically -- by design, AuthenticationError carries
// no distinguishing detail -- so a response body or log line built from this
// error can never leak *why* authentication failed, only that it did.
type AuthenticationError struct{}

func (e *AuthenticationError) Error() string { return "gateway: authentication failed" }

// staticTokenAuthenticator is an Authenticator that compares the request's
// Bearer token against one fixed token using a constant-time comparison.
type staticTokenAuthenticator struct {
	token []byte
}

// StaticToken returns an Authenticator that requires
// "Authorization: Bearer <token>" with token compared to the supplied value
// using crypto/subtle.ConstantTimeCompare. token is copied; the caller's
// string is not retained.
//
// StaticToken does not generate token: that is the responsibility of
// whatever constructs a Server (a later task) and injects the generated,
// per-process secret here.
func StaticToken(token string) Authenticator {
	return &staticTokenAuthenticator{token: []byte(token)}
}

// Authenticate implements Authenticator. Every failure path -- missing
// header, wrong scheme, oversized header, wrong length, or wrong bytes --
// returns the same *AuthenticationError, and the comparison against the
// configured token is constant-time whenever both sides are compared at all.
func (a *staticTokenAuthenticator) Authenticate(req *http.Request) error {
	if !checkBearerToken(req.Header.Get("Authorization"), a.token) {
		return &AuthenticationError{}
	}
	return nil
}

// checkBearerToken reports whether header is a well-formed
// "Bearer <token>" Authorization value whose token matches want, using a
// constant-time comparison. Every rejection path -- oversized header, wrong
// scheme, wrong length, wrong bytes -- is indistinguishable to a caller: this
// function returns only a bool, never a reason.
//
// It is the single shared implementation of gateway-local bearer-token
// checking, used by both staticTokenAuthenticator (this file) and Server's
// independent authMiddleware (server.go) -- those two authentication paths
// are deliberately separate concepts (see server.go's package doc for why),
// but the security-sensitive parsing/comparison mechanics must not be
// re-derived twice.
func checkBearerToken(header string, want []byte) bool {
	if len(header) > maxAuthorizationHeaderBytes {
		return false
	}
	if !strings.HasPrefix(header, bearerPrefix) {
		return false
	}
	supplied := []byte(header[len(bearerPrefix):])

	// A length mismatch is checked before the constant-time compare because
	// subtle.ConstantTimeCompare requires equal-length inputs (it reports
	// unequal immediately, without comparing, when lengths differ) -- so this
	// branch adds no exploitable timing signal beyond what the length of the
	// attacker-supplied header already reveals, and is in fact redundant with
	// the identical short-circuit ConstantTimeCompare performs internally.
	if len(supplied) != len(want) {
		return false
	}
	return subtle.ConstantTimeCompare(supplied, want) == 1
}
