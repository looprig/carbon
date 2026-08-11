package view

import (
	"strings"

	"charm.land/lipgloss/v2"
)

// WrapToWidth word-wraps s to width columns and trims trailing layout padding.
func WrapToWidth(s string, width int) []string {
	if width <= 0 {
		return []string{s}
	}
	wrapped := lipgloss.NewStyle().Width(width).Render(s)
	rows := strings.Split(wrapped, "\n")
	for i := range rows {
		rows[i] = strings.TrimRight(rows[i], " ")
	}
	return rows
}
