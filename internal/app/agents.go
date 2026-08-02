package app

import (
	"github.com/looprig/coderig/internal/catalog/builder"
	"github.com/looprig/coderig/internal/catalog/planner"
	"github.com/looprig/coderig/internal/catalog/reviewer"
	"github.com/looprig/harness/pkg/identity"
)

// builderSkills is the builder's closed set of allowed embedded skills. This is the
// single source of truth the loader's allow-map AND the agent's <available_skills>
// catalog are both derived from.
var builderSkills = []string{"code-style"}

// operatorSkills is retained for package-local legacy fixtures; builderSkills
// is the production source of truth.
//
//lint:ignore U1000 retained for package-local legacy fixtures.
var operatorSkills = builderSkills

// leafBuiltin is each agent's package-exported boundary as pure metadata: Name and Role
// plus allowed embedded Skills, runtime-skills eligibility, and the static per-role
// security mode. It no longer carries a tool builder — the composition root (swarm.go) builds
// each loop.Definition directly from the leaf package's BuildTools — so this struct is the ONE
// place the per-agent skill set, runtime-skills eligibility, and role prompt are declared for
// the skill loader allow-map and the <available_skills> catalog.
type leafBuiltin struct {
	name        identity.AgentName
	description string
	role        string
	skills      []string
	// allowsRuntimeSkills marks a leaf eligible for the untrusted, human-gated workspace
	// skill source (§7a). True ONLY for the builder, which owns workspace mutation. Both
	// this per-agent gate AND the swarm-wide cfg.RuntimeSkills mode must be true to wire
	// the workspace source.
	allowsRuntimeSkills bool
}

func plannerBuiltin() leafBuiltin {
	return leafBuiltin{
		name:        planner.Name,
		description: planner.Description,
		role:        planner.Role,
	}
}

func builderBuiltin() leafBuiltin {
	return leafBuiltin{
		name:                builder.Name,
		description:         builder.Description,
		role:                builder.Role,
		skills:              builderSkills,
		allowsRuntimeSkills: true,
	}
}

// operatorBuiltin is retained for package-local legacy fixtures. Production
// uses builderBuiltin as the workspace-writing role.
//
//lint:ignore U1000 retained for package-local legacy fixtures.
func operatorBuiltin() leafBuiltin { return builderBuiltin() }

func reviewerBuiltin() leafBuiltin {
	return leafBuiltin{
		name:        reviewer.Name,
		description: reviewer.Description,
		role:        reviewer.Role,
	}
}

// leafBuiltins is the fixed roster in deterministic catalog order.
func leafBuiltins() []leafBuiltin {
	return []leafBuiltin{plannerBuiltin(), builderBuiltin(), reviewerBuiltin()}
}
