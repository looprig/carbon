// This file defines the catalog's content addressing: the Digest type and the
// canonical encoding the catalog digest is computed over.
//
// The encoding exists to make one question cheap and exact: is this catalog the
// same catalog as that one? The framing that makes it deterministic and
// unambiguous lives in internal/canonical, which every identity digest in this
// module shares; what lives here is what the framing is applied to — this
// digest's domain, its schema version, and its field order, which are the parts
// that define what a catalog digest *means*.
//
// The one property this file owns rather than borrows is determinism of
// ordering: collections are ordered canonically before they are encoded (see
// catalog.go), and no map is ever ranged over here.

package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"

	"github.com/looprig/mcp/internal/canonical"
)

// Digest is a SHA-256 digest. It is a value type, so it can be compared with
// ==, used as a map key, and copied out of an immutable Generation without a
// clone.
type Digest [sha256.Size]byte

// String renders the digest as lowercase hex.
func (d Digest) String() string { return hex.EncodeToString(d[:]) }

// IsZero reports whether d is the zero digest, which this package uses to mean
// "absent" (see Tool.OutputSchemaDigest). It is not a digest any input can
// produce in practice: a preimage for the zero digest is not known.
func (d Digest) IsZero() bool { return d == Digest{} }

// Short renders the first ShortDigestBytes bytes as hex. It is for a name
// suffix and a human-readable label — never for an equality check, which must
// always compare the whole digest.
func (d Digest) Short() string { return hex.EncodeToString(d[:ShortDigestBytes]) }

// ShortDigestBytes is how much of a digest Short renders. Four bytes (eight hex
// characters) is a disambiguation suffix, not a security boundary: it only has
// to separate a handful of tool names that already collide within one server,
// where the alternative is an arbitrary winner.
const ShortDigestBytes = 4

// DigestBytes returns the digest of a raw byte string, domain-separated from
// the catalog encoding so that a schema document can never be confused with a
// catalog that happens to contain the same bytes.
func DigestBytes(b []byte) Digest {
	h := sha256.New()
	e := canonical.NewEncoder(h)
	e.Str(domainSchema)
	e.Uint(digestSchemaVersion)
	e.Bytes(b)
	return sum(h)
}

// digestName returns the digest of a raw tool name, for the disambiguation
// suffix in identity.go. It has its own domain: a name and a schema that happen
// to share bytes must not share a digest.
func digestName(binding, rawName string) Digest {
	h := sha256.New()
	e := canonical.NewEncoder(h)
	e.Str(domainName)
	e.Uint(digestSchemaVersion)
	e.Str(binding)
	e.Str(rawName)
	return sum(h)
}

// The domain tags. Each names one kind of thing that gets digested, so that two
// different kinds can never produce the same digest from the same bytes.
const (
	domainCatalog = "looprig.mcp.catalog"
	domainSchema  = "looprig.mcp.catalog.schema"
	domainName    = "looprig.mcp.catalog.name"
)

// digestSchemaVersion is the version of the encoding below. Bump it whenever
// the encoding changes in a way that would give existing content a new digest,
// so that a digest computed by an old build is never mistaken for one computed
// by a new one.
const digestSchemaVersion = 1

// Field tags. Each is written before its value, so that adding, removing or
// reordering a field changes the encoding rather than silently re-interpreting
// a neighbour's bytes. The strings are part of the digest's definition: change
// one and every digest changes, so bump digestSchemaVersion with it.
const (
	tagBinding         = "binding"
	tagProtocolVersion = "protocol_version"
	tagCapabilities    = "capabilities"
	tagServer          = "server"
	tagInstructions    = "instructions"
	tagTools           = "tools"
	tagPrompts         = "prompts"
	tagResources       = "resources"
	tagTemplates       = "resource_templates"
	tagAnnotations     = "annotations"
	tagArguments       = "arguments"
	tagEnd             = "end"
)

// computeDigest returns the canonical catalog digest.
//
// What it covers is what makes two catalogs the same catalog: the binding, the
// negotiated protocol and capabilities, the server's identity, the instructions,
// and every advertised tool, prompt, resource and template — tools by their
// schema *digests* rather than their schema bytes, per the design's catalog
// model.
//
// Three things are deliberately excluded, each because including it would make
// the digest answer a different question than the one it is asked:
//
//   - Generation.number is an ordinal, not content. Including it would make
//     every generation differ from every other by construction, which is
//     exactly the comparison the digest exists to perform (design §Change
//     notifications step 5: compare the candidate with the current generation).
//   - Warnings and decisions are derived: they are a deterministic function of
//     the content and the capabilities, both already covered. Including them
//     could not distinguish two catalogs that the content does not already
//     distinguish. This covers a tool's own Warnings as well as the
//     generation's. Two servers can produce identical catalog content with
//     different warnings — one dropped an oversized output schema, the other
//     never sent one — but a dropped schema is absent either way, so the tools
//     in force are the same tools and a model would see no difference. The
//     warning explains the catalog; it is not part of its identity.
//   - Host policy — a ToolFilter — is not in a Generation at all (see the
//     package doc). Two hosts with different filters see the same server and
//     must agree on its digest.
//
// Schema digests rather than schema bytes means the digest is byte-level, not
// semantic: a server that re-serializes an identical schema with different key
// order produces a new digest. That is the conservative direction — a spurious
// change costs a re-validation, where a missed one would silently apply an old
// schema to a new tool.
func (g *Generation) computeDigest() Digest {
	h := sha256.New()
	e := canonical.NewEncoder(h)

	e.Str(domainCatalog)
	e.Uint(digestSchemaVersion)

	e.Field(tagBinding)
	e.Str(g.binding)

	e.Field(tagProtocolVersion)
	e.Str(string(g.protocolVersion))

	e.Field(tagCapabilities)
	c := g.capabilities
	e.Bool(c.Tools)
	e.Bool(c.Prompts)
	e.Bool(c.Resources)
	e.Bool(c.ResourcesSubscribe)
	e.Bool(c.Logging)
	e.Bool(c.Completions)

	e.Field(tagServer)
	e.Str(g.server.Name)
	e.Str(g.server.Version)
	e.Str(g.server.Title)

	e.Field(tagInstructions)
	e.Str(g.instructions)

	e.Field(tagTools)
	e.Count(len(g.tools))
	for _, t := range g.tools {
		e.Str(t.RawName)
		e.Str(t.ModelName)
		e.Str(t.Title)
		e.Str(t.Description)
		e.Bytes(t.InputSchemaDigest[:])
		// Presence is encoded explicitly rather than implied by a zero digest,
		// so that "no output schema" is a distinct encoding from any digest.
		e.Bool(!t.OutputSchemaDigest.IsZero())
		e.Bytes(t.OutputSchemaDigest[:])
		e.Field(tagAnnotations)
		encodeAnnotations(e, t)
	}

	e.Field(tagPrompts)
	e.Count(len(g.prompts))
	for _, p := range g.prompts {
		e.Str(p.RawName)
		e.Str(p.Title)
		e.Str(p.Description)
		e.Field(tagArguments)
		// Argument order is content, not incidental ordering: it comes from one
		// prompt object the server sent, so a server that reorders its
		// arguments has changed the prompt. It is encoded as sent.
		e.Count(len(p.Arguments))
		for _, a := range p.Arguments {
			e.Str(a.Name)
			e.Str(a.Title)
			e.Str(a.Description)
			e.Bool(a.Required)
		}
	}

	e.Field(tagResources)
	e.Count(len(g.resources))
	for _, r := range g.resources {
		e.Str(r.URI)
		e.Str(r.Name)
		e.Str(r.Title)
		e.Str(r.Description)
		e.Str(r.MIMEType)
	}

	e.Field(tagTemplates)
	e.Count(len(g.templates))
	for _, t := range g.templates {
		e.Str(t.URITemplate)
		e.Str(t.Name)
		e.Str(t.Title)
		e.Str(t.Description)
		e.Str(t.MIMEType)
	}

	e.Field(tagEnd)
	return sum(h)
}

// encodeAnnotations encodes a tool's hints. A nil Annotations is encoded as a
// distinct absence rather than as an all-false struct: "the server said
// nothing" and "the server said no to everything" are different claims.
func encodeAnnotations(e *canonical.Encoder, t Tool) {
	if t.Annotations == nil {
		e.Bool(false)
		return
	}
	e.Bool(true)
	a := t.Annotations
	e.Str(a.Title)
	e.Bool(a.ReadOnlyHint)
	e.Bool(a.IdempotentHint)
	e.TriState(a.DestructiveHint)
	e.TriState(a.OpenWorldHint)
}

// sum finalizes a hash into a Digest.
func sum(h hash.Hash) Digest {
	var d Digest
	// Sum appends to its argument; a zero-length slice of the array's own
	// backing store writes the digest straight into d.
	h.Sum(d[:0])
	return d
}
