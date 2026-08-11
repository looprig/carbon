package delegationtool

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	inferencemodel "github.com/looprig/inference/model"
)

const (
	maxAgentArgsBytes    = 256 << 10
	maxAgentNameBytes    = 256
	maxAgentMessageBytes = 192 << 10
	maxAgentTypeBytes    = 128
	maxAgentModeBytes    = 128
	maxTimeoutSeconds    = 24 * 60 * 60
)

const (
	errCategoryOversized       = "oversized"
	errCategoryMalformed       = "malformed"
	errCategoryUnknownField    = "unknown_field"
	errCategoryFieldNotAllowed = "field_not_allowed"
	errCategoryMissingField    = "missing_field"
	errCategoryInvalidValue    = "invalid_value"
	errCategoryUnknownRuntime  = "unknown_runtime"
)

var errPreparationSentinel = errors.New("agent preparation rejected")

type preparationError struct{ category string }

func (e *preparationError) Error() string      { return errPreparationSentinel.Error() + ": " + e.category }
func (e *preparationError) Unwrap() error      { return errPreparationSentinel }
func preparationFailure(category string) error { return &preparationError{category: category} }

type startAgentWire struct {
	AgentType       string  `json:"agent_type"`
	Name            string  `json:"name,omitempty"`
	Instructions    string  `json:"instructions"`
	WaitForResponse *bool   `json:"wait_for_response,omitempty"`
	TimeoutSeconds  *int    `json:"timeout_seconds,omitempty"`
	AgentHarness    string  `json:"agent_harness,omitempty"`
	AgentSource     string  `json:"agent_source,omitempty"`
	Model           string  `json:"model,omitempty"`
	Effort          *string `json:"effort,omitempty"`
	AgentMode       string  `json:"agent_mode,omitempty"`
}

type messageAgentWire struct {
	AgentID         *string `json:"agent_id"`
	Message         string  `json:"message"`
	WaitForResponse *bool   `json:"wait_for_response,omitempty"`
	TimeoutSeconds  *int    `json:"timeout_seconds,omitempty"`
}

type listAgentsWire struct {
	AgentID *string `json:"agent_id,omitempty"`
}

type stopAgentWire struct {
	AgentID *string `json:"agent_id"`
}

// PreparedStartAgent contains validated StartAgent arguments ready for delegation.
type PreparedStartAgent struct {
	AgentType       string
	Name            string
	Instructions    string
	WaitForResponse bool
	TimeoutSeconds  *int
	AgentHarness    string
	AgentSource     string
	Model           string
	Effort          *string
	AgentMode       string
	Runtime         *tool.DelegateRuntime

	agentHarnessSet bool
	agentSourceSet  bool
	modelSet        bool
	effortSet       bool
	agentModeSet    bool
}

// PreparedMessageAgent contains validated MessageAgent arguments ready for delegation.
type PreparedMessageAgent struct {
	AgentID         uuid.UUID
	Message         string
	WaitForResponse bool
	TimeoutSeconds  *int
}

// PreparedListAgents contains validated ListAgents arguments ready for delegation.
type PreparedListAgents struct{ AgentID *uuid.UUID }

// PreparedStopAgent contains validated StopAgent arguments ready for delegation.
type PreparedStopAgent struct{ AgentID uuid.UUID }

func prepareStartAgent(argsJSON string) (PreparedStartAgent, error) {
	var wire startAgentWire
	present, err := decodeStrictAgentJSON(argsJSON, &wire)
	if err != nil {
		return PreparedStartAgent{}, err
	}
	if err := requireText("agent_type", wire.AgentType, present, maxAgentTypeBytes); err != nil {
		return PreparedStartAgent{}, err
	}
	if err := requireText("instructions", wire.Instructions, present, maxAgentMessageBytes); err != nil {
		return PreparedStartAgent{}, err
	}
	if err := validateOptionalText("name", wire.Name, present, maxAgentNameBytes); err != nil {
		return PreparedStartAgent{}, err
	}
	if err := validateOptionalText("agent_harness", wire.AgentHarness, present, maxAgentTypeBytes); err != nil {
		return PreparedStartAgent{}, err
	}
	if err := validateOptionalText("agent_source", wire.AgentSource, present, maxAgentTypeBytes); err != nil {
		return PreparedStartAgent{}, err
	}
	if err := validateOptionalText("model", wire.Model, present, maxAgentTypeBytes); err != nil {
		return PreparedStartAgent{}, err
	}
	if err := validateOptionalText("agent_mode", wire.AgentMode, present, maxAgentModeBytes); err != nil {
		return PreparedStartAgent{}, err
	}
	if err := validateWaitAndTimeout(wire.WaitForResponse, wire.TimeoutSeconds, present); err != nil {
		return PreparedStartAgent{}, err
	}
	if wire.Effort != nil {
		switch *wire.Effort {
		case "none", "low", "medium", "high", "max":
		default:
			return PreparedStartAgent{}, preparationFailure(errCategoryInvalidValue)
		}
	} else if _, supplied := present["effort"]; supplied {
		return PreparedStartAgent{}, preparationFailure(errCategoryInvalidValue)
	}
	return PreparedStartAgent{
		AgentType: wire.AgentType, Name: wire.Name, Instructions: wire.Instructions,
		WaitForResponse: waitForResponse(wire.WaitForResponse), TimeoutSeconds: wire.TimeoutSeconds,
		AgentHarness: wire.AgentHarness, AgentSource: wire.AgentSource, Model: wire.Model,
		Effort: wire.Effort, AgentMode: wire.AgentMode,
		agentHarnessSet: hasField(present, "agent_harness"), agentSourceSet: hasField(present, "agent_source"),
		modelSet: hasField(present, "model"), effortSet: hasField(present, "effort"), agentModeSet: hasField(present, "agent_mode"),
	}, nil
}

func prepareMessageAgent(argsJSON string) (PreparedMessageAgent, error) {
	var wire messageAgentWire
	present, err := decodeStrictAgentJSON(argsJSON, &wire)
	if err != nil {
		return PreparedMessageAgent{}, err
	}
	id, err := requireAgentID(wire.AgentID, present)
	if err != nil {
		return PreparedMessageAgent{}, err
	}
	if err := requireText("message", wire.Message, present, maxAgentMessageBytes); err != nil {
		return PreparedMessageAgent{}, err
	}
	if err := validateWaitAndTimeout(wire.WaitForResponse, wire.TimeoutSeconds, present); err != nil {
		return PreparedMessageAgent{}, err
	}
	return PreparedMessageAgent{AgentID: id, Message: wire.Message, WaitForResponse: waitForResponse(wire.WaitForResponse), TimeoutSeconds: wire.TimeoutSeconds}, nil
}

func prepareListAgents(argsJSON string) (PreparedListAgents, error) {
	var wire listAgentsWire
	present, err := decodeStrictAgentJSON(argsJSON, &wire)
	if err != nil {
		return PreparedListAgents{}, err
	}
	if !hasField(present, "agent_id") {
		return PreparedListAgents{}, nil
	}
	id, err := parseAgentID(wire.AgentID)
	if err != nil {
		return PreparedListAgents{}, err
	}
	return PreparedListAgents{AgentID: &id}, nil
}

func prepareStopAgent(argsJSON string) (PreparedStopAgent, error) {
	var wire stopAgentWire
	present, err := decodeStrictAgentJSON(argsJSON, &wire)
	if err != nil {
		return PreparedStopAgent{}, err
	}
	id, err := requireAgentID(wire.AgentID, present)
	if err != nil {
		return PreparedStopAgent{}, err
	}
	return PreparedStopAgent{AgentID: id}, nil
}

func decodeStrictAgentJSON(argsJSON string, destination any) (map[string]json.RawMessage, error) {
	if len(argsJSON) > maxAgentArgsBytes {
		return nil, preparationFailure(errCategoryOversized)
	}
	if !utf8.ValidString(argsJSON) {
		return nil, preparationFailure(errCategoryMalformed)
	}
	raw, err := oneJSONValue([]byte(argsJSON))
	if err != nil {
		return nil, preparationFailure(errCategoryMalformed)
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, preparationFailure(errCategoryMalformed)
	}
	present := make(map[string]json.RawMessage)
	if err := json.Unmarshal(trimmed, &present); err != nil || present == nil {
		return nil, preparationFailure(errCategoryMalformed)
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		if strings.Contains(err.Error(), "unknown field") {
			return nil, preparationFailure(errCategoryUnknownField)
		}
		return nil, preparationFailure(errCategoryMalformed)
	}
	return present, nil
}

func oneJSONValue(input []byte) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(input))
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		return nil, err
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errors.New("trailing JSON")
	}
	return raw, nil
}

func waitForResponse(value *bool) bool { return value == nil || *value }
func hasField(present map[string]json.RawMessage, name string) bool {
	_, ok := present[name]
	return ok
}

func validateWaitAndTimeout(wait *bool, timeout *int, present map[string]json.RawMessage) error {
	if wait == nil && hasField(present, "wait_for_response") {
		return preparationFailure(errCategoryInvalidValue)
	}
	if timeout == nil && hasField(present, "timeout_seconds") {
		return preparationFailure(errCategoryInvalidValue)
	}
	if timeout != nil && (*timeout < 0 || *timeout > maxTimeoutSeconds) {
		return preparationFailure(errCategoryInvalidValue)
	}
	return nil
}

func requireText(name, value string, present map[string]json.RawMessage, limit int) error {
	if !hasField(present, name) {
		return preparationFailure(errCategoryMissingField)
	}
	if strings.TrimSpace(value) == "" || len(value) > limit || !utf8.ValidString(value) {
		return preparationFailure(errCategoryInvalidValue)
	}
	return nil
}

func validateOptionalText(name, value string, present map[string]json.RawMessage, limit int) error {
	if !hasField(present, name) {
		return nil
	}
	if strings.TrimSpace(value) == "" || len(value) > limit || !utf8.ValidString(value) {
		return preparationFailure(errCategoryInvalidValue)
	}
	return nil
}

func requireAgentID(value *string, present map[string]json.RawMessage) (uuid.UUID, error) {
	if !hasField(present, "agent_id") {
		return uuid.UUID{}, preparationFailure(errCategoryMissingField)
	}
	return parseAgentID(value)
}

func parseAgentID(value *string) (uuid.UUID, error) {
	if value == nil || len(*value) > 36 || !utf8.ValidString(*value) {
		return uuid.UUID{}, preparationFailure(errCategoryInvalidValue)
	}
	id, err := uuid.Parse(*value)
	if err != nil || id.IsZero() {
		return uuid.UUID{}, preparationFailure(errCategoryInvalidValue)
	}
	return id, nil
}

func (s *agentToolConfig) prepareStartAgent(argsJSON string) (PreparedStartAgent, error) {
	prepared, err := prepareStartAgent(argsJSON)
	if err != nil {
		return PreparedStartAgent{}, err
	}
	if !s.hasAgentType(prepared.AgentType) {
		return PreparedStartAgent{}, preparationFailure(errCategoryUnknownRuntime)
	}
	if err := s.validateAgentMode(prepared); err != nil {
		return PreparedStartAgent{}, err
	}
	runtime, err := s.resolveDelegateRuntime(prepared)
	if err != nil {
		return PreparedStartAgent{}, err
	}
	prepared.Runtime = runtime
	return prepared, nil
}

func (s *agentToolConfig) validateAgentMode(prepared PreparedStartAgent) error {
	for _, role := range s.catalog {
		if string(role.Name) != prepared.AgentType {
			continue
		}
		if !prepared.agentModeSet {
			return nil
		}
		modes := selectableAgentModes(role.Modes)
		if len(modes) < 2 {
			return preparationFailure(errCategoryFieldNotAllowed)
		}
		for _, mode := range modes {
			if mode == prepared.AgentMode {
				return nil
			}
		}
		return preparationFailure(errCategoryInvalidValue)
	}
	return preparationFailure(errCategoryUnknownRuntime)
}

func (s *agentToolConfig) resolveDelegateRuntime(prepared PreparedStartAgent) (*tool.DelegateRuntime, error) {
	if !s.hasRuntimeCatalog {
		if prepared.agentHarnessSet || prepared.agentSourceSet || prepared.modelSet || prepared.effortSet {
			return nil, preparationFailure(errCategoryFieldNotAllowed)
		}
		return nil, nil
	}
	entries := s.runtimeCatalog.EntriesFor(identity.AgentName(prepared.AgentType))
	if len(entries) == 0 {
		if !s.runtimeCatalog.HasEntries() {
			if prepared.agentHarnessSet || prepared.agentSourceSet || prepared.modelSet || prepared.effortSet {
				return nil, preparationFailure(errCategoryFieldNotAllowed)
			}
			return nil, nil
		}
		return nil, preparationFailure(errCategoryUnknownRuntime)
	}
	selected := runtimeDefaultEntry(entries)
	selectedHarness := selected.AgentHarness
	if prepared.agentHarnessSet {
		if !runtimeHarnessSelectable(entries) {
			return nil, preparationFailure(errCategoryFieldNotAllowed)
		}
		selectedHarness = loop.AgentHarnessName(prepared.AgentHarness)
		harnessEntries := runtimeEntriesForHarness(entries, selectedHarness)
		if len(harnessEntries) == 0 {
			return nil, preparationFailure(errCategoryUnknownRuntime)
		}
		selected = runtimeDefaultEntry(harnessEntries)
	}
	harnessEntries := runtimeEntriesForHarness(entries, selectedHarness)
	if prepared.agentSourceSet {
		if !runtimeSourceSelectableForEntries(harnessEntries) {
			return nil, preparationFailure(errCategoryFieldNotAllowed)
		}
		var found bool
		selected, found = runtimeEntryForSource(harnessEntries, loop.RuntimeSourceName(prepared.AgentSource))
		if !found {
			return nil, preparationFailure(errCategoryUnknownRuntime)
		}
	}
	if !prepared.agentSourceSet && runtimeSourceSelectableForEntries(harnessEntries) {
		if filtered, ok := runtimeEntryForSource([]loop.RuntimeCatalogEntry{selected}, selected.Source); ok {
			selected = filtered
		}
	}
	advertised := runtimeAdvertisedSelectors(entries, selected)
	if prepared.agentSourceSet && !advertised.Source {
		return nil, preparationFailure(errCategoryFieldNotAllowed)
	}
	if prepared.modelSet {
		if !advertised.Model {
			return nil, preparationFailure(errCategoryFieldNotAllowed)
		}
		found := false
		for _, option := range selected.Models {
			if string(option.Alias) == prepared.Model {
				found = true
				break
			}
		}
		if !found {
			return nil, preparationFailure(errCategoryUnknownRuntime)
		}
	}
	var effort inferencemodel.Effort
	if prepared.effortSet {
		if !advertised.Effort {
			return nil, preparationFailure(errCategoryFieldNotAllowed)
		}
		effort = parseDelegateEffort(*prepared.Effort)
	}
	selectedSource := loop.RuntimeSourceName("")
	if prepared.agentSourceSet {
		selectedSource = loop.RuntimeSourceName(prepared.AgentSource)
	}
	resolved, err := s.runtimeCatalog.ResolveWithExplicitSource(identity.AgentName(prepared.AgentType), selectedHarness, selectedSource, loop.ModelAlias(prepared.Model), effort, prepared.effortSet)
	if err != nil {
		return nil, preparationFailure(errCategoryUnknownRuntime)
	}
	runtimeModel := string(resolved.ModelAlias)
	runtimeEffort := delegateEffortString(resolved.Effort)
	if resolved.SelectionKind == loop.RuntimeSelectionHarnessManaged {
		runtimeModel, runtimeEffort = "", ""
	}
	return &tool.DelegateRuntime{
		Harness: string(resolved.AgentHarness), Profile: string(resolved.Profile), Source: string(resolved.Source), SelectionKind: string(resolved.SelectionKind),
		Model: runtimeModel, SmallModel: string(resolved.SmallModel), Effort: runtimeEffort,
		Explicit: tool.DelegateRuntimeExplicit{Harness: prepared.agentHarnessSet, Source: prepared.agentSourceSet, Model: prepared.modelSet, Effort: prepared.effortSet},
	}, nil
}

func (s *agentToolConfig) hasAgentType(agent string) bool {
	for _, entry := range s.catalog {
		if string(entry.Name) == agent {
			return true
		}
	}
	return false
}

func delegateEffortString(effort inferencemodel.Effort) string {
	if effort == inferencemodel.EffortNone {
		return "none"
	}
	return string(effort)
}

func parseDelegateEffort(value string) inferencemodel.Effort {
	if value == "none" {
		return inferencemodel.EffortNone
	}
	return inferencemodel.Effort(value)
}
