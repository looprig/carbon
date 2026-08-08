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

func TestGenericSkillDefinitionBinds(t *testing.T) {
	t.Parallel()
	loader := testSkillLoader()
	for _, cfg := range []Config{{}, {RuntimeSkills: true}} {
		def := skillDefinitionFor(loader, cfg)
		if def == nil {
			t.Fatalf("skillDefinitionFor(%+v) = nil", cfg)
		}
		names := buildSkillTool(t, def, t.TempDir())
		if len(names) != 1 || names[0] != skillToolName {
			t.Errorf("built tool names = %v, want [%q]", names, skillToolName)
		}
	}
}
