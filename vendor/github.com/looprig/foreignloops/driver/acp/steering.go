package acp

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/looprig/acp/client"
	"github.com/looprig/acp/protocol"
	"github.com/looprig/core/content"
	"github.com/looprig/foreignloops/driver"
)

const (
	claudeAgentACPName      = "@agentclientprotocol/claude-agent-acp"
	claudeSteeringVersion   = "0.65.0"
	codexAgentACPName       = "@agentclientprotocol/codex-acp"
	legacyCodexAgentACPName = "codex-acp"
	codexSteeringVersion    = "1.1.9"
)

// steeringCapability applies the ACP steering policy at the initialize
// boundary. A capability advertisement is useful only when it proves both
// steering support and a host-owned idle fallback. The exact Claude 0.65.0
// profile is the sole compatibility exception for older advertisements.
// Current Codex ACP is deliberately excluded because its idle race starts an
// adapter-owned turn that Carbon cannot correlate.
func steeringCapability(harness Harness, metadata client.InitializeMetadata) bool {
	if harness != HarnessClaudeCode && harness != HarnessCodex {
		return false
	}
	supported, safeIdle := parseSteeringAdvertisement(metadata.Meta)
	if !supported {
		return false
	}
	if metadata.AgentInfo == nil {
		return safeIdle
	}
	name, version := metadata.AgentInfo.Name, metadata.AgentInfo.Version
	if harness == HarnessCodex && (version == codexSteeringVersion || isCurrentCodexProfile(name, version)) {
		return false
	}
	if safeIdle {
		return true
	}
	return harness == HarnessClaudeCode && isExactClaudeProfile(name, version)
}

func parseSteeringAdvertisement(raw json.RawMessage) (supported, safeIdle bool) {
	if len(raw) == 0 {
		return false, false
	}
	var wire struct {
		Steering *struct {
			Supported     bool     `json:"supported"`
			IdleBehaviors []string `json:"idleBehaviors"`
		} `json:"steering"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil || wire.Steering == nil {
		return false, false
	}
	if !wire.Steering.Supported {
		return false, false
	}
	for _, behavior := range wire.Steering.IdleBehaviors {
		if behavior == "promptRequired" {
			return true, true
		}
	}
	return true, false
}

func isExactClaudeProfile(name, version string) bool {
	return name == claudeAgentACPName && version == claudeSteeringVersion
}

func isCurrentCodexProfile(name, version string) bool {
	return (name == codexAgentACPName || name == legacyCodexAgentACPName) && version == codexSteeringVersion
}

func cloneMcpServers(servers []protocol.McpServer) []protocol.McpServer {
	if servers == nil {
		return nil
	}
	cloned := make([]protocol.McpServer, len(servers))
	for i, server := range servers {
		cloned[i] = cloneMcpServer(server)
	}
	return cloned
}

func cloneMcpServer(server protocol.McpServer) protocol.McpServer {
	cloned := protocol.McpServer{}
	if server.HTTP != nil {
		value := *server.HTTP
		value.Headers = cloneMcpHeaders(value.Headers)
		value.Meta = cloneRawMessage(value.Meta)
		cloned.HTTP = &value
	}
	if server.Sse != nil {
		value := *server.Sse
		value.Headers = cloneMcpHeaders(value.Headers)
		value.Meta = cloneRawMessage(value.Meta)
		cloned.Sse = &value
	}
	if server.Stdio != nil {
		value := *server.Stdio
		value.Args = append([]string(nil), value.Args...)
		value.Env = cloneMcpEnv(value.Env)
		value.Meta = cloneRawMessage(value.Meta)
		cloned.Stdio = &value
	}
	return cloned
}

func cloneMcpHeaders(headers []protocol.HTTPHeader) []protocol.HTTPHeader {
	if headers == nil {
		return nil
	}
	cloned := make([]protocol.HTTPHeader, len(headers))
	for i, header := range headers {
		cloned[i] = header
		cloned[i].Meta = cloneRawMessage(header.Meta)
	}
	return cloned
}

func cloneMcpEnv(env []protocol.EnvVariable) []protocol.EnvVariable {
	if env == nil {
		return nil
	}
	cloned := make([]protocol.EnvVariable, len(env))
	for i, variable := range env {
		cloned[i] = variable
		cloned[i].Meta = cloneRawMessage(variable.Meta)
	}
	return cloned
}

func cloneRawMessage(raw json.RawMessage) json.RawMessage {
	if raw == nil {
		return nil
	}
	cloned := make(json.RawMessage, len(raw))
	copy(cloned, raw)
	return cloned
}

const steeringIdleMeta = `{"steering":{"idleBehavior":"promptRequired"}}`

// normalizeSteering converts the fixed ACP extension result into the closed
// provider-neutral outcome set. Any admitted error or unrecognized result is
// ambiguous and therefore cannot be retried automatically.
func normalizeSteering(result client.SteerResult, callErr error) (driver.SteerResult, error) {
	if steeringErr, ok := callErr.(*client.SteeringError); ok && steeringErr != nil {
		if !result.WriteAdmitted {
			result.WriteAdmitted = steeringErr.WriteAdmitted
		}
		if result.ReceiveSequence == 0 {
			result.ReceiveSequence = steeringErr.ReceiveSequence
		}
		if result.ResponseSequence == 0 {
			result.ResponseSequence = steeringErr.ResponseSequence
		}
	}
	normalized := driver.SteerResult{
		Reason:           result.Reason,
		WriteAdmitted:    result.WriteAdmitted,
		ReceiveSequence:  result.ReceiveSequence,
		ResponseSequence: result.ResponseSequence,
	}
	if callErr != nil {
		if !result.WriteAdmitted || steeringErrorGuaranteesNoDelivery(callErr) {
			normalized.Outcome = driver.SteerOutcomeFallbackRequired
		} else {
			normalized.Outcome = driver.SteerOutcomeDeliveryUnknown
		}
		return normalized, callErr
	}

	switch result.Outcome {
	case client.SteerOutcomeInjected:
		normalized.Outcome = driver.SteerOutcomeInjected
	case client.SteerOutcomePromptRequired, client.SteerOutcomeFailed:
		normalized.Outcome = driver.SteerOutcomeFallbackRequired
	case client.SteerOutcomeStartedNewTurn:
		normalized.Outcome = driver.SteerOutcomeDeliveredUntrackable
	default:
		normalized.Outcome = driver.SteerOutcomeDeliveryUnknown
		return normalized, errors.New("acp: unrecognized steering outcome")
	}
	if err := normalized.Validate(); err != nil {
		normalized.Outcome = driver.SteerOutcomeDeliveryUnknown
		return normalized, fmt.Errorf("acp: malformed steering result: %w", err)
	}
	return normalized, nil
}

func steeringErrorGuaranteesNoDelivery(err error) bool {
	var steeringErr *client.SteeringError
	if !errors.As(err, &steeringErr) || steeringErr == nil {
		return false
	}
	return steeringErr.Code == protocol.ErrorCodeMethodNotFound || steeringErr.Code == protocol.ErrorCodeInvalidParams
}

func steerPromptBlocks(request driver.SteerRequest) ([]protocol.ContentBlock, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	blocks := request.Prompt()
	converted := make([]protocol.ContentBlock, 0, len(blocks))
	for i, block := range blocks {
		switch typed := block.(type) {
		case *content.TextBlock:
			if typed == nil {
				return nil, fmt.Errorf("acp: steering prompt[%d] is nil", i)
			}
			converted = append(converted, protocol.ContentBlock{Text: &protocol.TextContent{Text: typed.Text}})
		case *content.DocumentBlock:
			if typed == nil || typed.Text == "" {
				return nil, fmt.Errorf("acp: steering prompt[%d] document is not textual", i)
			}
			converted = append(converted, protocol.ContentBlock{Text: &protocol.TextContent{Text: typed.Text}})
		default:
			return nil, fmt.Errorf("acp: steering prompt[%d] content is unsupported", i)
		}
	}
	return converted, nil
}
