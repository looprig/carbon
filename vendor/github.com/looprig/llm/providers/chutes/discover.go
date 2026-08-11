package chutes

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	failure "github.com/looprig/inference/failure"
)

// mlkemPubKeySize is the raw byte length of an ML-KEM-768 encapsulation key
// (WIRE.md section 2; the Lua reference asserts this after base64-decode).
const mlkemPubKeySize = 1184

// instance is one TEE instance returned by E2E discovery: its id, the decoded
// ML-KEM-768 encapsulation key we seal requests to, the original base64 pubkey
// string, and the single-use invoke nonces the server will accept from us.
//
// pubKeyB64 retains the exact base64 string the server sent because the
// report_data binding (attest.go) hashes that string verbatim; re-encoding the
// decoded bytes risks a non-canonical-encoding mismatch.
type instance struct {
	id        string
	pubKey    []byte
	pubKeyB64 string
	nonces    []string
}

// discoverInstances does GET {baseURL}/e2e/instances/{chuteID} with a Bearer
// token and parses the response (WIRE.md section 5). Transport failures return
// *failure.NetworkError; a non-2xx status returns *failure.APIError; an empty
// instance list or a pubkey that does not decode to 1184 bytes returns an error.
func discoverInstances(ctx context.Context, hc *http.Client, baseURL, apiKey, chuteID string) ([]instance, error) {
	url := baseURL + "/e2e/instances/" + chuteID
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, &failure.NetworkError{Err: err}
	}
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)

	httpResp, err := hc.Do(httpReq)
	if err != nil {
		return nil, &failure.NetworkError{Err: err}
	}
	defer httpResp.Body.Close()

	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, &failure.NetworkError{Err: err}
	}
	if httpResp.StatusCode/100 != 2 {
		return nil, apiError(httpResp.StatusCode, respBody)
	}

	var env struct {
		Instances []struct {
			InstanceID string   `json:"instance_id"`
			E2EPubKey  string   `json:"e2e_pubkey"`
			Nonces     []string `json:"nonces"`
		} `json:"instances"`
	}
	if err := json.Unmarshal(respBody, &env); err != nil {
		return nil, fmt.Errorf("chutes discover: decode response: %w", err)
	}
	if len(env.Instances) == 0 {
		return nil, fmt.Errorf("chutes discover: no instances available for chute %s", chuteID)
	}

	out := make([]instance, 0, len(env.Instances))
	for _, in := range env.Instances {
		pub, err := base64.StdEncoding.DecodeString(in.E2EPubKey)
		if err != nil {
			return nil, fmt.Errorf("chutes discover: instance %s: decode e2e_pubkey: %w", in.InstanceID, err)
		}
		if len(pub) != mlkemPubKeySize {
			return nil, fmt.Errorf("chutes discover: instance %s: e2e_pubkey is %d bytes, want %d", in.InstanceID, len(pub), mlkemPubKeySize)
		}
		out = append(out, instance{id: in.InstanceID, pubKey: pub, pubKeyB64: in.E2EPubKey, nonces: in.Nonces})
	}
	return out, nil
}

// apiError builds a *failure.APIError from a non-2xx response, best-effort
// extracting a "detail" message from the Chutes/FastAPI error envelope.
func apiError(status int, body []byte) error {
	return failure.APIErrorFromResponse(status, body, nil, 0)
}
