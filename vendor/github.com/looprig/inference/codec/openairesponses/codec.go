package openairesponses

import (
	"bytes"
	"net/http"

	"github.com/looprig/inference"
	codec "github.com/looprig/inference/codec"
	"github.com/looprig/inference/wire/jsonbody"
)

// Codec is the OpenAI Responses API wire dialect expressed as a codec.Codec
// (and, via DecodeStream in stream.go, a codec.StreamingCodec) AND, unlike
// codec/openaiapi today, a full codec.ServerCodec: this package is built from
// scratch with both directions, since unlike Anthropic there is no prior
// client-only package to extend. It is stateless (an empty struct with
// value-receiver methods), so one value is safely shared across goroutines.
// The methods delegate to package-level free functions so the method surface
// and the free surface cannot diverge.
type Codec struct{}

// Compile-time proof that Codec honors the codec.Codec contract.
var _ codec.Codec = Codec{}

// Compile-time proof that Codec also honors the ingress-side codec.ServerCodec
// contract: recognizing and decoding native POST /v1/responses requests, and
// encoding results back into native Responses responses/streams.
var _ codec.ServerCodec = Codec{}

// MatchRequest reports whether req is a POST /v1/responses request.
func (Codec) MatchRequest(req *http.Request) bool {
	return matchResponsesRequest(req)
}

// DecodeRequest decodes a matched POST /v1/responses request into a
// codec.DecodedRequest, delegating to the free decodeResponsesRequest.
func (Codec) DecodeRequest(req *http.Request) (codec.DecodedRequest, error) {
	return decodeResponsesRequest(req)
}

// WriteResponse encodes a complete inference.Response as the native
// Responses API non-streaming response, delegating to the free
// writeResponsesResponse.
func (Codec) WriteResponse(w http.ResponseWriter, resp *inference.Response) error {
	return writeResponsesResponse(w, resp)
}

// OpenStream begins the native Responses streaming response and returns its
// request-scoped StreamEncoder, delegating to the free openResponsesStream.
func (Codec) OpenStream(w http.ResponseWriter) (codec.StreamEncoder, error) {
	return openResponsesStream(w)
}

// WriteError encodes err as the native Responses error envelope, delegating
// to the free writeResponsesError.
func (Codec) WriteError(w http.ResponseWriter, err error) {
	writeResponsesError(w, err)
}

// EncodeRequest builds the Responses request: a JSON body reader plus the
// application/json content type as an EncodedRequest. RequestModeStream sets
// "stream":true in the body, every other mode omits it.
func (Codec) EncodeRequest(req inference.Request, mode codec.RequestMode) (codec.EncodedRequest, error) {
	body, err := EncodeRequest(req, mode == codec.RequestModeStream)
	if err != nil {
		return codec.EncodedRequest{}, err
	}
	h := http.Header{}
	h.Set("Content-Type", jsonbody.ContentType)
	return codec.EncodedRequest{Header: h, Body: bytes.NewReader(body)}, nil
}

// DecodeResponse parses a non-streaming Responses response body, delegating
// to the free DecodeResponse.
func (Codec) DecodeResponse(body []byte) (*inference.Response, error) {
	return DecodeResponse(body)
}
