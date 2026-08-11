package backend

import (
	"log"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/foreignloops/driver"
	"github.com/looprig/harness/pkg/event"
)

// mapper translates one turn's provider-neutral driver events into Harness
// events and owns the tool-use correlation table for that turn.
type mapper struct {
	turnIndex event.TurnIndex
	idGen     func() (uuid.UUID, error)
	toolUse   map[string]uuid.UUID
}

func newMapper(turnIndex event.TurnIndex, idGen func() (uuid.UUID, error)) *mapper {
	return &mapper{
		turnIndex: turnIndex,
		idGen:     idGen,
		toolUse:   make(map[string]uuid.UUID),
	}
}

func (m *mapper) toEvents(input driver.Event) ([]event.Event, error) {
	switch input.Kind {
	case driver.KindTextDelta:
		return one(event.TokenDelta{TurnIndex: m.turnIndex, Chunk: &content.TextChunk{Text: input.Text}}), nil
	case driver.KindThinkingDelta:
		return one(event.TokenDelta{TurnIndex: m.turnIndex, Chunk: &content.ThinkingChunk{Thinking: input.Text}}), nil
	case driver.KindToolUse:
		return m.toolStarted(input)
	case driver.KindToolResult:
		return m.toolCompleted(input), nil
	case driver.KindStepComplete:
		return m.stepDone(input), nil
	case driver.KindTerminalOK:
		return one(event.TurnDone{TurnIndex: m.turnIndex, Message: input.Message}), nil
	case driver.KindTerminalError, driver.KindModelFacingError:
		return one(event.TurnFailed{TurnIndex: m.turnIndex, Err: resultError(input)}), nil
	case driver.KindInit:
		return nil, nil
	default:
		return nil, nil
	}
}

func (m *mapper) toolStarted(input driver.Event) ([]event.Event, error) {
	id, err := m.idGen()
	if err != nil {
		return nil, err
	}
	m.toolUse[input.ToolUseID] = id
	return one(event.ToolCallStarted{ToolExecutionID: id, ToolName: input.ToolName}), nil
}

func (m *mapper) toolCompleted(input driver.Event) []event.Event {
	id, ok := m.toolUse[input.ToolUseID]
	if !ok {
		log.Printf("foreignloop: orphan tool_result for tool_use id %q; soft-skipping", input.ToolUseID)
		return nil
	}
	return one(event.ToolCallCompleted{
		ToolExecutionID: id,
		IsError:         input.IsError,
		ResultPreview:   input.ResultPreview,
	})
}

func (m *mapper) stepDone(input driver.Event) []event.Event {
	if input.Message == nil {
		return nil
	}
	return one(event.StepDone{Messages: content.AgenticMessages{input.Message}})
}

func one(input event.Event) []event.Event { return []event.Event{input} }
