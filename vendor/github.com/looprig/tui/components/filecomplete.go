package components

import (
	"image/color"
	"strings"
	"unicode"

	"github.com/looprig/tui/styles"
)

// FileItem is one @path completion candidate. Path is the value to complete to (e.g.
// "src" or "src/main.go"); IsDir drives the trailing "/" affordance and whether
// selecting it keeps the panel open to drill in.
type FileItem struct {
	Path  string
	IsDir bool
}

// FileComplete is a filtered file list with a wrapping cursor — the @path completion
// panel, the disk-backed sibling of SlashComplete. It is display-only: package tui
// computes the candidate list (the filesystem read) and feeds it here.
type FileComplete struct {
	items        []FileItem
	cursor       int
	windowStart  int
	windowPinned bool
}

// NewFileComplete returns a completer over items, or nil when empty (nil = hidden).
func NewFileComplete(items []FileItem) *FileComplete {
	if len(items) == 0 {
		return nil
	}
	return &FileComplete{items: items, cursor: 0}
}

// Selected returns the item under the cursor.
func (f *FileComplete) Selected() FileItem { return f.items[f.cursor] }

// Cursor returns the absolute selected index in the filtered list.
func (f *FileComplete) Cursor() int { return f.cursor }

// Up moves the cursor up, wrapping to the bottom.
func (f *FileComplete) Up() {
	f.cursor = (f.cursor - 1 + len(f.items)) % len(f.items)
	f.windowPinned = false
}

// Down moves the cursor down, wrapping to the top.
func (f *FileComplete) Down() {
	f.cursor = (f.cursor + 1) % len(f.items)
	f.windowPinned = false
}

// SelectWindowRow moves the cursor to a row in the currently rendered maxRows window.
// It returns whether the selection changed; rows outside the visible window are ignored.
func (f *FileComplete) SelectWindowRow(row, maxRows int) bool {
	visible := min(len(f.items), maxRows)
	if row < 0 || row >= visible {
		return false
	}
	start := f.visibleWindowStart(maxRows)
	next := start + row
	if next == f.cursor {
		return false
	}
	f.cursor = next
	f.windowStart = start
	f.windowPinned = true
	return true
}

func (f *FileComplete) visibleWindowStart(maxRows int) int {
	visible := min(len(f.items), maxRows)
	if f.windowPinned && visible > 0 {
		start := max(0, min(f.windowStart, len(f.items)-visible))
		if f.cursor >= start && f.cursor < start+visible {
			return start
		}
	}
	return completionTrayWindowStart(len(f.items), f.cursor, maxRows)
}

// label is the complete displayed @path, plus a trailing "/" for a directory so a
// folder reads as drillable at a glance.
func (i FileItem) label() string {
	name := "@" + sanitizeFilePath(i.Path)
	if i.IsDir {
		return name + "/"
	}
	return name
}

// sanitizeFilePath makes an arbitrary filesystem name safe to paint in a terminal.
// It is deliberately display-only: FileItem.Path remains byte-for-byte unchanged for
// selection and completion. Replacing every Unicode control rune also neutralizes the
// ESC, C1 CSI/OSC, and terminator bytes used by terminal control sequences.
func sanitizeFilePath(path string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '\uFFFD'
		}
		return r
	}, path)
}

func (f *FileComplete) trayRows() []completionTrayRow {
	rows := make([]completionTrayRow, len(f.items))
	for i, item := range f.items {
		rows[i] = completionTrayRow{primary: item.label()}
	}
	return rows
}

// View renders the filtered list at its natural content width.
func (f *FileComplete) View() string {
	rows := f.trayRows()
	return renderCompletionTray(rows, f.cursor, completionTrayNaturalWidth(rows))
}

// ViewWidth renders the filtered list as a tray whose rows are padded or clamped
// ANSI-safely to width display columns.
func (f *FileComplete) ViewWidth(width int) string {
	return renderCompletionTray(f.trayRows(), f.cursor, width)
}

// ViewWindow renders a full-width tray capped to maxRows and keeps the selected path in the
// visible window. View and ViewWidth remain the unbounded variants.
func (f *FileComplete) ViewWindow(width, maxRows int) string {
	return renderCompletionTrayWindowBackgroundAt(f.trayRows(), f.cursor, width, maxRows, f.visibleWindowStart(maxRows), styles.TraySelectedBg)
}

// ViewWindowBackground renders the bounded tray with a caller-provided selected-row fill.
func (f *FileComplete) ViewWindowBackground(width, maxRows int, selectedBg color.Color) string {
	return renderCompletionTrayWindowBackgroundAt(f.trayRows(), f.cursor, width, maxRows, f.visibleWindowStart(maxRows), selectedBg)
}
