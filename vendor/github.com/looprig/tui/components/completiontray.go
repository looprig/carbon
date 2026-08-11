package components

import (
	"image/color"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/looprig/tui/styles"
)

type completionTrayRow struct {
	primary   string
	secondary string
}

func completionTrayNaturalWidth(rows []completionTrayRow) int {
	width := 0
	for _, row := range rows {
		line := styles.AccentBar + " " + row.primary
		if row.secondary != "" {
			line += "  " + row.secondary
		}
		width = max(width, ansi.StringWidth(line))
	}
	return width
}

func renderCompletionTray(rows []completionTrayRow, selected, width int) string {
	return renderCompletionTrayBackground(rows, selected, width, styles.TraySelectedBg)
}

func renderCompletionTrayBackground(rows []completionTrayRow, selected, width int, selectedBg color.Color) string {
	if width <= 0 || len(rows) == 0 {
		return ""
	}

	panelOpen, panelReset := styles.DeriveBackgroundSGR(styles.PanelBg)
	selectedOpen, selectedReset := styles.DeriveBackgroundSGR(selectedBg)
	rendered := make([]string, len(rows))
	for i, row := range rows {
		rail := styles.AccentBarStyle.Render(styles.AccentBar)
		primary := row.primary
		open, reset := panelOpen, panelReset
		if i == selected {
			rail = styles.CardRailStyle.Render(styles.AccentBar)
			primary = styles.CardSelectedStyle.Background(selectedBg).Render(primary)
			open, reset = selectedOpen, selectedReset
		}

		line := rail + " " + primary
		if row.secondary != "" {
			line += "  " + styles.CardHintStyle.Render(row.secondary)
		}
		line = ansi.Truncate(line, width, "")
		rendered[i] = styles.FillLineBackgroundWith(line, width, open, reset)
	}
	return strings.Join(rendered, "\n")
}

func renderCompletionTrayWindowBackgroundAt(rows []completionTrayRow, selected, width, maxRows, start int, selectedBg color.Color) string {
	if maxRows <= 0 || len(rows) == 0 {
		return ""
	}
	if maxRows >= len(rows) {
		return renderCompletionTrayBackground(rows, selected, width, selectedBg)
	}
	start = max(0, min(start, len(rows)-maxRows))
	return renderCompletionTrayBackground(rows[start:start+maxRows], selected-start, width, selectedBg)
}

func completionTrayWindowStart(rowCount, selected, maxRows int) int {
	if rowCount <= 0 || maxRows <= 0 || maxRows >= rowCount {
		return 0
	}
	selected = max(0, min(selected, rowCount-1))
	start := selected - maxRows + 1
	if start < 0 {
		start = 0
	}
	if start+maxRows > rowCount {
		start = rowCount - maxRows
	}
	return start
}
