package delegationtool

import (
	"context"
	"encoding/json"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

const listAgentsToolName = "ListAgents"

// ListAgentsTool is the ListAgents agent tool for inspecting owned child agents.
type ListAgentsTool struct {
	controller tool.DelegateController
	config     *agentToolConfig
}

func newListAgents(controller tool.DelegateController, config *agentToolConfig) *ListAgentsTool {
	return &ListAgentsTool{controller: controller, config: config}
}

// NewListAgents constructs a ListAgents agent tool with the supplied delegation policy and catalogs.
func NewListAgents(controller tool.DelegateController, style loop.DelegationStyle, catalog []AgentCatalogEntry, runtimeCatalog ...loop.RuntimeCatalog) *ListAgentsTool {
	return newListAgents(controller, newAgentToolConfig(style, catalog, runtimeCatalog...))
}

func (s *ListAgentsTool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{Name: listAgentsToolName, Desc: "List child agents directly owned by this loop.", Schema: json.RawMessage(buildListAgentsSchema())}, nil
}

func (*ListAgentsTool) AuditSummary(string) string { return listAgentsToolName }

func (s *ListAgentsTool) PrepareCall(_ context.Context, _ uuid.UUID, argsJSON string) (tool.Request, tool.PreparedArtifact, error) {
	if s.config.style != loop.DelegationManaged {
		return tool.Request{}, nil, preparationFailure(errCategoryInvalidValue)
	}
	prepared, err := prepareListAgents(argsJSON)
	if err != nil {
		return tool.Request{}, nil, err
	}
	request := tool.DelegateRequest{Operation: tool.DelegateStatus}
	if prepared.AgentID != nil {
		request.AgentID = *prepared.AgentID
	}
	return tool.Request{}, tool.DelegateArtifact{Request: request}, nil
}

func (s *ListAgentsTool) InvokableRun(ctx context.Context, _ string) (*tool.ToolResult, error) {
	return executeAgentCall(ctx, s.controller, tool.DelegateStatus, formatListAgentsResult)
}

func formatListAgentsResult(_ tool.DelegateRequest, result tool.DelegateResult) string {
	agents := make([]agentListItem, len(result.Agents))
	for i, agent := range result.Agents {
		agents[i] = agentListItem{
			AgentID: agent.AgentID.String(), Name: agent.Name, AgentType: agent.AgentType,
			State: agent.State, QueuedMessages: agent.QueuedMessages,
			AgentHarness: agent.Runtime.Harness, AgentSource: agent.Runtime.Source,
			Model: agent.Runtime.Model, Effort: agent.Runtime.Effort, AgentMode: agent.AgentMode,
		}
	}
	return marshalResult(agentListResult{Agents: agents, Truncated: result.Truncated})
}

type agentListItem struct {
	AgentID        string          `json:"agent_id"`
	Name           string          `json:"name"`
	AgentType      string          `json:"agent_type"`
	State          tool.AgentState `json:"state"`
	QueuedMessages int             `json:"queued_messages"`
	AgentHarness   string          `json:"agent_harness,omitempty"`
	AgentSource    string          `json:"agent_source,omitempty"`
	Model          string          `json:"model,omitempty"`
	Effort         string          `json:"effort,omitempty"`
	AgentMode      string          `json:"agent_mode,omitempty"`
}

type agentListResult struct {
	Agents    []agentListItem `json:"agents"`
	Truncated bool            `json:"truncated"`
}

var _ tool.InvokableTool = (*ListAgentsTool)(nil)
var _ tool.CallPreparer = (*ListAgentsTool)(nil)
var _ tool.Auditable = (*ListAgentsTool)(nil)
