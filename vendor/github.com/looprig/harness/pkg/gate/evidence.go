package gate

import (
	"context"

	"github.com/looprig/harness/pkg/tool"
)

// EvidenceAccessEvaluator is the deliberately non-interactive, read-only
// access seam Harness's evidence-tool runtime (internal/hustleruntime, design
// §13.1) uses to evaluate one prepared evidence Requirement's configured
// access state. AccessBindings satisfies it structurally without exposing
// approval, stored-rule, persistence, or grant capabilities; a consumer's own
// access source (for example a sandbox access-profile adapter) may implement
// it directly. It lives in this public package — rather than
// internal/hustleruntime, which an out-of-module consumer like a rig
// implementation cannot import — so a consumer can name and implement the
// interface and its collaborator types (see EvidenceContainmentPolicy) and
// install them via rig.WithPermissionReviewEvidence.
type EvidenceAccessEvaluator interface {
	AccessFor(tool.Requirement) (uint8, error)
}

// EvidenceContainmentPolicy is the complete security context exposed to an
// EvidenceContainmentVerifier. ReadRoot must be the canonical workspace root;
// SecurityCeiling is the effective, non-widenable policy for the ONE review
// this evidence call belongs to. It is always sourced from that review's own
// frozen basis (hustle.Request.SecurityCeiling), never a session-wide
// constant — a long-running session's later review must be bound against ITS
// OWN current ceiling, not one frozen at session or controller construction.
type EvidenceContainmentPolicy struct {
	ReadRoot        string
	SecurityCeiling string
}

// EvidenceContainmentVerifier independently resolves every prepared evidence
// target, including symlinks and ambiguous scopes, against the canonical read
// root and enforces the configured security ceiling (design §13.1). It
// receives no session, gate, mutation, grant, rule, or loop-control
// capability — only the two policy values above and a defensive clone of the
// normalized prepared request. Implementations must fail closed when a
// tool-owned Requirement cannot be mapped unambiguously. This is the narrow,
// trusted-caller seam a consumer implements and installs via
// rig.WithPermissionReviewEvidence.
type EvidenceContainmentVerifier interface {
	VerifyEvidenceContainment(ctx context.Context, policy EvidenceContainmentPolicy, request tool.Request) error
}
