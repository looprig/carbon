// Package protocol is the module-internal boundary between transports and the
// client. It is the only package (besides pkg/transport/*) allowed to import
// the MCP go-sdk.
//
// Its job is to convert SDK values into the module-neutral types declared
// here, so that no SDK type ever reaches a pkg/... exported API. Everything
// crossing this boundary comes from an untrusted server: conversions bound it
// (see Bounds) before retaining it, copy every slice and map they keep, and
// never panic on a malformed or nil input.
//
// This package must not import pkg/client: the client maps its own Limits into
// the narrow Bounds view below and passes it down.
package protocol

import (
	"context"
	"encoding/json"
)

// Conn is an established connection to an MCP server. A Conn is transport-
// agnostic: everything it returns has already been converted to the neutral,
// bounded types in this package, so no caller above it ever names an SDK type.
//
// A Conn is single-use for Initialize: the MCP handshake happens once per
// connection. Close is idempotent from the caller's point of view — the client
// guarantees it calls it at most once — and must never panic on a connection
// that was never initialized.
// A Conn's request methods must only be called between a successful Initialize
// and Close; they report an error otherwise rather than panicking. None of them
// checks whether the server advertised the capability behind the method — that
// is the caller's decision (see the design's compatibility rule) and this
// interface would have to guess at a policy to enforce it.
type Conn interface {
	// Initialize performs the MCP handshake and reports what the server said
	// about itself. Everything in the result is server-supplied and untrusted;
	// implementations bound it against the ConnectConfig Bounds before
	// returning it.
	Initialize(ctx context.Context) (InitializeResult, error)

	// ListTools fetches one page of tools. cursor is empty for the first page,
	// and otherwise the preceding page's NextCursor. Paginating — and bounding
	// the pagination — is the caller's job; see internal/catalog.
	ListTools(ctx context.Context, cursor string) (ToolPage, error)
	// ListPrompts fetches one page of prompts.
	ListPrompts(ctx context.Context, cursor string) (PromptPage, error)
	// ListResources fetches one page of concrete resources.
	ListResources(ctx context.Context, cursor string) (ResourcePage, error)
	// ListResourceTemplates fetches one page of resource templates.
	ListResourceTemplates(ctx context.Context, cursor string) (ResourceTemplatePage, error)

	// CallTool invokes a tool by its raw server name. args is passed through
	// verbatim; validating it against the tool's schema is the caller's job.
	// Cancelling ctx cancels the call at the protocol level.
	CallTool(ctx context.Context, rawName string, args json.RawMessage, opts CallOptions) (ToolResult, error)
	// GetPrompt fetches a prompt's messages.
	GetPrompt(ctx context.Context, name string, args map[string]string) (PromptResult, error)
	// ReadResource reads a resource by its opaque URI.
	ReadResource(ctx context.Context, uri string) (ResourceResult, error)
	// Subscribe asks the server to report changes to a resource.
	Subscribe(ctx context.Context, uri string) error
	// Unsubscribe asks the server to stop reporting changes to a resource.
	Unsubscribe(ctx context.Context, uri string) error
	// SetLogLevel asks the server to send logs at or above level. A server
	// sends none until this is called.
	SetLogLevel(ctx context.Context, level string) error

	// Close releases the connection's resources.
	Close(ctx context.Context) error
}

// ClientIdentity is what this client tells a server it is. It is cosmetic and
// carries no authority; it must never carry a credential.
type ClientIdentity struct {
	Name    string
	Version string
	Title   string
}

// ClientCapabilities are the optional client-side capabilities to advertise on
// a connection. The client only sets a field when the application both asked
// for the capability and installed a handler able to serve it, so a transport
// may advertise these verbatim.
type ClientCapabilities struct {
	Elicitation bool
	Sampling    bool
	Roots       bool
}

// ConnectConfig carries the client-side connection parameters a transport
// needs. It is secret-free by construction: credentials reach a transport
// through its own configuration, never through this struct.
type ConnectConfig struct {
	// Client identifies this client to the server.
	Client ClientIdentity
	// Capabilities are the client capabilities to advertise.
	Capabilities ClientCapabilities
	// Bounds caps everything the connection converts from server data. The
	// client passes a normalized (all-positive) value.
	Bounds Bounds
	// Wire caps what a transport buffers off the network, before any of it is
	// parsed. The client passes a normalized (all-positive) value.
	Wire WireLimits
	// OnLog receives the server's log messages, already bounded. Nil drops
	// them.
	//
	// It is a callback on the config rather than a method on Conn because a log
	// is unsolicited: it belongs to no request, so there is nothing to return
	// it from. It is invoked on the connection's notification goroutine and
	// blocks it, so an implementation must not do work.
	OnLog func(LogRecord)

	// OnListChanged receives the server's list-change notifications. Nil drops
	// them.
	//
	// Like OnLog it is a callback rather than a method for want of a request to
	// return it from, and it is invoked on the connection's notification
	// goroutine and blocks it: an implementation must record the change and
	// return, never fetch.
	OnListChanged func(ListChange)

	// OnResourceUpdated receives the server's resource-update notifications: a
	// server telling a subscriber that a resource it subscribed to has changed.
	// Nil drops them.
	//
	// Like OnListChanged it is a callback for want of a request to return it
	// from, and it is invoked on the connection's notification goroutine and
	// blocks it: an implementation must record the update and return, never
	// re-read the resource here. The URI it carries is server-supplied and
	// bounded before it arrives.
	OnResourceUpdated func(ResourceUpdate)

	// OnElicit serves the server's requests for human input, already bounded
	// and validated. Nil means no elicitation is served — and, because a
	// capability with nothing behind it must never reach the wire, nil also
	// means the elicitation capability is not advertised however Capabilities
	// is set (see Session.Initialize).
	//
	// Unlike OnLog and OnListChanged this one answers: it is the only callback
	// here whose return value goes to the server, and the only one that may
	// block. It is invoked on the connection's request-dispatch goroutine with
	// the caller's elicitation deadline already on ctx; returning an error
	// refuses the request, which is a complete and honest answer.
	//
	// It is a callback rather than a Conn method for the same reason as the
	// others: it belongs to no request this module made, so there is nothing to
	// return it from.
	OnElicit func(context.Context, ElicitRequest) (ElicitResult, error)

	// OnSample serves the server's requests for an LLM completion, already
	// bounded and validated. Nil means no sampling is served — and, because a
	// capability with nothing behind it must never reach the wire, nil also
	// means the sampling capability is not advertised however Capabilities is
	// set (see Session.Initialize).
	//
	// Like OnElicit it answers, and its return value goes to the server. It is
	// invoked on the connection's request-dispatch goroutine with the caller's
	// deadline already on ctx; returning an error refuses the request, which a
	// host is always entitled to do — sampling spends the host's money, and
	// "no" is a complete answer.
	OnSample func(context.Context, SampleRequest) (SampleResult, error)

	// OnRoots supplies the filesystem roots this client exposes to the server.
	// Nil means no roots are served — and, because a capability with nothing
	// behind it must never reach the wire, nil also means the roots capability
	// is not advertised however Capabilities is set (see Session.Initialize).
	//
	// Unlike the other callbacks it is not a server request handler: the SDK
	// answers roots/list from a set the client supplies, so this is consulted
	// once, at Initialize, to populate that set. The roots it returns are the
	// only ones a server ever learns — this module never invents host
	// filesystem roots of its own. Returning an error fails the handshake:
	// establishing a binding that advertises roots it cannot determine would be
	// advertise-without-honor, the very thing the gating exists to prevent.
	OnRoots func(context.Context) ([]Root, error)
}

// Root is a filesystem root a client exposes to a server, neutral and bounded.
// Exposing one grants a server knowledge of a path, never access to it. URI is
// the root's canonical identity (a file:// URI); Name is an optional display
// name.
type Root struct {
	URI  string
	Name string
}

// MaxRoots caps how many roots one client will advertise, so a misbehaving
// provider cannot hand an unbounded set to the SDK. A root without a URI has no
// identity and is dropped rather than sent; the cap bounds what remains.
const MaxRoots = 64

// ListFamily names the catalog family a list-change notification refers to. It
// is this package's own enum rather than internal/catalog's Family because
// catalog imports protocol, not the other way round; the client maps between
// them.
type ListFamily uint8

// The families a server can announce a change to. MCP defines one notification
// per family; there is none for resource templates, which travel with
// resources.
const (
	// ListFamilyTools is notifications/tools/list_changed.
	ListFamilyTools ListFamily = iota + 1
	// ListFamilyPrompts is notifications/prompts/list_changed.
	ListFamilyPrompts
	// ListFamilyResources is notifications/resources/list_changed.
	ListFamilyResources

	listFamilySentinel // must remain last; tests derive the declared range from it
)

// listFamilyNames maps each family to its stable lowercase identifier. These
// reach events and telemetry, so they must not change.
var listFamilyNames = [listFamilySentinel]string{
	ListFamilyTools:     "tools",
	ListFamilyPrompts:   "prompts",
	ListFamilyResources: "resources",
}

// String returns the family's stable identifier, or "unknown" for any value
// outside the declared range.
func (f ListFamily) String() string {
	if f < ListFamilyTools || f >= listFamilySentinel {
		return "unknown"
	}
	return listFamilyNames[f]
}

// ResourceUpdate is a server's announcement that a subscribed resource changed.
//
// It carries only the URI of the resource the server says changed, bounded
// before it reaches this type. Like a list-change notification it is a claim,
// not content: the resource's new value is not here, and a subscriber that wants
// it re-reads the resource, which is the only way to learn what changed from an
// untrusted peer rather than trusting its account of the delta.
type ResourceUpdate struct {
	// URI names the resource the server says changed. It may be a sub-resource
	// of the one that was subscribed to.
	URI string
}

// ListChange is a server's announcement that a catalog family changed.
//
// It carries no content: MCP's list-change notifications say only that a list
// changed, never how. That is a property worth keeping rather than papering
// over — a client that acted on a server's account of its own delta would be
// trusting an untrusted peer's diff. The only safe response is to refetch the
// family and compare, which is what internal/catalog and the client do.
type ListChange struct {
	// Family names the list the server says changed.
	Family ListFamily
}

// WireLimits bounds untrusted bytes at the point they arrive, which is a
// different job from Bounds and is why it is a different type. Bounds governs
// conversion — how much of a *decoded* value this module will retain — and can
// only be applied to something already in memory. WireLimits governs what may
// reach memory at all, so it is the only thing standing between a hostile
// server and an unbounded allocation.
//
// It is separate from Bounds rather than folded into it because only a
// byte-oriented transport can enforce it: there is nothing for a converter to
// do with MaxBodyBytes.
type WireLimits struct {
	// MaxBodyBytes caps one whole non-streaming response body.
	//
	// It deliberately does not cap a long-lived stream: an SSE stream carries
	// an entire session's worth of frames and is bounded per frame, by
	// MaxFrameBytes, because a total on it is just a slow session's expiry
	// date.
	MaxBodyBytes int
	// MaxFrameBytes caps one wire frame — one JSON-RPC message, or one SSE
	// event — however long the stream carrying it lives.
	MaxFrameBytes int
}

// ServerCapabilities is what a server advertised at initialize, reduced to the
// presence flags this module acts on. The SDK models each capability as a
// nillable struct whose only members are listChanged-style notification flags;
// those are not modelled here until a task consumes them.
type ServerCapabilities struct {
	// Tools reports whether the server exposes tools.
	Tools bool
	// Prompts reports whether the server exposes prompts.
	Prompts bool
	// Resources reports whether the server exposes resources.
	Resources bool
	// ResourcesSubscribe reports whether the server supports subscribing to
	// resource updates. It is only meaningful when Resources is set.
	ResourcesSubscribe bool
	// Logging reports whether the server sends log messages.
	Logging bool
	// Completions reports whether the server supports argument autocompletion.
	Completions bool
}

// InitializeResult is the outcome of the MCP handshake, bounded and detached
// from SDK memory. Every field is server-supplied: it describes a peer, it
// never authorizes one.
type InitializeResult struct {
	// Server is what the server claims to be.
	Server ServerIdentity
	// ProtocolVersion is the version the server wants to speak.
	ProtocolVersion ProtocolVersion
	// Instructions is the server's usage hint, truncated to
	// Bounds.MaxTextBytes.
	Instructions string
	// Capabilities is what the server advertised.
	Capabilities ServerCapabilities
}

// ProtocolVersion is an MCP protocol version string as negotiated during
// initialize (e.g. "2025-06-18"). It is server-supplied and untrusted.
type ProtocolVersion string

// statelessSince is the first spec revision with the stateless wire model
// (SEP-2575): no initialize handshake, no resumability, MRTR in place of
// server-initiated requests.
const statelessSince = "2026-07-28"

// Stateless reports whether this revision speaks the stateless wire model.
// Version strings are ISO dates, so lexical comparison is date comparison. A
// value that is not date-shaped is treated as legacy: the string is
// server-supplied and untrusted, and the legacy path is the conservative one.
func (v ProtocolVersion) Stateless() bool {
	if len(v) != len(statelessSince) {
		return false
	}
	for i, r := range v {
		if i == 4 || i == 7 {
			if r != '-' {
				return false
			}
			continue
		}
		if r < '0' || r > '9' {
			return false
		}
	}
	return string(v) >= statelessSince
}

// ServerIdentity is what a server claims to be. Every field is server-supplied
// and cosmetic: it names a peer, it never authorizes one.
type ServerIdentity struct {
	Name    string
	Version string
	Title   string
}

// Bounds is the narrow limit view this package needs — the subset of the
// client's Limits that applies to converting SDK values. The client passes a
// normalized (all-positive) value; a non-positive field is not "unbounded" but
// fails closed, rejecting or truncating everything it governs.
type Bounds struct {
	// MaxSchemaBytes caps one marshaled tool schema document.
	MaxSchemaBytes int
	// MaxSchemaDepth caps schema nesting.
	MaxSchemaDepth int
	// MaxTextBytes caps one text (or embedded-resource text) payload before
	// it is truncated.
	MaxTextBytes int
	// MaxStructuredBytes caps one structured-content document, enforced by
	// FromSDKCallToolResult.
	MaxStructuredBytes int
	// MaxBinaryItemBytes caps one binary (image/audio/blob) item.
	MaxBinaryItemBytes int
	// MaxBinaryItems caps how many binary items one content list may carry.
	MaxBinaryItems int
	// MaxLogBytes caps one server log message's text.
	MaxLogBytes int
	// MaxElicitMessageBytes caps one elicitation prompt: its message, and in
	// url mode the URL and correlation id shown with it. It is separate from
	// MaxTextBytes because it bounds what a *person* is shown, not what a model
	// consumes — a tool result may reasonably be a megabyte; a prompt may not.
	MaxElicitMessageBytes int
	// MaxElicitSchemaBytes caps one elicitation's requested schema. It is
	// separate from MaxSchemaBytes because a form a human fills in is a
	// different thing from a tool's interface; sizing them together would mean
	// raising one to raise the other.
	MaxElicitSchemaBytes int
}

// MaxWarnings caps the Warnings a single conversion may report, so a hostile
// server cannot turn tolerated defects into unbounded memory.
//
// Per-item messages beyond the cap are discarded, but the drops they described
// are not hidden: convertItems spends the last slot on a summary carrying the
// true total. The cap bounds how much is said, never whether it is said.
const MaxWarnings = 8

// MaxPromptArgs caps how many arguments one prompt may declare.
//
// A prompt's argument list is server-chosen and retained in a catalog for the
// life of a connection, so it needs a ceiling for the same reason the catalog
// itself does. The number is set where a prompt stops being one a person could
// fill in: past this, the server is not describing a prompt.
const MaxPromptArgs = 64

// ToolSpec is a tool as advertised by a server, bounded and detached from SDK
// memory. RawName is exactly what the server sent — unvalidated, unnamespaced,
// and not safe to use as an identifier until the client has qualified it.
type ToolSpec struct {
	RawName      string
	Title        string
	Description  string
	InputSchema  json.RawMessage
	OutputSchema json.RawMessage
	// Annotations are server-declared behavioural hints. They are untrusted
	// policy *input* and never authority: a server claiming ReadOnlyHint does
	// not make a tool read-only. Nil when the server sent none.
	Annotations *ToolAnnotations
	// Warnings records defects tolerated during conversion (e.g. a dropped
	// output schema). Bounded by MaxWarnings.
	Warnings []string
	// OutputSchemaDefect is the bounded reason this tool's optional output
	// schema was dropped, or empty when there was nothing wrong with it (which
	// includes the ordinary case of a server sending none at all).
	//
	// It exists so that dropping the schema is a *decision* the layer above can
	// make rather than one this conversion makes for it. The drop is the safe
	// tolerance, but whether it is applied is compatibility policy — a strict
	// profile rejects the tool instead — and policy cannot be expressed by
	// pattern-matching the text of a warning.
	OutputSchemaDefect string
}

// ToolAnnotations mirrors the server's behavioural hints. The tri-state hints
// are pointers because "unspecified" differs from "false": absent means the
// server said nothing, and the caller applies its own default.
type ToolAnnotations struct {
	Title           string
	ReadOnlyHint    bool
	IdempotentHint  bool
	DestructiveHint *bool
	OpenWorldHint   *bool
}

// copyBool returns a copy of a tri-state hint, preserving nil.
func copyBool(p *bool) *bool {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// PromptSpec is a prompt as advertised by a server. RawName carries the same
// caveat as ToolSpec.RawName.
type PromptSpec struct {
	RawName     string
	Title       string
	Description string
	Arguments   []PromptArgSpec
}

// PromptArgSpec is one argument a prompt accepts.
type PromptArgSpec struct {
	Name        string
	Title       string
	Description string
	Required    bool
}

// ResourceSpec is a concrete resource a server exposes.
type ResourceSpec struct {
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

// Content is a sealed union of the neutral content kinds. Only this package
// can add a member; callers exhaust it with a type switch and must handle
// UnsupportedContent, which is the default for anything else.
type Content interface {
	content()
}

// TextContent is a text payload, truncated to Bounds.MaxTextBytes. Truncation
// is normal, not an error: Truncated says whether it happened.
type TextContent struct {
	Text      string
	Truncated bool
}

// ImageContent is an image payload within Bounds.MaxBinaryItemBytes.
type ImageContent struct {
	Data     []byte
	MIMEType string
}

// AudioContent is an audio payload within Bounds.MaxBinaryItemBytes.
type AudioContent struct {
	Data     []byte
	MIMEType string
}

// EmbeddedResourceContent is resource content inlined in a result. Exactly one
// of Text or Data is meaningful, per what the server sent.
type EmbeddedResourceContent struct {
	URI       string
	MIMEType  string
	Text      string
	Data      []byte
	Truncated bool
}

// UnsupportedContent stands in for content this boundary will not retain: a
// kind the module does not model, or a payload over a bound. It records only
// bounded metadata, so an unusable item is always visible to the caller rather
// than silently disappearing.
type UnsupportedContent struct {
	// Kind is one of the Kind* constants.
	Kind string
	// Bytes is the size of the payload that was refused. It is exact for an
	// item dropped on a size bound (image, audio, embedded blob), and a
	// diagnostic lower bound for a kind dropped as unsupported, whose size is
	// summed from its known fields rather than by marshalling data we are
	// refusing. Treat it as a diagnostic, never as an accounting figure.
	Bytes int
}

// Values for UnsupportedContent.Kind. They mirror the MCP wire "type"
// discriminator, with KindUnknown for a content type this SDK version returns
// but this module does not know.
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

func (TextContent) content()             {}
func (ImageContent) content()            {}
func (AudioContent) content()            {}
func (EmbeddedResourceContent) content() {}
func (UnsupportedContent) content()      {}
