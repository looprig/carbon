package geminiapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/looprig/core/content"
	"github.com/looprig/inference"
	codec "github.com/looprig/inference/codec"
	"github.com/looprig/inference/internal/jsonstrict"
	model "github.com/looprig/inference/model"
	"github.com/looprig/inference/wire/jsonbody"
)

// Route constants. Unlike every other dialect in this module, Gemini's model
// name travels in the URL PATH, not the JSON body, and this one codec owns
// TWO distinct routes (non-streaming and streaming) rather than
// distinguishing streaming via a body flag on one shared route.
const (
	modelPathPrefix         = "/v1beta/models/"
	generateSuffix          = ":generateContent"
	streamGenerateSuffix    = ":streamGenerateContent"
	functionCallingModeAuto = "AUTO"

	// emptyObject is the fallback for a tool call's `args` (and, on encode,
	// an absent tool-use input): Gemini requires this to be a JSON object,
	// so an empty/absent neutral value becomes "{}".
	emptyObject = "{}"
)

// matchGenerateContentRequest reports whether req is a POST to either
// /v1beta/models/{model}:generateContent or
// /v1beta/models/{model}:streamGenerateContent. It does not inspect the
// body, Content-Type, or the `alt=sse` query parameter — see parseModelRoute
// for why the path suffix alone is this codec's routing signal.
func matchGenerateContentRequest(req *http.Request) bool {
	if req.Method != http.MethodPost {
		return false
	}
	_, _, ok := parseModelRoute(req.URL.Path)
	return ok
}

// parseModelRoute extracts {model} and the streaming flag from one of this
// codec's two routes. The streaming route's `?alt=sse` query parameter is a
// Google convention, not this codec's routing signal: the path suffix
// (:generateContent vs :streamGenerateContent) already unambiguously
// determines streaming, and Google's own docs describe alt=sse as
// conventional rather than load-bearing, so requiring its presence would
// only add a way to reject an otherwise well-formed streaming request that a
// real client sent with a different or absent alt value. req.URL.Path is
// already percent-decoded by net/http.
func parseModelRoute(path string) (modelName string, streaming bool, ok bool) {
	if !strings.HasPrefix(path, modelPathPrefix) {
		return "", false, false
	}
	rest := strings.TrimPrefix(path, modelPathPrefix)
	switch {
	case strings.HasSuffix(rest, streamGenerateSuffix):
		name := strings.TrimSuffix(rest, streamGenerateSuffix)
		if name == "" {
			return "", false, false
		}
		return name, true, true
	case strings.HasSuffix(rest, generateSuffix):
		name := strings.TrimSuffix(rest, generateSuffix)
		if name == "" {
			return "", false, false
		}
		return name, false, true
	default:
		return "", false, false
	}
}

// decodeGenerateContentRequest decodes a matched request into a
// codec.DecodedRequest. Request.Model is left at its zero value: the
// harness alias travels only in RequestedModel (extracted from the URL
// path here, not the body), and resolving it to a real Target is the
// gateway's job.
func decodeGenerateContentRequest(req *http.Request) (codec.DecodedRequest, error) {
	modelName, streaming, ok := parseModelRoute(req.URL.Path)
	if !ok {
		return codec.DecodedRequest{}, &ServerDecodeError{Reason: "unrecognized_route", Detail: req.URL.Path}
	}
	body, err := readJSONBody(req)
	if err != nil {
		return codec.DecodedRequest{}, err
	}
	r, err := decodeGenerateContentBody(body)
	if err != nil {
		return codec.DecodedRequest{}, err
	}
	return codec.DecodedRequest{Request: r, RequestedModel: modelName, Streaming: streaming}, nil
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

// decodeGenerateContentBody is the shared semantic decode core behind
// decodeGenerateContentRequest. It enforces unique object keys and strict
// field recognition (DisallowUnknownFields — decoding directly into the
// existing GenerateContentRequest wire type, since unlike the other three
// dialects Gemini's request shape is already homogeneous enough that no
// separate server-decode-only wire struct is needed). A multi-candidate
// request (the wire `candidateCount` field) is thereby rejected for free:
// GenerateContentRequest's generationConfig has no CandidateCount field, so
// DisallowUnknownFields fails it closed exactly like any other unmodeled
// field, with no dedicated "n>1"-style check required.
func decodeGenerateContentBody(raw []byte) (inference.Request, error) {
	if dupErr := rejectDuplicateObjectKeys(raw); dupErr != nil {
		return inference.Request{}, dupErr
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var wire GenerateContentRequest
	if err := dec.Decode(&wire); err != nil {
		return inference.Request{}, &ServerDecodeError{Reason: "malformed_body", Detail: err.Error()}
	}
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return inference.Request{}, &ServerDecodeError{Reason: "trailing_data"}
	}

	systemText, err := decodeSystemInstruction(wire.SystemInstruction)
	if err != nil {
		return inference.Request{}, err
	}

	messages, err := decodeContents(wire.Contents)
	if err != nil {
		return inference.Request{}, err
	}

	var tools []inference.Tool
	for _, t := range wire.Tools {
		for _, fd := range t.FunctionDeclarations {
			tools = append(tools, inference.Tool{Name: fd.Name, Description: fd.Description, Schema: fd.Parameters})
		}
	}

	toolChoiceValue, err := decodeFunctionCallingMode(wire.ToolConfig)
	if err != nil {
		return inference.Request{}, err
	}

	sampling := model.Sampling{}
	var output *inference.OutputSchema
	if wire.GenerationConfig != nil {
		gc := wire.GenerationConfig
		sampling.Temperature = gc.Temperature
		sampling.TopP = gc.TopP
		sampling.MaxTokens = gc.MaxOutputTokens
		if len(gc.StopSequences) > 0 {
			sampling.Stop = gc.StopSequences
		}
		if gc.ThinkingConfig != nil {
			effort, err := effortFromThinkingBudget(gc.ThinkingConfig.ThinkingBudget)
			if err != nil {
				return inference.Request{}, err
			}
			sampling.Effort = effort
		}
		schema, err := decodeResponseSchema(gc.ResponseMIMEType, gc.ResponseJSONSchema)
		if err != nil {
			return inference.Request{}, err
		}
		output = schema
	}

	return inference.Request{
		System:     systemText,
		Messages:   messages,
		Tools:      tools,
		Output:     output,
		ToolChoice: toolChoiceValue,
		Override:   &sampling,
	}, nil
}

// decodeFunctionCallingMode maps the wire toolConfig.functionCallingConfig.mode
// to the two-state neutral inference.ToolChoice, inverting the encoder's own
// AUTO-omitted/ANY-forced choice. Any other real Gemini mode (e.g. NONE, or
// the allowed-function-names restriction) is behavior the neutral vocabulary
// cannot represent, so it fails closed rather than silently degrading.
func decodeFunctionCallingMode(tc *toolConfig) (inference.ToolChoice, error) {
	if tc == nil || tc.FunctionCallingConfig == nil {
		return inference.ToolChoiceAuto, nil
	}
	switch tc.FunctionCallingConfig.Mode {
	case "", functionCallingModeAuto:
		return inference.ToolChoiceAuto, nil
	case functionCallingModeAny:
		return inference.ToolChoiceRequired, nil
	default:
		return inference.ToolChoiceAuto, &ServerDecodeError{Reason: "unsupported_function_calling_mode", Detail: tc.FunctionCallingConfig.Mode}
	}
}

// effortFromThinkingBudget maps the wire thinkingConfig.thinkingBudget back
// to the neutral model.Effort, inverting thinkingBudget (encode.go)'s four
// canonical values. A nil budget (thinkingConfig present but budget
// omitted) means "no explicit effort was requested" (EffortNone); any other
// value this codec's own encoder would never have produced is rejected
// rather than silently coerced to the nearest tier — a real Gemini client is
// free to request an arbitrary token budget the neutral Effort vocabulary
// cannot represent losslessly, and this is a documented, known limitation of
// the cross-dialect effort mapping, not something this task resolves.
func effortFromThinkingBudget(budget *int) (model.Effort, error) {
	if budget == nil {
		return model.EffortNone, nil
	}
	switch *budget {
	case -1:
		return model.EffortMax, nil
	case 4096:
		return model.EffortLow, nil
	case 8192:
		return model.EffortMedium, nil
	case 16384:
		return model.EffortHigh, nil
	default:
		return model.EffortNone, &ServerDecodeError{Reason: "unsupported_thinking_budget", Detail: strconv.Itoa(*budget)}
	}
}

// decodeResponseSchema maps the wire responseMimeType/responseJsonSchema
// pair to a neutral OutputSchema. An absent/empty mime type means plain
// unstructured output.
func decodeResponseSchema(mimeType string, schema json.RawMessage) (*inference.OutputSchema, error) {
	switch mimeType {
	case "":
		return nil, nil
	case responseMIMETypeJSON:
		if len(schema) == 0 {
			return nil, &ServerDecodeError{Reason: "missing_response_json_schema"}
		}
		return &inference.OutputSchema{Schema: schema}, nil
	default:
		return nil, &ServerDecodeError{Reason: "unsupported_response_mime_type", Detail: mimeType}
	}
}

// decodeSystemInstruction extracts the plain text of the top-level
// systemInstruction content. Gemini's systemInstruction is text-only in
// practice; a non-text part (image, function call, …) fails closed rather
// than being silently dropped, since this is untrusted client input.
func decodeSystemInstruction(sys *geminiContent) (string, error) {
	if sys == nil {
		return "", nil
	}
	var sb strings.Builder
	for _, p := range sys.Parts {
		if p.Text == "" || p.Thought || p.InlineData != nil || p.FileData != nil || p.FunctionCall != nil || p.FunctionResponse != nil {
			return "", &ServerDecodeError{Reason: "unsupported_system_instruction_part"}
		}
		sb.WriteString(p.Text)
	}
	return sb.String(), nil
}

// decodeContents maps the wire `contents` array to neutral Conversation
// turns: a "user" entry may expand into several turns (a functionResponse
// part becomes its own ToolResultMessage, matching the sibling dialects'
// identical splitting of a multi-purpose wire turn), and a "model" entry
// becomes one AIMessage.
func decodeContents(contents []geminiContent) ([]content.Conversation, error) {
	var out []content.Conversation
	for _, c := range contents {
		switch c.Role {
		case roleUser:
			msgs, err := decodeUserParts(c.Parts)
			if err != nil {
				return nil, err
			}
			out = append(out, msgs...)
		case roleModel:
			ai, err := decodeModelParts(c.Parts)
			if err != nil {
				return nil, err
			}
			out = append(out, ai)
		default:
			return nil, &ServerDecodeError{Reason: "unsupported_role", Detail: c.Role}
		}
	}
	return out, nil
}

// decodeUserParts splits a "user"-role content's parts into the neutral
// turns they represent: consecutive text/image parts are grouped into one
// UserMessage (preserving order), and each functionResponse part becomes its
// own ToolResultMessage, in the order both kinds appear on the wire —
// mirroring anthropicapi's decodeUserMessage splitting of tool_result blocks
// out of an otherwise-plain user turn.
func decodeUserParts(parts []geminiPart) ([]content.Conversation, error) {
	var out []content.Conversation
	var pending []content.Block

	flush := func() {
		if len(pending) > 0 {
			out = append(out, &content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: pending}})
			pending = nil
		}
	}

	for _, p := range parts {
		switch {
		case p.FunctionResponse != nil:
			flush()
			text, err := decodeFunctionResponseText(p.FunctionResponse.Response)
			if err != nil {
				return nil, err
			}
			out = append(out, &content.ToolResultMessage{
				Message:   content.Message{Role: content.RoleUser, Blocks: []content.Block{&content.TextBlock{Text: text}}},
				ToolUseID: p.FunctionResponse.ID,
			})
		case p.InlineData != nil:
			data, err := base64.StdEncoding.DecodeString(p.InlineData.Data)
			if err != nil {
				return nil, &ServerDecodeError{Reason: "invalid_image_data", Detail: err.Error()}
			}
			pending = append(pending, &content.ImageBlock{MediaType: content.MediaType(p.InlineData.MimeType), Source: content.ImageSource{Data: data}})
		case p.FileData != nil:
			// A harness-supplied fileUri is accepted as an opaque URL string,
			// same as an ImageBlock.Source.URL from any other dialect —
			// validating it is a "real" Gemini File API / gs:// / YouTube URI
			// is the outbound Gemini encoder's job when THIS decoded request
			// later gets re-encoded to a Gemini target, not this decoder's.
			pending = append(pending, &content.ImageBlock{MediaType: content.MediaType(p.FileData.MimeType), Source: content.ImageSource{URL: p.FileData.FileURI}})
		case p.Text != "" && !p.Thought:
			pending = append(pending, &content.TextBlock{Text: p.Text})
		default:
			return nil, &ServerDecodeError{Reason: "unsupported_or_empty_part"}
		}
	}
	flush()
	return out, nil
}

// decodeFunctionResponseText extracts the plain text of a functionResponse's
// `response` object. This codec's own outbound encoder always wraps tool
// result text as {"result": "<text>"} (functionResponsePayload, encode.go);
// the decode direction requires that same shape rather than accepting
// arbitrary JSON, matching the sibling dialects' text-only tool-result
// convention (openaiapi.toolResultText, anthropicapi's tool_result content).
func decodeFunctionResponseText(raw json.RawMessage) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var payload functionResponsePayload
	if err := dec.Decode(&payload); err != nil {
		return "", &ServerDecodeError{Reason: "unsupported_function_response_shape", Detail: err.Error()}
	}
	return payload.Result, nil
}

// decodeModelParts maps a "model"-role content's parts to a single
// AIMessage's blocks, in wire order: a functionCall part becomes a
// ToolUseBlock; a thought part (text and/or thoughtSignature) becomes a
// ThinkingBlock via content.NewThinkingBlock, so ProviderState is populated
// exactly like the client-decode direction's buildBlocks (decode.go); any
// other non-empty text part becomes a TextBlock. A functionCall part's own
// thoughtSignature (Gemini 2.5+ may attach one directly to the call rather
// than a separate thought part) has no home in ToolUseBlock and is a
// documented, known gap — out of this task's scope, which only covers
// thought-part signature preservation.
func decodeModelParts(parts []geminiPart) (*content.AIMessage, error) {
	var blocks []content.Block
	for _, p := range parts {
		switch {
		case p.FunctionCall != nil:
			blocks = append(blocks, &content.ToolUseBlock{ID: p.FunctionCall.ID, Name: p.FunctionCall.Name, Input: argsJSON(p.FunctionCall.Args)})
		case p.Thought && (p.Text != "" || p.ThoughtSignature != ""):
			blocks = append(blocks, content.NewThinkingBlock(p.Text, "", providerStateFromThoughtSignature(p.ThoughtSignature), providerStateFormatFor(p.ThoughtSignature)))
		case p.Text != "":
			blocks = append(blocks, &content.TextBlock{Text: p.Text})
		default:
			return nil, &ServerDecodeError{Reason: "unsupported_or_empty_part"}
		}
	}
	return &content.AIMessage{Message: content.Message{Role: content.RoleAssistant, Blocks: blocks}}, nil
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
