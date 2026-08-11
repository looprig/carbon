// Package protocol's jsonrpc.go implements the raw JSON-RPC 2.0 envelope
// layer: request/response/notification framing, id handling, and the
// validation limits that guard the wire boundary. It knows nothing about
// ACP's specific methods or domain types (those live in types_gen.go and
// methods_gen.go) — it is pure JSON-RPC 2.0.
//
// All input to ParseEnvelope is untrusted wire bytes (see acp/CLAUDE.md:
// validate at the boundary, fail closed). Size and nesting-depth guards run
// before any value is unmarshaled from the payload: MaxMessageBytes is
// checked against the raw byte length, and MaxNestingDepth is enforced by
// streaming the entire document through json.Decoder.Token, which never
// materializes Go values, before a single field is decoded.
package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// MaxMessageBytes is the largest JSON-RPC message this module will accept.
// Payloads larger than this are rejected before any parsing is attempted.
const MaxMessageBytes = 4 * 1024 * 1024 // 4 MiB

// MaxNestingDepth is the deepest object/array nesting this module will
// accept in a JSON-RPC message. Payloads nested deeper than this are
// rejected while streaming tokens, before any value is unmarshaled.
const MaxNestingDepth = 128

// Kind identifies which of the three JSON-RPC message shapes an Envelope
// holds.
type Kind uint8

const (
	// KindRequest is an id-bearing call that expects a Response.
	KindRequest Kind = iota + 1
	// KindResponse is the reply to a Request, carrying exactly one of a
	// result or an error.
	KindResponse
	// KindNotification is a method call with no id; it expects no Response.
	KindNotification
)

func (k Kind) String() string {
	switch k {
	case KindRequest:
		return "request"
	case KindResponse:
		return "response"
	case KindNotification:
		return "notification"
	default:
		return "unknown"
	}
}

// idKind discriminates the two JSON types an ID may hold. The JSON-RPC
// null id is deliberately not a variant here: on input it is canonicalized
// to "no id" (see ParseEnvelope), and this type is never asked to represent
// it.
type idKind uint8

const (
	idKindUnset idKind = iota
	idKindString
	idKindNumber
)

// ID is a JSON-RPC request id: a string-or-number sum type, never null or
// any other JSON type. Use NewStringID or NewNumberID to construct one, and
// String or Number to inspect it.
type ID struct {
	kind idKind
	str  string
	num  int64
}

// NewStringID constructs a string-valued ID.
func NewStringID(s string) ID { return ID{kind: idKindString, str: s} }

// NewNumberID constructs a number-valued ID.
func NewNumberID(n int64) ID { return ID{kind: idKindNumber, num: n} }

// String reports the string value of id and whether id holds a string.
func (id ID) String() (string, bool) { return id.str, id.kind == idKindString }

// Number reports the numeric value of id and whether id holds a number.
func (id ID) Number() (int64, bool) { return id.num, id.kind == idKindNumber }

// IsZero reports whether id is the unset zero value (never a valid wire id).
func (id ID) IsZero() bool { return id.kind == idKindUnset }

// MarshalJSON encodes id as a bare JSON string or number, matching how a
// JSON-RPC id is carried on the wire.
func (id ID) MarshalJSON() ([]byte, error) {
	switch id.kind {
	case idKindString:
		return json.Marshal(id.str)
	case idKindNumber:
		return json.Marshal(id.num)
	default:
		return nil, fmt.Errorf("protocol: cannot marshal a zero-value ID")
	}
}

// Request is a JSON-RPC 2.0 request object.
type Request struct {
	ID     ID
	Method string
	Params json.RawMessage
}

// MarshalJSON encodes r as a "jsonrpc":"2.0" request object.
func (r *Request) MarshalJSON() ([]byte, error) {
	wire := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      ID              `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params,omitempty"`
	}{JSONRPC: "2.0", ID: r.ID, Method: r.Method, Params: r.Params}
	return json.Marshal(wire)
}

// Response is a JSON-RPC 2.0 response object: exactly one of Result or Error
// is set.
type Response struct {
	ID     ID
	Result json.RawMessage
	Error  *Error

	// ReceiveSequence is stamped only on inbound responses by Conn's single
	// read loop. It is intentionally omitted from JSON-RPC marshaling: this is
	// a local observation fact, never wire data.
	ReceiveSequence uint64
}

// MarshalJSON encodes r as a "jsonrpc":"2.0" response object.
func (r *Response) MarshalJSON() ([]byte, error) {
	wire := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      ID              `json:"id"`
		Result  json.RawMessage `json:"result,omitempty"`
		Error   *Error          `json:"error,omitempty"`
	}{JSONRPC: "2.0", ID: r.ID, Result: r.Result, Error: r.Error}
	return json.Marshal(wire)
}

// Notification is a JSON-RPC 2.0 notification: a method call with no id.
// Its wire struct has no id field at all, so a Notification can never
// encode one — the "empty id" form is only ever something ParseEnvelope
// accepts and canonicalizes away on input, never something this module
// emits.
type Notification struct {
	Method string
	Params json.RawMessage

	// ReceiveSequence is stamped only on inbound notifications by Conn's
	// single read loop and is never serialized onto the wire.
	ReceiveSequence uint64
}

// MarshalJSON encodes n as a "jsonrpc":"2.0" notification object.
func (n *Notification) MarshalJSON() ([]byte, error) {
	wire := struct {
		JSONRPC string          `json:"jsonrpc"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params,omitempty"`
	}{JSONRPC: "2.0", Method: n.Method, Params: n.Params}
	return json.Marshal(wire)
}

// Envelope holds exactly one of Request, Response, or Notification,
// discriminated by Kind. It is the result of a successful ParseEnvelope.
type Envelope struct {
	Request      *Request
	Response     *Response
	Notification *Notification
}

// Kind reports which variant is set.
func (e *Envelope) Kind() Kind {
	switch {
	case e.Request != nil:
		return KindRequest
	case e.Response != nil:
		return KindResponse
	case e.Notification != nil:
		return KindNotification
	default:
		return 0
	}
}

// IssueKind names one specific way a decoded JSON-RPC envelope failed
// validation. Kinds are named, not free-form text, so diagnostics stay
// compact and never need to embed the payload that produced them.
type IssueKind string

const (
	IssueOversizedPayload   IssueKind = "oversized_payload"
	IssueExcessiveNesting   IssueKind = "excessive_nesting"
	IssueMalformedJSON      IssueKind = "malformed_json"
	IssueNotAnObject        IssueKind = "not_an_object"
	IssueTrailingData       IssueKind = "trailing_data"
	IssueDuplicateField     IssueKind = "duplicate_field"
	IssueUnknownField       IssueKind = "unknown_field"
	IssueWrongVersion       IssueKind = "wrong_jsonrpc_version"
	IssueInvalidIDType      IssueKind = "invalid_id_type"
	IssueMissingID          IssueKind = "missing_id"
	IssueInvalidMethodType  IssueKind = "invalid_method_type"
	IssueMalformedErrorObj  IssueKind = "malformed_error_object"
	IssueBothResultAndError IssueKind = "result_and_error_both_set"
	IssueAmbiguousShape     IssueKind = "ambiguous_message_shape"
)

// Issue is one validation problem found while decoding an envelope.
type Issue struct {
	Kind IssueKind
}

// ValidationError reports every structural problem found in a rejected
// envelope. Its Error method deliberately emits only the issue count and
// kinds — never any fragment of the payload that produced them, since wire
// input is untrusted and diagnostics must not become an exfiltration or log
// injection channel.
type ValidationError struct {
	Issues []Issue
}

func (e *ValidationError) Error() string {
	kinds := make([]string, len(e.Issues))
	for i, iss := range e.Issues {
		kinds[i] = string(iss.Kind)
	}
	return fmt.Sprintf("protocol: invalid json-rpc envelope: %d issue(s): %s", len(e.Issues), strings.Join(kinds, ", "))
}

// AsFault converts a ValidationError into a Fault suitable for sending back
// to a peer as a JSON-RPC error response. Payloads that could not be parsed
// as JSON at all map to ParseError; payloads that parsed as JSON but did not
// form a valid JSON-RPC object map to InvalidRequest.
func (e *ValidationError) AsFault() *Fault {
	for _, iss := range e.Issues {
		switch iss.Kind {
		case IssueMalformedJSON, IssueNotAnObject:
			return ParseError(e.Error(), nil)
		}
	}
	return InvalidRequest(e.Error(), nil)
}

var allowedTopLevelFields = map[string]bool{
	"jsonrpc": true,
	"id":      true,
	"method":  true,
	"params":  true,
	"result":  true,
	"error":   true,
}

// ParseEnvelope decodes untrusted wire bytes into an Envelope, or returns a
// *ValidationError describing every structural problem found. Size and
// nesting-depth guards run first, purely by streaming tokens, before any
// field value is unmarshaled.
func ParseEnvelope(data []byte) (*Envelope, error) {
	if len(data) > MaxMessageBytes {
		return nil, &ValidationError{Issues: []Issue{{Kind: IssueOversizedPayload}}}
	}
	if err := checkNestingDepth(data); err != nil {
		return nil, err
	}
	fields, issues, err := parseTopLevelFields(data)
	if err != nil {
		return nil, err
	}
	return classify(fields, issues)
}

// checkNestingDepth streams data through json.Decoder.Token, counting
// object/array nesting depth without ever constructing a Go value. It fails
// closed on the first sign of excessive depth or malformed JSON, before any
// unmarshal is attempted.
func checkNestingDepth(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	depth := 0
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return &ValidationError{Issues: []Issue{{Kind: IssueMalformedJSON}}}
		}
		if delim, ok := tok.(json.Delim); ok {
			switch delim {
			case '{', '[':
				depth++
				if depth > MaxNestingDepth {
					return &ValidationError{Issues: []Issue{{Kind: IssueExcessiveNesting}}}
				}
			case '}', ']':
				depth--
			}
		}
	}
}

// parseTopLevelFields performs the structural (post-guard) decode of the
// top-level JSON object: it extracts the known fields, and reports duplicate
// or unknown top-level fields as accumulated Issues rather than failing
// immediately, so they can be combined with the shape-validation issues
// found by classify into one compact diagnostic. A hard syntax problem
// (not an object, malformed JSON, trailing data) returns immediately since
// the extracted fields cannot be trusted at all in that case.
func parseTopLevelFields(data []byte) (map[string]json.RawMessage, []Issue, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()

	tok, err := dec.Token()
	if err != nil {
		return nil, nil, &ValidationError{Issues: []Issue{{Kind: IssueMalformedJSON}}}
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != '{' {
		return nil, nil, &ValidationError{Issues: []Issue{{Kind: IssueNotAnObject}}}
	}

	fields := make(map[string]json.RawMessage)
	var issues []Issue
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, nil, &ValidationError{Issues: []Issue{{Kind: IssueMalformedJSON}}}
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, nil, &ValidationError{Issues: []Issue{{Kind: IssueMalformedJSON}}}
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, nil, &ValidationError{Issues: []Issue{{Kind: IssueMalformedJSON}}}
		}
		if !allowedTopLevelFields[key] {
			issues = append(issues, Issue{Kind: IssueUnknownField})
			continue
		}
		if _, dup := fields[key]; dup {
			issues = append(issues, Issue{Kind: IssueDuplicateField})
			continue
		}
		fields[key] = raw
	}
	if _, err := dec.Token(); err != nil { // closing '}'
		return nil, nil, &ValidationError{Issues: []Issue{{Kind: IssueMalformedJSON}}}
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, nil, &ValidationError{Issues: []Issue{{Kind: IssueTrailingData}}}
	}

	return fields, issues, nil
}

// classify determines which of Request, Response, or Notification the
// top-level fields describe, validating jsonrpc version, id type, and
// result/error exclusivity along the way. All issues found (structural ones
// passed in from parseTopLevelFields, plus shape-validation ones found here)
// are merged into a single ValidationError if any exist; only a fully clean
// envelope is built and returned.
func classify(fields map[string]json.RawMessage, issues []Issue) (*Envelope, error) {
	if raw, ok := fields["jsonrpc"]; !ok {
		issues = append(issues, Issue{Kind: IssueWrongVersion})
	} else {
		var version string
		if err := json.Unmarshal(raw, &version); err != nil || version != "2.0" {
			issues = append(issues, Issue{Kind: IssueWrongVersion})
		}
	}

	hasMethod := false
	var method string
	if raw, ok := fields["method"]; ok {
		// encoding/json treats unmarshaling JSON null into a string as a
		// silent no-op (nil error, zero value) rather than a type error, so
		// it must be checked explicitly: a JSON-RPC method name must be an
		// actual string, never null.
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			issues = append(issues, Issue{Kind: IssueInvalidMethodType})
		} else if err := json.Unmarshal(raw, &method); err != nil || method == "" {
			issues = append(issues, Issue{Kind: IssueInvalidMethodType})
		} else {
			hasMethod = true
		}
	}

	var params json.RawMessage
	if raw, ok := fields["params"]; ok {
		params = raw
	}

	resultRaw, hasResult := fields["result"]
	errorRaw, hasError := fields["error"]
	if hasResult && hasError {
		issues = append(issues, Issue{Kind: IssueBothResultAndError})
	}

	idKeyPresent := false
	idIsNull := false
	idProblem := false
	var id ID
	if raw, ok := fields["id"]; ok {
		idKeyPresent = true
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			// The "empty id" form: canonicalized to "no id" on input, and
			// never something this module emits (see Notification.MarshalJSON).
			idIsNull = true
		} else {
			parsed, err := parseID(raw)
			if err != nil {
				idProblem = true
				issues = append(issues, Issue{Kind: IssueInvalidIDType})
			} else {
				id = parsed
			}
		}
	}
	haveUsableID := idKeyPresent && !idIsNull && !idProblem
	idEffectivelyAbsent := !idKeyPresent || idIsNull

	var wireErr *Error
	if hasError {
		we, err := parseWireError(errorRaw)
		if err != nil {
			issues = append(issues, Issue{Kind: IssueMalformedErrorObj})
		} else {
			wireErr = we
		}
	}

	var envelope *Envelope
	switch {
	case hasMethod && !hasResult && !hasError && idEffectivelyAbsent && !idProblem:
		envelope = &Envelope{Notification: &Notification{Method: method, Params: params}}
	case hasMethod && !hasResult && !hasError && haveUsableID:
		envelope = &Envelope{Request: &Request{ID: id, Method: method, Params: params}}
	case hasMethod && !hasResult && !hasError && idProblem:
		// Invalid id type already recorded above; no shape to build.
	case !hasMethod && (hasResult || hasError):
		if idEffectivelyAbsent {
			issues = append(issues, Issue{Kind: IssueMissingID})
		} else if haveUsableID {
			envelope = &Envelope{Response: &Response{ID: id, Result: resultRaw, Error: wireErr}}
		}
		// else: idProblem already recorded above.
	default:
		issues = append(issues, Issue{Kind: IssueAmbiguousShape})
	}

	if len(issues) > 0 {
		return nil, &ValidationError{Issues: issues}
	}
	return envelope, nil
}

// parseID decodes a non-null JSON-RPC id value as a string or an integer.
// Any other JSON type — bool, object, array, or a number with a fractional
// part — is rejected.
func parseID(raw json.RawMessage) (ID, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return NewStringID(s), nil
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		i, err := n.Int64()
		if err != nil {
			return ID{}, fmt.Errorf("protocol: id %q is not a whole number", n.String())
		}
		return NewNumberID(i), nil
	}
	return ID{}, fmt.Errorf("protocol: id is not a string or integer")
}

// parseWireError decodes a JSON-RPC error object's raw bytes into an *Error.
// The whole document was already depth/size guarded by ParseEnvelope before
// any unmarshal was attempted, so this per-field decode of an already-bounded
// sub-document is safe.
func parseWireError(raw json.RawMessage) (*Error, error) {
	var tmp struct {
		// Code is int32, matching ErrorCode's own width (the pinned schema
		// declares the error code as format "int32"): unmarshaling directly
		// into a fixed-width field rejects out-of-range codes instead of
		// silently truncating them.
		Code    *int32          `json:"code"`
		Message *string         `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &tmp); err != nil {
		return nil, err
	}
	if tmp.Code == nil || tmp.Message == nil {
		return nil, fmt.Errorf("protocol: error object missing required code or message field")
	}
	return &Error{Code: ErrorCode(*tmp.Code), Message: *tmp.Message, Data: tmp.Data}, nil
}
