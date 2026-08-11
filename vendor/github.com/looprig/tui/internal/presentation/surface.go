package presentation

import (
	"strings"

	"charm.land/lipgloss/v2"
)

// Active-surface row budget (design §"Input box · Active-surface budgeting"). The
// surface is the only managed region in scrollback-first mode: the terminal owns
// history, so there is no transcript viewport — only any unprinted startup entries,
// a live tail budgeted to the available rows, the bottom box, an optional slash panel,
// one status line, and the rotating tip.
const (
	statusH    = 1 // the single status line
	statusPadH = 1 // one blank pad line; the status row carries one above and one below it
	tipH       = 1 // the rotating Tips line at the very bottom, below the status line
	boxBorderH = 2 // the bottom box's top + bottom frame rows (a prompt box's border; the minimal composer has none, so this over-reserves harmlessly in compose mode)
)

// liveTailCap is the number of rows the live tail may occupy: the terminal height less
// the status/tip rows, the rows reserved below the tail (reservedH = startup + slash
// panel + queued-input affordance), and the bottom box (box frame + contentH; contentH
// is the composer height in compose/answer mode or the prompt-control height in prompt
// mode). The result is floored at 0 and is never negative.
//
// This is a terminal-safety budget, not a small-window policy. The live tail gets all
// rows left after chrome so long thinking/messages can fill the active screen while
// streaming. clampSurfaceHeight remains the hard invariant that keeps the managed frame
// below the terminal height; the vendored scrollback renderer pages large commits, so the
// older half-screen headroom rule is no longer needed here.
func liveTailCap(term, statusH, reservedH, contentH int) int {
	bottomH := boxBorderH + contentH
	free := term - statusH - reservedH - bottomH
	if free <= 0 {
		return 0
	}
	return free
}

// surfaceInputs is the synthetic, agent-free snapshot surfaceView composes from:
// the interaction model (mode + active prompt + composer + slash), the unprinted
// startup block, the rendered live-tail string, the session status and its live
// signals, and the terminal dimensions. It carries no agent and is never mutated —
// surfaceView is pure.
type surfaceInputs struct {
	Interaction   interactionModel
	Startup       string // committed startup entries not yet printed to native scrollback
	LiveTail      string // pre-rendered live thinking/text/tool ⋯ lines
	Queued        string // pre-rendered dim queued-input affordance lines (below the live tail)
	Status        Status
	StatusState   statusInputs
	Phase         uint   // live animation frame; flows the status-line gradient (label + dot) while a turn runs (0 at rest)
	Tip           string // the rotating hint shown faint on the Tips line at the very bottom
	Width, Height int
}

// surfaceView composes the active surface top to bottom: any unprinted startup entries,
// the live tail, one status line set off by a blank pad row above and below it, the
// bottom box (composer, prompt control, or answer field by interaction mode), and the
// slash-completion panel when visible. The composer is a
// borderless, ▌-edged dark-gray panel — there is no separator rule above it (a
// full-width rule was the most visible artifact stranded into scrollback on a resize
// desync; see styles.BoxStyle). It allocates no transcript viewport. Empty regions are omitted so
// the surface never emits stray blank rows.
//
// Every composed line is clamped to in.Width AND the whole surface to in.Height as the
// final step (clampSurfaceWidth then clampSurfaceHeight). Both are hard invariants for
// the bubbletea v2 inline renderer: it sizes its managed region from the View's LOGICAL
// line count (strings.Count(view, "\n")+1) and assumes each line occupies exactly one
// physical row. A line wider than the terminal soft-wraps onto an extra physical row,
// so the renderer's relative-cursor tracking under-counts the rows it drew; on the next
// frame (e.g. each step of a resize drag) it repaints from the wrong row and strands the
// prior frame's lines (the separator rule + input-box top border) into native scrollback
// — the resize artifact cascade. Symmetrically, a surface TALLER than the terminal
// desyncs the renderer's insertAbove (tea.Println) cursor math and strands a block of
// blank rows below the chrome — the "big gap once the AI message responded" symptom. The
// per-region budget (liveTailCap + cappedTail) caps only the live tail, so when the
// bottom chrome alone overflows the terminal, clampSurfaceHeight is the fail-safe that
// keeps the surface within in.Height. Truncating (never wrapping) and height-clamping
// keep the logical line count equal to the physical row count and within the terminal,
// which is exactly what the renderer requires.
func surfaceView(in surfaceInputs) string {
	bottom := bottomBox(in)
	slash := slashPanel(in.Interaction)
	status := renderStatusLine(in.Status, in.StatusState, in.Phase)
	tip := renderTip(in.Tip)

	// The live tail carries a trailing blank line, mirroring the one
	// scrollbackModel.Flush appends after every committed entry — so the gap below the
	// streaming assistant already matches its committed look, and the layout does not
	// jump by a row when the turn commits. The spacer is reserved out of the tail
	// capacity (capacity-1) so the surface row budget is unchanged.
	tail := ""
	if in.LiveTail != "" {
		tail = cappedTail(in.LiveTail, liveTailDisplayCap(in))
	}

	rows := make([]string, 0, 7)
	rows = appendNonEmpty(rows, in.Startup)
	if in.Startup != "" {
		rows = append(rows, "") // startup entries visually hand off to scrollback spacing
	}
	if tail != "" {
		rows = append(rows, tail, "") // tail + trailing blank (matches a committed entry's spacing)
	}
	rows = appendNonEmpty(rows, in.Queued)
	// The status row sits ABOVE the bottom box, set off by a blank pad line above and
	// below (statusPadH each). The row is always present (statusLabel never returns ""):
	// it reads "○ idle" at rest and the live label during a turn, so the composer's
	// vertical position is stable across the turn (every row is budgeted). The pad-above
	// is skipped when the preceding row is already blank — the live tail's trailing blank
	// doubles as it — so the gap above the status never grows to two rows.
	if status != "" {
		if len(rows) == 0 || rows[len(rows)-1] != "" {
			rows = append(rows, "") // pad above
		}
		rows = append(rows, status, "") // status + pad below
	}
	rows = appendNonEmpty(rows, bottom)
	rows = appendNonEmpty(rows, slash)
	// The rotating Tips line (tipH), when present, sits on the very bottom.
	rows = appendNonEmpty(rows, tip)
	// Width MUST be clamped before height: clampSurfaceWidth truncates (never wraps), so
	// every logical line is exactly one physical row before clampSurfaceHeight counts
	// lines. Reversing the order would let a wide line wrap onto an extra physical row
	// after the height count, reintroducing the very row-count desync these guards prevent.
	return clampSurfaceHeight(clampSurfaceWidth(strings.Join(rows, "\n"), in.Width), in.Height)
}

func liveTailDisplayCap(in surfaceInputs) int {
	contentH := bottomContentHeight(in)
	// The queued affordance and slash panel are first-class reserved rows: they sit
	// between the live tail and the bottom box (queued) or below the bottom box
	// (slash), so the tail gets only the rows left after BOTH are reserved — keeping
	// the logical line count equal to the physical row count the v2 inline renderer
	// requires (see clampSurfaceWidth).
	reserved := lipgloss.Height(slashPanel(in.Interaction)) + queuedHeight(in.Queued) + startupHeight(in.Startup)
	// The status row carries a blank pad above and below it (2*statusPadH) and tipH (the
	// Tips line at the very bottom) are reserved alongside statusH so the tail budget
	// accounts for the whole bottom chrome. During a live turn the tail's own trailing
	// blank doubles as the status's pad-above, so this slightly over-reserves — harmless.
	capacity := liveTailCap(in.Height, statusH+2*statusPadH+tipH, reserved, contentH)
	if capacity <= 0 {
		return 0
	}
	return capacity - 1
}

func startupHeight(startup string) int {
	if startup == "" {
		return 0
	}
	return lipgloss.Height(startup) + 1
}

// clampSurfaceHeight drops leading lines so the composed active surface never exceeds
// height-1 physical rows — the HEIGHT fail-safe symmetric to clampSurfaceWidth, and just
// as hard an invariant for the bubbletea v2 inline renderer. The renderer sizes its
// managed region from the View's LOGICAL line count (strings.Count(view,"\n")+1) and
// assumes each line is one physical row; if the surface emits MORE lines than the
// terminal height, the renderer's insertAbove (tea.Println) relative-cursor math —
// which positions itself off cellbuf.Height() — desyncs and strands a block of blank
// rows into native scrollback (the "big gap once the AI message responded" symptom).
//
// We clamp to height-1, NOT height, to RESERVE one row of headroom the renderer needs.
// The vendored, paged insertAbove commits scrollback in pages capped at `cap = termRows
// - h`, where h is the managed-region height (this surface's rendered line count) and
// termRows is the terminal height. Rendering a surface that fills the ENTIRE terminal
// (h == termRows) drives cap to 0, which the renderer floors to 1 — its degenerate
// 1-row-transient path, where a single committed row can transiently clamp at the top of
// the screen and strand the managed region. Keeping the surface at most termRows-1 rows
// holds h <= termRows-1, so cap >= 1 and the renderer never enters that degenerate path.
//
// The per-region budget (liveTailCap + cappedTail) normally keeps the surface within
// height, but it only caps the LIVE TAIL: when the bottom chrome alone (a grown
// composer, a queued affordance, or a prompt control whose budget floors above the
// terminal) plus the status/tip rows already exceeds height, the tail floors at 0 yet
// the chrome still overflows. This fail-safe guarantees the invariant unconditionally.
//
// It drops from the TOP (keeping the bottom-most height-1 lines) to MATCH the renderer's
// own over-tall-frame handling (cursed_renderer.go keeps the bottom s.height lines when
// frameHeight > s.height) AND to preserve the most important chrome: the bottom box, slash
// panel, and tip stay visible longest (the status now sits just above the box, so it sheds
// before the box does). In the common case it sheds the oldest live-tail rows first when
// the terminal itself is out of space; StepDone still commits the full authoritative
// content to scrollback. When the terminal is small enough that even the chrome overflows,
// chrome rows are shed from the top too — unavoidable at that size, and consistent with
// the renderer's own over-tall handling. A height <= 1 is a degenerate terminal (height-1
// leaves no renderable row) — the surface is dropped to the empty string.
func clampSurfaceHeight(surface string, height int) string {
	limit := height - 1 // reserve one row of headroom for the renderer's insertAbove (cap = termRows - h)
	if limit <= 0 {
		return ""
	}
	lines := strings.Split(surface, "\n")
	if len(lines) <= limit {
		return surface
	}
	return strings.Join(lines[len(lines)-limit:], "\n")
}

// queuedHeight is the row count of the pre-rendered queued affordance (0 when
// empty), reserved out of the live-tail budget so the tail never overlaps the
// affordance. An empty affordance reserves nothing.
func queuedHeight(queued string) int {
	if queued == "" {
		return 0
	}
	return lipgloss.Height(queued)
}

// clampSurfaceWidth truncates every line of the composed active surface to width
// display columns, the fail-safe that guarantees no active-surface line is ever
// wider than the terminal (see surfaceView for why the bubbletea v2 inline renderer
// requires this). It uses lipgloss MaxWidth, which is ANSI-aware: it counts display
// columns and preserves the per-line SGR styling while cutting the overflow. A
// non-positive width is a degenerate terminal (no managed region) — the surface is
// dropped to the empty string rather than emitting unclamped lines.
func clampSurfaceWidth(surface string, width int) string {
	if width <= 0 {
		return ""
	}
	clamp := lipgloss.NewStyle().MaxWidth(width)
	lines := strings.Split(surface, "\n")
	for i := range lines {
		lines[i] = clamp.Render(lines[i])
	}
	return strings.Join(lines, "\n")
}

// bottomBox renders the bottom box for the current interaction mode: the prompt
// control (permission or AskUser choices) when a prompt is active, the reused
// composer otherwise (compose and answer modes both render the editor — in answer
// mode it IS the answer field, with the question framed by renderAskUserBox above).
func bottomBox(in surfaceInputs) string {
	m := in.Interaction
	p := m.ActivePrompt()
	switch m.mode {
	case modePermissionPrompt:
		if p != nil {
			return renderPermissionBox(*p, in.Width, m.PendingCount())
		}
	case modeChoicePrompt:
		if p != nil {
			return renderAskUserBox(*p, in.Width, promptControlBudget(in.Height), m.PendingCount())
		}
	case modeFormPrompt:
		if p != nil {
			return renderFormBox(*p, in.Width, m.PendingCount())
		}
	case modeOpenURLPrompt:
		if p != nil {
			return renderOpenURLBox(*p, in.Width, m.PendingCount())
		}
	case modeAnswerPrompt:
		return answerSection(in)
	}
	return m.input.View()
}

// answerSection stacks the free-text framing (question + "answer" header) above the
// reused composer editor: renderAskUserBox supplies the framing, the input box the
// live answer field. With no active prompt it falls back to the bare composer.
func answerSection(in surfaceInputs) string {
	m := in.Interaction
	p := m.ActivePrompt()
	if p == nil {
		return m.input.View()
	}
	frame := renderAskUserBox(*p, in.Width, promptControlBudget(in.Height), m.PendingCount())
	return frame + "\n" + m.input.View()
}

// bottomContentHeight is the contentH fed to liveTailCap: the composer's content
// height in compose/answer mode, or the rendered prompt-control content height
// (rows inside its border) in a prompt mode.
func bottomContentHeight(in surfaceInputs) int {
	m := in.Interaction
	switch m.mode {
	case modePermissionPrompt, modeChoicePrompt, modeAnswerPrompt, modeFormPrompt, modeOpenURLPrompt:
		if p := m.ActivePrompt(); p != nil {
			return promptContentHeight(in)
		}
	}
	return m.input.Height()
}

// promptContentHeight is the row count of the active prompt control minus its
// border frame, the contentH the budget reserves for a prompt-mode bottom box.
func promptContentHeight(in surfaceInputs) int {
	box := bottomBox(in)
	h := lipgloss.Height(box) - boxBorderH
	if h < 1 {
		return 1
	}
	return h
}

// promptControlBudget is the row budget handed to renderAskUserBox for the choice
// window: roughly a third of the terminal so the control never dominates the
// surface, floored so at least a couple of choices show. It is intentionally simple
// — the window scroller keeps a high selection visible regardless of the budget.
func promptControlBudget(term int) int {
	budget := term / 3
	if budget < 3 {
		return 3
	}
	return budget
}

// slashPanel renders the active completion panel below the composer — the slash-command
// panel or the @path file panel (mutually exclusive) — else "". Both share this one
// reserved slot in the surface budget.
func slashPanel(m interactionModel) string {
	switch {
	case m.slash != nil:
		return m.slash.View()
	case m.files != nil:
		return m.files.View()
	default:
		return ""
	}
}

// cappedTail returns the last capacity lines of the pre-rendered live tail, dropping the
// oldest rows when the terminal cannot fit the full active block. A capacity of 0 (or an
// empty tail) yields "". StepDone later commits the full authoritative step to scrollback,
// so this is only a live-frame visibility limit.
func cappedTail(tail string, capacity int) string {
	if capacity <= 0 || tail == "" {
		return ""
	}
	lines := strings.Split(tail, "\n")
	if len(lines) <= capacity {
		return tail
	}
	return strings.Join(lines[len(lines)-capacity:], "\n")
}

// appendNonEmpty appends s to rows only when it is non-empty, so an omitted region
// (an empty tail, a hidden slash panel, an empty status) leaves no blank row.
func appendNonEmpty(rows []string, s string) []string {
	if s == "" {
		return rows
	}
	return append(rows, s)
}
