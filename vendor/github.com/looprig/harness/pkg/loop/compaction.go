package loop

import (
	"encoding/json"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/event"
	model "github.com/looprig/inference/model"
)

// CompactionWireVersion identifies the concrete adapter JSON contract.
type CompactionWireVersion uint8

const (
	CompactionWireVersionUnknown CompactionWireVersion = iota
	CompactionWireV1
)

// Valid reports whether the wire version is implemented.
func (v CompactionWireVersion) Valid() bool { return v == CompactionWireV1 }

// CompactionInput is the exact context identity and transcript summarized by one
// compaction hustle invocation.
type CompactionInput struct {
	Basis              event.ContextBasis
	Model              model.ModelKey
	RequestFingerprint [32]byte
	Transcript         content.AgenticMessages
	MaxSummaryTokens   content.TokenCount
}

// CompactionOutput is a validated summary tied to the exact input identity.
type CompactionOutput struct {
	Basis              event.ContextBasis
	Model              model.ModelKey
	RequestFingerprint [32]byte
	Summary            *content.UserMessage
}

// CompactionInputField identifies an invalid domain input component.
type CompactionInputField string

const (
	CompactionInputFieldBasis              CompactionInputField = "basis"
	CompactionInputFieldModel              CompactionInputField = "model"
	CompactionInputFieldRequestFingerprint CompactionInputField = "request_fingerprint"
	CompactionInputFieldTranscript         CompactionInputField = "transcript"
	CompactionInputFieldMaxSummaryTokens   CompactionInputField = "max_summary_tokens"
)

// CompactionInputError reports malformed typed input without rendering transcript
// or prompt bytes.
type CompactionInputError struct {
	Field CompactionInputField
	Cause error
}

func (e *CompactionInputError) Error() string {
	return "loop: invalid compaction input field " + string(e.Field)
}

func (e *CompactionInputError) Unwrap() error { return e.Cause }

// Validate checks the typed domain boundary before the adapter serializes it.
func (i CompactionInput) Validate() error {
	if i.Basis.Revision == 0 || i.Basis.ThroughEventID.IsZero() {
		return &CompactionInputError{Field: CompactionInputFieldBasis}
	}
	if err := i.Model.Validate(); err != nil {
		return &CompactionInputError{Field: CompactionInputFieldModel, Cause: err}
	}
	if i.RequestFingerprint == ([32]byte{}) {
		return &CompactionInputError{Field: CompactionInputFieldRequestFingerprint}
	}
	if err := validateCompactionTranscript(i.Transcript); err != nil {
		return &CompactionInputError{Field: CompactionInputFieldTranscript, Cause: err}
	}
	if i.MaxSummaryTokens == 0 {
		return &CompactionInputError{Field: CompactionInputFieldMaxSummaryTokens}
	}
	return nil
}

func validateCompactionTranscript(transcript content.AgenticMessages) error {
	if len(transcript) == 0 {
		return &compactionTranscriptError{}
	}
	for _, message := range transcript {
		blocks, ok := compactionMessageBlocks(message)
		if !ok {
			return &compactionTranscriptError{}
		}
		if err := validateCompactionBlocks(blocks, 0); err != nil {
			return &compactionTranscriptError{Cause: err}
		}
		if _, err := json.Marshal(message); err != nil {
			return &compactionTranscriptError{Cause: err}
		}
	}
	return nil
}

type compactionTranscriptError struct{ Cause error }

func (*compactionTranscriptError) Error() string   { return "loop: invalid compaction transcript" }
func (e *compactionTranscriptError) Unwrap() error { return e.Cause }

func compactionMessageBlocks(message content.Conversation) ([]content.Block, bool) {
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
	case *content.SystemMessage:
		if typed == nil || typed.Role != content.RoleSystem {
			return nil, false
		}
		return typed.Blocks, true
	case *content.ToolResultMessage:
		if typed == nil || typed.Role != content.RoleTool {
			return nil, false
		}
		return typed.Blocks, true
	default:
		return nil, false
	}
}

const maxCompactionBlockDepth = 128

func validateCompactionBlocks(blocks []content.Block, depth int) error {
	if depth > maxCompactionBlockDepth {
		return &compactionTranscriptBlockError{}
	}
	for _, block := range blocks {
		if typed, ok := block.(*content.ToolResultBlock); ok {
			if typed == nil {
				return &compactionTranscriptBlockError{}
			}
			if err := validateCompactionBlocks(typed.Content, depth+1); err != nil {
				return err
			}
			continue
		}
		if !validCompactionLeafBlock(block) {
			return &compactionTranscriptBlockError{}
		}
	}
	return nil
}

func validCompactionLeafBlock(block content.Block) bool {
	switch typed := block.(type) {
	case *content.TextBlock:
		return typed != nil
	case *content.ImageBlock:
		return typed != nil
	case *content.AudioBlock:
		return typed != nil
	case *content.DocumentBlock:
		return typed != nil
	case *content.ThinkingBlock:
		return typed != nil
	case *content.ToolUseBlock:
		return typed != nil
	default:
		return false
	}
}

type compactionTranscriptBlockError struct{}

func (*compactionTranscriptBlockError) Error() string {
	return "loop: invalid compaction transcript block"
}

// Validate checks the output's identity and single-user-text replacement shape.
// The strict XML grammar is enforced by the internal adapter before constructing
// this value.
func (o CompactionOutput) Validate() error {
	if o.Basis.Revision == 0 || o.Basis.ThroughEventID.IsZero() {
		return &InvalidSummaryError{Reason: InvalidSummaryIdentity}
	}
	if err := o.Model.Validate(); err != nil {
		return &InvalidSummaryError{Reason: InvalidSummaryIdentity, Cause: err}
	}
	if o.RequestFingerprint == ([32]byte{}) {
		return &InvalidSummaryError{Reason: InvalidSummaryIdentity}
	}
	if o.Summary == nil || o.Summary.Role != content.RoleUser || len(o.Summary.Blocks) != 1 {
		return &InvalidSummaryError{Reason: InvalidSummaryOutputShape}
	}
	text, ok := o.Summary.Blocks[0].(*content.TextBlock)
	if !ok || text == nil || text.Text == "" {
		return &InvalidSummaryError{Reason: InvalidSummaryOutputShape}
	}
	return nil
}

// InvalidSummaryReason is the closed, security-safe summary rejection category.
type InvalidSummaryReason string

const (
	InvalidSummaryWire         InvalidSummaryReason = "wire"
	InvalidSummaryIdentity     InvalidSummaryReason = "identity"
	InvalidSummaryOutputShape  InvalidSummaryReason = "output_shape"
	InvalidSummaryByteLimit    InvalidSummaryReason = "byte_limit"
	InvalidSummaryTokenUsage   InvalidSummaryReason = "token_usage"
	InvalidSummaryTokenLimit   InvalidSummaryReason = "token_limit"
	InvalidSummaryXMLSyntax    InvalidSummaryReason = "xml_syntax"
	InvalidSummaryXMLRoot      InvalidSummaryReason = "xml_root"
	InvalidSummaryXMLStructure InvalidSummaryReason = "xml_structure"
	InvalidSummaryXMLContent   InvalidSummaryReason = "xml_content"
)

// Valid reports whether the reason is recognized.
func (r InvalidSummaryReason) Valid() bool {
	switch r {
	case InvalidSummaryWire, InvalidSummaryIdentity, InvalidSummaryOutputShape,
		InvalidSummaryByteLimit, InvalidSummaryTokenUsage, InvalidSummaryTokenLimit,
		InvalidSummaryXMLSyntax, InvalidSummaryXMLRoot, InvalidSummaryXMLStructure,
		InvalidSummaryXMLContent:
		return true
	default:
		return false
	}
}

// InvalidSummaryError reports a bounded failure without rendering untrusted
// transcript or model output bytes.
type InvalidSummaryError struct {
	Reason InvalidSummaryReason
	Cause  error
}

func (e *InvalidSummaryError) Error() string {
	return "loop: invalid compaction summary (" + string(e.Reason) + ")"
}

func (e *InvalidSummaryError) Unwrap() error { return e.Cause }

// SummaryTooLargeError is reserved for the Task 26 complete-request count after
// replacement. Isolated hustle output budgeting uses InvalidSummaryTokenLimit.
type SummaryTooLargeError struct {
	Measurement event.ContextMeasurement
}

func (*SummaryTooLargeError) Error() string { return "loop: compacted request exceeds context limit" }
