package secrets

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"
)

// Reference limits keep parsing and canonicalization bounded at untrusted
// boundaries.  Paths are slash-separated opaque identifiers, not filesystem
// paths.
const (
	MaxReferenceLength     = 512
	MaxReferencePathLength = 448
	MaxReferenceSchemeLen  = 32
)

var (
	ErrInvalidReference = errors.New("secrets: invalid reference")
	ErrInvalidNamespace = errors.New("secrets: invalid namespace")
)

// Reference is a canonical, non-secret identifier.  The path is opaque to
// this package and is interpreted by the backend selected by Scheme; it is
// never treated as a filesystem path.
type Reference struct {
	scheme string
	path   string
}

// InvalidReferenceError reports malformed or unsafe reference text without
// retaining the caller's raw input (which could itself contain a credential).
type InvalidReferenceError struct{ reason errorReason }

// NewInvalidReferenceError constructs a bounded invalid-reference error.  The
// reason is normalized to a small package-owned vocabulary and is never
// retained verbatim.
func NewInvalidReferenceError(reason string) *InvalidReferenceError {
	return &InvalidReferenceError{reason: normalizeErrorReason(reason)}
}

func (e *InvalidReferenceError) Error() string {
	if e == nil {
		return ErrInvalidReference.Error()
	}
	return "secrets: invalid reference (" + errorReasonText(e.reason) + ")"
}

func (e *InvalidReferenceError) Unwrap() error { return ErrInvalidReference }

// Reason returns a normalized, secret-free reason label.
func (e *InvalidReferenceError) Reason() string {
	if e == nil {
		return "invalid"
	}
	return errorReasonText(e.reason)
}

func (e *InvalidReferenceError) Format(state fmt.State, verb rune) {
	formatSafeError(state, e.Error())
}

func (e *InvalidReferenceError) GoString() string { return e.Error() }

func (e *InvalidReferenceError) LogValue() slog.Value { return slog.StringValue(e.Error()) }

// InvalidNamespaceError reports a malformed namespace without retaining raw
// input.
type InvalidNamespaceError struct{ reason errorReason }

// NewInvalidNamespaceError constructs a bounded invalid-namespace error.
func NewInvalidNamespaceError(reason string) *InvalidNamespaceError {
	return &InvalidNamespaceError{reason: normalizeErrorReason(reason)}
}

func (e *InvalidNamespaceError) Error() string {
	if e == nil {
		return ErrInvalidNamespace.Error()
	}
	return "secrets: invalid namespace (" + errorReasonText(e.reason) + ")"
}

func (e *InvalidNamespaceError) Unwrap() error { return ErrInvalidNamespace }

// Reason returns a normalized, secret-free reason label.
func (e *InvalidNamespaceError) Reason() string {
	if e == nil {
		return "invalid"
	}
	return errorReasonText(e.reason)
}

func (e *InvalidNamespaceError) Format(state fmt.State, verb rune) {
	formatSafeError(state, e.Error())
}

func (e *InvalidNamespaceError) GoString() string { return e.Error() }

func (e *InvalidNamespaceError) LogValue() slog.Value { return slog.StringValue(e.Error()) }

// ParseReference parses a strict scheme://path reference and returns its
// canonical representation.  Schemes are normalized to lowercase; paths are
// preserved exactly after validation.  Query strings, fragments, URL
// authorities, filesystem traversal syntax, and endpoint schemes are not
// accepted.
func ParseReference(raw string) (Reference, error) {
	if len(raw) == 0 || len(raw) > MaxReferenceLength {
		return Reference{}, NewInvalidReferenceError("length")
	}
	if !utf8.ValidString(raw) {
		return Reference{}, NewInvalidReferenceError("encoding")
	}
	separator := strings.Index(raw, "://")
	if separator <= 0 {
		return Reference{}, NewInvalidReferenceError("scheme")
	}
	scheme, err := canonicalScheme(raw[:separator])
	if err != nil {
		return Reference{}, err
	}
	path := raw[separator+3:]
	if err := validateReferencePath(path, false); err != nil {
		return Reference{}, err
	}
	if len(scheme)+3+len(path) > MaxReferenceLength {
		return Reference{}, NewInvalidReferenceError("length")
	}
	return Reference{scheme: scheme, path: path}, nil
}

// NewReference builds a canonical reference from a validated scheme and
// opaque slash-separated path.
func NewReference(scheme, path string) (Reference, error) {
	canonical, err := canonicalScheme(scheme)
	if err != nil {
		return Reference{}, err
	}
	if err := validateReferencePath(path, false); err != nil {
		return Reference{}, err
	}
	if len(canonical)+3+len(path) > MaxReferenceLength {
		return Reference{}, NewInvalidReferenceError("length")
	}
	return Reference{scheme: canonical, path: path}, nil
}

// Scheme returns the validated backend scheme.
func (r Reference) Scheme() string { return r.scheme }

// Path returns the validated opaque path.
func (r Reference) Path() string { return r.path }

// String returns the canonical stable representation.  The zero Reference
// has an empty representation.
func (r Reference) String() string {
	if r.IsZero() {
		return ""
	}
	return r.scheme + "://" + r.path
}

// Canonical returns the same bounded stable representation as String.
func (r Reference) Canonical() string { return r.String() }

// IsZero reports whether r is the invalid zero Reference.
func (r Reference) IsZero() bool { return r.scheme == "" && r.path == "" }

// MarshalText allows safe persistence of the canonical reference.
func (r Reference) MarshalText() ([]byte, error) {
	if r.IsZero() {
		return nil, NewInvalidReferenceError("zero")
	}
	return []byte(r.String()), nil
}

// UnmarshalText parses canonical reference text and leaves r unchanged on
// error.
func (r *Reference) UnmarshalText(text []byte) error {
	if r == nil {
		return NewInvalidReferenceError("nil receiver")
	}
	if len(text) > MaxReferenceLength {
		return NewInvalidReferenceError("length")
	}
	parsed, err := ParseReference(string(text))
	if err != nil {
		return err
	}
	*r = parsed
	return nil
}

// Namespace is a canonical scheme and path prefix used to constrain listing.
// It cannot contain references from another scheme or a sibling prefix.
type Namespace struct {
	scheme string
	prefix string
}

// NewNamespace constructs a namespace.  A single trailing slash is accepted
// as presentation syntax and removed from the canonical prefix.
func NewNamespace(scheme, prefix string) (Namespace, error) {
	canonical, err := canonicalScheme(scheme)
	if err != nil {
		return Namespace{}, NewInvalidNamespaceError("scheme")
	}
	if len(prefix) > 0 && prefix[len(prefix)-1] == '/' {
		prefix = strings.TrimSuffix(prefix, "/")
	}
	if err := validateReferencePath(prefix, false); err != nil {
		return Namespace{}, NewInvalidNamespaceError("prefix")
	}
	if len(canonical)+3+len(prefix) > MaxReferenceLength {
		return Namespace{}, NewInvalidNamespaceError("length")
	}
	return Namespace{scheme: canonical, prefix: prefix}, nil
}

// ParseNamespace parses a namespace written in reference form.  It accepts a
// single trailing slash to make prefix literals convenient, while String
// always emits the canonical no-trailing-slash form.
func ParseNamespace(raw string) (Namespace, error) {
	if len(raw) == 0 || len(raw) > MaxReferenceLength {
		return Namespace{}, NewInvalidNamespaceError("length")
	}
	separator := strings.Index(raw, "://")
	if separator <= 0 {
		return Namespace{}, NewInvalidNamespaceError("scheme")
	}
	path := raw[separator+3:]
	if len(path) == 0 {
		return Namespace{}, NewInvalidNamespaceError("prefix")
	}
	if path[len(path)-1] != '/' {
		// Parse through NewNamespace for one implementation of path validation.
		return NewNamespace(raw[:separator], path)
	}
	if strings.HasSuffix(path, "//") {
		return Namespace{}, NewInvalidNamespaceError("prefix")
	}
	return NewNamespace(raw[:separator], path[:len(path)-1])
}

// Scheme returns the namespace's validated scheme.
func (n Namespace) Scheme() string { return n.scheme }

// Path returns the canonical path prefix without a trailing slash.
func (n Namespace) Path() string { return n.prefix }

// Prefix is an alias for Path, useful at call sites that emphasize boundary
// semantics.
func (n Namespace) Prefix() string { return n.prefix }

// String returns the canonical namespace representation.
func (n Namespace) String() string {
	if n.IsZero() {
		return ""
	}
	return n.scheme + "://" + n.prefix
}

// Canonical returns the bounded stable namespace representation.
func (n Namespace) Canonical() string { return n.String() }

// IsZero reports whether n is invalid/empty.
func (n Namespace) IsZero() bool { return n.scheme == "" && n.prefix == "" }

// Contains reports whether ref is the namespace itself or a descendant path.
// The segment boundary check prevents "personal" from containing
// "personally".
func (n Namespace) Contains(ref Reference) bool {
	if n.IsZero() || ref.IsZero() || n.scheme != ref.scheme {
		return false
	}
	return ref.path == n.prefix || strings.HasPrefix(ref.path, n.prefix+"/")
}

func canonicalScheme(scheme string) (string, error) {
	if len(scheme) == 0 || len(scheme) > MaxReferenceSchemeLen {
		return "", NewInvalidReferenceError("scheme")
	}
	for i := 0; i < len(scheme); i++ {
		ch := scheme[i]
		if i == 0 {
			if !isASCIILetter(ch) {
				return "", NewInvalidReferenceError("scheme")
			}
			continue
		}
		if !(isASCIILetter(ch) || isASCIIDigit(ch) || ch == '+' || ch == '-' || ch == '.') {
			return "", NewInvalidReferenceError("scheme")
		}
	}
	canonical := strings.ToLower(scheme)
	// Endpoint and filesystem URL schemes would turn the identifier into an
	// arbitrary URL or path.  Backends should use opaque names instead.
	switch canonical {
	case "file", "http", "https", "ftp":
		return "", NewInvalidReferenceError("endpoint scheme")
	}
	return canonical, nil
}

func validateReferencePath(path string, allowTrailingSlash bool) error {
	if len(path) == 0 || len(path) > MaxReferencePathLength {
		return NewInvalidReferenceError("path")
	}
	if !utf8.ValidString(path) {
		return NewInvalidReferenceError("path encoding")
	}
	if path[0] == '/' || path[len(path)-1] == '/' && !allowTrailingSlash {
		return NewInvalidReferenceError("path boundary")
	}
	if strings.Contains(path, "//") {
		return NewInvalidReferenceError("path separator")
	}
	segments := strings.Split(path, "/")
	for _, segment := range segments {
		if segment == "." || segment == ".." || segment == "" {
			return NewInvalidReferenceError("path traversal")
		}
		for i := 0; i < len(segment); i++ {
			if !isSafePathByte(segment[i]) {
				return NewInvalidReferenceError("path character")
			}
		}
	}
	return nil
}

func isSafePathByte(ch byte) bool {
	return isASCIILetter(ch) || isASCIIDigit(ch) || strings.ContainsRune("-._~!$&'()*+,;=@", rune(ch))
}

func isASCIILetter(ch byte) bool {
	return (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z')
}

func isASCIIDigit(ch byte) bool { return ch >= '0' && ch <= '9' }
