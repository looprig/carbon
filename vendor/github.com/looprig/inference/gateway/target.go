// Package gateway provides a local HTTP compatibility layer that lets
// coding-harness clients speaking different model-API dialects (Anthropic
// Messages, OpenAI Responses, OpenAI Chat Completions, Gemini) reach any
// injected inference.Client/model.Model target.
//
// This file defines Target: the fully bound routing destination shared by
// every Resolver implementation.
package gateway

import (
	"github.com/looprig/inference"
	"github.com/looprig/inference/model"
)

// Target is a fully bound inference destination.
//
// ID is a stable, secret-free diagnostic identity used for logs and
// metrics only: it is never sent upstream and is not a substitute for
// Model's own identity (Model.Key). An empty ID asserts no diagnostic
// identity and is a valid configuration.
//
// Client is already bound to its own credentials and connection policy --
// the gateway never handles secrets directly, so Client must never be nil.
//
// Model is the secret-free model descriptor sent with each request routed
// to this target.
type Target struct {
	ID     string
	Client inference.Client
	Model  model.Model

	// AuthoritativeEffort makes Model.Sampling.Effort authoritative for every
	// request served by this target. The handler will stamp it after ingress
	// decode and before validation/encoding, replacing ingress effort even when
	// the target effort is model.EffortNone. Other override fields remain
	// untouched. False preserves current behavior.
	AuthoritativeEffort bool
}
