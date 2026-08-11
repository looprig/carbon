package styles

import (
	"bytes"
	"errors"
	"strings"
	"unicode"

	"charm.land/glamour/v2"
	"charm.land/lipgloss/v2"
	xansi "github.com/charmbracelet/x/ansi"
	"github.com/yuin/goldmark"
	goldmarkast "github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	tableast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/text"
)

var errResponsiveTableCellRenderedEmpty = errors.New("nonempty responsive table cell rendered empty")

const (
	narrativeMinimumWords   = 4
	narrativeMinimumWidth   = 28
	minimumGridColumnWidth  = 3
	narrativeSoftFloor      = 16
	recordLeadingPadding    = 1
	recordFieldGap          = 2
	recordMinimumValueWidth = 24
	recordStackedIndent     = 2
)

type markdownTableCell struct {
	raw   string
	plain string
}

type markdownTable struct {
	start   int
	end     int
	prefix  string
	headers []markdownTableCell
	rows    [][]markdownTableCell
}

type markdownColumnMetrics struct {
	maxWidth  int
	words     int
	width     int
	nonEmpty  int
	narrative bool
}

type responsiveTableReplacement struct {
	table  markdownTable
	marker string
}

func renderMarkdownTables(r *glamour.TermRenderer, markdown string, width int) (string, error) {
	tables := parseResponsiveTables(markdown, width)
	if len(tables) == 0 {
		return renderOriginalMarkdown(r, markdown)
	}

	marked, replacements, ok := markResponsiveTables(markdown, tables)
	if !ok {
		return renderOriginalMarkdown(r, markdown)
	}
	rendered, err := r.Render(marked)
	if err != nil {
		return renderOriginalMarkdown(r, markdown)
	}
	for _, replacement := range replacements {
		rendered, ok, err = replaceResponsiveTable(r, rendered, replacement, width)
		if err != nil || !ok {
			return renderOriginalMarkdown(r, markdown)
		}
	}
	return rendered, nil
}

func renderOriginalMarkdown(r *glamour.TermRenderer, markdown string) (string, error) {
	return r.Render(markdown)
}

func markResponsiveTables(markdown string, tables []markdownTable) (string, []responsiveTableReplacement, bool) {
	replacements := make([]responsiveTableReplacement, len(tables))
	used := make(map[rune]bool, len(tables))
	for i, table := range tables {
		marker := unusedResponsiveTableMarker(markdown, used)
		if marker == "" || table.start < 0 || table.end < table.start || table.end > len(markdown) {
			return markdown, nil, false
		}
		used[[]rune(marker)[0]] = true
		replacements[i] = responsiveTableReplacement{table: table, marker: marker}
	}

	marked := markdown
	for i := len(replacements) - 1; i >= 0; i-- {
		replacement := replacements[i]
		source := markdown[replacement.table.start:replacement.table.end]
		lineEnding := ""
		switch {
		case strings.HasSuffix(source, "\r\n"):
			lineEnding = "\r\n"
		case strings.HasSuffix(source, "\n"):
			lineEnding = "\n"
		}
		placeholder := replacement.table.prefix + replacement.marker + lineEnding
		marked = marked[:replacement.table.start] + placeholder + marked[replacement.table.end:]
	}
	return marked, replacements, true
}

func unusedResponsiveTableMarker(markdown string, used map[rune]bool) string {
	for marker := rune(0xE000); marker <= 0xF8FF; marker++ {
		if !used[marker] && !strings.ContainsRune(markdown, marker) {
			return string(marker)
		}
	}
	for marker := rune(0xF0000); marker <= 0xFFFFD; marker++ {
		if !used[marker] && !strings.ContainsRune(markdown, marker) {
			return string(marker)
		}
	}
	return ""
}

func replaceResponsiveTable(
	r *glamour.TermRenderer,
	rendered string,
	replacement responsiveTableReplacement,
	width int,
) (string, bool, error) {
	lines := strings.Split(rendered, "\n")
	markerLine := -1
	markerOffset := -1
	for i, line := range lines {
		if offset := strings.Index(line, replacement.marker); offset >= 0 {
			if markerLine >= 0 {
				return rendered, false, nil
			}
			markerLine = i
			markerOffset = offset
		}
	}
	if markerLine < 0 {
		return rendered, false, nil
	}

	prefix := lines[markerLine][:markerOffset]
	if !isStructuralRenderedTablePrefix(prefix) {
		return rendered, false, nil
	}
	contentWidth := width - xansi.StringWidth(prefix)
	if contentWidth < 1 {
		return rendered, false, nil
	}
	records, err := renderResponsiveRecords(r, replacement.table, contentWidth)
	if err != nil || len(records) == 0 {
		return rendered, false, err
	}
	for i := range records {
		records[i] = prefix + records[i]
	}
	lines[markerLine] = strings.Join(records, "\n")
	return strings.Join(lines, "\n"), true, nil
}

func isStructuralRenderedTablePrefix(prefix string) bool {
	for _, char := range xansi.Strip(prefix) {
		if unicode.IsSpace(char) || char == '│' {
			continue
		}
		return false
	}
	return true
}

func renderResponsiveRecords(r *glamour.TermRenderer, table markdownTable, width int) ([]string, error) {
	labelWidth := 0
	for _, header := range table.headers {
		labelWidth = max(labelWidth, xansi.StringWidth(header.plain))
	}
	labelStyle := lipgloss.NewStyle().Bold(true)
	labels := make([]string, len(table.headers))
	for column, header := range table.headers {
		rendered, err := renderTableCell(r, header.raw)
		if err != nil {
			return nil, err
		}
		plainWidth := xansi.StringWidth(header.plain)
		if xansi.StringWidth(rendered) > plainWidth {
			rendered = xansi.Cut(rendered, 0, plainWidth)
		}
		labels[column] = labelStyle.Render(rendered)
	}
	alignedValueWidth := width - recordLeadingPadding - labelWidth - recordFieldGap
	stacked := alignedValueWidth < recordMinimumValueWidth
	valueWidth := alignedValueWidth
	if stacked {
		valueWidth = width - recordStackedIndent
	}
	if valueWidth < 1 {
		return nil, nil
	}

	ruleStyle := lipgloss.NewStyle().Faint(true)
	lines := make([]string, 0, len(table.rows)*len(table.headers))
	for rowIndex, row := range table.rows {
		for column, header := range table.headers {
			value, err := renderTableCell(r, row[column].raw)
			if err != nil {
				return nil, err
			}
			wrapped := strings.Split(xansi.Wrap(value, valueWidth, ""), "\n")
			if len(wrapped) == 0 {
				wrapped = []string{""}
			}
			label := labels[column]
			if stacked {
				labelLineWidth := width - recordLeadingPadding
				if labelLineWidth < 1 {
					return nil, nil
				}
				for _, labelLine := range strings.Split(xansi.Wrap(label, labelLineWidth, ""), "\n") {
					lines = append(lines, strings.Repeat(" ", recordLeadingPadding)+labelLine)
				}
				for _, valueLine := range wrapped {
					lines = append(lines, strings.Repeat(" ", recordStackedIndent)+valueLine)
				}
				continue
			}

			labelPadding := labelWidth - xansi.StringWidth(header.plain)
			fieldPrefix := strings.Repeat(" ", recordLeadingPadding) + label +
				strings.Repeat(" ", labelPadding+recordFieldGap)
			continuation := strings.Repeat(" ", recordLeadingPadding+labelWidth+recordFieldGap)
			for lineIndex, valueLine := range wrapped {
				if lineIndex == 0 {
					lines = append(lines, fieldPrefix+valueLine)
				} else {
					lines = append(lines, continuation+valueLine)
				}
			}
		}
		if rowIndex < len(table.rows)-1 {
			lines = append(lines, ruleStyle.Render(strings.Repeat("─", width)))
		}
	}
	return lines, nil
}

func renderTableCell(r *glamour.TermRenderer, raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	rendered, err := r.Render(raw)
	if err != nil {
		return "", err
	}
	lines := strings.Split(rendered, "\n")
	for len(lines) > 0 && strings.TrimSpace(xansi.Strip(lines[0])) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(xansi.Strip(lines[len(lines)-1])) == "" {
		lines = lines[:len(lines)-1]
	}
	for i, line := range lines {
		plain := strings.TrimRight(xansi.Strip(line), " \t\r")
		if plain == "" {
			lines[i] = ""
			continue
		}
		lines[i] = xansi.Cut(line, 0, xansi.StringWidth(plain))
	}
	result := strings.Join(lines, "\n")
	if strings.TrimSpace(raw) != "" && strings.TrimSpace(xansi.Strip(result)) == "" {
		return "", errResponsiveTableCellRenderedEmpty
	}
	return result, nil
}

func parseResponsiveTables(markdown string, width int) []markdownTable {
	tables := extractMarkdownTables(markdown)
	selected := make([]markdownTable, 0, len(tables))
	for _, table := range tables {
		if tableNeedsResponsiveLayout(table, width) {
			selected = append(selected, table)
		}
	}
	return selected
}

func extractMarkdownTables(markdown string) []markdownTable {
	source := []byte(markdown)
	parser := goldmark.New(
		goldmark.WithExtensions(extension.GFM, extension.DefinitionList),
	).Parser()
	document := parser.Parse(text.NewReader(source))

	var tables []markdownTable
	_ = goldmarkast.Walk(document, func(node goldmarkast.Node, entering bool) (goldmarkast.WalkStatus, error) {
		tableNode, ok := node.(*tableast.Table)
		if !entering || !ok {
			return goldmarkast.WalkContinue, nil
		}
		if table, ok := extractMarkdownTable(tableNode, source); ok {
			tables = append(tables, table)
		}
		return goldmarkast.WalkSkipChildren, nil
	})
	return tables
}

func extractMarkdownTable(node *tableast.Table, source []byte) (markdownTable, bool) {
	columns := len(node.Alignments)
	if columns == 0 {
		return markdownTable{}, false
	}

	var header *tableast.TableHeader
	var bodyRows []*tableast.TableRow
	for child := node.FirstChild(); child != nil; child = child.NextSibling() {
		switch typed := child.(type) {
		case *tableast.TableHeader:
			header = typed
		case *tableast.TableRow:
			bodyRows = append(bodyRows, typed)
		}
	}
	if header == nil {
		return markdownTable{}, false
	}

	lineStart := sourceLineStart(source, header.Pos())
	endOffset := sourceLineEnd(source, header.Pos())
	if len(bodyRows) > 0 {
		endOffset = sourceLineEnd(source, bodyRows[len(bodyRows)-1].Pos())
	} else if nextLine := endOffset; nextLine < len(source) {
		endOffset = sourceLineEnd(source, nextLine)
	}

	table := markdownTable{
		start:   lineStart,
		end:     endOffset,
		prefix:  string(source[lineStart:header.Pos()]),
		headers: tableCells(header, columns, source),
		rows:    make([][]markdownTableCell, 0, len(bodyRows)),
	}
	for _, row := range bodyRows {
		table.rows = append(table.rows, tableCells(row, columns, source))
	}
	return table, true
}

func tableCells(row goldmarkast.Node, columns int, source []byte) []markdownTableCell {
	cells := make([]markdownTableCell, 0, columns)
	for child := row.FirstChild(); child != nil && len(cells) < columns; child = child.NextSibling() {
		cell, ok := child.(*tableast.TableCell)
		if !ok {
			continue
		}
		var raw string
		if cell.Lines().Len() > 0 {
			segment := cell.Lines().At(0)
			raw = string(segment.Value(source))
		}
		cells = append(cells, markdownTableCell{
			raw:   strings.TrimSpace(raw),
			plain: strings.TrimSpace(tableCellPlainText(cell, source)),
		})
	}
	for len(cells) < columns {
		cells = append(cells, markdownTableCell{})
	}
	return cells
}

func tableCellPlainText(cell *tableast.TableCell, source []byte) string {
	var plain strings.Builder
	_ = goldmarkast.Walk(cell, func(node goldmarkast.Node, entering bool) (goldmarkast.WalkStatus, error) {
		if !entering {
			return goldmarkast.WalkContinue, nil
		}
		switch typed := node.(type) {
		case *goldmarkast.Text:
			plain.Write(typed.Value(source))
			if typed.SoftLineBreak() || typed.HardLineBreak() {
				plain.WriteByte('\n')
			}
		case *goldmarkast.String:
			plain.Write(typed.Value)
		}
		return goldmarkast.WalkContinue, nil
	})
	return plain.String()
}

func tableNeedsResponsiveLayout(table markdownTable, width int) bool {
	if len(table.rows) == 0 {
		return false
	}
	metrics := tableColumnMetrics(table)
	if len(metrics) == 0 {
		return false
	}

	const cellPadding = 2
	gridOverhead := len(metrics)*(cellPadding+1) + 1
	intrinsicWidth := gridOverhead
	for _, metric := range metrics {
		intrinsicWidth += metric.maxWidth
	}
	if intrinsicWidth <= width {
		return false
	}
	if width-gridOverhead < len(metrics)*minimumGridColumnWidth {
		return true
	}

	allocations := allocateGridColumns(metrics, width-gridOverhead)
	for column, metric := range metrics {
		if metric.narrative && metric.maxWidth > allocations[column] {
			return true
		}
	}
	return false
}

func tableColumnMetrics(table markdownTable) []markdownColumnMetrics {
	metrics := make([]markdownColumnMetrics, len(table.headers))
	for column, header := range table.headers {
		metrics[column].maxWidth = xansi.StringWidth(header.plain)
	}
	for _, row := range table.rows {
		for column, cell := range row {
			cellWidth := xansi.StringWidth(cell.plain)
			metrics[column].maxWidth = max(metrics[column].maxWidth, cellWidth)
			if cell.plain == "" {
				continue
			}
			metrics[column].nonEmpty++
			metrics[column].words += len(strings.Fields(cell.plain))
			metrics[column].width += cellWidth
		}
	}
	for column := range metrics {
		metric := &metrics[column]
		if metric.nonEmpty == 0 {
			continue
		}
		metric.narrative = metric.words >= narrativeMinimumWords*metric.nonEmpty ||
			metric.width >= narrativeMinimumWidth*metric.nonEmpty
	}
	return metrics
}

func allocateGridColumns(metrics []markdownColumnMetrics, available int) []int {
	allocations := make([]int, len(metrics))
	narrativeColumns := 0
	compactColumns := 0
	for _, metric := range metrics {
		if metric.narrative {
			narrativeColumns++
		} else {
			compactColumns++
		}
	}
	if narrativeColumns == 0 {
		for column, metric := range metrics {
			allocations[column] = metric.maxWidth
		}
		return allocations
	}

	minimumCompactBudget := compactColumns * minimumGridColumnWidth
	narrativeBudget := min(narrativeColumns*narrativeSoftFloor, max(0, available-minimumCompactBudget))
	compactBudget := max(0, available-narrativeBudget)
	remainingCompact := compactColumns
	for column, metric := range metrics {
		if metric.narrative {
			continue
		}
		maximumHere := max(0, compactBudget-(remainingCompact-1)*minimumGridColumnWidth)
		allocation := min(max(metric.maxWidth, minimumGridColumnWidth), maximumHere)
		allocations[column] = allocation
		compactBudget -= allocation
		remainingCompact--
	}

	narrativeBudget = max(0, available)
	for column, metric := range metrics {
		if !metric.narrative {
			narrativeBudget -= allocations[column]
		}
	}
	perColumn := narrativeBudget / narrativeColumns
	remainder := narrativeBudget % narrativeColumns
	for column, metric := range metrics {
		if !metric.narrative {
			continue
		}
		allocations[column] = perColumn
		if remainder > 0 {
			allocations[column]++
			remainder--
		}
	}
	return allocations
}

func sourceLineStart(source []byte, offset int) int {
	if offset > len(source) {
		offset = len(source)
	}
	if index := bytes.LastIndexByte(source[:offset], '\n'); index >= 0 {
		return index + 1
	}
	return 0
}

func sourceLineEnd(source []byte, offset int) int {
	if offset < 0 {
		offset = 0
	}
	if offset > len(source) {
		offset = len(source)
	}
	if newline := bytes.IndexByte(source[offset:], '\n'); newline >= 0 {
		return offset + newline + 1
	}
	return len(source)
}
