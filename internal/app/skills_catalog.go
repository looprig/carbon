package app

import (
	"context"
	"sort"
	"strings"

	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/tools/skill"
)

// skills_catalog.go is the composition-root seam that turns Generic's allowed
// allowed-skill sets into (a) the loader's allow-map, and (b) the trusted
// <available_skills> catalog block appended to a skilled agent's system prompt.
//
// The catalog descriptions come from the TRUSTED, embedded SkillsFS (reviewed
// in-repo content), so injecting them into the system prompt is safe — unlike a
// later phase's workspace skills, which are untrusted. The catalog is read through
// the loader's SkillDescriber, so it can only ever list a skill the agent is
// actually authorized to load (the same closed allow-set Load enforces).

// buildSkillAllow projects each agent's Skills slice onto the loader's per-agent
// allow-map (allow[agent] = set(agent.Skills)). An agent with no skills is absent
// from the map entirely — the loader's fail-secure default denies it everything.
// The skill names are the closed set: the Skill tool's {name} only SELECTS from
// this set, it is never interpolated into a path.
func buildSkillAllow(agents []skillScope) map[identity.AgentName]map[string]struct{} {
	allow := make(map[identity.AgentName]map[string]struct{}, len(agents))
	for _, a := range agents {
		if len(a.skills) == 0 {
			continue
		}
		set := make(map[string]struct{}, len(a.skills))
		for _, name := range a.skills {
			set[name] = struct{}{}
		}
		allow[a.name] = set
	}
	return allow
}

// skillScope is the minimal (agent name, allowed-skill names) pair the loader
// allow-map is built from — narrower than a full Agent (least privilege: the
// allow-map builder needs only identity + the skill set).
type skillScope struct {
	name   identity.AgentName
	skills []string
}

// availableSkillsCatalog renders the <available_skills> block for an agent: one
// "- <name>: <description>" line per allowed skill, read through the describer
// (so each entry is an authorized, parsed SKILL.md). Names are sorted for a
// deterministic prompt. An agent with no skills — or whose every skill fails to
// describe — yields the EMPTY string (no block), so a skill-less agent's system
// prompt is exactly Generic's catalog text, unchanged.
//
// A skill that fails to describe (missing/malformed embedded file — a catalogue
// integrity bug, not a runtime input) is SKIPPED rather than aborting the whole
// catalog: the agent still gets the skills that do resolve. This is fail-safe for
// a trusted, compiled-in catalogue; the loader's own tests pin the per-file
// parse behaviour.
func availableSkillsCatalog(ctx context.Context, describer skill.SkillDescriber, agent identity.AgentName, skills []string) string {
	if len(skills) == 0 {
		return ""
	}
	names := append([]string(nil), skills...)
	sort.Strings(names)

	var lines []string
	for _, name := range names {
		meta, err := describer.Describe(ctx, agent, name)
		if err != nil {
			continue // skip a skill whose embedded file is missing/malformed.
		}
		line := "- " + meta.Name
		if strings.TrimSpace(meta.Description) != "" {
			line += ": " + meta.Description
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("\n<available_skills>\n")
	for _, l := range lines {
		b.WriteString(l)
		b.WriteString("\n")
	}
	b.WriteString("</available_skills>")
	return b.String()
}
