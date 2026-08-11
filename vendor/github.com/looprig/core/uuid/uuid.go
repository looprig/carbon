package uuid

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"strconv"
)

// UUID is a 128-bit identifier stored as a raw 16-byte array.
type UUID [16]byte

// GenerateError reports a failure to read randomness while generating a UUID.
// It wraps the underlying reader error so callers can errors.As to a
// *GenerateError and errors.Unwrap (or read .Err) to inspect the cause.
type GenerateError struct{ Err error }

func (e *GenerateError) Error() string { return "uuid: generate: " + e.Err.Error() }

func (e *GenerateError) Unwrap() error { return e.Err }

// New returns a version-4 (random) UUID sourced from crypto/rand. It returns a
// *GenerateError if the randomness source cannot supply 16 bytes.
func New() (UUID, error) { return newFromReader(rand.Reader) }

// newFromReader is the testable seam behind New: it reads 16 random bytes from r
// and stamps the RFC-4122 version-4 and variant bits. A read failure (including
// a short read) yields the zero UUID and a *GenerateError. It is unexported so
// only New (with crypto/rand) is part of the public surface.
func newFromReader(r io.Reader) (UUID, error) {
	var u UUID
	if _, err := io.ReadFull(r, u[:]); err != nil {
		return UUID{}, &GenerateError{Err: err}
	}
	u[6] = (u[6] & 0x0f) | 0x40 // version 4
	u[8] = (u[8] & 0x3f) | 0x80 // variant 10 (RFC 4122)
	return u, nil
}

// errInvalidText is the context-free leaf sentinel for text that is not a
// structurally valid 8-4-4-4-12 hyphenated encoding (wrong length, a hyphen off
// its fixed offset, or a non-hex digit in a hex field). Parse wraps it in a
// *ParseError that adds the offending input, so callers can both
// errors.Is(err, errInvalidText) and errors.As(err, &ParseError{}).
var errInvalidText = errors.New("uuid: invalid text encoding")

// ParseError reports that a string was not a valid canonical UUID encoding. It
// carries the offending Input (always a UUID-sized string, so including it is
// bounded) and wraps the leaf sentinel errInvalidText as Err, which callers can
// reach via errors.As to a *ParseError or errors.Unwrap / errors.Is.
type ParseError struct {
	Input string
	Err   error
}

func (e *ParseError) Error() string {
	return "uuid: parse " + strconv.Quote(e.Input) + ": " + e.Err.Error()
}

func (e *ParseError) Unwrap() error { return e.Err }

// Parse decodes the canonical 8-4-4-4-12 hyphenated form into a UUID. Hyphens
// must sit at their fixed offsets and the remaining positions must be hex
// digits; hex is case-insensitive, and the decoded value is normalized so
// String/MarshalText always re-emit lowercase. Any structural or hex failure
// yields the zero UUID and a *ParseError wrapping errInvalidText (fail-secure:
// no partial value on error).
func Parse(s string) (UUID, error) {
	if len(s) != 36 ||
		s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return UUID{}, &ParseError{Input: s, Err: errInvalidText}
	}
	// Strip the hyphens into a contiguous 32-char hex buffer for decoding.
	var hexBuf [32]byte
	copy(hexBuf[0:8], s[0:8])
	copy(hexBuf[8:12], s[9:13])
	copy(hexBuf[12:16], s[14:18])
	copy(hexBuf[16:20], s[19:23])
	copy(hexBuf[20:32], s[24:36])
	var out UUID
	if _, err := hex.Decode(out[:], hexBuf[:]); err != nil {
		return UUID{}, &ParseError{Input: s, Err: errInvalidText}
	}
	return out, nil
}

// MustParse is Parse for pinned, package-level constant IDs: it returns the
// decoded UUID or panics on a parse error. Use it only with compile-time
// literals you control, never with external input.
func MustParse(s string) UUID {
	u, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

// String returns the canonical lowercase 8-4-4-4-12 hyphenated hex encoding.
func (u UUID) String() string {
	// 32 hex chars + 4 hyphens.
	buf := make([]byte, 36)
	hex.Encode(buf[0:8], u[0:4])
	buf[8] = '-'
	hex.Encode(buf[9:13], u[4:6])
	buf[13] = '-'
	hex.Encode(buf[14:18], u[6:8])
	buf[18] = '-'
	hex.Encode(buf[19:23], u[8:10])
	buf[23] = '-'
	hex.Encode(buf[24:36], u[10:16])
	return string(buf)
}

// IsZero reports whether u is the zero UUID (absent / root).
func (u UUID) IsZero() bool { return u == UUID{} }

// MarshalText encodes u as its canonical 8-4-4-4-12 hyphenated form, so JSON
// (and any other encoding.TextMarshaler consumer) emits a readable string
// rather than a 16-int byte array.
func (u UUID) MarshalText() ([]byte, error) { return []byte(u.String()), nil }

// UnmarshalText parses the 8-4-4-4-12 hyphenated form back into the 16 bytes by
// delegating to Parse (the single parsing implementation). The input must have
// hyphens at the fixed offsets and hex digits elsewhere; hex digits are
// case-insensitive (both 0x0a and 0x0A decode to 0x0a). The decoded value is
// normalized, so String/MarshalText always re-emit lowercase. Structurally
// invalid input returns a *ParseError (which unwraps to errInvalidText) and
// leaves the receiver unchanged (no partial write).
func (u *UUID) UnmarshalText(text []byte) error {
	p, err := Parse(string(text))
	if err != nil {
		return err
	}
	*u = p
	return nil
}
