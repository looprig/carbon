package chutes

import "github.com/looprig/llm"

// AttestReason names the specific attestation check that failed. It lets
// callers branch on the failure mode via errors.As without parsing strings.
type AttestReason string

const (
	// ReasonQuoteSignatureInvalid: the TDX quote's ECDSA signature or QE
	// report signature did not verify.
	ReasonQuoteSignatureInvalid AttestReason = "quote_signature_invalid"
	// ReasonRootCAUntrusted: the PCK certificate chain did not chain to the
	// trusted Intel SGX Root CA (or a chain cert was expired).
	ReasonRootCAUntrusted AttestReason = "root_ca_untrusted"
	// ReasonBindingMismatch: report_data[:32] did not match
	// sha256(nonceHex + pubKeyB64).
	ReasonBindingMismatch AttestReason = "binding_mismatch"
	// ReasonEvidenceMalformed: the raw quote bytes could not be parsed into a
	// TDX quote.
	ReasonEvidenceMalformed AttestReason = "evidence_malformed"
	// ReasonNvidiaVerdictInvalid: NRAS verification failed — the returned EAT
	// JWT did not verify (bad/absent signature, wrong alg, unknown kid,
	// malformed token) or the x-nvidia-overall-att-result claim was not true.
	ReasonNvidiaVerdictInvalid AttestReason = "nvidia_verdict_invalid"
)

// attestErr creates an *llm.AttestationError with a chutes-specific reason.
func attestErr(reason AttestReason, err error) error {
	return &llm.AttestationError{Reason: string(reason), Err: err}
}
