package presentation

import (
	"sort"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/looprig/core/uuid"
	"github.com/looprig/tui/styles"
)

// loopBarEntry is one loop's assembled row for the modern active-loops bar: its id (the
// focus target a click resolves to), its display name, and its liveness + gate flags. It is
// PURE presentation data — the caller (Task 7) maps the transcript's loops() + interaction
// state into these, so the bar depends on no transcript/agent type. live and gate are
// independent: a loop can be live-and-gated, idle-and-gated, etc.
type loopBarEntry struct {
	id         uuid.UUID
	name       string
	live, gate bool
}

// loopBar renders the session's loops as one clickable bar line and hit-tests a click
// column back to a loop id (design §Active-loops bar). It is a PURE view: entries are the
// assembled rows in stable (creation) order, and focused and active are two
// independent privileged loop ids the visible cap always keeps — focused is the loop the
// viewport renders, active is the session's current default target (a selection event advances
// it without moving focus). max is the visible cap: at most max entries render, the rest fold
// into a "… +N" overflow marker so the bar never grows unbounded. A max <= 0 means no count cap. The two
// ids may coincide (the switch in priority then picks the highest band). The segment layout is
// width-INDEPENDENT, so HitTest (which takes no width) can never disagree with Render.
type loopBar struct {
	entries []loopBarEntry
	focused uuid.UUID
	active  uuid.UUID
	max     int
	hovered uuid.UUID // pointer target; zero means no segment is hovered
	phase   uint      // one-shot glow frame for the hovered segment
}

// Bar glyphs (design §Active-loops bar): a leading mark per segment — a FILLED ● for the
// focused loop, a hollow ○ for every other loop — plus a trailing gate mark.
const (
	barFocusedMark   = "●"  // the focused loop (filled circle)
	barUnfocusedMark = "○"  // a non-focused loop (hollow circle)
	barGateMark      = "!"  // this loop has a pending gate awaiting the user
	barMarkSep       = " "  // between the leading mark and the loop name
	barSep           = "  " // between adjacent segments (and before the overflow marker)
)

// barSeg is one rendered segment's provenance: the loop id it maps to and the half-open
// cell-column span [start, end) it occupies in the rendered line. HitTest scans these so a
// click maps to exactly the loop whose segment covers the column; separators, the overflow
// marker, and columns past the last segment carry no barSeg and hit-test to (_, false).
type barSeg struct {
	id    uuid.UUID
	start int
	end   int
}

// Render draws the bar as a single line: each visible loop's
// "<mark> <name>" (plus a trailing "!" when gated), joined by a small gap,
// followed by a "… +N" overflow marker when
// the visible cap hides loops. The visible set is bounded by max (not width), so a wide frame
// and a narrow one draw the same segments in the same columns — which is exactly why HitTest
// can resolve a click without a width. A non-positive width (a degenerate frame) draws
// nothing; an empty bar renders "". The row is returned unpadded/untruncated so the caller
// can compose it with a right-aligned status; the max cap keeps it within a normal frame and
// the terminal clips any overflow (from the tail, which carries no clicks).
func (b loopBar) Render(width int) string {
	if width <= 0 {
		return ""
	}
	_, line := b.layout()
	return line
}

// HitTest maps cell column x to the loop id whose rendered segment covers it, returning
// (id, true) for an in-segment column and (uuid.UUID{}, false) for a gap between segments,
// the "… +N" overflow marker, or any column past the last segment (a negative x is likewise
// (_, false)). It computes the SAME width-independent layout Render draws (segment widths via
// lipgloss.Width), so the returned id is always the loop actually drawn at x.
//
// PRECONDITION: hit-test only a bar you actually rendered. HitTest takes no width because the
// segment layout is width-independent, but a Render(width) with width <= 0 draws NOTHING —
// hit-testing such a bar would resolve a click against a row the user never saw. The caller
// (the modern shell) must not route clicks to a bar it rendered at width <= 0.
func (b loopBar) HitTest(x int) (uuid.UUID, bool) {
	segs, _ := b.layout()
	for _, s := range segs {
		if x >= s.start && x < s.end {
			return s.id, true
		}
	}
	return uuid.UUID{}, false
}

// cycle moves focus by dir over the VISIBLE order — the bar's entries in their stable
// creation order — wrapping at both ends, and returns the newly focused loop id. dir > 0
// steps forward, dir < 0 steps back, dir == 0 leaves focus unchanged. When the current focus
// is not among the entries it seeds at the first entry (forward) or the last (backward). An
// empty bar returns the current focused id unchanged (nothing to cycle). Focusing a
// currently-overflowed loop is fine — the focused loop is always kept visible by layout, so
// it comes into view on the next Render.
func (b loopBar) cycle(dir int) uuid.UUID {
	n := len(b.entries)
	if n == 0 || dir == 0 {
		return b.focused
	}
	step := 1
	if dir < 0 {
		step = -1
	}
	cur := -1
	for i := range b.entries {
		if b.entries[i].id == b.focused {
			cur = i
			break
		}
	}
	if cur < 0 {
		if step > 0 {
			return b.entries[0].id
		}
		return b.entries[n-1].id
	}
	next := ((cur+step)%n + n) % n
	return b.entries[next].id
}

// layout is the single source of the bar's geometry, shared by Render and HitTest so a drawn
// column and a hit-tested column can never disagree. It selects the kept set — prioritizing
// the focused loop, then the active loop, live loops, and the most-recent idle loops, capped
// at max — then renders those loops (in stable display order, with their cell spans) plus the
// "… +N" overflow marker for the hidden loops. It returns the kept segments and the rendered
// line.
func (b loopBar) layout() ([]barSeg, string) {
	if len(b.entries) == 0 {
		return nil, ""
	}
	return b.render(b.keptByPriority())
}

// keptByPriority returns the loops to display, in stable creation order, capped at max
// (max <= 0 means no cap). When the cap bites, it keeps the highest-priority loops: priority is
// focused, then active, then live, then the most-recent idle (higher index = more
// recent), so the focused/active and live loops are the ones that survive the cut
// (design §bar).
func (b loopBar) keptByPriority() []loopBarEntry {
	if b.max <= 0 || len(b.entries) <= b.max {
		return append([]loopBarEntry(nil), b.entries...)
	}
	order := b.priorityOrder()
	keep := make(map[int]bool, b.max)
	for _, idx := range order[:b.max] {
		keep[idx] = true
	}
	var out []loopBarEntry
	for i := range b.entries {
		if keep[i] {
			out = append(out, b.entries[i])
		}
	}
	return out
}

// priorityOrder returns entry indices ordered highest-priority first (focused, then active,
// then live, then most-recent idle), a stable ordering used to pick the kept set
// under a cap. It never
// reorders the DISPLAY — only which loops survive the cut — so the bar itself stays in
// creation order.
func (b loopBar) priorityOrder() []int {
	idx := make([]int, len(b.entries))
	for i := range idx {
		idx[i] = i
	}
	// Sort by descending priority (highest first). priority is injective — each index maps to
	// a distinct score — so ties never occur; SliceStable is used only to keep the ordering a
	// single, unambiguous stdlib call.
	sort.SliceStable(idx, func(i, j int) bool {
		return b.priority(idx[i]) > b.priority(idx[j])
	})
	return idx
}

// priority scores entry i for the visible cap: focused ranks above active, which ranks above
// every live loop, which ranks above every idle loop;
// within a band the more-recent loop (higher index) ranks higher so recency breaks ties. The
// bands are spaced by len(entries) so no index crosses a band. The focused and active
// loops are each banded above live so they always survive the cap (focus stays visible, the
// session's active target stays visible), never culled by a crowd of live subagents. When
// focused and active name the same entry the switch
// picks the highest matching band — that entry still survives the cap.
func (b loopBar) priority(i int) int {
	base := i // recency: later loops rank higher within a band
	n := len(b.entries)
	switch {
	case b.entries[i].id == b.focused:
		return 4*n + base
	case b.entries[i].id == b.active:
		return 3*n + base
	case b.entries[i].live:
		return n + base
	default:
		return base
	}
}

// render builds the kept loops into styled segments (with their plain cell spans recorded for
// HitTest) plus the trailing overflow marker, and returns both the spans and the joined line.
// Cell spans are measured on the PLAIN text (styling is zero-width ANSI), so the spans stay
// valid over the styled line — the same plain-measured / styled-drawn split viewport.go uses.
func (b loopBar) render(kept []loopBarEntry) ([]barSeg, string) {
	var (
		segs []barSeg
		sb   strings.Builder
		col  int
	)
	for i := range kept {
		if i > 0 {
			sb.WriteString(barSep)
			col += lipgloss.Width(barSep)
		}
		plain := b.segPlain(kept[i])
		w := lipgloss.Width(plain)
		segs = append(segs, barSeg{id: kept[i].id, start: col, end: col + w})
		sb.WriteString(b.segStyled(kept[i]))
		col += w
	}
	if hidden := len(b.entries) - len(kept); hidden > 0 {
		sb.WriteString(barSep)
		sb.WriteString(styles.StatusStyle.Render(overflowText(hidden)))
	}
	return segs, sb.String()
}

// segBody is a loop segment's compact ANSI-free "<mark> <name>" — the shared layout
// both segPlain (which HitTest measures spans on) and segStyled (which Render draws)
// build from, so a drawn column and a hit-tested column can never disagree. The
// leading mark encodes focus: filled ● when focused, hollow ○ otherwise.
func (b loopBar) segBody(e loopBarEntry) string {
	return b.leadMark(e) + barMarkSep + e.name
}

// segPlain is a loop segment's ANSI-free text — segBody plus a trailing "!" when gated — the
// form HitTest measures cell spans on. The gate mark is a SUFFIX so it never shifts the body.
func (b loopBar) segPlain(e loopBarEntry) string {
	s := b.segBody(e)
	if e.gate {
		s += barGateMark
	}
	return s
}

// segStyled renders unfocused segments faint (StatusStyle), so they read as quiet,
// subordinate context, while the focused loop is bright white at normal weight. The segment under the pointer instead
// gets the solid pastel-blue action color, with only its text label underlined (not the mark).
// A pending gate's "!" remains in the warn
// color. Styling is zero-width, so segStyled has the same display width as segPlain and the
// recorded cell spans stay valid.
func (b loopBar) segStyled(e loopBarEntry) string {
	body := b.segBody(e)
	if b.hovered != (uuid.UUID{}) && e.id == b.hovered {
		labelStart := len(b.leadMark(e) + barMarkSep)
		out := brandBlueLabelSpan(body, labelStart, len(body), b.phase)
		if e.gate {
			out += styles.NoticeWarnStyle.Render(barGateMark)
		}
		return out
	}
	if e.id == b.focused {
		out := lipgloss.NewStyle().Foreground(lipgloss.Color("15")).Render(body)
		if e.gate {
			out += styles.NoticeWarnStyle.Render(barGateMark)
		}
		return out
	}
	out := styles.StatusStyle.Render(body)
	if e.gate {
		out += styles.NoticeWarnStyle.Render(barGateMark)
	}
	return out
}

// leadMark is a segment's leading glyph: a FILLED ● for the focused loop, a hollow ○ for every
// other loop (design §bar). It reads the bar's focused id, so segPlain and segStyled agree on
// the mark and HitTest's cell spans line up with the drawn marks.
func (b loopBar) leadMark(e loopBarEntry) string {
	if e.id == b.focused {
		return barFocusedMark
	}
	return barUnfocusedMark
}

// overflowText is the "… +N" marker shown when the visible cap hides N loops.
func overflowText(hidden int) string {
	return "… +" + strconv.Itoa(hidden)
}
