package bash

// prepare.go implements Bash's tool-owned preparation boundary (access-profile
// spec, "Tool preparation boundary"): decode and validate the untrusted args
// ONCE, normalize the command, resolve the confined spawn directory, and emit
// one typed access request containing the always-present command-backed
// requirement plus one requirement per EXPLICITLY DECLARED filesystem/network
// delta. The declaration requests authority — it never grants it, and an
// omitted gated delta remains OS-blocked by the sandbox at run time.
//
// Every requirement Bash emits is command-backed: its grant class/target pair
// names the exact sandbox enforcement class (command.start.v1,
// network.proxy-target.v1, network.broad.v1, filesystem.{path,tree,host}.*)
// and the target encoding sandbox.IssueGrant validates. The gate mints the
// grants only AFTER the combined decision; command Allow needs no token and
// command Deny never reaches issuance.

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/tools/permission"
)

// bashGrantValidity bounds the window between preparation and the last moment
// an issued grant may authorize the spawn: generous enough for a human to read
// the combined prompt, bounded so a stale approved call cannot execute later.
const bashGrantValidity = 15 * time.Minute

// accessDecl is the optional structured access request declared alongside the
// command. It is a request for authority, not a trusted description of what
// the shell will do.
type accessDecl struct {
	Network []networkDecl `json:"network"`
	Read    []fsDecl      `json:"read"`
	Write   []fsDecl      `json:"write"`
}

// networkDecl declares one egress need. A host names an exact proxy-enforced
// target; omitting the host requests a truthfully labeled BROAD, exact-command
// -bound egress delta for the port (the raw-TCP/proxy-unaware fallback).
type networkDecl struct {
	Transport string `json:"transport"`
	Host      string `json:"host"`
	Port      int    `json:"port"`
}

// fsDecl declares one filesystem delta: {"scope":"path","path":...},
// {"scope":"tree","path":...}, or the explicitly broad {"scope":"host"}.
type fsDecl struct {
	Scope string `json:"scope"`
	Path  string `json:"path"`
}

// bashArtifact binds the validated preparation output to one call. Execution
// consumes it verbatim — the raw args are never reparsed.
//
// The supervision-facing fields (supervised/background/yieldRequested/
// yieldTimeMS/tty/maxOutputBytes/noDeadline) are normalized and frozen here
// by Task 14 and consumed by InvokableRun's supervised dispatch
// (bash.go/supervised.go's runSupervised) for a call where supervised is
// true. timeout remains the legacy synchronous-path clamp (meaningless when
// noDeadline is true) and also backs runSupervised's processDeadline for a
// supervised call that did not request `timeout: 0`.
type bashArtifact struct {
	tool.TokenArtifact
	command    string
	workdirRaw string
	dirAbs     string
	timeout    time.Duration

	supervised        bool
	background        bool
	yieldRequested    bool
	yieldTimeMS       int
	tty               bool
	hasMaxOutputBytes bool
	maxOutputBytes    int64
	noDeadline        bool
}

// bashPrepareError is the typed preparation failure; its message is model-safe.
type bashPrepareError struct{ reason string }

func (e *bashPrepareError) Error() string { return e.reason }

func prepareFail(format string, args ...any) error {
	return &bashPrepareError{reason: fmt.Sprintf(format, args...)}
}

// PrepareCall decodes, validates, and normalizes one Bash call and produces
// its typed access request: the command.execute requirement (grant class
// command.start.v1, grant target = Match = the exact normalized command) plus
// one requirement per declared delta, all in ONE request so a gated command
// and its deltas share a single combined approval.
func (b *BashTool) PrepareCall(_ context.Context, executionID uuid.UUID, argsJSON string) (tool.Request, tool.PreparedArtifact, error) {
	if b.initErr != nil {
		return tool.Request{}, nil, b.initErr
	}
	var a bashArgs
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return tool.Request{}, nil, prepareFail("invalid arguments: not a JSON object")
	}
	command, err := normalizeBashCommand(a.Command)
	if err != nil {
		return tool.Request{}, nil, err
	}
	supervision, err := normalizeSupervision(a)
	if err != nil {
		return tool.Request{}, nil, err
	}
	if supervision.Supervised && hasDetachedSyntax(command) {
		return tool.Request{}, nil, prepareFail("command uses detached shell syntax, which is not allowed for a supervised call: %s", command)
	}
	dir, err := resolveSpawnDir(b.root, a.Workdir)
	if err != nil {
		return tool.Request{}, nil, prepareFail("workdir is outside the workspace: %s", a.Workdir)
	}

	requirements := []tool.Requirement{commandRequirement(command, b.familyEligible)}
	if a.Access != nil {
		deltas, err := declaredDeltas(command, dir, *a.Access)
		if err != nil {
			return tool.Request{}, nil, err
		}
		requirements = append(requirements, deltas...)
	}
	requirements = dedupeRequirements(requirements)

	request := tool.Request{
		ToolName:           bashToolName,
		ExecutionID:        executionID.String(),
		Command:            command,
		WorkingDirectory:   dir,
		ExpiresAtUnixMilli: time.Now().Add(bashGrantValidity).UnixMilli(),
		Requirements:       requirements,
	}
	artifact := &bashArtifact{
		command:           command,
		workdirRaw:        a.Workdir,
		dirAbs:            dir,
		timeout:           supervision.Timeout,
		supervised:        supervision.Supervised,
		background:        supervision.Background,
		yieldRequested:    supervision.YieldRequested,
		yieldTimeMS:       supervision.YieldTimeMS,
		tty:               supervision.TTY,
		hasMaxOutputBytes: supervision.HasMaxOutputBytes,
		maxOutputBytes:    supervision.MaxOutputBytes,
		noDeadline:        supervision.NoDeadline,
	}
	return request, artifact, nil
}

// supervisionArgs is the normalized, frozen form of Bash's supervision-facing
// arguments (long-running-command-supervision spec, "Bash API"). A zero value
// (Supervised == false) is the legacy synchronous call; a supervised call
// routes through the shared process supervisor (bash.go/supervised.go's
// runSupervised). PrepareCall freezes this value into the bashArtifact
// regardless, so mutating the raw args afterward changes nothing.
type supervisionArgs struct {
	Supervised        bool
	Background        bool
	YieldRequested    bool
	YieldTimeMS       int
	TTY               bool
	HasMaxOutputBytes bool
	MaxOutputBytes    int64
	NoDeadline        bool          // timeout:0 under supervision: run until session shutdown.
	Timeout           time.Duration // clamped hard lifetime deadline; meaningless when NoDeadline.
}

// normalizeSupervision decodes and validates Bash's presence-aware
// supervision arguments. Explicit supervision is detected from `background`
// or a PRESENT `yield_time_ms` (the spec: "timeout: 0 is valid only when
// background or yield_time_ms enables supervision") — `tty` and an explicit
// `timeout: 0` both REQUIRE that supervision rather than granting it
// themselves, so a foreground call using only the pre-existing fields decodes
// identically to before (Supervised stays false, Timeout is the same clamp
// clampBashTimeout always produced).
func normalizeSupervision(a bashArgs) (supervisionArgs, error) {
	settings := supervisionArgs{
		Supervised: a.Background || a.YieldTimeMS != nil,
		Background: a.Background,
		TTY:        a.TTY,
	}

	if a.YieldTimeMS != nil {
		if *a.YieldTimeMS < 0 {
			return supervisionArgs{}, prepareFail("yield_time_ms must be >= 0")
		}
		settings.YieldRequested = true
		settings.YieldTimeMS = *a.YieldTimeMS
	}

	if a.TTY && !settings.Supervised {
		return supervisionArgs{}, prepareFail("tty requires background or yield_time_ms")
	}

	if a.MaxOutputBytes != nil {
		if *a.MaxOutputBytes <= 0 {
			return supervisionArgs{}, prepareFail("max_output_bytes must be > 0")
		}
		settings.HasMaxOutputBytes = true
		settings.MaxOutputBytes = *a.MaxOutputBytes
	}

	switch {
	case a.Timeout == nil:
		settings.Timeout = clampBashTimeout(0)
	case *a.Timeout < 0:
		return supervisionArgs{}, prepareFail("timeout must be >= 0")
	case *a.Timeout == 0:
		if !settings.Supervised {
			return supervisionArgs{}, prepareFail("timeout: 0 requires background or yield_time_ms")
		}
		settings.NoDeadline = true
	default:
		settings.Timeout = clampBashTimeout(*a.Timeout)
	}

	return settings, nil
}

// detachedSyntaxTokens are shell utilities whose entire purpose is to detach a
// descendant from its parent's process group/session. Detecting them is
// intentionally conservative (a token match, not a shell parse): supervised
// process-tree containment is the real security boundary (spec, "Cross-
// platform process trees"), and this check exists only to reject an obvious
// attempt to defeat it before the call is ever admitted.
var detachedSyntaxTokens = []string{"nohup", "setsid", "disown"}

// hasDetachedSyntax conservatively reports whether command ends with a shell
// background operator ('&', as opposed to the '&&' AND-operator) or contains
// one of detachedSyntaxTokens as a whitespace-delimited word. It is applied
// only to calls that request supervision (spec: "Conservative detached-syntax
// rejection applies only when a call requests supervision"); legacy
// foreground execution keeps its existing shell compatibility unchanged.
func hasDetachedSyntax(command string) bool {
	if hasTrailingBackgroundOperator(command) {
		return true
	}
	for _, field := range strings.Fields(command) {
		trimmed := strings.Trim(field, "();|&")
		for _, token := range detachedSyntaxTokens {
			if trimmed == token {
				return true
			}
		}
	}
	return false
}

// hasTrailingBackgroundOperator reports whether command ends with a shell '&'
// job-control operator backgrounding its final pipeline, as opposed to the
// '&&' AND-operator.
func hasTrailingBackgroundOperator(command string) bool {
	trimmed := strings.TrimRight(command, " \t")
	return strings.HasSuffix(trimmed, "&") && !strings.HasSuffix(trimmed, "&&")
}

// normalizeBashCommand produces the exact normalized command every match,
// grant target, and stored candidate binds to: surrounding whitespace is
// trimmed and interior bytes are preserved VERBATIM. Reconstructing a
// whitespace-collapsed command from the shell tokenizer would be lossy (quoted
// arguments lose their quotes), so Bash does not attempt it; the token-aware
// family matcher already tolerates uncollapsed interior whitespace and
// exact-segment coverage compares the same raw bytes preparation emitted.
func normalizeBashCommand(raw string) (string, error) {
	command := strings.TrimSpace(raw)
	if command == "" {
		return "", prepareFail("a non-empty 'command' is required")
	}
	if !utf8.ValidString(command) || strings.ContainsRune(command, '\x00') {
		return "", prepareFail("command contains invalid bytes")
	}
	return command, nil
}

// commandRequirement builds the always-present command-backed requirement and
// its single reusable candidate: a token-prefix family through the injected
// eligibility catalog when the command qualifies, the exact normalized command
// otherwise. The candidate's grant pair mirrors the requirement's — issuance
// after any match remains exact-command command.start.v1. A command whose own
// text collides with the Bash(...) rule-syntax namespace gets no reusable
// candidate at all (ProposeCommandCandidate returns ""): once-only approval
// through the requirement still works, but no durable rule is offered,
// because the store would re-read the exact fallback as rule syntax.
func commandRequirement(command string, eligible permission.FamilyEligibility) tool.Requirement {
	requirement := tool.Requirement{
		Kind:        tool.CapabilityCommandExecute,
		Match:       command,
		Description: "execute `" + command + "`",
		GrantClass:  tool.GrantClassCommandStart,
		GrantTarget: command,
	}
	if candidateMatch := permission.ProposeCommandCandidate(command, eligible); candidateMatch != "" {
		requirement.Candidates = []tool.RuleCandidate{{
			Kind:        tool.CapabilityCommandExecute,
			Match:       candidateMatch,
			Description: "allow `" + candidateMatch + "`",
			GrantClass:  tool.GrantClassCommandStart,
			GrantTarget: command,
		}}
	}
	return requirement
}

// declaredDeltas converts the structured declaration into command-backed
// requirements. Each carries the sandbox enforcement class and grant-target
// encoding for its delta; broad deltas (host-less network, host filesystem)
// bind to the exact command and say so in their display text.
func declaredDeltas(command, dir string, access accessDecl) ([]tool.Requirement, error) {
	var requirements []tool.Requirement
	for _, declared := range access.Network {
		requirement, err := networkRequirement(command, declared)
		if err != nil {
			return nil, err
		}
		requirements = append(requirements, requirement)
	}
	for _, declared := range access.Read {
		requirement, err := filesystemRequirement(command, dir, declared, false)
		if err != nil {
			return nil, err
		}
		requirements = append(requirements, requirement)
	}
	for _, declared := range access.Write {
		requirement, err := filesystemRequirement(command, dir, declared, true)
		if err != nil {
			return nil, err
		}
		requirements = append(requirements, requirement)
	}
	return requirements, nil
}

// networkRequirement builds one network delta. The sandbox egress proxy
// enforces TCP only, so a non-tcp transport fails preparation rather than
// producing an unenforceable claim.
func networkRequirement(command string, declared networkDecl) (tool.Requirement, error) {
	if declared.Transport != "" && declared.Transport != "tcp" {
		return tool.Requirement{}, prepareFail("network transport must be tcp")
	}
	if declared.Port < 1 || declared.Port > 65535 {
		return tool.Requirement{}, prepareFail("network port must be in 1..65535")
	}
	if declared.Host == "" {
		// Broad, exact-command-bound egress: the display text makes the
		// broader destination scope explicit (spec, "Permission file model").
		target := "tcp:*:" + strconv.Itoa(declared.Port)
		match := permission.BroadEgressMatch(command, target)
		description := "broad network egress (any host, tcp port " + strconv.Itoa(declared.Port) + ") bound to `" + command + "`"
		return tool.Requirement{
			Kind:        permission.CapabilityNetwork,
			Match:       match,
			Description: description,
			GrantClass:  permission.ClassNetworkBroad,
			GrantTarget: target,
			Candidates: []tool.RuleCandidate{{
				Kind:        permission.CapabilityNetwork,
				Match:       match,
				Description: description,
				GrantClass:  permission.ClassNetworkBroad,
				GrantTarget: target,
			}},
		}, nil
	}
	host, err := normalizeDeclaredHost(declared.Host)
	if err != nil {
		return tool.Requirement{}, err
	}
	match := permission.NetworkTargetMatch("tcp", host, declared.Port)
	target := "tcp:" + net.JoinHostPort(host, strconv.Itoa(declared.Port))
	description := "network egress to " + host + ":" + strconv.Itoa(declared.Port) + " (tcp)"
	return tool.Requirement{
		Kind:        permission.CapabilityNetwork,
		Match:       match,
		Description: description,
		GrantClass:  permission.GrantClassNetworkProxyTarget,
		GrantTarget: target,
		Candidates: []tool.RuleCandidate{{
			Kind:        permission.CapabilityNetwork,
			Match:       match,
			Description: description,
			GrantClass:  permission.GrantClassNetworkProxyTarget,
			GrantTarget: target,
		}},
	}, nil
}

// filesystemRequirement builds one declared filesystem delta in read or write
// direction.
func filesystemRequirement(command, dir string, declared fsDecl, write bool) (tool.Requirement, error) {
	kind := permission.CapabilityFilesystemRead
	verb := "read"
	if write {
		kind = permission.CapabilityFilesystemWrite
		verb = "write"
	}
	pick := func(read, writeClass string) string {
		if write {
			return writeClass
		}
		return read
	}
	switch declared.Scope {
	case "path":
		abs, err := canonicalDeclaredPath(dir, declared.Path)
		if err != nil {
			return tool.Requirement{}, err
		}
		return grantedRequirement(kind, abs, abs, verb+" "+abs,
			pick(permission.ClassFilesystemPathRead, permission.ClassFilesystemPathWrite), abs), nil
	case "tree":
		root, err := canonicalDeclaredPath(dir, declared.Path)
		if err != nil {
			return tool.Requirement{}, err
		}
		return grantedRequirement(kind, "tree:"+root, permission.TreeMatch(root), verb+" files under "+root,
			pick(permission.ClassFilesystemTreeRead, permission.ClassFilesystemTreeWrite), root), nil
	case "host":
		if declared.Path != "" {
			return tool.Requirement{}, prepareFail("host scope takes no path (it is explicitly broad)")
		}
		description := verb + " any host path, bound to `" + command + "`"
		return grantedRequirement(kind, "host:*", permission.HostAccessMatch(command), description,
			pick(permission.ClassFilesystemHostRead, permission.ClassFilesystemHostWrite), "host:*"), nil
	default:
		return tool.Requirement{}, prepareFail("filesystem scope must be path, tree, or host")
	}
}

// grantedRequirement assembles one command-backed requirement whose single
// candidate mirrors the requirement (same kind, match, and grant pair).
func grantedRequirement(kind, scope, match, description, grantClass, grantTarget string) tool.Requirement {
	return tool.Requirement{
		Kind:        kind,
		Scope:       scope,
		Match:       match,
		Description: description,
		GrantClass:  grantClass,
		GrantTarget: grantTarget,
		Candidates: []tool.RuleCandidate{{
			Kind:        kind,
			Match:       match,
			Description: description,
			GrantClass:  grantClass,
			GrantTarget: grantTarget,
		}},
	}
}

// normalizeDeclaredHost lowercases, strips the trailing dot, and validates a
// declared hostname or IP literal against the shape both the durable network
// match and the sandbox grant-target encoding accept.
func normalizeDeclaredHost(raw string) (string, error) {
	host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(raw), "."))
	if host == "" {
		return "", prepareFail("network host must not be empty")
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr.String(), nil
	}
	for _, r := range host {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' && r != '.' {
			return "", prepareFail("network host %q is not a normalized hostname or IP", raw)
		}
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", prepareFail("network host %q is not a normalized hostname or IP", raw)
		}
	}
	return host, nil
}

// canonicalDeclaredPath resolves one declared path or tree root to a canonical
// absolute path: relative values resolve against the confined spawn directory
// (the command's cwd), everything is lexically cleaned, and control bytes are
// rejected. Declared paths may point OUTSIDE the workspace — that is the
// point of the declaration — so no containment is applied here; the profile
// decides access and the sandbox enforces the grant.
func canonicalDeclaredPath(dir, declared string) (string, error) {
	if declared == "" {
		return "", prepareFail("a filesystem declaration requires a path")
	}
	if strings.ContainsFunc(declared, func(r rune) bool { return r < 0x20 || r == 0x7f }) || !utf8.ValidString(declared) {
		return "", prepareFail("declared path contains invalid bytes")
	}
	path := declared
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	return filepath.Clean(path), nil
}

// dedupeRequirements drops exact duplicate declarations so a repeated entry
// cannot invalidate the request.
func dedupeRequirements(requirements []tool.Requirement) []tool.Requirement {
	seen := make(map[string]struct{}, len(requirements))
	out := requirements[:0]
	for _, requirement := range requirements {
		key := strings.Join([]string{requirement.Kind, requirement.Scope, requirement.Match, requirement.GrantClass, requirement.GrantTarget}, "\x00")
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, requirement)
	}
	return out
}

// compile-time assertion: BashTool implements the preparation capability.
var _ tool.CallPreparer = (*BashTool)(nil)
