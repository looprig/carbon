package presentation

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	contextcount "github.com/looprig/inference/contextcount"
	"github.com/looprig/tui/components"

	"github.com/looprig/tui/styles"
)

// Screen is the MODERN VIEWPORT presentation shell over the shared sessionCore
// transport (embedded). Where the scrollback-first Screen lets the terminal own history,
// Screen owns an alt-screen VIEWPORT the user can scroll and select/copy from while
// content streams: it renders the FOCUSED loop's projection into a hand-rolled
// viewportModel (scroll + drag-select + copy), applies a RETROACTIVE collapse fold
// (ctrl+t + header-click), draws a bottom active-loops bar and the reused composer, and
// subscribes to EVERY loop's live stream (AllLoopsEventFilter) so any focused subagent's tokens
// render live rather than freezing at Enduring StepDone granularity.
//
// The core owns event routing exactly as it does for Screen; Screen adds ONLY the
// viewport presentation. Update delegates transport to the core then re-renders the focused
// projection into the viewport (keeping the auto-follow tail pinned); View composes, top to
// bottom, the viewport content, one status line, a blank gap, an optional completion tray, the
// bottom box, a blank gap, and the active-loops bar, and returns a per-frame View with
// AltScreen + cell-motion mouse (the
// v2 fields the copy-while-scrolling design turns on). Agent() is promoted from the embedded
// sessionCore, so Screen satisfies the composition root's agentHolder through that single
// definition.
//
// Focus switching (Task 8): ctrl+n / ctrl+p cycle focus over the bar's loops and a bar-region
// click focuses the clicked loop, both repointing focusedLoopID and re-rendering that loop's
// projection (focusLoop). Focus is VIEW-ONLY — it never submits/interrupts a loop.
//
// Prompts + feature parity (Task 9): prompt keys route to the reused interaction model
// (handleKey precedence (2)) and the bottom box renders the head gate via the shared surface;
// a pending gate marks its loop with "!" in the bar WITHOUT stealing focus (bar() reads
// pendingGateLoops). A composer submit targets the FOCUSED loop (routeToInteraction — Stage 2:
// submitting while focused on any loop runs a new turn on that loop and stays focused there).
// A cold-restore session repaints its history: Init batches restoreBacklogCmd and handleRestored
// folds the backlog into the transcript + projections + loop table and re-renders. The remaining
// parity items (/clear reopen, esc/ctrl+c interrupt, queued input, image @path rejection) all
// flow through the shared sessionCore, so they are shared with Screen rather than re-implemented.
type Screen struct {
	sessionCore
	runtimeCatalog    RuntimeCatalog
	runtimeController RuntimeController
	runtime           runtimeProjection
	integrations      integrationProjection
	runtimeTray       *components.ValueComplete
	runtimeTrayKind   runtimeTrayKind
	runtimeTrayLoopID uuid.UUID
	// presentation is the synchronous, consumer-supplied session metadata (workspace,
	// fixed access profile, permission diagnostics). It is captured at construction; the
	// footer displays it and commitStartup commits its diagnostics before any gate can
	// arrive. On a reopen it is refreshed from the replacement Agent (SessionPresenter) so a
	// cross-session browser resume shows the RESUMED session's context — see
	// handleReopenResult. A /clear reopen (same session family) retains it when the Agent
	// supplies none; a browser resume clears it, so a stale context is never displayed.
	presentation   SessionPresentation
	sessionBrowser SessionBrowser
	sessionTray    *components.SessionComplete
	pendingResume  SessionID
	resuming       bool

	viewport viewportModel // the scrollable/selectable content window
	collapse collapseState // the retroactive thinking-fold state (ctrl+t + header-click)

	// focusedLoopID selects which loop's projection the viewport renders
	// (projectionFor(focusedLoopID)). It defaults to the agent's ACTIVE loop (Agent.ActiveLoopID,
	// which may differ from the transcript root) and is
	// repointed by focusLoop on a bar click or a ctrl+n/ctrl+p focus chord (Task 8). The
	// whole viewport is a pure VIEW over already-received, already-projected state, so a
	// focus change is a re-render, never a re-subscribe.
	focusedLoopID uuid.UUID

	width, height int
	ready         bool // true once the first WindowSizeMsg sizes the frame

	// startupPending/startupCommitted coordinate the opening banner with the first sized
	// frame: systemReadyMsg may arrive before WindowSizeMsg, so the banner commit is
	// deferred until the frame has a width to render into.
	startupPending   bool
	startupCommitted bool

	// restoring is the initial replay barrier. Subscription events continue to be read and
	// re-armed, but reducer inputs queue in arrival order until restoredMsg installs history.
	restoring     bool
	restoreBuffer []restoreInput
	staleClosings []*staleReopenClose
	// sessionGeneration advances after each successful replacement. Async stale-close
	// diagnostics may only render into the generation that rejected the replacement.
	sessionGeneration uint64
	staleCloseErr     error

	// mouseDragging distinguishes a drag (a text selection) from a plain click (a
	// header/body collapse toggle): a MouseMotion with the button held sets it, and a
	// release with it UNSET is treated as a click. It is reset on each fresh press.
	mouseDragging bool

	// hoveredEntry and hoveredLoop are the two pointer-action affordances. A committed
	// transcript header is stored only when its rendered row is marked clickable; a loop
	// id is stored only when the active bar's HitTest resolves its segment. Zero means no
	// hover. View applies the one-shot blue glow dynamically without rebuilding transcript
	// lines. hoverGlowFrame advances from gray directly to settled pastel blue;
	// hoverGlowEpoch invalidates ticks left behind by an earlier pointer target.
	hoveredEntry   displayID
	hoveredLoop    uuid.UUID
	hoverGlowFrame uint
	hoverGlowEpoch uint64

	// trayGlowFrame drives only the selected completion row's background. Keyboard and
	// pointer selection changes restart it at the neutral panel background; its independent
	// epoch prevents stale ticks from an earlier row advancing the new selection.
	trayGlowFrame uint
	trayGlowEpoch uint64

	// turnStartedAt records each running loop's current-turn start time — set from the
	// loop's TurnStarted event CreatedAt, deleted on its terminal (TurnDone/Failed/
	// Interrupted). The focused loop's entry drives the live status-line elapsed timer
	// (turnElapsed): elapsed is now - turnStartedAt[focused], measured from event/tick
	// timestamps, never a render-time wall clock.
	turnStartedAt map[uuid.UUID]time.Time

	// now is the timer's CLOCK: the latest tickMsg time (seeded from a TurnStarted's
	// CreatedAt so the first frame reads a sane elapsed before the first tick lands). The
	// status line reads it as "now" so elapsed advances only on the 1s tick, never from a
	// per-render time.Now().
	now time.Time

	// ticking guards the 1s tick chain so at most ONE is in flight: a turn becoming active
	// starts it (maybeStartTick); each tick reschedules only while a turn is still active
	// and otherwise stops, so the timer never ticks forever once the session goes idle.
	ticking bool

	// anim holds the status-line animation state: its frame counter flows the status-label +
	// dot gradient. A SINGLE anim tick chain, started at
	// Init and re-armed every blinkInterval, advances it for the life of the program and stops
	// only on quit (quitting) — kept continuous for lifecycle simplicity rather than gated on
	// status. Only ACTIVE states render the status gradient. Hover uses its own short-lived
	// glow tick so it can animate smoothly without speeding up the status shimmer.
	anim animState

	// quitting latches on ctrl+c or /exit so the continuous anim tick chain self-terminates instead of
	// leaking a reschedule past tea.Quit: handleAnim stops re-arming once it is set.
	quitting        bool
	quitAfterReopen bool // ctrl+c or /exit during /clear waits until the handoff result is consumed
}

// tickInterval is the status-line timer's cadence: one tick per second while a turn
// is active, matching the whole-second granularity of formatElapsed.
const tickInterval = time.Second

// tickMsg is one 1s timer tick carrying the tick's own time — the clock the status-line
// elapsed timer advances on (never a render-time wall clock).
type tickMsg struct{ at time.Time }

// tickCmd schedules the next 1s timer tick. It is re-issued from handleTick only while a
// turn is active, so the chain self-terminates when the session goes idle.
func tickCmd() tea.Cmd {
	return tea.Tick(tickInterval, func(t time.Time) tea.Msg { return tickMsg{at: t} })
}

// animMsg is one status-animation tick carrying its own time (unused at the UI; it satisfies
// tea.Tick's func(time.Time) tea.Msg shape). Handling it advances the status gradient phase
// (m.anim) by one frame and re-arms the next tick for the life of the program.
type animMsg time.Time

// animCmd schedules ONE status-animation tick after blinkInterval — Screen's animation cadence
// (commands.go) — delivering an animMsg. handleAnim re-arms it every tick until the shell is
// quitting, so the modern status gradient animates continuously without a per-turn gate. It never
// re-renders the transcript.
func animCmd() tea.Cmd {
	return tea.Tick(blinkInterval, func(t time.Time) tea.Msg { return animMsg(t) })
}

// hoverGlowInterval is the dedicated cadence of the one-shot hover light. One 70ms
// transition switches directly to the settled link color without changing the slower status shimmer.
const hoverGlowInterval = 70 * time.Millisecond

type hoverGlowMsg struct{ epoch uint64 }

func hoverGlowCmd(epoch uint64) tea.Cmd {
	return tea.Tick(hoverGlowInterval, func(time.Time) tea.Msg { return hoverGlowMsg{epoch: epoch} })
}

// trayGlowFinalFrame retains the completion tray's longer four-color background ignition;
// link hover uses its independent two-color transition.
const trayGlowFinalFrame uint = 3

type trayGlowMsg struct{ epoch uint64 }

func trayGlowCmd(epoch uint64) tea.Cmd {
	return tea.Tick(hoverGlowInterval, func(time.Time) tea.Msg { return trayGlowMsg{epoch: epoch} })
}

// loopBarCap is the visible-cap the modern active-loops bar renders under: at most
// this many loop segments show, the rest folding into a "… +N" overflow marker so the bar
// never grows unbounded across a long session's accumulated loops.
const loopBarCap = 8

// liveTailEntryID is the reserved provenance id every live-tail line carries. The live tail
// is NOT a committed entry, and the first committed entry allocates displayID 1, so the zero
// id can never collide with a committed entry: selection/copy still work by (entry, sub)
// identity, and a stray collapse-click on the live tail toggles an id no committed entry owns
// (a harmless no-op).
const liveTailEntryID displayID = 0

// New constructs an idle Screen driving agent, with open as the /clear thunk and
// banner the agent name/description shown as the opening info notice. The session
// subscription delivers EVERY loop's live Ephemeral stream (see AllLoopsEventFilter) — the
// modern mode renders any focused loop's whole live output.
// The viewport starts pinned to the tail (atTail) so streaming content auto-follows, the
// collapse state starts folded (dense; ctrl+t expands), and focus starts on the agent's ACTIVE
// loop (Agent.ActiveLoopID — the session's current default target); a later selection event
// never moves it.
func New(ctx context.Context, agent Agent, open OpenAgent, banner AgentBanner, supplied ...Option) Screen {
	options := screenOptions{}
	for _, apply := range supplied {
		apply(&options)
	}
	runtimeCatalog, _ := agent.(RuntimeCatalog)
	runtimeController, _ := agent.(RuntimeController)
	// The presentation is consumer-supplied (WithSessionPresentation) when a caller
	// explicitly passes it — that is the authoritative override. Otherwise it falls back to
	// the constructed agent's own SessionPresenter capability, mirroring
	// handleReopenResult's reopen-time refresh. Without this fallback, no caller in this
	// codebase ever supplies the option, so the footer's profile/workspace metadata stayed
	// blank until the first /clear reopen — the only path that ever type-asserted the agent.
	presented := options.presentation
	if !options.presentationSet {
		if presenter, ok := agent.(SessionPresenter); ok {
			presented = presenter.SessionPresentation()
		}
	}
	m := Screen{
		sessionCore:       newSessionCore(ctx, agent, open, banner),
		runtimeCatalog:    runtimeCatalog,
		runtimeController: runtimeController,
		runtime:           newRuntimeProjection(),
		integrations:      newIntegrationProjection(),
		presentation:      presented,
		sessionBrowser:    options.sessionBrowser,
		viewport:          viewportModel{atTail: true},
		collapse:          newCollapseState(),
		focusedLoopID:     agent.ActiveLoopID(),
		turnStartedAt:     make(map[uuid.UUID]time.Time),
		restoring:         true,
		trayGlowFrame:     trayGlowFinalFrame,
	}
	m.interaction = styleComposer(m.interaction)
	if runtimeCatalog != nil && runtimeController != nil {
		m.interaction.slashCommands = append(m.interaction.slashCommands,
			components.SlashCmd{Name: "/mode", Desc: "change the focused loop mode"},
			components.SlashCmd{Name: "/model", Desc: "change the focused loop model"},
			components.SlashCmd{Name: "/effort", Desc: "change reasoning effort"},
		)
	}
	if options.sessionBrowser != nil {
		m.interaction.slashCommands = append(m.interaction.slashCommands,
			components.SlashCmd{Name: "/sessions", Desc: "resume a previous session"},
			components.SlashCmd{Name: "/resume", Desc: "resume a previous session"},
		)
	}
	return m
}

// composerMinLines is the modern composer's default visible text height. One text row plus
// the vertical padding keeps the empty panel compact while still auto-growing to maxInputLines.
const composerMinLines = 1

// composerPadV is the modern composer's inner vertical padding: 1 gray pad row above and below
// the text, so the composer reads as a padded card matching the user-message card (padUserCard).
// The text region still auto-grows to maxInputLines as the user types — the padding only frames
// it. Scrollback's composer keeps 0 (it never calls SetVerticalPadding), staying bare.
const composerPadV = 1

// styleComposer applies the MODERN composer treatment to in's input box — the 1-line
// default height, the gray panel fill (styles.PanelBg), and one inner vertical pad row above
// and below the text (composerPadV, matching the user card) — and returns the updated model.
// It runs at every site that
// installs a fresh (default 1-line, background-free, unpadded) interaction for a Screen.
// The /clear reopen keeps the same input
// (ClearPrompts preserves it), so it needs no re-apply. Scrollback's Screen never calls this,
// so its composer stays byte-identical.
func styleComposer(in interactionModel) interactionModel {
	in.input.SetMinLines(composerMinLines)
	in.input.SetBackground(styles.PanelBg)
	in.input.SetVerticalPadding(composerPadV)
	return in
}

// Init focuses the composer (starting the cursor blink), schedules the opening banner
// (systemReadyMsg), schedules the cold-restore repaint, and attaches the session-lifetime
// ALL-LOOPS subscription (m.subscribe uses AllLoopsEventFilter). restoreBacklogCmd
// folds a restored session's historical Enduring backlog off the update loop. Live events may
// arrive first; the restore barrier buffers and continuously re-arms them, then applies them in
// arrival order after restoredMsg installs history. An empty backlog simply releases the barrier.
func (m Screen) Init() tea.Cmd {
	cmds := []tea.Cmd{
		m.interaction.input.Focus(),
		func() tea.Msg { return systemReadyMsg{} },
		restoreBacklogCmd(m.appCtx, m.agent),
		m.subscribe(),
		// The anim tick chain runs continuously for the life of the program (started here,
		// re-armed by handleAnim, stopped only on quit). It shimmers the gradient only for
		// ACTIVE states; while idle it just recomposes the static faint status line.
		animCmd(),
	}
	return tea.Batch(cmds...)
}

// Update advances the model. It is a value receiver so Screen satisfies tea.Model;
// the mutating handlers take a pointer to the addressable receiver and Update returns the
// updated value. Note the two-statement pattern for the pointer-receiver handlers (cmd :=
// …; return m, cmd): a `return m, m.handle(...)` would evaluate the first result (the OLD
// m) before the handler mutates it, stranding the mutation.
func (m Screen) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		return m.handleResize(msg)
	case tea.KeyPressMsg:
		return m.handleKey(msg)
	case tea.PasteMsg:
		return m.routeToEditor(msg)
	case tea.MouseMsg:
		cmd := m.handleMouse(msg)
		return m, cmd
	case eventMsg:
		if m.restoring {
			cmd := m.bufferRestoreEvent(msg)
			return m, cmd
		}
		cmd := m.handleEvent(msg.ev)
		if !m.pendingResume.IsZero() && isCompactionTerminal(msg.ev) && !m.compaction.IsActive(m.focusedLoopID) && !m.loopRunning[m.effectiveActiveLoopID()] {
			m.status = StatusIdle
		}
		if !m.pendingResume.IsZero() && isResumeTerminal(msg.ev) && m.status == StatusIdle {
			resume := m.beginSessionResume(m.pendingResume)
			m.pendingResume = SessionID{}
			return m, tea.Batch(cmd, resume)
		}
		return m, cmd
	case restoredMsg:
		cmd := m.handleRestored(msg)
		return m, cmd
	case subscribedMsg:
		cmd := m.handleSubscribed(msg)
		return m, cmd
	case subClosedMsg:
		cmd := m.handleSubClosed(msg)
		return m, cmd
	case submitResultMsg:
		if m.restoring {
			m.restoreBuffer = append(m.restoreBuffer, restoreInput{kind: restoreInputSubmit, submit: msg})
			return m, nil
		}
		cmd := m.handleSubmitResult(msg)
		return m, cmd
	case compactResultMsg:
		cmd := m.handleCompactResult(msg)
		return m, cmd
	case interruptResultMsg:
		cmd := m.handleInterruptResult(msg)
		if msg.err != nil || !msg.cancelled {
			m.pendingResume = SessionID{}
			m.status = StatusIdle
			m.deriveActiveStatus()
			if msg.err == nil {
				m.transcript = m.transcript.CommitGlobalNotice(noticeInfo, "Session is still active; resume was cancelled")
				m.rerender()
			}
		}
		return m, cmd
	case reopenResultMsg:
		if m.restoring {
			cmd := m.rejectStaleReopenResult(msg)
			return m, cmd
		}
		cmd := m.handleReopenResult(msg)
		return m, cmd
	case closeForQuitResultMsg:
		cmd := m.handleCloseForQuitResult(msg)
		return m, cmd
	case staleReopenCloseMsg:
		cmd := m.handleStaleReopenClose(msg)
		return m, cmd
	case promptResultMsg:
		cmd := m.handlePromptResult(msg)
		return m, cmd
	case systemReadyMsg:
		cmd := m.handleSystemReady()
		return m, cmd
	case tickMsg:
		cmd := m.handleTick(msg)
		return m, cmd
	case animMsg:
		cmd := m.handleAnim()
		return m, cmd
	case hoverGlowMsg:
		cmd := m.handleHoverGlow(msg)
		return m, cmd
	case trayGlowMsg:
		cmd := m.handleTrayGlow(msg)
		return m, cmd
	case runtimeChoicesMsg:
		cmd := m.handleRuntimeChoices(msg)
		return m, cmd
	case runtimeMutationMsg:
		cmd := m.handleRuntimeMutation(msg)
		return m, cmd
	case sessionsListedMsg:
		cmd := m.handleSessionsListed(msg)
		return m, cmd
	}
	// Anything the arms above do not claim belongs to a widget, not to the Screen: the
	// textarea's reply to its own ctrl+v clipboard read (an UNEXPORTED type, so it can
	// only be forwarded blind — no case could ever match it) and the cursor-blink tick.
	// Dropping them here is what made ctrl+v a silent no-op, so they are forwarded to the
	// editor under the same precedence a paste gets. ForwardToEditor only lets a
	// text-entry mode see them, and gates the completion-panel refresh on the value
	// actually changing, so a blink tick never reaches the filesystem.
	return m.routeToEditor(msg)
}

// FinalizeHandoff extends the core ownership finalizer with any stale replacement cleanup
// still in flight. agentCloseHandoff is sync.Once-backed, so a concurrent command completion
// and finalization close each rejected replacement exactly once.
func (m Screen) FinalizeHandoff() error {
	err := errors.Join(m.sessionCore.FinalizeHandoff(), m.staleCloseErr)
	for _, closing := range m.staleClosings {
		err = errors.Join(err, closing.handoff.close())
	}
	return err
}

// handleResize stores the terminal dimensions, resizes the composer, and commits a deferred
// opening banner (if systemReadyMsg arrived first). A WIDTH change reflows every rendered
// line (they were laid out at the old width) and a deferred banner commit changed the buffer
// — either needs a full rerender; a PURE height change only needs a size sync (the lines are
// unchanged, so re-rendering the transcript is wasted work).
func (m Screen) handleResize(msg tea.WindowSizeMsg) (tea.Model, tea.Cmd) {
	widthChanged := msg.Width != m.width
	m.width, m.height = msg.Width, msg.Height
	m.ready = true
	m.interaction.input.Resize(msg.Width)
	bannerCommitted := false
	if m.startupPending && !m.startupCommitted {
		m.commitStartup()
		bannerCommitted = true
	}
	if widthChanged || bannerCommitted {
		m.rerender()
	} else {
		m.resize()
	}
	return m, nil
}

// handleSystemReady commits the opening transcript once the frame has its first size. If
// systemReadyMsg arrives before WindowSizeMsg the commit is deferred (startupPending) so the
// banner renders in a real frame instead of at width 0.
func (m *Screen) handleSystemReady() tea.Cmd {
	if m.startupCommitted {
		return nil
	}
	if !m.ready {
		m.startupPending = true
		return nil
	}
	m.commitStartup()
	m.rerender()
	return nil
}

func (m *Screen) handleRuntimeChoices(msg runtimeChoicesMsg) tea.Cmd {
	if msg.err != nil {
		m.transcript = m.transcript.CommitGlobalError(fmt.Errorf("load runtime choices: %w", msg.err))
		m.rerender()
		return nil
	}
	tray := components.NewValueComplete(msg.items, "")
	if tray == nil {
		m.transcript = m.transcript.CommitGlobalNotice(noticeInfo, "No choices are available")
		m.rerender()
		return nil
	}
	m.runtimeTray = tray
	m.runtimeTrayKind = msg.kind
	m.runtimeTrayLoopID = msg.loopID
	m.resize()
	return m.startTrayGlow()
}

func (m *Screen) handleRuntimeMutation(msg runtimeMutationMsg) tea.Cmd {
	if msg.err != nil {
		m.transcript = m.transcript.CommitGlobalError(fmt.Errorf("change runtime: %w", msg.err))
		m.rerender()
	}
	// Successful mutations deliberately do not update current display. The durable event is
	// the acknowledgement and the runtime projection will fold it when it arrives.
	return nil
}

func (m *Screen) handleSessionsListed(msg sessionsListedMsg) tea.Cmd {
	if msg.err != nil {
		m.transcript = m.transcript.CommitGlobalError(fmt.Errorf("list sessions: %w", msg.err))
		m.rerender()
		return nil
	}
	items := make([]components.SessionItem, 0, len(msg.sessions))
	now := time.Now()
	for _, session := range msg.sessions {
		title := strings.TrimSpace(session.Title)
		if title == "" {
			title = "Untitled session"
		}
		id := session.ID.String()
		shortID := id
		if len(shortID) > 8 {
			shortID = shortID[:8]
		}
		items = append(items, components.SessionItem{
			ID:       id,
			Title:    title,
			State:    session.State,
			Activity: relativeActivity(now, session.LastActiveAt),
			LastUsed: lastUsedDate(session),
			ShortID:  shortID,
		})
	}
	m.sessionTray = components.NewSessionComplete(items)
	if m.sessionTray == nil {
		m.transcript = m.transcript.CommitGlobalNotice(noticeInfo, "No previous sessions")
		m.rerender()
		return nil
	}
	m.resize()
	return m.startTrayGlow()
}

// lastUsedDate is the picker's second-row date: when the session was actually last used.
// LastActiveAt stays the zero value for a session that never ran a turn, so it falls back
// to CreatedAt rather than showing a blank/zero date for a session that was merely opened.
func lastUsedDate(session SessionSummary) string {
	at := session.LastActiveAt
	if at.IsZero() {
		at = session.CreatedAt
	}
	return at.Format("2006-01-02")
}

func relativeActivity(now, at time.Time) string {
	if at.IsZero() {
		return ""
	}
	d := now.Sub(at)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m ago"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h ago"
	default:
		return strconv.Itoa(int(d/(24*time.Hour))) + "d ago"
	}
}

// beginSessionResume closes the current agent and opens the selected one via reopenAgent.
// A resume can be REJECTED (e.g. harness's RestoreRejectedError on config drift) or
// otherwise fail after old is already closed and its exclusive workspace lease released —
// there is no "keep the old session" fallback at that point. Rather than let that surface
// as a fatal reopen failure (old, already-closed one moment; whole TUI process exiting the
// next), a failed resume falls back to opening a FRESH session — the same path /clear
// uses, so the replacement is a real, live agent and every existing status/quit/interaction
// invariant keeps holding — carrying the original error forward as a non-fatal warning
// (reopenResultMsg.warning) instead of losing the session and the terminal in one step.
func (m *Screen) beginSessionResume(id SessionID) tea.Cmd {
	if m.sessionBrowser == nil || id.IsZero() || m.status == StatusResetting {
		return nil
	}
	m.status = StatusResetting
	m.resuming = true
	if m.sub != nil {
		_ = m.sub.Close()
		m.sub = nil
	}
	m.handoff = newReopenHandoff()
	var resumeErr error
	cmd := reopenAgent(m.appCtx, m.agent, func(ctx context.Context) (Agent, error) {
		agent, err := m.sessionBrowser.ResumeSession(ctx, id)
		if err == nil {
			return agent, nil
		}
		resumeErr = err
		return m.openAgent(ctx)
	}, m.handoff)
	return func() tea.Msg {
		msg := cmd()
		result, ok := msg.(reopenResultMsg)
		if !ok || resumeErr == nil {
			return msg
		}
		if result.err != nil {
			result.err = fmt.Errorf("resume rejected: %w; fallback to a fresh session also failed: %w", resumeErr, result.err)
		} else {
			result.warning = resumeErr
		}
		return result
	}
}

func isResumeTerminal(ev event.Event) bool {
	switch ev.(type) {
	case event.TurnDone, event.TurnFailed, event.TurnInterrupted, event.CompactionCommitted, event.CompactionRejected:
		return true
	default:
		return false
	}
}

func isCompactionTerminal(ev event.Event) bool {
	switch ev.(type) {
	case event.CompactionCommitted, event.CompactionRejected:
		return true
	default:
		return false
	}
}

// commitStartup commits the opening banner once per session. It reads the identity from
// the current agent at commit time, so a /clear replacement cannot display the prior
// session's ID. The banner is a rendered opening entry, NOT a turn or command (no Submit,
// no loop, never in model context). The viewport renders it directly, so there is no
// separate startup surface to manage.
func (m *Screen) commitStartup() {
	if m.startupCommitted {
		return
	}
	m.startupPending = false
	m.startupCommitted = true
	m.transcript = m.transcript.CommitGlobalNotice(noticeInfo, m.banner.bannerText(m.agent.SessionID()))
	// Permission diagnostics for manual, out-of-catalog allow families are surfaced in
	// the startup metadata area — committed HERE, at the opening banner, so they are
	// visible BEFORE the first permission gate can ever arrive (interactive consumers
	// must show them before the first prompt). They are display-ready strings from the
	// consumer; the TUI renders them as faint warning notices and infers nothing.
	for _, diagnostic := range m.presentation.diagnostics() {
		m.transcript = m.transcript.CommitGlobalNotice(noticeWarn, diagnostic)
	}
}

// handleEvent delegates one subscription event to the shared core (which routes it
// through BOTH reducers, derives the active-loop turn status, and re-arms the reader) and then
// re-renders the FOCUSED projection into the viewport, keeping the auto-follow tail pinned.
// The turn-phase cue is unused in Stage 1 (a static status line; Task 9 threads the blink).
//
// Before the core folds the event it stamps the model clock (m.now) onto an unstamped
// Ephemeral TokenDelta (stampEphemeralClock): the harness publishes TokenDeltas UNSTAMPED
// (only Enduring events get a Factory CreatedAt — see pkg/loop publish; per-token crypto/rand
// is deliberately avoided), so a thinking delta reaches the transcript reducer with a ZERO
// CreatedAt and its "thought for Nsec" span would always be 0. Stamping it with the SAME
// clock the turn timer uses (m.now, seeded from TurnStarted.CreatedAt and advanced by the 1s
// tick) gives the reducer a real, second-granularity timestamp — so every loop projection
// measures a genuine thinking span, deterministically
// driven by injected event/tick times (no render-time wall clock).
func (m *Screen) handleEvent(ev event.Event) tea.Cmd {
	ev = stampEphemeralClock(ev, m.now)
	m.runtime = m.runtime.ApplyEvent(ev)
	m.integrations = m.integrations.ApplyEvent(ev)
	rearm, _ := m.sessionCore.handleEvent(ev)
	// Commit the "turn ran for Ns" line BEFORE trackTurnClock clears the start time it reads.
	m.commitTurnRanNotice(ev)
	m.trackTurnClock(ev)
	m.rerender()
	// A turn becoming active (re)starts the 1s status-line tick if none is in flight; the
	// tick self-terminates once every turn has ended (handleTick), so it never leaks past
	// an idle session.
	return tea.Batch(rearm, m.maybeStartTick())
}

// bufferRestoreEvent retains one raw delivery behind the initial replay barrier while
// immediately re-arming the subscription reader. Token timestamps are deliberately deferred:
// ordered drain must let an earlier TurnStarted advance m.now before stamping a later delta.
func (m *Screen) bufferRestoreEvent(msg eventMsg) tea.Cmd {
	m.restoreBuffer = append(m.restoreBuffer, restoreInput{kind: restoreInputEvent, delivery: msg})
	if m.sub == nil {
		return nil
	}
	return subNext(m.sub)
}

// stampEphemeralClock returns ev with the model clock stamped onto its Header.CreatedAt when
// ev is an UNSTAMPED Ephemeral TokenDelta — the timing source for a committed thinking
// entry's "thought for Nsec" span. The harness never stamps Ephemeral events (they are never
// journaled and per-token crypto/rand is avoided), so a TokenDelta arrives with a zero
// CreatedAt; the reducer's recordThinking/recordNonThinking would then never seed a start and
// the span would collapse to 0. Stamping the delta with clock (m.now) — the same clock the
// turn timer advances on the 1s tick — feeds the reducer a real timestamp through its
// existing per-loop timing machinery, so subagent
// thinking is timed for free. It touches ONLY a zero-CreatedAt TokenDelta: any other event
// (including an already-stamped delta, e.g. a test that stamps its own) is returned unchanged,
// so trackTurnClock still reads the authoritative TurnStarted.CreatedAt. A zero clock (no turn
// has seeded m.now yet) is a no-op — the delta stays unstamped and reads a 0 span, the safe
// bare-"thought" fallback.
func stampEphemeralClock(ev event.Event, clock time.Time) event.Event {
	td, ok := ev.(event.TokenDelta)
	if !ok || clock.IsZero() || !td.CreatedAt.IsZero() {
		return ev
	}
	td.Header.CreatedAt = clock
	return td
}

// trackTurnClock maintains turnStartedAt from a loop's turn-lifecycle events: a TurnStarted
// records its start time (and seeds the clock so the first pre-tick frame reads a sane
// elapsed); any terminal (TurnDone/TurnFailed/TurnInterrupted) clears it. Every loop's turn
// events reach here through the all-loops stream. Non-turn events are ignored.
func (m *Screen) trackTurnClock(ev event.Event) {
	h := ev.EventHeader()
	switch ev.(type) {
	case event.TurnStarted:
		if m.turnStartedAt == nil {
			m.turnStartedAt = make(map[uuid.UUID]time.Time)
		}
		m.turnStartedAt[h.LoopID] = h.CreatedAt
		if m.now.Before(h.CreatedAt) {
			m.now = h.CreatedAt
		}
	case event.TurnDone, event.TurnFailed, event.TurnInterrupted:
		delete(m.turnStartedAt, h.LoopID)
	}
}

// commitTurnRanNotice appends a faint "○ turn ran for Ns" harness line when a loop's
// turn completes (event.TurnDone) — the frozen form of the live status-bar timer, so a finished
// turn leaves a record of how long it took. It MUST run before trackTurnClock clears
// turnStartedAt: the span is the TurnDone timestamp less turnStartedAt[loopID] (both Enduring
// CreatedAt values, so it is the TRUE turn duration, not the possibly-stale tick clock),
// formatted via formatElapsed ("25s" / "2m 5s"); a zero terminal timestamp falls back to the
// tick clock (m.now). It is a no-op for a failed/interrupted turn (those commit their own error/tombstone — a
// "ran for" summary would double up), and when no turn start was recorded (a cold restore
// carries no streaming timestamps).
func (m *Screen) commitTurnRanNotice(ev event.Event) {
	td, ok := ev.(event.TurnDone)
	if !ok {
		return
	}
	loopID := td.EventHeader().LoopID
	start, ok := m.turnStartedAt[loopID]
	if !ok || start.IsZero() {
		return
	}
	end := td.EventHeader().CreatedAt
	if end.IsZero() {
		end = m.now // the terminal carried no timestamp — fall back to the tick clock
	}
	d := end.Sub(start)
	if d < 0 {
		d = 0
	}
	m.transcript = m.transcript.CommitHarnessFor(loopID, "turn ran for "+formatElapsed(d))
}

// maybeStartTick launches the 1s status-line tick chain, but only when a turn is active
// AND no tick is already in flight — so a second concurrent TurnStarted never spawns a
// second chain, and an idle session never ticks. Returns nil when a tick is unnecessary.
func (m *Screen) maybeStartTick() tea.Cmd {
	if m.ticking || len(m.turnStartedAt) == 0 {
		return nil
	}
	m.ticking = true
	return tickCmd()
}

// handleTick advances the timer clock to the tick's time and reschedules the tick ONLY
// while a turn is still active, so the chain stops the moment the session goes idle. It
// does NOT re-render the viewport: the transcript is unchanged, and the status line (which
// carries the live elapsed) is recomposed by View every frame — so advancing the clock and
// letting View redraw is the whole update.
func (m *Screen) handleTick(msg tickMsg) tea.Cmd {
	m.ticking = false
	if msg.at.After(m.now) {
		m.now = msg.at
	}
	return m.maybeStartTick()
}

// handleAnim advances the status gradient one frame and re-arms the next anim tick,
// UNLESS the shell is quitting (then the chain stops so no tick leaks past tea.Quit). It is a
// PURE recompose: it advances ONLY m.anim (the gradient phase) and never re-renders the
// transcript buffer (renderFocused/SetLines) — the transcript is unchanged on an anim tick.
// View recomposes the status line from the new frame, so advancing the frame and returning
// the reschedule is the whole update. It runs continuously, though the status shimmers only
// while active. The turn timer and hover glow keep their own cadences.
func (m *Screen) handleAnim() tea.Cmd {
	if m.quitting {
		return nil
	}
	m.anim = m.anim.advance()
	return animCmd()
}

// handleHoverGlow advances only the currently hovered target's one-shot light. An epoch
// mismatch means the pointer moved since this tick was scheduled, so the stale chain ends.
func (m *Screen) handleHoverGlow(msg hoverGlowMsg) tea.Cmd {
	if msg.epoch != m.hoverGlowEpoch || (m.hoveredEntry == 0 && m.hoveredLoop == (uuid.UUID{})) {
		return nil
	}
	if m.hoverGlowFrame >= hoverGlowFinalFrame {
		return nil
	}
	m.hoverGlowFrame++
	if m.hoverGlowFrame >= hoverGlowFinalFrame {
		return nil
	}
	return hoverGlowCmd(msg.epoch)
}

func (m *Screen) handleTrayGlow(msg trayGlowMsg) tea.Cmd {
	if msg.epoch != m.trayGlowEpoch || !m.completionTrayOpen() {
		return nil
	}
	if m.trayGlowFrame >= trayGlowFinalFrame {
		return nil
	}
	m.trayGlowFrame++
	if m.trayGlowFrame >= trayGlowFinalFrame {
		return nil
	}
	return trayGlowCmd(msg.epoch)
}

func (m Screen) completionTrayOpen() bool {
	return m.sessionTray != nil || m.runtimeTray != nil || m.interaction.slash != nil || m.interaction.files != nil
}

func (m *Screen) startTrayGlow() tea.Cmd {
	m.trayGlowFrame = 0
	m.trayGlowEpoch++
	return trayGlowCmd(m.trayGlowEpoch)
}

// handleRestored releases the initial replay barrier. A non-empty historical fold installs
// first, preserving shell-global startup/error rows and composer draft; buffered subscription
// events then pass through the normal reducers in arrival order. Empty/error replay also drains
// the buffer. Startup flags are deliberately untouched so message ordering cannot duplicate or
// suppress the banner.
func (m *Screen) handleRestored(msg restoredMsg) tea.Cmd {
	buffered := m.restoreBuffer
	m.restoreBuffer = nil
	m.restoring = false
	if msg.err != nil {
		m.transcript = m.transcript.CommitGlobalError(msg.err)
	} else if msg.eventCount != 0 {
		m.transcript = installRestoredTranscript(m.transcript, msg.transcript)
		m.interaction = installRestoredInteraction(m.interaction, msg.interaction)
		m.compaction = msg.compaction
		m.runtime = msg.runtime
	}
	for _, input := range buffered {
		switch input.kind {
		case restoreInputEvent:
			ev := stampEphemeralClock(input.delivery.ev, m.now)
			if id := ev.EventHeader().EventID; !id.IsZero() {
				if _, replayed := msg.eventIDs[id]; replayed {
					continue
				}
			}
			m.runtime = m.runtime.ApplyEvent(ev)
			m.integrations = m.integrations.ApplyEvent(ev)
			_, _ = m.sessionCore.handleEvent(ev) // reader was re-armed when the delivery was buffered
			m.commitTurnRanNotice(ev)
			m.trackTurnClock(ev)
		case restoreInputSubmit:
			m.sessionCore.applySubmitResult(input.submit)
		}
	}
	m.rerender()
	return m.maybeStartTick()
}

type restoreInputKind uint8

const (
	restoreInputEvent restoreInputKind = iota
	restoreInputSubmit
)

// restoreInput is one reducer input observed while initial replay is pending. Event deliveries
// and submit outcomes share one slice so their cross-kind arrival order is preserved.
type restoreInput struct {
	kind     restoreInputKind
	delivery eventMsg
	submit   submitResultMsg
}

// installRestoredTranscript installs historical reducer state while preserving shell-global
// startup/error rows already committed before replay completed. Historical IDs are rebased
// above the current allocator so preserved globals never collide with restored rows.
func installRestoredTranscript(current, restored transcriptModel) transcriptModel {
	offset := current.nextID
	if offset != 0 {
		restored.global = rebaseEntries(restored.global, offset)
		if restored.projections != nil {
			next := make(map[uuid.UUID]*loopProjection, len(restored.projections))
			for id, p := range restored.projections {
				if p == nil {
					next[id] = nil
					continue
				}
				cp := *p
				cp.committed = rebaseEntries(cp.committed, offset)
				next[id] = &cp
			}
			restored.projections = next
		}
		restored.nextID += offset
	}
	restored.global = append(append([]entry(nil), current.global...), restored.global...)
	return restored
}

// installRestoredInteraction preserves the live composer/draft while installing historical
// prompts. When history has no prompts the already-styled live interaction is authoritative;
// buffered live prompt events are applied normally after this install.
func installRestoredInteraction(current, restored interactionModel) interactionModel {
	if len(restored.pending) == 0 {
		return current
	}
	restored.input = current.input
	restored.slash = current.slash
	restored.files = current.files
	restored.slashCommands = append([]components.SlashCmd(nil), current.slashCommands...)
	restored.composeDraft = current.input.Value()
	return restored
}

func rebaseEntries(entries []entry, offset displayID) []entry {
	if entries == nil {
		return nil
	}
	next := append([]entry(nil), entries...)
	for i := range next {
		next[i].ID += offset
	}
	return next
}

// handleSubscribed installs the session-lifetime subscription via the core and starts the
// reader. On error the core commits a fatal error entry the viewport re-renders; on success
// it returns subNext, the single reader driving every subsequent event.
func (m *Screen) handleSubscribed(msg subscribedMsg) tea.Cmd {
	cmd, present := m.sessionCore.applySubscribed(msg)
	if present {
		m.rerender()
	}
	return cmd
}

// handleSubClosed reacts to the reader observing a closed channel. A nil err is an
// intentional Close (nothing to surface); a non-nil err (hub-forced loss) commits an error
// entry the viewport re-renders.
func (m *Screen) handleSubClosed(msg subClosedMsg) tea.Cmd {
	if m.sessionCore.applySubClosed(msg) {
		m.rerender()
	}
	return nil
}

// handleSubmitResult surfaces a fire-and-forget Submit outcome via the core. Success commits
// nothing (the authoritative user row arrives from the loop's TurnStarted); a non-nil err
// commits a faint error entry the viewport re-renders.
func (m *Screen) handleSubmitResult(msg submitResultMsg) tea.Cmd {
	if m.sessionCore.applySubmitResult(msg) {
		m.rerender()
	}
	return nil
}

// handleCompactResult keeps successful dispatch silent and renders an immediate
// failure as a non-fatal error notice. Progress itself is driven by compaction
// events, never an optimistic slash-command spinner.
func (m *Screen) handleCompactResult(msg compactResultMsg) tea.Cmd {
	if m.sessionCore.applyCompactResult(msg) {
		m.rerender()
	}
	return nil
}

// handleInterruptResult applies an Interrupt outcome via the core. On error it returns to
// Running with a faint error entry the viewport re-renders; on success it stays Interrupting
// until the loop's TurnInterrupted terminal lands Idle.
func (m *Screen) handleInterruptResult(msg interruptResultMsg) tea.Cmd {
	if m.sessionCore.applyInterruptResult(msg) {
		m.rerender()
	}
	return nil
}

// handlePromptResult surfaces a bounded prompt-dispatch outcome via the core. A nil err is a
// silent success; a non-nil err commits a faint error entry the viewport re-renders.
func (m *Screen) handlePromptResult(msg promptResultMsg) tea.Cmd {
	if m.sessionCore.applyPromptResult(msg) {
		m.rerender()
	}
	return nil
}

// handleReopenResult applies a /clear reopen outcome. The core owns the transport ordering —
// on error no live agent remains, so it renders the error and exits; on success it swaps
// in the fresh agent, resets the shared transport, and
// re-subscribes with the INJECTED (all-loops) filter. Screen then resets its own
// viewport/collapse/focus so no remnant of the old session survives the swap.
func (m *Screen) handleReopenResult(msg reopenResultMsg) tea.Cmd {
	cmd, present := m.sessionCore.applyReopenResult(msg)
	if present {
		m.rerender()
		return cmd
	}
	// Successful reopen: reset the modern presentation to match the fresh session. Focus
	// initializes from the replacement agent's ActiveLoopID (the session's current default
	// target), mirroring New, and is set here once, never from a later
	// selection event.
	m.sessionGeneration++
	m.focusedLoopID = m.agent.ActiveLoopID()
	m.runtimeCatalog, _ = m.agent.(RuntimeCatalog)
	m.runtimeController, _ = m.agent.(RuntimeController)
	m.runtime = newRuntimeProjection()
	// The integrations belong to the session being replaced: its bindings are torn
	// down with it, so the replacement's own statuses are the only ones that can be
	// true. Carrying the old readings over would render a closed session's servers
	// as live until something contradicted them.
	m.integrations = newIntegrationProjection()
	// Refresh the security-context presentation (fixed profile, workspace, pre-gate
	// diagnostics) for the replacement session. A cross-session browser resume can land on
	// a DIFFERENT workspace root and DIFFERENT fixed access profile, so the footer and the
	// diagnostics commitStartup re-commits below MUST reflect the RESUMED session, never the
	// prior one. The replacement Agent is the authority: if it implements SessionPresenter it
	// supplies its own presentation (the product composition root's contract). If it does not, fail safe
	// per path — clear on a cross-session resume (show nothing, never a different session's
	// context) but retain on a /clear reopen, which is the same session family (same
	// workspace + fixed profile) and whose construction-time value is still correct.
	if presenter, ok := m.agent.(SessionPresenter); ok {
		m.presentation = presenter.SessionPresentation()
	} else if m.resuming {
		m.presentation = SessionPresentation{}
	}
	m.resuming = false
	m.collapse = newCollapseState()
	m.viewport = viewportModel{atTail: true}
	m.startupPending = false
	m.startupCommitted = false
	// /clear installs a brand-new session without re-running Init, so no later
	// systemReadyMsg will rebuild its opening surface. Commit the replacement's banner
	// here: otherwise a successful swap leaves an empty viewport and looks exactly like
	// the old session merely closed.
	m.commitStartup()
	if msg.warning != nil {
		m.transcript = m.transcript.CommitGlobalNotice(noticeWarn, "couldn't resume that session (\""+msg.warning.Error()+"\") — opened a new session instead")
	}
	m.rerender()
	if m.quitAfterReopen {
		m.closing = newAgentCloseHandoff(m.agent)
		return closeAgentForQuit(m.closing)
	}
	m.restoring = true
	return tea.Batch(cmd, restoreBacklogCmd(m.appCtx, m.agent))
}

func (m *Screen) rejectStaleReopenResult(msg reopenResultMsg) tea.Cmd {
	if msg.handoff != nil {
		msg.handoff.claim()
		if m.handoff == msg.handoff {
			m.handoff = nil
		}
	}
	if msg.err != nil {
		m.commitStaleReopenDiagnostic(fmt.Errorf("reject stale reopen result: %w", msg.err))
	}
	if msg.agent == nil {
		return nil
	}
	closing := &staleReopenClose{
		handoff:    newAgentCloseHandoff(msg.agent),
		generation: m.sessionGeneration,
	}
	m.staleClosings = append(m.staleClosings, closing)
	return closeStaleReopen(closing)
}

func (m *Screen) handleStaleReopenClose(msg staleReopenCloseMsg) tea.Cmd {
	for i, closing := range m.staleClosings {
		if closing == msg.closing {
			m.staleClosings = append(m.staleClosings[:i], m.staleClosings[i+1:]...)
			break
		}
	}
	if msg.closeErr == nil {
		return nil
	}
	diagnostic := fmt.Errorf("close stale reopen replacement: %w", msg.closeErr)
	if msg.closing.generation != m.sessionGeneration {
		// Preserve a cleanup failure for runtime finalization without painting a
		// predecessor-session diagnostic into the successor transcript.
		m.staleCloseErr = errors.Join(m.staleCloseErr, diagnostic)
		return nil
	}
	m.commitStaleReopenDiagnostic(diagnostic)
	return nil
}

func (m *Screen) commitStaleReopenDiagnostic(err error) {
	if err == nil {
		return
	}
	// This is a defense-in-depth diagnostic only: the valid old restore remains
	// authoritative, so rejecting a stale result must not become a terminal error.
	m.transcript = m.transcript.CommitGlobalError(err)
	m.rerender()
}

// handleCloseForQuitResult consumes the deferred replacement close before clearing ownership.
// A close failure is terminal and retained for runtime.Run; either way the close is complete.
func (m *Screen) handleCloseForQuitResult(msg closeForQuitResultMsg) tea.Cmd {
	if msg.handoff != m.closing {
		return nil
	}
	m.closing = nil
	m.agent = nil
	if msg.err != nil {
		m.terminalErr = fmt.Errorf("close replacement on deferred quit: %w", msg.err)
		m.transcript = m.transcript.CommitGlobalError(m.terminalErr)
	}
	return tea.Quit
}

// beginGracefulQuit owns the shared ctrl+c and /exit shutdown choreography. Once
// quitting is latched, repeats are no-ops so only one close command can own the
// agent. If a /clear handoff is in flight it defers teardown until
// handleReopenResult claims the replacement. Otherwise it stops animation, closes
// and clears the subscription best-effort, then closes the agent under the bounded
// command before quitting.
func (m *Screen) beginGracefulQuit() tea.Cmd {
	if m.quitting {
		return nil
	}
	if m.status == StatusResetting {
		m.quitting = true
		m.quitAfterReopen = true
		return nil
	}
	m.quitting = true
	if m.sub != nil {
		_ = m.sub.Close()
		m.sub = nil
	}
	return tea.Sequence(closeAgent(m.agent), tea.Quit)
}

// handleKey routes a key press in ACTUAL execution order: (1) the GLOBAL chords ctrl+c
// (quit), ctrl+t (toggle the global collapse fold), and ctrl+n/ctrl+p (cycle focus over the
// bar's loops) fire first, even with a prompt open — focus/fold are pure VIEW state and the
// composer must never swallow those chords; (2) an active prompt consumes its approve/deny/
// choice/answer keys; (3) with no
// prompt, Esc interrupts a running turn; (4) the viewport consumes ONLY its non-conflicting
// nav keys (PageUp/PageDown/Home/End); (5) everything else — the arrow keys and printable
// input — falls through to the composer.
// routeToEditor delivers a NON-keypress message to the composer. It serves the explicit
// tea.PasteMsg arm and the trailing default of Update, which together cover every way
// text can reach the editor without arriving as a keypress:
//
//   - A bracketed paste. Bubble Tea v2 decodes one into a single tea.PasteMsg — the
//     terminal's paste markers swallow the intervening bytes, so NONE of the pasted
//     characters arrive as keypresses and handleKey never sees them.
//   - The textarea's reply to its own ctrl+v binding, which reads the system clipboard in
//     a command. That reply type is UNEXPORTED by the textarea package, so it cannot be
//     matched by a case here — the only way to complete the round trip is to forward the
//     messages Update does not recognize, which is exactly what the default arm does.
//   - The cursor-blink tick that keeps the composer caret alive.
//
// It applies the same precedence handleKey does, minus the parts these messages cannot
// mean. None of them is a chord or a navigation key, so the global chords and the
// viewport paging keys are skipped outright:
//
//   - quitting wins first, exactly as in handleKey: once the bounded agent close is in
//     flight no input may mutate state or dispatch work.
//   - an open runtime/session tray owns the keyboard and offers no text field, so the
//     message is inert (matching those trays' `default: return m, nil` key arms) rather
//     than leaking into the composer hidden behind the tray.
//   - otherwise the interaction model decides by mode (see ForwardToEditor): the
//     composer takes it, a free-text answer prompt takes it, every other prompt ignores it.
//
// None of these ever changes the transcript — only the composer's text — so it syncs the
// viewport SIZE (the editor may have auto-grown) without re-rendering, the same cheap
// path routeToInteraction takes for a plain composer edit.
func (m Screen) routeToEditor(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.quitting {
		return m, nil
	}
	if m.runtimeTray != nil || m.sessionTray != nil {
		return m, nil
	}
	beforeKind, _, beforeOpen := m.completionCursor()
	var cmd tea.Cmd
	m.interaction, cmd = m.interaction.ForwardToEditor(msg)
	m.resize()
	afterKind, _, afterOpen := m.completionCursor()
	var trayGlow tea.Cmd
	if completionTrayDidOpen(beforeKind, beforeOpen, afterKind, afterOpen) {
		trayGlow = m.startTrayGlow()
	}
	return m, tea.Batch(cmd, trayGlow)
}

func (m Screen) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// Once shutdown owns the agent, every later key is inert until Bubble Tea
	// processes the queued QuitMsg. This guard must precede global chords, prompts,
	// viewport navigation, and composer routing so none can mutate state or dispatch
	// work while the bounded agent close is in flight.
	if m.quitting {
		return m, nil
	}
	switch msg.String() {
	case "ctrl+c":
		return m, m.beginGracefulQuit()
	case "ctrl+t":
		// Retroactive global fold: because the viewport re-renders from committed each frame,
		// flipping the default re-folds the WHOLE buffer (design §Collapse). Fires even with a
		// prompt open — it is pure display state.
		m.collapse.ToggleAll()
		m.rerender()
		return m, nil
	case "ctrl+n":
		// Focus the NEXT loop in the bar's VISIBLE (creation) order, wrapping. bar().cycle
		// reuses the exact ordering Render/HitTest draw, so the keyboard cycle and a bar click
		// agree on which loop sits where. A single-loop session cycles back to the focused
		// loop, so focusLoop no-ops. It is VIEW-ONLY — focusLoop never submits/interrupts, so
		// the chord returns a nil command.
		m.focusLoop(m.bar().cycle(1))
		return m, nil
	case "ctrl+p":
		// Focus the PREVIOUS loop (bar().cycle(-1), wrapping); see ctrl+n. View-only.
		m.focusLoop(m.bar().cycle(-1))
		return m, nil
	}
	if m.runtimeTray != nil {
		switch msg.String() {
		case "esc":
			m.runtimeTray = nil
			m.runtimeTrayKind = runtimeTrayNone
			m.resize()
			return m, nil
		case "up":
			m.runtimeTray.Up()
			return m, m.startTrayGlow()
		case "down", "tab":
			m.runtimeTray.Down()
			return m, m.startTrayGlow()
		case "enter":
			selected := m.runtimeTray.Selected()
			kind, loopID := m.runtimeTrayKind, m.runtimeTrayLoopID
			m.runtimeTray = nil
			m.runtimeTrayKind = runtimeTrayNone
			m.resize()
			return m, mutateRuntime(m.appCtx, m.runtimeController, kind, loopID, selected.ID)
		default:
			return m, nil
		}
	}
	if m.sessionTray != nil {
		switch msg.String() {
		case "esc":
			m.sessionTray = nil
			m.resize()
			return m, nil
		case "up":
			m.sessionTray.Up()
			return m, m.startTrayGlow()
		case "down", "tab":
			m.sessionTray.Down()
			return m, m.startTrayGlow()
		case "enter":
			id, err := uuid.Parse(m.sessionTray.Selected().ID)
			m.sessionTray = nil
			m.resize()
			if err != nil {
				m.transcript = m.transcript.CommitGlobalError(fmt.Errorf("resume session: %w", err))
				m.rerender()
				return m, nil
			}
			if m.status == StatusRunning || m.activePrompt() != nil || m.compaction.IsActive(m.focusedLoopID) {
				m.pendingResume = id
				if m.status == StatusRunning {
					return m, m.sessionCore.interruptRunning(m.focusedLoopID)
				}
				m.status = StatusInterrupting
				return m, interruptTurn(m.appCtx, m.agent)
			}
			return m, m.beginSessionResume(id)
		default:
			return m, nil
		}
	}
	// Precedence (2): an active prompt consumes its own approve/deny/choice/answer keys.
	if m.activePrompt() != nil {
		return m.routeToInteraction(msg)
	}
	// Precedence (3): an open completion tray consumes Esc before the global interrupt,
	// preserving the draft. With no tray, Esc keeps its legacy interrupt meaning.
	if msg.String() == "esc" {
		if m.interaction.slash != nil || m.interaction.files != nil {
			return m.routeToInteraction(msg)
		}
		return m, m.sessionCore.interruptRunning(m.focusedLoopID)
	}
	// Precedence (4): the viewport consumes ONLY PageUp/PageDown/Home/End (handleKey returns
	// false for every other key), so those scroll the content.
	if m.viewport.handleKey(msg) {
		return m, nil
	}
	// Precedence (5): everything else — the arrow keys and printable input — falls through to
	// the composer.
	return m.routeToInteraction(msg)
}

// routeToInteraction delegates a key to the interaction model and maps the resulting typed
// uiAction into the agent-driving command. When the action committed an out-of-band entry
// (e.g. /help, a submit build error) the rendered buffer changed, so the viewport re-renders
// to surface it. A plain composer edit leaves the transcript UNCHANGED — only the composer's
// auto-grow may have shrunk the content region — so it just syncs the viewport SIZE (resize),
// never re-rendering the transcript per keystroke. The editor's blink cmd is batched so the
// cursor keeps blinking in compose/answer modes.
//
// Submit goes to the FOCUSED loop (Stage 2): a uiSubmit is intercepted here and routed to the
// currently focused loop via the core's loop-targeted submitToLoop — a submit while focused on
// any loop runs a new turn on that loop and stays focused there (no auto-refocus). Only
// uiSubmit is intercepted; every other action kind (approve/deny/answer/slash/
// edit/interrupt) still routes through the shared core's mapAction unchanged, so Screen's
// default submit path (mapAction → submit → Submit) is untouched.
func (m Screen) routeToInteraction(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	beforeKind, beforeCursor, beforeOpen := m.completionCursor()
	var action uiAction
	var blink tea.Cmd
	m.interaction, action, blink = m.interaction.Update(msg)
	if m.restoring && action.Kind == uiRunSlash && action.Slash == "/clear" {
		m.transcript = m.transcript.CommitGlobalNotice(noticeInfo, "restore in progress; /clear is unavailable")
		m.rerender()
		return m, nil
	}
	var cmd tea.Cmd
	var present bool
	if action.Kind == uiSubmit {
		// Submit to the focused loop and do not refocus —
		// the user stays on the loop they are watching. Blocks are built the same way the
		// core's default submit does (buildBlocks from action.Text), and a build error commits
		// the same faint error entry.
		cmd, present = m.sessionCore.submitToLoop(m.focusedLoopID, action.Text)
	} else if action.Kind == uiInterrupt {
		// Interrupt follows FOCUS for the same reason submit does: the status line the
		// user read before pressing esc is the focused loop's. Intercepted here (rather
		// than left to mapAction) because focus is presentation-owned — the core has no
		// notion of which loop the user is watching.
		cmd = m.sessionCore.interruptRunning(m.focusedLoopID)
	} else if action.Kind == uiRunSlash && action.Slash == "/compact" {
		// Focus is presentation-owned and intentionally independent from the
		// session's active loop. Manual compaction follows what the user is
		// viewing, regardless of whether that loop is idle or running.
		cmd = m.sessionCore.compactToLoop(m.focusedLoopID)
	} else if action.Kind == uiRunSlash && action.Slash == "/exit" {
		cmd = m.beginGracefulQuit()
	} else if action.Kind == uiRunSlash {
		var kind runtimeTrayKind
		switch action.Slash {
		case "/mode":
			kind = runtimeTrayMode
		case "/model":
			kind = runtimeTrayModel
		case "/effort":
			kind = runtimeTrayEffort
		}
		if kind != runtimeTrayNone && m.runtimeCatalog != nil && m.runtimeController != nil {
			cmd = queryRuntimeChoices(m.appCtx, m.runtimeCatalog, kind, m.focusedLoopID)
		} else if (action.Slash == "/sessions" || action.Slash == "/resume") && m.sessionBrowser != nil {
			cmd = listSessionsCmd(m.appCtx, m.sessionBrowser)
		} else {
			cmd, present = m.sessionCore.mapAction(action)
		}
	} else {
		cmd, present = m.sessionCore.mapAction(action)
	}
	if present {
		m.rerender()
	} else {
		m.resize()
	}
	var trayGlow tea.Cmd
	afterKind, afterCursor, afterOpen := m.completionCursor()
	opened := completionTrayDidOpen(beforeKind, beforeOpen, afterKind, afterOpen)
	moved := (msg.String() == "up" || msg.String() == "down") &&
		beforeOpen && afterOpen && beforeKind == afterKind && beforeCursor != afterCursor
	if opened || moved {
		trayGlow = m.startTrayGlow()
	}
	return m, tea.Batch(cmd, blink, trayGlow)
}

func completionTrayDidOpen(beforeKind byte, beforeOpen bool, afterKind byte, afterOpen bool) bool {
	return afterOpen && (!beforeOpen || beforeKind != afterKind)
}

func (m Screen) completionCursor() (kind byte, cursor int, open bool) {
	switch {
	case m.sessionTray != nil:
		return 'r', m.sessionTray.Cursor(), true
	case m.runtimeTray != nil:
		return 'v', m.runtimeTray.Cursor(), true
	case m.interaction.slash != nil:
		return 's', m.interaction.slash.Cursor(), true
	case m.interaction.files != nil:
		return 'f', m.interaction.files.Cursor(), true
	default:
		return 0, 0, false
	}
}

// handleMouse routes a mouse event by the region it falls in. The wheel scrolls the content
// wherever the pointer sits (the viewport reads only the wheel's direction), so it is routed
// unconditionally. A content-region click/drag/release drives the viewport's select/copy and
// (on a plain click) toggles the clicked entry's fold. A bar-region LEFT click focuses the
// loop whose segment covers the column (barMouse → focusLoop). Pointer motion over a tray
// row updates its completion cursor. The status/box regions remain inert.
func (m *Screen) handleMouse(msg tea.MouseMsg) tea.Cmd {
	if w, ok := msg.(tea.MouseWheelMsg); ok {
		// Scrolling changes which content occupies the pointer's row. Clear the old target
		// rather than leaving an affordance attached to content no longer under the pointer.
		m.clearHover()
		return m.viewport.handleMouse(w)
	}
	mouse := msg.Mouse()
	region := m.regionAt(mouse.Y)
	var hoverCmd tea.Cmd
	if _, moving := msg.(tea.MouseMotionMsg); moving {
		hoverCmd = m.updateHover(region, mouse)
	}
	var regionCmd tea.Cmd
	switch region {
	case regionContent:
		regionCmd = m.contentMouse(msg, mouse)
	case regionBar:
		regionCmd = m.barMouse(msg, mouse)
	case regionTray:
		regionCmd = m.trayMouse(msg, mouse)
	}
	if hoverCmd == nil {
		return regionCmd
	}
	if regionCmd == nil {
		return hoverCmd
	}
	return tea.Batch(hoverCmd, regionCmd)
}

// trayMouse maps pointer motion to the visible completion row. The tray is selection-only:
// motion never completes, dispatches, or submits the highlighted item.
func (m *Screen) trayMouse(msg tea.MouseMsg, mouse tea.Mouse) tea.Cmd {
	if _, ok := msg.(tea.MouseMotionMsg); !ok {
		return nil
	}
	lay := m.layout()
	row := mouse.Y - lay.trayTop
	if row < 0 || row >= lay.trayH {
		return nil
	}
	if m.sessionTray != nil {
		if m.sessionTray.SelectWindowRow(row, lay.trayH) {
			return m.startTrayGlow()
		}
	} else if m.runtimeTray != nil {
		if m.runtimeTray.SelectWindowRow(row, lay.trayH) {
			return m.startTrayGlow()
		}
	} else if m.interaction.slash != nil {
		if m.interaction.slash.SelectWindowRow(row, lay.trayH) {
			return m.startTrayGlow()
		}
	} else if m.interaction.files != nil {
		if m.interaction.files.SelectWindowRow(row, lay.trayH) {
			return m.startTrayGlow()
		}
	}
	return nil
}

// updateHover maps a cell-motion event to the same hit targets clicks use. A held-left
// motion is a text drag, not a hover. Every motion first clears both targets, ensuring
// gaps and keyboard-only chrome immediately return to their inert appearance.
func (m *Screen) updateHover(region screenRegion, mouse tea.Mouse) tea.Cmd {
	var nextEntry displayID
	var nextLoop uuid.UUID
	if mouse.Button != tea.MouseLeft {
		switch region {
		case regionContent:
			if id, ok := m.viewport.clickableEntryAt(mouse.Y); ok {
				nextEntry = id
			}
		case regionBar:
			if m.width > 0 {
				if id, ok := m.footer().HitTest(mouse.X, mouse.Y-m.layout().barY, m.width); ok {
					nextLoop = id
				}
			}
		}
	}
	if nextEntry == m.hoveredEntry && nextLoop == m.hoveredLoop {
		return nil
	}
	m.hoveredEntry = nextEntry
	m.hoveredLoop = nextLoop
	m.hoverGlowFrame = 0
	m.hoverGlowEpoch++
	if nextEntry == 0 && nextLoop == (uuid.UUID{}) {
		return nil
	}
	return hoverGlowCmd(m.hoverGlowEpoch)
}

func (m *Screen) clearHover() {
	if m.hoveredEntry == 0 && m.hoveredLoop == (uuid.UUID{}) {
		return
	}
	m.hoveredEntry = 0
	m.hoveredLoop = uuid.UUID{}
	m.hoverGlowFrame = 0
	m.hoverGlowEpoch++
}

// barMouse focuses the loop whose active-loops-bar segment covers a LEFT click's column
// (design §Active-loops bar: focus is the bar's job). It builds the SAME loopBar View draws,
// so the hit-tested column and the drawn segment agree, and maps the click to a loop id via
// HitTest. The bar is drawn from column 0 of its row (composeBody stacks it as a full row),
// so the global mouse column IS the bar-local column HitTest expects. A click on a gap, the
// "… +N" overflow marker, or past the last segment (HitTest false) is a no-op, as is a
// non-left click or motion/release (focus is a single click). It refuses to hit-test a bar
// that would render EMPTY (width <= 0) — HitTest's precondition: resolving a click against a
// row the user never saw would be wrong. It is VIEW-ONLY — focusLoop repoints focus and
// re-renders, never submitting/interrupting — so it always returns a nil command.
func (m *Screen) barMouse(msg tea.MouseMsg, mouse tea.Mouse) tea.Cmd {
	click, ok := msg.(tea.MouseClickMsg)
	if !ok || click.Button != tea.MouseLeft || m.width <= 0 {
		return nil
	}
	if id, hit := m.footer().HitTest(mouse.X, mouse.Y-m.layout().barY, m.width); hit {
		m.focusLoop(id)
	}
	return nil
}

// focusLoop repoints the viewport to loopID's projection (design §Active-loops bar + focus).
// It is a strict no-op when loopID already has focus (no needless re-render or tail reset).
// Otherwise it is VIEW-ONLY — it changes ONLY the view (focusedLoopID + the viewport), never
// the transcript and never a submit/interrupt/approve command: focus lets the user READ any
// loop's live stream, it must never message or interrupt one. On the swap it (1) clears any
// active selection — its (entry, sub) anchors belong to the OLD projection's buffer and are
// meaningless in the new one; (2) resets the viewport to the tail so the new loop's LATEST
// content shows; and (3) re-renders the new projection (rerender pins to the just-set tail).
// The whole viewport is a pure view over already-received, already-projected state, so this
// is a re-render, never a re-subscribe.
func (m *Screen) focusLoop(loopID uuid.UUID) {
	if loopID == m.focusedLoopID {
		return
	}
	m.focusedLoopID = loopID
	m.viewport.clearSelection()
	m.viewport.resetToTail()
	m.rerender()
}

// contentMouse forwards a content-region mouse event to the viewport (which draws its own
// selection and returns a copy command on release) and implements the click-vs-drag collapse
// rule: a plain click (press+release with no intervening motion) on an entry's HEADER row
// toggles just that entry's fold, while a drag is a text selection and never toggles. The
// content region sits at row 0, so the global Y is already the viewport-local row.
func (m *Screen) contentMouse(msg tea.MouseMsg, mouse tea.Mouse) tea.Cmd {
	switch msg.(type) {
	case tea.MouseClickMsg:
		if mouse.Button == tea.MouseLeft {
			m.mouseDragging = false // a fresh press; a following motion promotes it to a drag
		}
		return m.viewport.handleMouse(msg)
	case tea.MouseMotionMsg:
		if mouse.Button == tea.MouseLeft {
			m.mouseDragging = true
		}
		return m.viewport.handleMouse(msg)
	case tea.MouseReleaseMsg:
		cmd := m.viewport.handleMouse(msg) // finishes any drag; copyCmd for a real selection
		// A plain click (no motion) on a row explicitly marked clickable (e.g. a
		// "│ thought" header or tool-run summary) toggles that entry's fold. Passive headers
		// and body rows do not toggle. A drag is a selection, never a toggle.
		if !m.mouseDragging {
			if id, ok := m.viewport.clickableEntryAt(mouse.Y); ok {
				m.collapse.Toggle(id)
				m.rerender()
			}
		}
		return cmd
	}
	return nil
}

// screenLayout is the frame's top-to-bottom region geometry, the SINGLE source both View
// (which draws the regions) and regionAt (which hit-tests a mouse row) compute from, so a
// drawn row and a hit-tested row can never disagree. Top to bottom: the content region
// occupies rows [0, contentH); then an inert blank pad row, the status line, a blank gap row,
// an optional completion tray, the bottom box, a second blank gap row, and finally the
// active-loops bar at the very bottom.
type screenLayout struct {
	contentH int // viewport content rows: region [0, contentH)
	padTopY  int // the inert blank pad row between the content and the status line
	statusY  int // the status line row
	gapTopY  int // the inert blank gap row between the status line and tray/box
	trayTop  int // the first row of the optional completion tray
	trayH    int // the completion tray's rendered height; zero when hidden
	boxTop   int // the first row of the bottom box
	boxH     int // the bottom box's rendered height
	gapBotY  int // the inert blank gap row between the box and the loop bar
	barY     int // the loop bar row (the very bottom)
	barH     int // wrapped footer row count
}

// layout derives the region geometry from the current frame: the bottom box is measured
// first (its height varies with the composer/prompt). The fixed chrome is reserved before the
// optional completion tray receives its remaining-row budget; the viewport content gets
// whatever remains after that (floored at 0). Because it is deterministic in the model state,
// View and regionAt compute the identical layout.
//
// The row order is pad → status → gap → optional tray → box → gap → bar. There is deliberately
// no blank row between tray and box, so their accent rails read as one continuous surface.
//
// NOT side-effect-free: measuring the box (bottomBoxView → the bubbles textarea's View)
// drives the textarea's internal render/measure cache, so layout must run only on Bubble
// Tea's single model goroutine (never concurrently — see the serial subtest note in the
// tests).
func (m Screen) layout() screenLayout {
	const padH, statusH, gapH = 1, 1, 1
	barH := lipgloss.Height(m.footerView())
	if barH < 1 {
		barH = 1
	}
	boxH := lipgloss.Height(m.bottomBoxView())
	if boxH < 1 {
		boxH = 1
	}
	fixedH := padH + statusH + gapH + boxH + gapH + barH
	trayBudget := m.height - fixedH
	if trayBudget < 0 {
		trayBudget = 0
	}
	trayH := 0
	if tray := m.completionTrayView(trayBudget); tray != "" {
		trayH = lipgloss.Height(tray)
	}
	contentH := m.height - fixedH - trayH
	if contentH < 0 {
		contentH = 0
	}
	padTopY := contentH
	statusY := padTopY + padH
	gapTopY := statusY + statusH
	trayTop := gapTopY + gapH
	boxTop := trayTop + trayH
	gapBotY := boxTop + boxH
	barY := gapBotY + gapH
	return screenLayout{
		contentH: contentH,
		padTopY:  padTopY,
		statusY:  statusY,
		gapTopY:  gapTopY,
		trayTop:  trayTop,
		trayH:    trayH,
		boxTop:   boxTop,
		boxH:     boxH,
		gapBotY:  gapBotY,
		barY:     barY,
		barH:     barH,
	}
}

// screenRegion is a frame region the mouse can fall in — the content viewport, the status
// line, the completion tray, the bottom box, the loop bar, or an inert gap row — the
// discriminant handleMouse routes on. Tray rows react to pointer motion only.
type screenRegion uint8

const (
	regionContent screenRegion = iota
	regionStatus
	regionBar
	regionBox
	regionTray
	regionGap
)

// regionAt maps a terminal row y to its frame region using the same layout View draws, so
// mouse routing (content select/copy vs a bar focus click) matches what the user sees. The
// inert blank pad row above the status line, the two blank gap rows (and any row past the bar)
// are regionGap — inert, no mouse behavior. Completion tray rows are regionTray so pointer
// motion can select them without ever being mistaken for transcript content or composer rows.
func (m Screen) regionAt(y int) screenRegion {
	lay := m.layout()
	switch {
	case y < lay.contentH:
		return regionContent
	case y == lay.statusY:
		return regionStatus
	case y >= lay.trayTop && y < lay.trayTop+lay.trayH:
		return regionTray
	case y >= lay.boxTop && y < lay.boxTop+lay.boxH:
		return regionBox
	case y >= lay.barY && y < lay.barY+lay.barH:
		return regionBar
	default: // the two blank gap rows (and any row past the bar) — inert
		return regionGap
	}
}

// contentWidth is the column budget the focused projection renders to — the full frame
// width (the viewport draws no side gutter). A negative width (never in a real frame) floors
// to 0 so the renderers stay well-defined.
func (m Screen) contentWidth() int {
	if m.width < 0 {
		return 0
	}
	return m.width
}

// resize and rerender are the two viewport-sync primitives, deliberately SPLIT so a
// composer keystroke does not re-render the whole transcript.
//
// resize syncs ONLY the viewport's size to the current layout's content region — it does
// NOT re-render the buffer. The composer auto-grows as the user types, which shrinks the
// content region, but the transcript LINES are unchanged, so a keystroke needs only this
// (SetSize re-clamps the offset and preserves the auto-follow tail). This keeps typing O(1)
// in the transcript length: renderFocused re-renders every committed entry through glamour
// (which builds a fresh renderer per block), which must NOT run per keystroke over a long
// buffer.
func (m *Screen) resize() {
	lay := m.layout()
	m.viewport.SetSize(m.contentWidth(), lay.contentH)
}

// rerender re-sizes the viewport AND re-renders the focused projection into it. It is the
// "the rendered buffer changed" sync — called ONLY when the lines actually differ: a
// transcript-changing event, a collapse toggle (ctrl+t / header-click), a focus change, or a
// width change (a new width reflows every line). Keeping the viewport height tied to the
// (variable) bottom chrome lets the mouse hit-test's row math (entryAt) agree with what View
// drew, and the auto-follow tail stays pinned across the re-render.
func (m *Screen) rerender() {
	m.resize()
	m.viewport.SetLines(m.renderFocused())
}

// renderFocused renders the focused loop's whole projection to the viewport's provenance-
// carrying lines: each committed entry through renderEntryLines under its EFFECTIVE collapse
// state (per-entry override else the global default), then the in-progress live segment
// appended after. It is the flat []renderedLine the viewport windows and selects over.
//
// MODERN-ONLY TOOL-RUN AGGREGATION: a maximal contiguous run of committed kindTool entries
// folds as a UNIT sharing thinking's ctrl+t fold and keyed on the run's first displayID
// (runID). Collapsed (the default) the run renders as one semantic activity summary node
// (toolRunSummaryLines) whose first line carries sub == 0 / entry == runID, so the existing
// header-click handler toggles the whole run; expanded, every tool entry renders on its own.
// Non-tool entries take the unchanged per-entry path.
//
// MODERN-ONLY SPACING: one blank breathing-space row follows every committed entry (see
// blankSeparator) — the opening banner included, so the first real message is not
// glued to the header — EXCEPT within a tool-call group (see intraTurnSeparator): an assistant
// message and the (possibly parallel) tool calls it led are joined by a faint "│" rail connector
// (railSeparator) instead of a blank, so the group reads as one continuous action rather than a
// ladder of separated rows. Because every group's LAST entry still carries its trailing blank,
// the streaming live tail sits exactly one blank below the last committed entry (one gap, never
// two), so no special-casing is needed at the tail seam. Scrollback's renderEntry owns its own
// spacing and is untouched.
//
// MODERN-ONLY USER CARD: a kindUser entry is first bracketed with rail pad rows (padUserCard)
// and then gray-filled (paintUserBackground) so its message reads as a padded card — the pad
// rows are part of the entry (gray, rail-continued), distinct from the transparent blank
// separator that still follows the whole card.
// endsInThinkingGap reports whether a committed assistant entry renders as thinking with
// NO narration — so its last row is renderThinking's trailing "│ " rail gap. That gap
// already connects into a following tool node, so renderFocused must NOT also append a
// railSeparator (which would double the rail; design: one unbroken timeline). An entry
// with narration text ends on its "● " body instead, and still needs the connector.
func endsInThinkingGap(e entry) bool {
	return e.Kind == kindAssistant && thinkingText(e.Blocks) != "" && assistantText(e.Blocks) == ""
}

func (m Screen) renderFocused() []renderedLine {
	committed, live := m.transcript.projectionFor(m.focusedLoopID)
	width := m.contentWidth()
	var out []renderedLine
	for i := 0; i < len(committed); {
		// A contiguous run of committed tool entries folds as a UNIT: collapsed → one
		// semantic activity summary node (keyed on the run's first id, runID), expanded →
		// every entry individually. Non-tool entries keep the per-entry path below.
		if committed[i].Kind == kindTool {
			j := i + 1
			for j < len(committed) && committed[j].Kind == kindTool {
				j++
			}
			runID := committed[i].ID
			if m.collapse.Effective(runID) {
				// Collapsed: one summary node for the whole run. A maximal tool run always ends at
				// the first NON-tool entry (or the buffer end), so its trailing gap is never
				// intra-turn — it's always a turn-boundary blank.
				sum := toolRunSummaryLines(committed[i:j], width)
				out = append(out, sum...)
				if n := len(sum); n > 0 {
					out = append(out, blankSeparator(runID, n))
				}
				i = j
				continue
			}
			// Expanded: emit every entry in the run individually, each keyed on its own id.
			for k := i; k < j; k++ {
				lines := renderEntryLines(committed[k], width, false)
				if k == i {
					lines = markClickableHeader(lines)
				}
				out = append(out, lines...)
				if n := len(lines); n > 0 {
					if intraTurnSeparator(committed, k) {
						out = append(out, railSeparator(committed[k].ID, n))
					} else {
						out = append(out, blankSeparator(committed[k].ID, n))
					}
				}
			}
			i = j
			continue
		}

		lines := renderEntryLines(committed[i], width, m.collapse.Effective(committed[i].ID))
		if committed[i].Kind == kindAssistant && thinkingText(committed[i].Blocks) != "" {
			lines = markClickableHeader(lines)
		}
		// MODERN-ONLY: bracket the user row with rail pad rows (a padded card), then paint the
		// gray panel behind the whole block — pads included (scrollback keeps user rows bare).
		if committed[i].Kind == kindUser {
			lines = padUserCard(lines)
			lines = paintUserBackground(lines, width)
		}
		out = append(out, lines...)
		// A zero-line entry (an unknown/empty kind) has no last sub and nothing to set off, so it
		// gets no separator — which also keeps each separator's sub non-header (>= 1) for every real
		// entry. A gap INTERNAL to a step (intraTurnSeparator: assistant→tool or tool→tool) becomes a
		// faint "│" rail connector so the timeline reads unbroken; every turn boundary keeps a blank.
		if n := len(lines); n > 0 {
			switch {
			case intraTurnSeparator(committed, i) && endsInThinkingGap(committed[i]):
				// A thinking-only assistant already ends in its own "│ " rail gap, which IS the
				// connector into the following tool node — a railSeparator here would double it.
			case intraTurnSeparator(committed, i):
				out = append(out, railSeparator(committed[i].ID, n))
			default:
				out = append(out, blankSeparator(committed[i].ID, n))
			}
		}
		i++
	}
	out = append(out, m.liveTailLines(live)...)
	// Entry-local rendering deliberately owns its railed spacer rows, while committed/live
	// composition owns ordinary blank separators. At a step seam those layers can overlap and
	// leave fully empty rows between a bare rail and its following tool node. Normalize only
	// that unmistakable shape; all turn-boundary blanks remain intact.
	out = removeEmptyStepGaps(out, committed)
	// The focused loop's pending queued inputs render LAST — below the live tail, where the
	// next turn's input will land — so a user firing several messages mid-turn sees them
	// stacked. They drop the instant each one's turn starts (startTurnUser commits the real
	// user row and dropQueued removes the affordance), so a queued row never duplicates its
	// committed row.
	out = append(out, m.queuedTailLines()...)
	return out
}

// removeEmptyStepGaps removes fully empty separator rows only when provenance says the gap is
// owned by a committed assistant entry and it interrupts that entry's otherwise connected rail:
// a bare "│" spacer immediately before the empty run and a tool node (or collapsed tool-run
// summary) immediately after it. Requiring the committed kindAssistant ID prevents a visually
// identical bare rail from an empty tool-result line from losing its intentional spacing before
// a following Subagent card. The pass returns a fresh slice and preserves every retained row's
// provenance.
func removeEmptyStepGaps(lines []renderedLine, committed []entry) []renderedLine {
	assistantIDs := make(map[displayID]struct{})
	for i := range committed {
		if committed[i].Kind == kindAssistant {
			assistantIDs[committed[i].ID] = struct{}{}
		}
	}

	out := make([]renderedLine, 0, len(lines))
	for i := 0; i < len(lines); {
		if lines[i].styled != "" || lines[i].plain != "" {
			out = append(out, lines[i])
			i++
			continue
		}

		j := i + 1
		for j < len(lines) && lines[j].styled == "" && lines[j].plain == "" {
			j++
		}
		_, assistantGap := assistantIDs[lines[i].entry]
		if len(out) > 0 && j < len(lines) && assistantGap &&
			out[len(out)-1].entry == lines[i].entry && isBareRailRow(out[len(out)-1]) && isToolNodeRow(lines[j]) {
			i = j
			continue
		}
		out = append(out, lines[i:j]...)
		i = j
	}
	return out
}

func isBareRailRow(line renderedLine) bool {
	return strings.TrimSpace(plainFromStyled(line.styled)) == "│"
}

func isToolNodeRow(line renderedLine) bool {
	text := line.plain
	if text == "" {
		text = plainFromStyled(line.styled)
	}
	return strings.HasPrefix(text, "○ ") || strings.HasPrefix(text, "◍ ")
}

// markClickableHeader marks the first rendered row as an effective click target. Callers
// invoke it only for content with a visible alternate fold state (thinking blocks and the
// first node of an expanded tool run).
func markClickableHeader(lines []renderedLine) []renderedLine {
	if len(lines) > 0 {
		lines[0].clickable = true
	}
	return lines
}

// blankSeparator is the MODERN-ONLY breathing-space row appended after a committed entry: an
// empty renderedLine (no styled, no plain, no background) that provides one blank line of
// visual separation between transcript entries. It is tagged with the PRECEDING entry's
// displayID and sub (the entry's last sub + 1, i.e. lineCount), so the gap "belongs" to the
// entry above it: a selection spanning entries naturally includes the newline (plain is empty),
// and a collapse-click that lands on it resolves to a non-header sub (>= 1) and never toggles.
// It carries NO styled bytes, so it never picks up the user gray fill (paintUserBackground has
// already run on the entry's own lines before this blank is appended).
func blankSeparator(id displayID, lineCount int) renderedLine {
	return renderedLine{styled: "", plain: "", entry: id, sub: lineCount}
}

// railSeparator is the intra-turn connector row: a faint "│" that continues the rail
// between a step's nodes (assistant→tool, tool→tool) so the timeline reads unbroken,
// in place of the blank that would otherwise glue them. Like blankSeparator it is tagged
// with the PRECEDING entry's id and its lineCount sub (a non-header sub >= 1), so a
// collapse-click that lands on it never toggles and a selection includes the newline.
// Its plain form is empty, so copied text carries no stray bar.
func railSeparator(id displayID, lineCount int) renderedLine {
	return renderedLine{styled: railConnector(0), plain: "", entry: id, sub: lineCount}
}

// intraTurnSeparator reports whether the gap after committed entry i is INTERNAL to a step and
// should therefore be a rail connector (railSeparator) rather than a blank. The gap is internal
// when the NEXT entry is a tool call led by this assistant message or a preceding tool call
// (kindAssistant→kindTool or kindTool→kindTool) — so a step's narration and its (possibly
// parallel) tool calls read as one cohesive group, the assistant message visibly leading its
// calls, the rail continuing unbroken between their nodes. Every other adjacency is a turn
// boundary and keeps its blank, and the last entry (no next, so i+1 is out of range) is always a
// boundary. removeEmptyStepGaps later removes that blank only when final composition proves
// the live tail continues the same rail directly into a tool node.
func intraTurnSeparator(committed []entry, i int) bool {
	if i+1 >= len(committed) || committed[i+1].Kind != kindTool {
		return false
	}
	return committed[i].Kind == kindAssistant || committed[i].Kind == kindTool
}

// liveTailLines renders the focused loop's in-progress live segment to viewport lines,
// REUSING the existing live renderer (renderLiveAssistant) — never a new one. For the
// focused projection it matches the scrollback surface's live tail: it suppresses the
// orchestrator's raw running Subagent card (its activity shows via the nested pending card)
// and appends the in-flight pending subagent cards.
// The live segment is EXCLUDED from the collapse fold: streaming thinking always renders
// fully EXPANDED ("│ thinking" + all reasoning) regardless of the global collapse default, so
// the user watches the model reason in real time; only COMMITTED thinking follows the collapse
// state (default collapsed → the one-line "│ thought for Nsec" summary), keyed on its committed
// displayID (the live tail has none). A zero animState renders a static tail (Task 9 threads
// the blink/spinner phase).
func (m Screen) liveTailLines(live liveSeg) []renderedLine {
	width := m.contentWidth()
	pending := m.transcript.pendingSubagentCardsFor(m.focusedLoopID)
	calls := live.Calls
	if len(pending) > 0 {
		calls = nonSubagentCalls(calls, len(pending))
	}
	if live.empty() && len(pending) == 0 {
		return nil
	}
	// expand=true unconditionally: the LIVE segment is not part of the retroactive collapse
	// fold — streaming reasoning is always shown in full, and collapses to the one-liner only
	// once the step COMMITS as an entry (which then follows collapse.Effective).
	styled := renderLiveAssistant(live.Thinking, live.Text, calls, pending, true, width, animState{})
	if styled == "" {
		return nil
	}
	lines := strings.Split(styled, "\n")
	out := make([]renderedLine, len(lines))
	for i, ln := range lines {
		out[i] = renderedLine{styled: ln, plain: plainFromStyled(ln), entry: liveTailEntryID, sub: i}
	}
	return out
}

// queuedTailEntryID is the reserved provenance id every queued-affordance line carries. Like
// the live tail (liveTailEntryID) a queued row is NOT a committed entry; using a distinct
// sentinel — the max displayID the incrementing allocator (starting at 1) never reaches —
// keeps queued rows from colliding with the live tail's (entry, sub) selection identity OR a
// committed entry's collapse-click, so a stray click on a queued row toggles an id no entry
// owns (a harmless no-op) and a selection spanning queued rows resolves unambiguously.
const queuedTailEntryID displayID = ^displayID(0)

// queuedTailLines renders the FOCUSED loop's pending queued-input affordances — the user
// messages fired while a turn is still running — to viewport lines appended below the live
// tail. It scopes the queue to the focused loop (QueuedInputsFor) so a subagent's queued
// message never leaks under another loop's view, and reuses the dim, "queued"-tagged
// renderQueued (the faint styles.QueuedStyle rows).
// Once a queued message's turn starts it commits as a real user row and drops from the queue
// (startTurnUser → dropQueued), so it never renders both queued and committed. An empty queue
// yields no lines.
func (m Screen) queuedTailLines() []renderedLine {
	styled := renderQueued(m.transcript.QueuedInputsFor(m.focusedLoopID), m.contentWidth())
	if styled == "" {
		return nil
	}
	lines := strings.Split(styled, "\n")
	out := make([]renderedLine, len(lines))
	for i, ln := range lines {
		out[i] = renderedLine{styled: ln, plain: plainFromStyled(ln), entry: queuedTailEntryID, sub: i}
	}
	return out
}

// bar builds the active-loops bar from the transcript's loop table (loops()) — the SINGLE
// source of the session's loops + liveness, never a second registry. focused is the currently
// focused loop. The gate marker ("!") is set for any loop with a PENDING prompt (permission or
// AskUser) awaiting the user, read from the interaction model's pending FIFO
// (pendingGateLoops): a gate on a non-focused loop signals it needs attention WITHOUT stealing
// focus (design §Prompts), leaving the user to focus it. A gate on the focused loop still marks
// (focus and gate are independent flags).
//
// The bar shows the loops currently doing work plus two always-kept exceptions:
// activeBarEntries keeps the LIVE loops, the FOCUSED loop (so the current view is always
// labeled), and the ACTIVE loop (the session's current default target), dropping only idle
// loops that are neither. A selection event advances active without moving focus.
func (m Screen) bar() loopBar {
	infos := m.transcript.loops()
	gated := m.interaction.pendingGateLoops()
	active := m.effectiveActiveLoopID()
	entries := activeBarEntries(infos, gated, m.focusedLoopID, active)
	return loopBar{
		entries: entries,
		focused: m.focusedLoopID,
		active:  active,
		max:     loopBarCap,
		hovered: m.hoveredLoop,
		phase:   m.hoverGlowFrame,
	}
}

// activeBarEntries maps the loop table into bar entries, keeping LIVE loops plus the FOCUSED,
// ACTIVE loops (both kept even when idle); an unrelated idle loop drops off. Entry order
// follows loops() (stable creation order), so the bar draws loops in the order they appeared.
func activeBarEntries(infos []loopInfo, gated map[uuid.UUID]bool, focused, active uuid.UUID) []loopBarEntry {
	entries := make([]loopBarEntry, 0, len(infos))
	for _, li := range infos {
		if !li.Live && li.ID != focused && li.ID != active {
			continue
		}
		entries = append(entries, loopBarEntry{id: li.ID, name: li.Name, live: li.Live, gate: gated[li.ID]})
	}
	return entries
}

// bottomBoxView renders the bottom box for the current interaction mode (the reused composer,
// or a prompt control when a prompt is active) via the shared surface primitive.
func (m Screen) bottomBoxView() string {
	return bottomBox(m.surfaceInputs())
}

// completionTrayView renders at most maxRows of the active compose-mode completion list at
// the full terminal width, with the selected candidate kept visible. Slash commands and @path
// files are mutually exclusive in interactionModel; slash wins defensively if an invalid
// state supplies both. Prompt modes suppress stale completers because their controls replace
// the composer until the pending prompt is resolved.
func (m Screen) completionTrayView(maxRows int) string {
	if m.interaction.mode != modeCompose || maxRows <= 0 {
		return ""
	}
	switch {
	case m.sessionTray != nil:
		return m.sessionTray.ViewWindowBackground(m.width, maxRows, traySelectionColor(m.trayGlowFrame))
	case m.runtimeTray != nil:
		return m.runtimeTray.ViewWindowBackground(m.width, maxRows, traySelectionColor(m.trayGlowFrame))
	case m.interaction.slash != nil:
		return m.interaction.slash.ViewWindowBackground(m.width, maxRows, traySelectionColor(m.trayGlowFrame))
	case m.interaction.files != nil:
		return m.interaction.files.ViewWindowBackground(m.width, maxRows, traySelectionColor(m.trayGlowFrame))
	default:
		return ""
	}
}

// surfaceInputs builds the agent-free snapshot the shared bottom-box + status primitives
// read: the interaction model, the FOCUSED loop's status + live signals, and the frame
// dimensions.
func (m Screen) surfaceInputs() surfaceInputs {
	return surfaceInputs{
		Interaction: m.interaction,
		Status:      m.focusedStatus(),
		StatusState: m.statusInputs(),
		Width:       m.width,
		Height:      m.height,
	}
}

// focusedStatus is the turn-lifecycle Status the status line reflects for the FOCUSED loop
// (design §Status line). It follows the FOCUSED loop's OWN turn liveness, independent of which
// loop is currently active. This matters because the core status (m.status) now tracks the
// ACTIVE loop: reusing it for a different focused loop would wrongly read "running" when that
// focused loop is idle but the active loop is mid-turn. The only exception is the two
// session-global transitions the core owns — StatusInterrupting (an interrupt in flight) and
// StatusResetting (a /clear reopen) — which surface regardless of focus. The ordinary
// Running/Idle is read from the focused loop's per-loop turn bit
// (sessionCore.loopRunning, folded from EVERY loop's TurnStarted/terminal, keyed by loop id).
// That map is the accurate per-loop signal. statusInputs() then refines a Running label into
// thinking…/streaming… from the focused loop's live signals. This is a deliberately MINIMAL
// "you are viewing loop X, and whether it is live" indication, NOT a full per-loop status
// machine: Interrupting/Resetting are session-global transitions.
func (m Screen) focusedStatus() Status {
	if m.status == StatusInterrupting || m.status == StatusResetting {
		return m.status
	}
	focus := m.focusedLoopID
	if m.loopRunning[focus] {
		return StatusRunning
	}
	return StatusIdle
}

// statusInputs snapshots the status signals for the FOCUSED loop: whether its live segment
// is streaming narration or only thinking, and which prompt (if any) is active. The status
// line reflects the focused loop's activity (design §Status line).
func (m Screen) statusInputs() statusInputs {
	_, live := m.transcript.projectionFor(m.focusedLoopID)
	in := statusInputs{
		streaming:        live.Text != "",
		thinking:         live.Text == "" && live.Thinking != "",
		compactionActive: m.compaction.IsActive(m.focusedLoopID),
	}
	if p := m.activePrompt(); p != nil {
		in.permissionActive = p.Kind == promptPermission
		in.userInputActive = p.Kind == promptUserInput
	}
	return in
}

// statusLine is the focused loop's status line for the frame: the shared ANIMATED
// gradient status label (label + dot flow with m.anim.frame — the same phase Screen threads,
// advanced continuously by the anim tick so idle/waiting/thinking/streaming all shimmer) PLUS,
// while the focused loop's turn is running, a faint live-elapsed suffix " (Ns)" / " (Nm Ss)".
// Idle / no active turn → no suffix. The suffix is a single faint Render call so its "(2m 34s)"
// text stays contiguous (substring-findable) rather than split by per-glyph styling, and it is
// deliberately NOT part of the gradient so it reads as a quiet, static timer beside the shimmer.
func (m Screen) statusLine() string {
	if m.resuming {
		return styles.StatusStyle.Render("resuming…")
	}
	line := renderStatusLine(m.focusedStatus(), m.statusInputs(), m.anim.frame)
	if d, ok := m.turnElapsed(); ok {
		line += styles.StatusStyle.Render(" (" + formatElapsed(d) + ")")
	}
	if metadata := m.focusedRuntimeStatus(); metadata != "" {
		line += styles.StatusStyle.Render(statusMetadataSeparator + metadata)
	}
	// Integrations trail the loop's own metadata: they are session-wide, not a
	// property of the focused loop, so they read last. The segment is empty
	// whenever every integration is Ready, which is why this costs nothing at
	// rest and needs no row of its own (see integrationProjection.statusSegment).
	if integrations := m.integrations.statusSegment(); integrations != "" {
		line += styles.StatusStyle.Render("  ") + integrations
	}
	return line
}

// focusedRuntimeStatus renders event-authoritative metadata for the loop the user is viewing.
// Catalog options never appear here because availability is not current state.
func (m Screen) focusedRuntimeStatus() string {
	state, ok := m.runtime.loop(m.focusedLoopID)
	if !ok {
		return ""
	}
	parts := make([]string, 0, 3)
	if state.runtime.Key.Model != "" {
		parts = append(parts, state.runtime.Key.Model)
		effort := string(state.runtime.Effort)
		if effort == "" {
			effort = "none"
		}
		parts = append(parts, effort)
	}
	if state.hasContext && state.context.InputLimit > 0 {
		pct := uint64(state.context.InputTokens) * 100 / uint64(state.context.InputLimit)
		prefix := ""
		if state.context.Quality == contextcount.CountQualityHeuristicEstimate {
			prefix = "~"
		}
		parts = append(parts, prefix+strconv.FormatUint(pct, 10)+"% context")
	}
	return strings.Join(parts, statusMetadataSeparator)
}

// turnElapsed returns the focused loop's live turn-elapsed duration and whether to show it.
// It shows ONLY when the focused loop is Running and has a recorded, non-zero turn start:
// "awaiting approval" is a Running sub-state, so its timer keeps counting, while idle,
// interrupting, and clearing carry none. Elapsed is now - turnStartedAt[focused] (the tick
// clock less the event start time), floored at zero so a not-yet-arrived tick never shows a
// negative span. A zero start (a restore/backlog with no streaming timestamps) reads as "no
// timer" — mirroring formatThought's bare-"Thought" fallback.
func (m Screen) turnElapsed() (time.Duration, bool) {
	if m.focusedStatus() != StatusRunning {
		return 0, false
	}
	lid := m.focusedLoopID
	start, ok := m.turnStartedAt[lid]
	if !ok || start.IsZero() {
		return 0, false
	}
	d := m.now.Sub(start)
	if d < 0 {
		d = 0
	}
	return d, true
}

// formatElapsed renders a live turn-elapsed span in whole seconds: under a minute as "Ns"
// (e.g. "8s"), a minute or more as "Nm Ss" (e.g. "2m 34s"), matching the status-line
// examples. A negative span floors to "0s" (a defensive guard; turnElapsed already floors).
func formatElapsed(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	secs := int(d / time.Second)
	if secs < 60 {
		return strconv.Itoa(secs) + "s"
	}
	return strconv.Itoa(secs/60) + "m " + strconv.Itoa(secs%60) + "s"
}

// View composes the frame top to bottom — the viewport content (the focused projection with
// collapse), one status line, a blank gap, an optional completion tray, the bottom box, a
// blank gap, and the active-loops bar — and returns a per-frame View with the modern
// configuration: AltScreen on and all-motion mouse (the v2 per-frame mode required for
// pointer-only hover as well as copy-while-scrolling), plus the composer's Kitty
// keyboard request (see Screen.View for why). It returns an empty view until the first sized
// frame (avoids a 0×0 first frame).
func (m Screen) View() tea.View {
	if !m.ready {
		return tea.NewView("")
	}
	v := tea.NewView(m.composeBody(m.layout()))
	v.AltScreen = true
	v.MouseMode = tea.MouseModeAllMotion
	v.KeyboardEnhancements.ReportAllKeysAsEscapeCodes = true
	return v
}

// composeBody stacks the frame's rows for lay: the viewport content padded/clamped to
// EXACTLY contentH rows (so the chrome sits at the fixed rows regionAt assumes), then an inert
// blank pad row, the status line, an inert blank gap row, the optional completion tray, the
// bottom box, a second inert blank gap row, and the active-loops bar at the very bottom. The
// tray directly touches the input so their thick rails connect. The blank pad row sets the
// status line off the content above it, and the input box sits ABOVE the loop bar, set off by
// a blank row so the chrome does not read as cramped. The viewport is sized to
// lay.contentH on a LOCAL copy before rendering, so the drawn content region always matches
// the current layout even if the bottom chrome changed since the last Update-side
// resize/rerender. Every line is width-clamped (truncate, never wrap) so no row exceeds the
// frame width. The stacking order MUST match layout()/regionAt exactly.
func (m Screen) composeBody(lay screenLayout) string {
	vp := m.viewport
	vp.SetSize(m.contentWidth(), lay.contentH)

	rows := make([]string, 0, m.height)
	if content := vp.viewHovered(m.hoveredEntry, m.hoverGlowFrame); content != "" {
		rows = append(rows, strings.Split(content, "\n")...)
	}
	if len(rows) > lay.contentH {
		rows = rows[:lay.contentH]
	}
	for len(rows) < lay.contentH {
		rows = append(rows, "")
	}
	rows = append(rows, "")             // inert pad: content → status
	rows = append(rows, m.statusLine()) // status line
	rows = append(rows, "")             // inert gap: status → tray/box
	if tray := m.completionTrayView(lay.trayH); tray != "" {
		rows = append(rows, strings.Split(tray, "\n")...) // completion tray (touches input)
	}
	rows = append(rows, strings.Split(m.bottomBoxView(), "\n")...) // bottom box (input)
	rows = append(rows, "")                                        // inert gap: box → bar
	rows = append(rows, m.footerView())                            // rig + profile + workspace + local loop focus
	return clampSurfaceWidth(strings.Join(rows, "\n"), m.width)
}

func (m Screen) footerView() string {
	return m.footer().View(m.width)
}

// footer builds the bottom footer header from the FIXED session metadata supplied
// synchronously as SessionPresentation. The profile is displayed as metadata here,
// never as a mutable control (there is no way to change it from the TUI). Product
// branding remains in the startup banner rather than repeating in every frame.
func (m Screen) footer() loopFooter {
	parts := m.presentation.footerParts()
	header := ""
	if len(parts) > 0 {
		header = strings.Join(parts, " ")
	}
	return loopFooter{header: header, bar: m.bar()}
}

// compile-time assertion that Screen is a tea.Model (Init/Update/View, value receiver).
var _ tea.Model = Screen{}
