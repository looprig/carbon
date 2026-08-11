package delegationtool

import (
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

const startAgentToolName = "StartAgent"

// StartAgentTool is the StartAgent agent tool for creating child agents.
type StartAgentTool struct {
	controller tool.DelegateController
	config     *agentToolConfig
}

func newStartAgent(controller tool.DelegateController, config *agentToolConfig) *StartAgentTool {
	return &StartAgentTool{controller: controller, config: config}
}

// NewStartAgent constructs a StartAgent agent tool with the supplied delegation policy and catalogs.
func NewStartAgent(controller tool.DelegateController, style loop.DelegationStyle, catalog []AgentCatalogEntry, runtimeCatalog ...loop.RuntimeCatalog) *StartAgentTool {
	return newStartAgent(controller, newAgentToolConfig(style, catalog, runtimeCatalog...))
}

func (s *StartAgentTool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{Name: startAgentToolName, Desc: buildStartAgentDescription(s.config.catalog, s.config.runtimeCatalog), Schema: json.RawMessage(s.config.startSchema())}, nil
}

func (*StartAgentTool) AuditSummary(string) string { return startAgentToolName }

func (s *StartAgentTool) PrepareCall(ctx context.Context, _ uuid.UUID, argsJSON string) (tool.Request, tool.PreparedArtifact, error) {
	prepared, err := s.config.prepareStartAgent(argsJSON)
	if err != nil {
		return tool.Request{}, nil, err
	}
	if s.config.style == loop.DelegationSyncOnly && !prepared.WaitForResponse {
		return tool.Request{}, nil, preparationFailure(errCategoryInvalidValue)
	}
	request := tool.DelegateRequest{Operation: tool.DelegateStart, AgentType: prepared.AgentType, Name: prepared.Name, AgentMode: prepared.AgentMode, Message: prepared.Instructions, WaitForResponse: prepared.WaitForResponse, TimeoutSeconds: prepared.TimeoutSeconds, Runtime: prepared.Runtime}
	return tool.Request{}, tool.DelegateArtifact{Request: request, Runtime: prepared.Runtime}, nil
}

func (s *StartAgentTool) InvokableRun(ctx context.Context, _ string) (*tool.ToolResult, error) {
	return executeAgentCall(ctx, s.controller, tool.DelegateStart, formatStartAgentResult)
}

func formatStartAgentResult(req tool.DelegateRequest, result tool.DelegateResult) string {
	if req.WaitForResponse {
		if result.ResponseStatus == tool.DelegateResponseFailed {
			return formatStartAgentFailure(result.Response)
		}
		return formatForeground(result)
	}
	return formatBackground(result)
}

const startAgentFailurePrefix = "error: agent failed: "

func formatStartAgentFailure(detail string) string {
	detail = boundAgentOutput(strings.ToValidUTF8(detail, "\uFFFD"))
	maxDetailBytes := maxAgentResultBytes - len(startAgentFailurePrefix)
	if maxDetailBytes <= 0 {
		return "error: agent failed"
	}
	if len(detail) > maxDetailBytes {
		end := maxDetailBytes
		for end > 0 && !utf8.RuneStart(detail[end]) {
			end--
		}
		detail = detail[:end]
	}
	if detail == "" {
		return "error: agent failed"
	}
	return startAgentFailurePrefix + detail
}

var _ tool.InvokableTool = (*StartAgentTool)(nil)
var _ tool.CallPreparer = (*StartAgentTool)(nil)
var _ tool.Auditable = (*StartAgentTool)(nil)
