package foreign

import (
	"context"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/loop"
)

// RestoredForeign is the journal-recovered seed for a foreign loop: the recovered
// foreign session id, the committed turn count, and the committed conversation thread.
// A restored loop comes up idle, seeded with this state, and resumes (never re-creates)
// the recorded session on its next turn.
type RestoredForeign struct {
	ForeignSID string
	// AgentSessionID is optional journaled agent state. It is empty for
	// legacy events and for foreign runtimes that do not expose one.
	AgentSessionID string
	TurnIndex      event.TurnIndex
	Msgs           content.AgenticMessages
}

// RestoredBuilder is the composition-root seam a session uses to reconstruct a foreign
// loop from journal-recovered state. It mirrors Builder but carries the RestoredForeign
// seed and returns no sid because the seed already holds it.
type RestoredBuilder func(
	loopCtx context.Context,
	sessionID, loopID uuid.UUID,
	parent loop.Provenance,
	pub EventPublisher,
	cfg loop.BoundDefinition,
	idGen func() (uuid.UUID, error),
	fac *event.Factory,
	seed RestoredForeign,
) (loop.Backend, error)

// ServicesRestoredBuilder is the additive restored-loop construction seam. It
// mirrors RestoredBuilder and receives the immutable Services snapshot last.
type ServicesRestoredBuilder func(
	loopCtx context.Context,
	sessionID, loopID uuid.UUID,
	parent loop.Provenance,
	pub EventPublisher,
	cfg loop.BoundDefinition,
	idGen func() (uuid.UUID, error),
	fac *event.Factory,
	seed RestoredForeign,
	services Services,
) (loop.Backend, error)
