package command

import (
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/tool"
)

const (
	CommandReplaceLoopExternalTools CommandName  = "ReplaceLoopExternalTools"
	ReplaceLoopExternalToolsAck     CommandField = "Ack"
	ReplaceLoopExternalToolsSource  CommandField = "Source"
)

// LoopToolsResult is the loop actor's synchronous reply to ReplaceLoopExternalTools.
// Err is the typed failure (nil on success); on success Generation echoes the
// installed generation and Installed reports how many external tools that source now
// contributes to the next turn. It rides a live channel and is never serialized.
type LoopToolsResult struct {
	Err        error
	Generation string
	Installed  int
}

// ReplaceLoopExternalTools atomically REPLACES one source's external tool slot on a
// loop. Like SetLoopMode it is a CONTROL command carried on a live reply channel, not
// a journaled wire command: its durable, replayable record is the
// event.LoopExternalToolsetChanged the actor emits, so it is deliberately absent from
// the intent-log codec. The change takes effect at the NEXT turn boundary; a running
// turn keeps the toolset it started under.
//
// Tools are ALREADY BUILT. Building is the session's job, not the actor's: the
// session owns the loop's tool.Bindings (the actor only ever receives a
// BoundDefinition), and an external factory may perform I/O — building on the actor
// goroutine would stall the loop and, with it, the very idle detection this feature
// depends on. Identities is the matching durable identity projection, computed from
// the built tools by the same caller so the emitted record cannot drift from what is
// installed; it is index-aligned with Tools.
//
// An empty Tools is legal: it clears the source's slot. Ack is required and must be
// non-nil and buffered(1).
type ReplaceLoopExternalTools struct {
	Header
	Source     string                       `json:"source,omitzero"`
	Generation string                       `json:"generation,omitzero"`
	Tools      []tool.InvokableTool         `json:"-"` // live values; no JSON representation
	Identities []event.ExternalToolIdentity `json:"-"` // durable projection of Tools
	Ack        chan<- LoopToolsResult       `json:"-"` // live reply channel
}

func (ReplaceLoopExternalTools) isCommand() {}

// Validate checks the actor-protecting structural contract: Ack must be present AND
// buffered (cap >= 1) — the actor replies with a single non-blocking direct send, so
// an unbuffered Ack would wedge it — and Tools must be index-aligned with Identities,
// since the actor installs Tools while the journal records Identities and a mismatch
// would make the durable record a lie. Source is required here because it names the
// slot the actor would otherwise replace ambiguously. The tool VALUES and the
// name-collision rules are validated by the caller and the actor, not here.
func (c ReplaceLoopExternalTools) Validate() error {
	if c.Ack == nil {
		return &InvalidCommandError{Command: CommandReplaceLoopExternalTools, Field: ReplaceLoopExternalToolsAck}
	}
	if cap(c.Ack) < 1 {
		return &UnbufferedAckError{Command: CommandReplaceLoopExternalTools, Field: ReplaceLoopExternalToolsAck}
	}
	if c.Source == "" {
		return &InvalidCommandError{Command: CommandReplaceLoopExternalTools, Field: ReplaceLoopExternalToolsSource}
	}
	if len(c.Tools) != len(c.Identities) {
		return &InvalidCommandError{Command: CommandReplaceLoopExternalTools, Field: ReplaceLoopExternalToolsTools}
	}
	return nil
}

// ReplaceLoopExternalToolsTools names the Tools/Identities alignment violation.
const ReplaceLoopExternalToolsTools CommandField = "Tools"
