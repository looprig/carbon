package gate

import (
	"fmt"
	"strings"

	"github.com/looprig/harness/pkg/tool"
)

// CurrentAccessVersion is the structural access ABI version understood by Gate.
const CurrentAccessVersion uint16 = 1

// Access values are fixed by the structural access ABI. AccessSource
// implementations deliberately return uint8 so Gate need not import an
// enforcement package's named access type.
const (
	AccessDeny  uint8 = 0
	AccessGated uint8 = 1
	AccessAllow uint8 = 2
)

// AccessSource reports the configured access state for normalized kind/scope
// pairs. Implementations must fail closed for unknown kinds and malformed
// scopes.
type AccessSource interface {
	AccessVersion() uint16
	AccessFor(kind, scope string) (uint8, error)
}

// AccessBinding routes one requirement kind to one access source.
type AccessBinding struct {
	Kind   string
	Source AccessSource
}

// AccessErrorKind classifies a fail-closed access routing failure.
type AccessErrorKind string

const (
	AccessKindInvalid        AccessErrorKind = "kind_invalid"
	AccessSourceMissing      AccessErrorKind = "source_missing"
	AccessSourceDuplicate    AccessErrorKind = "source_duplicate"
	AccessSourceNil          AccessErrorKind = "source_nil"
	AccessVersionUnsupported AccessErrorKind = "version_unsupported"
	AccessValueInvalid       AccessErrorKind = "value_invalid"
	AccessSourceFailed       AccessErrorKind = "source_failed"
)

// AccessError reports a configuration or source failure while routing access.
type AccessError struct {
	Kind        AccessErrorKind
	Requirement string
	Cause       error
}

func (e *AccessError) Error() string {
	msg := fmt.Sprintf("gate: access %s", e.Kind)
	if e.Requirement != "" {
		msg += " for " + e.Requirement
	}
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}
	return msg
}

func (e *AccessError) Unwrap() error { return e.Cause }

// AccessBindings is a validated, exact-kind routing table.
type AccessBindings struct {
	byKind map[string]AccessSource
}

// NewAccessBindings validates that each configured kind has exactly one current
// access source.
func NewAccessBindings(bindings []AccessBinding) (AccessBindings, error) {
	byKind := make(map[string]AccessSource, len(bindings))
	for _, binding := range bindings {
		if binding.Kind == "" || strings.TrimSpace(binding.Kind) != binding.Kind {
			return AccessBindings{}, &AccessError{Kind: AccessKindInvalid, Requirement: binding.Kind}
		}
		if binding.Source == nil {
			return AccessBindings{}, &AccessError{Kind: AccessSourceNil, Requirement: binding.Kind}
		}
		if _, exists := byKind[binding.Kind]; exists {
			return AccessBindings{}, &AccessError{Kind: AccessSourceDuplicate, Requirement: binding.Kind}
		}
		if version := binding.Source.AccessVersion(); version != CurrentAccessVersion {
			return AccessBindings{}, &AccessError{
				Kind:        AccessVersionUnsupported,
				Requirement: binding.Kind,
				Cause:       fmt.Errorf("got %d, want %d", version, CurrentAccessVersion),
			}
		}
		byKind[binding.Kind] = binding.Source
	}
	return AccessBindings{byKind: byKind}, nil
}

// AccessFor routes requirement to its sole configured source.
func (b AccessBindings) AccessFor(requirement tool.Requirement) (uint8, error) {
	source, ok := b.byKind[requirement.Kind]
	if !ok {
		return AccessDeny, &AccessError{Kind: AccessSourceMissing, Requirement: requirement.Kind}
	}
	access, err := source.AccessFor(requirement.Kind, requirement.Scope)
	if err != nil {
		return AccessDeny, &AccessError{Kind: AccessSourceFailed, Requirement: requirement.Kind, Cause: err}
	}
	switch access {
	case AccessDeny, AccessGated, AccessAllow:
		return access, nil
	default:
		return AccessDeny, &AccessError{Kind: AccessValueInvalid, Requirement: requirement.Kind, Cause: fmt.Errorf("got %d", access)}
	}
}
