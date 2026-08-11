package presentation

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/tui/styles"
)

// previewLineCap is the HARD cap on result-preview lines a tool card shows: a result with
// more lines is trimmed to the first previewLineCap lines plus a "… N more lines" marker.
// The cap applies ALWAYS — regardless of the ctrl+t expand fold (which now governs only the
// thinking block). A huge tool result (e.g. a long diff) otherwise fills the live tail,
// scrolling the assistant bullet off the top AND, on commit, stranding a screen-height
// scrollback gap (the inline renderer's insertAbove math is sized off the tall live tail).
const previewLineCap = 3

// liveCallCap bounds how many of a step's tool cards the LIVE tail shows: only the most
// recent liveCallCap, prefixed with a "… N earlier calls" marker. A step that fires many
// tools would otherwise grow the live tail (the bubbletea managed region) to fill the whole
// screen — scrolling the assistant bullet off the top and forcing a full-screen repaint each
// frame. The elided cards still commit IN FULL to scrollback at the step's StepDone.
const liveCallCap = 3

// noOutput is the placeholder shown for a completed tool call with no result lines.
const noOutput = "(no output)"

// hintSeparator joins the fields of the subagent done line ("<verb> · N steps"). It is
// " · " (a U+00B7 middle dot framed by single spaces), kept in one place so the hint
// stays consistent.
const hintSeparator = " · "

// cardConnector is the RETIRED "⎿ " tree connector of the old indented tool-card
// layout. The unified rail renders tool calls as rail nodes ("○"/"◍" beside a "│ "
// detail rail — see renderToolNode), never this connector. It survives ONLY as the
// sentinel a render test asserts is ABSENT from rail output.
const cardConnector = "⎿ "

// dotWidth is the display width of the assistant bullet prefix ("● "), which also
// matches glamour's "dark" document left margin and the shared "│ " rail width.
// Narration wraps to this much less than the content width so continuation lines —
// railed to align under the first line — still fit.
const dotWidth = 2

// renderMD renders markdown to ANSI behind the static committed bullet (styles.LitDot,
// the DotColor-foregrounded "●"). It is the committed/scrollback path: a frozen
// assistant "●" never animates, so it always uses the lit dot. The live tail uses
// renderMDDot with a blink-phased bullet.
func renderMD(md string, width int) string {
	return renderMDDot(md, width, styles.LitDot)
}

// renderMDDot renders markdown to ANSI and prefixes it with dot so the narration
// begins on the SAME line as the bullet. The shared renderer has a zero-column
// document margin; its surrounding blank lines are stripped so the text aligns with
// the dot — first line "<dot>text", continuation
// lines use the shared rail in the dot's column. On a glamour construction or render error it
// falls back to the raw text behind the dot, so the UI always gets readable output.
// dot MUST be dotWidth (2) columns wide so continuation-line alignment holds; callers
// pass either the static styles.Dot (committed) or a blink-phased live bullet.
func renderMDDot(md string, width int, dot string) string {
	if strings.TrimSpace(md) == "" {
		return ""
	}

	renderWidth := max(0, width-dotWidth)
	r, err := styles.NewMarkdownRenderer(renderWidth)
	if err != nil {
		return dot + md
	}
	out, err := styles.RenderMarkdown(r, md, renderWidth)
	if err != nil {
		return dot + md
	}

	lines := dedentDocument(out)
	continuationRail := railSpine(1)
	for i := range lines {
		if i == 0 {
			lines[i] = dot + lines[i]
		} else {
			lines[i] = continuationRail + lines[i]
		}
	}
	return strings.Join(lines, "\n")
}

// dedentDocument strips Glamour's trailing fill and surrounding blank lines. The
// renderer's document margin is explicitly zero, so leading spaces are semantic
// Markdown layout (record indentation, continuation alignment, code, and lists) and
// must never be removed independently from individual lines. It returns at least one
// line. The historical name remains because this is the shared document-framing seam.
func dedentDocument(s string) []string {
	raw := strings.Split(s, "\n")
	out := make([]string, 0, len(raw))
	for _, ln := range raw {
		out = append(out, strings.TrimRight(ln, " "))
	}
	for len(out) > 0 && out[0] == "" {
		out = out[1:]
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	if len(out) == 0 {
		return []string{""}
	}
	return out
}

// renderToolCalls renders a segment's tool-call children as rail nodes on the committed
// (scrollback) path: each call becomes a status-tinted "○"/"◍" node beside its faint
// header, then its result preview as "│ "-railed detail rows (renderToolNode at depth 0).
// The preview is HARD-capped to previewLineCap lines plus a "… N more lines" marker
// regardless of expandTools (the ctrl+t fold governs only the thinking block now), so a
// huge result can never fill the live tail. An empty result renders "(no output)". An
// error node's result is capped the same way, never hidden. Detail rows are width-wrapped
// so a long node never blows the viewport. Committed nodes always render their full body
// (liveRunning=false). Returns "" when there are no calls.
func renderToolCalls(calls []ToolCallView, expandTools bool, width int) string {
	if len(calls) == 0 {
		return ""
	}
	parts := make([]string, 0, len(calls))
	for i := range calls {
		parts = append(parts, strings.Join(renderToolNode(calls[i], 0, expandTools, width, false), "\n"))
	}
	return strings.Join(parts, "\n")
}

// toolNodeStatus maps a tool's lifecycle status to its rail-node tint: error and
// cancelled are the failed (red) node, running is the pulsing node, and OK (or any
// unrecognised status) is the lime hollow node (fail-visible, never a panic).
func toolNodeStatus(s ToolStatus) styles.NodeStatus {
	switch s {
	case ToolError, ToolCancelled:
		return styles.NodeFailed
	case ToolRunning:
		return styles.NodeRunning
	default: // ToolOK and any unknown value
		return styles.NodeOK
	}
}

// renderToolNode renders one tool call as a rail node at depth: a status-tinted
// "○" (ok/failed) / "◍" (running) node glyph beside its faint header, then its result
// preview as "│ "-railed detail rows (the SAME hard-cap fold as before). A still-RUNNING
// card on the LIVE path (liveRunning) renders header-only — its body appears once the
// card commits to scrollback (the live→committed handoff fix, design Option B: a running
// card has no result yet, so dropping its "(no output)" body keeps the live tail one line
// tall and the running→completed transition reads as a clean continuation, not a
// multi-line shrink). It applies ONLY to ToolRunning cards on the live path; resolved
// cards (live or committed) and the committed path always render their full body. depth
// nests the node under a parent spine — Task 6 reuses this at depth 1 for subagent
// children.
func renderToolNode(c ToolCallView, depth int, expandTools bool, width int, liveRunning bool) []string {
	return renderToolNodeGlyph(c, depth, expandTools, width, liveRunning, styles.ToolNode(toolNodeStatus(c.Status)))
}

// renderToolNodeGlyph renders a tool call as a rail node using an explicit pre-styled node
// glyph — the seam that lets the LIVE path animate a running node's pulse (liveRunningNode)
// while the committed path uses the static status glyph (styles.ToolNode). Behaviour is
// otherwise identical to renderToolNode: a still-RUNNING card on the LIVE path (liveRunning)
// renders header-only; every resolved card and the committed path render the full body.
func renderToolNodeGlyph(c ToolCallView, depth int, expandTools bool, width int, liveRunning bool, glyph string) []string {
	header := railNodeStyled(glyph, toolNodeHeaderText(c), styles.ToolCallStyle, depth, width)
	if liveRunning && c.Status == ToolRunning {
		return header // compact running indicator; body appears once, on commit
	}
	out := header
	for _, rl := range previewLines(c.Result, expandTools) {
		out = append(out, railDetail(rl, depth, width)...)
	}
	return out
}

// toolNodeHeaderText assembles a tool node's header text "<verb >ToolName(detail)" — the
// permission-decision verb ("Approved "/"Denied ") when the call prompted (empty for an
// ungated/pre-approved call) and the args detail in parens. The parens are omitted when
// there is no detail. It carries NO status glyph: the rail node glyph conveys status.
func toolNodeHeaderText(c ToolCallView) string {
	head := c.ToolName
	if detail := toolCallDetail(c); detail != "" {
		head = c.ToolName + "(" + detail + ")"
	}
	if v := decisionVerb(c.Decision); v != "" {
		head = v + " " + head
	}
	return head
}

// toolCallDetail returns the argument/target text to render inside ToolName(...).
// A permission prompt body is preferred because it is what the user approved.
// Otherwise the redacted audit summary is normalized to avoid duplicating the
// tool name inside the header.
func toolCallDetail(c ToolCallView) string {
	if c.Permission != "" {
		return c.Permission
	}
	return stripToolNamePrefix(c.ToolName, c.Summary)
}

// stripToolNamePrefix trims summaries such as "Bash: make test" and
// "ReadFile main.go" down to their argument text. The card header already renders
// the tool name, so repeating it obscures the useful detail.
func stripToolNamePrefix(toolName, summary string) string {
	s := strings.TrimSpace(summary)
	if s == "" {
		return ""
	}
	if s == toolName {
		return ""
	}
	if rest, ok := strings.CutPrefix(s, toolName+":"); ok {
		return strings.TrimSpace(rest)
	}
	if rest, ok := strings.CutPrefix(s, toolName+" "); ok {
		return strings.TrimSpace(rest)
	}
	return s
}

// decisionVerb maps a permission decision to its card-header verb. A call that never
// prompted (gateNone) — ungated or pre-approved by an existing grant — gets no verb;
// a still-pending gate likewise shows none (it resolves before the card commits).
func decisionVerb(d gateDecision) string {
	switch d {
	case gateApproved:
		return "Approved"
	case gateDenied:
		return "Denied"
	default:
		return ""
	}
}

// previewLines selects the result lines to display for a card. An empty result
// yields the single "(no output)" placeholder. When collapsed and the result has
// more than previewLineCap lines, it returns the first previewLineCap lines plus a
// "… N more lines · ctrl+t" marker (N = the remainder). When expanded, every line
// shows (the runner already capped the preview — no extra TUI cap).
func previewLines(result []string, _ bool) []string {
	// The expand flag is intentionally IGNORED for tool results: the preview is HARD-capped
	// to previewLineCap lines regardless of ctrl+t, so a huge result can never fill the live
	// tail (hiding the assistant bullet) or strand a commit-time scrollback gap. ctrl+t still
	// folds the thinking block. (The bool param is retained for call-site compatibility.)
	if len(result) == 0 {
		return []string{noOutput}
	}
	if len(result) <= previewLineCap {
		return result
	}
	remaining := len(result) - previewLineCap
	shown := make([]string, 0, previewLineCap+1)
	shown = append(shown, result[:previewLineCap]...)
	shown = append(shown, "… "+strconv.Itoa(remaining)+" more lines")
	return shown
}

// renderAssistant renders a committed assistant segment under the node-presence rule:
// the thinking rail first (when the segment has reasoning), then the neon "●" AI-message
// node — via renderMD, which prefixes styles.LitDot — ONLY when the narration text is
// non-empty. Empty text renders no "●" node and no umbrella: a thinking-only segment is
// just its rail, and a pure-tool step commits no assistant entry at all (its tool calls
// stand alone as their own kindTool rail nodes). Committed tool cards are their OWN
// kindTool entries, so this never renders cards inline. expand drives the thinking
// block's compact/full fold. thinkHeader is the reasoning block's header label — the
// committed caller (renderEntry) passes formatThought(duration) so the rail reads
// "│ thought for Nsec" / "│ thought"; the live-spill caller passes styles.ThinkingHeader.
func renderAssistant(thinking, text string, expand bool, width int, thinkHeader string) string {
	var b strings.Builder
	if t := renderThinking(thinking, expand, width, thinkHeader); t != "" {
		b.WriteString(t)
	}
	if text != "" {
		body := renderMD(text, width)
		if b.Len() > 0 {
			b.WriteString("\n") // AI message follows its thinking block directly, no blank line
		}
		b.WriteString(body)
	}
	return b.String()
}

// subagentTerminalVerb maps a child loop's terminal status to its done-line verb
// (design §4): subDone→"done", subFailed→"failed", subInterrupted→"interrupted". An
// outstanding child (subRunning, the zero value) reads "running" — a defensive label
// for a card committed before its terminal (a card normally commits only after the
// child has handed back, so this is the rare in-flight case).
func subagentTerminalVerb(s subStatus) string {
	switch s {
	case subDone:
		return "done"
	case subFailed:
		return "failed"
	case subInterrupted:
		return "interrupted"
	default:
		return "running"
	}
}

// subagentNodeStatus maps a child loop's terminal status to its rail-node tint.
func subagentNodeStatus(s subStatus) styles.NodeStatus {
	switch s {
	case subFailed, subInterrupted:
		return styles.NodeFailed
	case subRunning:
		return styles.NodeRunning
	default: // subDone
		return styles.NodeOK
	}
}

// renderSubagentCard renders a reconciled Subagent as one "○" header node
// "Subagent(agent) "task"" (tinted by the child's terminal status) followed by a plain
// "│ verb · N steps — "summary"" status line. The inner status deliberately has no second
// hollow-circle node. The subagent's OWN nested tool calls are deliberately NOT shown — the
// card reports the subagent and its outcome, not the individual tools it ran. The second bool
// (formerly the child-fold expand) is unused now that children are elided.
func renderSubagentCard(c ToolCallView, _ bool, width int) string {
	status := subagentNodeStatus(c.SubStatus)
	lines := railNodeStyled(styles.ToolNode(status), subagentHeaderText(c), styles.ToolCallStyle, 0, width)
	lines = append(lines, railDetail(subagentDoneText(c), 0, width)...)
	return strings.Join(lines, "\n")
}

// subagentHeaderText assembles a Subagent card's header body: the standard tool-card
// form "Subagent(<agent>)" (tool name + the agent as its argument) plus the task in
// quotes when present. The task quotes are omitted for an empty task.
func subagentHeaderText(c ToolCallView) string {
	head := c.ToolName + "(" + c.Agent + ")"
	if c.Task != "" {
		head += `  "` + c.Task + `"`
	}
	return head
}

// subagentDoneText is the subagent's terminal summary "verb · N steps[ — "summary"]".
// The summary (the hand-back text, this being the ONLY place it appears) is omitted for
// an interrupted child.
func subagentDoneText(c ToolCallView) string {
	line := subagentTerminalVerb(c.SubStatus) + hintSeparator + plural(c.Steps, "step")
	if summary := strings.Join(c.Result, " "); summary != "" && c.SubStatus != subInterrupted {
		line += " — " + `"` + summary + `"`
	}
	return line
}

// plural renders a count with grammatical agreement on unit: "1 <unit>" (singular) for
// n == 1, "N <unit>s" (plural) otherwise. Used by the Subagent done line ("step") and
// the collapsed thinking summary ("line").
func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return strconv.Itoa(n) + " " + unit + "s"
}

// nonSubagentCalls returns the calls that are not raw agent-spawn tool cards. The LIVE
// tail suppresses those because the child accumulator already renders the richer pending
// agent card; rendering both would double the call. Both the legacy Subagent name and the
// current StartAgent name are accepted during the migration. At most replacements calls
// are removed, so parallel starts without child accumulators remain visible. The result is
// a fresh slice; the input is not mutated.
func nonSubagentCalls(calls []ToolCallView, replacements int) []ToolCallView {
	var out []ToolCallView
	for _, c := range calls {
		if replacements > 0 && isAgentSpawnTool(c.ToolName) {
			replacements--
			continue
		}
		out = append(out, c)
	}
	return out
}

// renderLiveAssistant renders the in-progress (live) assistant segment on the unified rail,
// with the animation state threaded in: the leading AI-message bullet blinks (liveDot) and a
// still-running tool node pulses (liveRunningNode's "◍"), while resolved nodes keep their
// static "○"/tinted glyph. It mirrors renderAssistant's ordering (thinking → narration →
// tool nodes) but is the LIVE path ONLY — the committed renderAssistant stays static and is
// never given an anim. Empty parts are omitted, and the live tail is always expanded.
//
// The AI-message "●" bullet renders under the node-presence rule — ONLY when there is
// narration text; a tools-only live step shows its tool nodes directly with no working-word
// umbrella. calls is the NON-Subagent live tool list (the caller filters the raw Subagent
// call out via nonSubagentCalls), capped to the most recent liveCallCap with a rail
// "… N earlier calls" detail row for the remainder; subagentCards are the in-flight nested
// Subagent cards (pendingSubagentCards), each rendered as its own "●"-level card
// (renderSubagentCard) below the ordinary calls, separated by a blank line.
func renderLiveAssistant(thinking, text string, calls, subagentCards []ToolCallView, expand bool, width int, a animState) string {
	var b strings.Builder

	// The LIVE tail's reasoning header is the present-tense "thinking" (styles.ThinkingHeader):
	// the step has not committed, so no duration is known yet. It flips to "thought for Nsec"
	// once StepDone commits the step (renderEntry passes formatThought there).
	if t := renderThinking(thinking, expand, width, styles.ThinkingHeader); t != "" {
		b.WriteString(t)
	}

	// AI-message node: the blinking "●" bullet, rendered ONLY when there is narration text
	// (node presence — no working-word umbrella; a tools-only live step shows its tool
	// nodes directly, not a bullet).
	if text != "" {
		body := renderMDDot(text, width, liveDot(a.blink))
		if b.Len() > 0 {
			b.WriteString("\n") // the message follows its thinking block directly, no blank line
		}
		b.WriteString(body)
	}

	if len(calls) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		// A "│" connector row joins the "●" AI-message body to the first tool node so the
		// live rail is continuous (matching the committed path). It fires ONLY after a "●"
		// text body: a thinking block already ends in its own trailing "│ " rail line, and a
		// tools-only step's first node opens the rail itself, so neither needs a leading
		// connector (one would double the rail).
		if text != "" {
			b.WriteString(railConnector(0) + "\n")
		}
		// Cap the live tail to the most recent liveCallCap tool nodes (keeping the running
		// one), prefixed with a "… N earlier calls" rail detail row, so a many-tool step
		// can't grow the managed region to fill the screen and drop the assistant bullet.
		// The full set commits to scrollback at StepDone.
		shown := calls
		if hidden := len(calls) - liveCallCap; hidden > 0 {
			shown = calls[hidden:]
			b.WriteString(strings.Join(railDetail("… "+strconv.Itoa(hidden)+" earlier calls", 0, width), "\n") + "\n")
		}
		// Each live call renders as a rail node; a still-RUNNING call uses the pulsing "◍"
		// glyph (liveRunningNode) and is header-only (liveRunning=true) so the live→committed
		// handoff is a one-line→full-card continuation, not a multi-line live shrink.
		nodes := make([]string, 0, len(shown))
		for i := range shown {
			glyph := styles.ToolNode(toolNodeStatus(shown[i].Status))
			if shown[i].Status == ToolRunning {
				glyph = liveRunningNode(a.blink)
			}
			nodes = append(nodes, strings.Join(renderToolNodeGlyph(shown[i], 0, expand, width, true, glyph), "\n"))
		}
		// Consecutive tool nodes are separated by a "│" connector row — the committed path's
		// continuous rail between nodes.
		b.WriteString(strings.Join(nodes, "\n"+railConnector(0)+"\n"))
	}

	// Each pending subagent card is its OWN "●"-level card (like the committed form),
	// separated by a blank line from whatever precedes it.
	for i := range subagentCards {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(renderSubagentCard(subagentCards[i], expand, width))
	}
	return b.String()
}

// barWidth is the display columns a left-bar prefix ("▌ " / "│ ") consumes.
const barWidth = 2

// renderUser renders a committed user message as MARKDOWN behind the gray "▌ " rail:
// the text goes through the same glamour renderer as assistant narration (so a fenced
// file attachment shows as a code block, and lists/headings/inline-code/links render),
// then every output line is prefixed with the bar so the message reads as the user's
// with its left-accent identity. This is display-only — the literal text is what was
// sent to the model. On a glamour construction/render error it falls back to the raw,
// width-wrapped text behind the bar (bad markdown shows as-is).
func renderUser(text string, width int) string {
	return renderMDRail(text, width, styles.AccentBarStyle.Render(styles.AccentBarPrompt))
}

// renderMDRail renders md to ANSI (glamour) and prefixes EVERY line with bar — a left
// rail down the whole block. It strips Glamour's blank document framing while keeping
// semantic leading indentation, then places the content behind the bar. It falls back
// to raw, width-wrapped text on any Glamour error.
func renderMDRail(md string, width int, bar string) string {
	if strings.TrimSpace(md) == "" {
		return ""
	}
	raw := func() string {
		var out []string
		for _, para := range strings.Split(md, "\n") {
			for _, line := range wrapToWidth(para, width-barWidth) {
				out = append(out, bar+line)
			}
		}
		return strings.Join(out, "\n")
	}
	renderWidth := max(0, width-barWidth)
	r, err := styles.NewMarkdownRenderer(renderWidth)
	if err != nil {
		return raw()
	}
	out, err := styles.RenderMarkdown(r, md, renderWidth)
	if err != nil {
		return raw()
	}
	lines := dedentDocument(out)
	for i := range lines {
		lines[i] = bar + lines[i]
	}
	return strings.Join(lines, "\n")
}

// firstLine returns the first "\n"-delimited line of s (s itself when single-line).
// The queued affordance previews only the first line of a multi-line submission —
// it is a compact pending hint, not the full committed row.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// thinkingRail is the plain left-rail margin used when matching clickable transcript
// labels. renderThinking produces the same visible prefix with the dedicated RailStyle.
const thinkingRail = "│ "

// renderThinking renders the model's reasoning under the unified ctrl+t expand flag,
// behind a caller-supplied header label so the SAME renderer serves both the live tail
// and committed scrollback: the LIVE tail passes the present-tense "thinking"
// (styles.ThinkingHeader); a COMMITTED entry passes formatThought(duration) — "thought
// for Ns" once its span is known, or the bare "thought" fallback when it is not
// (a cold restore replays no timing). Both modes render on the "│ " rail and BOTH carry a
// blank "│" rail gap row directly under the header (it stays put whether collapsed or
// expanded): COLLAPSED is the "│ <header>" line then the "│" gap (no reasoning body);
// EXPANDED is that same header + gap, then the "│ "-prefixed, width-wrapped reasoning, then a
// TRAILING "│" gap that sets the body off from the AI message below — an unbroken vertical
// rail down the left margin, gapped top and bottom. Empty/whitespace-only reasoning renders
// nothing in either mode.
func renderThinking(s string, expand bool, width int, header string) string {
	s = strings.TrimSpace(s) // drop the model's leading/trailing blank reasoning lines
	if s == "" {
		return ""
	}
	// The header, then a blank "│" rail gap row that pads it off whatever follows. The gap is
	// present in BOTH forms — it stays put whether the block is collapsed or expanded — so
	// "thought for Ns" always reads with one line of breathing space beneath it (before the
	// AI message when collapsed, before the reasoning body when expanded). The "│ " rail stays
	// unbroken (like an empty reasoning line), never a bare gap.
	out := []string{
		railSpine(1) + styles.ThinkingStyle.Render(header),
		railSpine(1),
	}
	if !expand {
		// COLLAPSED: header + the rail gap, no reasoning body. No arrow, no glyph, no
		// "· N lines · ctrl+t" summary.
		return strings.Join(out, "\n")
	}
	for _, raw := range strings.Split(s, "\n") {
		for _, line := range wrapToWidth(raw, width-barWidth) {
			out = append(out, railSpine(1)+styles.ThinkingStyle.Render(line))
		}
	}
	// A trailing rail gap closes the expanded block: it sets the reasoning body off from the
	// AI message that follows, keeping the "│" rail (a bare blank line would break it).
	out = append(out, railSpine(1))
	return strings.Join(out, "\n")
}

// formatThought renders a committed thinking block's header from its measured span (the
// modern shell stamps the model clock onto each Ephemeral thinking chunk — see
// Screen.handleEventModern): a zero duration (a cold restore / backlog carries NO timing)
// yields the bare lowercase "thought"; any measured span under a second is floored to
// "thought for 1s" (thinking that ran is never shown as timeless — thinkDuration reports the
// positive measuredFloor sentinel for a same-tick span); under a minute "thought for Ns"
// (whole seconds, truncated); a minute or more "thought for Nm Ns". It is the committed
// counterpart of the live tail's present-tense "thinking" header — the same rail, flipped
// from present to past once the step commits.
func formatThought(d time.Duration) string {
	if d <= 0 {
		return "thought"
	}
	if d < time.Second {
		return "thought for 1s"
	}
	secs := int(d / time.Second)
	if secs < 60 {
		return "thought for " + strconv.Itoa(secs) + "s"
	}
	return "thought for " + strconv.Itoa(secs/60) + "m " + strconv.Itoa(secs%60) + "s"
}

// renderInlineBlocks renders each block to plain text and joins with newlines.
// Used for user rows where blocks are shown verbatim (no markdown).
func renderInlineBlocks(blocks []content.Block) string {
	parts := make([]string, 0, len(blocks))
	for _, blk := range blocks {
		parts = append(parts, renderBlock(blk))
	}
	return strings.Join(parts, "\n")
}

// assistantText concatenates the narration of an assistant segment for markdown
// rendering: every block except ThinkingBlock (rendered separately as the dim
// thinking block by renderThinking, so it must not be markdown-rendered here too).
func assistantText(blocks []content.Block) string {
	parts := make([]string, 0, len(blocks))
	for _, blk := range blocks {
		if _, ok := blk.(*content.ThinkingBlock); ok {
			continue
		}
		parts = append(parts, renderBlock(blk))
	}
	return strings.Join(parts, "\n")
}

// thinkingText concatenates the reasoning of every ThinkingBlock in blocks, the
// source for an assistant row's dim thinking block.
func thinkingText(blocks []content.Block) string {
	var b strings.Builder
	for _, blk := range blocks {
		if tb, ok := blk.(*content.ThinkingBlock); ok {
			b.WriteString(tb.Thinking)
		}
	}
	return b.String()
}

// firstText returns the text of the first TextBlock, or "" if there is none.
// Used by single-block roles (the leveled notice).
func firstText(blocks []content.Block) string {
	for _, blk := range blocks {
		if tb, ok := blk.(*content.TextBlock); ok {
			return tb.Text
		}
	}
	return ""
}

// renderBlock renders one block to its display string via a type switch over the
// sealed Block interface. Unknown types fall through to a safe placeholder.
func renderBlock(blk content.Block) string {
	switch b := blk.(type) {
	case *content.TextBlock:
		return b.Text
	case *content.ThinkingBlock:
		return b.Thinking
	case *content.ImageBlock:
		return fmt.Sprintf("[image: %s, %d bytes]", string(b.MediaType), len(b.Source.Data))
	default:
		return "[unsupported block]"
	}
}
