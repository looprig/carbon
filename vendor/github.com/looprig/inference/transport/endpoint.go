package transport

import (
	"github.com/looprig/inference/model"
)

// Endpoint is explicit client-binding metadata for one connection.
type Endpoint struct {
	BaseURL   string
	Provider  model.ProviderName
	APIFormat model.APIFormat
}
