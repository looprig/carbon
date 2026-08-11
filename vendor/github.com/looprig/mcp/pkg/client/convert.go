// This file converts internal/protocol's neutral types onto this package's
// public ones. It is the last hop of the boundary: everything above it is a
// consumer contract, everything below it is an implementation detail.
//
// The conversions are mechanical and total. They never fail — the bounding and
// the validation already happened, below — and they copy every slice they
// carry, so a value handed to a caller shares nothing with the client's own
// state.

package client

import (
	"slices"

	"github.com/looprig/mcp/internal/catalog"
	"github.com/looprig/mcp/internal/protocol"
)

// fromProtocolContent maps one internal content item onto its public form.
//
// The default case is the one that matters: an internal kind this function does
// not know becomes Unsupported rather than nil or a panic. A nil Content in a
// result would be a caller's crash, and a panic here would be a hostile
// server's crash — so an unmapped kind degrades to the union member that exists
// for exactly that.
func fromProtocolContent(c protocol.Content) Content {
	switch v := c.(type) {
	case protocol.TextContent:
		return Text{Text: v.Text, Truncated: v.Truncated}
	case protocol.ImageContent:
		return Image{Data: slices.Clone(v.Data), MIMEType: v.MIMEType}
	case protocol.AudioContent:
		return Audio{Data: slices.Clone(v.Data), MIMEType: v.MIMEType}
	case protocol.EmbeddedResourceContent:
		return EmbeddedResource{
			URI:       v.URI,
			MIMEType:  v.MIMEType,
			Text:      v.Text,
			Data:      slices.Clone(v.Data),
			Truncated: v.Truncated,
		}
	case protocol.UnsupportedContent:
		return Unsupported{Kind: v.Kind, Bytes: v.Bytes}
	case nil:
		return Unsupported{Kind: KindUnknown}
	default:
		return Unsupported{Kind: KindUnknown}
	}
}

// fromProtocolContents maps a content list.
func fromProtocolContents(cs []protocol.Content) []Content {
	if len(cs) == 0 {
		return nil
	}
	out := make([]Content, len(cs))
	for i, c := range cs {
		out[i] = fromProtocolContent(c)
	}
	return out
}

// fromProtocolCapabilities maps the server's advertised capabilities.
func fromProtocolCapabilities(c protocol.ServerCapabilities) ServerCapabilities {
	return ServerCapabilities{
		Tools:              c.Tools,
		Prompts:            c.Prompts,
		Resources:          c.Resources,
		ResourcesSubscribe: c.ResourcesSubscribe,
		Logging:            c.Logging,
		Completions:        c.Completions,
	}
}

// fromProtocolAnnotations maps a tool's hints, preserving the tri-states.
func fromProtocolAnnotations(a *protocol.ToolAnnotations) *ToolAnnotations {
	if a == nil {
		return nil
	}
	return &ToolAnnotations{
		Title:           a.Title,
		ReadOnlyHint:    a.ReadOnlyHint,
		IdempotentHint:  a.IdempotentHint,
		DestructiveHint: copyBool(a.DestructiveHint),
		OpenWorldHint:   copyBool(a.OpenWorldHint),
	}
}

// copyBool copies a tri-state hint, preserving nil.
func copyBool(p *bool) *bool {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// fromCatalogTool maps one catalog tool onto its public form.
//
// The digests are rendered as hex strings rather than exposed as
// catalog.Digest: a digest is an opaque identity to a consumer — something to
// compare and to log — and a public array type would pin an internal
// representation into the contract for no gain.
func fromCatalogTool(t catalog.Tool) ToolSpec {
	spec := ToolSpec{
		RawName:           t.RawName,
		ModelName:         t.ModelName,
		Title:             t.Title,
		Description:       t.Description,
		InputSchema:       slices.Clone(t.InputSchema),
		OutputSchema:      slices.Clone(t.OutputSchema),
		InputSchemaDigest: t.InputSchemaDigest.String(),
		Annotations:       fromProtocolAnnotations(t.Annotations),
		Warnings:          slices.Clone(t.Warnings),
	}
	// The zero digest means "no output schema"; rendering it as 64 zeros would
	// look like a real digest.
	if !t.OutputSchemaDigest.IsZero() {
		spec.OutputSchemaDigest = t.OutputSchemaDigest.String()
	}
	return spec
}

// fromProtocolPrompt maps one advertised prompt.
func fromProtocolPrompt(p protocol.PromptSpec) PromptSpec {
	spec := PromptSpec{
		Name:        p.RawName,
		Title:       p.Title,
		Description: p.Description,
	}
	if len(p.Arguments) > 0 {
		spec.Arguments = make([]PromptArg, len(p.Arguments))
		for i, a := range p.Arguments {
			spec.Arguments[i] = PromptArg{
				Name:        a.Name,
				Title:       a.Title,
				Description: a.Description,
				Required:    a.Required,
			}
		}
	}
	return spec
}

// fromProtocolResource maps one advertised resource.
func fromProtocolResource(r protocol.ResourceSpec) ResourceSpec {
	return ResourceSpec{
		URI:         r.URI,
		Name:        r.Name,
		Title:       r.Title,
		Description: r.Description,
		MIMEType:    r.MIMEType,
	}
}

// fromProtocolResourceTemplate maps one advertised resource template.
func fromProtocolResourceTemplate(t protocol.ResourceTemplateSpec) ResourceTemplateSpec {
	return ResourceTemplateSpec{
		URITemplate: t.URITemplate,
		Name:        t.Name,
		Title:       t.Title,
		Description: t.Description,
		MIMEType:    t.MIMEType,
	}
}
