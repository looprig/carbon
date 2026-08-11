package chutes

import (
	"encoding/base64"
	"encoding/json"

	"github.com/looprig/inference"
	"github.com/looprig/inference/codec/openaiapi"
)

// chutesRequest extends the standard OpenAI chat request with the chutes-specific
// E2E response encryption key. Embedding openaiapi.ChatRequest ensures the base
// fields are marshaled as a flat JSON object alongside e2e_response_pk.
type chutesRequest struct {
	openaiapi.ChatRequest
	E2EResponsePK string `json:"e2e_response_pk,omitempty"`
}

// encodeRequest converts a provider-neutral inference.Request to a chutes JSON
// body: the standard OpenAI fields plus e2e_response_pk (the base64 ML-KEM
// encapsulation key) so the server can seal its response to the client's
// ephemeral key.
func encodeRequest(req inference.Request, stream bool, responseEK []byte) ([]byte, error) {
	base, err := openaiapi.BuildChatRequest(req, stream)
	if err != nil {
		return nil, err
	}
	return json.Marshal(chutesRequest{
		ChatRequest:   base,
		E2EResponsePK: base64.StdEncoding.EncodeToString(responseEK),
	})
}
