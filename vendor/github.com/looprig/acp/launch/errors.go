package launch

import "fmt"

// ConfigError reports an invalid Config discovered before Dial starts
// anything: neither or both of OwnedProxy/SharedProxy set, or no
// HarnessAdapter supplied. Caught here rather than surfacing as a
// nil-pointer panic or a wastefully-started proxy.
type ConfigError struct{ Reason string }

func (e *ConfigError) Error() string { return "acp/launch: invalid config: " + e.Reason }

// ProxyNotReadyError reports that an owned ModelProxy's Start returned
// successfully but its own Binding immediately reported ready=false -- a
// startup-contract violation Dial treats as a failure (closing the proxy
// it just started) rather than proceeding with an undefined binding.
type ProxyNotReadyError struct{}

func (e *ProxyNotReadyError) Error() string {
	return "acp/launch: owned proxy started but reported not ready"
}

// PathError reports that a caller-supplied executable path -- the ACP
// adapter binary itself, or an underlying CLI a connector pins -- was not a
// clean, absolute path. Validated before any process is started; this
// package never resolves a path via PATH lookup or a shell.
type PathError struct{ Field, Reason string }

func (e *PathError) Error() string { return "acp/launch: " + e.Field + ": " + e.Reason }

// ConflictingEnvError reports that a caller-supplied stdio.Command.Env
// already contained a security-sensitive variable name a HarnessAdapter
// must exclusively own (either because the adapter sets it itself, or
// because the child's own contract requires it be absent) -- rejected
// rather than silently overwritten or stripped.
type ConflictingEnvError struct{ Key string }

func (e *ConflictingEnvError) Error() string {
	return fmt.Sprintf("acp/launch: conflicting environment variable %q already present", e.Key)
}

// ModelAliasError reports that a requested harness-facing model alias
// matched no value the connected adapter's "model" select config option
// advertised -- including the case where no such option was advertised at
// all. Model selection never silently no-ops: an unmatched alias is always
// a typed error.
type ModelAliasError struct{ Alias string }

func (e *ModelAliasError) Error() string {
	return fmt.Sprintf("acp/launch: model alias %q not advertised by the connected adapter", e.Alias)
}

// CodexVersionError reports that a codex-acp version probe (see
// ProbeCodexVersion in version.go) did not clear MinCodexVersion, for any
// reason: below the floor, a legacy binary with no recognizable version
// output, unparseable output, a nonzero exit, or a timeout. Every one of
// those classifications fails closed identically from a caller's
// perspective -- reject the adapter -- with Result carrying the specific
// CodexVersionClass a caller can inspect or log.
type CodexVersionError struct {
	Path   string
	Result CodexVersionResult
}

func (e *CodexVersionError) Error() string {
	return fmt.Sprintf("acp/launch: codex-acp at %q failed the version probe (%s)", e.Path, e.Result.Class)
}
