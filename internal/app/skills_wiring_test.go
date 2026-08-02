package app

import (
	"strings"
	"testing"

	"github.com/looprig/coderig/internal/catalog/builder"
	"github.com/looprig/coderig/internal/catalog/planner"
	"github.com/looprig/coderig/internal/catalog/reviewer"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/tools/skill"
)

// skills_wiring_test.go proves the composition root wires the per-agent Skill tool and the
// trusted <available_skills> system-prompt catalog correctly: the builder (which owns the
// embedded code-style skill) gets both; the reviewer (no skills) gets neither.

// testSkillLoader builds the embedded skill loader over the roster's allow-map, exactly as
// swarmDefinitions does.
func testSkillLoader() skill.SkillLoader {
	scopes := []skillScope{
		{name: planner.Name},
		{name: builder.Name, skills: builderSkills},
		{name: reviewer.Name},
	}
	return skill.NewEmbeddedSkillLoader(SkillsFS, buildSkillAllow(scopes))
}

// TestDefinitionSystemPromptsCarrySkillCatalog proves the builder loops' system prompt carries
// the <available_skills> catalog naming code-style, and the reviewer's does not.
func TestDefinitionSystemPromptsCarrySkillCatalog(t *testing.T) {
	t.Parallel()

	access, cfg := headlessTestAccess(t, Config{}, t.TempDir())
	defs, err := swarmDefinitions(&fakeLLM{}, testModel(), cfg, access)
	if err != nil {
		t.Fatalf("swarmDefinitions() error = %v", err)
	}
	byName := map[string]loop.Definition{}
	for _, d := range defs {
		byName[string(d.Name())] = d
	}

	builderSys := byName[string(builder.Name)].FingerprintInitial().EffectiveSystem
	if !strings.Contains(builderSys, "<available_skills>") || !strings.Contains(builderSys, "code-style") {
		t.Errorf("builder system prompt missing the skill catalog: %q", builderSys)
	}
	plannerSys := byName[string(planner.Name)].FingerprintInitial().EffectiveSystem
	if strings.Contains(plannerSys, "<available_skills>") {
		t.Errorf("planner system prompt unexpectedly carries a skill catalog: %q", plannerSys)
	}
	reviewerSys := byName[string(reviewer.Name)].FingerprintInitial().EffectiveSystem
	if strings.Contains(reviewerSys, "<available_skills>") {
		t.Errorf("reviewer system prompt unexpectedly carries a skill catalog: %q", reviewerSys)
	}
}

// TestSkillDefinitionForGating proves skillDefinitionFor honors the §7a gate: the builder (an
// embedded-skill owner) always gets a Skill definition; the reviewer (no skills, not
// runtime-eligible) never does.
func TestSkillDefinitionForGating(t *testing.T) {
	t.Parallel()
	loader := testSkillLoader()

	tests := []struct {
		name    string
		builtin leafBuiltin
		cfg     Config
		wantNil bool
	}{
		{name: "planner no skills off", builtin: plannerBuiltin(), cfg: Config{}, wantNil: true},
		{name: "planner not runtime-eligible", builtin: plannerBuiltin(), cfg: Config{RuntimeSkills: true}, wantNil: true},
		{name: "builder embedded-only", builtin: builderBuiltin(), cfg: Config{}, wantNil: false},
		{name: "builder runtime-skills on", builtin: builderBuiltin(), cfg: Config{RuntimeSkills: true}, wantNil: false},
		{name: "reviewer no skills off", builtin: reviewerBuiltin(), cfg: Config{}, wantNil: true},
		{name: "reviewer not runtime-eligible", builtin: reviewerBuiltin(), cfg: Config{RuntimeSkills: true}, wantNil: true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			def := skillDefinitionFor(loader, tt.builtin, tt.cfg)
			if (def == nil) != tt.wantNil {
				t.Errorf("skillDefinitionFor() nil = %v, want %v", def == nil, tt.wantNil)
			}
		})
	}
}

// TestBuildSkillAllowAuthorizesOnlyDeclaredSkills proves the loader allow-map authorizes the
// builder's code-style skill and nothing for planner or reviewer.
func TestBuildSkillAllowAuthorizesOnlyDeclaredSkills(t *testing.T) {
	t.Parallel()

	allow := buildSkillAllow([]skillScope{
		{name: planner.Name},
		{name: builder.Name, skills: builderSkills},
		{name: reviewer.Name},
	})
	builderAllowed, ok := allow[builder.Name]
	if !ok {
		t.Fatalf("builder absent from the allow-map")
	}
	if _, ok := builderAllowed["code-style"]; !ok {
		t.Errorf("builder allow-map = %v, want it to authorize code-style", builderAllowed)
	}
	if _, ok := allow[planner.Name]; ok {
		t.Errorf("planner present in the allow-map, want absent (no skills)")
	}
	if _, ok := allow[reviewer.Name]; ok {
		t.Errorf("reviewer present in the allow-map, want absent (no skills)")
	}
}
