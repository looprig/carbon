// This file holds the SDK -> neutral conversions. Every function here treats
// its input as hostile: nil-checked, bounded before retention, and deep-copied
// so no neutral value ever aliases SDK memory.

package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/looprig/mcp/internal/limits"
)

// WhatSchemaBytes is the limits.OverLimitError.What reported when a schema
// document exceeds Bounds.MaxSchemaBytes.
const WhatSchemaBytes = "schema_bytes"

// errNilInput is the class of "the server sent nothing where the protocol
// requires something". Callers classify with errors.Is.
var errNilInput = errors.New("protocol: nil input")

// isNilContent reports whether c is nil or a non-nil interface holding a nil
// pointer. The latter cannot be caught by `c == nil`, and every mcp.Content
// implementation has pointer-receiver methods that would dereference it.
func isNilContent(c mcp.Content) bool {
	if c == nil {
		return true
	}
	v := reflect.ValueOf(c)
	return v.Kind() == reflect.Ptr && v.IsNil()
}

// FromSDKTool converts an SDK tool, enforcing the schema bounds before
// retaining anything.
//
// The two schemas are deliberately asymmetric, because only one of them is
// load-bearing. An input schema constrains the arguments a model may send, so
// a missing, malformed, non-object, or over-bound one is an error: the tool is
// rejected rather than exposed with unconstrained arguments (widening a
// schema is never a safe fallback).
//
// An output schema is optional and only describes what comes back, so any
// defect in one — malformed, non-object, or over a bound — is tolerated
// identically: the schema is dropped, OutputSchema is left nil, and the
// reason is reported in Warnings. Failing the tool instead would let a server
// make an otherwise-usable tool unavailable by padding an optional field,
// while protecting nothing that dropping does not already protect.
func FromSDKTool(t *mcp.Tool, b Bounds) (ToolSpec, error) {
	if t == nil {
		return ToolSpec{}, fmt.Errorf("%w: tool", errNilInput)
	}
	if t.Name == "" {
		return ToolSpec{}, errors.New("protocol: tool has no name")
	}

	spec := ToolSpec{
		RawName:     t.Name,
		Title:       t.Title,
		Description: t.Description,
		Annotations: fromSDKToolAnnotations(t.Annotations),
	}

	in, err := marshalSchema(t.InputSchema)
	if err != nil {
		return ToolSpec{}, fmt.Errorf("protocol: tool %q input schema: %w", t.Name, err)
	}
	if err := checkSchemaBounds(in, b); err != nil {
		return ToolSpec{}, fmt.Errorf("protocol: tool %q input schema: %w", t.Name, err)
	}
	spec.InputSchema = in

	// An absent output schema is the norm, not a defect: no warning.
	if !isAbsentSchema(t.OutputSchema) {
		out, err := marshalSchema(t.OutputSchema)
		if err == nil {
			err = checkSchemaBounds(out, b)
		}
		if err != nil {
			// Dropping already achieves what the bound exists for — the
			// oversized document is not retained — so there is nothing left
			// to protect by failing the tool as well.
			//
			// Recorded structurally as well as in the warning: whether the drop
			// is tolerated at all is the caller's compatibility policy (see
			// OutputSchemaDefect), and a policy must not be made to read
			// English.
			spec.OutputSchemaDefect = boundDefect(err.Error())
			spec.warn(fmt.Sprintf("output schema dropped: %v", err))
		} else {
			spec.OutputSchema = out
		}
	}
	return spec, nil
}

// MaxDefectBytes bounds a recorded defect reason. The text comes from an error
// this package produced, but it can quote a server's own bytes (a JSON decoder
// naming the offending token), so it is bounded like anything else that came
// from a peer.
const MaxDefectBytes = 256

// boundDefect truncates a defect reason to MaxDefectBytes.
func boundDefect(s string) string {
	out, _ := limits.TruncateText(s, MaxDefectBytes)
	return out
}

// warn appends a bounded warning, dropping anything past MaxWarnings.
func (s *ToolSpec) warn(msg string) {
	if len(s.Warnings) >= MaxWarnings {
		return
	}
	s.Warnings = append(s.Warnings, msg)
}

// isAbsentSchema reports whether the SDK's `any` schema field carries nothing
// at all. The SDK types both schema fields as `any`: a server that omitted the
// field leaves it nil, and one that sent JSON null leaves a RawMessage of
// "null". Neither is a schema.
func isAbsentSchema(schema any) bool {
	if schema == nil {
		return true
	}
	raw, ok := schema.(json.RawMessage)
	return ok && bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// marshalSchema renders the SDK's `any`-typed schema field to raw JSON and
// verifies it is a JSON object, as MCP requires. The result is freshly
// allocated by json.Marshal and so never aliases SDK memory.
func marshalSchema(schema any) (json.RawMessage, error) {
	if isAbsentSchema(schema) {
		return nil, errors.New("missing")
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		// A json.RawMessage holding malformed bytes lands here, as does any
		// value that cannot be represented as JSON.
		return nil, fmt.Errorf("not marshalable: %w", err)
	}
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, errors.New("not a JSON object")
	}
	return raw, nil
}

// checkSchemaBounds enforces the byte and depth bounds on an already-marshaled
// schema.
func checkSchemaBounds(raw json.RawMessage, b Bounds) error {
	if len(raw) > b.MaxSchemaBytes {
		return fmt.Errorf("%d bytes: %w", len(raw),
			&limits.OverLimitError{What: WhatSchemaBytes, Limit: b.MaxSchemaBytes})
	}
	if err := limits.CheckJSONDepth(raw, b.MaxSchemaDepth); err != nil {
		return err
	}
	return nil
}

func fromSDKToolAnnotations(a *mcp.ToolAnnotations) *ToolAnnotations {
	if a == nil {
		return nil
	}
	// The pointer hints are copied, never aliased: the SDK value stays the
	// SDK's to mutate.
	return &ToolAnnotations{
		Title:           a.Title,
		ReadOnlyHint:    a.ReadOnlyHint,
		IdempotentHint:  a.IdempotentHint,
		DestructiveHint: copyBool(a.DestructiveHint),
		OpenWorldHint:   copyBool(a.OpenWorldHint),
	}
}

// FromSDKPrompt converts an SDK prompt, copying the argument list.
//
// Identifiers are bounded by REJECTION and prose by TRUNCATION, and the split is
// deliberate. A prompt's name and its arguments' names are used to address the
// thing later; a truncated identifier is a different identifier that still looks
// like one, so an over-long name drops the prompt (with a warning, via
// convertItems) rather than silently becoming a name the server never chose.
// Title and Description are cosmetic — they reach logs and UIs and nothing
// addresses them — so they truncate, for the same reason FromSDKServerIdentity's
// do: padding is not a reason to refuse an otherwise-working prompt.
//
// The argument COUNT is bounded too. Bounding each string while accepting a
// million of them would be a bound in name only.
func FromSDKPrompt(p *mcp.Prompt, b Bounds) (PromptSpec, error) {
	if p == nil {
		return PromptSpec{}, fmt.Errorf("%w: prompt", errNilInput)
	}
	if p.Name == "" {
		return PromptSpec{}, errors.New("protocol: prompt has no name")
	}
	if len(p.Name) > b.MaxTextBytes {
		return PromptSpec{}, fmt.Errorf("protocol: prompt name is %d bytes, max %d", len(p.Name), b.MaxTextBytes)
	}
	if len(p.Arguments) > MaxPromptArgs {
		return PromptSpec{}, fmt.Errorf("protocol: prompt %q declares %d arguments, max %d",
			p.Name, len(p.Arguments), MaxPromptArgs)
	}
	title, _ := limits.TruncateText(p.Title, b.MaxTextBytes)
	description, _ := limits.TruncateText(p.Description, b.MaxTextBytes)
	spec := PromptSpec{
		RawName:     p.Name,
		Title:       title,
		Description: description,
	}
	if len(p.Arguments) > 0 {
		spec.Arguments = make([]PromptArgSpec, 0, len(p.Arguments))
		for i, a := range p.Arguments {
			if a == nil {
				return PromptSpec{}, fmt.Errorf("%w: prompt %q argument %d", errNilInput, p.Name, i)
			}
			if a.Name == "" {
				return PromptSpec{}, fmt.Errorf("protocol: prompt %q argument %d has no name", p.Name, i)
			}
			if len(a.Name) > b.MaxTextBytes {
				return PromptSpec{}, fmt.Errorf("protocol: prompt %q argument %d name is %d bytes, max %d",
					p.Name, i, len(a.Name), b.MaxTextBytes)
			}
			argTitle, _ := limits.TruncateText(a.Title, b.MaxTextBytes)
			argDescription, _ := limits.TruncateText(a.Description, b.MaxTextBytes)
			spec.Arguments = append(spec.Arguments, PromptArgSpec{
				Name:        a.Name,
				Title:       argTitle,
				Description: argDescription,
				Required:    a.Required,
			})
		}
	}
	return spec, nil
}

// FromSDKResource converts an SDK resource. A resource without a URI cannot be
// read and is rejected.
//
// Bounds are applied the same way as FromSDKPrompt's: the URI is the address a
// later read is issued to, so an over-long one is rejected rather than truncated
// into a URI naming something else. Name and MIMEType are rejected on the same
// grounds — both are matched against, not merely shown. Title and Description
// are prose and truncate.
func FromSDKResource(r *mcp.Resource, b Bounds) (ResourceSpec, error) {
	if r == nil {
		return ResourceSpec{}, fmt.Errorf("%w: resource", errNilInput)
	}
	if r.URI == "" {
		return ResourceSpec{}, errors.New("protocol: resource has no URI")
	}
	if err := checkIdentifiers(b, "resource", identifier{"URI", r.URI}, identifier{"name", r.Name}, identifier{"MIME type", r.MIMEType}); err != nil {
		return ResourceSpec{}, err
	}
	title, _ := limits.TruncateText(r.Title, b.MaxTextBytes)
	description, _ := limits.TruncateText(r.Description, b.MaxTextBytes)
	return ResourceSpec{
		URI:         r.URI,
		Name:        r.Name,
		Title:       title,
		Description: description,
		MIMEType:    r.MIMEType,
	}, nil
}

// identifier is one addressable server-chosen string and the word for it in an
// error.
type identifier struct {
	what  string
	value string
}

// checkIdentifiers rejects any identifier over b.MaxTextBytes.
//
// It exists so the resource converters state the rule once. An identifier is
// bounded by refusal, never truncation: see FromSDKPrompt for why.
func checkIdentifiers(b Bounds, kind string, ids ...identifier) error {
	for _, id := range ids {
		if len(id.value) > b.MaxTextBytes {
			return fmt.Errorf("protocol: %s %s is %d bytes, max %d", kind, id.what, len(id.value), b.MaxTextBytes)
		}
	}
	return nil
}

// FromSDKResourceTemplate converts an SDK resource template. The URI template
// is carried verbatim; expanding it safely is the caller's problem.
//
// Bounded exactly as FromSDKResource is, and the template is an identifier for
// the same reason a URI is — it is expanded into one.
func FromSDKResourceTemplate(rt *mcp.ResourceTemplate, b Bounds) (ResourceTemplateSpec, error) {
	if rt == nil {
		return ResourceTemplateSpec{}, fmt.Errorf("%w: resource template", errNilInput)
	}
	if rt.URITemplate == "" {
		return ResourceTemplateSpec{}, errors.New("protocol: resource template has no URI template")
	}
	if err := checkIdentifiers(b, "resource template",
		identifier{"URI template", rt.URITemplate}, identifier{"name", rt.Name}, identifier{"MIME type", rt.MIMEType}); err != nil {
		return ResourceTemplateSpec{}, err
	}
	title, _ := limits.TruncateText(rt.Title, b.MaxTextBytes)
	description, _ := limits.TruncateText(rt.Description, b.MaxTextBytes)
	return ResourceTemplateSpec{
		URITemplate: rt.URITemplate,
		Name:        rt.Name,
		Title:       title,
		Description: description,
		MIMEType:    rt.MIMEType,
	}, nil
}

// FromSDKContent converts one content item.
//
// A content kind this module does not model — resource links and the
// sampling-only tool_use/tool_result kinds today, anything new the SDK grows
// tomorrow — converts to UnsupportedContent rather than an error: one exotic
// item must not fail a whole result, and it must not vanish silently either.
// Errors are reserved for content that is structurally broken (nil, or an
// embedded resource with no contents).
//
// Binary items over Bounds.MaxBinaryItemBytes also become UnsupportedContent,
// carrying the size that was refused. The oversized bytes are never retained.
func FromSDKContent(c mcp.Content, b Bounds) (Content, error) {
	// Every SDK content kind is a pointer type whose methods dereference the
	// receiver, so a typed nil is checked once here rather than per case: the
	// kinds that fall through to unsupported() would otherwise panic while
	// measuring one, as would any kind a future SDK adds.
	if isNilContent(c) {
		return nil, fmt.Errorf("%w: content", errNilInput)
	}
	switch v := c.(type) {
	case *mcp.TextContent:
		text, truncated := limits.TruncateText(v.Text, b.MaxTextBytes)
		return TextContent{Text: text, Truncated: truncated}, nil

	case *mcp.ImageContent:
		if len(v.Data) > b.MaxBinaryItemBytes {
			return UnsupportedContent{Kind: KindImage, Bytes: len(v.Data)}, nil
		}
		return ImageContent{Data: bytes.Clone(v.Data), MIMEType: v.MIMEType}, nil

	case *mcp.AudioContent:
		if len(v.Data) > b.MaxBinaryItemBytes {
			return UnsupportedContent{Kind: KindAudio, Bytes: len(v.Data)}, nil
		}
		return AudioContent{Data: bytes.Clone(v.Data), MIMEType: v.MIMEType}, nil

	case *mcp.EmbeddedResource:
		return fromSDKEmbeddedResource(v, b)

	case *mcp.ResourceLink:
		return unsupported(KindResourceLink, c), nil
	//lint:ignore SA1019 supported for peers ≤2025-11-25 (SEP-2577)
	case *mcp.ToolUseContent:
		return unsupported(KindToolUse, c), nil
	//lint:ignore SA1019 supported for peers ≤2025-11-25 (SEP-2577)
	case *mcp.ToolResultContent:
		return unsupported(KindToolResult, c), nil
	default:
		return unsupported(KindUnknown, c), nil
	}
}

func fromSDKEmbeddedResource(v *mcp.EmbeddedResource, b Bounds) (Content, error) {
	if v.Resource == nil {
		return nil, fmt.Errorf("%w: embedded resource contents", errNilInput)
	}
	r := v.Resource
	// A blob is bounded like any other binary item; text is truncated.
	if len(r.Blob) > b.MaxBinaryItemBytes {
		return UnsupportedContent{Kind: KindResource, Bytes: len(r.Blob)}, nil
	}
	text, truncated := limits.TruncateText(r.Text, b.MaxTextBytes)
	return EmbeddedResourceContent{
		URI:       r.URI,
		MIMEType:  r.MIMEType,
		Text:      text,
		Data:      bytes.Clone(r.Blob),
		Truncated: truncated,
	}, nil
}

// unsupported measures a content item we will not retain — without
// marshalling it. Rendering hostile data in full purely to count bytes we then
// discard would allocate the entire payload we are refusing, which is the
// opposite of what refusing it is for.
//
// For the kinds the SDK enumerates, the size is summed from the fields that
// carry the item's weight; see UnsupportedContent.Bytes for what that number
// promises. Binary items never reach here — they are refused at the call site
// against their exact len(Data).
func unsupported(kind string, c mcp.Content) UnsupportedContent {
	return UnsupportedContent{Kind: kind, Bytes: unsupportedSize(c)}
}

func unsupportedSize(c mcp.Content) int {
	switch v := c.(type) {
	case *mcp.ResourceLink:
		return len(v.URI) + len(v.Name) + len(v.Title) + len(v.Description) + len(v.MIMEType)
	//lint:ignore SA1019 supported for peers ≤2025-11-25 (SEP-2577)
	case *mcp.ToolUseContent:
		return len(v.ID) + len(v.Name)
	//lint:ignore SA1019 supported for peers ≤2025-11-25 (SEP-2577)
	case *mcp.ToolResultContent:
		return len(v.ToolUseID)
	default:
		// A kind this SDK version does not enumerate: no field can be named
		// to size it, so its marshaled form is the only signal there is. This
		// is unreachable with go-sdk v1.6.1 — every Content kind it defines is
		// either converted natively or handled above — so the allocation is a
		// forward-compatibility cost we only pay for a kind we do not know.
		raw, err := c.MarshalJSON()
		if err != nil {
			return 0
		}
		return len(raw)
	}
}

// FromSDKContents converts a content list, additionally enforcing
// Bounds.MaxBinaryItems across the binary items in it. Items past the budget
// become UnsupportedContent, so the list keeps its length and position: a
// caller can always tell which item it lost.
//
// Only items actually retained spend the budget. An item already refused on
// size does not, or a single oversized image would evict a later good one.
func FromSDKContents(cs []mcp.Content, b Bounds) ([]Content, error) {
	if len(cs) == 0 {
		return nil, nil
	}
	out := make([]Content, 0, len(cs))
	binary := 0
	for i, c := range cs {
		conv, err := FromSDKContent(c, b)
		if err != nil {
			return nil, fmt.Errorf("protocol: content item %d: %w", i, err)
		}
		if kind, isBinary := binaryKind(conv); isBinary {
			if binary >= b.MaxBinaryItems {
				conv = UnsupportedContent{Kind: kind, Bytes: binarySize(conv)}
			} else {
				binary++
			}
		}
		out = append(out, conv)
	}
	return out, nil
}

// binaryKind reports whether a converted item counts against MaxBinaryItems,
// and under which kind. An embedded resource counts only when it actually
// carries a blob — a text one is not a binary item.
func binaryKind(c Content) (string, bool) {
	switch v := c.(type) {
	case ImageContent:
		return KindImage, true
	case AudioContent:
		return KindAudio, true
	case EmbeddedResourceContent:
		return KindResource, len(v.Data) > 0
	default:
		return "", false
	}
}

// binarySize is the payload size of a binary item, for reporting once it is
// refused.
func binarySize(c Content) int {
	switch v := c.(type) {
	case ImageContent:
		return len(v.Data)
	case AudioContent:
		return len(v.Data)
	case EmbeddedResourceContent:
		return len(v.Data)
	default:
		return 0
	}
}

// FromSDKServerIdentity converts the server's self-description. A nil
// Implementation yields the zero identity rather than an error: identity here
// is cosmetic, and an anonymous server is not a protocol violation.
//
// Every field is truncated to b.MaxTextBytes. An identity is the one piece of
// server data this module keeps for the whole life of a connection and renders
// freely — it reaches logs, telemetry and UIs through client.Status — so a
// server must not be able to make its own name a memory or log-volume problem.
// Truncation rather than rejection, for the same reason as Instructions: a
// padded name is not a reason to refuse an otherwise-working server.
func FromSDKServerIdentity(impl *mcp.Implementation, b Bounds) ServerIdentity {
	if impl == nil {
		return ServerIdentity{}
	}
	name, _ := limits.TruncateText(impl.Name, b.MaxTextBytes)
	version, _ := limits.TruncateText(impl.Version, b.MaxTextBytes)
	title, _ := limits.TruncateText(impl.Title, b.MaxTextBytes)
	return ServerIdentity{
		Name:    name,
		Version: version,
		Title:   title,
	}
}
