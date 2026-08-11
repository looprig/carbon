package loopruntime

import (
	"github.com/looprig/core/content"
	"github.com/looprig/core/content/streamaccumulator"
)

// blockState is the assistant block state for one AIMessage: thinking, text, and
// tool-use blocks accumulated from chunks. The zero value is ready to use.
//
// Phase 10 (Open Items A) validated the thin `block{state blockState}` wrapper
// against real code and COLLAPSED it: it was a one-field struct with no methods
// and no runtime role beyond holding its state — every caller reached straight
// through to blockState. blockState carries the materialization methods
// (AIMessage/ToolUses) directly, so the wrapper added no value (YAGNI). The step
// wrapper was collapsed for the same reason; the turn runtime state is the real
// one and is owned by the actor.
type blockState struct {
	msgs blockMessages
}

// blockMessages holds the three stream accumulators that fold streamed chunks
// into complete content blocks. Each is a pure converter from internal/content/
// streamaccumulator; the loop owns event emission and policy, not these.
type blockMessages struct {
	thinking streamaccumulator.Thinking
	text     streamaccumulator.Text
	toolUses streamaccumulator.ToolUses
}

// AIMessage materializes the single assistant message from the accumulated
// thinking, text, and tool-use blocks, in that block order (thinking, then text,
// then tool-use blocks in ascending Index order). An empty accumulator
// contributes no block; an all-empty blockState yields an AIMessage with no
// blocks. The tool-use blocks carry the RAW concatenated Input verbatim; any
// validation or sanitization of the stored message is the caller's policy.
func (b *blockState) AIMessage() *content.AIMessage {
	var blocks []content.Block
	if tb := b.msgs.thinking.Block(); tb != nil {
		blocks = append(blocks, tb)
	}
	if tb := b.msgs.text.Block(); tb != nil {
		blocks = append(blocks, tb)
	}
	for _, tu := range b.msgs.toolUses.Blocks() {
		// Go 1.22+ scopes tu per iteration, so &tu is a distinct pointer for each
		// block (no per-iteration copy needed for correctness).
		blocks = append(blocks, &tu)
	}
	return &content.AIMessage{Message: content.Message{Role: content.RoleAssistant, Blocks: blocks}}
}

// ToolUses returns the executable view of the tool-use blocks contained in the
// assistant message, in ascending Index order, with their RAW concatenated
// Input. This is a distinct allocation from the AIMessage's child tool-use
// blocks, so the caller may sanitize the stored message's Input independently of
// the raw executable view. It returns nil when no tool-use chunk was folded.
func (b *blockState) ToolUses() []content.ToolUseBlock {
	return b.msgs.toolUses.Blocks()
}
