package journal

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
)

// FenceEncodeError wraps a failure to marshal a LeaseFence to JSON. A LeaseFence
// is a single uint64, so this is effectively unreachable, but the codec returns a
// typed error rather than dropping the json.Marshal error to satisfy the
// errors-are-typed contract at the package API.
type FenceEncodeError struct{ Cause error }

func (e *FenceEncodeError) Error() string {
	return "journal: encode lease fence: " + e.Cause.Error()
}
func (e *FenceEncodeError) Unwrap() error { return e.Cause }

// FenceDecodeError wraps a failure to decode LeaseFence bytes at the untrusted
// restore boundary: malformed JSON, a wrong field type, a non-object, or trailing
// data after the object. The codec fails closed with this typed error so callers
// inspect the cause via errors.As rather than guessing an epoch.
type FenceDecodeError struct {
	Reason string
	Cause  error
}

func (e *FenceDecodeError) Error() string {
	if e.Cause == nil {
		return "journal: decode lease fence: " + e.Reason
	}
	return "journal: decode lease fence: " + e.Reason + ": " + e.Cause.Error()
}
func (e *FenceDecodeError) Unwrap() error { return e.Cause }

// MarshalLeaseFence encodes a LeaseFence as its minimal JSON object {"epoch":N}.
func MarshalLeaseFence(f LeaseFence) ([]byte, error) {
	data, err := json.Marshal(f)
	if err != nil {
		return nil, &FenceEncodeError{Cause: err}
	}
	return data, nil
}

// UnmarshalLeaseFence decodes bytes produced by MarshalLeaseFence. It fails closed
// with a *FenceDecodeError on any malformed input — empty bytes, non-object JSON, a
// non-numeric/negative epoch, an unknown field, or trailing bytes after the
// object. A negative or non-integer epoch fails because Epoch is a uint64.
func UnmarshalLeaseFence(data []byte) (LeaseFence, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var f LeaseFence
	if err := dec.Decode(&f); err != nil {
		return LeaseFence{}, &FenceDecodeError{Reason: "invalid json", Cause: err}
	}
	// Reject trailing bytes after the object (e.g. `{"epoch":1}trailing`): a single
	// record decodes to exactly one JSON value with nothing after it.
	if _, err := dec.Token(); err != io.EOF {
		return LeaseFence{}, &FenceDecodeError{Reason: "trailing data after object"}
	}
	return f, nil
}

// GatePreparedEncodeError wraps a failure to marshal a GatePreparedRecord to JSON:
// either the embedded GatePrepared event or the gate.OpenPayload failed its codec.
type GatePreparedEncodeError struct {
	Stage string // "prepared" or "payload"
	Cause error
}

func (e *GatePreparedEncodeError) Error() string {
	return "journal: encode gate prepared " + e.Stage + ": " + e.Cause.Error()
}
func (e *GatePreparedEncodeError) Unwrap() error { return e.Cause }

// GatePreparedDecodeError wraps a failure to decode GatePreparedRecord bytes at
// the untrusted restore boundary: malformed JSON, a malformed embedded event, or
// a malformed embedded payload. It fails secure rather than skipping or
// zero-valuing the record.
type GatePreparedDecodeError struct {
	Stage string // "json", "prepared", or "payload"
	Cause error
}

func (e *GatePreparedDecodeError) Error() string {
	return "journal: decode gate prepared " + e.Stage + ": " + e.Cause.Error()
}
func (e *GatePreparedDecodeError) Unwrap() error { return e.Cause }

// gatePreparedWire is the JSON body of a GatePreparedRecord's envelope: the
// serialized GatePrepared event and the serialized gate.OpenPayload, both
// pre-encoded by their own codecs so the record is self-describing on restore.
type gatePreparedWire struct {
	Prepared json.RawMessage `json:"prepared"`
	Payload  json.RawMessage `json:"payload"`
}

// MarshalGatePreparedRecord encodes a GatePreparedRecord into the JSON body that
// the sessionstore envelope carries: the GatePrepared event (via
// event.MarshalEvent) and the sealed gate.OpenPayload (via gate.MarshalPayload),
// both as raw sibling keys. A nil payload is rejected fail-closed — a prepared
// record without its validation payload is corrupt.
func MarshalGatePreparedRecord(rec GatePreparedRecord) ([]byte, error) {
	preparedJSON, err := event.MarshalEvent(rec.Prepared())
	if err != nil {
		return nil, &GatePreparedEncodeError{Stage: "prepared", Cause: err}
	}
	payloadJSON, err := gate.MarshalPayload(rec.Payload())
	if err != nil {
		return nil, &GatePreparedEncodeError{Stage: "payload", Cause: err}
	}
	out, err := json.Marshal(gatePreparedWire{Prepared: preparedJSON, Payload: payloadJSON})
	if err != nil {
		return nil, &GatePreparedEncodeError{Stage: "json", Cause: err}
	}
	return out, nil
}

// UnmarshalGatePreparedRecord decodes bytes produced by MarshalGatePreparedRecord
// back into a GatePreparedRecord. It fails closed with a typed
// *GatePreparedDecodeError on any malformed input — malformed JSON, a malformed
// embedded event, or a malformed embedded payload — so restore never silently
// drops or zero-values a private prepared record.
func UnmarshalGatePreparedRecord(data []byte) (GatePreparedRecord, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var w gatePreparedWire
	if err := dec.Decode(&w); err != nil {
		return GatePreparedRecord{}, &GatePreparedDecodeError{Stage: "json", Cause: err}
	}
	if _, err := dec.Token(); err != io.EOF {
		return GatePreparedRecord{}, &GatePreparedDecodeError{Stage: "json", Cause: errors.New("trailing data after object")}
	}
	prepared, err := event.UnmarshalEvent(w.Prepared)
	if err != nil {
		return GatePreparedRecord{}, &GatePreparedDecodeError{Stage: "prepared", Cause: err}
	}
	gp, ok := prepared.(event.GatePrepared)
	if !ok {
		return GatePreparedRecord{}, &GatePreparedDecodeError{Stage: "prepared", Cause: errors.New("embedded event is not GatePrepared")}
	}
	payload, err := gate.UnmarshalPayload(w.Payload)
	if err != nil {
		return GatePreparedRecord{}, &GatePreparedDecodeError{Stage: "payload", Cause: err}
	}
	open, ok := payload.(gate.OpenPayload)
	if !ok {
		return GatePreparedRecord{}, &GatePreparedDecodeError{Stage: "payload", Cause: errors.New("embedded payload is not OpenPayload")}
	}
	return NewGatePreparedRecord(gp, open), nil
}
