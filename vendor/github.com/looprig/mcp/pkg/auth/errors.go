// This file defines auth's own typed error taxonomy.
//
// The package deliberately does not import pkg/client. The module's dependency
// graph makes pkg/auth a leaf (stdlib only, plus the dependency-free
// internal/limits helpers), and pkg/transport/* imports both — so a transport,
// not this package, is the right place to translate an auth Class into a
// client.FailureClass. Importing pkg/client here would invert that: client
// would gain an auth dependency the moment it wanted to classify one of these,
// and the leaf would stop being a leaf.
//
// The mapping transports apply is 1:1 and fixed:
//
//	ClassInvalidConfig -> client.FailureInvalidConfig
//	ClassNoToken       -> client.FailureAuthRequired
//	ClassRequired      -> client.FailureAuthRequired
//	ClassDenied        -> client.FailureAuthDenied
//	ClassExpired       -> client.FailureAuthExpired
//	ClassFailed        -> client.FailureAuthFailed
//
// Unlike client.Error, an auth Error never renders its wrapped cause. An auth
// failure's cause is routinely an HTTP or keyring error whose text may quote a
// request URL, a header, or a token; rendering it would make redaction depend
// on every cause in the module being well behaved. Instead the rendered text is
// only ever class + op + an explicit, bounded, normalized Msg. The cause stays
// reachable through errors.Is/errors.As for programmatic inspection.

package auth

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"

	"github.com/looprig/mcp/internal/limits"
)

// Class classifies an auth failure so callers can branch on what went wrong
// without parsing error text. The zero value is not a valid class.
type Class uint8

// Auth failure classes. Values are contiguous starting at 1; the zero value is
// reserved as "no class".
const (
	// ClassInvalidConfig is a malformed key, header, or auth configuration.
	// It is a programmer or operator error, not a protocol outcome.
	ClassInvalidConfig Class = iota + 1
	// ClassNoToken is a token store reporting that it holds no token for a
	// key. It means "absent", never "broken" — see ErrNoToken.
	ClassNoToken
	// ClassRequired is a server demanding credentials the client does not
	// have.
	ClassRequired
	// ClassDenied is an authorization request the resource owner or server
	// refused.
	ClassDenied
	// ClassExpired is a token past its expiry that could not be refreshed.
	ClassExpired
	// ClassFailed is any other auth failure: discovery, registration, or a
	// refresh that broke rather than being refused.
	ClassFailed
	classSentinel // must remain last; used by tests for exhaustiveness
)

// String returns a stable lowercase snake_case identifier for the class.
// Undeclared values return "unknown".
func (c Class) String() string {
	switch c {
	case ClassInvalidConfig:
		return "invalid_config"
	case ClassNoToken:
		return "no_token"
	case ClassRequired:
		return "required"
	case ClassDenied:
		return "denied"
	case ClassExpired:
		return "expired"
	case ClassFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// MaxMessageBytes bounds every message this package renders — Error.Msg and
// Status.Failure alike. It is deliberately smaller than client.MaxMessageBytes:
// auth messages are classifications written by this module, not server text
// relayed through it, so there is nothing legitimate to say at length.
const MaxMessageBytes = 256

// ErrNoToken reports that a token store holds no token for a key. It is the
// contract that separates "absent" from "failed": a store that cannot tell the
// difference forces its caller to treat every read failure as a reason to start
// an interactive login, which is neither fail-closed nor usable.
//
// TokenStore implementations must return an error that satisfies
// errors.Is(err, ErrNoToken) for an absent key, and must not use it for any
// other condition. NewNoTokenError builds a conforming value.
var ErrNoToken = errors.New("no token stored")

// Error is the package's operational error. Msg is already normalized and
// bounded (NewError enforces this); construct values with NewError rather than
// a composite literal so the bound holds.
type Error struct {
	// Class states what kind of auth failure occurred.
	Class Class
	// Op names the operation that failed (e.g. "load", "refresh").
	Op string
	// Msg is a bounded, normalized, secret-free description.
	Msg string
	// Err is the wrapped cause, if any. It is never rendered — not through
	// Error, not through any fmt verb, and not through a copy of this struct.
	// See the receivers on Error's methods: they are VALUES, deliberately.
	Err error
}

// NewError builds an *Error with msg normalized (control characters replaced by
// spaces, invalid UTF-8 repaired) and bounded to MaxMessageBytes.
//
// msg must not contain secret material: it is the only caller-supplied text
// this package renders, and it is rendered verbatim.
func NewError(class Class, op string, msg string, wrapped error) *Error {
	return &Error{
		Class: class,
		Op:    op,
		Msg:   boundMessage(msg),
		Err:   wrapped,
	}
}

// NewNoTokenError builds the error a TokenStore must return for an absent key:
// class ClassNoToken, wrapping ErrNoToken so errors.Is matches.
func NewNoTokenError(op string) *Error {
	return NewError(ClassNoToken, op, "", ErrNoToken)
}

// Error renders "auth: <op>: <class>: <msg>", omitting empty segments. The
// wrapped cause is never rendered — see the file comment.
func (e Error) Error() string {
	parts := make([]string, 0, 4)
	parts = append(parts, "auth")
	if e.Op != "" {
		parts = append(parts, e.Op)
	}
	parts = append(parts, e.Class.String())
	if e.Msg != "" {
		parts = append(parts, e.Msg)
	}
	return strings.Join(parts, ": ")
}

// Unwrap returns the wrapped cause for errors.Is / errors.As traversal.
func (e Error) Unwrap() error {
	return e.Err
}

// Format routes every fmt verb through Error, so that the decision not to
// render the wrapped cause actually holds.
//
// Without this, the decision is only skin deep: fmt consults the error
// interface for %v, %s, %q, %x and %X, but sends every other verb to
// reflection — which walks the struct and prints the Err field's contents in
// full. `fmt.Sprintf("%d", err)` on an Error wrapping an HTTP failure would
// print whatever that failure's text contains, which is exactly the material
// Error refuses to render through Error(). A verb typo must not be the
// difference.
//
// The VALUE receiver is the other half, and it is not a style choice — it is
// the same reasoning TokenSet's receivers follow. A pointer receiver puts
// Format in *Error's method set only, so a value copy — `fmt.Sprintf("%v",
// *err)`, an Error stored or ranged by value, a struct embedding one — misses
// Formatter entirely and falls to reflection, which prints Err in full. This
// package hands callers a *Error, but "never rendered" is a claim about the
// TYPE, and a caller who copies one has done nothing wrong. A value receiver
// covers both method sets, so the claim is true of every value of this type
// rather than only of the pointers this package happens to return.
func (e Error) Format(f fmt.State, verb rune) {
	// fmt.Formatter cannot report a write error and fmt ignores them
	// anyway; discarding is the contract here, not an oversight.
	_, _ = io.WriteString(f, e.Error())
}

// ClassOf walks err's chain and reports the class of the outermost *Error.
// It returns false when the chain contains no *Error.
func ClassOf(err error) (Class, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e.Class, true
	}
	return 0, false
}

// boundMessage normalizes s (control characters become single spaces, invalid
// UTF-8 becomes U+FFFD) and truncates it to at most MaxMessageBytes.
//
// The control-character pass is not cosmetic: this text reaches logs, and a
// newline in a log line is a forged record. It also repairs invalid UTF-8 for
// free — strings.Map decodes invalid bytes as U+FFFD and re-encodes them — so
// no separate validity pass is needed.
func boundMessage(s string) string {
	s = strings.Map(func(r rune) rune {
		// isLineBreaking covers U+2028/U+2029 as well as the ASCII controls;
		// unicode.IsControl reports neither, since they are Zl/Zp rather than
		// Cc, yet both end a line for a JSON or JavaScript log consumer.
		if unicode.IsControl(r) || isLineBreaking(r) {
			return ' '
		}
		return r
	}, s)
	out, _ := limits.TruncateText(s, MaxMessageBytes)
	return out
}
