package openairesponses

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"

	"github.com/looprig/core/content"
	"github.com/looprig/inference"
	codec "github.com/looprig/inference/codec"
	"github.com/looprig/inference/internal/jsonstrict"
	model "github.com/looprig/inference/model"
	"github.com/looprig/inference/wire/jsonbody"
)

// pathResponses is the native Responses API route this codec recognizes.
const pathResponses = "/v1/responses"

// matchResponsesRequest reports whether req is a POST /v1/responses request.
// It does not inspect the body or Content-Type; DecodeRequest owns that
// rejection.
func matchResponsesRequest(req *http.Request) bool {
	return req.Method == http.MethodPost && req.URL.Path == pathResponses
}

// decodeResponsesRequest decodes a matched POST /v1/responses request into a
// codec.DecodedRequest. Request.Model is left at its zero value: the harness
// alias travels only in RequestedModel, and resolving it to a real Target is
// the gateway's job (this codec has no routing table).
func decodeResponsesRequest(req *http.Request) (codec.DecodedRequest, error) {
	body, err := readJSONBody(req)
	if err != nil {
		return codec.DecodedRequest{}, err
	}
	r, requestedModel, streaming, err := decodeResponsesBody(body)
	if err != nil {
		return codec.DecodedRequest{}, err
	}
	return codec.DecodedRequest{Request: r, RequestedModel: requestedModel, Streaming: streaming}, nil
}

func readJSONBody(req *http.Request) ([]byte, error) {
	if err := checkJSONContentType(req); err != nil {
		return nil, err
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, &ServerDecodeError{Reason: "read_body", Detail: err.Error()}
	}
	return body, nil
}

func checkJSONContentType(req *http.Request) error {
	ct := req.Header.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(ct)
	if err != nil || mediaType != jsonbody.ContentType {
		return &ServerDecodeError{Reason: "unsupported_content_type", Detail: ct}
	}
	return nil
}

// wireDecodeRequest is the server-decode-direction wire form of the
// Responses request body. It reuses wireItem/wireTool/wireText/
// wireTextFormat/wireReasoningConfig (types.go) wherever they losslessly
// model the same shape; ToolChoice is decode-only json.RawMessage so both
// the object and "none" forms real clients may send can be classified and
// rejected rather than failing an opaque JSON-shape error. ParallelToolCalls,
// Include, and Metadata are decoded and then intentionally dropped: they are
// documented benign fields, matching the design's cache_control/metadata
// precedent for the Anthropic codec. PreviousResponseID is decoded only to
// detect and reject it: server-stored Responses conversations are explicitly
// out of scope, and honoring the field while silently ignoring it would give
// a harness materially wrong (stateless) behavior it did not ask for.
type wireDecodeRequest struct {
	Model              string               `json:"model"`
	Instructions       string               `json:"instructions"`
	Input              []wireItem           `json:"input"`
	Tools              []wireTool           `json:"tools"`
	ToolChoice         json.RawMessage      `json:"tool_choice"`
	ParallelToolCalls  json.RawMessage      `json:"parallel_tool_calls"`
	MaxOutputTokens    *int                 `json:"max_output_tokens"`
	Temperature        *float64             `json:"temperature"`
	TopP               *float64             `json:"top_p"`
	Text               *wireText            `json:"text"`
	Reasoning          *wireReasoningConfig `json:"reasoning"`
	Include            []string             `json:"include"`
	Store              *bool                `json:"store"`
	Stream             bool                 `json:"stream"`
	Metadata           json.RawMessage      `json:"metadata"`
	PreviousResponseID json.RawMessage      `json:"previous_response_id"`
}

// decodeResponsesBody is the shared semantic decode core behind
// decodeResponsesRequest. It enforces unique object keys, strict field
// recognition (DisallowUnknownFields, so any unsupported wire field fails
// closed rather than being silently dropped — except the documented benign
// fields above), and maps the wire shape into a provider-neutral
// inference.Request. It never resolves Model — only the string alias is
// returned.
func decodeResponsesBody(raw []byte) (req inference.Request, requestedModel string, streaming bool, err error) {
	if dupErr := rejectDuplicateObjectKeys(raw); dupErr != nil {
		return inference.Request{}, "", false, dupErr
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var wire wireDecodeRequest
	if err := dec.Decode(&wire); err != nil {
		return inference.Request{}, "", false, &ServerDecodeError{Reason: "malformed_body", Detail: err.Error()}
	}
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return inference.Request{}, "", false, &ServerDecodeError{Reason: "trailing_data"}
	}

	if wire.Model == "" {
		return inference.Request{}, "", false, &ServerDecodeError{Reason: "missing_model"}
	}
	if isPresentNonNull(wire.PreviousResponseID) {
		return inference.Request{}, "", false, &ServerDecodeError{Reason: "unsupported_previous_response_id"}
	}
	if wire.Store != nil && *wire.Store {
		return inference.Request{}, "", false, &ServerDecodeError{Reason: "unsupported_store_true"}
	}

	messages, err := decodeInputItems(wire.Input)
	if err != nil {
		return inference.Request{}, "", false, err
	}

	var tools []inference.Tool
	for _, t := range wire.Tools {
		if t.Type != toolTypeFunction {
			return inference.Request{}, "", false, &ServerDecodeError{Reason: "unsupported_tool_type", Detail: t.Type}
		}
		tools = append(tools, inference.Tool{Name: t.Name, Description: t.Description, Schema: t.Parameters})
	}

	toolChoiceValue, err := decodeToolChoice(wire.ToolChoice)
	if err != nil {
		return inference.Request{}, "", false, err
	}

	sampling := model.Sampling{}
	if wire.MaxOutputTokens != nil {
		sampling.MaxTokens = wire.MaxOutputTokens
	}
	if wire.Temperature != nil {
		sampling.Temperature = wire.Temperature
	}
	if wire.TopP != nil {
		sampling.TopP = wire.TopP
	}
	if wire.Reasoning != nil && wire.Reasoning.Effort != "" {
		effort, err := parseEffort(wire.Reasoning.Effort)
		if err != nil {
			return inference.Request{}, "", false, err
		}
		sampling.Effort = effort
	}

	output, err := decodeOutputSchemaConfig(wire.Text)
	if err != nil {
		return inference.Request{}, "", false, err
	}

	req = inference.Request{
		System:     wire.Instructions,
		Messages:   messages,
		Tools:      tools,
		Output:     output,
		ToolChoice: toolChoiceValue,
		Override:   &sampling,
	}
	return req, wire.Model, wire.Stream, nil
}

func isPresentNonNull(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && string(trimmed) != "null"
}

// decodeToolChoice maps the wire tool_choice to the two-state neutral
// inference.ToolChoice. Responses' "none" choice and the named-function
// object form are real behavior the neutral vocabulary cannot represent, so
// both fail closed rather than silently degrading to auto/required.
func decodeToolChoice(raw json.RawMessage) (inference.ToolChoice, error) {
	if !isPresentNonNull(raw) {
		return inference.ToolChoiceAuto, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return inference.ToolChoiceAuto, &ServerDecodeError{Reason: "unsupported_tool_choice", Detail: "object form"}
	}
	switch s {
	case toolChoiceAuto:
		return inference.ToolChoiceAuto, nil
	case toolChoiceRequired:
		return inference.ToolChoiceRequired, nil
	default:
		return inference.ToolChoiceAuto, &ServerDecodeError{Reason: "unsupported_tool_choice", Detail: s}
	}
}

// parseEffort maps the wire reasoning.effort value to the neutral
// model.Effort, inverting effortValue (encode.go).
func parseEffort(wire string) (model.Effort, error) {
	switch wire {
	case "low":
		return model.EffortLow, nil
	case "medium":
		return model.EffortMedium, nil
	case "high":
		return model.EffortHigh, nil
	default:
		return model.EffortNone, &ServerDecodeError{Reason: "unsupported_effort", Detail: wire}
	}
}

// decodeOutputSchemaConfig maps the wire `text.format` to a neutral
// OutputSchema. A "text" (or absent) format means plain, unstructured output.
func decodeOutputSchemaConfig(text *wireText) (*inference.OutputSchema, error) {
	if text == nil || text.Format == nil {
		return nil, nil
	}
	switch text.Format.Type {
	case "", textFormatPlainText:
		return nil, nil
	case textFormatJSONSchema:
		return &inference.OutputSchema{
			Name:   text.Format.Name,
			Schema: text.Format.Schema,
			Strict: text.Format.Strict,
		}, nil
	default:
		return nil, &ServerDecodeError{Reason: "unsupported_text_format", Detail: text.Format.Type}
	}
}

// decodeInputItems maps the wire `input` array to neutral Conversation turns.
// Unlike Anthropic's content-blocks-in-one-message shape, Responses
// represents one logical assistant turn (text + tool calls + reasoning) as
// SEVERAL sibling top-level items; this groups consecutive assistant-owned
// items (message[role=assistant], function_call, reasoning) back into a
// single neutral AIMessage, the same way anthropicapi's decodeUserMessage
// groups tool_result blocks the other direction.
func decodeInputItems(items []wireItem) ([]content.Conversation, error) {
	var out []content.Conversation
	var pendingAI []content.Block

	flushAI := func() {
		if len(pendingAI) > 0 {
			out = append(out, &content.AIMessage{
				Message: content.Message{Role: content.RoleAssistant, Blocks: pendingAI},
			})
			pendingAI = nil
		}
	}

	for _, item := range items {
		switch item.Type {
		case itemTypeMessage:
			blocks, conv, err := decodeMessageItem(item)
			if err != nil {
				return nil, err
			}
			if conv != nil {
				flushAI()
				out = append(out, conv)
				continue
			}
			pendingAI = append(pendingAI, blocks...)

		case itemTypeFunctionCall:
			if item.CallID == "" {
				return nil, &ServerDecodeError{Reason: "missing_call_id"}
			}
			args, err := decodeFunctionCallArgumentsStrict(item.Arguments)
			if err != nil {
				return nil, err
			}
			pendingAI = append(pendingAI, &content.ToolUseBlock{ID: item.CallID, Name: item.Name, Input: args})

		case itemTypeReasoning:
			pendingAI = append(pendingAI, decodeReasoningItem(item.Summary, item.EncryptedContent))

		case itemTypeFunctionCallOutput:
			flushAI()
			if item.CallID == "" {
				return nil, &ServerDecodeError{Reason: "missing_call_id"}
			}
			out = append(out, &content.ToolResultMessage{
				Message:   content.Message{Role: content.RoleUser, Blocks: []content.Block{&content.TextBlock{Text: item.Output}}},
				ToolUseID: item.CallID,
			})

		default:
			return nil, &ServerDecodeError{Reason: "unsupported_item_type", Detail: item.Type}
		}
	}
	flushAI()
	return out, nil
}

// decodeMessageItem decodes one `message` input item. A user/system/developer
// role produces a complete Conversation turn directly (returned as conv); an
// assistant role instead returns its decoded blocks for the caller to fold
// into the current pending AIMessage group.
func decodeMessageItem(item wireItem) (blocks []content.Block, conv content.Conversation, err error) {
	switch item.Role {
	case roleUser:
		b, err := decodeUserContentParts(item.Content)
		if err != nil {
			return nil, nil, err
		}
		return nil, &content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: b}}, nil
	case roleSystem, roleDeveloper:
		b, err := decodeSystemContentParts(item.Content)
		if err != nil {
			return nil, nil, err
		}
		return nil, &content.SystemMessage{Message: content.Message{Role: content.RoleSystem, Blocks: b}}, nil
	case roleAssistant:
		b, err := decodeAssistantContentParts(item.Content)
		if err != nil {
			return nil, nil, err
		}
		return b, nil, nil
	default:
		return nil, nil, &ServerDecodeError{Reason: "unsupported_role", Detail: item.Role}
	}
}

func decodeUserContentParts(parts []wireContentPart) ([]content.Block, error) {
	out := make([]content.Block, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case contentTypeInputText:
			out = append(out, &content.TextBlock{Text: p.Text})
		case contentTypeInputImage:
			img, err := decodeImageURL(p.ImageURL)
			if err != nil {
				return nil, err
			}
			out = append(out, img)
		default:
			return nil, &ServerDecodeError{Reason: "unsupported_content_part_type", Detail: p.Type}
		}
	}
	return out, nil
}

func decodeSystemContentParts(parts []wireContentPart) ([]content.Block, error) {
	out := make([]content.Block, 0, len(parts))
	for _, p := range parts {
		if p.Type != contentTypeInputText {
			return nil, &ServerDecodeError{Reason: "unsupported_content_part_type", Detail: p.Type}
		}
		out = append(out, &content.TextBlock{Text: p.Text})
	}
	return out, nil
}

func decodeAssistantContentParts(parts []wireContentPart) ([]content.Block, error) {
	out := make([]content.Block, 0, len(parts))
	for _, p := range parts {
		if p.Type != contentTypeOutputText {
			return nil, &ServerDecodeError{Reason: "unsupported_content_part_type", Detail: p.Type}
		}
		out = append(out, &content.TextBlock{Text: p.Text})
	}
	return out, nil
}

// decodeFunctionCallArgumentsStrict is the untrusted-input counterpart to
// decodeFunctionCallArguments (decode.go, which trusts an upstream provider
// response): a harness-supplied `arguments` string must be valid JSON, so a
// malformed value fails closed rather than becoming a corrupt
// ToolUseBlock.Input.
func decodeFunctionCallArgumentsStrict(raw string) (json.RawMessage, error) {
	if raw == "" {
		return json.RawMessage(emptyObject), nil
	}
	if !json.Valid([]byte(raw)) {
		return nil, &ServerDecodeError{Reason: "invalid_function_call_arguments"}
	}
	return json.RawMessage(raw), nil
}

// --- duplicate JSON object key detection -----------------------------------
//
// The actual scan lives in internal/jsonstrict, shared by every codec/*api
// dialect's server-decode path (extracted once a fourth identical copy of
// this logic appeared — see that package's doc comment). This wrapper only
// translates jsonstrict's dialect-neutral error types to this package's own
// ServerDecodeError/DuplicateKeyError, so callers and existing tests see no
// change in behavior.

// rejectDuplicateObjectKeys reports the first duplicate object member name
// found anywhere in raw (at any nesting depth), or nil if raw has none. A
// JSON syntax error is also propagated as an error: it is not this
// function's job to validate JSON, but it must never silently accept a body
// it cannot fully walk.
func rejectDuplicateObjectKeys(raw []byte) error {
	switch err := jsonstrict.RejectDuplicateKeys(raw).(type) {
	case nil:
		return nil
	case *jsonstrict.DuplicateKeyError:
		return &DuplicateKeyError{Key: err.Key}
	case *jsonstrict.MalformedError:
		return &ServerDecodeError{Reason: "malformed_body", Detail: err.Detail}
	default:
		return err
	}
}
