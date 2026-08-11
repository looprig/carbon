package inference

import (
	"crypto/sha256"

	"github.com/looprig/inference/stream"
)

const (
	// MaxStructuredOutputDiagnosticBytes bounds caller-controlled string
	// metadata retained by structured-output errors.
	MaxStructuredOutputDiagnosticBytes = 128

	// MaxStructuredResultBytes bounds a native structured result before JSON
	// parsing or compaction. It matches the 1 MiB request-schema bound so both
	// structured-output boundaries have a single conservative memory ceiling.
	MaxStructuredResultBytes = 1 << 20

	// StructuredOutputFinishReasonOther classifies a non-empty finish reason
	// outside the provider-neutral set without retaining provider input.
	StructuredOutputFinishReasonOther stream.FinishReason = "other"
)

// SchemaValidationField identifies the output-schema component that failed
// validation. Values are stable classifications suitable for errors.As callers.
type SchemaValidationField string

const (
	SchemaFieldName                 SchemaValidationField = "Name"
	SchemaFieldDescription          SchemaValidationField = "Description"
	SchemaFieldSchema               SchemaValidationField = "Schema"
	SchemaFieldKeyword              SchemaValidationField = "Keyword"
	SchemaFieldType                 SchemaValidationField = "Type"
	SchemaFieldProperties           SchemaValidationField = "Properties"
	SchemaFieldItems                SchemaValidationField = "Items"
	SchemaFieldEnum                 SchemaValidationField = "Enum"
	SchemaFieldRequired             SchemaValidationField = "Required"
	SchemaFieldAdditionalProperties SchemaValidationField = "AdditionalProperties"
	SchemaFieldOutput               SchemaValidationField = "Output"
)

// SchemaValidationReason identifies why an output-schema component failed
// validation. It intentionally contains no caller-provided content.
type SchemaValidationReason string

const (
	SchemaReasonEmpty             SchemaValidationReason = "empty"
	SchemaReasonInvalid           SchemaValidationReason = "invalid"
	SchemaReasonReserved          SchemaValidationReason = "reserved"
	SchemaReasonTooLong           SchemaValidationReason = "too long"
	SchemaReasonInvalidUTF8       SchemaValidationReason = "invalid UTF-8"
	SchemaReasonMalformed         SchemaValidationReason = "malformed"
	SchemaReasonTooLarge          SchemaValidationReason = "too large"
	SchemaReasonRootNotObject     SchemaValidationReason = "root is not an object schema"
	SchemaReasonUnknownKeyword    SchemaValidationReason = "unknown keyword"
	SchemaReasonMissing           SchemaValidationReason = "missing"
	SchemaReasonUnsupported       SchemaValidationReason = "unsupported"
	SchemaReasonMustBeFalse       SchemaValidationReason = "must be false"
	SchemaReasonDuplicate         SchemaValidationReason = "duplicate"
	SchemaReasonUnknownProperty   SchemaValidationReason = "unknown property"
	SchemaReasonTypeMismatch      SchemaValidationReason = "type mismatch"
	SchemaReasonTooDeep           SchemaValidationReason = "too deep"
	SchemaReasonTooManyProperties SchemaValidationReason = "too many properties"
	SchemaReasonInvalidTarget     SchemaValidationReason = "invalid target"
	SchemaReasonDecodeFailed      SchemaValidationReason = "decode failed"
)

// SchemaValidationError reports a stable, bounded validation classification.
// It never retains schema bytes, property names, descriptions, or JSON decoder
// errors because those values may contain sensitive caller input.
type SchemaValidationError struct {
	Field      SchemaValidationField
	ReasonCode SchemaValidationReason
}

func (e *SchemaValidationError) Error() string {
	return "inference: invalid output schema field " + string(e.Field) + ": " + string(e.ReasonCode)
}

// StructuredOutputUnsupportedError reports that a model does not advertise
// native structured output. Model is diagnostic metadata only.
type StructuredOutputUnsupportedError struct {
	Model string
}

func (e *StructuredOutputUnsupportedError) Error() string {
	return "inference: structured output unsupported"
}

// StructuredOutputWithToolsUnsupportedError reports that a model does not
// advertise the distinct native structured-output-with-tools capability.
// Model is diagnostic metadata only.
type StructuredOutputWithToolsUnsupportedError struct {
	Model string
}

func (e *StructuredOutputWithToolsUnsupportedError) Error() string {
	return "inference: structured output with tools unsupported"
}

// ImageInputUnsupportedError reports that a request thread carries image
// blocks but the model does not advertise the AcceptsImages capability.
// Model is bounded diagnostic metadata only.
type ImageInputUnsupportedError struct {
	Model string
}

func (e *ImageInputUnsupportedError) Error() string {
	return "inference: image input unsupported"
}

// StructuredOutputConflictError reports an invalid request feature
// combination. Feature is a bounded classification, never a schema or tool
// payload supplied by the caller.
type StructuredOutputConflictError struct {
	Feature string
}

func (e *StructuredOutputConflictError) Error() string {
	return "inference: structured output feature conflict: " + e.Feature
}

// MalformedStructuredOutputReason is a bounded classification for an invalid
// structured response representation. It never contains model output.
type MalformedStructuredOutputReason string

const (
	MalformedReasonNilResponse           MalformedStructuredOutputReason = "nil response"
	MalformedReasonNilMessage            MalformedStructuredOutputReason = "nil message"
	MalformedReasonWrongRole             MalformedStructuredOutputReason = "wrong role"
	MalformedReasonEmpty                 MalformedStructuredOutputReason = "empty"
	MalformedReasonMalformedJSON         MalformedStructuredOutputReason = "malformed JSON"
	MalformedReasonRootNotObject         MalformedStructuredOutputReason = "root is not object"
	MalformedReasonInvalidRepresentation MalformedStructuredOutputReason = "invalid representation"
	MalformedReasonAmbiguous             MalformedStructuredOutputReason = "ambiguous"
	MalformedReasonInvalidBlock          MalformedStructuredOutputReason = "invalid block"
	MalformedReasonNilBlock              MalformedStructuredOutputReason = "nil block"
	MalformedReasonTooLarge              MalformedStructuredOutputReason = "too large"
)

// MalformedStructuredOutputError reports bounded metadata about malformed
// model output. SHA256 and Length support correlation without retaining or
// exposing the raw output bytes.
type MalformedStructuredOutputError struct {
	ReasonCode MalformedStructuredOutputReason
	Length     int
	SHA256     [sha256.Size]byte
}

func (e *MalformedStructuredOutputError) Error() string {
	return "inference: malformed structured output: " + string(e.ReasonCode)
}

// StructuredOutputFinishError reports finish metadata that cannot safely
// produce the requested structured result.
type StructuredOutputFinishError struct {
	Reason stream.FinishReason
}

func (e *StructuredOutputFinishError) Error() string {
	return "inference: structured output rejected by finish reason"
}
