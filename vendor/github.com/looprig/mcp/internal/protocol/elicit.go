// This file is the elicitation seam: the conversion between the SDK's
// elicitation/create request and this package's neutral types, plus the
// Session-side handler that carries one to the client's callback.
//
// Elicitation inverts the usual direction of trust checking. Everywhere else in
// this package a server *answers*; here a server *asks*, and what it asks for is
// a human's attention and a human's input. That makes two things load-bearing:
//
//   - The capability must never be advertised on a client's behalf (see
//     Session.Initialize). A server only asks because it was told it may.
//   - Everything the server sends is shown to a person, so it is bounded and
//     validated before it reaches one — an unbounded prompt is a denial of
//     service against a human, and an unknown mode is a request this module
//     cannot honestly describe to them.

package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/looprig/mcp/internal/limits"
)

// The limits.OverLimitError.What values this file reports.
const (
	// WhatElicitMessageBytes is reported when a server's elicitation prompt
	// exceeds Bounds.MaxElicitMessageBytes.
	WhatElicitMessageBytes = "elicit_message_bytes"
	// WhatElicitSchemaBytes is reported when a server's requested schema
	// exceeds Bounds.MaxElicitSchemaBytes.
	WhatElicitSchemaBytes = "elicit_schema_bytes"
)

// ElicitMode is which kind of elicitation a server sent. The zero value is not
// a mode: a request this module cannot classify is refused, never guessed.
type ElicitMode uint8

// The elicitation modes MCP defines.
const (
	// ElicitModeForm is a bounded schema of typed fields to fill in.
	ElicitModeForm ElicitMode = iota + 1
	// ElicitModeURL is an out-of-band action to perform at a URL.
	ElicitModeURL

	elicitModeSentinel // must remain last; tests derive the declared range from it
)

// elicitModeWire maps each mode to its MCP wire identifier. These are the
// protocol's own strings, so they must not change.
var elicitModeWire = [elicitModeSentinel]string{
	ElicitModeForm: "form",
	ElicitModeURL:  "url",
}

// String returns the mode's wire identifier, or "unknown" for any value outside
// the declared range.
func (m ElicitMode) String() string {
	if m < ElicitModeForm || m >= elicitModeSentinel {
		return "unknown"
	}
	return elicitModeWire[m]
}

// ElicitRequest is a server's request for human input, bounded and detached from
// SDK memory. Every field is server-supplied and untrusted: it is content to
// show a person, never an instruction to the host.
type ElicitRequest struct {
	// Mode is the kind of elicitation. It is always a declared mode: an
	// unrecognized one is refused at conversion.
	Mode ElicitMode
	// Message is the prompt to show the human, within
	// Bounds.MaxElicitMessageBytes.
	Message string
	// Schema is the JSON Schema the answer must satisfy, in form mode only; nil
	// otherwise. It is raw JSON because a schema is a serialization-boundary
	// document, not domain data.
	Schema json.RawMessage
	// URL is the out-of-band action's URL, in url mode only; empty otherwise.
	URL string
	// ElicitationID is the server's correlation id, in url mode only. It may be
	// empty even there: MCP does not require one.
	ElicitationID string
}

// ElicitAction is how a human answered. The zero value is not a valid action:
// a caller must say what happened, because "nothing" is not an answer this
// module may put on the wire.
type ElicitAction uint8

// The elicitation outcomes, per the MCP elicitation result actions.
const (
	// ElicitAccept means the human supplied the requested content.
	ElicitAccept ElicitAction = iota + 1
	// ElicitDecline means the human refused.
	ElicitDecline
	// ElicitCancel means the human dismissed the request without deciding.
	ElicitCancel

	elicitActionSentinel // must remain last; tests derive the declared range from it
)

// elicitActionWire maps each action to its MCP wire identifier.
var elicitActionWire = [elicitActionSentinel]string{
	ElicitAccept:  "accept",
	ElicitDecline: "decline",
	ElicitCancel:  "cancel",
}

// String returns the action's wire identifier, or "unknown".
func (a ElicitAction) String() string {
	if a < ElicitAccept || a >= elicitActionSentinel {
		return "unknown"
	}
	return elicitActionWire[a]
}

// ElicitResult is the human's answer, as the layer above produced it. Content is
// meaningful only when Action is ElicitAccept.
type ElicitResult struct {
	Action ElicitAction
	// Content is the accepted answer as a JSON object, or nil. It is raw JSON
	// for the same reason Schema is: its shape is the server's schema, which is
	// data, so there is no Go type to narrow it to.
	Content json.RawMessage
}

// errNoElicitHandler reports an elicitation that arrived with nothing to serve
// it. It is unreachable in this module — the SDK handler is only registered when
// a callback exists (see Session.Initialize) — and it exists so that if that
// invariant is ever broken the server is refused rather than a nil callback
// invoked.
var errNoElicitHandler = errors.New("protocol: no elicitation handler is installed")

// onElicit converts a server's elicitation, hands it to the client's callback,
// and converts the answer back.
//
// An error returned from here becomes a JSON-RPC error to the server. That is
// the right shape for every failure it can have: a request this module refused
// to describe to a human, and a human who could not be asked, are both "no
// answer", and a server must learn that rather than wait.
func (s *Session) onElicit(ctx context.Context, params *mcp.ElicitParams) (*mcp.ElicitResult, error) {
	fn := s.cfg.OnElicit
	if fn == nil {
		return nil, errNoElicitHandler
	}
	req, err := FromSDKElicitParams(params, s.cfg.Bounds)
	if err != nil {
		return nil, err
	}
	res, err := fn(ctx, req)
	if err != nil {
		return nil, err
	}
	return ToSDKElicitResult(res)
}

// FromSDKElicitParams converts an elicitation/create request, bounding it.
//
// It is exported separately from onElicit so a fuzzer can drive it with anything
// a server could send.
//
// An over-bound message or schema is rejected outright rather than truncated,
// which is the opposite of what this package does to a server's log line or
// instructions — and deliberately. Those are read; this is *acted on*. A
// truncated prompt is a prompt whose meaning this module silently altered, and a
// human who accepts it has consented to something they were not shown. A
// truncated schema is not a schema at all.
func FromSDKElicitParams(params *mcp.ElicitParams, b Bounds) (ElicitRequest, error) {
	if params == nil {
		return ElicitRequest{}, fmt.Errorf("%w: elicitation/create params", errNilInput)
	}

	mode, err := elicitMode(params.Mode)
	if err != nil {
		return ElicitRequest{}, err
	}
	if len(params.Message) > b.MaxElicitMessageBytes {
		return ElicitRequest{}, fmt.Errorf("elicitation message: %d bytes: %w", len(params.Message),
			&limits.OverLimitError{What: WhatElicitMessageBytes, Limit: b.MaxElicitMessageBytes})
	}

	req := ElicitRequest{Mode: mode, Message: params.Message}
	switch mode {
	case ElicitModeForm:
		// A schema is optional even in form mode: a server may ask for a bare
		// confirmation. What it may not do is send an unusable one.
		if !isAbsentSchema(params.RequestedSchema) {
			raw, err := marshalSchema(params.RequestedSchema)
			if err != nil {
				return ElicitRequest{}, fmt.Errorf("elicitation requested schema: %w", err)
			}
			if err := checkElicitSchemaBounds(raw, b); err != nil {
				return ElicitRequest{}, fmt.Errorf("elicitation requested schema: %w", err)
			}
			req.Schema = raw
		}
	case ElicitModeURL:
		// The URL is shown to the human alongside the message, so it is part of
		// the same prompt and shares its bound. The mode's other fields are
		// dropped rather than carried: a schema has no meaning here, and passing
		// one on would invite a handler to act on it.
		if len(params.URL) > b.MaxElicitMessageBytes {
			return ElicitRequest{}, fmt.Errorf("elicitation url: %d bytes: %w", len(params.URL),
				&limits.OverLimitError{What: WhatElicitMessageBytes, Limit: b.MaxElicitMessageBytes})
		}
		if len(params.ElicitationID) > b.MaxElicitMessageBytes {
			return ElicitRequest{}, fmt.Errorf("elicitation id: %d bytes: %w", len(params.ElicitationID),
				&limits.OverLimitError{What: WhatElicitMessageBytes, Limit: b.MaxElicitMessageBytes})
		}
		req.URL = params.URL
		req.ElicitationID = params.ElicitationID
	}
	return req, nil
}

// elicitMode maps the wire mode onto the enum.
//
// An empty mode is form: that is the SDK's own default for an absent field (see
// its Client.elicit), and MCP's — elicitation predates the mode field. An
// unrecognized one is refused, because the mode says what the human is being
// asked to *do*, and a module that guessed would be showing someone a prompt it
// does not understand.
func elicitMode(mode string) (ElicitMode, error) {
	if mode == "" {
		return ElicitModeForm, nil
	}
	for m := ElicitModeForm; m < elicitModeSentinel; m++ {
		if elicitModeWire[m] == mode {
			return m, nil
		}
	}
	return 0, fmt.Errorf("protocol: unsupported elicitation mode %q", mode)
}

// checkElicitSchemaBounds bounds a requested schema's size and nesting.
//
// The byte bound is elicitation's own — a form a person fills in is a different
// thing from a tool's interface, and sizing them together would mean raising one
// to raise the other. The depth bound is shared with tool schemas: it exists to
// bound recursive traversal of a JSON document, and a document is a document.
func checkElicitSchemaBounds(raw json.RawMessage, b Bounds) error {
	if len(raw) > b.MaxElicitSchemaBytes {
		return fmt.Errorf("%d bytes: %w", len(raw),
			&limits.OverLimitError{What: WhatElicitSchemaBytes, Limit: b.MaxElicitSchemaBytes})
	}
	return limits.CheckJSONDepth(raw, b.MaxSchemaDepth)
}

// ToSDKElicitResult converts an answer into the SDK's result.
//
// The Content conversion to map[string]any is this module's only elicitation
// `any`: it is the SDK's wire type for the JSON object the schema describes, and
// this is the serialization boundary where such a narrowing belongs. Handing the
// SDK the object (rather than the raw bytes) is also what lets it validate the
// answer against the requested schema before it reaches the server — a check
// this module wants and does not have to write.
func ToSDKElicitResult(r ElicitResult) (*mcp.ElicitResult, error) {
	if r.Action < ElicitAccept || r.Action >= elicitActionSentinel {
		return nil, fmt.Errorf("protocol: elicitation result has no action (%d)", r.Action)
	}
	out := &mcp.ElicitResult{Action: elicitActionWire[r.Action]}
	if r.Action != ElicitAccept {
		if len(r.Content) > 0 {
			// A refusal carrying content is a caller bug with two plausible
			// readings — send the content, or honor the refusal — and this is
			// exactly the ambiguity that gets denied rather than guessed. Either
			// way the content does not go on the wire.
			return nil, fmt.Errorf("protocol: elicitation result is %q but carries content", r.Action)
		}
		return out, nil
	}
	if len(r.Content) == 0 {
		// An accept with nothing to submit is legal: the server may have asked
		// only for confirmation.
		return out, nil
	}
	var content map[string]any
	if err := json.Unmarshal(r.Content, &content); err != nil {
		return nil, fmt.Errorf("protocol: elicitation content is not a JSON object: %w", err)
	}
	if content == nil {
		// `null` unmarshals into a nil map without error, and would reach the
		// server as an accept with no content — a different answer from the one
		// the caller wrote.
		return nil, errors.New("protocol: elicitation content is JSON null, not an object")
	}
	out.Content = content
	return out, nil
}
