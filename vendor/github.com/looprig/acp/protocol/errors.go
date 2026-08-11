package protocol

import (
	"encoding/json"
	"fmt"
)

// Error is the JSON-RPC 2.0 error object shape (the "Error" $def excluded
// from generation in internal/gen/model.go — it collides in spirit with Go's
// builtin error and is owned here instead). It is the wire representation
// carried in Response.Error: exactly the bytes that cross the process
// boundary, nothing more.
//
// Error deliberately holds no cause chain: Data is the only channel for
// additional context, and it is populated exclusively by Fault.WithData, so
// whatever a Fault's internal cause was, it never reaches the wire unless a
// caller explicitly whitelists it into Data.
type Error struct {
	// Code is the JSON-RPC error code. See the ErrorCode constants in
	// types_gen.go (generated from the pinned schema's ErrorCode $def).
	Code ErrorCode `json:"code"`
	// Message is a short, single-sentence description of the error.
	Message string `json:"message"`
	// Data is optional, caller-whitelisted additional context. It is never
	// populated automatically from an internal cause.
	Data json.RawMessage `json:"data,omitempty"`
}

// Error implements the built-in error interface so an *Error can be used
// anywhere a Go error is expected (for example, returned by a transport that
// received an error Response).
func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("protocol: jsonrpc error %d: %s", e.Code, e.Message)
}

// Fault is the internal, typed representation of a JSON-RPC failure. Unlike
// Error, it may wrap an internal cause (a filesystem error, a decode error,
// etc.) for local diagnosis via errors.Is/errors.As/errors.Unwrap. That cause
// is never sent over the wire: ToWireError only ever copies Code, Message,
// and whatever the caller explicitly attached via WithData.
type Fault struct {
	Code    ErrorCode
	Message string
	Data    json.RawMessage
	cause   error
}

// Error implements the built-in error interface. Unlike the wire Error type,
// this may include cause detail — it is for local logs/diagnostics, never
// serialized to a peer.
func (f *Fault) Error() string {
	if f == nil {
		return "<nil>"
	}
	if f.cause != nil {
		return fmt.Sprintf("protocol: %s (code %d): %v", f.Message, f.Code, f.cause)
	}
	return fmt.Sprintf("protocol: %s (code %d)", f.Message, f.Code)
}

// Unwrap exposes the typed internal cause, if any, to errors.Is/errors.As.
// The cause is local-only: it is never included in ToWireError's output.
func (f *Fault) Unwrap() error {
	if f == nil {
		return nil
	}
	return f.cause
}

// WithData attaches caller-whitelisted, JSON-serializable context that IS
// safe to send to a peer. It marshals v immediately (rather than storing an
// `any`) so Fault only ever carries wire-safe bytes beyond this boundary
// call. A marshal failure is dropped silently in favor of sending no data at
// all, rather than failing the whole fault construction over optional
// context.
func (f *Fault) WithData(v any) *Fault {
	if f == nil {
		return nil
	}
	clone := *f
	if raw, err := json.Marshal(v); err == nil {
		clone.Data = raw
	}
	return &clone
}

func newFault(code ErrorCode, message string, cause error) *Fault {
	return &Fault{Code: code, Message: message, cause: cause}
}

// ParseError constructs the standard JSON-RPC "invalid JSON was received"
// fault (code -32700).
func ParseError(message string, cause error) *Fault {
	return newFault(ErrorCodeParseError, message, cause)
}

// InvalidRequest constructs the standard JSON-RPC "the JSON sent is not a
// valid Request object" fault (code -32600).
func InvalidRequest(message string, cause error) *Fault {
	return newFault(ErrorCodeInvalidRequest, message, cause)
}

// MethodNotFound constructs the standard JSON-RPC "method does not exist or
// is not available" fault (code -32601).
func MethodNotFound(message string, cause error) *Fault {
	return newFault(ErrorCodeMethodNotFound, message, cause)
}

// InvalidParams constructs the standard JSON-RPC "invalid method
// parameter(s)" fault (code -32602).
func InvalidParams(message string, cause error) *Fault {
	return newFault(ErrorCodeInvalidParams, message, cause)
}

// InternalError constructs the standard JSON-RPC "internal JSON-RPC error"
// fault (code -32603).
func InternalError(message string, cause error) *Fault {
	return newFault(ErrorCodeInternalError, message, cause)
}

// AuthRequired constructs the ACP-specific "authentication is required
// before this operation can be performed" fault (code -32000).
func AuthRequired(message string, cause error) *Fault {
	return newFault(ErrorCodeAuthenticationRequired, message, cause)
}

// ResourceNotFound constructs the ACP-specific "a given resource, such as a
// file, was not found" fault (code -32002).
func ResourceNotFound(message string, cause error) *Fault {
	return newFault(ErrorCodeResourceNotFound, message, cause)
}

// ToWireError converts a local Fault into the wire Error object sent to a
// peer. Only Code, Message, and whitelisted Data cross this boundary; any
// internal cause on f is intentionally dropped.
func ToWireError(f *Fault) *Error {
	if f == nil {
		return nil
	}
	return &Error{Code: f.Code, Message: f.Message, Data: f.Data}
}

// FromWireError reconstructs a local Fault from a wire Error object received
// from a peer. The result never has a local cause: the wire never carried
// one, so none is fabricated.
func FromWireError(w *Error) *Fault {
	if w == nil {
		return nil
	}
	return &Fault{Code: w.Code, Message: w.Message, Data: w.Data}
}
