package event

import "errors"

// RestoredError is the leaf error a restored TurnFailed (or RestoreErrored) carries
// in place of its original typed cause. A live error value has no stable JSON codec
// — its concrete type, fields, and wrapping chain cannot round-trip through
// encoding/json — so the event codec projects TurnFailed.Err to a {kind,message}
// pair on marshal and reconstructs a *RestoredError on unmarshal. Kind is the stable
// classification (see ErrKind); Message is the original Error() text, preserved
// verbatim so no human-readable detail is lost even when the concrete type is not.
// RestoredError deliberately does not implement ModelFacingError: ordinary and
// legacy journal records must never become model-facing merely because their
// message or kind happens to look safe.
type RestoredError struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// Error renders "<kind>: <message>", mirroring how the original typed cause read.
func (e *RestoredError) Error() string { return e.Kind + ": " + e.Message }

// RestoredModelFacingError is the distinct restore form for an error that was
// explicitly marked safe before it crossed the durable event boundary. Detail is
// kept separate from Kind and Message so the restore path never infers safety
// from arbitrary legacy error text.
type RestoredModelFacingError struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
	Detail  string `json:"-"`
}

func (e *RestoredModelFacingError) Error() string { return e.Kind + ": " + e.Message }

// ModelFacingError returns the already-normalized, bounded detail persisted by
// the event codec. Callers still bound it at their presentation boundary.
func (e *RestoredModelFacingError) ModelFacingError() string {
	if e == nil {
		return ""
	}
	return e.Detail
}

// Stable kind strings ErrKind projects the event package's own TurnFailed.Err causes
// to. They are part of the durable wire contract: never rename one (old journals
// carry the old string); only add new constants. Provider/stream errors that flow
// through streamFailure are not enumerated here on purpose — the event package is a
// leaf (it must not import the LLM provider layer), so they fall back to KindUnknown
// while their full Error() text is still preserved in RestoredError.Message.
const (
	// KindEmptyResponse classifies *EmptyResponseError.
	KindEmptyResponse = "empty_response"
	// KindToolLimit classifies *ToolLimitError (the runaway guard).
	KindToolLimit = "tool_limit"
	// KindTurnPanic classifies *TurnPanicError (a recovered turn-goroutine panic).
	KindTurnPanic = "turn_panic"
	// KindUnknown is the fail-open fallback for any error the switch does not
	// recognize, including the open-ended provider/stream errors. It is stable: a
	// caller can rely on "unknown" for an unrecognized cause.
	KindUnknown = "unknown"
)

// ErrKind maps a concrete error to its stable kind string via an errors.As switch
// over the known in-package TurnFailed.Err causes. It is what the event codec uses
// to project TurnFailed.Err into RestoredError.Kind on marshal. An already-restored
// *RestoredError re-projects to its own Kind (idempotent re-marshal); a nil or
// unrecognized error yields KindUnknown. errors.As (not a bare type switch) so a
// wrapped known cause is still classified by its concrete type.
func ErrKind(err error) string {
	if err == nil {
		return KindUnknown
	}
	var (
		empty         *EmptyResponseError
		toolLim       *ToolLimitError
		panicErr      *TurnPanicError
		restored      *RestoredError
		modelRestored *RestoredModelFacingError
	)
	switch {
	case errors.As(err, &empty):
		return KindEmptyResponse
	case errors.As(err, &toolLim):
		return KindToolLimit
	case errors.As(err, &panicErr):
		return KindTurnPanic
	case errors.As(err, &modelRestored):
		return modelRestored.Kind
	case errors.As(err, &restored):
		// Idempotent: re-projecting an already-restored error preserves its Kind
		// rather than collapsing it to "unknown" on a second round-trip.
		return restored.Kind
	default:
		return KindUnknown
	}
}
