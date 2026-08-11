package chutes

import (
	"crypto/mlkem"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	"github.com/looprig/inference"
	"github.com/looprig/inference/codec/openaiapi"
	"github.com/looprig/llm/e2e"
)

// decodeResponse opens the sealed /e2e/invoke response: respMlkemCT(1088) is
// decapsulated with the ephemeral response key to recover the shared secret,
// then e2e.Open derives the e2e-resp-v1 key, AEAD-opens, and gunzips the
// OpenAI JSON. The plaintext is then parsed into a provider-neutral
// *inference.Response.
func decodeResponse(body []byte, respDK *mlkem.DecapsulationKey768) (*inference.Response, error) {
	if len(body) < e2e.MLKEMCTSize {
		return nil, &e2e.Error{Op: "open response", Err: e2e.ErrShortBlob}
	}
	respCT := body[:e2e.MLKEMCTSize]
	blob := body[e2e.MLKEMCTSize:]
	shared, err := respDK.Decapsulate(respCT)
	if err != nil {
		return nil, &e2e.Error{Op: "decapsulate response", Err: err}
	}
	plaintext, err := e2e.Open(shared, respCT, blob, []byte("e2e-resp-v1"), true)
	if err != nil {
		return nil, err
	}
	return openaiapi.DecodeResponse(plaintext)
}

// tryDecryptErrorBody peels chutes' two-layer error wrapping so apiError
// surfaces something readable. HTTP body is plaintext JSON
// `{"detail": "<upstream body, sometimes base64 of binary>"}`. When the inner
// payload happens to be an e2e envelope sealed to our response key (rare in
// practice — chutes-api just relays response.text per its source), we open it.
// When it's opaque binary (the common case, because the upstream model layer
// emits its own un-keyed binary blob — verified live), we substitute a
// synthetic body whose detail names the most likely cause, so callers do not
// see ~2KB of base64 garbage in chat. Returns the body to hand to apiError.
func tryDecryptErrorBody(body []byte, respDK *mlkem.DecapsulationKey768) []byte {
	if respDK == nil {
		return body
	}
	if plaintext := tryDecryptJSONWrappedDetail(body, respDK); plaintext != nil {
		return plaintext
	}
	if plaintext := tryDecryptRawEnvelope(body, respDK); plaintext != nil {
		return plaintext
	}
	// Couldn't decrypt. Two sub-cases:
	//   - body is {"detail":"<opaque base64 of binary>"} — substitute a
	//     friendly synthetic detail so chat shows actionable text.
	//   - body is something else (raw binary, plain text, JSON without
	//     detail, …) — leave it alone so apiError can still try its
	//     extractors.
	if synthetic := synthesizeOpaqueDetail(body); synthetic != nil {
		return synthetic
	}
	dumpUndecryptableBody(body)
	return body
}

// synthesizeOpaqueDetail recognizes {"detail":"<base64 of binary>"} bodies
// and returns a substitute JSON body whose detail is a human-readable
// explanation. Returns nil if body does not match that shape (so the caller
// preserves the original bytes for the next layer to inspect).
func synthesizeOpaqueDetail(body []byte) []byte {
	var env struct {
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.Detail == "" {
		return nil
	}
	decoded, derr := base64.StdEncoding.DecodeString(env.Detail)
	if derr != nil || len(decoded) < 32 {
		return nil
	}
	// Heuristic: if the decoded bytes are not printable, treat as opaque.
	// 32 bytes is enough to distinguish (binary entropy will have <50%
	// printable; plain text or JSON will have near-100%).
	printable := 0
	for _, b := range decoded[:32] {
		if (b >= 0x20 && b < 0x7f) || b == '\n' || b == '\r' || b == '\t' {
			printable++
		}
	}
	if printable >= 24 { // >= 75% printable: probably text after all, keep original
		return nil
	}
	msg := fmt.Sprintf(
		"chutes returned an opaque encrypted error (%d bytes, client cannot decrypt). "+
			"Most common cause: prompt exceeded the model's context window. "+
			"Run a smaller prompt or check the model's context_length on /v1/models.",
		len(decoded),
	)
	out, err := json.Marshal(map[string]string{"detail": msg})
	if err != nil {
		return nil
	}
	dumpUndecryptableBody(body) // still capture forensics in case the heuristic was wrong
	return out
}

// maxDumpBodySize caps how much of an undecryptable body we write to disk.
// Prevents an adversarially large response from filling the temp filesystem.
const maxDumpBodySize = 1 << 20 // 1 MiB

// dumpUndecryptableBody persists a body we couldn't decrypt to a unique temp
// file and logs the location. Cheap forensics: lets a maintainer compare the
// real wire format against our assumed envelope. Best-effort; any IO failure
// is silently ignored (we still surface the raw body to the caller).
func dumpUndecryptableBody(body []byte) {
	if len(body) > maxDumpBodySize {
		slog.Warn("chutes: error body decryption failed; body too large to dump",
			"size", len(body),
			"limit", maxDumpBodySize,
		)
		return
	}
	f, err := os.CreateTemp("", "chutes-undecryptable-*.bin")
	if err != nil {
		return
	}
	_, _ = f.Write(body)
	_ = f.Close()
	slog.Warn("chutes: error body decryption failed; dumped raw bytes for analysis",
		"path", f.Name(),
		"size", len(body),
	)
	// If body is JSON with a base64 detail, also dump the decoded inner bytes
	// so we don't have to chain `jq .detail | base64 -d` to inspect them.
	var env struct {
		Detail string `json:"detail"`
	}
	if json.Unmarshal(body, &env) != nil || env.Detail == "" {
		return
	}
	decoded, derr := base64.StdEncoding.DecodeString(env.Detail)
	if derr != nil {
		return
	}
	g, err := os.CreateTemp("", "chutes-undecryptable-detail-*.bin")
	if err != nil {
		return
	}
	_, _ = g.Write(decoded)
	_ = g.Close()
	slog.Warn("chutes: also dumped decoded detail bytes",
		"path", g.Name(),
		"size", len(decoded),
	)
}

// tryDecryptJSONWrappedDetail handles {"detail":"<base64-of-e2e-envelope>"}.
// Returns nil (NOT body) on any failure so the caller can fall through.
func tryDecryptJSONWrappedDetail(body []byte, respDK *mlkem.DecapsulationKey768) []byte {
	var env struct {
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.Detail == "" {
		return nil
	}
	decoded, err := base64.StdEncoding.DecodeString(env.Detail)
	if err != nil {
		return nil
	}
	return tryDecryptRawEnvelope(decoded, respDK)
}

// tryDecryptRawEnvelope handles a bare mlkem_ct || nonce || ct || tag blob.
// Returns nil on any failure (length too small, decap fail, open fail).
func tryDecryptRawEnvelope(blob []byte, respDK *mlkem.DecapsulationKey768) []byte {
	if len(blob) < e2e.MLKEMCTSize+e2e.NonceSize+e2e.TagSize {
		return nil
	}
	respCT := blob[:e2e.MLKEMCTSize]
	sealed := blob[e2e.MLKEMCTSize:]
	shared, err := respDK.Decapsulate(respCT)
	if err != nil {
		return nil
	}
	if plaintext, err := e2e.Open(shared, respCT, sealed, []byte("e2e-resp-v1"), true); err == nil {
		return plaintext
	}
	if plaintext, err := e2e.Open(shared, respCT, sealed, []byte("e2e-resp-v1"), false); err == nil {
		return plaintext
	}
	return nil
}
