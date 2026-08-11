package backend

import (
	"context"
	"fmt"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/event"
)

// snapshotReq and snapshotResult are the actor query handshake.
type snapshotReq struct {
	reply chan snapshotResult
}

type snapshotResult struct {
	msgs      content.AgenticMessages
	turnIndex event.TurnIndex
}

func cloneMessages(messages content.AgenticMessages) content.AgenticMessages {
	if messages == nil {
		return nil
	}
	cloned := make(content.AgenticMessages, len(messages))
	for index, message := range messages {
		cloned[index] = cloneConversation(message)
	}
	return cloned
}

func cloneConversation(message content.Conversation) content.Conversation {
	if message == nil {
		return nil
	}
	switch typed := message.(type) {
	case *content.UserMessage:
		if typed == nil {
			return (*content.UserMessage)(nil)
		}
		return &content.UserMessage{Message: cloneMessage(typed.Message)}
	case *content.AIMessage:
		if typed == nil {
			return (*content.AIMessage)(nil)
		}
		cloned := &content.AIMessage{Message: cloneMessage(typed.Message)}
		if typed.Usage != nil {
			usage := *typed.Usage
			cloned.Usage = &usage
		}
		return cloned
	case *content.SystemMessage:
		if typed == nil {
			return (*content.SystemMessage)(nil)
		}
		return &content.SystemMessage{Message: cloneMessage(typed.Message)}
	case *content.ToolResultMessage:
		if typed == nil {
			return (*content.ToolResultMessage)(nil)
		}
		return &content.ToolResultMessage{
			Message:   cloneMessage(typed.Message),
			ToolUseID: typed.ToolUseID,
			IsError:   typed.IsError,
		}
	default:
		panic(fmt.Sprintf("foreignloop: unsupported content conversation %T", message))
	}
}

func cloneMessage(message content.Message) content.Message {
	return content.Message{Role: message.Role, Blocks: cloneBlocks(message.Blocks)}
}

func cloneBlocks(blocks []content.Block) []content.Block {
	if blocks == nil {
		return nil
	}
	cloned := make([]content.Block, len(blocks))
	for index, block := range blocks {
		cloned[index] = cloneBlock(block)
	}
	return cloned
}

func cloneBlock(block content.Block) content.Block {
	if block == nil {
		return nil
	}
	switch typed := block.(type) {
	case *content.TextBlock:
		if typed == nil {
			return (*content.TextBlock)(nil)
		}
		cloned := *typed
		return &cloned
	case *content.ImageBlock:
		if typed == nil {
			return (*content.ImageBlock)(nil)
		}
		cloned := *typed
		cloned.Source.Data = cloneBytes(typed.Source.Data)
		return &cloned
	case *content.AudioBlock:
		if typed == nil {
			return (*content.AudioBlock)(nil)
		}
		cloned := *typed
		cloned.Data = cloneBytes(typed.Data)
		return &cloned
	case *content.DocumentBlock:
		if typed == nil {
			return (*content.DocumentBlock)(nil)
		}
		cloned := *typed
		cloned.Data = cloneBytes(typed.Data)
		return &cloned
	case *content.ThinkingBlock:
		if typed == nil {
			return (*content.ThinkingBlock)(nil)
		}
		cloned := *typed
		return &cloned
	case *content.ToolUseBlock:
		if typed == nil {
			return (*content.ToolUseBlock)(nil)
		}
		cloned := *typed
		cloned.Input = cloneBytes(typed.Input)
		return &cloned
	case *content.ToolResultBlock:
		if typed == nil {
			return (*content.ToolResultBlock)(nil)
		}
		cloned := *typed
		cloned.Content = cloneBlocks(typed.Content)
		return &cloned
	default:
		panic(fmt.Sprintf("foreignloop: unsupported content block %T", block))
	}
}

func cloneBytes[Bytes ~[]byte](input Bytes) Bytes {
	if input == nil {
		return nil
	}
	cloned := make(Bytes, len(input))
	copy(cloned, input)
	return cloned
}

// Snapshot requests a consistent defensive view from the backend actor.
func (l *Loop) Snapshot(ctx context.Context) (content.AgenticMessages, event.TurnIndex, error) {
	reply := make(chan snapshotResult, 1)
	select {
	case l.snapshots <- snapshotReq{reply: reply}:
	case <-l.Done:
		return nil, 0, &SnapshotError{Reason: SnapshotLoopExited}
	case <-ctx.Done():
		return nil, 0, &SnapshotError{Reason: SnapshotContextDone, Cause: ctx.Err()}
	}
	select {
	case result := <-reply:
		return result.msgs, result.turnIndex, nil
	case <-l.Done:
		return nil, 0, &SnapshotError{Reason: SnapshotLoopExited}
	case <-ctx.Done():
		return nil, 0, &SnapshotError{Reason: SnapshotContextDone, Cause: ctx.Err()}
	}
}
