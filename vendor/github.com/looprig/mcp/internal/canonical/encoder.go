// Package canonical owns the module's canonical encoding: the framing every
// identity digest in this module is computed over.
//
// It exists because there is more than one thing worth digesting — a server's
// catalog (internal/catalog), a binding's configuration identity
// (pkg/harness) — and they must agree on how bytes are framed. Two encoders
// that start identical and drift apart are the specific hazard: the drift is
// invisible (each side's own golden test still passes) and its symptom is two
// digests that disagree about content they agree on, or worse, agree about
// content they differ on.
//
// The framing gives every caller two properties that a naive hash of a struct
// does not have:
//
//   - Determinism. The same content produces the same digest however it
//     arrived. Nothing here iterates a map; ordering collections canonically
//     before encoding them is the caller's job, because only the caller knows
//     which orderings are content and which are incidental.
//   - Unambiguity. No two different inputs encode to the same bytes. Every
//     value is length-delimited or fixed-width, so fields cannot be slid into
//     one another: Str("ab") + Str("") and Str("a") + Str("b") are distinct
//     encodings where a naive concatenation would render both as "ab".
//
// The scheme follows the fingerprint guidance in the session-versioning design:
// explicit domain, explicit schema version, stable field ordering,
// length-delimited values, deterministic collection ordering. This package
// supplies the framing; each caller supplies its own domain tag, its own schema
// version, and its own field order, because those are the parts that define
// what a particular digest means.
package canonical

import (
	"encoding/binary"
	"hash"
)

// Encoder writes the canonical encoding into a hash. Every method is
// length-delimited or fixed-width, so the concatenation of any sequence of
// calls is unambiguous.
//
// It writes to a hash.Hash, which never errors (hash.Hash's Write contract
// forbids it), so no method here returns one. That is the reason this is not an
// io.Writer wrapper with error plumbing nobody could act on.
type Encoder struct {
	h hash.Hash
}

// NewEncoder returns an Encoder that writes into h.
func NewEncoder(h hash.Hash) *Encoder { return &Encoder{h: h} }

// Field writes a field tag. It is a length-delimited string like any other,
// which is enough to separate fields: the hash sees a different byte sequence
// for any different tag, and a tag can never be confused with a value because
// both are framed identically.
//
// A field tag is not needed for unambiguity — the length delimiters already
// supply that — it is needed for meaning: it pins a value to the field it was
// read from, so that reordering two same-typed fields changes the digest.
func (e *Encoder) Field(tag string) { e.Str(tag) }

// Str writes a length-delimited string.
func (e *Encoder) Str(s string) {
	e.length(len(s))
	_, _ = e.h.Write([]byte(s))
}

// Bytes writes a length-delimited byte string.
func (e *Encoder) Bytes(b []byte) {
	e.length(len(b))
	_, _ = e.h.Write(b)
}

// Uint writes a fixed-width unsigned integer. Fixed-width rather than varint:
// the width is then never a function of the value, so no encoding of one
// integer can be a prefix of the encoding of another.
func (e *Encoder) Uint(u uint64) {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], u)
	_, _ = e.h.Write(buf[:])
}

// Int writes a signed integer as a sign byte followed by a magnitude, rather
// than by casting a possibly-negative value straight to uint64.
//
// The sign is written separately so that the magnitude is always a genuine
// count. A bare uint64(i) would encode -1 as 2^64-1, which is a legal encoding
// of a legal value — so a negative field and an enormous positive one would
// share a digest, and a configuration error would be indistinguishable from a
// configuration.
func (e *Encoder) Int(i int64) {
	if i < 0 {
		e.Bool(true)
		// The magnitude of a negative i, computed without overflow: -i is
		// undefined for math.MinInt64, so the value is folded to -(i+1) —
		// which is in [0, MaxInt64] for every negative i, including the
		// minimum — and the borrowed 1 is added back in uint64 space.
		//
		// #nosec G115 -- this conversion is the one gosec cannot prove and the
		// comment above is its proof: i < 0 is checked on the line above, so
		// i+1 is in [MinInt64+1, 0] and -(i+1) is in [0, MaxInt64]. It is
		// non-negative by construction, so the conversion cannot wrap.
		e.Uint(uint64(-(i + 1)) + 1)
		return
	}
	e.Bool(false)
	// Non-negative by the branch above, so this conversion is exact.
	e.Uint(uint64(i))
}

// Count writes a collection's length.
func (e *Encoder) Count(n int) { e.length(n) }

// length writes a value's byte length: the delimiter every Str and Bytes call
// depends on, and the collection size every loop depends on.
//
// Every caller passes a len(), which is never negative, so the guard below is
// unreachable. It is written as a guard rather than asserted in a comment
// because the conversion to uint64 is the one place a negative would not fail
// loudly — it would wrap to an enormous length and silently produce a digest
// for a framing that never existed.
func (e *Encoder) length(n int) {
	if n < 0 {
		n = 0
	}
	e.Uint(uint64(n))
}

// Bool writes a single byte.
func (e *Encoder) Bool(b bool) {
	var v byte
	if b {
		v = 1
	}
	_, _ = e.h.Write([]byte{v})
}

// TriState writes an optional bool in one byte, keeping "absent" distinct from
// "false" — which is the whole reason a protocol models an optional hint as a
// pointer.
func (e *Encoder) TriState(p *bool) {
	switch {
	case p == nil:
		_, _ = e.h.Write([]byte{0})
	case *p:
		_, _ = e.h.Write([]byte{2})
	default:
		_, _ = e.h.Write([]byte{1})
	}
}
