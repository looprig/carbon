package chutes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/looprig/llm/tee"
)

// bindingHash computes the report_data key-binding hash for chutes_version >=
// 0.6.0: the 64-hex attestation nonce string and the base64 e2e_pubkey string
// are concatenated (string concat, not raw bytes), UTF-8 encoded, SHA256'd,
// and hex-encoded lowercase. The result is compared to report_data[:32]
// (the first 64 hex chars). See WIRE.md section 6.
func bindingHash(nonceHex, pubKeyB64 string) string {
	sum := sha256.Sum256([]byte(nonceHex + pubKeyB64))
	return hex.EncodeToString(sum[:])
}

// chutesReason maps the shared tee.Reason vocabulary onto chutes's existing
// AttestReason constants. Both name the same set of failure modes and use the
// same underlying string values, so the mapping is a type cast.
func chutesReason(r tee.Reason) AttestReason {
	return AttestReason(r)
}

// wrapTeeErr lifts a *tee.Error returned by llm/tee into a chutes
// *llm.AttestationError so chutes callers can keep their existing errors.As
// branches. Any non-tee.Error (e.g. *failure.NetworkError, *failure.APIError)
// is returned unchanged.
func wrapTeeErr(err error) error {
	var te *tee.Error
	if errors.As(err, &te) {
		return attestErr(chutesReason(te.Reason), te.Err)
	}
	return err
}

// verifyTDXQuote delegates the signature + chain check to llm/tee, then runs
// the chutes-specific report_data binding (sha256(nonceHex + pubKeyB64)).
// The binding stays here because it is chutes-specific: phala uses a different
// report_data layout. Returns *llm.AttestationError on any failure (Reason set),
// nil on success.
//
// Network dependency: tee.VerifyTDXQuote configures go-tdx-guest with
// GetCollateral=false and CheckRevocations=false, so this performs no network
// I/O. The trade-off is that TCB level / QE identity / CRL revocation are not
// checked; we verify the ECDSA signature and that the chain roots to the
// trusted Intel root.
func verifyTDXQuote(rawQuote []byte, nonceHex, pubKeyB64 string) error {
	rd, err := tee.VerifyTDXQuote(rawQuote)
	if err != nil {
		return wrapTeeErr(err)
	}
	// report_data[:32] (64 hex chars, lowercase) is the SHA256 of
	// (nonceHex + pubKeyB64). report_data[32:64] holds a TLS cert-public-key
	// hash that the server checks against its own mTLS leaf; we intentionally
	// ignore it in v1 because this client does not terminate that mTLS
	// connection, so we have nothing to compare it against.
	gotBinding := hex.EncodeToString(rd[:32])
	wantBinding := bindingHash(nonceHex, pubKeyB64)
	if gotBinding != wantBinding {
		return attestErr(ReasonBindingMismatch,
			errors.New("report_data binding does not match nonce + e2e_pubkey"))
	}
	return nil
}

// parseGPUEvidence extracts the gpu_evidence list from a raw TeeInstanceEvidence
// JSON document (the body of GET .../evidence). The shape of that envelope is
// chutes-specific (WIRE.md section 6); the per-entry struct is the shared
// tee.GPUEvidence so it feeds straight into tee.VerifyGPUEvidence.
func parseGPUEvidence(evidenceJSON []byte) ([]tee.GPUEvidence, error) {
	var doc struct {
		GPUEvidence []tee.GPUEvidence `json:"gpu_evidence"`
	}
	if err := json.Unmarshal(evidenceJSON, &doc); err != nil {
		return nil, err
	}
	return doc.GPUEvidence, nil
}

// verifyNvidiaEvidence delegates NRAS verification to llm/tee and wraps any
// *tee.Error into a chutes *llm.AttestationError so callers keep their existing
// errors.As branches. Transport / API errors pass through unchanged.
func verifyNvidiaEvidence(ctx context.Context, hc *http.Client, nrasURL, jwksURL string, gpu []tee.GPUEvidence) error {
	return wrapTeeErr(tee.VerifyGPUEvidence(ctx, hc, nrasURL, jwksURL, gpu))
}
