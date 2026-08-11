package command

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/tool"
)

// schemaVersion is the current wire-envelope schema version stamped into every
// encoded command under the "v" key. It starts at 1; bump it (never reuse a
// number) when the on-disk envelope shape changes incompatibly, so a future
// reader can branch on the version of an old intent-log record. It is part of the
// durable wire contract and mirrors the event codec's version.
const schemaVersion = 1

// maxCommandBytes caps the serialized size UnmarshalCommand accepts at the
// untrusted restore boundary. A command's payload is small (header ids + capped
// scalars/strings) plus at most a slice of content blocks, which is bounded by the
// block codec's own caps; this envelope cap fails closed on absurd top-level input
// before any delegated decode runs. Conservative starting value; tune to real
// history sizes later. Mirrors the event codec's maxEventBytes.
const maxCommandBytes = 16 << 20 // 16 MiB

// UnknownCommandTypeError is returned by UnmarshalCommand when the envelope's
// "type" tag names no concrete command (including the empty/missing tag), or by
// MarshalCommand when a foreign concrete type is handed in (one not in
// classifyCommand's sealed union). The restore path is an untrusted boundary;
// callers fail secure on this error rather than guess a concrete command to
// reconstruct.
type UnknownCommandTypeError struct{ Type CommandName }

func (e *UnknownCommandTypeError) Error() string {
	return fmt.Sprintf("command: unknown command type %q", string(e.Type))
}

// CommandEncodeError wraps a failure to marshal a command's payload (a
// json.Marshal failure, or a delegated content-block codec failure on the marshal
// path).
type CommandEncodeError struct {
	Type  CommandName
	Cause error
}

func (e *CommandEncodeError) Error() string {
	return fmt.Sprintf("command: encode %s: %v", string(e.Type), e.Cause)
}
func (e *CommandEncodeError) Unwrap() error { return e.Cause }

// CommandDecodeError wraps a failure to unmarshal serialized command bytes
// (malformed JSON, wrong field types) once a known "type" tag has been read, or
// the initial envelope probe failure (nil/garbage/non-object), in which case Type
// is empty.
type CommandDecodeError struct {
	Type  CommandName
	Cause error
}

func (e *CommandDecodeError) Error() string {
	return fmt.Sprintf("command: decode %s: %v", string(e.Type), e.Cause)
}
func (e *CommandDecodeError) Unwrap() error { return e.Cause }

// CommandLimitError is returned when serialized input exceeds the envelope byte
// cap at the untrusted decode boundary.
type CommandLimitError struct {
	Got int
	Max int
}

func (e *CommandLimitError) Error() string {
	return fmt.Sprintf("command: input exceeds byte cap (%d > %d)", e.Got, e.Max)
}

// classifyCommand returns the wire discriminator for a concrete Command and
// whether cmd is a member of the sealed union. It reuses the CommandName naming
// source (the constants in validate.go + interrupt.go + shutdown.go) so the codec
// shares ONE source of truth with ValidateCommand: there is no second tag table to
// drift. A foreign type (one outside the union, e.g. a same-package test double)
// yields (CommandUnknown, false) and fails the codec closed. This is the marshal-
// side mirror of the event package's classify.
func classifyCommand(cmd Command) (CommandName, bool) {
	switch cmd.(type) {
	case UserInput:
		return CommandUserInput, true
	case SubagentResult:
		return CommandSubagentResult, true
	case ApproveToolCall:
		return CommandApproveToolCall, true
	case DenyToolCall:
		return CommandDenyToolCall, true
	case ProvideUserInput:
		return CommandProvideUserInput, true
	case CancelQueuedInput:
		return CommandCancelQueuedInput, true
	case CancelDelegateRequest:
		return CommandCancelDelegateRequest, true
	case Interrupt:
		return CommandInterrupt, true
	case Shutdown:
		return CommandShutdown, true
	case Compact:
		return CommandCompact, true
	case ProcessNotification:
		return CommandProcessNotification, true
	default:
		return CommandUnknown, false
	}
}

// MarshalCommand encodes a Command into the durable intent-log wire envelope: a
// JSON object carrying a "type" discriminator (== the CommandName naming source,
// the package's single source of truth), a "v" schema version, the embedded Header
// fields, and the type-specific payload. It fails closed on a type outside the
// sealed union (UnknownCommandTypeError). The transient ack channels
// (Interrupt.Ack/Shutdown.Ack, tagged json:"-") never serialize. The content
// blocks (UserInput.Blocks/SubagentResult.Blocks) are a sealed-interface slice with
// no general codec, so they are delegated to content.MarshalBlocks; every other
// field round-trips through encoding/json.
func MarshalCommand(cmd Command) ([]byte, error) {
	name, ok := classifyCommand(cmd)
	if !ok {
		return nil, &UnknownCommandTypeError{Type: name}
	}
	payload, err := encodePayload(name, cmd)
	if err != nil {
		return nil, err
	}
	return mergeEnvelope(name, payload)
}

// encodePayload marshals the concrete command's wire form (header + type-specific
// fields, with content-block fields delegated to the block codec). classifyCommand
// has already proven cmd is in the union, so the default arm is unreachable but
// fails secure with UnknownCommandTypeError rather than panicking.
func encodePayload(name CommandName, cmd Command) ([]byte, error) {
	switch c := cmd.(type) {
	case UserInput:
		return marshalUserInput(c)
	case SubagentResult:
		return marshalSubagentResult(c)
	case ApproveToolCall, DenyToolCall, ProvideUserInput, CancelQueuedInput, CancelDelegateRequest,
		Interrupt, Shutdown, Compact, ProcessNotification:
		// Every field round-trips through encoding/json directly: header + scalars/
		// strings/ids (uuid.UUID has its own text codec) + embedded Coordinates/
		// GateRoute, or (ProcessNotification) the bounded tool.ProcessCompletionNotification
		// DTO's own tagged fields. The two block-bearing commands (UserInput/SubagentResult)
		// are handled above; the transient channels (Interrupt/Shutdown/CancelDelegateRequest/
		// ProcessNotification.Result) are json:"-" and drop out here. This arm is the
		// lossless-plain remainder.
		return marshalPlain(name, cmd)
	default:
		return nil, &UnknownCommandTypeError{Type: name}
	}
}

// marshalPlain marshals a command whose fields all round-trip through
// encoding/json directly (no sealed-interface payload to delegate).
func marshalPlain(name CommandName, cmd Command) ([]byte, error) {
	out, err := json.Marshal(cmd)
	if err != nil {
		return nil, &CommandEncodeError{Type: name, Cause: err}
	}
	return out, nil
}

// userInputWire is UserInput's wire form: Blocks is pre-encoded by the content
// block codec ([]content.Block is a sealed-interface slice with no general struct
// codec, so it cannot ride as a plain field).
type userInputWire struct {
	Header
	Blocks                json.RawMessage       `json:"blocks,omitempty"`
	NoFold                bool                  `json:"no_fold,omitzero"`
	TargetLoopID          uuid.UUID             `json:"target_loop_id,omitzero"`
	BackgroundHandBack    bool                  `json:"background_hand_back,omitzero"`
	DelegateDeliveryPhase DelegateDeliveryPhase `json:"delegate_delivery_phase,omitzero"`
}

func marshalUserInput(c UserInput) ([]byte, error) {
	blocks, err := marshalBlocks(CommandUserInput, c.Blocks)
	if err != nil {
		return nil, err
	}
	out, err := json.Marshal(userInputWire{
		Header:                c.Header,
		Blocks:                blocks,
		NoFold:                c.NoFold,
		TargetLoopID:          c.TargetLoopID,
		BackgroundHandBack:    c.BackgroundHandBack,
		DelegateDeliveryPhase: c.DelegateDeliveryPhase,
	})
	if err != nil {
		return nil, &CommandEncodeError{Type: CommandUserInput, Cause: err}
	}
	return out, nil
}

// subagentResultWire is SubagentResult's wire form: the embedded Coordinates is
// promoted (the parent-loop delivery target) and Blocks is pre-encoded by the
// content block codec, mirroring userInputWire.
type subagentResultWire struct {
	Header
	identity.Coordinates
	Blocks json.RawMessage `json:"blocks,omitempty"`
}

func marshalSubagentResult(c SubagentResult) ([]byte, error) {
	blocks, err := marshalBlocks(CommandSubagentResult, c.Blocks)
	if err != nil {
		return nil, err
	}
	out, err := json.Marshal(subagentResultWire{Header: c.Header, Coordinates: c.Coordinates, Blocks: blocks})
	if err != nil {
		return nil, &CommandEncodeError{Type: CommandSubagentResult, Cause: err}
	}
	return out, nil
}

// marshalBlocks delegates a content-block slice to the content codec, returning a
// nil RawMessage for an empty/nil slice so omitempty drops the key (keeping the
// codec a fixed point: an empty slice round-trips back to nil, not []Block{}).
func marshalBlocks(name CommandName, blocks []content.Block) (json.RawMessage, error) {
	if len(blocks) == 0 {
		return nil, nil
	}
	b, err := content.MarshalBlocks(blocks)
	if err != nil {
		return nil, &CommandEncodeError{Type: name, Cause: err}
	}
	return b, nil
}

// mergeEnvelope merges the type discriminator and schema version into a
// pre-encoded payload object as sibling keys, mirroring the block/event codecs (a
// merge — never an embedding wrapper, which would let a payload field shadow "type"
// or "v"). The payload is always a JSON object here (every command marshals to
// one).
func mergeEnvelope(name CommandName, payload []byte) ([]byte, error) {
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(payload, &fields); err != nil {
		return nil, &CommandEncodeError{Type: name, Cause: err}
	}
	typeJSON, _ := json.Marshal(string(name))     // a string; cannot fail
	versionJSON, _ := json.Marshal(schemaVersion) // an int; cannot fail
	fields["type"] = typeJSON
	fields["v"] = versionJSON
	out, err := json.Marshal(fields)
	if err != nil {
		return nil, &CommandEncodeError{Type: name, Cause: err}
	}
	return out, nil
}

// UnmarshalCommand decodes a durable intent-log wire envelope back into a concrete
// Command. It fails closed on the untrusted restore boundary: input over the byte
// cap → CommandLimitError; malformed envelope → CommandDecodeError; an unknown or
// missing "type" tag → UnknownCommandTypeError; a malformed payload for a known
// type → CommandDecodeError. A successfully decoded command is validated against
// the ID fill matrix (ValidateCommand), so a structurally-valid but semantically-
// invalid record is rejected rather than resurrected. The transient ack channels
// are never on the wire, so a restored Interrupt/Shutdown carries a nil Ack.
func UnmarshalCommand(data []byte) (Command, error) {
	if len(data) > maxCommandBytes {
		return nil, &CommandLimitError{Got: len(data), Max: maxCommandBytes}
	}
	var probe struct {
		Type CommandName `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, &CommandDecodeError{Type: "", Cause: err}
	}
	cmd, err := decodePayload(probe.Type, data)
	if err != nil {
		return nil, err
	}
	if err := ValidateCommand(cmd); err != nil {
		return nil, err
	}
	return cmd, nil
}

// decodePayload dispatches on the "type" tag to the concrete decoder. An unknown
// or empty tag fails secure with UnknownCommandTypeError; a malformed payload
// surfaces as CommandDecodeError from the per-type decoder.
func decodePayload(tag CommandName, data []byte) (Command, error) {
	switch tag {
	case CommandUserInput:
		return decodeUserInput(data)
	case CommandSubagentResult:
		return decodeSubagentResult(data)
	case CommandApproveToolCall:
		return decodePlain[ApproveToolCall](tag, data)
	case CommandDenyToolCall:
		return decodePlain[DenyToolCall](tag, data)
	case CommandProvideUserInput:
		return decodePlain[ProvideUserInput](tag, data)
	case CommandCancelQueuedInput:
		return decodePlain[CancelQueuedInput](tag, data)
	case CommandCancelDelegateRequest:
		return decodePlain[CancelDelegateRequest](tag, data)
	case CommandInterrupt:
		return decodePlain[Interrupt](tag, data)
	case CommandShutdown:
		return decodePlain[Shutdown](tag, data)
	case CommandCompact:
		return decodePlain[Compact](tag, data)
	case CommandProcessNotification:
		return decodeProcessNotification(data)
	default:
		return nil, &UnknownCommandTypeError{Type: tag}
	}
}

// processNotificationWire is ProcessNotification's wire form. It mirrors
// pkg/event's strict process-lifecycle decode (decodeProcessLifecycleEvent):
// the payload is a CLOSED set of keys — the envelope discriminator, the
// generic Header, and the bounded notification DTO's own fields — decoded
// with DisallowUnknownFields and a trailing-value check so an attempt to
// smuggle command text, output, stdin, a host path, an OS PID, or any other
// free-form key (at the envelope level OR inside "notification") fails
// closed with a typed *CommandDecodeError rather than being silently
// dropped. This is a deliberate, narrower exception to the rest of the
// package's permissive decodePlain: ProcessNotification is the one command
// whose payload is explicitly metadata-only by design.
type processNotificationWire struct {
	Type string  `json:"type"`
	V    *uint32 `json:"v"`
	Header
	Notification tool.ProcessCompletionNotification `json:"notification"`
}

func decodeProcessNotification(data []byte) (Command, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()

	var wire processNotificationWire
	if err := decoder.Decode(&wire); err != nil {
		return nil, &CommandDecodeError{Type: CommandProcessNotification, Cause: err}
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("trailing JSON value")
		}
		return nil, &CommandDecodeError{Type: CommandProcessNotification, Cause: err}
	}
	return ProcessNotification{Header: wire.Header, Notification: wire.Notification}, nil
}

// decodePlain decodes a command whose fields all round-trip through encoding/json
// directly (the inverse of marshalPlain). The extra "type"/"v" envelope keys are
// ignored by the struct decode, and the json:"-" ack channels stay nil.
func decodePlain[T any](tag CommandName, data []byte) (Command, error) {
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, &CommandDecodeError{Type: tag, Cause: err}
	}
	cmd, ok := any(v).(Command)
	if !ok {
		// Unreachable: every T in the dispatch is a concrete Command. Fail secure.
		return nil, &UnknownCommandTypeError{Type: tag}
	}
	return cmd, nil
}

func decodeUserInput(data []byte) (Command, error) {
	var w userInputWire
	if err := json.Unmarshal(data, &w); err != nil {
		return nil, &CommandDecodeError{Type: CommandUserInput, Cause: err}
	}
	blocks, err := decodeBlocks(CommandUserInput, w.Blocks)
	if err != nil {
		return nil, err
	}
	return UserInput{
		Header:                w.Header,
		Blocks:                blocks,
		NoFold:                w.NoFold,
		TargetLoopID:          w.TargetLoopID,
		BackgroundHandBack:    w.BackgroundHandBack,
		DelegateDeliveryPhase: w.DelegateDeliveryPhase,
	}, nil
}

func decodeSubagentResult(data []byte) (Command, error) {
	var w subagentResultWire
	if err := json.Unmarshal(data, &w); err != nil {
		return nil, &CommandDecodeError{Type: CommandSubagentResult, Cause: err}
	}
	blocks, err := decodeBlocks(CommandSubagentResult, w.Blocks)
	if err != nil {
		return nil, err
	}
	return SubagentResult{Header: w.Header, Coordinates: w.Coordinates, Blocks: blocks}, nil
}

// decodeBlocks delegates a pre-encoded block array to the content codec, returning
// nil for an absent/empty array (matching the marshal path's omitempty) so the
// codec is a stable fixed point.
func decodeBlocks(name CommandName, raw json.RawMessage) ([]content.Block, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	blocks, err := content.UnmarshalBlocks(raw)
	if err != nil {
		return nil, &CommandDecodeError{Type: name, Cause: err}
	}
	return blocks, nil
}
