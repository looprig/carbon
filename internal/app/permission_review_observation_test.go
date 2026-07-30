package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/looprig/harness/pkg/gate"
)

// permission_review_observation_test.go unit-tests permissionReviewEvidenceObservation,
// CodeRig's gate.EvidenceObservationVerifier (design §13.4, TOCTOU —
// Addendum 4): the fingerprint formula's fidelity against an independently
// hand-written reference, and VerifyEvidenceObservations' fail-closed
// recheck behavior in isolation (no live session, no classifier, no
// Hustle). The genuine end-to-end proof that a symlink swap between
// evidence-gathering and auto-approval actually blocks a real gate lives in
// permission_review_integration_test.go
// (TestPermissionReviewObservationSymlinkSwapBlocksAutoApprovalEndToEnd).

// ---- cross-check: independent formula reproduction ------------------------

// referenceFilesystemObservationToken hand-reimplements
// github.com/looprig/classifiers/internal/evidence/observation.go's
// filesystemObservationFingerprint formula DIRECTLY in this test — it does
// NOT import classifiers' package (that package is `internal` to a sibling
// module and unimportable from here regardless) and does NOT call
// CodeRig's own filesystemObservationFingerprint. It is a second,
// independently typed implementation of the exact same spec, so
// TestFingerprintMatchesIndependentReferenceFormula is a genuine
// cross-check of byte-for-byte wire compatibility, not a tautology against
// CodeRig's own implementation code.
func referenceFilesystemObservationToken(t *testing.T, root, rel string) string {
	t.Helper()
	r, err := os.OpenRoot(root)
	if err != nil {
		t.Fatalf("OpenRoot(%q): %v", root, err)
	}
	defer r.Close()

	info, err := r.Lstat(rel)
	if err != nil {
		if os.IsNotExist(err) {
			return referenceObservationDigest("fsv1", "absent")
		}
		t.Fatalf("Lstat(%q): %v", rel, err)
	}

	fields := []string{"fsv1", "present"}
	fields = append(fields, referenceFingerprintFields(info)...)

	if info.Mode()&os.ModeSymlink == 0 {
		return referenceObservationDigest(fields...)
	}

	target, err := r.Readlink(rel)
	if err != nil {
		fields = append(fields, "link_unreadable")
		return referenceObservationDigest(fields...)
	}
	fields = append(fields, "link_target", target)

	resolved, err := r.Stat(rel)
	switch {
	case err == nil:
		fields = append(fields, "link_resolved")
		fields = append(fields, referenceFingerprintFields(resolved)...)
	case os.IsNotExist(err):
		fields = append(fields, "link_target_absent")
	default:
		fields = append(fields, "link_target_unresolvable")
	}
	return referenceObservationDigest(fields...)
}

// referenceFingerprintFields independently reproduces the 4-tuple
// (entryKind, decimal size, decimal raw mode bits, decimal UnixNano mtime)
// classifiers' fileInfoFingerprintFields computes.
func referenceFingerprintFields(info os.FileInfo) []string {
	kind := "other"
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		kind = "symlink"
	case info.IsDir():
		kind = "dir"
	case info.Mode().IsRegular():
		kind = "file"
	}
	return []string{
		kind,
		strconv.FormatInt(info.Size(), 10),
		strconv.FormatUint(uint64(info.Mode()), 10),
		strconv.FormatInt(info.ModTime().UTC().UnixNano(), 10),
	}
}

// referenceObservationDigest independently reproduces classifiers'
// observationDigest netstring-encoding scheme
// (<decimal-byte-length>:<raw-bytes>, concatenated, then SHA-256 hex).
func referenceObservationDigest(fields ...string) string {
	var encoded []byte
	for _, f := range fields {
		encoded = append(encoded, []byte(fmt.Sprintf("%d:", len(f)))...)
		encoded = append(encoded, f...)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// TestFingerprintMatchesIndependentReferenceFormula proves CodeRig's own
// filesystemObservationFingerprint produces the IDENTICAL hex digest the
// independently hand-written reference formula above does, across a
// present file, a nested present file, a symlink, and an absent target —
// the byte-for-byte wire compatibility the whole recheck mechanism depends
// on.
func TestFingerprintMatchesIndependentReferenceFormula(t *testing.T) {
	t.Parallel()
	root := canonicalTempDir(t)
	writeEvidenceFile(t, root, "a.txt", "hello")
	writeEvidenceFile(t, root, "dir/nested.txt", "nested")
	if err := os.Symlink(filepath.Join(root, "a.txt"), filepath.Join(root, "link.txt")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	for _, rel := range []string{"a.txt", "dir/nested.txt", "link.txt", "missing.txt", "dir", "."} {
		rel := rel
		t.Run(rel, func(t *testing.T) {
			t.Parallel()
			got, err := filesystemObservationFingerprint(root, rel)
			if err != nil {
				t.Fatalf("filesystemObservationFingerprint(%q, %q) error = %v", root, rel, err)
			}
			want := referenceFilesystemObservationToken(t, root, rel)
			if got != want {
				t.Errorf("filesystemObservationFingerprint(%q, %q) = %q, want %q (independent reference formula)", root, rel, got, want)
			}
		})
	}
}

// TestFingerprintChangesWhenTargetSwappedForSymlink mirrors classifiers'
// own TestPathStatObservationTokenChangesWhenTargetSwappedForSymlink at the
// fingerprint-function level (before VerifyEvidenceObservations is even
// involved): the classic TOCTOU swap must change the token.
func TestFingerprintChangesWhenTargetSwappedForSymlink(t *testing.T) {
	t.Parallel()
	root := canonicalTempDir(t)
	writeEvidenceFile(t, root, "a.txt", "hello")

	tokenBefore, err := filesystemObservationFingerprint(root, "a.txt")
	if err != nil {
		t.Fatalf("filesystemObservationFingerprint (before): %v", err)
	}

	if err := os.Remove(filepath.Join(root, "a.txt")); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	writeEvidenceFile(t, root, "elsewhere.txt", "different contents entirely")
	if err := os.Symlink(filepath.Join(root, "elsewhere.txt"), filepath.Join(root, "a.txt")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	tokenAfter, err := filesystemObservationFingerprint(root, "a.txt")
	if err != nil {
		t.Fatalf("filesystemObservationFingerprint (after): %v", err)
	}
	if tokenBefore == tokenAfter {
		t.Errorf("token did not change after the real file was swapped for a symlink: both %q", tokenBefore)
	}
}

// ---- permissionReviewEvidenceObservation: VerifyEvidenceObservations -----

func mustObservationToken(t *testing.T, root, rel string) string {
	t.Helper()
	token, err := filesystemObservationFingerprint(root, rel)
	if err != nil {
		t.Fatalf("filesystemObservationFingerprint(%q, %q) error = %v", root, rel, err)
	}
	return token
}

func TestPermissionReviewEvidenceObservationImplementsInterface(t *testing.T) {
	var _ gate.EvidenceObservationVerifier = permissionReviewEvidenceObservation{}
}

func TestPermissionReviewEvidenceObservationSucceedsWhenUnchanged(t *testing.T) {
	t.Parallel()
	root := canonicalTempDir(t)
	target := writeEvidenceFile(t, root, "a.txt", "hello")
	token := mustObservationToken(t, root, "a.txt")

	verifier := newPermissionReviewEvidenceObservation("trusted")
	policy := gate.EvidenceContainmentPolicy{ReadRoot: root, SecurityCeiling: "trusted"}
	requirements := []gate.ObservationRequirement{{Target: target, Token: token}}

	if err := verifier.VerifyEvidenceObservations(context.Background(), policy, requirements); err != nil {
		t.Fatalf("VerifyEvidenceObservations() error = %v, want nil when nothing changed since the recorded token", err)
	}
}

func TestPermissionReviewEvidenceObservationSucceedsWithNoRequirements(t *testing.T) {
	t.Parallel()
	root := canonicalTempDir(t)
	verifier := newPermissionReviewEvidenceObservation("trusted")
	policy := gate.EvidenceContainmentPolicy{ReadRoot: root, SecurityCeiling: "trusted"}

	if err := verifier.VerifyEvidenceObservations(context.Background(), policy, nil); err != nil {
		t.Fatalf("VerifyEvidenceObservations() error = %v, want nil for an empty requirement set", err)
	}
}

func TestPermissionReviewEvidenceObservationFailsWhenMtimeChanged(t *testing.T) {
	t.Parallel()
	root := canonicalTempDir(t)
	target := writeEvidenceFile(t, root, "a.txt", "hello")
	token := mustObservationToken(t, root, "a.txt")

	future := time.Now().Add(2 * time.Hour)
	if err := os.Chtimes(target, future, future); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	verifier := newPermissionReviewEvidenceObservation("trusted")
	policy := gate.EvidenceContainmentPolicy{ReadRoot: root, SecurityCeiling: "trusted"}
	requirements := []gate.ObservationRequirement{{Target: target, Token: token}}

	if err := verifier.VerifyEvidenceObservations(context.Background(), policy, requirements); err == nil {
		t.Fatal("VerifyEvidenceObservations() error = nil, want rejection after the target's mtime changed")
	}
}

// TestPermissionReviewEvidenceObservationFailsWhenTargetSwappedForSymlink is
// the TOCTOU attack scenario itself: the real file recorded at
// evidence-gathering time is swapped for a symlink to different content
// before the recheck runs.
func TestPermissionReviewEvidenceObservationFailsWhenTargetSwappedForSymlink(t *testing.T) {
	t.Parallel()
	root := canonicalTempDir(t)
	target := writeEvidenceFile(t, root, "a.txt", "hello")
	token := mustObservationToken(t, root, "a.txt")

	if err := os.Remove(target); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	writeEvidenceFile(t, root, "elsewhere.txt", "different contents entirely")
	if err := os.Symlink(filepath.Join(root, "elsewhere.txt"), target); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	verifier := newPermissionReviewEvidenceObservation("trusted")
	policy := gate.EvidenceContainmentPolicy{ReadRoot: root, SecurityCeiling: "trusted"}
	requirements := []gate.ObservationRequirement{{Target: target, Token: token}}

	if err := verifier.VerifyEvidenceObservations(context.Background(), policy, requirements); err == nil {
		t.Fatal("VerifyEvidenceObservations() error = nil, want rejection: the target was swapped for a symlink (TOCTOU)")
	}
}

func TestPermissionReviewEvidenceObservationFailsClosedOutsideReadRoot(t *testing.T) {
	t.Parallel()
	root := canonicalTempDir(t)
	outside := canonicalTempDir(t)
	target := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(target, []byte("nope"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	token := mustObservationToken(t, outside, "secret.txt")

	verifier := newPermissionReviewEvidenceObservation("trusted")
	policy := gate.EvidenceContainmentPolicy{ReadRoot: root, SecurityCeiling: "trusted"}
	requirements := []gate.ObservationRequirement{{Target: target, Token: token}}

	if err := verifier.VerifyEvidenceObservations(context.Background(), policy, requirements); err == nil {
		t.Fatal("VerifyEvidenceObservations() error = nil, want rejection: target lies outside ReadRoot (containment violation)")
	}
}

func TestPermissionReviewEvidenceObservationFailsWhenAbsentBecomesPresent(t *testing.T) {
	t.Parallel()
	root := canonicalTempDir(t)
	target := filepath.Join(root, "later.txt")
	token := mustObservationToken(t, root, "later.txt") // records the "absent" token

	writeEvidenceFile(t, root, "later.txt", "now it exists")

	verifier := newPermissionReviewEvidenceObservation("trusted")
	policy := gate.EvidenceContainmentPolicy{ReadRoot: root, SecurityCeiling: "trusted"}
	requirements := []gate.ObservationRequirement{{Target: target, Token: token}}

	if err := verifier.VerifyEvidenceObservations(context.Background(), policy, requirements); err == nil {
		t.Fatal("VerifyEvidenceObservations() error = nil, want rejection: recorded absent, now present")
	}
}

func TestPermissionReviewEvidenceObservationFailsWhenPresentBecomesAbsent(t *testing.T) {
	t.Parallel()
	root := canonicalTempDir(t)
	target := writeEvidenceFile(t, root, "gone.txt", "here for now")
	token := mustObservationToken(t, root, "gone.txt")

	if err := os.Remove(target); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	verifier := newPermissionReviewEvidenceObservation("trusted")
	policy := gate.EvidenceContainmentPolicy{ReadRoot: root, SecurityCeiling: "trusted"}
	requirements := []gate.ObservationRequirement{{Target: target, Token: token}}

	if err := verifier.VerifyEvidenceObservations(context.Background(), policy, requirements); err == nil {
		t.Fatal("VerifyEvidenceObservations() error = nil, want rejection: recorded present, now absent")
	}
}

func TestPermissionReviewEvidenceObservationRejectsCeilingMismatch(t *testing.T) {
	t.Parallel()
	root := canonicalTempDir(t)
	target := writeEvidenceFile(t, root, "a.txt", "hello")
	token := mustObservationToken(t, root, "a.txt")

	verifier := newPermissionReviewEvidenceObservation("trusted")
	policy := gate.EvidenceContainmentPolicy{ReadRoot: root, SecurityCeiling: "unconfined"}
	requirements := []gate.ObservationRequirement{{Target: target, Token: token}}

	if err := verifier.VerifyEvidenceObservations(context.Background(), policy, requirements); err == nil {
		t.Fatal("VerifyEvidenceObservations() error = nil, want ceiling-mismatch rejection")
	}
}

func TestPermissionReviewEvidenceObservationRejectsUnconfiguredCeiling(t *testing.T) {
	t.Parallel()
	root := canonicalTempDir(t)
	target := writeEvidenceFile(t, root, "a.txt", "hello")
	token := mustObservationToken(t, root, "a.txt")

	verifier := newPermissionReviewEvidenceObservation("") // never legitimately constructed this way in production
	policy := gate.EvidenceContainmentPolicy{ReadRoot: root, SecurityCeiling: ""}
	requirements := []gate.ObservationRequirement{{Target: target, Token: token}}

	if err := verifier.VerifyEvidenceObservations(context.Background(), policy, requirements); err == nil {
		t.Fatal("VerifyEvidenceObservations() error = nil, want fail-closed rejection of an unconfigured ceiling")
	}
}

func TestPermissionReviewEvidenceObservationRejectsTokenMismatch(t *testing.T) {
	t.Parallel()
	root := canonicalTempDir(t)
	target := writeEvidenceFile(t, root, "a.txt", "hello")

	verifier := newPermissionReviewEvidenceObservation("trusted")
	policy := gate.EvidenceContainmentPolicy{ReadRoot: root, SecurityCeiling: "trusted"}
	requirements := []gate.ObservationRequirement{{Target: target, Token: "not-a-real-token"}}

	if err := verifier.VerifyEvidenceObservations(context.Background(), policy, requirements); err == nil {
		t.Fatal("VerifyEvidenceObservations() error = nil, want rejection of a fabricated/mismatched token")
	}
}

func TestPermissionReviewEvidenceObservationMultipleRequirementsAllMustPass(t *testing.T) {
	t.Parallel()
	root := canonicalTempDir(t)
	okTarget := writeEvidenceFile(t, root, "ok.txt", "fine")
	okToken := mustObservationToken(t, root, "ok.txt")
	badTarget := writeEvidenceFile(t, root, "bad.txt", "will change")
	badToken := mustObservationToken(t, root, "bad.txt")

	future := time.Now().Add(2 * time.Hour)
	if err := os.Chtimes(badTarget, future, future); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	verifier := newPermissionReviewEvidenceObservation("trusted")
	policy := gate.EvidenceContainmentPolicy{ReadRoot: root, SecurityCeiling: "trusted"}
	requirements := []gate.ObservationRequirement{
		{Target: okTarget, Token: okToken},
		{Target: badTarget, Token: badToken},
	}

	if err := verifier.VerifyEvidenceObservations(context.Background(), policy, requirements); err == nil {
		t.Fatal("VerifyEvidenceObservations() error = nil, want rejection when ANY requirement fails its recheck")
	}
}

func TestPermissionReviewEvidenceObservationNeverPanics(t *testing.T) {
	t.Parallel()
	root := canonicalTempDir(t)
	verifier := newPermissionReviewEvidenceObservation("trusted")

	adversarial := [][]gate.ObservationRequirement{
		nil,
		{{}},
		{{Target: "relative/path", Token: "x"}},
		{{Target: string(make([]byte, 8192)), Token: "x"}},
	}
	for i, reqs := range adversarial {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("case %d: VerifyEvidenceObservations panicked: %v", i, r)
				}
			}()
			policy := gate.EvidenceContainmentPolicy{ReadRoot: root, SecurityCeiling: "trusted"}
			_ = verifier.VerifyEvidenceObservations(context.Background(), policy, reqs)
		}()
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	target := writeEvidenceFile(t, root, "a.txt", "hello")
	token := mustObservationToken(t, root, "a.txt")
	policy := gate.EvidenceContainmentPolicy{ReadRoot: root, SecurityCeiling: "trusted"}
	requirements := []gate.ObservationRequirement{{Target: target, Token: token}}
	if err := verifier.VerifyEvidenceObservations(ctx, policy, requirements); err == nil {
		t.Fatal("VerifyEvidenceObservations() with a cancelled context error = nil, want a fail-closed rejection")
	}
}
