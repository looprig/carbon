// updates.go implements Update (the typed value delivered on
// Session.Updates()) and the _meta decoding used both to expose that
// metadata to callers and to drive live-update dedup (see session.go's
// deliver).
package client

import (
	"encoding/json"

	"github.com/looprig/acp/protocol"
)

// UpdateMeta is this package's decoding of the `_meta` object a producing
// agent facade stamps onto every session/update notification. Field names
// and JSON tags intentionally mirror acp/agent/translate.go's updateMeta
// wire shape exactly (eventId, promptId, isReplay) — acp/client cannot
// import acp/agent (see acp/CLAUDE.md's layering rule), so this is a
// independently-owned but wire-compatible twin, not a shared type.
//
// A notification with no _meta object, or one that fails to decode, yields
// the zero UpdateMeta rather than an error: _meta is optional ACP
// extensibility data (see protocol/types_gen.go's SessionNotification.Meta
// doc), and an agent that omits or mis-shapes it should degrade to "no
// metadata available," never break update delivery entirely.
type UpdateMeta struct {
	EventID  string
	PromptID string
	IsReplay bool
}

// DecodeUpdateMeta parses raw (a SessionNotification's Meta field) into an
// UpdateMeta, defaulting to the zero value on absence or malformed input.
// Exported (not just used internally by dispatch.go) so wire-compatibility
// tests outside this package — see acp/agent/meta_roundtrip_test.go, which
// cannot import this package's unexported symbols across the acp/agent /
// acp/client boundary any other way — can feed it real producer-side bytes
// and assert the fields land correctly, without this package ever needing
// to import acp/agent back.
func DecodeUpdateMeta(raw json.RawMessage) UpdateMeta {
	if len(raw) == 0 {
		return UpdateMeta{}
	}
	var wire struct {
		EventID  string `json:"eventId"`
		PromptID string `json:"promptId"`
		IsReplay bool   `json:"isReplay"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return UpdateMeta{}
	}
	return UpdateMeta{EventID: wire.EventID, PromptID: wire.PromptID, IsReplay: wire.IsReplay}
}

// Update is one session/update notification delivered to a Session's
// Updates() channel, decoded into the ACP update payload plus its _meta.
type Update struct {
	// SessionUpdate is the update payload (a message chunk, tool call, plan,
	// usage update, and so on — see protocol.SessionUpdate).
	SessionUpdate protocol.SessionUpdate
	// Meta is this update's decoded _meta object.
	Meta UpdateMeta
	// ReceiveSequence is the Conn-owned monotonic sequence assigned to the
	// inbound session/update notification before it entered the ordered
	// notification worker.
	ReceiveSequence uint64
}
