package app

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/tool"
)

// permission_review_evidence.go is CodeRig's implementation of the two
// read-only, trusted-caller seams a registered permission classifier's
// evidence tools run under (design §13.1, harness/pkg/gate/evidence.go):
// gate.EvidenceAccessEvaluator and gate.EvidenceContainmentVerifier. Both are
// installed together via rig.WithPermissionReviewEvidence
// (internal/app/permission_review.go) whenever permission review is enabled.
//
// Harness's evidence runner (internal/hustleruntime.authorizeEvidenceRequest)
// already enforces, BEFORE either seam here is ever consulted:
//   - every requirement's Kind is a member of the consumer-supplied
//     allowedKinds allowlist (commandsafety.RequiredEvidenceKinds(), passed
//     to rig.WithPermissionReviewEvidence alongside these two values);
//   - no requirement carries grant semantics (GrantClass, GrantTarget, or
//     Candidates) — an evidence call never mints an executor grant;
//   - the prepared tool.Request passes tool.ValidateRequest.
//
// That division of labor makes permissionReviewEvidenceAccess.AccessFor
// structurally simple: by the time it is called, kind-allowlisting is
// already done, so its only job is a defensive re-check (never trust a
// caller blindly) before answering AccessAllow. The REAL per-target security
// decision — whether the requirement's resolved target actually lives inside
// the review's own workspace root, and whether this review's security
// ceiling matches what CodeRig expects — is permissionReviewEvidenceContainment's
// job, run as an INDEPENDENT second check alongside the evidence tools' own
// syscall-level *os.Root confinement (classifiers/internal/evidence): defense
// in depth, not the only line of defense.

// permissionReviewEvidenceAccess is CodeRig's gate.EvidenceAccessEvaluator.
// It is stateless and safe for concurrent use.
type permissionReviewEvidenceAccess struct{}

// newPermissionReviewEvidenceAccess returns CodeRig's evidence access
// evaluator.
func newPermissionReviewEvidenceAccess() permissionReviewEvidenceAccess {
	return permissionReviewEvidenceAccess{}
}

// AccessFor answers the configured access state for one prepared evidence
// Requirement. Every requirement that reaches this method has already
// survived Harness's allowedKinds filter and grant-semantics rejection (see
// the file doc comment); AccessFor re-checks both defensively rather than
// trusting that contract blindly, and fails closed (AccessDeny, non-nil
// error — never a panic) on anything it cannot classify. It grants no
// authority itself: the actual per-target decision is
// permissionReviewEvidenceContainment's, which Harness always runs before
// AccessFor is consulted for the surrounding request.
func (permissionReviewEvidenceAccess) AccessFor(requirement tool.Requirement) (uint8, error) {
	if strings.TrimSpace(requirement.Kind) == "" {
		return gate.AccessDeny, errors.New("coderig: evidence requirement has no kind")
	}
	if requirement.GrantClass != "" || requirement.GrantTarget != "" || len(requirement.Candidates) != 0 {
		return gate.AccessDeny, errors.New("coderig: evidence requirement unexpectedly carries grant semantics")
	}
	return gate.AccessAllow, nil
}

var _ gate.EvidenceAccessEvaluator = permissionReviewEvidenceAccess{}

// Sentinel containment errors. They are unexported and carry no request
// content, matching this file's fail-closed, secret-free error discipline.
var (
	errEvidenceContainmentAmbiguous = errors.New("coderig: evidence requirement target could not be unambiguously resolved within the review workspace")
	errEvidenceContainmentEscape    = errors.New("coderig: evidence requirement target resolves outside the review workspace")
	errEvidenceContainmentCeiling   = errors.New("coderig: evidence review security ceiling does not match this session")
)

// permissionReviewEvidenceContainment is CodeRig's gate.EvidenceContainmentVerifier.
// ceiling is the ONE security-ceiling value this session's evidence reviews
// may ever present (CodeRig's selected AccessProfile name — the same
// effective-ceiling representation accessConfigDigest/access.go already
// folds into the session's durable access identity); any other value fails
// closed rather than being compared with a string-ordering heuristic. It is
// stateless beyond that one immutable field and safe for concurrent use.
type permissionReviewEvidenceContainment struct {
	ceiling string
}

// newPermissionReviewEvidenceContainment returns CodeRig's evidence
// containment verifier bound to ceiling (the session's selected access
// profile name). An empty ceiling is accepted structurally but VerifyEvidenceContainment
// then fails closed for every call, since an unconfigured expected ceiling
// can never legitimately match a live review's SecurityCeiling.
func newPermissionReviewEvidenceContainment(ceiling string) permissionReviewEvidenceContainment {
	return permissionReviewEvidenceContainment{ceiling: ceiling}
}

// VerifyEvidenceContainment independently resolves every requirement in
// request against policy.ReadRoot, symlinks and all, and enforces
// policy.SecurityCeiling against the one ceiling value this session trusts.
// It receives only the two policy values and a defensive clone of the
// prepared request (per gate.EvidenceContainmentVerifier's contract) — no
// session, gate, mutation, grant, rule, or loop-control capability. It never
// panics: any unexpected internal failure is recovered and reported as a
// plain error, so a malformed or adversarial input can only ever fail
// closed, never crash the caller.
func (v permissionReviewEvidenceContainment) VerifyEvidenceContainment(ctx context.Context, policy gate.EvidenceContainmentPolicy, request tool.Request) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = errEvidenceContainmentAmbiguous
		}
	}()
	if ctx == nil {
		return errEvidenceContainmentAmbiguous
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	// Equality against the ONE ceiling this session trusts — never a
	// string-ordering/prefix heuristic (a naive "ceiling >= required" scheme
	// invites exactly the kind of silent-widening bug this check exists to
	// prevent).
	if v.ceiling == "" || policy.SecurityCeiling != v.ceiling {
		return errEvidenceContainmentCeiling
	}

	// Canonicalize the ROOT itself, not just each target: a mismatch here
	// would falsely reject legitimate targets (or, worse, falsely accept an
	// escape) if only one side of the later Rel comparison were resolved.
	canonicalRoot, err := canonicalizeEvidenceReadRoot(policy.ReadRoot)
	if err != nil {
		return err
	}

	for _, requirement := range request.Requirements {
		if err := verifyRequirementContainment(canonicalRoot, requirement); err != nil {
			return err
		}
	}
	return nil
}

var _ gate.EvidenceContainmentVerifier = permissionReviewEvidenceContainment{}

// canonicalizeEvidenceReadRoot canonicalizes root (a gate.EvidenceContainmentPolicy's
// ReadRoot) via filepath.EvalSymlinks, requiring it to be a non-empty
// absolute path that exists. This is the ONE root-canonicalization both
// permissionReviewEvidenceContainment.VerifyEvidenceContainment (above) and
// permissionReviewEvidenceObservation.VerifyEvidenceObservations
// (permission_review_observation.go) use — both verifiers share the exact
// same notion of "the review workspace root" so the two can never
// independently drift on what "contained" means.
func canonicalizeEvidenceReadRoot(root string) (string, error) {
	root = strings.TrimSpace(root)
	if root == "" || !filepath.IsAbs(root) {
		return "", errEvidenceContainmentAmbiguous
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", errEvidenceContainmentAmbiguous
	}
	return canonicalRoot, nil
}

// verifyRequirementContainment resolves ONE requirement's target against
// canonicalRoot (already symlink-resolved) and fails closed unless the
// resolved target is demonstrably contained within it.
func verifyRequirementContainment(canonicalRoot string, requirement tool.Requirement) error {
	if strings.TrimSpace(requirement.Kind) == "" {
		return errEvidenceContainmentAmbiguous
	}
	rel, err := evidenceRelativeTarget(requirement.Match)
	if err != nil {
		return err
	}
	// rel is already lexically root-relative (no "..", not absolute), so this
	// join can never lexically escape canonicalRoot. The REAL, authoritative
	// check is canonicalizeWithinRoot below, which resolves symlinks — lexical
	// containment alone (a raw strings.HasPrefix-style check) is exactly the
	// naive-prefix bug this function must not repeat.
	candidate := filepath.Join(canonicalRoot, rel)

	canonicalCandidate, err := canonicalizeWithinRoot(canonicalRoot, candidate)
	if err != nil {
		return err
	}
	relToRoot, err := filepath.Rel(canonicalRoot, canonicalCandidate)
	if err != nil {
		return errEvidenceContainmentEscape
	}
	if relToRoot == ".." || strings.HasPrefix(relToRoot, ".."+string(filepath.Separator)) || filepath.IsAbs(relToRoot) {
		return errEvidenceContainmentEscape
	}
	return nil
}

// evidenceRelativeTarget independently derives a root-relative candidate path
// from a requirement's Match field, WITHOUT trusting that an evidence tool
// already validated it (design's "the runtime does not infer containment
// from the generic, tool-owned Requirement.Scope or Requirement.Match
// strings" — this is CodeRig's OWN re-validation, defense in depth against a
// buggy or compromised tool). "" and "." both mean "the workspace root
// itself" (the real filesystem/git evidence tools use exactly this
// convention — see classifiers/internal/evidence/path.go's
// resolveRootRelative — including the git tools' fixed non-path sentinel
// Match values such as "repository-status", which this function treats like
// any other plain root-relative component: if no such entry exists under
// root, containment falls back to root itself, which trivially succeeds).
// Any absolute path or a cleaned path starting with ".." is rejected
// outright as an escape attempt, matching the guidance to reject a pattern
// carrying ".." or an absolute component before ever touching the
// filesystem.
func evidenceRelativeTarget(match string) (string, error) {
	if !utf8.ValidString(match) || strings.ContainsRune(match, '\x00') {
		return "", errEvidenceContainmentAmbiguous
	}
	if match == "" || match == "." {
		return ".", nil
	}
	if filepath.IsAbs(match) {
		return "", errEvidenceContainmentEscape
	}
	cleaned := filepath.Clean(match)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) || filepath.IsAbs(cleaned) {
		return "", errEvidenceContainmentEscape
	}
	return cleaned, nil
}

// canonicalizeWithinRoot resolves candidate (canonicalRoot joined with an
// already-validated root-relative path) to its canonical, symlink-resolved
// form. filepath.EvalSymlinks errors on a target that does not exist yet
// (relevant for e.g. a stat call against a path that may not exist), so this
// walks up to the DEEPEST EXISTING ancestor, canonicalizes THAT, and
// reattaches the (by definition non-existent, so symlink-free) trailing
// components. canonicalRoot itself is guaranteed to exist (the caller
// already resolved it), so the walk is always bounded and always
// terminates. If even canonicalRoot's own resolution had failed, the caller
// would already have failed closed before this is ever invoked.
func canonicalizeWithinRoot(canonicalRoot, candidate string) (string, error) {
	current := candidate
	var trailing []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			full := resolved
			for i := len(trailing) - 1; i >= 0; i-- {
				full = filepath.Join(full, trailing[i])
			}
			return full, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			// Any other resolution failure (permission denied, I/O error, a
			// non-directory component in the middle of the path, ...) is not
			// a "missing target" — fail closed rather than guessing.
			return "", errEvidenceContainmentAmbiguous
		}
		parent := filepath.Dir(current)
		if parent == current || len(current) < len(canonicalRoot) {
			// Walked at or above canonicalRoot's own depth without finding an
			// existing ancestor. canonicalRoot itself is known to exist, so
			// this is unreachable in practice; fail closed rather than loop.
			return "", errEvidenceContainmentAmbiguous
		}
		trailing = append(trailing, filepath.Base(current))
		current = parent
	}
}

// evidenceCeilingFor returns the ONE SecurityCeiling value CodeRig's
// permission-review evidence containment verifier trusts for profile: the
// selected AccessProfile's name, defaulting exactly like access.go's own
// buildSessionAccess does when profile is unset. It is the single source
// both newPermissionReviewRegistration (production wiring) and this file's
// tests use, so the two can never silently drift.
func evidenceCeilingFor(profile AccessProfile) string {
	if profile == "" {
		profile = DefaultAccessProfile
	}
	return string(profile)
}
