package openairesponses

import (
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/looprig/core/content"
	"github.com/looprig/inference"
	stream "github.com/looprig/inference/stream"
	"github.com/looprig/inference/wire/jsonbody"
)

// encodeWireResponse is the server-ENCODE-direction wire form of a
// non-streaming Responses API response. It deliberately does NOT reuse
// wireResponse (types.go): that backs the existing client-DECODE direction
// and carries Usage as *wireUsage, whose usagenorm.Count fields hold only an
// unexported raw-JSON capture with no MarshalJSON — they can decode a real
// count but cannot encode one. Output DOES reuse wireItem, which is fully
// marshal-safe.
type encodeWireResponse struct {
	ID                string                 `json:"id"`
	Status            string                 `json:"status"`
	Model             string                 `json:"model"`
	Output            []wireItem             `json:"output"`
	Usage             *encodeWireUsage       `json:"usage,omitempty"`
	IncompleteDetails *wireIncompleteDetails `json:"incomplete_details,omitempty"`
	Error             *wireResponseError     `json:"error,omitempty"`
}

// encodeWireUsage is the encode-direction counterpart to wireUsage: plain
// exported uint64 fields that json.Marshal can actually serialize.
type encodeWireUsage struct {
	InputTokens         uint64                   `json:"input_tokens"`
	OutputTokens        uint64                   `json:"output_tokens"`
	TotalTokens         uint64                   `json:"total_tokens"`
	InputTokensDetails  encodeInputTokensDetail  `json:"input_tokens_details"`
	OutputTokensDetails encodeOutputTokensDetail `json:"output_tokens_details"`
}

type encodeInputTokensDetail struct {
	CachedTokens uint64 `json:"cached_tokens"`
}

type encodeOutputTokensDetail struct {
	ReasoningTokens uint64 `json:"reasoning_tokens"`
}

// writeResponsesResponse encodes a complete inference.Response as the native
// Responses API non-streaming response and writes a 200 with it.
func writeResponsesResponse(w http.ResponseWriter, resp *inference.Response) error {
	wire, err := buildWireResponse(resp)
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

func buildWireResponse(resp *inference.Response) (encodeWireResponse, error) {
	if resp == nil {
		resp = &inference.Response{}
	}
	ids := newToolIDGenerator()

	var output []wireItem
	if resp.Message != nil {
		items, err := blocksToItems(resp.Message.Blocks, ids)
		if err != nil {
			return encodeWireResponse{}, err
		}
		output = items
	}

	status, incomplete := statusForFinishReason(resp.FinishReason)

	return encodeWireResponse{
		ID:                "resp_" + randomHex(12),
		Status:            status,
		Model:             resp.Model,
		Output:            output,
		Usage:             encodeUsage(resp.Usage),
		IncompleteDetails: incomplete,
	}, nil
}

// statusForFinishReason maps the neutral stream.FinishReason to Responses'
// status (plus incomplete_details when applicable), inverting
// deriveFinishReason (decode.go)'s status/incomplete_details half. There is
// no wire status corresponding to FinishReasonContentFilter or
// FinishReasonUnknown, so both — like FinishReasonStop and
// FinishReasonToolUse — encode as "completed": Responses has no per-status
// content-filter distinction the way an Anthropic stop_reason does, and any
// response reaching this encoder (as opposed to WriteError/Fail) already
// succeeded, so "completed" is the least presumptive terminal status.
func statusForFinishReason(r stream.FinishReason) (string, *wireIncompleteDetails) {
	if r == stream.FinishReasonLength {
		return statusIncomplete, &wireIncompleteDetails{Reason: incompleteReasonMaxOutputTokens}
	}
	return statusCompleted, nil
}

// encodeUsage inverts normalizeUsage (decode.go): input_tokens is
// reconstructed as the gross prompt total (neutral InputTokens plus
// CacheReadTokens), matching how Responses reports it, and
// CacheCreationTokens — which Responses has no wire field for, since its
// caching is fully automatic — is folded into the gross total rather than
// silently dropped from the reported count.
func encodeUsage(u *content.Usage) *encodeWireUsage {
	if u == nil {
		return nil
	}
	grossInput := uint64(u.InputTokens) + uint64(u.CacheReadTokens) + uint64(u.CacheCreationTokens)
	total := grossInput + uint64(u.OutputTokens)
	return &encodeWireUsage{
		InputTokens:         grossInput,
		OutputTokens:        uint64(u.OutputTokens),
		TotalTokens:         total,
		InputTokensDetails:  encodeInputTokensDetail{CachedTokens: uint64(u.CacheReadTokens)},
		OutputTokensDetails: encodeOutputTokensDetail{ReasoningTokens: uint64(u.ReasoningTokens)},
	}
}

// --- id synthesis -------------------------------------------------------

// newToolIDGenerator returns a closure that yields fresh, collision-resistant,
// call-scoped synthetic ids (used for both function_call ids and response/
// item ids). It combines a per-call random prefix (guarding against
// collision across different responses/streams) with a monotonic counter
// (guarding against collision within one response/stream, even with zero
// entropy available). A cross-dialect upstream target might not supply a
// tool-call id at all.
func newToolIDGenerator() func() string {
	prefix := randomHex(6)
	counter := 0
	return func() string {
		counter++
		return fmt.Sprintf("fc_gw_%s_%d", prefix, counter)
	}
}

// randomHex returns n random bytes hex-encoded. On the practically-impossible
// event crypto/rand fails, it falls back to a fixed all-zero value rather than
// panicking or failing the response.
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := crand.Read(b); err != nil {
		return strings.Repeat("0", n*2)
	}
	return hex.EncodeToString(b)
}
