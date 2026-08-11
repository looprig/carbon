package codec

import (
	"io"
	"net/http"

	"github.com/looprig/core/content"
	"github.com/looprig/inference"
	"github.com/looprig/inference/stream"
)

// EncodedRequest is a semantic API encoding of a Request: the wire headers the encoder wants
// applied and the single-shot request body. Body is consumed exactly once — the generic
// transport must not retry or replay it.
type EncodedRequest struct {
	Header http.Header
	Body   io.Reader
}

// RequestEncoder maps a provider-neutral Request into wire form. It handles normal request
// bodies, including streaming-mode bodies when the wire API represents streaming as a JSON flag.
type RequestEncoder interface {
	EncodeRequest(req inference.Request, mode RequestMode) (EncodedRequest, error)
}

// ResponseDecoder decodes a non-streaming response from an already-drained successful body.
// The transport owns HTTP status mapping, body closing, and read errors.
type ResponseDecoder interface {
	DecodeResponse(body []byte) (*inference.Response, error)
}

// StreamFrame is a raw stream event: an optional event Name, optional Metadata, and the Data
// payload. It is wire-level, not semantic — a StreamDecoder maps frames to content.Chunk.
// StreamFramer converts a streaming body into raw stream events. It owns closing that body
// through the returned StreamReader; if it returns an error before returning a reader, it must
// close the body before returning.
type StreamFramer interface {
	DecodeStreamFrames(body io.ReadCloser) (*stream.StreamReader[stream.StreamFrame], error)
}

// StreamDecoder owns the full streaming response path from a successful HTTP response to
// content.Chunk. Once DecodeStream is called it owns resp.Body; if it returns an error before
// returning a reader, it must close resp.Body before returning.
type StreamDecoder interface {
	DecodeStream(resp *http.Response) (*stream.StreamReader[content.Chunk], error)
}

// Codec is the non-streaming composition of request encoding and response decoding. It does
// NOT embed StreamDecoder: streaming is optional, so a non-streaming API satisfies Codec
// without stubbing DecodeStream.
type Codec interface {
	RequestEncoder
	ResponseDecoder
}

// StreamingCodec is a Codec that also decodes streaming responses.
type StreamingCodec interface {
	Codec
	StreamDecoder
}

// DecodedRequest is a semantic decoding of a native harness HTTP request: the
// provider-neutral Request, the untrusted harness-requested model name (never
// trusted as an upstream target identity — the gateway resolves it to a Target
// and overwrites Request.Model before invocation), and whether the harness asked
// for a streaming response.
type DecodedRequest struct {
	Request        inference.Request
	RequestedModel string
	Streaming      bool
}

// ServerCodec is the ingress-side counterpart to Codec: it recognizes and decodes
// one dialect's native HTTP request, and encodes inference results back into that
// dialect's native HTTP response. A ServerCodec owns dialect request recognition
// and semantic body decoding, and dialect response JSON and stream event encoding.
// It does NOT own authentication, body limits, route resolution, target
// substitution, upstream invocation, cancellation, or HTTP lifecycle — those
// belong to the gateway package.
//
// Implementations must be stateless and safe for concurrent use; only the
// StreamEncoder returned by OpenStream is request-scoped.
type ServerCodec interface {
	// MatchRequest reports whether req's method and path belong to this codec's
	// dialect. It must not consume req.Body.
	MatchRequest(req *http.Request) bool

	// DecodeRequest decodes a matched request into a DecodedRequest. It owns
	// semantic validation of the native request shape, including rejecting an
	// unsupported Content-Type and malformed bodies, and must never panic on
	// malformed input.
	DecodeRequest(req *http.Request) (DecodedRequest, error)

	// WriteResponse encodes a complete non-streaming inference.Response as this
	// dialect's native successful HTTP response.
	WriteResponse(w http.ResponseWriter, resp *inference.Response) error

	// OpenStream begins this dialect's native streaming HTTP response and
	// returns a request-scoped StreamEncoder for the remainder of the stream.
	// Once called, the returned StreamEncoder owns w until Finish or Fail is
	// called.
	OpenStream(w http.ResponseWriter) (StreamEncoder, error)

	// WriteError encodes err as this dialect's native error envelope, including
	// its HTTP status code. It must not panic regardless of the concrete error
	// type.
	WriteError(w http.ResponseWriter, err error)
}

// StreamEncoder owns one in-flight native streaming HTTP response body. It is
// request-scoped and may hold request-local state such as generated tool-call
// identifiers and event sequence numbers; it is never shared across requests.
//
// Exactly one of Finish or Fail terminates a StreamEncoder. Once either has been
// called, every further call — including a second Finish or Fail — must return an
// error rather than writing to the closed stream again, and must never panic.
type StreamEncoder interface {
	// WriteChunk encodes and flushes one content.Chunk as a native stream event.
	WriteChunk(chunk content.Chunk) error

	// Finish encodes the dialect's native stream-completion event(s) from
	// authoritative terminal metadata and closes the stream cleanly.
	Finish(result stream.StreamResult) error

	// Fail encodes a native in-stream error event, if the dialect distinguishes
	// one, and closes the stream.
	Fail(err error) error
}
