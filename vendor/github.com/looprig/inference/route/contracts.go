package route

import (
	"net/http"

	"github.com/looprig/inference"
	"github.com/looprig/inference/codec"
)

// Route is a fully-resolved outbound request target: method, absolute URL, and route headers.
// It is what a Router returns so the generic transport need not hardcode POST, a chat path, a
// Content-Type, or a streaming Accept header.
type Route struct {
	Method string
	URL    string
	Header http.Header
}

// Router builds the Route for a given base endpoint, request, and mode. It is the seam that
// keeps mode-aware wire APIs (such as Gemini's model-in-path generateContent vs
// streamGenerateContent routes) out of the transport. Concrete builders live in the route
// package; callers may supply any Router.
type Router interface {
	BuildRoute(baseURL string, req inference.Request, mode codec.RequestMode) (Route, error)
}
