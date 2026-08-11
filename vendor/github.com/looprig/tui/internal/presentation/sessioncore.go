package presentation

import (
	"context"
	"errors"

	tea "charm.land/bubbletea/v2"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	inputpkg "github.com/looprig/tui/internal/input"
)

// sessionCore is the SHARED transport both TUI presentation shells embed: the
// scrollback-first Screen and the modern viewport Screen. It owns everything an
// event router must own — the agent wiring, the ONE session-lifetime subscription and
// its lifecycle (stale/nil guards), the dispatch of each event into the transcript,
// interaction, and compaction reducers, the active-loop turn status, the /clear ordering, and
// the submit/interrupt/gate command wiring — but NO presentation. Extracting it means
// event routing cannot drift between the two modes: a new event type handled here is
// handled by both shells at once.
//
// Event transport: a SINGLE whole-session subscription (sub), established once at
// startup and read continuously by subNext, is the sole event source — it spans every
// turn and loop and never EOFs per turn. Submissions are fire-and-forget (submitCmd →
// agent.Submit); the LOOP owns queueing, so the core keeps no queue. User rows are
// EVENT-DRIVEN (committed by the transcript reducer from the loop's TurnStarted/
// TurnFoldedInto Message), never optimistically at submit. EVERY loop's turn events fold a
// per-loop running bit, but the displayed turn status follows only the ACTIVE loop's bit —
// the session's active loop, whose baseline is read at subscribe and advanced by each
// ActiveLoopChanged; a background loop's turn events fold silently.
//
// Presentation stays in the embedding shell: the core's methods MUTATE the shared
// transport state and return the TRANSPORT command (re-arm, submit, reopen, …) plus,
// for the paths that commit an out-of-band entry, a boolean signal telling the shell to
// present it (scrollback flush for Screen, viewport re-render for Screen). The
// core never touches scrollback, held tails, animation, or a viewport.
type sessionCore struct {
	agent     Agent
	openAgent OpenAgent       // builds a replacement agent on /clear
	appCtx    context.Context // long-lived; cancelled on quit
	banner    AgentBanner     // agent name + description, shown as the startup info notice

	transcript  transcriptModel  // reconstructs the turn; tracks committed entries + the live segment
	interaction interactionModel // the bottom surface's composer + pending-prompt FIFO

	status Status      // Idle | Running | Interrupting | Resetting
	sub    EventStream // the session-lifetime event subscription; nil until subscribed
	// terminalErr is set only when a /clear ownership handoff has closed the old agent but
	// cannot install a replacement. Bubble Tea exits cleanly; runtime.Run reads this error from
	// the final model to return a process failure instead of exit success.
	terminalErr error
	// handoff is non-nil while an asynchronous /clear command owns the old/replacement
	// lifecycle. Agent reports nil in this state so forced runtime teardown cannot double-close.
	handoff *reopenHandoff
	// closing owns the replacement between a deferred ctrl+c and consumption of its close
	// result. runtime.Run finalization and the Bubble Tea command share its exactly-once Close.
	closing *agentCloseHandoff

	// activeLoopID is the session's authoritative current selection — the post-subscription
	// baseline read from Agent.ActiveLoopID in applySubscribed, then advanced causally by
	// each ActiveLoopChanged. It is zero until the first successful subscribe (no authority
	// yet); selection reconciliation FAILS CLOSED while zero, and status derivation falls
	// back to Agent.ActiveLoopID (effectiveActiveLoopID).
	activeLoopID uuid.UUID
	// loopRunning folds EVERY loop's turn-liveness (TurnStarted → true; any terminal →
	// false). The DISPLAYED status follows only the active loop's bit; a background loop's
	// turn events fold here without moving the status. Never nil (seeded in newSessionCore).
	loopRunning map[uuid.UUID]bool

	// compaction is the pure per-loop activity projection shared by live delivery and
	// restore folding. Terminal attempt tombstones prevent replay/buffer overlap from
	// resurrecting an already-finished ephemeral start.
	compaction compactionProjection
}

// newSessionCore builds an idle sessionCore over agent, with open as the /clear thunk and
// banner the agent metadata. Every delivered loop folds into its own transcript projection.
func newSessionCore(ctx context.Context, agent Agent, open OpenAgent, banner AgentBanner) sessionCore {
	return sessionCore{
		agent:       agent,
		openAgent:   open,
		appCtx:      ctx,
		banner:      banner,
		status:      StatusIdle,
		transcript:  transcriptModel{},
		interaction: newInteractionModel(),
		// The AUTHORITATIVE active baseline is NOT established here — it is read from the
		// agent only once the subscription is live (applySubscribed), so no selection
		// transition can slip past the baseline read. loopRunning is seeded non-nil so the
		// per-loop turn fold never touches a nil map.
		loopRunning: map[uuid.UUID]bool{},
	}
}

// subscribe builds the session-lifetime subscription command with the all-loops
// AllLoopsEventFilter against the CURRENT agent. It is the single subscribe point both
// startup and the /clear re-subscribe route through, so the all-loops scope is honored
// uniformly — at startup and after a /clear agent swap.
func (c sessionCore) subscribe() tea.Cmd {
	return subscribeWith(c.agent, AllLoopsEventFilter())
}

// Agent returns the live agent. Product commands use this for a bounded backstop Close of
// whichever agent /clear may have swapped in. It is a value receiver so it promotes
// into every embedding shell's method set — both Screen and Screen satisfy the
// composition root's agentHolder through this one definition.
func (c sessionCore) Agent() Agent {
	if c.handoff != nil || c.closing != nil {
		return nil
	}
	return c.agent
}

// TerminalError reports a fatal transport ownership failure that caused the TUI to quit.
// Ordinary user-visible command errors remain in the transcript and return nil here.
func (c sessionCore) TerminalError() error { return c.terminalErr }

// FinalizeHandoff waits boundedly for an in-flight /clear command, closes an unconsumed
// replacement, and completes any deferred replacement close. runtime.Run calls it before returning
// to a composition root that may close stores; cancellation keeps the open phase bounded.
func (c sessionCore) FinalizeHandoff() error {
	var err error
	if c.handoff != nil {
		err = c.handoff.finalize()
	}
	if c.closing != nil {
		err = errors.Join(err, c.closing.close())
	}
	return err
}

// turnPhase is the ACTIVE-loop turn-status transition applyTurnStatus reports so a
// presentation shell can react to it (Screen arms the live-tail blink on turnStarted
// and rotates its tip on turnEnded; the modern shell pulses its focused-loop status).
// The status STATE itself is set on the core — the phase is only the presentation cue.
type turnPhase uint8

const (
	// turnUnchanged is a non-turn event or a background (non-active) loop's turn event:
	// the displayed status did not move.
	turnUnchanged turnPhase = iota
	// turnStarted is the active loop's TurnStarted: status went Running.
	turnStarted
	// turnEnded is an active-loop terminal (TurnDone/TurnFailed/TurnInterrupted): status
	// returned to Idle.
	turnEnded
)

// handleEvent applies one subscription event to the display reducers — the transcript (which
// reconstructs the live segment and commits user/tool/prompt/terminal entries) and the
// interaction model (which enqueues prompts and clears its queue on terminals), plus the
// per-loop compaction activity projection — then
// reconciles an ActiveLoopChanged into the active-loop baseline (applyActiveSelection),
// folds every loop's turn-lifecycle events into the per-loop running map and derives the
// displayed STATUS from the ACTIVE loop's bit (applyTurnStatus), and re-arms the
// continuous reader. It returns the re-arm command (nil while no live subscription is
// installed — during the /clear window sub is transiently nil) plus the turn-phase cue
// for the shell. Reading continues unconditionally: the loop is blocked on a permission
// GATE, not the stream, so the user's keypress is what releases it. Presentation — the
// scrollback flush or the viewport render — is the embedding shell's job.
func (c *sessionCore) handleEvent(ev event.Event) (tea.Cmd, turnPhase) {
	c.transcript = c.transcript.ApplyEvent(ev)
	c.interaction = c.interaction.ApplyEvent(ev)
	c.compaction = c.compaction.ApplyEvent(ev)
	if sel, ok := ev.(event.ActiveLoopChanged); ok {
		c.applyActiveSelection(sel)
	}
	phase := c.applyTurnStatus(ev)
	// Re-arm only while a live subscription is installed: during the /clear window
	// c.sub is transiently nil, and the fresh subscription's reader is (re)started by
	// the subscribed handler, not here. subNext is also nil-guarded as a backstop.
	var rearm tea.Cmd
	if c.sub != nil {
		rearm = subNext(c.sub)
	}
	return rearm, phase
}

// applyTurnStatus folds a turn-lifecycle event into the per-loop running map for EVERY loop
// (events carry Header.LoopID) and, when the event belongs to the ACTIVE loop, re-derives the
// displayed status from that loop's freshly folded bit. A background (non-active) loop's turn
// events fold their bit but leave the displayed status untouched — the active-loop projection,
// not the root, owns the status now. Interrupting/Resetting are owned by their own handlers
// and are NOT set here, but a terminal resolves Interrupting → Idle (completing an in-flight
// interrupt). It returns the phase transition (relative to the active loop) so a
// shell can drive its presentation reaction; non-turn and background-loop turn events return
// turnUnchanged.
//
// The Interrupting resolution is deliberately checked BEFORE the background-loop early return,
// and accepts ANY loop's terminal. Session interrupt is session-wide (harness Session.Interrupt
// snapshots every live loop), so the loop that reports the terminal need not be the active one —
// requiring the ACTIVE loop's terminal wedged the UI in "Interrupting…" forever whenever the
// interrupted loop was a subagent. Interrupting is a session-GLOBAL status (focusedStatus
// surfaces it regardless of focus), so that wedge froze every loop's status line, not just the
// interrupted one's.
func (c *sessionCore) applyTurnStatus(ev event.Event) turnPhase {
	loopID := ev.EventHeader().LoopID
	var terminal bool
	switch ev.(type) {
	case event.TurnStarted:
		c.loopRunning[loopID] = true
	case event.TurnDone, event.TurnFailed, event.TurnInterrupted:
		c.loopRunning[loopID] = false
		terminal = true
	default:
		return turnUnchanged
	}
	if terminal && c.status == StatusInterrupting {
		c.status = StatusIdle // any loop's terminal resolves the session-wide interrupt
		return turnEnded
	}
	if loopID != c.effectiveActiveLoopID() {
		return turnUnchanged // a background loop's bit folded; the displayed status is unmoved
	}
	c.deriveActiveStatus()
	if terminal {
		return turnEnded
	}
	return turnStarted
}

// applyActiveSelection reconciles one ActiveLoopChanged against the current authoritative
// baseline. It FAILS CLOSED while no baseline exists (activeLoopID zero): without an
// authority a selection cannot be ordered, so it is ignored rather than trusted. Otherwise it
// advances the baseline only when the transition departs from it (Previous matches), treats a
// selection the baseline already reflects as a no-op, and never regresses on a stale event —
// then re-derives the displayed status for the (possibly new) active loop.
func (c *sessionCore) applyActiveSelection(ev event.ActiveLoopChanged) {
	if c.activeLoopID.IsZero() {
		return // no authoritative baseline: fail closed
	}
	switch c.activeLoopID {
	case ev.PreviousLoopID:
		c.activeLoopID = ev.ActiveLoopID
	case ev.ActiveLoopID:
		// Baseline already includes this transition.
	default:
		// Stale relative to the post-subscription baseline; do not regress.
	}
	c.deriveActiveStatus()
}

// effectiveActiveLoopID is the loop the displayed status follows: the authoritative
// activeLoopID once established, else Agent.ActiveLoopID as the pre-baseline fallback.
// Status-display ONLY — the selection reconcile (applyActiveSelection) must key on
// activeLoopID directly and fail closed when it is zero, never on this fallback, so a
// stale selection cannot be ordered against a synthesized baseline.
func (c sessionCore) effectiveActiveLoopID() uuid.UUID {
	if c.activeLoopID.IsZero() {
		return c.agent.ActiveLoopID()
	}
	return c.activeLoopID
}

// deriveActiveStatus sets the ordinary displayed status from the active loop's running bit
// (running → Running, else Idle). It MUST NOT clobber the two explicitly-owned statuses:
// Interrupting is resolved only by its handler or the active loop's terminal, and Resetting
// persists until the /clear reopen completes.
func (c *sessionCore) deriveActiveStatus() {
	if c.status == StatusInterrupting || c.status == StatusResetting {
		return
	}
	if c.loopRunning[c.effectiveActiveLoopID()] {
		c.status = StatusRunning
		return
	}
	c.status = StatusIdle
}

// applySubscribed installs the session-lifetime subscription and reports the outcome. On
// a non-nil err the TUI cannot observe the session at all — it commits a fatal error
// entry (the shell must present it: the returned bool is true) and installs no stream.
// On success it stores the stream and returns subNext, the single reader that drives
// every subsequent event.
func (c *sessionCore) applySubscribed(msg subscribedMsg) (tea.Cmd, bool) {
	if msg.err != nil {
		c.transcript = c.transcript.CommitGlobalError(msg.err)
		return nil, true
	}
	c.sub = msg.sub
	// The subscription is now live, so the ActiveLoopID read cannot miss a selection that
	// predates it: this is the AUTHORITATIVE baseline every later ActiveLoopChanged is
	// reconciled against. Derive the displayed status for the freshly established active loop.
	c.activeLoopID = c.agent.ActiveLoopID()
	c.deriveActiveStatus()
	return subNext(c.sub), false
}

// applySubClosed reacts to the continuous reader observing a closed channel. A nil err
// is an intentional Close (a /clear swap or quit teardown) — nothing to surface (returns
// false). A non-nil err is a hub-forced loss (egress overflow); it commits an error
// entry so the user learns the live stream was dropped and asks the shell to present it.
// Either way the reader is not re-armed (the channel is closed); a /clear re-subscribe
// is the path back to a live stream.
func (c *sessionCore) applySubClosed(msg subClosedMsg) bool {
	if msg.err == nil {
		return false
	}
	c.transcript = c.transcript.CommitGlobalError(msg.err)
	return true
}

// applySubmitResult surfaces a fire-and-forget Submit outcome. On success it records the
// submit (RecordSubmit) under the loop-assigned InputID so the queued affordance can show
// once the loop's InputQueued event arrives; the authoritative user row is committed
// later from the loop's TurnStarted/TurnFoldedInto Message, NOT here — so success commits
// nothing out-of-band (returns false). A non-nil err commits a faint, NON-FATAL error
// entry noting the send failed and asks the shell to present it.
func (c *sessionCore) applySubmitResult(msg submitResultMsg) bool {
	if msg.err != nil {
		c.transcript = c.transcript.CommitGlobalError(msg.err)
		return true
	}
	c.transcript = c.transcript.RecordSubmit(msg.inputID, msg.blocks)
	return false
}

// applyCompactResult surfaces only an immediate manual-compaction failure. A
// successful request is silent: enduring compaction events own subsequent user
// feedback, so this reducer never paints an optimistic progress row.
func (c *sessionCore) applyCompactResult(msg compactResultMsg) bool {
	if msg.err == nil {
		return false
	}
	c.transcript = c.transcript.CommitGlobalError(msg.err)
	return true
}

// applyInterruptResult applies the outcome of an Interrupt call. On error the turn may
// still be live, so the status returns to Running and a faint error entry commits (the
// shell presents it). On success it stays Interrupting — the active loop's TurnInterrupted
// terminal on the subscription (applyTurnStatus) resolves it to Idle.
func (c *sessionCore) applyInterruptResult(msg interruptResultMsg) bool {
	if msg.err != nil {
		c.transcript = c.transcript.CommitGlobalError(msg.err)
		c.status = StatusRunning
		return true
	}
	return false
}

// applyReopenResult applies a /clear reopen outcome (the model is Resetting). The reopen
// command has already closed the old agent before attempting the replacement. On error no
// live session remains: the old agent is dropped, an error entry is presented, and the TUI
// exits. On success the fresh agent is swapped in, shared transport state resets, and a new
// subscription is established against it.
//
// Ordering matters: the agent is swapped to msg.agent BEFORE the re-subscribe command is
// built so it reads the NEW agent (c.subscribe reads c.agent). The old subscription is
// c.sub was closed and cleared when /clear began, so a late subClosedMsg from the old stream
// (nil err — an intentional Close) is a harmless no-op.
func (c *sessionCore) applyReopenResult(msg reopenResultMsg) (tea.Cmd, bool) {
	if msg.handoff != nil {
		msg.handoff.claim()
	}
	c.handoff = nil
	if msg.err != nil {
		c.transcript = c.transcript.CommitGlobalError(msg.err)
		c.agent = nil
		c.terminalErr = msg.err
		return tea.Quit, true
	}
	c.agent = msg.agent
	// The agent swap happened first; reset the uniform transcript before subscribing to the
	// replacement's stream.
	c.transcript = transcriptModel{}
	c.interaction = c.interaction.ClearPrompts()
	c.status = StatusIdle
	// Drop the old session's authoritative active baseline and per-loop running fold. The
	// baseline is re-read from the fresh agent only on the next applySubscribed (not here),
	// so a selection cannot slip past the fresh subscription's setup window.
	c.activeLoopID = uuid.UUID{}
	c.loopRunning = map[uuid.UUID]bool{}
	c.compaction = compactionProjection{}
	// Re-subscribe via the INJECTED filter (c.subscribe) against the freshly swapped
	// agent, so a /clear re-attaches the all-loops scope rather than silently narrowing
	// the post-clear session and starving every subagent projection.
	return c.subscribe(), false
}

// applyPromptResult surfaces a bounded prompt-dispatch (approve/deny/answer) outcome. A
// nil err is a silent success (the gate released; the next events arrive on the stream) —
// returns false. A non-nil err commits a faint, NON-FATAL error entry: the prompt was
// already optimistically popped, and a terminal event clears any siblings — this only adds
// a record the shell presents.
func (c *sessionCore) applyPromptResult(msg promptResultMsg) bool {
	if msg.err == nil {
		return false
	}
	c.transcript = c.transcript.CommitGlobalError(msg.err)
	return true
}

// mapAction turns the interaction model's typed uiAction into the command that drives
// the agent (or mutates transport state). uiNoop returns nil. The prompt-gate actions
// (approve/deny/answer) and interrupt reuse the bounded commands from commands.go;
// submit/runSlash drive the compose path. It returns the driving command plus whether it
// committed an out-of-band entry the shell must present.
func (c *sessionCore) mapAction(a uiAction) (tea.Cmd, bool) {
	switch a.Kind {
	case uiSubmit:
		return c.submit(a.Text)
	case uiRunSlash:
		return c.runSlash(a.Slash)
	case uiApprove:
		// Record the decision so the call's committed card reads "Approved …". The loop
		// emits no decision event, so the keypress is the only source (the gate was
		// remembered by the transcript on PermissionRequested). Both approve actions —
		// once and always-for-this-workspace — read as approved on the card.
		c.transcript = c.transcript.ResolveGate(a.LoopID, a.ToolExecutionID, gateApproved)
		return approveCmd(c.appCtx, c.agent, a.LoopID, a.ToolExecutionID, a.Approval), false
	case uiDeny:
		c.transcript = c.transcript.ResolveGate(a.LoopID, a.ToolExecutionID, gateDenied)
		return denyCmd(c.appCtx, c.agent, a.LoopID, a.ToolExecutionID), false
	case uiAnswer:
		return provideAnswerCmd(c.appCtx, c.agent, a.LoopID, a.ToolExecutionID, a.Text), false
	case uiGateRespond:
		return respondGateCmd(c.appCtx, c.agent, a.GateID, a.GateAction, a.Values), false
	case uiInterrupt:
		// A prompt-raised interrupt (esc on a choice/answer card) carries the gate's own
		// loop, which is the loop the user is being asked about. A zero LoopID falls back
		// to the session-status gate inside interruptRunning.
		return c.interruptRunning(a.LoopID), false
	default: // uiNoop
		return nil, false
	}
}

// submit builds blocks from the composed text and sends them fire-and-forget. The LOOP
// owns queueing now (a submission while Running is queued by the loop, not by the TUI),
// so there is no status branching and no TUI-side queue. It does NOT commit a user row
// optimistically: the authoritative user row is committed from the loop's TurnStarted/
// TurnFoldedInto Message (event-driven), so submit only fires submitCmd. A buildBlocks
// error commits a faint error entry (and a plain user row so the message is preserved in
// scrollback) and sends nothing — the returned bool is true so the shell presents it.
func (c *sessionCore) submit(text string) (tea.Cmd, bool) {
	// Query the capability of the loop this submission actually runs on: agent.Submit
	// targets the session's ACTIVE loop, so effectiveActiveLoopID (activeLoopID when set,
	// else the root fallback) is the right image-capability target — and it avoids a
	// spurious zero-loop fail-closed rejection in the pre-baseline startup window.
	target := c.effectiveActiveLoopID()
	blocks, err := inputpkg.BuildBlocks(text, c.agent.AcceptsImages(target))
	if err != nil {
		// A build error (e.g. an unsupported image on a text-only model) commits the
		// message as a plain user row FIRST, then the error beneath it — the message is
		// preserved in scrollback rather than lost, and composeEnter already emptied the
		// input. The message was NOT sent to the model (the build failed).
		c.transcript = c.transcript.CommitGlobalUserText(text)
		c.transcript = c.transcript.CommitGlobalError(err)
		return nil, true
	}
	return submitCmd(c.appCtx, c.agent, blocks), false
}

// submitToLoop is submit's loop-targeted counterpart: it builds blocks from the composed
// text exactly as submit does and sends them fire-and-forget to a SPECIFIC loop (the
// modern viewport's focused loop) via submitToLoopCmd, rather than the default target. A
// buildBlocks error commits the SAME plain-user-row-plus-faint-error entries and sends
// nothing (returning true so the shell presents them). It exists so the modern shell can
// route a composer submit to the focused loop while reusing the one buildBlocks +
// error-commit path used by Screen.
func (c *sessionCore) submitToLoop(loopID uuid.UUID, text string) (tea.Cmd, bool) {
	blocks, err := inputpkg.BuildBlocks(text, c.agent.AcceptsImages(loopID))
	if err != nil {
		c.transcript = c.transcript.CommitGlobalUserText(text)
		c.transcript = c.transcript.CommitGlobalError(err)
		return nil, true
	}
	return submitToLoopCmd(c.appCtx, c.agent, loopID, blocks), false
}

// compactToLoop requests manual compaction for one exact loop. It deliberately
// has no turn-status gate: the harness coordinates the request at its safe
// boundary whether the focused loop is currently idle or running.
func (c *sessionCore) compactToLoop(loopID uuid.UUID) tea.Cmd {
	return compactToLoopCmd(c.appCtx, c.agent, loopID)
}

// runSlash executes a known slash command. /help commits the listing (the shell presents
// it: the returned bool is true); /clear (only while Idle) flips to Resetting and reopens
// the agent. An unknown or non-actionable command is a no-op.
func (c *sessionCore) runSlash(name string) (tea.Cmd, bool) {
	switch name {
	case "/help":
		c.transcript = c.transcript.CommitSystem(helpText())
		return nil, true
	case "/clear":
		if c.status == StatusIdle {
			c.status = StatusResetting
			if c.sub != nil {
				_ = c.sub.Close()
				c.sub = nil
			}
			c.handoff = newReopenHandoff()
			return reopenAgent(c.appCtx, c.agent, c.openAgent, c.handoff), false
		}
		return nil, false
	default:
		return nil, false
	}
}

// interruptRunning begins an interrupt and returns the bounded Interrupt command, flipping
// to Interrupting. It is the home for both the Esc-in-compose path and the uiInterrupt
// action raised from a choice/answer prompt.
//
// watched is the loop the USER is looking at (Screen's focusedLoopID). It must be part of
// the gate because the two things the user reads as "this loop is busy" both follow focus,
// not the session-active loop: focusedStatus draws the status line from the focused loop's
// own bit, and a composer submit is routed to the focused loop by submitToLoop. Gating on
// c.status alone — which tracks the ACTIVE loop — made Esc a silent no-op in exactly the
// situation that needs it most: watching a running subagent while the active loop sits
// idle, with submissions piling up in the watched loop's queue behind the turn Esc was
// supposed to stop. The session status stays in the gate so the pre-existing behavior (Esc
// while the active loop runs) is preserved; this is strictly a widening.
//
// The loop owns queueing, so there is no TUI-side queue to drop. The harness RETAINS
// human-authored queued input across an ordinary interrupt (loopruntime's
// retainUserQueuedInbox: AgencyUser entries stay in FIFO order, machine-created
// continuation/delegate entries are cancelled), so the queued message becomes the next
// turn rather than being discarded.
func (c *sessionCore) interruptRunning(watched uuid.UUID) tea.Cmd {
	if c.status != StatusRunning && !c.loopRunning[watched] {
		return nil
	}
	c.status = StatusInterrupting
	return interruptTurn(c.appCtx, c.agent)
}

// activePrompt returns the interaction model's active (head) prompt, or nil. It is the
// transport query the shells' key routing and status derivation share (compose vs
// permission/choice/answer, and whether a gate owns the bottom surface).
func (c sessionCore) activePrompt() *prompt {
	im := c.interaction
	return im.ActivePrompt()
}
