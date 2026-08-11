package presentation

import (
	"fmt"
	"strings"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/identity"
)

// splitLines splits a tool-result preview into display lines on "\n". An empty
// preview yields nil (no result lines; the renderer shows "(no output)"); a
// non-empty preview always yields at least one line. A trailing newline produces a
// trailing empty line, preserved as-is (the runner caps/marks the preview).
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// splitStepGroup splits a StepDone.Messages group into its single AIMessage and a
// ToolUseID→ToolResultMessage index of the tool results that follow it. The step
// shape is one AIMessage followed by zero or more ToolResultMessages (loop-machine
// design §Step); a missing AIMessage yields nil so the caller commits no assistant
// entry. UserMessages (a folded tool-continuation input) and any other message types
// are ignored — the transcript commits those from their own TurnStarted/TurnFoldedInto
// events, not from a StepDone group.
func splitStepGroup(msgs content.AgenticMessages) (*content.AIMessage, map[string]*content.ToolResultMessage) {
	var ai *content.AIMessage
	results := make(map[string]*content.ToolResultMessage)
	for _, msg := range msgs {
		switch v := msg.(type) {
		case *content.AIMessage:
			if ai == nil {
				ai = v
			}
		case *content.ToolResultMessage:
			results[v.ToolUseID] = v
		}
	}
	return ai, results
}

// toolUsesOf returns the AIMessage's tool-use blocks in block order — the executable
// children of the assistant message. A nil message yields nil.
func toolUsesOf(ai *content.AIMessage) []content.ToolUseBlock {
	if ai == nil {
		return nil
	}
	var out []content.ToolUseBlock
	for _, b := range ai.Blocks {
		if tu, ok := b.(*content.ToolUseBlock); ok {
			out = append(out, *tu)
		}
	}
	return out
}

// textOnly concatenates ONLY the narration (TextBlocks) of an assistant message,
// joined by "\n". Thinking blocks (rendered separately as the dim reasoning block)
// and tool-use blocks (rendered as their own tool cards) are excluded, so the
// committed assistant entry's Blocks carry exactly the markdown narration. An
// all-thinking/all-tool message yields "" (no narration entry).
func textOnly(blocks []content.Block) string {
	var parts []string
	for _, b := range blocks {
		if tb, ok := b.(*content.TextBlock); ok {
			parts = append(parts, tb.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// toolResultText flattens a ToolResultMessage's TextBlocks into one display string.
// The loop builds a ToolResultMessage carrying a single flattened TextBlock, so this
// concatenates every TextBlock; non-text blocks are skipped (they have no display
// form here — the live card's redacted preview is the display path for those).
func toolResultText(r *content.ToolResultMessage) string {
	if r == nil {
		return ""
	}
	var b strings.Builder
	for _, blk := range r.Blocks {
		if tb, ok := blk.(*content.TextBlock); ok {
			b.WriteString(tb.Text)
		}
	}
	return b.String()
}

// entryKind discriminates the source/kind of a committed transcript entry.
// kindTool is one resolved tool call (terminal state); kindNotice carries a leveled
// notification (info/warn/error) — including a turn-failure message at error level;
// kindInterrupted is the content-less tombstone for an interrupted turn.
type entryKind uint8

const (
	kindUser entryKind = iota
	kindAssistant
	kindTool
	kindPromptRecord
	kindInterrupted
	// kindNotice is a leveled, out-of-band notification line (the startup banner, the
	// /help listing, a non-fatal error, a turn failure). It carries a single TextBlock
	// and a noticeLevel; renderEntry renders it with the shared "▌ " accent bar colored
	// per level (see noticeLevel and styles.NoticeStyle).
	kindNotice
	// kindSubagent is the legacy collapsed activity line for a subagent loop:
	// one compact "▸ <agent>: <verb>" row attributing
	// the step to the agent driving that loop (the agent name learned from the loop's
	// LoopStarted, or the loopID short form when unknown/empty). It carries Agent +
	// Verb; renderEntry renders it via renderSubagentLine.
	kindSubagent
	// kindHarness is an out-of-band, faint status line the shell (not the model) emits —
	// e.g. "turn ran for 25s" committed when a loop's turn ends. It carries a
	// single TextBlock and renders as a hollow-circle "○ <text>" row in the faint,
	// out-of-focus status tone (renderHarnessLine), distinct from a leveled kindNotice
	// (no "▌ " accent bar).
	kindHarness
	// kindWorkflowActivity is one durable, session-global workflow notification. Each
	// activity is its own entry; continuation is represented by the entry's marker rather
	// than by a collapsible/grouped container.
	kindWorkflowActivity
)

// noticeLevel grades a kindNotice's severity, selecting its accent-bar color. The
// three levels share the SAME "▌ " wrapper (the user-message accent bar) and differ
// only in color: info is the neutral user-message gray, warn is yellow, error is red.
// It is an explicit enum (not a bool/string) so the renderer and styles map levels to
// colors exhaustively. The zero value is noticeInfo.
type noticeLevel uint8

const (
	noticeInfo noticeLevel = iota
	noticeWarn
	noticeError
)

// promptContext is the FULL AskUser payload a kindPromptRecord entry commits to
// scrollback when a user-input gate opens: the question + every choice. It is the
// append-only SCROLLBACK record — distinct from the interaction layer's compact
// bottom-box control (prompt in prompt.go), which carries selection state and is
// redrawn every frame. Permission gates do NOT use this: they surface as the
// "Approved …"/"Denied …" verb on their committed tool card, never as a record.
type promptContext struct {
	Question string   // the AskUser question
	Choices  []string // every offered choice, in order
	// Agent attributes the prompt to the SUBAGENT that opened it (design §6d Option B:
	// child gates surface as attributed records). It is the resolved label — the
	// subagent's agent name, or its loopID short form when the name is unknown/empty.
	// It is EMPTY for a owning-loop AskUser (the orchestrator's own question is not
	// agent-labeled); renderUserInputRecord prepends "<agent>: " only when it is set.
	Agent string
}

// entry is one committed (finalized) row of the transcript. It stores the minimal
// data needed to render the row later: its stable ID, its kind, and the content
// blocks captured when the live segment was committed. Calls holds the tool-call
// children of an assistant segment; it is left unpopulated in this task (tool-call
// reconstruction lands in a later task).
type entry struct {
	ID     displayID
	Kind   entryKind
	Blocks []content.Block
	Calls  []ToolCallView
	// Level grades a kindNotice entry's severity (info/warn/error), selecting its
	// accent-bar color. It is meaningful ONLY for kindNotice; every other kind leaves
	// it at the zero value (noticeInfo) and the renderer ignores it.
	Level noticeLevel
	// Prompt carries the FULL prompt context for a kindPromptRecord entry; it is
	// nil for every other kind. Kept as a pointer so non-prompt entries pay no
	// per-entry cost and a nil here is an unambiguous "not a prompt record".
	Prompt *promptContext
	// Agent is the attribution label of a kindSubagent line — the agent name driving the
	// producing subagent loop, or that loop's id short form when the name is unknown/
	// empty. Meaningful ONLY for kindSubagent; empty for every other kind.
	Agent string
	// Verb is the activity word of a kindSubagent line ("done" for a committed StepDone).
	// Meaningful ONLY for kindSubagent; empty for every other kind.
	Verb string
	// Workflow carries the safe metadata and marker state for a kindWorkflowActivity
	// entry. It is nil for every other kind. Workflow activities are session-global, so
	// their entries live in transcriptModel.global and appear in every loop projection.
	Workflow *workflowActivityEntry
	// thinkDur is the measured span of a kindAssistant entry's thinking block, captured from
	// the TokenDelta timestamps at commit (liveSeg.thinkDuration) and rendered as the
	// "│ thought for Nsec" header (formatThought). The harness never stamps Ephemeral
	// TokenDeltas, so the modern shell stamps each with its model clock (m.now) before the
	// reducer folds it — the timing source in live modern mode (see Screen.stampEphemeralClock).
	// It is LIVE-ONLY: a cold restore replays only persisted Enduring StepDone events, which
	// carry NO chunk timing, so a restored thinking entry legitimately has thinkDur == 0 and
	// renders the bare "│ thought" — the accepted restore behavior, NOT a bug. Because that makes a
	// live fold and a restore fold of the same session diverge on this field, it is
	// normalized OUT of the restore-equivalence comparison (EqualTranscript). Zero for every
	// non-assistant entry and for an assistant entry with no thinking block.
	thinkDur time.Duration
}

// workflowActivityEntry is the presentation-safe subset of event.WorkflowActivity. Raw
// event/run identifiers are retained only for reducer idempotency and marker state; the
// renderer never includes them in visible text.
type workflowActivityEntry struct {
	EventID           uuid.UUID
	RunID             uuid.UUID
	WorkflowName      string
	WorkflowVersion   string
	Kind              event.WorkflowActivityKind
	Status            event.WorkflowRunStatus
	VertexLabel       string
	CompletedVertices uint32
	TotalVertices     uint32
	Continuation      bool
}

// liveSeg is the in-progress assistant segment for the current turn: the streamed
// reasoning (Thinking) and narration (Text) plus the tool calls reconstructed from
// the event stream. It is committed to an entry when the turn ends. Calls stays
// empty until the event-reconstruction state machine populates it (a later task).
// active marks that a turn is in progress. It is the transcriptModel's own segment
// type — the single in-progress segment the scrollback-first path renders.
type liveSeg struct {
	Thinking string
	Text     string
	Calls    []ToolCallView
	active   bool
	// gateDecisions records, by gate ToolExecutionID, how each PERMISSION gate of the current
	// step was resolved: gatePending on PermissionRequested, then gateApproved/
	// gateDenied once Screen calls ResolveGate from the user's keypress. toolStarted
	// bakes the decision into its live card, so the committed card reads "Approved …" /
	// "Denied …". It is reset (to nil) at each StepDone with the rest of the segment.
	gateDecisions map[uuid.UUID]gateDecision
	// gateDescriptions records the same gate's human-readable permission body
	// (PermissionRequest.Description), so the committed card can show exactly what
	// was approved/denied rather than only the redacted audit summary.
	gateDescriptions map[uuid.UUID]string
	// thinkStart/thinkLast/thinkEnd measure the span of THIS segment's thinking from the
	// TokenDelta timestamps (ev.CreatedAt — which the modern shell stamps from its model clock,
	// since the harness leaves Ephemeral deltas unstamped), so a committed thinking entry can
	// show "thought for Nsec" (thinkDuration). The rule is DETERMINISTIC: thinkStart is the
	// FIRST ThinkingChunk's timestamp; thinkLast tracks the LAST ThinkingChunk's timestamp
	// (the fallback end when thinking is the last thing before StepDone); thinkEnd is SEALED
	// once — the FIRST non-thinking (Text) chunk that arrives after thinking began — marking
	// where reasoning gave way to narration. They are LIVE-ONLY (Ephemeral TokenDeltas are
	// never journaled, so a cold restore captures none) and are reset with the rest of the
	// segment at each StepDone / terminal, so they never affect restore-equivalence
	// (EqualTranscript normalizes them out defensively). A zero timestamp (an event with no
	// CreatedAt) never "sticks" as a start, so such a stream yields no duration.
	thinkStart time.Time
	thinkLast  time.Time
	thinkEnd   time.Time
}

// recordThinking folds one ThinkingChunk's timestamp into the segment's thinking span:
// the first non-zero timestamp seeds thinkStart, and every thinking chunk advances
// thinkLast (the fallback end). A zero timestamp is ignored for the start so a
// timestamp-less stream (a cold restore) yields no duration.
func (s *liveSeg) recordThinking(at time.Time) {
	if s.thinkStart.IsZero() {
		s.thinkStart = at
	}
	s.thinkLast = at
}

// recordNonThinking seals the thinking span's end at the FIRST non-thinking (Text) chunk
// that arrives after thinking began (thinkStart set, thinkEnd not yet sealed): that is
// where reasoning gave way to narration. A non-thinking chunk before any thinking, or a
// later one once sealed, is ignored.
func (s *liveSeg) recordNonThinking(at time.Time) {
	if !s.thinkStart.IsZero() && s.thinkEnd.IsZero() {
		s.thinkEnd = at
	}
}

// measuredFloor is the minimum span thinkDuration reports for thinking that WAS measured but
// whose computed span is non-positive — every ThinkingChunk (and the sealing narration chunk)
// landed inside a single model-clock tick, the common fast/first-turn case. It is a positive
// sentinel (below one second) so formatThought floors it to "thought for 1s" rather than the
// bare "thought" reserved for UNMEASURED thinking (thinkStart never set — a cold restore carries
// no Ephemeral timing). It must stay sub-second so it never inflates a real second count.
const measuredFloor = time.Nanosecond

// thinkDuration is the measured wall-clock span of this segment's thinking: end - start,
// where end is the sealed thinkEnd (first non-thinking chunk after thinking) or, when
// thinking ran up to StepDone with no narration after it, the last thinking chunk
// (thinkLast). It returns 0 ONLY when no thinking streamed (thinkStart zero) — the bare
// "thought" fallback. When thinking WAS measured but the span is non-positive (all chunks in
// one clock tick, or clock skew / out-of-order timestamps) it returns measuredFloor, a positive
// sentinel that renders "thought for 1s" so measured thinking is never shown as timeless.
func (s liveSeg) thinkDuration() time.Duration {
	if s.thinkStart.IsZero() {
		return 0
	}
	end := s.thinkEnd
	if end.IsZero() {
		end = s.thinkLast
	}
	if d := end.Sub(s.thinkStart); d > 0 {
		return d
	}
	return measuredFloor
}

// applyChunk routes one streamed chunk into this segment: text accumulates into Text,
// thinking into Thinking, and the event's CreatedAt (at) folds into the thinking span
// (recordThinking / recordNonThinking) so a committed thinking entry can show its "Thought
// for Ns" duration. Any other chunk variant (e.g. a tool-use chunk) is skipped — tool-call
// reconstruction is a later task. It is the SINGLE chunk-fold implementation shared by the
// loop projection (m.fold.live.applyChunk) and every per-loop projection (p.live.applyChunk), so a
// focused subagent view can never drift from the root.
func (s *liveSeg) applyChunk(c content.Chunk, at time.Time) {
	switch chunk := c.(type) {
	case *content.TextChunk:
		s.Text += chunk.Text
		s.recordNonThinking(at)
	case *content.ThinkingChunk:
		s.Thinking += chunk.Thinking
		s.recordThinking(at)
	}
}

// empty reports whether the live segment carries no committable content — no
// streamed reasoning, no streamed narration, and no reconstructed tool call.
// active is intentionally not consulted: an active-but-content-less segment is
// still empty and must not commit.
func (s liveSeg) empty() bool {
	return s.Thinking == "" && s.Text == "" && len(s.Calls) == 0
}

// queuedInput is a transient affordance for one submitted-but-not-yet-committed
// user message: its submit correlation id, the loop it was queued for (stamped from
// the InputQueued event so the modern per-loop view never shows a subagent's queue
// under another loop), the blocks the TUI remembers from the submit (InputQueued
// carries no Message, so the affordance text comes from here), and a shown flag the
// loop's InputQueued event flips on. It is NOT a committed transcript entry — it is a
// pending hint rendered below the live tail until the authoritative
// TurnStarted/TurnFoldedInto commits the real user row (or InputCancelled/TurnRejected
// drops it). loopID is authoritative only once shown is set (markQueued stamps both
// from the same InputQueued event); a submit-first placeholder carries a zero loopID
// until its InputQueued arrives, and QueuedInputs/QueuedInputsFor skip a still-unshown
// entry, so a rendered affordance always has its loop.
type queuedInput struct {
	inputID uuid.UUID
	loopID  uuid.UUID
	blocks  []content.Block
	shown   bool
}

// loopProjection is one loop's own committed + live stream, folded from that
// loop's events so modern mode can render any focused loop's whole transcript
// independently of the loop projection (design §Per-loop projections). committed holds
// the loop's finalized rows; live is its in-progress segment. Its entries draw from the
// model's SINGLE nextID allocator (never a per-projection counter), so displayIDs are
// globally unique across every projection and the loop projection — a later collapse
// map keyed by displayID cannot collide.
type loopProjection struct {
	committed []entry
	live      liveSeg
}

// transcriptModel is the pure, side-effect-free reducer over the session event
// stream. Global entries are visible from every loop projection; queued holds pending
// queued-input affordances (ordered by submit); nextID is the next stable ID to
// allocate. It is applied by value: ApplyEvent returns the next model.
type transcriptModel struct {
	global []entry
	queued []queuedInput
	nextID displayID
	// fold is a call-local reducer target installed only while ApplyEvent folds one
	// loop event. ApplyEvent clears it before returning, so no loop becomes hidden
	// current/root state in the persisted transcript model.
	fold *loopProjection
	// loopAgents maps a loop id to its PRESENTATION LABEL, learned from each loop's LoopStarted
	// as those Enduring/all-loops events arrive on the TUI's lifetime subscription. The label is
	// the loop's DisplayName when non-empty, else its Header.AgentName (see loopStartedLabel) —
	// stored as an identity.AgentName for pragmatic reuse (no new label type; the value is the
	// display label, not necessarily the raw agent name). It is the source of a subagent's
	// attribution label (the "▸ <agent>: done" collapsed StepDone line and an attributed AskUser
	// record). A loop absent from the map (LoopStarted not yet seen) or mapped to an empty label
	// (a legacy/no-name loop) falls back to the loopID short form — see agentLabel. It stays
	// keyed by LoopID and is cloned on write so the by-value reducer never aliases a prior
	// model's map.
	loopAgents map[uuid.UUID]identity.AgentName
	// loopLive maps a loop id to its liveness bit — true between the loop's LoopStarted /
	// turn activity and its LoopIdle, false once it parks idle (design §Loop lifecycle &
	// liveness). It is BI-STATE (live | idle): subagent loops are never deleted and there is
	// NO loop-exited event, so a loop never leaves this map — it only toggles. It is the
	// SINGLE source of loop liveness (no parallel registry that could drift); the modern
	// active-loops bar reads it via loops(). It is cloned on write alongside loopOrder
	// (value-copy contract), mirroring loopAgents / recordLoopAgent.
	loopLive map[uuid.UUID]bool
	// loopOrder is the stable creation order of the loop ids (appended on a loop's FIRST
	// sight in recordLoopLive), so loops() enumerates the session's loops deterministically
	// (map iteration order is unspecified). It is the companion of loopLive — a loop id is in
	// loopOrder iff it is in loopLive (recordLoopLive is the sole writer of both). It is
	// cloned on write — appended to a fresh slice, never mutated in a shared backing array —
	// mirroring accumOrder.
	loopOrder []uuid.UUID
	// loopParent maps a SUBAGENT child loop id to its spawn key — the parent loop/turn/
	// step coordinates plus the durable provider tool-use id of the Subagent call that
	// spawned it — recorded from a child LoopStarted whose ParentToolUseID is non-empty
	// (design §3). A child loop NOT in this map (empty/absent ParentToolUseID, e.g. a
	// non-tool spawn) keeps the legacy collapsed "▸ <agent>: done" fallback line. It is
	// cloned on write (value-copy contract).
	loopParent map[uuid.UUID]spawnKey
	// subagentAccum holds, per spawn key, the detached accumulator a child's ENDURING
	// events build up (design §1/§3): agent, task, nested children, step count, terminal
	// status. It is DETACHED (not state on a card) because the child's events precede the
	// parent's StepDone (the parent card does not exist yet) and a cold restore rebuilds
	// it from scratch. The orchestrator's StepDone reconciles each Subagent block against
	// it by spawn key. It is cloned on write (value-copy contract).
	subagentAccum map[spawnKey]*subagentAccumulator
	// accumOrder is the stable creation order of the subagentAccum keys (appended when an
	// accumulator is first created in ensureAccum), so pendingSubagentCards renders the
	// in-flight nested cards deterministically (map iteration order is unspecified). It is
	// cloned on write (value-copy contract) — appended to a fresh slice, never mutated in
	// a shared backing array — mirroring the map clone-on-write alongside it.
	accumOrder []spawnKey
	// projections holds every loop's committed+live stream. There is no privileged root
	// alias: event LoopID selects the projection and projectionFor reads that same key.
	// It is cloned on write
	// (value-copy contract), mirroring subagentAccum: a freshly created projection is
	// freshly allocated so a prior model never sees it, and in-place fold into the pointer
	// is the intended write path within one linear reducer chain.
	projections map[uuid.UUID]*loopProjection
	// compactionCompletions owns display idempotency for durable compaction success rows.
	// Event IDs are session-global and the map is cloned on write so replay, live delivery,
	// and restore folding all share one value-safe deduplication rule.
	compactionCompletions map[uuid.UUID]struct{}
	// workflowActivityIDs deduplicates durable workflow notifications by their stable event
	// identity. It is deliberately separate from the Screen restore barrier's event-ID set:
	// direct reducer folds and live/restore handoff both need the same idempotency behavior.
	workflowActivityIDs map[uuid.UUID]struct{}
	// workflowRuns tracks whether the latest activity for a run left that run active. A
	// false/missing value starts a new blue solid marker; an active value produces the blue
	// continuation rail until a terminal activity closes it.
	workflowRuns map[uuid.UUID]bool
}

// spawnKey identifies one Subagent tool call's spawn: the parent loop/turn/step
// coordinates (where the Subagent block lives, read from the orchestrator's StepDone
// header) plus the durable provider tool-use id of that block. It is built identically
// on the two sides that must meet — the child LoopStarted (Cause.* + ParentToolUseID)
// and the orchestrator StepDone (its own LoopID/TurnID/StepID + block.ID) — so the
// accumulator a child fills is the one the parent card looks up.
type spawnKey struct {
	parentLoopID uuid.UUID
	parentTurnID uuid.UUID
	parentStepID uuid.UUID
	toolUseID    string
}

// subagentAccumulator is the detached, per-spawn build-up of a subagent's nested card,
// assembled PURELY from the child's ENDURING events (LoopStarted→agent, first
// TurnStarted→task, each StepDone→a child card + steps++, the terminal→status). The
// orchestrator's StepDone copies these onto the committed Subagent ToolCallView (design
// §3), and later child events refresh that card through committedID. It is a pointer in
// the map so an in-place field bump (steps++, append a child)
// is visible without re-storing — but the MAP is cloned on write so a by-value reducer
// never aliases a prior model's map; a freshly-cloned map's pointers are themselves
// freshly allocated when first created here (see ensureAccum).
type subagentAccumulator struct {
	agent    string         // the child LoopStarted.AgentName
	task     string         // the child's first TurnStarted.Message, truncated
	children []ToolCallView // nested tool cards from the child's StepDone groups
	steps    int            // count of the child's StepDone events
	status   subStatus      // the child terminal state (running until a terminal arrives)
	nested   int            // depth-2 collapsed counter (Task 7; zero here)
	// reconciled marks that the orchestrator's StepDone has copied this accumulator onto
	// its committed Subagent card (reconcileSubagent). Until then the accumulator is
	// PENDING — pendingSubagentCards exposes it so the LIVE tail renders the in-flight
	// nested card; once reconciled the card lives in scrollback and pendingSubagentCards
	// skips it (no double render). It is set in-place at reconciliation, consistent with
	// the rest of the accumulator's in-place fill.
	reconciled bool
	// committedID identifies the parent projection entry that owns the reconciled card,
	// allowing later cross-loop child events to update that single card in place.
	committedID displayID
}

// ApplyEvent folds one turn-stream event into the model and returns the next
// model. LoopStarted records the producing loop's agent name (LoopID→AgentName) for
// later subagent attribution, marks the loop LIVE, and appends it to the ordered loop
// table on first sight (recordLoopLive); it commits nothing. LoopIdle marks the loop
// IDLE (bi-state — there is no loop-exited event). TurnStarted begins/keeps a live
// assistant segment, marks the loop LIVE (activity implies live), AND — for GENUINE user
// input only (Header.Cause.LoopID == 0; a subagent hand-back carries a
// non-zero one and commits NO user row) — commits the authoritative user row from
// its Message and drops the matching queued affordance. TurnFoldedInto does the
// same user-row commit for a folded tool-continuation input. InputQueued reveals
// the queued affordance for its InputID; InputCancelled drops it (no row);
// TurnRejected drops it and commits an error notice (a rejected message must not
// silently vanish). TokenDelta routes
// *content.TextChunk → live.Text and *content.ThinkingChunk → live.Thinking as a
// PROVISIONAL live render; ToolCallStarted/ToolCallCompleted drive the live tool
// cards (in the live tail only — they are not committed to scrollback here). These
// Ephemeral folds are projection-local: EventHeader.LoopID selects the only projection
// mutated, even though AllLoopsEventFilter delivers every loop's stream.
//
// StepDone is the authoritative commit point and the self-heal anchor: it SNAPS the
// transcript to the loop's finalized StepDone.Messages (the step's AIMessage + its
// ToolResultMessages), committing that group as separate entries and discarding the
// provisional live segment — so a dropped/partial TokenDelta never survives past the
// step boundary, and the displayed transcript equals the committed transcript by
// construction. A multi-step turn therefore renders as multiple separate assistant +
// tool entries, never one merged entry.
//
// TurnDone is a lifecycle terminal: every completed step already committed via its
// StepDone, so it only flushes any leftover provisional live (defensive) and resets.
// PermissionRequested only REMEMBERS the gate (by ToolExecutionID) so the call's committed card
// can read "Approved …" / "Denied …" once Screen reports the keypress via ResolveGate;
// it commits nothing (the permission shows on the tool card, not a separate record). This
// gate recording is projection-local; the interaction model still enqueues the prompt
// for every loop.
// UserInputRequested (AskUser is not a tool) commits ONLY the prompt record. Neither
// commits pending prose — the provisional live prose stays live and commits exactly
// once via StepDone (no duplicate) — and neither resets the live segment, so the turn
// continues while the gate is pending.
// TurnInterrupted/TurnFailed are the abnormal terminals: the in-flight INCOMPLETE step
// never emitted a StepDone, so its provisional live is committed (partial work stays
// visible) before the tombstone/error. It returns ONLY the next transcriptModel — no
// uiAction; prompt clearing on terminals and active-surface control are the
// interactionModel's job, not the transcript's.
func (m transcriptModel) ApplyEvent(ev event.Event) transcriptModel {
	switch ev := ev.(type) {
	case event.WorkflowActivity:
		return m.applyWorkflowActivity(ev)
	case *event.WorkflowActivity:
		if ev == nil {
			return m
		}
		return m.applyWorkflowActivity(*ev)
	}

	loopID := ev.EventHeader().LoopID
	switch ev := ev.(type) {
	case event.LoopStarted:
		m.recordLoopAgent(ev.LoopID, loopStartedLabel(ev))
		m.recordLoopLive(ev.LoopID, true)
		m.loopSpawned(ev)
	case event.LoopIdle:
		m.recordLoopLive(ev.LoopID, false)
	case event.InputQueued:
		m.markQueued(ev.Cause.CommandID, ev.LoopID)
	case event.InputCancelled:
		m.dropQueued(ev.Cause.CommandID)
	case event.CompactionCommitted:
		m = m.commitCompactionCompletion(ev)
	}
	p := m.ensureProjection(loopID)
	m.fold = p
	switch ev := ev.(type) {
	case event.TurnStarted:
		m.fold.live.active = true
		m.recordLoopLive(ev.LoopID, true)
		m.subagentTask(ev.LoopID, ev.Message)
		m.startTurnUser(ev.Cause.LoopID, ev.Cause.CommandID, ev.Message)
	case event.TurnFoldedInto:
		m.startTurnUser(ev.Cause.LoopID, ev.Cause.CommandID, ev.Message)
	case event.TurnRejected:
		m.rejectInput(ev.Cause.CommandID, ev.Reason)
	case event.TokenDelta:
		m.fold.live.applyChunk(ev.Chunk, ev.EventHeader().CreatedAt)
	case event.ToolCallStarted:
		m.toolStarted(ev)
	case event.ToolCallCompleted:
		m.toolCompleted(ev)
	case event.StepDone:
		if _, child := m.loopParent[ev.LoopID]; child {
			m.subagentStep(ev)
		}
		m.stepDone(ev)
	case event.PermissionRequested:
		m.permissionRequested(ev)
	case event.UserInputRequested:
		m.userInputRequested(ev)
	case event.TurnDone:
		m.subagentTerminal(ev.LoopID, subDone)
		m.clearLoopAccums(ev.LoopID)
		m.commitLive()
	case event.TurnInterrupted:
		m.subagentTerminal(ev.LoopID, subInterrupted)
		m.clearLoopAccums(ev.LoopID)
		m.turnInterrupted()
	case event.TurnFailed:
		m.subagentTerminal(ev.LoopID, subFailed)
		m.clearLoopAccums(ev.LoopID)
		m.turnFailed(ev)
	}
	m.fold = nil
	return m
}

// applyWorkflowActivity appends one session-global activity row. The durable EventID is
// the reducer's idempotency key, so a duplicate replay cannot create a second visible row.
// The run-state map is updated only after the row is materialized: a terminal activity may
// itself use the continuation marker, but the next activity for that run starts a new rail.
func (m transcriptModel) applyWorkflowActivity(ev event.WorkflowActivity) transcriptModel {
	if id := ev.EventID; !id.IsZero() {
		if _, duplicate := m.workflowActivityIDs[id]; duplicate {
			return m
		}
		ids := cloneWorkflowActivityIDs(m.workflowActivityIDs)
		ids[id] = struct{}{}
		m.workflowActivityIDs = ids
	}

	continuation := m.workflowRuns[ev.RunID]
	activity := &workflowActivityEntry{
		EventID:           ev.EventID,
		RunID:             ev.RunID,
		WorkflowName:      ev.WorkflowName,
		WorkflowVersion:   ev.WorkflowVersion,
		Kind:              ev.Kind,
		Status:            ev.Status,
		VertexLabel:       ev.VertexLabel,
		CompletedVertices: ev.CompletedVertices,
		TotalVertices:     ev.TotalVertices,
		Continuation:      continuation,
	}
	m.nextID++
	e := entry{
		ID:       m.nextID,
		Kind:     kindWorkflowActivity,
		Blocks:   []content.Block{&content.TextBlock{Text: workflowActivityText(*activity)}},
		Workflow: activity,
	}
	m.global = append(append([]entry(nil), m.global...), e)

	runs := cloneWorkflowRunStates(m.workflowRuns)
	if workflowActivityTerminal(ev) {
		delete(runs, ev.RunID)
	} else {
		runs[ev.RunID] = true
	}
	m.workflowRuns = runs
	return m
}

func cloneWorkflowActivityIDs(src map[uuid.UUID]struct{}) map[uuid.UUID]struct{} {
	dst := make(map[uuid.UUID]struct{}, len(src)+1)
	for id := range src {
		dst[id] = struct{}{}
	}
	return dst
}

func cloneWorkflowRunStates(src map[uuid.UUID]bool) map[uuid.UUID]bool {
	dst := make(map[uuid.UUID]bool, len(src)+1)
	for id, active := range src {
		dst[id] = active
	}
	return dst
}

func workflowActivityTerminal(ev event.WorkflowActivity) bool {
	switch ev.Kind {
	case event.WorkflowActivityRunInterrupted,
		event.WorkflowActivityRunCompleted,
		event.WorkflowActivityRunCancelled,
		event.WorkflowActivityRunFailed:
		return true
	default:
		return ev.Status != event.WorkflowRunStatusRunning
	}
}

func workflowActivityText(a workflowActivityEntry) string {
	name := a.WorkflowName
	if a.WorkflowVersion != "" {
		name += "@" + a.WorkflowVersion
	}
	parts := []string{name, string(a.Kind), string(a.Status)}
	if a.VertexLabel != "" {
		parts = append(parts, a.VertexLabel)
	}
	if a.TotalVertices > 0 {
		parts = append(parts, fmt.Sprintf("%d/%d", a.CompletedVertices, a.TotalVertices))
	}
	return strings.Join(parts, " · ")
}

func (m transcriptModel) commitCompactionCompletion(ev event.CompactionCommitted) transcriptModel {
	eventID := ev.EventHeader().EventID
	if eventID.IsZero() {
		return m
	}
	if _, duplicate := m.compactionCompletions[eventID]; duplicate {
		return m
	}
	m.compactionCompletions = cloneCompactionCompletionIDs(m.compactionCompletions)
	m.compactionCompletions[eventID] = struct{}{}
	m = m.detachCompactionCompletionProjection(ev.EventHeader().LoopID)
	return m.CommitHarnessFor(ev.EventHeader().LoopID, "conversation compacted in "+formatElapsed(ev.Duration))
}

// detachCompactionCompletionProjection gives the next reducer value ownership of the
// target projection before CommitHarnessFor appends its row. Other loop projections remain
// shared and untouched; an absent target is left for CommitHarnessFor to create normally.
func (m transcriptModel) detachCompactionCompletionProjection(loopID uuid.UUID) transcriptModel {
	projection, ok := m.projections[loopID]
	if !ok || projection == nil {
		return m
	}
	next := make(map[uuid.UUID]*loopProjection, len(m.projections))
	for id, existing := range m.projections {
		next[id] = existing
	}
	cloned := *projection
	cloned.committed = append([]entry(nil), projection.committed...)
	next[loopID] = &cloned
	m.projections = next
	return m
}

func cloneCompactionCompletionIDs(in map[uuid.UUID]struct{}) map[uuid.UUID]struct{} {
	out := make(map[uuid.UUID]struct{}, len(in)+1)
	for id := range in {
		out[id] = struct{}{}
	}
	return out
}

// loopStartedLabel resolves the presentation label a LoopStarted contributes to the loop
// table: the loop's user-facing DisplayName when non-empty, else the Header.AgentName (older
// journals carry no DisplayName, so consumers fall back to the agent name — see
// event.LoopStarted.DisplayName). It is stored as the loop's identity.AgentName label so the
// existing keyed-by-LoopID map, attribution line, and bar rendering are all unchanged; only the
// SOURCE of the label shifts from the raw agent name to the display name. A fully empty result
// (both empty) still resolves to the loopID short form downstream via agentLabel.
func loopStartedLabel(ev event.LoopStarted) identity.AgentName {
	if ev.DisplayName != "" {
		return identity.AgentName(ev.DisplayName)
	}
	return ev.AgentName
}

// recordLoopAgent records loopID's PRESENTATION LABEL (the LoopStarted boundary), so a later
// subagent StepDone/AskUser from that loop can be attributed. The label is resolved by the
// caller via loopStartedLabel — the loop's DisplayName when non-empty, else its Header.AgentName
// — and stored as an identity.AgentName (keyed by LoopID). The map is cloned on write
// (value-copy contract) so the by-value reducer never aliases a prior model's map. A re-seen
// loop id overwrites — the label is immutable, so this is idempotent in practice.
func (m *transcriptModel) recordLoopAgent(loopID uuid.UUID, name identity.AgentName) {
	next := make(map[uuid.UUID]identity.AgentName, len(m.loopAgents)+1)
	for k, v := range m.loopAgents {
		next[k] = v
	}
	next[loopID] = name
	m.loopAgents = next
}

// recordLoopLive sets loopID's liveness bit (LoopStarted / turn activity → true, LoopIdle
// → false) and appends loopID to loopOrder on its FIRST sight, so loops() enumerates the
// session's loops in stable creation order. Liveness is BI-STATE (live | idle) — there is
// no loop-exited event, so a loop is only ever toggled, never removed. Both loopLive (a
// cloned map) and loopOrder (a cloned slice) are written value-copy so the by-value reducer
// never aliases a prior model's state, mirroring recordLoopAgent and accumOrder. This is
// the SOLE writer of both, so a loop is in loopOrder iff it is in loopLive.
func (m *transcriptModel) recordLoopLive(loopID uuid.UUID, live bool) {
	_, seen := m.loopLive[loopID]
	next := make(map[uuid.UUID]bool, len(m.loopLive)+1)
	for k, v := range m.loopLive {
		next[k] = v
	}
	next[loopID] = live
	m.loopLive = next
	if !seen {
		m.loopOrder = append(append([]uuid.UUID(nil), m.loopOrder...), loopID)
	}
}

// agentLabel resolves the attribution label for loopID: the agent name learned from
// its LoopStarted when known and non-empty, else the loopID short form (loopShortForm)
// — never an empty string, so a label is always shown and a missing/legacy name never
// renders a dangling "▸ :". It is the single resolver shared by the subagent StepDone
// line and the attributed AskUser record.
func (m transcriptModel) agentLabel(loopID uuid.UUID) string {
	if name, ok := m.loopAgents[loopID]; ok && name != "" {
		return string(name)
	}
	return loopShortForm(loopID)
}

// loopShortForm is the loopID fallback label: the first hyphen-delimited group of the
// canonical uuid string (its 8 leading hex chars), a compact stable id for a loop with
// no known/empty agent name. The zero loop id yields "00000000" (still a non-empty,
// unambiguous label).
func loopShortForm(loopID uuid.UUID) string {
	s := loopID.String()
	if i := strings.IndexByte(s, '-'); i >= 0 {
		return s[:i]
	}
	return s
}

// loopInfo is one loop's entry in the ordered loop table exposed by loops(): its id, its
// resolved display name (agentLabel — the agent name, or the loopID short form when
// unknown/empty), and its bi-state liveness. It is the shape Task 7 maps into the modern
// active-loops bar's entries; the transcript's loop table is the SINGLE source of both.
type loopInfo struct {
	ID   uuid.UUID
	Name string
	Live bool
}

// loops returns the session's loops in stable creation order (loopOrder), each carrying its
// resolved name (agentLabel) and liveness (loopLive). It is the ordered accessor the modern
// active-loops bar renders from — a READ over the EXISTING loop table, never a second
// registry. An unseen loop (no lifecycle event yet) does not appear; a loop with no known
// agent name falls back to its loopID short form via agentLabel. The returned slice is
// freshly built, so a caller cannot reach the model's internal order.
func (m transcriptModel) loops() []loopInfo {
	out := make([]loopInfo, 0, len(m.loopOrder))
	for _, id := range m.loopOrder {
		out = append(out, loopInfo{ID: id, Name: m.agentLabel(id), Live: m.loopLive[id]})
	}
	return out
}

// CommitUser appends the user's submitted message as one kindUser entry with a
// fresh stable ID and returns the next model. Its authoritative caller is
// startTurnUser, which passes the loop's event Message.Blocks (the stored user
// message) — NOT the submit-built blocks, which now only feed the queued affordance.
// It does NOT touch the live segment: a message folded mid-turn must land in
// scrollback without truncating the in-progress assistant output. An empty Blocks
// slice still commits one entry — emptiness is rejected upstream at the input
// boundary, not here.
func (m transcriptModel) CommitUser(blocks []content.Block) transcriptModel {
	m.nextID++
	e := entry{ID: m.nextID, Kind: kindUser, Blocks: blocks}
	if m.fold == nil {
		m.global = append(m.global, e)
	} else {
		m.fold.committed = append(m.fold.committed, e)
	}
	return m
}

// CommitUserText commits a plain-text user row (no attachments). It is for the
// submit-FAILED path: when buildBlocks rejects a message (e.g. an image on a text-only
// model) the message is shown in scrollback as the user's row — even though it was
// never sent to the model — so the user sees what they asked alongside the error.
func (m transcriptModel) CommitUserText(text string) transcriptModel {
	return m.CommitUser([]content.Block{&content.TextBlock{Text: text}})
}

// CommitHarnessFor appends a loop-scoped harness notice to that loop's projection.
func (m transcriptModel) CommitHarnessFor(loopID uuid.UUID, text string) transcriptModel {
	p := m.ensureProjection(loopID)
	m.nextID++
	p.committed = append(p.committed, entry{ID: m.nextID, Kind: kindHarness, Blocks: []content.Block{&content.TextBlock{Text: text}}})
	return m
}

func (m transcriptModel) CommitGlobalUserText(text string) transcriptModel {
	m.nextID++
	e := entry{ID: m.nextID, Kind: kindUser, Blocks: []content.Block{&content.TextBlock{Text: text}}}
	m.global = append(m.global, e)
	return m
}

func (m transcriptModel) CommitGlobalNotice(level noticeLevel, text string) transcriptModel {
	m.nextID++
	e := entry{ID: m.nextID, Kind: kindNotice, Level: level, Blocks: []content.Block{&content.TextBlock{Text: text}}}
	m.global = append(m.global, e)
	return m
}

func (m transcriptModel) CommitGlobalError(err error) transcriptModel {
	msg := "unknown error"
	if err != nil {
		msg = err.Error()
	}
	return m.CommitGlobalNotice(noticeError, msg)
}

// RecordSubmit registers a fire-and-forget submit by its correlation id so the
// queued affordance can show the remembered blocks once the loop's InputQueued
// event arrives. The remembered blocks are DISPLAY-ONLY and assumed immutable after
// submit (the committed row comes from the event's authoritative Message, not these);
// callers must not mutate the slice they pass. It is called by the Screen on a
// successful submitResultMsg. If an
// entry for inputID already exists (the InputQueued event raced ahead of the
// submit result), this FILLS its blocks rather than appending a duplicate — so a
// shown-but-blockless placeholder gets its text. Otherwise it appends a fresh
// queuedInput (shown=false) at the submit-order tail. It returns the next model.
//
// Value-copy contract: a fresh queued slice is built (never an in-place mutation
// of a shared backing array) so the by-value reducer never aliases a prior model's
// queue — mirroring the interaction model's cloneHead rationale.
func (m transcriptModel) RecordSubmit(inputID uuid.UUID, blocks []content.Block) transcriptModel {
	next := append([]queuedInput(nil), m.queued...)
	for i := range next {
		if next[i].inputID == inputID {
			next[i].blocks = blocks
			m.queued = next
			return m
		}
	}
	m.queued = append(next, queuedInput{inputID: inputID, blocks: blocks})
	return m
}

// QueuedInputs returns, in submit order, the blocks of every queued affordance
// that is ready to render (shown by an InputQueued event AND carrying remembered
// blocks). A still-blockless placeholder (InputQueued arrived before RecordSubmit
// filled the blocks) is skipped until its blocks land. The returned slice is a
// fresh copy, so a caller cannot reach the model's internal queue.
func (m transcriptModel) QueuedInputs() [][]content.Block {
	var out [][]content.Block
	for _, q := range m.queued {
		if q.shown && q.blocks != nil {
			out = append(out, q.blocks)
		}
	}
	return out
}

// QueuedInputsFor is QueuedInputs scoped to a single loop: it returns, in submit order,
// the blocks of every ready queued affordance (shown AND carrying remembered blocks)
// whose target loop is loopID. It is the modern viewport's per-loop queue read — a
// submit while focused on a subagent queues onto THAT loop (Stage 2), so its affordance
// must show under the subagent's view and not leak under another loop. The returned slice is a fresh copy, so a
// caller cannot reach the model's internal queue.
func (m transcriptModel) QueuedInputsFor(loopID uuid.UUID) [][]content.Block {
	var out [][]content.Block
	for _, q := range m.queued {
		if q.shown && q.blocks != nil && q.loopID == loopID {
			out = append(out, q.blocks)
		}
	}
	return out
}

// startTurnUser commits the authoritative user row for a turn-start event
// (TurnStarted/TurnFoldedInto) and drops the matching queued affordance. It
// commits a kindUser row only when triggeredBy is zero and a Message is present.
// A SubagentResult hand-back has a non-zero Cause.LoopID and is not user input.
// The row is committed from the event's
// authoritative blocks, never from remembered submit state, which sidesteps the
// submit↔event arrival race. The queued affordance for this InputID is always
// dropped (the real row, if any, supersedes it).
func (m *transcriptModel) startTurnUser(triggeredBy, inputID uuid.UUID, msg *content.UserMessage) {
	if triggeredBy.IsZero() && msg != nil {
		*m = m.CommitUser(msg.Blocks)
	}
	m.dropQueued(inputID)
}

// markQueued reveals the queued affordance for inputID (InputQueued boundary) and
// stamps the loop it was queued for (loopID, the InputQueued event's producing loop)
// so the per-loop modern view can scope its queue. If no entry exists yet (InputQueued
// raced ahead of RecordSubmit) it creates a shown-but-blockless placeholder — already
// carrying its loopID — so the affordance appears the instant the remembered blocks
// land via RecordSubmit; until then QueuedInputs/QueuedInputsFor skip it. It rebuilds
// the slice rather than mutating a shared backing array (value-copy contract).
func (m *transcriptModel) markQueued(inputID, loopID uuid.UUID) {
	next := append([]queuedInput(nil), m.queued...)
	for i := range next {
		if next[i].inputID == inputID {
			next[i].shown = true
			next[i].loopID = loopID
			m.queued = next
			return
		}
	}
	m.queued = append(next, queuedInput{inputID: inputID, loopID: loopID, shown: true})
}

// dropQueued removes the queued affordance for inputID, if present. It rebuilds the
// slice (value-copy contract) so the reducer never mutates a prior model's queue.
// An unknown inputID is a no-op.
func (m *transcriptModel) dropQueued(inputID uuid.UUID) {
	if len(m.queued) == 0 {
		return
	}
	next := make([]queuedInput, 0, len(m.queued))
	for _, q := range m.queued {
		if q.inputID != inputID {
			next = append(next, q)
		}
	}
	m.queued = next
}

// rejectInput is the TurnRejected boundary: a submitted message the loop refused
// must not silently vanish. It drops the queued affordance for inputID and commits
// an error-level notice naming the reason, so the user sees the rejection.
func (m *transcriptModel) rejectInput(inputID uuid.UUID, reason event.RejectReason) {
	m.dropQueued(inputID)
	*m = m.CommitNotice(noticeError, "input rejected: "+rejectReasonText(reason))
}

// rejectReasonText maps a RejectReason to a short user-facing phrase. The
// zero-value sentinel (RejectUnspecified) and any unknown value degrade to a
// neutral "refused" rather than printing a raw enum number.
func rejectReasonText(reason event.RejectReason) string {
	switch reason {
	case event.RejectQueueFull:
		return "queue full"
	case event.RejectShuttingDown:
		return "shutting down"
	case event.RejectInternal:
		return "internal error"
	default:
		return "refused"
	}
}

// CommitNotice appends a leveled, out-of-band notification as one kindNotice entry
// carrying level + text with a fresh stable ID, and returns the next model. It is the
// single notice-commit primitive — the startup banner and /help (info), and the
// error paths (error) all route through it. It does NOT touch the live segment: a
// notice is out-of-band from the assistant's in-progress output. An empty text still
// commits one entry (the bar marks the event).
func (m transcriptModel) CommitNotice(level noticeLevel, text string) transcriptModel {
	m.nextID++
	m.appendCommitted(entry{
		ID:     m.nextID,
		Kind:   kindNotice,
		Level:  level,
		Blocks: []content.Block{&content.TextBlock{Text: text}},
	})
	return m
}

// CommitSystem appends an info-level notice (e.g. the /help listing). It is a thin
// wrapper over CommitNotice(noticeInfo, …) — a system notice IS an info notice.
func (m transcriptModel) CommitSystem(text string) transcriptModel {
	return m.CommitNotice(noticeInfo, text)
}

// CommitHarness appends one kindHarness status line carrying text with a fresh stable ID,
// and returns the next model. It is the shell-emitted, out-of-band status primitive (e.g.
// the "turn ran for 25s" line committed when a loop's turn ends). Like a notice it
// does NOT touch the live segment; unlike a notice it renders as a faint "○ <text>" row (no
// leveled accent bar). An empty text still commits one entry.
func (m transcriptModel) CommitHarness(text string) transcriptModel {
	m.nextID++
	m.appendCommitted(entry{
		ID:     m.nextID,
		Kind:   kindHarness,
		Blocks: []content.Block{&content.TextBlock{Text: text}},
	})
	return m
}

// appendCommitted writes an event-folded row to that event's loop projection and an
// out-of-band row to the global stream. ApplyEvent is the only owner of fold.
func (m *transcriptModel) appendCommitted(e entry) {
	if m.fold == nil {
		m.global = append(m.global, e)
		return
	}
	m.fold.committed = append(m.fold.committed, e)
}

// CommitError appends an error-level notice for a non-fatal failure. It is the
// out-of-band error path — distinct from a turn failure's terminal notice
// (turnFailed) — used by Screen for submit/dispatch/reopen failures that must be
// surfaced without ending a turn. A nil err commits an empty message (the entry
// still marks the failure). It is a thin wrapper over CommitNotice(noticeError, …).
func (m transcriptModel) CommitError(err error) transcriptModel {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	return m.CommitNotice(noticeError, msg)
}

// permissionRequested is the permission-gate boundary: it REMEMBERS the gate by its
// ToolExecutionID (decision gatePending) so the call's committed card can read "Approved …" /
// "Denied …" once Screen reports the user's keypress via ResolveGate. It commits
// NOTHING — the permission shows on the tool card itself (the verb + the ✓/✗ glyph),
// not as a separate record — and it does NOT commit pending live prose (the
// provisional narration stays live and commits once at StepDone). live is NOT reset:
// the turn continues while the gate is pending. The map is cloned on write so the
// value-copy reducer never aliases a prior model's gate map.
func (m *transcriptModel) permissionRequested(ev event.PermissionRequested) {
	g := cloneGates(m.fold.live.gateDecisions)
	g[ev.ToolExecutionID] = gatePending
	m.fold.live.gateDecisions = g
	if summary := permissionRequestSummary(ev.Request); summary != "" {
		d := cloneGateDescriptions(m.fold.live.gateDescriptions)
		d[ev.ToolExecutionID] = summary
		m.fold.live.gateDescriptions = d
	}
}

// ResolveGate records the user's decision for a pending permission gate (loopID, callID),
// the source the loop never emits as an event — Screen calls it from the approve/deny
// keypress. An unknown callID (no matching pending gate) is a no-op. The map is cloned
// on write (value-copy contract). It returns the next model.
func (m transcriptModel) ResolveGate(loopID, callID uuid.UUID, decision gateDecision) transcriptModel {
	projection, ok := m.projections[loopID]
	if !ok || projection == nil {
		return m
	}
	if _, ok := projection.live.gateDecisions[callID]; !ok {
		return m
	}
	p := m.ensureProjection(loopID)
	g := cloneGates(p.live.gateDecisions)
	g[callID] = decision
	p.live.gateDecisions = g
	return m
}

// cloneGates returns a fresh copy of a gate-decision map (nil-safe), so a by-value
// reducer mutation never writes through a map a prior model still holds — the map
// analogue of the slice value-copy contract used elsewhere in this model.
func cloneGates(g map[uuid.UUID]gateDecision) map[uuid.UUID]gateDecision {
	next := make(map[uuid.UUID]gateDecision, len(g)+1)
	for k, v := range g {
		next[k] = v
	}
	return next
}

// cloneGateDescriptions returns a fresh copy of the permission-description map
// keyed by ToolExecutionID.
func cloneGateDescriptions(g map[uuid.UUID]string) map[uuid.UUID]string {
	next := make(map[uuid.UUID]string, len(g)+1)
	for k, v := range g {
		next[k] = v
	}
	return next
}

// userInputRequested is the AskUser prompt-open boundary: it commits the FULL
// user-input context (Question + ALL Choices) as a kindPromptRecord entry. Choices
// are copied so a later mutation of the event's slice cannot reach the committed
// record. A SUBAGENT loop's AskUser (LoopID != loopID) is attributed: ctx.Agent
// is set to the loop's label (agentLabel) so the record reads "<agent>: <question>"; a
// owning-loop AskUser leaves Agent empty (the orchestrator's own question is not
// agent-labeled). It does NOT commit pending live prose: the provisional narration
// stays in the live segment and is committed exactly once by the step's StepDone
// (committing it here would duplicate it in append-only scrollback). live is NOT reset
// — the turn continues while the gate is pending.
func (m *transcriptModel) userInputRequested(ev event.UserInputRequested) {
	ctx := promptContext{Question: ev.Question}
	if len(ev.Choices) > 0 {
		ctx.Choices = append([]string(nil), ev.Choices...)
	}
	m.commitPrompt(ctx)
}

// commitPrompt appends one kindPromptRecord entry carrying ctx with a fresh stable
// ID. It is the AskUser prompt-open boundary's commit (permission gates surface as the
// verb on their tool card, not as a committed record).
func (m *transcriptModel) commitPrompt(ctx promptContext) {
	m.nextID++
	m.fold.committed = append(m.fold.committed, entry{ID: m.nextID, Kind: kindPromptRecord, Prompt: &ctx})
}

// commitLive is the TurnDone lifecycle path. In a well-formed stream every step
// already committed via its StepDone (which resets the live segment), so live is
// empty here and this is a pure reset. It is DEFENSIVE: should a turn somehow end with
// uncommitted provisional live (no StepDone for an in-flight step), it flushes that
// prose as one kindAssistant entry and any leftover live.Calls in their CURRENT status
// (TurnDone is a normal completion, NOT a cancellation — flushCalls with the identity
// transform), so a stray segment is never silently lost. It finally resets live.
func (m *transcriptModel) commitLive() {
	m.commitProse()
	m.flushCalls(func(c ToolCallView) ToolCallView { return c })
	m.fold.live = liveSeg{}
}

// flushCalls commits every live call as its own kindTool entry, in order, after
// applying transform to each (so a terminal can rewrite status — e.g. running →
// cancelled — while a normal completion leaves it untouched). It is the shared
// drain used by both commitLive (identity transform: preserve status) and
// turnInterrupted (cancel running calls). It does NOT reset live.Calls; callers
// reset the whole live segment afterward.
func (m *transcriptModel) flushCalls(transform func(ToolCallView) ToolCallView) {
	for i := range m.fold.live.Calls {
		m.commitCall(transform(m.fold.live.Calls[i]))
	}
}

// commitProse appends the live segment's pending reasoning/narration to committed
// as one kindAssistant entry (leading ThinkingBlock, then TextBlock; empty blocks
// omitted), allocates its stable ID, and clears ONLY the prose fields — live.Calls
// and active are left intact so a running batch survives the prose commit. It is a
// no-op when there is no pending prose. It is the PROVISIONAL-prose path used at the
// prompt-open boundaries and the abnormal terminals (TurnInterrupted/TurnFailed) to
// flush an in-flight step's narration before its tombstone/error; the normal,
// finalized prose path is stepDone → commitStepAssistant (which renders the AIMessage,
// not the accumulated provisional text).
func (m *transcriptModel) commitProse() {
	if m.fold.live.Thinking == "" && m.fold.live.Text == "" {
		return
	}
	var blocks []content.Block
	var thinkDur time.Duration
	if m.fold.live.Thinking != "" {
		blocks = append(blocks, &content.ThinkingBlock{Thinking: m.fold.live.Thinking})
		// The live timing is still populated on this provisional path (an interrupt/failure
		// before StepDone), so an interrupted turn shows the real "thought for Nsec" it spent —
		// matching a completed step. Restore-equivalence is unaffected (EqualTranscript
		// normalizes thinkDur out either way).
		thinkDur = m.fold.live.thinkDuration()
	}
	if m.fold.live.Text != "" {
		blocks = append(blocks, &content.TextBlock{Text: m.fold.live.Text})
	}
	m.nextID++
	m.fold.committed = append(m.fold.committed, entry{ID: m.nextID, Kind: kindAssistant, Blocks: blocks, thinkDur: thinkDur})
	m.fold.live.Thinking, m.fold.live.Text = "", ""
}

// toolStarted records a freshly started tool call as a running card in live.Calls.
// The card lives in the live tail (the in-progress assistant segment) and is NOT
// committed to scrollback here: a step's tool cards are committed as a group when its
// StepDone snaps the finalized step in (or, defensively, when a turn ends with an
// incomplete in-flight step). It carries the event's redacted Summary so the live and
// committed cards show the same one-line, secret-free header.
func (m *transcriptModel) toolStarted(ev event.ToolCallStarted) {
	m.fold.live.Calls = append(m.fold.live.Calls, ToolCallView{
		ToolExecutionID: ev.ToolExecutionID,
		ToolName:        ev.ToolName,
		Summary:         ev.Summary,
		Permission:      m.fold.live.gateDescriptions[ev.ToolExecutionID],
		Status:          ToolRunning,
		// Bake in the permission decision (if this call prompted): permission resolves
		// BEFORE ToolCallStarted, so the gate is already gateApproved/gateDenied here
		// (gateNone for an ungated/pre-approved call). The card carries it through to
		// the committed entry, so it reads "Approved …" / "Denied …".
		Decision: m.fold.live.gateDecisions[ev.ToolExecutionID],
	})
}

// toolCompleted resolves the matching live call (by ToolExecutionID) IN PLACE — setting its
// terminal status and its capped, redacted ResultPreview — so the live tail shows the
// completed card. It does NOT commit the card or remove it from live.Calls: the card
// is committed only at the step boundary (StepDone) or, defensively, at the turn
// terminal. Keeping the resolved live card lets StepDone reuse its redacted
// Summary/preview when it commits the finalized group (the stored ToolResultMessage
// carries the raw, uncapped result; the resolved live card carries the display-safe
// one). An unknown ToolExecutionID is a no-op — no panic.
func (m *transcriptModel) toolCompleted(ev event.ToolCallCompleted) {
	for i := range m.fold.live.Calls {
		if m.fold.live.Calls[i].ToolExecutionID != ev.ToolExecutionID {
			continue
		}
		m.fold.live.Calls[i].Status = ToolOK
		if ev.IsError {
			m.fold.live.Calls[i].Status = ToolError
		}
		m.fold.live.Calls[i].Result = splitLines(ev.ResultPreview)
		return
	}
	// unknown ToolExecutionID: no-op
}

// stepDone is the StepDone commit point for the event's loop projection. It snaps the
// projection to the finalized step group and builds each tool-use
// block's card (reusing the resolved
// LIVE card — with its redacted Summary, capped preview, and permission Decision — or
// falling back to the stored block + ToolResultMessage when no live card streamed),
// commits the step's AIMessage (thinking + narration, under the node-presence rule) as
// one kindAssistant entry, then commits each card as its own kindTool entry. An
// empty-text step commits no assistant entry at all — its tool nodes stand alone. A
// multi-step turn renders as separate per-step groups, never merged. After committing,
// the provisional live segment is reset (active preserved): the dropped/partial
// TokenDeltas of this step vanish — the self-heal — and the step's gate decisions are
// cleared.
func (m *transcriptModel) stepDone(ev event.StepDone) {
	ai, results := splitStepGroup(ev.Messages)
	uses := toolUsesOf(ai)
	cards := make([]ToolCallView, len(uses))
	for i := range uses {
		cards[i] = m.stepToolCard(uses[i], results, i)
		cards[i] = m.reconcileSubagent(ev, uses[i], results, cards[i])
	}
	m.commitStepAssistant(ai)
	for i := range cards {
		m.commitCall(cards[i])
	}
	// SNAP: drop the provisional live for this step; active stays so the turn's next
	// step (or its terminal) is still seen as in-progress.
	active := m.fold.live.active
	m.fold.live = liveSeg{active: active}
}

// loopSpawned records a TOOL-spawned subagent's spawn relationship at its child
// LoopStarted: a non-empty ParentToolUseID means the loop was spawned by a Subagent
// call, so it maps child loopID → spawnKey{Cause.LoopID,Cause.TurnID,Cause.StepID,
// ParentToolUseID} (loopParent) and seeds the detached accumulator with the agent name
// (subagentAccum). An empty ParentToolUseID (root/non-tool spawn) is left alone —
// that loop keeps the legacy collapsed "▸ <agent>: done" fallback. Both maps are cloned
// on write (value-copy contract).
func (m *transcriptModel) loopSpawned(ev event.LoopStarted) {
	if ev.ParentToolUseID == "" {
		return
	}
	key := spawnKey{ev.Cause.LoopID, ev.Cause.TurnID, ev.Cause.StepID, ev.ParentToolUseID}
	m.loopParent = cloneLoopParent(m.loopParent)
	m.loopParent[ev.LoopID] = key
	acc := m.ensureAccum(key)
	acc.agent = string(ev.AgentName)
	if _, nested := m.loopParent[key.parentLoopID]; !nested {
		m.reconcileCommittedSubagent(key)
	}
}

// subagentTask sets a child accumulator's task from the child's FIRST TurnStarted
// message (truncated to one line), if loopID is a recorded subagent child. Only the
// first message wins: a later TurnStarted for the same loop (a folded continuation)
// leaves the task as the original spawn task. A non-child loop, a missing accumulator,
// or an absent/empty message is a no-op.
func (m *transcriptModel) subagentTask(loopID uuid.UUID, msg *content.UserMessage) {
	key, ok := m.loopParent[loopID]
	if !ok || msg == nil {
		return
	}
	acc, ok := m.subagentAccum[key]
	if !ok || acc.task != "" {
		return
	}
	text := subagentTruncate(textOnly(msg.Blocks))
	if text == "" {
		return
	}
	acc = m.ensureAccum(key)
	acc.task = text
}

// subagentStep folds one child StepDone into its accumulator (design §1): it splits the
// finalized group and builds each tool-use block's card via the PURE storedStepToolCard
// (NEVER stepToolCard — a child must not consult m.fold.live.Calls), appends them as nested
// children, and increments the child's step count. It does NOT commit anything — the
// cards live on the accumulator until the orchestrator's StepDone reconciles them onto
// the parent Subagent card. loopID is known to be a recorded child (the caller checked
// loopParent).
//
// A DEPTH-2 (or deeper) loop's StepDone is NOT folded as a child card (design §6):
// instead it increments the DEPTH-1 ancestor card's collapsed Nested counter, found by
// walking the spawn-parent chain up to the loop whose parent is the root. A depth-1
// loop's own StepDone (its spawn parent IS the root) folds children normally.
func (m *transcriptModel) subagentStep(ev event.StepDone) {
	key := m.loopParent[ev.LoopID]
	if _, nested := m.loopParent[key.parentLoopID]; nested {
		// Depth ≥ 2: collapse into the depth-1 ancestor's Nested counter, never a card.
		if d1, ok := m.depth1Key(ev.LoopID); ok {
			m.ensureAccum(d1).nested++
			m.updateReconciledSubagent(d1)
		}
		return
	}
	ai, results := splitStepGroup(ev.Messages)
	uses := toolUsesOf(ai)
	acc := m.ensureAccum(key)
	for i := range uses {
		acc.children = append(acc.children, storedStepToolCard(uses[i], results))
	}
	acc.steps++
	m.updateReconciledSubagent(key)
}

// depth1Key walks the spawn-parent chain from loopID up to the DEPTH-1 loop — the one
// whose spawn parent is the first ancestor not itself recorded as a child — and returns
// that loop's spawn key (design
// §6: attribute a deeper StepDone to the right depth-1 card by ancestry, not the spawn
// id). It follows each loop's recorded spawnKey.parentLoopID; the chain ends at the
// top-level owning loop. It returns false when the starting child is unknown. A
// guard caps the walk at the number of known child loops so a malformed cycle cannot
// spin.
func (m transcriptModel) depth1Key(loopID uuid.UUID) (spawnKey, bool) {
	for i := 0; i <= len(m.loopParent); i++ {
		key, ok := m.loopParent[loopID]
		if !ok {
			return spawnKey{}, false
		}
		if _, nested := m.loopParent[key.parentLoopID]; !nested {
			return key, true
		}
		loopID = key.parentLoopID
	}
	return spawnKey{}, false
}

// subagentTerminal records a child loop's terminal status on its accumulator and
// reports whether loopID was a recorded subagent child. Each terminal still finalizes that
// loop's own projection; this hook only updates the detached parent-card accumulator.
func (m *transcriptModel) subagentTerminal(loopID uuid.UUID, status subStatus) bool {
	key, ok := m.loopParent[loopID]
	if !ok {
		return false
	}
	acc := m.ensureAccum(key)
	acc.status = status
	m.updateReconciledSubagent(key)
	if acc.reconciled {
		m.retireSubagentAccum(loopID, key)
	}
	return true
}

// reconcileSubagent attaches a child accumulator onto the orchestrator's committed
// Subagent card (design §3). For a Subagent tool-use block it computes the spawn key
// from the orchestrator's OWN coordinates (ev.LoopID/TurnID/StepID) + block.ID and
// looks up subagentAccum: first by the full coordinate key, then (when those parent
// turn/step coordinates diverge) by a UNIQUE pending parent-loop + provider-tool-use-id
// fallback. On a hit it copies the agent/task/children/steps/status and
// sets the done summary from the block's paired ToolResultMessage text (truncated),
// then SUPPRESSES the card's normal result body so the hand-back text shows ONLY in the
// done child, not doubled (Task 7 renders the suppression; here Agent being set is the
// marker). The fallback fails closed when zero or multiple pending candidates match. On
// a miss (spawn failed before any child LoopStarted, or malformed ambiguous identity) the
// card is returned unchanged — it renders normally with its error result text. A
// non-Subagent block is returned unchanged.
func (m *transcriptModel) reconcileSubagent(ev event.StepDone, use content.ToolUseBlock, results map[string]*content.ToolResultMessage, card ToolCallView) ToolCallView {
	if !isAgentSpawnTool(use.Name) {
		return card
	}
	key := spawnKey{ev.LoopID, ev.TurnID, ev.StepID, use.ID}
	// Retain the durable parent/tool identity even when the child LoopStarted has not
	// arrived yet. A later child event can then promote this committed card in place.
	card.spawn = key
	acc, matchedKey, ok := m.matchSubagentAccum(key)
	if !ok {
		return card // spawn failed before any child loop: render the error result normally
	}
	card.Agent = acc.agent
	card.Task = acc.task
	// Copy the children: the committed card must be structurally FROZEN, never aliasing
	// the live subagentAccum backing slice (the one place a committed entry could share
	// mutable backing with live reducer state). Safe-by-construction, not by ordering —
	// it hardens the reflect.DeepEqual restore-equivalence (Task 8) ahead of Task 7's
	// further accumulator mutation (Nested).
	card.Children = append([]ToolCallView(nil), acc.children...)
	card.Steps = acc.steps
	card.SubStatus = acc.status
	card.Nested = acc.nested
	card.spawn = matchedKey
	// The done summary is the hand-back text; suppress the card's own result body so it
	// is not also shown as the normal result preview (design §4: no doubling).
	card.Result = splitLines(subagentTruncate(toolResultText(results[use.ID])))
	// Mark the accumulator reconciled (in-place, consistent with the rest of the
	// accumulator's fill): the card now lives in the committed transcript, so the live
	// tail must stop rendering it as a pending in-flight card (pendingSubagentCards).
	acc.reconciled = true
	return card
}

// reconcileCommittedSubagent handles the reverse cross-loop delivery order: the
// parent's StepDone committed an unresolved spawn card before the child's LoopStarted
// created its accumulator. It finds the unique parent card by durable tool-use identity,
// clones the parent projection, and promotes that existing card in place. Exact
// coordinates win; a unique parent-loop + tool-use-id match tolerates stale turn/step
// coordinates in the same way matchSubagentAccum does. Ambiguity fails closed.
func (m *transcriptModel) reconcileCommittedSubagent(key spawnKey) {
	p := m.projections[key.parentLoopID]
	acc := m.subagentAccum[key]
	if p == nil || acc == nil || acc.reconciled {
		return
	}
	type position struct {
		entry int
		call  int
	}
	var exact *position
	var fallback *position
	fallbacks := 0
	for entryIndex := range p.committed {
		for callIndex := range p.committed[entryIndex].Calls {
			card := p.committed[entryIndex].Calls[callIndex]
			if card.Agent != "" || !isAgentSpawnTool(card.ToolName) {
				continue
			}
			candidate := card.spawn
			pos := position{entry: entryIndex, call: callIndex}
			if candidate == key {
				exact = &pos
				break
			}
			if candidate.parentLoopID != key.parentLoopID || candidate.toolUseID == "" || candidate.toolUseID != key.toolUseID {
				continue
			}
			fallbacks++
			fallback = &pos
		}
		if exact != nil {
			break
		}
	}
	match := exact
	if match == nil && fallbacks == 1 {
		match = fallback
	}
	if match == nil {
		return
	}

	next := make(map[uuid.UUID]*loopProjection, len(m.projections))
	for loopID, projection := range m.projections {
		next[loopID] = projection
	}
	cloned := *p
	cloned.committed = append([]entry(nil), p.committed...)
	cloned.committed[match.entry].Calls = append([]ToolCallView(nil), p.committed[match.entry].Calls...)
	next[key.parentLoopID] = &cloned
	m.projections = next

	entry := &cloned.committed[match.entry]
	card := &entry.Calls[match.call]
	card.Agent = acc.agent
	card.Task = acc.task
	card.Children = append([]ToolCallView(nil), acc.children...)
	card.Steps = acc.steps
	card.SubStatus = acc.status
	card.Nested = acc.nested
	card.spawn = key
	acc.reconciled = true
	acc.committedID = entry.ID
}

// matchSubagentAccum returns the accumulator owned by a committed parent Subagent call.
// Full coordinates are authoritative when they agree. The harness also carries the provider
// tool-use id explicitly because replay/fan-in paths can report different parent turn/step
// coordinates; in that case a UNIQUE, unreconciled accumulator under the same parent loop and
// tool-use id is the same call. Multiple matches are malformed and fail closed rather than
// attaching the wrong child card.
func (m transcriptModel) matchSubagentAccum(key spawnKey) (*subagentAccumulator, spawnKey, bool) {
	if acc, ok := m.subagentAccum[key]; ok && acc != nil {
		return acc, key, true
	}
	var match *subagentAccumulator
	var matchKey spawnKey
	for candidate, acc := range m.subagentAccum {
		if acc == nil || acc.reconciled || candidate.parentLoopID != key.parentLoopID || candidate.toolUseID != key.toolUseID {
			continue
		}
		if match != nil {
			return nil, spawnKey{}, false
		}
		match = acc
		matchKey = candidate
	}
	return match, matchKey, match != nil
}

// pendingSubagentCards returns, in stable creation order (accumOrder), one ToolCallView
// per NOT-YET-reconciled accumulator — the in-flight subagent cards the LIVE tail renders
// WHILE the subagent streams (its ENDURING child events fill the accumulator before the
// orchestrator's StepDone commits the card). Each card mirrors what reconcileSubagent will
// later copy onto the committed card (agent/task/children/steps/status/nested), with the
// children copied so a caller cannot reach the live accumulator's backing slice. A
// reconciled accumulator (its card already in scrollback) is skipped — no double render —
// as is a nil entry (defensive). It is DISPLAY-ONLY: it never mutates the model.
func (m transcriptModel) pendingSubagentCards() []ToolCallView {
	var out []ToolCallView
	for _, key := range m.accumOrder {
		acc := m.subagentAccum[key]
		if acc == nil || acc.reconciled {
			continue
		}
		// Cap the LIVE card's children to the most recent liveCallCap: a subagent that runs
		// many tools would otherwise grow the live tail to fill the screen (the "running · N
		// steps" line still conveys the total). The FULL children commit to scrollback at the
		// orchestrator StepDone via reconcileSubagent.
		children := acc.children
		if len(children) > liveCallCap {
			children = children[len(children)-liveCallCap:]
		}
		out = append(out, ToolCallView{
			ToolName:  startAgentToolName,
			Agent:     acc.agent,
			Task:      acc.task,
			Children:  append([]ToolCallView(nil), children...),
			Steps:     acc.steps,
			SubStatus: acc.status,
			Nested:    acc.nested,
		})
	}
	return out
}

func (m transcriptModel) pendingSubagentCardsFor(parentLoopID uuid.UUID) []ToolCallView {
	var out []ToolCallView
	for _, key := range m.accumOrder {
		if key.parentLoopID != parentLoopID {
			continue
		}
		acc := m.subagentAccum[key]
		if acc == nil || acc.reconciled {
			continue
		}
		children := acc.children
		if len(children) > liveCallCap {
			children = children[len(children)-liveCallCap:]
		}
		out = append(out, ToolCallView{ToolName: startAgentToolName, Agent: acc.agent, Task: acc.task, Children: append([]ToolCallView(nil), children...), Steps: acc.steps, SubStatus: acc.status, Nested: acc.nested})
	}
	return out
}

// Agent spawn tool names are duplicated here rather than importing concrete tool packages
// solely for unexported string constants. Subagent is the legacy synchronous tool name;
// StartAgent is the current AgentTools name.
const (
	subagentToolName   = "Subagent"
	startAgentToolName = "StartAgent"
)

func isAgentSpawnTool(name string) bool {
	return name == subagentToolName || name == startAgentToolName
}

// ensureAccum returns the accumulator for key, creating it (and cloning the map on
// write) when absent so a child event can fill it before the parent card exists. The
// returned pointer is owned by the (possibly freshly cloned) map, so in-place mutation
// of its fields is the intended write path — the MAP clone, not a per-accumulator
// clone, is what upholds the value-copy reducer contract (a child's events are folded
// in document order into the one accumulator the parent later reads).
func (m *transcriptModel) ensureAccum(key spawnKey) *subagentAccumulator {
	if acc, ok := m.subagentAccum[key]; ok {
		return acc
	}
	next := make(map[spawnKey]*subagentAccumulator, len(m.subagentAccum)+1)
	for k, v := range m.subagentAccum {
		next[k] = v
	}
	acc := &subagentAccumulator{}
	next[key] = acc
	m.subagentAccum = next
	// Record the key's stable creation order in a CLONED slice (value-copy contract — a
	// fresh backing array, never an in-place append into a slice a prior model holds), so
	// pendingSubagentCards renders in-flight cards deterministically.
	m.accumOrder = append(append([]spawnKey(nil), m.accumOrder...), key)
	return acc
}

// clearLoopAccums drops every subagent accumulator spawned BY loopID — called when that
// loop's turn reaches a terminal (TurnDone/Interrupted/Failed). A reconciled accumulator
// that is still running is retained because cross-loop delivery can place the parent's
// terminal before the child's terminal; all other matching accumulators are released. An
// accumulator left UNRECONCILED (a spawn whose turn/step/tool-use coordinates never matched the
// orchestrator's, or a turn cut short before its StepDone) is now stale and must be
// released — otherwise pendingSubagentCards keeps exposing it and the live tail renders it
// perpetually below the committed "turn ran" line, and it leaks into the next turn.
//
// Scoped to the parent LOOP ONLY (not the turn): the very coordinate divergence that
// defeats reconciliation also makes an accumulator's parentTurnID unreliable, but its
// parentLoopID is the parent loop (that is how pendingSubagentCards found it under this
// loop), which equals the terminating turn's LoopID — so loop-scope is the one match that
// always holds. Clone-on-write (value-copy contract) uses a fresh map + accumOrder omitting
// the keys, never in-place.
// The matching child→spawn relationships are retired at the same boundary. Without that
// tombstone, an already-published child terminal delivered after the parent's terminal can
// call subagentTerminal, recreate the deleted accumulator, and make the card reappear below
// the committed turn notice.
func (m *transcriptModel) clearLoopAccums(loopID uuid.UUID) {
	drop := make(map[spawnKey]bool)
	for k, acc := range m.subagentAccum {
		// A parent terminal can overtake its child's terminal on the all-loops stream.
		// Keep an already-bound running accumulator and relationship so the late child
		// event can refresh the committed card; subagentTerminal retires it afterward.
		if k.parentLoopID == loopID && !(acc.reconciled && acc.status == subRunning) {
			drop[k] = true
		}
	}
	if len(drop) == 0 {
		return
	}
	next := make(map[spawnKey]*subagentAccumulator, len(m.subagentAccum))
	for k, v := range m.subagentAccum {
		if !drop[k] {
			next[k] = v
		}
	}
	order := make([]spawnKey, 0, len(m.accumOrder))
	for _, k := range m.accumOrder {
		if !drop[k] {
			order = append(order, k)
		}
	}
	m.subagentAccum = next
	m.accumOrder = order

	// Retire every direct child whose spawn belonged to the terminating parent. All-loop
	// delivery preserves each loop's order, not cross-loop ordering, so a child's terminal
	// may legitimately arrive after this parent terminal. Removing the relationship makes
	// every late child fold a no-op for the detached card instead of resurrecting it.
	parents := cloneLoopParent(m.loopParent)
	for child, key := range parents {
		if drop[key] {
			delete(parents, child)
		}
	}
	m.loopParent = parents
}

// retireSubagentAccum releases one reconciled accumulator after its terminal state has
// been copied to the committed card. It also retires the child relationship so a malformed
// late event cannot recreate a pending card.
func (m *transcriptModel) retireSubagentAccum(loopID uuid.UUID, key spawnKey) {
	next := make(map[spawnKey]*subagentAccumulator, len(m.subagentAccum))
	for k, v := range m.subagentAccum {
		if k != key {
			next[k] = v
		}
	}
	order := make([]spawnKey, 0, len(m.accumOrder))
	for _, k := range m.accumOrder {
		if k != key {
			order = append(order, k)
		}
	}
	m.subagentAccum = next
	m.accumOrder = order
	parents := cloneLoopParent(m.loopParent)
	delete(parents, loopID)
	m.loopParent = parents
}

// cloneLoopParent returns a fresh copy of the child→spawnKey map (nil-safe), so a
// by-value reducer mutation never writes through a map a prior model still holds — the
// map analogue of the slice value-copy contract used throughout this model.
func cloneLoopParent(p map[uuid.UUID]spawnKey) map[uuid.UUID]spawnKey {
	next := make(map[uuid.UUID]spawnKey, len(p)+1)
	for k, v := range p {
		next[k] = v
	}
	return next
}

// subagentTruncate normalizes a task/summary string to a single transcript line:
// newlines→spaces, trim, then cap at subagentLineCap display runes with an ellipsis
// (truncate). The full text remains in the durable journal. An empty/whitespace string
// yields "".
func subagentTruncate(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.TrimSpace(s)
	return truncate(s, subagentLineCap)
}

// subagentLineCap is the ~80-col cap for a subagent task/summary line (design §Definitions).
const subagentLineCap = 80

// commitStepAssistant commits the AIMessage's reasoning + narration as one kindAssistant
// entry, under the node-presence rule. A nil AIMessage commits nothing. It commits the
// thinking rail (when the step reasoned) and the narration (when the text is non-empty).
// When both are empty it commits NOTHING — a pure-tool step has no assistant entry; its
// tool calls stand alone as their own kindTool rail nodes. So a thinking-only step
// commits just the thinking rail, a narration step commits the "●" node (plus rail), and
// an empty-text tool step commits no assistant entry at all — its tool calls stand alone
// as their own kindTool rail nodes.
func (m *transcriptModel) commitStepAssistant(ai *content.AIMessage) {
	if ai == nil {
		return
	}
	var blocks []content.Block
	var thinkDur time.Duration
	if th := thinkingText(ai.Blocks); th != "" {
		blocks = append(blocks, &content.ThinkingBlock{Thinking: th})
		// Attach the step's measured thinking span from the (not-yet-reset) live segment,
		// so the committed rail reads "thought for Nsec"; a stream with no chunk timing
		// (e.g. a cold restore) yields 0 → the bare "thought" fallback.
		thinkDur = m.fold.live.thinkDuration()
	}
	if text := textOnly(ai.Blocks); text != "" {
		blocks = append(blocks, &content.TextBlock{Text: text})
	}
	if len(blocks) == 0 {
		return // pure-tool step: no thinking, no text — the tool nodes stand alone
	}
	m.nextID++
	m.fold.committed = append(m.fold.committed, entry{
		ID:       m.nextID,
		Kind:     kindAssistant,
		Blocks:   blocks,
		thinkDur: thinkDur,
	})
}

// stepToolCard builds the committed ToolCallView for the index-th tool-use block of a
// owning-loop step. It prefers the resolved live card at the same position (carrying
// the redacted Summary and capped preview already shown live); when there is none it
// falls back to storedStepToolCard (the stored block + matching ToolResultMessage). The
// live-preference branch is correct for the event loop's own step. Nested child cards
// must not consult m.fold.live.Calls (it could steal a same-index parent live
// card in a mixed batch), so child reconstruction uses storedStepToolCard exclusively
// (design §3a).
func (m *transcriptModel) stepToolCard(use content.ToolUseBlock, results map[string]*content.ToolResultMessage, idx int) ToolCallView {
	if idx < len(m.fold.live.Calls) {
		live := m.fold.live.Calls[idx]
		if live.ToolName == use.Name {
			if live.Status == ToolRunning {
				live.Status = ToolOK // the step finalized: a still-"running" live card resolves OK
			}
			if summary := toolUseSummary(use.Name, use.Input); summary != "" {
				live.Summary = summary
			}
			return live
		}
	}
	return storedStepToolCard(use, results)
}

// storedStepToolCard builds a ToolCallView PURELY from the stored tool-use block and
// its paired ToolResultMessage (correlated by use.ID), with NO m.fold.live.Calls access. It
// is the durable, position-independent card builder: the shared fallback of
// stepToolCard and the exclusive builder for a subagent's nested
// children (where a same-index parent live card must never leak in — design §3a). The
// card shows no summary (the redacted summary is not carried in the stored message);
// its ✓/✗ status comes from ToolResultMessage.IsError, which the stored message
// preserves (an error result commits a ✗ card on this path too). It is a free function
// — it depends on nothing but its arguments, which is the property the design relies on.
func storedStepToolCard(use content.ToolUseBlock, results map[string]*content.ToolResultMessage) ToolCallView {
	card := ToolCallView{ToolName: use.Name, Summary: toolUseSummary(use.Name, use.Input), Status: ToolOK}
	if r, ok := results[use.ID]; ok {
		card.Result = splitLines(toolResultText(r))
		if r.IsError {
			card.Status = ToolError
		}
	}
	return card
}

// commitCall appends one resolved tool call as its own kindTool entry with a fresh
// stable ID. The single-element Calls slice carries the terminal ToolCallView so the
// renderer can reuse the existing tool-node rendering.
func (m *transcriptModel) commitCall(call ToolCallView) {
	m.nextID++
	m.fold.committed = append(m.fold.committed, entry{
		ID:    m.nextID,
		Kind:  kindTool,
		Calls: []ToolCallView{call},
	})
	if call.spawn != (spawnKey{}) {
		if acc := m.subagentAccum[call.spawn]; acc != nil {
			acc.committedID = m.nextID
		}
	}
}

// updateReconciledSubagent refreshes the one committed parent card from its detached
// accumulator. Parent and child event order is independent, so child steps and terminals
// may arrive after the parent's StepDone; updating by the bound display ID keeps a single
// authoritative card instead of rendering a stale committed card plus a pending duplicate.
func (m *transcriptModel) updateReconciledSubagent(key spawnKey) {
	acc := m.subagentAccum[key]
	if acc == nil || !acc.reconciled || acc.committedID == 0 {
		return
	}
	p := m.projections[key.parentLoopID]
	if p == nil {
		return
	}
	for i := range p.committed {
		e := &p.committed[i]
		if e.ID != acc.committedID || len(e.Calls) != 1 {
			continue
		}
		card := &e.Calls[0]
		card.Agent = acc.agent
		card.Task = acc.task
		card.Children = append([]ToolCallView(nil), acc.children...)
		card.Steps = acc.steps
		card.SubStatus = acc.status
		card.Nested = acc.nested
		return
	}
}

// turnInterrupted is the cancellation terminal: it commits pending prose, marks
// every still-running live call cancelled and commits each as its own kindTool
// entry (so completed/cancelled tool work stays visible), appends the
// content-less kindInterrupted tombstone, and resets live.
func (m *transcriptModel) turnInterrupted() {
	m.commitProse()
	m.flushCalls(func(c ToolCallView) ToolCallView {
		if c.Status == ToolRunning {
			c.Status = ToolCancelled
		}
		return c
	})
	m.nextID++
	m.fold.committed = append(m.fold.committed, entry{ID: m.nextID, Kind: kindInterrupted})
	m.fold.live = liveSeg{}
}

// turnFailed is the failure terminal: it commits pending prose so partial work
// stays visible, appends an error-level kindNotice carrying the failure message (a
// nil Err yields an empty message — the entry still marks the failure), and resets
// live. The error-notice commit reuses the same noticeError path as CommitError.
func (m *transcriptModel) turnFailed(ev event.TurnFailed) {
	m.commitProse()
	msg := ""
	if ev.Err != nil {
		msg = ev.Err.Error()
	}
	m.nextID++
	m.fold.committed = append(m.fold.committed, entry{
		ID:     m.nextID,
		Kind:   kindNotice,
		Level:  noticeError,
		Blocks: []content.Block{&content.TextBlock{Text: msg}},
	})
	m.fold.live = liveSeg{}
}

// projectionFor returns the global rows plus the requested loop's committed and live
// stream. An absent projection yields only global rows and an empty live segment.
func (m transcriptModel) projectionFor(loopID uuid.UUID) (committed []entry, live liveSeg) {
	if p, ok := m.projections[loopID]; ok {
		return append(append([]entry(nil), m.global...), p.committed...), p.live
	}
	return m.global, liveSeg{}
}

// committedLen counts finalized rows across the global stream and every loop. It is
// independent of the last event folded, including session-scoped tail events.
func (m transcriptModel) committedLen() int {
	n := len(m.global)
	for _, p := range m.projections {
		if p != nil {
			n += len(p.committed)
		}
	}
	return n
}

// ensureProjection returns the projection for a loop, creating it (and
// cloning the projections map on write) on first sight so a later event folds into the
// same projection. A freshly created projection is freshly allocated, so the by-value
// reducer never aliases a prior model's projection; the MAP clone upholds the value-copy
// contract, mirroring ensureAccum.
func (m *transcriptModel) ensureProjection(loopID uuid.UUID) *loopProjection {
	if p, ok := m.projections[loopID]; ok {
		return p
	}
	next := make(map[uuid.UUID]*loopProjection, len(m.projections)+1)
	for k, v := range m.projections {
		next[k] = v
	}
	p := &loopProjection{}
	next[loopID] = p
	m.projections = next
	return p
}
