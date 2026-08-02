package app

import (
	"context"
	"testing"

	"github.com/looprig/coderig/internal/catalog/builder"
	"github.com/looprig/coderig/internal/catalog/planner"
	"github.com/looprig/coderig/internal/catalog/reviewer"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/tools/skill"
)

// runtime_skills_test.go proves the §7a per-agent Skill definition builds a bound Skill tool
// for the builder both embedded-only (RuntimeSkills off) and workspace-enabled (RuntimeSkills
// on), and that the reviewer never gets one.

// runtimeSkillLoader mirrors swarmDefinitions' loader over the roster allow-map.
func runtimeSkillLoader() skill.SkillLoader {
	return skill.NewEmbeddedSkillLoader(SkillsFS, buildSkillAllow([]skillScope{
		{name: planner.Name},
		{name: builder.Name, skills: builderSkills},
		{name: reviewer.Name},
	}))
}

// buildSkillTool binds def at root and returns the produced tool names.
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

// TestBuilderSkillDefinitionBinds proves the builder's Skill definition builds one "Skill"
// tool both embedded-only and workspace-enabled (the workspace-enabled path reads the bound
// root per bind).
func TestBuilderSkillDefinitionBinds(t *testing.T) {
	t.Parallel()
	loader := runtimeSkillLoader()

	tests := []struct {
		name string
		cfg  Config
	}{
		{name: "embedded only (RuntimeSkills off)", cfg: Config{}},
		{name: "workspace enabled (RuntimeSkills on)", cfg: Config{RuntimeSkills: true}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			def := skillDefinitionFor(loader, builderBuiltin(), tt.cfg)
			if def == nil {
				t.Fatal("skillDefinitionFor(builder) = nil, want a Skill definition")
			}
			names := buildSkillTool(t, def, t.TempDir())
			if len(names) != 1 || names[0] != skillToolName {
				t.Errorf("built tool names = %v, want exactly [%q]", names, skillToolName)
			}
		})
	}
}

func TestPlannerHasNoSkillDefinition(t *testing.T) {
	t.Parallel()
	loader := runtimeSkillLoader()
	if def := skillDefinitionFor(loader, plannerBuiltin(), Config{RuntimeSkills: true}); def != nil {
		t.Errorf("skillDefinitionFor(planner, RuntimeSkills on) = non-nil, want nil")
	}
}

// TestReviewerHasNoSkillDefinition proves the reviewer never gets a Skill definition (no
// embedded skills, not runtime-eligible), even with RuntimeSkills on.
func TestReviewerHasNoSkillDefinition(t *testing.T) {
	t.Parallel()
	loader := runtimeSkillLoader()
	if def := skillDefinitionFor(loader, reviewerBuiltin(), Config{RuntimeSkills: true}); def != nil {
		t.Errorf("skillDefinitionFor(reviewer, RuntimeSkills on) = non-nil, want nil")
	}
	_ = reviewer.Name
}
