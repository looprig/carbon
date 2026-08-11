package components

import (
	"fmt"
	"image/color"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/looprig/tui/styles"
)

// SessionItem is the already-formatted, secret-free view data for one session record.
type SessionItem struct {
	ID       string
	Title    string
	State    string
	Activity string
	LastUsed string
	ShortID  string
}

// SessionComplete selects records, while each record renders as two content rows plus one
// unboxed padding row. The accent rail remains continuous across all three rows.
type SessionComplete struct {
	items  []SessionItem
	cursor int
}

func NewSessionComplete(items []SessionItem) *SessionComplete {
	if len(items) == 0 {
		return nil
	}
	return &SessionComplete{items: append([]SessionItem(nil), items...)}
}

func (s *SessionComplete) Selected() SessionItem { return s.items[s.cursor] }
func (s *SessionComplete) Cursor() int           { return s.cursor }
func (s *SessionComplete) Up()                   { s.cursor = (s.cursor - 1 + len(s.items)) % len(s.items) }
func (s *SessionComplete) Down()                 { s.cursor = (s.cursor + 1) % len(s.items) }

// SelectWindowRow selects by record. The blank third row between records remains inert.
func (s *SessionComplete) SelectWindowRow(row, maxRows int) bool {
	if row < 0 || row%3 == 2 {
		return false
	}
	records := max(1, maxRows/3)
	start := completionTrayWindowStart(len(s.items), s.cursor, records)
	next := start + row/3
	if next < 0 || next >= min(len(s.items), start+records) || next == s.cursor {
		return false
	}
	s.cursor = next
	return true
}

func (s *SessionComplete) ViewWindowBackground(width, maxRows int, selectedBg color.Color) string {
	if width <= 0 || maxRows < 2 || len(s.items) == 0 {
		return ""
	}
	records := max(1, maxRows/3)
	start := completionTrayWindowStart(len(s.items), s.cursor, records)
	end := min(len(s.items), start+records)
	rows := make([]string, 0, (end-start)*3)
	for i := start; i < end; i++ {
		item := s.items[i]
		selected := i == s.cursor
		rows = append(rows,
			renderSessionLine(width, item.Title+joinMetadata(item.State, item.Activity), selected, selectedBg),
			renderSessionLine(width, fmt.Sprintf("%s · %s", item.LastUsed, item.ShortID), selected, selectedBg),
		)
		if i+1 < end {
			rows = append(rows, renderSessionLine(width, "", false, selectedBg))
		}
	}
	return strings.Join(rows, "\n")
}

func joinMetadata(parts ...string) string {
	var kept []string
	for _, part := range parts {
		if strings.TrimSpace(part) != "" {
			kept = append(kept, part)
		}
	}
	if len(kept) == 0 {
		return ""
	}
	return " · " + strings.Join(kept, " · ")
}

func renderSessionLine(width int, content string, selected bool, selectedBg color.Color) string {
	rail := styles.AccentBarStyle.Render(styles.AccentBar)
	open, reset := styles.DeriveBackgroundSGR(styles.PanelBg)
	if selected {
		rail = styles.CardRailStyle.Render(styles.AccentBar)
		open, reset = styles.DeriveBackgroundSGR(selectedBg)
	}
	line := ansi.Truncate(rail+" "+content, width, "")
	return styles.FillLineBackgroundWith(line, width, open, reset)
}
