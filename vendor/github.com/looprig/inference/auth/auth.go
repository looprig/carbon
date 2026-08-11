// Package auth provides the legacy inference authorization facade. New
// credential-backed callers should use credentials/httpauth directly; the
// constructors here remain source-compatible wrappers for static API keys and
// explicit unauthenticated requests.
package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/looprig/credentials/httpauth"
	"github.com/looprig/secrets"
)

// APIKey is a bearer/API-key secret. A named type prevents a base URL from
// being passed where a key belongs, and keeps the old provider constructor API.
type APIKey string

// Key returns an Authorizer that sets "Authorization: Bearer <k>".
func Key(k APIKey) Authenticator {
	return staticHeader("Authorization", "Bearer "+string(k))
}

// Header returns an Authorizer that sets an arbitrary header (for example
// "x-api-key") to the key value.
func Header(k APIKey, name string) Authenticator {
	return staticHeader(name, string(k))
}

// None returns an explicit no-credentials authorizer. Unlike a nil
// authorizer, it is safe to pass through the legacy transport path.
func None() Authenticator { return legacyNoneAuth{} }

// staticHeader delegates valid values to credentials/httpauth. Empty values
// were accepted by the legacy constructors, so that one compatibility corner
// retains the old header-setting behavior. Invalid or oversized values produce
// a redaction-safe authorizer error instead of retaining untrusted material.
func staticHeader(name, value string) Authenticator {
	if _, err := secrets.New([]byte(value)); err != nil && !errors.Is(err, secrets.ErrEmptySecret) {
		return failedAuthorizer{err: err}
	}
	// Legacy static authenticators intentionally do not inspect context. The
	// net/http boundary reports a canceled request as NetworkError, preserving
	// the historical inference/provider behavior. Call-scoped callers should
	// use credentials/httpauth directly for typed authorization cancellation.
	return legacyHeaderAuth{name: name, value: value}
}

type failedAuthorizer struct{ err error }

func (a failedAuthorizer) Authorize(context.Context, *http.Request) error { return a.err }
func (a failedAuthorizer) String() string                                 { return "auth: authorization unavailable" }
func (a failedAuthorizer) GoString() string                               { return a.String() }
func (a failedAuthorizer) Format(state fmt.State, _ rune)                 { _, _ = state.Write([]byte(a.String())) }
func (a failedAuthorizer) LogValue() slog.Value                           { return slog.StringValue(a.String()) }

// legacyHeaderAuth is used only for the empty-value compatibility case. It
// deliberately stores no non-empty secret and mirrors httpauth's replacement
// semantics so requests never accumulate stale values.
type legacyHeaderAuth struct{ name, value string }

func (a legacyHeaderAuth) Authorize(ctx context.Context, request *http.Request) error {
	if request == nil {
		return httpauth.ErrNilRequest
	}
	_ = ctx
	if a.name == "" {
		return httpauth.ErrInvalidHeaderName
	}
	if request.Header == nil {
		request.Header = make(http.Header)
	}
	for key := range request.Header {
		if strings.EqualFold(key, a.name) {
			delete(request.Header, key)
		}
	}
	request.Header.Set(a.name, a.value)
	return nil
}

type legacyNoneAuth struct{}

func (legacyNoneAuth) Authorize(context.Context, *http.Request) error { return nil }

func (a legacyHeaderAuth) String() string                 { return "auth.header(REDACTED)" }
func (a legacyHeaderAuth) GoString() string               { return a.String() }
func (a legacyHeaderAuth) Format(state fmt.State, _ rune) { _, _ = state.Write([]byte(a.String())) }
func (a legacyHeaderAuth) LogValue() slog.Value           { return slog.StringValue(a.String()) }

var (
	_ fmt.Stringer   = failedAuthorizer{}
	_ fmt.GoStringer = failedAuthorizer{}
	_ slog.LogValuer = failedAuthorizer{}
	_ fmt.Stringer   = legacyHeaderAuth{}
	_ fmt.GoStringer = legacyHeaderAuth{}
	_ slog.LogValuer = legacyHeaderAuth{}
)
