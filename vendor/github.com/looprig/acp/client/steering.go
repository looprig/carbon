package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/looprig/acp/protocol"
)

// methodSessionSteering is the one fixed ACP extension method exposed by
// Session.Steer. It intentionally remains unexported: callers cannot turn
// this typed API into arbitrary method probing.
const methodSessionSteering = "_session/steering"

// SteerParams is the typed request shape for _session/steering. SessionID is
// overwritten with the receiver's ID by Session.Steer, so a caller cannot
// steer another Session through an existing Session value. Meta is optional
// ACP extension metadata and is passed through as caller-owned JSON bytes;
// this package does not interpret adapter capability or profile policy.
type SteerParams struct {
	SessionID protocol.SessionID      `json:"sessionId"`
	Prompt    []protocol.ContentBlock `json:"prompt"`
	Meta      json.RawMessage         `json:"_meta,omitempty"`
}

// SteerOutcome is the bounded normalized outcome vocabulary understood by the
// foreign driver. An empty or unknown value is preserved as an empty/unknown
// outcome so the driver can fail closed; the client never guesses policy from
// a method error or probes a second method.
type SteerOutcome string

const (
	SteerOutcomeInjected       SteerOutcome = "injected"
	SteerOutcomePromptRequired SteerOutcome = "promptRequired"
	SteerOutcomeStartedNewTurn SteerOutcome = "startedNewTurn"
	SteerOutcomeFailed         SteerOutcome = "failed"
)

// SteerResult is the typed, bounded result of Session.Steer. Outcome and
// Reason are the extension's normalized response facts; raw wire payloads
// are deliberately not exposed. Transport facts remain available even when
// err is non-nil, which lets a caller distinguish a proven pre-admission
// failure from an admitted but ambiguous/erroring call.
type SteerResult struct {
	Outcome SteerOutcome
	Reason  string

	WriteAdmitted    bool
	ReceiveSequence  uint64
	ResponseSequence uint64
}

// SteerCompletion is the exactly-once terminal value delivered by a
// SteerHandle. Result retains typed wire and transport facts even when Err is
// non-nil; Err is bounded by newSteeringError whenever the request reached
// protocol transport.
type SteerCompletion struct {
	Result SteerResult
	Err    error
}

// SteerHandle owns one asynchronous fixed-method steering request. Both
// channels have capacity one and deliver exactly one value before closing.
// Cancel is idempotent. If admission already reported true, cancellation
// stops response observation but does not retract the admitted frame.
type SteerHandle struct {
	admission chan bool
	result    chan SteerCompletion
	cancel    context.CancelFunc
}

// Admission reports whether this request crossed the protocol Writer queue
// admission boundary. A false value proves the steering frame was not
// eligible for the underlying transport.
func (h *SteerHandle) Admission() <-chan bool {
	if h == nil {
		return nil
	}
	return h.admission
}

// Result reports the one final steering completion.
func (h *SteerHandle) Result() <-chan SteerCompletion {
	if h == nil {
		return nil
	}
	return h.result
}

// Cancel cancels this handle's response observation. It never retracts a
// frame that already crossed Writer admission.
func (h *SteerHandle) Cancel() {
	if h != nil && h.cancel != nil {
		h.cancel()
	}
}

const maxSteerReasonBytes = 1024

// maxSteeringErrorMessageBytes bounds peer-controlled diagnostic text kept by
// a SteeringError. Wire Data is intentionally discarded entirely; the driver
// receives only this bounded code/message pair and transport facts.
const maxSteeringErrorMessageBytes = 1024

// SteeringError is the bounded typed error returned by Session.Steer after a
// request reached the protocol layer. It never exposes the peer's raw error
// Data or an unbounded transport diagnostic. Code is the peer JSON-RPC code
// when one was received, or ErrorCodeInternalError for a local/transport
// failure.
type SteeringError struct {
	Code             protocol.ErrorCode
	Message          string
	WriteAdmitted    bool
	ReceiveSequence  uint64
	ResponseSequence uint64
	cause            error
}

type steeringTransportCauseKind uint8

const (
	steeringTransportCauseGeneric steeringTransportCauseKind = iota + 1
	steeringTransportCauseConnectionClosed
	steeringTransportCauseWriterClosed
)

// boundedSteeringTransportCause retains local transport classification while
// avoiding an unbounded local error string or an accidental peer Fault cause
// chain. Is keeps sentinel identity checks useful; As exposes only bounded
// typed closure facts.
type boundedSteeringTransportCause struct {
	kind     steeringTransportCauseKind
	identity error
}

func (e *boundedSteeringTransportCause) Error() string {
	switch e.kind {
	case steeringTransportCauseConnectionClosed:
		return "acp/client: connection closed"
	case steeringTransportCauseWriterClosed:
		return "acp/client: writer closed"
	default:
		return "acp/client: steering transport failed"
	}
}

func (e *boundedSteeringTransportCause) Is(target error) bool {
	if target == nil {
		return false
	}
	switch e.kind {
	case steeringTransportCauseConnectionClosed:
		if _, ok := target.(*protocol.ConnClosedError); ok {
			return true
		}
		if _, ok := target.(*ClosedError); ok {
			return true
		}
	case steeringTransportCauseWriterClosed:
		if _, ok := target.(*protocol.WriterClosedError); ok {
			return true
		}
	}
	return e.identity != nil && errors.Is(e.identity, target)
}

func (e *boundedSteeringTransportCause) As(target any) bool {
	switch target := target.(type) {
	case **protocol.ConnClosedError:
		if e.kind == steeringTransportCauseConnectionClosed {
			*target = &protocol.ConnClosedError{}
			return true
		}
	case **ClosedError:
		if e.kind == steeringTransportCauseConnectionClosed {
			*target = &ClosedError{}
			return true
		}
	case **protocol.WriterClosedError:
		if e.kind == steeringTransportCauseWriterClosed {
			*target = &protocol.WriterClosedError{}
			return true
		}
	}
	return false
}

func (e *SteeringError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Message == "" {
		return fmt.Sprintf("acp/client: steering failed (code %d)", e.Code)
	}
	return fmt.Sprintf("acp/client: steering failed (code %d): %s", e.Code, e.Message)
}

// Unwrap preserves cancellation and local transport classification without
// ever retaining a peer *protocol.Fault (whose Data may contain raw wire
// payload). Peer faults are represented only by SteeringError's bounded code
// and message.
func (e *SteeringError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// StartSteer starts the fixed _session/steering request for this Session. It
// does not perform capability/profile allowlisting or an unknown-method
// probe; those policy decisions belong to the foreign driver. The returned
// handle always resolves both channels exactly once, including not-dialed and
// closed-client failures.
func (s *Session) StartSteer(ctx context.Context, p SteerParams) *SteerHandle {
	if ctx == nil {
		ctx = context.Background()
	}
	callCtx, cancel := context.WithCancel(ctx)
	h := &SteerHandle{
		admission: make(chan bool, 1),
		result:    make(chan SteerCompletion, 1),
		cancel:    cancel,
	}
	go s.runSteer(callCtx, h, p)
	return h
}

func (s *Session) runSteer(ctx context.Context, h *SteerHandle, p SteerParams) {
	defer h.cancel()
	defer close(h.result)

	agent, err := s.client.currentAgent()
	if err != nil {
		h.admission <- false
		close(h.admission)
		h.result <- SteerCompletion{Err: err}
		return
	}

	p.SessionID = s.id
	var wire struct {
		Outcome string `json:"outcome"`
		Reason  string `json:"reason,omitempty"`
	}
	call, err := agent.StartExtensionCall(ctx, methodSessionSteering, p, &wire)
	if err != nil {
		h.admission <- false
		close(h.admission)
		facts := protocol.CallResult{}
		h.result <- SteerCompletion{Err: newSteeringError(err, facts)}
		return
	}

	admitted, ok := <-call.Admission()
	if !ok {
		admitted = false
	}
	h.admission <- admitted
	close(h.admission)

	completion, ok := <-call.Result()
	if !ok {
		completion = protocol.AsyncCallResult{
			Facts: protocol.CallResult{WriteAdmitted: admitted},
			Err:   errors.New("acp/client: steering call ended without a completion"),
		}
	}
	result := steerResultFromWire(wire, completion.Facts)
	if completion.Err != nil {
		completion.Err = newSteeringError(completion.Err, completion.Facts)
	}
	h.result <- SteerCompletion{Result: result, Err: completion.Err}
}

func steerResultFromWire(wire struct {
	Outcome string `json:"outcome"`
	Reason  string `json:"reason,omitempty"`
}, facts protocol.CallResult) SteerResult {
	return SteerResult{
		Outcome:          SteerOutcome(wire.Outcome),
		Reason:           boundSteerReason(wire.Reason),
		WriteAdmitted:    facts.WriteAdmitted,
		ReceiveSequence:  facts.ReceiveSequence,
		ResponseSequence: facts.ResponseSequence,
	}
}

// Steer sends the fixed _session/steering request for this Session and waits
// for StartSteer's exactly-once completion. It is retained as the synchronous
// compatibility wrapper for callers that do not need early admission.
func (s *Session) Steer(ctx context.Context, p SteerParams) (SteerResult, error) {
	h := s.StartSteer(ctx, p)
	completion, ok := <-h.Result()
	if !ok {
		return SteerResult{}, errors.New("acp/client: steering call ended without a completion")
	}
	return completion.Result, completion.Err
}

func newSteeringError(err error, facts protocol.CallResult) error {
	if err == nil {
		return nil
	}
	steeringErr := &SteeringError{
		Code:             protocol.ErrorCodeInternalError,
		Message:          "steering request failed",
		WriteAdmitted:    facts.WriteAdmitted,
		ReceiveSequence:  facts.ReceiveSequence,
		ResponseSequence: facts.ResponseSequence,
	}
	var fault *protocol.Fault
	if errors.As(err, &fault) && fault != nil {
		steeringErr.Code = fault.Code
		steeringErr.Message = boundSteeringMessage(fault.Message, maxSteeringErrorMessageBytes)
		return steeringErr
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		steeringErr.Code = protocol.ErrorCodeRequestCancelled
		steeringErr.Message = "steering request canceled"
		steeringErr.cause = err
		return steeringErr
	}
	steeringErr.cause = boundedSteeringTransportCauseFor(err)
	return steeringErr
}

func boundedSteeringTransportCauseFor(err error) error {
	var connClosed *protocol.ConnClosedError
	if errors.As(err, &connClosed) {
		return &boundedSteeringTransportCause{
			kind:     steeringTransportCauseConnectionClosed,
			identity: err,
		}
	}
	var writerClosed *protocol.WriterClosedError
	if errors.As(err, &writerClosed) {
		return &boundedSteeringTransportCause{
			kind:     steeringTransportCauseWriterClosed,
			identity: err,
		}
	}
	return &boundedSteeringTransportCause{
		kind:     steeringTransportCauseGeneric,
		identity: err,
	}
}

func boundSteerReason(reason string) string {
	return boundSteeringMessage(reason, maxSteerReasonBytes)
}

func boundSteeringMessage(message string, limit int) string {
	if len(message) <= limit && utf8.ValidString(message) {
		return sanitizeSteeringMessage(message)
	}
	if limit <= 0 {
		return ""
	}
	bounded := make([]byte, 0, minInt(len(message), limit))
	for len(message) > 0 && len(bounded) < limit {
		r, size := utf8.DecodeRuneInString(message)
		encodedSize := utf8.RuneLen(r)
		if encodedSize < 0 {
			encodedSize = 1
		}
		if size == 0 || len(bounded)+encodedSize > limit {
			break
		}
		if r < 0x20 && r != '\t' {
			r = ' '
		}
		bounded = utf8.AppendRune(bounded, r)
		message = message[size:]
	}
	return string(bounded)
}

func sanitizeSteeringMessage(message string) string {
	bounded := make([]byte, 0, len(message))
	for len(message) > 0 {
		r, size := utf8.DecodeRuneInString(message)
		if size == 0 {
			break
		}
		if r < 0x20 && r != '\t' {
			r = ' '
		}
		bounded = utf8.AppendRune(bounded, r)
		message = message[size:]
	}
	return string(bounded)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
