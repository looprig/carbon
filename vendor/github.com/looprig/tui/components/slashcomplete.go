package components

import (
	"image/color"
	"strings"

	"github.com/looprig/tui/styles"
)

// SlashCmd is one slash command's display metadata. The action is dispatched by
// package tui keyed on Name; this widget only filters and displays.
type SlashCmd struct {
	Name string // e.g. "/clear"
	Desc string // e.g. "clear the conversation"
}

// SlashCommands is the canonical list (exported so package tui can map Name→action).
var SlashCommands = []SlashCmd{
	{"/clear", "start a new conversation"},
	{"/compact", "compact the current conversation"},
	{"/exit", "exit Looprig"},
}

var slashRelatedWords = map[string][]string{
	"/clear":   {"new", "reset", "restart"},
	"/compact": {"compress", "context", "shorten"},
	"/exit":    {"quit", "close", "leave"},
}

// SlashComplete is a filtered command list with a wrapping cursor.
type SlashComplete struct {
	items        []SlashCmd
	cursor       int
	windowStart  int
	windowPinned bool
}

// NewSlashComplete returns a case-insensitive, relevance-ranked completer for prefix.
// The optional leading slash is ignored while matching. Returns nil when nothing
// matches (nil = panel hidden).
func NewSlashComplete(prefix string) *SlashComplete {
	return NewSlashCompleteWithCommands(prefix, SlashCommands)
}

// NewSlashCompleteWithCommands builds a completer from an immutable caller-owned catalog.
// The slice and aliases are copied so a live tray cannot change underneath keyboard input.
func NewSlashCompleteWithCommands(prefix string, commands []SlashCmd) *SlashComplete {
	query := strings.ToLower(strings.TrimPrefix(prefix, "/"))
	if query == "" {
		return &SlashComplete{items: append([]SlashCmd(nil), commands...)}
	}

	var matches []SlashCmd
	for rank := 0; rank < 5; rank++ {
		for _, command := range commands {
			if slashMatchRank(command, query) == rank {
				matches = append(matches, command)
			}
		}
	}
	if len(matches) == 0 {
		return nil
	}
	return &SlashComplete{items: matches, cursor: 0}
}

func slashMatchRank(command SlashCmd, query string) int {
	name := strings.ToLower(strings.TrimPrefix(command.Name, "/"))
	switch {
	case name == query:
		return 0
	case strings.HasPrefix(name, query):
		return 1
	}

	related := slashRelatedWords[command.Name]
	for _, word := range related {
		word = strings.ToLower(word)
		if word == query {
			return 2
		}
	}
	for _, word := range related {
		word = strings.ToLower(word)
		if strings.Contains(word, query) {
			return 3
		}
	}
	if strings.Contains(name, query) {
		return 4
	}
	return -1
}

// Selected returns the item under the cursor.
func (s *SlashComplete) Selected() SlashCmd {
	return s.items[s.cursor]
}

// Cursor returns the absolute selected index in the filtered list.
func (s *SlashComplete) Cursor() int { return s.cursor }

// Up moves the cursor up, wrapping to the bottom.
func (s *SlashComplete) Up() {
	s.cursor = (s.cursor - 1 + len(s.items)) % len(s.items)
	s.windowPinned = false
}

// Down moves the cursor down, wrapping to the top.
func (s *SlashComplete) Down() {
	s.cursor = (s.cursor + 1) % len(s.items)
	s.windowPinned = false
}

// SelectWindowRow moves the cursor to a row in the currently rendered maxRows window.
// It returns whether the selection changed; rows outside the visible window are ignored.
func (s *SlashComplete) SelectWindowRow(row, maxRows int) bool {
	visible := min(len(s.items), maxRows)
	if row < 0 || row >= visible {
		return false
	}
	start := s.visibleWindowStart(maxRows)
	next := start + row
	if next == s.cursor {
		return false
	}
	s.cursor = next
	s.windowStart = start
	s.windowPinned = true
	return true
}

func (s *SlashComplete) visibleWindowStart(maxRows int) int {
	visible := min(len(s.items), maxRows)
	if s.windowPinned && visible > 0 {
		start := max(0, min(s.windowStart, len(s.items)-visible))
		if s.cursor >= start && s.cursor < start+visible {
			return start
		}
	}
	return completionTrayWindowStart(len(s.items), s.cursor, maxRows)
}

func (s *SlashComplete) trayRows() []completionTrayRow {
	rows := make([]completionTrayRow, len(s.items))
	for i, item := range s.items {
		rows[i] = completionTrayRow{primary: item.Name, secondary: item.Desc}
	}
	return rows
}

// View renders the filtered list at its natural content width.
func (s *SlashComplete) View() string {
	rows := s.trayRows()
	return renderCompletionTray(rows, s.cursor, completionTrayNaturalWidth(rows))
}

// ViewWidth renders the filtered list as a tray whose rows are padded or clamped
// ANSI-safely to width display columns.
func (s *SlashComplete) ViewWidth(width int) string {
	return renderCompletionTray(s.trayRows(), s.cursor, width)
}

// ViewWindow renders a full-width tray capped to maxRows and keeps the selected command in
// the visible window. View and ViewWidth remain the unbounded variants.
func (s *SlashComplete) ViewWindow(width, maxRows int) string {
	return renderCompletionTrayWindowBackgroundAt(s.trayRows(), s.cursor, width, maxRows, s.visibleWindowStart(maxRows), styles.TraySelectedBg)
}

// ViewWindowBackground renders the bounded tray with a caller-provided selected-row fill.
func (s *SlashComplete) ViewWindowBackground(width, maxRows int, selectedBg color.Color) string {
	return renderCompletionTrayWindowBackgroundAt(s.trayRows(), s.cursor, width, maxRows, s.visibleWindowStart(maxRows), selectedBg)
}
