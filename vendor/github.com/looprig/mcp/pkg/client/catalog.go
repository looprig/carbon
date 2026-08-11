// This file defines the public view of a binding's adopted catalog.
//
// It is a value type, copied out of the immutable generation the client
// adopted. A caller may hold it, mutate it, and outlive the client with it: it
// is a snapshot, and nothing it contains points back at the client's state.
//
// # What a Catalog shows, and what it does not
//
// A Catalog is the *model-facing projection*, not the raw server catalog. The
// distinction is the design's: the catalog holds what the server offers,
// filtering shapes what the model sees. So the internal generation records
// every tool the server advertised — that is server truth, and it is what the
// digest identifies, which is what lets two hosts with different policies agree
// on whether they are looking at the same server — and this view applies the
// binding's ToolFilter on the way out.
//
// The filter is therefore not enforced *here*: a projection is a view, and a
// view is not a security boundary. CallTool re-checks the filter itself against
// the Definition (see calls.go), so a caller that reaches for a denied tool by
// name is refused whether or not it ever looked at a Catalog.

package client

import (
	"encoding/json"

	"github.com/looprig/mcp/internal/catalog"
)

// ServerCapabilities is what a server advertised at initialize, reduced to the
// presence flags this client acts on. It reports what the server said it can
// do; it grants nothing.
type ServerCapabilities struct {
	// Tools reports whether the server exposes tools.
	Tools bool
	// Prompts reports whether the server exposes prompts.
	Prompts bool
	// Resources reports whether the server exposes resources.
	Resources bool
	// ResourcesSubscribe reports whether the server supports subscribing to
	// resource updates. Only meaningful when Resources is set.
	ResourcesSubscribe bool
	// Logging reports whether the server sends log messages.
	Logging bool
	// Completions reports whether the server supports argument autocompletion.
	Completions bool
}

// ToolAnnotations are a server's behavioural hints about a tool.
//
// They are untrusted policy *input* and never authority: a server claiming
// ReadOnlyHint does not make a tool read-only, and a host must not skip a
// permission check because a tool said it was safe. The tri-state hints are
// pointers because "unspecified" differs from "false" — absent means the server
// said nothing, and the caller applies its own default.
type ToolAnnotations struct {
	Title           string
	ReadOnlyHint    bool
	IdempotentHint  bool
	DestructiveHint *bool
	OpenWorldHint   *bool
}

// ToolSpec is one tool as this binding sees it.
type ToolSpec struct {
	// RawName is the server's own name for the tool. It is what goes on the
	// wire, and what CallTool takes.
	RawName string
	// ModelName is the sanitized, binding-qualified identity to show a model.
	// It is deterministic, unique within the catalog, and bounded to fit
	// inference-provider limits.
	//
	// It must never be parsed to recover RawName: sanitization is lossy, so the
	// mapping only holds in this direction. Resolve it with
	// Catalog.ToolByModelName.
	ModelName string

	Title       string
	Description string

	// InputSchema is the JSON Schema a model's arguments must satisfy. It is
	// raw JSON because a schema is a serialization-boundary document, not
	// domain data. It is always present.
	InputSchema json.RawMessage
	// OutputSchema describes the tool's result, when the server supplied a
	// usable one. Nil otherwise; see Warnings.
	OutputSchema json.RawMessage

	// InputSchemaDigest is the hex digest of InputSchema, for detecting a
	// schema change across generations without comparing documents.
	InputSchemaDigest string
	// OutputSchemaDigest is the hex digest of OutputSchema, empty when there is
	// no output schema.
	OutputSchemaDigest string

	// Annotations are the server's hints, or nil.
	Annotations *ToolAnnotations
	// Warnings records defects tolerated for this tool, such as a dropped
	// output schema.
	Warnings []string
}

// PromptSpec is one prompt a server advertises.
type PromptSpec struct {
	Name        string
	Title       string
	Description string
	Arguments   []PromptArg
}

// PromptArg is one argument a prompt accepts.
type PromptArg struct {
	Name        string
	Title       string
	Description string
	Required    bool
}

// ResourceSpec is one concrete resource a server advertises.
type ResourceSpec struct {
	// URI is the resource's opaque protocol identifier — not a host path.
	URI         string
	Name        string
	Title       string
	Description string
	MIMEType    string
}

// ResourceTemplateSpec is an RFC 6570 template for a family of resources.
type ResourceTemplateSpec struct {
	URITemplate string
	Name        string
	Title       string
	Description string
	MIMEType    string
}

// Catalog is a snapshot of the generation a binding has adopted.
//
// The zero Catalog is what a binding with no adopted generation reports;
// Valid distinguishes it from a real, if empty, catalog.
type Catalog struct {
	// Binding names the server binding.
	Binding Name
	// Generation is the generation's ordinal within the binding. It is 0 in the
	// zero Catalog and 1 or more in any real one.
	Generation uint64
	// Digest is the hex canonical catalog digest. Two catalogs with the same
	// digest describe the same server offering; it does not depend on this
	// host's filter or policy.
	Digest string

	// ProtocolVersion is the version negotiated at initialize.
	ProtocolVersion string
	// Server is what the server claims to be. Cosmetic: it names a peer, it
	// never authorizes one.
	Server ServerIdentity
	// Capabilities is what the server advertised.
	Capabilities ServerCapabilities
	// Instructions is the server's bounded usage hint.
	//
	// It is reported, never applied. Concatenating it into a host's system
	// instructions would let any connected server acquire instruction authority
	// merely by being connected; an application that wants it must install it
	// deliberately.
	Instructions string

	// Tools are the tools this binding's ToolFilter permits, in a stable order.
	Tools []ToolSpec
	// Prompts are the prompts the server advertises.
	Prompts []PromptSpec
	// Resources are the concrete resources the server advertises.
	Resources []ResourceSpec
	// ResourceTemplates are the resource templates the server advertises.
	ResourceTemplates []ResourceTemplateSpec

	// Warnings records defects tolerated during discovery, such as a dropped
	// tool. It is bounded.
	Warnings []string
	// AppliedTolerances are the compatibility tolerances this generation
	// actually needed, in a deterministic order. It is empty for a server that
	// implements the specification faithfully — it reports what was bent, not
	// what the binding's profile would have allowed.
	AppliedTolerances []Tolerance
}

// Valid reports whether c describes an adopted generation. It is false for the
// zero Catalog — a binding that has not discovered yet, or never will.
//
// It is not the same as "has tools": a server may legitimately offer none.
func (c Catalog) Valid() bool { return c.Generation != 0 }

// ToolByRawName returns the tool the server calls rawName.
func (c Catalog) ToolByRawName(rawName string) (ToolSpec, bool) {
	for _, t := range c.Tools {
		if t.RawName == rawName {
			return t, true
		}
	}
	return ToolSpec{}, false
}

// ToolByModelName returns the tool a model knows as modelName. It is the
// reverse mapping a caller must use instead of parsing a model name.
func (c Catalog) ToolByModelName(modelName string) (ToolSpec, bool) {
	for _, t := range c.Tools {
		if t.ModelName == modelName {
			return t, true
		}
	}
	return ToolSpec{}, false
}

// Catalog returns the binding's adopted catalog.
//
// It is the model-facing projection: the tools are those the Definition's
// ToolFilter permits. See this file's header for why the filter lives here and
// not in the generation.
//
// Before a binding is ready it returns the zero Catalog, whose Valid reports
// false. Connect never returns a Client that has not adopted a generation, so a
// caller holding one from Connect always gets a real catalog.
//
// After Close it keeps returning the last adopted catalog rather than going
// blank. A catalog is an immutable snapshot of what a server offered, and that
// remains true after the connection is gone; a caller that wants to know
// whether the binding is usable asks Status, and every call path refuses a
// closed binding on its own. Blanking it would only destroy information a
// post-mortem wants.
func (c *Client) Catalog() Catalog {
	c.mu.Lock()
	gen := c.generation
	c.mu.Unlock()
	if gen == nil {
		return Catalog{}
	}
	return c.projectCatalog(gen)
}

// projectCatalog copies a generation into the public view, applying the
// ToolFilter.
func (c *Client) projectCatalog(gen *catalog.Generation) Catalog {
	out := Catalog{
		Binding:         Name(gen.Binding()),
		Generation:      gen.Number(),
		Digest:          gen.Digest().String(),
		ProtocolVersion: string(gen.ProtocolVersion()),
		Server: ServerIdentity{
			Name:    gen.Server().Name,
			Version: gen.Server().Version,
			Title:   gen.Server().Title,
		},
		Capabilities: fromProtocolCapabilities(gen.Capabilities()),
		Instructions: gen.Instructions(),
		Warnings:     gen.Warnings(),
	}
	for _, tol := range gen.AppliedTolerances() {
		if public, ok := fromCatalogTolerance(tol); ok {
			out.AppliedTolerances = append(out.AppliedTolerances, public)
		}
	}

	for _, t := range gen.Tools() {
		if !c.def.ToolFilter.Permits(t.RawName) {
			continue
		}
		out.Tools = append(out.Tools, fromCatalogTool(t))
	}
	for _, p := range gen.Prompts() {
		out.Prompts = append(out.Prompts, fromProtocolPrompt(p))
	}
	for _, r := range gen.Resources() {
		out.Resources = append(out.Resources, fromProtocolResource(r))
	}
	for _, t := range gen.ResourceTemplates() {
		out.ResourceTemplates = append(out.ResourceTemplates, fromProtocolResourceTemplate(t))
	}
	return out
}
