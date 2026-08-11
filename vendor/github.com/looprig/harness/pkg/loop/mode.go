package loop

import (
	"reflect"

	"github.com/looprig/harness/pkg/tool"
	model "github.com/looprig/inference/model"
)

const (
	defaultMaxToolIterations    = 25
	defaultMaxToolCallsPerTurn  = 100
	defaultMaxParallelToolCalls = 8
)

// ModeName identifies a predeclared loop mode. The empty name identifies the base mode.
type ModeName string

// ToolLimits bounds tool activity during one turn.
type ToolLimits struct {
	Iterations int
	Calls      int
	Parallel   int
}

// Mode declares a validated alternative to a definition's base inference settings.
// Define defensively copies Tools and the model's sampling values.
type Mode struct {
	Name         ModeName
	Model        model.Model
	Effort       model.Effort
	Tools        []tool.Definition
	ToolLimits   ToolLimits
	Instructions string
}

// BoundMode is one immutable definition mode resolved to runtime tool instances.
// Values returned by BoundDefinition are defensive copies.
type BoundMode struct {
	Name         ModeName
	Model        model.Model
	Effort       model.Effort
	Tools        []tool.InvokableTool
	ToolLimits   ToolLimits
	Instructions string
}

func cloneModel(value model.Model) model.Model {
	value.Sampling = value.Sampling.Clone()
	return value
}

func cloneMode(mode Mode) Mode {
	mode.Model = cloneModel(mode.Model)
	mode.Tools = append([]tool.Definition(nil), mode.Tools...)
	return mode
}

func cloneBoundMode(mode BoundMode) BoundMode {
	mode.Model = cloneModel(mode.Model)
	mode.Tools = append([]tool.InvokableTool(nil), mode.Tools...)
	return mode
}

func zeroModel(value model.Model) bool {
	return reflect.DeepEqual(value, model.Model{})
}

func resolveLimits(base, override ToolLimits) ToolLimits {
	result := base
	if override.Iterations > 0 {
		result.Iterations = override.Iterations
	}
	if override.Calls > 0 {
		result.Calls = override.Calls
	}
	if override.Parallel > 0 {
		result.Parallel = override.Parallel
	}
	return result
}

func defaultLimits(limits ToolLimits) ToolLimits {
	if limits.Iterations == 0 {
		limits.Iterations = defaultMaxToolIterations
	}
	if limits.Calls == 0 {
		limits.Calls = defaultMaxToolCallsPerTurn
	}
	if limits.Parallel == 0 {
		limits.Parallel = defaultMaxParallelToolCalls
	}
	return limits
}

func invalidLimits(limits ToolLimits) bool {
	return limits.Iterations < 0 || limits.Calls < 0 || limits.Parallel < 0
}
