package chutes

import (
	"bytes"
	"context"
	"crypto/mlkem"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/looprig/core/content"
	"github.com/looprig/inference/codec/openaiapi"
	failure "github.com/looprig/inference/failure"
	stream "github.com/looprig/inference/stream"
	"github.com/looprig/llm/e2e"
)

// errStreamInitMissing is returned when an e2e frame arrives before the
// e2e_init event that establishes the stream key. Fail closed: we never try to
// open a frame without an authenticated, freshly-derived key.
var errStreamInitMissing = errors.New("chutes stream: e2e frame before e2e_init")

// invokeStream POSTs the sealed plaintext to /e2e/invoke with the streaming
// headers and returns the open response on a text/event-stream 2xx. Any other
// outcome returns a typed error (the body is read and closed first). It pops a
// single-use nonce; recovery/retry is intentionally omitted for the stream path
// to keep it simple (a fresh Stream call re-attests as needed).
func (c *Client) invokeStream(ctx context.Context, chuteID string, sess *attestedSession, plaintext []byte, respDK *mlkem.DecapsulationKey768) (*http.Response, error) {
	nonce, ok := sess.popNonce()
	if !ok {
		c.dropSession(chuteID)
		return nil, failure.NewAPIError(http.StatusForbidden, "forbidden", "", 0)
	}

	mlkemCT, blob, err := e2e.Seal(plaintext, sess.key, []byte("e2e-req-v1"), true)
	if err != nil {
		return nil, err
	}
	reqBody := append(mlkemCT, blob...)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiBase+"/e2e/invoke", bytes.NewReader(reqBody))
	if err != nil {
		return nil, &failure.NetworkError{Err: err}
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpReq.Header.Set("X-Chute-Id", chuteID)
	httpReq.Header.Set("X-Instance-Id", sess.instanceID)
	httpReq.Header.Set("X-E2E-Nonce", nonce)
	httpReq.Header.Set("X-E2E-Stream", "true")
	httpReq.Header.Set("X-E2E-Path", "/v1/chat/completions")
	httpReq.Header.Set("Content-Type", "application/octet-stream")
	httpReq.Header.Set("Accept", "text/event-stream")

	httpResp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, &failure.NetworkError{Err: err}
	}
	if httpResp.StatusCode/100 != 2 {
		defer httpResp.Body.Close()
		body, _ := io.ReadAll(httpResp.Body)
		// Streaming pre-stream errors can be e2e-sealed to respDK too (server
		// decapsulates the request before discovering e.g. the prompt overflowed
		// the model context, then encrypts its error JSON to e2e_response_pk).
		// tryDecryptErrorBody is a no-op for plaintext bodies, so this is safe.
		return nil, apiError(httpResp.StatusCode, tryDecryptErrorBody(body, respDK))
	}
	ct := httpResp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/event-stream") {
		defer httpResp.Body.Close()
		return nil, fmt.Errorf("chutes stream: expected text/event-stream, got %s", ct)
	}
	return httpResp, nil
}

// chatStream sends a streaming e2e chat request and returns a
// *stream.StreamReader[content.Chunk] over the decrypted deltas. The returned
// reader MUST be Closed by the caller; Close stops the internal reader goroutine
// and closes the upstream response.
//
// Goroutine ownership: chatStream spawns exactly ONE reader goroutine (pump).
// The pump and caller-close path share an at-most-once closer for the HTTP body;
// the pump owns the SSE reader and pipe writer. It exits — and ALWAYS closes the
// body before returning — on any of:
//   - e2e_init missing / arriving after an e2e frame (typed error -> pipe),
//   - a frame that fails to AEAD-open (fail closed, no skip -> pipe),
//   - an e2e_error event (typed error -> pipe),
//   - clean EOF or the [DONE] terminal (writes `data: [DONE]` -> pipe, nil),
//   - the caller cancelling streamCtx (via Close) — the body close unblocks the
//     read and pump returns,
//   - the caller Closing the returned stream, which closes the pipe reader: the
//     next pipe write returns io.ErrClosedPipe, pump stops and closes the body.
//
// It never leaks: every pump return path runs the deferred body close, while a
// caller Close eagerly closes both pipe and body to interrupt blocked I/O.
func (c *Client) chatStream(ctx context.Context, chuteID string, sess *attestedSession, plaintext []byte, respDK *mlkem.DecapsulationKey768) (*stream.StreamReader[content.Chunk], error) {
	httpResp, err := c.invokeStream(ctx, chuteID, sess, plaintext, respDK)
	if err != nil {
		return nil, err
	}

	// streamCtx lets Close cancel an in-flight body read even if pump is not
	// currently blocked on a pipe write.
	streamCtx, cancel := context.WithCancel(ctx)
	pr, pw := io.Pipe()
	body := &onceReadCloser{ReadCloser: httpResp.Body}
	go c.pump(streamCtx, body, respDK, pw)

	// The returned stream's Close cancels streamCtx, closes the pipe reader so a
	// write unblocks, and closes the shared upstream body so a read unblocks.
	// openaiapi.NewStream invokes this closer through the ReadCloser it owns.
	rc := &cancelReadCloser{Reader: pr, pipe: pr, upstream: body, cancel: cancel}
	return openaiapi.NewStream(rc), nil
}

// pump reads encrypted SSE events from body, decrypts e2e frames with the
// stream key established by e2e_init, and writes plaintext OpenAI SSE bytes to
// pw. It owns body and pw: both are closed exactly once before it returns. See
// the ownership note on chatStream.
func (c *Client) pump(ctx context.Context, body io.ReadCloser, respDK *mlkem.DecapsulationKey768, pw *io.PipeWriter) {
	if c.streamDone != nil {
		defer c.streamDone()
	}
	defer body.Close()

	// closeErr finalizes the pipe with err (nil = clean) and returns. The
	// deferred body.Close above still runs.
	closeErr := func(err error) {
		_ = pw.CloseWithError(err) // nil => io.EOF to the reader
	}

	reader := newSSEEventReader(body)
	var streamKey []byte

	for {
		// Honor cancellation between events (the body close on Close also
		// unblocks an in-flight read, but check here too for promptness).
		select {
		case <-ctx.Done():
			closeErr(ctx.Err())
			return
		default:
		}

		data, err := reader.next()
		if errors.Is(err, errSSEDone) {
			c.finishClean(pw)
			return
		}
		if err == io.EOF {
			c.finishClean(pw)
			return
		}
		if err != nil {
			// A read error after cancellation reports ctx.Err for clarity.
			if ctx.Err() != nil {
				closeErr(ctx.Err())
				return
			}
			closeErr(&failure.NetworkError{Err: err})
			return
		}

		var ev struct {
			Init  *string         `json:"e2e_init"`
			Frame *string         `json:"e2e"`
			Usage json.RawMessage `json:"usage"`
			Error json.RawMessage `json:"e2e_error"`
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			closeErr(&e2e.Error{Op: "decode stream event", Err: err})
			return
		}

		switch {
		case ev.Init != nil:
			key, err := deriveStreamKey(respDK, *ev.Init)
			if err != nil {
				closeErr(err)
				return
			}
			streamKey = key

		case ev.Frame != nil:
			if streamKey == nil {
				closeErr(&e2e.Error{Op: "open stream frame", Err: errStreamInitMissing})
				return
			}
			frame, err := base64.StdEncoding.DecodeString(*ev.Frame)
			if err != nil {
				closeErr(&e2e.Error{Op: "decode stream frame", Err: err})
				return
			}
			plaintext, err := e2e.OpenFrame(streamKey, frame)
			if err != nil {
				closeErr(err) // fail closed: do not skip a frame that won't open.
				return
			}
			// The decrypted plaintext is already a full OpenAI SSE chunk
			// ("data: {json}"); append the blank-line boundary (matches the
			// reference transport's `yield f"{decrypted}\n\n"`).
			if _, err := pw.Write(append(plaintext, '\n', '\n')); err != nil {
				return // pipe reader closed (caller Close): stop, body closes via defer.
			}

		case ev.Error != nil:
			closeErr(failure.NewAPIError(0, "api_error", "", 0))
			return

		case ev.Usage != nil:
			// Plaintext billing event. Pass it through as an OpenAI SSE line so
			// the openaiapi stream can pick up usage if present.
			line := append([]byte("data: "), data...)
			if _, err := pw.Write(append(line, '\n', '\n')); err != nil {
				return
			}

		default:
			// Unknown event: ignore (tolerant in).
		}
	}
}

// finishClean writes the [DONE] terminal so openaiapi.NewStream sees a clean
// end, then closes the pipe with nil (io.EOF to the reader).
func (c *Client) finishClean(pw *io.PipeWriter) {
	_, _ = pw.Write([]byte("data: [DONE]\n\n"))
	_ = pw.CloseWithError(nil)
}

// deriveStreamKey decapsulates the e2e_init ML-KEM ciphertext with the
// ephemeral response key and derives the stream key (info "e2e-stream-v1",
// salt = initCT[:16], handled inside e2e.DeriveKey).
func deriveStreamKey(respDK *mlkem.DecapsulationKey768, initB64 string) ([]byte, error) {
	initCT, err := base64.StdEncoding.DecodeString(initB64)
	if err != nil {
		return nil, &e2e.Error{Op: "decode e2e_init", Err: err}
	}
	shared, err := respDK.Decapsulate(initCT)
	if err != nil {
		return nil, &e2e.Error{Op: "decapsulate stream", Err: err}
	}
	return e2e.DeriveKey(shared, initCT, []byte("e2e-stream-v1"))
}

// cancelReadCloser wraps the pipe reader so the StreamReader's Close both
// cancels streamCtx (unblocking an in-flight body read in pump) and closes the
// pipe reader (unblocking an in-flight pipe write in pump). Either way pump
// exits and closes the upstream body.
type cancelReadCloser struct {
	io.Reader
	pipe     io.Closer
	upstream io.Closer
	cancel   context.CancelFunc
}

func (c *cancelReadCloser) Close() error {
	c.cancel()
	return errors.Join(c.pipe.Close(), c.upstream.Close())
}

// onceReadCloser gives the pump and caller-close path shared ownership of one
// upstream response body while closing that body at most once.
type onceReadCloser struct {
	io.ReadCloser
	closeOnce sync.Once
	closeErr  error
}

func (c *onceReadCloser) Close() error {
	c.closeOnce.Do(func() { c.closeErr = c.ReadCloser.Close() })
	return c.closeErr
}
