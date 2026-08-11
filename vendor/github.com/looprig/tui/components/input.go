package components

import (
	"image/color"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/looprig/tui/styles"
)

// minInputLines and maxInputLines bound the composer's content height in lines. The
// editor starts at one row and grows with content up to the cap, after which the
// bubbles textarea scrolls internally (keeping the cursor visible) rather than
// pushing the surrounding layout off-screen.
const (
	minInputLines = 1
	maxInputLines = 10
)

// contentHeightSecurityLimit is the textarea's own MaxHeight. It is set far above
// maxInputLines on purpose: MaxHeight in the bubbles textarea is BOTH the visible-row
// cap AND an input gate — once the logical line count reaches MaxHeight the textarea
// refuses to insert further newlines (textarea.atContentLimit), silently dropping
// content. We must never drop the user's text, so the textarea's MaxHeight is parked
// high and the VISIBLE cap (maxInputLines) is enforced separately via SetHeight on the
// viewport window. The value just needs to exceed any realistic composer input.
const contentHeightSecurityLimit = 10000

// placeholder is the dim hint shown while the editor is empty.
const placeholder = "Type a message…"

// InputBox wraps a bubbles textarea: an auto-growing editor with the shared "▌"
// accent bar as its prompt (matching user-message rows), rendered inside a bordered
// box. No char limit, no line numbers, no "> " prompt. The box height tracks the
// content between minLines and maxInputLines.
//
// minLines, bg, and padV are per-INSTANCE so a single composer implementation serves both
// shells: the scrollback Screen keeps the historical 1-line, background-free, unpadded editor
// (the NewInputBox defaults), and the modern viewport opts into a taller, gray-filled, padded
// panel via SetMinLines/SetBackground/SetVerticalPadding. Nothing else changes, so the
// scrollback composer stays byte-identical.
type InputBox struct {
	ta       textarea.Model
	minLines int    // visible-height floor; default minInputLines (1)
	width    int    // last Resize width — the column budget each row's fill spans
	hasBG    bool   // whether the modern gray panel fill is enabled (default: off)
	bgOpen   string // SGR that turns the fill on (derived once in SetBackground); "" when off
	bgReset  string // SGR that turns the fill off
	padV     int    // background-filled padding rows above AND below the text region (default 0)
}

// NewInputBox returns a configured, focused prompt editor.
//
// Enter is left unbound on the textarea so screen.go can use it as submit; newline
// insertion is bound to TWO keys so it works regardless of terminal capability:
//
//   - Shift+Enter (PRIMARY, preferred) — only distinguishable from plain Enter on
//     terminals that implement the Kitty keyboard protocol AND only when the program
//     requests "report all keys as escape codes" (flag 8). screen.go's View() sets
//     KeyboardEnhancements.ReportAllKeysAsEscapeCodes for exactly this reason; without
//     it the Kitty spec keeps Enter as a legacy byte and Shift+Enter arrives as plain
//     Enter (→ submit). Supported on kitty, Ghostty, WezTerm, foot, Alacritty, and
//     recent iTerm2 (with the protocol option enabled).
//   - Ctrl+J (UNIVERSAL FALLBACK) — the LF byte (0x0A), delivered by EVERY terminal
//     with no protocol required; v2 decodes it as Code 'j' + ModCtrl (String()=="ctrl+j").
//     This is the only way to type a literal newline on terminals that cannot deliver a
//     distinct Shift+Enter (Apple Terminal, many VS Code setups). It is purely additive
//     — Shift+Enter stays primary. Ctrl+J does not collide with any global binding in
//     screen.go (which handles only ctrl+c, ctrl+t, and esc).
func NewInputBox() InputBox {
	ta := textarea.New()
	ta.CharLimit = 0
	ta.ShowLineNumbers = false
	// No per-line prompt: the ▌ accent is now the composer panel's left border (drawn
	// by styles.BoxStyle over every row, including the top/bottom padding), so a
	// textarea prompt would double it. The leading gap is the box's PaddingLeft.
	ta.Prompt = ""
	ta.Placeholder = placeholder
	// Bind newline insertion to Shift+Enter (primary) OR Ctrl+J (universal fallback),
	// freeing Enter for submit in screen.go. See the doc comment above for why both.
	ta.KeyMap.InsertNewline = key.NewBinding(
		key.WithKeys("shift+enter", "ctrl+j"),
		key.WithHelp("shift+enter", "insert newline"),
	)
	// v2 restructures the per-state styles under a single Styles value accessed via
	// Styles()/SetStyles. The composer paints NO background (lowest resize-stranding
	// footprint — see styles.BoxStyle), so the only style fix is clearing the focused
	// CursorLine: the default DefaultDarkStyles gives it a black background ("0"), a
	// stray dark patch as wide as the text. An empty style leaves the editor plain, like
	// the user-message rows.
	//
	// The default Cursor style is left untouched, and is safe to leave so: textarea's
	// DefaultDarkStyles is built by resolving lipgloss's LightDark light/dark *closure*
	// at construction with a literal dark choice, so the resulting style holds only
	// static colors. No LightDark value survives into the live style, so rendering it
	// never triggers a runtime OSC-11 background query (which the codebase deliberately
	// avoids; see styles.NewMarkdownRenderer).
	s := ta.Styles()
	s.Focused.CursorLine = lipgloss.NewStyle()
	ta.SetStyles(s)
	// DynamicHeight makes the textarea recompute its height from the VISUAL (soft-wrap
	// aware) line count on every mutation, and — crucially — clamp its internal viewport
	// scroll offset back to the top when the content fits. Without it, the viewport stays
	// scrolled to wherever the cursor was as the user typed each new line: SetHeight only
	// ever scrolls DOWN to reveal the cursor, never back UP to collapse the slack above
	// it, so the box hid the first line(s) and showed a phantom trailing blank. MaxHeight
	// is parked at contentHeightSecurityLimit (NOT maxInputLines) because MaxHeight doubles as
	// an input gate that drops newlines once reached; the visible [1, maxInputLines] cap
	// is applied separately in capHeight via SetHeight.
	ta.DynamicHeight = true
	ta.MinHeight = minInputLines
	ta.MaxHeight = contentHeightSecurityLimit
	ta.Focus()
	b := InputBox{ta: ta, minLines: minInputLines}
	b.capHeight()
	return b
}

// SetMinLines sets the composer's minimum visible height (default minInputLines). The
// MODERN viewport uses 2 for a roomier panel; the scrollback Screen never calls this, so
// it keeps the historical single line. Below-1 values are ignored (fail-safe). It moves
// BOTH the textarea's own MinHeight and the visible cap in lockstep, then re-caps so the
// change takes effect immediately.
func (b *InputBox) SetMinLines(n int) {
	if n < 1 {
		return
	}
	b.minLines = n
	b.ta.MinHeight = n
	b.capHeight()
}

// SetBackground enables the MODERN gray panel fill of color bg behind every composer row.
// It derives bg's SGR open/reset pair ONCE (styles.DeriveBackgroundSGR) and View then paints
// each rendered line to the box width with it — a per-row fill that re-opens the background
// after the textarea's internal SGR resets, so it never leaves the holes a plain
// Background() wrap does (and the empty end-of-buffer rows fill too). It does NOT tint the
// textarea's own Base style: the focused cursor line therefore keeps the empty style
// NewInputBox already set (no default "black box"), and the uniform post-fill supplies the
// gray instead. The scrollback Screen never calls this, so its composer stays
// background-free (styles.BoxStyle). MODERN-safe: the viewport re-renders the whole frame
// per tick, so the fill never strands into scrollback the way it could in the print-once
// surface.
func (b *InputBox) SetBackground(bg color.Color) {
	open, reset := styles.DeriveBackgroundSGR(bg)
	if open == "" {
		return // degenerate derivation: leave the composer background-free (fail-safe)
	}
	b.hasBG = true
	b.bgOpen = open
	b.bgReset = reset
}

// SetVerticalPadding sets the number of padding rows View draws ABOVE and BELOW the text region,
// so the modern composer reads as a padded box ([pad][text…][pad]) rather than a bare line. It
// defaults to 0 (the scrollback Screen never calls this, so its composer stays byte-identical);
// the modern viewport sets 1. Negative values are ignored (fail-safe). Each padding row carries
// the box's ▌ accent edge (the rail runs unbroken through the padding), gray-filled by the modern
// panel (SetBackground) so it reads as part of the box. Padding does NOT change the editor's
// auto-grow — the text region still grows to maxInputLines — it only frames it, so the box's
// rendered height is the text height plus 2*padV.
func (b *InputBox) SetVerticalPadding(n int) {
	if n < 0 {
		return
	}
	b.padV = n
}

// Height is the editor's visible content height in rows: the textarea's current row
// count clamped to [minInputLines, maxInputLines]. It excludes the border frame.
//
// It reads ta.Height() rather than ta.LineCount() so it tracks VISUAL rows (a single
// long logical line that soft-wraps occupies several rows), matching what View()
// actually renders. DynamicHeight keeps ta.Height() equal to the total visual line
// count (capped at contentHeightSecurityLimit, far above maxInputLines), so once capHeight
// has applied the visible cap this returns that capped value, and before capping it
// returns the true content height — both already within [min, max] after clamp.
func (b InputBox) Height() int {
	return clamp(b.ta.Height(), b.minLines, maxInputLines)
}

// capHeight pins the visible viewport window to [minInputLines, maxInputLines]. The
// textarea's DynamicHeight grows ta.Height() to the full visual line count on each
// mutation (and resets its scroll to the top while the content fits); this caps the
// visible window so the box grows only up to maxInputLines, after which the textarea
// scrolls internally to keep the cursor in view. Call after every mutation.
func (b *InputBox) capHeight() {
	b.ta.SetHeight(b.Height())
}

// Value returns the current text.
func (b *InputBox) Value() string {
	return b.ta.Value()
}

// Reset clears the text.
func (b *InputBox) Reset() {
	b.ta.Reset()
	b.capHeight()
}

// SetValue replaces the text.
func (b *InputBox) SetValue(s string) {
	b.ta.SetValue(s)
	b.capHeight()
}

// Resize sets the box width; the inner textarea is the box width minus the border's
// horizontal frame. The height auto-grows with content, so it is not set here.
func (b *InputBox) Resize(width int) {
	b.width = width
	inner := width - styles.BoxStyle.GetHorizontalFrameSize()
	if inner < 1 {
		inner = 1
	}
	b.ta.SetWidth(inner)
}

// Focus focuses the editor and returns its Blink command.
func (b *InputBox) Focus() tea.Cmd {
	return b.ta.Focus()
}

// Update forwards the message to the textarea and grows the editor to fit the
// current content (capped at maxInputLines, past which it scrolls internally).
func (b *InputBox) Update(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	b.ta, cmd = b.ta.Update(msg)
	b.capHeight()
	return cmd
}

// View renders the editor inside the bordered box. The box grows with the content because
// the inner textarea height tracks Height(). In MODERN mode (SetBackground called) every
// rendered row — the ▌ edge, its one-column left pad, the text, and any empty end-of-buffer
// rows — is filled to the box width with the gray panel color, so the composer reads as one
// continuous panel; the default (scrollback) box paints nothing.
//
// With vertical padding (SetVerticalPadding, modern sets 1) padV rail rows are added ABOVE and
// BELOW the text rows so the composer reads as a padded box ([pad][text…][pad]) rather than a
// bare line. A padding row is the box's ▌ edge alone (so the accent rail runs unbroken through
// it), gray-filled to the box width when a background is set. The scrollback composer sets
// neither background nor padding, so it returns the bare box unchanged (byte-identical).
func (b *InputBox) View() string {
	view := styles.BoxStyle.Render(b.ta.View())
	if !b.hasBG && b.padV == 0 {
		return view
	}
	lines := strings.Split(view, "\n")
	if b.hasBG {
		for i, line := range lines {
			lines[i] = styles.FillLineBackgroundWith(line, b.width, b.bgOpen, b.bgReset)
		}
	}
	if b.padV > 0 {
		// A pad row is the box's ▌ edge alone (BoxStyle.Render of empty content), so the accent
		// rail runs UNBROKEN through the padding rather than breaking at a blank gap. It is then
		// gray-filled like the text rows (when a background is set) so it reads as one panel.
		pad := styles.BoxStyle.Render("")
		if b.hasBG {
			pad = styles.FillLineBackgroundWith(pad, b.width, b.bgOpen, b.bgReset)
		}
		padded := make([]string, 0, len(lines)+2*b.padV)
		for i := 0; i < b.padV; i++ {
			padded = append(padded, pad)
		}
		padded = append(padded, lines...)
		for i := 0; i < b.padV; i++ {
			padded = append(padded, pad)
		}
		lines = padded
	}
	return strings.Join(lines, "\n")
}

// clamp constrains v to the inclusive range [lo, hi].
func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
