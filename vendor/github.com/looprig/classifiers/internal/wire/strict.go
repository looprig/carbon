package wire

// Strict JSON decoding helpers shared by input.go and output.go. This is a
// deliberate structural replica of the discipline
// github.com/looprig/harness/pkg/gate uses for its own strict wire codecs
// (see review_wire.go and payload.go in that module): a duplicate-key
// scanner, DisallowUnknownFields plus a trailing-value check, and an
// explicit raw-JSON object/array/string walk so a JSON `null` can never
// masquerade as an omitted (and therefore zero-valued) field. It is
// reimplemented here rather than imported because pkg/gate does not export
// it, and this module may only depend on Harness's public packages.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// maxScanJSONDepth bounds the duplicate-field scanner's recursion on
// untrusted input, matching the nesting limit encoding/json enforces during
// Decode so the scanner can never be driven deeper than the decode that
// follows it.
const maxScanJSONDepth = 10000

func isExplicitJSONNull(data []byte) bool {
	return bytes.Equal(bytes.TrimSpace(data), []byte("null"))
}

// decodeStrict decodes exactly one JSON value into v, rejecting unknown
// fields and any trailing JSON content.
func decodeStrict(data []byte, v any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(v); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

// rejectDuplicateJSONFields walks data and fails if any JSON object in it
// (at any depth) repeats a key, or if data is not exactly one JSON value.
func rejectDuplicateJSONFields(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := scanJSONValue(decoder, 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder, depth int) error {
	if depth > maxScanJSONDepth {
		return errors.New("JSON nesting exceeds depth limit")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("object key is not a string")
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("duplicate JSON field %q", key)
			}
			seen[key] = struct{}{}
			if err := scanJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	_, err = decoder.Token()
	return err
}

// requireWireObject decodes data as a JSON object with EXACTLY the given
// required member names present (no fewer, no more) and each value
// non-null. A `null` object, a missing key, or an extra key are all
// rejected here, so a required field can never be silently supplied as
// JSON null even though its Go zero value would otherwise be
// indistinguishable from an omitted key.
func requireWireObject(data []byte, required []string) (map[string]json.RawMessage, error) {
	if isExplicitJSONNull(data) {
		return nil, errWireShape
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return nil, errWireShape
	}
	if len(object) != len(required) {
		return nil, errWireShape
	}
	for _, key := range required {
		if _, ok := object[key]; !ok {
			return nil, errWireShape
		}
	}
	return object, nil
}

// requireWireArray decodes data as a JSON array, rejecting an explicit
// JSON null (an empty array `[]` remains valid).
func requireWireArray(data []byte) ([]json.RawMessage, error) {
	if isExplicitJSONNull(data) {
		return nil, errWireShape
	}
	var values []json.RawMessage
	if err := json.Unmarshal(data, &values); err != nil || values == nil {
		return nil, errWireShape
	}
	return values, nil
}

// requireWireStrings validates that every named member of object is present
// as a non-null JSON string (an empty string "" is a valid string value;
// only `null` and non-string values are rejected here).
func requireWireStrings(object map[string]json.RawMessage, keys ...string) error {
	for _, key := range keys {
		if err := requireWireString(object[key]); err != nil {
			return err
		}
	}
	return nil
}

func requireWireString(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '"' {
		return errWireShape
	}
	var value string
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return errWireShape
	}
	return nil
}

var errWireShape = errors.New("wire: malformed shape")
