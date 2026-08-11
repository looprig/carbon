package command

import (
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/identity"
)

// Header is the correlation/idempotency metadata embedded in every command.
// The sender stamps the fields before sending the command (the session does this
// via newCommandID); zero-valued fields mean the sender supplied none.
type Header struct {
	CommandID uuid.UUID       `json:"command_id,omitzero"` // fresh per command instance, stamped by the sender before send
	Cause     identity.Cause  `json:"cause,omitzero"`      // the direct cause of this command; zero = root (user-initiated)
	Agency    identity.Agency `json:"agency,omitzero"`     // who issued this command; AgencyMachine (zero) by default

	// CreatedAt is when this command was created, stamped at the session dispatch
	// boundary from the injected clock (mirrors event.Header.CreatedAt: minted at
	// creation, not delivery). It is the journal's creation timestamp for the
	// intent-log record and round-trips through the command codec as-is. Zero means
	// the sender supplied none (omitzero drops it), so an in-process command that is
	// never persisted is unaffected.
	CreatedAt time.Time `json:"created_at,omitzero"`
}

// CommandHeader is promoted onto every command that embeds Header.
func (h Header) CommandHeader() Header { return h }
