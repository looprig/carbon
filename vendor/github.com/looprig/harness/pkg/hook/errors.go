package hook

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxPolicyRevisionBytes = 128
	maxDenialCodeBytes     = 64
	maxDenialReasonBytes   = 1024
)

// ConfigErrorKind identifies an invalid hook declaration or typed payload.
type ConfigErrorKind string

const (
	ConfigUnknownOperation         ConfigErrorKind = "unknown_operation"
	ConfigOperationNotGuardable    ConfigErrorKind = "operation_not_guardable"
	ConfigNilGuard                 ConfigErrorKind = "nil_guard"
	ConfigNilAround                ConfigErrorKind = "nil_around"
	ConfigMissingPolicyRevision    ConfigErrorKind = "missing_policy_revision"
	ConfigUnexpectedPolicyRevision ConfigErrorKind = "unexpected_policy_revision"
	ConfigInvalidPolicyRevision    ConfigErrorKind = "invalid_policy_revision"
	ConfigInvalidDenial            ConfigErrorKind = "invalid_denial"
)

// ConfigError reports invalid hook-set or denial configuration.
type ConfigError struct {
	Kind      ConfigErrorKind
	Operation Operation
	Index     int
	Field     string
}

func (e *ConfigError) Error() string {
	message := "hook: invalid configuration: " + string(e.Kind)
	if e.Field != "" {
		message += " (" + e.Field + ")"
	}
	if e.Operation != 0 {
		message += fmt.Sprintf(" for operation %d", e.Operation)
	}
	return message
}

// CallErrorKind identifies a malformed operation call snapshot.
type CallErrorKind string

const (
	CallUnknownOperation CallErrorKind = "unknown_operation"
	CallInvalidPayload   CallErrorKind = "invalid_payload"
)

// CallError reports a malformed runtime operation snapshot.
type CallError struct {
	Kind      CallErrorKind
	Operation Operation
}

func (e *CallError) Error() string {
	return fmt.Sprintf("hook: invalid call: %s for operation %d", e.Kind, e.Operation)
}

// GuardError reports an internal guard callback failure. Intentional denials
// are returned as validated *Denial values instead.
type GuardError struct {
	Operation Operation
	Index     int
	Cause     error
}

func (e *GuardError) Error() string {
	return fmt.Sprintf("hook: guard failed for operation %d at index %d", e.Operation, e.Index)
}

// Unwrap exposes the trusted in-process cause for classification.
func (e *GuardError) Unwrap() error {
	return e.Cause
}

// CloneErrorKind identifies the sealed union that gained an unsupported variant.
type CloneErrorKind string

const (
	CloneUnknownConversation CloneErrorKind = "unknown_conversation"
	CloneUnknownBlock        CloneErrorKind = "unknown_block"
)

// CloneError reports a sealed content variant the hook snapshot clone does not
// yet support. CloneCall panics with this error rather than silently losing data.
type CloneError struct {
	Kind      CloneErrorKind
	ValueType string
}

func (e *CloneError) Error() string {
	return "hook: clone: " + string(e.Kind) + ": " + e.ValueType
}

// Denial is an intentional, bounded guard refusal.
type Denial struct {
	Code   string
	Reason string
}

func (e *Denial) Error() string {
	return "hook: denied: " + e.Code + ": " + e.Reason
}

// Deny constructs an intentional denial or returns ConfigError when its
// diagnostic fields violate the bounded public contract.
func Deny(code, reason string) error {
	if !validDenialCode(code) || !validDenialReason(reason) {
		return &ConfigError{Kind: ConfigInvalidDenial, Field: "denial"}
	}
	return &Denial{Code: code, Reason: reason}
}

// AsDenial classifies an intentional guard denial. It revalidates exported
// Denial fields so direct construction cannot bypass the bounded contract and
// returns an independent copy owned by the caller.
func AsDenial(err error) (*Denial, bool) {
	var denial *Denial
	if !errors.As(err, &denial) || denial == nil {
		return nil, false
	}
	if !validDenialCode(denial.Code) || !validDenialReason(denial.Reason) {
		return nil, false
	}
	classified := *denial
	return &classified, true
}

func validDenialCode(code string) bool {
	if len(code) == 0 || len(code) > maxDenialCodeBytes {
		return false
	}
	if code[0] < 'a' || code[0] > 'z' {
		return false
	}
	for index := 1; index < len(code); index++ {
		value := code[index]
		if (value < 'a' || value > 'z') &&
			(value < '0' || value > '9') &&
			value != '_' && value != '.' && value != '-' {
			return false
		}
	}
	return true
}

func validPolicyRevision(revision string) bool {
	if len(revision) == 0 ||
		len(revision) > maxPolicyRevisionBytes ||
		!utf8.ValidString(revision) {
		return false
	}
	for _, r := range revision {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func validDenialReason(reason string) bool {
	if strings.TrimSpace(reason) == "" ||
		len(reason) > maxDenialReasonBytes ||
		!utf8.ValidString(reason) {
		return false
	}
	for _, r := range reason {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
