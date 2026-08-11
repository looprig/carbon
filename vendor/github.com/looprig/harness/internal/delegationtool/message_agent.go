package delegationtool

import (
	"context"
	"encoding/json"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

const messageAgentToolName = "MessageAgent"

// MessageAgentTool is the MessageAgent agent tool for sending work to a child agent.
type MessageAgentTool struct {
	controller tool.DelegateController
	config     *agentToolConfig
}

func newMessageAgent(controller tool.DelegateController, config *agentToolConfig) *MessageAgentTool {
	return &MessageAgentTool{controller: controller, config: config}
}

// NewMessageAgent constructs a MessageAgent agent tool with the supplied delegation policy and catalogs.
func NewMessageAgent(controller tool.DelegateController, style loop.DelegationStyle, catalog []AgentCatalogEntry, runtimeCatalog ...loop.RuntimeCatalog) *MessageAgentTool {
	return newMessageAgent(controller, newAgentToolConfig(style, catalog, runtimeCatalog...))
}

func (s *MessageAgentTool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{Name: messageAgentToolName, Desc: "Send a message to an existing child agent.", Schema: json.RawMessage(buildMessageAgentSchema(s.config.style))}, nil
}

func (*MessageAgentTool) AuditSummary(string) string { return messageAgentToolName }

func (s *MessageAgentTool) PrepareCall(_ context.Context, _ uuid.UUID, argsJSON string) (tool.Request, tool.PreparedArtifact, error) {
	if s.config.style != loop.DelegationManaged {
		return tool.Request{}, nil, preparationFailure(errCategoryInvalidValue)
	}
	prepared, err := prepareMessageAgent(argsJSON)
	if err != nil {
		return tool.Request{}, nil, err
	}
	request := tool.DelegateRequest{Operation: tool.DelegateSend, AgentID: prepared.AgentID, Message: prepared.Message, WaitForResponse: prepared.WaitForResponse, TimeoutSeconds: prepared.TimeoutSeconds}
	return tool.Request{}, tool.DelegateArtifact{Request: request}, nil
}

func (s *MessageAgentTool) InvokableRun(ctx context.Context, _ string) (*tool.ToolResult, error) {
	return executeAgentCall(ctx, s.controller, tool.DelegateSend, formatMessageAgentResult)
}

func formatMessageAgentResult(req tool.DelegateRequest, result tool.DelegateResult) string {
	if !req.WaitForResponse {
		// Background admission is an immediate delivery envelope. Any response
		// fields on the controller result belong to a later hand-back and must
		// not leak into this tool result.
		result.Response = ""
		result.ResponseStatus = tool.DelegateResponseUnknown
		return formatBackground(result)
	}
	return formatForeground(result)
}

var _ tool.InvokableTool = (*MessageAgentTool)(nil)
var _ tool.CallPreparer = (*MessageAgentTool)(nil)
var _ tool.Auditable = (*MessageAgentTool)(nil)
