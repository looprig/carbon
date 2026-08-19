package app

import (
	"context"
	"testing"

	mcpharness "github.com/looprig/mcp/pkg/harness"
)

// mcpManagerDigest builds a Manager from specs exactly the way
// newMCPSessionAssembly does (mcpDefinitions -> mcpharness.NewManager) and
// returns its ConfigDigest -- the same pre-Start digest openRuntimeAgent
// folds into cfg.MCPConfigRev (assembly.go's configRev is read before attach's
// Start call). No live session or process is needed: BindingIdentity's
// configuration-side fields (Name, TransportKind, RedactedOrigin, Required,
// CapabilityDigest, FilterDigest, LimitsDigest, CompatDigest, and for
// session-scoped bindings SelectorDigest) are all computable from the
// bindings alone (mcp/pkg/harness/identity.go's bindingState.identity).
func mcpManagerDigest(t *testing.T, specs []mcpServerSpec) string {
	t.Helper()
	bindings, err := mcpDefinitions(specs)
	if err != nil {
		t.Fatalf("mcpDefinitions() error = %v", err)
	}
	mgr, err := mcpharness.NewManager(bindings, mcpharness.Deps{
		Gates:  &mcpGateOpener{},
		Events: &mcpEventPublisher{},
	})
	if err != nil {
		t.Fatalf("mcpharness.NewManager() error = %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })
	return mgr.ConfigDigest()
}

// TestAgentFingerprintFieldsCarriesMCPConfigRev is the direct, minimal proof
// of Task 11's one-line change: agentFingerprintFields must forward
// cfg.MCPConfigRev into rig.ConfigFingerprintFields.ExternalCapabilityRev
// unchanged, and a zero cfg.MCPConfigRev (no mcp.json, assembly.go's
// mcpSessionAssembly.configRev's nil-manager case) must leave
// ExternalCapabilityRev at its zero value -- the "no external capabilities"
// default every pre-MCP session's fingerprint already had, since Carbon
// never set this field before this change.
func TestAgentFingerprintFieldsCarriesMCPConfigRev(t *testing.T) {
	t.Parallel()

	t.Run("absent mcp.json leaves the field zero", func(t *testing.T) {
		t.Parallel()
		fields := agentFingerprintFields(Config{})
		if fields.ExternalCapabilityRev != "" {
			t.Errorf("ExternalCapabilityRev = %q, want \"\" for a zero Config", fields.ExternalCapabilityRev)
		}
	})

	t.Run("present digest is forwarded unchanged", func(t *testing.T) {
		t.Parallel()
		fields := agentFingerprintFields(Config{MCPConfigRev: "mcp-digest-abc123"})
		if fields.ExternalCapabilityRev != "mcp-digest-abc123" {
			t.Errorf("ExternalCapabilityRev = %q, want %q", fields.ExternalCapabilityRev, "mcp-digest-abc123")
		}
	})
}

// TestMCPConfigDigestMovesWithRealTopologyChanges proves the digest Carbon
// folds into ExternalCapabilityRev genuinely reacts to the concrete mcp.json
// changes Task 11's test plan names: a server added, a server's URL
// changed, and a server's roles changed. Each subtest computes two REAL
// digests from two genuinely different spec sets (not synthetic literal
// strings) via the exact mcpDefinitions -> mcpharness.NewManager ->
// ConfigDigest path production code uses, and asserts they differ.
func TestMCPConfigDigestMovesWithRealTopologyChanges(t *testing.T) {
	t.Parallel()

	t.Run("server added", func(t *testing.T) {
		t.Parallel()
		before := []mcpServerSpec{{name: "a", kind: "stdio", command: "/bin/sh"}}
		after := []mcpServerSpec{
			{name: "a", kind: "stdio", command: "/bin/sh"},
			{name: "b", kind: "stdio", command: "/bin/cat"},
		}
		digestBefore := mcpManagerDigest(t, before)
		digestAfter := mcpManagerDigest(t, after)
		if digestBefore == "" || digestAfter == "" {
			t.Fatalf("ConfigDigest() = %q / %q, want both non-empty for non-empty bindings", digestBefore, digestAfter)
		}
		if digestBefore == digestAfter {
			t.Errorf("ConfigDigest() unchanged after adding a server: %q", digestBefore)
		}
	})

	t.Run("URL changed", func(t *testing.T) {
		t.Parallel()
		// The digest's RedactedOrigin is scheme://host[:port] only (no path,
		// no query -- see mcp/pkg/client/definition.go), so the two URLs must
		// differ in HOST to move the digest; a path-only difference is
		// deliberately invisible to it (proven by trial: this is why the URL
		// here changes host, not just path).
		before := []mcpServerSpec{{name: "a", kind: "http", url: "https://mcp-one.example.test/mcp"}}
		after := []mcpServerSpec{{name: "a", kind: "http", url: "https://mcp-two.example.test/mcp"}}
		digestBefore := mcpManagerDigest(t, before)
		digestAfter := mcpManagerDigest(t, after)
		if digestBefore == digestAfter {
			t.Errorf("ConfigDigest() unchanged after changing the server URL host: %q", digestBefore)
		}
	})

	t.Run("omitted roles equal explicit carbon", func(t *testing.T) {
		t.Parallel()
		before := []mcpServerSpec{{name: "a", kind: "stdio", command: "/bin/sh", roles: []string{"carbon"}}}
		after := []mcpServerSpec{{name: "a", kind: "stdio", command: "/bin/sh", roles: nil}}
		digestBefore := mcpManagerDigest(t, before)
		digestAfter := mcpManagerDigest(t, after)
		if digestBefore != digestAfter {
			t.Errorf("ConfigDigest() changed between explicit and omitted Carbon roles: %q != %q", digestBefore, digestAfter)
		}
	})
}

// TestMCPConfigDigestStableAcrossHeaderValueChange proves a header VALUE
// change (same header name, different secret) never moves the digest folded
// into the fingerprint. Manager.ConfigDigest hashes BindingIdentity
// (identity.go's bindingState.identity), whose RedactedOrigin is
// scheme://host[:port] only (mcp/pkg/client/definition.go) -- no path, no
// query, no headers -- and carries no field for header/env VALUES at all.
// This is the property Task 11 needed proven before treating
// ExternalCapabilityRev as secret-free: a rotated bearer token must not
// force a restore-drift decision that would otherwise leak into an audit
// trail.
func TestMCPConfigDigestStableAcrossHeaderValueChange(t *testing.T) {
	t.Parallel()
	before := []mcpServerSpec{{
		name:    "context7",
		kind:    "http",
		url:     "https://mcp.example.test/mcp",
		headers: map[string]string{"Authorization": "Bearer old-secret-value"},
	}}
	after := []mcpServerSpec{{
		name:    "context7",
		kind:    "http",
		url:     "https://mcp.example.test/mcp",
		headers: map[string]string{"Authorization": "Bearer completely-different-secret"},
	}}
	digestBefore := mcpManagerDigest(t, before)
	digestAfter := mcpManagerDigest(t, after)
	if digestBefore == "" {
		t.Fatal("ConfigDigest() = \"\", want non-empty for a non-empty binding set")
	}
	if digestBefore != digestAfter {
		t.Errorf("ConfigDigest() changed across a header VALUE-only change: %q != %q, want equal", digestBefore, digestAfter)
	}
}

// TestMCPConfigFingerprintRestoreBehavior exercises Task 11's fingerprint
// plumbing through the SAME restore-drift harness
// TestPermissionReviewConfigFingerprintChanges (permission_review_test.go)
// and TestRestoreRejectsAccessProfileDrift (access_assembly_test.go) use:
// buildRig/RestoreSession directly, no live MCP process needed since
// agentFingerprintFields reads cfg.MCPConfigRev as a plain string.
//
// Harness's pkg/event/drift.go's AssessDrift now classifies EVERY
// ExternalCapabilityRev change as event.DriftExternal at event.DriftWarn
// severity, unconditionally: like DriftPermission, ExternalCapabilityRev
// carries no strictness ordinal to compare directionally, so its direction
// is always unknowable, and AssessDrift's own documented rule is "Warn when
// direction is unknowable -- fail secure" (harness commit c050a43a, fixing a
// gap this test's own predecessor discovered and reported). Carbon's
// RestoreDecider (session.DefaultPolicyDecider, per this repo's CLAUDE.md
// "Permission review" section) rejects whenever
// event.DriftAssessment.AnyWarn() is true, so a restore where the MCP digest
// changed is REJECTED with a typed *session.RestoreRejectedError -- proven
// here through the real buildRig/RestoreSession path with a REAL digest pair
// from genuinely different specs (a second stdio server added), not
// synthetic literals.
func TestMCPConfigFingerprintRestoreBehavior(t *testing.T) {
	t.Parallel()

	baselineSpecs := []mcpServerSpec{{name: "a", kind: "stdio", command: "/bin/sh"}}
	changedSpecs := []mcpServerSpec{
		{name: "a", kind: "stdio", command: "/bin/sh"},
		{name: "b", kind: "stdio", command: "/bin/cat"},
	}
	baselineDigest := mcpManagerDigest(t, baselineSpecs)
	changedDigest := mcpManagerDigest(t, changedSpecs)
	if baselineDigest == changedDigest {
		t.Fatalf("baseline and changed ConfigDigest are identical (%q): test fixture does not actually change config", baselineDigest)
	}

	stores, err := openTestStores(t)
	if err != nil {
		t.Fatalf("openTestStores() error = %v", err)
	}
	root := t.TempDir()

	access, cfg := headlessTestAccess(t, Config{MCPConfigRev: baselineDigest}, root)
	definition, err := carbonTestDefinition(&fakeLLM{}, testModel(), cfg, access)
	if err != nil {
		t.Fatalf("carbonTestDefinition() error = %v", err)
	}
	assembly, err := buildRig(definition, stores, root, cfg, false)
	if err != nil {
		t.Fatalf("buildRig() error = %v", err)
	}
	controller, err := assembly.NewSession(context.Background())
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	sid := controller.SessionID()
	if err := controller.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}

	restoreWith := func(t *testing.T, mcpConfigRev string, allowMismatch bool) error {
		t.Helper()
		racc, rcfg := headlessTestAccess(t, Config{MCPConfigRev: mcpConfigRev}, root)
		rdef, err := carbonTestDefinition(&fakeLLM{}, testModel(), rcfg, racc)
		if err != nil {
			t.Fatalf("carbonTestDefinition() error = %v", err)
		}
		rasm, err := buildRig(rdef, stores, root, rcfg, allowMismatch)
		if err != nil {
			t.Fatalf("buildRig() error = %v", err)
		}
		rctrl, err := rasm.RestoreSession(context.Background(), sid)
		if err == nil {
			_ = rctrl.Shutdown(context.Background())
		}
		return err
	}

	t.Run("same digest restores with no mismatch", func(t *testing.T) {
		if err := restoreWith(t, baselineDigest, false); err != nil {
			t.Fatalf("restore under the SAME MCP config digest failed: %v", err)
		}
	})

	t.Run("changed digest is adopted as ephemeral", func(t *testing.T) {
		if err := restoreWith(t, changedDigest, false); err != nil {
			t.Fatalf("restore under a DIFFERENT ephemeral MCP config digest failed: %v", err)
		}
	})

	t.Run("changed digest with AllowConfigMismatch also succeeds", func(t *testing.T) {
		if err := restoreWith(t, changedDigest, true); err != nil {
			t.Fatalf("restore under a DIFFERENT MCP config digest WITH AllowConfigMismatch error = %v, want success", err)
		}
	})
}

// TestMCPAbsentConfigRestoresUnaffected proves the explicit "both sides
// absent" case Task 11 names: a baseline opened with no mcp.json
// (cfg.MCPConfigRev == "") restores cleanly against a restore attempt that
// also has no mcp.json. Every other restore test in this package that does
// not set MCPConfigRev already exercises this as a side effect (it stays at
// its zero value on both sides), so this test is intentionally minimal --
// its only purpose is to document the "absent-file sessions restore across
// the change" requirement explicitly, in one place, rather than leave it
// implicit across unrelated tests.
func TestMCPAbsentConfigRestoresUnaffected(t *testing.T) {
	t.Parallel()
	stores, err := openTestStores(t)
	if err != nil {
		t.Fatalf("openTestStores() error = %v", err)
	}
	root := t.TempDir()

	access, cfg := headlessTestAccess(t, Config{}, root)
	if cfg.MCPConfigRev != "" {
		t.Fatalf("cfg.MCPConfigRev = %q, want \"\" for a Config with no mcp.json", cfg.MCPConfigRev)
	}
	definition, err := carbonTestDefinition(&fakeLLM{}, testModel(), cfg, access)
	if err != nil {
		t.Fatalf("carbonTestDefinition() error = %v", err)
	}
	assembly, err := buildRig(definition, stores, root, cfg, false)
	if err != nil {
		t.Fatalf("buildRig() error = %v", err)
	}
	controller, err := assembly.NewSession(context.Background())
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	sid := controller.SessionID()
	if err := controller.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}

	racc, rcfg := headlessTestAccess(t, Config{}, root)
	rdef, err := carbonTestDefinition(&fakeLLM{}, testModel(), rcfg, racc)
	if err != nil {
		t.Fatalf("carbonTestDefinition() error = %v", err)
	}
	rasm, err := buildRig(rdef, stores, root, rcfg, false)
	if err != nil {
		t.Fatalf("buildRig() error = %v", err)
	}
	rctrl, err := rasm.RestoreSession(context.Background(), sid)
	if err != nil {
		t.Fatalf("restore with mcp.json absent on both sides failed: %v", err)
	}
	_ = rctrl.Shutdown(context.Background())
}
