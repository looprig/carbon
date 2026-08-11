package command

import (
	"github.com/looprig/harness/pkg/tool"
)

// ProcessNotificationResult is the transient, non-serialized outcome of
// dispatching a ProcessNotification to its owning loop (Task 24C). It answers
// "what happened to THIS delivery attempt", never a durable record:
//
//   - ProcessNotificationAccepted: the durable append genuinely persisted a NEW
//     frame (or no-persistence/headless mode has no durable concept at all) and
//     the loop took ownership of the notification.
//   - ProcessNotificationDuplicate: the append's idempotency id already named an
//     IDENTICAL durable record (a same-CommandID retry) — no second frame was
//     written; the loop may still have been (re)delivered the value, which is
//     harmless because a completion notification is metadata-only.
//   - ProcessNotificationCollision: the append's idempotency id already named a
//     durable record with a DIFFERENT persisted payload — a genuine id reuse
//     bug or forged retry. Never dispatched to the loop.
//   - ProcessNotificationStopped: delivery could not complete right now — the
//     owning loop has exited, its bounded live notification set is full, or the
//     addressed loop cannot accept process notifications at all (a foreign
//     engine). The durable command (once appended) remains authoritative: the
//     caller may retry dispatch later with the SAME CommandID.
type ProcessNotificationResult uint8

const (
	ProcessNotificationAccepted ProcessNotificationResult = iota + 1
	ProcessNotificationDuplicate
	ProcessNotificationCollision
	ProcessNotificationStopped
)

// CommandProcessNotification/FieldNotification name this command for
// CommandValidationError, alongside the shared vocabulary in validate.go.
const (
	CommandProcessNotification CommandName  = "ProcessNotification"
	FieldNotification          CommandField = "Notification"
)

// ProcessNotification is the sealed, durable wire command that carries Task 4's
// metadata-only tool.ProcessCompletionNotification DTO to its owning loop. It is
// deliberately as narrow as the DTO it wraps: it adds only the generic command
// envelope (Header) and a transient live disposition channel — never a command,
// output, stdin, host path, OS PID, or any other free-form field. Restore
// reconstructs it directly from the durable intent log (round-tripping through
// the sealed codec below) to detect and re-enqueue undelivered notifications.
//
// Header.CommandID and Notification.CommandID carry the SAME stable,
// pre-persisted id Tools allocates before a completion can be published.
// Header.CommandID is the generic envelope field the journal's idempotency
// index and the intent-log CommandRecord key on (mirroring every other sealed
// command); Notification.CommandID is Task 4's own DTO field. ValidateCommand
// rejects any disagreement fail-secure rather than silently preferring one.
type ProcessNotification struct {
	Header
	Notification tool.ProcessCompletionNotification `json:"notification"`

	// Result is the transient, optional live disposition channel: nil for a
	// restore-reconstructed redelivery (fire-and-forget — restore seeds the
	// loop directly rather than replaying this command live) or for a decoded
	// wire record (json:"-": it never rides the durable payload), and a
	// caller-owned buffered channel for a live NotifyProcessCompletion call
	// awaiting the loop's disposition.
	Result chan<- ProcessNotificationResult `json:"-"`
}

func (ProcessNotification) isCommand() {}
