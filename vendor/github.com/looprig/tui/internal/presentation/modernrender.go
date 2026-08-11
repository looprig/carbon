package presentation

import (
	"strings"

	"github.com/looprig/core/content"

	"github.com/looprig/tui/styles"
)

// queuedTag is the marker word for a queued-affordance: renderQueued uppercases it into a
// "QUEUED" banner row shown ON TOP of each stacked, not-yet-running user message so the state
// reads UNAMBIGUOUSLY as queued — distinct from both the bold committed user row it will promote
// to and the live assistant tail it sits between. The queue is placed inline in the live tail, so
// it spells out the state rather than relying on faintness + position alone.
const queuedTag = "queued"

// renderQueued renders the focused loop's pending queued-input affordances for the
// viewport: each message is a blue "▌ QUEUED" banner row (styles.QueuedLabelStyle) with the
// message's first line of text on the row(s) beneath it — every row prefixed with the gray
// "▌ " accent bar, the message text wrapped to width and rendered faint (styles.QueuedStyle).
// The QUEUED label sits ON TOP of the message so the pending state reads unambiguously. It is
// a compact per-message preview of a throwaway affordance, never committed to scrollback.
// Empty messages yields "" so renderFocused appends no queued rows.
func renderQueued(messages [][]content.Block, width int) string {
	if len(messages) == 0 {
		return ""
	}
	bar := styles.AccentBarStyle.Render(styles.AccentBarPrompt)
	label := bar + styles.QueuedLabelStyle.Render(strings.ToUpper(queuedTag))
	var out []string
	for _, blocks := range messages {
		out = append(out, label)
		text := firstLine(renderInlineBlocks(blocks))
		for _, line := range wrapToWidth(text, width-barWidth) {
			out = append(out, bar+styles.QueuedStyle.Render(line))
		}
	}
	return strings.Join(out, "\n")
}

// userPadRows is the number of rail pad rows padUserCard brackets a user card with on EACH
// side (top and bottom) — the vertical breathing room inside the gray panel.
const userPadRows = 1

// padUserCard brackets a user entry's rendered lines with userPadRows rail pad row(s) above and
// below so the MODERN gray panel reads as a padded card rather than text flush to the panel edge.
// A pad row carries the accent bar in styled (the rail runs unbroken top-to-bottom, so the card
// reads as one block once paintUserBackground fills it gray) but NO plain text — it is vertical
// whitespace, so nothing extra reaches the clipboard. Every line's sub is reassigned 0-based
// across the padded block so provenance (selection anchoring) stays unique and ordered. Empty
// input is returned unchanged (a zero-line entry has nothing to bracket). MODERN-ONLY: it runs
// before paintUserBackground so the pad rows pick up the same gray fill; scrollback never pads.
func padUserCard(lines []renderedLine) []renderedLine {
	if len(lines) == 0 {
		return lines
	}
	id := lines[0].entry
	rail := renderedLine{styled: styles.AccentBarStyle.Render(styles.AccentBar), entry: id}
	out := make([]renderedLine, 0, len(lines)+2*userPadRows)
	for i := 0; i < userPadRows; i++ {
		out = append(out, rail)
	}
	out = append(out, lines...)
	for i := 0; i < userPadRows; i++ {
		out = append(out, rail)
	}
	for i := range out {
		out[i].sub = i
	}
	return out
}

// paintUserBackground is the MODERN-ONLY post-process that paints the gray panel behind a
// user entry's rendered lines: it replaces each line's styled form with a full-width
// gray-filled version (styles.FillLineBackground) and leaves plain/entry/sub untouched, so
// selection, copy, and the click-to-collapse provenance are unaffected — the fill is
// display-only and never reaches the clipboard. The scrollback Screen renders the SAME
// entry through renderEntry with no fill (see styles.BoxStyle); this transform runs only in
// renderFocused.
func paintUserBackground(lines []renderedLine, width int) []renderedLine {
	out := make([]renderedLine, len(lines))
	for i, ln := range lines {
		out[i] = renderedLine{
			styled:    styles.FillLineBackground(ln.styled, width),
			plain:     ln.plain,
			entry:     ln.entry,
			sub:       ln.sub,
			clickable: ln.clickable,
		}
	}
	return out
}
