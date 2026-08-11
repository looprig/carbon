package credentials

import (
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"
)

// Bounds apply before any canonicalization or allocation at an untrusted
// boundary. A credential reference is deliberately narrower than a general
// URL: exactly two safe identifier components are allowed.
const (
	MaxReferenceLength          = 128
	MaxReferenceComponentLength = 48
)

const referenceScheme = "credential"

// Reference is an opaque, comparable credential identity. Its fields are
// private so a valid value can only be obtained through a constructor or text
// parser. It contains no authority, URL, filesystem path, or secret value.
type Reference struct {
	provider string
	name     string
}

// ParseReference parses exactly credential://provider/name. The scheme and
// identifier components are canonicalized to lower case; no query, fragment,
// URL authority, path traversal, or additional path segment is accepted.
func ParseReference(raw string) (Reference, error) {
	if len(raw) == 0 || len(raw) > MaxReferenceLength {
		return Reference{}, NewInvalidReferenceError("length")
	}
	if !utf8.ValidString(raw) {
		return Reference{}, NewInvalidReferenceError("encoding")
	}
	separator := strings.Index(raw, "://")
	if separator <= 0 || !strings.EqualFold(raw[:separator], referenceScheme) {
		return Reference{}, NewInvalidReferenceError("scheme")
	}
	path := raw[separator+3:]
	parts := strings.Split(path, "/")
	if len(parts) != 2 {
		return Reference{}, NewInvalidReferenceError("path")
	}
	provider, err := canonicalComponent(parts[0], "provider")
	if err != nil {
		return Reference{}, err
	}
	name, err := canonicalComponent(parts[1], "name")
	if err != nil {
		return Reference{}, err
	}
	ref := Reference{provider: provider, name: name}
	if len(ref.String()) > MaxReferenceLength {
		return Reference{}, NewInvalidReferenceError("length")
	}
	return ref, nil
}

// NewReference constructs an opaque credential identity from safe provider
// and name components.
func NewReference(provider, name string) (Reference, error) {
	provider, err := canonicalComponent(provider, "provider")
	if err != nil {
		return Reference{}, err
	}
	name, err = canonicalComponent(name, "name")
	if err != nil {
		return Reference{}, err
	}
	ref := Reference{provider: provider, name: name}
	if len(ref.String()) > MaxReferenceLength {
		return Reference{}, NewInvalidReferenceError("length")
	}
	return ref, nil
}

func canonicalComponent(raw, field string) (string, error) {
	if len(raw) == 0 || len(raw) > MaxReferenceComponentLength {
		return "", NewInvalidReferenceError(field)
	}
	if !utf8.ValidString(raw) {
		return "", NewInvalidReferenceError("encoding")
	}
	if raw == "." || raw == ".." {
		return "", NewInvalidReferenceError("path")
	}
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch {
		case c >= 'A' && c <= 'Z':
			// Uppercase input is canonicalized below.
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
		default:
			return "", NewInvalidReferenceError(field)
		}
	}
	canonical := strings.ToLower(raw)
	if canonical[0] == '.' || canonical[0] == '-' || canonical[0] == '_' ||
		canonical[len(canonical)-1] == '.' || canonical[len(canonical)-1] == '-' || canonical[len(canonical)-1] == '_' {
		return "", NewInvalidReferenceError(field)
	}
	return canonical, nil
}

// Provider returns the safe provider component.
func (r Reference) Provider() string { return r.provider }

// Name returns the safe human-selected credential name component.
func (r Reference) Name() string { return r.name }

// Scheme returns the fixed credential scheme.
func (r Reference) Scheme() string {
	if r.IsZero() {
		return ""
	}
	return referenceScheme
}

// String returns the canonical safe representation, or an empty string for
// the invalid zero value.
func (r Reference) String() string {
	if r.IsZero() {
		return ""
	}
	return referenceScheme + "://" + r.provider + "/" + r.name
}

func (r Reference) Canonical() string { return r.String() }

func (r Reference) IsZero() bool { return r.provider == "" && r.name == "" }

func (r Reference) Valid() bool {
	if r.IsZero() || len(r.String()) > MaxReferenceLength {
		return false
	}
	provider, err := canonicalComponent(r.provider, "provider")
	if err != nil || provider != r.provider {
		return false
	}
	name, err := canonicalComponent(r.name, "name")
	return err == nil && name == r.name
}

func (r Reference) Validate() error {
	if !r.Valid() {
		return NewInvalidReferenceError("reference")
	}
	return nil
}

func (r Reference) MarshalText() ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return []byte(r.String()), nil
}

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

func (r Reference) Format(state fmt.State, _ rune) { formatSafe(state, r.String()) }
func (r Reference) GoString() string               { return r.String() }
func (r Reference) LogValue() slog.Value           { return slog.StringValue(r.String()) }
