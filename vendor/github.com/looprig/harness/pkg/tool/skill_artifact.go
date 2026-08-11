package tool

// SkillArtifact is the concrete PreparedArtifact a workspace-skill preparation
// step produces: a single TOCTOU-safe snapshot of an untrusted
// `.skills/<name>/SKILL.md` taken ONCE — before the permission prompt — and
// bound to the call (design §7a). It satisfies the sealed PreparedArtifact
// interface via the unexported preparedArtifact marker, so it must live in this
// package alongside the seal; a type in another package cannot supply the
// marker and so cannot masquerade as a PreparedArtifact.
//
// One artifact carries BOTH halves of the load:
//   - the snapshot Body — read at execution time so the bytes that run are EXACTLY
//     the bytes that were approved (never a re-open, which a workspace writer could
//     swap between prompt and execution);
//   - the metadata + hash (RelPath, Size, SHA256) — surfaced for the human gate,
//     which renders the metadata but never the Body.
//
// Workspace distinguishes a workspace (untrusted, gated) snapshot from any future
// trusted source; embedded skills are auto-approved and need no artifact.
type SkillArtifact struct {
	Workspace bool   // true for an untrusted workspace snapshot
	RelPath   string // workspace-relative source path, e.g. ".skills/<name>/SKILL.md"
	Size      int64  // snapshot length in bytes
	SHA256    string // full hex SHA-256 of the snapshot bytes
	Body      string // the parsed markdown body to inject (the approved bytes)
}

func (SkillArtifact) preparedArtifact() {}
