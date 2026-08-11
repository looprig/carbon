package hustleruntime

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"

	"github.com/looprig/core/content"
	"github.com/looprig/inference"
	"github.com/looprig/inference/stream"
)

const (
	maxProviderResponseBlocks = 4096
	maxProviderThinkingBytes  = 1 << 20
	maxProviderCallIDBytes    = 1024
	maxProviderToolNameBytes  = 64
	maxProviderResponseBytes  = 20 << 20
	maxEvidenceJSONDepth      = 64
	maxEvidenceJSONMembers    = 65536
	maxEvidenceJSONTokens     = 262144
)

type toolResponseLimits struct {
	outputBytes      int
	maxCallsPerRound int
}

// classifiedToolResponse is a sealed internal union. A successful
// classification is exactly one terminal result or one non-empty ordered call
// list.
type classifiedToolResponse interface {
	isClassifiedToolResponse()
}

type terminalToolResponse struct {
	output json.RawMessage
}

func (terminalToolResponse) isClassifiedToolResponse() {}

type evidenceToolCall struct {
	id    string
	name  string
	input json.RawMessage
}

type evidenceToolResponse struct {
	calls []evidenceToolCall
}

func (evidenceToolResponse) isClassifiedToolResponse() {}

// classifyToolResponse exhaustively classifies one complete provider response.
// knownTools is the exact evidence catalog exposed for the invocation.
func classifyToolResponse(
	response *inference.Response,
	knownTools map[string]struct{},
	limits toolResponseLimits,
) (classifiedToolResponse, error) {
	if err := preflightToolResponse(response, limits); err != nil {
		return nil, err
	}

	shape, reason := inspectToolResponseBlocks(response.Message.Blocks)
	if reason.Valid() {
		return nil, toolResponseError(reason)
	}
	if reason := validateToolResponseFinish(response.FinishReason, shape); reason.Valid() {
		return nil, toolResponseError(reason)
	}

	if shape.textSeen || shape.terminalCount == 1 {
		rawBytes, overflow := terminalResponseBytes(shape)
		if limits.outputBytes <= 0 || overflow || rawBytes > limits.outputBytes {
			return nil, toolResponseError(ToolResponseFailureTooLarge)
		}
		output, err := inference.StructuredMessageResult(response.Message)
		if err != nil {
			return nil, toolResponseError(ToolResponseFailureInvalidTerminal)
		}
		return terminalToolResponse{output: append(json.RawMessage(nil), output...)}, nil
	}

	calls, reason := classifyEvidenceCalls(shape.ordinary, knownTools)
	if reason.Valid() {
		return nil, toolResponseError(reason)
	}
	return evidenceToolResponse{calls: calls}, nil
}

func preflightToolResponse(response *inference.Response, limits toolResponseLimits) error {
	if response == nil || response.Message == nil || response.Message.Role != content.RoleAssistant {
		return toolResponseError(ToolResponseFailureInvalidShape)
	}
	blocks := response.Message.Blocks
	if len(blocks) > maxProviderResponseBlocks {
		return toolResponseError(ToolResponseFailureTooLarge)
	}

	ordinaryCalls := 0
	for _, block := range blocks {
		switch typed := block.(type) {
		case *content.TextBlock:
			if typed == nil {
				return toolResponseError(ToolResponseFailureInvalidShape)
			}
		case *content.ThinkingBlock:
			if typed == nil {
				return toolResponseError(ToolResponseFailureInvalidShape)
			}
		case *content.ToolUseBlock:
			if typed == nil {
				return toolResponseError(ToolResponseFailureInvalidShape)
			}
			if typed.Name != inference.StructuredOutputToolName {
				ordinaryCalls++
			}
		case *content.ImageBlock:
			return toolResponseError(ToolResponseFailureInvalidShape)
		case *content.AudioBlock:
			return toolResponseError(ToolResponseFailureInvalidShape)
		case *content.DocumentBlock:
			return toolResponseError(ToolResponseFailureInvalidShape)
		case *content.ToolResultBlock:
			return toolResponseError(ToolResponseFailureInvalidShape)
		default:
			return toolResponseError(ToolResponseFailureInvalidShape)
		}
	}
	if ordinaryCalls > limits.maxCallsPerRound {
		return evidenceError(EvidenceFailureCallsPerRoundExceeded)
	}

	totalBytes, thinkingBytes, argumentBytes := 0, 0, 0
	for _, block := range blocks {
		switch typed := block.(type) {
		case *content.TextBlock:
			if limits.outputBytes <= 0 || len(typed.Text) > limits.outputBytes ||
				!addWithinLimit(&totalBytes, len(typed.Text), maxProviderResponseBytes) {
				return toolResponseError(ToolResponseFailureTooLarge)
			}
		case *content.ThinkingBlock:
			if !addWithinLimit(&thinkingBytes, len(typed.Thinking), maxProviderThinkingBytes) ||
				!addWithinLimit(&thinkingBytes, len(typed.Signature), maxProviderThinkingBytes) ||
				!addWithinLimit(&totalBytes, len(typed.Thinking), maxProviderResponseBytes) ||
				!addWithinLimit(&totalBytes, len(typed.Signature), maxProviderResponseBytes) {
				return toolResponseError(ToolResponseFailureTooLarge)
			}
		case *content.ToolUseBlock:
			if len(typed.ID) > maxProviderCallIDBytes ||
				len(typed.Name) > maxProviderToolNameBytes ||
				limits.outputBytes <= 0 ||
				len(typed.Input) > limits.outputBytes ||
				!addWithinLimit(&argumentBytes, len(typed.Input), limits.outputBytes) ||
				!addWithinLimit(&totalBytes, len(typed.ID), maxProviderResponseBytes) ||
				!addWithinLimit(&totalBytes, len(typed.Name), maxProviderResponseBytes) ||
				!addWithinLimit(&totalBytes, len(typed.Input), maxProviderResponseBytes) {
				return toolResponseError(ToolResponseFailureTooLarge)
			}
		}
	}
	return nil
}

func addWithinLimit(total *int, increment, limit int) bool {
	if total == nil || increment < 0 || limit < 0 || *total < 0 || increment > limit-*total {
		return false
	}
	*total += increment
	return true
}

type inspectedToolResponse struct {
	textSeen      bool
	textBytes     int
	textOverflow  bool
	terminal      *content.ToolUseBlock
	terminalCount int
	ordinary      []*content.ToolUseBlock
}

func inspectToolResponseBlocks(blocks []content.Block) (inspectedToolResponse, ToolResponseFailureReason) {
	var shape inspectedToolResponse
	for _, block := range blocks {
		switch typed := block.(type) {
		case *content.TextBlock:
			if typed == nil {
				return inspectedToolResponse{}, ToolResponseFailureInvalidShape
			}
			shape.textSeen = true
			if len(typed.Text) > math.MaxInt-shape.textBytes {
				shape.textOverflow = true
			} else {
				shape.textBytes += len(typed.Text)
			}
		case *content.ThinkingBlock:
			if typed == nil {
				return inspectedToolResponse{}, ToolResponseFailureInvalidShape
			}
		case *content.ToolUseBlock:
			if typed == nil {
				return inspectedToolResponse{}, ToolResponseFailureInvalidShape
			}
			if typed.Name == inference.StructuredOutputToolName {
				shape.terminalCount++
				shape.terminal = typed
			} else {
				shape.ordinary = append(shape.ordinary, typed)
			}
		case *content.ImageBlock:
			return inspectedToolResponse{}, ToolResponseFailureInvalidShape
		case *content.AudioBlock:
			return inspectedToolResponse{}, ToolResponseFailureInvalidShape
		case *content.DocumentBlock:
			return inspectedToolResponse{}, ToolResponseFailureInvalidShape
		case *content.ToolResultBlock:
			return inspectedToolResponse{}, ToolResponseFailureInvalidShape
		default:
			return inspectedToolResponse{}, ToolResponseFailureInvalidShape
		}
	}

	if shape.terminalCount > 1 {
		return inspectedToolResponse{}, ToolResponseFailureDuplicateTerminal
	}
	if shape.textSeen && (shape.terminalCount > 0 || len(shape.ordinary) > 0) ||
		shape.terminalCount > 0 && len(shape.ordinary) > 0 {
		return inspectedToolResponse{}, ToolResponseFailureMixed
	}
	if !shape.textSeen && shape.terminalCount == 0 && len(shape.ordinary) == 0 {
		return inspectedToolResponse{}, ToolResponseFailureInvalidShape
	}
	return shape, ""
}

func validateToolResponseFinish(
	finish stream.FinishReason,
	shape inspectedToolResponse,
) ToolResponseFailureReason {
	switch finish {
	case stream.FinishReasonUnknown:
		return ""
	case stream.FinishReasonStop:
		if shape.textSeen {
			return ""
		}
	case stream.FinishReasonToolUse:
		if !shape.textSeen && (shape.terminalCount == 1 || len(shape.ordinary) > 0) {
			return ""
		}
	case stream.FinishReasonLength, stream.FinishReasonContentFilter:
	default:
	}
	return ToolResponseFailureFinishReason
}

func terminalResponseBytes(shape inspectedToolResponse) (int, bool) {
	if shape.textSeen {
		return shape.textBytes, shape.textOverflow
	}
	return len(shape.terminal.Input), false
}

func classifyEvidenceCalls(
	blocks []*content.ToolUseBlock,
	knownTools map[string]struct{},
) ([]evidenceToolCall, ToolResponseFailureReason) {
	seenIDs := make(map[string]struct{}, len(blocks))
	calls := make([]evidenceToolCall, 0, len(blocks))
	for _, block := range blocks {
		if block.ID == "" {
			return nil, ToolResponseFailureMissingCallID
		}
		if _, duplicate := seenIDs[block.ID]; duplicate {
			return nil, ToolResponseFailureDuplicateCallID
		}
		seenIDs[block.ID] = struct{}{}
		if _, known := knownTools[block.Name]; !known {
			return nil, ToolResponseFailureUnknownTool
		}
		if !validEvidenceArguments(block.Input) {
			return nil, ToolResponseFailureMalformedArguments
		}
		calls = append(calls, evidenceToolCall{
			id:    block.ID,
			name:  block.Name,
			input: append(json.RawMessage(nil), block.Input...),
		})
	}
	return calls, ""
}

func validEvidenceArguments(input json.RawMessage) bool {
	trimmed := bytes.TrimSpace(input)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	tokenCount := 0
	nextToken := func() (json.Token, error) {
		if tokenCount >= maxEvidenceJSONTokens {
			return nil, errEvidenceJSONLimit
		}
		token, err := decoder.Token()
		if err == nil {
			tokenCount++
		}
		return token, err
	}
	token, err := nextToken()
	if err != nil || token != json.Delim('{') {
		return false
	}

	type frame struct {
		kind       json.Delim
		wantKey    bool
		objectKeys map[string]struct{}
	}
	stack := []frame{{kind: json.Delim('{'), wantKey: true, objectKeys: make(map[string]struct{})}}
	memberCount := 0
	for len(stack) > 0 {
		token, err := nextToken()
		if err != nil {
			return false
		}
		top := &stack[len(stack)-1]
		if top.kind == json.Delim('{') && top.wantKey {
			if token == json.Delim('}') {
				stack = stack[:len(stack)-1]
				continue
			}
			name, ok := token.(string)
			if !ok || memberCount >= maxEvidenceJSONMembers {
				return false
			}
			if _, duplicate := top.objectKeys[name]; duplicate {
				return false
			}
			top.objectKeys[name] = struct{}{}
			memberCount++
			top.wantKey = false
			continue
		}

		if top.kind == json.Delim('[') && token == json.Delim(']') {
			stack = stack[:len(stack)-1]
			continue
		}
		if delim, ok := token.(json.Delim); ok {
			if delim != json.Delim('{') && delim != json.Delim('[') ||
				len(stack) >= maxEvidenceJSONDepth {
				return false
			}
			if top.kind == json.Delim('{') {
				top.wantKey = true
			}
			child := frame{kind: delim}
			if delim == json.Delim('{') {
				child.wantKey = true
				child.objectKeys = make(map[string]struct{})
			}
			stack = append(stack, child)
			continue
		}
		if top.kind == json.Delim('{') {
			top.wantKey = true
		}
		switch token.(type) {
		case nil, bool, string, json.Number:
		default:
			return false
		}
	}
	_, err = decoder.Token()
	return err == io.EOF
}

var errEvidenceJSONLimit = errors.New("evidence JSON structural limit")

func toolResponseError(reason ToolResponseFailureReason) *ToolResponseError {
	return &ToolResponseError{Reason: reason}
}
