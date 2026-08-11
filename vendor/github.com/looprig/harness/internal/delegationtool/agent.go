package delegationtool

import (
	"context"
	"encoding/json"
	"unicode/utf8"

	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

const maxAgentResultBytes = 256 << 10

// AgentCatalogEntry is one child capability advertised to StartAgent.
type AgentCatalogEntry struct {
	Name        identity.AgentName
	Description string
	Modes       []loop.ModeName
}

// agentToolConfig is an immutable, parent-scoped catalogue snapshot shared by
// the four tools built as one atomic bundle.
type agentToolConfig struct {
	style             loop.DelegationStyle
	catalog           []AgentCatalogEntry
	runtimeCatalog    loop.RuntimeCatalog
	hasRuntimeCatalog bool
}

func newAgentToolConfig(style loop.DelegationStyle, catalog []AgentCatalogEntry, runtimeCatalog ...loop.RuntimeCatalog) *agentToolConfig {
	config := &agentToolConfig{style: style, catalog: cloneAgentCatalog(catalog), hasRuntimeCatalog: true}
	if len(runtimeCatalog) > 0 {
		config.runtimeCatalog = runtimeCatalog[0]
	}
	return config
}

func cloneAgentCatalog(catalog []AgentCatalogEntry) []AgentCatalogEntry {
	result := append([]AgentCatalogEntry(nil), catalog...)
	for i := range result {
		result[i].Modes = append([]loop.ModeName(nil), result[i].Modes...)
	}
	return result
}

func (c *agentToolConfig) startSchema() string {
	return buildStartAgentSchema(c.style, c.catalog, c.runtimeCatalog)
}

func executeAgentCall(ctx context.Context, controller tool.DelegateController, operation tool.DelegateOperation, format func(tool.DelegateRequest, tool.DelegateResult) string) (*tool.ToolResult, error) {
	prepared, ok := loop.PreparedCallFromContext(ctx)
	if !ok {
		return tool.TextResult("error: agent call unavailable"), nil
	}
	artifact, ok := prepared.Artifact.(tool.DelegateArtifact)
	if !ok || artifact.Request.Operation != operation {
		return tool.TextResult("error: agent call unavailable"), nil
	}
	req := artifact.Request
	req.Runtime = artifact.Runtime
	req.ParentToolUseID = ""
	if toolUseID, present := loop.ToolUseIDFrom(ctx); present {
		req.ParentToolUseID = toolUseID
	}
	result, err := controller.Execute(ctx, req)
	if err != nil {
		if operation == tool.DelegateStart {
			if detail, marked := tool.ModelFacingErrorDetail(err); marked {
				return tool.TextResult(formatStartAgentFailure(detail)), nil
			}
			return tool.TextResult("error: agent failed"), nil
		}
		if detail, marked := tool.ModelFacingErrorDetail(err); marked && detail != "" {
			return tool.TextResult("error: " + tool.BoundModelFacingErrorDetail(detail)), nil
		}
		return tool.TextResult("error: agent request failed"), nil
	}
	return tool.TextResult(format(req, result)), nil
}

func formatForeground(result tool.DelegateResult) string {
	if result.DeliveryStatus != "" {
		responseStatus := wireResponseStatus(result.ResponseStatus)
		var response *string
		if result.DeliveryStatus == tool.DelegateDeliveryUnknown || result.DeliveryStatus == tool.DelegateDeliveryUntrackable {
			responseStatus = ""
		} else if result.ResponseStatus == tool.DelegateResponseCompleted || (responseStatus != "" && result.Response != "") {
			response = &result.Response
		}
		return marshalForegroundResult(foregroundResult{
			AgentID:        result.AgentID.String(),
			Name:           result.Name,
			State:          result.State,
			DeliveryStatus: result.DeliveryStatus,
			ResponseStatus: responseStatus,
			Response:       response,
		})
	}
	switch result.ResponseStatus {
	case tool.DelegateResponseCompleted:
		return marshalForegroundResult(foregroundResult{AgentID: result.AgentID.String(), Name: result.Name, State: result.State, Response: &result.Response})
	case tool.DelegateResponseFailed:
		return "error: agent failed"
	case tool.DelegateResponseInterrupted:
		return "error: agent interrupted"
	case tool.DelegateResponseTimedOut:
		return "error: agent timed out"
	default:
		return "error: agent returned invalid response status"
	}
}

func boundAgentOutput(value string) string {
	if len(value) <= maxAgentResultBytes {
		return value
	}
	end := maxAgentResultBytes
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end]
}

type foregroundResult struct {
	AgentID        string                      `json:"agent_id"`
	Name           string                      `json:"name"`
	State          tool.AgentState             `json:"state"`
	DeliveryStatus tool.DelegateDeliveryStatus `json:"delivery_status,omitempty"`
	ResponseStatus string                      `json:"response_status,omitempty"`
	Response       *string                     `json:"response,omitempty"`
}

type backgroundResult struct {
	AgentID        string                      `json:"agent_id"`
	Name           string                      `json:"name"`
	State          tool.AgentState             `json:"state"`
	DeliveryStatus tool.DelegateDeliveryStatus `json:"delivery_status,omitempty"`
}

func marshalForegroundResult(result foregroundResult) string {
	includeResponse := result.Response != nil
	response := ""
	if includeResponse {
		response = boundAgentOutput(*result.Response)
	}
	prefixEnds := make([]int, 1, len(response)+1)
	for end := 0; end < len(response); {
		_, size := utf8.DecodeRuneInString(response[end:])
		end += size
		prefixEnds = append(prefixEnds, end)
	}

	var best []byte
	low, high := 0, len(prefixEnds)
	for low < high {
		mid := low + (high-low)/2
		if includeResponse {
			candidate := response[:prefixEnds[mid]]
			result.Response = &candidate
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			return "error: agent result unavailable"
		}
		if len(encoded) <= maxAgentResultBytes {
			best = encoded
			low = mid + 1
		} else {
			high = mid
		}
	}
	if best == nil {
		return "error: agent result unavailable"
	}
	return string(best)
}

func formatBackground(result tool.DelegateResult) string {
	return marshalResult(backgroundResult{AgentID: result.AgentID.String(), Name: result.Name, State: result.State, DeliveryStatus: result.DeliveryStatus})
}

func wireResponseStatus(status tool.DelegateResponseStatus) string {
	switch status {
	case tool.DelegateResponseCompleted:
		return "completed"
	case tool.DelegateResponseFailed:
		return "failed"
	case tool.DelegateResponseInterrupted:
		return "interrupted"
	case tool.DelegateResponseTimedOut:
		return "timed_out"
	default:
		return ""
	}
}

func marshalResult(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "error: agent result unavailable"
	}
	return string(encoded)
}
