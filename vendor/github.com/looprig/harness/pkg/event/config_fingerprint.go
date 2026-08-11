package event

// ConfigFingerprint is the stable identity of the agent configuration a session
// started under, stamped onto SessionStarted so a durable journal can detect when a
// restore is being attempted against a materially changed config (a different
// model, system prompt, tool policy, skill-trust mode, workspace, foreign adapter, or
// permission posture). It is a fingerprint, not the config itself: each field is either
// a verbatim identifier (AgentKind, ModelID, WorkspaceRoot, AgentAdapter,
// PermissionPosture), a content digest (SystemPromptRev, ToolPolicyRev), or a mode flag
// (RuntimeSkills) — never the raw prompt text or tool definitions — so it is safe to
// persist and compare without leaking definition internals. Package rig freezes the
// fingerprint from its registered loop definitions, topology, and composition fields;
// this package only defines the durable value and its equality.
//
// The fields evolve ADDITIVELY: every field is omitzero, so an old journal record
// that predates a field decodes it as the zero value and compares Equal to a record
// that also leaves it empty — a session persisted before a field was added restores
// without a spurious mismatch.
type ConfigFingerprint struct {
	// TopologyRev is the digest of ordered loop definitions, primer roots, active
	// primer, and delegation edges owned by the rig.
	TopologyRev string `json:"topology_rev,omitzero"`
	// AgentKind names the application and Loop role this session ran (e.g. "carbon:carbon").
	// It is empty for a caller that does not inject a kind (a non-swarm/legacy session).
	AgentKind string `json:"agent_kind,omitzero"`
	// ModelID is the model identifier the session ran against (the llm.Model.Name).
	ModelID string `json:"model_id,omitzero"`
	// SystemPromptRev is a content digest (hex sha256) of the system prompt text, so
	// a prompt change is detectable without persisting the prompt itself.
	SystemPromptRev string `json:"system_prompt_rev,omitzero"`
	// ToolPolicyRev is a content digest (hex sha256) over the tool set's stable
	// identity (its sorted tool names), so a tool-set change is detectable without
	// persisting the tool definitions.
	ToolPolicyRev string `json:"tool_policy_rev,omitzero"`
	// RuntimeSkills records whether the untrusted, human-gated workspace skill source
	// was enabled for this session. A session must not silently resume under a
	// different skill-trust mode, so the flag is part of the fingerprint. It is the
	// MODE only — the flag alone does NOT distinguish two repos' .skills/, which is
	// what WorkspaceRoot is for.
	RuntimeSkills bool `json:"runtime_skills,omitzero"`
	// WorkspaceRoot is the canonical absolute workspace-root id (filepath.Clean of the
	// absolute root). It binds the session to the repo whose .skills/ (and file tools)
	// it ran against, so a session cannot silently resume under a different repo's
	// workspace. Empty for a caller that does not inject a root.
	WorkspaceRoot string `json:"workspace_root,omitzero"`
	// AgentAdapter identifies the foreign-agent adapter that backed this session
	// (e.g. "claude"). Empty for a native session. A session must not silently resume
	// under a different foreign adapter, so it is part of the fingerprint.
	AgentAdapter string `json:"agent_adapter,omitzero"`
	// PermissionPosture is the non-interactive permission mode the foreign agent ran
	// under (e.g. "default", "acceptEdits"). Empty for a native session. A change in
	// posture is a behavior change that must not resume unnoticed.
	PermissionPosture string `json:"permission_posture,omitzero"`
	// NativePermissionPolicyRev is an opaque content digest (hex sha256) of the
	// NATIVE permission and access configuration, computed and injected by the
	// composition root (e.g. over the selected access profiles and rule policy).
	// Harness only compares it. Empty for a foreign session (which uses
	// PermissionPosture) or a caller that does not inject it. A change is a
	// behavior change that must not resume unnoticed.
	NativePermissionPolicyRev string `json:"native_permission_policy_rev,omitzero"`
	// ExternalCapabilityRev is a content digest over the identity of the EXTERNAL
	// capabilities an application attached to this session — tools, prompts and
	// resources served by processes Harness does not own, such as MCP servers.
	// Empty means the session had none, which is what makes the field additive:
	// a journal written before it existed decodes it empty and compares Equal to
	// a live config that also has none.
	//
	// Harness neither computes nor interprets it. It is supplied by the
	// composition root, which is the only layer that knows what it attached; the
	// canonical producer today is github.com/looprig/mcp's
	// mcpharness.Manager.ConfigDigest. Harness's part of the contract is the two
	// properties every other Rev field here has: it is a digest, so it carries
	// identity and not the configuration — a server's credentials, headers, and
	// environment must never reach a journal — and it is compared, never parsed.
	//
	// It is ONE opaque string rather than a structured manifest deliberately.
	// The richer model — per-binding manifests, configuration epochs, and typed
	// drift — is specified in docs/plans/2026-07-16-session-versioning-migration-design.md
	// and is NOT implemented. Until it is, external capability drift is reported
	// through the same one-shot mechanism as every other config change: a
	// fingerprint mismatch at restore, which sessionruntime's
	// WithAllowConfigMismatch decides on. A field that promised more than that
	// would be a promise nothing here keeps.
	ExternalCapabilityRev string `json:"external_capability_rev,omitzero"`
	// RuntimeProfile is the secret-free backend profile selected for this bound
	// loop. Empty preserves native and legacy callers.
	RuntimeProfile string `json:"runtime_profile,omitzero"`
	// RuntimeCatalogRev identifies the parent-scoped runtime catalog snapshot.
	RuntimeCatalogRev string `json:"runtime_catalog_rev,omitzero"`
	// RuntimeIdentityRev is an opaque hex digest of the selected runtime tuple.
	// It is produced by loop.BoundDefinition.RuntimeIdentity().Digest() and
	// covers the model alias, target key, effective effort (including canonical
	// none), profile, and catalog without persisting raw model descriptors.
	RuntimeIdentityRev string `json:"runtime_identity_rev,omitzero"`
}

// Equal reports whether two fingerprints identify the same configuration: true iff
// every field is equal. It is the comparison a restore uses to decide whether the
// persisted config still matches the live one. New fields are additive (omitzero), so
// an old record's empty new field equals a current record that also leaves it empty.
func (f ConfigFingerprint) Equal(other ConfigFingerprint) bool {
	return f.TopologyRev == other.TopologyRev &&
		f.AgentKind == other.AgentKind &&
		f.ModelID == other.ModelID &&
		f.SystemPromptRev == other.SystemPromptRev &&
		f.ToolPolicyRev == other.ToolPolicyRev &&
		f.RuntimeSkills == other.RuntimeSkills &&
		f.WorkspaceRoot == other.WorkspaceRoot &&
		f.AgentAdapter == other.AgentAdapter &&
		f.PermissionPosture == other.PermissionPosture &&
		f.NativePermissionPolicyRev == other.NativePermissionPolicyRev &&
		f.ExternalCapabilityRev == other.ExternalCapabilityRev &&
		f.RuntimeProfile == other.RuntimeProfile &&
		f.RuntimeCatalogRev == other.RuntimeCatalogRev &&
		f.RuntimeIdentityRev == other.RuntimeIdentityRev
}
