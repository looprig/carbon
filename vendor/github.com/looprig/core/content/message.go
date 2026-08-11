package content

import (
	"encoding/json"
	"reflect"
)

// Role identifies the author of a message in a conversation thread.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleSystem    Role = "system"
	RoleTool      Role = "tool"
)

// Message is the base type for all conversation turns: a role and an ordered
// sequence of content blocks. Typed message structs embed this so the role and
// blocks are always accessible via the embedded field.
type Message struct {
	Role   Role
	Blocks []Block
}

// UserMessage is a turn authored by the human user.
type UserMessage struct{ Message }

// AIMessage is a turn authored by the AI model.
type AIMessage struct {
	Message
	Usage *Usage
}

// SystemMessage carries a system prompt that shapes model behavior.
type SystemMessage struct{ Message }

// ToolResultMessage carries the result of a tool invocation back to the model.
// ToolUseID ties this result to the specific ToolUseBlock that requested it.
// IsError is true when the tool reported an error result; it is the message-level
// error signal the loop carries from the result and the display layer reads.
type ToolResultMessage struct {
	Message
	ToolUseID string
	IsError   bool
}

// Conversation is a sealed interface: only message types defined in this
// package can participate in a conversation thread. The unexported marker
// method prevents external packages from satisfying the interface accidentally,
// keeping the discriminated union closed.
type Conversation interface{ isMessage() }

func (*UserMessage) isMessage()       {}
func (*AIMessage) isMessage()         {}
func (*SystemMessage) isMessage()     {}
func (*ToolResultMessage) isMessage() {}

// AgenticMessages is an ordered conversation thread. A nil or empty slice is a
// valid zero value representing an empty thread.
type AgenticMessages []Conversation

// messageJSON is the wire form of Message. Blocks go through the slice codec so
// nested blocks stay tagged.
type messageJSON struct {
	Role   Role            `json:"role"`
	Blocks json.RawMessage `json:"blocks,omitempty"`
}

func (m Message) MarshalJSON() ([]byte, error) {
	var blocks json.RawMessage
	if len(m.Blocks) > 0 {
		b, err := MarshalBlocks(m.Blocks)
		if err != nil {
			return nil, err
		}
		blocks = b
	}
	return json.Marshal(messageJSON{Role: m.Role, Blocks: blocks})
}

func (m *Message) UnmarshalJSON(data []byte) error {
	var j messageJSON
	if err := json.Unmarshal(data, &j); err != nil {
		return err
	}
	m.Role = j.Role
	if len(j.Blocks) > 0 {
		blocks, err := UnmarshalBlocks(j.Blocks)
		if err != nil {
			return err
		}
		m.Blocks = blocks
	}
	return nil
}

// aiMessageJSON is the wire form of AIMessage. AIMessage defines its own codec
// pair so the promoted Message methods do not silently drop Usage.
type aiMessageJSON struct {
	Role   Role            `json:"role"`
	Blocks json.RawMessage `json:"blocks,omitempty"`
	Usage  *usageJSON      `json:"usage,omitempty"`
}

// usageJSON keeps the package's existing exported-field wire names while
// omitting zero values. A non-nil zero Usage therefore remains distinguishable
// from nil by encoding as an empty object.
type usageJSON struct {
	InputTokens         TokenCount `json:",omitempty"`
	OutputTokens        TokenCount `json:",omitempty"`
	CacheReadTokens     TokenCount `json:",omitempty"`
	CacheCreationTokens TokenCount `json:",omitempty"`
	ReasoningTokens     TokenCount `json:",omitempty"`
}

func (m AIMessage) MarshalJSON() ([]byte, error) {
	var blocks json.RawMessage
	if len(m.Blocks) > 0 {
		b, err := MarshalBlocks(m.Blocks)
		if err != nil {
			return nil, err
		}
		blocks = b
	}

	var usage *usageJSON
	if m.Usage != nil {
		if err := m.Usage.Validate(); err != nil {
			return nil, err
		}
		u := usageJSON(*m.Usage)
		usage = &u
	}

	return json.Marshal(aiMessageJSON{Role: m.Role, Blocks: blocks, Usage: usage})
}

func (m *AIMessage) UnmarshalJSON(data []byte) error {
	if m == nil {
		return &json.InvalidUnmarshalError{Type: reflect.TypeOf(m)}
	}
	*m = AIMessage{}

	var j aiMessageJSON
	if err := json.Unmarshal(data, &j); err != nil {
		return err
	}

	var blocks []Block
	if len(j.Blocks) > 0 {
		decoded, err := UnmarshalBlocks(j.Blocks)
		if err != nil {
			return err
		}
		blocks = decoded
	}

	var usage *Usage
	if j.Usage != nil {
		u := Usage(*j.Usage)
		if err := u.Validate(); err != nil {
			return err
		}
		usage = &u
	}

	*m = AIMessage{
		Message: Message{Role: j.Role, Blocks: blocks},
		Usage:   usage,
	}
	return nil
}

// toolResultMessageJSON is the wire form of ToolResultMessage. ToolResultMessage
// defines its own codec pair so the promoted Message methods do not silently drop
// ToolUseID. IsError uses omitempty, so a false value is omitted on the wire and an
// absent field decodes back to false — a lossless round-trip for both values,
// matching the sibling ToolResultBlock codec (block_json.go).
type toolResultMessageJSON struct {
	Role      Role            `json:"role"`
	Blocks    json.RawMessage `json:"blocks,omitempty"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error,omitempty"`
}

func (m ToolResultMessage) MarshalJSON() ([]byte, error) {
	var blocks json.RawMessage
	if len(m.Blocks) > 0 {
		b, err := MarshalBlocks(m.Blocks)
		if err != nil {
			return nil, err
		}
		blocks = b
	}
	return json.Marshal(toolResultMessageJSON{Role: m.Role, Blocks: blocks, ToolUseID: m.ToolUseID, IsError: m.IsError})
}

func (m *ToolResultMessage) UnmarshalJSON(data []byte) error {
	var j toolResultMessageJSON
	if err := json.Unmarshal(data, &j); err != nil {
		return err
	}
	m.Role = j.Role
	m.ToolUseID = j.ToolUseID
	m.IsError = j.IsError
	if len(j.Blocks) > 0 {
		blocks, err := UnmarshalBlocks(j.Blocks)
		if err != nil {
			return err
		}
		m.Blocks = blocks
	}
	return nil
}
