package loopruntime

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/looprig/core/content"
)

// compactionToolResultRunes is the v1 model-facing cap for one tool result.
// Keep it private so a future projection revision can change the budget without
// accidentally becoming a live-history or event-wire contract.
const compactionToolResultRunes = 2000

const (
	compactionToolResultTruncatedMarker = "[tool result truncated for compaction: omitted %d runes]"
	compactionThinkingPlaceholder       = "[thinking omitted for compaction]"
	compactionImagePlaceholder          = "[image omitted for compaction]"
	compactionAudioPlaceholder          = "[audio omitted for compaction]"
	compactionDocumentPlaceholder       = "[document omitted for compaction]"
)

const compactionProjectionMaxDepth = 128

// projectCompactionTranscript builds a fresh, text-oriented view for a
// summarization request. It never mutates or stores the source conversation;
// callers must keep the returned projection at the compaction boundary only.
func projectCompactionTranscript(messages content.AgenticMessages) (content.AgenticMessages, error) {
	projected := make(content.AgenticMessages, len(messages))
	for index, message := range messages {
		value, err := projectCompactionMessage(message)
		if err != nil {
			return nil, err
		}
		projected[index] = value
	}
	return projected, nil
}

type compactionProjectionError struct{ field string }

func (e *compactionProjectionError) Error() string {
	return "loopruntime: invalid compaction projection " + e.field
}

func projectCompactionMessage(message content.Conversation) (content.Conversation, error) {
	if message == nil {
		return nil, &compactionProjectionError{field: "message"}
	}
	switch typed := message.(type) {
	case *content.UserMessage:
		if typed == nil || typed.Role != content.RoleUser {
			return nil, &compactionProjectionError{field: "message"}
		}
		blocks, err := projectCompactionBlocks(typed.Blocks, 0)
		if err != nil {
			return nil, err
		}
		return &content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: blocks}}, nil
	case *content.AIMessage:
		if typed == nil || typed.Role != content.RoleAssistant {
			return nil, &compactionProjectionError{field: "message"}
		}
		blocks, err := projectCompactionBlocks(typed.Blocks, 0)
		if err != nil {
			return nil, err
		}
		var usage *content.Usage
		if typed.Usage != nil {
			cloned := *typed.Usage
			usage = &cloned
		}
		return &content.AIMessage{Message: content.Message{Role: content.RoleAssistant, Blocks: blocks}, Usage: usage}, nil
	case *content.SystemMessage:
		if typed == nil || typed.Role != content.RoleSystem {
			return nil, &compactionProjectionError{field: "message"}
		}
		blocks, err := projectCompactionBlocks(typed.Blocks, 0)
		if err != nil {
			return nil, err
		}
		return &content.SystemMessage{Message: content.Message{Role: content.RoleSystem, Blocks: blocks}}, nil
	case *content.ToolResultMessage:
		if typed == nil || typed.Role != content.RoleTool {
			return nil, &compactionProjectionError{field: "message"}
		}
		if len(typed.Blocks) == 0 {
			var blocks []content.Block
			if typed.Blocks != nil {
				blocks = make([]content.Block, 0)
			}
			return &content.ToolResultMessage{
				Message: content.Message{Role: content.RoleTool, Blocks: blocks}, ToolUseID: typed.ToolUseID, IsError: typed.IsError,
			}, nil
		}
		body, err := projectCompactionToolResultBody(typed.Blocks, 0)
		if err != nil {
			return nil, err
		}
		return &content.ToolResultMessage{
			Message:   content.Message{Role: content.RoleTool, Blocks: []content.Block{&content.TextBlock{Text: body}}},
			ToolUseID: typed.ToolUseID,
			IsError:   typed.IsError,
		}, nil
	default:
		return nil, &compactionProjectionError{field: "message"}
	}
}

func projectCompactionBlocks(blocks []content.Block, depth int) ([]content.Block, error) {
	if depth > compactionProjectionMaxDepth {
		return nil, &compactionProjectionError{field: "depth"}
	}
	if blocks == nil {
		return nil, nil
	}
	projected := make([]content.Block, len(blocks))
	for index, block := range blocks {
		text, err := projectCompactionBlock(block, depth)
		if err != nil {
			return nil, err
		}
		projected[index] = &content.TextBlock{Text: text}
	}
	return projected, nil
}

func projectCompactionToolResultBody(blocks []content.Block, depth int) (string, error) {
	if depth > compactionProjectionMaxDepth {
		return "", &compactionProjectionError{field: "depth"}
	}
	var accumulator compactionRuneAccumulator
	for _, block := range blocks {
		if err := accumulator.appendBlock(block, depth); err != nil {
			return "", err
		}
	}
	return accumulator.string(), nil
}

// compactionRuneFragment retains one rune's original bytes. Keeping fragments
// rather than []rune preserves Go's range semantics for invalid UTF-8 while
// bounding auxiliary storage to the projection cap.
type compactionRuneFragment struct {
	bytes [utf8.UTFMax]byte
	count int
}

type compactionRuneAccumulator struct {
	head      []compactionRuneFragment
	tail      []compactionRuneFragment
	tailStart int
	tailCount int
	total     int
}

func (a *compactionRuneAccumulator) appendBlock(block content.Block, depth int) error {
	if depth > compactionProjectionMaxDepth || block == nil {
		return &compactionProjectionError{field: "block"}
	}
	switch typed := block.(type) {
	case *content.TextBlock:
		if typed == nil {
			return &compactionProjectionError{field: "block"}
		}
		a.appendString(typed.Text)
	case *content.ToolUseBlock:
		if typed == nil {
			return &compactionProjectionError{field: "block"}
		}
		a.appendString("[called tool: ")
		a.appendString(typed.Name)
		a.appendString("]")
	case *content.ThinkingBlock:
		if typed == nil {
			return &compactionProjectionError{field: "block"}
		}
		a.appendString(compactionThinkingPlaceholder)
	case *content.ImageBlock:
		if typed == nil {
			return &compactionProjectionError{field: "block"}
		}
		a.appendString(compactionImagePlaceholder)
	case *content.AudioBlock:
		if typed == nil {
			return &compactionProjectionError{field: "block"}
		}
		a.appendString(compactionAudioPlaceholder)
	case *content.DocumentBlock:
		if typed == nil {
			return &compactionProjectionError{field: "block"}
		}
		a.appendString(compactionDocumentPlaceholder)
	case *content.ToolResultBlock:
		if typed == nil {
			return &compactionProjectionError{field: "block"}
		}
		nested, err := projectCompactionToolResultBody(typed.Content, depth+1)
		if err != nil {
			return err
		}
		a.appendString(nested)
	default:
		return &compactionProjectionError{field: "block"}
	}
	return nil
}

func (a *compactionRuneAccumulator) appendString(value string) {
	for offset := 0; offset < len(value); {
		runeValue, size := utf8.DecodeRuneInString(value[offset:])
		if runeValue == utf8.RuneError && size == 0 {
			size = 1
		}
		var fragment compactionRuneFragment
		fragment.count = size
		copy(fragment.bytes[:], value[offset:offset+size])
		a.append(fragment)
		offset += size
	}
}

func (a *compactionRuneAccumulator) append(fragment compactionRuneFragment) {
	a.total++
	if len(a.head) < compactionToolResultRunes {
		a.head = append(a.head, fragment)
	}
	if len(a.tail) < compactionToolResultRunes {
		a.tail = append(a.tail, fragment)
		a.tailCount++
		return
	}
	a.tail[a.tailStart] = fragment
	a.tailStart = (a.tailStart + 1) % compactionToolResultRunes
}

func (a *compactionRuneAccumulator) string() string {
	if a.total <= compactionToolResultRunes {
		return a.fragmentsString(a.head, 0, len(a.head))
	}
	marker := fmt.Sprintf(compactionToolResultTruncatedMarker, a.total)
	for iteration := 0; iteration < 8; iteration++ {
		available := compactionToolResultRunes - utf8.RuneCountInString(marker)
		if available <= 0 {
			return string([]rune(marker)[:compactionToolResultRunes])
		}
		headCount := available / 2
		tailCount := available - headCount
		updated := fmt.Sprintf(compactionToolResultTruncatedMarker, a.total-headCount-tailCount)
		if updated == marker {
			return a.fragmentsStringWithMarker(headCount, marker, tailCount)
		}
		marker = updated
	}
	available := compactionToolResultRunes - utf8.RuneCountInString(marker)
	if available <= 0 {
		return string([]rune(marker)[:compactionToolResultRunes])
	}
	headCount := available / 2
	tailCount := available - headCount
	return a.fragmentsStringWithMarker(headCount, marker, tailCount)
}

func (a *compactionRuneAccumulator) fragmentsStringWithMarker(headCount int, marker string, tailCount int) string {
	var builder strings.Builder
	builder.Grow(len(marker) + headCount + tailCount)
	a.writeFragments(&builder, a.head, 0, headCount)
	builder.WriteString(marker)
	start := (a.tailStart + a.tailCount - tailCount) % compactionToolResultRunes
	a.writeFragmentsRing(&builder, start, tailCount)
	return builder.String()
}

func (a *compactionRuneAccumulator) fragmentsString(fragments []compactionRuneFragment, start, count int) string {
	var builder strings.Builder
	for index := start; index < start+count; index++ {
		builder.Write(fragments[index].bytes[:fragments[index].count])
	}
	return builder.String()
}

func (a *compactionRuneAccumulator) writeFragments(builder *strings.Builder, fragments []compactionRuneFragment, start, count int) {
	for index := start; index < start+count; index++ {
		builder.Write(fragments[index].bytes[:fragments[index].count])
	}
}

func (a *compactionRuneAccumulator) writeFragmentsRing(builder *strings.Builder, start, count int) {
	for index := 0; index < count; index++ {
		fragment := a.tail[(start+index)%compactionToolResultRunes]
		builder.Write(fragment.bytes[:fragment.count])
	}
}

func projectCompactionBlock(block content.Block, depth int) (string, error) {
	if depth > compactionProjectionMaxDepth || block == nil {
		return "", &compactionProjectionError{field: "block"}
	}
	switch typed := block.(type) {
	case *content.TextBlock:
		if typed == nil {
			return "", &compactionProjectionError{field: "block"}
		}
		return typed.Text, nil
	case *content.ToolUseBlock:
		if typed == nil {
			return "", &compactionProjectionError{field: "block"}
		}
		return "[called tool: " + typed.Name + "]", nil
	case *content.ThinkingBlock:
		if typed == nil {
			return "", &compactionProjectionError{field: "block"}
		}
		return compactionThinkingPlaceholder, nil
	case *content.ImageBlock:
		if typed == nil {
			return "", &compactionProjectionError{field: "block"}
		}
		return compactionImagePlaceholder, nil
	case *content.AudioBlock:
		if typed == nil {
			return "", &compactionProjectionError{field: "block"}
		}
		return compactionAudioPlaceholder, nil
	case *content.DocumentBlock:
		if typed == nil {
			return "", &compactionProjectionError{field: "block"}
		}
		return compactionDocumentPlaceholder, nil
	case *content.ToolResultBlock:
		if typed == nil {
			return "", &compactionProjectionError{field: "block"}
		}
		body, err := projectCompactionToolResultBody(typed.Content, depth+1)
		if err != nil {
			return "", err
		}
		return capCompactionToolResult(body), nil
	default:
		return "", &compactionProjectionError{field: "block"}
	}
}

func capCompactionToolResult(value string) string {
	runes := []rune(value)
	if len(runes) <= compactionToolResultRunes {
		return value
	}
	marker := fmt.Sprintf(compactionToolResultTruncatedMarker, len(runes))
	for iteration := 0; iteration < 8; iteration++ {
		available := compactionToolResultRunes - len([]rune(marker))
		if available <= 0 {
			return string([]rune(marker)[:compactionToolResultRunes])
		}
		headCount := available / 2
		tailCount := available - headCount
		omitted := len(runes) - headCount - tailCount
		updated := fmt.Sprintf(compactionToolResultTruncatedMarker, omitted)
		if updated == marker {
			return string(runes[:headCount]) + marker + string(runes[len(runes)-tailCount:])
		}
		marker = updated
	}
	available := compactionToolResultRunes - len([]rune(marker))
	if available <= 0 {
		return string([]rune(marker)[:compactionToolResultRunes])
	}
	headCount := available / 2
	tailCount := available - headCount
	return string(runes[:headCount]) + marker + string(runes[len(runes)-tailCount:])
}
