package compat

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// NormalizeOpenAIReasoning accepts the common provider aliases used by
// OpenAI-compatible gateways and emits the shared inference codec's canonical
// reasoning_content field. It is deliberately scoped to string aliases used in
// Chat Completions responses; Responses API reasoning has its own codec.
func NormalizeOpenAIReasoning(body []byte) ([]byte, error) {
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return nil, err
	}
	if !normalizeReasoningValue(value) {
		return body, nil
	}
	return json.Marshal(value)
}

// NormalizeOpenAIReasoningStream applies the same normalization lazily to SSE
// data frames, preserving streaming instead of buffering the provider response.
func NormalizeOpenAIReasoningStream(response *http.Response) (*http.Response, error) {
	if response == nil || response.Body == nil {
		return nil, fmt.Errorf("compat: OpenAI reasoning stream has no response body")
	}
	clone := *response
	clone.Body = &reasoningStreamBody{source: response.Body, reader: bufio.NewReader(response.Body)}
	return &clone, nil
}

func normalizeReasoningValue(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		changed := false
		if _, canonical := typed["reasoning_content"]; !canonical {
			for _, alias := range []string{"reasoning_text", "reasoning"} {
				if text, ok := typed[alias].(string); ok && text != "" {
					typed["reasoning_content"] = text
					delete(typed, alias)
					changed = true
					break
				}
			}
		}
		for _, child := range typed {
			if normalizeReasoningValue(child) {
				changed = true
			}
		}
		return changed
	case []any:
		changed := false
		for _, child := range typed {
			if normalizeReasoningValue(child) {
				changed = true
			}
		}
		return changed
	default:
		return false
	}
}

type reasoningStreamBody struct {
	source  io.ReadCloser
	reader  *bufio.Reader
	pending []byte
	err     error
}

func (b *reasoningStreamBody) Read(p []byte) (int, error) {
	for len(b.pending) == 0 && b.err == nil {
		line, err := b.reader.ReadBytes('\n')
		if len(line) > 0 {
			b.pending = normalizeSSELine(line)
		}
		if err != nil {
			b.err = err
		}
	}
	if len(b.pending) == 0 {
		return 0, b.err
	}
	n := copy(p, b.pending)
	b.pending = b.pending[n:]
	if len(b.pending) == 0 && b.err != nil {
		return n, b.err
	}
	return n, nil
}

func (b *reasoningStreamBody) Close() error { return b.source.Close() }

func normalizeSSELine(line []byte) []byte {
	if !bytes.HasPrefix(line, []byte("data: ")) {
		return line
	}
	payload := bytes.TrimSuffix(bytes.TrimSuffix(line[len("data: "):], []byte("\n")), []byte("\r"))
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return line
	}
	normalized, err := NormalizeOpenAIReasoning(payload)
	if err != nil || bytes.Equal(normalized, payload) {
		return line
	}
	suffix := []byte(nil)
	switch {
	case bytes.HasSuffix(line, []byte("\r\n")):
		suffix = []byte("\r\n")
	case bytes.HasSuffix(line, []byte("\n")):
		suffix = []byte("\n")
	}
	out := append([]byte("data: "), normalized...)
	return append(out, suffix...)
}
