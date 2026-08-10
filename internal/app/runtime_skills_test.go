package app

import (
	"context"
	"testing"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/tool"
)

func buildSkillTool(t *testing.T, def tool.Definition, root string) []string {
	t.Helper()
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New() error = %v", err)
	}
	built, err := def.Build(context.Background(), tool.Bindings{
		SessionID: id,
		LoopID:    id,
		Workspace: &tool.WorkspaceBinding{Root: root, Coordinator: &testWorkspaceCoordinator{}, Observations: tool.NewWorkspaceObservations()},
	})
	if err != nil {
		t.Fatalf("def.Build() error = %v", err)
	}
	names := make([]string, 0, len(built))
	for _, tl := range built {
		info, err := tl.Info(context.Background())
		if err != nil {
			t.Fatalf("Info() error = %v", err)
		}
		names = append(names, info.Name)
	}
	return names
}

func TestCarbonSkillDefinitionBinds(t *testing.T) {
	t.Parallel()
	def := skillDefinitionFor(testSkillLoader())
	if def == nil {
		t.Fatal("skillDefinitionFor() = nil")
	}
	names := buildSkillTool(t, def, t.TempDir())
	if len(names) != 1 || names[0] != skillToolName {
		t.Errorf("built tool names = %v, want [%q]", names, skillToolName)
	}
}
