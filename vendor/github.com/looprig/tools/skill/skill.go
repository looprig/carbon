// Package skill implements the Skill tool: an on-demand reader of curated
// embedded (and optionally untrusted workspace) SKILL.md bodies, scoped to the
// one agent the tool is bound to. Preparation validates the name once, takes a
// TOCTOU-safe workspace snapshot when applicable, and emits a typed
// context.load requirement scoped to the skill identity.
package skill

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/tools/internal/workspace"
	"github.com/looprig/tools/permission"
)

// skill.go implements the Skill tool: an on-demand reader of a single curated,
// embedded SKILL.md body, scoped to the ONE agent the tool is bound to (design
// §7). The model names a skill ({name}) from the <available_skills> catalog the
// swarm injects into the agent's system prompt; the tool asks its injected
// SkillLoader for that body and returns it as the tool result.
//
// PER-AGENT BINDING / LEAST PRIVILEGE. A Skill tool is constructed PER AGENT and
// carries that agent's identity.AgentName as immutable state. It forwards (agent,
// name) to the loader, which authorizes the name against THAT agent's closed
// allow-set — so an agent can never load a skill outside its own boundary, and a
// traversal/unknown name is denied at the loader's gate before any path is built.
// The tool itself holds no catalog and no allow-map (DIP): it depends only on the
// narrow SkillLoader interface, never on a product package.
//
// TWO SKILL SOURCES — the enforced-gate model (design §7a). A Skill tool is
// constructed in one of two shapes:
//
//   - EMBEDDED-ONLY (NewSkill) — the agent may load only its curated,
//     compiled-in skills. Each load emits context.load scoped to the embedded
//     skill identity; the consumer's product access source decides its access.
//   - WORKSPACE-ENABLED (NewSkill + WithWorkspaceRoot) — the agent may ALSO load
//     an untrusted, project-local `.skills/<name>/SKILL.md`. An EMBEDDED name
//     still resolves via the loader (embedded-wins); a NON-embedded name is
//     treated as a WORKSPACE load, gated on its workspace skill identity and
//     bound to a TOCTOU-safe snapshot taken before the prompt.
//
// CAPABILITY SURFACE. Skill is an InvokableTool + CallPreparer + Auditable.
// PrepareCall owns the whole preparation boundary (decode/validate the name
// once, take the workspace snapshot once, emit the typed request); InvokableRun
// executes only the prepared artifact:
//
//   - Embedded name (embedded-wins, the workspace is NEVER consulted): request =
//     one context.load requirement scoped to the embedded skill identity;
//     artifact = the validated name (tool.TokenArtifact).
//   - Workspace-enabled non-embedded name: request = context.load scoped to the
//     workspace skill identity + the applicable filesystem.read requirement for
//     the canonical snapshot path (ONE combined prompt covers both); artifact =
//     the *tool.SkillArtifact snapshot (TOCTOU-safe: the bytes that run are the
//     bytes that were approved).
//   - InvokableRun: a workspace *tool.SkillArtifact in ctx → return its approved
//     snapshot Body (NEVER a re-read); a tool.TokenArtifact → the embedded
//     loader path for the PREPARED name; no artifact → fail closed.
//
// AUDIT. Skill implements tool.Auditable: the summary is "Skill <name>" — the
// requested skill NAME only, never the body. The body is curated/untrusted
// markdown, but the audit event is a redacted one-liner by contract, so only the
// name is surfaced.
//
// FAILURE MODEL. InvokableRun renders every execution failure as a tool-result
// error STRING (never a Go error, never echoing a body on an error path).
// PrepareCall is the one place a Go error is returned — by contract: unparsable
// args, an empty name, or a workspace load failure fail the call fail-secure in
// the runner before any gate opens or body executes.

// skillToolName is the EXACT tool name the runner and consumers key on.
const skillToolName = "Skill"

// skillSchema is the JSON Schema for Skill's argument object: a single required
// {name} string selecting a skill from the agent's <available_skills> catalog.
const skillSchema = `{
  "type": "object",
  "properties": {
    "name": {"type": "string", "description": "The name of the skill to load (see the available skills listed in the system prompt)."}
  },
  "required": ["name"]
}`

const skillDesc = "Load a named skill's instructions on demand and return them as the result. Pick a name from the <available_skills> catalog in your system prompt; the skill body is injected so you can apply it."

// skillArgs is the typed decode of Skill's untrusted argsJSON. The JSON field
// contract is {name string} — required.
type skillArgs struct {
	Name string `json:"name"`
}

// Skill loads a single named SKILL.md body for the ONE agent it is bound to. It
// depends only on the narrow SkillLoader (DIP) and its own immutable agent
// identity; it never imports a product package and holds no allow-map of its own.
//
// workspaceRoot is the OPTIONAL untrusted workspace source. When empty (the
// default), the tool is embedded-only and behaves exactly as in P2 (auto-approve,
// no Prepare/gate). When set (via WithWorkspaceRoot at the composition root, for
// an agent the operator grants runtime skills in Phase 3c), a non-embedded name is
// resolved as a human-gated workspace load. It is immutable after construction.
type Skill struct {
	loader        SkillLoader
	agent         identity.AgentName
	workspaceRoot string // "" → embedded-only; set → workspace-enabled (Ask-gated)
}

// SkillOption configures an optional capability on a Skill at construction. Options
// are applied at the composition root only; the resulting Skill is immutable.
type SkillOption func(*Skill)

// WithWorkspaceRoot enables the untrusted workspace skill source rooted at root: a
// non-embedded skill name is then resolved from <root>/.skills/<name>/SKILL.md as a
// human-gated (Ask) load bound to a TOCTOU-safe snapshot. An empty root is a no-op
// (the Skill stays embedded-only) — the fail-secure default. Only the wiring for an
// agent the operator grants runtime skills sets this (Phase 3c); embedded-only
// agents never receive it and so never gain the workspace source.
func WithWorkspaceRoot(root string) SkillOption {
	return func(s *Skill) { s.workspaceRoot = root }
}

// NewSkill constructs a Skill bound to a loader and the agent identity it serves.
// The swarm wires one per skilled agent at the composition root; the agent name is
// fixed at construction so every Load is scoped to that agent's closed allow-set.
// With no options it is embedded-only; pass WithWorkspaceRoot to additionally allow
// human-gated workspace skill loads.
func NewSkill(loader SkillLoader, agent identity.AgentName, opts ...SkillOption) *Skill {
	s := &Skill{loader: loader, agent: agent}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// workspaceEnabled reports whether this Skill has an untrusted workspace source
// configured (a non-empty workspace root). When false the tool is embedded-only.
func (s *Skill) workspaceEnabled() bool { return s.workspaceRoot != "" }

// Info returns Skill's self-description. Name MUST equal "Skill". The catalog
// of which skills are available is rendered into the agent's SYSTEM PROMPT by
// the swarm, not here, so the description is a static lead.
func (s *Skill) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{
		Name:   skillToolName,
		Desc:   skillDesc,
		Schema: json.RawMessage(skillSchema),
	}, nil
}

// AuditSummary returns a redacted, body-free one-line summary: the skill NAME
// only. An unparseable or empty-name args document yields a generic summary; the
// skill body is never included.
func (s *Skill) AuditSummary(argsJSON string) string {
	var a skillArgs
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil || strings.TrimSpace(a.Name) == "" {
		return "Skill (unparsable args)"
	}
	return "Skill " + a.Name
}

// decodeName decodes the untrusted argsJSON into the trimmed skill {name} and
// whether it is a non-empty, decodable name. It is the single argument-parsing
// seam shared by CheckEffect, Prepare, and InvokableRun so every path agrees on
// what "the requested name" is. ok=false means the args were unparseable or the
// name was empty — every caller treats that fail-secure.
func (s *Skill) decodeName(argsJSON string) (name string, ok bool) {
	var a skillArgs
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return "", false
	}
	name = strings.TrimSpace(a.Name)
	if name == "" {
		return "", false
	}
	return name, true
}

// CapabilityContextLoad is the normalized capability kind for loading curated
// context (a skill body) into the conversation. It is a PRODUCT-bound kind:
// the consumer routes it to its own access source via a gate AccessBinding
// (the access-profile spec names the product composition root's binding), never to the sandbox
// profile, and it is never silently mapped to command execution. It has no
// durable workspace-rule representation, so context.load requirements carry NO
// reusable candidates.
const CapabilityContextLoad = "context.load"

// EmbeddedSkillIdentity is the canonical skill identity (requirement Scope and
// Match) for a curated, compiled-in skill load.
func EmbeddedSkillIdentity(name string) string { return "embedded:" + name }

// WorkspaceSkillIdentity is the canonical skill identity (requirement Scope
// and Match) for an untrusted, project-local workspace skill load.
func WorkspaceSkillIdentity(name string) string { return "workspace:" + name }

// contextLoadRequirement builds the direct context.load requirement for one
// canonical skill identity (empty grant pair — the tool serves only the bound
// snapshot/curated body itself; no executor grant exists for context).
func contextLoadRequirement(identity, name string) tool.Requirement {
	return tool.Requirement{
		Kind:        CapabilityContextLoad,
		Scope:       identity,
		Match:       identity,
		Description: "load skill " + name,
	}
}

// PrepareCall decodes and validates the untrusted {name} ONCE and produces the
// typed access request plus the per-call artifact executed by InvokableRun —
// the TOCTOU-safe snapshot seam for a workspace load. Malformed or empty-name
// args fail here (fail-secure: no gate, no execution) in every configuration.
//
//   - Embedded-wins: an embedded name never consults the workspace (so an
//     embedded skill can never be shadowed by an attacker-planted workspace
//     file of the same name). The request carries one context.load requirement
//     scoped to the canonical EMBEDDED skill identity; the artifact binds the
//     validated name (a tool.TokenArtifact) so execution never reparses JSON.
//   - Workspace-enabled, non-embedded name → loadWorkspaceSkill takes the
//     snapshot ONCE (body + RelPath/Size/SHA256) and the request carries
//     context.load scoped to the WORKSPACE skill identity PLUS the applicable
//     filesystem.read requirement for the canonical snapshot path, so one
//     combined prompt covers both. A load error (containment/malformed/
//     not-found) fails preparation.
//   - Embedded-only, unknown name → an EMPTY request (no capability is
//     exercised: the load can only fail) with the name-bound artifact;
//     InvokableRun fails secure at the result with the UnknownSkillError
//     string, which names the valid recovery (pick a listed skill).
func (s *Skill) PrepareCall(_ context.Context, executionID uuid.UUID, argsJSON string) (tool.Request, tool.PreparedArtifact, error) {
	name, ok := s.decodeName(argsJSON)
	if !ok {
		return tool.Request{}, nil, &SkillContainmentError{Name: "", Reason: "name is empty or unparseable"}
	}
	request := tool.Request{ToolName: skillToolName, ExecutionID: executionID.String()}

	// Embedded-wins: never consult the workspace for an embedded name.
	if s.loader.Allowed(s.agent, name) {
		request.Requirements = []tool.Requirement{contextLoadRequirement(EmbeddedSkillIdentity(name), name)}
		return request, tool.TokenArtifact{Token: name}, nil
	}
	if !s.workspaceEnabled() {
		// Embedded-only, unknown name: no capability to request — execution
		// fails secure at the result with the loader's UnknownSkillError.
		return request, tool.TokenArtifact{Token: name}, nil
	}

	// Untrusted workspace load: take the TOCTOU-safe snapshot now.
	art, err := loadWorkspaceSkill(s.workspaceRoot, name)
	if err != nil {
		return tool.Request{}, nil, err
	}
	// Canonicalize the snapshot path for the filesystem.read requirement (the
	// spec's combined-prompt rule: a workspace skill also emits the applicable
	// filesystem requirement). loadWorkspaceSkill proved os.Root containment;
	// ContainedPath yields the canonical resolved form the profile scopes on.
	abs, err := workspace.ContainedPath(s.workspaceRoot, art.RelPath)
	if err != nil {
		return tool.Request{}, nil, &SkillContainmentError{Name: name, Reason: "skill path is not contained in the workspace root"}
	}
	loadReq := contextLoadRequirement(WorkspaceSkillIdentity(name), name)
	readReq := tool.Requirement{
		Kind:        permission.CapabilityFilesystemRead,
		Scope:       abs,
		Match:       abs,
		Description: "read " + abs,
	}
	request.Requirements = []tool.Requirement{loadReq, readReq}
	return request, &art, nil
}

// InvokableRun returns the requested skill body as the tool result, executing
// ONLY the prepared artifact bound to this call (the raw argsJSON is never
// reparsed; without an artifact the tool fails closed). For a WORKSPACE load
// it returns the APPROVED SNAPSHOT body PrepareCall bound to this call — read
// back from ctx via loop.PreparedCallFromContext, NEVER re-reading the file —
// so the bytes that run are exactly the bytes the human approved (TOCTOU-safe;
// a workspace writer cannot swap the body between the prompt and execution).
// For the embedded path it asks the bound loader for the curated body of the
// PREPARED name, scoped to this tool's agent. Every failure mode is a
// tool-result error STRING — never a Go error and never echoing a body on an
// error path.
func (s *Skill) InvokableRun(ctx context.Context, _ string) (*tool.ToolResult, error) {
	call, ok := loop.PreparedCallFromContext(ctx)
	if !ok {
		return tool.TextResult("error: permission denied: Skill requires its prepared call artifact"), nil
	}
	switch art := call.Artifact.(type) {
	case *tool.SkillArtifact:
		// Workspace path: the approved snapshot. Return its Body verbatim — no
		// decode, no loader, no re-read of the file.
		if art != nil && art.Workspace {
			return tool.TextResult(art.Body), nil
		}
	case tool.TokenArtifact:
		// Embedded path: the validated prepared name. Ask the loader for the
		// curated body.
		body, err := s.loader.Load(ctx, s.agent, art.Token)
		if err != nil {
			// The loader's typed errors carry a non-secret message (the skill
			// name + a reason); render it as a tool-result string so the model
			// can recover.
			return tool.TextResult("error: " + err.Error()), nil
		}
		return tool.TextResult(body), nil
	}
	return tool.TextResult("error: permission denied: Skill requires its prepared call artifact"), nil
}

// compile-time assertions: Skill is always an InvokableTool, a CallPreparer,
// and Auditable.
var (
	_ tool.InvokableTool = (*Skill)(nil)
	_ tool.CallPreparer  = (*Skill)(nil)
	_ tool.Auditable     = (*Skill)(nil)
)
