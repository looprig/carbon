package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/sandbox"
	"github.com/looprig/tools/skill"
)

// access.go owns Carbon's product access policy: the three named profiles built
// directly from the reusable sandbox.Profile API, the product-owned access source
// for the consumer-bound
// tool.invoke/context.load kinds, and the secret-free durable access digest that
// a restore compares. The profile names, combinations, and product kinds live
// HERE, never in sandbox, harness, or tools; reusable modules ship no named
// presets. Assembly (Task 5.2) consumes these surfaces; this file constructs
// them.

// productAccessVersion is Carbon's structural access ABI version. It is fixed at
// 1 and stated explicitly (never iota-derived) so an independent module cannot
// reorder it. It must equal gate.CurrentAccessVersion; TestProductAccessVersion
// pins both directions.
const productAccessVersion uint16 = 1

// capabilityToolInvoke is the consumer-bound requirement kind every external MCP
// tool emits. It has no sandbox meaning and is routed to the product access
// source, never to a sandbox profile. It MUST match
// github.com/looprig/mcp/pkg/harness.CapabilityToolInvoke ("tool.invoke");
// Carbon does not depend on the mcp module, so the string is the structural
// contract and TestProductAccessSourceRouting pins it.
const capabilityToolInvoke = "tool.invoke"

// AccessProfile is the Carbon-selected, session-fixed product access profile.
type AccessProfile string

// The three product profile names. These names and their capability
// combinations are Carbon product behavior; sandbox provides no such presets.
const (
	AccessReadOnly   AccessProfile = "readonly"
	AccessTrusted    AccessProfile = "trusted"
	AccessUnconfined AccessProfile = "unconfined"
)

// DefaultAccessProfile is the command default. Trusted gives Carbon ordinary
// workspace-write, host-read, and network access while retaining a gate for
// host writes and keeping execution sandboxed.
const DefaultAccessProfile = AccessTrusted

// ParseAccessProfile validates an access-profile name at the CLI boundary. It
// accepts EXACTLY the three known names and reports whether the name was valid,
// so unknown input fails closed before the Rig is constructed rather than
// silently defaulting to a surprising authority.
func ParseAccessProfile(name string) (AccessProfile, bool) {
	switch AccessProfile(name) {
	case AccessReadOnly, AccessTrusted, AccessUnconfined:
		return AccessProfile(name), true
	default:
		return "", false
	}
}

// normalizeAccessProfile applies the product default and then validates the
// selected profile through the same parser used at the CLI boundary. Every
// composition path that consumes Config.AccessProfile uses this helper so an
// empty value cannot drift from the command convention.
func normalizeAccessProfile(profile AccessProfile) (AccessProfile, error) {
	if profile == "" {
		profile = DefaultAccessProfile
	}
	normalized, ok := ParseAccessProfile(string(profile))
	if !ok {
		return "", fmt.Errorf("carbon: unknown access profile %q", profile)
	}
	return normalized, nil
}

// carbonProfile constructs the immutable sandbox profile for the selected
// product profile over the canonical workspace root. It uses direct construction
// (not a generic registry) and validates exactly the three names. Unconfined
// carries the explicit AckUnconfined so sandbox validation accepts direct host
// execution; any other name is rejected.
func carbonProfile(name AccessProfile, workspace string) (*sandbox.Profile, error) {
	config := sandbox.ProfileConfig{
		WorkspaceRoot: workspace,
		Home:          sandbox.IsolatedHome,
		Isolation:     sandbox.Sandboxed,
	}

	switch name {
	case AccessReadOnly:
		config.WorkspaceRead = sandbox.Allow
		config.WorkspaceWrite = sandbox.Deny
		config.HostRead = sandbox.Deny
		config.HostWrite = sandbox.Deny
		config.Network = sandbox.Deny
		config.Command = sandbox.Gated
	case AccessTrusted:
		config.WorkspaceRead = sandbox.Allow
		config.WorkspaceWrite = sandbox.Allow
		config.HostRead = sandbox.Allow
		config.HostWrite = sandbox.Gated
		config.Network = sandbox.Allow
		config.Command = sandbox.Allow
	case AccessUnconfined:
		config.WorkspaceRead = sandbox.Allow
		config.WorkspaceWrite = sandbox.Allow
		config.HostRead = sandbox.Allow
		config.HostWrite = sandbox.Allow
		config.Network = sandbox.Allow
		config.Command = sandbox.Allow
		config.Home = sandbox.RealHome
		config.Isolation = sandbox.Unconfined
		config.AckUnconfined = true
	default:
		return nil, fmt.Errorf("carbon: unknown access profile %q", name)
	}

	return sandbox.NewProfile(config)
}

// productAccessSource is Carbon's small, immutable access source for the two
// consumer-bound requirement kinds that carry no sandbox meaning:
//
//   - tool.invoke   (external MCP tools), scoped to a stable tool identity;
//   - context.load  (skill loads), scoped to a canonical skill identity.
//
// Both are always Gated: one combined approval or one persisted workspace rule
// admits the exact identity, and neither is ever mapped to sandbox command
// authority. It is stateless and profile-independent — the product policy for
// these kinds does not change with the selected sandbox profile.
type productAccessSource struct{}

// newProductAccessSource returns the immutable product access source. Assembly
// binds it to the tool.invoke and context.load kinds.
func newProductAccessSource() productAccessSource { return productAccessSource{} }

// AccessVersion reports the fixed product access ABI version (1).
func (productAccessSource) AccessVersion() uint16 { return productAccessVersion }

// AccessFor resolves the product-bound kinds. tool.invoke and context.load are
// Gated when scoped to a non-empty, trimmed identity; every other kind, and a
// malformed scope, fails closed with an error rather than an indistinguishable
// Deny.
func (productAccessSource) AccessFor(kind, scope string) (uint8, error) {
	switch kind {
	case capabilityToolInvoke, skill.CapabilityContextLoad:
		if scope == "" || strings.TrimSpace(scope) != scope {
			return gate.AccessDeny, fmt.Errorf("carbon: %s requires a non-empty canonical scope", kind)
		}
		return gate.AccessGated, nil
	default:
		return gate.AccessDeny, fmt.Errorf("carbon: product access source has no kind %q", kind)
	}
}

// Compile-time assertions that both access sources Carbon binds satisfy the
// generic gate seam using only built-in Go types (no harness import in sandbox).
var (
	_ gate.AccessSource = (*sandbox.Profile)(nil)
	_ gate.AccessSource = productAccessSource{}
)

// accessConfigDigest is the secret-free durable identity of a session's access
// configuration. It folds the access ABI version, selected profile name, the
// complete normalized Carbon profile, and the non-secret egress route identity
// and declared guarantees. Upstream proxy credentials never enter it: the route
// contributes only its Fingerprint and guarantee bits.
func accessConfigDigest(selected AccessProfile, profile *sandbox.Profile, route sandbox.EgressRoute) string {
	payload, _ := json.Marshal(struct {
		Version          uint16 `json:"version"`
		Selected         string `json:"selected"`
		Carbon           string `json:"carbon"`
		Route            string `json:"route"`
		TargetGuarantee  bool   `json:"target_guarantee"`
		AddressGuarantee bool   `json:"address_guarantee"`
	}{
		Version:          productAccessVersion,
		Selected:         string(selected),
		Carbon:           profile.Fingerprint(),
		Route:            route.Fingerprint(),
		TargetGuarantee:  route.TargetGuarantee(),
		AddressGuarantee: route.AddressGuarantee(),
	})
	digest := sha256.Sum256(payload)
	return "carbon-access-v1:" + hex.EncodeToString(digest[:])
}
