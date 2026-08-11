// Package gateway (this file) implements Handler.serveStreaming: the
// incremental pull loop that drives a streaming inference response.
//
// serveStreaming replaces the intentionally minimal Task-6 stub declared in
// handler.go (see that method's remaining doc comment there for the seam
// contract: ServeHTTP's call site and the admission acquire/release around
// it are unchanged by this file). It is responsible for, in order:
//
//  1. Calling target.Client.Stream (decoded.Request.Model has already been
//     replaced with target.Model by serveInference).
//  2. Classifying a pre-header Stream error exactly like the non-streaming
//     Invoke failure path (a *UpstreamInvocationError via h.writeError).
//  3. Owning the returned *stream.StreamReader[content.Chunk] for its
//     entire lifetime once Stream succeeds (a deferred Close covers clean
//     EOF, in-stream failure, and cancellation alike).
//  4. Opening the native streaming response via sc.OpenStream -- the point
//     headers commit, after which every further failure goes through the
//     returned codec.StreamEncoder's Fail, never sc.WriteError again.
//  5. Pulling chunks one at a time and writing each to the StreamEncoder in
//     order, terminating with exactly one of Finish (clean EOF) or Fail
//     (any other error).
//  6. Arming a watcher that closes the reader when the inbound request's
//     context is canceled, so a blocked Next() cannot outlive a client
//     disconnect or server shutdown -- and disarming that watcher once the
//     pull loop itself returns, so a normal, fast request never leaves a
//     goroutine (or context bookkeeping) waiting on a longer-lived parent
//     context.
package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/looprig/core/content"
	"github.com/looprig/inference/codec"
	"github.com/looprig/inference/stream"
)

// serveStreaming implements steps 10/11 of the pipeline documented on
// ServeHTTP for a streaming request (DecodedRequest.Streaming == true). See
// the package doc above this function for the full contract.
func (h *Handler) serveStreaming(w http.ResponseWriter, r *http.Request, decoded codec.DecodedRequest, target Target, sc codec.ServerCodec) error {
	ctx := r.Context()

	reader, err := target.Client.Stream(ctx, decoded.Request)
	if err != nil {
		// Pre-header failure: no native streaming response has started, so
		// this classifies and writes exactly like a normal pipeline error,
		// via the same h.upstreamError construction the non-streaming
		// Invoke failure path uses (see serveInference), for consistent
		// classification between the two upstream-failure paths.
		wrapped := h.upstreamError(ctx, err)
		h.writeError(sc, w, wrapped)
		return wrapped
	}
	// From here on this method owns reader's entire lifetime: clean EOF,
	// in-stream failure, and cancellation all exit through the pull loop
	// below, and every one of those exits is covered by this single
	// deferred Close.
	defer reader.Close()

	enc, err := sc.OpenStream(w)
	if err != nil {
		// Best-effort fallback only: header-commitment state is
		// codec-internal, so there is no reliable way to know whether
		// sc.WriteError can still produce a clean pre-header error
		// response at this point. Attempting it anyway is harmless if the
		// codec already wrote something.
		h.writeError(sc, w, err)
		return err
	}

	// Arm a watcher that interrupts a blocking reader.Next() when the
	// inbound request's context is canceled (client disconnect, server
	// shutdown, or an upstream deadline), per stream.StreamReader.Close's
	// documented "Close is deliberately not serialized behind a blocking
	// Next so it can interrupt I/O" contract. stop is deferred immediately
	// so the watcher is disarmed the moment the pull loop returns for any
	// other reason -- a fast, non-canceled request never leaves this
	// registered against a longer-lived parent context.
	//
	// This watcher only ever calls reader.Close(); it never touches enc. Only
	// pullStream calls enc.Finish/enc.Fail, and it does so exactly once as its
	// sole terminal action, so this watcher cannot race a second
	// Finish/Fail call onto enc no matter when it fires relative to the pull
	// loop's own termination.
	stop := context.AfterFunc(ctx, func() {
		_ = reader.Close()
	})
	defer stop()

	return h.pullStream(reader, enc, decoded.RequestedModel)
}

// pullStream repeatedly pulls one content.Chunk at a time from reader and
// writes it to enc, in order, until the stream ends -- terminating enc with
// exactly one of Finish or Fail as its terminal action:
//
//   - a clean io.EOF calls Finish with the reader's authoritative terminal
//     stream.StreamResult, its Model overwritten with requestedModel so the
//     harness-facing terminal metadata reports the alias, never the
//     resolved upstream model name (mirroring serveInference's non-streaming
//     resp.Model assignment). A defensive false from reader.Result() (no
//     producer result -- not expected from a well-behaved client, but must
//     not panic or skip termination) still calls Finish, with a zero-value
//     StreamResult carrying only that same Model.
//   - a WriteChunk failure, or any other non-EOF error from Next
//     (including one surfaced by a cancellation-triggered Close racing the
//     read), calls Fail. Fail's own error, if any, is not actionable here --
//     the loop is already unwinding on failure -- so it is discarded.
func (h *Handler) pullStream(reader *stream.StreamReader[content.Chunk], enc codec.StreamEncoder, requestedModel string) error {
	for {
		chunk, err := reader.Next()
		if err == nil {
			if writeErr := enc.WriteChunk(chunk); writeErr != nil {
				_ = enc.Fail(writeErr)
				return writeErr
			}
			continue
		}

		if errors.Is(err, io.EOF) {
			result, ok := reader.Result()
			if !ok {
				result = stream.StreamResult{}
			}
			result.Model = requestedModel
			return enc.Finish(result)
		}

		_ = enc.Fail(err)
		return err
	}
}
