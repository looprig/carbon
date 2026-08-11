package event

import (
	"crypto/sha256"
	"strings"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
)

type CompactAttemptID uuid.UUID

func (id CompactAttemptID) IsZero() bool { return uuid.UUID(id).IsZero() }

func (id CompactAttemptID) MarshalText() ([]byte, error) {
	return uuid.UUID(id).MarshalText()
}

func (id *CompactAttemptID) UnmarshalText(text []byte) error {
	var parsed uuid.UUID
	if err := parsed.UnmarshalText(text); err != nil {
		return err
	}
	*id = CompactAttemptID(parsed)
	return nil
}

type CompactionReason uint8

const (
	CompactionReasonUnspecified CompactionReason = iota
	CompactionReasonManual
	CompactionReasonAutomatic
)

func (r CompactionReason) Valid() bool {
	return r >= CompactionReasonManual && r <= CompactionReasonAutomatic
}

type CompactRejectReason uint8

const (
	CompactRejectUnspecified CompactRejectReason = iota
	CompactRejectControlLaneFull
	CompactRejectShuttingDown
	CompactRejectInterrupted
	CompactRejectCanceled
	CompactRejectStaleBasis
	CompactRejectProgressPublication
	CompactRejectUnavailable
	CompactRejectExecutionFailed
	CompactRejectInvalidSummary
	CompactRejectContextCountFailed
	CompactRejectSummaryTooLarge
	CompactRejectInternal
	CompactRejectContextLimitUnknown
)

func (r CompactRejectReason) Valid() bool {
	return r >= CompactRejectControlLaneFull && r <= CompactRejectContextLimitUnknown
}

type CompactionStarted struct {
	ephemeral
	loopScoped
	Header
	AttemptID CompactAttemptID `json:"attempt_id"`
	Reason    CompactionReason `json:"reason"`
	Basis     ContextBasis     `json:"basis"`
}

type CompactionCommitted struct {
	enduring
	loopScoped
	Header
	AttemptID        CompactAttemptID     `json:"attempt_id"`
	WaiterCommandIDs []uuid.UUID          `json:"waiter_command_ids"`
	Reason           CompactionReason     `json:"reason"`
	Basis            ContextBasis         `json:"basis"`
	Summary          *content.UserMessage `json:"summary"`
	PostContext      ContextMeasurement   `json:"post_context"`
	Duration         time.Duration        `json:"duration,omitzero"`
}

type CompactionRejected struct {
	enduring
	loopScoped
	Header
	AttemptID        CompactAttemptID    `json:"attempt_id"`
	WaiterCommandIDs []uuid.UUID         `json:"waiter_command_ids"`
	Reason           CompactionReason    `json:"reason"`
	Basis            ContextBasis        `json:"basis"`
	RejectReason     CompactRejectReason `json:"reject_reason"`
	Duration         time.Duration       `json:"duration,omitzero"`
}

type CompactWaiterResolved struct {
	enduring
	loopScoped
	Header
	AttemptID        CompactAttemptID `json:"attempt_id"`
	CommittedEventID uuid.UUID        `json:"committed_event_id"`
}

type CompactWaiterRejected struct {
	enduring
	loopScoped
	Header
	AttemptID CompactAttemptID    `json:"attempt_id"`
	Reason    CompactRejectReason `json:"reason"`
}

func (CompactionStarted) isEvent()     {}
func (CompactionCommitted) isEvent()   {}
func (CompactionRejected) isEvent()    {}
func (CompactWaiterResolved) isEvent() {}
func (CompactWaiterRejected) isEvent() {}
func (CompactWaiterResolved) isReply() {}
func (CompactWaiterRejected) isReply() {}

// CompactWaiterReplyID derives the idempotency key for one per-command outcome.
func CompactWaiterReplyID(attempt CompactAttemptID, commandID uuid.UUID, resolved bool) uuid.UUID {
	material := make([]byte, 0, len("looprig.compaction.waiter-reply.v1\x00")+len(attempt)+len(commandID)+1)
	material = append(material, "looprig.compaction.waiter-reply.v1\x00"...)
	attemptUUID := uuid.UUID(attempt)
	material = append(material, attemptUUID[:]...)
	material = append(material, commandID[:]...)
	if resolved {
		material = append(material, 1)
	} else {
		material = append(material, 0)
	}
	sum := sha256.Sum256(material)
	var id uuid.UUID
	copy(id[:], sum[:len(id)])
	id[6] = (id[6] & 0x0f) | 0x80
	id[8] = (id[8] & 0x3f) | 0x80
	return id
}

func validateCompactionStarted(value CompactionStarted) error {
	const name EventName = "CompactionStarted"
	if value.Visibility() != Public {
		return invalidCompaction(name, FieldVisibility)
	}
	if value.AttemptID.IsZero() {
		return invalidCompaction(name, FieldAttemptID)
	}
	if !value.Reason.Valid() {
		return invalidCompaction(name, FieldReason)
	}
	return validateCompactionBasis(name, value.Basis)
}

func validateCompactionCommitted(value CompactionCommitted) error {
	const name EventName = "CompactionCommitted"
	if value.Visibility() != Public {
		return invalidCompaction(name, FieldVisibility)
	}
	if value.AttemptID.IsZero() {
		return invalidCompaction(name, FieldAttemptID)
	}
	if err := validateCompactionWaiters(name, value.WaiterCommandIDs); err != nil {
		return err
	}
	if !value.Reason.Valid() {
		return invalidCompaction(name, FieldReason)
	}
	if err := validateCompactionBasis(name, value.Basis); err != nil {
		return err
	}
	if !validCompactionSummary(value.Summary) {
		return invalidCompaction(name, FieldSummary)
	}
	if err := value.PostContext.Validate(); err != nil {
		return invalidCompaction(name, FieldPostContext)
	}
	if value.Duration < 0 {
		return invalidCompaction(name, FieldDuration)
	}
	return nil
}

func validateCompactionRejected(value CompactionRejected) error {
	const name EventName = "CompactionRejected"
	if value.Visibility() != Public {
		return invalidCompaction(name, FieldVisibility)
	}
	if value.AttemptID.IsZero() {
		return invalidCompaction(name, FieldAttemptID)
	}
	if err := validateCompactionWaiters(name, value.WaiterCommandIDs); err != nil {
		return err
	}
	if !value.Reason.Valid() {
		return invalidCompaction(name, FieldReason)
	}
	if err := validateCompactionBasis(name, value.Basis); err != nil {
		return err
	}
	if !value.RejectReason.Valid() {
		return invalidCompaction(name, FieldRejectReason)
	}
	if value.Duration < 0 {
		return invalidCompaction(name, FieldDuration)
	}
	return nil
}

func validateCompactWaiterResolved(value CompactWaiterResolved) error {
	const name EventName = "CompactWaiterResolved"
	if value.Visibility() != Public {
		return invalidCompaction(name, FieldVisibility)
	}
	if value.AttemptID.IsZero() {
		return invalidCompaction(name, FieldAttemptID)
	}
	if value.Cause.CommandID.IsZero() {
		return invalidCompaction(name, FieldCommandID)
	}
	if value.CommittedEventID.IsZero() {
		return invalidCompaction(name, FieldCommittedEventID)
	}
	if value.EventID != CompactWaiterReplyID(value.AttemptID, value.Cause.CommandID, true) {
		return invalidCompaction(name, FieldEventID)
	}
	return nil
}

func validateCompactWaiterRejected(value CompactWaiterRejected) error {
	const name EventName = "CompactWaiterRejected"
	if value.Visibility() != Public {
		return invalidCompaction(name, FieldVisibility)
	}
	if value.AttemptID.IsZero() {
		return invalidCompaction(name, FieldAttemptID)
	}
	if value.Cause.CommandID.IsZero() {
		return invalidCompaction(name, FieldCommandID)
	}
	if !value.Reason.Valid() {
		return invalidCompaction(name, FieldRejectReason)
	}
	if value.EventID != CompactWaiterReplyID(value.AttemptID, value.Cause.CommandID, false) {
		return invalidCompaction(name, FieldEventID)
	}
	return nil
}

func validateCompactionBasis(name EventName, basis ContextBasis) error {
	if basis.Revision == 0 || basis.ThroughEventID.IsZero() {
		return invalidCompaction(name, FieldName("Basis"))
	}
	return nil
}

func validateCompactionWaiters(name EventName, waiters []uuid.UUID) error {
	if len(waiters) == 0 {
		return invalidCompaction(name, FieldWaiterCommandIDs)
	}
	seen := make(map[uuid.UUID]struct{}, len(waiters))
	for _, waiter := range waiters {
		if waiter.IsZero() {
			return invalidCompaction(name, FieldWaiterCommandIDs)
		}
		if _, duplicate := seen[waiter]; duplicate {
			return invalidCompaction(name, FieldWaiterCommandIDs)
		}
		seen[waiter] = struct{}{}
	}
	return nil
}

func validCompactionSummary(summary *content.UserMessage) bool {
	if summary == nil || summary.Role != content.RoleUser || len(summary.Blocks) != 1 {
		return false
	}
	text, ok := summary.Blocks[0].(*content.TextBlock)
	return ok && text != nil && strings.TrimSpace(text.Text) != ""
}

func invalidCompaction(name EventName, field FieldName) *InvalidEventError {
	return &InvalidEventError{Event: name, Field: field, Rule: RuleInvalid}
}
