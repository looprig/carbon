package event

import (
	"crypto/sha256"
	"encoding/json"
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
	CompactRejectRetainedTailTooLarge
)

func (r CompactRejectReason) Valid() bool {
	return r >= CompactRejectControlLaneFull && r <= CompactRejectRetainedTailTooLarge
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
	AttemptID        CompactAttemptID        `json:"attempt_id"`
	WaiterCommandIDs []uuid.UUID             `json:"waiter_command_ids"`
	Reason           CompactionReason        `json:"reason"`
	Basis            ContextBasis            `json:"basis"`
	Summary          *content.UserMessage    `json:"summary"`
	Retained         content.AgenticMessages `json:"retained,omitempty"`
	PostContext      ContextMeasurement      `json:"post_context"`
	Duration         time.Duration           `json:"duration,omitzero"`
}

// compactionCommittedWire keeps the interface-valued retained suffix on the
// same tagged message-slice codec used by StepDone. encoding/json cannot
// reconstruct []content.Conversation on its own, while old records simply omit
// the additive field and decode with a nil suffix.
type compactionCommittedWire struct {
	Header
	AttemptID        CompactAttemptID     `json:"attempt_id"`
	WaiterCommandIDs []uuid.UUID          `json:"waiter_command_ids"`
	Reason           CompactionReason     `json:"reason"`
	Basis            ContextBasis         `json:"basis"`
	Summary          *content.UserMessage `json:"summary"`
	Retained         json.RawMessage      `json:"retained,omitempty"`
	PostContext      ContextMeasurement   `json:"post_context"`
	Duration         time.Duration        `json:"duration,omitzero"`
}

// MarshalJSON gives CompactionCommitted's additive retained graph the same
// tagged wire shape as the existing message-bearing events. Validation remains
// at ValidateEvent/MarshalEvent, so direct JSON marshaling retains the package's
// established behavior of encoding the value without revalidating all fields.
func (value CompactionCommitted) MarshalJSON() ([]byte, error) {
	var retained json.RawMessage
	if len(value.Retained) > 0 {
		encoded, err := marshalMessages(value.Retained)
		if err != nil {
			return nil, err
		}
		retained = encoded
	}
	return json.Marshal(compactionCommittedWire{
		Header: value.Header, AttemptID: value.AttemptID, WaiterCommandIDs: value.WaiterCommandIDs,
		Reason: value.Reason, Basis: value.Basis, Summary: value.Summary, Retained: retained,
		PostContext: value.PostContext, Duration: value.Duration,
	})
}

// UnmarshalJSON decodes both legacy summary-only records and the additive
// retained message graph. Empty or omitted retained arrays normalize to nil,
// matching omitempty's fixed point and keeping old event values stable.
func (value *CompactionCommitted) UnmarshalJSON(data []byte) error {
	var wire compactionCommittedWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	var retained content.AgenticMessages
	if len(wire.Retained) > 0 {
		decoded, err := unmarshalMessages(wire.Retained)
		if err != nil {
			return err
		}
		retained = decoded
	}
	*value = CompactionCommitted{
		Header: wire.Header, AttemptID: wire.AttemptID, WaiterCommandIDs: wire.WaiterCommandIDs,
		Reason: wire.Reason, Basis: wire.Basis, Summary: wire.Summary, Retained: retained,
		PostContext: wire.PostContext, Duration: wire.Duration,
	}
	return nil
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
	if !validCompactionRetained(value.Retained) || !validCompactionRetainedHistory(value.Retained) {
		return invalidCompaction(name, FieldRetained)
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

// validCompactionRetained validates the message/block graph without imposing a
// transcript ordering policy. Selection already guarantees a complete suffix;
// this boundary check only rejects nil/typed-nil messages, role/type mismatches,
// unsupported blocks, and malformed nested tool-result blocks from external
// journals. The same graph rules are used by loop compaction input validation.
func validCompactionRetained(messages content.AgenticMessages) bool {
	for _, message := range messages {
		blocks, ok := compactionMessageBlocks(message)
		if !ok || !validCompactionRetainedBlocks(blocks, 0) {
			return false
		}
		if _, err := json.Marshal(message); err != nil {
			return false
		}
	}
	return true
}

// validCompactionRetainedHistory validates the provider transcript ordering
// guaranteed by compaction-tail selection. An omitted or empty suffix is the
// legacy summary-only representation and remains valid. A non-empty suffix is
// a complete user-anchored sequence: an assistant tool-call batch is followed
// by exactly one result for each call. Folded user messages may occur before
// those results because TurnFoldedInto can add a user turn while the pair is
// still outstanding. This permits parallel calls/results (including result
// completion in any order) while rejecting orphan, duplicate, and crossing
// pairs before restore can install them as provider history.
func validCompactionRetainedHistory(messages content.AgenticMessages) bool {
	if len(messages) == 0 {
		return true
	}
	if user, ok := messages[0].(*content.UserMessage); !ok || user == nil || user.Role != content.RoleUser {
		return false
	}

	outstanding := make(map[string]struct{})
	for _, message := range messages {
		switch typed := message.(type) {
		case *content.UserMessage:
			if typed == nil || typed.Role != content.RoleUser {
				return false
			}
		case *content.AIMessage:
			if typed == nil || typed.Role != content.RoleAssistant || len(outstanding) > 0 {
				return false
			}
			for _, block := range typed.Blocks {
				call, ok := block.(*content.ToolUseBlock)
				if !ok {
					continue
				}
				if call == nil || call.ID == "" {
					return false
				}
				if _, duplicate := outstanding[call.ID]; duplicate {
					return false
				}
				outstanding[call.ID] = struct{}{}
			}
		case *content.ToolResultMessage:
			if typed == nil || typed.Role != content.RoleTool || typed.ToolUseID == "" {
				return false
			}
			if _, exists := outstanding[typed.ToolUseID]; !exists {
				return false
			}
			delete(outstanding, typed.ToolUseID)
		case *content.SystemMessage:
			if typed == nil || typed.Role != content.RoleSystem || len(outstanding) > 0 {
				return false
			}
		default:
			return false
		}
	}
	return len(outstanding) == 0
}

func compactionMessageBlocks(message content.Conversation) ([]content.Block, bool) {
	switch typed := message.(type) {
	case *content.UserMessage:
		if typed == nil || typed.Role != content.RoleUser {
			return nil, false
		}
		return typed.Blocks, true
	case *content.AIMessage:
		if typed == nil || typed.Role != content.RoleAssistant {
			return nil, false
		}
		return typed.Blocks, true
	case *content.SystemMessage:
		if typed == nil || typed.Role != content.RoleSystem {
			return nil, false
		}
		return typed.Blocks, true
	case *content.ToolResultMessage:
		if typed == nil || typed.Role != content.RoleTool {
			return nil, false
		}
		return typed.Blocks, true
	default:
		return nil, false
	}
}

const maxCompactionRetainedBlockDepth = 128

func validCompactionRetainedBlocks(blocks []content.Block, depth int) bool {
	if depth > maxCompactionRetainedBlockDepth {
		return false
	}
	for _, block := range blocks {
		if block == nil {
			return false
		}
		if nested, ok := block.(*content.ToolResultBlock); ok {
			if nested == nil || !validCompactionRetainedBlocks(nested.Content, depth+1) {
				return false
			}
			continue
		}
		switch typed := block.(type) {
		case *content.TextBlock:
			if typed == nil {
				return false
			}
		case *content.ImageBlock:
			if typed == nil {
				return false
			}
		case *content.AudioBlock:
			if typed == nil {
				return false
			}
		case *content.DocumentBlock:
			if typed == nil {
				return false
			}
		case *content.ThinkingBlock:
			if typed == nil {
				return false
			}
		case *content.ToolUseBlock:
			if typed == nil {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func invalidCompaction(name EventName, field FieldName) *InvalidEventError {
	return &InvalidEventError{Event: name, Field: field, Rule: RuleInvalid}
}
