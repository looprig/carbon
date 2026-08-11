package credentials

import (
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"
)

// Scheme describes the mechanism that establishes outbound authority.
type Scheme string

const (
	SchemeNone             Scheme = "none"
	SchemeAPIKey           Scheme = "api_key"
	SchemeOAuth            Scheme = "oauth"
	SchemeSigV4            Scheme = "sigv4"
	SchemeWorkloadIdentity Scheme = "workload_identity"
)

func (s Scheme) Valid() bool {
	switch s {
	case SchemeNone, SchemeAPIKey, SchemeOAuth, SchemeSigV4, SchemeWorkloadIdentity:
		return true
	default:
		return false
	}
}

func (s Scheme) IsZero() bool { return s == "" }
func (s Scheme) String() string {
	if s.IsZero() {
		return ""
	}
	if !s.Valid() {
		return "invalid"
	}
	return string(s)
}
func (s Scheme) Format(state fmt.State, _ rune) { formatSafe(state, s.String()) }
func (s Scheme) GoString() string               { return s.String() }
func (s Scheme) LogValue() slog.Value           { return slog.StringValue(s.String()) }

// UsageClass describes how use of a credential is accounted for independently
// of the authority mechanism.
type UsageClass string

const (
	UsageLocal        UsageClass = "local"
	UsageMeteredAPI   UsageClass = "metered_api"
	UsageSubscription UsageClass = "subscription"
)

func (u UsageClass) Valid() bool {
	switch u {
	case UsageLocal, UsageMeteredAPI, UsageSubscription:
		return true
	default:
		return false
	}
}

func (u UsageClass) IsZero() bool { return u == "" }
func (u UsageClass) String() string {
	if u.IsZero() {
		return ""
	}
	if !u.Valid() {
		return "invalid"
	}
	return string(u)
}
func (u UsageClass) Format(state fmt.State, _ rune) { formatSafe(state, u.String()) }
func (u UsageClass) GoString() string               { return u.String() }
func (u UsageClass) LogValue() slog.Value           { return slog.StringValue(u.String()) }

const (
	MaxDescriptorLength           = 1024
	MaxDescriptorFieldLength      = 256
	MaxDescriptorIdentifierLength = 96
)

// Descriptor is a bounded, secret-free binding between a source and the
// exact provider transport it may authorize. Authenticated bindings require
// non-empty issuer and audience values; the explicit none/local binding is
// the only unauthenticated exception and requires both values to be empty.
type Descriptor struct {
	Provider  string
	Transport string
	Scheme    Scheme
	Usage     UsageClass
	Issuer    string
	Audience  string
	Label     string
}

// NewDescriptor validates and canonicalizes a descriptor. Provider and
// transport identifiers are lower-case; safe textual identity fields are
// trimmed but retain case because URI and audience paths can be case-sensitive.
func NewDescriptor(provider, transport string, scheme Scheme, usage UsageClass, issuer, audience, label string) (Descriptor, error) {
	provider, err := canonicalDescriptorIdentifier(provider, "provider")
	if err != nil {
		return Descriptor{}, err
	}
	transport, err = canonicalDescriptorIdentifier(transport, "transport")
	if err != nil {
		return Descriptor{}, err
	}
	if !scheme.Valid() {
		return Descriptor{}, NewInvalidDescriptorError("scheme")
	}
	if !usage.Valid() {
		return Descriptor{}, NewInvalidDescriptorError("usage")
	}
	if scheme == SchemeNone && usage == UsageLocal {
		if issuer != "" {
			return Descriptor{}, NewInvalidDescriptorError("issuer")
		}
		if audience != "" {
			return Descriptor{}, NewInvalidDescriptorError("audience")
		}
		issuer, err = canonicalDescriptorText(issuer, "issuer")
		if err != nil {
			return Descriptor{}, err
		}
		audience, err = canonicalDescriptorText(audience, "audience")
		if err != nil {
			return Descriptor{}, err
		}
	} else {
		issuer, err = canonicalDescriptorAuthority(issuer, "issuer")
		if err != nil {
			return Descriptor{}, err
		}
		audience, err = canonicalDescriptorAuthority(audience, "audience")
		if err != nil {
			return Descriptor{}, err
		}
	}
	label, err = canonicalDescriptorText(label, "label")
	if err != nil {
		return Descriptor{}, err
	}
	descriptor := Descriptor{
		Provider: provider, Transport: transport, Scheme: scheme, Usage: usage,
		Issuer: issuer, Audience: audience, Label: label,
	}
	if len(strings.Join([]string{descriptor.Provider, descriptor.Transport, string(descriptor.Scheme), string(descriptor.Usage), descriptor.Issuer, descriptor.Audience, descriptor.Label}, "\x1f")) > MaxDescriptorLength {
		return Descriptor{}, NewInvalidDescriptorError("length")
	}
	return descriptor, nil
}

func canonicalDescriptorIdentifier(raw, field string) (string, error) {
	if len(raw) == 0 || len(raw) > MaxDescriptorIdentifierLength {
		return "", NewInvalidDescriptorError(field)
	}
	if !utf8.ValidString(raw) {
		return "", NewInvalidDescriptorError(field)
	}
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch {
		case c >= 'A' && c <= 'Z':
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
		default:
			return "", NewInvalidDescriptorError(field)
		}
	}
	canonical := strings.ToLower(raw)
	if canonical[0] == '.' || canonical[0] == '-' || canonical[0] == '_' ||
		canonical[len(canonical)-1] == '.' || canonical[len(canonical)-1] == '-' || canonical[len(canonical)-1] == '_' {
		return "", NewInvalidDescriptorError(field)
	}
	return canonical, nil
}

func canonicalDescriptorText(raw, field string) (string, error) {
	if len(raw) > MaxDescriptorFieldLength {
		return "", NewInvalidDescriptorError(field)
	}
	if !utf8.ValidString(raw) {
		return "", NewInvalidDescriptorError(field)
	}
	canonical := strings.TrimSpace(raw)
	if len(canonical) > MaxDescriptorFieldLength {
		return "", NewInvalidDescriptorError(field)
	}
	for i := 0; i < len(canonical); i++ {
		c := canonical[i]
		if c < 0x20 || c == 0x7f || c == '?' || c == '#' || c == '@' {
			return "", NewInvalidDescriptorError(field)
		}
	}
	return canonical, nil
}

// canonicalDescriptorAuthority accepts a bounded URI-like authority binding
// without assigning provider-specific meaning to it. The syntax is ASCII and
// uses RFC 3986's unreserved, sub-delimiter, and path/authority punctuation,
// except query, fragment, and user-info delimiters. Percent escapes must be
// complete so the value is safe to use as an exact opaque binding.
func canonicalDescriptorAuthority(raw, field string) (string, error) {
	if len(raw) == 0 || len(raw) > MaxDescriptorFieldLength {
		return "", NewInvalidDescriptorError(field)
	}
	if !utf8.ValidString(raw) {
		return "", NewInvalidDescriptorError(field)
	}
	canonical := strings.TrimSpace(raw)
	if len(canonical) == 0 || len(canonical) > MaxDescriptorFieldLength {
		return "", NewInvalidDescriptorError(field)
	}
	for i := 0; i < len(canonical); i++ {
		c := canonical[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case strings.ContainsRune("-._~!$&'()*+,;=:/[]", rune(c)):
		case c == '%':
			if i+2 >= len(canonical) || !isHex(canonical[i+1]) || !isHex(canonical[i+2]) {
				return "", NewInvalidDescriptorError(field)
			}
			i += 2
		default:
			return "", NewInvalidDescriptorError(field)
		}
	}
	return canonical, nil
}

func isHex(value byte) bool {
	return (value >= '0' && value <= '9') ||
		(value >= 'a' && value <= 'f') ||
		(value >= 'A' && value <= 'F')
}

// Validate verifies that an exported descriptor value is one that a
// constructor could have produced. It never mutates the receiver.
func (d Descriptor) Validate() error {
	canonical, err := NewDescriptor(d.Provider, d.Transport, d.Scheme, d.Usage, d.Issuer, d.Audience, d.Label)
	if err != nil {
		return err
	}
	if canonical != d {
		return NewInvalidDescriptorError("descriptor")
	}
	return nil
}

func (d Descriptor) Valid() bool { return d.Validate() == nil }

// Canonical returns a stable, bounded key for exact descriptor matching. The
// separator is package-owned and cannot appear in any safe field.
func (d Descriptor) Canonical() string {
	if err := d.Validate(); err != nil {
		return ""
	}
	return strings.Join([]string{d.Provider, d.Transport, string(d.Scheme), string(d.Usage), d.Issuer, d.Audience, d.Label}, "\x1f")
}

func (d Descriptor) String() string {
	if !d.Valid() {
		return ErrInvalidDescriptor.Error()
	}
	return d.Provider + "/" + d.Transport + " " + string(d.Scheme) + "/" + string(d.Usage)
}

func (d Descriptor) Format(state fmt.State, _ rune) { formatSafe(state, d.String()) }
func (d Descriptor) GoString() string               { return d.String() }
func (d Descriptor) LogValue() slog.Value           { return slog.StringValue(d.String()) }
