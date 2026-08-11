package sessionruntime

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"reflect"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/internal/hustleruntime"
	"github.com/looprig/harness/internal/loopruntime"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	model "github.com/looprig/inference/model"
)

type compactionHustleRunner interface {
	RunAndFinalize(context.Context, hustle.Request, hustleruntime.ValidateResult, hustleruntime.Finalizer) error
}

type compactionAdapter struct {
	runner      compactionHustleRunner
	name        hustle.Name
	loopID      uuid.UUID
	outputBytes int
}

var _ loopruntime.Compactor = (*compactionAdapter)(nil)

type compactionAdapterField string

const (
	compactionAdapterFieldRunner     compactionAdapterField = "runner"
	compactionAdapterFieldDescriptor compactionAdapterField = "descriptor"
	compactionAdapterFieldLoopID     compactionAdapterField = "loop_id"
	compactionAdapterFieldFinalizer  compactionAdapterField = "finalizer"
)

type compactionAdapterError struct{ Field compactionAdapterField }

func (e *compactionAdapterError) Error() string {
	return "sessionruntime: invalid compaction adapter field " + string(e.Field)
}

func newCompactionAdapter(runner compactionHustleRunner, descriptor hustle.DefinitionDescriptor, loopID uuid.UUID) (*compactionAdapter, error) {
	if nilInterfaceValue(runner) {
		return nil, &compactionAdapterError{Field: compactionAdapterFieldRunner}
	}
	if err := descriptor.Validate(); err != nil || descriptor.ModelSource != hustle.ModelSourceCurrentLoop {
		return nil, &compactionAdapterError{Field: compactionAdapterFieldDescriptor}
	}
	if loopID.IsZero() {
		return nil, &compactionAdapterError{Field: compactionAdapterFieldLoopID}
	}
	return &compactionAdapter{
		runner: runner, name: descriptor.Name, loopID: loopID, outputBytes: descriptor.Limits.OutputBytes,
	}, nil
}

func (s *Session) compactorFor(bound loop.BoundDefinition, loopID uuid.UUID) (loopruntime.Compactor, error) {
	policy, configured := bound.CompactionPolicy()
	if !configured {
		return nil, nil
	}
	if s.hustleController == nil {
		return nil, &HustleConstructionError{
			Reason: HustleConstructionMissingCollaborator, Name: policy.Hustle, Field: "compaction_hustle_controller",
		}
	}
	for _, definition := range s.hustleDefinitions {
		descriptor := definition.Descriptor()
		if descriptor.Name != policy.Hustle {
			continue
		}
		if descriptor.ModelSource != hustle.ModelSourceCurrentLoop {
			return nil, &HustleConstructionError{
				Reason: HustleConstructionBindFailed, Name: policy.Hustle, Field: "compaction_hustle_model_source",
			}
		}
		adapter, err := newCompactionAdapter(s.hustleController, descriptor, loopID)
		if err != nil {
			return nil, &HustleConstructionError{
				Reason: HustleConstructionRuntimeFailed, Name: policy.Hustle, Field: "compaction_adapter", Cause: err,
			}
		}
		return adapter, nil
	}
	return nil, &HustleConstructionError{
		Reason: HustleConstructionMissingCollaborator, Name: policy.Hustle, Field: "compaction_hustle_definition",
	}
}

func nilInterfaceValue(value compactionHustleRunner) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	return reflected.Kind() == reflect.Pointer && reflected.IsNil()
}

func (a *compactionAdapter) CompactAndFinalize(ctx context.Context, input loop.CompactionInput, finalizer func(context.Context, loopruntime.CompactionOutcome) error) error {
	if finalizer == nil {
		return &compactionAdapterError{Field: compactionAdapterFieldFinalizer}
	}
	raw, err := marshalCompactionInput(input)
	if err != nil {
		return err
	}
	var validated *loop.CompactionOutput
	validate := func(_ context.Context, result hustle.Result) error {
		output, validationErr := validateCompactionResult(result, input, a.outputBytes)
		if validationErr == nil {
			validated = output
		}
		return validationErr
	}
	finish := func(finalizeCtx context.Context, outcome hustle.Outcome) error {
		return finalizer(finalizeCtx, translateCompactionOutcome(outcome, validated))
	}
	return a.runner.RunAndFinalize(ctx, hustle.Request{
		Name: a.name, Cause: identity.Cause{Coordinates: identity.Coordinates{LoopID: a.loopID}}, Input: raw,
	}, validate, finish)
}

func translateCompactionOutcome(outcome hustle.Outcome, validated *loop.CompactionOutput) loopruntime.CompactionOutcome {
	if outcome.Err != nil {
		return loopruntime.CompactionOutcome{Err: translateCompactionError(outcome.Err)}
	}
	if outcome.Result == nil || validated == nil {
		return loopruntime.CompactionOutcome{Err: &loop.InvalidSummaryError{Reason: loop.InvalidSummaryOutputShape}}
	}
	return loopruntime.CompactionOutcome{Value: validated}
}

func translateCompactionError(err error) error {
	var invalid *loop.InvalidSummaryError
	if errors.As(err, &invalid) {
		return invalid
	}
	var output *hustleruntime.OutputError
	if !errors.As(err, &output) || !output.Valid() || output.Cause != nil {
		return err
	}
	switch output.Reason {
	case hustleruntime.OutputFailureInvalidShape, hustleruntime.OutputFailureEmptyText:
		return &loop.InvalidSummaryError{Reason: loop.InvalidSummaryOutputShape, Cause: err}
	case hustleruntime.OutputFailureTooLarge:
		return &loop.InvalidSummaryError{Reason: loop.InvalidSummaryByteLimit, Cause: err}
	case hustleruntime.OutputFailureInvalidJSON:
		return &loop.InvalidSummaryError{Reason: loop.InvalidSummaryWire, Cause: err}
	default:
		return err
	}
}

type compactionBasisWire struct {
	Revision       event.ContextRevision `json:"revision"`
	ThroughEventID uuid.UUID             `json:"through_event_id"`
}

type compactionModelWire struct {
	Provider model.ProviderName `json:"provider"`
	Model    string             `json:"model"`
}

type compactionInputWire struct {
	Version            loop.CompactionWireVersion `json:"version"`
	Basis              compactionBasisWire        `json:"basis"`
	Model              compactionModelWire        `json:"model"`
	RequestFingerprint string                     `json:"request_fingerprint"`
	Transcript         []json.RawMessage          `json:"transcript"`
	MaxSummaryTokens   content.TokenCount         `json:"max_summary_tokens"`
}

type compactionInputDecodeWire struct {
	Version            *loop.CompactionWireVersion `json:"version"`
	Basis              *compactionBasisDecodeWire  `json:"basis"`
	Model              *compactionModelDecodeWire  `json:"model"`
	RequestFingerprint *string                     `json:"request_fingerprint"`
	Transcript         json.RawMessage             `json:"transcript"`
	MaxSummaryTokens   *content.TokenCount         `json:"max_summary_tokens"`
}

type compactionBasisDecodeWire struct {
	Revision       *event.ContextRevision `json:"revision"`
	ThroughEventID *string                `json:"through_event_id"`
}

type compactionModelDecodeWire struct {
	Provider *model.ProviderName `json:"provider"`
	Model    *string             `json:"model"`
}

func marshalCompactionInput(input loop.CompactionInput) (json.RawMessage, error) {
	if err := input.Validate(); err != nil {
		return nil, err
	}
	transcript := make([]json.RawMessage, len(input.Transcript))
	for index, message := range input.Transcript {
		raw, err := encodeCompactionMessage(message)
		if err != nil {
			return nil, &loop.CompactionInputError{Field: loop.CompactionInputFieldTranscript, Cause: err}
		}
		transcript[index] = raw
	}
	return json.Marshal(compactionInputWire{
		Version:            loop.CompactionWireV1,
		Basis:              compactionBasisWire{Revision: input.Basis.Revision, ThroughEventID: input.Basis.ThroughEventID},
		Model:              compactionModelWire{Provider: input.Model.Provider, Model: input.Model.Model},
		RequestFingerprint: hex.EncodeToString(input.RequestFingerprint[:]), Transcript: transcript,
		MaxSummaryTokens: input.MaxSummaryTokens,
	})
}

func unmarshalCompactionInput(raw []byte) (loop.CompactionInput, error) {
	var wire compactionInputDecodeWire
	if err := decodeStrictJSON(raw, &wire); err != nil {
		return loop.CompactionInput{}, &compactionInputWireError{Cause: err}
	}
	if wire.Version == nil || *wire.Version != loop.CompactionWireV1 || wire.Basis == nil || wire.Model == nil ||
		wire.RequestFingerprint == nil || wire.Transcript == nil || wire.MaxSummaryTokens == nil {
		return loop.CompactionInput{}, &compactionInputWireError{}
	}
	if wire.Basis.Revision == nil || wire.Basis.ThroughEventID == nil || wire.Model.Provider == nil || wire.Model.Model == nil {
		return loop.CompactionInput{}, &compactionInputWireError{}
	}
	fingerprint, err := decodeCompactionFingerprint(*wire.RequestFingerprint)
	if err != nil {
		return loop.CompactionInput{}, &loop.CompactionInputError{Field: loop.CompactionInputFieldRequestFingerprint, Cause: err}
	}
	transcript, err := decodeCompactionTranscript(wire.Transcript)
	if err != nil {
		return loop.CompactionInput{}, &loop.CompactionInputError{Field: loop.CompactionInputFieldTranscript, Cause: err}
	}
	throughEventID, err := decodeCompactionUUID(*wire.Basis.ThroughEventID)
	if err != nil {
		return loop.CompactionInput{}, &loop.CompactionInputError{Field: loop.CompactionInputFieldBasis, Cause: err}
	}
	input := loop.CompactionInput{
		Basis:              event.ContextBasis{Revision: *wire.Basis.Revision, ThroughEventID: throughEventID},
		Model:              model.ModelKey{Provider: *wire.Model.Provider, Model: *wire.Model.Model},
		RequestFingerprint: fingerprint, Transcript: transcript, MaxSummaryTokens: *wire.MaxSummaryTokens,
	}
	if err := input.Validate(); err != nil {
		return loop.CompactionInput{}, err
	}
	return input, nil
}

type compactionInputWireError struct{ Cause error }

func (*compactionInputWireError) Error() string {
	return "sessionruntime: invalid compaction input wire"
}
func (e *compactionInputWireError) Unwrap() error { return e.Cause }

type compactionOutputDecodeWire struct {
	Version            *loop.CompactionWireVersion `json:"version"`
	Basis              *compactionBasisDecodeWire  `json:"basis"`
	Model              *compactionModelDecodeWire  `json:"model"`
	RequestFingerprint *string                     `json:"request_fingerprint"`
	Summary            *string                     `json:"summary"`
}

func validateCompactionResult(result hustle.Result, input loop.CompactionInput, outputBytes int) (*loop.CompactionOutput, error) {
	if outputBytes <= 0 || len(result.Output) > outputBytes {
		return nil, &loop.InvalidSummaryError{Reason: loop.InvalidSummaryByteLimit}
	}
	if err := validateCompactionUsage(result.Usage, input.MaxSummaryTokens); err != nil {
		return nil, err
	}
	wire, throughEventID, fingerprint, err := decodeCompactionOutput(result.Output)
	if err != nil {
		return nil, err
	}
	if err := validateCompactionOutputIdentity(wire, throughEventID, fingerprint, input); err != nil {
		return nil, err
	}
	summary, err := loopruntime.ParseCompactionSummaryXML([]byte(*wire.Summary))
	if err != nil {
		return nil, err
	}
	output := &loop.CompactionOutput{Basis: input.Basis, Model: input.Model, RequestFingerprint: input.RequestFingerprint, Summary: summary}
	if err := output.Validate(); err != nil {
		return nil, err
	}
	return output, nil
}

func validateCompactionUsage(usage *content.Usage, maximum content.TokenCount) error {
	if usage == nil || usage.OutputTokens == 0 {
		return &loop.InvalidSummaryError{Reason: loop.InvalidSummaryTokenUsage}
	}
	if err := usage.Validate(); err != nil {
		return &loop.InvalidSummaryError{Reason: loop.InvalidSummaryTokenUsage, Cause: err}
	}
	if usage.OutputTokens > maximum {
		return &loop.InvalidSummaryError{Reason: loop.InvalidSummaryTokenLimit}
	}
	return nil
}

func decodeCompactionOutput(raw json.RawMessage) (compactionOutputDecodeWire, uuid.UUID, [32]byte, error) {
	var wire compactionOutputDecodeWire
	if err := decodeStrictJSON(raw, &wire); err != nil {
		return wire, uuid.UUID{}, [32]byte{}, &loop.InvalidSummaryError{Reason: loop.InvalidSummaryWire, Cause: err}
	}
	if wire.Version == nil || *wire.Version != loop.CompactionWireV1 || wire.Basis == nil || wire.Model == nil ||
		wire.RequestFingerprint == nil || wire.Summary == nil || wire.Basis.Revision == nil ||
		wire.Basis.ThroughEventID == nil || wire.Model.Provider == nil || wire.Model.Model == nil {
		return wire, uuid.UUID{}, [32]byte{}, &loop.InvalidSummaryError{Reason: loop.InvalidSummaryWire}
	}
	throughEventID, err := decodeCompactionUUID(*wire.Basis.ThroughEventID)
	if err != nil {
		return wire, uuid.UUID{}, [32]byte{}, &loop.InvalidSummaryError{Reason: loop.InvalidSummaryWire, Cause: err}
	}
	fingerprint, err := decodeCompactionFingerprint(*wire.RequestFingerprint)
	if err != nil {
		return wire, uuid.UUID{}, [32]byte{}, &loop.InvalidSummaryError{Reason: loop.InvalidSummaryWire, Cause: err}
	}
	return wire, throughEventID, fingerprint, nil
}

func validateCompactionOutputIdentity(wire compactionOutputDecodeWire, throughEventID uuid.UUID, fingerprint [32]byte, input loop.CompactionInput) error {
	if *wire.Basis.Revision != input.Basis.Revision || throughEventID != input.Basis.ThroughEventID ||
		*wire.Model.Provider != input.Model.Provider || *wire.Model.Model != input.Model.Model || fingerprint != input.RequestFingerprint {
		return &loop.InvalidSummaryError{Reason: loop.InvalidSummaryIdentity}
	}
	return nil
}

func decodeCompactionUUID(encoded string) (uuid.UUID, error) {
	decoded, err := uuid.Parse(encoded)
	if err != nil || decoded.String() != encoded {
		return uuid.UUID{}, &compactionUUIDError{}
	}
	return decoded, nil
}

type compactionUUIDError struct{}

func (*compactionUUIDError) Error() string {
	return "sessionruntime: noncanonical compaction UUID"
}

func decodeStrictJSON(raw []byte, destination interface{}) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return &compactionJSONError{}
		}
		return err
	}
	return nil
}

type compactionJSONError struct{}

func (*compactionJSONError) Error() string { return "sessionruntime: invalid compaction JSON" }

func decodeCompactionFingerprint(encoded string) ([32]byte, error) {
	var fingerprint [32]byte
	if len(encoded) != hex.EncodedLen(len(fingerprint)) {
		return fingerprint, &compactionFingerprintError{}
	}
	for _, character := range encoded {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return fingerprint, &compactionFingerprintError{}
		}
	}
	decoded, err := hex.DecodeString(encoded)
	if err != nil {
		return fingerprint, &compactionFingerprintError{}
	}
	copy(fingerprint[:], decoded)
	return fingerprint, nil
}

type compactionFingerprintError struct{}

func (*compactionFingerprintError) Error() string {
	return "sessionruntime: invalid compaction fingerprint"
}

func decodeCompactionTranscript(raw json.RawMessage) (content.AgenticMessages, error) {
	var messages []json.RawMessage
	if err := decodeStrictJSON(raw, &messages); err != nil || messages == nil {
		return nil, &compactionTranscriptWireError{Cause: err}
	}
	result := make(content.AgenticMessages, len(messages))
	for index, message := range messages {
		decoded, err := decodeCompactionMessage(message)
		if err != nil {
			return nil, &compactionTranscriptWireError{Cause: err}
		}
		result[index] = decoded
	}
	return result, nil
}

type compactionMessageRoleWire struct {
	Role *content.Role `json:"role"`
}

func decodeCompactionMessage(raw json.RawMessage) (content.Conversation, error) {
	var role compactionMessageRoleWire
	if err := json.Unmarshal(raw, &role); err != nil || role.Role == nil {
		return nil, &compactionMessageWireError{}
	}
	switch *role.Role {
	case content.RoleUser:
		return decodeCompactionUserMessage(raw)
	case content.RoleSystem:
		return decodeCompactionSystemMessage(raw)
	case content.RoleAssistant:
		return decodeCompactionAIMessage(raw)
	case content.RoleTool:
		return decodeCompactionToolMessage(raw)
	default:
		return nil, &compactionMessageWireError{}
	}
}

func decodeCompactionUserMessage(raw json.RawMessage) (content.Conversation, error) {
	message, err := decodeCompactionBasicMessageWire(raw, content.RoleUser)
	if err != nil {
		return nil, err
	}
	return &content.UserMessage{Message: message}, nil
}

func decodeCompactionSystemMessage(raw json.RawMessage) (content.Conversation, error) {
	message, err := decodeCompactionBasicMessageWire(raw, content.RoleSystem)
	if err != nil {
		return nil, err
	}
	return &content.SystemMessage{Message: message}, nil
}

func decodeCompactionBasicMessageWire(raw json.RawMessage, role content.Role) (content.Message, error) {
	var wire compactionBasicMessageDecodeWire
	if err := decodeStrictJSON(raw, &wire); err != nil {
		return content.Message{}, err
	}
	return decodeCompactionMessageFields(wire.Role, wire.Blocks, role)
}

func decodeCompactionAIMessage(raw json.RawMessage) (content.Conversation, error) {
	var wire compactionAIMessageDecodeWire
	if err := decodeStrictJSON(raw, &wire); err != nil {
		return nil, err
	}
	message, err := decodeCompactionMessageFields(wire.Role, wire.Blocks, content.RoleAssistant)
	if err != nil {
		return nil, err
	}
	usage, err := decodeCompactionHistoricalUsage(wire.Usage)
	if err != nil {
		return nil, err
	}
	return &content.AIMessage{Message: message, Usage: usage}, nil
}

func decodeCompactionHistoricalUsage(wire *compactionUsageWire) (*content.Usage, error) {
	if wire == nil {
		return nil, nil
	}
	usage := &content.Usage{
		InputTokens: wire.InputTokens, OutputTokens: wire.OutputTokens,
		CacheReadTokens: wire.CacheReadTokens, CacheCreationTokens: wire.CacheCreationTokens,
		ReasoningTokens: wire.ReasoningTokens,
	}
	if err := usage.Validate(); err != nil {
		return nil, err
	}
	return usage, nil
}

func decodeCompactionToolMessage(raw json.RawMessage) (content.Conversation, error) {
	var wire compactionToolMessageDecodeWire
	if err := decodeStrictJSON(raw, &wire); err != nil {
		return nil, err
	}
	message, err := decodeCompactionMessageFields(wire.Role, wire.Blocks, content.RoleTool)
	if err != nil || wire.ToolUseID == nil || wire.IsError == nil {
		return nil, &compactionMessageWireError{}
	}
	return &content.ToolResultMessage{Message: message, ToolUseID: *wire.ToolUseID, IsError: *wire.IsError}, nil
}

type compactionBasicMessageWire struct {
	Role   content.Role      `json:"role"`
	Blocks []json.RawMessage `json:"blocks"`
}

type compactionBasicMessageDecodeWire struct {
	Role   *content.Role   `json:"role"`
	Blocks json.RawMessage `json:"blocks"`
}

type compactionAIMessageWire struct {
	Role   content.Role         `json:"role"`
	Blocks []json.RawMessage    `json:"blocks"`
	Usage  *compactionUsageWire `json:"usage"`
}

type compactionAIMessageDecodeWire struct {
	Role   *content.Role        `json:"role"`
	Blocks json.RawMessage      `json:"blocks"`
	Usage  *compactionUsageWire `json:"usage"`
}

type compactionToolMessageWire struct {
	Role      content.Role      `json:"role"`
	Blocks    []json.RawMessage `json:"blocks"`
	ToolUseID string            `json:"tool_use_id"`
	IsError   bool              `json:"is_error"`
}

type compactionToolMessageDecodeWire struct {
	Role      *content.Role   `json:"role"`
	Blocks    json.RawMessage `json:"blocks"`
	ToolUseID *string         `json:"tool_use_id"`
	IsError   *bool           `json:"is_error"`
}

type compactionUsageWire struct {
	InputTokens         content.TokenCount `json:"input_tokens"`
	OutputTokens        content.TokenCount `json:"output_tokens"`
	CacheReadTokens     content.TokenCount `json:"cache_read_tokens"`
	CacheCreationTokens content.TokenCount `json:"cache_creation_tokens"`
	ReasoningTokens     content.TokenCount `json:"reasoning_tokens"`
}

func encodeCompactionMessage(message content.Conversation) (json.RawMessage, error) {
	blocks, err := encodeCompactionBlocks(messageBlocks(message), 0)
	if err != nil {
		return nil, err
	}
	switch typed := message.(type) {
	case *content.UserMessage:
		return json.Marshal(compactionBasicMessageWire{Role: typed.Role, Blocks: blocks})
	case *content.SystemMessage:
		return json.Marshal(compactionBasicMessageWire{Role: typed.Role, Blocks: blocks})
	case *content.AIMessage:
		var usage *compactionUsageWire
		if typed.Usage != nil {
			usage = &compactionUsageWire{
				InputTokens: typed.Usage.InputTokens, OutputTokens: typed.Usage.OutputTokens,
				CacheReadTokens: typed.Usage.CacheReadTokens, CacheCreationTokens: typed.Usage.CacheCreationTokens,
				ReasoningTokens: typed.Usage.ReasoningTokens,
			}
		}
		return json.Marshal(compactionAIMessageWire{Role: typed.Role, Blocks: blocks, Usage: usage})
	case *content.ToolResultMessage:
		return json.Marshal(compactionToolMessageWire{Role: typed.Role, Blocks: blocks, ToolUseID: typed.ToolUseID, IsError: typed.IsError})
	default:
		return nil, &compactionMessageWireError{}
	}
}

func messageBlocks(message content.Conversation) []content.Block {
	switch typed := message.(type) {
	case *content.UserMessage:
		return typed.Blocks
	case *content.SystemMessage:
		return typed.Blocks
	case *content.AIMessage:
		return typed.Blocks
	case *content.ToolResultMessage:
		return typed.Blocks
	default:
		return nil
	}
}

func decodeCompactionMessageFields(encodedRole *content.Role, rawBlocks json.RawMessage, role content.Role) (content.Message, error) {
	if encodedRole == nil || *encodedRole != role || rawBlocks == nil {
		return content.Message{}, &compactionMessageWireError{}
	}
	blocks, err := decodeCompactionBlocks(rawBlocks, 0)
	if err != nil {
		return content.Message{}, err
	}
	return content.Message{Role: role, Blocks: blocks}, nil
}

const maxCompactionBlockDepth = 128

type compactionBlockProbe struct {
	Type *content.BlockType `json:"type"`
}

func encodeCompactionBlocks(blocks []content.Block, depth int) ([]json.RawMessage, error) {
	if depth > maxCompactionBlockDepth {
		return nil, &compactionMessageWireError{}
	}
	result := make([]json.RawMessage, len(blocks))
	for index, block := range blocks {
		raw, err := encodeCompactionBlock(block, depth)
		if err != nil {
			return nil, err
		}
		result[index] = raw
	}
	return result, nil
}

func decodeCompactionBlocks(raw json.RawMessage, depth int) ([]content.Block, error) {
	if depth > maxCompactionBlockDepth {
		return nil, &compactionMessageWireError{}
	}
	var encoded []json.RawMessage
	if err := decodeStrictJSON(raw, &encoded); err != nil || encoded == nil {
		return nil, &compactionMessageWireError{}
	}
	blocks := make([]content.Block, len(encoded))
	for index, block := range encoded {
		decoded, err := decodeCompactionBlock(block, depth)
		if err != nil {
			return nil, err
		}
		blocks[index] = decoded
	}
	return blocks, nil
}

type compactionTextBlockWire struct {
	Type content.BlockType `json:"type"`
	Text string            `json:"text"`
}

type compactionTextBlockDecodeWire struct {
	Type *content.BlockType `json:"type"`
	Text *string            `json:"text"`
}

type compactionImageSourceWire struct {
	URL  string `json:"url"`
	Data []byte `json:"data"`
}

type compactionImageBlockWire struct {
	Type      content.BlockType         `json:"type"`
	MediaType content.MediaType         `json:"media_type"`
	Source    compactionImageSourceWire `json:"source"`
}

type compactionImageBlockDecodeWire struct {
	Type      *content.BlockType               `json:"type"`
	MediaType *content.MediaType               `json:"media_type"`
	Source    *compactionImageSourceDecodeWire `json:"source"`
}

type compactionImageSourceDecodeWire struct {
	URL  *string         `json:"url"`
	Data json.RawMessage `json:"data"`
}

type compactionAudioBlockWire struct {
	Type      content.BlockType `json:"type"`
	MediaType content.MediaType `json:"media_type"`
	Data      []byte            `json:"data"`
}

type compactionAudioBlockDecodeWire struct {
	Type      *content.BlockType `json:"type"`
	MediaType *content.MediaType `json:"media_type"`
	Data      json.RawMessage    `json:"data"`
}

type compactionDocumentBlockWire struct {
	Type      content.BlockType `json:"type"`
	MediaType content.MediaType `json:"media_type"`
	Name      string            `json:"name"`
	Data      []byte            `json:"data"`
	Text      string            `json:"text"`
}

type compactionDocumentBlockDecodeWire struct {
	Type      *content.BlockType `json:"type"`
	MediaType *content.MediaType `json:"media_type"`
	Name      *string            `json:"name"`
	Data      json.RawMessage    `json:"data"`
	Text      *string            `json:"text"`
}

type compactionThinkingBlockWire struct {
	Type      content.BlockType `json:"type"`
	Thinking  string            `json:"thinking"`
	Signature string            `json:"signature"`
}

type compactionThinkingBlockDecodeWire struct {
	Type      *content.BlockType `json:"type"`
	Thinking  *string            `json:"thinking"`
	Signature *string            `json:"signature"`
}

type compactionToolUseBlockWire struct {
	Type  content.BlockType `json:"type"`
	ID    string            `json:"id"`
	Name  string            `json:"name"`
	Input json.RawMessage   `json:"input"`
}

type compactionToolUseBlockDecodeWire struct {
	Type  *content.BlockType `json:"type"`
	ID    *string            `json:"id"`
	Name  *string            `json:"name"`
	Input json.RawMessage    `json:"input"`
}

type compactionToolResultBlockWire struct {
	Type      content.BlockType `json:"type"`
	ToolUseID string            `json:"tool_use_id"`
	Content   []json.RawMessage `json:"content"`
	IsError   bool              `json:"is_error"`
}

type compactionToolResultBlockDecodeWire struct {
	Type      *content.BlockType `json:"type"`
	ToolUseID *string            `json:"tool_use_id"`
	Content   json.RawMessage    `json:"content"`
	IsError   *bool              `json:"is_error"`
}

func encodeCompactionBlock(block content.Block, depth int) (json.RawMessage, error) {
	switch typed := block.(type) {
	case *content.TextBlock:
		return json.Marshal(compactionTextBlockWire{Type: content.TypeText, Text: typed.Text})
	case *content.ImageBlock:
		return json.Marshal(compactionImageBlockWire{Type: content.TypeImage, MediaType: typed.MediaType, Source: compactionImageSourceWire{URL: typed.Source.URL, Data: typed.Source.Data}})
	case *content.AudioBlock:
		return json.Marshal(compactionAudioBlockWire{Type: content.TypeAudio, MediaType: typed.MediaType, Data: typed.Data})
	case *content.DocumentBlock:
		return json.Marshal(compactionDocumentBlockWire{Type: content.TypeDocument, MediaType: typed.MediaType, Name: typed.Name, Data: typed.Data, Text: typed.Text})
	case *content.ThinkingBlock:
		return json.Marshal(compactionThinkingBlockWire{Type: content.TypeThinking, Thinking: typed.Thinking, Signature: typed.Signature})
	case *content.ToolUseBlock:
		return json.Marshal(compactionToolUseBlockWire{Type: content.TypeToolUse, ID: typed.ID, Name: typed.Name, Input: typed.Input})
	case *content.ToolResultBlock:
		nested, err := encodeCompactionBlocks(typed.Content, depth+1)
		if err != nil {
			return nil, err
		}
		return json.Marshal(compactionToolResultBlockWire{Type: content.TypeToolResult, ToolUseID: typed.ToolUseID, Content: nested, IsError: typed.IsError})
	default:
		return nil, &compactionMessageWireError{}
	}
}

func decodeCompactionBlock(raw json.RawMessage, depth int) (content.Block, error) {
	var probe compactionBlockProbe
	if err := json.Unmarshal(raw, &probe); err != nil || probe.Type == nil {
		return nil, &compactionMessageWireError{}
	}
	switch *probe.Type {
	case content.TypeText:
		return decodeCompactionTextBlock(raw)
	case content.TypeImage:
		return decodeCompactionImageBlock(raw)
	case content.TypeAudio:
		return decodeCompactionAudioBlock(raw)
	case content.TypeDocument:
		return decodeCompactionDocumentBlock(raw)
	case content.TypeThinking:
		return decodeCompactionThinkingBlock(raw)
	case content.TypeToolUse:
		return decodeCompactionToolUseBlock(raw)
	case content.TypeToolResult:
		return decodeCompactionToolResultBlock(raw, depth)
	default:
		return nil, &compactionMessageWireError{}
	}
}

func decodeCompactionTextBlock(raw json.RawMessage) (content.Block, error) {
	var wire compactionTextBlockDecodeWire
	if err := decodeStrictJSON(raw, &wire); err != nil || wire.Type == nil || wire.Text == nil {
		return nil, &compactionMessageWireError{}
	}
	return &content.TextBlock{Text: *wire.Text}, nil
}

func decodeCompactionThinkingBlock(raw json.RawMessage) (content.Block, error) {
	var wire compactionThinkingBlockDecodeWire
	if err := decodeStrictJSON(raw, &wire); err != nil || wire.Thinking == nil || wire.Signature == nil {
		return nil, &compactionMessageWireError{}
	}
	return &content.ThinkingBlock{Thinking: *wire.Thinking, Signature: *wire.Signature}, nil
}

func decodeCompactionToolUseBlock(raw json.RawMessage) (content.Block, error) {
	var wire compactionToolUseBlockDecodeWire
	if err := decodeStrictJSON(raw, &wire); err != nil || wire.ID == nil || wire.Name == nil || wire.Input == nil {
		return nil, &compactionMessageWireError{}
	}
	return &content.ToolUseBlock{ID: *wire.ID, Name: *wire.Name, Input: append(json.RawMessage(nil), wire.Input...)}, nil
}

func decodeCompactionToolResultBlock(raw json.RawMessage, depth int) (content.Block, error) {
	var wire compactionToolResultBlockDecodeWire
	if err := decodeStrictJSON(raw, &wire); err != nil || wire.ToolUseID == nil || wire.Content == nil || wire.IsError == nil {
		return nil, &compactionMessageWireError{}
	}
	nested, err := decodeCompactionBlocks(wire.Content, depth+1)
	if err != nil {
		return nil, err
	}
	return &content.ToolResultBlock{ToolUseID: *wire.ToolUseID, Content: nested, IsError: *wire.IsError}, nil
}

func decodeCompactionImageBlock(raw json.RawMessage) (content.Block, error) {
	var wire compactionImageBlockDecodeWire
	if err := decodeStrictJSON(raw, &wire); err != nil || wire.MediaType == nil || wire.Source == nil || wire.Source.URL == nil || wire.Source.Data == nil {
		return nil, &compactionMessageWireError{}
	}
	data, err := decodeCompactionBytes(wire.Source.Data)
	if err != nil {
		return nil, err
	}
	return &content.ImageBlock{MediaType: *wire.MediaType, Source: content.ImageSource{URL: *wire.Source.URL, Data: data}}, nil
}

func decodeCompactionAudioBlock(raw json.RawMessage) (content.Block, error) {
	var wire compactionAudioBlockDecodeWire
	if err := decodeStrictJSON(raw, &wire); err != nil || wire.MediaType == nil || wire.Data == nil {
		return nil, &compactionMessageWireError{}
	}
	data, err := decodeCompactionBytes(wire.Data)
	if err != nil {
		return nil, err
	}
	return &content.AudioBlock{MediaType: *wire.MediaType, Data: data}, nil
}

func decodeCompactionDocumentBlock(raw json.RawMessage) (content.Block, error) {
	var wire compactionDocumentBlockDecodeWire
	if err := decodeStrictJSON(raw, &wire); err != nil || wire.MediaType == nil || wire.Name == nil || wire.Data == nil || wire.Text == nil {
		return nil, &compactionMessageWireError{}
	}
	data, err := decodeCompactionBytes(wire.Data)
	if err != nil {
		return nil, err
	}
	return &content.DocumentBlock{MediaType: *wire.MediaType, Name: *wire.Name, Data: data, Text: *wire.Text}, nil
}

func decodeCompactionBytes(raw json.RawMessage) ([]byte, error) {
	var data []byte
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, &compactionMessageWireError{}
	}
	return data, nil
}

type compactionTranscriptWireError struct{ Cause error }

func (*compactionTranscriptWireError) Error() string {
	return "sessionruntime: invalid compaction transcript wire"
}
func (e *compactionTranscriptWireError) Unwrap() error { return e.Cause }

type compactionMessageWireError struct{}

func (*compactionMessageWireError) Error() string {
	return "sessionruntime: invalid compaction message wire"
}
