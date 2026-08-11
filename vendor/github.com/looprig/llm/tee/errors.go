// Package tee — shared TEE attestation primitives. Each provider package
// (llm/chutes, llm/phala) handles its own provider-specific report_data
// binding; this package handles the parts that are common: Intel TDX quote
// signature + chain verification against the embedded Intel SGX Root CA, and
// NVIDIA GPU evidence verification against NRAS.
package tee

import "fmt"

// Reason names the failing TEE attestation check. Provider packages may wrap
// these in their own typed errors, but the shared reason vocabulary lives
// here so both chutes and phala speak the same names for the same failure.
type Reason string

const (
	// ReasonQuoteSignatureInvalid: the TDX quote's ECDSA signature or QE
	// report signature did not verify.
	ReasonQuoteSignatureInvalid Reason = "quote_signature_invalid"
	// ReasonRootCAUntrusted: the PCK certificate chain did not chain to the
	// trusted Intel SGX Root CA (or a chain cert was expired).
	ReasonRootCAUntrusted Reason = "root_ca_untrusted"
	// ReasonEvidenceMalformed: the raw quote bytes could not be parsed into
	// a TDX quote.
	ReasonEvidenceMalformed Reason = "evidence_malformed"
	// ReasonNvidiaVerdictInvalid: NRAS verification failed (Task 2 will use
	// this).
	ReasonNvidiaVerdictInvalid Reason = "nvidia_verdict_invalid"
)

// Error is the typed error returned from every llm/tee verification function.
// Callers inspect Reason via errors.As; Err is the underlying cause.
type Error struct {
	Reason Reason
	Err    error
}

func (e *Error) Error() string { return fmt.Sprintf("tee: %s: %v", e.Reason, e.Err) }
func (e *Error) Unwrap() error { return e.Err }
