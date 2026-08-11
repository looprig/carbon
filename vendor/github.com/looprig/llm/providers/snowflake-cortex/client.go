// Package snowflake provides Snowflake Cortex's documented OpenAI-compatible
// Chat Completions endpoint.
package snowflake

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/looprig/core/content"
	"github.com/looprig/inference"
	"github.com/looprig/inference/auth"
	"github.com/looprig/inference/failure"
	model "github.com/looprig/inference/model"
	"github.com/looprig/inference/stream"

	"github.com/looprig/llm"
	"github.com/looprig/llm/providers/internal/simple"
)

const accountEnvironment = "SNOWFLAKE_ACCOUNT"

type Option func(*config)

type config struct {
	account string
	options []simple.Option
}

// WithAccount sets the Snowflake account identifier used when Model.BaseURL is
// empty. It accepts the account locator/org form used in the Cortex URL.
func WithAccount(account string) Option {
	return func(c *config) { c.account = strings.TrimSpace(account) }
}

func WithHeader(name, value string) Option {
	return func(c *config) { c.options = append(c.options, simple.WithHeader(name, value)) }
}

func WithReasoningEnabled(enabled bool) Option {
	return func(c *config) { c.options = append(c.options, simple.WithReasoningEnabled(enabled)) }
}

func WithServiceTier(value string) Option {
	return func(c *config) { c.options = append(c.options, simple.WithServiceTier(value)) }
}

// New constructs a Snowflake Cortex client. Snowflake's Chat Completions API
// names the output limit max_completion_tokens; the adapter translates the
// shared OpenAI request field without changing the public inference contract.
func New(selected model.Model, key auth.APIKey, options ...Option) (inference.Client, error) {
	if key == "" {
		key = auth.APIKey(strings.TrimSpace(os.Getenv("SNOWFLAKE_CORTEX_TOKEN")))
		if key == "" {
			key = auth.APIKey(strings.TrimSpace(os.Getenv("SNOWFLAKE_CORTEX_PAT")))
		}
	}
	var cfg config
	for _, option := range options {
		if option != nil {
			option(&cfg)
		}
	}
	baseURL := strings.TrimRight(strings.TrimSpace(selected.BaseURL), "/")
	if baseURL == "" {
		account := cfg.account
		if account == "" {
			account = strings.TrimSpace(os.Getenv(accountEnvironment))
		}
		if account == "" {
			return nil, &ConfigurationError{Reason: AccountMissing}
		}
		if !validAccount(account) {
			return nil, &ConfigurationError{Reason: AccountInvalid}
		}
		baseURL = (&url.URL{
			Scheme: "https",
			Host:   account + ".snowflakecomputing.com",
			Path:   "/api/v2/cortex/v1",
		}).String()
	}
	selected.BaseURL = baseURL
	defaults := []simple.Option{
		simple.WithBodyPatch(func(body map[string]json.RawMessage) error {
			if value, ok := body["max_tokens"]; ok {
				if _, alreadySet := body["max_completion_tokens"]; !alreadySet {
					body["max_completion_tokens"] = append(json.RawMessage(nil), value...)
				}
				delete(body, "max_tokens")
			}
			return nil
		}),
	}
	defaults = append(defaults, cfg.options...)
	inner, err := simple.New(selected, key, simple.Definition{
		Provider:          llm.ProviderSnowflakeCortex,
		DefaultBaseURL:    baseURL,
		DefaultPath:       "/chat/completions",
		Authentication:    auth.AuthAPIKey,
		NormalizeResponse: normalizeEmptyAssistantRole,
		NormalizeStream:   normalizeEmptyAssistantRoleStream,
	}, defaults...)
	if err != nil {
		return nil, err
	}
	return &client{inner: inner}, nil
}

type client struct {
	inner inference.Client
}

var _ inference.Client = (*client)(nil)

func (c *client) Invoke(ctx context.Context, req inference.Request) (*inference.Response, error) {
	response, err := c.inner.Invoke(ctx, req)
	if err != nil && isConversationComplete(err) {
		return emptyConversationResponse(req.Model.Name), nil
	}
	return response, err
}

func (c *client) Stream(ctx context.Context, req inference.Request) (*stream.StreamReader[content.Chunk], error) {
	reader, err := c.inner.Stream(ctx, req)
	if err != nil && isConversationComplete(err) {
		return emptyConversationStream(req.Model.Name), nil
	}
	return reader, err
}

func isConversationComplete(err error) bool {
	var apiErr *failure.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest {
		return false
	}
	code := strings.ToLower(strings.TrimSpace(apiErr.Code))
	if code == "" {
		code = strings.ToLower(strings.TrimSpace(apiErr.ProviderCode))
	}
	return code == "conversation_complete"
}

func emptyConversationResponse(modelName string) *inference.Response {
	return &inference.Response{
		Model:        modelName,
		FinishReason: stream.FinishReasonStop,
		Message: &content.AIMessage{
			Message: content.Message{Role: content.RoleAssistant},
		},
	}
}

func emptyConversationStream(modelName string) *stream.StreamReader[content.Chunk] {
	return stream.NewStreamReaderWithResult(
		func() (content.Chunk, error) { return nil, io.EOF },
		nil,
		func() (stream.StreamResult, bool, error) {
			return stream.StreamResult{
				Model:        modelName,
				FinishReason: stream.FinishReasonStop,
			}, true, nil
		},
	)
}

func normalizeEmptyAssistantRole(body []byte) ([]byte, error) {
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return nil, err
	}
	if !normalizeEmptyAssistantRoleValue(value) {
		return body, nil
	}
	return json.Marshal(value)
}

func normalizeEmptyAssistantRoleValue(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		changed := false
		if role, ok := typed["role"].(string); ok && role == "" {
			typed["role"] = string(content.RoleAssistant)
			changed = true
		}
		for _, child := range typed {
			if normalizeEmptyAssistantRoleValue(child) {
				changed = true
			}
		}
		return changed
	case []any:
		changed := false
		for _, child := range typed {
			if normalizeEmptyAssistantRoleValue(child) {
				changed = true
			}
		}
		return changed
	default:
		return false
	}
}

func normalizeEmptyAssistantRoleStream(response *http.Response) (*http.Response, error) {
	if response == nil || response.Body == nil {
		return nil, fmt.Errorf("snowflake: response stream has no body")
	}
	clone := *response
	clone.Body = &emptyRoleStreamBody{
		source: response.Body,
		reader: bufio.NewReader(response.Body),
	}
	return &clone, nil
}

type emptyRoleStreamBody struct {
	source  io.ReadCloser
	reader  *bufio.Reader
	pending []byte
	err     error
}

func (b *emptyRoleStreamBody) Read(p []byte) (int, error) {
	for len(b.pending) == 0 && b.err == nil {
		line, err := b.reader.ReadBytes('\n')
		if len(line) > 0 {
			b.pending = normalizeEmptyRoleSSELine(line)
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

func (b *emptyRoleStreamBody) Close() error { return b.source.Close() }

func normalizeEmptyRoleSSELine(line []byte) []byte {
	if !bytes.HasPrefix(line, []byte("data: ")) {
		return line
	}
	payload := bytes.TrimSuffix(bytes.TrimSuffix(line[len("data: "):], []byte("\n")), []byte("\r"))
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return line
	}
	normalized, err := normalizeEmptyAssistantRole(payload)
	if err != nil || bytes.Equal(normalized, payload) {
		return line
	}
	var suffix []byte
	switch {
	case bytes.HasSuffix(line, []byte("\r\n")):
		suffix = []byte("\r\n")
	case bytes.HasSuffix(line, []byte("\n")):
		suffix = []byte("\n")
	}
	out := append([]byte("data: "), normalized...)
	return append(out, suffix...)
}

func validAccount(account string) bool {
	if len(account) == 0 || len(account) > 255 {
		return false
	}
	for _, label := range strings.Split(account, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[0] == '_' {
			return false
		}
		last := label[len(label)-1]
		if last == '-' || last == '_' {
			return false
		}
		for _, ch := range label {
			switch {
			case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9', ch == '-', ch == '_':
			default:
				return false
			}
		}
	}
	return true
}
