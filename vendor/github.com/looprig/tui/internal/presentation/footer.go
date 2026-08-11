package presentation

import (
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/looprig/core/uuid"
	"github.com/looprig/tui/styles"
)

type footerHit struct {
	id         uuid.UUID
	row, start int
	end        int
}

// loopFooter wraps only between complete segments and derives hit geometry from the same
// layout pass used to render.
type loopFooter struct {
	header string
	bar    loopBar
}

func (f loopFooter) View(width int) string {
	lines, _ := f.layout(width)
	return strings.Join(lines, "\n")
}

func (f loopFooter) HitTest(x, row, width int) (uuid.UUID, bool) {
	_, hits := f.layout(width)
	for _, hit := range hits {
		if hit.row == row && x >= hit.start && x < hit.end {
			return hit.id, true
		}
	}
	return uuid.UUID{}, false
}

func (f loopFooter) layout(width int) ([]string, []footerHit) {
	if width <= 0 {
		return nil, nil
	}
	lines, widths := []string{""}, []int{0}
	if f.header != "" {
		header := ansi.Truncate(styles.StatusStyle.Render(f.header), width, "")
		lines[0], widths[0] = header, lipgloss.Width(header)
	}
	kept := f.bar.keptByPriority()
	if f.header != "" && len(f.bar.entries) > 0 {
		lines, widths = append(lines, ""), append(widths, 0)
	}
	hits := make([]footerHit, 0, len(kept))
	for _, entry := range kept {
		entryWidth := lipgloss.Width(f.bar.segPlain(entry))
		row := len(lines) - 1
		sep := ""
		if widths[row] > 0 {
			sep = barSep
		}
		if widths[row] > 0 && widths[row]+lipgloss.Width(sep)+entryWidth > width {
			lines, widths = append(lines, ""), append(widths, 0)
			row, sep = row+1, ""
		}
		start := widths[row] + lipgloss.Width(sep)
		lines[row] += sep + f.bar.segStyled(entry)
		widths[row] = start + entryWidth
		hits = append(hits, footerHit{id: entry.id, row: row, start: start, end: widths[row]})
	}
	if hidden := len(f.bar.entries) - len(kept); hidden > 0 {
		row := len(lines) - 1
		text, sep := styles.StatusStyle.Render(overflowText(hidden)), ""
		if widths[row] > 0 {
			sep = barSep
		}
		if widths[row] > 0 && widths[row]+lipgloss.Width(sep)+lipgloss.Width(text) > width {
			lines = append(lines, text)
		} else {
			lines[row] += sep + text
		}
	}
	if len(lines) == 1 && lines[0] == "" {
		return nil, nil
	}
	return lines, hits
}
