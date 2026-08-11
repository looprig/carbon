package journal

import (
	"fmt"
	"strconv"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/identity"
)

// CommandRouteMismatchError reports disagreement between a durable delegate
// command's embedded target and the live CommandRecord dispatch route.
type CommandRouteMismatchError struct {
	RecordLoopID uuid.UUID
	TargetLoopID uuid.UUID
}

func (e *CommandRouteMismatchError) Error() string {
	return fmt.Sprintf("journal: delegate command route mismatch: record loop %s != target loop %s", e.RecordLoopID, e.TargetLoopID)
}

// DeliveryTransitionError reports a phased delegate command that violates the
// logical intent-to-fallback ordering or otherwise cannot participate in the
// transition index. It carries only bounded identity/category data; payloads
// are deliberately excluded.
type DeliveryTransitionError struct {
	CommandID uuid.UUID
	Phase     command.DelegateDeliveryPhase
	Reason    string
}

func (e *DeliveryTransitionError) Error() string {
	return fmt.Sprintf("journal: invalid delegate delivery transition for command %s phase %q: %s", e.CommandID, e.Phase, e.Reason)
}

// JournalRecord is the sealed sum a session's serialized writer persists: an
// Enduring event, a command (the intent log), or an internal LeaseFence. It is a
// marker plus the one backend-neutral fact the writer needs to persist without
// re-inspecting the payload — the record's idempotency id (the stable per-record id
// a backend uses to de-duplicate a redelivered append). The concrete payload codec
// is the existing event/command marshaler; a record only carries the typed payload
// and exposes how to identify it. How a record is routed/stored is the backend's
// concern (a storage ledger name, a subject, …), never the record's.
//
// The set is sealed by the unexported isJournalRecord marker: only the wrapper
// types in this package implement it, so the serializer's switch over the sum is
// exhaustive and a foreign type can never masquerade as a record.
type JournalRecord interface {
	isJournalRecord()
	// IdempotencyID is the stable per-record id a backend uses as its de-dup key so
	// a redelivered append de-duplicates: an event's EventID, a command's physical
	// CommandRecordID, or a fence's epoch.
	IdempotencyID() string
}

// EventRecord wraps an Enduring event.Event as a JournalRecord. The event already
// carries its producer Coordinates and Scope; its id is the event's EventID. The
// serializer encodes the wrapped event via event.MarshalEvent (which fails closed on
// an Ephemeral event); the record never re-encodes.
type EventRecord struct {
	ev event.Event
}

// NewEventRecord wraps ev for the journal. ev must be an Enduring event; the
// Ephemeral check is the serializer's (event.MarshalEvent), not this wrapper's.
func NewEventRecord(ev event.Event) EventRecord { return EventRecord{ev: ev} }

// Event returns the wrapped event for the serializer to marshal.
func (r EventRecord) Event() event.Event { return r.ev }

func (EventRecord) isJournalRecord() {}

// IdempotencyID is the event's EventID rendered canonically.
func (r EventRecord) IdempotencyID() string { return r.ev.EventHeader().EventID.String() }

// CommandRecord wraps a command.Command targeting a specific loop. Unlike an
// event, a command does not uniformly carry its routing coordinates (Interrupt and
// Shutdown carry only a Header; the session dispatches them by other means), so the
// writer supplies the target sessionID/loopID at construction. Its id is the
// command's physical CommandRecordID. The serializer encodes the wrapped command via
// command.MarshalCommand.
type CommandRecord struct {
	sessionID uuid.UUID
	loopID    uuid.UUID
	cmd       command.Command
}

// CommandRecordID is the physical idempotency identity of one command record.
// Delivery fallback records retain the logical command UUID while using their
// phase as a typed suffix, so the intent and fallback frames can both be
// durable without weakening the journal's ordinary idempotency collision rule.
type CommandRecordID struct {
	CommandID uuid.UUID
	Phase     command.DelegateDeliveryPhase
}

// String returns the canonical physical id. Ordinary and intent commands use
// the logical UUID unchanged; only fallback_queued has a distinct physical id.
func (id CommandRecordID) String() string {
	if id.Phase == command.DelegateDeliveryPhaseFallbackQueued {
		return id.CommandID.String() + "/" + string(id.Phase)
	}
	return id.CommandID.String()
}

// LogicalCommandID returns the command UUID independent of its durable phase.
func (r CommandRecord) LogicalCommandID() uuid.UUID { return r.cmd.CommandHeader().CommandID }

// DeliveryPhase returns the durable delegate-delivery phase carried by a
// UserInput, or the zero phase for every other command kind.
func (r CommandRecord) DeliveryPhase() command.DelegateDeliveryPhase {
	input, ok := r.cmd.(command.UserInput)
	if !ok {
		return ""
	}
	return input.DelegateDeliveryPhase
}

// PhysicalID returns the typed idempotency identity used by the journal
// envelope and backend message id. Keeping phase selection here prevents
// callers from constructing unsafe ad-hoc string ids.
func (r CommandRecord) PhysicalID() CommandRecordID {
	return CommandRecordID{CommandID: r.LogicalCommandID(), Phase: r.DeliveryPhase()}
}

// NormalizedDeliveryFingerprint fingerprints the exact UserInput payload with
// only DelegateDeliveryPhase cleared. Accepted is json:"-" and therefore does
// not enter the fingerprint; blocks, route, agency, hand-back, timestamps, and
// all other durable fields remain part of it.
func (r CommandRecord) NormalizedDeliveryFingerprint() (Fingerprint, error) {
	input, ok := r.cmd.(command.UserInput)
	if !ok || !input.DelegateDeliveryPhase.Valid() {
		return Fingerprint{}, &DeliveryTransitionError{
			CommandID: r.LogicalCommandID(),
			Phase:     r.DeliveryPhase(),
			Reason:    "command is not a phased user input",
		}
	}
	input.DelegateDeliveryPhase = ""
	body, err := command.MarshalCommand(input)
	if err != nil {
		return Fingerprint{}, err
	}
	return NewFingerprint("command", body), nil
}

// NewCommandRecord wraps cmd as the intent-log record targeting loop loopID in
// session sessionID. The caller is the writer, which knows the dispatch target;
// the command itself may not carry it.
func NewCommandRecord(sessionID, loopID uuid.UUID, cmd command.Command) CommandRecord {
	return CommandRecord{sessionID: sessionID, loopID: loopID, cmd: cmd}
}

// ValidateCommandRecordRoute validates the command's own identity contract, then
// cross-checks the duplicated live dispatch route for machine NoFold or phased
// delegate input. A zero record LoopID is accepted only because storage replay
// cannot reconstruct it.
func ValidateCommandRecordRoute(record CommandRecord) error {
	if err := command.ValidateCommand(record.cmd); err != nil {
		return err
	}
	input, ok := record.cmd.(command.UserInput)
	if !ok || input.Agency != identity.AgencyMachine || (!input.NoFold && input.DelegateDeliveryPhase == "") {
		return nil
	}
	if record.loopID.IsZero() || record.loopID == input.TargetLoopID {
		return nil
	}
	return &CommandRouteMismatchError{RecordLoopID: record.loopID, TargetLoopID: input.TargetLoopID}
}

// Command returns the wrapped command for the serializer to marshal.
func (r CommandRecord) Command() command.Command { return r.cmd }

// SessionID is the session this command was recorded under.
func (r CommandRecord) SessionID() uuid.UUID { return r.sessionID }

// LoopID is the dispatch target the writer recorded for this command — the loop the
// intent-log entry belongs to. It is the backend-neutral routing coordinate a
// consumer keys on (replacing the subject a NATS backend derived it into).
func (r CommandRecord) LoopID() uuid.UUID { return r.loopID }

func (CommandRecord) isJournalRecord() {}

// IdempotencyID is the command's physical id rendered canonically.
func (r CommandRecord) IdempotencyID() string { return r.PhysicalID().String() }

// LeaseFence is an internal journal record marking a lease-handover boundary: a
// monotonically increasing Epoch fenced into the stream when ownership of a
// session's writer lease changes. It is journal-private (not an event or a
// command); the EventReplayer never decodes it. Codec:
// MarshalLeaseFence/UnmarshalLeaseFence in record_json.go.
type LeaseFence struct {
	Epoch uint64 `json:"epoch"`
}

// FenceRecord wraps a LeaseFence as a JournalRecord. The fence carries no session
// id of its own, so the writer supplies the target sessionID at construction; its
// idempotency id is the epoch.
type FenceRecord struct {
	sessionID uuid.UUID
	fence     LeaseFence
}

// NewFenceRecord wraps fence as the fence record for session sessionID.
func NewFenceRecord(sessionID uuid.UUID, fence LeaseFence) FenceRecord {
	return FenceRecord{sessionID: sessionID, fence: fence}
}

// Fence returns the wrapped LeaseFence for the serializer to marshal.
func (r FenceRecord) Fence() LeaseFence { return r.fence }

// SessionID is the session this fence marks a lease handover for.
func (r FenceRecord) SessionID() uuid.UUID { return r.sessionID }

func (FenceRecord) isJournalRecord() {}

// IdempotencyID is the epoch rendered as a decimal string.
func (r FenceRecord) IdempotencyID() string { return strconv.FormatUint(r.fence.Epoch, 10) }

// GatePreparedRecord is the PRIVATE durable record for a gate's prepare step. It
// carries the GatePrepared event (the public envelope stored privately until
// ActivateGate appends the public GateOpened) PLUS the sealed gate.Payload the
// resolver needs for response validation and restore — a payload that must NEVER
// be exposed to SSE/history and must NEVER be appended through NewEventRecord or
// hub.PublishEvent. Its idempotency id is the prepared event's EventID.
type GatePreparedRecord struct {
	prepared event.GatePrepared
	payload  gate.OpenPayload
}

// NewGatePreparedRecord wraps the private prepared projection and its typed
// payload as a single private journal record. The caller must NOT also append
// the GatePrepared event as a public EventRecord.
func NewGatePreparedRecord(prepared event.GatePrepared, payload gate.OpenPayload) GatePreparedRecord {
	return GatePreparedRecord{prepared: prepared, payload: payload}
}

// Prepared returns the private prepared projection for the serializer to marshal.
func (r GatePreparedRecord) Prepared() event.GatePrepared { return r.prepared }

// Payload returns the private typed payload the resolver uses for validation/restore.
func (r GatePreparedRecord) Payload() gate.OpenPayload { return r.payload }

func (GatePreparedRecord) isJournalRecord() {}

// IdempotencyID is the prepared event's EventID rendered canonically.
func (r GatePreparedRecord) IdempotencyID() string {
	return r.prepared.EventHeader().EventID.String()
}
