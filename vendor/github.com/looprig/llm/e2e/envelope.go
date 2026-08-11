package e2e

import (
	"bytes"
	"compress/gzip"
	"crypto/hkdf"
	"crypto/mlkem"
	"crypto/rand"
	"crypto/sha256"
	"io"

	"golang.org/x/crypto/chacha20poly1305"
)

// Pinned wire sizes (chutes WIRE.md sections 1-2).
const (
	MLKEMCTSize = 1088 // ML-KEM-768 ciphertext
	SaltSize    = 16   // HKDF salt = mlkem_ct[:16]
	KeySize     = 32   // ChaCha20-Poly1305 key / ML-KEM shared secret
	NonceSize   = chacha20poly1305.NonceSize
	TagSize     = chacha20poly1305.Overhead
)

// DeriveKey computes the per-message AEAD key:
// HKDF-SHA256(ikm=shared, salt=mlkemCT[:16], info), 32 bytes. The salt is the
// first 16 bytes of THIS message's ML-KEM ciphertext, not a constant
// (chutes WIRE.md section 1).
func DeriveKey(shared, mlkemCT, info []byte) ([]byte, error) {
	if len(mlkemCT) < SaltSize {
		return nil, &Error{Op: "hkdf", Err: ErrShortBlob}
	}
	key, err := hkdf.Key(sha256.New, shared, mlkemCT[:SaltSize], string(info), KeySize)
	if err != nil {
		return nil, &Error{Op: "hkdf", Err: err}
	}
	return key, nil
}

// Seal encapsulates a fresh shared secret to the recipient's ML-KEM-768
// encapsulation key, derives the direction key via info, optionally gzips the
// plaintext, then AEAD-seals it under a random nonce. It returns the ML-KEM
// ciphertext (1088 bytes) and blob = nonce(12) || ciphertext || tag(16).
// Callers prepend mlkemCT to blob for the request/response wire layout; stream
// frames omit it.
func Seal(plaintext, recipientPub, info []byte, gzipFirst bool) (mlkemCT, blob []byte, err error) {
	ek, err := mlkem.NewEncapsulationKey768(recipientPub)
	if err != nil {
		return nil, nil, &Error{Op: "parse pubkey", Err: err}
	}
	shared, mlkemCT := ek.Encapsulate()

	key, err := DeriveKey(shared, mlkemCT, info)
	if err != nil {
		return nil, nil, err
	}

	pt := plaintext
	if gzipFirst {
		pt, err = gzipBytes(plaintext)
		if err != nil {
			return nil, nil, err
		}
	}

	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, nil, &Error{Op: "aead init", Err: err}
	}
	nonce := make([]byte, NonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, &Error{Op: "nonce", Err: err}
	}
	// Seal appends ct||tag after nonce, giving nonce||ct||tag.
	blob = aead.Seal(nonce, nonce, pt, nil)
	return mlkemCT, blob, nil
}

// Open reverses Seal given the already-decapsulated shared secret, the
// mlkemCT used for the HKDF salt, and blob = nonce || ct || tag. It derives
// the direction key from info, AEAD-opens, and optionally gunzips.
func Open(shared, mlkemCT, blob, info []byte, gunzip bool) ([]byte, error) {
	key, err := DeriveKey(shared, mlkemCT, info)
	if err != nil {
		return nil, err
	}
	pt, err := OpenFrame(key, blob)
	if err != nil {
		return nil, err
	}
	if gunzip {
		return gunzipBytes(pt)
	}
	return pt, nil
}

// OpenFrame opens a single AEAD frame (blob = nonce || ct || tag) with an
// already-derived key. This is the stream path: the stream key is derived once
// from the e2e_init ciphertext and reused, and stream frames are never gzipped.
func OpenFrame(key, blob []byte) ([]byte, error) {
	if len(blob) < NonceSize+TagSize {
		return nil, &Error{Op: "open", Err: ErrShortBlob}
	}
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, &Error{Op: "aead init", Err: err}
	}
	nonce := blob[:NonceSize]
	ct := blob[NonceSize:]
	pt, err := aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, &Error{Op: "aead open", Err: err}
	}
	return pt, nil
}

// SealFrame is the inverse of OpenFrame: it AEAD-seals plaintext under an
// already-derived key and a fresh random nonce, returning nonce || ct || tag.
// No ML-KEM, no gzip — this is exactly the stream-chunk wire layout (chutes
// WIRE.md section 2). It exists so test/fixture code can produce real e2e
// frames; the production client never seals stream frames (the server does).
func SealFrame(key, plaintext []byte) ([]byte, error) {
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, &Error{Op: "aead init", Err: err}
	}
	nonce := make([]byte, NonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, &Error{Op: "nonce", Err: err}
	}
	return aead.Seal(nonce, nonce, plaintext, nil), nil
}

func gzipBytes(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(data); err != nil {
		return nil, &Error{Op: "gzip", Err: err}
	}
	if err := w.Close(); err != nil {
		return nil, &Error{Op: "gzip", Err: err}
	}
	return buf.Bytes(), nil
}

func gunzipBytes(data []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, &Error{Op: "gunzip", Err: err}
	}
	out, err := io.ReadAll(r)
	if err != nil {
		return nil, &Error{Op: "gunzip", Err: err}
	}
	return out, nil
}
