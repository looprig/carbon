package rig

import "github.com/looprig/harness/pkg/workspacestore"

// session_options.go defines the per-call NewSession options. Today the only option is
// WithSeedSnapshot; the type is variadic so future per-session knobs compose without an
// API break.

// SessionOption configures a single Rig.NewSession call.
type SessionOption func(*sessionOptions) error

// sessionOptions accumulates the resolved per-call NewSession configuration.
type sessionOptions struct {
	seed    workspacestore.Ref
	seedSet bool
}

// WithSeedSnapshot materializes ref into the new session's workspace before constructing
// loops and journals it as the first workspace checkpoint (design §"Seeding"). It is valid
// only for per-session roots and an EMPTY exclusive root, never for a shared root, and the
// ref must resolve in the configured workspace store — all enforced at NewSession, which
// fails closed on a bad seed.
func WithSeedSnapshot(ref workspacestore.Ref) SessionOption {
	return func(o *sessionOptions) error {
		if o.seedSet {
			return &SessionOptionError{Kind: SessionOptionDuplicateSeed}
		}
		if ref == "" {
			return &SessionOptionError{Kind: SessionOptionEmptySeed}
		}
		o.seedSet = true
		o.seed = ref
		return nil
	}
}

// resolveSessionOptions applies the NewSession options, returning the accumulated config.
func resolveSessionOptions(opts []SessionOption) (sessionOptions, error) {
	var resolved sessionOptions
	for _, opt := range opts {
		if opt == nil {
			return sessionOptions{}, &SessionOptionError{Kind: SessionOptionNil}
		}
		if err := opt(&resolved); err != nil {
			return sessionOptions{}, err
		}
	}
	return resolved, nil
}
