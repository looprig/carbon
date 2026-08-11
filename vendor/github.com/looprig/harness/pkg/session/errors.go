package session

import (
	"strconv"
	"strings"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/identity"
)

type SessionErrorKind string

const (
	SessionIDGenerationFailed            SessionErrorKind = "id_generation_failed"
	SessionLoopIDGenerationFailed        SessionErrorKind = "loop_id_generation_failed"
	SessionLoopExited                    SessionErrorKind = "loop_exited"
	SessionLoopNotFound                  SessionErrorKind = "loop_not_found"
	SessionEventChannelClosed            SessionErrorKind = "event_channel_closed"
	SessionContextDone                   SessionErrorKind = "context_done"
	SessionClosing                       SessionErrorKind = "session_closing"
	SessionFaulted                       SessionErrorKind = "session_faulted"
	SessionLoopDepthExceeded             SessionErrorKind = "loop_depth_exceeded"
	SessionLoopQuotaExceeded             SessionErrorKind = "loop_quota_exceeded"
	SessionForeignBuilderMissing         SessionErrorKind = "foreign_builder_missing"
	SessionCompactionUnsupported         SessionErrorKind = "compaction_unsupported"
	SessionDelegateIntentAppendFailed    SessionErrorKind = "delegate_intent_append_failed"
	SessionDelegateAdmissionCommitFailed SessionErrorKind = "delegate_admission_commit_failed"
)

type SessionError struct {
	Kind  SessionErrorKind
	Cause error
}

func (e *SessionError) Error() string {
	messages := map[SessionErrorKind]string{
		SessionIDGenerationFailed: "session: id generation failed", SessionLoopIDGenerationFailed: "session: loop id generation failed",
		SessionLoopExited: "session: loop exited", SessionLoopNotFound: "session: loop not found",
		SessionEventChannelClosed: "session: event channel closed without terminal event", SessionContextDone: "session: context done",
		SessionClosing: "session: closing", SessionFaulted: "session: faulted (durable persistence failure)",
		SessionLoopDepthExceeded: "session: loop spawn depth limit exceeded", SessionLoopQuotaExceeded: "session: loop spawn quota exceeded",
		SessionForeignBuilderMissing:         "session: foreign engine selected but no foreign builder wired",
		SessionCompactionUnsupported:         "session: loop does not support native conversation compaction",
		SessionDelegateIntentAppendFailed:    "session: required delegate intent append failed",
		SessionDelegateAdmissionCommitFailed: "session: delegate admission commit failed after durable intent",
	}
	msg := messages[e.Kind]
	if msg == "" {
		msg = "session: error"
	}
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}
	return msg
}
func (e *SessionError) Unwrap() error { return e.Cause }

type TurnRejectedError struct{ Reason event.RejectReason }

func (e *TurnRejectedError) Error() string {
	switch e.Reason {
	case event.RejectQueueFull:
		return "session: turn rejected: queue full"
	case event.RejectShuttingDown:
		return "session: turn rejected: loop shutting down"
	case event.RejectInternal:
		return "session: turn rejected: transient internal failure"
	default:
		return "session: turn rejected"
	}
}

// ConfigMismatchError is the legacy config-drift restore error. For a
// manifest-carrying session it is superseded by RestoreRejectedError, which
// carries a typed drift assessment; the legacy fingerprint path (a session with
// no ConfigManifest configured) still returns it during the deprecation window.
// Its formal deprecation and removal path is open question 9 in
// docs/plans/2026-07-16-session-versioning-migration-design.md; it is not marked
// Deprecated here because internal code still depends on it.
type ConfigMismatchError struct{ Persisted, Live event.ConfigFingerprint }

func (e *ConfigMismatchError) Error() string {
	changed := make([]string, 0, 11)
	if e.Persisted.TopologyRev != e.Live.TopologyRev {
		changed = append(changed, "topology")
	}
	if e.Persisted.AgentKind != e.Live.AgentKind {
		changed = append(changed, configValueChange("agent kind", e.Persisted.AgentKind, e.Live.AgentKind))
	}
	if e.Persisted.ModelID != e.Live.ModelID {
		changed = append(changed, configValueChange("model", e.Persisted.ModelID, e.Live.ModelID))
	}
	if e.Persisted.SystemPromptRev != e.Live.SystemPromptRev {
		changed = append(changed, "system prompt")
	}
	if e.Persisted.ToolPolicyRev != e.Live.ToolPolicyRev {
		changed = append(changed, "tool policy")
	}
	if e.Persisted.RuntimeSkills != e.Live.RuntimeSkills {
		changed = append(changed, "runtime skills ("+strconv.FormatBool(e.Persisted.RuntimeSkills)+" -> "+strconv.FormatBool(e.Live.RuntimeSkills)+")")
	}
	if e.Persisted.WorkspaceRoot != e.Live.WorkspaceRoot {
		changed = append(changed, configValueChange("workspace root", e.Persisted.WorkspaceRoot, e.Live.WorkspaceRoot))
	}
	if e.Persisted.AgentAdapter != e.Live.AgentAdapter {
		changed = append(changed, configValueChange("agent adapter", e.Persisted.AgentAdapter, e.Live.AgentAdapter))
	}
	if e.Persisted.PermissionPosture != e.Live.PermissionPosture {
		changed = append(changed, configValueChange("permission posture", e.Persisted.PermissionPosture, e.Live.PermissionPosture))
	}
	if e.Persisted.NativePermissionPolicyRev != e.Live.NativePermissionPolicyRev {
		changed = append(changed, "native permission policy")
	}
	// A digest, so it is named and not printed — the same treatment every other
	// Rev field here gets, and for the same reason: two hex strings tell a reader
	// nothing they can act on. What they need is which of their configuration
	// moved, and for this field the answer is "the external capabilities you
	// attached" — the MCP servers, whose identity the composition root supplied.
	if e.Persisted.ExternalCapabilityRev != e.Live.ExternalCapabilityRev {
		changed = append(changed, "external capability")
	}
	return "session: restore config mismatch: changed fields: " + strings.Join(changed, ", ") + "; pass WithAllowConfigMismatch to override"
}

func configValueChange(field, persisted, live string) string {
	return field + " (" + strconv.Quote(persisted) + " -> " + strconv.Quote(live) + ")"
}

// RestoreRejectedError reports a restore refused by the configured RestoreDecider
// (or by default policy). It carries the full typed assessment so callers and
// operators see exactly which fields drifted and how severely. Cause is set only
// when the rejection was caused by the decider ITSELF failing (a returned error or
// a timeout — a timeout is a rejection): it stays inspectable via Unwrap so callers
// can errors.As/errors.Is through to the underlying cause (e.g.
// context.DeadlineExceeded). A plain policy rejection leaves Cause nil.
type RestoreRejectedError struct {
	Assessment event.DriftAssessment
	Source     event.DecisionSource
	Cause      error
}

func (e *RestoreRejectedError) Error() string {
	warnCategories := make([]string, 0, len(e.Assessment.Changes))
	infoCount := 0
	for _, change := range e.Assessment.Changes {
		switch change.Severity {
		case event.DriftWarn:
			warnCategories = append(warnCategories, string(change.Category))
		default:
			infoCount++
		}
	}
	source := string(e.Source)
	if source == "" {
		source = "policy"
	}
	msg := "session: restore rejected by " + source + ": " +
		strconv.Itoa(len(warnCategories)) + " warn " + pluralize("category", "categories", len(warnCategories))
	if len(warnCategories) > 0 {
		msg += " (" + strings.Join(warnCategories, ", ") + ")"
	}
	msg += "; " + strconv.Itoa(infoCount) + " info " + pluralize("change", "changes", infoCount)
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}
	return msg
}

// Unwrap exposes the underlying decider failure (nil for a plain policy rejection)
// so errors.As/errors.Is reach it.
func (e *RestoreRejectedError) Unwrap() error { return e.Cause }

func pluralize(singular, plural string, n int) string {
	if n == 1 {
		return singular
	}
	return plural
}

type AgentNameMismatchError struct{ Persisted, Configured identity.AgentName }

func (e *AgentNameMismatchError) Error() string {
	return "session: restore agent name mismatch: persisted=" + strconv.Quote(string(e.Persisted)) + " != configured=" + strconv.Quote(string(e.Configured)) + "; pass WithAllowConfigMismatch to override"
}

const (
	RestoreRuntimeMissing            = "missing_runtime"
	RestoreRuntimeUnavailable        = "runtime_unavailable"
	RestoreRuntimeTargetMismatch     = "target_mismatch"
	RestoreRuntimeCredentialMismatch = "credential_mismatch" // #nosec G101 -- closed error-category label, not a credential
	RestoreRuntimeEffortMismatch     = "effort_mismatch"
)

// RestoreRuntimeMismatchError reports a fail-closed adapter restore decision.
// Its public text is deliberately category-only: journal selectors, model keys,
// credentials, and catalog/provider details never reach model-facing errors.
type RestoreRuntimeMismatchError struct {
	Kind  string
	Cause error
}

func (e *RestoreRuntimeMismatchError) Error() string {
	if e == nil || e.Kind == "" {
		return "session: restore runtime mismatch"
	}
	return "session: restore runtime mismatch: " + string(e.Kind)
}

func (e *RestoreRuntimeMismatchError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

type RestoreDiscoveryErrorKind string

const (
	RestoreNoSessionStarted RestoreDiscoveryErrorKind = "no_session_started"
	RestoreNoPrimerLoop     RestoreDiscoveryErrorKind = "no_primer_loop"
)

type RestoreDiscoveryError struct {
	Kind      RestoreDiscoveryErrorKind
	SessionID uuid.UUID
}

func (e *RestoreDiscoveryError) Error() string {
	if e.Kind == RestoreNoSessionStarted {
		return "session: restore: no SessionStarted in stream for " + e.SessionID.String()
	}
	if e.Kind == RestoreNoPrimerLoop {
		return "session: restore: no root LoopStarted in stream for " + e.SessionID.String()
	}
	return "session: restore: discovery failed for " + e.SessionID.String()
}

type RestoreErrorKind string

const (
	RestoreLeaseFailed   RestoreErrorKind = "lease_failed"
	RestoreJournalFailed RestoreErrorKind = "journal_failed"
	RestoreReplayFailed  RestoreErrorKind = "replay_failed"
	RestoreAppendFailed  RestoreErrorKind = "append_failed"
	// RestoreAdoptionInvalid names the specific failure of building/validating the
	// durable ConfigurationAdopted (event.ValidateEvent rejected it), distinct from
	// RestoreAppendFailed (an actual journal Append failure — lost lease, storage
	// error). It lets a caller tell "the decision produced a malformed adoption"
	// apart from "the journal write failed".
	RestoreAdoptionInvalid       RestoreErrorKind = "adoption_invalid"
	RestoreLoopFailed            RestoreErrorKind = "loop_failed"
	RestoreContextDone           RestoreErrorKind = "context_done"
	RestoreIDGenerationFailed    RestoreErrorKind = "id_generation_failed"
	RestoreForeignSIDMissing     RestoreErrorKind = "foreign_sid_missing"
	RestoreForeignBuilderMissing RestoreErrorKind = "foreign_builder_missing"
	RestoreMaterializeFailed     RestoreErrorKind = "materialize_failed"
)

type RestoreError struct {
	Kind  RestoreErrorKind
	Cause error
}

func (e *RestoreError) Error() string {
	msg := "session: restore failed (" + string(e.Kind) + ")"
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}
	return msg
}
func (e *RestoreError) Unwrap() error { return e.Cause }

type GateErrorKind string

const (
	GateNotFound      GateErrorKind = "not_found"
	GateNotReady      GateErrorKind = "not_ready"
	GateKindMismatch  GateErrorKind = "kind_mismatch"
	GateActionInvalid GateErrorKind = "action_invalid"
	GateCapacity      GateErrorKind = "capacity"
	GateAppendFailed  GateErrorKind = "append_failed"
)

type GateError struct {
	GateID gate.ID
	Kind   GateErrorKind
	Cause  error
}

func (e *GateError) Error() string {
	prefix := "session: gate"
	if e.GateID != (gate.ID{}) {
		prefix += " " + e.GateID.String()
	}
	switch e.Kind {
	case GateNotFound:
		return prefix + " not found"
	case GateNotReady:
		return prefix + " not ready"
	case GateKindMismatch:
		return prefix + " kind mismatch"
	case GateActionInvalid:
		return prefix + " action invalid"
	case GateCapacity:
		return prefix + " capacity exceeded"
	case GateAppendFailed:
		if e.Cause != nil {
			return prefix + " append failed: " + e.Cause.Error()
		}
		return prefix + " append failed"
	}
	return prefix + " error"
}
func (e *GateError) Unwrap() error         { return e.Cause }
func (e *GateError) GateErrorKind() string { return string(e.Kind) }

type WorkspaceNotConfiguredError struct{}

func (*WorkspaceNotConfiguredError) Error() string {
	return "session: workspace checkpointing is not configured"
}

// WorkspaceRootBusyError reports that an exclusive workspace root is already
// leased by another session. HolderEpoch is copied from the storage refusal so
// callers never need an internal runtime type to diagnose contention.
type WorkspaceRootBusyError struct {
	Root        string
	HolderEpoch uint64
	Cause       error
}

func (e *WorkspaceRootBusyError) Error() string {
	msg := "session: workspace root busy: " + e.Root
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}
	return msg
}

func (e *WorkspaceRootBusyError) Unwrap() error { return e.Cause }

// WorkspaceRootLeaseLostError reports that an exclusive workspace lease ended
// while its session was live. It is the public leaf chained by SessionFaulted.
type WorkspaceRootLeaseLostError struct{}

func (*WorkspaceRootLeaseLostError) Error() string {
	return "session: workspace root lease lost"
}

// WorkspaceRecoveryError reports that a per-session workspace destination could
// not be established or recovered safely. Path identifies the refused filesystem
// object, Reason is a stable diagnostic, and Cause preserves any syscall failure.
type WorkspaceRecoveryError struct {
	Path   string
	Reason string
	Cause  error
}

func (e *WorkspaceRecoveryError) Error() string {
	msg := "session: unsafe workspace recovery destination: " + e.Path
	if e.Reason != "" {
		msg += ": " + e.Reason
	}
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}
	return msg
}

func (e *WorkspaceRecoveryError) Unwrap() error { return e.Cause }
