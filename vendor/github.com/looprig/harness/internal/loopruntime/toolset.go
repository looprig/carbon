package loopruntime

import (
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

// ToolSet is the actor-private resolved tool bundle. Access is the combined
// prepared-access decision gate; a nil Access denies every tool call (fail
// closed) rather than running ungated.
type ToolSet struct {
	Access      loop.AccessGate
	Registry    []tool.InvokableTool
	Middlewares []tool.ToolMiddleware

	MaxToolIterations    int
	MaxToolCallsPerTurn  int
	MaxParallelToolCalls int
}

const (
	defaultMaxToolIterations    = 25
	defaultMaxToolCallsPerTurn  = 100
	defaultMaxParallelToolCalls = 8
)

func resolveMaxToolIterations(n int) int {
	if n <= 0 {
		return defaultMaxToolIterations
	}
	return n
}

func resolveMaxToolCallsPerTurn(n int) int {
	if n <= 0 {
		return defaultMaxToolCallsPerTurn
	}
	return n
}

func resolveMaxParallelToolCalls(n int) int {
	if n <= 0 {
		return defaultMaxParallelToolCalls
	}
	return n
}

func resolveToolSetCaps(ts ToolSet) ToolSet {
	ts.MaxToolIterations = resolveMaxToolIterations(ts.MaxToolIterations)
	ts.MaxToolCallsPerTurn = resolveMaxToolCallsPerTurn(ts.MaxToolCallsPerTurn)
	ts.MaxParallelToolCalls = resolveMaxParallelToolCalls(ts.MaxParallelToolCalls)
	return ts
}
