// Package permission implements the single hardened workspace permission
// store defined by the access-profile specification.
//
// The package owns three things and nothing else:
//
//   - the schema-version-2 capability rule model and its strict JSON codec;
//   - hardened loading of one explicit permission file (interactive
//     read/write or headless read-only); and
//   - atomic, interprocess-safe persistence of the exact displayed allow
//     candidates after "Approve always for this workspace".
//
// It does not parse tool arguments, decide Deny/Gated/Allow, or discover
// HOME. The harness gate evaluator consumes the Store structurally as its
// RuleMatcher and RuleWriter.
package permission

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"
)

// SchemaVersion is the only supported permission-file schema version.
const SchemaVersion = 2

// NormalizationVersion is the only supported match-normalization version. A
// file recording a different normalization version is unsupported: its rules
// were produced by an incompatible normalizer and must not match.
const NormalizationVersion = 1

// Effect is the decision a rule contributes. Deny always beats allow.
type Effect string

// The two rule effects.
const (
	EffectAllow Effect = "allow"
	EffectDeny  Effect = "deny"
)

// Normalized capability kinds a rule may control. Filesystem and network
// kinds mirror the sandbox profile kinds; command execution reuses the
// harness constant value.
const (
	CapabilityCommandExecute  = "command.execute"
	CapabilityNetwork         = "network"
	CapabilityFilesystemRead  = "filesystem.read"
	CapabilityFilesystemWrite = "filesystem.write"
)

// Enforcement classes understood by schema version 2. Filesystem and broad
// network classes are aligned with the sandbox grant-class identifiers they
// correspond to; the command-invoke classes are gate-decision classes and the
// grant minted after a match is always the exact-command command.start.v1.
const (
	// ClassCommandInvoke matches one exact normalized command.
	ClassCommandInvoke = "command.invoke.v1"
	// ClassCommandInvokeWildcard is the stored representation of Bash(*).
	// It satisfies only the command-execution decision.
	ClassCommandInvokeWildcard = "command.invoke.wildcard.v1"
	// ClassCommandInvokeFamily is the token-prefix family (Bash(git log:*)).
	// This package stores and validates its shape; the token-aware segment
	// matcher plugs into matchesRequirement (match.go) without reshaping the
	// store.
	ClassCommandInvokeFamily = "command.invoke.shell-segment-glob.v1"
	// ClassNetworkTarget is target-scoped network access.
	ClassNetworkTarget = "network.target.v1"
	// ClassNetworkBroad is exact-command-bound broad egress (sandbox
	// grant class network.broad.v1).
	ClassNetworkBroad = "network.broad.v1"
	// Filesystem classes (sandbox grant-class identifiers).
	ClassFilesystemPathRead  = "filesystem.path.read.v1"
	ClassFilesystemPathWrite = "filesystem.path.write.v1"
	ClassFilesystemTreeRead  = "filesystem.tree.read.v1"
	ClassFilesystemTreeWrite = "filesystem.tree.write.v1"
	ClassFilesystemHostRead  = "filesystem.host.read.v1"
	ClassFilesystemHostWrite = "filesystem.host.write.v1"
)

// Grant classes that appear on prepared requirements and persisted
// candidates. GrantClassCommandStart mirrors harness
// tool.GrantClassCommandStart; GrantClassNetworkProxyTarget is the sandbox
// target-scoped egress grant class.
const (
	GrantClassCommandStart       = "command.start.v1"
	GrantClassNetworkProxyTarget = "network.proxy-target.v1"
)

// Rule is one normalized capability record. Exactly the fields belonging to
// its enforcement class are populated; the strict codec rejects any other
// combination so a wildcard or family command record can never carry a
// filesystem or network delta.
type Rule struct {
	Effect     Effect
	Capability string
	Class      string

	// ClassCommandInvoke: the exact normalized command.
	// ClassNetworkBroad, ClassFilesystemHost{Read,Write}: the exact
	// normalized command the broad delta is bound to.
	Command string

	// ClassCommandInvokeFamily.
	Tokens            []string
	TrailingArguments bool

	// ClassNetworkTarget. Host is required; Transport and Port are optional
	// constraints, and omitting one deliberately broadens the rule.
	Transport string
	Host      string
	Port      int

	// ClassNetworkBroad: the backend enforcement target (for example a port
	// class) the broad delta was approved for.
	Target string

	// ClassFilesystemPath{Read,Write}: one canonical absolute path.
	Path string

	// ClassFilesystemTree{Read,Write}: one canonical absolute root.
	Root string
}

// RuleError reports one invalid rule at load or write time. Loading a file
// containing an invalid rule fails: silently dropping an explicit consumer
// record could widen (dropped deny) or misrepresent (dropped allow) policy.
type RuleError struct {
	Index  int // position in the file or candidate batch, -1 when unknown
	Reason string
}

func (e *RuleError) Error() string {
	return fmt.Sprintf("permission: invalid rule %d: %s", e.Index, e.Reason)
}

// classesByCapability pins which enforcement classes may control each
// capability. Any other pairing is rejected at load and at write.
var classesByCapability = map[string]map[string]bool{
	CapabilityCommandExecute: {
		ClassCommandInvoke:         true,
		ClassCommandInvokeWildcard: true,
		ClassCommandInvokeFamily:   true,
	},
	CapabilityNetwork: {
		ClassNetworkTarget: true,
		ClassNetworkBroad:  true,
	},
	CapabilityFilesystemRead: {
		ClassFilesystemPathRead: true,
		ClassFilesystemTreeRead: true,
		ClassFilesystemHostRead: true,
	},
	CapabilityFilesystemWrite: {
		ClassFilesystemPathWrite: true,
		ClassFilesystemTreeWrite: true,
		ClassFilesystemHostWrite: true,
	},
}

// validate rejects every malformed rule. index is used for error reporting
// only.
func (r Rule) validate(index int) error {
	fail := func(reason string) error { return &RuleError{Index: index, Reason: reason} }

	if r.Effect != EffectAllow && r.Effect != EffectDeny {
		return fail("effect must be allow or deny")
	}
	classes, ok := classesByCapability[r.Capability]
	if !ok {
		return fail("unknown capability " + strconv.Quote(r.Capability))
	}
	if !classes[r.Class] {
		return fail("enforcement class " + strconv.Quote(r.Class) + " is not valid for capability " + strconv.Quote(r.Capability))
	}

	switch r.Class {
	case ClassCommandInvoke:
		if !validCommand(r.Command) {
			return fail("exact command rule requires a normalized command")
		}
	case ClassCommandInvokeWildcard:
		// No fields. The strict codec guarantees the match object is empty,
		// so a wildcard record cannot carry another capability delta.
	case ClassCommandInvokeFamily:
		if err := validateFamilyTokens(r.Tokens); err != nil {
			return fail(err.Error())
		}
		if !r.TrailingArguments {
			return fail("family rule requires trailing_arguments true")
		}
	case ClassNetworkTarget:
		if !validHost(r.Host) {
			return fail("network target rule requires a normalized host")
		}
		if r.Transport != "" && r.Transport != "tcp" && r.Transport != "udp" {
			return fail("network transport must be tcp or udp")
		}
		if r.Port < 0 || r.Port > 65535 {
			return fail("network port out of range")
		}
	case ClassNetworkBroad:
		if !validCommand(r.Command) {
			return fail("broad egress rule requires its bound normalized command")
		}
		if !validToken(r.Target) {
			return fail("broad egress rule requires a backend enforcement target")
		}
	case ClassFilesystemPathRead, ClassFilesystemPathWrite:
		if !validCanonicalPath(r.Path) {
			return fail("path rule requires one canonical absolute path")
		}
	case ClassFilesystemTreeRead, ClassFilesystemTreeWrite:
		if !validCanonicalPath(r.Root) {
			return fail("tree rule requires one canonical absolute root")
		}
	case ClassFilesystemHostRead, ClassFilesystemHostWrite:
		if !validCommand(r.Command) {
			return fail("host filesystem rule requires its bound normalized command")
		}
	}
	return nil
}

// identity returns a canonical comparison key so merges deduplicate
// identical records without a second encoding scheme.
func (r Rule) identity() string {
	parts := []string{
		string(r.Effect), r.Capability, r.Class, r.Command,
		strings.Join(r.Tokens, "\x1f"), strconv.FormatBool(r.TrailingArguments),
		r.Transport, r.Host, strconv.Itoa(r.Port), r.Target, r.Path, r.Root,
	}
	return strings.Join(parts, "\x00")
}

// validCommand accepts a normalized command string: non-empty, no
// surrounding space, no control bytes.
func validCommand(command string) bool {
	if command == "" || strings.TrimSpace(command) != command {
		return false
	}
	return !containsControl(command)
}

// validToken accepts one non-empty token with no whitespace or control
// bytes.
func validToken(token string) bool {
	if token == "" || strings.ContainsAny(token, " \t") {
		return false
	}
	return !containsControl(token)
}

// familyTokenUnsafe lists bytes that make a family token ambiguous or
// shell-active. The conservative set fails closed; the token-aware family
// matcher may only narrow it further.
// #nosec G101 -- shell metacharacter denylist, not a credential.
const familyTokenUnsafe = "*?[]{}()<>|&;$`\\\"'~#!"

// validateFamilyTokens applies the conservative token shape shared by load
// and write. The token-aware matcher (shell-segment parsing) refines
// semantics, not this storage validation.
func validateFamilyTokens(tokens []string) error {
	if len(tokens) == 0 {
		return fmt.Errorf("family rule requires at least one token")
	}
	for _, token := range tokens {
		if !validToken(token) || strings.ContainsAny(token, familyTokenUnsafe) {
			return fmt.Errorf("family token %s is not a plain literal token", strconv.Quote(token))
		}
	}
	return nil
}

// validHost accepts a normalized lowercase hostname or IP literal.
func validHost(host string) bool {
	if host == "" || strings.ContainsAny(host, " \t/@") || containsControl(host) {
		return false
	}
	if host != strings.ToLower(host) {
		return false
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	return !strings.Contains(host, ":")
}

// validCanonicalPath accepts one canonical absolute path: rooted, cleaned,
// no traversal, no control bytes.
func validCanonicalPath(path string) bool {
	if path == "" || path[0] != '/' || containsControl(path) {
		return false
	}
	if path != "/" && strings.HasSuffix(path, "/") {
		return false
	}
	for _, segment := range strings.Split(path[1:], "/") {
		if path != "/" && (segment == "" || segment == "." || segment == "..") {
			return false
		}
	}
	return true
}

func containsControl(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

// Canonical requirement-match encodings.
//
// The store matches durable rules against tool.Requirement.Match strings.
// Preparation (file/context tools and Bash/Fetch/WebSearch) must build those
// strings with the helpers below; they are the contract between preparation
// and matching:
//
//	command.execute            the exact normalized command string
//	network, target-scoped     NetworkTargetMatch: "tcp://github.com:443"
//	network, command-bound     BroadEgressMatch: {"command":...,"target":...}
//	filesystem, exact path     the canonical absolute path
//	filesystem, one tree       TreeMatch: "tree:/canonical/root"
//	filesystem, command-bound  HostAccessMatch: {"command":...}

// NetworkTargetMatch builds the canonical durable match string for one
// normalized network target.
func NetworkTargetMatch(transport, host string, port int) string {
	return transport + "://" + host + ":" + strconv.Itoa(port)
}

// parseNetworkTargetMatch parses the NetworkTargetMatch encoding.
func parseNetworkTargetMatch(match string) (transport, host string, port int, ok bool) {
	transport, rest, found := strings.Cut(match, "://")
	if !found || transport == "" {
		return "", "", 0, false
	}
	idx := strings.LastIndexByte(rest, ':')
	if idx <= 0 {
		return "", "", 0, false
	}
	host = rest[:idx]
	port, err := strconv.Atoi(rest[idx+1:])
	if err != nil || port < 1 || port > 65535 || !validHost(host) {
		return "", "", 0, false
	}
	return transport, host, port, true
}

// commandBoundMatch is the JSON composite used by the command-bound broad
// classes. Struct-field order makes the encoding deterministic.
type commandBoundMatch struct {
	Command string `json:"command"`
	Target  string `json:"target,omitempty"`
}

// BroadEgressMatch builds the canonical durable match string for one
// exact-command-bound broad egress delta.
func BroadEgressMatch(command, target string) string {
	encoded, _ := json.Marshal(commandBoundMatch{Command: command, Target: target})
	return string(encoded)
}

// HostAccessMatch builds the canonical durable match string for one
// exact-command-bound broad host filesystem delta.
func HostAccessMatch(command string) string {
	encoded, _ := json.Marshal(commandBoundMatch{Command: command})
	return string(encoded)
}

// TreeMatch builds the canonical durable match string for one configured
// tree root.
func TreeMatch(root string) string { return "tree:" + root }

// parseCommandBoundMatch parses BroadEgressMatch/HostAccessMatch encodings.
func parseCommandBoundMatch(match string) (commandBoundMatch, bool) {
	decoder := json.NewDecoder(strings.NewReader(match))
	decoder.DisallowUnknownFields()
	var parsed commandBoundMatch
	if err := decoder.Decode(&parsed); err != nil {
		return commandBoundMatch{}, false
	}
	if decoder.More() {
		return commandBoundMatch{}, false
	}
	return parsed, true
}

// sortDiagnostics keeps diagnostic output deterministic across merges.
func sortDiagnostics(diagnostics []Diagnostic) {
	sort.SliceStable(diagnostics, func(i, j int) bool {
		return diagnostics[i].RuleIndex < diagnostics[j].RuleIndex
	})
}
