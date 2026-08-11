// Package sessionstore frames a session's ledger records for durable storage. The
// envelope defined here is the versioned wire frame that wraps one record's codec
// bytes with the small amount of metadata a writer needs to route and de-duplicate
// it (its kind and idempotency id) without re-decoding the payload.
package sessionstore

import (
	"encoding/json"
	"strconv"
)

// envelopeVersion is the current frame version. A frame carrying any other version
// is rejected on decode (and encode) — the codec fails closed rather than guessing
// at a layout it does not know.
const envelopeVersion = 1

// kind names the record a frame carries. It is a closed set of four named string
// constants; a frame whose kind is outside this set is rejected (fail closed).
type kind string

const (
	kindEvent        kind = "event"
	kindCommand      kind = "command"
	kindFence        kind = "fence"
	kindBlobPtr      kind = "blobptr"
	kindGatePrepared kind = "gate_prepared"
)

// envelope is the versioned wire frame for one ledger record. Body is the record's
// codec bytes (event/command/fence payload, or the blobPointer JSON). encoding/json
// base64s []byte, so the frame is codec-agnostic — it carries opaque payload bytes
// and never inspects them.
type envelope struct {
	V    int    `json:"v"`    // frame version; currently envelopeVersion
	Kind string `json:"kind"` // one of the four kind constants
	ID   string `json:"id"`   // domain idempotency id (ex-Nats-Msg-Id)
	Body []byte `json:"body"` // opaque record payload; empty is valid
}

// blobPointer is the Body payload of a blobptr envelope: it references a blob held
// out-of-line in the blob store by key, with its size and content hash for
// integrity verification on read.
type blobPointer struct {
	Key    string `json:"key"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// EnvelopeError reports a failure to encode or decode a frame (or a blobPointer
// body): a malformed JSON payload, an unknown kind, or an unsupported version.
// Reason carries the human-readable context; Cause, when non-nil, is the underlying
// encoding/json error reachable via errors.As / errors.Unwrap. A semantic rejection
// (unknown kind or unsupported version) has no underlying cause and leaves Cause nil.
type EnvelopeError struct {
	Reason string
	Cause  error
}

func (e *EnvelopeError) Error() string {
	if e.Cause != nil {
		return "sessionstore: envelope " + e.Reason + ": " + e.Cause.Error()
	}
	return "sessionstore: envelope " + e.Reason
}

func (e *EnvelopeError) Unwrap() error { return e.Cause }

// validate enforces the frame invariants shared by encode and decode: the version
// must be the current one and the kind must be a known constant. It fails closed —
// an unrecognized version or kind is rejected rather than passed through.
func (env envelope) validate() error {
	if env.V != envelopeVersion {
		return &EnvelopeError{Reason: "unsupported version " + strconv.Itoa(env.V)}
	}
	switch kind(env.Kind) {
	case kindEvent, kindCommand, kindFence, kindBlobPtr, kindGatePrepared:
		return nil
	default:
		return &EnvelopeError{Reason: "unknown kind " + strconv.Quote(env.Kind)}
	}
}

// encodeEnvelope marshals env to its JSON wire frame, failing closed on an unknown
// kind or unsupported version so a malformed frame can never reach durable storage.
func encodeEnvelope(env envelope) ([]byte, error) {
	if err := env.validate(); err != nil {
		return nil, err
	}
	b, err := json.Marshal(env)
	if err != nil {
		return nil, &EnvelopeError{Reason: "encode", Cause: err}
	}
	return b, nil
}

// decodeEnvelope parses a JSON wire frame back into an envelope. It fails closed:
// invalid JSON is rejected with the json error as Cause; a well-formed frame with an
// unknown kind or unsupported version is rejected with no cause. Body bytes are
// preserved exactly (encoding/json base64-decodes them), including empty.
func decodeEnvelope(b []byte) (envelope, error) {
	var env envelope
	if err := json.Unmarshal(b, &env); err != nil {
		return envelope{}, &EnvelopeError{Reason: "decode", Cause: err}
	}
	if err := env.validate(); err != nil {
		return envelope{}, err
	}
	return env, nil
}

// encodeBlobPointer marshals p to the JSON that becomes a blobptr envelope's Body.
func encodeBlobPointer(p blobPointer) ([]byte, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return nil, &EnvelopeError{Reason: "encode blobptr", Cause: err}
	}
	return b, nil
}

// decodeBlobPointer parses a blobptr envelope's Body back into a blobPointer,
// failing closed with the json error as Cause on malformed input.
func decodeBlobPointer(b []byte) (blobPointer, error) {
	var p blobPointer
	if err := json.Unmarshal(b, &p); err != nil {
		return blobPointer{}, &EnvelopeError{Reason: "decode blobptr", Cause: err}
	}
	return p, nil
}
