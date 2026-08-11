// This file answers one question for a composing application: is the MCP
// configuration this Session is about to resume under the same configuration it
// ran under before?
//
// It answers it with identity, not with configuration. A BindingIdentity is a
// secret-free description of a binding — what it is mounted as, who may see it,
// where it points, what it will bend for, and what the server turned out to
// offer — reduced to values and digests that are safe to persist in a journal,
// render in a UI, and ship to telemetry. The design's manifest list (§Session
// restore and configuration identity) is its field list, and the exclusions are
// the same design's: credentials, tokens, raw headers, full environment values,
// resource contents, prompt bodies, and tool results are not here.
//
// The exclusions hold structurally rather than by care. Nothing in this file
// can reach a secret: a Definition is secret-free by construction (credentials
// arrive via providers at connect time, held in closures), a transport surfaces
// only Kind and RedactedOrigin, and a catalog carries schema digests rather
// than schema bytes. A test puts a canary in every secret-bearing input the
// module has and proves it reaches neither an identity value nor the digest.
//
// # What this is not
//
// It is not a configuration manifest, an epoch, or a typed drift report. Those
// belong to 2026-07-16-session-versioning-migration-design.md, which is
// deferred. Harness today has one-shot event.ConfigFingerprint stamped at
// SessionStarted and a boolean restore override, so what this module
// contributes is a digest that fits that model: one string, compared for
// equality, empty when there is no MCP configuration at all. See ConfigDigest.

package mcpharness

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"slices"

	"github.com/looprig/core/uuid"
	"github.com/looprig/mcp/internal/canonical"
	"github.com/looprig/mcp/pkg/client"
)

// The identity digests' domains and encoding version.
//
// Each domain names one kind of thing that gets digested, so that two different
// kinds can never produce the same digest from the same bytes. The version
// guards the encoding: bump it whenever a change here would give an existing
// configuration a new digest, so a digest computed by an old build is never
// mistaken for one computed by a new one.
const (
	domainBinding      = "looprig.mcp.harness.binding"
	domainConfig       = "looprig.mcp.harness.config"
	domainSelector     = "looprig.mcp.harness.selector"
	domainCapabilities = "looprig.mcp.harness.capabilities"
	domainFilter       = "looprig.mcp.harness.filter"
	domainLimits       = "looprig.mcp.harness.limits"

	identitySchemaVersion = 1
)

// Field tags. Each is written before its value, so that reordering two
// same-typed fields changes the encoding rather than silently re-interpreting a
// neighbour's bytes. The strings are part of the digests' definition: change one
// and every digest changes, so bump identitySchemaVersion with it.
const (
	tagName            = "name"
	tagScope           = "scope"
	tagLoop            = "loop"
	tagSelector        = "selector"
	tagTransport       = "transport"
	tagRequired        = "required"
	tagCapabilities    = "capabilities"
	tagFilter          = "filter"
	tagLimits          = "limits"
	tagCompat          = "compat"
	tagServer          = "server"
	tagProtocolVersion = "protocol_version"
	tagCatalog         = "catalog"
	tagTools           = "tools"
	tagBindings        = "bindings"
	tagTimeouts        = "timeouts"
	tagAllow           = "allow"
	tagDeny            = "deny"
	tagEnd             = "end"
)

// ToolIdentity is one tool's contribution to a binding's identity: what it is
// called, on the wire and to a model, and the shape of its interface.
//
// The schemas are present as digests, never as documents. That is the design's
// rule for the manifest and it is also the only form that is useful here: a
// manifest is compared, not read, and a schema document is unbounded input from
// an untrusted server.
type ToolIdentity struct {
	// RawName is the server's own name for the tool.
	RawName string
	// ModelName is the sanitized, binding-qualified name a model sees. It is
	// separate identity from RawName because the mapping is this host's, not the
	// server's: two hosts can see the same raw tool under different model names,
	// and a model name changing under a stable raw name is a change to what the
	// model was shown.
	ModelName string
	// InputSchemaDigest is the hex digest of the tool's input schema.
	InputSchemaDigest string
	// OutputSchemaDigest is the hex digest of the tool's output schema, empty
	// when the tool has none.
	OutputSchemaDigest string
}

// BindingIdentity is one binding's secret-free identity: the design's
// configuration-manifest entry (§Session restore and configuration identity).
//
// It mixes two kinds of fact, and the mix is deliberate. The configuration half
// (Name through CompatDigest) is what the application declared and is knowable
// before anything connects. The negotiated half (Server through Tools) is what
// the server turned out to be and is only knowable after discovery, so it is
// zero on a binding that never became ready. Both belong in the same identity
// because both are things a session must not silently resume under a change to:
// a server swapped behind a stable URL is as much a change as a URL swapped.
type BindingIdentity struct {
	// Name is the binding's stable name within the Session.
	Name string
	// Scope names the connection's owner.
	Scope Scope
	// Loop is the owning Loop for a loop-scoped binding, zero otherwise. It is
	// the "owner identity" half of the design's "Loop selector identity (or
	// owner identity)".
	Loop uuid.UUID
	// SelectorDigest is the hex digest of a session-scoped binding's Visibility:
	// which Loops may consume it. It is empty for a loop-scoped binding, which
	// has no selector — its audience is its owner, which Loop names.
	//
	// It is a digest rather than the membership list because the list is
	// unbounded and, for a Named selector, is application vocabulary that has no
	// business in a journal. What a restore needs is whether the audience
	// changed, and equality answers that.
	SelectorDigest string
	// TransportKind names the transport, e.g. "stdio".
	TransportKind string
	// RedactedOrigin is the transport's display origin. It is redacted at the
	// source: TransportFactory.RedactedOrigin's contract is that it never
	// contains credentials, and this field is that value verbatim.
	RedactedOrigin string
	// Required reports the binding's startup posture. It is identity because it
	// is a behavior contract: the same servers with one flipped from optional to
	// required is a Session that now refuses to start.
	Required bool

	// CapabilityDigest is the hex digest of the client capabilities this binding
	// advertises to its server.
	CapabilityDigest string
	// FilterDigest is the hex digest of the binding's ToolFilter.
	FilterDigest string
	// LimitsDigest is the hex digest of the binding's declared timeouts and
	// limits.
	LimitsDigest string
	// CompatDigest is the hex digest of the binding's compatibility profile —
	// client.Profile.Digest, the checkable form of Status.CompatProfile's label.
	CompatDigest string

	// Server is what the server claimed to be at initialize. Zero before a
	// handshake. Cosmetic in the protocol's terms — it names a peer, it never
	// authorizes one — but a change to it is still a change worth reporting.
	Server client.ServerIdentity
	// ProtocolVersion is the version negotiated at initialize. Empty before the
	// handshake.
	ProtocolVersion string
	// CatalogGeneration is the ordinal of the adopted catalog, 0 before one is
	// adopted.
	//
	// It is reported but NOT digested: a generation number is an ordinal, not
	// content. Two runs that discover the same catalog after a different number
	// of refreshes have the same configuration, and digesting the ordinal would
	// report drift on every reconnect. CatalogDigest is the content.
	CatalogGeneration uint64
	// CatalogDigest is the hex digest of the adopted catalog — the server's
	// whole offering, independent of this host's filter. Empty before one is
	// adopted.
	CatalogDigest string
	// Tools are the tools this binding actually exposes: the catalog projected
	// through the ToolFilter, in the catalog's stable order.
	//
	// Filtered, where CatalogDigest is not, and the pair is the point. The
	// catalog digest says what the server offers; these say what this binding
	// made of it. A filter change moves these and not the catalog digest, which
	// is exactly the distinction a drift report wants to draw.
	Tools []ToolIdentity

	// Digest is the hex digest of every other field of this identity: the
	// binding's whole secret-free identity in one comparable value.
	Digest string
}

// ConfigIdentity returns the secret-free identity of every binding, in
// deterministic name order.
//
// It is a snapshot, like Status: it reads what each binding is right now, so a
// binding that has not finished discovery reports its configuration half and a
// zero negotiated half. An application comparing identities across a restore
// therefore takes it after Start — comparing a pre-discovery identity against a
// post-discovery one reports drift that is only earliness.
//
// After Start is not the same as after every binding has settled, and the gap is
// Required's: Start waits for the required bindings and returns while the
// optional ones are still connecting (see Start). So an optional binding may
// legitimately report a zero negotiated half from a Manager that is fully
// started, and a digest taken twice at different moments may differ for no
// reason but timing. That is a property of optional bindings rather than a
// defect here — an optional server is one the owner does not wait for, so its
// catalog is not something the owner can be said to have started under — but an
// application that stamps a fingerprint from bindings it expects to be
// discovered should mark them Required, which is what "the owner may not come up
// without this" already means.
func (m *Manager) ConfigIdentity() []BindingIdentity {
	m.mu.Lock()
	states := slices.Collect(maps.Values(m.states))
	m.mu.Unlock()

	out := make([]BindingIdentity, 0, len(states))
	for _, bs := range states {
		out = append(out, bs.identity())
	}
	slices.SortFunc(out, func(a, b BindingIdentity) int { return cmp.Compare(a.Name, b.Name) })
	return out
}

// ConfigDigest returns the hex digest of the whole MCP configuration identity:
// the value a composing application stamps into
// event.ConfigFingerprint.ExternalCapabilityRev.
//
// It is empty for a Manager with no bindings. That is the contract the
// fingerprint field needs rather than an accident: the field's meaning is
// "empty = no external capability", so an application that configures no MCP at
// all must produce the empty string and compare Equal to a journal written
// before the field existed. A Manager with bindings can never digest to empty —
// no preimage for the empty string is known, because it is not a digest.
//
// See ConfigIdentity for why this is taken after Start.
//
// # The restore flow
//
// Harness does not call this. Nothing in this module knows what a Session is, so
// the composition root — which owns both the Manager and the Session — is what
// joins them, and the whole protocol is:
//
//  1. Recreate the Manager from the CURRENT application configuration. Never
//     from journal bytes: MCP connections are live resources, and a stdio child
//     or an OAuth token from last week is not a thing to reconstitute (design
//     §Session restore).
//  2. Start it, so the negotiated half of the identity is real — the servers'
//     catalogs are what they are today, not what they were.
//  3. Take ConfigDigest and stamp it into the ConfigFingerprint's
//     ExternalCapabilityRev, alongside the model, prompt and tool-policy revs the
//     application already supplies.
//  4. Restore the Session. Harness compares the whole fingerprint against the one
//     stamped at SessionStarted and rejects a mismatch with a typed
//     *ConfigMismatchError. An application that means to resume across a change
//     passes sessionruntime's WithAllowConfigMismatch.
//
// Step 4 is the entire drift report, and it is deliberately blunt: one boolean
// decision over one equality. There is no per-binding drift here, no adopted
// epoch, and no "which server changed" — the design that specifies those
// (2026-07-16-session-versioning-migration-design.md) is not implemented, and a
// richer report from this module would be a shape Harness has nothing to do
// with. ConfigIdentity is what an application renders when it wants to TELL a
// human what moved; the digest is what the machine decides on.
//
// # What drift does not do
//
// It does not make the journal unreadable. Changing servers, tools, schemas,
// auth posture, transports, or filters is a configuration change, not a
// migration: historical MCP calls in the journal remain data, and a tool that no
// longer exists yields a structured ToolUnavailable result on the next call
// rather than an unrestorable Session (design §Session restore). Accepting drift
// is therefore a normal thing to do, which is why the override is a boolean and
// not a repair.
func (m *Manager) ConfigDigest() string {
	return digestConfig(m.ConfigIdentity())
}

// digestConfig returns the hex digest of a whole configuration's identity, or
// the empty string when there are no bindings. It is separate from ConfigDigest
// so the aggregate encoding can be pinned to a golden without standing a
// Manager up around it.
func digestConfig(ids []BindingIdentity) string {
	if len(ids) == 0 {
		return ""
	}
	h := sha256.New()
	e := canonical.NewEncoder(h)
	e.Str(domainConfig)
	e.Uint(identitySchemaVersion)
	e.Field(tagBindings)
	e.Count(len(ids))
	for _, id := range ids {
		// The per-binding digest, not the binding's fields again: it already
		// covers them, and covering them twice would mean two encodings to keep
		// in step.
		e.Str(id.Digest)
	}
	e.Field(tagEnd)
	return hex.EncodeToString(h.Sum(nil))
}

// identity reads one binding's identity under its own lock.
func (bs *bindingState) identity() BindingIdentity {
	bs.mu.Lock()
	b, cl := bs.binding, bs.cl
	bs.mu.Unlock()

	id := BindingIdentity{
		Name:             b.Name,
		Scope:            b.Scope,
		Loop:             b.Loop,
		TransportKind:    b.Server.Transport.Kind(),
		RedactedOrigin:   b.Server.Transport.RedactedOrigin(),
		Required:         b.Required,
		CapabilityDigest: digestCapabilities(b.Server.Capabilities),
		FilterDigest:     digestFilter(b.Server.ToolFilter),
		LimitsDigest:     digestLimits(b.Server.Timeouts, b.Server.Limits),
		CompatDigest:     compatOf(b.Server).Digest(),
	}
	if b.Scope == ScopeSession {
		id.SelectorDigest = digestSelector(b.Visibility)
	}
	if cl != nil {
		if cat := cl.Catalog(); cat.Valid() {
			id.Server = cat.Server
			id.ProtocolVersion = cat.ProtocolVersion
			id.CatalogGeneration = cat.Generation
			id.CatalogDigest = cat.Digest
			for _, t := range cat.Tools {
				id.Tools = append(id.Tools, ToolIdentity{
					RawName:            t.RawName,
					ModelName:          t.ModelName,
					InputSchemaDigest:  t.InputSchemaDigest,
					OutputSchemaDigest: t.OutputSchemaDigest,
				})
			}
		}
	}
	id.Digest = id.computeDigest()
	return id
}

// compatOf returns the profile a definition will actually run under, resolving
// the zero profile to the default the same way client.Definition does.
//
// Resolving rather than digesting the zero value keeps two definitions that
// behave identically from reporting drift: "I did not choose a profile" and "I
// chose the default" are the same policy, and an identity that called them
// different would report a change to a configuration nobody changed.
func compatOf(d client.Definition) client.Profile {
	if d.Compat.Name == "" && d.Compat.Version == 0 && len(d.Compat.Tolerances) == 0 {
		return client.ProfileDefault
	}
	return d.Compat
}

// computeDigest returns the canonical digest of a binding's identity.
//
// It covers every other field of the BindingIdentity, with one exception, which
// is documented on CatalogGeneration: an ordinal is not content.
func (id BindingIdentity) computeDigest() string {
	h := sha256.New()
	e := canonical.NewEncoder(h)

	e.Str(domainBinding)
	e.Uint(identitySchemaVersion)

	e.Field(tagName)
	e.Str(id.Name)

	e.Field(tagScope)
	e.Uint(uint64(id.Scope))

	e.Field(tagLoop)
	e.Str(id.Loop.String())

	e.Field(tagSelector)
	e.Str(id.SelectorDigest)

	e.Field(tagTransport)
	e.Str(id.TransportKind)
	e.Str(id.RedactedOrigin)

	e.Field(tagRequired)
	e.Bool(id.Required)

	e.Field(tagCapabilities)
	e.Str(id.CapabilityDigest)

	e.Field(tagFilter)
	e.Str(id.FilterDigest)

	e.Field(tagLimits)
	e.Str(id.LimitsDigest)

	e.Field(tagCompat)
	e.Str(id.CompatDigest)

	e.Field(tagServer)
	e.Str(id.Server.Name)
	e.Str(id.Server.Version)
	e.Str(id.Server.Title)

	e.Field(tagProtocolVersion)
	e.Str(id.ProtocolVersion)

	e.Field(tagCatalog)
	e.Str(id.CatalogDigest)

	e.Field(tagTools)
	e.Count(len(id.Tools))
	for _, t := range id.Tools {
		e.Str(t.RawName)
		e.Str(t.ModelName)
		e.Str(t.InputSchemaDigest)
		e.Str(t.OutputSchemaDigest)
	}

	e.Field(tagEnd)
	return hex.EncodeToString(h.Sum(nil))
}

// digestSelector returns the hex digest of a LoopSelector's identity: its mode
// and its membership.
//
// The members are sorted and deduplicated, so two selectors that permit the same
// Loops digest the same however they were written. That is the same canonical
// treatment client.Profile.Digest gives its tolerances, and for the same reason:
// the order a caller happened to list them in is not policy.
//
// The mode is covered separately from the membership because an empty ID
// selector and an empty name selector are different policies that would
// otherwise share an encoding — and because AllLoops, which has no membership at
// all, must not collide with either.
func digestSelector(s LoopSelector) string {
	h := sha256.New()
	e := canonical.NewEncoder(h)
	e.Str(domainSelector)
	e.Uint(identitySchemaVersion)
	e.Uint(uint64(s.mode))

	ids := slices.Clone(s.ids)
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return cmp.Compare(a.String(), b.String()) })
	ids = slices.Compact(ids)
	e.Count(len(ids))
	for _, id := range ids {
		e.Str(id.String())
	}

	names := slices.Clone(s.names)
	slices.Sort(names)
	names = slices.Compact(names)
	e.Count(len(names))
	for _, n := range names {
		e.Str(n)
	}

	e.Field(tagEnd)
	return hex.EncodeToString(h.Sum(nil))
}

// digestCapabilities returns the hex digest of the optional client capabilities
// a binding advertises.
//
// A digest of three booleans is not a space saving. It is uniformity: the
// manifest's shape is "capability, filter, limits, and compatibility policy
// digests", and a field that is a digest today cannot become a leak tomorrow
// when a capability grows a payload. It is also domain-separated, so it can
// never be confused with a filter digest that happens to encode to the same
// bytes.
func digestCapabilities(c client.ClientCapabilities) string {
	h := sha256.New()
	e := canonical.NewEncoder(h)
	e.Str(domainCapabilities)
	e.Uint(identitySchemaVersion)
	e.Bool(c.Elicitation)
	e.Bool(c.Sampling)
	e.Bool(c.Roots)
	e.Field(tagEnd)
	return hex.EncodeToString(h.Sum(nil))
}

// digestFilter returns the hex digest of a binding's ToolFilter.
//
// Allow and Deny are each sorted and deduplicated: the filter's behavior is set
// membership (see ToolFilter.Permits, which uses slices.Contains), so the order
// a caller wrote them in is not policy and must not be identity. They are
// counted and framed separately, so a name cannot slide from one set to the
// other.
func digestFilter(f client.ToolFilter) string {
	h := sha256.New()
	e := canonical.NewEncoder(h)
	e.Str(domainFilter)
	e.Uint(identitySchemaVersion)
	for _, set := range []struct {
		tag     string
		entries []string
	}{
		{tagAllow, f.Allow},
		{tagDeny, f.Deny},
	} {
		e.Field(set.tag)
		entries := slices.Clone(set.entries)
		slices.Sort(entries)
		entries = slices.Compact(entries)
		e.Count(len(entries))
		for _, entry := range entries {
			e.Str(entry)
		}
	}
	e.Field(tagEnd)
	return hex.EncodeToString(h.Sum(nil))
}

// digestLimits returns the hex digest of a binding's declared timeouts and
// limits.
//
// It digests what the application DECLARED, not the effective values a zero
// field resolves to at connect time. The two differ only for a caller that
// writes a default out explicitly, and the declared form is the one this package
// can read without widening pkg/client's API to expose normalization. The cost
// is a spurious drift report for a configuration edited from "0" to the exact
// default — which is a configuration edit, and reporting it is the conservative
// direction: a spurious drift costs an operator a glance, where a missed one
// resumes a session under bounds it never agreed to.
func digestLimits(t client.Timeouts, l client.Limits) string {
	h := sha256.New()
	e := canonical.NewEncoder(h)
	e.Str(domainLimits)
	e.Uint(identitySchemaVersion)

	e.Field(tagTimeouts)
	e.Int(int64(t.Startup))
	e.Int(int64(t.Request))
	e.Int(int64(t.Elicitation))

	e.Field(tagLimits)
	// Field order here is the struct's order, and it is load-bearing: these are
	// all ints, so the field tags above cannot separate them from one another.
	// What keeps them apart is that each is written at a fixed position, so
	// swapping two of them changes the encoding. Add a limit at the END, and
	// bump identitySchemaVersion.
	for _, v := range []int{
		l.MaxConcurrentRequests,
		l.MaxCatalogPages,
		l.MaxCatalogItems,
		l.MaxFrameBytes,
		l.MaxBodyBytes,
		l.MaxSchemaBytes,
		l.MaxSchemaDepth,
		l.MaxTextResultBytes,
		l.MaxStructuredBytes,
		l.MaxBinaryItemBytes,
		l.MaxBinaryItems,
		l.MaxLogMessageBytes,
		l.MaxElicitMessageBytes,
		l.MaxElicitSchemaBytes,
		l.MaxPromptCount,
		l.MaxResourceCount,
		l.MaxSamplingDepth,
		l.MaxSamplingConcurrency,
		l.MaxSamplingTokens,
	} {
		e.Int(int64(v))
	}

	e.Field(tagEnd)
	return hex.EncodeToString(h.Sum(nil))
}
