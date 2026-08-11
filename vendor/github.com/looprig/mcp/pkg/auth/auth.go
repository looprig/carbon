// Package auth provides the reusable OAuth and bearer-token contracts an
// application plugs into when connecting to a network MCP server.
//
// The package is a leaf: it depends on the standard library and on the module's
// dependency-free internal/limits helpers, and on nothing else in the module.
// It defines seams, not policy — where tokens live (TokenStore), how a browser
// is opened (BrowserOpener), how non-OAuth credentials are supplied
// (HeaderProvider), what an auth posture looks like from outside (Status), and
// how auth failures are classified (Class). The OAuth flow that drives these
// seams, and the transports that consume them, live elsewhere; nothing here
// speaks HTTP.
//
// # Secrets
//
// The module's standing rule is that token values, client secrets,
// authorization codes, verifiers, and bearer headers never enter events,
// catalogs, fingerprints, or logs. This package is where that rule is
// mechanized, because it is the only package that holds such material.
//
// Every type here that holds a secret — TokenSet, Header — keeps it in an
// unexported field and exposes it only through a named accessor, so that:
//
//   - no reflection-based encoder can reach it: encoding/json refuses via
//     MarshalJSON, and encoding/gob refuses because there is nothing exported
//     to encode;
//   - fmt renders it redacted for every verb, because these types implement
//     fmt.Formatter — Stringer alone would cover only %v, %s, %q, %x and %X
//     and let %d and friends fall through to reflection, which reads unexported
//     fields;
//   - reading it is a deliberate, greppable act.
//
// Values are non-secret metadata (expiry, scopes, header names, auth state) and
// are exported normally: Status in particular is designed to be logged as-is.
//
// The dividing line is that leaking must require intent. Accessors make
// intentional use easy and accidental use hard.
//
// This holds even where methods cannot reach. fmt renders a value held in
// another struct's unexported field by reflection, skipping Formatter,
// Stringer and GoStringer alike, and %p and %w bypass those methods outright —
// so the secrets additionally sit behind a pointer, which fmt's reflection
// prints as an address rather than following. Redaction is therefore a
// property of the layout, not only of the methods.
package auth

import (
	"context"
	"fmt"
	"io"
	"strings"
)

// BrowserOpener opens a URL in the user's browser, so an OAuth
// authorization-code flow can reach the resource owner.
//
// The module supplies no implementation: what "open a browser" means is a
// property of the application. A desktop app shells out to the platform opener,
// a TUI prints the URL for the user to copy, an SSH session may only be able to
// do the latter, and a headless service should refuse outright rather than
// block on a human who is not there. That last case is why this is an interface
// and not a helper: refusing is a legitimate implementation.
//
// Implementations must honor ctx, and must return promptly — opening a browser
// means handing the URL off, never waiting for the user to finish.
//
// The URL carries authorization parameters. It is not for logging.
type BrowserOpener interface {
	// OpenURL presents url to the user, or returns an error if it cannot.
	OpenURL(ctx context.Context, url string) error
}

// HeaderProvider supplies credential headers for an outbound request. It is the
// seam for everything that is not the OAuth flow this package drives: a static
// bearer token from an environment variable, an API key, a signed header from a
// cloud credential chain, or a token minted by an application's own broker.
//
// Headers is called per request rather than once per connection, so an
// implementation backed by an expiring credential can refresh transparently.
// Implementations must honor ctx — minting a credential may mean I/O — and must
// be safe for concurrent use, since a connection may have several requests in
// flight.
//
// Returned values are secret. A transport writes them onto a request and
// nothing else: they never reach an event, a log, or an error.
type HeaderProvider interface {
	// Headers returns the headers to attach, or an error if they cannot be
	// obtained. An empty result is valid and means "add nothing".
	Headers(ctx context.Context) ([]Header, error)
}

// Header is one credential header.
//
// Value is secret and is reachable only through Value(); Name is not. Construct
// one with NewHeader. The zero value is an invalid header — Validate rejects it
// — and renders as redacted like any other.
type Header struct {
	name  string
	value *secret // SECRET — reachable only via Value()
}

// NewHeader builds a Header. Call Validate before using one built from
// configuration or from any other untrusted source.
func NewHeader(name, value string) Header {
	return Header{name: name, value: newSecret(value)}
}

// Name returns the header's field name, which is not secret.
func (h Header) Name() string { return h.name }

// Value returns the header's value. This is secret material: write it to a
// request, and nothing else.
func (h Header) Value() string { return h.value.value() }

// Validate reports whether h is a well-formed HTTP header. Violations are
// returned as *Error with class ClassInvalidConfig.
//
// This is a header-injection check, not a style check. A newline in a value, or
// a colon or space in a name, lets whoever controls that string append headers
// of their own to the request — so the check is against the RFC 9110 grammar
// (token for the name, visible ASCII plus space and tab for the value) and
// fails closed on anything outside it.
//
// An empty value is allowed: it is a legal header, and a provider legitimately
// emits one to unset something. An empty name is not.
func (h Header) Validate() error {
	fail := func(msg string) error {
		return NewError(ClassInvalidConfig, "validate", msg, nil)
	}

	if h.name == "" {
		return fail("header name is empty")
	}
	for i := 0; i < len(h.name); i++ {
		if !isTokenByte(h.name[i]) {
			// The name is not secret, so quoting it is safe and useful.
			return fail(fmt.Sprintf("header name %q contains an invalid byte 0x%02x at index %d", h.name, h.name[i], i))
		}
	}
	value := h.value.value()
	for i := 0; i < len(value); i++ {
		if !isFieldValueByte(value[i]) {
			// The value IS secret: report the offense and its position, never
			// the byte or the value. A validation error is precisely the kind
			// of thing that gets logged.
			return fail(fmt.Sprintf("header %q has an invalid value: byte at index %d is not a visible ASCII character", h.name, i))
		}
	}
	return nil
}

// String renders the header with its value redacted.
func (h Header) String() string {
	name := h.name
	if name == "" {
		name = "<empty>"
	}
	return fmt.Sprintf("auth.Header{name:%s, value:%s}", name, presence(h.value))
}

// GoString renders redacted text for a direct caller; Format is what serves
// %#v. See TokenSet.GoString.
func (h Header) GoString() string { return h.String() }

// Format routes every fmt verb through the redacted rendering; see
// TokenSet.Format for why Stringer alone is insufficient.
func (h Header) Format(f fmt.State, verb rune) {
	// fmt.Formatter cannot report a write error and fmt ignores them
	// anyway; discarding is the contract here, not an oversight.
	_, _ = io.WriteString(f, h.String())
}

// MarshalJSON always fails, for the reason TokenSet.MarshalJSON does: a header
// reaching a JSON encoder is a leak, and refusing is the loud answer.
func (h Header) MarshalJSON() ([]byte, error) {
	return nil, fmt.Errorf("auth.Header: %w", ErrMarshalRefused)
}

// UnmarshalJSON always fails; see TokenSet.UnmarshalJSON.
func (h *Header) UnmarshalJSON([]byte) error {
	return fmt.Errorf("auth.Header: %w", ErrMarshalRefused)
}

// tcharPunctuation is the punctuation permitted in the RFC 9110 "tchar"
// production, which defines what may appear in a header field name.
const tcharPunctuation = "!#$%&'*+-.^_`|~"

// isTokenByte reports whether c is legal in a header field name (RFC 9110
// token).
func isTokenByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	default:
		return strings.IndexByte(tcharPunctuation, c) >= 0
	}
}

// isFieldValueByte reports whether c is legal in a header field value: visible
// ASCII, space, or horizontal tab. Notably excludes CR and LF, which is the
// point.
func isFieldValueByte(c byte) bool {
	return c == '\t' || (c >= 0x20 && c <= 0x7e)
}
