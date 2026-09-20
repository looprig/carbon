//go:build integration

package browser

// This file keeps the scripted inference fixture used by the browser Factory
// integration tests. The old end-to-end tests in this file exercised Carbon's
// retired process-global serve adapter; the current browser composition is
// covered by orchestration_integration_test.go.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/inference"
	"github.com/looprig/inference/model"
	"github.com/looprig/inference/stream"
	"github.com/looprig/llm"
)

// syncBuffer is read while browser lifecycle writes its resolved address.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func waitForSubstring(t *testing.T, out *syncBuffer, want string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := out.String(); strings.Contains(got, want) {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("output never contained %q; got %q", want, out.String())
	return ""
}

// scriptedClient is a minimal inference.Client whose Stream is driven by a caller
// supplied script. It is intentionally small because these tests exercise the
// browser composition and only need deterministic model output.
type scriptedClient struct {
	mu    sync.Mutex
	calls int
	fn    func(call int, req inference.Request) []content.Chunk
}

func (c *scriptedClient) Invoke(context.Context, inference.Request) (*inference.Response, error) {
	return nil, fmt.Errorf("carbon browser test: Invoke is not scripted")
}

func (c *scriptedClient) Stream(_ context.Context, req inference.Request) (*stream.StreamReader[content.Chunk], error) {
	c.mu.Lock()
	call := c.calls
	c.calls++
	c.mu.Unlock()

	chunks := c.fn(call, req)
	index := 0
	next := func() (content.Chunk, error) {
		if index == len(chunks) {
			return nil, io.EOF
		}
		chunk := chunks[index]
		index++
		return chunk, nil
	}
	return stream.NewStreamReader(next, nil), nil
}

// testServeModel is a secret-free, tool-capable model descriptor. The scripted
// client never dials it; Carbon needs a valid model to assemble the loop
// definition and resolve its context window.
func testServeModel() model.Model {
	return model.CustomModel(
		model.ProviderName(llm.ProviderLMStudio), model.APIFormatOpenAI,
		"http://localhost:1234/v1", "carbon-browser-test",
		model.WithTools(),
		model.WithContextLimits(model.ContextLimits{WindowTokens: 128_000}),
	)
}
