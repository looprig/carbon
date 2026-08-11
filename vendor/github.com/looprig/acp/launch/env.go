// env.go builds the environment a HarnessAdapter hands its ACP child,
// reusable by any adapter this package (or a later one, e.g. Codex/Gemini)
// implements: deep-copy the caller's Command rather than mutate it, add or
// replace only the adapter's own documented variables, and reject rather
// than silently strip a caller-supplied value for a name the child
// contract forbids.
package launch

import (
	"strings"

	"github.com/looprig/acp/transport/stdio"
)

// envOverride is one environment variable a HarnessAdapter sets on the
// child it configures: existing cmd.Env entries with the same key are
// replaced in place (the design doc's "add or replace only their
// documented gateway variables"); a key with no existing entry is
// appended.
type envOverride struct {
	Key   string
	Value string
}

// buildChildCommand returns a deep copy of cmd with each override applied
// and Args defensively copied, after first rejecting any name in forbidden
// that is already present in cmd.Env regardless of value (the design doc's
// "reject duplicate/conflicting security-sensitive values": a caller-
// supplied value for a name the child's contract forbids is a
// configuration bug to surface, never silently stripped or overwritten).
//
// cmd itself, and its Env/Args backing arrays, are never mutated -- callers
// always get back a fresh Command, so mutating the result (or the original
// cmd's slices, afterward) can never alias the other's memory. This
// function never inspects the ambient process environment: cmd.Env is
// already the caller's complete allowlisted child environment (see
// stdio.Command's own doc), and this is the only input read here.
func buildChildCommand(cmd stdio.Command, overrides []envOverride, forbidden []string) (stdio.Command, error) {
	env := append([]string(nil), cmd.Env...)
	for _, key := range forbidden {
		if envIndex(env, key) >= 0 {
			return stdio.Command{}, &ConflictingEnvError{Key: key}
		}
	}
	for _, ov := range overrides {
		entry := ov.Key + "=" + ov.Value
		prefix := ov.Key + "="
		replaced := false
		for i, kv := range env {
			if strings.HasPrefix(kv, prefix) {
				env[i] = entry
				replaced = true
			}
		}
		if !replaced {
			env = append(env, entry)
		}
	}

	out := cmd
	out.Env = env
	out.Args = append([]string(nil), cmd.Args...)
	return out, nil
}

// envIndex returns the index of key's "key=value" entry in env, or -1 if
// key has no entry there.
func envIndex(env []string, key string) int {
	prefix := key + "="
	for i, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return i
		}
	}
	return -1
}
