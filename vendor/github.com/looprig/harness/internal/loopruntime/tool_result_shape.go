package loopruntime

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const toolResultTruncatedMarker = "\n[tool output truncated: omitted %d of %d bytes]\n"

func shapeToolResultText(text string, limit int) string {
	if limit <= 0 {
		return text
	}
	if utf8.ValidString(text) {
		if len(text) <= limit {
			return text
		}
		return shapeNormalizedToolResult(text, "", len(text), limit)
	}
	normalized := strings.ToValidUTF8(text, "\uFFFD")
	if len(normalized) <= limit {
		return normalized
	}
	return shapeNormalizedToolResult(normalized, text, len(text), limit)
}

func shapeNormalizedToolResult(normalized, source string, originalBytes, limit int) string {
	marker := fmt.Sprintf(toolResultTruncatedMarker, originalBytes, originalBytes)
	for i := 0; i < 8; i++ {
		remaining := limit - len(marker)
		if remaining <= 0 {
			return boundedUTF8(normalized, limit)
		}
		headBudget := remaining / 2
		tailBudget := remaining - headBudget
		headEnd := utf8Boundary(normalized, headBudget)
		tailStart := utf8TailBoundary(normalized, tailBudget)
		head := normalized[:headEnd]
		tail := normalized[tailStart:]
		omitted := originalBytes - sourceBytesForNormalizedRange(source, 0, headEnd) - sourceBytesForNormalizedRange(source, tailStart, len(normalized))
		updated := fmt.Sprintf(toolResultTruncatedMarker, omitted, originalBytes)
		if updated == marker {
			return head + marker + tail
		}
		marker = updated
	}

	remaining := limit - len(marker)
	if remaining <= 0 {
		return boundedUTF8(normalized, limit)
	}
	headBudget := remaining / 2
	tailBudget := remaining - headBudget
	headEnd := utf8Boundary(normalized, headBudget)
	tailStart := utf8TailBoundary(normalized, tailBudget)
	head := normalized[:headEnd]
	return head + marker + normalized[tailStart:]
}

func sourceBytesForNormalizedRange(source string, start, end int) int {
	if source == "" {
		return end - start
	}
	result := 0
	normalizedOffset := 0
	for sourceOffset := 0; sourceOffset < len(source) && normalizedOffset < end; {
		sourceBytes, normalizedBytes := nextNormalizedSpan(source, sourceOffset)
		if normalizedOffset >= start {
			result += sourceBytes
		}
		sourceOffset += sourceBytes
		normalizedOffset += normalizedBytes
	}
	return result
}

func nextNormalizedSpan(source string, offset int) (sourceBytes, normalizedBytes int) {
	runeValue, size := utf8.DecodeRuneInString(source[offset:])
	if runeValue != utf8.RuneError || size != 1 {
		return size, size
	}
	start := offset
	offset += size
	for offset < len(source) {
		next, nextSize := utf8.DecodeRuneInString(source[offset:])
		if next != utf8.RuneError || nextSize != 1 {
			break
		}
		offset += nextSize
	}
	return offset - start, len("\uFFFD")
}

func utf8Boundary(value string, budget int) int {
	if budget <= 0 {
		return 0
	}
	if budget >= len(value) {
		return len(value)
	}
	for budget > 0 && !utf8.RuneStart(value[budget]) {
		budget--
	}
	return budget
}

func utf8TailBoundary(value string, budget int) int {
	if budget <= 0 {
		return len(value)
	}
	if budget >= len(value) {
		return 0
	}
	start := len(value) - budget
	for start < len(value) && !utf8.RuneStart(value[start]) {
		start++
	}
	return start
}

func boundedUTF8(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if limit >= len(value) {
		return value
	}
	return value[:utf8Boundary(value, limit)]
}
