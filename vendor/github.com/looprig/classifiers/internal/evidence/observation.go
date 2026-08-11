package evidence

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/looprig/harness/pkg/tool"
)

// This file implements Harness's optional tool.EvidenceObservation
// capability (design §13.4, TOCTOU — see
// github.com/looprig/harness-permission-classifier's
// docs/plans/2026-07-27-permission-classifier-hustle-implementation.md,
// Addendum 4) for the two evidence tools in this package that genuinely
// resolve and report the state of ONE canonical target:
//
//   - evidence_filesystem_stat (pathStatTool): the flagship case — it
//     already exists specifically to report one path's lstat-level identity.
//   - evidence_filesystem_read (readFileTool): reads exactly one file's
//     bytes; the file it read is exactly as single-target as the path stat
//     already reports on.
//
// Every OTHER evidence tool in this package is deliberately left
// unimplemented:
//
//   - evidence_filesystem_list, evidence_filesystem_glob,
//     evidence_filesystem_grep: each reports on a VARIABLE-SIZE set of
//     matched/listed entries under one starting directory, not one target's
//     own canonical identity — recording an observation for "the directory
//     listing" or "the grep match set" does not map to "one target's
//     canonical identity, then and now" the way design §13.4 means it
//     (this is the addendum's own explicit exclusion for glob/grep-style
//     multi-match tools, applied identically to list).
//   - evidence_git_repository_status, evidence_git_remotes: both report
//     aggregate, whole-repository state (status across every changed path,
//     every configured remote) — the same "spans many things, not one
//     canonical target" shape as list/glob/grep, not a single resolved
//     ref/path.
//   - evidence_git_diff, evidence_git_branch: each DOES resolve something
//     that could naturally be target-sensitive (a ref-scoped diff, HEAD's
//     resolved branch/commit). They are still deliberately NOT wired here:
//     tool.Requirement's own doc comment reserves Match for "stored-rule
//     matching" and Scope for "access routing" — neither is a general
//     carrier for the extra structured data (git diff's ref/staged flag;
//     which specific ref branch resolution used) an observation token would
//     need to be self-describing from Target alone. Repurposing either
//     field, or overloading Request.Command (meant for command-backed
//     capabilities) to smuggle that data through, would risk silently
//     breaking whatever already depends on their documented contracts.
//     Giving Git evidence tools a real, precisely-specified target/token
//     scheme needs a small, deliberate carrier added to tool.Requirement (or
//     an equivalent), which is a Harness-side change — a follow-up dispatch,
//     not a tuck-in here. (When that lands, prefer the resolved commit/blob
//     SHA `git rev-parse` already gives as the token: it is naturally
//     content-addressed, and design §13.4 explicitly prefers reusing that
//     over inventing a weaker derived one.)
//
// ---- token derivation scheme (read this before changing anything below) --
//
// Both wired tools share ONE token FORMULA, implemented by
// fingerprintFields/fingerprintAbsentFields below: a SHA-256 over the
// target path's lstat-level identity (never following the final symlink)
// plus, only when the path IS itself a symlink, its immediate link-target
// text and the link target's own resolved metadata (when the link resolves
// at all). This directly captures "whether the path is/was a symlink and
// what it resolves to" — the exact signal design §13.4's own worked example
// (a deletion target being swapped for a symlink between evidence-gathering
// and auto-approval) requires; a token that omitted it would not defend
// against the attack this mechanism exists to close.
//
// filesystemObservationFingerprint(root, rel) performs this formula against
// a FRESH Lstat/Readlink/Stat lookup of its own, and is what readFileTool's
// captureObservation uses (its own internal read syscalls don't produce an
// Lstat-based result to reuse). pathStatTool instead calls
// fingerprintFields/fingerprintAbsentFields DIRECTLY from InvokableRun,
// reusing the exact Lstat/Readlink/Stat results it already computed to
// build its own model-visible text — the same formula, zero additional
// filesystem calls. Either way, the token is captured synchronously inside
// InvokableRun (via observationCapture, see below) and never re-derived
// later from ObservedRequirement — see fingerprintFields's own doc comment
// for why that distinction is the actual TOCTOU fix this file implements.
//
// Concretely, for a target that EXISTS, the hashed field sequence (via
// observationDigest, see its own doc comment for the exact wire encoding)
// is:
//
//	"fsv1", "present",
//	<lstat fields: entryKind, decimal size, decimal raw os.FileMode bits,
//	 decimal UnixNano mtime>,
//	then, ONLY if the lstat entry is itself a symlink, one of:
//	  "link_target", <raw readlink() text>, "link_resolved",
//	    <resolved-target lstat fields, same 4-tuple as above>
//	  "link_target_absent"        (link target does not exist)
//	  "link_target_unresolvable"  (link target could not be resolved —
//	                                e.g. it escapes containment)
//	  "link_unreadable"           (readlink itself failed)
//
// For a target that does NOT exist, the hashed sequence is just
// "fsv1", "absent" — deliberately not "no observation" (ok=false): a
// classifier that observed "this deletion target does not exist" IS making
// a real, TOCTOU-relevant observation (design's own worked example), so
// absence must produce a real, valid token, not a skip.
//
// "fsv1" is a fixed scheme-version literal: change it (to "fsv2", etc.)
// whenever the field list, order, or format below changes, so an old
// recorded token can never silently compare equal to a new one computed
// under a different scheme.
//
// entryKind, decimal size/mode/mtime are computed EXACTLY as
// entryKind/fileInfoFingerprintFields below do — see those for the precise
// formulas (Go's os.FileInfo.Size()/Mode()/ModTime(), no reformatting).
//
// HOW TO INDEPENDENTLY RE-DERIVE THIS TOKEN (for the Carbon-side
// gate.EvidenceObservationVerifier that rechecks it against a fresh look):
// call filesystemObservationFingerprint with root = the verifier's own
// canonicalized read root (gate.EvidenceContainmentPolicy's ReadRoot — or,
// equivalently, treat the recorded Target as already-absolute: it always
// equals filepath.Join(root, rel), so root can also be recovered as
// Target's directory chain up to the policy's own ReadRoot) and rel = the
// path relative to that root. Re-running the identical algorithm against
// live filesystem state and comparing the resulting hex digest to the
// recorded Token IS the recheck — no other data is required, and none of
// this format depends on anything ephemeral to one particular tool call
// (there is no offset/limit/content dependency: see readFileTool's
// ObservedRequirement doc comment below for why).

// filesystemObservationFingerprint computes the token described above for
// one root-relative path by performing its own fresh Lstat/Readlink/Stat
// lookups. It is the SOLE token FORMULA for every tool.EvidenceObservation
// implementation in this file — pathStatTool and readFileTool ultimately
// produce a token via the exact same field sequence, so a target's
// fingerprint means the same thing regardless of which tool observed it —
// but it is deliberately NOT how pathStatTool captures its own observation
// (see fingerprintFields's doc comment for why performing a fresh lookup
// here would reopen the TOCTOU gap this whole mechanism exists to close).
// It remains the right tool for readFileTool, whose own internal read
// syscalls (Open, which follows symlinks, then Stat on the open file
// descriptor) do not themselves compute an Lstat-based result to reuse; see
// readFileTool's own ObservedRequirement doc comment.
func filesystemObservationFingerprint(root, rel string) (string, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return "", err
	}
	defer r.Close()

	info, err := r.Lstat(rel)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return observationDigest(fingerprintAbsentFields()...), nil
	case err != nil:
		return "", err
	}

	if info.Mode()&fs.ModeSymlink == 0 {
		return observationDigest(fingerprintFields(info, "", nil, nil, nil)...), nil
	}

	target, linkErr := r.Readlink(rel)
	if linkErr != nil {
		return observationDigest(fingerprintFields(info, "", linkErr, nil, nil)...), nil
	}
	resolved, statErr := r.Stat(rel)
	return observationDigest(fingerprintFields(info, target, nil, resolved, statErr)...), nil
}

// fingerprintAbsentFields is the field sequence for a target that does NOT
// exist — see this file's top-of-file doc comment for why absence is a
// real, valid observation rather than "no observation."
func fingerprintAbsentFields() []string {
	return []string{"fsv1", "absent"}
}

// fingerprintFields is the PURE, I/O-free half of the token formula for a
// target that DOES exist: given an already-performed Lstat result (info)
// and — only when info is itself a symlink — the outcome of already
// resolving it (linkTarget/linkErr from Readlink, resolved/resolvedErr from
// a subsequent Stat), it assembles the exact "fsv1"/"present"/... field
// sequence this file's top-of-file doc comment specifies. It performs no
// lookup of its own.
//
// This is deliberately split out from filesystemObservationFingerprint (the
// I/O-performing half) so a caller that has ALREADY performed these exact
// lookups — as pathStatTool.InvokableRun always has, to build its own
// model-visible result — can feed this function the SAME info/linkTarget/
// resolved values it already holds, with zero additional filesystem calls,
// rather than asking filesystemObservationFingerprint to open the root and
// look everything up a second time. That distinction is the actual fix for
// the review finding this file addresses: the observation token must
// attest to the exact state InvokableRun's own result was built from, never
// a fresh, later, independent look — see pathStatTool's ObservedRequirement
// doc comment and observationCapture below for how that state is carried
// from InvokableRun forward to ObservedRequirement.
func fingerprintFields(info fs.FileInfo, linkTarget string, linkErr error, resolved fs.FileInfo, resolvedErr error) []string {
	fields := []string{"fsv1", "present"}
	fields = append(fields, fileInfoFingerprintFields(info)...)
	if info.Mode()&fs.ModeSymlink == 0 {
		return fields
	}
	if linkErr != nil {
		return append(fields, "link_unreadable")
	}
	fields = append(fields, "link_target", linkTarget)
	switch {
	case resolvedErr == nil:
		fields = append(fields, "link_resolved")
		fields = append(fields, fileInfoFingerprintFields(resolved)...)
	case errors.Is(resolvedErr, fs.ErrNotExist):
		fields = append(fields, "link_target_absent")
	default:
		fields = append(fields, "link_target_unresolvable")
	}
	return fields
}

// observationCapture holds, per root-relative path, the target/token pair
// most recently captured DURING an InvokableRun call for that path — never
// independently recomputed later. record is called from InvokableRun itself
// (see pathStatTool's and readFileTool's own InvokableRun for exactly
// where), synchronously, before InvokableRun returns its result up through
// the evidence runtime; lookup is called from the matching ObservedRequirement
// call the evidence runtime's own strictly sequential per-call loop always
// issues immediately afterward (internal/hustleruntime/evidence_runner.go:
// executeEvidenceCall's InvokableRun, THEN recordEvidenceObservation's
// ObservedRequirement, before the next call in the loop begins). lookup does
// NOT delete on read: the evidence runtime is free to call ObservedRequirement
// more than once for the same completed call (harmless — it is a pure
// accessor from Harness's side), and a stale entry can only ever be read by
// a rel path whose most recent InvokableRun genuinely produced it, since a
// later InvokableRun call for the same rel always overwrites it first.
//
// Keyed by rel rather than a single unkeyed slot purely as defense in
// depth: InvokableRun has no access to a per-call execution identity (that
// is Harness-internal state, never exposed to tool.InvokableTool's
// signature), so rel is the only stable, available correlation key: if this
// package's strictly-sequential-per-call invariant were ever violated
// elsewhere, a keyed map fails closed (a mismatched/absent entry) rather
// than silently returning some OTHER call's observation for the wrong
// target.
type observationCapture struct {
	mu      sync.Mutex
	pending map[string]capturedObservation
}

type capturedObservation struct {
	target string
	token  string
}

// record stores the target/token pair captured during the InvokableRun call
// for rel, overwriting whatever was previously recorded for rel.
func (c *observationCapture) record(rel, target, token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pending == nil {
		c.pending = make(map[string]capturedObservation, 1)
	}
	c.pending[rel] = capturedObservation{target: target, token: token}
}

// lookup returns the most recently recorded target/token pair for rel.
// ok=false means no InvokableRun call has yet recorded an observation for
// rel — treated identically to every other "this call made no
// target-sensitive observation" case (see filesystemObservationRequirement).
func (c *observationCapture) lookup(rel string) (target, token string, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	captured, found := c.pending[rel]
	if !found {
		return "", "", false
	}
	return captured.target, captured.token, true
}

// fileInfoFingerprintFields is the exact 4-tuple every lstat/stat result
// (the target itself, and separately a symlink's resolved target)
// contributes to the fingerprint: entryKind (this package's existing
// file/dir/symlink/other classifier), decimal byte size, the RAW decimal
// os.FileMode bit pattern (type bits and permission bits together — a
// mode change, including a type change such as file-to-symlink, always
// changes this even if entryKind's own coarser classification did not),
// and the modification time as decimal UnixNano (full available precision,
// not the truncated-to-the-second text writeFileInfo prints for humans).
func fileInfoFingerprintFields(info fs.FileInfo) []string {
	return []string{
		entryKind(info),
		strconv.FormatInt(info.Size(), 10),
		strconv.FormatUint(uint64(info.Mode()), 10),
		strconv.FormatInt(info.ModTime().UTC().UnixNano(), 10),
	}
}

// observationDigest deterministically and unambiguously combines fields
// into one SHA-256 hex digest. Each field is netstring-encoded
// (<decimal-byte-length>:<raw-bytes>, e.g. "3:abc") rather than joined with
// a fixed separator byte: a separator byte could appear INSIDE a field this
// package does not fully control (a symlink target's raw text may contain
// almost any byte short of NUL and '/'), which would let two different
// field sequences serialize to the identical byte string and collide. A
// length prefix makes that impossible regardless of a field's own content.
func observationDigest(fields ...string) string {
	var encoded []byte
	for _, f := range fields {
		encoded = append(encoded, []byte(fmt.Sprintf("%d:", len(f)))...)
		encoded = append(encoded, f...)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// canonicalObservationTarget is the Target half of every
// ObservationRequirement this file produces: root and rel joined and
// cleaned into one absolute path (filepath.Join already does both). This is
// self-describing on its own (an independent re-verifier needs no
// side-channel to know what it names), and is exactly what
// filesystemObservationFingerprint(root, rel) reports on.
func canonicalObservationTarget(root, rel string) string {
	return filepath.Join(root, rel)
}

// filesystemObservationRequirement extracts (root, rel) from a prepared
// evidence Request's single Requirement, requiring it to carry exactly the
// expected Kind and non-empty Scope/Match — the same shape every filesystem
// evidence tool's own PrepareCall in this package already produces. Any
// other shape (multiple/zero Requirements, a mismatched Kind, an empty
// field) is treated as "this call made no target-sensitive observation"
// (ok=false), never as an error — ObservedRequirement's own contract is
// that a malformed report is simply dropped, not fatal.
func filesystemObservationRequirement(request tool.Request, kind string) (root, rel string, ok bool) {
	if len(request.Requirements) != 1 {
		return "", "", false
	}
	requirement := request.Requirements[0]
	if requirement.Kind != kind || requirement.Scope == "" || requirement.Match == "" {
		return "", "", false
	}
	return requirement.Scope, requirement.Match, true
}

// ObservedRequirement implements tool.EvidenceObservation for
// evidence_filesystem_stat. It reports an observation for EVERY successful
// stat call, including one whose target does not exist — design §13.4's own
// worked example ("confirms a deletion target is empty/non-symlinked") is
// exactly an absence observation, and absence is exactly as TOCTOU-relevant
// as presence (see filesystemObservationFingerprint's doc comment).
//
// It performs NO filesystem lookup of its own: the token was already
// captured by InvokableRun, from the identical Lstat/Readlink/Stat results
// InvokableRun used to build its own model-visible result, and stashed in
// t.observations. Deriving it here via a fresh, independent restat (the
// pre-fix design) would reopen exactly the TOCTOU gap this mechanism exists
// to close — an attacker able to swap the target in the window between
// InvokableRun returning (with the model already having seen its result)
// and the evidence runtime calling ObservedRequirement afterward would have
// the swapped, post-attack state recorded as the "baseline," not what the
// model actually reasoned about. A rel with no captured entry (ok=false)
// means no matching InvokableRun call ever ran for it — dropped, not fatal,
// per filesystemObservationRequirement's own contract.
func (t *pathStatTool) ObservedRequirement(request tool.Request, _ *tool.ToolResult) (target, token string, ok bool) {
	root, rel, ok := filesystemObservationRequirement(request, KindFilesystemStat)
	if !ok || root != t.root {
		return "", "", false
	}
	return t.observations.lookup(rel)
}

var _ tool.EvidenceObservation = (*pathStatTool)(nil)

// ObservedRequirement implements tool.EvidenceObservation for
// evidence_filesystem_read. Like pathStatTool's, it performs no lookup of
// its own here — it returns whatever InvokableRun already captured into
// t.observations (see readFileTool.captureObservation) — but unlike
// pathStatTool, that capture itself still requires ONE fresh
// filesystemObservationFingerprint call: readFileTool's own internal read
// syscalls (Open, which follows a within-root symlink, then Stat on the
// already-open file descriptor) never compute an Lstat-based result the
// token formula could reuse directly, and reusing the identical
// lstat/symlink-chain formula Stat uses (rather than a resolved-target,
// content-following one) is a deliberate, documented choice — see the
// paragraph below. The fix for the review finding this file addresses is
// not "zero filesystem calls for Read" (not achievable without abandoning
// that documented formula) but WHEN that call happens: captureObservation
// runs synchronously via defer, immediately before InvokableRun returns —
// not later, after the result has already left this function and crossed
// back through the evidence runtime — which is what closes the actual
// TOCTOU window the finding identified.
//
// tool.Request carries only ToolName/Summary/ExecutionID/Command/
// WorkingDirectory/ExpiresAtUnixMilli/Requirements; a read call's
// offset/limit arguments never reach it (readFileTool's PrepareCall records
// only Requirement{Kind, Scope: root, Match: rel, Description}, exactly
// like every other tool in this package). A token that depended on the
// exact byte range read would therefore NOT be independently re-derivable
// from Target alone — the whole point this scheme exists to satisfy (see
// this file's top-of-file doc comment) — since nothing durable records
// which offset/limit produced it. Reusing the identical whole-file
// lstat/symlink-chain fingerprint instead keeps Read's token exactly as
// meaningful and exactly as reproducible as Stat's: it still catches the
// same classic TOCTOU swap (a file replaced by a symlink between the read
// and the eventual auto-approval), just not a content edit that leaves
// size/mode/mtime untouched — a strictly narrower, forgeable-only-by-an-
// attacker-who-can-also-forge-metadata gap Stat's own token already
// accepts.
func (t *readFileTool) ObservedRequirement(request tool.Request, _ *tool.ToolResult) (target, token string, ok bool) {
	root, rel, ok := filesystemObservationRequirement(request, KindFilesystemRead)
	if !ok || rel == "." || root != t.root {
		return "", "", false
	}
	return t.observations.lookup(rel)
}

var _ tool.EvidenceObservation = (*readFileTool)(nil)
