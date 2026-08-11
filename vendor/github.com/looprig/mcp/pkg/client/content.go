// This file defines the public content union — what a tool result, a prompt
// message or a resource read is made of — and converts the internal one onto
// it.
//
// It is a mirror of internal/protocol's union rather than a re-export because
// of the module's central rule: no consumer may be forced to name an SDK type,
// and internal/... is not nameable from outside the module at all. The mirror
// is what makes the boundary real instead of nominal.
//
// Every value here has already been bounded by the conversion that produced it.
// It is still external data: a server said it, so it is content to render, not
// an instruction to follow.

package client

// Content is a sealed union of the content kinds this client produces. Only
// this package can add a member, so a caller may exhaust it with a type switch
// — but it must still handle Unsupported, and should keep a default case:
// Unsupported is what a kind this module does not model becomes, and it exists
// precisely so that a new one is a case to handle rather than a panic.
type Content interface {
	content()
}

// Text is a text payload. Truncated reports whether it was cut at
// Limits.MaxTextResultBytes; truncation is normal, not an error.
type Text struct {
	Text      string
	Truncated bool
}

// Image is an image payload within Limits.MaxBinaryItemBytes.
type Image struct {
	Data     []byte
	MIMEType string
}

// Audio is an audio payload within Limits.MaxBinaryItemBytes.
type Audio struct {
	Data     []byte
	MIMEType string
}

// EmbeddedResource is resource content inlined in a result. Exactly one of Text
// or Data is meaningful, per what the server sent.
type EmbeddedResource struct {
	// URI is the resource's opaque protocol identifier. It is not a host path
	// and must never be resolved as one.
	URI       string
	MIMEType  string
	Text      string
	Data      []byte
	Truncated bool
}

// Unsupported stands in for content this client will not retain: a kind it does
// not model, or a payload over a bound.
//
// It carries only bounded metadata, which is the point: the design requires
// that unknown or oversized content becomes bounded opaque metadata rather than
// being injected or silently disappearing. An item a caller cannot use is
// always still visible as an item.
type Unsupported struct {
	// Kind is the content kind that was refused, one of the Kind* constants.
	Kind string
	// Bytes is the size of the refused payload. It is exact for an item dropped
	// on a size bound and a diagnostic lower bound for a kind dropped as
	// unsupported. Treat it as a diagnostic, never as an accounting figure.
	Bytes int
}

// Values for Unsupported.Kind. They mirror the MCP wire "type" discriminator,
// with KindUnknown for a content type this module does not model.
const (
	KindText         = "text"
	KindImage        = "image"
	KindAudio        = "audio"
	KindResource     = "resource"
	KindResourceLink = "resource_link"
	KindToolUse      = "tool_use"
	KindToolResult   = "tool_result"
	KindUnknown      = "unknown"
)

func (Text) content()             {}
func (Image) content()            {}
func (Audio) content()            {}
func (EmbeddedResource) content() {}
func (Unsupported) content()      {}
