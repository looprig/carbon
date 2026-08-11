package session

import (
	"context"

	"github.com/looprig/harness/pkg/event"
)

// RestoreDecision is an application's answer to a drift assessment. Source,
// Actor, and Message are recorded durably on the resulting ConfigurationAdopted.
//
// Contract for an ACCEPTING decision (Accept == true): the valid decider Sources
// are user | policy | operator. `migration` is RESERVED for Harness itself (a
// Phase-2 migration) and must never be stamped by a decider. An empty OR migration
// Source on accept is normalized to policy by the restore constructor, so a decider
// can neither omit a source nor forge a migration adoption. Actor and Message are
// BOUNDED audit fields: the restore constructor truncates them (to MaxConfigActorLen
// / MaxConfigMessageLen bytes) before writing the durable adoption, so an over-long
// value is silently shortened rather than bricking the restore — never rely on their
// full length surviving.
type RestoreDecision struct {
	Accept  bool
	Source  event.DecisionSource // user | policy | operator (migration reserved for Harness)
	Actor   string
	Message string
}

// RestoreDecider answers a restore drift assessment. It runs while the restore
// lease is held; ctx carries the restore deadline, and a timeout is a rejection.
//
// An ACCEPTING RestoreDecision must honor the RestoreDecision contract above: a
// valid Source (empty defaults to policy) and bounded Actor/Message (truncated,
// not rejected). A decider therefore cannot brick a restore with a malformed
// decision — the constructor normalizes an accepting decision before it becomes a
// durable ConfigurationAdopted.
type RestoreDecider interface {
	DecideRestore(ctx context.Context, assessment event.DriftAssessment) (RestoreDecision, error)
}

// DefaultPolicyDecider is the fail-secure default: accept when every change is
// Info, reject when any change is Warn.
type DefaultPolicyDecider struct{}

func (DefaultPolicyDecider) DecideRestore(_ context.Context, a event.DriftAssessment) (RestoreDecision, error) {
	return RestoreDecision{Accept: !a.AnyWarn(), Source: event.DecisionSourcePolicy}, nil
}

// AcceptAllDecider accepts every assessment. It backs the deprecated
// WithAllowConfigMismatch shim (wired in a later task).
type AcceptAllDecider struct{}

func (AcceptAllDecider) DecideRestore(_ context.Context, _ event.DriftAssessment) (RestoreDecision, error) {
	return RestoreDecision{Accept: true, Source: event.DecisionSourcePolicy}, nil
}
