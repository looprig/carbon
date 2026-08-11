package task

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/tools/internal/prepared"
)

const taskCreateToolName = "TaskCreate"

const taskCreateDescription = "Create one task in this Loop's private task graph."

const taskCreateSchema = `{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "subject": {"type": "string", "minLength": 1, "description": "Brief task title."},
    "description": {"type": "string", "minLength": 1, "description": "Detailed task requirements."},
    "activeForm": {"type": "string", "description": "Present-continuous text shown while the task is active."},
    "blockedBy": {"type": "array", "items": {"type": "string"}, "maxItems": 128, "description": "Existing task UUIDs that must complete first."},
    "metadata": {"type": "object", "description": "Optional structured task metadata."}
  },
  "required": ["subject", "description"]
}`

type taskCreateArgs struct {
	Subject     string          `json:"subject"`
	Description string          `json:"description"`
	ActiveForm  string          `json:"activeForm"`
	BlockedBy   []string        `json:"blockedBy"`
	Metadata    json.RawMessage `json:"metadata"`
}

// taskCreateArtifact is the complete, owned input for one prepared create
// call. InvokableRun deliberately consumes this value instead of decoding the
// raw argument string again.
type taskCreateArtifact struct {
	tool.TokenArtifact
	input createInput
}

// TaskCreate creates tasks in one Loop-local graph.
type TaskCreate struct {
	toolBase
	store *store
}

// newTaskCreate constructs a TaskCreate over the supplied Loop-local store.
// The store type stays private so callers use the future task bundle factory
// rather than reaching into storage internals.
func newTaskCreate(s *store) *TaskCreate {
	if s == nil {
		s = newStore(nil)
	}
	return &TaskCreate{
		toolBase: toolBase{name: taskCreateToolName, desc: taskCreateDescription},
		store:    s,
	}
}

func (t *TaskCreate) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{
		Name:   taskCreateToolName,
		Desc:   taskCreateDescription,
		Schema: json.RawMessage(taskCreateSchema),
	}, nil
}

// AuditSummary intentionally excludes every model-supplied task field.
func (*TaskCreate) AuditSummary(string) string { return taskCreateToolName }

func (t *TaskCreate) PrepareCall(_ context.Context, executionID uuid.UUID, argsJSON string) (tool.Request, tool.PreparedArtifact, error) {
	var args taskCreateArgs
	fields, err := decodeObject(argsJSON, &args)
	if err != nil {
		return tool.Request{}, nil, err
	}
	input, err := prepareCreateInput(fields, args)
	if err != nil {
		return tool.Request{}, nil, err
	}

	return tool.Request{ToolName: taskCreateToolName}, &taskCreateArtifact{
		TokenArtifact: tool.TokenArtifact{Token: executionID.String()},
		input:         cloneCreateInput(input),
	}, nil
}

func prepareCreateInput(fields objectFields, args taskCreateArgs) (createInput, error) {
	if !fields.has("subject") || !fields.has("description") {
		return createInput{}, &prepareError{}
	}
	if err := validateStringField(fields, "subject", args.Subject); err != nil {
		return createInput{}, err
	}
	if err := validateStringField(fields, "description", args.Description); err != nil {
		return createInput{}, err
	}
	if strings.TrimSpace(args.Subject) == "" || len(args.Subject) > maxSubjectBytes {
		return createInput{}, &prepareError{}
	}
	if strings.TrimSpace(args.Description) == "" || len(args.Description) > maxDescriptionBytes {
		return createInput{}, &prepareError{}
	}

	if fields.has("activeForm") {
		if err := validateStringField(fields, "activeForm", args.ActiveForm); err != nil {
			return createInput{}, err
		}
		if len(args.ActiveForm) > maxActiveFormBytes {
			return createInput{}, &prepareError{}
		}
	}

	var dependencies []string
	if fields.has("blockedBy") {
		if err := validateStringArrayField(fields["blockedBy"], args.BlockedBy); err != nil {
			return createInput{}, err
		}
		var err error
		dependencies, err = canonicalTaskIDs(args.BlockedBy)
		if err != nil {
			return createInput{}, err
		}
	}

	metadata, err := canonicalMetadata(args.Metadata)
	if err != nil {
		return createInput{}, err
	}
	return createInput{
		Subject:     args.Subject,
		Description: args.Description,
		ActiveForm:  args.ActiveForm,
		BlockedBy:   cloneStrings(dependencies),
		Metadata:    cloneRawMessage(metadata),
	}, nil
}

func validateStringField(fields objectFields, name, value string) error {
	raw, ok := fields[name]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return &prepareError{}
	}
	var decoded string
	if err := json.Unmarshal(raw, &decoded); err != nil || decoded != value {
		return &prepareError{}
	}
	return nil
}

func validateStringArrayField(raw json.RawMessage, values []string) error {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return &prepareError{}
	}
	var elements []json.RawMessage
	if err := json.Unmarshal(raw, &elements); err != nil || len(elements) != len(values) {
		return &prepareError{}
	}
	for index, element := range elements {
		if bytes.Equal(bytes.TrimSpace(element), []byte("null")) {
			return &prepareError{}
		}
		var decoded string
		if err := json.Unmarshal(element, &decoded); err != nil || decoded != values[index] {
			return &prepareError{}
		}
	}
	return nil
}

func canonicalTaskIDs(values []string) ([]string, error) {
	if len(values) > maxDependencies {
		return nil, &prepareError{}
	}
	seen := make(map[string]struct{}, len(values))
	canonical := make([]string, 0, len(values))
	for _, value := range values {
		parsed, err := uuid.Parse(value)
		if err != nil || parsed.IsZero() {
			return nil, &prepareError{}
		}
		id := parsed.String()
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		canonical = append(canonical, id)
	}
	sort.Strings(canonical)
	if len(canonical) == 0 {
		return nil, nil
	}
	return canonical, nil
}

func cloneCreateInput(input createInput) createInput {
	input.BlockedBy = cloneStrings(input.BlockedBy)
	input.Metadata = cloneRawMessage(input.Metadata)
	return input
}

func (t *TaskCreate) InvokableRun(ctx context.Context, _ string) (*tool.ToolResult, error) {
	if ctx == nil {
		return tool.TextResult("error: TaskCreate requires its prepared call artifact"), nil
	}
	artifact, ok := prepared.FromContext[*taskCreateArtifact](ctx)
	if !ok || artifact == nil {
		return tool.TextResult("error: TaskCreate requires its prepared call artifact"), nil
	}
	if t == nil || t.store == nil {
		return tool.TextResult("error: TaskCreate is unavailable"), nil
	}

	created, err := t.store.create(cloneCreateInput(artifact.input))
	if err != nil {
		return taskErrorResult("could not create task", err), nil
	}
	return taskJSONResult(created)
}

func taskErrorResult(prefix string, err error) *tool.ToolResult {
	if err == nil {
		return tool.TextResult("error: " + prefix)
	}
	return tool.TextResult("error: " + prefix + ": " + err.Error())
}

func taskJSONResult(value Task) (*tool.ToolResult, error) {
	result, err := jsonResult(struct {
		Task Task `json:"task"`
	}{Task: value})
	if err != nil {
		return tool.TextResult("error: could not encode task result"), nil
	}
	return result, nil
}

// Compile-time capability assertions keep the optional interfaces narrow and
// make the prepared-artifact relationship explicit.
var (
	_ tool.InvokableTool    = (*TaskCreate)(nil)
	_ tool.CallPreparer     = (*TaskCreate)(nil)
	_ tool.Auditable        = (*TaskCreate)(nil)
	_ tool.Sequential       = (*TaskCreate)(nil)
	_ tool.PreparedArtifact = (*taskCreateArtifact)(nil)
)

func (*TaskCreate) Sequential() bool { return true }
