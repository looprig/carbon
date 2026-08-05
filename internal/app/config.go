package app

import (
	"github.com/looprig/harness/pkg/loop"
	model "github.com/looprig/inference/model"
)

// Config contains the user-selected CodeRig application modes and the resolved,
// session-fixed access configuration. It is the app-level composition input the
// CLI fills before the Rig is constructed; the access profile cannot change for
// the lifetime of the session.
type Config struct {
	// ACPChildren is the optional, already-preflighted delegated-child
	// composition. Native primer operation remains available when it is nil or
	// when its catalog has no executable profiles.
	ACPChildren *ACPComposition
	// RuntimeCatalog is the complete parent-scoped catalogue of selectable
	// child runtimes. It remains available when ACP is disabled; ACPChildren
	// contributes only executable foreign builders and preflighted ACP rows.
	RuntimeCatalog loop.RuntimeCatalog
	// RuntimeSkills enables the untrusted, human-gated workspace skill source.
	RuntimeSkills bool
	// AccessProfile is the selected product access profile (readonly by
	// default). It is validated at the CLI boundary before Rig construction.
	AccessProfile AccessProfile
	// AccessConfigRev is the secret-free durable digest of the effective access
	// configuration (access ABI version, selected profile, normalized operator
	// and reviewer profiles, and the non-secret egress route identity and
	// guarantees). Assembly computes it with accessConfigDigest; the composition
	// root folds it into the configuration fingerprint so a product-profile,
	// reviewer-restriction, or egress-boundary change invalidates a restore. It
	// never carries a secret.
	AccessConfigRev string
	// ModelConfigRev is the secret-free digest of the normalized process model
	// configuration. Production composition sets it before rig assembly.
	ModelConfigRev string
	// PrimerAlias is the public, stable selector for the configured primer. It
	// is used only by runtime controls; provider routing remains private.
	PrimerAlias string
	// PrimerEfforts is the exact normalized effort allowlist for the configured
	// primer. Production composition copies it before runtime assembly.
	PrimerEfforts []model.Effort
	// PrimerCandidates is every configured primer-capable model
	// (uses: ["primer", ...]), in models.json order. Production composition
	// copies it from productionModels before runtime assembly; RuntimeAgent
	// uses it to offer a real /model picker instead of one fixed choice.
	PrimerCandidates []PrimerCandidate
	// DelegateModels is every configured gateway-backed delegate model
	// (models.json's ACPGatewaySource catalog) — the models StartAgent can
	// bind a native, in-process delegate loop to. Every native loop declares
	// these transports alongside PrimerCandidates' so a session with an
	// active or prior delegate on a different transport than the primer can
	// still restore. NativeACP delegates are not included: they never bind
	// to a CodeRig-owned loop.Definition.
	DelegateModels []model.Model
	// PermissionReviewEnabled turns on classifier-based automatic permission
	// review (off by default — a zero Config never auto-approves anything).
	// See internal/app/permission_review.go for the composition this enables.
	PermissionReviewEnabled bool
	// PermissionReviewModel is the named model bound to the command-safety
	// classifier. Required when PermissionReviewEnabled is true; CodeRig never
	// reuses an operator Loop's current-loop model for this (the design requires
	// an explicit named binding, not implicit inheritance).
	PermissionReviewModel model.Model
	// PermissionReviewStrictPolicy selects the stricter of the two supported
	// local decision policies (see internal/app/permission_review.go) instead of
	// the Codex-compatible default. It can only ever tighten the default's
	// ceilings, never loosen them. Ignored when PermissionReviewEnabled is false.
	PermissionReviewStrictPolicy bool
}

// ModelConfigCapabilityError reports that a production operation has no
// configured primer capability. It is intentionally bounded and carries no
// path, raw configuration, or credential material.
type ModelConfigCapabilityError struct{}

func (*ModelConfigCapabilityError) Error() string {
	return "coderig: model configuration has no configured primer"
}

// ModelFactory returns the secret-free model descriptor shared by CodeRig Loops.
type ModelFactory func() model.Model
