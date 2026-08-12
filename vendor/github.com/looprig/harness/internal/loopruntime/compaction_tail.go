package loopruntime

import (
	"encoding/json"

	"github.com/looprig/core/content"
)

// compactionTailSelection partitions one validated transcript at a complete
// conversational boundary. Head is the history that may be summarized; Retained
// is the newest protected suffix that must survive compaction unchanged.
type compactionTailSelection struct {
	Head     content.AgenticMessages
	Retained content.AgenticMessages
}

type compactionTailError struct{ field string }

func (e *compactionTailError) Error() string {
	return "loopruntime: invalid compaction tail " + e.field
}

type compactionTailSegment struct {
	start  int
	end    int
	tokens content.TokenCount
}

type compactionTailToolPair struct {
	call   int
	result int
}

// selectCompactionTail deterministically protects the newest complete user-
// anchored segments within both configured limits. The token estimate is the
// deliberately conservative v1 estimate ceil(JSON bytes / 4), calculated over
// the original unprojected message. It returns owned clones on success.
func selectCompactionTail(
	transcript content.AgenticMessages,
	derivedPrefix int,
	maxSegments int,
	maxTokens content.TokenCount,
) (compactionTailSelection, error) {
	if len(transcript) == 0 {
		return compactionTailSelection{}, &compactionTailError{field: "transcript"}
	}
	if derivedPrefix < 0 || derivedPrefix > len(transcript) {
		return compactionTailSelection{}, &compactionTailError{field: "derived_prefix"}
	}
	if maxSegments <= 0 {
		return compactionTailSelection{}, &compactionTailError{field: "max_segments"}
	}
	if maxTokens <= 0 {
		return compactionTailSelection{}, &compactionTailError{field: "max_tokens"}
	}

	messageTokens := make([]content.TokenCount, len(transcript))
	pairs, err := validateCompactionTailTranscript(transcript, derivedPrefix, messageTokens)
	if err != nil {
		return compactionTailSelection{}, err
	}
	segments := compactionTailSegments(transcript, derivedPrefix, messageTokens)
	if len(segments) == 0 {
		return compactionTailSelection{Head: cloneMessages(transcript)}, nil
	}

	startSegment := chooseCompactionTailSegments(segments, maxSegments, maxTokens)
	// A tool result can legally arrive after a user message folded into the
	// running turn. If the initial suffix cut would separate its call from its
	// result, move the cut to the call's segment. This can exceed either target,
	// but preserving a complete pair is stronger than silently dropping context.
	for {
		adjusted := startSegment
		for _, pair := range pairs {
			callSegment := compactionTailSegmentIndex(segments, pair.call)
			resultSegment := compactionTailSegmentIndex(segments, pair.result)
			if callSegment < adjusted && resultSegment >= adjusted {
				adjusted = callSegment
			}
		}
		if adjusted == startSegment {
			break
		}
		startSegment = adjusted
	}

	cut := segments[startSegment].start
	retained := cloneMessages(transcript[cut:])
	head := cloneMessages(transcript[:cut])
	return compactionTailSelection{Head: head, Retained: retained}, nil
}

func chooseCompactionTailSegments(
	segments []compactionTailSegment,
	maxSegments int,
	maxTokens content.TokenCount,
) (start int) {
	start = len(segments) - 1
	retainedTokens := segments[start].tokens
	retainedSegments := 1
	for start > 0 && retainedSegments < maxSegments {
		if retainedTokens > maxTokens {
			break
		}
		candidate := segments[start-1].tokens
		if candidate > maxTokens-retainedTokens {
			break
		}
		start--
		retainedSegments++
		retainedTokens += candidate
	}
	return start
}

func compactionTailSegmentIndex(segments []compactionTailSegment, messageIndex int) int {
	// Segments are ordered and cover every non-derived message. A message in
	// the derived prefix is never eligible for a retained suffix; callers have
	// already rejected a pair crossing that boundary during validation.
	for index := len(segments) - 1; index >= 0; index-- {
		if messageIndex >= segments[index].start {
			return index
		}
	}
	return -1
}

func compactionTailSegments(
	transcript content.AgenticMessages,
	derivedPrefix int,
	messageTokens []content.TokenCount,
) []compactionTailSegment {
	starts := make([]int, 0)
	for index := derivedPrefix; index < len(transcript); index++ {
		if _, ok := transcript[index].(*content.UserMessage); ok {
			starts = append(starts, index)
		}
	}
	segments := make([]compactionTailSegment, len(starts))
	for index, start := range starts {
		end := len(transcript)
		if index+1 < len(starts) {
			end = starts[index+1]
		}
		var tokens content.TokenCount
		for _, estimate := range messageTokens[start:end] {
			tokens += estimate
		}
		segments[index] = compactionTailSegment{start: start, end: end, tokens: tokens}
	}
	return segments
}

func validateCompactionTailTranscript(
	transcript content.AgenticMessages,
	derivedPrefix int,
	messageTokens []content.TokenCount,
) ([]compactionTailToolPair, error) {
	type toolCall struct {
		index int
		used  bool
	}
	calls := make(map[string]toolCall)
	pairs := make([]compactionTailToolPair, 0)
	seenNonDerivedUser := false

	for index, message := range transcript {
		blocks, ok := compactionTailMessageBlocks(message)
		if !ok {
			return nil, &compactionTailError{field: "message"}
		}
		if err := validateCompactionTailBlocks(blocks, 0); err != nil {
			return nil, err
		}
		raw, err := json.Marshal(message)
		if err != nil {
			return nil, &compactionTailError{field: "encoding"}
		}
		messageTokens[index] = content.TokenCount((len(raw) + 3) / 4)

		if index >= derivedPrefix {
			if user, isUser := message.(*content.UserMessage); !isUser || user == nil {
				if !seenNonDerivedUser {
					return nil, &compactionTailError{field: "segment_anchor"}
				}
			} else {
				seenNonDerivedUser = true
			}
		}

		switch typed := message.(type) {
		case *content.AIMessage:
			for _, block := range typed.Blocks {
				call, isCall := block.(*content.ToolUseBlock)
				if !isCall {
					continue
				}
				if call == nil || call.ID == "" {
					return nil, &compactionTailError{field: "tool_call"}
				}
				if _, duplicate := calls[call.ID]; duplicate {
					return nil, &compactionTailError{field: "tool_call"}
				}
				calls[call.ID] = toolCall{index: index}
			}
		case *content.ToolResultMessage:
			if typed.ToolUseID == "" {
				return nil, &compactionTailError{field: "tool_result"}
			}
			call, exists := calls[typed.ToolUseID]
			if !exists || call.used {
				return nil, &compactionTailError{field: "tool_result"}
			}
			call.used = true
			delete(calls, typed.ToolUseID)
			if call.index < derivedPrefix && index >= derivedPrefix {
				return nil, &compactionTailError{field: "derived_tool_pair"}
			}
			pairs = append(pairs, compactionTailToolPair{call: call.index, result: index})
		}
	}
	if len(calls) != 0 {
		return nil, &compactionTailError{field: "orphan_tool_call"}
	}
	return pairs, nil
}

func compactionTailMessageBlocks(message content.Conversation) ([]content.Block, bool) {
	switch typed := message.(type) {
	case *content.UserMessage:
		if typed == nil || typed.Role != content.RoleUser {
			return nil, false
		}
		return typed.Blocks, true
	case *content.AIMessage:
		if typed == nil || typed.Role != content.RoleAssistant {
			return nil, false
		}
		return typed.Blocks, true
	case *content.ToolResultMessage:
		if typed == nil || typed.Role != content.RoleTool {
			return nil, false
		}
		return typed.Blocks, true
	case *content.SystemMessage:
		if typed == nil || typed.Role != content.RoleSystem {
			return nil, false
		}
		return typed.Blocks, true
	default:
		return nil, false
	}
}

func validateCompactionTailBlocks(blocks []content.Block, depth int) error {
	if depth > compactionProjectionMaxDepth {
		return &compactionTailError{field: "block_depth"}
	}
	for _, block := range blocks {
		if block == nil {
			return &compactionTailError{field: "block"}
		}
		if nested, ok := block.(*content.ToolResultBlock); ok {
			if nested == nil {
				return &compactionTailError{field: "block"}
			}
			if err := validateCompactionTailBlocks(nested.Content, depth+1); err != nil {
				return err
			}
			continue
		}
		switch typed := block.(type) {
		case *content.TextBlock:
			if typed == nil {
				return &compactionTailError{field: "block"}
			}
		case *content.ImageBlock:
			if typed == nil {
				return &compactionTailError{field: "block"}
			}
		case *content.AudioBlock:
			if typed == nil {
				return &compactionTailError{field: "block"}
			}
		case *content.DocumentBlock:
			if typed == nil {
				return &compactionTailError{field: "block"}
			}
		case *content.ThinkingBlock:
			if typed == nil {
				return &compactionTailError{field: "block"}
			}
		case *content.ToolUseBlock:
			if typed == nil {
				return &compactionTailError{field: "block"}
			}
		default:
			return &compactionTailError{field: "block"}
		}
	}
	return nil
}
