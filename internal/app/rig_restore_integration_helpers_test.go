//go:build integration

package app

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/tool"
)

func listAgentsCall(id, input string) []content.Chunk { return namedToolCall(id, "ListAgents", input) }

func stopAgentCall(id, input string) []content.Chunk { return namedToolCall(id, "StopAgent", input) }

func parseAgentHandle(text string) (agentHandle, error) {
	var got agentHandle
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		return agentHandle{}, fmt.Errorf("agent result %q: %w", text, err)
	}
	if got.AgentID == "" {
		return agentHandle{}, fmt.Errorf("agent result missing id: %q", text)
	}
	return got, nil
}

type approveAllAccessGate struct{}

func (approveAllAccessGate) Authorize(context.Context, tool.Request) (gate.Resolution, error) {
	return gate.Resolution{Approved: true}, nil
}
