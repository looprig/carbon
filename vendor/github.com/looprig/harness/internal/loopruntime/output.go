package loopruntime

import (
	"bytes"
	"unicode/utf8"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/inference"
	model "github.com/looprig/inference/model"
	stream "github.com/looprig/inference/stream"
)

type outputStrategy uint8

const (
	outputStrategyNone outputStrategy = iota
	outputStrategyNative
	outputStrategyTerminalTool
)

// turnOutputPlan is the immutable request shape selected once at turn start.
// apply returns fresh schema/tool storage so neither provider clients nor
// measurement hooks can mutate a later continuation request.
type turnOutputPlan struct {
	strategy   outputStrategy
	output     *inference.OutputSchema
	tools      []inference.Tool
	toolChoice inference.ToolChoice
}

func resolveTurnOutput(current model.Model, configured *inference.OutputSchema, ordinary []inference.Tool) (turnOutputPlan, error) {
	plan := turnOutputPlan{strategy: outputStrategyNone, tools: cloneInferenceTools(ordinary)}
	if err := validateReservedOutputTools(ordinary); err != nil {
		return turnOutputPlan{}, err
	}
	if configured == nil {
		return plan, nil
	}
	if err := current.Validate(); err != nil {
		return turnOutputPlan{}, err
	}
	if err := inference.ValidateOutputSchema(*configured); err != nil {
		return turnOutputPlan{}, err
	}
	if err := validateOrdinaryOutputTools(ordinary); err != nil {
		return turnOutputPlan{}, err
	}

	output := configured.Clone()
	if current.Caps.StructuredOutput && (len(ordinary) == 0 || current.Caps.StructuredOutputWithTools) {
		plan.strategy = outputStrategyNative
		plan.output = &output
		return plan, nil
	}
	if !current.Caps.Tools {
		return turnOutputPlan{}, &inference.StructuredOutputUnsupportedError{Model: structuredOutputModelDiagnostic(current.Name)}
	}

	plan.strategy = outputStrategyTerminalTool
	plan.toolChoice = inference.ToolChoiceRequired
	plan.tools = append(plan.tools, terminalOutputTool(output))
	return plan, nil
}

// terminalOutputTool builds the request-only control definition used by the
// portability fallback. Empty descriptions are valid for model-facing tools, so
// they remain empty rather than acquiring Harness-authored policy text.
func terminalOutputTool(output inference.OutputSchema) inference.Tool {
	return inference.Tool{
		Name:        inference.StructuredOutputToolName,
		Description: output.Description,
		Schema:      append([]byte(nil), output.Schema...),
	}
}

func structuredOutputModelDiagnostic(name string) string {
	if !utf8.ValidString(name) {
		return "invalid-utf8"
	}
	if len(name) <= inference.MaxStructuredOutputDiagnosticBytes {
		return name
	}
	end := inference.MaxStructuredOutputDiagnosticBytes
	for end > 0 && !utf8.RuneStart(name[end]) {
		end--
	}
	return name[:end]
}

func validateOrdinaryOutputTools(tools []inference.Tool) error {
	seen := make(map[string]struct{}, len(tools))
	for _, definition := range tools {
		if _, exists := seen[definition.Name]; exists {
			return &inference.StructuredOutputConflictError{Feature: "duplicate_tool_name"}
		}
		seen[definition.Name] = struct{}{}
	}
	return validateReservedOutputTools(tools)
}

func validateReservedOutputTools(tools []inference.Tool) error {
	for _, definition := range tools {
		if loop.IsReservedToolName(definition.Name) {
			return &inference.StructuredOutputConflictError{Feature: "reserved_structured_output_tool"}
		}
	}
	return nil
}

func (p turnOutputPlan) apply(request inference.Request) inference.Request {
	request.Tools = cloneInferenceTools(p.tools)
	request.ToolChoice = p.toolChoice
	request.Output = cloneOutputSchema(p.output)
	return request
}

func cloneOutputSchema(output *inference.OutputSchema) *inference.OutputSchema {
	if output == nil {
		return nil
	}
	clone := output.Clone()
	return &clone
}

func cloneInferenceTools(tools []inference.Tool) []inference.Tool {
	if tools == nil {
		return nil
	}
	clone := make([]inference.Tool, len(tools))
	for i := range tools {
		clone[i] = tools[i]
		clone[i].Schema = append([]byte(nil), tools[i].Schema...)
	}
	return clone
}

// validateNativeStep applies the native-output finish contract before the turn
// actor observes any part of the current step. A complete ordinary tool batch
// is a continuation; a no-tool response must be a valid text representation and
// is canonicalized to one compact JSON TextBlock. Reserved terminal-tool frames
// belong only to the fallback strategy and fail closed here.
func validateNativeStep(
	message *content.AIMessage,
	calls []content.ToolUseBlock,
	result *stream.StreamResult,
) (*content.AIMessage, bool, error) {
	if result == nil {
		return nil, false, &inference.StructuredOutputFinishError{Reason: inference.StructuredOutputFinishReasonOther}
	}

	response := &inference.Response{Message: message, FinishReason: result.FinishReason}
	if len(calls) > 0 {
		if result.FinishReason != stream.FinishReasonToolUse {
			_, err := inference.StructuredResult(response)
			if err != nil {
				return nil, false, err
			}
			return nil, false, &inference.StructuredOutputFinishError{Reason: inference.StructuredOutputFinishReasonOther}
		}
		for _, call := range calls {
			if loop.IsReservedToolName(call.Name) {
				return nil, false, &inference.StructuredOutputConflictError{Feature: "native_terminal_tool"}
			}
			if !validToolCall(call) {
				return nil, false, &inference.StructuredOutputConflictError{Feature: "incomplete_tool_call"}
			}
		}
		return nil, false, nil
	}

	raw, err := inference.StructuredResult(response)
	if err != nil {
		return nil, false, err
	}
	canonical := &content.AIMessage{Message: content.Message{
		Role:   content.RoleAssistant,
		Blocks: []content.Block{&content.TextBlock{Text: string(raw)}},
	}}
	canonical.Usage = cloneUsage(message.Usage)
	return canonical, true, nil
}

// validateTerminalStep applies the reserved-tool fallback contract before the
// current step can affect usage, tool limits, permissions, execution, durable
// history, or candidate measurement. Ordinary-only tool batches remain normal
// continuations. A batch containing the reserved control frame is final only
// when it is the sole complete call under an authoritative tool_use finish.
func validateTerminalStep(
	message *content.AIMessage,
	calls []content.ToolUseBlock,
	result *stream.StreamResult,
) (*content.AIMessage, bool, error) {
	if result == nil {
		return nil, false, &inference.StructuredOutputFinishError{Reason: inference.StructuredOutputFinishReasonOther}
	}
	if err := validateRawToolFrame(message, calls); err != nil {
		return nil, false, err
	}

	terminalCalls := 0
	for _, call := range calls {
		if call.Name == inference.StructuredOutputToolName {
			terminalCalls++
		}
	}

	if terminalCalls == 0 {
		if len(calls) == 0 {
			return nil, false, terminalOutputRequiredError(result.FinishReason)
		}
		if result.FinishReason != stream.FinishReasonToolUse {
			return nil, false, terminalOutputFinishError(result.FinishReason)
		}
		for _, call := range calls {
			if !validToolCall(call) {
				return nil, false, &inference.StructuredOutputConflictError{Feature: "incomplete_tool_call"}
			}
		}
		return nil, false, nil
	}

	if result.FinishReason != stream.FinishReasonToolUse {
		return nil, false, terminalOutputFinishError(result.FinishReason)
	}
	if len(calls) != 1 || terminalCalls != 1 {
		// Delegate representation classification to the shared extractor. It
		// rejects duplicate terminal calls, mixed action/control batches, and
		// semantic text without retaining any raw block content in the error.
		_, err := inference.StructuredResult(&inference.Response{Message: message, FinishReason: result.FinishReason})
		if err != nil {
			return nil, false, err
		}
		return nil, false, &inference.StructuredOutputConflictError{Feature: "ambiguous_terminal_output"}
	}
	// The reserved terminal frame is intercepted and never paired with a tool
	// result, so it does not require the provider-generated ID ordinary calls do.
	if calls[0].Name == "" {
		return nil, false, &inference.StructuredOutputConflictError{Feature: "incomplete_tool_call"}
	}

	raw, err := inference.StructuredResult(&inference.Response{Message: message, FinishReason: result.FinishReason})
	if err != nil {
		return nil, false, err
	}
	canonical := &content.AIMessage{Message: content.Message{
		Role:   content.RoleAssistant,
		Blocks: []content.Block{&content.TextBlock{Text: string(raw)}},
	}}
	canonical.Usage = cloneUsage(message.Usage)
	return canonical, true, nil
}

// validateRawToolFrame verifies the accumulator's materialized message and
// executable call view describe the same complete tool frames. The stream
// accumulator normally guarantees this; checking at the interception boundary
// keeps future block sources and typed-nil values fail-closed before execution.
func validateRawToolFrame(message *content.AIMessage, calls []content.ToolUseBlock) error {
	if message == nil || message.Role != content.RoleAssistant {
		return &inference.StructuredOutputConflictError{Feature: "inconsistent_tool_frame"}
	}
	callIndex := 0
	for _, block := range message.Blocks {
		switch typed := block.(type) {
		case *content.TextBlock:
			if typed == nil {
				return &inference.StructuredOutputConflictError{Feature: "inconsistent_tool_frame"}
			}
		case *content.ThinkingBlock:
			if typed == nil {
				return &inference.StructuredOutputConflictError{Feature: "inconsistent_tool_frame"}
			}
		case *content.ToolUseBlock:
			if typed == nil || callIndex >= len(calls) {
				return &inference.StructuredOutputConflictError{Feature: "inconsistent_tool_frame"}
			}
			call := calls[callIndex]
			if typed.ID != call.ID || typed.Name != call.Name || !bytes.Equal(typed.Input, call.Input) {
				return &inference.StructuredOutputConflictError{Feature: "inconsistent_tool_frame"}
			}
			callIndex++
		default:
			return &inference.StructuredOutputConflictError{Feature: "inconsistent_tool_frame"}
		}
	}
	if callIndex != len(calls) {
		return &inference.StructuredOutputConflictError{Feature: "inconsistent_tool_frame"}
	}
	return nil
}

func terminalOutputRequiredError(reason stream.FinishReason) error {
	switch reason {
	case stream.FinishReasonLength, stream.FinishReasonContentFilter:
		return &inference.StructuredOutputFinishError{Reason: reason}
	default:
		return &inference.StructuredOutputConflictError{Feature: "terminal_output_required"}
	}
}

func terminalOutputFinishError(reason stream.FinishReason) error {
	switch reason {
	case stream.FinishReasonStop, stream.FinishReasonLength, stream.FinishReasonContentFilter:
		return &inference.StructuredOutputFinishError{Reason: reason}
	default:
		return &inference.StructuredOutputFinishError{Reason: inference.StructuredOutputFinishReasonOther}
	}
}
