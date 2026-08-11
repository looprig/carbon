// Package client provides the MCP client surface for the looprig/mcp module.
//
// This file defines the typed error taxonomy every package in the module
// classifies failures with. All message text carried by Error is normalized
// and bounded at construction, and wrapped/server-derived text is rendered —
// bounded and normalized — only when no explicit message was provided; an
// explicit Msg fully suppresses it. Callers should supply an explicit Msg for
// auth-adjacent failures rather than relying on redaction, since bounded
// wrapped-error text is otherwise rendered verbatim.
package client

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

// FailureClass classifies a failure so callers can branch on what went wrong
// without parsing error text. The zero value is not a valid class.
type FailureClass uint8

// Failure classes. Values are contiguous starting at 1; the zero value is
// reserved as "no class".
const (
	FailureInvalidConfig FailureClass = iota + 1
	FailureUnsupportedProtocol
	FailureStartupTimeout
	FailureAuthRequired
	FailureAuthDenied
	FailureAuthExpired
	FailureAuthFailed
	FailureTransportClosed
	FailureFraming
	FailureRemoteHTTP
	FailureServerProtocol
	FailureDeadline
	FailureCancelled
	FailureCatalogInvalid
	FailureCatalogStale
	FailureCatalogOverLimit
	FailureNotFound
	FailureToolUnavailable
	FailureToolSchemaChanged
	FailureRemoteToolError
	FailureLimitExceeded
	FailureElicitationDeclined
	FailureElicitationCancelled
	FailureElicitationInvalid
	FailureElicitationTimeout
	FailureSamplingDenied
	FailureSamplingOverBudget
	FailureIndeterminate
	FailureShutdown
	failureClassSentinel // must remain last; used by tests for exhaustiveness
)

// String returns a stable lowercase snake_case identifier for the class.
// Undeclared values return "unknown".
func (c FailureClass) String() string {
	switch c {
	case FailureInvalidConfig:
		return "invalid_config"
	case FailureUnsupportedProtocol:
		return "unsupported_protocol"
	case FailureStartupTimeout:
		return "startup_timeout"
	case FailureAuthRequired:
		return "auth_required"
	case FailureAuthDenied:
		return "auth_denied"
	case FailureAuthExpired:
		return "auth_expired"
	case FailureAuthFailed:
		return "auth_failed"
	case FailureTransportClosed:
		return "transport_closed"
	case FailureFraming:
		return "framing"
	case FailureRemoteHTTP:
		return "remote_http"
	case FailureServerProtocol:
		return "server_protocol"
	case FailureDeadline:
		return "deadline"
	case FailureCancelled:
		return "cancelled"
	case FailureCatalogInvalid:
		return "catalog_invalid"
	case FailureCatalogStale:
		return "catalog_stale"
	case FailureCatalogOverLimit:
		return "catalog_over_limit"
	case FailureNotFound:
		return "not_found"
	case FailureToolUnavailable:
		return "tool_unavailable"
	case FailureToolSchemaChanged:
		return "tool_schema_changed"
	case FailureRemoteToolError:
		return "remote_tool_error"
	case FailureLimitExceeded:
		return "limit_exceeded"
	case FailureElicitationDeclined:
		return "elicitation_declined"
	case FailureElicitationCancelled:
		return "elicitation_cancelled"
	case FailureElicitationInvalid:
		return "elicitation_invalid"
	case FailureElicitationTimeout:
		return "elicitation_timeout"
	case FailureSamplingDenied:
		return "sampling_denied"
	case FailureSamplingOverBudget:
		return "sampling_over_budget"
	case FailureIndeterminate:
		return "indeterminate"
	case FailureShutdown:
		return "shutdown"
	default:
		return "unknown"
	}
}

// MaxMessageBytes bounds every message rendered by Error, both the explicit
// message passed to NewError and any wrapped-error text substituted for it.
// Longer text is truncated at a rune boundary and suffixed with a marker.
const MaxMessageBytes = 1024

// truncationMarker terminates any message that was cut at MaxMessageBytes.
const truncationMarker = "...[truncated]"

// Error is the module's operational error. Msg is already normalized and
// bounded (NewError enforces this); construct values with NewError rather
// than a composite literal so the bound holds.
type Error struct {
	// Class states what kind of failure occurred.
	Class FailureClass
	// Binding names the server binding the failure belongs to, if any.
	Binding Name
	// Op names the operation that failed (e.g. "initialize", "call_tool").
	Op string
	// Msg is a bounded, normalized human-readable description.
	Msg string
	// Err is the wrapped cause, if any.
	Err error
}

// NewError builds an *Error with msg normalized (control characters replaced
// by spaces, invalid UTF-8 repaired) and bounded to MaxMessageBytes.
func NewError(class FailureClass, binding Name, op string, msg string, wrapped error) *Error {
	return &Error{
		Class:   class,
		Binding: binding,
		Op:      op,
		Msg:     boundMessage(msg),
		Err:     wrapped,
	}
}

// Error renders "mcp: <binding>: <op>: <class>: <msg>", omitting empty
// segments. When Msg is empty and a wrapped error is present, its text is
// substituted — bounded to MaxMessageBytes — so output length stays bounded
// regardless of what a server or transport produced.
func (e *Error) Error() string {
	parts := make([]string, 0, 5)
	parts = append(parts, "mcp")
	if e.Binding != "" {
		parts = append(parts, string(e.Binding))
	}
	if e.Op != "" {
		parts = append(parts, e.Op)
	}
	parts = append(parts, e.Class.String())
	switch {
	case e.Msg != "":
		parts = append(parts, e.Msg)
	case e.Err != nil:
		if text := boundMessage(e.Err.Error()); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, ": ")
}

// Unwrap returns the wrapped cause for errors.Is / errors.As traversal.
func (e *Error) Unwrap() error {
	return e.Err
}

// ClassOf walks err's chain and reports the class of the outermost *Error.
// It returns false when the chain contains no *Error.
func ClassOf(err error) (FailureClass, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e.Class, true
	}
	return 0, false
}

// boundMessage normalizes s (control characters become single spaces, invalid
// UTF-8 becomes U+FFFD) and truncates it to at most MaxMessageBytes, cutting
// at a rune boundary and appending truncationMarker when it was cut.
func boundMessage(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	if len(s) <= MaxMessageBytes {
		return s
	}
	cut := MaxMessageBytes - len(truncationMarker)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + truncationMarker
}
