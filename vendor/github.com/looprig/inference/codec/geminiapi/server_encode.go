package geminiapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/looprig/core/content"
	"github.com/looprig/inference"
	failure "github.com/looprig/inference/failure"
	stream "github.com/looprig/inference/stream"
	"github.com/looprig/inference/wire/jsonbody"
)

// encodeGenerateContentResponse is the server-ENCODE-direction wire form of a
// non-streaming generateContent response. It deliberately does NOT reuse
// GenerateContentResponse's usageMetadata (types.go): that backs the
// existing client-DECODE direction and carries usage as usagenorm.Count
// fields, which hold only an unexported raw-JSON capture with no
// MarshalJSON — they can decode a real count but cannot encode one.
// Candidates' `content` field DOES reuse geminiContent/geminiPart directly:
// unlike usageMetadata, those are plain marshal-safe types shared by both
// directions (mirroring how openairesponses' wireItem is reused for both its
// decode and encode directions).
type encodeGenerateContentResponse struct {
	Candidates    []encodeCandidate    `json:"candidates"`
	UsageMetadata *encodeUsageMetadata `json:"usageMetadata,omitempty"`
	ModelVersion  string               `json:"modelVersion,omitempty"`
}

type encodeCandidate struct {
	Content      geminiContent `json:"content"`
	FinishReason string        `json:"finishReason,omitempty"`
	Index        int           `json:"index"`
}

type encodeUsageMetadata struct {
	PromptTokenCount        uint64 `json:"promptTokenCount"`
	CandidatesTokenCount    uint64 `json:"candidatesTokenCount"`
	CachedContentTokenCount uint64 `json:"cachedContentTokenCount,omitempty"`
	ThoughtsTokenCount      uint64 `json:"thoughtsTokenCount,omitempty"`
	TotalTokenCount         uint64 `json:"totalTokenCount"`
}

// writeGenerateContentResponse encodes a complete inference.Response as the
// native generateContent non-streaming response and writes a 200 with it.
func writeGenerateContentResponse(w http.ResponseWriter, resp *inference.Response) error {
	wire, err := buildGenerateContentResponse(resp)
	if err != nil {
		return err
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", jsonbody.ContentType)
	w.WriteHeader(http.StatusOK)
	_, err = w.Write(body)
	return err
}

func buildGenerateContentResponse(resp *inference.Response) (encodeGenerateContentResponse, error) {
	if resp == nil {
		resp = &inference.Response{}
	}
	var parts []geminiPart
	if resp.Message != nil {
		p, err := buildResponseParts(resp.Message.Blocks)
		if err != nil {
			return encodeGenerateContentResponse{}, err
		}
		parts = p
	}
	return encodeGenerateContentResponse{
		Candidates: []encodeCandidate{{
			Content:      geminiContent{Role: roleModel, Parts: parts},
			FinishReason: encodeFinishReason(resp.FinishReason),
			Index:        0,
		}},
		UsageMetadata: buildUsageMetadata(resp.Usage),
		ModelVersion:  resp.Model,
	}, nil
}

// buildResponseParts maps a slice of neutral content blocks to Gemini parts,
// mirroring buildBlocks' (decode.go) order in reverse: thinking, then text,
// then tool calls. Unlike the OUTBOUND request encoder (encode.go's
// encodeAIParts, which drops a signature-less ThinkingBlock because a real
// Gemini target requires an exact replay signature), this is the
// harness-facing RESPONSE direction: the harness is not Gemini itself and
// has no such replay requirement, so a thought part is always written when
// there is visible thinking text or a signature to carry — dropping visible
// reasoning here would lose information the harness should see. thoughtSignature
// is populated straight from ProviderState when present (e.g. carried
// through from a real Gemini target via the now-fixed client-decode
// direction, decode.go's buildBlocks) and omitted (via the wire field's
// omitempty) otherwise.
func buildResponseParts(blocks []content.Block) ([]geminiPart, error) {
	parts := make([]geminiPart, 0, len(blocks))
	for _, b := range blocks {
		switch b := b.(type) {
		case *content.ThinkingBlock:
			var sig string
			if b.ReplayableAs(providerStateFormatGemini) {
				s, err := providerStateToThoughtSignature(b.ProviderState)
				if err != nil {
					return nil, err
				}
				sig = s
			}
			parts = append(parts, geminiPart{Thought: true, Text: b.Thinking, ThoughtSignature: sig})
		case *content.TextBlock:
			parts = append(parts, geminiPart{Text: b.Text})
		case *content.ToolUseBlock:
			parts = append(parts, geminiPart{FunctionCall: &functionCall{ID: b.ID, Name: b.Name, Args: argsJSON(b.Input)}})
		default:
			return nil, &UnsupportedBlockError{Block: fmt.Sprintf("%T", b)}
		}
	}
	return parts, nil
}

// encodeFinishReason maps the neutral stream.FinishReason to Gemini's
// finishReason, inverting mapFinishReason (stream.go). FinishReasonToolUse
// has no dedicated wire value — real Gemini reports "STOP" (or an empty
// reason) even when the candidate carries a functionCall, which is exactly
// what responseFinishReason (decode.go) and the stream collector (stream.go)
// already upgrade back to FinishReasonToolUse by detecting the functionCall
// part itself, so this mapping stays consistent with a same-dialect round
// trip. FinishReasonContentFilter picks "SAFETY" as the representative wire
// value among Gemini's several safety-family reasons (mapFinishReason's
// decode direction folds all of them down to this one neutral value, so the
// choice among them on encode is inherently lossy and documented).
// FinishReasonUnknown encodes as "" (omitted, via the wire field's
// omitempty), which mapFinishReason's own default case decodes back to
// FinishReasonUnknown, keeping this a lossless round trip for that case.
func encodeFinishReason(r stream.FinishReason) string {
	switch r {
	case stream.FinishReasonStop, stream.FinishReasonToolUse:
		return "STOP"
	case stream.FinishReasonLength:
		return "MAX_TOKENS"
	case stream.FinishReasonContentFilter:
		return "SAFETY"
	default:
		return ""
	}
}

// buildUsageMetadata inverts normalizeUsage/normalizeInputUsage/
// normalizeOutputUsage (decode.go): promptTokenCount is reconstructed as the
// gross prompt total (neutral InputTokens plus CacheReadTokens — Gemini has
// no cache-CREATION wire concept, so CacheCreationTokens has nowhere to go
// and is not represented), and candidatesTokenCount/thoughtsTokenCount are
// split back apart from the neutral OutputTokens total (which
// normalizeOutputUsage's decode direction computed as their ADDITIVE sum,
// unlike openaiapi's chat completions where reasoning is a subset already
// included in the gross total).
func buildUsageMetadata(u *content.Usage) *encodeUsageMetadata {
	if u == nil {
		return nil
	}
	prompt := uint64(u.InputTokens) + uint64(u.CacheReadTokens)
	thoughts := uint64(u.ReasoningTokens)
	candidates := subtractClampedUint64(uint64(u.OutputTokens), thoughts)
	total := prompt + candidates + thoughts
	return &encodeUsageMetadata{
		PromptTokenCount:        prompt,
		CandidatesTokenCount:    candidates,
		CachedContentTokenCount: uint64(u.CacheReadTokens),
		ThoughtsTokenCount:      thoughts,
		TotalTokenCount:         total,
	}
}

// subtractClampedUint64 returns a-b, clamped to 0 rather than wrapping
// around when b > a (defensive: a Usage that reached this encoder should
// already be internally consistent, but this must never panic or produce a
// nonsensical huge count).
func subtractClampedUint64(a, b uint64) uint64 {
	if b > a {
		return 0
	}
	return a - b
}

// --- native error envelope --------------------------------------------------

// geminiErrorEnvelope is this codec's own best-effort choice for the native
// Gemini error shape (`{"error":{"code":...,"message":...,"status":...}}`,
// matching Google's real documented error envelope); it is not verified
// against a live endpoint, see the package doc.
type geminiErrorEnvelope struct {
	Error geminiErrorBody `json:"error"`
}

type geminiErrorBody struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status,omitempty"`
}

// writeGenerateContentError encodes err as a native Gemini error envelope
// and writes it with the classified HTTP status. It never panics, regardless
// of err's concrete type (including nil).
func writeGenerateContentError(w http.ResponseWriter, err error) {
	status, wireStatus, message := classifyGeminiError(err)
	body, marshalErr := json.Marshal(geminiErrorEnvelope{Error: geminiErrorBody{Code: status, Message: message, Status: wireStatus}})
	if marshalErr != nil {
		// Marshaling a plain string/int-only struct cannot realistically
		// fail, but WriteError must never panic: fall back to a fixed, valid
		// envelope.
		body = []byte(`{"error":{"code":500,"message":"internal encode error"}}`)
	}
	w.Header().Set("Content-Type", jsonbody.ContentType)
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// classifyGeminiError maps an arbitrary Go error to the HTTP status and a
// short machine-readable wire status, used by both writeGenerateContentError
// (pre-header) and StreamEncoder.Fail (post-header, via the terminal
// error-shaped SSE record carrying the message half of this same
// classification). A nil err classifies as a generic 500 INTERNAL so callers
// never need a nil guard before calling this.
func classifyGeminiError(err error) (status int, wireStatus string, message string) {
	if err == nil {
		return http.StatusInternalServerError, "INTERNAL", "unknown error"
	}

	var decErr *ServerDecodeError
	if errors.As(err, &decErr) {
		return http.StatusBadRequest, "INVALID_ARGUMENT", decErr.Error()
	}
	var dupErr *DuplicateKeyError
	if errors.As(err, &dupErr) {
		return http.StatusBadRequest, "INVALID_ARGUMENT", dupErr.Error()
	}
	var blockErr *UnsupportedBlockError
	if errors.As(err, &blockErr) {
		return http.StatusBadRequest, "INVALID_ARGUMENT", blockErr.Error()
	}
	var chunkErr *UnsupportedChunkError
	if errors.As(err, &chunkErr) {
		return http.StatusBadRequest, "INVALID_ARGUMENT", chunkErr.Error()
	}
	var apiErr *failure.APIError
	if errors.As(err, &apiErr) {
		status := apiErr.Status
		if status < 100 || status > 599 {
			status = http.StatusBadGateway
		}
		return status, wireStatusForHTTPStatus(status), err.Error()
	}

	return http.StatusInternalServerError, "INTERNAL", err.Error()
}

// wireStatusForHTTPStatus maps an HTTP status to Gemini's short wire status
// string for statuses this codec did not otherwise classify from a typed
// error (e.g. an opaque upstream *failure.APIError).
func wireStatusForHTTPStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "INVALID_ARGUMENT"
	case http.StatusUnauthorized:
		return "UNAUTHENTICATED"
	case http.StatusForbidden:
		return "PERMISSION_DENIED"
	case http.StatusNotFound:
		return "NOT_FOUND"
	case http.StatusTooManyRequests:
		return "RESOURCE_EXHAUSTED"
	default:
		if status >= 500 {
			return "INTERNAL"
		}
		return "INVALID_ARGUMENT"
	}
}
