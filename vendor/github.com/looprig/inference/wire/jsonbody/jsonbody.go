// Package jsonbody holds small stdlib helpers for JSON HTTP bodies: marshal a value
// into a request-body reader (with its content type) and unmarshal response bytes back
// into a value. It is byte-level wire framing only — it knows nothing about LLM
// messages, tools, usage, or provider semantics.
package jsonbody

import (
	"bytes"
	"encoding/json"
	"io"
)

// ContentType is the media type a JSON body advertises. Exported so callers (codecs,
// custom encoders) set the request Content-Type from a single source of truth.
const ContentType = "application/json"

// EncodeError wraps a JSON marshal failure. Typed per the repo rule so callers can
// errors.As it rather than string-matching.
type EncodeError struct {
	Err error
}

func (e *EncodeError) Error() string { return "jsonbody: encode: " + e.Err.Error() }
func (e *EncodeError) Unwrap() error { return e.Err }

// DecodeError wraps a JSON unmarshal failure. Typed per the repo rule.
type DecodeError struct {
	Err error
}

func (e *DecodeError) Error() string { return "jsonbody: decode: " + e.Err.Error() }
func (e *DecodeError) Unwrap() error { return e.Err }

// Encode marshals v to a JSON body, returning an io.Reader over the bytes and the
// application/json content type. v is `any` because this is the JSON serialization
// boundary, where the repo rule permits it.
func Encode(v any) (io.Reader, string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, "", &EncodeError{Err: err}
	}
	return bytes.NewReader(b), ContentType, nil
}

// Decode unmarshals JSON bytes into v. v is `any` because this is the JSON
// serialization boundary.
func Decode(data []byte, v any) error {
	if err := json.Unmarshal(data, v); err != nil {
		return &DecodeError{Err: err}
	}
	return nil
}
