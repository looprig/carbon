package sessionruntime

import "github.com/looprig/harness/pkg/hook"

// WithHooks installs the immutable operation-hook runner for native loops.
func WithHooks(runner *hook.Runner) Option {
	return func(s *Session) {
		s.hooks = runner
	}
}
