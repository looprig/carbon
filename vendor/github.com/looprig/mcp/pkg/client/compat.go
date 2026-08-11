// This file defines compatibility profiles: the named, versioned policy for how
// far a binding will bend for a server that does not implement the
// specification perfectly.
//
// # Why a closed enum, not a set of booleans
//
// The design divides deviations into safe and unsafe, and the unsafe list is not
// a warning — it is a list of things that must be impossible:
//
//   - replacing an invalid input schema with unconstrained arguments;
//   - disabling TLS verification;
//   - treating malformed framing as a valid message;
//   - retrying non-idempotent tool calls;
//   - treating an auth failure as anonymous success.
//
// None of them can be requested here, because none of them can be *named* here.
// Tolerance is a closed enum of exactly the three safe deviations, so a
// configuration — however misguided, and whatever an operator pastes into it —
// has no way to express "widen the arguments". A profile is a choice among safe
// options, not a dial that goes to unsafe. compat_test.go asserts the enum's
// declared range is exactly those three, so adding an unsafe member is a test
// failure rather than a review question.
//
// # Why profiles are versioned
//
// A profile is part of the binding's configuration identity (see Digest). What
// "default" tolerates may change as this module learns about more servers, and a
// manifest recording that a session ran under "default" would then record
// nothing at all. The version is what makes the record mean something a year
// later.

package client

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"

	"github.com/looprig/mcp/internal/canonical"
	"github.com/looprig/mcp/internal/catalog"
)

// Tolerance is one safe deviation a compatibility profile may permit. The zero
// value is not a valid tolerance.
//
// Permitting a tolerance does not apply it: a server that implements the
// specification faithfully needs none of these, and a binding reports only what
// it actually had to bend (see Catalog.AppliedTolerances).
type Tolerance uint8

// The tolerances a profile may permit. This list is exhaustive and every member
// is safe by construction — see this file's header.
const (
	// TolerateInvalidOutputSchema drops a defective *optional* output schema and
	// keeps its tool, with a warning.
	//
	// It is safe because an output schema only describes what comes back: no
	// authority rests on it, and dropping it is what the bound existed for. The
	// input schema is never treated this way — a tool whose input schema is
	// missing or malformed is rejected, because keeping it would mean letting a
	// model send unconstrained arguments, which is the design's first named
	// unsafe tolerance.
	TolerateInvalidOutputSchema Tolerance = iota + 1

	// TolerateLegacySSE accepts the legacy SSE transport.
	//
	// It is safe because it is opt-in and narrow: it admits an older wire
	// protocol for interoperability, and weakens no validation, auth, limit or
	// cancellation rule. Without it a binding configured with an SSE transport
	// is refused at Validate — the design's "accepting a legacy SSE transport
	// only when explicitly configured", enforced rather than documented.
	TolerateLegacySSE

	// TolerateDisplayNameNormalization rewrites a raw name that inference
	// providers will not accept into one they will.
	//
	// It is safe because it is display-only and lossless where it counts: the
	// raw name is preserved and remains the only thing that ever goes on the
	// wire, and the mapping back is a lookup table rather than a reparse. A
	// binding without it refuses a tool it cannot show a model under its own
	// name, rather than showing it under a name that might mean something else.
	TolerateDisplayNameNormalization

	toleranceSentinel // must remain last; tests derive the declared range from it
)

// toleranceNames maps each tolerance to its stable lowercase identifier. They
// reach diagnostics and configuration identity, so they must not change.
var toleranceNames = [toleranceSentinel]string{
	TolerateInvalidOutputSchema:      "invalid_output_schema",
	TolerateLegacySSE:                "legacy_sse",
	TolerateDisplayNameNormalization: "display_name_normalization",
}

// String returns the tolerance's stable identifier, or "unknown" for any value
// outside the declared range.
func (t Tolerance) String() string {
	if !t.valid() {
		return "unknown"
	}
	return toleranceNames[t]
}

// valid reports whether t is a declared tolerance.
func (t Tolerance) valid() bool { return t >= TolerateInvalidOutputSchema && t < toleranceSentinel }

// MaxProfileNameBytes bounds a profile name. A profile name is an identifier
// that reaches status, diagnostics and configuration identity; it is not a
// description.
const MaxProfileNameBytes = 64

// Profile is a named, versioned compatibility policy: the deviations a binding
// is willing to tolerate in a server.
//
// It is secret-free by construction — a name, a version, and a set of enum
// values — and is part of the binding's configuration identity (see Digest). The
// zero Profile means "unset" and Definition.normalized replaces it with
// ProfileDefault; a Profile with a name and no tolerances is a real, strict
// policy and is left alone.
type Profile struct {
	// Name identifies the profile. It is a stable identifier, not prose.
	Name string
	// Version distinguishes revisions of a profile under one name. Bump it
	// whenever the tolerances change, so that a record of "default v1" keeps
	// meaning what it meant.
	Version int
	// Tolerances are the deviations this profile permits. Order and duplicates
	// are not significant; Digest canonicalizes both.
	Tolerances []Tolerance
}

// The profiles this module ships. An application may also declare its own.
var (
	// ProfileStrict tolerates nothing: every deviation from the specification
	// makes the offending catalog invalid.
	//
	// It is the right choice against a server you control, where a defect is a
	// bug worth failing over rather than a fact of life worth working around.
	ProfileStrict = Profile{Name: "strict", Version: 1}

	// ProfileDefault tolerates the deviations that are common in servers in the
	// wild and safe to absorb: a defective optional output schema, and a raw
	// name no inference provider would accept.
	//
	// It is what a Definition gets when it names no profile. Legacy SSE is not
	// in it: a transport is a deliberate choice, and a binding should not
	// acquire an older wire protocol by default.
	ProfileDefault = Profile{
		Name:    "default",
		Version: 1,
		Tolerances: []Tolerance{
			TolerateInvalidOutputSchema,
			TolerateDisplayNameNormalization,
		},
	}

	// ProfileLegacy is ProfileDefault plus the legacy SSE transport, for
	// interoperating with servers that predate Streamable HTTP.
	ProfileLegacy = Profile{
		Name:    "legacy",
		Version: 1,
		Tolerances: []Tolerance{
			TolerateInvalidOutputSchema,
			TolerateDisplayNameNormalization,
			TolerateLegacySSE,
		},
	}
)

// Permits reports whether the profile allows tolerance t.
func (p Profile) Permits(t Tolerance) bool { return slices.Contains(p.Tolerances, t) }

// String renders the profile's identity as "name/version", which is what Status
// and diagnostics carry.
func (p Profile) String() string { return fmt.Sprintf("%s/v%d", p.Name, p.Version) }

// isZero reports whether the profile is unset.
func (p Profile) isZero() bool { return p.Name == "" && p.Version == 0 && len(p.Tolerances) == 0 }

// validate reports the first violation. It fails closed: an undeclared tolerance
// is refused rather than ignored, because a configuration that asks for
// something this build does not know about must not silently get a weaker
// policy than it asked for — nor a stronger one it did not plan for.
func (p Profile) validate() error {
	switch {
	case p.Name == "":
		return fmt.Errorf("Compat.Name: a profile must be named, so that what a binding tolerated is recordable")
	case len(p.Name) > MaxProfileNameBytes:
		return fmt.Errorf("Compat.Name: %d bytes, max %d", len(p.Name), MaxProfileNameBytes)
	case p.Version < 1:
		return fmt.Errorf("Compat.Version: %d: a profile must be versioned from 1", p.Version)
	}
	for i, t := range p.Tolerances {
		if !t.valid() {
			return fmt.Errorf("Compat.Tolerances[%d]: %d is not a tolerance this client can apply", i, t)
		}
	}
	return nil
}

// clone returns a deep copy, detaching the tolerance slice from a caller's
// backing array.
func (p Profile) clone() Profile {
	p.Tolerances = slices.Clone(p.Tolerances)
	return p
}

// tolerances projects the profile onto the narrow policy internal/catalog
// enforces.
//
// The mapping is explicit and partial, and both properties are deliberate.
// TolerateLegacySSE has no catalog meaning — it is a transport decision, made at
// Validate — so it does not appear, rather than being passed down as a flag the
// catalog would have to ignore.
func (p Profile) tolerances() catalog.Tolerances {
	return catalog.Tolerances{
		InvalidOutputSchema:   p.Permits(TolerateInvalidOutputSchema),
		NormalizeDisplayNames: p.Permits(TolerateDisplayNameNormalization),
	}
}

// fromCatalogTolerance maps an internal tolerance onto the public one.
//
// It is an explicit mapping rather than a numeric conversion: the two enums are
// declared in different packages for different audiences — the catalog's covers
// what a generation can bend, the public one also covers a transport decision —
// and a cast would silently mistranslate the day either gains a member. An
// internal tolerance with no public counterpart is dropped rather than guessed
// at, so the public vocabulary stays exactly the documented one.
func fromCatalogTolerance(t catalog.Tolerance) (Tolerance, bool) {
	switch t {
	case catalog.ToleranceInvalidOutputSchema:
		return TolerateInvalidOutputSchema, true
	case catalog.ToleranceNormalizedDisplayName:
		return TolerateDisplayNameNormalization, true
	default:
		return 0, false
	}
}

// The digest's domain and encoding version. The domain separates a profile
// digest from every other digest this module computes; the version guards the
// encoding, and must be bumped whenever a change here would give an existing
// profile a new digest.
const (
	profileDigestDomain  = "looprig.mcp.compat.profile"
	profileDigestVersion = 1
)

// Digest returns the profile's hex identity digest: the value a configuration
// manifest records so that "this session ran under this compatibility policy" is
// checkable rather than a claim.
//
// Nothing in this module consumes it yet. It is the compatibility half of the
// design's manifest identity — which lists "compatibility policy digests"
// separately from the "adopted catalog digest" — and the manifest work is a
// later stage, so this is the seam waiting for it rather than something already
// wired. What a binding reports about its profile today is Status.CompatProfile
// ("name/vN"), which is a label for a human; this is the checkable form. See
// catalog.Generation.AppliedTolerances for why the two identities are separate
// and why neither belongs inside the other.
//
// It is canonical. The tolerances are sorted and deduplicated first, so two
// profiles that permit the same things digest the same however they were
// written; and every value is length-delimited, so no two different profiles can
// encode to the same bytes. The framing is internal/canonical's, shared with the
// catalog and binding-identity digests: one encoder, so the three cannot drift
// apart into disagreeing about what a canonical encoding is.
//
// It covers the whole profile, name and version included. Two profiles that
// permit the same deviations under different names are different policies: the
// name is what an operator reasons about and what a later revision is compared
// against, so a digest that ignored it would report no drift when "strict" was
// quietly swapped for "default" with the same contents.
func (p Profile) Digest() string {
	h := sha256.New()
	e := canonical.NewEncoder(h)

	e.Str(profileDigestDomain)
	e.Uint(profileDigestVersion)
	e.Str(p.Name)
	// A version is signed in the struct (it is an ordinary count) but negative
	// values never reach here: validate refuses them. The conversion is still
	// done on the absolute value's own terms rather than by casting a possibly
	// negative int, so an unvalidated Profile digests to something stable rather
	// than to a wrapped enormity.
	if p.Version > 0 {
		e.Uint(uint64(p.Version))
	} else {
		e.Uint(0)
	}

	sorted := slices.Clone(p.Tolerances)
	slices.Sort(sorted)
	sorted = slices.Compact(sorted)
	e.Count(len(sorted))
	for _, t := range sorted {
		// The stable identifier, not the numeric value: an enum's numbers are an
		// implementation detail that a later insertion could renumber, and a
		// digest must not change because a constant moved.
		e.Str(t.String())
	}

	return hex.EncodeToString(h.Sum(nil))
}

// sseTransportKind is the transport kind gated by TolerateLegacySSE. It matches
// the Kind() a legacy SSE transport reports.
const sseTransportKind = "sse"

// checkTransportCompat refuses a transport this binding's profile does not
// permit.
//
// Today that is exactly the legacy SSE transport, and the check lives at
// Validate rather than at Connect on purpose: "only when explicitly configured"
// is a property of the configuration, so it is decided when the configuration is
// checked, before anything is launched or contacted.
func (d Definition) checkTransportCompat() error {
	if d.Transport.Kind() != sseTransportKind {
		return nil
	}
	if d.Compat.Permits(TolerateLegacySSE) {
		return nil
	}
	return fmt.Errorf("the legacy SSE transport (Transport.Kind %q) is compatibility-only and this binding's compatibility profile (%s) does not permit it: use ProfileLegacy, or add TolerateLegacySSE to a profile of your own", sseTransportKind, d.Compat)
}
