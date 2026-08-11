package event

import (
	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	contextcount "github.com/looprig/inference/contextcount"
	model "github.com/looprig/inference/model"
)

// ContextRevision is the loop-local revision of the committed active context.
// Zero is invalid because it cannot identify a committed context mutation.
type ContextRevision uint64

// ContextBasis identifies the exact durable context included in a measurement.
type ContextBasis struct {
	Revision       ContextRevision `json:"revision"`
	ThroughEventID uuid.UUID       `json:"through_event_id"`
}

// ContextMeasurement is one authoritative complete-request input count.
type ContextMeasurement struct {
	Basis              ContextBasis              `json:"basis"`
	Model              model.ModelKey            `json:"model"`
	RequestFingerprint [32]byte                  `json:"request_fingerprint"`
	InputTokens        content.TokenCount        `json:"input_tokens"`
	InputLimit         content.TokenCount        `json:"input_limit"`
	Quality            contextcount.CountQuality `json:"quality"`
}

// ContextField identifies an invalid structural measurement field.
type ContextField string

const (
	ContextFieldRevision           ContextField = "Revision"
	ContextFieldThroughEventID     ContextField = "ThroughEventID"
	ContextFieldModel              ContextField = "Model"
	ContextFieldRequestFingerprint ContextField = "RequestFingerprint"
	ContextFieldInputLimit         ContextField = "InputLimit"
	ContextFieldQuality            ContextField = "Quality"
)

// ContextValidationError reports malformed replayable context metadata.
type ContextValidationError struct {
	Field ContextField
	Cause error
}

func (e *ContextValidationError) Error() string {
	message := "event: invalid context field " + string(e.Field)
	if e.Cause != nil {
		return message + ": " + e.Cause.Error()
	}
	return message
}

func (e *ContextValidationError) Unwrap() error { return e.Cause }

// Validate checks structural invariants without treating over-limit occupancy as
// malformed: raw counts above InputLimit remain valid audit evidence.
func (m ContextMeasurement) Validate() error {
	if m.Basis.Revision == 0 {
		return &ContextValidationError{Field: ContextFieldRevision}
	}
	if m.Basis.ThroughEventID.IsZero() {
		return &ContextValidationError{Field: ContextFieldThroughEventID}
	}
	if err := m.Model.Validate(); err != nil {
		return &ContextValidationError{Field: ContextFieldModel, Cause: err}
	}
	if m.RequestFingerprint == ([32]byte{}) {
		return &ContextValidationError{Field: ContextFieldRequestFingerprint}
	}
	if m.InputLimit == 0 {
		return &ContextValidationError{Field: ContextFieldInputLimit}
	}
	if !validContextCountQuality(m.Quality) {
		return &ContextValidationError{Field: ContextFieldQuality}
	}
	return nil
}

func validContextCountQuality(quality contextcount.CountQuality) bool {
	return quality == contextcount.CountQualityExactProvider ||
		quality == contextcount.CountQualityExactLocal ||
		quality == contextcount.CountQualityHeuristicEstimate
}

// BasisPoints is a percentage scaled by 100. Values above 10_000 are invalid.
type BasisPoints uint16

const FullScaleBasisPoints BasisPoints = 10_000

// PressureLevel is the closed current-context pressure domain.
type PressureLevel uint8

const (
	PressureUnknown PressureLevel = iota
	PressureNormal
	PressureCompact
	PressureHardLimit
)

func (p PressureLevel) validCurrent() bool {
	return p >= PressureNormal && p <= PressureHardLimit
}

func (p PressureLevel) validPrevious() bool {
	return p >= PressureUnknown && p <= PressureHardLimit
}

// ContextMeasured durably publishes the latest authoritative measurement.
type ContextMeasured struct {
	enduring
	loopScoped
	Header
	Measurement ContextMeasurement `json:"measurement"`
}

// ContextPressure is a droppable public level-change signal.
type ContextPressure struct {
	ephemeral
	loopScoped
	Header
	Measurement ContextMeasurement `json:"measurement"`
	Occupancy   BasisPoints        `json:"occupancy"`
	Previous    PressureLevel      `json:"previous"`
	Current     PressureLevel      `json:"current"`
}

func validateContextPressure(value ContextPressure) error {
	if err := value.Measurement.Validate(); err != nil {
		return err
	}
	if value.Occupancy > FullScaleBasisPoints {
		return &ContextValidationError{Field: ContextField("Occupancy")}
	}
	if !value.Previous.validPrevious() {
		return &ContextValidationError{Field: ContextField("Previous")}
	}
	if !value.Current.validCurrent() {
		return &ContextValidationError{Field: ContextField("Current")}
	}
	if value.Previous == value.Current {
		return &ContextValidationError{Field: ContextField("Current")}
	}
	return nil
}

func (ContextMeasured) isEvent() {}
func (ContextPressure) isEvent() {}
