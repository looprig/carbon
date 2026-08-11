package gate

import (
	"fmt"
	"net/url"
)

// GateValidationErrorKind classifies a rejected gate envelope.
type GateValidationErrorKind string

const (
	// GateRestorableNotAllowed reports a gate marked Restorable whose kind can
	// never be restored.
	GateRestorableNotAllowed GateValidationErrorKind = "restorable_not_allowed"
	// GateOriginInvalid reports a gate whose Prompt.Origin is missing or is not
	// a bare origin, on a kind that requires one.
	GateOriginInvalid GateValidationErrorKind = "origin_invalid"
)

// GateValidationError reports a gate envelope that violates a kind's invariants.
type GateValidationError struct {
	Kind     GateValidationErrorKind
	GateKind Kind
	Cause    error
}

func (e *GateValidationError) Error() string {
	msg := fmt.Sprintf("gate: invalid %q gate (%s)", string(e.GateKind), string(e.Kind))
	if e.Cause != nil {
		return msg + ": " + e.Cause.Error()
	}
	return msg
}

func (e *GateValidationError) Unwrap() error { return e.Cause }

// ValidateGate checks the envelope invariants that a gate's Kind implies. It is
// called on the open path so an invariant cannot be violated by a caller who
// simply forgot it.
//
// It is deliberately narrow. Every kind that predates it validates as nil — this
// is an additive hook, not a retroactive schema check, and it must stay that way
// unless an existing kind's contract is separately tightened.
//
// Today it enforces two rules, both on open-url gates:
//
//   - It may not be Restorable. OpenURLPayload's action target is never
//     journaled, so a "restored" open-url gate would present a human with an
//     origin and no URL to open — it would fail open into a broken prompt.
//     Restore must instead close it as unavailable and let a live integration
//     mint a fresh request.
//   - Prompt.Origin must be a bare origin. The envelope is the only thing a
//     renderer sees, and the origin is what the human's trust decision is made
//     on. Validating it HERE — with the same validateDisplayOrigin the durable
//     payload uses, not a second opinion — is what makes "the envelope's origin
//     is a real origin" a contract a renderer can rely on structurally, rather
//     than a convention like an opener stuffing text into Prompt.Body. An
//     open-url gate with no origin is a prompt that asks a human to authorize
//     an unnamed party, so it is refused rather than rendered.
func ValidateGate(g Gate) error {
	switch g.Kind {
	case KindOpenURL:
		if g.Restorable {
			return &GateValidationError{Kind: GateRestorableNotAllowed, GateKind: g.Kind}
		}
		if err := validateDisplayOrigin(g.Prompt.Origin); err != nil {
			return &GateValidationError{Kind: GateOriginInvalid, GateKind: g.Kind, Cause: err}
		}
		return nil
	default:
		return nil
	}
}

// OpenURLPayloadErrorKind classifies a rejected open-url payload.
type OpenURLPayloadErrorKind string

const (
	// OpenURLTargetMissing reports an open-url payload with no action URL.
	OpenURLTargetMissing OpenURLPayloadErrorKind = "target_missing"
)

// OpenURLPayloadError reports an open-url payload that cannot be acted on.
type OpenURLPayloadError struct {
	Kind OpenURLPayloadErrorKind
}

func (e *OpenURLPayloadError) Error() string {
	return fmt.Sprintf("gate: invalid open-url payload (%s)", string(e.Kind))
}

// ValidateOpenURLPayload reports whether p is a live, well-formed open-url
// request. It is the open-url counterpart of ValidateFormSchema: the check an
// opener runs BEFORE a gate exists, so a broken request is refused instead of
// shown to a human.
//
// Two rules, and both are load-bearing:
//
//   - DisplayOrigin must be a bare, journal-safe origin. This is the same check
//     the codec applies, hoisted to the open path because the codec is not
//     always on it — a session with no journal (the nop appender) would
//     otherwise render whatever an integration passed straight to a human, which
//     is precisely the trust decision the origin exists to inform.
//   - URL must be present. A decoded OpenURLPayload always has an empty URL (the
//     action target is never journaled), so an empty URL here means the request
//     is a restored or half-built one with nothing to open. Refusing it is the
//     same fail-closed rule ValidateGate applies to a Restorable open-url gate:
//     an origin with no target is a broken prompt, not a degraded one.
func ValidateOpenURLPayload(p OpenURLPayload) error {
	if err := validateDisplayOrigin(p.DisplayOrigin); err != nil {
		return err
	}
	if p.URL == "" {
		return &OpenURLPayloadError{Kind: OpenURLTargetMissing}
	}
	return nil
}

// originErrorKind classifies a rejected DisplayOrigin.
type originErrorKind string

const (
	originEmpty     originErrorKind = "origin_empty"
	originMalformed originErrorKind = "origin_malformed"
	originScheme    originErrorKind = "origin_scheme_unsupported"
	originNotBare   originErrorKind = "origin_not_bare"
)

// DisplayOriginError reports a DisplayOrigin that is not a bare, journal-safe
// origin.
type DisplayOriginError struct {
	kind originErrorKind
}

func (e *DisplayOriginError) Error() string {
	return fmt.Sprintf("gate: invalid display origin (%s)", string(e.kind))
}

// validateDisplayOrigin enforces that an OpenURLPayload's DisplayOrigin is a
// BARE origin: an http/https scheme, a host, and nothing else.
//
// This check is what makes the URL exclusion real rather than nominal. Dropping
// the URL field from the durable record achieves nothing if a caller can pass
// "https://idp.example/authorize?state=SECRET&code_challenge=..." as the
// "origin" and have it journaled verbatim. Rejecting any path, query, fragment,
// or userinfo means the durable record can only ever carry the coarse
// destination a human needs to make a trust decision.
func validateDisplayOrigin(origin string) error {
	if origin == "" {
		return &DisplayOriginError{kind: originEmpty}
	}
	parsed, err := url.Parse(origin)
	if err != nil {
		return &DisplayOriginError{kind: originMalformed}
	}
	switch parsed.Scheme {
	case "http", "https":
	default:
		return &DisplayOriginError{kind: originScheme}
	}
	if parsed.Host == "" {
		return &DisplayOriginError{kind: originMalformed}
	}
	if parsed.User != nil {
		return &DisplayOriginError{kind: originNotBare}
	}
	// A bare "/" carries no information and is a conventional spelling of an
	// origin, so it is accepted; any deeper path is not.
	if parsed.Path != "" && parsed.Path != "/" {
		return &DisplayOriginError{kind: originNotBare}
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.Opaque != "" {
		return &DisplayOriginError{kind: originNotBare}
	}
	return nil
}
