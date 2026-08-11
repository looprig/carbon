// Package catalog holds a binding's view of what an MCP server offers, as a
// sequence of immutable generations.
//
// A Generation is the unit of catalog identity: it is built once, from data a
// server supplied, and never changes afterwards. Everything a caller reads back
// is a copy, so a consumer can neither observe a torn value nor rewrite one
// another consumer is relying on. That immutability is what lets several
// readers hold different generations at once — one loop still serving a turn
// against generation 4 while another has already adopted generation 5 — without
// any of them locking.
//
// The package is deliberately ignorant of two things. It does not fetch
// (discover.go drives a connection, but the connection is injected), and it does
// not decide what a *model* may see: a Generation records what the server
// offered, in full. Client-side policy — a ToolFilter, a permission rule —
// shapes the projection above this package, never the record inside it. Keeping
// server truth and host policy in separate layers is what makes a generation
// comparable across configurations: the same server yields the same digest
// whatever the host allows.
//
// Everything here originates from an untrusted peer. Bounds are enforced before
// retention, names are validated before they become identifiers, and nothing
// panics on malformed input.
package catalog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/looprig/mcp/internal/protocol"
)

// Family names one catalog family — one list method's worth of a server's
// offering. It is typed rather than a bare string because discovery, warnings
// and (later) change notifications all route on it.
type Family uint8

// The catalog families this module fetches.
const (
	// FamilyTools is tools/list.
	FamilyTools Family = iota + 1
	// FamilyPrompts is prompts/list.
	FamilyPrompts
	// FamilyResources is resources/list.
	FamilyResources
	// FamilyResourceTemplates is resources/templates/list.
	FamilyResourceTemplates

	familySentinel // must remain last; tests derive the declared range from it
)

// familyNames maps each family to its stable lowercase identifier. These
// identifiers reach warnings, decisions and telemetry, so they must not change.
var familyNames = [familySentinel]string{
	FamilyTools:             "tools",
	FamilyPrompts:           "prompts",
	FamilyResources:         "resources",
	FamilyResourceTemplates: "resource_templates",
}

// String returns the family's stable identifier, or "unknown" for any value
// outside the declared range.
func (f Family) String() string {
	if f < FamilyTools || f >= familySentinel {
		return "unknown"
	}
	return familyNames[f]
}

// DecisionAction is what discovery did about a family, and why it is a closed
// enum: a compatibility decision is a fact the binding reports, not free text.
type DecisionAction uint8

// The decisions discovery can reach for a family.
const (
	// ActionFetched means the family was advertised and fetched.
	ActionFetched DecisionAction = iota + 1
	// ActionSkippedNotAdvertised means the server did not advertise the
	// capability, so the method was never called. This is the compatibility
	// rule in force, not an error: a client that calls an unadvertised method
	// is guessing.
	ActionSkippedNotAdvertised

	actionSentinel // must remain last; tests derive the declared range from it
)

// actionNames maps each action to its stable lowercase identifier.
var actionNames = [actionSentinel]string{
	ActionFetched:              "fetched",
	ActionSkippedNotAdvertised: "skipped_not_advertised",
}

// String returns the action's stable identifier, or "unknown".
func (a DecisionAction) String() string {
	if a < ActionFetched || a >= actionSentinel {
		return "unknown"
	}
	return actionNames[a]
}

// Tolerance is a safe deviation this catalog applied to a server that does not
// implement the specification perfectly.
//
// The set is closed and every member is one of the design's *safe* tolerances.
// The unsafe ones — replacing an invalid input schema with unconstrained
// arguments, treating malformed framing as valid, treating an auth failure as
// success — are not here, and their absence is the point: a tolerance that
// cannot be named cannot be applied, whatever a configuration asks for.
type Tolerance uint8

// The tolerances this package can apply.
const (
	// ToleranceInvalidOutputSchema means a defective *optional* output schema
	// was dropped and its tool kept. The input schema — the one that constrains
	// what a model may send — is never tolerated this way: a tool with a
	// defective input schema is rejected, because keeping it would mean
	// widening its arguments.
	ToleranceInvalidOutputSchema Tolerance = iota + 1
	// ToleranceNormalizedDisplayName means a raw name that inference providers
	// will not accept was normalized into one they will. The raw name is
	// preserved and remains what goes on the wire (see identity.go).
	ToleranceNormalizedDisplayName

	toleranceSentinel // must remain last; tests derive the declared range from it
)

// toleranceNames maps each tolerance to its stable lowercase identifier. These
// reach warnings, diagnostics and configuration identity, so they must not
// change.
var toleranceNames = [toleranceSentinel]string{
	ToleranceInvalidOutputSchema:   "invalid_output_schema",
	ToleranceNormalizedDisplayName: "display_name_normalization",
}

// String returns the tolerance's stable identifier, or "unknown".
func (t Tolerance) String() string {
	if t < ToleranceInvalidOutputSchema || t >= toleranceSentinel {
		return "unknown"
	}
	return toleranceNames[t]
}

// Tolerances is the compatibility policy in force for one build: which safe
// deviations this binding is willing to apply.
//
// The zero value tolerates nothing, which is the strict reading of the
// specification and the fail-closed default for a package that cannot know what
// its caller configured. The client passes what its profile permits.
type Tolerances struct {
	// InvalidOutputSchema allows a tool whose optional output schema was
	// defective to be kept without it. When false, such a tool makes the whole
	// generation invalid.
	InvalidOutputSchema bool
	// NormalizeDisplayNames allows a raw name that is not provider-compatible
	// to be sanitized or truncated into one that is. When false, such a tool
	// makes the whole generation invalid.
	NormalizeDisplayNames bool
}

// Decision records what discovery did about one family. It is a compatibility
// decision in the design's sense: the record of a choice the client made
// against what the server advertised, kept so that "the server has no prompts"
// and "we never asked" are distinguishable after the fact.
type Decision struct {
	Family Family
	Action DecisionAction
}

// Tool is one tool in a generation, with the derived identity and digests the
// rest of the module routes on.
//
// The two names are not interchangeable and neither is derived from the other at
// use time. RawName is what goes on the wire; ModelName is what a model sees.
// The mapping between them is this struct — a caller resolves a ModelName by
// looking it up here, never by parsing it (see identity.go).
type Tool struct {
	// RawName is the server's own name for the tool, exactly as sent.
	RawName string
	// ModelName is the sanitized, binding-qualified identity a model sees.
	ModelName string

	Title       string
	Description string

	// InputSchema is the tool's argument schema. It is always present: a tool
	// without one is rejected at conversion.
	InputSchema json.RawMessage
	// OutputSchema is the optional result schema, nil when the server sent none
	// or when a defective one was dropped (see Warnings).
	OutputSchema json.RawMessage

	// InputSchemaDigest is the digest of InputSchema's bytes.
	InputSchemaDigest Digest
	// OutputSchemaDigest is the digest of OutputSchema's bytes, zero when there
	// is no output schema.
	OutputSchemaDigest Digest

	// Annotations are the server's behavioural hints. They are untrusted policy
	// input and never authority. Nil when the server sent none.
	Annotations *protocol.ToolAnnotations
	// Warnings records defects tolerated when this tool was converted.
	Warnings []string
}

// clone deep-copies t so that neither the caller nor the generation can reach
// the other's memory.
func (t Tool) clone() Tool {
	t.InputSchema = slices.Clone(t.InputSchema)
	t.OutputSchema = slices.Clone(t.OutputSchema)
	t.Warnings = slices.Clone(t.Warnings)
	if t.Annotations != nil {
		a := *t.Annotations
		a.DestructiveHint = cloneBool(a.DestructiveHint)
		a.OpenWorldHint = cloneBool(a.OpenWorldHint)
		t.Annotations = &a
	}
	return t
}

// cloneBool copies a tri-state hint, preserving nil.
func cloneBool(p *bool) *bool {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// Generation is one immutable snapshot of a server's catalog.
//
// Every field is unexported and every accessor returns a copy: a Generation is
// shared by reference across goroutines with no lock, which is only sound
// because nothing can mutate it — including a caller who was handed a slice out
// of it.
//
// Build is the only constructor. The zero value is not a usable Generation.
type Generation struct {
	binding         string
	number          uint64
	protocolVersion protocol.ProtocolVersion
	capabilities    protocol.ServerCapabilities
	server          protocol.ServerIdentity
	instructions    string

	tools     []Tool
	prompts   []protocol.PromptSpec
	resources []protocol.ResourceSpec
	templates []protocol.ResourceTemplateSpec

	warnings   []string
	decisions  []Decision
	tolerances []Tolerance

	digest Digest

	// byRawName and byModelName index tools for lookup. They are built once, at
	// Build, and never written afterwards, so concurrent reads need no lock.
	byRawName   map[string]int
	byModelName map[string]int
}

// Builder accumulates the parts of a Generation. It is an ordinary mutable
// value: a caller fills it in, calls Build, and the result is immutable. A
// Builder may be discarded, reused or mutated afterwards without affecting a
// Generation it already produced — Build copies everything it keeps.
type Builder struct {
	// Binding is the configured name of the server binding this catalog
	// belongs to. It qualifies every model-facing tool name, so it is part of
	// the catalog's identity rather than ambient context.
	Binding string
	// Number is the generation's ordinal within the binding. It orders
	// generations; it deliberately does not participate in the digest (see
	// Digest).
	Number uint64

	ProtocolVersion protocol.ProtocolVersion
	Capabilities    protocol.ServerCapabilities
	Server          protocol.ServerIdentity
	// Instructions is the server's usage hint, already bounded by the
	// conversion that produced it.
	Instructions string

	Tools             []protocol.ToolSpec
	Prompts           []protocol.PromptSpec
	Resources         []protocol.ResourceSpec
	ResourceTemplates []protocol.ResourceTemplateSpec

	// Warnings records defects tolerated during discovery.
	Warnings []string
	// DroppedWarnings is how many warnings the caller raised but could not keep
	// before Build was reached — discovery bounds each family as it fetches it.
	// It is carried so that the summary Build writes counts every warning that
	// was ever raised, not merely those that survived to here.
	DroppedWarnings int
	// Decisions records what discovery did about each family.
	Decisions []Decision
	// Tolerances is the compatibility policy Build applies. The zero value
	// tolerates nothing: a defect that is not explicitly tolerated makes the
	// generation invalid.
	Tolerances Tolerances
}

// MaxWarnings caps the warnings one generation retains, so that a server which
// produces a tolerated defect per item cannot turn diagnostics into unbounded
// memory.
//
// The text of a warning past the cap is dropped; the fact of it is not. The
// last slot is spent on a summary naming how many were raised in all, because a
// cap that simply stopped appending would make a server with 64 tolerated
// defects indistinguishable from one with 6400 — and a reader would take the 64
// messages for the whole story. The cap bounds how much is said, never whether
// it is said.
const MaxWarnings = 64

// warningBudget accumulates warnings under MaxWarnings while remembering how
// many it could not keep.
//
// The count is why this is a type and not an append helper. Warning text is
// server-influenced and must be bounded, but the number of warnings is one
// integer, and it survives every layer that folds one budget into another — so
// the total the summary reports is the true one and not merely what reached the
// last layer.
type warningBudget struct {
	kept    []string
	dropped int
}

// add keeps each message until the budget is full, then counts it instead. The
// final slot is left free for the summary render writes there.
func (b *warningBudget) add(msgs ...string) {
	for _, m := range msgs {
		if len(b.kept) >= MaxWarnings-1 {
			b.dropped++
			continue
		}
		b.kept = append(b.kept, m)
	}
}

// merge folds another budget in: its messages compete for the remaining slots,
// and whatever it had already dropped stays counted.
func (b *warningBudget) merge(o warningBudget) {
	b.add(o.kept...)
	b.dropped += o.dropped
}

// render returns the bounded list, ending in a summary of what the cap hid
// whenever it hid anything.
func (b warningBudget) render() []string {
	if b.dropped == 0 {
		return slices.Clone(b.kept)
	}
	return append(slices.Clone(b.kept), fmt.Sprintf(
		"%d further warning(s) were discarded without their text (%d raised in all, over this client's %d warning cap)",
		b.dropped, len(b.kept)+b.dropped, MaxWarnings))
}

// Build validates the accumulated parts and returns the immutable Generation.
//
// It is where a generation's derived identity is settled: raw names are
// validated, the collections are put in a canonical order, model-facing names
// are constructed for the tool set as a whole (which is why naming happens here
// and not per item — collision resolution needs to see every sibling), schemas
// are digested, and the catalog digest is computed over the result.
//
// Build fails closed. A duplicate raw name, an unusable name, or an empty
// binding is a defect that makes the catalog ambiguous rather than merely
// smaller, so it rejects the whole generation instead of publishing a partial
// one.
func (b Builder) Build() (*Generation, error) {
	if b.Binding == "" {
		return nil, &DefectError{Reason: "binding name is empty"}
	}

	g := &Generation{
		binding:         b.Binding,
		number:          b.Number,
		protocolVersion: b.ProtocolVersion,
		capabilities:    b.Capabilities,
		server:          b.Server,
		instructions:    b.Instructions,
		decisions:       slices.Clone(b.Decisions),
	}

	warnings := warningBudget{dropped: b.DroppedWarnings}
	warnings.add(b.Warnings...)

	tools, applied, err := buildTools(b.Binding, b.Tools, b.Tolerances)
	if err != nil {
		return nil, err
	}
	g.tools = tools
	g.tolerances = applied
	for _, t := range applied {
		// Every applied tolerance is reported in diagnostics as well as being
		// enumerable: a warning is what an operator reads, and
		// AppliedTolerances is what a program branches on.
		warnings.add("compatibility: applied tolerance " + t.String())
	}
	g.warnings = warnings.render()

	if g.prompts, err = buildPrompts(b.Prompts); err != nil {
		return nil, err
	}
	if g.resources, err = buildResources(b.Resources); err != nil {
		return nil, err
	}
	if g.templates, err = buildTemplates(b.ResourceTemplates); err != nil {
		return nil, err
	}

	g.byRawName = make(map[string]int, len(g.tools))
	g.byModelName = make(map[string]int, len(g.tools))
	for i, t := range g.tools {
		g.byRawName[t.RawName] = i
		g.byModelName[t.ModelName] = i
	}

	g.digest = g.computeDigest()
	return g, nil
}

// buildTools validates, orders, digests and names the tool set, applying the
// compatibility policy and reporting which of its tolerances it needed.
//
// It fails closed against the policy: a defect the policy does not tolerate
// rejects the whole generation rather than the one tool. That is the same
// all-or-nothing rule discovery follows, and for the same reason — a catalog
// silently missing a tool is indistinguishable from a server that removed it.
func buildTools(binding string, specs []protocol.ToolSpec, tol Tolerances) ([]Tool, []Tolerance, error) {
	var applied []Tolerance
	tools := make([]Tool, 0, len(specs))
	seen := make(map[string]struct{}, len(specs))
	for _, s := range specs {
		if err := validateRawName(FamilyTools, s.RawName); err != nil {
			return nil, nil, err
		}
		if err := checkInputSchema(s); err != nil {
			return nil, nil, err
		}
		if s.OutputSchemaDefect != "" {
			if !tol.InvalidOutputSchema {
				return nil, nil, &DefectError{
					Family: FamilyTools,
					Reason: fmt.Sprintf("tool %q has an invalid output schema (%s) and this binding's compatibility profile does not tolerate one", s.RawName, s.OutputSchemaDefect),
				}
			}
			if !slices.Contains(applied, ToleranceInvalidOutputSchema) {
				applied = append(applied, ToleranceInvalidOutputSchema)
			}
		}
		if _, dup := seen[s.RawName]; dup {
			// Two tools with one name make every call to that name ambiguous.
			// There is no safe pick — either could be the destructive one — so
			// the catalog is rejected rather than silently resolved.
			return nil, nil, &DefectError{
				Family: FamilyTools,
				Reason: fmt.Sprintf("duplicate tool name %q", s.RawName),
			}
		}
		seen[s.RawName] = struct{}{}

		t := Tool{
			RawName:           s.RawName,
			Title:             s.Title,
			Description:       s.Description,
			InputSchema:       slices.Clone(s.InputSchema),
			OutputSchema:      slices.Clone(s.OutputSchema),
			InputSchemaDigest: DigestBytes(s.InputSchema),
			Annotations:       s.Annotations,
			Warnings:          slices.Clone(s.Warnings),
		}
		// A zero OutputSchemaDigest means "no output schema", which is why it is
		// not digested when absent: DigestBytes(nil) is a real digest of the
		// empty input and would be indistinguishable from one.
		if len(s.OutputSchema) > 0 {
			t.OutputSchemaDigest = DigestBytes(s.OutputSchema)
		}
		// Clone the annotations rather than alias the caller's.
		tools = append(tools, t.clone())
	}

	// Canonical order first, then naming: assignModelNames resolves collisions
	// by looking at the whole set, and must see it in a deterministic order or
	// the resolution would depend on the order the server happened to paginate.
	slices.SortFunc(tools, func(a, b Tool) int { return compareStrings(a.RawName, b.RawName) })
	normalized, err := assignModelNames(binding, tools, tol.NormalizeDisplayNames)
	if err != nil {
		return nil, nil, err
	}
	if normalized {
		applied = append(applied, ToleranceNormalizedDisplayName)
	}
	return tools, applied, nil
}

// checkInputSchema refuses a tool that would reach a model with nothing
// constraining its arguments.
//
// This is defence in depth, and it is deliberate duplication: internal/protocol
// already rejects a tool whose input schema is missing or not a JSON object, and
// today Discover is the only thing that fills a Builder, so nothing here is
// reachable. The reason to keep it is what rests on it. "Never replace an
// invalid input schema with unconstrained arguments" is the design's first named
// *unsafe* tolerance, and it is the one invariant in this package whose failure
// is silent: a tool with no schema is not a broken tool, it is a tool a model may
// call with anything, and it looks perfectly healthy in a catalog. An invariant
// that severe should not rest on one layer and a comment about who calls whom —
// Builder is exported within the module, and the next caller (a test fixture, a
// replay, a future non-Discover path) inherits the guarantee rather than having
// to know about it.
//
// It checks shape, not content: presence, valid JSON, and a JSON object, which
// is exactly what "there is a schema constraining these arguments" means here.
// Validating the schema *document* is a job for whoever validates arguments
// against it, and duplicating that would be the bad kind of duplication —
// two validators to disagree with each other.
func checkInputSchema(s protocol.ToolSpec) error {
	defect := func(reason string) error {
		return &DefectError{
			Family: FamilyTools,
			Reason: fmt.Sprintf("tool %q %s: a tool without one would reach a model with unconstrained arguments", s.RawName, reason),
		}
	}
	trimmed := bytes.TrimSpace(s.InputSchema)
	switch {
	case len(trimmed) == 0:
		return defect("has no input schema")
	case trimmed[0] != '{':
		return defect("has an input schema that is not a JSON object")
	case !json.Valid(trimmed):
		return defect("has an input schema that is not valid JSON")
	}
	return nil
}

func buildPrompts(specs []protocol.PromptSpec) ([]protocol.PromptSpec, error) {
	out := make([]protocol.PromptSpec, 0, len(specs))
	seen := make(map[string]struct{}, len(specs))
	for _, s := range specs {
		if err := validateRawName(FamilyPrompts, s.RawName); err != nil {
			return nil, err
		}
		if _, dup := seen[s.RawName]; dup {
			return nil, &DefectError{
				Family: FamilyPrompts,
				Reason: fmt.Sprintf("duplicate prompt name %q", s.RawName),
			}
		}
		seen[s.RawName] = struct{}{}
		s.Arguments = slices.Clone(s.Arguments)
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b protocol.PromptSpec) int { return compareStrings(a.RawName, b.RawName) })
	return out, nil
}

func buildResources(specs []protocol.ResourceSpec) ([]protocol.ResourceSpec, error) {
	out := make([]protocol.ResourceSpec, 0, len(specs))
	seen := make(map[string]struct{}, len(specs))
	for _, s := range specs {
		// A resource is addressed by URI, not by name, so the URI is what must
		// be well-formed and unique. Its Name is a display string.
		if err := validateRawName(FamilyResources, s.URI); err != nil {
			return nil, err
		}
		if _, dup := seen[s.URI]; dup {
			return nil, &DefectError{
				Family: FamilyResources,
				Reason: fmt.Sprintf("duplicate resource URI %q", s.URI),
			}
		}
		seen[s.URI] = struct{}{}
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b protocol.ResourceSpec) int { return compareStrings(a.URI, b.URI) })
	return out, nil
}

func buildTemplates(specs []protocol.ResourceTemplateSpec) ([]protocol.ResourceTemplateSpec, error) {
	out := make([]protocol.ResourceTemplateSpec, 0, len(specs))
	seen := make(map[string]struct{}, len(specs))
	for _, s := range specs {
		if err := validateRawName(FamilyResourceTemplates, s.URITemplate); err != nil {
			return nil, err
		}
		if _, dup := seen[s.URITemplate]; dup {
			return nil, &DefectError{
				Family: FamilyResourceTemplates,
				Reason: fmt.Sprintf("duplicate resource template %q", s.URITemplate),
			}
		}
		seen[s.URITemplate] = struct{}{}
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b protocol.ResourceTemplateSpec) int {
		return compareStrings(a.URITemplate, b.URITemplate)
	})
	return out, nil
}

// compareStrings orders by raw bytes. strings.Compare, not a locale- or
// case-aware comparison: the ordering feeds a digest, so it must depend on
// nothing but the bytes.
func compareStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// Binding returns the binding name this catalog belongs to.
func (g *Generation) Binding() string { return g.binding }

// Number returns the generation's ordinal within its binding.
func (g *Generation) Number() uint64 { return g.number }

// Digest returns the canonical catalog digest. See computeDigest for what it
// covers.
func (g *Generation) Digest() Digest { return g.digest }

// ProtocolVersion returns the negotiated protocol version.
func (g *Generation) ProtocolVersion() protocol.ProtocolVersion { return g.protocolVersion }

// Capabilities returns what the server advertised at initialize.
func (g *Generation) Capabilities() protocol.ServerCapabilities { return g.capabilities }

// Server returns the raw server identity, exactly as the server claimed it.
func (g *Generation) Server() protocol.ServerIdentity { return g.server }

// Instructions returns the server's bounded usage hint.
func (g *Generation) Instructions() string { return g.instructions }

// ToolCount reports how many tools the generation holds, without copying them.
func (g *Generation) ToolCount() int { return len(g.tools) }

// Tools returns a deep copy of the tool set, in canonical (raw-name) order.
func (g *Generation) Tools() []Tool {
	out := make([]Tool, len(g.tools))
	for i, t := range g.tools {
		out[i] = t.clone()
	}
	return out
}

// ToolByRawName returns the tool the server calls rawName.
func (g *Generation) ToolByRawName(rawName string) (Tool, bool) {
	i, ok := g.byRawName[rawName]
	if !ok {
		return Tool{}, false
	}
	return g.tools[i].clone(), true
}

// ToolByModelName returns the tool a model knows as modelName. This is the
// reverse mapping that makes routing possible without ever parsing a display
// name back into its parts.
func (g *Generation) ToolByModelName(modelName string) (Tool, bool) {
	i, ok := g.byModelName[modelName]
	if !ok {
		return Tool{}, false
	}
	return g.tools[i].clone(), true
}

// Prompts returns a deep copy of the prompt set, in canonical order.
func (g *Generation) Prompts() []protocol.PromptSpec {
	out := make([]protocol.PromptSpec, len(g.prompts))
	for i, p := range g.prompts {
		p.Arguments = slices.Clone(p.Arguments)
		out[i] = p
	}
	return out
}

// Resources returns a copy of the resource set, in canonical (URI) order.
func (g *Generation) Resources() []protocol.ResourceSpec {
	return slices.Clone(g.resources)
}

// ResourceTemplates returns a copy of the template set, in canonical order.
func (g *Generation) ResourceTemplates() []protocol.ResourceTemplateSpec {
	return slices.Clone(g.templates)
}

// Warnings returns a copy of the defects tolerated during discovery.
//
// It holds at most MaxWarnings entries. When more were raised than that, the
// last entry is a summary naming how many there were in all, so a list that
// stops short is never mistaken for the whole story.
func (g *Generation) Warnings() []string { return slices.Clone(g.warnings) }

// AppliedTolerances returns a copy of the compatibility tolerances this
// generation needed, in a deterministic order. It is empty for a server that
// implements the specification faithfully.
//
// It is deliberately not part of the catalog digest, for three reasons that
// point the same way:
//
//   - The digest answers "is this the same server offering?", and two hosts with
//     different compatibility profiles looking at one server must agree on the
//     answer — the same reason a ToolFilter is not in it (see the package doc).
//   - The design's configuration manifest lists "capability, filter, limits, and
//     compatibility policy digests" separately from "adopted catalog digest and
//     tool schema digests". They are two identity components, not one: the
//     policy's identity is the profile's own digest (client.Profile.Digest), and
//     the catalog's is this one.
//   - Including it could not distinguish anything anyway. What a tolerance
//     changed is already digested through the value it changed — a normalized
//     ModelName and an absent OutputSchema are both covered — so "same digest,
//     different applied tolerances" describes two catalogs that are, tool for
//     tool and schema for schema, the same catalog.
func (g *Generation) AppliedTolerances() []Tolerance { return slices.Clone(g.tolerances) }

// Decisions returns a copy of the compatibility decisions discovery made.
func (g *Generation) Decisions() []Decision { return slices.Clone(g.decisions) }
