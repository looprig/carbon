package loopruntime

import (
	"github.com/looprig/core/content"
	"github.com/looprig/inference"
)

// cloneInferenceRequest gives an external request collaborator independent
// ownership of every reference-backed field while preserving scalar request
// intent. The original request remains safe for the current provider call.
func cloneInferenceRequest(request inference.Request) inference.Request {
	cloned := request
	cloned.Model = request.Model.Clone()
	if request.Messages == nil {
		cloned.Messages = nil
	} else {
		cloned.Messages = cloneMessages(request.Messages)
	}
	cloned.Tools = cloneInferenceTools(request.Tools)
	cloned.Output = cloneOutputSchema(request.Output)
	if request.Override != nil {
		override := request.Override.Clone()
		cloned.Override = &override
	}
	return cloned
}

// cloneMessages gives a conversation graph one owner. It recursively copies
// messages, blocks, usage, raw JSON, and binary payloads so mutation cannot cross
// runtime boundaries through content's pointer-backed sealed unions.
func cloneMessages(messages content.AgenticMessages) content.AgenticMessages {
	cloned := make(content.AgenticMessages, len(messages))
	for i, message := range messages {
		cloned[i] = cloneConversation(message)
	}
	return cloned
}

func cloneConversation(message content.Conversation) content.Conversation {
	switch typed := message.(type) {
	case *content.UserMessage:
		return cloneUserMessage(typed)
	case *content.AIMessage:
		return cloneAIMessage(typed)
	case *content.SystemMessage:
		return cloneSystemMessage(typed)
	case *content.ToolResultMessage:
		return cloneToolResultMessage(typed)
	default:
		return nil
	}
}

func cloneUserMessage(message *content.UserMessage) *content.UserMessage {
	if message == nil {
		return nil
	}
	return &content.UserMessage{Message: cloneMessage(message.Message)}
}

func cloneAIMessage(message *content.AIMessage) *content.AIMessage {
	if message == nil {
		return nil
	}
	cloned := &content.AIMessage{Message: cloneMessage(message.Message)}
	if message.Usage != nil {
		usage := *message.Usage
		cloned.Usage = &usage
	}
	return cloned
}

func cloneSystemMessage(message *content.SystemMessage) *content.SystemMessage {
	if message == nil {
		return nil
	}
	return &content.SystemMessage{Message: cloneMessage(message.Message)}
}

func cloneToolResultMessage(message *content.ToolResultMessage) *content.ToolResultMessage {
	if message == nil {
		return nil
	}
	return &content.ToolResultMessage{
		Message:   cloneMessage(message.Message),
		ToolUseID: message.ToolUseID,
		IsError:   message.IsError,
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
	for i, block := range blocks {
		cloned[i] = cloneBlock(block)
	}
	return cloned
}

func cloneBlock(block content.Block) content.Block {
	switch typed := block.(type) {
	case *content.TextBlock:
		if typed == nil {
			return (*content.TextBlock)(nil)
		}
		return &content.TextBlock{Text: typed.Text}
	case *content.ImageBlock:
		if typed == nil {
			return (*content.ImageBlock)(nil)
		}
		return &content.ImageBlock{
			MediaType: typed.MediaType,
			Source: content.ImageSource{
				URL:  typed.Source.URL,
				Data: cloneBytes(typed.Source.Data),
			},
		}
	case *content.AudioBlock:
		if typed == nil {
			return (*content.AudioBlock)(nil)
		}
		return &content.AudioBlock{MediaType: typed.MediaType, Data: cloneBytes(typed.Data)}
	case *content.DocumentBlock:
		if typed == nil {
			return (*content.DocumentBlock)(nil)
		}
		return &content.DocumentBlock{
			MediaType: typed.MediaType,
			Name:      typed.Name,
			Data:      cloneBytes(typed.Data),
			Text:      typed.Text,
		}
	case *content.ThinkingBlock:
		if typed == nil {
			return (*content.ThinkingBlock)(nil)
		}
		return &content.ThinkingBlock{Thinking: typed.Thinking, Signature: typed.Signature}
	case *content.ToolUseBlock:
		if typed == nil {
			return (*content.ToolUseBlock)(nil)
		}
		return &content.ToolUseBlock{ID: typed.ID, Name: typed.Name, Input: cloneBytes(typed.Input)}
	case *content.ToolResultBlock:
		if typed == nil {
			return (*content.ToolResultBlock)(nil)
		}
		return &content.ToolResultBlock{
			ToolUseID: typed.ToolUseID,
			Content:   cloneBlocks(typed.Content),
			IsError:   typed.IsError,
		}
	default:
		return nil
	}
}

func cloneBytes(value []byte) []byte {
	if value == nil {
		return nil
	}
	return append([]byte(nil), value...)
}
