package gateway

import (
	"reflect"

	"github.com/looprig/inference/codec"
	"github.com/looprig/inference/contextcount"
	"github.com/looprig/inference/model"
)

// DefaultMaxRequestBody is the request body size bound applied when
// Config.MaxRequestBody is zero. 10 MiB comfortably covers a large
// multi-turn agentic conversation with several inlined images while still
// bounding worst-case per-request memory.
const DefaultMaxRequestBody int64 = 10 << 20 // 10 MiB

// DefaultMaxConcurrent is the global in-flight admission bound applied when
// Config.MaxConcurrent is zero. 64 is a conservative default for a local,
// single-process sidecar gateway: generous enough not to bottleneck a single
// harness session, small enough to bound worst-case upstream fan-out and
// local memory from a misbehaving or compromised caller.
const DefaultMaxConcurrent = 64

// Config configures a Handler. Every field except MaxRequestBody,
// MaxConcurrent, and ContextCounter is required; New validates the required
// fields and rejects an invalid Config with a *ConfigError.
type Config struct {
	// Resolver maps (ingress format, requested model alias) to a Target.
	// Required.
	Resolver Resolver

	// Codecs is every ingress dialect this Handler serves, keyed by the
	// model.APIFormat it decodes/encodes. The key doubles as the Ingress
	// value passed to Resolver.Resolve when that codec's MatchRequest wins
	// route selection for a request -- codec.ServerCodec itself carries no
	// APIFormat accessor, so Config is the one place that association is
	// made. At least one entry is required.
	Codecs map[model.APIFormat]codec.ServerCodec

	// Authenticate authenticates the gateway's own local inbound token.
	// Required.
	Authenticate Authenticator

	// ContextCounter serves the Anthropic-dialect
	// POST /v1/messages/count_tokens auxiliary route (matched via
	// anthropicapi.MatchCountTokensRequest, which lives outside the
	// generic codec.ServerCodec surface -- see handler.go). This field is
	// NOT part of the design doc's abbreviated Config sketch
	// (Resolver/Codecs/Authenticate/MaxRequestBody/MaxConcurrent); it was
	// added here because Task 6 requires the count_tokens route to call a
	// configured contextcount.ContextCounter, and the sketch had nowhere to
	// configure one. Optional: a nil ContextCounter is a valid
	// configuration -- a count_tokens request then fails cleanly with a
	// typed *CountTokensUnavailableError (503) instead of panicking on a
	// nil interface call.
	ContextCounter contextcount.ContextCounter

	// MaxRequestBody bounds a request body in bytes, applied before JSON
	// decoding. Zero means DefaultMaxRequestBody.
	MaxRequestBody int64

	// MaxConcurrent bounds global in-flight admission (Target.Client.Invoke/
	// Stream and ContextCounter.CountContext calls). Admission is
	// reject-on-full (a *ConcurrencyLimitExceededError), never queued. Zero
	// means DefaultMaxConcurrent.
	MaxConcurrent int
}

// New validates config and builds a ready-to-use Handler. It rejects an
// invalid config with a *ConfigError (the same type NewMux/Fixed use --
// there is exactly one "invalid configuration" type across this package):
//
//   - a nil Resolver;
//   - an empty Codecs map;
//   - a nil Authenticate;
//   - a negative MaxRequestBody or MaxConcurrent;
//   - two entries in Codecs whose concrete dynamic type is identical (a
//     cheap, best-effort proxy for an obviously duplicate registration --
//     see the doc comment below for its limits).
//
// Duplicate/ambiguous *route* detection cannot be fully done here: two
// structurally different codecs can still both return true from
// MatchRequest for the same concrete request, and that can only be observed
// once a live *http.Request exists. That full check happens at request time
// in Handler.ServeHTTP, which returns a distinct *AmbiguousCodecMatchError
// (HTTP 500) when it happens. The construction-time check here only catches
// the narrower, cheaper case of the literal same concrete codec type
// registered under two different Codecs keys by mistake.
func New(config Config) (*Handler, error) {
	if config.Resolver == nil {
		return nil, &ConfigError{Location: "Config.Resolver", Reason: "must not be nil"}
	}
	if len(config.Codecs) == 0 {
		return nil, &ConfigError{Location: "Config.Codecs", Reason: "must not be empty"}
	}
	if config.Authenticate == nil {
		return nil, &ConfigError{Location: "Config.Authenticate", Reason: "must not be nil"}
	}
	if config.MaxRequestBody < 0 {
		return nil, &ConfigError{Location: "Config.MaxRequestBody", Reason: "must not be negative"}
	}
	if config.MaxConcurrent < 0 {
		return nil, &ConfigError{Location: "Config.MaxConcurrent", Reason: "must not be negative"}
	}

	seenTypes := make(map[reflect.Type]model.APIFormat, len(config.Codecs))
	codecs := make(map[model.APIFormat]codec.ServerCodec, len(config.Codecs))
	for format, c := range config.Codecs {
		if c == nil {
			return nil, &ConfigError{Location: "Config.Codecs[" + string(format) + "]", Reason: "must not be nil"}
		}
		t := reflect.TypeOf(c)
		if other, ok := seenTypes[t]; ok {
			return nil, &ConfigError{
				Location: "Config.Codecs",
				Reason:   "the same concrete codec type is registered under both " + string(other) + " and " + string(format),
			}
		}
		seenTypes[t] = format
		codecs[format] = c
	}

	maxRequestBody := config.MaxRequestBody
	if maxRequestBody == 0 {
		maxRequestBody = DefaultMaxRequestBody
	}
	maxConcurrent := config.MaxConcurrent
	if maxConcurrent == 0 {
		maxConcurrent = DefaultMaxConcurrent
	}

	return &Handler{
		resolver:       config.Resolver,
		codecs:         codecs,
		authenticate:   config.Authenticate,
		contextCounter: config.ContextCounter,
		maxRequestBody: maxRequestBody,
		sem:            make(chan struct{}, maxConcurrent),
	}, nil
}
