// Package commandsafety provides the public construction API for the
// initial gate.command-safety permission classifier described in
// docs/plans/2026-07-27-permission-classifier-hustle-design.md §19.
//
// The classifier reviews permission gates whose prepared request contains
// command execution or a command-triggered combination of filesystem and
// network requirements. It owns the classifier's prompt, wire codecs, risk
// policy, and evidence-tool catalog, and New returns a value that satisfies
// Harness's public gate.PermissionClassifier contract. A classifier result
// is evidence, never authority: only trusted Harness code can turn an
// eligible assessment into an ordinary gate approval.
//
// DefaultPolicy is internal/policy.DefaultPolicy's deterministic tightening
// policy for the complete gate.ReviewRiskCategory taxonomy: it floors the
// minimum honest risk a category implies (for example, destructive_shared
// and production_mutation never resolve below high), routes a fixed set of
// categories to a human at any authorization level (data_exfiltration,
// prompt_injection, authorization_conflict, target_ambiguity, and
// insufficient_evidence — the last three because they are the classifier's
// own signal that it lacks the confidence auto-approval requires), and
// requires a minimum authorization at each risk level. Reconcile applies
// this policy to an already strictly-decoded model assessment and only ever
// tightens an allow recommendation to needs_human; it never widens
// eligibility.
//
// StandardEvidence is the complete initial filesystem and Git evidence pack
// (design §13.2): 5 read-only filesystem tools (canonical path resolution,
// lstat-style and resolved-target metadata, bounded directory listing,
// bounded file reading, bounded glob, and bounded grep) confined to the
// review workspace root through Go's syscall-level, symlink-aware *os.Root,
// plus 4 read-only Git tools (repository status, diff metadata, configured
// remotes with optional visibility resolution, and branch/upstream/
// default-branch state) that invoke a fixed Git binary with a fixed,
// non-shell argument list. No evidence tool can write, mutate Git state, or
// contact a remote with write semantics.
package commandsafety
