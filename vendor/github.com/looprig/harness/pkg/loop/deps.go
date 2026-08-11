package loop

import (
	"context"

	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/tool"
)

// AccessGate is the runner's view of the combined three-state access decision
// for one prepared request. It is satisfied by *gate.Evaluator (interactive or
// headless construction) or by a consumer-provided equivalent.
//
// Authorize evaluates the complete typed request once, opens at most one
// combined approval (interactive construction only), and returns the fresh
// execution-bound grant tokens for the approved call. An unapproved Resolution
// with a nil error is a policy or user denial; any error is fail-closed. An
// implementation must be safe for concurrent calls.
type AccessGate interface {
	Authorize(ctx context.Context, request tool.Request) (gate.Resolution, error)
}

// DelegationStyle selects the model-facing delegation action set.
type DelegationStyle uint8

const (
	DelegationSyncOnly DelegationStyle = iota
	DelegationManaged
)

// Delegation is the immutable delegation policy copied into a Definition.
type Delegation struct{ Style DelegationStyle }

// deps.go is the loop runtime's consumer surface for the tool subsystem (design §3b).
// The loop depends only on these interfaces and value types; it never imports
// the concrete `tools/` package. The composition root wires concrete
// implementations (an access evaluator, read guards) while binding an immutable
// Definition for the private actor runtime.

// ReadGuard is the narrow read-side policy the read tools enforce themselves
// (Interface Segregation: read tools depend only on this, not the full gate).
// DeniedRead filters denied paths during Glob/Grep traversal and results;
// MaxReadBytes is the per-file cap ReadFile/Grep apply via io.LimitReader.
//
// This is the read-adaptation SEAM: it is deliberately stdlib-typed (no import
// of any sandbox package) so a consumer can build one ReadGuard from its sandbox
// profile's read rules and bind the native ReadFile/Grep/Glob tools IDENTICALLY
// to a sandboxed `sh -c cat` — a single source of truth, with no drift between
// the in-process guards and OS enforcement.
type ReadGuard interface {
	// DeniedRead reports whether reading absPath is denied by policy (e.g. the
	// §5.3 secret deny-reads such as "**/.env*", or a zerotrust restricted-read).
	//
	// CANONICAL-PATH CONTRACT (fail-secure): absPath MUST be an ABSOLUTE,
	// filepath.Clean'ed, SYMLINK-RESOLVED path. The guard is purely LEXICAL — it
	// matches the string it is handed and performs NO filesystem resolution of its
	// own. Resolving symlinks (and, on a case-insensitive volume such as default
	// macOS/APFS, canonicalising case) BEFORE the call is the CALLER's (the tool's)
	// responsibility: a guard fed a non-canonical path can be bypassed by a symlink
	// or a case variant that resolves to the denied file. The native read tools
	// honour this — ReadFile passes the containedPath-resolved abs, Grep/Glob pass
	// the EvalSymlinks'd path via denyFilteredRel. This mirrors the sandbox Resolve
	// contract, so the confinement adapter and the native tools must both feed canonical
	// paths or a deny is trivially evaded.
	DeniedRead(absPath string) bool
	// MaxReadBytes is the per-file read cap (bytes) ReadFile/Grep apply via
	// io.LimitReader.
	MaxReadBytes() int64
}
