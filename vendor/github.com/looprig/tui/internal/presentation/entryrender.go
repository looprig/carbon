package presentation

import (
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/looprig/tui/styles"
)

// interruptedTombstone is the content-less marker rendered for an interrupted turn.
const interruptedTombstone = "└─ interrupted"

// renderEntry turns one committed transcript entry into its scrollback lines,
// dispatching on the entry's kind. It is the per-entry renderer scrollbackModel.Flush
// is given: each kind reuses the render.go primitives (renderUser/renderAssistant/
// renderToolCalls/styles) so scrollback and the live tail render identically. expand
// is the single ctrl+t flag, threaded to the assistant kind's thinking and tool-card
// folds. An unknown kind returns no lines (fail-safe). Returned lines carry no
// trailing blank line — the caller (Flush) owns inter-entry spacing.
func renderEntry(e entry, expand bool, width int) []string {
	switch e.Kind {
	case kindUser:
		return splitNonEmpty(renderUser(renderInlineBlocks(e.Blocks), width))
	case kindAssistant:
		// A committed thinking block's header carries its measured streaming span
		// (formatThought(e.thinkDur)): "│ Thought for Ns", or the bare "│ Thought" when no
		// duration was captured (a cold restore has no streaming timestamps).
		return splitNonEmpty(renderAssistant(thinkingText(e.Blocks), assistantText(e.Blocks), expand, width, formatThought(e.thinkDur)))
	case kindTool:
		// A reconciled Subagent card (Agent set) renders as its OWN rail node opening a
		// compact secondary rail — its child tools stay hidden and a plain status line closes it,
		// never as an ordinary tool node.
		if len(e.Calls) == 1 && e.Calls[0].Agent != "" {
			return splitNonEmpty(renderSubagentCard(e.Calls[0], expand, width))
		}
		return splitNonEmpty(renderToolCalls(e.Calls, expand, width))
	case kindPromptRecord:
		return renderPromptRecord(e.Prompt, width)
	case kindNotice:
		return renderNotice(e.Level, firstText(e.Blocks), width)
	case kindInterrupted:
		return []string{styles.InterruptedStyle.Render(interruptedTombstone)}
	case kindSubagent:
		return renderSubagentLine(e.Agent, e.Verb, width)
	case kindHarness:
		return renderHarnessLine(firstText(e.Blocks), width)
	case kindWorkflowActivity:
		return renderWorkflowActivity(e.Workflow, width)
	default:
		return nil
	}
}

// renderWorkflowActivity renders one durable session notification as an independent
// blue-marker row. The marker is the only colored chrome; safe workflow metadata remains
// ordinary text, and the raw event/run IDs held by the reducer never reach the terminal.
func renderWorkflowActivity(a *workflowActivityEntry, width int) []string {
	if a == nil {
		return nil
	}
	marker := styles.WorkflowActivityMarker
	if a.Continuation {
		marker = styles.WorkflowActivityContinuationMarker
	}
	prefix := styles.WorkflowActivityStyle.Render(marker) + " "
	return []string{prefix + truncate(workflowActivityText(*a), width-2)}
}

// harnessMark is the hollow-circle glyph (plus its trailing space) prefixing a kindHarness
// status line — mirroring the idle status dot's ○ so a shell-emitted line reads as chrome,
// not model output.
const harnessMark = "○ "

// renderHarnessLine renders a shell-emitted status line (kindHarness) as one faint,
// out-of-focus "○ <text>" row, width-truncated. It mirrors the idle status dot's hollow
// circle and the status timer's faint tone (styles.StatusStyle) so a "turn ran for 25s" line
// reads as a quiet, out-of-band marker rather than assistant output. An empty text still
// renders the lone marker.
func renderHarnessLine(text string, width int) []string {
	return []string{styles.StatusStyle.Render(truncate(harnessMark+text, width))}
}

// renderSubagentLine renders a collapsed subagent activity entry as one faint
// "▸ <agent>: <verb>" row (design §6d Option B), width-truncated. agent is the resolved
// attribution label (the agent name, or the loopID short form via agentLabel — normally
// non-empty), so the row never reads "▸ : verb". An empty agent (defensive)
// still renders the cursor + verb rather than a dangling colon.
func renderSubagentLine(agent, verb string, width int) []string {
	label := agent
	if label != "" {
		label += ": "
	}
	line := styles.SubagentCursor + label + verb
	return []string{styles.SubagentStyle.Render(truncate(line, width))}
}

// renderNotice renders one leveled notice as "▌ "-bar lines colored per level (info
// neutral, warn yellow, error red — see styles.NoticeStyle): every width-wrapped line
// of text is prefixed with the level-colored accent bar, matching the user-message
// bar layout. An empty text still yields a single bar line so the notice marks the
// event. The bar and the text share the per-level style so the row reads as one
// coherent colored unit.
func renderNotice(level noticeLevel, text string, width int) []string {
	style := styles.NoticeStyle(uint8(level))
	out := barWrap(style, strings.Split(text, "\n"), width)
	if len(out) == 0 {
		out = append(out, style.Render(styles.AccentBarPrompt))
	}
	return out
}

// barWrap renders each raw line as a "▌ "-bar-prefixed, width-wrapped row in style:
// the accent bar and the wrapped text share one style so the rows read as a single
// coherent colored unit (the leveled-notice / info-notification layout). Every wrapped
// row keeps the bar, and the wrap budget reserves barWidth columns for it. It is the
// shared bar-rendering primitive reused by renderNotice and the scrollback prompt
// record so neither duplicates the bar layout.
func barWrap(style lipgloss.Style, rawLines []string, width int) []string {
	bar := style.Render(styles.AccentBarPrompt)
	var out []string
	for _, raw := range rawLines {
		for _, line := range wrapToWidth(raw, width-barWidth) {
			out = append(out, bar+style.Render(line))
		}
	}
	return out
}

// renderPromptRecord renders the FULL AskUser context committed to scrollback — the
// question + every numbered choice, NOT the compact bottom-box control (prompt.go
// renders that). It is rendered as one info notification: every line carries the
// neutral "▌ " info bar (styles.NoticeInfoStyle), matching the leveled-notice family.
// A nil context renders nothing (fail-safe). Permission gates never reach here — they
// surface as the "Approved …"/"Denied …" verb on their committed tool card, not a record.
func renderPromptRecord(p *promptContext, width int) []string {
	if p == nil {
		return nil
	}
	return renderUserInputRecord(*p, width)
}

// renderUserInputRecord renders an AskUser request's scrollback record as one info
// notification: the "▌ "-barred Question followed by every offered choice as a
// "▌ "-barred numbered line (promptChoiceLines). Every line carries the neutral info
// bar (styles.NoticeInfoStyle) so the request reads as one information notice. A
// free-text request (no choices) records just the question. A SUBAGENT-attributed
// record (p.Agent set) prefixes the question with "<agent>: " so the user sees WHICH
// agent is asking; a loop-local record (p.Agent empty) renders the question as-is.
func renderUserInputRecord(p promptContext, width int) []string {
	question := p.Question
	if p.Agent != "" {
		question = p.Agent + ": " + question
	}
	lines := append([]string{question}, promptChoiceLines(p.Choices)...)
	return barWrap(styles.NoticeInfoStyle, lines, width)
}

// choiceIndent is the 2-space lead applied to every recorded-choice line so a
// wrapped "N. text" choice's continuation rows align under the "N. " number.
const choiceIndent = "  "

// promptChoiceLines builds each choice as a "  N. text" numbered raw line (1-based),
// the choiceIndent leading every line so a barWrap-wrapped continuation row aligns
// under the number. Width-wrapping and the info bar are applied by barWrap (which the
// caller runs these through) — this only lays out the numbered text. Returns nil for
// an empty choice list (a free-text question records no choices).
func promptChoiceLines(choices []string) []string {
	out := make([]string, 0, len(choices))
	for i, c := range choices {
		out = append(out, choiceIndent+strconv.Itoa(i+1)+". "+c)
	}
	return out
}

// splitNonEmpty splits a rendered block on newlines into scrollback lines,
// returning nil for an empty render so an empty kind contributes no lines.
func splitNonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
