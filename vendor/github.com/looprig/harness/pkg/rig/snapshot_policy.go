package rig

import (
	"fmt"
	"time"
)

type SnapshotTrigger uint8

const (
	SnapshotTriggerUnset SnapshotTrigger = iota
	SnapshotManual
	SnapshotOnIdle
	SnapshotOnTurnDone
	SnapshotOnStepDone
)

type SnapshotPriority uint8

const (
	SnapshotBestEffort SnapshotPriority = iota
	SnapshotRequired
)

type SnapshotPolicy struct {
	Trigger  SnapshotTrigger
	Priority SnapshotPriority
	Timeout  time.Duration
}

const defaultSnapshotTimeout = 60 * time.Second

func (p SnapshotPolicy) resolve() (SnapshotPolicy, error) {
	if p.Trigger == SnapshotTriggerUnset {
		p.Trigger = SnapshotOnIdle
	}
	if p.Trigger < SnapshotManual || p.Trigger > SnapshotOnStepDone {
		return SnapshotPolicy{}, &SnapshotPolicyError{Kind: SnapshotPolicyInvalidTrigger, Value: int(p.Trigger)}
	}
	if p.Priority > SnapshotRequired {
		return SnapshotPolicy{}, &SnapshotPolicyError{Kind: SnapshotPolicyInvalidPriority, Value: int(p.Priority)}
	}
	if p.Timeout < 0 {
		return SnapshotPolicy{}, &SnapshotPolicyError{Kind: SnapshotPolicyInvalidTimeout}
	}
	if p.Timeout == 0 {
		p.Timeout = defaultSnapshotTimeout
	}
	return p, nil
}

type SnapshotPolicyErrorKind string

const (
	SnapshotPolicyRequired         SnapshotPolicyErrorKind = "required"
	SnapshotPolicyWithoutWorkspace SnapshotPolicyErrorKind = "without_workspace"
	SnapshotPolicyInvalidTrigger   SnapshotPolicyErrorKind = "invalid_trigger"
	SnapshotPolicyInvalidPriority  SnapshotPolicyErrorKind = "invalid_priority"
	SnapshotPolicyInvalidTimeout   SnapshotPolicyErrorKind = "invalid_timeout"
	SnapshotPolicySharedRequired   SnapshotPolicyErrorKind = "shared_required"
)

type SnapshotPolicyError struct {
	Kind  SnapshotPolicyErrorKind
	Value int
}

func (e *SnapshotPolicyError) Error() string {
	msg := "rig: invalid snapshot policy (" + string(e.Kind) + ")"
	if e.Value != 0 {
		msg += fmt.Sprintf(": %d", e.Value)
	}
	return msg
}

func WithSnapshots(policy SnapshotPolicy) Option {
	return func(state *definitionState) error {
		if state.seen[keySnapshots] {
			return &DefinitionError{Kind: DefinitionDuplicateOption, Name: string(keySnapshots)}
		}
		state.seen[keySnapshots] = true
		resolved, err := policy.resolve()
		if err != nil {
			return err
		}
		state.snapshotPolicy = &resolved
		return nil
	}
}
