// version.go implements ProbeCodexVersion: a bounded, injectable one-shot
// probe of a codex-acp binary's own "--version" output, and the fail-closed
// classification of its result. See codex.go/codex_connector.go for the
// connector this probe gates: Configure itself never runs this probe (it
// takes no context and must never spawn a process -- see codex.go's own
// doc), so a caller is expected to run ProbeCodexVersion once, before ever
// constructing a Config.Command whose Path points at that binary, exactly
// the "one-shot --version invocation ... outside the ACP session lifecycle"
// carve-out acp/CLAUDE.md documents for exec.CommandContext.
package launch

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// CodexVersion is a parsed codex-acp "<major>.<minor>.<patch>" version.
type CodexVersion struct {
	Major, Minor, Patch int
}

// String renders v in dotted form.
func (v CodexVersion) String() string {
	return strconv.Itoa(v.Major) + "." + strconv.Itoa(v.Minor) + "." + strconv.Itoa(v.Patch)
}

// Less reports whether v sorts strictly before other.
func (v CodexVersion) Less(other CodexVersion) bool {
	if v.Major != other.Major {
		return v.Major < other.Major
	}
	if v.Minor != other.Minor {
		return v.Minor < other.Minor
	}
	return v.Patch < other.Patch
}

// MinCodexVersion is the oldest codex-acp version this connector supports
// (see the design doc's Codex connector section): the legacy
// @zed-industries/codex-acp 0.16.x binary predates this and does not even
// answer --version. Not meant to be mutated by callers; exposed as a
// documented, inspectable constant rather than an unexported literal.
var MinCodexVersion = CodexVersion{Major: 1, Minor: 1, Patch: 7}

// DefaultCodexVersionProbeTimeout is the bound ProbeCodexVersion applies to
// its probe when a caller passes timeout <= 0.
const DefaultCodexVersionProbeTimeout = 5 * time.Second

// CodexVersionClass classifies a codex-acp version probe's outcome. Every
// value other than CodexVersionModern is a fail-closed rejection: an
// adapter is accepted only at CodexVersionModern, never on ambiguity.
type CodexVersionClass int

const (
	// CodexVersionUnknown is CodexVersionClass's zero value: it is never
	// returned by ProbeCodexVersion, which always assigns one of the
	// named classes below.
	CodexVersionUnknown CodexVersionClass = iota
	// CodexVersionModern: parseable, >= MinCodexVersion. The only class
	// ProbeCodexVersion accepts (returns a nil error for).
	CodexVersionModern
	// CodexVersionBelowMinimum: parseable, but < MinCodexVersion.
	CodexVersionBelowMinimum
	// CodexVersionLegacyNoVersion: the probe exited successfully but
	// produced no recognizable version output at all (empty stdout) --
	// one plausible shape for a legacy binary that silently ignores
	// --version. (The historical @zed-industries/codex-acp 0.16.x binary
	// in practice exits non-zero for an unrecognized flag, which
	// classifies as CodexVersionNonzeroExit instead; both fail closed
	// identically, so the practical rejection outcome is the same either
	// way.)
	CodexVersionLegacyNoVersion
	// CodexVersionUnparseable: the probe produced non-empty output that
	// does not match the expected "<major>.<minor>.<patch>" shape (a
	// partial version, a prerelease/build suffix, or unrelated text).
	CodexVersionUnparseable
	// CodexVersionNonzeroExit: the probe process itself failed to run to
	// a clean, successful exit (a real invocation error, or a nonzero
	// exit status).
	CodexVersionNonzeroExit
	// CodexVersionTimeout: the probe did not complete within the bound
	// ProbeCodexVersion applied.
	CodexVersionTimeout
)

// String renders c as a short, stable diagnostic label.
func (c CodexVersionClass) String() string {
	switch c {
	case CodexVersionModern:
		return "modern"
	case CodexVersionBelowMinimum:
		return "below-minimum"
	case CodexVersionLegacyNoVersion:
		return "legacy-no-version"
	case CodexVersionUnparseable:
		return "unparseable"
	case CodexVersionNonzeroExit:
		return "nonzero-exit"
	case CodexVersionTimeout:
		return "timeout"
	default:
		return "unknown"
	}
}

// CodexVersionResult is ProbeCodexVersion's outcome: a classification plus
// (when parseable) the actual version observed and the raw probe output,
// kept only for diagnosis -- callers must never parse Raw themselves; the
// only supported interpretation of a probe is Class (and Version, when
// Class is CodexVersionModern or CodexVersionBelowMinimum).
type CodexVersionResult struct {
	Class   CodexVersionClass
	Version CodexVersion
	Raw     string
}

// CodexVersionRunner runs one bounded codex-acp version probe and returns
// its raw stdout and the invocation's error (nil on a clean, successful
// exit). ctx is already bound to ProbeCodexVersion's own timeout; a real
// implementation (defaultCodexVersionRunner) must honor ctx's
// deadline/cancellation and must never block past it. Tests substitute a
// fake runner so no real codex-acp binary is ever spawned (see
// version_test.go / codex_test.go).
type CodexVersionRunner func(ctx context.Context, path string) (stdout []byte, err error)

// defaultCodexVersionRunner runs `<path> --version` via exec.CommandContext
// -- a one-shot invocation outside the ACP session lifecycle (see
// acp/CLAUDE.md), never a shell, never PATH lookup: path is the caller's
// own already-resolved absolute path, passed as a single argument value,
// never interpolated into a command line.
func defaultCodexVersionRunner(ctx context.Context, path string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, path, "--version")
	return cmd.Output()
}

var _ CodexVersionRunner = defaultCodexVersionRunner

// ProbeCodexVersion runs runner(ctx, path) -- defaultCodexVersionRunner when
// runner is nil, the production path -- bounded by timeout (or
// DefaultCodexVersionProbeTimeout when timeout <= 0), and classifies the
// result. path must already be a clean, absolute path (see
// cleanAbsolutePath): an invalid path fails with *PathError before runner
// is ever invoked.
//
// ProbeCodexVersion returns a nil error only for CodexVersionModern; every
// other classification returns a non-nil *CodexVersionError alongside the
// same CodexVersionResult, so a caller can treat "err == nil" as the
// entire adapter-acceptance decision while still inspecting Result.Class
// for diagnostics.
func ProbeCodexVersion(ctx context.Context, path string, timeout time.Duration, runner CodexVersionRunner) (CodexVersionResult, error) {
	if !cleanAbsolutePath(path) {
		return CodexVersionResult{}, &PathError{Field: "Path", Reason: "must be a clean absolute path to codex-acp"}
	}
	if runner == nil {
		runner = defaultCodexVersionRunner
	}
	if timeout <= 0 {
		timeout = DefaultCodexVersionProbeTimeout
	}

	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	stdout, runErr := runner(probeCtx, path)
	raw := strings.TrimSpace(string(stdout))

	result := classifyCodexVersion(probeCtx, raw, runErr)
	if result.Class != CodexVersionModern {
		return result, &CodexVersionError{Path: path, Result: result}
	}
	return result, nil
}

// classifyCodexVersion is ProbeCodexVersion's decision table. Precedence,
// most specific first: a timed-out probe is always CodexVersionTimeout
// regardless of what runErr/raw otherwise look like; a genuine invocation
// failure (runErr != nil) is always CodexVersionNonzeroExit; only once both
// of those are ruled out does raw's own content decide between
// legacy-no-version, unparseable, below-minimum, and modern.
//
// "Timed-out" here means probeCtx's own internal timeout specifically: if
// the caller instead cancels the *outer* ctx passed to ProbeCodexVersion
// while the runner is still blocked, probeCtx.Err() reports
// context.Canceled, not context.DeadlineExceeded, so this falls through to
// the runErr != nil case and classifies as CodexVersionNonzeroExit instead
// of CodexVersionTimeout. Still a fail-closed rejection either way -- just
// not the label a caller might expect from an outer-canceled probe.
func classifyCodexVersion(ctx context.Context, raw string, runErr error) CodexVersionResult {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return CodexVersionResult{Class: CodexVersionTimeout, Raw: raw}
	}
	if runErr != nil {
		return CodexVersionResult{Class: CodexVersionNonzeroExit, Raw: raw}
	}
	if raw == "" {
		return CodexVersionResult{Class: CodexVersionLegacyNoVersion, Raw: raw}
	}
	version, ok := parseCodexVersion(raw)
	if !ok {
		return CodexVersionResult{Class: CodexVersionUnparseable, Raw: raw}
	}
	if version.Less(MinCodexVersion) {
		return CodexVersionResult{Class: CodexVersionBelowMinimum, Version: version, Raw: raw}
	}
	return CodexVersionResult{Class: CodexVersionModern, Version: version, Raw: raw}
}

// parseCodexVersion parses codex-acp's own "--version" output: package name
// and dotted version separated by whitespace, e.g.
// "@agentclientprotocol/codex-acp 1.1.7" (the reference shape this
// connector's contract was verified against -- see the design doc). The
// parse is deliberately strict: the last whitespace-separated field must be
// exactly three non-negative, all-digit, dot-separated components. Partial
// versions ("1.2") and prerelease/build tags ("1.2.0-rc1") both fail
// closed as unparseable rather than being optimistically accepted.
func parseCodexVersion(raw string) (CodexVersion, bool) {
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return CodexVersion{}, false
	}
	parts := strings.Split(fields[len(fields)-1], ".")
	if len(parts) != 3 {
		return CodexVersion{}, false
	}
	var nums [3]int
	for i, p := range parts {
		if !isDigits(p) {
			return CodexVersion{}, false
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return CodexVersion{}, false
		}
		nums[i] = n
	}
	return CodexVersion{Major: nums[0], Minor: nums[1], Patch: nums[2]}, true
}

// isDigits reports whether s is non-empty and consists entirely of ASCII
// digits: strconv.Atoi alone would also accept a leading "+" or "-" sign,
// neither of which is a valid version-component shape.
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
