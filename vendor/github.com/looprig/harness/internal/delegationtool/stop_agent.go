package delegationtool

import (
	"context"
	"encoding/json"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

const stopAgentToolName = "StopAgent"

// StopAgentTool is the StopAgent agent tool for interrupting a child agent.
type StopAgentTool struct {
	controller tool.DelegateController
	config     *agentToolConfig
}

func newStopAgent(controller tool.DelegateController, config *agentToolConfig) *StopAgentTool {
	return &StopAgentTool{controller: controller, config: config}
}

// NewStopAgent constructs a StopAgent agent tool with the supplied delegation policy and catalogs.
func NewStopAgent(controller tool.DelegateController, style loop.DelegationStyle, catalog []AgentCatalogEntry, runtimeCatalog ...loop.RuntimeCatalog) *StopAgentTool {
	return newStopAgent(controller, newAgentToolConfig(style, catalog, runtimeCatalog...))
}

func (s *StopAgentTool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{Name: stopAgentToolName, Desc: "Stop an existing child agent's current response.", Schema: json.RawMessage(buildStopAgentSchema())}, nil
}

func (*StopAgentTool) AuditSummary(string) string { return stopAgentToolName }

func (s *StopAgentTool) PrepareCall(_ context.Context, _ uuid.UUID, argsJSON string) (tool.Request, tool.PreparedArtifact, error) {
	if s.config.style != loop.DelegationManaged {
		return tool.Request{}, nil, preparationFailure(errCategoryInvalidValue)
	}
	prepared, err := prepareStopAgent(argsJSON)
	if err != nil {
		return tool.Request{}, nil, err
	}
	return tool.Request{}, tool.DelegateArtifact{Request: tool.DelegateRequest{Operation: tool.DelegateInterrupt, AgentID: prepared.AgentID}}, nil
}

func (s *StopAgentTool) InvokableRun(ctx context.Context, _ string) (*tool.ToolResult, error) {
	return executeAgentCall(ctx, s.controller, tool.DelegateInterrupt, formatStopAgentResult)
}

func formatStopAgentResult(_ tool.DelegateRequest, result tool.DelegateResult) string {
	return marshalResult(stopResult{AgentID: result.AgentID.String(), PreviousState: result.PreviousState, State: result.State})
}

type stopResult struct {
	AgentID       string          `json:"agent_id"`
	PreviousState tool.AgentState `json:"previous_state"`
	State         tool.AgentState `json:"state"`
}

var _ tool.InvokableTool = (*StopAgentTool)(nil)
var _ tool.CallPreparer = (*StopAgentTool)(nil)
var _ tool.Auditable = (*StopAgentTool)(nil)
