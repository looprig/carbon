package task

import (
	"context"
	"encoding/json"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/tools/internal/prepared"
)

const taskListToolName = "TaskList"

const taskListDescription = "List every task in this Loop's private task graph in deterministic order."

const taskListSchema = `{
  "type": "object",
  "additionalProperties": false,
  "properties": {}
}`

type taskListArgs struct{}

// taskListArtifact contains the complete prepared input for a list call. The
// empty argument object needs no mutable state beyond the execution token.
type taskListArtifact struct {
	tool.TokenArtifact
}

// TaskList lists every task in one Loop-local graph.
type TaskList struct {
	toolBase
	store *store
}

// newTaskList constructs a TaskList over the supplied Loop-local store. The
// private store parameter leaves storage ownership with the task bundle.
func newTaskList(s *store) *TaskList {
	if s == nil {
		s = newStore(nil)
	}
	return &TaskList{
		toolBase: toolBase{name: taskListToolName, desc: taskListDescription},
		store:    s,
	}
}

func (t *TaskList) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{
		Name:   taskListToolName,
		Desc:   taskListDescription,
		Schema: json.RawMessage(taskListSchema),
	}, nil
}

func (t *TaskList) PrepareCall(_ context.Context, executionID uuid.UUID, argsJSON string) (tool.Request, tool.PreparedArtifact, error) {
	var args taskListArgs
	if _, err := decodeObject(argsJSON, &args); err != nil {
		return tool.Request{}, nil, err
	}

	return tool.Request{ToolName: taskListToolName}, &taskListArtifact{
		TokenArtifact: tool.TokenArtifact{Token: executionID.String()},
	}, nil
}

func (t *TaskList) InvokableRun(ctx context.Context, _ string) (*tool.ToolResult, error) {
	if ctx == nil {
		return tool.TextResult("error: TaskList requires its prepared call artifact"), nil
	}
	artifact, ok := prepared.FromContext[*taskListArtifact](ctx)
	if !ok || artifact == nil {
		return tool.TextResult("error: TaskList requires its prepared call artifact"), nil
	}
	if t == nil || t.store == nil {
		return tool.TextResult("error: TaskList is unavailable"), nil
	}

	tasks := t.store.list()
	if tasks == nil {
		tasks = make([]Task, 0)
	}
	result, err := jsonResult(struct {
		Tasks []Task `json:"tasks"`
	}{Tasks: tasks})
	if err != nil {
		return taskErrorResult("could not list tasks", err), nil
	}
	return result, nil
}

var (
	_ tool.InvokableTool    = (*TaskList)(nil)
	_ tool.CallPreparer     = (*TaskList)(nil)
	_ tool.Auditable        = (*TaskList)(nil)
	_ tool.Sequential       = (*TaskList)(nil)
	_ tool.PreparedArtifact = (*taskListArtifact)(nil)
)

func (*TaskList) Sequential() bool { return true }
