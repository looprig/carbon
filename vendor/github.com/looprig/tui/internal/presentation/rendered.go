package presentation

import (
	"regexp"
	"strings"

	"github.com/looprig/tui/styles"
)

// renderedLine is one rendered transcript line carrying BOTH its drawn form and the
// provenance the modern viewport needs. styled is the ANSI-saturated string the
// existing entry renderer produced (drawn verbatim); plain is the ANSI-free visible
// text — the ONLY thing selection measures, extracts and copies, so no escape sequence
// ever reaches the clipboard and cell↔rune math operates on measurable text. entry and
// sub locate the line: entry is the source entry's displayID (for click-to-collapse and
// stable anchoring) and sub is the 0-based line index within that entry. clickable is
// deliberately row-level provenance: only a header whose click produces a visible fold
// change sets it, so hover styling can never make passive transcript text look actionable.
type renderedLine struct {
	styled    string    // drawn (ANSI) — from the existing renderer, unchanged
	plain     string    // ANSI-free visible text; the ONLY thing selection extracts/copies
	entry     displayID // provenance: which entry this line belongs to
	sub       int       // intra-entry line index (0-based)
	clickable bool      // true only when clicking this exact row performs a visible action
}

// renderEntryLines renders one committed entry to its provenance-carrying lines. It
// WRAPS the existing scrollback renderer (renderEntry) — the styled output is the exact
// scrollback lines, byte for byte — and, per styled line, attaches the ANSI-free plain
// text and the (entry, sub) provenance. collapsed is the modern viewport's fold state
// and is the INVERSE of renderEntry's expand flag, so a collapsed thinking fold yields
// fewer lines than an expanded one. It never re-implements or alters rendering.
func renderEntryLines(e entry, width int, collapsed bool) []renderedLine {
	styled := renderEntry(e, !collapsed, width)
	out := make([]renderedLine, len(styled))
	for i, line := range styled {
		out[i] = renderedLine{
			styled: line,
			plain:  plainFromStyled(line),
			entry:  e.ID,
			sub:    i,
		}
	}
	return out
}

// toolRunSummaryLines renders a collapsed contiguous run of tool entries as one semantic
// activity summary node. Its first line carries sub == 0 and entry == the run's
// first displayID (runID), so a click toggles the whole run's fold via the existing
// header-click handler; ctrl+t (ToggleAll) flips it with the global default. The node is
// red-tinted when any call failed.
func toolRunSummaryLines(run []entry, width int) []renderedLine {
	runID := run[0].ID
	calls := make([]ToolCallView, 0, len(run))
	for i := range run {
		if len(run[i].Calls) > 0 {
			calls = append(calls, run[i].Calls[0])
		}
	}
	text, anyFailed := toolRunSummary(calls)
	status := styles.NodeOK
	if anyFailed {
		status = styles.NodeFailed
	}
	styled := railNodeStyled(styles.ToolNode(status), text, styles.ToolCallStyle, 0, width)
	out := make([]renderedLine, len(styled))
	for i, line := range styled {
		out[i] = renderedLine{styled: line, plain: plainFromStyled(line), entry: runID, sub: i, clickable: i == 0}
	}
	return out
}

// The escape-stripping passes, compiled once. plainFromStyled feeds the system
// clipboard (a trust boundary), and tool-card bodies pass raw subprocess bytes through
// VERBATIM, so a styled line may carry escape families the renderer itself never emits.
// The passes therefore cover far more than glamour/lipgloss output, and a final guard
// makes "no 0x1b byte" a hard guarantee (fail secure: over-stripping exotic malformed
// content at the clipboard boundary is the correct tradeoff).
var (
	// ansiOSC matches an OSC string sequence: ESC ']', a body, and a BEL (0x07) or ST
	// (ESC '\\') terminator. It covers the OSC-8 hyperlink wrappers glamour emits for
	// markdown links, stripping the wrapper to its visible link text.
	ansiOSC = regexp.MustCompile("\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\)")
	// ansiString matches the other string sequences that can appear in raw payloads —
	// DCS (ESC P), SOS (ESC X), PM (ESC ^) and APC (ESC _) — up to their ST or BEL
	// terminator. Stripped BEFORE the generic single-escape pass so the introducer byte
	// (P/X/^/_) is not mis-eaten as a bare escape's final byte.
	ansiString = regexp.MustCompile("\x1b[P^X_].*?(?:\x1b\\\\|\x07)")
	// ansiCSI matches a CSI sequence: ESC '[', parameter bytes (0-9 ; : ?), optional
	// intermediate bytes (0x20–0x2F) and one final byte (0x40–0x7E). The ':' allows the
	// ISO-8613-6 colon sub-parameter form (e.g. truecolor "38:2:r:g:b") a subprocess may
	// emit, which the plain ';'/'?' class would miss.
	ansiCSI = regexp.MustCompile("\x1b\\[[0-9;:?]*[ -/]*[@-~]")
	// ansiEscape matches an nF / two-byte / charset-select escape: ESC, optional
	// intermediate bytes (0x20–0x2F) and one final byte (0x30–0x7E) — e.g. "\x1b(B",
	// "\x1bM". It runs AFTER the string-sequence and CSI passes so a well-formed OSC/DCS/
	// CSI is already gone and only genuine single-byte-final escapes remain to match.
	ansiEscape = regexp.MustCompile("\x1b[ -/]*[0-~]")
)

// plainFromStyled strips ALL terminal escape sequences from a styled line so the result
// is exactly the visible text — no 0x1b byte remains, GUARANTEED. This is the correct
// semantic for "copy what you see": selection measures and extracts plain, never the
// styled string (whose ANSI would corrupt cell math and reach the clipboard). Because
// tool-card bodies carry raw subprocess bytes, the styled line may hold escape families
// the renderer never emits; the passes cover OSC, DCS/SOS/PM/APC, CSI (with colon
// sub-parameters) and generic single escapes, in an order where string sequences are
// removed before the generic pass, then a final guard removes any residual ESC left by a
// malformed or truncated sequence — the hard fail-secure guarantee at the clipboard
// boundary.
func plainFromStyled(styled string) string {
	s := ansiOSC.ReplaceAllString(styled, "")
	s = ansiString.ReplaceAllString(s, "")
	s = ansiCSI.ReplaceAllString(s, "")
	s = ansiEscape.ReplaceAllString(s, "")
	return strings.ReplaceAll(s, "\x1b", "")
}
