package task

import (
	"context"
	"encoding/json"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/tools/internal/prepared"
)

const taskUpdateToolName = "TaskUpdate"

const taskUpdateDescription = "Patch, transition, rewire, or delete one task in this Loop's private task graph."

const taskUpdateSchema = `{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "taskId": {"type": "string", "description": "Task UUID to update."},
    "subject": {"type": "string", "description": "Replacement task title."},
    "description": {"type": "string", "description": "Replacement task requirements."},
    "activeForm": {"type": "string", "description": "Replacement present-continuous activity text."},
    "status": {"type": "string", "enum": ["pending", "in_progress", "completed", "deleted"], "description": "Replacement status, or deleted to remove the task."},
    "addBlockedBy": {"type": "array", "items": {"type": "string"}, "maxItems": 128, "description": "Existing task UUIDs to add as dependencies."},
    "removeBlockedBy": {"type": "array", "items": {"type": "string"}, "maxItems": 128, "description": "Task UUIDs to remove as dependencies."},
    "metadata": {"type": "object", "description": "Replacement metadata object; an empty object clears metadata."}
  },
  "required": ["taskId"]
}`

type taskUpdateArgs struct {
	TaskID          string          `json:"taskId"`
	Subject         string          `json:"subject"`
	Description     string          `json:"description"`
	ActiveForm      string          `json:"activeForm"`
	Status          string          `json:"status"`
	AddBlockedBy    []string        `json:"addBlockedBy"`
	RemoveBlockedBy []string        `json:"removeBlockedBy"`
	Metadata        json.RawMessage `json:"metadata"`
}

// taskUpdateArtifact owns every value selected during preparation. Execution
// consumes this typed input and never reparses the raw argument string.
type taskUpdateArtifact struct {
	tool.TokenArtifact
	input updateInput
}

// TaskUpdate patches tasks in one Loop-local graph.
type TaskUpdate struct {
	toolBase
	store *store
}

// newTaskUpdate constructs a TaskUpdate over the supplied Loop-local store.
// The private store parameter leaves storage ownership with the task bundle.
func newTaskUpdate(s *store) *TaskUpdate {
	if s == nil {
		s = newStore(nil)
	}
	return &TaskUpdate{
		toolBase: toolBase{name: taskUpdateToolName, desc: taskUpdateDescription},
		store:    s,
	}
}

func (t *TaskUpdate) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{
		Name:   taskUpdateToolName,
		Desc:   taskUpdateDescription,
		Schema: json.RawMessage(taskUpdateSchema),
	}, nil
}

func (t *TaskUpdate) PrepareCall(_ context.Context, executionID uuid.UUID, argsJSON string) (tool.Request, tool.PreparedArtifact, error) {
	var args taskUpdateArgs
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

	input := updateInput{TaskID: taskID}
	if fields.has("subject") {
		if err := validateStringField(fields, "subject", args.Subject); err != nil {
			return tool.Request{}, nil, err
		}
		if len(args.Subject) > maxSubjectBytes {
			return tool.Request{}, nil, &prepareError{}
		}
		value := args.Subject
		input.Subject = &value
	}
	if fields.has("description") {
		if err := validateStringField(fields, "description", args.Description); err != nil {
			return tool.Request{}, nil, err
		}
		if len(args.Description) > maxDescriptionBytes {
			return tool.Request{}, nil, &prepareError{}
		}
		value := args.Description
		input.Description = &value
	}
	if fields.has("activeForm") {
		if err := validateStringField(fields, "activeForm", args.ActiveForm); err != nil {
			return tool.Request{}, nil, err
		}
		if len(args.ActiveForm) > maxActiveFormBytes {
			return tool.Request{}, nil, &prepareError{}
		}
		value := args.ActiveForm
		input.ActiveForm = &value
	}
	if fields.has("status") {
		if err := validateStringField(fields, "status", args.Status); err != nil {
			return tool.Request{}, nil, err
		}
		status := Status(args.Status)
		switch status {
		case StatusPending, StatusInProgress, StatusCompleted, StatusCommandDeleted:
		default:
			return tool.Request{}, nil, &prepareError{}
		}
		input.Status = &status
	}
	if fields.has("addBlockedBy") {
		if err := validateStringArrayField(fields["addBlockedBy"], args.AddBlockedBy); err != nil {
			return tool.Request{}, nil, err
		}
		dependencies, err := canonicalTaskIDs(args.AddBlockedBy)
		if err != nil {
			return tool.Request{}, nil, err
		}
		input.AddBlockedBy = dependencies
	}
	if fields.has("removeBlockedBy") {
		if err := validateStringArrayField(fields["removeBlockedBy"], args.RemoveBlockedBy); err != nil {
			return tool.Request{}, nil, err
		}
		dependencies, err := canonicalTaskIDs(args.RemoveBlockedBy)
		if err != nil {
			return tool.Request{}, nil, err
		}
		input.RemoveBlockedBy = dependencies
	}
	if fields.has("metadata") {
		metadata, err := canonicalMetadata(args.Metadata)
		if err != nil {
			return tool.Request{}, nil, err
		}
		input.Metadata = &metadata
	}

	return tool.Request{ToolName: taskUpdateToolName}, &taskUpdateArtifact{
		TokenArtifact: tool.TokenArtifact{Token: executionID.String()},
		input:         cloneUpdateInput(input),
	}, nil
}

func cloneUpdateInput(input updateInput) updateInput {
	cloned := input
	if input.Subject != nil {
		value := *input.Subject
		cloned.Subject = &value
	}
	if input.Description != nil {
		value := *input.Description
		cloned.Description = &value
	}
	if input.ActiveForm != nil {
		value := *input.ActiveForm
		cloned.ActiveForm = &value
	}
	if input.Status != nil {
		value := *input.Status
		cloned.Status = &value
	}
	cloned.AddBlockedBy = cloneStrings(input.AddBlockedBy)
	cloned.RemoveBlockedBy = cloneStrings(input.RemoveBlockedBy)
	if input.Metadata != nil {
		metadata := cloneRawMessage(*input.Metadata)
		cloned.Metadata = &metadata
	}
	return cloned
}

func (t *TaskUpdate) InvokableRun(ctx context.Context, _ string) (*tool.ToolResult, error) {
	if ctx == nil {
		return tool.TextResult("error: TaskUpdate requires its prepared call artifact"), nil
	}
	artifact, ok := prepared.FromContext[*taskUpdateArtifact](ctx)
	if !ok || artifact == nil {
		return tool.TextResult("error: TaskUpdate requires its prepared call artifact"), nil
	}
	if t == nil || t.store == nil {
		return tool.TextResult("error: TaskUpdate is unavailable"), nil
	}

	updated, deleted, err := t.store.update(cloneUpdateInput(artifact.input))
	if err != nil {
		return taskErrorResult("could not update task", err), nil
	}
	if deleted {
		return jsonResult(struct {
			DeletedTaskID string `json:"deletedTaskId"`
		}{DeletedTaskID: artifact.input.TaskID})
	}
	return taskJSONResult(updated)
}

var (
	_ tool.InvokableTool    = (*TaskUpdate)(nil)
	_ tool.CallPreparer     = (*TaskUpdate)(nil)
	_ tool.Auditable        = (*TaskUpdate)(nil)
	_ tool.Sequential       = (*TaskUpdate)(nil)
	_ tool.PreparedArtifact = (*taskUpdateArtifact)(nil)
)

func (*TaskUpdate) Sequential() bool { return true }
