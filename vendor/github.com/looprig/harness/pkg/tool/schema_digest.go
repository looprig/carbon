package tool

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// SchemaDigest returns the lowercase hex SHA-256 of a tool's argument JSON Schema,
// canonicalized with encoding/json's Compact so that insignificant whitespace cannot
// change a tool's recorded identity. It is the identity projection used for durable
// external-toolset records: the schema of an externally supplied tool may embed
// third-party text (descriptions, defaults, examples) that must never reach the
// journal, so only this digest crosses the boundary.
//
// An empty or nil schema digests as the empty byte string rather than erroring: a
// schema-less tool is legal (a tool taking no arguments) and has a stable identity.
// Invalid JSON is a typed error — an unparseable schema has no canonical form, so
// fabricating a digest over raw bytes would let two different schemas that differ
// only in whitespace claim different identities while a caller believes the digest
// is canonical.
func SchemaDigest(schema json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(schema)
	if len(trimmed) == 0 {
		sum := sha256.Sum256(nil)
		return hex.EncodeToString(sum[:]), nil
	}
	var canonical bytes.Buffer
	if err := json.Compact(&canonical, trimmed); err != nil {
		return "", &InvalidSchemaError{Cause: err}
	}
	sum := sha256.Sum256(canonical.Bytes())
	return hex.EncodeToString(sum[:]), nil
}

// InvalidSchemaError reports that a tool's advertised argument schema is not valid
// JSON, so it has no canonical form to digest. Callers errors.As it to distinguish a
// malformed tool self-description from a transport or build failure.
type InvalidSchemaError struct{ Cause error }

func (e *InvalidSchemaError) Error() string {
	return "tool: invalid argument schema: " + e.Cause.Error()
}

func (e *InvalidSchemaError) Unwrap() error { return e.Cause }
