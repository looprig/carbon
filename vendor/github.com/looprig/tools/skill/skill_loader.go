package skill

import (
	"context"
	"errors"
	"io/fs"
	"path"

	"github.com/looprig/harness/pkg/identity"
)

// SkillLoader resolves a named skill into the markdown body to inject into an
// agent's context. It is the narrow seam between the Skill tool and the on-disk
// (embedded) skill catalogue: the tool asks for a body by (agent, name) and the
// loader is responsible for authorizing the request and reading the document.
//
// The agent identity is a parameter — not loader state — so a single loader can
// serve every agent in a swarm while still scoping each call to that agent's own
// allowed-skill set. Implementations MUST be fail-secure: an unauthorized or
// unknown name is denied, never guessed.
//
// Allowed is the read-only membership predicate over the SAME closed allow-set
// Load authorizes against, WITHOUT touching the filesystem: it answers "is name an
// embedded skill this agent may load?". The workspace-aware Skill tool uses it as
// the embedded-wins discriminator (embedded names auto-approve and resolve via
// Load; only a NON-embedded name is ever considered for an untrusted workspace
// load). It is fail-secure: an unknown agent, a nil allow-map, or a non-member
// name all report false. Both methods are used by the single Skill-tool consumer,
// so combining them does not over-widen the interface (interface segregation).
type SkillLoader interface {
	Load(ctx context.Context, agent identity.AgentName, name string) (string, error)
	Allowed(agent identity.AgentName, name string) bool
}

// SkillDescriber resolves a named skill into its frontmatter METADATA
// (name+description) WITHOUT the body — the data the swarm renders into an
// agent's <available_skills> catalog. It authorizes (agent, name) against the
// SAME closed allow-set as SkillLoader.Load, so a catalog can only ever list a
// skill the agent is actually allowed to load. It is a separate, focused
// interface (interface segregation): the Skill tool depends only on SkillLoader;
// the catalog builder depends only on SkillDescriber.
type SkillDescriber interface {
	Describe(ctx context.Context, agent identity.AgentName, name string) (SkillMeta, error)
}

// embeddedSkillLoader reads SKILL.md documents from a compiled-in fs.FS (the
// product injects its embedded skills at the composition root and authorizes each
// request against a static, per-agent allow-map.
//
// The allow-map is a closed set per agent: allow[agent] is the exact set of
// skill names that agent may load, and a name is authorized iff it is a member.
// The map is owned by the loader and treated as read-only after construction; it
// is never mutated by Load, so concurrent Load calls are safe (fs.FS reads are
// independent and the embed.FS the swarm injects is itself read-only).
type embeddedSkillLoader struct {
	fsys  fs.FS
	allow map[identity.AgentName]map[string]struct{}
}

// NewEmbeddedSkillLoader wires an embeddedSkillLoader from an injected file
// system and a per-agent allow-map. fsys is the catalogue root holding
// skills/<name>/SKILL.md (an embed.FS satisfies fs.FS); allow[agent] is that
// agent's closed set of permitted skill names. Dependencies are injected here at
// the composition root so the tools package never imports the embed package
// product, keeping the dependency arrow product -> tools and cycle-free.
//
// A nil allow-map is treated as "no agent is authorized for anything" — the
// fail-secure default. The returned concrete type satisfies SkillLoader.
func NewEmbeddedSkillLoader(fsys fs.FS, allow map[identity.AgentName]map[string]struct{}) *embeddedSkillLoader {
	return &embeddedSkillLoader{fsys: fsys, allow: allow}
}

// Load authorizes (agent, name) against the closed allow-set, then reads and
// parses skills/<name>/SKILL.md, returning the markdown body. It delegates to
// resolve, which owns the deliberate authorize-before-path traversal-safety
// guarantee.
func (l *embeddedSkillLoader) Load(ctx context.Context, agent identity.AgentName, name string) (string, error) {
	_, body, err := l.resolve(ctx, agent, name)
	if err != nil {
		return "", err
	}
	return body, nil
}

// Describe authorizes (agent, name) against the SAME closed allow-set as Load,
// then reads and parses skills/<name>/SKILL.md and returns ONLY its frontmatter
// metadata (name+description), discarding the body. The swarm uses it to build
// the trusted <available_skills> catalog injected into the agent's system prompt.
func (l *embeddedSkillLoader) Describe(ctx context.Context, agent identity.AgentName, name string) (SkillMeta, error) {
	meta, _, err := l.resolve(ctx, agent, name)
	if err != nil {
		return SkillMeta{}, err
	}
	return meta, nil
}

// resolve is the shared authorize→build-path→read→parse core behind Load and
// Describe. The authorization gate runs BEFORE any path is built (the
// traversal-safety guarantee): the model-supplied name is untrusted, so it must
// first be proven a member of the agent's closed allow-set; only a validated
// member is ever joined into a path. A traversal payload like "../../etc/passwd"
// can never be a member of the curated set, so it is rejected at the gate and no
// path is ever constructed from it. The subsequent path.Join is defense-in-depth.
func (l *embeddedSkillLoader) resolve(ctx context.Context, agent identity.AgentName, name string) (SkillMeta, string, error) {
	// Honor cancellation up front; reads below are from a compiled-in fs and do
	// not otherwise observe the context, so this is the one place it can matter.
	if err := ctx.Err(); err != nil {
		return SkillMeta{}, "", err
	}

	// 1. Authorize first (fail-secure). The name is untrusted until it is proven
	// to be a member of this agent's closed allow-set.
	if !l.authorized(agent, name) {
		return SkillMeta{}, "", &UnknownSkillError{Agent: agent, Name: name}
	}

	// 2. Build the path only now that name is a validated closed-set member.
	// path.Join cleans the result; with an authorized (traversal-free) name this
	// is purely defensive.
	skillPath := path.Join("skills", name, "SKILL.md")

	raw, err := fs.ReadFile(l.fsys, skillPath)
	if err != nil {
		return SkillMeta{}, "", &SkillNotFoundError{Name: name, Err: err}
	}

	// 3. Parse. Propagate the parser's *MalformedSkillError, stamping the now-known
	// skill name (the parser sets Name="" since it sees only raw bytes).
	meta, body, err := parseSkill(raw)
	if err != nil {
		var me *MalformedSkillError
		if errors.As(err, &me) && me.Name == "" {
			me.Name = name
		}
		return SkillMeta{}, "", err
	}

	return meta, body, nil
}

// Allowed reports whether name is an embedded skill this agent may load, reading
// the closed allow-set ONLY (no filesystem touch). It is the exported, read-only
// view of the same gate Load authorizes against; the workspace-aware Skill tool
// consults it as the embedded-wins discriminator. Fail-secure: unknown agent, nil
// map, or non-member name → false.
func (l *embeddedSkillLoader) Allowed(agent identity.AgentName, name string) bool {
	return l.authorized(agent, name)
}

// authorized reports whether agent may load the named skill: true iff name is a
// member of the agent's closed allow-set. An agent absent from the map, a nil
// map, or a name absent from the agent's set all deny — the fail-secure default.
func (l *embeddedSkillLoader) authorized(agent identity.AgentName, name string) bool {
	set, ok := l.allow[agent]
	if !ok {
		return false
	}
	_, ok = set[name]
	return ok
}
