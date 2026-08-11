package filemutation

import (
	"context"

	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/tools/internal/workspace"
)

// file_mutation_permit.go carries the OPTIONAL session workspace coordinator into the
// structured file mutators (WriteFile/EditFile) and wraps their commit critical
// section in a SHARED session-mutation + canonical-PATH permit (design §"File-tool
// optimistic concurrency and binding", §"Native checkpoint boundary and workspace
// gate"). A PathMutation permit is shared across DIFFERENT canonical paths but
// EXCLUSIVE on the SAME canonical path, so it serializes writes to one real file
// ACROSS loops — the per-loop observation map only serializes WITHIN a loop — and it
// is wholly excluded by a Bash whole-workspace permit and by a checkpoint permit.
//
// The permit is OPTIONAL so the standalone/bare tool (no coordinator bound, as in the
// direct-construction unit tests) keeps Task 12's exact behavior: no permit, no health
// gate. The runtime always binds a real coordinator through the Files definition.

// fileMutatorConfig is the resolved construction config shared by WriteFile and
// EditFile.
type fileMutatorConfig struct {
	coord      tool.WorkspaceCoordinator
	hostWrites bool
}

// FileMutatorOption configures a structured file mutator (WriteFile/EditFile) at
// construction (functional-options pattern), preserving the coordinator-free
// constructors used by the unit tests.
type FileMutatorOption func(*fileMutatorConfig)

// WithMutationCoordinator binds the session workspace coordinator so the mutator's
// commit runs under a PathMutation permit and verifies lease health. A nil or
// typed-nil coordinator is ignored (the tool stays coordinator-free).
func WithMutationCoordinator(coord tool.WorkspaceCoordinator) FileMutatorOption {
	return func(cfg *fileMutatorConfig) {
		if !workspace.IsNil(coord) {
			cfg.coord = coord
		}
	}
}

// WithHostWrites lets an absolute write/edit target resolve OUTSIDE the
// workspace instead of being rejected at prepare time -- the write-side
// counterpart to readfile.WithHostReads()/grep.WithHostReads()/glob.WithHostReads().
// It grants nothing itself: an uncontained resolved target still emits a
// filesystem.write requirement, and the caller's bound access source (the
// sandbox.Profile) makes the actual Allow/Deny/Gated decision. A relative
// "../" traversal is never widened by this option.
func WithHostWrites() FileMutatorOption {
	return func(cfg *fileMutatorConfig) { cfg.hostWrites = true }
}

func resolveFileMutatorConfig(opts []FileMutatorOption) fileMutatorConfig {
	var cfg fileMutatorConfig
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	return cfg
}

// acquirePathMutation takes the SHARED session-mutation + canonical-PATH permit for a
// structured mutator, then verifies lease health — fail-secure: a mutator that cannot
// verify lease health does NOT write. A nil coordinator (standalone/bare path) yields
// a no-op permit and skips the health gate, preserving the coordinator-free behavior.
// The returned permit MUST be Released by the caller (deferred immediately). ctx is
// the per-call ctx: a canceled acquire returns its typed error and no permit.
func acquirePathMutation(ctx context.Context, coord tool.WorkspaceCoordinator, key canonicalObservationKey) (tool.WorkspacePermit, error) {
	if workspace.IsNil(coord) {
		return noPermit{}, nil
	}
	permit, err := coord.Acquire(ctx, tool.WorkspaceOperationPathMutation, string(key))
	if err != nil {
		return nil, err
	}
	if err := coord.Healthy(); err != nil {
		permit.Release()
		return nil, &LeaseUnhealthyError{Cause: err}
	}
	return permit, nil
}

// noPermit is the no-op tool.WorkspacePermit returned when no coordinator is bound, so
// callers can uniformly defer Release.
type noPermit struct{}

func (noPermit) Release() {}

// LeaseUnhealthyError reports that a structured mutation was refused because the
// workspace lease could not be verified healthy at commit time (fail-secure). Its
// message carries no secret; Cause is the underlying lease-health error.
type LeaseUnhealthyError struct{ Cause error }

func (e *LeaseUnhealthyError) Error() string {
	return "workspace lease is not healthy; refusing to write: " + e.Cause.Error()
}

func (e *LeaseUnhealthyError) Unwrap() error { return e.Cause }

var _ tool.WorkspacePermit = noPermit{}
