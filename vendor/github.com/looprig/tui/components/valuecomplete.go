package components

import (
	"image/color"
	"strings"

	"github.com/looprig/tui/styles"
)

// ValueItem is one typed runtime choice. ID is the opaque payload returned on selection.
type ValueItem struct {
	ID          string
	Label       string
	Description string
	Aliases     []string
}

// ValueComplete is a filtered runtime-choice tray with a record cursor.
type ValueComplete struct {
	items  []ValueItem
	cursor int
}

func NewValueComplete(items []ValueItem, query string) *ValueComplete {
	q := strings.ToLower(strings.TrimSpace(query))
	filtered := make([]ValueItem, 0, len(items))
	for _, item := range items {
		if q == "" || valueMatches(item, q) {
			item.Aliases = append([]string(nil), item.Aliases...)
			filtered = append(filtered, item)
		}
	}
	if len(filtered) == 0 {
		return nil
	}
	return &ValueComplete{items: filtered}
}

func valueMatches(item ValueItem, query string) bool {
	if strings.Contains(strings.ToLower(item.Label), query) || strings.Contains(strings.ToLower(item.Description), query) {
		return true
	}
	for _, alias := range item.Aliases {
		if strings.Contains(strings.ToLower(alias), query) {
			return true
		}
	}
	return false
}

func (v *ValueComplete) Selected() ValueItem { return v.items[v.cursor] }
func (v *ValueComplete) Cursor() int         { return v.cursor }
func (v *ValueComplete) Len() int            { return len(v.items) }
func (v *ValueComplete) Up()                 { v.cursor = (v.cursor - 1 + len(v.items)) % len(v.items) }
func (v *ValueComplete) Down()               { v.cursor = (v.cursor + 1) % len(v.items) }

func (v *ValueComplete) SelectWindowRow(row, maxRows int) bool {
	visible := min(len(v.items), maxRows)
	if row < 0 || row >= visible {
		return false
	}
	start := completionTrayWindowStart(len(v.items), v.cursor, maxRows)
	next := start + row
	if next == v.cursor {
		return false
	}
	v.cursor = next
	return true
}

func (v *ValueComplete) ViewWindowBackground(width, maxRows int, selectedBg color.Color) string {
	rows := make([]completionTrayRow, len(v.items))
	for i, item := range v.items {
		rows[i] = completionTrayRow{primary: item.Label, secondary: item.Description}
	}
	return renderCompletionTrayWindowBackgroundAt(rows, v.cursor, width, maxRows, completionTrayWindowStart(len(rows), v.cursor, maxRows), selectedBg)
}

func (v *ValueComplete) ViewWindow(width, maxRows int) string {
	return v.ViewWindowBackground(width, maxRows, styles.TraySelectedBg)
}
