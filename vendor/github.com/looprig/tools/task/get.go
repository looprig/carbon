package task

import (
	"context"
	"encoding/json"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/tools/internal/prepared"
)

const taskGetToolName = "TaskGet"

const taskGetDescription = "Get one complete task from this Loop's private task graph."

const taskGetSchema = `{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "taskId": {"type": "string", "description": "Task UUID to retrieve."}
  },
  "required": ["taskId"]
}`

type taskGetArgs struct {
	TaskID string `json:"taskId"`
}

// taskGetArtifact owns the canonical ID selected during preparation. Execution
// never reparses or re-reads the raw argument JSON.
type taskGetArtifact struct {
	tool.TokenArtifact
	taskID string
}

// TaskGet retrieves tasks from one Loop-local graph.
type TaskGet struct {
	toolBase
	store *store
}

// newTaskGet constructs a TaskGet over the supplied Loop-local store. The
// private store parameter leaves storage ownership with the task bundle.
func newTaskGet(s *store) *TaskGet {
	if s == nil {
		s = newStore(nil)
	}
	return &TaskGet{
		toolBase: toolBase{name: taskGetToolName, desc: taskGetDescription},
		store:    s,
	}
}

func (t *TaskGet) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{
		Name:   taskGetToolName,
		Desc:   taskGetDescription,
		Schema: json.RawMessage(taskGetSchema),
	}, nil
}

// AuditSummary intentionally excludes the task ID and all task contents.
func (*TaskGet) AuditSummary(string) string { return taskGetToolName }

func (t *TaskGet) PrepareCall(_ context.Context, executionID uuid.UUID, argsJSON string) (tool.Request, tool.PreparedArtifact, error) {
	var args taskGetArgs
	fields, err := decodeObject(argsJSON, &args)
	if err != nil {
		return tool.Request{}, nil, err
	}
	if !fields.has("taskId") {
		return tool.Request{}, nil, &prepareError{}
	}
	if err := validateStringField(fields, "taskId", args.TaskID); err != nil {
		return tool.Request{}, nil, err
	}
	taskID, err := canonicalTaskID(args.TaskID)
	if err != nil {
		return tool.Request{}, nil, err
	}

	return tool.Request{ToolName: taskGetToolName}, &taskGetArtifact{
		TokenArtifact: tool.TokenArtifact{Token: executionID.String()},
		taskID:        taskID,
	}, nil
}

func canonicalTaskID(value string) (string, error) {
	parsed, err := uuid.Parse(value)
	if err != nil || parsed.IsZero() {
		return "", &prepareError{}
	}
	return parsed.String(), nil
}

func (t *TaskGet) InvokableRun(ctx context.Context, _ string) (*tool.ToolResult, error) {
	if ctx == nil {
		return tool.TextResult("error: TaskGet requires its prepared call artifact"), nil
	}
	artifact, ok := prepared.FromContext[*taskGetArtifact](ctx)
	if !ok || artifact == nil {
		return tool.TextResult("error: TaskGet requires its prepared call artifact"), nil
	}
	if t == nil || t.store == nil {
		return tool.TextResult("error: TaskGet is unavailable"), nil
	}

	found, err := t.store.get(artifact.taskID)
	if err != nil {
		return taskErrorResult("could not get task", err), nil
	}
	return taskJSONResult(found)
}

var (
	_ tool.InvokableTool    = (*TaskGet)(nil)
	_ tool.CallPreparer     = (*TaskGet)(nil)
	_ tool.Auditable        = (*TaskGet)(nil)
	_ tool.Sequential       = (*TaskGet)(nil)
	_ tool.PreparedArtifact = (*taskGetArtifact)(nil)
)

func (*TaskGet) Sequential() bool { return true }
