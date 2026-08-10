package app

import (
	"strings"
	"testing"

	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/tools/skill"
)

func testSkillLoader() skill.SkillLoader {
	return skill.NewEmbeddedSkillLoader(nil, nil)
}

func TestCarbonSystemPromptOmitsEmbeddedSkillCatalog(t *testing.T) {
	t.Parallel()
	access, cfg := headlessTestAccess(t, Config{}, t.TempDir())
	definition, err := carbonDefinition(&fakeLLM{}, testModel(), cfg, access, nil)
	if err != nil {
		t.Fatalf("carbonDefinition() error = %v", err)
	}
	system := definition.FingerprintInitial().EffectiveSystem
	if strings.Contains(system, "<available_skills>") || strings.Contains(system, "code-style") {
		t.Fatalf("Carbon system prompt contains removed embedded skill catalog: %q", system)
	}
}

func TestCarbonSkillDefinitionAlwaysUsesWorkspace(t *testing.T) {
	t.Parallel()
	def := skillDefinitionFor(testSkillLoader())
	if def == nil {
		t.Fatal("skillDefinitionFor() = nil, want Carbon Skill definition")
	}
	if got := def.Requirements(); got != tool.RequiresWorkspace {
		t.Fatalf("Skill requirements = %v, want %v", got, tool.RequiresWorkspace)
	}
}

func TestCarbonDefinitionUsesRuntimeContext(t *testing.T) {
	t.Parallel()
	access, cfg := headlessTestAccess(t, Config{}, t.TempDir())
	definition, err := carbonDefinition(&fakeLLM{}, testModel(), cfg, access, nil)
	if err != nil {
		t.Fatalf("carbonDefinition() error = %v", err)
	}
	if definition.PolicyRevision() == "" || definition.ToolRequirements() == 0 {
		t.Fatalf("Carbon definition missing runtime/policy wiring: policy=%q requirements=%v", definition.PolicyRevision(), definition.ToolRequirements())
	}
	_ = loop.ModeName("quick")
}
