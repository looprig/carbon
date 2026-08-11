package presentation

import (
	"charm.land/lipgloss/v2"

	"github.com/looprig/tui/internal/view"
)

func wrapToWidth(s string, width int) []string { return view.WrapToWidth(s, width) }
func railNodeStyled(glyph, text string, style lipgloss.Style, depth, width int) []string {
	return view.RailNodeStyled(glyph, text, style, depth, width)
}
func railDetail(text string, depth, width int) []string {
	return view.RailDetail(text, depth, width)
}
func railSpine(depth int) string     { return view.RailSpine(depth) }
func railConnector(depth int) string { return view.RailConnector(depth) }
