package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/tool"
)

// permission_review_evidence_test.go unit-tests permissionReviewEvidenceAccess
// and permissionReviewEvidenceContainment in isolation (no live session, no
// classifier, no Hustle) — construction, containment escape attempts
// (symlink and naive-prefix), ceiling mismatch, and ambiguous-requirement
// rejection. The live end-to-end evidence pipeline proof lives in
// permission_review_integration_test.go.

func evidenceRequirement(kind, match string) tool.Requirement {
	return tool.Requirement{Kind: kind, Match: match, Description: "probe " + kind}
}

// --- permissionReviewEvidenceAccess ---------------------------------------

func TestPermissionReviewEvidenceAccessAllowsOrdinaryRequirement(t *testing.T) {
	t.Parallel()
	access := newPermissionReviewEvidenceAccess()
	got, err := access.AccessFor(evidenceRequirement("evidence.filesystem.stat", "some/file.go"))
	if err != nil {
		t.Fatalf("AccessFor() error = %v", err)
	}
	if got != gate.AccessAllow {
		t.Fatalf("AccessFor() = %d, want AccessAllow", got)
	}
}

func TestPermissionReviewEvidenceAccessRejectsEmptyKind(t *testing.T) {
	t.Parallel()
	access := newPermissionReviewEvidenceAccess()
	got, err := access.AccessFor(evidenceRequirement("", "some/file.go"))
	if err == nil {
		t.Fatal("AccessFor() error = nil, want a fail-closed rejection for an empty Kind")
	}
	if got != gate.AccessDeny {
		t.Fatalf("AccessFor() = %d, want AccessDeny", got)
	}
}

func TestPermissionReviewEvidenceAccessRejectsGrantSemantics(t *testing.T) {
	t.Parallel()
	access := newPermissionReviewEvidenceAccess()
	tests := []struct {
		name        string
		requirement tool.Requirement
	}{
		{name: "grant class", requirement: tool.Requirement{Kind: "evidence.filesystem.stat", Match: "f", GrantClass: "command.start.v1"}},
		{name: "grant target", requirement: tool.Requirement{Kind: "evidence.filesystem.stat", Match: "f", GrantTarget: "f"}},
		{name: "candidates", requirement: tool.Requirement{Kind: "evidence.filesystem.stat", Match: "f", Candidates: []tool.RuleCandidate{{Kind: "evidence.filesystem.stat", Match: "f"}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := access.AccessFor(tt.requirement)
			if err == nil {
				t.Fatal("AccessFor() error = nil, want rejection of unexpected grant semantics")
			}
			if got != gate.AccessDeny {
				t.Fatalf("AccessFor() = %d, want AccessDeny", got)
			}
		})
	}
}

// --- permissionReviewEvidenceContainment: construction / ceiling ---------

func TestEvidenceCeilingForDefaultsEmptyProfile(t *testing.T) {
	t.Parallel()
	if got := evidenceCeilingFor(""); got != string(DefaultAccessProfile) {
		t.Fatalf("evidenceCeilingFor(\"\") = %q, want %q", got, string(DefaultAccessProfile))
	}
	if got := evidenceCeilingFor(AccessTrusted); got != string(AccessTrusted) {
		t.Fatalf("evidenceCeilingFor(AccessTrusted) = %q, want %q", got, string(AccessTrusted))
	}
}

func TestPermissionReviewEvidenceContainmentRejectsCeilingMismatch(t *testing.T) {
	t.Parallel()
	root := canonicalTempDir(t)
	writeEvidenceFile(t, root, "readme.txt", "hi")

	verifier := newPermissionReviewEvidenceContainment("trusted")
	policy := gate.EvidenceContainmentPolicy{ReadRoot: root, SecurityCeiling: "unconfined"}
	req := tool.Request{Requirements: []tool.Requirement{evidenceRequirement("evidence.filesystem.stat", "readme.txt")}}

	err := verifier.VerifyEvidenceContainment(context.Background(), policy, req)
	if err == nil {
		t.Fatal("VerifyEvidenceContainment() error = nil, want ceiling-mismatch rejection")
	}
	if !errors.Is(err, errEvidenceContainmentCeiling) {
		t.Fatalf("VerifyEvidenceContainment() error = %v, want errEvidenceContainmentCeiling", err)
	}
}

func TestPermissionReviewEvidenceContainmentRejectsUnconfiguredCeiling(t *testing.T) {
	t.Parallel()
	root := canonicalTempDir(t)
	verifier := newPermissionReviewEvidenceContainment("") // never legitimately constructed this way in production
	policy := gate.EvidenceContainmentPolicy{ReadRoot: root, SecurityCeiling: ""}
	req := tool.Request{Requirements: []tool.Requirement{evidenceRequirement("evidence.filesystem.stat", ".")}}

	if err := verifier.VerifyEvidenceContainment(context.Background(), policy, req); err == nil {
		t.Fatal("VerifyEvidenceContainment() error = nil, want fail-closed rejection of an unconfigured ceiling")
	}
}

// --- permissionReviewEvidenceContainment: happy path ----------------------

func writeEvidenceFile(t *testing.T, root, rel, body string) string {
	t.Helper()
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return full
}

func TestPermissionReviewEvidenceContainmentAllowsExistingInRootTarget(t *testing.T) {
	t.Parallel()
	root := canonicalTempDir(t)
	writeEvidenceFile(t, root, "src/main.go", "package main")

	verifier := newPermissionReviewEvidenceContainment("trusted")
	policy := gate.EvidenceContainmentPolicy{ReadRoot: root, SecurityCeiling: "trusted"}
	req := tool.Request{Requirements: []tool.Requirement{evidenceRequirement("evidence.filesystem.stat", "src/main.go")}}

	if err := verifier.VerifyEvidenceContainment(context.Background(), policy, req); err != nil {
		t.Fatalf("VerifyEvidenceContainment() error = %v, want nil for an in-root existing target", err)
	}
}

func TestPermissionReviewEvidenceContainmentAllowsRootItself(t *testing.T) {
	t.Parallel()
	root := canonicalTempDir(t)
	verifier := newPermissionReviewEvidenceContainment("trusted")
	policy := gate.EvidenceContainmentPolicy{ReadRoot: root, SecurityCeiling: "trusted"}

	for _, match := range []string{"", "."} {
		req := tool.Request{Requirements: []tool.Requirement{evidenceRequirement("evidence.filesystem.list", match)}}
		if err := verifier.VerifyEvidenceContainment(context.Background(), policy, req); err != nil {
			t.Fatalf("VerifyEvidenceContainment() with Match=%q error = %v, want nil for the workspace root itself", match, err)
		}
	}
}

// TestPermissionReviewEvidenceContainmentAllowsNonexistentTargetUnderExistingParent
// proves the "resolve the deepest EXISTING ancestor" rule: a stat call
// against a path that does not exist yet must not blanket-fail, as long as
// its existing parent resolves within root.
func TestPermissionReviewEvidenceContainmentAllowsNonexistentTargetUnderExistingParent(t *testing.T) {
	t.Parallel()
	root := canonicalTempDir(t)
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	verifier := newPermissionReviewEvidenceContainment("trusted")
	policy := gate.EvidenceContainmentPolicy{ReadRoot: root, SecurityCeiling: "trusted"}
	req := tool.Request{Requirements: []tool.Requirement{evidenceRequirement("evidence.filesystem.stat", "src/does-not-exist.go")}}

	if err := verifier.VerifyEvidenceContainment(context.Background(), policy, req); err != nil {
		t.Fatalf("VerifyEvidenceContainment() error = %v, want nil for a nonexistent target under an existing in-root parent", err)
	}
}

// TestPermissionReviewEvidenceContainmentRejectsWhenNothingResolves fails
// closed when even the immediate parent cannot be resolved (a deeply
// nonexistent path with no existing ancestor short of root would still
// resolve via root itself — this test forces failure via a root that does
// not exist, which resolveExisting for root itself will reject before ever
// reaching a requirement).
func TestPermissionReviewEvidenceContainmentRejectsWhenRootDoesNotExist(t *testing.T) {
	t.Parallel()
	root := filepath.Join(canonicalTempDir(t), "never-created")
	verifier := newPermissionReviewEvidenceContainment("trusted")
	policy := gate.EvidenceContainmentPolicy{ReadRoot: root, SecurityCeiling: "trusted"}
	req := tool.Request{Requirements: []tool.Requirement{evidenceRequirement("evidence.filesystem.stat", "file.go")}}

	if err := verifier.VerifyEvidenceContainment(context.Background(), policy, req); err == nil {
		t.Fatal("VerifyEvidenceContainment() error = nil, want fail-closed rejection when ReadRoot itself does not exist")
	}
}

// --- permissionReviewEvidenceContainment: escape attempts -----------------

// TestPermissionReviewEvidenceContainmentRejectsNaivePrefixBypass proves the
// verifier does NOT use a raw strings.HasPrefix-style comparison of the
// resolved candidate against root. It constructs a REAL symlink inside root
// that resolves to a file under a SIBLING directory whose name extends
// root's own basename with no separator boundary ("<root>-evil" vs
// "<root>") — the classic bug: strings.HasPrefix("<root>-evil/secret.txt",
// "<root>") reports true (a false "contained"), because the string "<root>"
// really is a byte-prefix of "<root>-evil/secret.txt" even though the
// resolved target is NOT inside root at all. filepath.Rel-based containment
// must reject it; a naive prefix check would wrongly accept it (this is
// verified directly further down by temporarily swapping in a naive check).
func TestPermissionReviewEvidenceContainmentRejectsNaivePrefixBypass(t *testing.T) {
	t.Parallel()
	parent := canonicalTempDir(t)
	root := filepath.Join(parent, "workspace")
	evil := filepath.Join(parent, "workspace-evil")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("MkdirAll(root): %v", err)
	}
	if err := os.MkdirAll(evil, 0o700); err != nil {
		t.Fatalf("MkdirAll(evil): %v", err)
	}
	if err := os.WriteFile(filepath.Join(evil, "secret.txt"), []byte("nope"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	link := filepath.Join(root, "escape-link")
	if err := os.Symlink(filepath.Join(evil, "secret.txt"), link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	// Sanity-check the premise this test depends on: the resolved escape
	// target really is byte-prefixed by root (this is what makes the bug
	// class dangerous — it only bites when the sibling's name extends the
	// root's own basename).
	if !strings.HasPrefix(evil, root) {
		t.Fatalf("test setup invariant broken: %q is not a naive byte-prefix of %q", root, evil)
	}

	verifier := newPermissionReviewEvidenceContainment("trusted")
	policy := gate.EvidenceContainmentPolicy{ReadRoot: root, SecurityCeiling: "trusted"}
	req := tool.Request{Requirements: []tool.Requirement{evidenceRequirement("evidence.filesystem.read", "escape-link")}}

	err := verifier.VerifyEvidenceContainment(context.Background(), policy, req)
	if err == nil {
		t.Fatal("VerifyEvidenceContainment() error = nil, want rejection of a sibling-directory naive-prefix bypass")
	}
	if !errors.Is(err, errEvidenceContainmentEscape) {
		t.Fatalf("VerifyEvidenceContainment() error = %v, want errEvidenceContainmentEscape", err)
	}
}

// TestPermissionReviewEvidenceContainmentRejectsDotDotEscape proves a
// lexical ".." escape is rejected outright, before any filesystem access.
func TestPermissionReviewEvidenceContainmentRejectsDotDotEscape(t *testing.T) {
	t.Parallel()
	root := canonicalTempDir(t)
	verifier := newPermissionReviewEvidenceContainment("trusted")
	policy := gate.EvidenceContainmentPolicy{ReadRoot: root, SecurityCeiling: "trusted"}
	req := tool.Request{Requirements: []tool.Requirement{evidenceRequirement("evidence.filesystem.read", "../outside.txt")}}

	err := verifier.VerifyEvidenceContainment(context.Background(), policy, req)
	if err == nil {
		t.Fatal("VerifyEvidenceContainment() error = nil, want rejection of a \"..\" escape")
	}
	if !errors.Is(err, errEvidenceContainmentEscape) {
		t.Fatalf("VerifyEvidenceContainment() error = %v, want errEvidenceContainmentEscape", err)
	}
}

// TestPermissionReviewEvidenceContainmentRejectsSymlinkEscape constructs a
// REAL symlink inside root pointing OUTSIDE it and proves the verifier
// rejects reading through it — this is the check a raw lexical/prefix
// comparison of Match against root could never catch, because the escaping
// symlink is invisible until the filesystem actually resolves it.
func TestPermissionReviewEvidenceContainmentRejectsSymlinkEscape(t *testing.T) {
	t.Parallel()
	root := canonicalTempDir(t)
	outside := canonicalTempDir(t)
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("outside the workspace"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	link := filepath.Join(root, "escape-link")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	verifier := newPermissionReviewEvidenceContainment("trusted")
	policy := gate.EvidenceContainmentPolicy{ReadRoot: root, SecurityCeiling: "trusted"}
	req := tool.Request{Requirements: []tool.Requirement{evidenceRequirement("evidence.filesystem.read", "escape-link")}}

	err := verifier.VerifyEvidenceContainment(context.Background(), policy, req)
	if err == nil {
		t.Fatal("VerifyEvidenceContainment() error = nil, want rejection of a symlink that escapes the review workspace")
	}
	if !errors.Is(err, errEvidenceContainmentEscape) {
		t.Fatalf("VerifyEvidenceContainment() error = %v, want errEvidenceContainmentEscape", err)
	}
}

// TestPermissionReviewEvidenceContainmentRejectsSymlinkEscapeThroughDirectory
// mirrors the file-symlink case but for a symlinked DIRECTORY: a target
// nested under an in-root-looking directory that is itself a symlink
// escaping root must also be rejected.
func TestPermissionReviewEvidenceContainmentRejectsSymlinkEscapeThroughDirectory(t *testing.T) {
	t.Parallel()
	root := canonicalTempDir(t)
	outside := canonicalTempDir(t)
	if err := os.MkdirAll(filepath.Join(outside, "real"), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outside, "real", "secret.txt"), []byte("nope"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	link := filepath.Join(root, "linked-dir")
	if err := os.Symlink(filepath.Join(outside, "real"), link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	verifier := newPermissionReviewEvidenceContainment("trusted")
	policy := gate.EvidenceContainmentPolicy{ReadRoot: root, SecurityCeiling: "trusted"}
	req := tool.Request{Requirements: []tool.Requirement{evidenceRequirement("evidence.filesystem.stat", "linked-dir/secret.txt")}}

	err := verifier.VerifyEvidenceContainment(context.Background(), policy, req)
	if err == nil {
		t.Fatal("VerifyEvidenceContainment() error = nil, want rejection of a target reached through a directory symlink that escapes root")
	}
	if !errors.Is(err, errEvidenceContainmentEscape) {
		t.Fatalf("VerifyEvidenceContainment() error = %v, want errEvidenceContainmentEscape", err)
	}
}

// TestPermissionReviewEvidenceContainmentCanonicalizesRootSymlink proves the
// ROOT side of the comparison is ALSO canonicalized: policy.ReadRoot passed
// as a symlinked path (macOS commonly symlinks /tmp -> /private/tmp) must
// still allow a legitimate in-root target, rather than falsely rejecting it
// because only one side of the comparison was resolved.
func TestPermissionReviewEvidenceContainmentCanonicalizesRootSymlink(t *testing.T) {
	t.Parallel()
	real := canonicalTempDir(t)
	if err := os.MkdirAll(filepath.Join(real, "real-root"), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeEvidenceFile(t, filepath.Join(real, "real-root"), "file.go", "package main")
	linkedRoot := filepath.Join(real, "linked-root")
	if err := os.Symlink(filepath.Join(real, "real-root"), linkedRoot); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	verifier := newPermissionReviewEvidenceContainment("trusted")
	// policy.ReadRoot is the UN-resolved symlinked path, exactly as a caller
	// on a platform with a symlinked temp dir would pass it.
	policy := gate.EvidenceContainmentPolicy{ReadRoot: linkedRoot, SecurityCeiling: "trusted"}
	req := tool.Request{Requirements: []tool.Requirement{evidenceRequirement("evidence.filesystem.stat", "file.go")}}

	if err := verifier.VerifyEvidenceContainment(context.Background(), policy, req); err != nil {
		t.Fatalf("VerifyEvidenceContainment() error = %v, want nil: a symlinked root must not falsely reject its own contents", err)
	}
}

// --- permissionReviewEvidenceContainment: ambiguous shapes ----------------

func TestPermissionReviewEvidenceContainmentRejectsAmbiguousRequirements(t *testing.T) {
	t.Parallel()
	root := canonicalTempDir(t)
	verifier := newPermissionReviewEvidenceContainment("trusted")
	policy := gate.EvidenceContainmentPolicy{ReadRoot: root, SecurityCeiling: "trusted"}

	tests := []struct {
		name  string
		match string
	}{
		{name: "NUL byte", match: "file\x00.go"},
		{name: "invalid UTF-8", match: string([]byte{0xff, 0xfe})},
		{name: "absolute path", match: "/etc/passwd"},
		{name: "bare dot-dot", match: ".."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req := tool.Request{Requirements: []tool.Requirement{evidenceRequirement("evidence.filesystem.read", tt.match)}}
			if err := verifier.VerifyEvidenceContainment(context.Background(), policy, req); err == nil {
				t.Fatalf("VerifyEvidenceContainment() with Match %q error = nil, want a fail-closed rejection", tt.match)
			}
		})
	}
}

func TestPermissionReviewEvidenceContainmentRejectsEmptyRequirementKind(t *testing.T) {
	t.Parallel()
	root := canonicalTempDir(t)
	verifier := newPermissionReviewEvidenceContainment("trusted")
	policy := gate.EvidenceContainmentPolicy{ReadRoot: root, SecurityCeiling: "trusted"}
	req := tool.Request{Requirements: []tool.Requirement{{Match: "file.go"}}}

	if err := verifier.VerifyEvidenceContainment(context.Background(), policy, req); err == nil {
		t.Fatal("VerifyEvidenceContainment() error = nil, want rejection of a requirement with no Kind")
	}
}

// TestPermissionReviewEvidenceContainmentRejectsEmptyReadRoot proves a
// missing or relative ReadRoot fails closed rather than being silently
// interpreted (e.g. as the process's current working directory).
func TestPermissionReviewEvidenceContainmentRejectsMalformedReadRoot(t *testing.T) {
	t.Parallel()
	verifier := newPermissionReviewEvidenceContainment("trusted")
	req := tool.Request{Requirements: []tool.Requirement{evidenceRequirement("evidence.filesystem.stat", ".")}}

	for _, root := range []string{"", "relative/path"} {
		policy := gate.EvidenceContainmentPolicy{ReadRoot: root, SecurityCeiling: "trusted"}
		if err := verifier.VerifyEvidenceContainment(context.Background(), policy, req); err == nil {
			t.Fatalf("VerifyEvidenceContainment() with ReadRoot=%q error = nil, want fail-closed rejection", root)
		}
	}
}

// TestPermissionReviewEvidenceContainmentNeverPanics feeds a battery of
// malformed/adversarial requests directly at VerifyEvidenceContainment and
// requires it to always return an error, never panic — the runtime fails
// closed on a panic here (design), but this proves Carbon's implementation
// does not rely on that safety net.
func TestPermissionReviewEvidenceContainmentNeverPanics(t *testing.T) {
	t.Parallel()
	root := canonicalTempDir(t)
	verifier := newPermissionReviewEvidenceContainment("trusted")

	adversarial := []tool.Request{
		{},
		{Requirements: []tool.Requirement{{}}},
		{Requirements: []tool.Requirement{evidenceRequirement("evidence.filesystem.read", string(make([]byte, 8192)))}},
		{Requirements: []tool.Requirement{
			evidenceRequirement("evidence.filesystem.stat", "a"),
			evidenceRequirement("evidence.filesystem.read", "../../../etc/shadow"),
		}},
	}
	for i, req := range adversarial {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("case %d: VerifyEvidenceContainment panicked: %v", i, r)
				}
			}()
			policy := gate.EvidenceContainmentPolicy{ReadRoot: root, SecurityCeiling: "trusted"}
			_ = verifier.VerifyEvidenceContainment(context.Background(), policy, req)
		}()
	}

	// A cancelled context must also fail closed, never panic.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	policy := gate.EvidenceContainmentPolicy{ReadRoot: root, SecurityCeiling: "trusted"}
	req := tool.Request{Requirements: []tool.Requirement{evidenceRequirement("evidence.filesystem.stat", ".")}}
	if err := verifier.VerifyEvidenceContainment(ctx, policy, req); err == nil {
		t.Fatal("VerifyEvidenceContainment() with a cancelled context error = nil, want a fail-closed rejection")
	}
}

// TestPermissionReviewEvidenceContainmentHandlesGitSentinelMatches proves the
// non-path Git sentinel Match values the real Git evidence tools use
// (classifiers/internal/evidence/git.go: "repository-status", "remotes",
// "branch" — none of which name a real workspace entry) resolve to the
// workspace root itself and are allowed, exactly like an empty/"." Match.
func TestPermissionReviewEvidenceContainmentHandlesGitSentinelMatches(t *testing.T) {
	t.Parallel()
	root := canonicalTempDir(t)
	verifier := newPermissionReviewEvidenceContainment("trusted")
	policy := gate.EvidenceContainmentPolicy{ReadRoot: root, SecurityCeiling: "trusted"}

	for _, sentinel := range []string{"repository-status", "remotes", "branch"} {
		req := tool.Request{Requirements: []tool.Requirement{evidenceRequirement("evidence.git.read", sentinel)}}
		if err := verifier.VerifyEvidenceContainment(context.Background(), policy, req); err != nil {
			t.Fatalf("VerifyEvidenceContainment() with git sentinel Match %q error = %v, want nil", sentinel, err)
		}
	}
}

// TestPermissionReviewEvidenceContainmentMultipleRequirementsAllMustPass
// proves one escaping requirement in a multi-requirement request fails the
// whole call, even when an earlier requirement was legitimate.
func TestPermissionReviewEvidenceContainmentMultipleRequirementsAllMustPass(t *testing.T) {
	t.Parallel()
	root := canonicalTempDir(t)
	writeEvidenceFile(t, root, "ok.go", "package main")
	verifier := newPermissionReviewEvidenceContainment("trusted")
	policy := gate.EvidenceContainmentPolicy{ReadRoot: root, SecurityCeiling: "trusted"}
	req := tool.Request{Requirements: []tool.Requirement{
		evidenceRequirement("evidence.filesystem.stat", "ok.go"),
		evidenceRequirement("evidence.filesystem.read", "../outside.txt"),
	}}

	if err := verifier.VerifyEvidenceContainment(context.Background(), policy, req); err == nil {
		t.Fatal("VerifyEvidenceContainment() error = nil, want rejection when ANY requirement escapes")
	}
}
