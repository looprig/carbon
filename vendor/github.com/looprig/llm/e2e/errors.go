// Package e2e — shared end-to-end envelope primitives. ML-KEM-768
// encapsulation, HKDF-SHA256 key derivation, ChaCha20-Poly1305 AEAD, and gzip,
// in the wire layout used by chutes. Each provider package owns its own
// discovery, attestation, transport, and stream handling; this package only
// translates plaintext <-> wire envelopes.
package e2e

import (
	"errors"
	"fmt"
)

// ErrShortBlob is returned when an encrypted wire blob is too short to hold the
// required nonce and authentication tag. Exported for callers that branch on it
// via errors.Is.
var ErrShortBlob = errors.New("blob shorter than nonce+tag")

// Error wraps any envelope failure: HKDF derivation, ML-KEM
// encapsulation/decapsulation, ChaCha20-Poly1305 seal/open, gzip, or a
// malformed wire blob. Op names the failing operation; Err is the cause and is
// inspectable via errors.As / errors.Is.
type Error struct {
	Op  string
	Err error
}

func (e *Error) Error() string { return fmt.Sprintf("e2e: %s: %v", e.Op, e.Err) }
func (e *Error) Unwrap() error { return e.Err }
