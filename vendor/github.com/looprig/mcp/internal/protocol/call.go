// This file holds the request methods that act rather than enumerate: calling a
// tool, getting a prompt, reading a resource, subscribing to one — and the two
// server-initiated streams that arrive alongside them, progress and logging.
//
// Everything here converts through conv.go and returns neutral, bounded types.
// None of it decides whether a call is *allowed*: capability gating, tool
// filtering and deadlines are policy, they live in pkg/client, and a Conn that
// enforced them would be enforcing a policy it cannot see.

package protocol

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/looprig/mcp/internal/limits"
)

// WhatStructuredBytes is the limits.OverLimitError.What reported when a
// structured tool result exceeds Bounds.MaxStructuredBytes.
const WhatStructuredBytes = "structured_bytes"

// ToolResult is the outcome of a tools/call, bounded and detached from SDK
// memory.
//
// IsError carries the MCP protocol-level tool error, which is emphatically not
// a transport failure: the call succeeded, and the tool reported that whatever
// it was asked to do did not work. The design requires that distinction survive
// all the way to the model — an expected remote failure becomes a structured
// result, not a control-plane error — so it is a field here rather than an
// error return.
type ToolResult struct {
	// IsError reports that the tool itself failed. Content carries the tool's
	// explanation.
	IsError bool
	// Content is the unstructured result, already bounded.
	Content []Content
	// Structured is the optional structured result, within
	// Bounds.MaxStructuredBytes. Nil when the server sent none, or when an
	// over-bound one was dropped (see Warnings).
	Structured json.RawMessage
	// Warnings records defects tolerated during conversion.
	Warnings []string
}

// warn appends a bounded warning.
func (r *ToolResult) warn(msg string) {
	if len(r.Warnings) >= MaxWarnings {
		return
	}
	r.Warnings = append(r.Warnings, msg)
}

// PromptResult is the outcome of a prompts/get.
type PromptResult struct {
	Description string
	Messages    []PromptMessage
}

// PromptMessage is one message of a prompt. Role is the server's string
// verbatim ("user" or "assistant" in practice); it is not narrowed to an enum
// because a prompt message is content to render, not a decision to route on,
// and an unknown role must not fail the fetch.
type PromptMessage struct {
	Role    string
	Content Content
}

// ResourceResult is the outcome of a resources/read.
type ResourceResult struct {
	Contents []ResourceContent
}

// ResourceContent is one item of a resource's contents. Exactly one of Text or
// Data is meaningful, per what the server sent.
type ResourceContent struct {
	URI       string
	MIMEType  string
	Text      string
	Data      []byte
	Truncated bool
}

// ProgressUpdate is one progress notification for an in-flight call. Every
// field is server-supplied: it is a hint to render, never a fact to act on.
type ProgressUpdate struct {
	// Progress is how far the server claims to have got.
	Progress float64
	// Total is the server's claimed total, or 0 when it did not say.
	Total float64
	// Message is the server's bounded description of what it is doing.
	Message string
}

// CallOptions are the per-call knobs a Conn honors. Deadlines are deliberately
// absent: a deadline is a ctx, and a second way to spell one is a second thing
// to get wrong.
type CallOptions struct {
	// Progress, when non-nil, receives the call's progress notifications. It is
	// invoked on the connection's notification goroutine and blocks it, so it
	// must not do work.
	//
	// Registering it is what puts a progress token on the request; a server is
	// only allowed to send progress for a request that carries one, so a nil
	// Progress means the notifications are never generated in the first place.
	Progress func(ProgressUpdate)
}

// LogRecord is one server log message, bounded. It is the neutral form of an
// MCP logging notification.
type LogRecord struct {
	// Level is the severity the server claimed, verbatim.
	Level string
	// Logger is the server-side logger name, if it sent one.
	Logger string
	// Text is the message, truncated to Bounds.MaxLogBytes.
	Text string
}

// CallTool invokes a tool by its raw server name.
//
// args is raw JSON because tool arguments are a serialization-boundary
// document: their shape is the server's input schema, which is data, so there
// is no Go type to narrow them to. They are passed through untouched —
// validating them against the schema is the caller's job, and doing it here
// would mean this layer had an opinion about a schema it did not fetch.
//
// Cancelling ctx cancels the call at the protocol level: the SDK sends
// notifications/cancelled and stops waiting. The server is told, rather than
// merely abandoned.
func (s *Session) CallTool(ctx context.Context, rawName string, args json.RawMessage, opts CallOptions) (ToolResult, error) {
	cs, err := s.established()
	if err != nil {
		return ToolResult{}, err
	}
	if rawName == "" {
		return ToolResult{}, errors.New("protocol: tool name is empty")
	}

	params := &mcp.CallToolParams{Name: rawName}
	if len(args) > 0 {
		// json.RawMessage marshals verbatim, so the server sees exactly the
		// bytes the caller supplied.
		params.Arguments = args
	}

	if opts.Progress != nil {
		token, err := progressToken()
		if err != nil {
			return ToolResult{}, err
		}
		params.SetProgressToken(token)
		s.addProgress(token, opts.Progress)
		defer s.removeProgress(token)
	}

	res, err := cs.CallTool(ctx, withLogLevel(s, params))
	if err != nil {
		return ToolResult{}, fmt.Errorf("tools/call %q: %w", rawName, err)
	}
	return FromSDKCallToolResult(res, s.cfg.Bounds)
}

// FromSDKCallToolResult converts a tools/call result.
//
// It is exported separately from CallTool so a fuzzer can drive it with any
// result a server could return.
func FromSDKCallToolResult(res *mcp.CallToolResult, b Bounds) (ToolResult, error) {
	if res == nil {
		return ToolResult{}, fmt.Errorf("%w: tools/call result", errNilInput)
	}
	out := ToolResult{IsError: res.IsError}

	content, err := FromSDKContents(res.Content, b)
	if err != nil {
		return ToolResult{}, err
	}
	out.Content = content

	structured, warning := boundStructured(res.StructuredContent, b)
	out.Structured = structured
	if warning != "" {
		out.warn(warning)
	}
	return out, nil
}

// boundStructured enforces Bounds.MaxStructuredBytes on a structured result,
// returning the retained document and a warning if one was dropped.
//
// An over-bound document is dropped with a warning rather than failing the
// call, for the same reason FromSDKTool drops an oversized output schema:
// dropping already achieves what the bound exists for — the document is not
// retained — and the unstructured Content is still there, so the result stays
// usable. Failing instead would let a server make a working tool unusable by
// padding an optional field.
//
// The document is measured after marshalling, which is the only point its size
// is knowable: the SDK types the field as `any`, so until it is rendered there
// is nothing to count. The marshal is the allocation the bound cannot prevent;
// what it prevents is retaining it. The wire framing bound (WireLimits) is what
// stops an enormous one arriving at all.
func boundStructured(structured any, b Bounds) (json.RawMessage, string) {
	if structured == nil {
		return nil, ""
	}
	raw, err := json.Marshal(structured)
	if err != nil {
		return nil, fmt.Sprintf("structured content dropped: not marshalable: %v", err)
	}
	// A server that sent JSON null sent no structured content.
	if isAbsentSchema(json.RawMessage(raw)) {
		return nil, ""
	}
	if len(raw) > b.MaxStructuredBytes {
		return nil, fmt.Sprintf("structured content dropped: %v",
			&limits.OverLimitError{What: WhatStructuredBytes, Limit: b.MaxStructuredBytes})
	}
	if err := limits.CheckJSONDepth(raw, b.MaxSchemaDepth); err != nil {
		// Depth is bounded for the same reason a schema's is: anything that
		// walks this document recursively would otherwise recurse as deep as
		// the server chose.
		return nil, fmt.Sprintf("structured content dropped: %v", err)
	}
	return raw, ""
}

// GetPrompt fetches a prompt's messages, substituting args.
//
// args is map[string]string rather than a typed struct because that is exactly
// what MCP defines a prompt's arguments to be: a flat string map, whose keys
// are named by the prompt's own declared arguments. There is no domain type
// here to narrow to.
func (s *Session) GetPrompt(ctx context.Context, name string, args map[string]string) (PromptResult, error) {
	cs, err := s.established()
	if err != nil {
		return PromptResult{}, err
	}
	if name == "" {
		return PromptResult{}, errors.New("protocol: prompt name is empty")
	}
	res, err := cs.GetPrompt(ctx, withLogLevel(s, &mcp.GetPromptParams{Name: name, Arguments: args}))
	if err != nil {
		return PromptResult{}, fmt.Errorf("prompts/get %q: %w", name, err)
	}
	return FromSDKGetPromptResult(res, s.cfg.Bounds)
}

// FromSDKGetPromptResult converts a prompts/get result.
func FromSDKGetPromptResult(res *mcp.GetPromptResult, b Bounds) (PromptResult, error) {
	if res == nil {
		return PromptResult{}, fmt.Errorf("%w: prompts/get result", errNilInput)
	}
	description, _ := limits.TruncateText(res.Description, b.MaxTextBytes)
	out := PromptResult{Description: description}
	if len(res.Messages) == 0 {
		return out, nil
	}
	out.Messages = make([]PromptMessage, 0, len(res.Messages))
	for i, m := range res.Messages {
		if m == nil {
			return PromptResult{}, fmt.Errorf("%w: prompt message %d", errNilInput, i)
		}
		content, err := FromSDKContent(m.Content, b)
		if err != nil {
			return PromptResult{}, fmt.Errorf("protocol: prompt message %d: %w", i, err)
		}
		role, _ := limits.TruncateText(string(m.Role), b.MaxTextBytes)
		out.Messages = append(out.Messages, PromptMessage{Role: role, Content: content})
	}
	return out, nil
}

// ReadResource reads a resource by URI. The URI is opaque: it is a protocol
// identifier the server issued, not a host path, and nothing here resolves it.
func (s *Session) ReadResource(ctx context.Context, uri string) (ResourceResult, error) {
	cs, err := s.established()
	if err != nil {
		return ResourceResult{}, err
	}
	if uri == "" {
		return ResourceResult{}, errors.New("protocol: resource URI is empty")
	}
	res, err := cs.ReadResource(ctx, withLogLevel(s, &mcp.ReadResourceParams{URI: uri}))
	if err != nil {
		return ResourceResult{}, fmt.Errorf("resources/read %q: %w", uri, err)
	}
	return FromSDKReadResourceResult(res, s.cfg.Bounds)
}

// FromSDKReadResourceResult converts a resources/read result.
func FromSDKReadResourceResult(res *mcp.ReadResourceResult, b Bounds) (ResourceResult, error) {
	if res == nil {
		return ResourceResult{}, fmt.Errorf("%w: resources/read result", errNilInput)
	}
	if len(res.Contents) == 0 {
		return ResourceResult{}, nil
	}
	out := ResourceResult{Contents: make([]ResourceContent, 0, len(res.Contents))}
	binary := 0
	for i, c := range res.Contents {
		if c == nil {
			return ResourceResult{}, fmt.Errorf("%w: resource contents %d", errNilInput, i)
		}
		item := ResourceContent{URI: c.URI, MIMEType: c.MIMEType}
		// A blob over the item bound, or past the binary-item budget, is
		// summarized rather than injected: the design requires unsupported
		// binary data to be summarized, and the metadata (URI, MIME type) is
		// what makes the omission visible.
		switch {
		case len(c.Blob) == 0:
			item.Text, item.Truncated = limits.TruncateText(c.Text, b.MaxTextBytes)
		case len(c.Blob) > b.MaxBinaryItemBytes, binary >= b.MaxBinaryItems:
			item.Truncated = true
		default:
			item.Data = append([]byte(nil), c.Blob...)
			binary++
		}
		out.Contents = append(out.Contents, item)
	}
	return out, nil
}

// Subscribe asks the server to notify this connection when a resource changes.
//
// Whether the server supports subscribing at all is the caller's check: this
// method issues the request, it does not gate it.
func (s *Session) Subscribe(ctx context.Context, uri string) error {
	cs, err := s.established()
	if err != nil {
		return err
	}
	if uri == "" {
		return errors.New("protocol: resource URI is empty")
	}
	if err := cs.Subscribe(ctx, &mcp.SubscribeParams{URI: uri}); err != nil {
		return fmt.Errorf("resources/subscribe %q: %w", uri, err)
	}
	return nil
}

// Unsubscribe asks the server to stop reporting changes to a resource. It is the
// counterpart to Subscribe: after it returns, the server sends no further
// resource-update notifications for uri.
func (s *Session) Unsubscribe(ctx context.Context, uri string) error {
	cs, err := s.established()
	if err != nil {
		return err
	}
	if uri == "" {
		return errors.New("protocol: resource URI is empty")
	}
	if err := cs.Unsubscribe(ctx, &mcp.UnsubscribeParams{URI: uri}); err != nil {
		return fmt.Errorf("resources/unsubscribe %q: %w", uri, err)
	}
	return nil
}

// SetLogLevel asks the server to send log messages at or above level.
//
// It is required, not optional: an MCP server sends nothing until the client
// sets a level, so a client that installs a log handler and never calls this
// receives silence and cannot tell it from a quiet server.
//
// The level is also remembered on the session (see withLogLevel): on a peer
// negotiating protocol >= 2026-07-28, the legacy logging/setLevel RPC this
// method sends is not enough on its own to keep the promise above — see
// withLogLevel's comment for why.
func (s *Session) SetLogLevel(ctx context.Context, level string) error {
	cs, err := s.established()
	if err != nil {
		return err
	}
	//lint:ignore SA1019 supported for peers ≤2025-11-25 (SEP-2577)
	if err := cs.SetLoggingLevel(ctx, &mcp.SetLoggingLevelParams{Level: mcp.LoggingLevel(level)}); err != nil {
		return fmt.Errorf("logging/setLevel %q: %w", level, err)
	}
	s.mu.Lock()
	s.logLevel = level
	s.mu.Unlock()
	return nil
}

// withLogLevel stamps the session's active log level onto p's request
// metadata, when one has been set, and returns p.
//
// It is applied at CallTool, GetPrompt and ReadResource — the multi round-trip
// methods (SEP-2322) whose handlers are where a server does the work that
// might log — because of a wire-level fact about SDK v1.7.0 that the legacy
// logging/setLevel RPC alone does not survive: on a peer negotiating protocol
// >= 2026-07-28, the SDK ties a server's effective log level to the request
// that is "in flight" rather than to the session (SEP-2575) —
// ServerSession.Log reads it from that specific request's
// `_meta["io.modelcontextprotocol/logLevel"]`, and resets the session's
// stored level to whatever (or nothing) that value says on *every* request
// that carries the new protocol's metadata. logging/setLevel itself never
// carries that metadata, so its effect would otherwise be undone by the very
// next such call this session makes. Stamping the active level onto each of
// them here is what keeps SetLogLevel's contract — "sends log messages at or
// above level" — true across calls on such a peer, instead of true only until
// the next one.
//
// It is a no-op, correctly, in two cases: SetLogLevel was never called (there
// is nothing to keep alive), and the peer negotiated an older protocol (the
// SDK never overwrites the level there, so nothing needs re-asserting, and the
// stray _meta key is simply ignored by a server that does not look for it).
func withLogLevel[P interface {
	GetMeta() map[string]any
	SetMeta(map[string]any)
}](s *Session, p P) P {
	s.mu.Lock()
	level := s.logLevel
	s.mu.Unlock()
	if level == "" {
		return p
	}
	m := p.GetMeta()
	if m == nil {
		m = map[string]any{}
	}
	_, ok := m[mcp.MetaKeyLogLevel] //lint:ignore SA1019 supported for peers ≤2025-11-25 (SEP-2577); this key is also exactly what keeps this working for peers >=2026-07-28
	if !ok {
		m[mcp.MetaKeyLogLevel] = level //lint:ignore SA1019 supported for peers ≤2025-11-25 (SEP-2577)
	}
	p.SetMeta(m)
	return p
}

// progressTokenBytes is the entropy in a progress token. A token only has to be
// unique among this connection's in-flight calls, but it is generated from
// crypto/rand rather than a counter so that a server cannot predict the next
// one and address a notification at a call it was never given.
const progressTokenBytes = 16

// progressToken mints a token for one call.
func progressToken() (string, error) {
	var b [progressTokenBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("protocol: generating a progress token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// addProgress registers a call's progress callback.
func (s *Session) addProgress(token string, fn func(ProgressUpdate)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.progress == nil {
		s.progress = map[string]func(ProgressUpdate){}
	}
	s.progress[token] = fn
}

// removeProgress deregisters a call's progress callback. Every registration is
// paired with one of these in a defer: the map must not grow with the number of
// calls a connection has ever made.
func (s *Session) removeProgress(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.progress, token)
}

// onProgress routes a progress notification to the call that asked for it.
//
// A notification whose token names no in-flight call is dropped silently. That
// is the normal case, not a defect: a server may emit progress just as a call
// returns, and there is nothing left to tell. It is also the fail-closed
// outcome for a server that invents a token — there is no call to attribute it
// to, so nobody is told anything.
func (s *Session) onProgress(params *mcp.ProgressNotificationParams) {
	if params == nil {
		return
	}
	token, ok := params.ProgressToken.(string)
	if !ok {
		// This client only ever issues string tokens, so anything else names no
		// call of ours.
		return
	}

	s.mu.Lock()
	fn := s.progress[token]
	s.mu.Unlock()
	if fn == nil {
		return
	}

	message, _ := limits.TruncateText(params.Message, s.cfg.Bounds.MaxTextBytes)
	fn(ProgressUpdate{Progress: params.Progress, Total: params.Total, Message: message})
}

// onLog converts and delivers a server log message.
//
//lint:ignore SA1019 supported for peers ≤2025-11-25 (SEP-2577)
func (s *Session) onLog(params *mcp.LoggingMessageParams) {
	if params == nil || s.cfg.OnLog == nil {
		return
	}
	s.cfg.OnLog(FromSDKLogParams(params, s.cfg.Bounds))
}

// onListChanged routes a list-change notification to the config's callback.
//
// There is nothing to convert and nothing to bound: the notification's params
// carry no content this module reads (see ListChange), so what crosses the
// boundary is the family alone — a value from this package's own enum, chosen
// by the handler that received the notification, never by the server.
func (s *Session) onListChanged(f ListFamily) {
	if s.cfg.OnListChanged == nil {
		return
	}
	s.cfg.OnListChanged(ListChange{Family: f})
}

// onResourceUpdated routes a resource-update notification to the config's
// callback, bounding the server-supplied URI first.
//
// The URI is the one field an update carries, and it is untrusted like every
// other server-supplied string here: it is truncated to Bounds.MaxTextBytes so
// a hostile server cannot turn a notification into an unbounded allocation. A
// nil params, or an update with no callback installed, is dropped — there is no
// failure a caller could act on and nothing to deliver.
func (s *Session) onResourceUpdated(params *mcp.ResourceUpdatedNotificationParams) {
	if params == nil || s.cfg.OnResourceUpdated == nil {
		return
	}
	uri, _ := limits.TruncateText(params.URI, s.cfg.Bounds.MaxTextBytes)
	s.cfg.OnResourceUpdated(ResourceUpdate{URI: uri})
}

// FromSDKLogParams converts a logging notification, bounding its text.
//
// The MCP log payload is `any`: a server may log a string or an arbitrary JSON
// value. A string is used as-is and anything else is rendered as JSON, so that
// a structured log is readable rather than dropped. Either way the result is
// truncated to Bounds.MaxLogBytes — a log line is a diagnostic from an
// untrusted peer, not a data channel.
//
// Nil params yield the zero LogRecord. The function reports no error — a log
// line has no failure a caller could act on — so nil cannot be an error return,
// and it must not be a panic: this converter is exported precisely so a fuzzer
// can drive it directly with anything a server could produce, and "the caller
// must nil-check first" is a contract every sibling converter here declines to
// rely on. It matches the treatment of a nil Data payload, which is likewise
// empty rather than fatal.
//
//lint:ignore SA1019 supported for peers ≤2025-11-25 (SEP-2577)
func FromSDKLogParams(params *mcp.LoggingMessageParams, b Bounds) LogRecord {
	if params == nil {
		return LogRecord{}
	}
	rec := LogRecord{Level: string(params.Level), Logger: params.Logger}
	rec.Logger, _ = limits.TruncateText(rec.Logger, b.MaxLogBytes)

	switch data := params.Data.(type) {
	case nil:
		rec.Text = ""
	case string:
		rec.Text, _ = limits.TruncateText(data, b.MaxLogBytes)
	default:
		raw, err := json.Marshal(data)
		if err != nil {
			rec.Text = "[unrenderable log payload]"
			return rec
		}
		rec.Text, _ = limits.TruncateText(string(raw), b.MaxLogBytes)
	}
	return rec
}
