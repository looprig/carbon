package event

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/tool"
	model "github.com/looprig/inference/model"
)

// schemaVersion is the current wire-envelope schema version stamped into every
// encoded event under the "v" key. It starts at 1; bump it (never reuse a number)
// when the on-disk envelope shape changes incompatibly, so a future reader can
// branch on the version of an old journal record. It is part of the durable wire
// contract.
const schemaVersion = 1

// maxEventBytes caps the serialized size MarshalEvent emits and UnmarshalEvent
// accepts at durable boundaries. An event's payload is small (header ids + capped strings) plus
// at most a committed step group, which is bounded by the block codec's own caps;
// this envelope cap fails closed on absurd top-level input before any delegated
// decode runs. Conservative starting value; tune to real history sizes later.
const maxEventBytes = 16 << 20 // 16 MiB

// EphemeralNotPersistableError is returned by MarshalEvent when handed any
// Ephemeral event.
// The Ephemeral set is never persisted — it self-heals from a later authoritative
// event and TokenDelta.Chunk has no durable codec — so the marshaler fails closed
// rather than emit a lossy record. Type is the classify name of the rejected event
// so a caller learns exactly which event it tried to persist.
type EphemeralNotPersistableError struct{ Type string }

func (e *EphemeralNotPersistableError) Error() string {
	return fmt.Sprintf("event: %s is Ephemeral and not persistable", e.Type)
}

// UnknownEventTypeError is returned by UnmarshalEvent when the envelope's "type"
// tag names no concrete event (including the empty/missing tag), or by
// MarshalEvent when a foreign concrete type is handed in (one not in classify's
// sealed union). The restore path is an untrusted boundary; callers fail secure on
// this error rather than guess a concrete event to reconstruct.
type UnknownEventTypeError struct{ Type string }

func (e *UnknownEventTypeError) Error() string {
	return fmt.Sprintf("event: unknown event type %q", e.Type)
}

// UnsupportedSchemaError reports a durable record whose schema version is
// newer than this binary supports. It is the dispatch hook a future migration
// layer catches; today it is a hard, typed restore failure.
type UnsupportedSchemaError struct {
	Kind    string // "event"
	Version uint32
}

func (e *UnsupportedSchemaError) Error() string {
	return fmt.Sprintf("event: unsupported %s schema version %d (max %d)", e.Kind, e.Version, schemaVersion)
}

// EventEncodeError wraps a failure to marshal an event's payload (a json.Marshal
// failure, or a delegated content/tool codec failure on the marshal path).
type EventEncodeError struct {
	Type  string
	Cause error
}

func (e *EventEncodeError) Error() string {
	return fmt.Sprintf("event: encode %s: %v", e.Type, e.Cause)
}
func (e *EventEncodeError) Unwrap() error { return e.Cause }

// EventDecodeError wraps a failure to unmarshal serialized event bytes (malformed
// JSON, wrong field types) once a known "type" tag has been read.
type EventDecodeError struct {
	Type  string
	Cause error
}

func (e *EventDecodeError) Error() string {
	return fmt.Sprintf("event: decode %s: %v", e.Type, e.Cause)
}
func (e *EventDecodeError) Unwrap() error { return e.Cause }

// LegacyRuntimeMigrationError reports a v1 lifecycle payload that cannot be
// migrated to the current ModelRuntime representation without guessing. Valid
// pre-runtime LoopInferenceChanged records always carry model; accepting a
// record with neither model nor runtime would silently erase the selected model.
type LegacyRuntimeMigrationError struct {
	Type   string
	Field  string
	Reason string
}

func (e *LegacyRuntimeMigrationError) Error() string {
	return "event: cannot migrate legacy " + e.Type + " field " + e.Field + ": " + e.Reason
}

// EventLimitError is returned when an event codec input or output exceeds its cap.
type EventLimitError struct {
	Got int
	Max int
}

func (e *EventLimitError) Error() string {
	return fmt.Sprintf("event: codec limit exceeded (%d > %d)", e.Got, e.Max)
}

// MarshalEvent encodes an Enduring event into the durable wire envelope: a JSON
// object carrying a "type" discriminator (== the classify name, the package's
// single naming source of truth), a "v" schema version, the embedded Header
// fields, and the type-specific payload. It fails closed on an Ephemeral event
// (EphemeralNotPersistableError) and on a type outside the sealed union
// (UnknownEventTypeError). The interface-valued fields that have no general codec
// (PermissionRequested.Request, TurnFailed.Err, RestoreErrored.Err) are projected
// onto durable forms; every other field round-trips through encoding/json.
func MarshalEvent(ev Event) ([]byte, error) {
	name, _, ok := classify(ev)
	if !ok {
		return nil, &UnknownEventTypeError{Type: name}
	}
	if !ev.Visibility().Valid() {
		return nil, &InvalidEventError{Event: EventName(name), Field: FieldVisibility, Rule: RuleInvalid}
	}
	if ev.Class() == Ephemeral {
		return nil, &EphemeralNotPersistableError{Type: name}
	}
	// Both identity AND body validation are required at the durable write boundary.
	// Validating only the body on write was the write/read asymmetry that let a
	// zero-coordinate gate marshal clean and poison a future restore: the decode path
	// validates full identity, so an event that violates its identity profile must be
	// REFUSED here — at open time — rather than silently persisted to break replay.
	// Every event reaching this boundary already has its coordinates stamped (the hub
	// stamps lifecycle/loop/turn/step events; the gate open path in
	// sessionruntime/gates.go stamps GatePrepared/GateOpened/GateResolved before
	// append), so this rejects only genuinely malformed identity, never a legitimate
	// pre-stamp marshal.
	if err := validateEventIdentity(ev); err != nil {
		return nil, err
	}
	if err := validateEventBody(ev); err != nil {
		return nil, err
	}
	payload, err := encodePayload(ev)
	if err != nil {
		return nil, err
	}
	out, err := mergeEnvelope(name, payload)
	if err != nil {
		return nil, err
	}
	if len(out) > maxEventBytes {
		return nil, &EventLimitError{Got: len(out), Max: maxEventBytes}
	}
	return out, nil
}

// encodePayload marshals the concrete event's wire form (header + type-specific
// fields, with interface fields projected). It is the single per-type marshal
// dispatch; classify has already proven ev is in the union, so the default arm is
// unreachable but fails secure with UnknownEventTypeError rather than panicking.
func encodePayload(ev Event) ([]byte, error) {
	switch e := ev.(type) {
	case StepDone:
		return marshalStepDone(e)
	case PermissionRequested:
		return marshalPermissionRequested(e)
	case TurnFailed:
		return marshalTurnFailed(e)
	case RestoreErrored:
		return marshalRestoreErrored(e)
	case GateResolved:
		return marshalGateResolved(e)
	case SessionStarted, SessionActive, SessionIdle, SessionStopped,
		ConfigurationAdopted,
		RestoreStarted, RestoreDone, WorkspaceCheckpointed, WorkspaceRestored,
		ActiveLoopChanged,
		DelegateDeliveryStateChanged,
		LoopRestoreTombstoned,
		HustleStarted, HustleCompleted, HustleFailed,
		PermissionReviewStarted, PermissionReviewCompleted,
		ProcessStarted, ProcessBackgrounded, ProcessCompleted, ProcessStopRequested, ProcessLost,
		LoopIdle, LoopStarted, DelegateRequestAccepted, LoopInferenceChanged, LoopModeChanged,
		LoopExternalToolsetChanged, ContextMeasured, WorkflowActivity,
		CompactionCommitted, CompactionRejected, CompactWaiterResolved, CompactWaiterRejected,
		ForeignSessionBound, LoopAgentSessionBound, TurnRejected,
		UserInputRequested, TurnInterrupted,
		TurnStarted, TurnFoldedInto, InputCancelled, TurnDone,
		PermissionDecided, GatePrepared, GateOpened:
		// Every field round-trips through encoding/json directly: header + scalars/
		// strings/slices, and for the Message-bearing four (TurnStarted/TurnFoldedInto/
		// InputCancelled/TurnDone) the content.Message codec tags nested blocks. The
		// four interface-field events (StepDone/PermissionRequested/TurnFailed/
		// RestoreErrored) are handled above; this arm is the lossless-plain remainder.
		return marshalPlain(ev)
	default:
		name, _, _ := classify(ev)
		return nil, &UnknownEventTypeError{Type: name}
	}
}

// marshalPlain marshals an event whose fields all round-trip through encoding/json
// directly. The Message-bearing events (TurnStarted/TurnFoldedInto/InputCancelled/
// TurnDone) qualify: content.Message/ToolResultMessage define their own MarshalJSON
// that tags nested blocks, so a *content.UserMessage / *content.AIMessage value
// serializes losslessly here.
func marshalPlain(ev Event) ([]byte, error) {
	name, _, _ := classify(ev)
	out, err := json.Marshal(ev)
	if err != nil {
		return nil, &EventEncodeError{Type: name, Cause: err}
	}
	return out, nil
}

// stepDoneWire is StepDone's wire form: the header is promoted and Messages is
// pre-encoded by the message-slice codec (content.AgenticMessages is a []sealed-
// interface with no general struct codec, so it cannot ride as a plain field).
type stepDoneWire struct {
	Header
	Messages json.RawMessage `json:"messages,omitempty"`
}

func marshalStepDone(e StepDone) ([]byte, error) {
	var msgs json.RawMessage
	if len(e.Messages) > 0 {
		m, err := marshalMessages(e.Messages)
		if err != nil {
			return nil, &EventEncodeError{Type: "StepDone", Cause: err}
		}
		msgs = m
	}
	out, err := json.Marshal(stepDoneWire{Header: e.Header, Messages: msgs})
	if err != nil {
		return nil, &EventEncodeError{Type: "StepDone", Cause: err}
	}
	return out, nil
}

// permissionRequestedWire is PermissionRequested's wire form: the typed
// prepared Request is validated (tool.ValidateRequest) and pre-encoded into a
// sibling "request" key; the decoder routes it through the strict
// gate.DecodeRequest, so a malformed or legacy sealed-request record fails
// closed at the untrusted restore boundary.
type permissionRequestedWire struct {
	Header
	ToolExecutionID uuid.UUID       `json:"tool_execution_id,omitzero"`
	Request         json.RawMessage `json:"request,omitempty"`
}

func marshalPermissionRequested(e PermissionRequested) ([]byte, error) {
	if err := tool.ValidateRequest(e.Request); err != nil {
		return nil, &EventEncodeError{Type: "PermissionRequested", Cause: err}
	}
	req, err := json.Marshal(e.Request)
	if err != nil {
		return nil, &EventEncodeError{Type: "PermissionRequested", Cause: err}
	}
	out, err := json.Marshal(permissionRequestedWire{
		Header:          e.Header,
		ToolExecutionID: e.ToolExecutionID,
		Request:         req,
	})
	if err != nil {
		return nil, &EventEncodeError{Type: "PermissionRequested", Cause: err}
	}
	return out, nil
}

// turnFailedWire is TurnFailed's wire form: the Err interface has no general codec,
// so it is projected to a stable {kind,message} pair plus an additive explicit
// model-facing classification/detail when the live error opts into that marker.
// Legacy records containing only {kind,message} remain ordinary on restore.
type turnFailedWire struct {
	Header
	TurnIndex TurnIndex          `json:"turn_index,omitzero"`
	Err       *restoredErrorWire `json:"err,omitempty"`
}

func marshalTurnFailed(e TurnFailed) ([]byte, error) {
	out, err := json.Marshal(turnFailedWire{
		Header:    e.Header,
		TurnIndex: e.TurnIndex,
		Err:       projectError(e.Err),
	})
	if err != nil {
		return nil, &EventEncodeError{Type: "TurnFailed", Cause: err}
	}
	return out, nil
}

// restoreErroredWire mirrors turnFailedWire for the session-scoped restore failure.
type restoreErroredWire struct {
	Header
	Err *restoredErrorWire `json:"err,omitempty"`
}

func marshalRestoreErrored(e RestoreErrored) ([]byte, error) {
	out, err := json.Marshal(restoreErroredWire{
		Header: e.Header,
		Err:    projectError(e.Err),
	})
	if err != nil {
		return nil, &EventEncodeError{Type: "RestoreErrored", Cause: err}
	}
	return out, nil
}

// restoredErrorWire is the additive durable projection for an error-bearing event.
// model_facing and model_facing_detail are omitted for ordinary errors, preserving
// the legacy {kind,message} shape. Safety is accepted only when the explicit boolean
// marker is true; message/kind alone never opt an error into the model-facing path.
type restoredErrorWire struct {
	Kind              string `json:"kind"`
	Message           string `json:"message"`
	ModelFacing       bool   `json:"model_facing,omitempty"`
	ModelFacingDetail string `json:"model_facing_detail,omitempty"`
}

func (w *restoredErrorWire) errorValue() error {
	if w == nil {
		return nil
	}
	if w.ModelFacing {
		return &RestoredModelFacingError{
			Kind: w.Kind, Message: w.Message,
			Detail: tool.BoundModelFacingErrorDetail(w.ModelFacingDetail),
		}
	}
	return &RestoredError{Kind: w.Kind, Message: w.Message}
}

// projectError projects a live error to its durable form. A nil error projects to
// KindUnknown with an empty message (an absent cause), so the restored event always
// carries a concrete ordinary RestoredError. An explicitly marked safe detail is
// copied into separate additive fields after normalization; no ordinary error text
// or stable kind can opt into that classification.
func projectError(err error) *restoredErrorWire {
	if err == nil {
		return &restoredErrorWire{Kind: KindUnknown, Message: ""}
	}
	var modelRestored *RestoredModelFacingError
	if errors.As(err, &modelRestored) && modelRestored != nil {
		wire := &restoredErrorWire{Kind: modelRestored.Kind, Message: modelRestored.Message}
		if detail, marked := tool.ModelFacingErrorDetail(err); marked {
			wire.ModelFacing = true
			wire.ModelFacingDetail = tool.BoundModelFacingErrorDetail(detail)
		}
		return wire
	}
	var restored *RestoredError
	if errors.As(err, &restored) {
		wire := &restoredErrorWire{Kind: restored.Kind, Message: restored.Message}
		if detail, marked := tool.ModelFacingErrorDetail(err); marked {
			wire.ModelFacing = true
			wire.ModelFacingDetail = tool.BoundModelFacingErrorDetail(detail)
		}
		return wire
	}
	wire := &restoredErrorWire{Kind: ErrKind(err), Message: err.Error()}
	if detail, marked := tool.ModelFacingErrorDetail(err); marked {
		wire.ModelFacing = true
		wire.ModelFacingDetail = tool.BoundModelFacingErrorDetail(detail)
	}
	return wire
}

// mergeEnvelope merges the type discriminator and schema version into a pre-encoded
// payload object as sibling keys, mirroring the block/permission-request codecs (a
// merge — never an embedding wrapper, which would let a payload field shadow "type"
// or "v"). The payload is always a JSON object here (every event marshals to one).
func mergeEnvelope(name string, payload []byte) ([]byte, error) {
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(payload, &fields); err != nil {
		return nil, &EventEncodeError{Type: name, Cause: err}
	}
	typeJSON, _ := json.Marshal(name)             // a string; cannot fail
	versionJSON, _ := json.Marshal(schemaVersion) // an int; cannot fail
	fields["type"] = typeJSON
	fields["v"] = versionJSON
	out, err := json.Marshal(fields)
	if err != nil {
		return nil, &EventEncodeError{Type: name, Cause: err}
	}
	return out, nil
}

// UnmarshalEvent decodes a durable wire envelope back into a concrete Event. It
// fails closed on the untrusted restore boundary: input over the byte cap →
// EventLimitError; malformed envelope → EventDecodeError; an unknown or missing
// "type" tag → UnknownEventTypeError; a malformed payload for a known type →
// EventDecodeError. A successfully decoded event is validated against the ID fill
// matrix (ValidateEvent), so a structurally-valid but semantically-invalid record
// is rejected rather than resurrected.
func UnmarshalEvent(data []byte) (Event, error) {
	if len(data) > maxEventBytes {
		return nil, &EventLimitError{Got: len(data), Max: maxEventBytes}
	}
	if !utf8.Valid(data) {
		return nil, &EventDecodeError{Cause: errors.New("invalid UTF-8 event envelope")}
	}
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return nil, &EventDecodeError{Cause: err}
	}
	var probe struct {
		Type string  `json:"type"`
		V    *uint32 `json:"v"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, &EventDecodeError{Type: "", Cause: err}
	}
	// The "v" envelope key is the durable schema version. Absent means version 1
	// (old records predate the field guarantee); a value newer than this binary
	// supports fails closed with a typed error the future migration layer catches.
	// A present-but-malformed "v" (non-number) already surfaced as EventDecodeError
	// from the probe unmarshal above.
	if probe.V != nil && *probe.V > schemaVersion {
		return nil, &UnsupportedSchemaError{Kind: "event", Version: *probe.V}
	}
	ev, err := decodePayload(probe.Type, data)
	if err != nil {
		return nil, err
	}
	if err := validateDecodedEvent(ev, data); err != nil {
		return nil, err
	}
	return ev, nil
}

func rejectDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	return inspectJSONValue(decoder, token)
}

func inspectJSONValue(decoder *json.Decoder, token json.Token) error {
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("non-string object key")
			}
			canonical := strings.ToLower(key)
			if _, exists := seen[canonical]; exists {
				return fmt.Errorf("duplicate field %q", key)
			}
			seen[canonical] = struct{}{}
			valueToken, err := decoder.Token()
			if err != nil {
				return err
			}
			if err := inspectJSONValue(decoder, valueToken); err != nil {
				return err
			}
		}
		_, err := decoder.Token()
		return err
	case '[':
		for decoder.More() {
			valueToken, err := decoder.Token()
			if err != nil {
				return err
			}
			if err := inspectJSONValue(decoder, valueToken); err != nil {
				return err
			}
		}
		_, err := decoder.Token()
		return err
	default:
		return fmt.Errorf("unexpected delimiter %q", delim)
	}
}

// validateDecodedEvent preserves the one additive compatibility exception in the
// event schema: checkpoints written before consistency/trigger existed decode with
// both enum values unknown. Explicit zero values, partial metadata, and newly
// constructed unknown-valued checkpoints remain invalid through ValidateEvent.
func validateDecodedEvent(ev Event, data []byte) error {
	if err := validateEventIdentity(ev); err != nil {
		return err
	}
	if legacy, err := missingLegacyRuntime(ev, data); err != nil {
		return err
	} else if legacy {
		return nil
	}
	if _, ok := ev.(WorkspaceCheckpointed); ok {
		presence, err := inspectCheckpointMetadata(data)
		if err != nil {
			return err
		}
		if !presence.consistency && !presence.trigger {
			return nil
		}
	}
	return validateEventBody(ev)
}

// missingLegacyRuntime preserves replay compatibility for lifecycle records
// that never carried a resolved runtime. A LoopStarted with agent_runtime is
// not legacy: native/harness-managed records intentionally omit runtime, but
// their AgentRuntime must still pass strict validation. Legacy
// LoopInferenceChanged is not an absence-only case: decodeLoopInferenceChanged
// migrates its model+effort payload before validation. An explicitly present
// zero/invalid runtime continues through strict validation.
func missingLegacyRuntime(ev Event, data []byte) (bool, error) {
	name := ""
	switch ev.(type) {
	case LoopStarted:
		name = "LoopStarted"
	case LoopModeChanged:
		name = "LoopModeChanged"
	default:
		return false, nil
	}
	runtimePresent, err := inspectTopLevelField(data, name, "runtime")
	if err != nil {
		return false, err
	}
	agentRuntimePresent, err := inspectTopLevelField(data, name, "agent_runtime")
	if err != nil {
		return false, err
	}
	return !runtimePresent && !agentRuntimePresent, nil
}

func inspectTopLevelField(data []byte, typeName, fieldName string) (bool, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	if _, err := dec.Token(); err != nil {
		return false, &EventDecodeError{Type: typeName, Cause: err}
	}
	present := false
	for dec.More() {
		keyToken, err := dec.Token()
		if err != nil {
			return false, &EventDecodeError{Type: typeName, Cause: err}
		}
		key, ok := keyToken.(string)
		if !ok {
			return false, &EventDecodeError{Type: typeName, Cause: fmt.Errorf("non-string object key")}
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return false, &EventDecodeError{Type: typeName, Cause: err}
		}
		if strings.EqualFold(key, fieldName) {
			if present {
				return false, &EventDecodeError{Type: typeName, Cause: fmt.Errorf("duplicate %s field", fieldName)}
			}
			present = true
		}
	}
	if _, err := dec.Token(); err != nil {
		return false, &EventDecodeError{Type: typeName, Cause: err}
	}
	return present, nil
}

type checkpointMetadataPresence struct {
	consistency bool
	trigger     bool
}

// inspectCheckpointMetadata enumerates top-level member names without first
// collapsing them into a map. encoding/json matches struct fields case-
// insensitively, so presence detection must do the same. Enumerating also lets us
// reject two spellings of one logical field instead of allowing last-value-wins
// decoding to hide an explicit unknown or conflicting value.
func inspectCheckpointMetadata(data []byte) (checkpointMetadataPresence, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	if _, err := dec.Token(); err != nil {
		return checkpointMetadataPresence{}, &EventDecodeError{Type: "WorkspaceCheckpointed", Cause: err}
	}
	var presence checkpointMetadataPresence
	for dec.More() {
		keyToken, err := dec.Token()
		if err != nil {
			return checkpointMetadataPresence{}, &EventDecodeError{Type: "WorkspaceCheckpointed", Cause: err}
		}
		key, ok := keyToken.(string)
		if !ok {
			return checkpointMetadataPresence{}, &EventDecodeError{Type: "WorkspaceCheckpointed", Cause: fmt.Errorf("non-string object key")}
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return checkpointMetadataPresence{}, &EventDecodeError{Type: "WorkspaceCheckpointed", Cause: err}
		}
		switch {
		case strings.EqualFold(key, "consistency"):
			if presence.consistency {
				return checkpointMetadataPresence{}, &EventDecodeError{Type: "WorkspaceCheckpointed", Cause: fmt.Errorf("duplicate consistency field")}
			}
			presence.consistency = true
		case strings.EqualFold(key, "trigger"):
			if presence.trigger {
				return checkpointMetadataPresence{}, &EventDecodeError{Type: "WorkspaceCheckpointed", Cause: fmt.Errorf("duplicate trigger field")}
			}
			presence.trigger = true
		}
	}
	if _, err := dec.Token(); err != nil {
		return checkpointMetadataPresence{}, &EventDecodeError{Type: "WorkspaceCheckpointed", Cause: err}
	}
	return presence, nil
}

// decodePayload dispatches on the "type" tag to the concrete decoder. An unknown or
// empty tag fails secure with UnknownEventTypeError; a malformed payload surfaces as
// EventDecodeError from the per-type decoder.
func decodePayload(tag string, data []byte) (Event, error) {
	switch tag {
	case "SessionStarted":
		return decodePlain[SessionStarted](tag, data)
	case "SessionActive":
		return decodePlain[SessionActive](tag, data)
	case "SessionIdle":
		return decodePlain[SessionIdle](tag, data)
	case "SessionStopped":
		return decodePlain[SessionStopped](tag, data)
	case "ConfigurationAdopted":
		return decodePlain[ConfigurationAdopted](tag, data)
	case "RestoreStarted":
		return decodePlain[RestoreStarted](tag, data)
	case "RestoreDone":
		return decodePlain[RestoreDone](tag, data)
	case "RestoreErrored":
		return decodeRestoreErrored(data)
	case "WorkspaceCheckpointed":
		return decodePlain[WorkspaceCheckpointed](tag, data)
	case "WorkspaceRestored":
		return decodePlain[WorkspaceRestored](tag, data)
	case "ActiveLoopChanged":
		return decodePlain[ActiveLoopChanged](tag, data)
	case "DelegateDeliveryStateChanged":
		return decodeDelegateDeliveryStateChanged(data)
	case "WorkflowActivity":
		return decodeWorkflowActivity(data)
	case "LoopRestoreTombstoned":
		return decodePlain[LoopRestoreTombstoned](tag, data)
	case "HustleStarted":
		return decodePlain[HustleStarted](tag, data)
	case "HustleCompleted":
		return decodePlain[HustleCompleted](tag, data)
	case "HustleFailed":
		return decodePlain[HustleFailed](tag, data)
	case "PermissionReviewStarted":
		return decodePlain[PermissionReviewStarted](tag, data)
	case "PermissionReviewCompleted":
		return decodePlain[PermissionReviewCompleted](tag, data)
	case "ProcessStarted":
		return decodeProcessLifecycleEvent(tag, data)
	case "ProcessBackgrounded":
		return decodeProcessLifecycleEvent(tag, data)
	case "ProcessCompleted":
		return decodeProcessLifecycleEvent(tag, data)
	case "ProcessStopRequested":
		return decodeProcessLifecycleEvent(tag, data)
	case "ProcessLost":
		return decodeProcessLifecycleEvent(tag, data)
	case "LoopIdle":
		return decodePlain[LoopIdle](tag, data)
	case "LoopStarted":
		return decodePlain[LoopStarted](tag, data)
	case "DelegateRequestAccepted":
		return decodePlain[DelegateRequestAccepted](tag, data)
	case "LoopInferenceChanged":
		return decodeLoopInferenceChanged(data)
	case "LoopModeChanged":
		return decodePlain[LoopModeChanged](tag, data)
	case "LoopExternalToolsetChanged":
		return decodePlain[LoopExternalToolsetChanged](tag, data)
	case "ContextMeasured":
		return decodePlain[ContextMeasured](tag, data)
	case "CompactionCommitted":
		return decodePlain[CompactionCommitted](tag, data)
	case "CompactionRejected":
		return decodePlain[CompactionRejected](tag, data)
	case "CompactWaiterResolved":
		return decodePlain[CompactWaiterResolved](tag, data)
	case "CompactWaiterRejected":
		return decodePlain[CompactWaiterRejected](tag, data)
	case "ForeignSessionBound":
		return decodePlain[ForeignSessionBound](tag, data)
	case "LoopAgentSessionBound":
		return decodePlain[LoopAgentSessionBound](tag, data)
	case "TurnStarted":
		return decodePlain[TurnStarted](tag, data)
	case "StepDone":
		return decodeStepDone(data)
	case "TurnFoldedInto":
		return decodePlain[TurnFoldedInto](tag, data)
	case "InputCancelled":
		return decodePlain[InputCancelled](tag, data)
	case "TurnRejected":
		return decodePlain[TurnRejected](tag, data)
	case "TurnDone":
		return decodePlain[TurnDone](tag, data)
	case "TurnFailed":
		return decodeTurnFailed(data)
	case "TurnInterrupted":
		return decodePlain[TurnInterrupted](tag, data)
	case "PermissionRequested":
		return decodePermissionRequested(data)
	case "PermissionDecided":
		return decodePlain[PermissionDecided](tag, data)
	case "UserInputRequested":
		return decodePlain[UserInputRequested](tag, data)
	case "GatePrepared":
		return decodePlain[GatePrepared](tag, data)
	case "GateOpened":
		return decodePlain[GateOpened](tag, data)
	case "GateResolved":
		return decodeGateResolved(data)
	default:
		return nil, &UnknownEventTypeError{Type: tag}
	}
}

type legacyModelCapsWire struct {
	MaxContext int64
}

type legacyModelWire struct {
	Provider model.ProviderName
	Name     string
	Caps     legacyModelCapsWire
}

type legacyLoopInferenceChangedWire struct {
	Header
	Model  legacyModelWire `json:"model"`
	Effort model.Effort    `json:"effort,omitzero"`
}

// decodeLoopInferenceChanged accepts both v1 shapes. Current records carry the
// resolved runtime directly. Pre-runtime v1 records carry a secret-free Model
// plus effort; only the stable provider/name identity and MaxContext are durable
// in the replacement shape, so endpoint, API dialect, capabilities, sampling,
// and provenance are intentionally discarded.
func decodeLoopInferenceChanged(data []byte) (Event, error) {
	runtimePresent, err := inspectTopLevelField(data, "LoopInferenceChanged", "runtime")
	if err != nil {
		return nil, err
	}
	modelPresent, err := inspectTopLevelField(data, "LoopInferenceChanged", "model")
	if err != nil {
		return nil, err
	}
	if runtimePresent && modelPresent {
		return nil, &EventDecodeError{Type: "LoopInferenceChanged", Cause: &LegacyRuntimeMigrationError{
			Type: "LoopInferenceChanged", Field: "model/runtime", Reason: "both legacy and current payloads are present",
		}}
	}
	if runtimePresent {
		return decodePlain[LoopInferenceChanged]("LoopInferenceChanged", data)
	}
	if !modelPresent {
		return nil, &EventDecodeError{Type: "LoopInferenceChanged", Cause: &LegacyRuntimeMigrationError{
			Type: "LoopInferenceChanged", Field: "model", Reason: "missing model and runtime",
		}}
	}

	var wire legacyLoopInferenceChangedWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, &EventDecodeError{Type: "LoopInferenceChanged", Cause: err}
	}
	if wire.Model.Caps.MaxContext < 0 {
		return nil, &EventDecodeError{Type: "LoopInferenceChanged", Cause: &LegacyRuntimeMigrationError{
			Type: "LoopInferenceChanged", Field: "model.Caps.MaxContext", Reason: "must not be negative",
		}}
	}
	return LoopInferenceChanged{
		Header: wire.Header,
		Runtime: ModelRuntime{
			Key: model.ModelKey{
				Provider: wire.Model.Provider,
				Model:    wire.Model.Name,
			},
			Limits: model.ContextLimits{WindowTokens: content.TokenCount(wire.Model.Caps.MaxContext)},
			Effort: wire.Effort,
		},
	}, nil
}

// decodePlain decodes an event whose fields all round-trip through encoding/json
// directly (the inverse of marshalPlain). The extra "type"/"v" envelope keys are
// ignored by the struct decode.
func decodePlain[T any](tag string, data []byte) (Event, error) {
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, &EventDecodeError{Type: tag, Cause: err}
	}
	ev, ok := any(v).(Event)
	if !ok {
		// Unreachable: every T in the dispatch is a concrete Event. Fail secure.
		return nil, &UnknownEventTypeError{Type: tag}
	}
	return ev, nil
}

type processLifecycleEventWire struct {
	Type string  `json:"type"`
	V    *uint32 `json:"v"`
	Header
	Process tool.ProcessLifecycleMetadata `json:"process"`
}

func decodeProcessLifecycleEvent(tag string, data []byte) (Event, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()

	var wire processLifecycleEventWire
	if err := decoder.Decode(&wire); err != nil {
		return nil, &EventDecodeError{Type: tag, Cause: err}
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			err = errors.New("trailing JSON value")
		}
		return nil, &EventDecodeError{Type: tag, Cause: err}
	}

	switch tag {
	case "ProcessStarted":
		return ProcessStarted{Header: wire.Header, Process: wire.Process}, nil
	case "ProcessBackgrounded":
		return ProcessBackgrounded{Header: wire.Header, Process: wire.Process}, nil
	case "ProcessCompleted":
		return ProcessCompleted{Header: wire.Header, Process: wire.Process}, nil
	case "ProcessStopRequested":
		return ProcessStopRequested{Header: wire.Header, Process: wire.Process}, nil
	case "ProcessLost":
		return ProcessLost{Header: wire.Header, Process: wire.Process}, nil
	default:
		return nil, &UnknownEventTypeError{Type: tag}
	}
}

// delegateDeliveryStateChangedWire is the deliberately closed wire shape for
// DelegateDeliveryStateChanged. Unlike ordinary plain events, this record is a
// durable ABI boundary: accepting an extra field could let transport-only or
// model-visible data leak into the journal. Keep the envelope keys and Header
// fields explicit through embedding, and reject every other key below.
type delegateDeliveryStateChangedWire struct {
	Type string  `json:"type"`
	V    *uint32 `json:"v"`
	Header
	RequestID    uuid.UUID             `json:"request_id"`
	TargetLoopID uuid.UUID             `json:"target_loop_id"`
	State        DelegateDeliveryState `json:"state"`
}

func decodeDelegateDeliveryStateChanged(data []byte) (Event, error) {
	const tag = "DelegateDeliveryStateChanged"
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()

	var wire delegateDeliveryStateChangedWire
	if err := decoder.Decode(&wire); err != nil {
		return nil, &EventDecodeError{Type: tag, Cause: err}
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			err = errors.New("trailing JSON value")
		}
		return nil, &EventDecodeError{Type: tag, Cause: err}
	}

	return DelegateDeliveryStateChanged{
		Header:       wire.Header,
		RequestID:    wire.RequestID,
		TargetLoopID: wire.TargetLoopID,
		State:        wire.State,
	}, nil
}

// workflowActivityWire is deliberately explicit. WorkflowActivity is a public
// journal ABI, so unlike ordinary plain events its decoder must reject a future
// or transport-only field instead of silently accepting and dropping it.
type workflowActivityWire struct {
	Type string  `json:"type"`
	V    *uint32 `json:"v"`
	Header
	RunID             uuid.UUID            `json:"run_id"`
	WorkflowName      string               `json:"workflow_name"`
	WorkflowVersion   string               `json:"workflow_version"`
	Kind              WorkflowActivityKind `json:"kind"`
	Status            WorkflowRunStatus    `json:"status"`
	VertexID          uuid.UUID            `json:"vertex_id,omitzero"`
	VertexLabel       string               `json:"vertex_label,omitempty"`
	CompletedVertices uint32               `json:"completed_vertices,omitzero"`
	TotalVertices     uint32               `json:"total_vertices,omitzero"`
	Message           string               `json:"message,omitempty"`
	OccurredAt        time.Time            `json:"occurred_at"`
}

func decodeWorkflowActivity(data []byte) (Event, error) {
	const tag = "WorkflowActivity"
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()

	var wire workflowActivityWire
	if err := decoder.Decode(&wire); err != nil {
		return nil, &EventDecodeError{Type: tag, Cause: err}
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			err = errors.New("trailing JSON value")
		}
		return nil, &EventDecodeError{Type: tag, Cause: err}
	}
	if wire.Type != "" && wire.Type != tag {
		return nil, &EventDecodeError{Type: tag, Cause: fmt.Errorf("unexpected type %q", wire.Type)}
	}

	return WorkflowActivity{
		Header:            wire.Header,
		RunID:             wire.RunID,
		WorkflowName:      wire.WorkflowName,
		WorkflowVersion:   wire.WorkflowVersion,
		Kind:              wire.Kind,
		Status:            wire.Status,
		VertexID:          wire.VertexID,
		VertexLabel:       wire.VertexLabel,
		CompletedVertices: wire.CompletedVertices,
		TotalVertices:     wire.TotalVertices,
		Message:           wire.Message,
		OccurredAt:        wire.OccurredAt,
	}, nil
}

func decodeStepDone(data []byte) (Event, error) {
	var w stepDoneWire
	if err := json.Unmarshal(data, &w); err != nil {
		return nil, &EventDecodeError{Type: "StepDone", Cause: err}
	}
	var msgs content.AgenticMessages
	if len(w.Messages) > 0 {
		m, err := unmarshalMessages(w.Messages)
		if err != nil {
			return nil, &EventDecodeError{Type: "StepDone", Cause: err}
		}
		msgs = m
	}
	return StepDone{Header: w.Header, Messages: msgs}, nil
}

func decodePermissionRequested(data []byte) (Event, error) {
	var w permissionRequestedWire
	if err := json.Unmarshal(data, &w); err != nil {
		return nil, &EventDecodeError{Type: "PermissionRequested", Cause: err}
	}
	ev := PermissionRequested{Header: w.Header, ToolExecutionID: w.ToolExecutionID}
	if len(w.Request) > 0 {
		req, err := gate.DecodeRequest(w.Request)
		if err != nil {
			return nil, &EventDecodeError{Type: "PermissionRequested", Cause: err}
		}
		ev.Request = req
	}
	return ev, nil
}

func decodeTurnFailed(data []byte) (Event, error) {
	var w turnFailedWire
	if err := json.Unmarshal(data, &w); err != nil {
		return nil, &EventDecodeError{Type: "TurnFailed", Cause: err}
	}
	ev := TurnFailed{Header: w.Header, TurnIndex: w.TurnIndex}
	if w.Err != nil {
		ev.Err = w.Err.errorValue()
	}
	return ev, nil
}

func decodeRestoreErrored(data []byte) (Event, error) {
	var w restoreErroredWire
	if err := json.Unmarshal(data, &w); err != nil {
		return nil, &EventDecodeError{Type: "RestoreErrored", Cause: err}
	}
	ev := RestoreErrored{Header: w.Header}
	if w.Err != nil {
		ev.Err = w.Err.errorValue()
	}
	return ev, nil
}

// gateResolvedWire is GateResolved's wire form: the Audit sealed interface is
// persisted IN FULL via gate.MarshalResponseAudit (a bare interface would marshal
// to lossy, un-keyed output), pre-encoded into a sibling "audit" key. A nil Audit
// serializes to an absent key.
type gateResolvedWire struct {
	Header
	GateID   gate.ID             `json:"gate_id,omitzero"`
	Resolver gate.ResolverKind   `json:"resolver,omitempty"`
	Reason   gate.CloseReason    `json:"reason,omitempty"`
	Action   string              `json:"action,omitempty"`
	Source   gate.ResponseSource `json:"source,omitzero"`
	Audit    json.RawMessage     `json:"audit,omitempty"`
}

func marshalGateResolved(e GateResolved) ([]byte, error) {
	var auditJSON json.RawMessage
	if e.Audit != nil {
		a, err := gate.MarshalResponseAudit(e.Audit)
		if err != nil {
			return nil, &EventEncodeError{Type: "GateResolved", Cause: err}
		}
		auditJSON = a
	}
	out, err := json.Marshal(gateResolvedWire{
		Header:   e.Header,
		GateID:   e.GateID,
		Resolver: e.Resolver,
		Reason:   e.Reason,
		Action:   e.Action,
		Source:   e.Source,
		Audit:    auditJSON,
	})
	if err != nil {
		return nil, &EventEncodeError{Type: "GateResolved", Cause: err}
	}
	return out, nil
}

func decodeGateResolved(data []byte) (Event, error) {
	var w gateResolvedWire
	if err := json.Unmarshal(data, &w); err != nil {
		return nil, &EventDecodeError{Type: "GateResolved", Cause: err}
	}
	ev := GateResolved{
		Header:   w.Header,
		GateID:   w.GateID,
		Resolver: w.Resolver,
		Reason:   w.Reason,
		Action:   w.Action,
		Source:   w.Source,
	}
	if len(w.Audit) > 0 {
		audit, err := gate.UnmarshalResponseAudit(w.Audit)
		if err != nil {
			return nil, &EventDecodeError{Type: "GateResolved", Cause: err}
		}
		ev.Audit = audit
	}
	return ev, nil
}

// maxMessagesPerStep caps the element count of a committed step group accepted at
// the untrusted restore boundary. A step group is one AIMessage plus its tool
// results, so a generous cap still fails closed on absurd input. Each message's
// blocks are independently capped by the content block codec.
const maxMessagesPerStep = 10_000

// UnknownMessageRoleError is returned by the message-slice decoder when a message's
// "role" names no concrete Conversation type (including an empty/missing role). The
// Conversation union is sealed and discriminated by role; an unknown role fails
// closed rather than guess a concrete message to reconstruct.
type UnknownMessageRoleError struct{ Role string }

func (e *UnknownMessageRoleError) Error() string {
	return fmt.Sprintf("event: unknown message role %q", e.Role)
}

// marshalMessages encodes a content.AgenticMessages (an ordered []Conversation, a
// sealed interface that has no general struct codec) as a JSON array. Each element
// is marshaled by its own MarshalJSON, which stamps the "role" discriminator and
// tags nested blocks via the content block codec — so the array is self-describing
// and the decoder can reconstruct the concrete type from role alone.
func marshalMessages(msgs content.AgenticMessages) ([]byte, error) {
	raws := make([]json.RawMessage, len(msgs))
	for i, m := range msgs {
		r, err := json.Marshal(m)
		if err != nil {
			return nil, err
		}
		raws[i] = r
	}
	return json.Marshal(raws)
}

// unmarshalMessages decodes a JSON array of role-tagged messages back into a
// content.AgenticMessages, allocating the concrete type for each element from its
// "role" and delegating block decoding to the content message codecs. It enforces
// the element-count cap and fails closed on an unknown role.
func unmarshalMessages(data []byte) (content.AgenticMessages, error) {
	var raws []json.RawMessage
	if err := json.Unmarshal(data, &raws); err != nil {
		return nil, err
	}
	if len(raws) > maxMessagesPerStep {
		return nil, &EventLimitError{Got: len(raws), Max: maxMessagesPerStep}
	}
	if len(raws) == 0 {
		// An empty array decodes to nil, matching the marshal path (omitempty drops
		// a nil/empty slice) so the codec is a stable fixed point under re-marshal.
		return nil, nil
	}
	msgs := make(content.AgenticMessages, 0, len(raws))
	for _, r := range raws {
		if bytes.Equal(bytes.TrimSpace(r), []byte("null")) {
			msgs = append(msgs, nil)
			continue
		}
		m, err := unmarshalMessage(r)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	return msgs, nil
}

// unmarshalMessage reads the "role" discriminator and decodes the bytes into the
// matching concrete Conversation type. RoleTool maps to ToolResultMessage (which
// carries tool_use_id/is_error and defines its own codec); the other roles map to
// the role-only message wrappers. Pointer forms are returned because every
// Conversation implementor satisfies the interface with a pointer receiver, mirror-
// ing how the messages are constructed and committed in the loop.
func unmarshalMessage(data []byte) (content.Conversation, error) {
	var probe struct {
		Role content.Role `json:"role"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, err
	}
	switch probe.Role {
	case content.RoleUser:
		return decodeMessage[content.UserMessage](data)
	case content.RoleAssistant:
		return decodeMessage[content.AIMessage](data)
	case content.RoleSystem:
		return decodeMessage[content.SystemMessage](data)
	case content.RoleTool:
		return decodeMessage[content.ToolResultMessage](data)
	default:
		return nil, &UnknownMessageRoleError{Role: string(probe.Role)}
	}
}

// decodeMessage unmarshals data into a freshly allocated *T and returns it as a
// content.Conversation. Each *T satisfies Conversation, and its UnmarshalJSON
// (promoted from Message or defined on ToolResultMessage) decodes nested blocks via
// the content codec.
func decodeMessage[T any](data []byte) (content.Conversation, error) {
	v := new(T)
	if err := json.Unmarshal(data, v); err != nil {
		return nil, err
	}
	c, ok := any(v).(content.Conversation)
	if !ok {
		// Unreachable: each *T in the switch is a Conversation. Fail secure.
		return nil, &UnknownMessageRoleError{}
	}
	return c, nil
}
