package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/looprig/harness/pkg/gate"
)

// permission_review_observation.go is Carbon's implementation of the
// TOCTOU-recheck seam, gate.EvidenceObservationVerifier (design §13.4,
// Addendum 4, harness/pkg/gate/observation.go). It is installed via
// rig.WithPermissionReviewObservations alongside
// permission_review_evidence.go's two seams whenever permission review is
// enabled (permission_review.go's newPermissionReviewRegistration).
//
// gate.EvidenceObservationVerifier's own doc comment mirrors
// gate.EvidenceContainmentVerifier's shape exactly, and this file follows
// suit: same trusted-caller narrowness (no session, gate, mutation, grant,
// rule, or loop-control capability — only the policy and a defensive copy
// of the requirements to recheck), same reused gate.EvidenceContainmentPolicy,
// same fail-closed-on-anything-ambiguous posture as
// permissionReviewEvidenceContainment, and the SAME shared root
// canonicalization (canonicalizeEvidenceReadRoot, permission_review_evidence.go)
// so the two verifiers' notion of "the review workspace root" can never
// drift apart.
//
// Harness's runtime (internal/sessionruntime/gates.go's
// verifyPermissionReviewObservations) calls this ONLY when a
// classifier-originated auto-approval is about to claim a gate and at least
// one target-sensitive evidence tool recorded an observation during that
// review's evidence gathering. It runs synchronously, immediately before the
// gate claim, and any non-nil error here leaves the gate open for a human —
// never denies, never persists a rule, never widens anything.
//
// ---- token derivation scheme: this file INDEPENDENTLY reimplements it ----
//
// github.com/looprig/classifiers/internal/evidence/observation.go defines
// the ONE token formula every target-sensitive evidence tool
// (evidence_filesystem_stat, evidence_filesystem_read) uses to record an
// ObservationRequirement at evidence-gathering time. This file
// independently reproduces that EXACT formula — not by importing
// classifiers' package (an internal package of a separate module, and
// deliberately not shared even if it were importable: Harness never trusts
// the reporter, only a completely independent fresh recomputation makes
// this recheck genuinely trustworthy) — so the resulting hex digest is
// byte-for-byte identical to whatever a real filesystem evidence tool
// recorded, for the SAME live target. See
// filesystemObservationFingerprint below for the field-by-field formula,
// and permission_review_observation_test.go's
// TestFingerprintMatchesIndependentReferenceFormula for the cross-check
// that proves this reimplementation stayed faithful.
//
// "fsv1" is a fixed scheme-version literal, copied verbatim from
// classifiers' own scheme: changing the field list, order, or format here
// without also bumping classifiers' own "fsv1" would silently break the
// wire compatibility this whole mechanism depends on — the two are meant to
// change together, by design, even though they are two independently
// maintained implementations.

// permissionReviewEvidenceObservation is Carbon's gate.EvidenceObservationVerifier.
// ceiling is the ONE security-ceiling value this session's observation
// rechecks may ever present — the identical value permissionReviewEvidenceContainment
// trusts (evidenceCeilingFor), never independently derived. It is stateless
// beyond that one immutable field and safe for concurrent use.
type permissionReviewEvidenceObservation struct {
	ceiling string
}

// newPermissionReviewEvidenceObservation returns Carbon's observation
// verifier bound to ceiling (the session's selected access profile name).
// An empty ceiling is accepted structurally but VerifyEvidenceObservations
// then fails closed for every call with at least one requirement, since an
// unconfigured expected ceiling can never legitimately match a live
// review's SecurityCeiling.
func newPermissionReviewEvidenceObservation(ceiling string) permissionReviewEvidenceObservation {
	return permissionReviewEvidenceObservation{ceiling: ceiling}
}

// Sentinel observation errors. Unexported and secret-free, matching this
// package's existing containment sentinels' discipline.
var (
	errEvidenceObservationAmbiguous = errors.New("carbon: observation requirement target could not be unambiguously resolved within the review workspace")
	errEvidenceObservationEscape    = errors.New("carbon: observation requirement target resolves outside the review workspace")
	errEvidenceObservationCeiling   = errors.New("carbon: observation review security ceiling does not match this session")
	errEvidenceObservationMismatch  = errors.New("carbon: observation token no longer matches the target's current state")
)

// VerifyEvidenceObservations independently re-derives, from the LIVE
// filesystem, every requirement's current token via
// filesystemObservationFingerprint and compares it to requirement.Token.
// ANY mismatch, ANY target that cannot be unambiguously resolved (escapes
// policy.ReadRoot, or hits an I/O error during the recheck), or a
// SecurityCeiling that does not match the ONE ceiling this verifier trusts
// returns a non-nil error — fail closed, exactly
// gate.EvidenceObservationVerifier's own doc comment requires. It never
// panics: any unexpected internal failure is recovered and reported as a
// plain error, mirroring permissionReviewEvidenceContainment.VerifyEvidenceContainment's
// own safety net.
func (v permissionReviewEvidenceObservation) VerifyEvidenceObservations(ctx context.Context, policy gate.EvidenceContainmentPolicy, requirements []gate.ObservationRequirement) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = errEvidenceObservationAmbiguous
		}
	}()
	if ctx == nil {
		return errEvidenceObservationAmbiguous
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	// Equality against the ONE ceiling this session trusts — never a
	// string-ordering/prefix heuristic, matching permissionReviewEvidenceContainment's
	// own reasoning.
	if v.ceiling == "" || policy.SecurityCeiling != v.ceiling {
		return errEvidenceObservationCeiling
	}

	canonicalRoot, err := canonicalizeEvidenceReadRoot(policy.ReadRoot)
	if err != nil {
		return err
	}

	for _, requirement := range requirements {
		if err := verifyObservationRequirement(canonicalRoot, requirement); err != nil {
			return err
		}
	}
	return nil
}

var _ gate.EvidenceObservationVerifier = permissionReviewEvidenceObservation{}

// verifyObservationRequirement resolves ONE requirement's Target against
// canonicalRoot (already symlink-resolved), recomputes its current
// fingerprint, and fails closed unless the recomputed digest matches
// requirement.Token exactly.
func verifyObservationRequirement(canonicalRoot string, requirement gate.ObservationRequirement) error {
	if !requirement.Valid() {
		return errEvidenceObservationAmbiguous
	}
	rel, err := observationRelativeTarget(canonicalRoot, requirement.Target)
	if err != nil {
		return err
	}
	token, err := filesystemObservationFingerprint(canonicalRoot, rel)
	if err != nil {
		return errEvidenceObservationAmbiguous
	}
	if token != requirement.Token {
		return errEvidenceObservationMismatch
	}
	return nil
}

// observationRelativeTarget derives a canonicalRoot-relative path from an
// ObservationRequirement's Target field. Every Target this file's own
// filesystemObservationFingerprint-derived tokens ever pair with is, by
// classifiers' own contract, an absolute path constructed as
// filepath.Join(root, rel) against the SAME canonical root the evidence
// tool used — so a lexical filepath.Rel comparison against canonicalRoot is
// exactly the right check here, deliberately WITHOUT resolving Target's own
// symlinks first (unlike permissionReviewEvidenceContainment's containment
// check, which resolves through symlinks precisely to determine whether an
// escaping symlink is safe to READ). Resolving symlinks here would defeat
// the whole point: filesystemObservationFingerprint below is exactly what
// safely detects a symlink swap, via os.OpenRoot + Lstat's own
// root-sandboxed containment (a directory component that itself escapes
// canonicalRoot via a symlink makes r.Lstat fail rather than silently
// traverse outside it, so this lexical check does not weaken containment —
// it just avoids resolving away the very swap this recheck exists to
// catch).
func observationRelativeTarget(canonicalRoot, target string) (string, error) {
	if !filepath.IsAbs(target) {
		return "", errEvidenceObservationAmbiguous
	}
	cleanTarget := filepath.Clean(target)
	rel, err := filepath.Rel(canonicalRoot, cleanTarget)
	if err != nil {
		return "", errEvidenceObservationAmbiguous
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", errEvidenceObservationEscape
	}
	return rel, nil
}

// ---- independently reimplemented token formula ----------------------------
// (see this file's top-of-file doc comment for why this is a deliberate,
// from-scratch reimplementation rather than a shared import.)

// filesystemObservationFingerprint computes the SAME "fsv1" token
// classifiers' evidence_filesystem_stat/evidence_filesystem_read tools
// record, for one root-relative path, resolved fresh against the live
// filesystem right now.
//
// For a target that does not exist: "fsv1", "absent".
//
// For a target that exists and is NOT itself a symlink: "fsv1", "present",
// then its lstat 4-tuple (entryKind, decimal size, decimal raw os.FileMode
// bits, decimal UnixNano mtime).
//
// For a target that exists and IS itself a symlink: the same "fsv1",
// "present" lstat 4-tuple, followed by either "link_unreadable" (readlink
// failed) or "link_target", <raw readlink text>, and one of
// "link_resolved" + the resolved target's own 4-tuple, "link_target_absent",
// or "link_target_unresolvable".
//
// Every field is combined via observationDigest's netstring encoding before
// hashing — see that function's own doc comment for why.
func filesystemObservationFingerprint(root, rel string) (string, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return "", err
	}
	defer r.Close()

	info, err := r.Lstat(rel)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return observationDigest("fsv1", "absent"), nil
	case err != nil:
		return "", err
	}

	fields := []string{"fsv1", "present"}
	fields = append(fields, fileInfoFingerprintFields(info)...)

	if info.Mode()&fs.ModeSymlink == 0 {
		return observationDigest(fields...), nil
	}

	target, linkErr := r.Readlink(rel)
	if linkErr != nil {
		fields = append(fields, "link_unreadable")
		return observationDigest(fields...), nil
	}
	fields = append(fields, "link_target", target)

	resolved, statErr := r.Stat(rel)
	switch {
	case statErr == nil:
		fields = append(fields, "link_resolved")
		fields = append(fields, fileInfoFingerprintFields(resolved)...)
	case errors.Is(statErr, fs.ErrNotExist):
		fields = append(fields, "link_target_absent")
	default:
		fields = append(fields, "link_target_unresolvable")
	}
	return observationDigest(fields...), nil
}

// fileInfoFingerprintFields is the exact 4-tuple every lstat/stat result
// contributes to the fingerprint: entryKind, decimal byte size, the RAW
// decimal os.FileMode bit pattern (type bits and permission bits together —
// a file-to-symlink type change always changes this), and the modification
// time as decimal UnixNano (full available precision).
func fileInfoFingerprintFields(info fs.FileInfo) []string {
	return []string{
		entryKind(info),
		strconv.FormatInt(info.Size(), 10),
		strconv.FormatUint(uint64(info.Mode()), 10),
		strconv.FormatInt(info.ModTime().UTC().UnixNano(), 10),
	}
}

// entryKind classifies info into the same four coarse buckets classifiers'
// own entryKind reports, with IDENTICAL string values: "symlink", "dir",
// "file", or "other". These exact literals are part of the wire format —
// changing any of them would silently break token compatibility with
// classifiers' own recorded tokens.
func entryKind(info fs.FileInfo) string {
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		return "symlink"
	case info.IsDir():
		return "dir"
	case info.Mode().IsRegular():
		return "file"
	default:
		return "other"
	}
}

// observationDigest deterministically and unambiguously combines fields
// into one SHA-256 hex digest. Each field is netstring-encoded
// (<decimal-byte-length>:<raw-bytes>, e.g. "3:abc") rather than joined with
// a fixed separator byte, matching classifiers' own scheme exactly: a
// separator byte could appear INSIDE a field this file does not fully
// control (a symlink target's raw text may contain almost any byte short of
// NUL and '/'), which would let two different field sequences serialize to
// the identical byte string and collide. A length prefix makes that
// impossible regardless of a field's own content.
func observationDigest(fields ...string) string {
	var encoded []byte
	for _, f := range fields {
		encoded = append(encoded, []byte(strconv.Itoa(len(f))+":")...)
		encoded = append(encoded, f...)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}
