package app

import (
	"strings"
	"testing"

	"github.com/looprig/coderig/internal/catalog/generic"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/tools/skill"
)

func testSkillLoader() skill.SkillLoader {
	return skill.NewEmbeddedSkillLoader(SkillsFS, buildSkillAllow([]skillScope{{name: generic.Name, skills: genericSkills}}))
}

func TestGenericSystemPromptCarriesSkillCatalog(t *testing.T) {
	t.Parallel()
	access, cfg := headlessTestAccess(t, Config{}, t.TempDir())
	definition, err := genericDefinition(&fakeLLM{}, testModel(), cfg, access, nil)
	if err != nil {
		t.Fatalf("genericDefinition() error = %v", err)
	}
	system := definition.FingerprintInitial().EffectiveSystem
	if !strings.Contains(system, "<available_skills>") || !strings.Contains(system, "code-style") {
		t.Fatalf("Generic system prompt missing skill catalog: %q", system)
	}
	if strings.Count(system, "<available_skills>") != 1 {
		t.Fatalf("skill catalog appears more than once: %q", system)
	}
}

func TestGenericSkillDefinitionAlwaysAvailableAndRuntimeGated(t *testing.T) {
	t.Parallel()
	loader := testSkillLoader()
	for _, cfg := range []Config{{}, {RuntimeSkills: true}} {
		if def := skillDefinitionFor(loader, cfg); def == nil {
			t.Fatalf("skillDefinitionFor(%+v) = nil, want Generic Skill definition", cfg)
		}
	}

	allow := buildSkillAllow([]skillScope{{name: generic.Name, skills: genericSkills}})
	if _, ok := allow[generic.Name]; !ok {
		t.Fatalf("Generic absent from skill allow map: %v", allow)
	}
	if _, ok := allow[generic.Name]["code-style"]; !ok {
		t.Fatalf("Generic allow map = %v, want code-style", allow)
	}
	if len(allow) != 1 {
		t.Fatalf("skill allow map has %d scopes, want only Generic: %v", len(allow), allow)
	}
}

func TestGenericDefinitionUsesRuntimeContext(t *testing.T) {
	t.Parallel()
	access, cfg := headlessTestAccess(t, Config{}, t.TempDir())
	definition, err := genericDefinition(&fakeLLM{}, testModel(), cfg, access, nil)
	if err != nil {
		t.Fatalf("genericDefinition() error = %v", err)
	}
	if definition.PolicyRevision() == "" || definition.ToolRequirements() == 0 {
		t.Fatalf("Generic definition missing runtime/policy wiring: policy=%q requirements=%v", definition.PolicyRevision(), definition.ToolRequirements())
	}
	_ = loop.ModeName("quick")
}
