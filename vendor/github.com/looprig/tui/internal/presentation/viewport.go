package presentation

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// selPoint is one end of a selection, anchored to CONTENT (not a screen row): entry+sub
// identify the exact rendered line and cell is a 0-based cell column within that line's
// plain text. Anchoring to (entry, sub) rather than a raw offset+Y row is what lets a
// selection survive reflow (a StepDone snap, a collapse toggle, a width change) — the
// same logical line is re-found by identity after the buffer changes.
type selPoint struct {
	entry displayID
	sub   int
	cell  int
}

// selection is an active or completed drag: two content-anchored endpoints. anchor is
// where the drag began (mouse-down); cursor is the moving end. Order is not normalized
// here — normSel orders them at read time.
type selection struct {
	anchor, cursor selPoint
}

// viewportModel is the hand-rolled scrollable/selectable window over rendered transcript
// lines. It shows lines[offset : offset+height] and draws its own selection so wheel/key
// scroll and drag-to-select-and-copy coexist inside a single alt-screen frame (the
// terminal's native selection is unavailable once the app grabs the mouse).
//
// atTail is the auto-follow flag: the scroll handlers SET it (landing at the bottom
// -> true, moving off it -> false); SetLines and SetSize READ it to decide whether new
// content pins to the tail. It is never recomputed from line counts, which reflow.
//
// During a drag the content buffer is frozen (snapshotted at mouse-down) so a mid-drag
// reflow cannot move the selection under the cursor; SelectedText and View read frozen
// until release, then fall back to lines (resolving the anchored points by identity).
type viewportModel struct {
	lines  []renderedLine
	offset int       // index of the top visible line
	height int       // visible rows
	sel    selection // meaningful only when hasSel; a VALUE (not a pointer) so copying
	hasSel bool      // the model never aliases a shared selection across Elm Update copies
	atTail bool
	frozen []renderedLine // snapshot taken at mouse-down; nil when no drag is active
}

// selectionStyle draws a selected cell span. Reverse video is applied to the line's
// PLAIN slice (never spliced into the styled string, which would corrupt its escapes),
// so a highlighted line drops its color while selected — the accepted design tradeoff.
var selectionStyle = lipgloss.NewStyle().Reverse(true)

// SetLines replaces the content buffer. When atTail the offset snaps to the new bottom
// (live output stays pinned); otherwise the previous offset is preserved and clamped to
// the new content. atTail is left untouched — it is a stored flag, never recomputed from
// counts. frozen is not touched, so a SetLines during a drag does not disturb the frozen
// snapshot the selection is reading.
func (m *viewportModel) SetLines(lines []renderedLine) {
	m.lines = lines
	m.reclamp()
}

// SetSize sets the visible height, clamps a negative height to 0 so maxOffset is always
// well-defined, then re-derives the offset the same way SetLines does (pin to tail when
// following, else clamp). The width argument is accepted to match the composing shell's
// call shape but is not stored: the shell renders lines to width before SetLines, so the
// viewport never needs it.
func (m *viewportModel) SetSize(width, height int) {
	_ = width
	if height < 0 {
		height = 0
	}
	m.height = height
	m.reclamp()
}

// reclamp re-derives offset after lines or height changed: pin to the bottom when atTail,
// otherwise clamp the existing offset into range.
func (m *viewportModel) reclamp() {
	if m.atTail {
		m.offset = m.maxOffset()
		return
	}
	m.offset = clamp(m.offset, 0, m.maxOffset())
}

// maxOffset is the largest valid top-line index: max(0, len(lines)-height).
func (m viewportModel) maxOffset() int {
	if max := len(m.lines) - m.height; max > 0 {
		return max
	}
	return 0
}

// scrollBy moves the top-line offset by n rows, clamped into range, and updates the
// auto-follow flag from where it landed: at the bottom -> atTail true, off it -> false.
// This is the ONE place atTail is set by scrolling.
func (m *viewportModel) scrollBy(n int) {
	m.offset = clamp(m.offset+n, 0, m.maxOffset())
	m.atTail = m.offset >= m.maxOffset()
}

// resetToTail re-pins the viewport to the tail: it SETS the auto-follow flag and snaps the
// offset to the bottom of the current buffer, so the newly focused loop's LATEST content is
// what shows after a focus swap. A following SetLines/SetSize keeps it pinned (reclamp reads
// atTail), so a focus change lands the view at the new projection's tail regardless of where
// the old projection was scrolled. It is the focus-swap companion of scrollBy (the other
// atTail writer).
func (m *viewportModel) resetToTail() {
	m.atTail = true
	m.offset = m.maxOffset()
}

// clearSelection drops any active or completed selection AND the frozen drag snapshot. A
// selection is anchored to (entry, sub) ids in the buffer it was made over; after a focus
// swap those ids belong to the OLD loop's projection and are meaningless in the new one, so
// the swap must clear the selection rather than carry a dangling anchor into the new view.
func (m *viewportModel) clearSelection() {
	m.hasSel = false
	m.sel = selection{}
	m.frozen = nil
}

// handleKey consumes the viewport's non-conflicting navigation keys — PageUp/PageDown
// (a page is height-1 rows, one row of overlap) and Home/End — mutating in place and
// returning whether the key was consumed so the composing shell can route un-consumed
// keys elsewhere. Line-scroll deliberately lives on the wheel and Page keys, not the
// arrow keys (which belong to the composer/prompt layer).
func (m *viewportModel) handleKey(msg tea.KeyPressMsg) bool {
	page := m.height - 1
	if page < 1 {
		page = 1
	}
	switch msg.String() {
	case "pgup":
		m.scrollBy(-page)
	case "pgdown":
		m.scrollBy(page)
	case "home":
		m.scrollBy(-len(m.lines))
	case "end":
		m.scrollBy(len(m.lines))
	default:
		return false
	}
	return true
}

// handleMouse routes mouse events, mutating in place and returning any follow-up command:
// the wheel scrolls; a left click begins a selection and freezes the buffer; motion with
// the button held moves the cursor against the frozen buffer; release keeps the selection
// visible and returns a copy command for its text (nil for an empty selection, so an empty
// drag never clobbers the clipboard).
//
// Coordinate contract: msg.X and msg.Y are VIEWPORT-LOCAL — the viewport renders at row 0
// of its own region, so (0,0) is its top-left cell. The composing shell (Task 7) MUST
// translate global terminal coordinates into viewport-local ones (subtract the region's
// top row / left column) before calling this.
func (m *viewportModel) handleMouse(msg tea.MouseMsg) tea.Cmd {
	switch e := msg.(type) {
	case tea.MouseWheelMsg:
		switch e.Button {
		case tea.MouseWheelUp:
			m.scrollBy(-3)
		case tea.MouseWheelDown:
			m.scrollBy(3)
		}
	case tea.MouseClickMsg:
		if e.Button == tea.MouseLeft {
			m.beginSelect(e.X, e.Y)
		}
	case tea.MouseMotionMsg:
		if m.frozen != nil && e.Button == tea.MouseLeft {
			m.moveCursor(e.X, e.Y)
		}
	case tea.MouseReleaseMsg:
		if m.frozen != nil {
			return m.endSelect()
		}
	}
	return nil
}

// beginSelect starts a drag at viewport-local (x, y): it snapshots the current lines into
// frozen and anchors both endpoints to the clicked content line's (entry, sub) identity
// at cell x. A click outside the content rows (below the last line or a negative row)
// starts nothing.
func (m *viewportModel) beginSelect(x, y int) {
	if y < 0 || y >= m.height {
		return
	}
	row := m.offset + y
	if row < 0 || row >= len(m.lines) {
		return
	}
	m.frozen = m.lines
	pt := selPoint{entry: m.lines[row].entry, sub: m.lines[row].sub, cell: x}
	m.sel = selection{anchor: pt, cursor: pt}
	m.hasSel = true
}

// moveCursor updates the drag's moving endpoint from viewport-local (x, y), reading the
// FROZEN buffer so a concurrent reflow of lines cannot shift the selection. The row is
// clamped into the frozen buffer.
func (m *viewportModel) moveCursor(x, y int) {
	if len(m.frozen) == 0 || !m.hasSel {
		return
	}
	row := clamp(m.offset+y, 0, len(m.frozen)-1)
	m.sel.cursor = selPoint{entry: m.frozen[row].entry, sub: m.frozen[row].sub, cell: x}
}

// endSelect finishes a drag: it extracts the selected text from the frozen buffer, clears
// frozen (so later reads use lines, resolving the anchored points by identity), and keeps
// the selection visible. It returns a copy command for non-empty text, or nil for an
// empty selection (never clearing the clipboard).
func (m *viewportModel) endSelect() tea.Cmd {
	text := m.SelectedText()
	m.frozen = nil
	if text == "" {
		return nil
	}
	return copyCmd(text)
}

// clickableEntryAt resolves an actionable header at viewport-local row localY. It uses
// the same active buffer as View and selection, and trusts the row-level clickable bit
// produced by renderFocused rather than assuming every sub-zero row is interactive.
func (m viewportModel) clickableEntryAt(localY int) (displayID, bool) {
	if localY < 0 || localY >= m.height {
		return 0, false
	}
	buf := m.activeBuffer()
	row := m.offset + localY
	if row < 0 || row >= len(buf) || !buf[row].clickable {
		return 0, false
	}
	return buf[row].entry, true
}

// activeBuffer is the buffer selection and rendering read: the frozen snapshot while a
// drag is active, otherwise the live lines.
func (m viewportModel) activeBuffer() []renderedLine {
	if m.frozen != nil {
		return m.frozen
	}
	return m.lines
}

// resolvedPoint is a selection endpoint resolved to a concrete buffer row and cell.
type resolvedPoint struct {
	row, cell int
}

// normSel resolves both endpoints against buf (by entry+sub identity) and returns them
// ordered so start <= end (by row, then cell). ok is false when there is no selection,
// an endpoint is no longer present in buf, or the span is empty (start == end).
func (m viewportModel) normSel(buf []renderedLine) (start, end resolvedPoint, ok bool) {
	if !m.hasSel {
		return resolvedPoint{}, resolvedPoint{}, false
	}
	aRow, aOK := lineIndexOf(buf, m.sel.anchor)
	cRow, cOK := lineIndexOf(buf, m.sel.cursor)
	if !aOK || !cOK {
		return resolvedPoint{}, resolvedPoint{}, false
	}
	a := resolvedPoint{row: aRow, cell: m.sel.anchor.cell}
	b := resolvedPoint{row: cRow, cell: m.sel.cursor.cell}
	if a.row > b.row || (a.row == b.row && a.cell > b.cell) {
		a, b = b, a
	}
	if a == b {
		return resolvedPoint{}, resolvedPoint{}, false
	}
	return a, b, true
}

// lineIndexOf finds the buffer index of the line matching pt's (entry, sub) identity, or
// (-1, false) when it is not present.
func lineIndexOf(buf []renderedLine, pt selPoint) (int, bool) {
	for i := range buf {
		if buf[i].entry == pt.entry && buf[i].sub == pt.sub {
			return i, true
		}
	}
	return -1, false
}

// SelectedText extracts the selected span from the active buffer's plain text: the first
// line from its start cell to end, whole middle lines, the last line from 0 to its end
// cell, joined with "\n". Column math is by CELL (a wide rune spans two cells), never by
// rune index, and reads plain only — so no escape sequence ever reaches the clipboard. An
// empty or unresolved selection yields "".
func (m viewportModel) SelectedText() string {
	buf := m.activeBuffer()
	start, end, ok := m.normSel(buf)
	if !ok {
		return ""
	}
	if start.row == end.row {
		return cellSpan(buf[start.row].plain, start.cell, end.cell)
	}
	var b strings.Builder
	for row := start.row; row <= end.row; row++ {
		plain := buf[row].plain
		switch row {
		case start.row:
			b.WriteString(cellSpan(plain, start.cell, lipgloss.Width(plain)))
		case end.row:
			b.WriteString(cellSpan(plain, 0, end.cell))
		default:
			b.WriteString(plain)
		}
		if row != end.row {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// View joins the visible window (activeBuffer[offset : offset+height]). A line that
// intersects the selection is redrawn from its plain text with the selected cell span
// wrapped in reverse video (plain-prefix + reverse(selected) + plain-suffix); every other
// line is drawn from its styled string verbatim. The selected span is built from plain,
// never by splicing reverse into styled (which would corrupt the ANSI).
func (m viewportModel) View() string {
	return m.viewHovered(0, 0)
}

// viewHovered draws the ordinary viewport plus a solid pastel-blue foreground on the
// one actionable header identified by hovered. The line is redrawn from plain text
// so nested renderer ANSI cannot mask the hover color; selection remains higher priority.
// A zero hovered id means no hover (committed display ids start at one).
func (m viewportModel) viewHovered(hovered displayID, phase uint) string {
	if m.height <= 0 {
		return ""
	}
	buf := m.activeBuffer()
	// Clamp to len(buf): during a drag the buffer is frozen while offset may have advanced
	// (a SetLines that pinned to a now-longer lines). If offset lands past the frozen end,
	// start clamps to len(buf) and the window is empty for that one frame — an accepted
	// transient (a drag is momentary), not a correctness bug.
	start := clamp(m.offset, 0, len(buf))
	end := clamp(start+m.height, 0, len(buf))
	selStart, selEnd, hasSel := m.normSel(buf)

	rows := make([]string, 0, end-start)
	for i := start; i < end; i++ {
		if hasSel && i >= selStart.row && i <= selEnd.row {
			rows = append(rows, highlightLine(buf[i].plain, i, selStart, selEnd))
			continue
		}
		if hovered != 0 && buf[i].clickable && buf[i].entry == hovered {
			rows = append(rows, brandBlueTranscriptLabel(buf[i].plain, phase))
			continue
		}
		rows = append(rows, buf[i].styled)
	}
	return strings.Join(rows, "\n")
}

// highlightLine redraws one selected line from its plain text with the selected cell span
// in reverse video. For a line fully inside the selection the whole line is reversed; for
// the first/last line only the [lo, hi) cell span is. A degenerate span (lo >= hi) falls
// back to the plain text unstyled.
func highlightLine(plain string, row int, start, end resolvedPoint) string {
	w := lipgloss.Width(plain)
	lo, hi := 0, w
	if row == start.row {
		lo = start.cell
	}
	if row == end.row {
		hi = end.cell
	}
	lo = clamp(lo, 0, w)
	hi = clamp(hi, 0, w)
	if lo >= hi {
		return plain
	}
	prefix := cellSpan(plain, 0, lo)
	mid := cellSpan(plain, lo, hi)
	suffix := cellSpan(plain, hi, w)
	return prefix + selectionStyle.Render(mid) + suffix
}

// cellSpan returns the substring of plain covering cell columns [lo, hi), mapping each
// cell bound to a rune index via cellToRune.
func cellSpan(plain string, lo, hi int) string {
	runes := []rune(plain)
	l := cellToRune(plain, lo)
	h := cellToRune(plain, hi)
	if l > h {
		l, h = h, l
	}
	return string(runes[l:h])
}

// cellToRune maps a target cell column to a rune index in plain by accumulating
// lipgloss.Width over the rune prefix until the accumulated width reaches the target: it
// returns the smallest rune index i whose prefix occupies >= cell cells (a mid-wide-rune
// target snaps forward past that rune), clamped to len(runes).
//
// Limitation: per-rune width is exact for ASCII (1 cell) and single CJK/emoji (2 cells)
// but can be off by a cluster for multi-rune graphemes (ZWJ emoji, combining marks),
// because it measures each rune independently rather than the grapheme cluster. That
// mid-cluster boundary is an accepted v1 edge — fixing it would need a segmentation
// dependency we deliberately do not add.
func cellToRune(plain string, cell int) int {
	if cell <= 0 {
		return 0
	}
	runes := []rune(plain)
	width := 0
	for i, r := range runes {
		if width >= cell {
			return i
		}
		width += lipgloss.Width(string(r))
	}
	return len(runes)
}

// clamp returns v confined to the inclusive range [lo, hi].
func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
