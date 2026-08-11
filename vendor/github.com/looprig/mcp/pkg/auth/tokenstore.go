// This file defines the token-persistence contract: what identifies a token
// (Key), what a token is (TokenSet), where tokens live (TokenStore), and a
// reference store for tests and deliberately-ephemeral applications
// (MemoryStore).
//
// TokenSet's fields are unexported. That is the whole secret-safety design, and
// it is worth stating why it beats the obvious alternative of exported fields
// plus redacting String/GoString/MarshalJSON methods:
//
//   - Redacting methods only cover the paths that call them. Unexported fields
//     also close the paths that do not: encoding/json and encoding/gob have
//     nothing to reflect over.
//   - `set.Access` as a field is a leak one keystroke away, and reviewing for
//     it means reviewing every use forever. `set.Access()` as a method is the
//     same keystroke, but it is greppable: the audit question "who reads token
//     material?" has an exact, finite answer.
//   - Unexported slices make the aliasing bug class structurally impossible.
//     A caller cannot hold a reference into a stored TokenSet's scopes, because
//     the only way out is Scopes(), which clones.
//
// Unexported fields are necessary but NOT sufficient, and it is worth being
// exact about why, because the obvious mental model is wrong: fmt's reflection
// path reads unexported fields perfectly well. `fmt.Sprintf("%d", set)` on a
// type with only a Stringer prints the tokens verbatim, because fmt consults
// Stringer for just %v, %s, %q, %x and %X and sends every other verb to
// reflection. That is why these types implement fmt.Formatter, which fmt
// consults first, for every verb. String and GoString remain for direct callers
// and for readable output; Format is what makes the coverage total.
//
// Formatter still leaves three paths uncovered — %p and %w, which fmt resolves
// before consulting it; anything routed through badVerb, which suppresses
// methods; and a value inside someone else's unexported field, which fmt
// reaches by reflection but cannot call a method on. Those are closed by where
// the bytes live rather than by any method: see the secret type below.
//
// MarshalJSON refuses rather than redacts. Redacting would make
// json.Marshal(set) succeed and silently produce a token-shaped document with
// no token in it: a store built that way passes its round-trip test only if the
// test checks the token, and fails in production by losing credentials. Since
// Access/Refresh exist as the deliberate way to persist, there is no legitimate
// caller of json.Marshal on a TokenSet, and the honest answer to an illegitimate
// one is a loud error.

package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Redacted is the text that stands in for secret material in every rendering
// this package produces.
const Redacted = "[REDACTED]"

// secret holds credential material inside a closure.
//
// The closure looks like a needless allocation and is not. Its justification is
// specifically NOT the standard library, so it is worth being exact, because
// the plausible-sounding version of this comment is false and would get the
// indirection deleted:
//
//   - Against stdlib fmt, Format plus a plain string field is nearly enough,
//     and any pointer field would finish the job. fmt's reflection follows a
//     pointer only at depth 0 and only into an Array, Slice, Struct or Map, so
//     even a *string is never dereferenced — the paths that skip methods (%p,
//     %w, badVerb, and a value inside another struct's unexported field) all
//     print an address. A `*string` here would sweep every verb clean. That is
//     measured, not assumed: see TestSecretsSurviveUnexportedFieldWrapper.
//   - The threat the closure answers is the reflection walkers that are not
//     fmt: go-spew, and structured loggers and debug dumpers built the same
//     way. They use unsafe.Pointer + reflect.NewAt to rebuild an unexported
//     field as a readable value, which defeats CanInterface, and they follow
//     pointers at any depth. Such a walker renders a *string field as the
//     string. It renders this as `&auth.secret{reveal:func@0x...}`, because a
//     closure's captured environment is not a field: the only way to the bytes
//     is to call the func, and a walker cannot know to. Measured too, by
//     TestSecretsSurviveUnsafeReflection, which fails if this becomes a
//     *string.
//
// So: the methods handle fmt, and this handles everything that reflects
// harder than fmt does.
//
// A secret is immutable, so copying a TokenSet by value — which shares the
// pointer — is safe, and reveal is never nil for a non-nil secret.
type secret struct{ reveal func() string }

// newSecret wraps v, or returns nil for the empty string so that "absent" has
// exactly one representation.
func newSecret(v string) *secret {
	if v == "" {
		return nil
	}
	return &secret{reveal: func() string { return v }}
}

// value returns the wrapped material, or "" when there is none.
func (s *secret) value() string {
	if s == nil {
		return ""
	}
	return s.reveal()
}

// ErrMarshalRefused reports that a value holding secret material refused to
// serialize itself. See the file comment for why refusal beats redaction here;
// use the explicit accessors to persist a token deliberately.
var ErrMarshalRefused = errors.New("refusing to marshal secret material; use the explicit accessors to persist it")

// Bounds on the components of a Key. Both are generous for legitimate values
// and exist to keep a Key's rendering — which goes to logs — bounded.
const (
	// MaxOriginBytes bounds ServerOrigin.
	MaxOriginBytes = 512
	// MaxClientIDBytes bounds ClientID.
	MaxClientIDBytes = 256
)

// Key identifies the tokens held for one protected resource as accessed by one
// client. Both fields are non-secret identifiers and are safe to log.
//
// A Key is a cache key, so ServerOrigin must be canonical: two spellings of the
// same origin would silently become two entries, and the second would trigger a
// redundant interactive login. Validate therefore rejects non-canonical
// spellings rather than normalizing them — a store keyed by a value the caller
// did not choose is a surprising store. Callers canonicalize when they build
// the Key.
type Key struct {
	// ServerOrigin is the protected resource's identity as a canonical
	// origin: scheme://host[:port], lowercase, with no default port, path,
	// query, fragment, or userinfo. This is RFC 6454 origin serialization.
	ServerOrigin string
	// ClientID is the OAuth client identifier the tokens were issued to. It
	// is empty before dynamic client registration has run — an unregistered
	// client legitimately has no ID yet — so Validate accepts an empty value.
	// It is part of the key because the same server, reached by two registered
	// clients, must not share a token.
	ClientID string
}

// Validate reports whether k is a well-formed, canonical key. Violations are
// returned as *Error with class ClassInvalidConfig.
//
// The scheme rule mirrors what the HTTP transport will require: https
// everywhere, with http tolerated only for loopback, where there is no network
// to eavesdrop on and where local development and OAuth redirect listeners
// actually live.
func (k Key) Validate() error {
	fail := func(msg string) error {
		return NewError(ClassInvalidConfig, "validate", msg, nil)
	}

	if k.ServerOrigin == "" {
		return fail("ServerOrigin is empty")
	}
	if len(k.ServerOrigin) > MaxOriginBytes {
		return fail(fmt.Sprintf("ServerOrigin is %d bytes, max %d", len(k.ServerOrigin), MaxOriginBytes))
	}
	if i := indexOfControl(k.ServerOrigin); i >= 0 {
		return fail(fmt.Sprintf("ServerOrigin contains a control character at index %d", i))
	}
	if err := validateOrigin(k.ServerOrigin); err != nil {
		return fail(err.Error())
	}

	if len(k.ClientID) > MaxClientIDBytes {
		return fail(fmt.Sprintf("ClientID is %d bytes, max %d", len(k.ClientID), MaxClientIDBytes))
	}
	if i := indexOfControl(k.ClientID); i >= 0 {
		return fail(fmt.Sprintf("ClientID contains a control character at index %d", i))
	}
	return nil
}

// String renders the key for logs as "<origin>#<client-id>", with "-" standing
// in for an unregistered client.
//
// ClientID is included deliberately. RFC 6749 §2.2 is explicit that a client
// identifier "is not a secret; it is exposed to the resource owner" — it is a
// public identifier, unlike the client secret that may accompany it. Including
// it is what makes two keys for the same server distinguishable in a log, which
// is exactly when someone is reading these lines. Validate has already bounded
// both fields and rejected control characters, so the result is safe to embed
// in a log line.
func (k Key) String() string {
	clientID := k.ClientID
	if clientID == "" {
		clientID = "-"
	}
	return k.ServerOrigin + "#" + clientID
}

// validateOrigin reports whether origin is a canonical, transport-acceptable
// origin. It compares origin against the canonical serialization of its own
// parse, which catches uppercase, default ports, and trailing slashes in one
// check, and reports the structural violations separately so the message names
// the actual mistake.
func validateOrigin(origin string) error {
	u, err := url.Parse(origin)
	if err != nil {
		return fmt.Errorf("ServerOrigin is not a valid URL")
	}
	if u.Opaque != "" {
		return fmt.Errorf("ServerOrigin must be scheme://host[:port], not an opaque URL")
	}
	switch {
	case u.Path != "":
		return fmt.Errorf("ServerOrigin must have no path")
	case u.RawQuery != "" || u.ForceQuery:
		return fmt.Errorf("ServerOrigin must have no query")
	case u.Fragment != "":
		return fmt.Errorf("ServerOrigin must have no fragment")
	case u.User != nil:
		return fmt.Errorf("ServerOrigin must have no userinfo")
	}

	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("ServerOrigin has no host")
	}
	// An internationalized name must arrive as its punycode A-label. The
	// Unicode and A-label spellings are the same server, so accepting both
	// would key them separately and cost a redundant login — and a Unicode
	// homograph reaching a log line through Key.String is its own problem.
	// Converting here would need golang.org/x/net/idna, which is not a
	// sanctioned dependency; rejecting is correct regardless, since the
	// caller is the one who knows what it meant.
	if i := strings.IndexFunc(host, func(r rune) bool { return r > 127 }); i >= 0 {
		return fmt.Errorf("ServerOrigin host must be ASCII; encode an internationalized name as its punycode A-label")
	}
	// "example.com." is the same name as "example.com" with the root label
	// spelled out — another two-spellings-one-server split.
	if strings.HasSuffix(host, ".") {
		return fmt.Errorf("ServerOrigin host must not have a trailing dot; want %q", canonicalOrigin(u.Scheme, strings.TrimSuffix(host, "."), u.Port()))
	}
	// An IPv6 zone identifier is scoped to one machine's interfaces, so it is
	// not an identity a token can meaningfully be keyed by — and it cannot
	// survive the round trip anyway: Hostname() unescapes "%25" to "%", which
	// url.Parse then rejects, so the canonical form we would suggest is one
	// the caller cannot use.
	if strings.Contains(host, "%") {
		return fmt.Errorf("ServerOrigin host must not carry an IPv6 zone identifier")
	}
	// Checked explicitly rather than left to the canonical comparison below:
	// url.Parse lowercases the scheme but preserves the host's case, so the
	// canonical serialization would echo back whatever case it was given and
	// never catch this.
	if lower := strings.ToLower(host); lower != host {
		return fmt.Errorf("ServerOrigin host must be lowercase; want %q", canonicalOrigin(u.Scheme, lower, u.Port()))
	}
	loopback := isLoopbackHost(host)
	switch u.Scheme {
	case "https":
	case "http":
		if !loopback {
			return fmt.Errorf("ServerOrigin scheme http is allowed only for loopback hosts, got host %q", host)
		}
	default:
		return fmt.Errorf("ServerOrigin scheme must be https (or http for loopback), got %q", u.Scheme)
	}

	port := u.Port()
	if port != "" {
		// url.Parse guarantees the port is digits, but nothing more: it
		// accepts ":0", ":99999" and ":0443" alike.
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("ServerOrigin has an invalid port")
		}
		// A leading zero is the canonicalization hole that matters: ":0443"
		// and ":443" are the same port, so comparing the string against "443"
		// misses it and canonicalOrigin echoes the input back, letting it
		// through both checks — two keys, one server, a redundant login.
		// Comparing against the re-rendered number closes it for every
		// non-minimal spelling at once.
		if strconv.Itoa(n) != port {
			return fmt.Errorf("ServerOrigin port must have no leading zeros; want %q", canonicalOrigin(u.Scheme, host, strconv.Itoa(n)))
		}
		if (u.Scheme == "https" && n == 443) || (u.Scheme == "http" && n == 80) {
			return fmt.Errorf("ServerOrigin must omit the default port for scheme %q", u.Scheme)
		}
	}

	if canonical := canonicalOrigin(u.Scheme, host, port); canonical != origin {
		return fmt.Errorf("ServerOrigin is not canonical; want %q", canonical)
	}
	return nil
}

// canonicalOrigin serializes the parts of an already-validated origin.
func canonicalOrigin(scheme, host, port string) string {
	if strings.Contains(host, ":") { // IPv6 literals are bracketed in a URL
		host = "[" + host + "]"
	}
	origin := scheme + "://" + host
	if port != "" {
		origin += ":" + port
	}
	return origin
}

// isLoopbackHost reports whether host is a loopback address or the loopback
// name. "localhost" is accepted by name because it is the redirect host OAuth
// native-app flows are specified against (RFC 8252 §7.3), even though what it
// resolves to is a matter of local configuration.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// indexOfControl returns the index of the first line-breaking or control
// character in s, or -1.
//
// U+2028 and U+2029 are included alongside the ASCII controls because they are
// category Zl/Zp rather than Cc — unicode.IsControl does not report them — yet
// they terminate a line for a JavaScript or JSON log consumer just as newline
// does. A Key is rendered into logs by Key.String, so a forged line here is a
// forged record there.
func isLineBreaking(r rune) bool {
	return r < 0x20 || r == 0x7f || r == '\u2028' || r == '\u2029'
}

// indexOfControl returns the index of the first such character in s, or -1.
func indexOfControl(s string) int {
	return strings.IndexFunc(s, isLineBreaking)
}

// ExpirySkew is subtracted from a token's expiry when deciding whether it is
// still usable. It covers the round trip between deciding to use a token and
// the server validating it, plus clock drift between the two — a token that is
// valid for another two seconds is not worth sending, because the request will
// land after it dies and cost a retry.
//
// Thirty seconds is the conventional value and is deliberately larger than any
// plausible request latency, since refreshing early is cheap and being rejected
// mid-operation is not.
const ExpirySkew = 30 * time.Second

// TokenSet is one set of OAuth credentials.
//
// Its fields are unexported and its secrets are reachable only through Access
// and Refresh; see the file comment for the reasoning. Construct one with
// NewTokenSet. The zero value is a valid, empty set: Valid reports false for it.
//
// A TokenSet is a value and is safe to copy. It is immutable after
// construction, so copies cannot diverge and concurrent readers need no
// synchronization.
type TokenSet struct {
	access  *secret // SECRET — reachable only via Access()
	refresh *secret // SECRET — reachable only via Refresh()
	expiry  time.Time
	scopes  []string
}

// NewTokenSet builds a TokenSet. scopes is cloned, so the caller's slice and
// the TokenSet cannot alias.
//
// A zero expiry means "no known expiry": some servers issue tokens without one,
// and Expired treats such a token as never expiring rather than as immediately
// dead.
func NewTokenSet(access, refresh string, expiry time.Time, scopes []string) TokenSet {
	return TokenSet{
		access:  newSecret(access),
		refresh: newSecret(refresh),
		expiry:  expiry,
		scopes:  slices.Clone(scopes),
	}
}

// Access returns the access token. This is secret material: send it to a
// server, or hand it to a store that is persisting it. Never log it, never put
// it in an error, never put it in an event.
func (t TokenSet) Access() string { return t.access.value() }

// Refresh returns the refresh token, which is empty when the grant did not
// include one. This is secret material — and the more valuable of the two, since
// it mints access tokens. The rules for Access apply with more force.
func (t TokenSet) Refresh() string { return t.refresh.value() }

// Expiry returns the access token's expiry, or the zero time when the server
// did not state one.
func (t TokenSet) Expiry() time.Time { return t.expiry }

// Scopes returns the granted scopes as a copy; mutating it does not affect t.
func (t TokenSet) Scopes() []string { return slices.Clone(t.scopes) }

// Expired reports whether the access token is expired as of now, treating it as
// expired ExpirySkew early. A set with no stated expiry never expires.
func (t TokenSet) Expired(now time.Time) bool {
	if t.expiry.IsZero() {
		return false
	}
	return !now.Before(t.expiry.Add(-ExpirySkew))
}

// Valid reports whether the set carries an access token at all. It says nothing
// about expiry — a caller that needs a usable token checks both, and the two are
// separate because an expired token with a refresh token is a different
// situation from no token at all.
func (t TokenSet) Valid() bool { return t.access != nil }

// String renders the set with its secrets redacted, reporting only the metadata
// an operator needs: whether each token is present, when it expires, and what it
// is good for.
func (t TokenSet) String() string {
	return fmt.Sprintf(
		"auth.TokenSet{access:%s, refresh:%s, expiry:%s, scopes:%v}",
		presence(t.access), presence(t.refresh), renderExpiry(t.expiry), t.scopes,
	)
}

// GoString renders redacted text for a direct caller.
//
// It is not what serves %#v — Format is consulted before GoStringer, so fmt
// never reaches this. It is kept as defense in depth: if Format is ever
// removed, this becomes load-bearing again for %#v, and a caller invoking
// GoString itself still gets redacted text.
func (t TokenSet) GoString() string { return t.String() }

// Format routes every fmt verb through the redacted rendering.
//
// Stringer is not enough on its own, and this is the subtle part of the whole
// design: fmt consults Stringer only for %v, %s, %q, %x and %X. Any other verb
// — %d, %t, %f, %c, ... — falls through to reflection, and fmt's reflection
// path reads unexported fields, so `fmt.Sprintf("%d", set)` would print
// `{%!d(string=<the token>) ...}`. A wrong verb is a typo, not a decision, and
// a typo must not be the difference between a redacted log line and a leaked
// credential.
//
// Formatter is consulted before Stringer and before reflection, for every verb,
// which closes the whole class. The verb is deliberately ignored: there is no
// verb for which printing this value in any form other than redacted is
// correct.
func (t TokenSet) Format(f fmt.State, verb rune) {
	// fmt.Formatter cannot report a write error and fmt ignores them
	// anyway; discarding is the contract here, not an oversight.
	_, _ = io.WriteString(f, t.String())
}

// MarshalJSON always fails, so that a TokenSet reaching a JSON encoder — a log
// line, an event payload, an HTTP response — is a loud error rather than a
// silent leak or a silent loss. See the file comment.
func (t TokenSet) MarshalJSON() ([]byte, error) {
	return nil, fmt.Errorf("auth.TokenSet: %w", ErrMarshalRefused)
}

// UnmarshalJSON always fails, for the mirror of MarshalJSON's reason. Without
// it, decoding into a struct containing a TokenSet would silently produce an
// empty one — the same silent-credential-loss failure that argues against a
// redacting MarshalJSON, arriving from the other direction. A store that
// persists tokens reconstructs them with NewTokenSet.
func (t *TokenSet) UnmarshalJSON([]byte) error {
	return fmt.Errorf("auth.TokenSet: %w", ErrMarshalRefused)
}

// presence renders whether a secret is set, without revealing anything about
// it. Presence is not itself secret: it is the difference between "the server
// gave us no refresh token" and "we lost it", which is what an operator reading
// a log is trying to tell apart.
func presence(s *secret) string {
	if s == nil {
		return "<empty>"
	}
	return Redacted
}

// renderExpiry formats an expiry, distinguishing "none stated" from a time.
func renderExpiry(expiry time.Time) string {
	if expiry.IsZero() {
		return "<none>"
	}
	return expiry.UTC().Format(time.RFC3339)
}

// TokenStore is where an application keeps tokens between runs. The module
// supplies no persistent implementation on purpose: a keyring, a file, a
// database, and a secrets manager are all legitimate, and which one is correct
// is a property of the application, not of MCP.
//
// Implementations must:
//
//   - return an error satisfying errors.Is(err, ErrNoToken) from Load when the
//     key is absent, and use it for nothing else;
//   - treat Delete of an absent key as success — the caller's intent already
//     holds;
//   - honor ctx, since a real store does I/O;
//   - be safe for concurrent use.
//
// A TokenSet handed to Store or returned from Load is a value; implementations
// persist it through the Access/Refresh accessors, which is the deliberate,
// auditable path to token material.
type TokenStore interface {
	// Load returns the tokens held for key, or an error wrapping ErrNoToken
	// when there are none.
	Load(ctx context.Context, key Key) (TokenSet, error)
	// Store saves tokens for key, replacing any already held.
	Store(ctx context.Context, key Key, set TokenSet) error
	// Delete removes the tokens held for key. Deleting an absent key is not
	// an error.
	Delete(ctx context.Context, key Key) error
}

// MemoryStore is an in-memory TokenStore: the reference implementation of the
// contract, the store tests use, and the right choice for an application that
// wants tokens to die with the process.
//
// It is safe for concurrent use. The zero value is not usable; call
// NewMemoryStore.
type MemoryStore struct {
	mu     sync.RWMutex
	tokens map[Key]TokenSet
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{tokens: make(map[Key]TokenSet)}
}

// Load implements TokenStore.
func (s *MemoryStore) Load(ctx context.Context, key Key) (TokenSet, error) {
	if err := s.check(ctx, key, "load"); err != nil {
		return TokenSet{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	set, ok := s.tokens[key]
	if !ok {
		return TokenSet{}, NewNoTokenError("load")
	}
	// set is a copy already: TokenSet is a value type whose only slice is
	// unreachable except through Scopes, which clones.
	return set, nil
}

// Store implements TokenStore.
func (s *MemoryStore) Store(ctx context.Context, key Key, set TokenSet) error {
	if err := s.check(ctx, key, "store"); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	s.tokens[key] = set
	return nil
}

// Delete implements TokenStore.
func (s *MemoryStore) Delete(ctx context.Context, key Key) error {
	if err := s.check(ctx, key, "delete"); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.tokens, key)
	return nil
}

// check applies the preconditions every operation shares: a live context and a
// valid key. Validating the key on the way in keeps a malformed origin from
// becoming a distinct, permanently-missing map entry — failing closed at the
// boundary rather than degrading into a cache miss.
func (s *MemoryStore) check(ctx context.Context, key Key, op string) error {
	if err := ctx.Err(); err != nil {
		return NewError(ClassFailed, op, "context is done", err)
	}
	if err := key.Validate(); err != nil {
		return err
	}
	return nil
}

// String renders the store without its contents. A map of tokens is exactly the
// thing that should never be printed, and MemoryStore is a plausible field of
// some larger struct that someone will log.
func (s *MemoryStore) String() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return fmt.Sprintf("auth.MemoryStore{keys:%d}", len(s.tokens))
}

// GoString renders redacted text for a direct caller; Format is what serves
// %#v. See TokenSet.GoString.
func (s *MemoryStore) GoString() string { return s.String() }

// Format routes every fmt verb through the redacted rendering.
//
// A store needs this for a reason its contents do not: fmt skips the methods of
// a value it reaches through an *unexported* field, and the token map is one.
// Without Format, an unhandled verb would walk straight past TokenSet's own
// Format into the map and render every entry. Holding secrets transitively is
// the same obligation as holding them directly.
func (s *MemoryStore) Format(f fmt.State, verb rune) {
	// fmt.Formatter cannot report a write error and fmt ignores them
	// anyway; discarding is the contract here, not an oversight.
	_, _ = io.WriteString(f, s.String())
}
