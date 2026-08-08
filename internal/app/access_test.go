package app

import (
	"path/filepath"
	"testing"

	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/sandbox"
	"github.com/looprig/tools/skill"
)

// canonicalTempDir returns a canonical (symlink-resolved) temporary directory so
// a profile built on it and a filesystem query against it agree on the same
// normalized root (macOS TempDir lives under a /var -> /private/var symlink).
func canonicalTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	return filepath.Clean(dir)
}

// accessAt is a readable AccessFor wrapper that fails the test on error.
func accessAt(t *testing.T, p *sandbox.Profile, kind, scope string) uint8 {
	t.Helper()
	value, err := p.AccessFor(kind, scope)
	if err != nil {
		t.Fatalf("AccessFor(%q, %q) error = %v", kind, scope, err)
	}
	return value
}

// TestProductAccessVersion pins the product access ABI to 1 in both directions so
// it can never drift from the gate contract or be reordered.
func TestProductAccessVersion(t *testing.T) {
	t.Parallel()
	if productAccessVersion != 1 {
		t.Fatalf("productAccessVersion = %d, want 1", productAccessVersion)
	}
	if uint16(gate.CurrentAccessVersion) != productAccessVersion {
		t.Fatalf("gate.CurrentAccessVersion = %d, want %d", gate.CurrentAccessVersion, productAccessVersion)
	}
	if got := newProductAccessSource().AccessVersion(); got != productAccessVersion {
		t.Fatalf("productAccessSource.AccessVersion() = %d, want %d", got, productAccessVersion)
	}
}

// TestParseAccessProfile pins exactly the three valid names, the default, and
// fail-closed rejection of anything else.
func TestParseAccessProfile(t *testing.T) {
	t.Parallel()
	if DefaultAccessProfile != AccessReadOnly {
		t.Fatalf("DefaultAccessProfile = %q, want %q", DefaultAccessProfile, AccessReadOnly)
	}
	valid := []AccessProfile{AccessReadOnly, AccessTrusted, AccessUnconfined}
	for _, name := range valid {
		got, ok := ParseAccessProfile(string(name))
		if !ok || got != name {
			t.Errorf("ParseAccessProfile(%q) = %q,%v; want %q,true", name, got, ok, name)
		}
	}
	for _, bad := range []string{"", "READONLY", "write", "zerotrust", "trusted ", "unknown"} {
		if got, ok := ParseAccessProfile(bad); ok {
			t.Errorf("ParseAccessProfile(%q) = %q,true; want _,false", bad, got)
		}
	}
}

// TestCoderigProfileExactValues asserts every profile's four sandbox access
// fields against the spec table AND pins the complete normalization (HOME,
// isolation, acknowledgement, guarantees) via fingerprint equality with an
// independently spelled reference profile.
func TestCoderigProfileExactValues(t *testing.T) {
	t.Parallel()
	ws := canonicalTempDir(t)
	const (
		deny  = uint8(sandbox.Deny)
		gated = uint8(sandbox.Gated)
		allow = uint8(sandbox.Allow)
	)

	tests := []struct {
		name    AccessProfile
		wsRead  uint8
		wsWrite uint8
		hRead   uint8
		hWrite  uint8
		network uint8
		command uint8
		ref     sandbox.ProfileConfig
	}{
		{
			name: AccessReadOnly, wsRead: allow, wsWrite: deny, hRead: deny, hWrite: deny, network: deny, command: gated,
			ref: sandbox.ProfileConfig{
				WorkspaceRoot: ws, WorkspaceRead: sandbox.Allow, WorkspaceWrite: sandbox.Deny,
				HostRead: sandbox.Deny, HostWrite: sandbox.Deny, Network: sandbox.Deny, Command: sandbox.Gated,
				Home: sandbox.IsolatedHome, Isolation: sandbox.Sandboxed,
			},
		},
		{
			name: AccessTrusted, wsRead: allow, wsWrite: allow, hRead: allow, hWrite: gated, network: allow, command: allow,
			ref: sandbox.ProfileConfig{
				WorkspaceRoot: ws, WorkspaceRead: sandbox.Allow, WorkspaceWrite: sandbox.Allow,
				HostRead: sandbox.Allow, HostWrite: sandbox.Gated, Network: sandbox.Allow, Command: sandbox.Allow,
				Home: sandbox.IsolatedHome, Isolation: sandbox.Sandboxed,
			},
		},
		{
			name: AccessUnconfined, wsRead: allow, wsWrite: allow, hRead: allow, hWrite: allow, network: allow, command: allow,
			ref: sandbox.ProfileConfig{
				WorkspaceRoot: ws, WorkspaceRead: sandbox.Allow, WorkspaceWrite: sandbox.Allow,
				HostRead: sandbox.Allow, HostWrite: sandbox.Allow, Network: sandbox.Allow, Command: sandbox.Allow,
				Home: sandbox.RealHome, Isolation: sandbox.Unconfined, AckUnconfined: true,
			},
		},
	}
	for _, tt := range tests {
		t.Run(string(tt.name), func(t *testing.T) {
			t.Parallel()
			p, err := coderigProfile(tt.name, ws)
			if err != nil {
				t.Fatalf("coderigProfile(%q) error = %v", tt.name, err)
			}
			if got := accessAt(t, p, "filesystem.read", ws); got != tt.wsRead {
				t.Errorf("workspace read = %d, want %d", got, tt.wsRead)
			}
			if got := accessAt(t, p, "filesystem.write", ws); got != tt.wsWrite {
				t.Errorf("workspace write = %d, want %d", got, tt.wsWrite)
			}
			if got := accessAt(t, p, "filesystem.read", "host:*"); got != tt.hRead {
				t.Errorf("host read = %d, want %d", got, tt.hRead)
			}
			if got := accessAt(t, p, "filesystem.write", "host:*"); got != tt.hWrite {
				t.Errorf("host write = %d, want %d", got, tt.hWrite)
			}
			if got := accessAt(t, p, "network", ""); got != tt.network {
				t.Errorf("network = %d, want %d", got, tt.network)
			}
			if got := accessAt(t, p, "command.execute", ""); got != tt.command {
				t.Errorf("command = %d, want %d", got, tt.command)
			}
			ref, err := sandbox.NewProfile(tt.ref)
			if err != nil {
				t.Fatalf("reference NewProfile error = %v", err)
			}
			if p.Fingerprint() != ref.Fingerprint() {
				t.Errorf("normalized profile fingerprint mismatch:\n got=%s\nwant=%s", p.Fingerprint(), ref.Fingerprint())
			}
		})
	}
}

// TestUnconfinedRequiresAck proves the acknowledgement CodeRig sets on the
// unconfined profile is load-bearing: the same unconfined configuration without
// it is rejected by sandbox validation.
func TestUnconfinedRequiresAck(t *testing.T) {
	t.Parallel()
	ws := canonicalTempDir(t)

	if _, err := coderigProfile(AccessUnconfined, ws); err != nil {
		t.Fatalf("coderigProfile(unconfined) error = %v", err)
	}
	_, err := sandbox.NewProfile(sandbox.ProfileConfig{
		WorkspaceRoot: ws, WorkspaceRead: sandbox.Allow, WorkspaceWrite: sandbox.Allow,
		HostRead: sandbox.Allow, HostWrite: sandbox.Allow, Network: sandbox.Allow, Command: sandbox.Allow,
		Home: sandbox.RealHome, Isolation: sandbox.Unconfined, // no AckUnconfined
	})
	if err == nil {
		t.Fatal("unconfined profile without acknowledgement was accepted; want rejection")
	}
}

// TestCoderigProfileRejectsUnknown fails closed on a name outside the three.
func TestCoderigProfileRejectsUnknown(t *testing.T) {
	t.Parallel()
	ws := canonicalTempDir(t)
	if _, err := coderigProfile(AccessProfile("write"), ws); err == nil {
		t.Fatal("coderigProfile(\"write\") = nil error; want rejection")
	}
}

// TestProductAccessSourceRouting proves the product source resolves the two
// consumer-bound kinds as Gated with a valid scope, and fails closed for an
// empty/untrimmed scope or an unknown kind.
func TestProductAccessSourceRouting(t *testing.T) {
	t.Parallel()
	source := newProductAccessSource()

	// The two product kinds resolve Gated with a canonical scope.
	gatedCases := []struct {
		kind  string
		scope string
	}{
		{capabilityToolInvoke, "mcp:github:search_issues"},
		{skill.CapabilityContextLoad, skill.EmbeddedSkillIdentity("generic")},
		{skill.CapabilityContextLoad, skill.WorkspaceSkillIdentity("local")},
	}
	for _, tc := range gatedCases {
		got, err := source.AccessFor(tc.kind, tc.scope)
		if err != nil {
			t.Errorf("AccessFor(%q,%q) error = %v", tc.kind, tc.scope, err)
		}
		if got != gate.AccessGated {
			t.Errorf("AccessFor(%q,%q) = %d, want Gated(%d)", tc.kind, tc.scope, got, gate.AccessGated)
		}
	}

	// Contract pin: the bound kind strings are exactly the cross-module kinds.
	if capabilityToolInvoke != "tool.invoke" {
		t.Errorf("capabilityToolInvoke = %q, want tool.invoke", capabilityToolInvoke)
	}
	if skill.CapabilityContextLoad != "context.load" {
		t.Errorf("skill.CapabilityContextLoad = %q, want context.load", skill.CapabilityContextLoad)
	}

	// Empty and untrimmed scopes fail closed.
	for _, scope := range []string{"", "  ", " mcp:x:y", "mcp:x:y "} {
		if _, err := source.AccessFor(capabilityToolInvoke, scope); err == nil {
			t.Errorf("AccessFor(tool.invoke, %q) = nil error; want fail-closed", scope)
		}
	}

	// A sandbox kind (or any unknown kind) is not owned by the product source.
	for _, kind := range []string{"command.execute", "filesystem.read", "network", "unknown"} {
		if _, err := source.AccessFor(kind, "scope"); err == nil {
			t.Errorf("AccessFor(%q, ...) = nil error; want fail-closed", kind)
		}
	}
}

// TestAccessConfigDigestDrift proves each of the selected profile, the Generic
// profile, and the egress route contributes to the durable access digest, so
// any of those changes invalidates a restore; identical inputs are stable.
func TestAccessConfigDigestDrift(t *testing.T) {
	t.Parallel()
	ws := canonicalTempDir(t)

	readOnly, err := coderigProfile(AccessReadOnly, ws)
	if err != nil {
		t.Fatalf("coderigProfile(readonly): %v", err)
	}
	trusted, err := coderigProfile(AccessTrusted, ws)
	if err != nil {
		t.Fatalf("coderigProfile(trusted): %v", err)
	}
	direct, err := sandbox.NewDirectEgressRoute()
	if err != nil {
		t.Fatalf("NewDirectEgressRoute: %v", err)
	}
	upstream, err := sandbox.NewUpstreamEgressRoute("http://proxy.example:8080", false)
	if err != nil {
		t.Fatalf("NewUpstreamEgressRoute: %v", err)
	}

	base := accessConfigDigest(AccessReadOnly, readOnly, direct)

	if got := accessConfigDigest(AccessReadOnly, readOnly, direct); got != base {
		t.Errorf("identical inputs produced different digests:\n%s\n%s", got, base)
	}
	if got := accessConfigDigest(AccessTrusted, trusted, direct); got == base {
		t.Error("selected/Generic profile change did not invalidate the digest")
	}
	if got := accessConfigDigest(AccessReadOnly, readOnly, upstream); got == base {
		t.Error("egress route change did not invalidate the digest")
	}
}
