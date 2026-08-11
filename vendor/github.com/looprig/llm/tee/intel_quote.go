package tee

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/google/go-tdx-guest/abi"
	pb "github.com/google/go-tdx-guest/proto/tdx"
	"github.com/google/go-tdx-guest/verify"
	"github.com/google/go-tdx-guest/verify/trust"
)

// Options configures TDX quote verification. The zero value is the safe, no
// network default used by VerifyTDXQuote: collateral and revocation checks
// off, the bounded getter (unused when no network is performed), and Now ==
// time.Now.
//
// Open/Closed: new verification knobs are added here without touching the
// parse/validate prologue.
type Options struct {
	// GetCollateral, when true, makes the verifier fetch TCB info / QE
	// identity collateral from Intel PCS over the network using Getter.
	GetCollateral bool
	// CheckRevocations, when true, makes the verifier fetch the CRL and check
	// the PCK certificate chain for revocation over the network using Getter.
	CheckRevocations bool
	// Getter fetches collateral/CRL bytes. If nil, a bounded, context-aware,
	// HTTPS-only getter is used (never the library's unbounded default).
	Getter trust.HTTPSGetter
	// Now produces the time at which certificate / collateral validity is
	// judged. If nil, time.Now is used. It is a func so tests can pin time;
	// it is called once per verification.
	Now func() time.Time
}

// VerifyTDXQuote parses raw TDX quote bytes, verifies the ECDSA signature and
// PCK certificate chain against the embedded Intel SGX Root CA, and returns
// the 64-byte report_data field. Callers do their own report_data binding
// check (the binding is provider-specific).
//
// No network I/O: it is a thin back-compat wrapper over
// VerifyTDXQuoteWithOptions with a zero Options, so GetCollateral and
// CheckRevocations are both false and TCB level / QE identity / CRL revocation
// are NOT checked.
func VerifyTDXQuote(rawQuote []byte) ([]byte, error) {
	return VerifyTDXQuoteWithOptions(rawQuote, Options{})
}

// VerifyTDXQuoteWithOptions is VerifyTDXQuote with injectable, bounded DCAP
// options. It performs the same parse/validate prologue, then threads all four
// option fields into verify.Options. When opts.GetCollateral or
// opts.CheckRevocations is set, the (bounded by default) Getter is used to
// fetch collateral/CRL data; otherwise no network I/O occurs.
func VerifyTDXQuoteWithOptions(rawQuote []byte, opts Options) ([]byte, error) {
	q, err := abi.QuoteToProto(rawQuote)
	if err != nil {
		return nil, &Error{Reason: ReasonEvidenceMalformed, Err: err}
	}
	qv4, ok := q.(*pb.QuoteV4)
	if !ok {
		return nil, &Error{Reason: ReasonEvidenceMalformed, Err: errors.New("unexpected quote type (not QuoteV4)")}
	}
	rd := qv4.GetTdQuoteBody().GetReportData()
	if len(rd) != 64 {
		return nil, &Error{Reason: ReasonEvidenceMalformed, Err: errors.New("report_data is not 64 bytes")}
	}

	now := time.Now()
	if opts.Now != nil {
		now = opts.Now()
	}
	vopts := &verify.Options{
		GetCollateral:    opts.GetCollateral,
		CheckRevocations: opts.CheckRevocations,
		Getter:           resolveGetter(opts),
		Now:              now,
	}
	if err := verify.TdxQuote(q, vopts); err != nil {
		return nil, &Error{Reason: classifyVerifyErr(err), Err: err}
	}
	return rd, nil
}

// rtmrLength is the byte length of each TDX runtime measurement register
// (RTMR). The TDX quote body carries four of them, each a 48-byte SHA-384
// measurement.
const rtmrLength = 48

// rtmr3Index is the position of RTMR3 in the quote body's RTMRs slice. RTMR3 is
// the runtime-extended register the Dstack event log's IMR3 replay must match.
const rtmr3Index = 3

// TDXQuoteRTMR3 parses raw TDX quote bytes and returns a copy of RTMR3 (the 4th
// runtime measurement register, 48 bytes) from the quote body. It performs no
// signature or chain verification and no network I/O — it is a pure parse-and-
// extract used by the Dstack ACI event-log replay to compare the replayed IMR3
// against the value the (separately verified) quote attests.
//
// The returned slice is a fresh copy, so callers may mutate it without
// affecting the parsed quote. Any failure — unparseable bytes, an unexpected
// quote type, or a missing/short RTMR3 — returns a *tee.Error with
// ReasonEvidenceMalformed.
func TDXQuoteRTMR3(rawQuote []byte) ([]byte, error) {
	q, err := abi.QuoteToProto(rawQuote)
	if err != nil {
		return nil, &Error{Reason: ReasonEvidenceMalformed, Err: err}
	}
	qv4, ok := q.(*pb.QuoteV4)
	if !ok {
		return nil, &Error{Reason: ReasonEvidenceMalformed, Err: errors.New("unexpected quote type (not QuoteV4)")}
	}
	rtmrs := qv4.GetTdQuoteBody().GetRtmrs()
	if len(rtmrs) <= rtmr3Index || len(rtmrs[rtmr3Index]) != rtmrLength {
		return nil, &Error{Reason: ReasonEvidenceMalformed, Err: errors.New("RTMR3 missing or not 48 bytes")}
	}
	out := make([]byte, rtmrLength)
	copy(out, rtmrs[rtmr3Index])
	return out, nil
}

// resolveGetter returns the caller-supplied getter, or the bounded default if
// none was supplied. It never returns the library's unbounded
// trust.DefaultHTTPSGetter (which wraps the http.Get-backed SimpleHTTPSGetter
// with no per-request timeout, no TLS floor, and no scheme/body bounds).
func resolveGetter(opts Options) trust.HTTPSGetter {
	if opts.Getter != nil {
		return opts.Getter
	}
	return newBoundedGetter()
}

// classifyVerifyErr maps a go-tdx-guest verification error to a Reason.
// Hash/signature failures map to ReasonQuoteSignatureInvalid; everything else
// from the chain-verification path (untrusted root, expired or invalid certs)
// maps to ReasonRootCAUntrusted.
func classifyVerifyErr(err error) Reason {
	switch {
	case errors.Is(err, verify.ErrHashVerificationFail),
		errors.Is(err, verify.ErrSHA56VerificationFail):
		return ReasonQuoteSignatureInvalid
	default:
		return ReasonRootCAUntrusted
	}
}

// Bounded getter timeouts and limits. These cap how long a single collateral
// fetch may run and how many bytes it may return, so a slow or hostile PCS/CRL
// endpoint cannot hang or exhaust memory during verification.
const (
	// boundedClientTimeout bounds the whole request (connect + TLS + headers +
	// body) for the underlying http.Client.
	boundedClientTimeout = 30 * time.Second
	// boundedRequestTimeout bounds each Get via a context deadline; it is the
	// hard ceiling the verify path observes per collateral fetch.
	boundedRequestTimeout = 30 * time.Second
	// boundedTLSHandshakeTimeout bounds the TLS handshake alone.
	boundedTLSHandshakeTimeout = 10 * time.Second
	// boundedResponseHeaderTimeout bounds time spent waiting for response
	// headers after the request is written.
	boundedResponseHeaderTimeout = 10 * time.Second
	// boundedMaxResponseBytes caps the response body read to bound memory.
	// Intel PCS collateral (TCB info, QE identity, CRL) is well under this.
	boundedMaxResponseBytes = 4 << 20 // 4 MiB
	// boundedTLSMinVersion is the TLS floor for collateral fetches.
	boundedTLSMinVersion = tls.VersionTLS12
)

// getterError is the typed error for bounded-getter failures (non-HTTPS URL,
// transport failure, non-2xx status, or body read failure). Callers can
// errors.As to it to distinguish fetch failures from verification failures.
type getterError struct {
	URL string
	Op  string
	Err error
}

func (e *getterError) Error() string {
	return fmt.Sprintf("tee: bounded getter %s %q: %v", e.Op, e.URL, e.Err)
}

func (e *getterError) Unwrap() error { return e.Err }

// boundedGetter implements trust.HTTPSGetter using an http.Client with explicit
// timeouts, a TLS floor, HTTPS-only enforcement, a per-request context
// deadline, and a bounded body read. It is the safe replacement for the
// library's unbounded SimpleHTTPSGetter / DefaultHTTPSGetter.
type boundedGetter struct {
	client         *http.Client
	requestTimeout time.Duration
	maxBodyBytes   int64
}

// newBoundedGetter constructs a boundedGetter with the package timeouts/limits.
func newBoundedGetter() *boundedGetter {
	return &boundedGetter{
		client: &http.Client{
			Timeout: boundedClientTimeout,
			Transport: &http.Transport{
				TLSClientConfig:       &tls.Config{MinVersion: boundedTLSMinVersion},
				TLSHandshakeTimeout:   boundedTLSHandshakeTimeout,
				ResponseHeaderTimeout: boundedResponseHeaderTimeout,
				ForceAttemptHTTP2:     true,
			},
		},
		requestTimeout: boundedRequestTimeout,
		maxBodyBytes:   boundedMaxResponseBytes,
	}
}

// Get fetches the URL over HTTPS with a bounded context deadline and a bounded
// body read, matching SimpleHTTPSGetter's (header, body, error) contract and
// its status >= 300 semantics. Non-HTTPS URLs are rejected before any I/O.
func (g *boundedGetter) Get(rawURL string) (map[string][]string, []byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, nil, &getterError{URL: rawURL, Op: "parse", Err: err}
	}
	if u.Scheme != "https" {
		return nil, nil, &getterError{
			URL: rawURL,
			Op:  "scheme",
			Err: fmt.Errorf("refusing non-https scheme %q", u.Scheme),
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), g.requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, nil, &getterError{URL: rawURL, Op: "request", Err: err}
	}

	resp, err := g.client.Do(req)
	if err != nil {
		return nil, nil, &getterError{URL: rawURL, Op: "do", Err: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return nil, nil, &getterError{
			URL: rawURL,
			Op:  "status",
			Err: fmt.Errorf("status code received %d", resp.StatusCode),
		}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, g.maxBodyBytes))
	if err != nil {
		return nil, nil, &getterError{URL: rawURL, Op: "read", Err: err}
	}
	return resp.Header, body, nil
}
