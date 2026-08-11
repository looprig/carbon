// Package route holds concrete route.Router builders for the bundled wire APIs: a
// static chat route (OpenAI/Anthropic style) and Gemini's mode-aware model-in-path
// route. These are wire-API facts only — no provider default endpoints, auth policy,
// or model catalogue lives here. Callers may supply any route.Router; these are
// conveniences for the shapes the bundled codecs speak.
package route

import (
	"net/http"
	"strings"

	"github.com/looprig/inference"
	codec "github.com/looprig/inference/codec"
)

// staticChat builds POST {base}{path} for both invoke and stream modes with no query.
// OpenAI- and Anthropic-compatible APIs select streaming via a JSON body flag, so the
// route is mode-independent.
type staticChat struct {
	path string
}

// StaticChat returns a Router that targets POST {trimRight(base,"/")}{path} for every
// mode. Use "/chat/completions" for OpenAI-compatible APIs and "/messages" for
// Anthropic-compatible APIs.
func StaticChat(path string) Router { return staticChat{path: path} }

func (s staticChat) BuildRoute(baseURL string, _ inference.Request, _ codec.RequestMode) (Route, error) {
	return Route{
		Method: http.MethodPost,
		URL:    strings.TrimRight(baseURL, "/") + s.path,
	}, nil
}

// geminiGenerateContent builds Gemini's model-in-path routes: invoke targets
// :generateContent, stream targets :streamGenerateContent?alt=sse. The model id comes
// from req.Model.Name.
type geminiGenerateContent struct{}

// GeminiGenerateContent returns a Router for Gemini's generateContent API:
//
//	invoke: POST {base}/models/{model}:generateContent
//	stream: POST {base}/models/{model}:streamGenerateContent?alt=sse
//
// The model name is read from req.Model.Name.
func GeminiGenerateContent() Router { return geminiGenerateContent{} }

// MissingModelError is returned when a Gemini route is built for a request whose Model
// carries no Name — the name is required for the model-in-path URL. Typed per the repo
// rule so callers can errors.As it rather than string-matching.
type MissingModelError struct{}

func (e *MissingModelError) Error() string {
	return "route: gemini generateContent requires a non-empty Model.Name"
}

func (geminiGenerateContent) BuildRoute(baseURL string, req inference.Request, mode codec.RequestMode) (Route, error) {
	if req.Model.Name == "" {
		return Route{}, &MissingModelError{}
	}
	base := strings.TrimRight(baseURL, "/")
	if mode == codec.RequestModeStream {
		return Route{
			Method: http.MethodPost,
			URL:    base + "/models/" + req.Model.Name + ":streamGenerateContent?alt=sse",
		}, nil
	}
	return Route{
		Method: http.MethodPost,
		URL:    base + "/models/" + req.Model.Name + ":generateContent",
	}, nil
}
