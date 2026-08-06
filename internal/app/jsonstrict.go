package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// errDuplicateJSONKey is returned by rejectDuplicateJSONKeys when the same
// object key appears twice at any depth. It never carries the offending key
// text: callers report a generic, secret-free message so an attacker- or
// operator-controlled key name is never echoed back.
var errDuplicateJSONKey = errors.New("duplicate JSON object key")

// rejectDuplicateJSONKeys reports whether data, taken as a single top-level
// JSON value, contains a duplicate object key at any depth.
//
// encoding/json silently keeps the last occurrence of a duplicate key, which
// would let a malformed or adversarial config file smuggle a value past
// DisallowUnknownFields (a benign first occurrence of a known key, then the
// same key repeated with the value that actually takes effect) or mislead an
// operator editing the file about which entry is authoritative. This walks
// the raw token stream directly so any strict-decoded config in this package
// can fail closed on that shape without re-implementing a JSON parser.
//
// Shared by modelconfig.go and mcpconfig.go; it carries no schema-specific
// logic and operates only on raw JSON bytes/tokens.
func rejectDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := walkJSONValue(decoder); err != nil {
		if errors.Is(err, errDuplicateJSONKey) {
			return errDuplicateJSONKey
		}
		return errors.New("invalid JSON")
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return errors.New("multiple top-level JSON values")
		}
		return errors.New("invalid JSON")
	}
	return nil
}

// walkJSONValue recursively consumes exactly one JSON value from decoder,
// returning errDuplicateJSONKey the first time an object repeats a key at
// any depth.
func walkJSONValue(decoder *json.Decoder) error {
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
				return errDuplicateJSONKey
			}
			seen[key] = struct{}{}
			if err := walkJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim('}') {
			return errors.New("malformed JSON object")
		}
	case '[':
		for decoder.More() {
			if err := walkJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim(']') {
			return errors.New("malformed JSON array")
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	return nil
}
