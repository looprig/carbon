// Package secrets defines the opaque value and reference contracts used by
// LoopRig's credential stores.  Secret values deliberately have no useful
// ordinary representation: callers must opt in to Bytes at the point where a
// value is consumed.
package secrets

import (
	"errors"
	"fmt"
	"log/slog"
)

// MaxSecretSize is the largest value accepted by New.  Keeping the bound in
// the base package gives stores a common limit to enforce before allocating
// or decoding an envelope.
const MaxSecretSize = 1 << 20

var (
	// ErrEmptySecret identifies an empty value passed to New.
	ErrEmptySecret = errors.New("secrets: empty secret")
	// ErrSecretTooLarge identifies a value over MaxSecretSize.
	ErrSecretTooLarge = errors.New("secrets: secret value too large")
	// ErrZeroSecret identifies an invalid zero Secret supplied to a record or
	// mutation.  A zero Secret has no value that can be disclosed or stored.
	ErrZeroSecret = errors.New("secrets: zero secret")
)

const redactedSecret = "[REDACTED]"

// Secret is an immutable opaque value.  Its bytes are private and all normal
// formatting and structured logging paths are redacted.
//
// The zero value is invalid.  Construct values with New and call Bytes only at
// an explicit value-consumption boundary.
type Secret struct {
	value []byte
}

// EmptySecretError reports an empty value passed to New.
type EmptySecretError struct{}

func (e *EmptySecretError) Error() string { return ErrEmptySecret.Error() }

func (e *EmptySecretError) Unwrap() error { return ErrEmptySecret }

func (e *EmptySecretError) Format(state fmt.State, verb rune) {
	formatSafeError(state, e.Error())
}

func (e *EmptySecretError) GoString() string { return e.Error() }

func (e *EmptySecretError) LogValue() slog.Value { return slog.StringValue(e.Error()) }

// SecretSizeError reports a value larger than the module's bound.  It carries
// only sizes, never any value bytes.
type SecretSizeError struct {
	Limit int
	Got   int
}

func (e *SecretSizeError) Error() string {
	if e == nil {
		return ErrSecretTooLarge.Error()
	}
	return fmt.Sprintf("secrets: secret value exceeds %d-byte limit (got %d)", e.Limit, e.Got)
}

func (e *SecretSizeError) Unwrap() error { return ErrSecretTooLarge }

func (e *SecretSizeError) Format(state fmt.State, verb rune) {
	formatSafeError(state, e.Error())
}

func (e *SecretSizeError) GoString() string { return e.Error() }

func (e *SecretSizeError) LogValue() slog.Value { return slog.StringValue(e.Error()) }

// ZeroSecretError reports use of a zero Secret where a value is required.
type ZeroSecretError struct{}

func (e *ZeroSecretError) Error() string { return ErrZeroSecret.Error() }

func (e *ZeroSecretError) Unwrap() error { return ErrZeroSecret }

func (e *ZeroSecretError) Format(state fmt.State, verb rune) {
	formatSafeError(state, e.Error())
}

func (e *ZeroSecretError) GoString() string { return e.Error() }

func (e *ZeroSecretError) LogValue() slog.Value { return slog.StringValue(e.Error()) }

// New copies value and returns an opaque Secret.  The input remains owned by
// the caller and may be changed or cleared immediately after this call.
func New(value []byte) (Secret, error) {
	if len(value) == 0 {
		return Secret{}, &EmptySecretError{}
	}
	if len(value) > MaxSecretSize {
		return Secret{}, &SecretSizeError{Limit: MaxSecretSize, Got: len(value)}
	}
	copyValue := make([]byte, len(value))
	copy(copyValue, value)
	return Secret{value: copyValue}, nil
}

// Bytes returns a copy of the value.  It returns nil for the invalid zero
// Secret, so the zero value never discloses a value or appears storable.
func (s Secret) Bytes() []byte {
	if len(s.value) == 0 {
		return nil
	}
	value := make([]byte, len(s.value))
	copy(value, s.value)
	return value
}

// Valid reports whether s was constructed by New and contains a bounded,
// non-empty value.
func (s Secret) Valid() bool {
	return len(s.value) > 0 && len(s.value) <= MaxSecretSize
}

// IsZero reports whether s is the invalid zero Secret.
func (s Secret) IsZero() bool { return len(s.value) == 0 }

// Validate returns a typed error when s is the zero/invalid value.
func (s Secret) Validate() error {
	if !s.Valid() {
		return &ZeroSecretError{}
	}
	return nil
}

// String implements fmt.Stringer and always returns a fixed redaction.
func (s Secret) String() string { return redactedSecret }

// Format implements fmt.Formatter and ignores every verb, flag, width, and
// precision. This prevents numeric and nested formatting from falling back to
// the unexported byte slice representation.
func (s Secret) Format(state fmt.State, verb rune) {
	formatSafeError(state, redactedSecret)
}

// GoString implements fmt.GoStringer for %#v and always returns a fixed
// redaction.  It deliberately does not include the underlying byte length.
func (s Secret) GoString() string { return redactedSecret }

// LogValue implements slog.LogValuer and keeps Secret values out of
// structured logs, including when passed through slog.Any.
func (s Secret) LogValue() slog.Value { return slog.StringValue(redactedSecret) }
