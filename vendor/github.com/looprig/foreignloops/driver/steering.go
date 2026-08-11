package driver

import (
	"errors"
	"fmt"

	"github.com/looprig/core/content"
)

// SteerRequest is an immutable, validated prompt for an optional active-turn
// steering operation. Its prompt is retained privately and every accessor
// returns a deep copy, including mutable byte payloads and nested tool-result
// blocks.
type SteerRequest struct {
	prompt []content.Block
}

// NewSteerRequest validates and takes ownership of a deep copy of prompt.
// Empty prompts and nil blocks are rejected because a steering operation must
// carry one complete message payload.
func NewSteerRequest(prompt []content.Block) (SteerRequest, error) {
	cloned, err := cloneSteerBlocks(prompt, 0, make(map[*content.ToolResultBlock]struct{}))
	if err != nil {
		return SteerRequest{}, err
	}
	return SteerRequest{prompt: cloned}, nil
}

// Validate reports whether the request contains a non-empty, structurally
// valid prompt. Requests returned by NewSteerRequest are already validated;
// this method also makes the zero value fail closed.
func (r SteerRequest) Validate() error {
	_, err := cloneSteerBlocks(r.prompt, 0, make(map[*content.ToolResultBlock]struct{}))
	return err
}

// Prompt returns a deep copy of the steering prompt. Mutating the returned
// slice, blocks, or any nested byte payload cannot mutate the request.
func (r SteerRequest) Prompt() []content.Block {
	cloned, err := cloneSteerBlocks(r.prompt, 0, make(map[*content.ToolResultBlock]struct{}))
	if err != nil {
		// The fields are private and constructors validate them. A malformed
		// value can only arise from the zero value, for which nil is the safe
		// immutable snapshot.
		return nil
	}
	return cloned
}

// cloneSteerBlocks validates and recursively copies every sealed content
// block. ToolResultBlock can contain nested blocks, so a pointer set prevents
// a hostile cyclic value from recursing forever.
func cloneSteerBlocks(blocks []content.Block, depth int, active map[*content.ToolResultBlock]struct{}) ([]content.Block, error) {
	if depth > maxSteerBlockDepth {
		return nil, &steerRequestError{field: "prompt", reason: "nested block depth exceeded"}
	}
	if len(blocks) == 0 {
		return nil, &steerRequestError{field: "prompt", reason: "required"}
	}
	cloned := make([]content.Block, len(blocks))
	for i, block := range blocks {
		copyBlock, err := cloneSteerBlock(block, depth, active)
		if err != nil {
			return nil, &steerRequestError{field: fmt.Sprintf("prompt[%d]", i), reason: err.Error()}
		}
		cloned[i] = copyBlock
	}
	return cloned, nil
}

const maxSteerBlockDepth = 64

func cloneSteerBlock(block content.Block, depth int, active map[*content.ToolResultBlock]struct{}) (content.Block, error) {
	switch typed := block.(type) {
	case *content.TextBlock:
		if typed == nil {
			return nil, &steerRequestError{field: "block", reason: "nil"}
		}
		return &content.TextBlock{Text: typed.Text}, nil
	case *content.ImageBlock:
		if typed == nil {
			return nil, &steerRequestError{field: "block", reason: "nil"}
		}
		return &content.ImageBlock{
			MediaType: typed.MediaType,
			Source: content.ImageSource{
				URL:  typed.Source.URL,
				Data: cloneSteerBytes(typed.Source.Data),
			},
		}, nil
	case *content.AudioBlock:
		if typed == nil {
			return nil, &steerRequestError{field: "block", reason: "nil"}
		}
		return &content.AudioBlock{MediaType: typed.MediaType, Data: cloneSteerBytes(typed.Data)}, nil
	case *content.DocumentBlock:
		if typed == nil {
			return nil, &steerRequestError{field: "block", reason: "nil"}
		}
		return &content.DocumentBlock{
			MediaType: typed.MediaType,
			Name:      typed.Name,
			Data:      cloneSteerBytes(typed.Data),
			Text:      typed.Text,
		}, nil
	case *content.ThinkingBlock:
		if typed == nil {
			return nil, &steerRequestError{field: "block", reason: "nil"}
		}
		return &content.ThinkingBlock{
			Thinking:            typed.Thinking,
			Signature:           typed.Signature,
			ProviderState:       cloneSteerBytes(typed.ProviderState),
			ProviderStateFormat: typed.ProviderStateFormat,
		}, nil
	case *content.ToolUseBlock:
		if typed == nil {
			return nil, &steerRequestError{field: "block", reason: "nil"}
		}
		return &content.ToolUseBlock{
			ID:    typed.ID,
			Name:  typed.Name,
			Input: cloneSteerBytes(typed.Input),
		}, nil
	case *content.ToolResultBlock:
		if typed == nil {
			return nil, &steerRequestError{field: "block", reason: "nil"}
		}
		if _, seen := active[typed]; seen {
			return nil, &steerRequestError{field: "block", reason: "cyclic nested block"}
		}
		active[typed] = struct{}{}
		defer delete(active, typed)
		var clonedContent []content.Block
		if len(typed.Content) > 0 {
			var err error
			clonedContent, err = cloneSteerBlocks(typed.Content, depth+1, active)
			if err != nil {
				return nil, err
			}
		}
		return &content.ToolResultBlock{
			ToolUseID: typed.ToolUseID,
			Content:   clonedContent,
			IsError:   typed.IsError,
		}, nil
	default:
		return nil, &steerRequestError{field: "block", reason: "unknown type"}
	}
}

func cloneSteerBytes(input []byte) []byte {
	if input == nil {
		return nil
	}
	output := make([]byte, len(input))
	copy(output, input)
	return output
}

type steerRequestError struct {
	field  string
	reason string
}

func (e *steerRequestError) Error() string {
	if e == nil {
		return "driver: invalid steering request"
	}
	return "driver: invalid steering request: " + e.field + ": " + e.reason
}

// SteerOutcome is the closed, provider-neutral classification of one steering
// attempt. Unknown values are invalid and must be treated as ambiguous by a
// driver adapter rather than guessed.
type SteerOutcome string

const (
	SteerOutcomeInjected             SteerOutcome = "injected"
	SteerOutcomeFallbackRequired     SteerOutcome = "fallback_required"
	SteerOutcomeUnsupported          SteerOutcome = "unsupported"
	SteerOutcomeAdmissionUnknown     SteerOutcome = "admission_unknown"
	SteerOutcomeDeliveryUnknown      SteerOutcome = "delivery_unknown"
	SteerOutcomeDeliveredUntrackable SteerOutcome = "delivered_untrackable"
)

// Valid reports whether o is one of the normalized steering outcomes.
func (o SteerOutcome) Valid() bool {
	switch o {
	case SteerOutcomeInjected, SteerOutcomeFallbackRequired, SteerOutcomeUnsupported,
		SteerOutcomeAdmissionUnknown,
		SteerOutcomeDeliveryUnknown, SteerOutcomeDeliveredUntrackable:
		return true
	default:
		return false
	}
}

// RetrySafe reports whether the adapter proved that no steering delivery can
// have occurred. Only unsupported and fallback_required permit an automatic
// normal-turn retry; all uncertainty and lifecycle-breach outcomes are
// intentionally non-retryable.
func (o SteerOutcome) RetrySafe() bool {
	return o == SteerOutcomeUnsupported || o == SteerOutcomeFallbackRequired
}

// SteerResult is the transport-normalized result of one steering attempt.
// WriteAdmitted is true once the request crossed the adapter writer boundary;
// ReceiveSequence and ResponseSequence identify the monotonic inbound
// response order. A zero sequence means no inbound response was observed.
type SteerResult struct {
	Outcome          SteerOutcome
	Reason           string
	WriteAdmitted    bool
	ReceiveSequence  uint64
	ResponseSequence uint64
	OrderSequence    uint64
}

// Validate rejects an unknown outcome. Sequence and admission facts may be
// zero on a proven pre-admission failure, so they are intentionally not
// required for every outcome.
func (r SteerResult) Validate() error {
	if !r.Outcome.Valid() {
		return errors.New("driver: invalid steering outcome")
	}
	if r.ReceiveSequence != r.ResponseSequence {
		return errors.New("driver: invalid steering receive sequence")
	}
	if r.ResponseSequence != 0 && !r.WriteAdmitted {
		return errors.New("driver: steering response was not writer-admitted")
	}
	switch r.Outcome {
	case SteerOutcomeUnsupported:
		if r.WriteAdmitted {
			return errors.New("driver: unsupported steering was writer-admitted")
		}
		if r.ReceiveSequence != 0 {
			return errors.New("driver: unsupported steering has a response sequence")
		}
	case SteerOutcomeInjected, SteerOutcomeDeliveredUntrackable:
		if !r.WriteAdmitted {
			return errors.New("driver: steering outcome was not writer-admitted")
		}
		if r.ResponseSequence == 0 {
			return errors.New("driver: steering outcome has no response sequence")
		}
	case SteerOutcomeFallbackRequired:
		if r.WriteAdmitted && r.ResponseSequence == 0 {
			return errors.New("driver: admitted steering fallback has no response sequence")
		}
	case SteerOutcomeAdmissionUnknown:
		// AdmissionUnknown means the adapter may have been invoked, but no
		// writer fact was observed. A true bit would turn uncertainty into a
		// fabricated admission claim; a response sequence is likewise invalid
		// without a positive admission fact (checked above).
		if r.WriteAdmitted {
			return errors.New("driver: admission-unknown steering was writer-admitted")
		}
	case SteerOutcomeDeliveryUnknown:
		if !r.WriteAdmitted {
			return errors.New("driver: unknown steering delivery was not writer-admitted")
		}
	}
	return nil
}

// ObservationKind identifies the normalized observation family in an ordered
// stream. The values correspond to ACP prompt completion, session update, and
// steering response observations.
type ObservationKind uint8

const (
	ObservationPrompt ObservationKind = iota
	ObservationUpdate
	ObservationSteer
)

// Valid reports whether k identifies a defined observation family.
func (k ObservationKind) Valid() bool {
	return k == ObservationPrompt || k == ObservationUpdate || k == ObservationSteer
}

// Observation is the ordered, provider-neutral view consumed by a backend.
// Implementations are sealed so adapters cannot inject observations that do
// not carry one of the normalized payloads below.
type Observation interface {
	Kind() ObservationKind
	Sequence() uint64
	observation()
}

// OrderedStream is an optional stream capability. It does not replace the
// existing Stream.Events channel: legacy consumers may continue to consume
// normalized Event values, while a steering-aware backend type-asserts
// OrderedStream and consumes one observation channel as its authoritative
// prompt/update/steer order.
//
// The observation channel is owned by the stream implementation. It is read
// only by consumers and is closed exactly once after the stream has finished
// producing observations; consumers must not close it. Stream.Close remains
// the lifecycle operation and remains idempotent. Every Steerer.Steer call
// produces exactly one SteerObservation for every call accepted by the
// steering actor, including accepted calls ending in a typed error or a
// pre-admission result. A call rejected before actor admission (for example by
// a bounded reservation lane) produces no observation. A producer emits
// observations in nondecreasing ReceiveSequence order. Multiple translated
// observations may share one receive sequence; their channel order is the
// tie-breaker and consumers must not reorder equal-sequence observations.
// Sequence reports an effective order key: raw ReceiveSequence is preserved
// for protocol facts, while observations without transport sequence receive a
// strictly increasing adapter-owned key.
type OrderedStream interface {
	// Observations returns the stream-owned ordered projection. A stream selects
	// exactly one projection before production starts: legacy Events or this
	// channel. The inactive projection is closed and carries no traffic.
	Observations() <-chan Observation
}

// PromptObservation is one normalized prompt completion. It mirrors ACP's
// PromptResult transport facts while leaving provider stop-reason vocabulary
// at the adapter boundary. Err is bounded/typed by the adapter when a prompt
// completion failed before a result was available.
type PromptObservation struct {
	// StopReason is an adapter-normalized stop classification. It remains a
	// string at this provider-neutral boundary because concrete ACP stop
	// vocabularies do not belong in driver contracts.
	StopReason string
	// Message is the assembled assistant transcript for this prompt, when the
	// adapter has one. Keeping it on the prompt observation lets ordered
	// consumers commit the final answer without a second legacy event path.
	Message          *content.AIMessage
	WriteAdmitted    bool
	ReceiveSequence  uint64
	ResponseSequence uint64
	OrderSequence    uint64
	Err              error
}

func (PromptObservation) Kind() ObservationKind { return ObservationPrompt }
func (o PromptObservation) Sequence() uint64 {
	if o.OrderSequence != 0 {
		return o.OrderSequence
	}
	return o.ReceiveSequence
}
func (PromptObservation) observation() {}

// UpdateObservation is one normalized update translated from an ACP session
// notification. One notification may produce multiple observations, each
// carrying the same receive sequence so an ordered backend never relies on
// competing goroutine arrival order.
type UpdateObservation struct {
	Event           Event
	ReceiveSequence uint64
	OrderSequence   uint64
}

func (UpdateObservation) Kind() ObservationKind { return ObservationUpdate }
func (o UpdateObservation) Sequence() uint64 {
	if o.OrderSequence != 0 {
		return o.OrderSequence
	}
	return o.ReceiveSequence
}
func (UpdateObservation) observation() {}

// SteerObservation is one ordered steering response. Embedding SteerResult
// keeps admission and receive-order facts available without a second mutable
// representation.
type SteerObservation struct {
	SteerResult
	Err error
}

func (SteerObservation) Kind() ObservationKind { return ObservationSteer }
func (o SteerObservation) Sequence() uint64 {
	if o.OrderSequence != 0 {
		return o.OrderSequence
	}
	return o.ReceiveSequence
}
func (SteerObservation) observation() {}

var _ Observation = PromptObservation{}
var _ Observation = UpdateObservation{}
var _ Observation = SteerObservation{}
