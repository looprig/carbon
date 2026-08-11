package tee

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"

	failure "github.com/looprig/inference/failure"
)

// GPUEvidence is one entry of the per-instance gpu_evidence list returned by a
// TEE attestation endpoint: a base64 NVIDIA attestation report and the base64
// cert chain that signed it, plus the GPU architecture ("HOPPER", "BLACKWELL").
// Exported because both llm/chutes and llm/phala build this slice from the
// provider-specific attestation response.
type GPUEvidence struct {
	Certificate string `json:"certificate"`
	Evidence    string `json:"evidence"`
	Arch        string `json:"arch"`
}

// gpuNonceOffset is where the 32-byte requester nonce sits in the decoded
// NVIDIA GPU attestation report: after the 4-byte SPDM 1.1 MEASUREMENTS
// response header (SPDMVersion, RequestResponseCode, Param1, Param2). The
// server generates this nonce and binds the GPU evidence to it; NRAS validates
// the request nonce against it (so it is NOT the TDX-quote nonce).
const gpuNonceOffset = 4

// gpuEvidenceNonce decodes a base64 GPU evidence blob and returns the 64-hex
// requester nonce the server bound it to (bytes [4:36]). This is the nonce that
// must be sent to NRAS, not the TDX-quote nonce.
func gpuEvidenceNonce(evidenceB64 string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(evidenceB64)
	if err != nil {
		return "", &Error{Reason: ReasonNvidiaVerdictInvalid, Err: fmt.Errorf("decode gpu evidence: %w", err)}
	}
	if len(raw) < gpuNonceOffset+32 {
		return "", &Error{Reason: ReasonNvidiaVerdictInvalid, Err: errors.New("gpu evidence too short for nonce")}
	}
	return hex.EncodeToString(raw[gpuNonceOffset : gpuNonceOffset+32]), nil
}

// VerifyGPUEvidence POSTs the GPU evidence to NRAS (nrasURL), then verifies
// the returned EAT JWT (ES384) against the NVIDIA JWKS (jwksURL) and asserts
// the x-nvidia-overall-att-result claim is true. The NRAS nonce is read from
// the GPU evidence itself (bytes [4:36] of the decoded NVIDIA attestation
// report), NOT supplied by the caller.
//
// Returns:
//   - *tee.Error{Reason: ReasonNvidiaVerdictInvalid} on any verification
//     failure (no evidence, bad signature, wrong alg, unknown kid, malformed
//     token, or verdict false).
//   - *failure.NetworkError on transport failure.
//   - *failure.APIError when NRAS or the JWKS endpoint returns a non-2xx status.
func VerifyGPUEvidence(ctx context.Context, hc *http.Client, nrasURL, jwksURL string, gpu []GPUEvidence) error {
	if len(gpu) == 0 {
		return &Error{Reason: ReasonNvidiaVerdictInvalid, Err: errors.New("no gpu evidence")}
	}

	// Build the NRAS request body. The per-GPU arch is hoisted to a single
	// top-level field (the SDK asserts every item shares the first item's arch).
	type evItem struct {
		Evidence    string `json:"evidence"`
		Certificate string `json:"certificate"`
	}
	// The GPU evidence is bound to the nonce the SERVER embedded in the SPDM
	// MEASUREMENTS response, NOT the TDX-quote nonce. NRAS rejects the request
	// (NONCE_NOT_MATCHING) unless we echo that embedded nonce. It lives at bytes
	// [4:36] of the decoded NVIDIA attestation report (SPDM 1.1 response: 4-byte
	// header, then the 32-byte requester nonce).
	gpuNonce, err := gpuEvidenceNonce(gpu[0].Evidence)
	if err != nil {
		return err
	}
	body := struct {
		Nonce         string   `json:"nonce"`
		EvidenceList  []evItem `json:"evidence_list"`
		Arch          string   `json:"arch"`
		ClaimsVersion string   `json:"claims_version"`
	}{
		Nonce:         gpuNonce,
		Arch:          gpu[0].Arch,
		ClaimsVersion: "2.0",
	}
	for _, g := range gpu {
		body.EvidenceList = append(body.EvidenceList, evItem{Evidence: g.Evidence, Certificate: g.Certificate})
	}
	reqJSON, err := json.Marshal(body)
	if err != nil {
		return &Error{Reason: ReasonNvidiaVerdictInvalid, Err: err}
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, nrasURL, bytes.NewReader(reqJSON))
	if err != nil {
		return &failure.NetworkError{Err: err}
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := hc.Do(httpReq)
	if err != nil {
		return &failure.NetworkError{Err: err}
	}
	defer httpResp.Body.Close()
	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return &failure.NetworkError{Err: err}
	}
	if httpResp.StatusCode/100 != 2 {
		return apiError(httpResp.StatusCode, respBody)
	}

	// Extract the compact EAT JWT from the NRAS 200 body. The body is either a
	// bare JSON-string JWT or a nested EAT array [[ "JWT", "<jwt>" ], {...}].
	// Sniff the first non-space byte: '"' => bare string; '[' => nested array.
	token, err := extractNRASToken(respBody)
	if err != nil {
		return err
	}

	claims, err := verifyEAT(ctx, hc, jwksURL, token)
	if err != nil {
		return err
	}

	result, ok := claims["x-nvidia-overall-att-result"].(bool)
	if !ok {
		return &Error{Reason: ReasonNvidiaVerdictInvalid, Err: errors.New("x-nvidia-overall-att-result claim missing or not a bool")}
	}
	if !result {
		return &Error{Reason: ReasonNvidiaVerdictInvalid, Err: errors.New("x-nvidia-overall-att-result is false")}
	}
	return nil
}

// extractNRASToken pulls the compact EAT JWT out of an NRAS 200 body, tolerant
// of the two candidate wire shapes: a bare JSON string, or a nested EAT array
// [[ "JWT", "<jwt>" ], {...detached claims...}] whose overall token's second
// element is the compact JWT. Any shape mismatch fails closed with
// ReasonNvidiaVerdictInvalid rather than panicking.
func extractNRASToken(respBody []byte) (string, error) {
	bad := func(err error) error { return &Error{Reason: ReasonNvidiaVerdictInvalid, Err: err} }

	trimmed := bytes.TrimLeft(respBody, " \t\r\n")
	if len(trimmed) == 0 {
		return "", bad(errors.New("empty NRAS response body"))
	}
	switch trimmed[0] {
	case '"':
		var token string
		if err := json.Unmarshal(trimmed, &token); err != nil {
			return "", bad(fmt.Errorf("decode NRAS token (bare string): %w", err))
		}
		return token, nil
	case '[':
		// Decode only as far as needed: the outer array's first element is the
		// overall token [type, jwt]; the compact JWT is its second element.
		var eat []json.RawMessage
		if err := json.Unmarshal(trimmed, &eat); err != nil {
			return "", bad(fmt.Errorf("decode NRAS EAT array: %w", err))
		}
		if len(eat) == 0 {
			return "", bad(errors.New("NRAS EAT array is empty"))
		}
		var overall []json.RawMessage
		if err := json.Unmarshal(eat[0], &overall); err != nil {
			return "", bad(fmt.Errorf("decode NRAS overall token: %w", err))
		}
		if len(overall) < 2 {
			return "", bad(errors.New("NRAS overall token has no JWT element"))
		}
		var token string
		if err := json.Unmarshal(overall[1], &token); err != nil {
			return "", bad(fmt.Errorf("decode NRAS overall JWT: %w", err))
		}
		return token, nil
	default:
		return "", bad(fmt.Errorf("unexpected NRAS response leading byte %q", trimmed[0]))
	}
}

// verifyEAT verifies an ES384-signed EAT JWT against the NVIDIA JWKS and
// returns the decoded payload claims. The alg is pinned to ES384 (no
// alg-confusion: a token claiming "none"/"HS256" is rejected before any
// signature work). The signature is the raw 96-byte R||S concatenation, not
// ASN.1 DER. Verification failures return *tee.Error{ReasonNvidiaVerdictInvalid};
// JWKS transport failures return *failure.NetworkError / *failure.APIError.
func verifyEAT(ctx context.Context, hc *http.Client, jwksURL, token string) (map[string]any, error) {
	bad := func(err error) error { return &Error{Reason: ReasonNvidiaVerdictInvalid, Err: err} }

	parts := bytes.Split([]byte(token), []byte("."))
	if len(parts) != 3 {
		return nil, bad(errors.New("token is not header.payload.signature"))
	}
	headerB64, payloadB64, sigB64 := string(parts[0]), string(parts[1]), string(parts[2])

	headerJSON, err := base64.RawURLEncoding.DecodeString(headerB64)
	if err != nil {
		return nil, bad(fmt.Errorf("decode header: %w", err))
	}
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(headerJSON, &hdr); err != nil {
		return nil, bad(fmt.Errorf("parse header: %w", err))
	}
	// Pin the algorithm. Reject before fetching keys or verifying anything,
	// closing the classic alg-confusion ("none"/HMAC) hole.
	if hdr.Alg != "ES384" {
		return nil, bad(fmt.Errorf("unexpected alg %q, want ES384", hdr.Alg))
	}

	pub, err := nvidiaSigningKey(ctx, hc, jwksURL, hdr.Kid)
	if err != nil {
		return nil, err
	}

	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		return nil, bad(fmt.Errorf("decode signature: %w", err))
	}
	// ES384 raw signature is two 48-byte big-endian integers R||S, not ASN.1.
	if len(sig) != 96 {
		return nil, bad(fmt.Errorf("signature is %d bytes, want 96 (raw R||S)", len(sig)))
	}
	r := new(big.Int).SetBytes(sig[:48])
	s := new(big.Int).SetBytes(sig[48:])

	signingInput := headerB64 + "." + payloadB64
	digest := sha512.Sum384([]byte(signingInput))
	if !ecdsa.Verify(pub, digest[:], r, s) {
		return nil, bad(errors.New("ES384 signature verification failed"))
	}

	payloadJSON, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return nil, bad(fmt.Errorf("decode payload: %w", err))
	}
	var claims map[string]any
	if err := json.Unmarshal(payloadJSON, &claims); err != nil {
		return nil, bad(fmt.Errorf("parse payload: %w", err))
	}
	return claims, nil
}

// nvidiaSigningKey fetches the NVIDIA JWKS, finds the entry whose kid matches,
// parses its x5c[0] (base64 DER cert), and returns the cert's ECDSA P-384
// public key. A missing kid or a non-ECDSA key is a verdict failure.
func nvidiaSigningKey(ctx context.Context, hc *http.Client, jwksURL, kid string) (*ecdsa.PublicKey, error) {
	bad := func(err error) error { return &Error{Reason: ReasonNvidiaVerdictInvalid, Err: err} }

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, jwksURL, nil)
	if err != nil {
		return nil, &failure.NetworkError{Err: err}
	}
	httpResp, err := hc.Do(httpReq)
	if err != nil {
		return nil, &failure.NetworkError{Err: err}
	}
	defer httpResp.Body.Close()
	jwksBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, &failure.NetworkError{Err: err}
	}
	if httpResp.StatusCode/100 != 2 {
		return nil, apiError(httpResp.StatusCode, jwksBody)
	}

	var jwks struct {
		Keys []struct {
			Kid string   `json:"kid"`
			X5c []string `json:"x5c"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(jwksBody, &jwks); err != nil {
		return nil, bad(fmt.Errorf("parse JWKS: %w", err))
	}

	for _, k := range jwks.Keys {
		if k.Kid != kid {
			continue
		}
		if len(k.X5c) == 0 {
			return nil, bad(fmt.Errorf("JWKS entry %q has no x5c", kid))
		}
		der, err := base64.StdEncoding.DecodeString(k.X5c[0])
		if err != nil {
			return nil, bad(fmt.Errorf("decode x5c[0]: %w", err))
		}
		// We trust this leaf cert because it is served over HTTPS from the
		// pinned NVIDIA JWKS URL; full cert-chain-to-NVIDIA-root validation is
		// intentionally deferred (consistent with the "signature + binding
		// only" scope on VerifyTDXQuote).
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, bad(fmt.Errorf("parse x5c[0] cert: %w", err))
		}
		pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
		if !ok {
			return nil, bad(fmt.Errorf("JWKS entry %q key is not ECDSA", kid))
		}
		// Defense-in-depth: the EAT is signed ES384, so the key must be on
		// P-384. Reject a mismatched-curve JWKS entry before any verify.
		if pub.Curve != elliptic.P384() {
			return nil, bad(fmt.Errorf("JWKS entry %q key is not on P-384", kid))
		}
		return pub, nil
	}
	return nil, bad(fmt.Errorf("no JWKS entry for kid %q", kid))
}

// apiError builds a *failure.APIError from a non-2xx response, best-effort
// extracting a "detail" message from the FastAPI/NRAS error envelope.
func apiError(status int, body []byte) error {
	return failure.APIErrorFromResponse(status, body, nil, 0)
}
