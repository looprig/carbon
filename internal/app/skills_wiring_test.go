package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/tools/skill"
)

func writeProductionCatalogFixture(t *testing.T, root, name, description string) {
	t.Helper()
	dir := filepath.Join(root, ".skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", dir, err)
	}
	document := "---\nname: " + name + "\ndescription: " + description + "\n---\n\n# " + name + "\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(document), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", name, err)
	}
}

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

func TestCarbonDefinitionRuntimeContextDiscoversCurrentWorkspaceSkills(t *testing.T) {
	workspace := t.TempDir()
	writeProductionCatalogFixture(t, workspace, "alpha", "Initial alpha metadata.")

	access, cfg := headlessTestAccess(t, Config{}, workspace)
	definition, err := carbonDefinition(&fakeLLM{}, testModel(), cfg, access, nil)
	if err != nil {
		t.Fatalf("carbonDefinition() error = %v", err)
	}
	if system := definition.FingerprintInitial().EffectiveSystem; strings.Contains(system, "<available_skills>") || strings.Contains(system, "Initial alpha metadata.") {
		t.Fatalf("FingerprintInitial().EffectiveSystem contains runtime skill metadata: %q", system)
	}

	bound, err := definition.Bind(context.Background(), sessionScopedBindingsFor(
		t,
		mustUUID(t),
		mustUUID(t),
		workspace,
		&fakeSessionResourceRegistry{dir: t.TempDir()},
	))
	if err != nil {
		t.Fatalf("definition.Bind() error = %v", err)
	}
	provider := bound.RuntimeContext()

	initial := soleText(t, provider.Blocks(context.Background()))
	for _, want := range []string{"<available_skills>", "<name>alpha</name>", "<description>Initial alpha metadata.</description>"} {
		if !strings.Contains(initial, want) {
			t.Errorf("initial runtime context missing %q:\n%s", want, initial)
		}
	}

	writeProductionCatalogFixture(t, workspace, "alpha", "Changed alpha metadata.")
	writeProductionCatalogFixture(t, workspace, "beta", "Added beta metadata.")
	changed := soleText(t, provider.Blocks(context.Background()))
	for _, want := range []string{"<description>Changed alpha metadata.</description>", "<name>beta</name>", "<description>Added beta metadata.</description>"} {
		if !strings.Contains(changed, want) {
			t.Errorf("changed runtime context missing %q:\n%s", want, changed)
		}
	}
	if strings.Contains(changed, "Initial alpha metadata.") {
		t.Fatalf("changed runtime context retained stale metadata:\n%s", changed)
	}

	if err := os.RemoveAll(filepath.Join(workspace, ".skills", "alpha")); err != nil {
		t.Fatalf("RemoveAll(alpha): %v", err)
	}
	removed := soleText(t, provider.Blocks(context.Background()))
	if strings.Contains(removed, "<name>alpha</name>") || strings.Contains(removed, "Changed alpha metadata.") {
		t.Fatalf("runtime context retained removed skill metadata:\n%s", removed)
	}
	if !strings.Contains(removed, "<name>beta</name>") {
		t.Fatalf("runtime context lost remaining skill metadata:\n%s", removed)
	}
}

func TestCarbonDefinitionRuntimeSkillCatalogUsesSessionAccessWorkspace(t *testing.T) {
	accessRoot := t.TempDir()
	bindRoot := t.TempDir()
	cwdRoot := t.TempDir()
	writeProductionCatalogFixture(t, accessRoot, "access-skill", "Metadata from session access.")
	writeProductionCatalogFixture(t, bindRoot, "bind-decoy", "Metadata from definition binding.")
	writeProductionCatalogFixture(t, cwdRoot, "cwd-decoy", "Metadata from process cwd.")
	t.Chdir(cwdRoot)

	access, cfg := headlessTestAccess(t, Config{}, accessRoot)
	definition, err := carbonDefinition(&fakeLLM{}, testModel(), cfg, access, nil)
	if err != nil {
		t.Fatalf("carbonDefinition() error = %v", err)
	}
	bound, err := definition.Bind(context.Background(), sessionScopedBindingsFor(
		t,
		mustUUID(t),
		mustUUID(t),
		bindRoot,
		&fakeSessionResourceRegistry{dir: t.TempDir()},
	))
	if err != nil {
		t.Fatalf("definition.Bind() error = %v", err)
	}

	text := soleText(t, bound.RuntimeContext().Blocks(context.Background()))
	for _, want := range []string{"<name>access-skill</name>", "<description>Metadata from session access.</description>"} {
		if !strings.Contains(text, want) {
			t.Errorf("runtime context missing access-root metadata %q:\n%s", want, text)
		}
	}
	for _, decoy := range []string{"bind-decoy", "Metadata from definition binding.", "cwd-decoy", "Metadata from process cwd."} {
		if strings.Contains(text, decoy) {
			t.Errorf("runtime context contains decoy metadata %q:\n%s", decoy, text)
		}
	}
}

func TestRuntimeSkillCatalogForAccessDoesNotFallbackWithoutWorkspace(t *testing.T) {
	for name, access := range map[string]*sessionAccess{
		"nil access":      nil,
		"empty workspace": {},
	} {
		t.Run(name, func(t *testing.T) {
			if catalog := runtimeSkillCatalogForAccess(access); catalog != nil {
				t.Fatalf("runtimeSkillCatalogForAccess(%s) = non-nil, want no catalog", name)
			}
		})
	}
}
