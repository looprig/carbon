// This file is the sampling seam: the conversion between the SDK's
// sampling/createMessage request and this package's neutral types, plus the
// Session-side handler that carries one to the client's callback.
//
// Sampling inverts trust the same way elicitation does — a server *asks* rather
// than answers — but what it asks for is different, and worse. Elicitation asks
// for a person's attention; sampling asks for the host's model, which means the
// host's money. A server that can drive sampling can spend on the host's
// account, so two things are load-bearing here:
//
//   - The capability must never be advertised on a client's behalf (see
//     Session.Initialize). A server only asks because it was told it may.
//   - What arrives is a *request*, never an instruction. The server's model
//     preferences, sampling parameters, and context requests are dropped at this
//     boundary rather than carried: the host picks the model and the budget, and
//     a field that reached a handler would be a field a handler might honor.
//
// What this boundary deliberately does not carry, and why:
//
//   - ModelPreferences, Temperature, StopSequences, Metadata — the server's
//     preferences for how the host's model should run. The design gives model
//     selection to the application, so a server's opinion about it is not
//     modelled. Dropping them is not a limitation to be fixed later: carrying
//     them would let a server steer spend it does not pay for.
//   - IncludeContext — a request that the host attach context from this (or
//     every) MCP server to the prompt. Honoring it would mean a server could
//     make the host harvest other servers' data into a prompt it will see the
//     completion of. It is dropped, and the matching capability
//     (SamplingCapabilities.Context) is never advertised, so a well-behaved
//     server does not ask.
//   - Tool use — the SDK models tool-bearing sampling as a separate handler
//     (CreateMessageWithToolsHandler) with its own advertised capability. This
//     module registers only the basic CreateMessageHandler and never advertises
//     SamplingCapabilities.Tools, so tools cannot enter a sampling request at
//     all. That is the structural half of the design's "sampling never receives
//     an unrestricted tool registry": there is no field to put one in.
//
// Non-text content (image, audio, tool_use, tool_result) is refused rather than
// dropped. A conversation with a message silently removed is a different
// conversation from the one the server sent, and this module will not ask a
// model to complete something nobody wrote.

package protocol

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/looprig/mcp/internal/limits"
)

// WhatSampleTextBytes is the limits.OverLimitError.What this file reports when
// a sampling conversation's text exceeds Bounds.MaxTextBytes.
const WhatSampleTextBytes = "sample_text_bytes"

// SampleRole is who authored a sampling message. The zero value is not a role:
// a message this module cannot attribute is refused, never guessed.
type SampleRole uint8

// The sampling message roles MCP defines.
const (
	// SampleRoleUser is a message from the user's side of the conversation.
	SampleRoleUser SampleRole = iota + 1
	// SampleRoleAssistant is a message from the model's side.
	SampleRoleAssistant

	sampleRoleSentinel // must remain last; tests derive the declared range from it
)

// sampleRoleWire maps each role to its MCP wire identifier. These are the
// protocol's own strings, so they must not change.
var sampleRoleWire = [sampleRoleSentinel]string{
	SampleRoleUser:      "user",
	SampleRoleAssistant: "assistant",
}

// String returns the role's wire identifier, or "unknown" for any value outside
// the declared range.
func (r SampleRole) String() string {
	if r < SampleRoleUser || r >= sampleRoleSentinel {
		return "unknown"
	}
	return sampleRoleWire[r]
}

// SampleMessage is one turn of a sampling conversation, bounded and detached
// from SDK memory.
type SampleMessage struct {
	// Role is always a declared role: an unrecognized one is refused at
	// conversion.
	Role SampleRole
	// Text is the message's text content.
	Text string
}

// SampleRequest is a server's request for an LLM completion, bounded and
// detached from SDK memory. Every field is server-supplied: it describes what a
// server would like completed, and authorizes nothing.
type SampleRequest struct {
	// SystemPrompt is the server's requested system prompt. The host may modify
	// or ignore it.
	SystemPrompt string
	// Messages is the conversation to complete. It is never empty: a request
	// with nothing to complete is refused at conversion.
	Messages []SampleMessage
	// MaxTokens is the server's requested completion budget, always positive.
	// It is a request, not a grant: the layer above caps it against its own
	// limit, and a server can only ever lower the ceiling, never raise it.
	MaxTokens int
}

// SampleResult is the completion the host produced.
type SampleResult struct {
	// Model names the model the host actually used, which need not be one the
	// server asked for.
	Model string
	// Text is the completion.
	Text string
	// StopReason is why generation stopped.
	StopReason string
}

// errNoSampleHandler reports a sampling request that arrived with nothing to
// serve it. It is unreachable in this module — the SDK handler is only
// registered when a callback exists (see Session.Initialize) — and it exists so
// that if that invariant is ever broken the server is refused rather than a nil
// callback invoked.
var errNoSampleHandler = errors.New("protocol: no sampling handler is installed")

// onSample converts a server's sampling request, hands it to the client's
// callback, and converts the completion back.
//
// An error returned from here becomes a JSON-RPC error to the server, which is
// the right shape for every failure it can have: a request this module refused
// to describe, a host that declined to spend, and a model that failed are all
// "no completion", and a server must learn that rather than wait.
//
//lint:ignore SA1019 supported for peers ≤2025-11-25 (SEP-2577)
func (s *Session) onSample(ctx context.Context, params *mcp.CreateMessageParams) (*mcp.CreateMessageResult, error) {
	fn := s.cfg.OnSample
	if fn == nil {
		return nil, errNoSampleHandler
	}
	req, err := FromSDKCreateMessageParams(params, s.cfg.Bounds)
	if err != nil {
		return nil, err
	}
	res, err := fn(ctx, req)
	if err != nil {
		return nil, err
	}
	return ToSDKCreateMessageResult(res, s.cfg.Bounds)
}

// FromSDKCreateMessageParams converts a sampling/createMessage request,
// bounding it.
//
// It is exported separately from onSample so a fuzzer can drive it with
// anything a server could send.
//
// Over-bound text is rejected outright rather than truncated, for the reason
// FromSDKElicitParams gives about a prompt: this text is not read, it is *acted
// on*. A truncated conversation is a conversation whose meaning this module
// silently altered, and the completion of it would be the answer to a question
// nobody asked.
//
//lint:ignore SA1019 supported for peers ≤2025-11-25 (SEP-2577)
func FromSDKCreateMessageParams(params *mcp.CreateMessageParams, b Bounds) (SampleRequest, error) {
	if params == nil {
		return SampleRequest{}, fmt.Errorf("%w: sampling/createMessage params", errNilInput)
	}
	if len(params.Messages) == 0 {
		return SampleRequest{}, errors.New("protocol: sampling request has no messages")
	}

	tokens, err := sampleMaxTokens(params.MaxTokens)
	if err != nil {
		return SampleRequest{}, err
	}

	// One budget for the whole conversation, not one per message. A per-message
	// bound with no total is not a bound: a server would simply send more
	// messages. The wire frame limit caps how much can arrive at all; this caps
	// how much of it this module will retain and hand to a model.
	total := len(params.SystemPrompt)
	if total > b.MaxTextBytes {
		return SampleRequest{}, fmt.Errorf("sampling system prompt: %d bytes: %w", total,
			&limits.OverLimitError{What: WhatSampleTextBytes, Limit: b.MaxTextBytes})
	}

	msgs := make([]SampleMessage, 0, len(params.Messages))
	for i, m := range params.Messages {
		if m == nil {
			return SampleRequest{}, fmt.Errorf("%w: sampling message %d", errNilInput, i)
		}
		role, err := sampleRole(m.Role)
		if err != nil {
			return SampleRequest{}, fmt.Errorf("sampling message %d: %w", i, err)
		}
		text, err := sampleText(m.Content)
		if err != nil {
			return SampleRequest{}, fmt.Errorf("sampling message %d: %w", i, err)
		}
		total += len(text)
		if total > b.MaxTextBytes {
			return SampleRequest{}, fmt.Errorf("sampling conversation: over %d bytes at message %d: %w",
				b.MaxTextBytes, i, &limits.OverLimitError{What: WhatSampleTextBytes, Limit: b.MaxTextBytes})
		}
		msgs = append(msgs, SampleMessage{Role: role, Text: text})
	}

	return SampleRequest{
		SystemPrompt: params.SystemPrompt,
		Messages:     msgs,
		MaxTokens:    tokens,
	}, nil
}

// sampleMaxTokens narrows the wire's int64 budget to an int.
//
// A non-positive budget is refused rather than defaulted: maxTokens is a
// required field, and a request to sample "at most zero tokens" is not
// something this module can honor or usefully guess at. An oversized one is
// clamped to MaxInt rather than refused — it is still just a request, and the
// layer above caps it against the host's own limit, which is what actually
// decides the budget.
func sampleMaxTokens(n int64) (int, error) {
	if n <= 0 {
		return 0, fmt.Errorf("protocol: sampling request has non-positive maxTokens (%d)", n)
	}
	if n > math.MaxInt {
		return math.MaxInt, nil
	}
	return int(n), nil
}

// sampleRole maps the wire role onto the enum. An unrecognized one is refused:
// the role says who is speaking, and a module that guessed would be handing a
// model a conversation it has misattributed.
func sampleRole(role mcp.Role) (SampleRole, error) {
	for r := SampleRoleUser; r < sampleRoleSentinel; r++ {
		if sampleRoleWire[r] == string(role) {
			return r, nil
		}
	}
	return 0, fmt.Errorf("protocol: unsupported sampling role %q", string(role))
}

// sampleText extracts a message's text, refusing every other content kind.
//
// This module's SampleMessage carries text and nothing else, so a non-text
// message has no honest conversion: dropping it would change the conversation,
// and rendering it as a placeholder would put words in a speaker's mouth.
// Refusing tells the server exactly what happened.
func sampleText(c mcp.Content) (string, error) {
	switch v := c.(type) {
	case *mcp.TextContent:
		if v == nil {
			return "", fmt.Errorf("%w: text content", errNilInput)
		}
		return v.Text, nil
	case nil:
		return "", fmt.Errorf("%w: message content", errNilInput)
	default:
		return "", fmt.Errorf("protocol: sampling supports text content only, got %T", c)
	}
}

// ToSDKCreateMessageResult converts the host's completion into the SDK's
// result.
//
// The output is bounded too, and against the same budget as the input. This is
// the host's own text rather than a server's, so it is not untrusted — but a
// bound that only ever runs one way is not a bound on the conversation, and the
// server on the other end of this reply is entitled to the same protection from
// an unbounded payload that this module gives itself.
//
//lint:ignore SA1019 supported for peers ≤2025-11-25 (SEP-2577)
func ToSDKCreateMessageResult(r SampleResult, b Bounds) (*mcp.CreateMessageResult, error) {
	if r.Model == "" {
		// The model that ran is part of the answer, per MCP: the server is told
		// what completed its request. An empty one would be this module
		// asserting a fact it does not have.
		return nil, errors.New("protocol: sampling result names no model")
	}
	if len(r.Text) > b.MaxTextBytes {
		return nil, fmt.Errorf("sampling completion: %d bytes: %w", len(r.Text),
			&limits.OverLimitError{What: WhatSampleTextBytes, Limit: b.MaxTextBytes})
	}
	//lint:ignore SA1019 supported for peers ≤2025-11-25 (SEP-2577)
	return &mcp.CreateMessageResult{
		Content:    &mcp.TextContent{Text: r.Text},
		Model:      r.Model,
		Role:       mcp.Role(sampleRoleWire[SampleRoleAssistant]),
		StopReason: r.StopReason,
	}, nil
}
