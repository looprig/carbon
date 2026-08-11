// Package styles holds the shared lipgloss styles and glamour helpers for the
// LoopRig TUI. It is a leaf package: it depends only on charm libraries and
// must never import the tui package or any of its other subpackages.
package styles

import (
	"image/color"
	"strings"

	"charm.land/glamour/v2"
	"charm.land/glamour/v2/ansi"
	glamourstyles "charm.land/glamour/v2/styles"
	"charm.land/lipgloss/v2"
)

// Dot is the leading marker rendered before assistant/markdown blocks — the PLAIN
// layout form (the bullet glyph + a trailing space, dotWidth columns). The rendered
// bullet is COLORED via LitDot; Dot itself stays uncolored so it doubles as the
// width/layout reference (and the ANSI-stripped substring tests match against).
const Dot = "● "

// DotColor is the assistant bullet's foreground color.
var DotColor = lipgloss.Color("#D4F84D")

// Nexus markdown palette applied by NewMarkdownRenderer over glamour's DarkStyleConfig
// for non-code markdown, plus the base neutral shared with the code-block theme below.
// MarkdownHeadingColor replaces glamour's heading blue (ANSI 256 "39") and
// MarkdownInlineCodeColor replaces glamour's inline `code` red (ANSI 256 "203"), both
// with the same softer brand blue. MarkdownCodeNeutralColor is the base foreground for
// code text. They are hex strings (not lipgloss.Color) because glamour's
// StylePrimitive.Color is a *string.
var (
	MarkdownHeadingColor     = "#A2D2FF"
	MarkdownInlineCodeColor  = "#A2D2FF"
	MarkdownCodeNeutralColor = "#C4C4C4"
)

// Nexus code-block syntax-highlighting palette — four calm tones over a dark fill.
// Deliberately contains NO red: glamour's stock chroma theme is full of reds (salmon
// operators, red diff/deleted lines, and a RED-background Error token that painted
// box-drawing file trees like "├──" as alarm bars), and the old approach of inheriting
// that theme and patching reds token-by-token left every un-patched token a latent red.
// nexusChroma owns the whole theme instead, so a token can only ever be a color we chose.
var (
	codeKeywordColor = "#A2D2FF" // keywords, types, builtins, function/class names — brand blue
	codeStringColor  = "#D4F84D" // string literals — lime accent (matches the assistant dot)
	codeCommentColor = "#737373" // comments — faint gray (matches the input/accent bars)
	codeBgColor      = "#373737" // code-block fill — dark gray (glamour's default block bg)
)

// colorPtr / flagPtr adapt palette values to glamour's pointer-typed StylePrimitive fields.
func colorPtr(s string) *string { return &s }
func flagPtr(b bool) *bool      { return &b }

// nexusChroma is the single source of truth for code-block syntax highlighting: a
// complete chroma theme built from the Nexus palette. NewMarkdownRenderer assigns it to
// cfg.CodeBlock.Chroma, so glamour highlights against our colors directly and never
// touches glamour's DarkStyleConfig chroma defaults. Every token is set explicitly — an
// unset token would fall back to chroma's own built-in style — and none is red. It is
// read-only package state and is never mutated, so sharing the pointer is safe.
var nexusChroma = ansi.Chroma{
	// Base — identifiers, operators, punctuation, numbers, and chroma's Error catch-all
	// (the token emitted for input a lexer can't parse, e.g. box-drawing tree glyphs):
	// all neutral, so untokenizable text reads as plain code rather than a red alarm.
	Text:          ansi.StylePrimitive{Color: colorPtr(MarkdownCodeNeutralColor)},
	Error:         ansi.StylePrimitive{Color: colorPtr(MarkdownCodeNeutralColor)},
	Operator:      ansi.StylePrimitive{Color: colorPtr(MarkdownCodeNeutralColor)},
	Punctuation:   ansi.StylePrimitive{Color: colorPtr(MarkdownCodeNeutralColor)},
	Name:          ansi.StylePrimitive{Color: colorPtr(MarkdownCodeNeutralColor)},
	NameAttribute: ansi.StylePrimitive{Color: colorPtr(MarkdownCodeNeutralColor)},
	NameConstant:  ansi.StylePrimitive{Color: colorPtr(MarkdownCodeNeutralColor)},
	NameOther:     ansi.StylePrimitive{Color: colorPtr(MarkdownCodeNeutralColor)},
	Literal:       ansi.StylePrimitive{Color: colorPtr(MarkdownCodeNeutralColor)},
	LiteralNumber: ansi.StylePrimitive{Color: colorPtr(MarkdownCodeNeutralColor)},
	LiteralDate:   ansi.StylePrimitive{Color: colorPtr(MarkdownCodeNeutralColor)},

	// Language constructs — keywords, types, builtins, definitions, decorators, tags.
	Keyword:          ansi.StylePrimitive{Color: colorPtr(codeKeywordColor)},
	KeywordReserved:  ansi.StylePrimitive{Color: colorPtr(codeKeywordColor)},
	KeywordNamespace: ansi.StylePrimitive{Color: colorPtr(codeKeywordColor)},
	KeywordType:      ansi.StylePrimitive{Color: colorPtr(codeKeywordColor)},
	NameBuiltin:      ansi.StylePrimitive{Color: colorPtr(codeKeywordColor)},
	NameClass:        ansi.StylePrimitive{Color: colorPtr(codeKeywordColor), Bold: flagPtr(true)},
	NameFunction:     ansi.StylePrimitive{Color: colorPtr(codeKeywordColor)},
	NameDecorator:    ansi.StylePrimitive{Color: colorPtr(codeKeywordColor)},
	NameTag:          ansi.StylePrimitive{Color: colorPtr(codeKeywordColor)},
	NameException:    ansi.StylePrimitive{Color: colorPtr(codeKeywordColor)},

	// Strings — lime.
	LiteralString:       ansi.StylePrimitive{Color: colorPtr(codeStringColor)},
	LiteralStringEscape: ansi.StylePrimitive{Color: colorPtr(codeStringColor)},

	// Comments & meta — faint gray.
	Comment:           ansi.StylePrimitive{Color: colorPtr(codeCommentColor)},
	CommentPreproc:    ansi.StylePrimitive{Color: colorPtr(codeCommentColor)},
	GenericSubheading: ansi.StylePrimitive{Color: colorPtr(codeCommentColor)},

	// Diff markers stay neutral (no red removed-lines, no green added-lines); emphasis
	// and strong keep their weight rather than a color.
	GenericDeleted:  ansi.StylePrimitive{Color: colorPtr(MarkdownCodeNeutralColor)},
	GenericInserted: ansi.StylePrimitive{Color: colorPtr(MarkdownCodeNeutralColor)},
	GenericEmph:     ansi.StylePrimitive{Color: colorPtr(MarkdownCodeNeutralColor), Italic: flagPtr(true)},
	GenericStrong:   ansi.StylePrimitive{Color: colorPtr(MarkdownCodeNeutralColor), Bold: flagPtr(true)},

	// The dark fill behind highlighted code.
	Background: ansi.StylePrimitive{BackgroundColor: colorPtr(codeBgColor)},
}

// LitDot is the COLORED leading marker actually rendered before an assistant bullet:
// the DotColor-foregrounded glyph plus a plain trailing space. Its display width equals
// Dot's (the color is zero-width ANSI), so narration alignment is unchanged.
var LitDot = lipgloss.NewStyle().Foreground(DotColor).Render("●") + " "

// NodeStatus selects a rail node's tint. A hollow-circle tool/subagent node is
// lime when OK, red when failed, and a pulsing filled glyph while running.
type NodeStatus uint8

const (
	NodeOK NodeStatus = iota
	NodeFailed
	NodeRunning
)

// dotNodeHollow is the tool/subagent node glyph; dotNodeRunning is its running form.
const (
	dotNodeHollow  = "○"
	dotNodeRunning = "◍"
)

// FailColor tints a failed rail node and its header. Kept distinct from DotColor so a
// failure reads red against the neon assistant bullet.
var FailColor = lipgloss.Color("#FF6B6B")

// ToolNode returns the COLORED node glyph (glyph + trailing space, 2 columns wide like
// LitDot) for a tool/subagent node at the given status.
func ToolNode(s NodeStatus) string {
	switch s {
	case NodeFailed:
		return lipgloss.NewStyle().Foreground(FailColor).Render(dotNodeHollow) + " "
	case NodeRunning:
		return lipgloss.NewStyle().Foreground(DotColor).Render(dotNodeRunning) + " "
	default: // NodeOK
		return lipgloss.NewStyle().Foreground(DotColor).Render(dotNodeHollow) + " "
	}
}

// AccentBar is the left bar marker shared by user-message rows and the input
// prompt. AccentBarPrompt is the bar plus its trailing space, used as the prompt.
const (
	AccentBar       = "▌"
	AccentBarPrompt = AccentBar + " "
	// WorkflowActivityMarker starts a new workflow-run notification rail.
	WorkflowActivityMarker = "●"
	// WorkflowActivityContinuationMarker continues an already-active workflow-run rail.
	WorkflowActivityContinuationMarker = "|"
)

// ThinkingHeader labels the model's reasoning block.
const ThinkingHeader = "thinking"

// Role styles (exported so package tui can use them).
var (
	UserStyle        = lipgloss.NewStyle().Bold(true)
	InterruptedStyle = lipgloss.NewStyle().Faint(true).Italic(true)
	StatusStyle      = lipgloss.NewStyle().Faint(true)
	// StatusWorkingStyle / StatusWorkingAltStyle are the two phases of the status-line
	// icon while the model is actively working: lit (the assistant lime) and its blink
	// alternate (white). Waiting/thinking alternate between them on the blink tick for a
	// gentle pulse; streaming holds the lit lime. At rest / when blocked the icon falls
	// back to the faint StatusStyle.
	StatusWorkingStyle    = lipgloss.NewStyle().Foreground(DotColor)
	StatusWorkingAltStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("15"))
	// QueuedStyle renders the transient queued-input affordance — the pending,
	// not-yet-running echo of a submitted user message shown below the live tail. It
	// is FAINT (not bold like UserStyle) so a queued line reads as a quieter "this is
	// waiting" hint, distinct from the bold committed user row it later promotes to.
	QueuedStyle = lipgloss.NewStyle().Faint(true)
	// QueuedLabelStyle renders the "QUEUED" banner shown ABOVE a queued message: the
	// brand blue (MarkdownHeadingColor, #A2D2FF), bold, so the pending state reads as a
	// clear labeled header distinct from the faint message preview beneath it.
	QueuedLabelStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(MarkdownHeadingColor)).Bold(true)
)

// HeadlineStyle renders the bold headline word shown beside the assistant dot for an
// empty-text tool step (the live "working" synonym and the committed "Done") — design
// §3 rule 4. Bold so the headline stands out beside the bullet, matching the bold
// emphasis the user message and prompt headers use.
var HeadlineStyle = lipgloss.NewStyle().Bold(true)

// Notice styles color a leveled notification's shared "▌ " accent bar (and text) by
// severity. All three reuse the SAME accent-bar wrapper as user messages and differ
// only in foreground color: info is a neutral gray (color 8), warn is bright yellow
// (color 11), error is red (color 9). They are selected per entry via NoticeStyle;
// callers must not branch on the level themselves.
var (
	NoticeInfoStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))  // neutral gray (user-message tone)
	NoticeWarnStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("11")) // bright yellow
	NoticeErrorStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))  // red
)

// NoticeStyle maps a notice level (0=info, 1=warn, 2=error) to its style. An unknown
// level falls back to the neutral info style (fail-safe: an unrecognised level must
// never panic or pick an alarming color). The level argument is a plain uint8 so the
// leaf styles package need not import package tui's noticeLevel type (it depends only
// on charm libraries — see the package doc).
func NoticeStyle(level uint8) lipgloss.Style {
	switch level {
	case 1:
		return NoticeWarnStyle
	case 2:
		return NoticeErrorStyle
	default:
		return NoticeInfoStyle
	}
}

// Tool-call styles: a tool card and its result preview render dim, subordinate to
// the assistant narration they nest beneath.
var (
	ToolCallStyle   = lipgloss.NewStyle().Faint(true) // "└ ToolName  Summary  <glyph>" lines
	ToolResultStyle = lipgloss.NewStyle().Faint(true) // indented result-preview lines
)

// AccentBarStyle colors the left accent bar ("▌") on user rows (and the queued-input
// echo) a mid gray (#737373).
var AccentBarStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#737373"))

// InputAccent colors the composer's left ▌ edge — the same mid gray as the
// AccentBarStyle bar on user rows, so the input reads as the same accent.
var InputAccent = lipgloss.Color("#737373")

// PanelBg is the subtle dark-gray fill painted behind MODERN mode's user-message
// rows and its composer panel — a quiet block that sets the user's own input apart from
// the assistant narration without competing with the accent bar. It sits in the same
// neutral family as AccentBarStyle's #737373 and is dark enough that both bold text and
// the dim placeholder stay readable on a dark terminal. MODERN-ONLY: the scrollback rows
// and the scrollback composer never paint a background (see BoxStyle's rationale) — the
// fill is safe in the alt-screen viewport because it re-renders the whole frame each tick.
var PanelBg = lipgloss.Color("#303030")

// UserBgStyle is the background-only style the modern gray fill is derived from (its SGR
// open/close pair). It carries no foreground, so a filled row's own accent bar and markdown
// keep their colors on top of the fill.
var UserBgStyle = lipgloss.NewStyle().Background(PanelBg)

// SGR reset sequences a styled line may carry. glamour ends its spans with the long form
// ("\x1b[0m"); lipgloss/x-ansi use the short form ("\x1b[m"). Both fully reset — INCLUDING
// any background — so a naive Background() wrap loses the fill at the first one.
const (
	sgrResetLong  = "\x1b[0m"
	sgrResetShort = "\x1b[m"
)

// bgOpen / bgReset are the SGR pair that turns the ModernPanelBg fill on and
// off, derived ONCE from UserBgStyle so the color has a single source of truth and no
// escape is hardcoded here.
var bgOpen, bgReset = DeriveBackgroundSGR(PanelBg)

// DeriveBackgroundSGR returns the SGR pair that turns a background fill of color bg on
// (open) and off (reset). It renders a NUL sentinel through a background-only style
// (lipgloss emits open + NUL + reset) and splits on the sentinel, so no escape is ever
// hardcoded. A caller filling many lines with one color derives the pair once and passes it
// to FillLineBackgroundWith. A degenerate result (open == "") signals the caller to skip
// filling (fail-safe).
func DeriveBackgroundSGR(bg color.Color) (open, reset string) {
	rendered := lipgloss.NewStyle().Background(bg).Render("\x00")
	if i := strings.IndexByte(rendered, 0); i >= 0 {
		return rendered[:i], rendered[i+1:]
	}
	return "", sgrResetShort
}

// FillLineBackground paints the ModernPanelBg gray fill behind one already-styled line to
// width display columns — the convenience over FillLineBackgroundWith for the one shared
// gray. See FillLineBackgroundWith for the fill semantics. MODERN-ONLY: scrollback never
// fills a background (see BoxStyle).
func FillLineBackground(line string, width int) string {
	return FillLineBackgroundWith(line, width, bgOpen, bgReset)
}

// FillLineBackgroundWith paints the fill whose SGR pair is (open, reset) behind one
// already-styled line so it spans width display columns: it opens the fill, RE-OPENS it
// after every inner SGR reset (glamour and lipgloss end their spans with full resets that
// would otherwise punch holes in the fill — a plain Background() wrap only tints up to the
// first reset), pads the visible content out to width with fill-carrying spaces, and closes
// with a reset. An empty open returns the line unchanged (fail-safe: a degenerate
// derivation must never emit a broken escape). A width at or below the line's own display
// width adds no padding (the fill still spans the content). It is the shared primitive
// behind MODERN mode's gray user rows and its gray composer panel.
func FillLineBackgroundWith(line string, width int, open, reset string) string {
	if open == "" {
		return line
	}
	body := strings.ReplaceAll(line, sgrResetLong, sgrResetLong+open)
	body = strings.ReplaceAll(body, sgrResetShort, sgrResetShort+open)
	pad := width - lipgloss.Width(line)
	if pad < 0 {
		pad = 0
	}
	return open + body + strings.Repeat(" ", pad) + reset
}

// composerBorder draws ONLY a left ▌ edge (no top/right/bottom), so the accent runs
// down the left of the (auto-growing) editor.
var composerBorder = lipgloss.Border{Left: AccentBar}

// BoxStyle frames the composer (input) box as the lowest-footprint affordance: a
// left-only ▌ accent edge + one space of left padding, and NOTHING else — no border
// box, no background fill, no top/bottom padding. This is deliberate: the bubbletea v2
// inline renderer strands the composer's rows into scrollback when a resize desyncs
// its relative cursor, and the loudness of that artifact scales with how much each row
// paints. A single, full-width-tinted or bordered row strands as a bar/box; a bare
// "▌ text" row strands as a faint one-glyph fragment. So the box paints no full-width
// glyphs and occupies the fewest rows possible (just the editor's content lines). The
// editor renders to the right of the edge; callers subtract the horizontal frame (edge
// + left padding = 2 cols) from the box width to size the inner textarea.
var BoxStyle = lipgloss.NewStyle().
	Border(composerBorder, false, false, false, true).
	BorderForeground(InputAccent).
	PaddingLeft(1)

// PromptHeaderStyle renders a prompt box's header label (e.g. "Approve Bash?"),
// bold so the action reads at a glance above the body.
var PromptHeaderStyle = lipgloss.NewStyle().Bold(true)

// PromptHintStyle renders a prompt box's faint secondary hints — the key legend
// ("↑/↓ select · …") and the "(+N more pending)" queue-depth note.
var PromptHintStyle = lipgloss.NewStyle().Faint(true)

// PromptCursorStyle renders the ▸ cursor marking the selected choice row, bold so
// the selection stands out from the unselected rows.
var PromptCursorStyle = lipgloss.NewStyle().Bold(true)

// SubagentCursor is the leading marker of a collapsed subagent activity line — the
// "▸ <agent>: <verb>" row attributing a subagent loop's StepDone to its agent. It
// reuses the same ▸ glyph as the prompt choice cursor (a "drill-in"/secondary marker),
// plus a trailing space.
const SubagentCursor = "▸ "

// SubagentStyle renders a collapsed subagent activity line ("▸ <agent>: done"). It is
// FAINT so a subagent's collapsed-but-present step reads as quieter, subordinate
// chatter beneath the primary (orchestrator) narration — matching the faint tool-card
// and queued-affordance tone, distinct from the bold primary user/assistant rows.
var SubagentStyle = lipgloss.NewStyle().Faint(true)

// ThinkingStyle renders the model's reasoning block: faint (never italic),
// subordinate to the assistant narration it precedes. Italic is deliberately
// omitted — it skewed the "│ " left rail and broke the column alignment; a
// non-italic rail renders as a clean, unbroken vertical line.
var ThinkingStyle = lipgloss.NewStyle().Faint(true)

// RailColor is the quiet dark-neutral foreground used only for the assistant-step
// timeline. It keeps the structural "│" visible enough to follow without competing
// with reasoning, narration, result text, or the status-colored node glyphs.
const RailColor = "#505050"

// RailStyle renders timeline spines and connectors independently from ThinkingStyle,
// allowing the rail to recede without also reducing the readability of reasoning text.
var RailStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(RailColor))

// NewMarkdownRenderer builds a glamour renderer for the given wrap width.
//
// It uses the static DarkStyleConfig deliberately — never glamour.WithAutoStyle().
// Auto style calls termenv.HasDarkBackground(), which writes an OSC-11 background
// query plus a CSI-6n cursor probe to the terminal and reads the replies back off
// stdin. Inside a Bubble Tea program — which owns stdin in raw mode — those replies
// race the input reader and (a) leak into the UI as stray bytes like "]11;rgb:…" and
// "[…;…R", (b) desync the renderer's cursor tracking, and (c) stall the render loop.
// The static config does no terminal I/O.
//
// The document's left margin is zeroed so narration aligns flush under the "●"
// bullet that package tui prepends (otherwise glamour indents every line by 2).
//
// The Nexus palette is applied to glamour's defaults: markdown headings (glamour's
// blue) and inline `code` spans (glamour's red) are recolored to MarkdownHeadingColor
// and MarkdownInlineCodeColor. The inline `code` span additionally drops glamour's
// dark background fill and its U+00A0 prefix/suffix (which padded each span with a
// leading/trailing space), so a `code` span renders as bare colored text. The H2–H6
// heading prefixes (glamour's literal "## ", "### ", … markers) are cleared so a
// heading renders as clean styled text rather than echoing its markdown hashes; H1
// keeps its colored background bar (it never carried a "#" marker).
//
// Code-block syntax highlighting uses the Nexus theme outright: cfg.CodeBlock.Chroma is
// pointed at the package-level nexusChroma rather than inheriting glamour's reds-laden
// DarkStyleConfig chroma and patching it token by token. See nexusChroma for the palette
// and the rationale (this is why box-drawing file trees no longer render on a red
// background, and why path slashes, arrows and +/- markers are not red).
//
// cfg is a value copy of DarkStyleConfig, so reassigning its (non-pointer) fields never
// mutates the shared package-level config (the same copy-then-override pattern as the
// document margin).
//
// Returns an error if glamour fails to construct (caller decides fallback).
func NewMarkdownRenderer(width int) (*glamour.TermRenderer, error) {
	cfg := glamourstyles.DarkStyleConfig
	noMargin := uint(0)
	cfg.Document.Margin = &noMargin

	heading := MarkdownHeadingColor
	cfg.Heading.Color = &heading
	cfg.H2.Prefix = "" // drop glamour's literal "## " heading marker
	cfg.H3.Prefix = "" // drop "### "
	cfg.H4.Prefix = "" // drop "#### "
	cfg.H5.Prefix = "" // drop "##### "
	cfg.H6.Prefix = "" // drop "###### "

	code := MarkdownInlineCodeColor
	cfg.Code.Color = &code
	cfg.Code.BackgroundColor = nil // no background fill
	cfg.Code.Prefix = ""           // no leading U+00A0 padding space
	cfg.Code.Suffix = ""           // no trailing U+00A0 padding space

	// Own the code-block syntax theme outright: point at the Nexus chroma palette rather
	// than inheriting glamour's DarkStyleConfig chroma and patching its reds token by
	// token. nexusChroma is read-only and never mutated, so sharing the pointer is safe.
	cfg.CodeBlock.Chroma = &nexusChroma

	return glamour.NewTermRenderer(
		glamour.WithStyles(cfg),
		glamour.WithWordWrap(width),
	)
}

// RenderMarkdown keeps the TUI's responsive table policy outside Glamour while
// preserving Glamour's exact output for documents whose tables remain readable.
func RenderMarkdown(r *glamour.TermRenderer, markdown string, width int) (string, error) {
	return renderMarkdownTables(r, markdown, width)
}
