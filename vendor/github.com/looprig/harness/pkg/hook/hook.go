// Package hook defines the in-process interception contracts for bounded
// Harness runtime operations.
package hook

import (
	"context"
	"strings"
)

// Operation identifies one bounded runtime operation.
type Operation uint8

const (
	OperationTurn Operation = iota + 1
	OperationStep
	OperationInference
	OperationCompaction
	OperationToolCall
	OperationGateWait
	OperationToolExecution
	OperationJournalAppend
)

// Valid reports whether operation is recognized by this package.
func (o Operation) Valid() bool {
	return o >= OperationTurn && o <= OperationJournalAppend
}

// Guardable reports whether a guard may synchronously deny operation.
func (o Operation) Guardable() bool {
	switch o {
	case OperationTurn, OperationInference, OperationCompaction, OperationToolCall:
		return true
	default:
		return false
	}
}

// Outcome identifies how an operation ended.
type Outcome uint8

const (
	OutcomeCompleted Outcome = iota + 1
	OutcomeDenied
	OutcomeFailed
	OutcomeCanceled
)

// Valid reports whether outcome is recognized by this package.
func (o Outcome) Valid() bool {
	return o >= OutcomeCompleted && o <= OutcomeCanceled
}

// StepIndex is the zero-based index of a step within a turn.
type StepIndex uint64

// RecordFamily identifies the bounded journal record family being appended.
type RecordFamily string

const (
	RecordEvent        RecordFamily = "event"
	RecordCommand      RecordFamily = "command"
	RecordGatePrepared RecordFamily = "gate_prepared"
	RecordFence        RecordFamily = "fence"
)

// GuardFunc checks whether a guardable operation may proceed.
type GuardFunc func(context.Context, Call) error

// BeginFunc begins observation of an operation and returns its derived context
// and optional terminal callback. A returned context may add values or tighter
// cancellation, but Runner preserves cancellation and deadlines from the input
// context even if the returned context is detached.
type BeginFunc func(context.Context, Call) (context.Context, FinishFunc)

// FinishFunc observes the terminal result of an operation.
type FinishFunc func(Result)

// Guard registers one synchronous check for a guardable operation.
type Guard struct {
	Operation Operation
	Check     GuardFunc
}

// Around registers one paired begin/finish observer for an operation.
type Around struct {
	Operation Operation
	Begin     BeginFunc
}

// Set is an ordered collection of guards and around observers. A Set and its
// backing slices are immutable after installation. Callbacks may run
// concurrently for different operations and must be concurrency-safe; Call and
// Result arguments are read-only snapshots.
type Set struct {
	// PolicyRevision identifies behavior implemented by Guards. It is required
	// when Guards is non-empty and forbidden otherwise; Around observers are
	// operational configuration and do not contribute to policy identity.
	PolicyRevision string
	// Guards run in registration order at guardable operation boundaries.
	Guards []Guard
	// Around observers begin in registration order and finish in reverse order.
	Around []Around
}

// ValidateSet validates one declarative hook set.
func ValidateSet(set Set) error {
	if len(set.Guards) == 0 {
		if set.PolicyRevision != "" {
			return &ConfigError{Kind: ConfigUnexpectedPolicyRevision, Field: "policy_revision"}
		}
	} else if strings.TrimSpace(set.PolicyRevision) == "" {
		return &ConfigError{Kind: ConfigMissingPolicyRevision, Field: "policy_revision"}
	} else if !validPolicyRevision(set.PolicyRevision) {
		return &ConfigError{Kind: ConfigInvalidPolicyRevision, Field: "policy_revision"}
	}

	for index, guard := range set.Guards {
		if !guard.Operation.Valid() {
			return &ConfigError{
				Kind: ConfigUnknownOperation, Operation: guard.Operation, Index: index, Field: "guards",
			}
		}
		if !guard.Operation.Guardable() {
			return &ConfigError{
				Kind: ConfigOperationNotGuardable, Operation: guard.Operation, Index: index, Field: "guards",
			}
		}
		if guard.Check == nil {
			return &ConfigError{
				Kind: ConfigNilGuard, Operation: guard.Operation, Index: index, Field: "guards",
			}
		}
	}

	for index, around := range set.Around {
		if !around.Operation.Valid() {
			return &ConfigError{
				Kind: ConfigUnknownOperation, Operation: around.Operation, Index: index, Field: "around",
			}
		}
		if around.Begin == nil {
			return &ConfigError{
				Kind: ConfigNilAround, Operation: around.Operation, Index: index, Field: "around",
			}
		}
	}
	return nil
}
