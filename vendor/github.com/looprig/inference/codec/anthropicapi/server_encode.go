package anthropicapi

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

// wireMessageResponse is the server-ENCODE-direction wire form of a
// non-streaming Messages API response. It deliberately does NOT reuse
// messageResponse/messageUsage (types.go): those back the existing
// client-DECODE direction and carry Usage as *messageUsage, whose
// usagenorm.Count fields hold only an unexported raw-JSON capture with no
// MarshalJSON — they can decode a real count but cannot encode one. Content
// blocks DO reuse anthropicBlock, which is fully marshal-safe (encode.go
// already produces request blocks with it).
type wireMessageResponse struct {
	ID         string           `json:"id"`
	Type       string           `json:"type"`
	Role       string           `json:"role"`
	Model      string           `json:"model"`
	Content    []anthropicBlock `json:"content"`
	StopReason string           `json:"stop_reason"`
	Usage      *wireUsage       `json:"usage,omitempty"`
}

// wireUsage is the encode-direction counterpart to messageUsage: plain
// exported uint64 fields that json.Marshal can actually serialize.
type wireUsage struct {
	InputTokens         uint64 `json:"input_tokens"`
	OutputTokens        uint64 `json:"output_tokens"`
	CacheReadTokens     uint64 `json:"cache_read_input_tokens,omitempty"`
	CacheCreationTokens uint64 `json:"cache_creation_input_tokens,omitempty"`
}

// writeMessageResponse encodes a complete inference.Response as the native
// Anthropic Messages API non-streaming response and writes a 200 with it.
func writeMessageResponse(w http.ResponseWriter, resp *inference.Response) error {
	wire, err := buildWireMessageResponse(resp)
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

func buildWireMessageResponse(resp *inference.Response) (wireMessageResponse, error) {
	if resp == nil {
		resp = &inference.Response{}
	}
	ids := newToolIDGenerator()

	var blocks []anthropicBlock
	var usage *content.Usage
	if resp.Message != nil {
		eb, err := encodeResponseBlocks(resp.Message.Blocks, ids)
		if err != nil {
			return wireMessageResponse{}, err
		}
		blocks = eb
	}
	usage = resp.Usage

	return wireMessageResponse{
		ID:         "msg_" + randomHex(12),
		Type:       "message",
		Role:       roleAssistant,
		Model:      resp.Model,
		Content:    blocks,
		StopReason: encodeFinishReason(resp.FinishReason),
		Usage:      encodeUsage(usage),
	}, nil
}

// encodeResponseBlocks maps neutral response blocks to their Anthropic wire
// form. It mirrors encodeBlock (encode.go, the outbound-request direction) but
// additionally synthesizes a tool_use id when the neutral block arrived with
// none: Anthropic tool_use blocks always carry a provider-issued id, but a
// same-shape response assembled from a cross-dialect target might not.
func encodeResponseBlocks(blocks []content.Block, ids func() string) ([]anthropicBlock, error) {
	out := make([]anthropicBlock, 0, len(blocks))
	for _, b := range blocks {
		eb, err := encodeResponseBlock(b, ids)
		if err != nil {
			return nil, err
		}
		out = append(out, eb)
	}
	return out, nil
}

func encodeResponseBlock(b content.Block, ids func() string) (anthropicBlock, error) {
	switch b := b.(type) {
	case *content.TextBlock:
		return anthropicBlock{Type: blockTypeText, Text: b.Text}, nil
	case *content.ThinkingBlock:
		return anthropicBlock{Type: blockTypeThinking, Thinking: b.Thinking, Signature: b.Signature}, nil
	case *content.ToolUseBlock:
		id := b.ID
		if id == "" {
			id = ids()
		}
		return anthropicBlock{Type: blockTypeToolUse, ID: id, Name: b.Name, Input: inputOrEmpty(b.Input)}, nil
	case *content.ImageBlock:
		return anthropicBlock{Type: blockTypeImage, Source: imageSourceOf(b)}, nil
	default:
		return anthropicBlock{}, &UnsupportedBlockError{Block: fmt.Sprintf("%T", b)}
	}
}

// encodeFinishReason maps the neutral stream.FinishReason to its Anthropic wire
// stop_reason, inverting mapFinishReason (stream.go). FinishReasonUnknown (and
// any value mapFinishReason itself would never have produced) falls back to
// "end_turn": Anthropic responses always carry a non-empty stop_reason, and
// "end_turn" is its least presumptive value.
func encodeFinishReason(r stream.FinishReason) string {
	switch r {
	case stream.FinishReasonStop:
		return "end_turn"
	case stream.FinishReasonLength:
		return "max_tokens"
	case stream.FinishReasonToolUse:
		return "tool_use"
	case stream.FinishReasonContentFilter:
		return "refusal"
	default:
		return "end_turn"
	}
}

func encodeUsage(u *content.Usage) *wireUsage {
	if u == nil {
		return nil
	}
	return &wireUsage{
		InputTokens:         uint64(u.InputTokens),
		OutputTokens:        uint64(u.OutputTokens),
		CacheReadTokens:     uint64(u.CacheReadTokens),
		CacheCreationTokens: uint64(u.CacheCreationTokens),
	}
}

// --- count_tokens response encoding -----------------------------------------

// countTokensResponse is the native `{"input_tokens": N}` body Anthropic's
// count_tokens endpoint returns.
type countTokensResponse struct {
	InputTokens int `json:"input_tokens"`
}

// WriteCountTokensResponse writes Anthropic's count_tokens response shape given
// an already-computed token count. It does not compute the count itself: the
// gateway resolves the target model and calls a contextcount.ContextCounter,
// then calls this helper with the result.
func WriteCountTokensResponse(w http.ResponseWriter, inputTokens int) error {
	body, err := json.Marshal(countTokensResponse{InputTokens: inputTokens})
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", jsonbody.ContentType)
	w.WriteHeader(http.StatusOK)
	_, err = w.Write(body)
	return err
}

// --- id synthesis -------------------------------------------------------

// newToolIDGenerator returns a closure that yields fresh, collision-resistant,
// call-scoped synthetic tool_use ids. It combines a per-call random prefix
// (guarding against collision across different responses/streams) with a
// monotonic counter (guarding against collision within one response/stream,
// even with zero entropy available). Anthropic requires every tool_use block to
// carry a non-empty id; a cross-dialect upstream target might not supply one.
func newToolIDGenerator() func() string {
	prefix := randomHex(6)
	counter := 0
	return func() string {
		counter++
		return fmt.Sprintf("toolu_gw_%s_%d", prefix, counter)
	}
}

// randomHex returns n random bytes hex-encoded. On the practically-impossible
// event crypto/rand fails, it falls back to a fixed all-zero value rather than
// panicking or failing the response — a ropey synthetic id is far less harmful
// than an encoder crash.
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := crand.Read(b); err != nil {
		return strings.Repeat("0", n*2)
	}
	return hex.EncodeToString(b)
}
