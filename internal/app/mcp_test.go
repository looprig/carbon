package app

import (
	"errors"
	"strings"
	"testing"

	"github.com/looprig/core/uuid"
	mcpharness "github.com/looprig/mcp/pkg/harness"
)

// mcpTestSecret stands in for a header or env value across this file's
// tables; every failing-case assertion checks it never appears in a
// rendered error, matching mcpconfig_test.go's own "poisoned fixture"
// pattern.
const mcpTestSecret = "test-secret-do-not-log"

// mcpTestLoopID is a fresh, non-zero Loop identity used only to exercise
// LoopSelector.Permits, which matches a Named() selector on name alone and
// ignores loopID entirely -- any non-colliding value works here.
func mcpTestLoopID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.New()
	if err != nil {
		t.Fatalf("uuid.New() error = %v", err)
	}
	return id
}

func TestMCPDefinitionsStdioHappyPath(t *testing.T) {
	spec := mcpServerSpec{name: "sh", kind: "stdio", command: "/bin/sh"}

	bindings, err := mcpDefinitions([]mcpServerSpec{spec})
	if err != nil {
		t.Fatalf("mcpDefinitions() error = %v", err)
	}
	if len(bindings) != 1 {
		t.Fatalf("mcpDefinitions() bindings = %d, want 1", len(bindings))
	}

	binding := bindings[0]
	if binding.Name != "sh" {
		t.Errorf("binding.Name = %q, want sh", binding.Name)
	}
	if binding.Scope != mcpharness.ScopeSession {
		t.Errorf("binding.Scope = %v, want ScopeSession", binding.Scope)
	}
	if binding.Required {
		t.Errorf("binding.Required = true, want false")
	}
	if binding.Server.Transport == nil || binding.Server.Transport.Kind() != "stdio" {
		t.Errorf("binding.Server.Transport.Kind() = %v, want stdio", binding.Server.Transport)
	}
	if err := binding.Validate(); err != nil {
		t.Errorf("binding.Validate() error = %v", err)
	}

	loopID := mcpTestLoopID(t)
	for _, role := range []string{"planner", "builder", "reviewer"} {
		if !binding.Visibility.Permits(loopID, role) {
			t.Errorf("binding.Visibility.Permits(_, %q) = false, want true (roles empty -> all three)", role)
		}
	}
	if binding.Visibility.Permits(loopID, "not-a-role") {
		t.Errorf("binding.Visibility.Permits(_, %q) = true, want false", "not-a-role")
	}
}

func TestMCPDefinitionsStdioMissingCommandFailsClosed(t *testing.T) {
	spec := mcpServerSpec{name: "ghost", kind: "stdio", command: "definitely-not-a-command-xyzzy"}

	bindings, err := mcpDefinitions([]mcpServerSpec{spec})
	if err == nil {
		t.Fatal("mcpDefinitions() error = nil, want error")
	}
	if bindings != nil {
		t.Errorf("mcpDefinitions() bindings = %v, want nil on error", bindings)
	}
	var configErr *MCPConfigError
	if !errors.As(err, &configErr) {
		t.Fatalf("mcpDefinitions() error = %T, want *MCPConfigError", err)
	}
	if configErr.Binding != "ghost" {
		t.Errorf("MCPConfigError.Binding = %q, want ghost", configErr.Binding)
	}
}

func TestMCPDefinitionsVisibilityDefaultsToAllRolesWhenEmpty(t *testing.T) {
	spec := mcpServerSpec{name: "sh", kind: "stdio", command: "/bin/sh"} // roles nil

	bindings, err := mcpDefinitions([]mcpServerSpec{spec})
	if err != nil {
		t.Fatalf("mcpDefinitions() error = %v", err)
	}
	binding := bindings[0]
	loopID := mcpTestLoopID(t)
	for _, role := range []string{"planner", "builder", "reviewer"} {
		if !binding.Visibility.Permits(loopID, role) {
			t.Errorf("empty roles: Permits(_, %q) = false, want true", role)
		}
	}
}

func TestMCPDefinitionsVisibilityHonorsExplicitPartialRoles(t *testing.T) {
	spec := mcpServerSpec{
		name:    "sh",
		kind:    "stdio",
		command: "/bin/sh",
		roles:   []string{"builder", "planner"},
	}

	bindings, err := mcpDefinitions([]mcpServerSpec{spec})
	if err != nil {
		t.Fatalf("mcpDefinitions() error = %v", err)
	}
	binding := bindings[0]
	loopID := mcpTestLoopID(t)
	for _, role := range []string{"planner", "builder"} {
		if !binding.Visibility.Permits(loopID, role) {
			t.Errorf("explicit roles: Permits(_, %q) = false, want true", role)
		}
	}
	if binding.Visibility.Permits(loopID, "reviewer") {
		t.Errorf("explicit roles [planner,builder]: Permits(_, reviewer) = true, want false")
	}
}

func TestMCPDefinitionsHTTPHappyPath(t *testing.T) {
	spec := mcpServerSpec{
		name:    "context7",
		kind:    "http",
		url:     "https://mcp.example.test/mcp",
		headers: map[string]string{"Authorization": mcpTestSecret},
	}

	bindings, err := mcpDefinitions([]mcpServerSpec{spec})
	if err != nil {
		t.Fatalf("mcpDefinitions() error = %v", err)
	}
	if len(bindings) != 1 {
		t.Fatalf("mcpDefinitions() bindings = %d, want 1", len(bindings))
	}
	binding := bindings[0]
	if binding.Server.Transport == nil || binding.Server.Transport.Kind() != "streamablehttp" {
		t.Errorf("binding.Server.Transport.Kind() = %v, want streamablehttp", binding.Server.Transport)
	}
	if err := binding.Validate(); err != nil {
		t.Errorf("binding.Validate() error = %v", err)
	}
}

func TestMCPDefinitionsHTTPBrokenHeaderNeverLeaksValue(t *testing.T) {
	spec := mcpServerSpec{
		name:    "context7",
		kind:    "http",
		url:     "https://mcp.example.test/mcp",
		headers: map[string]string{"Bad Header": mcpTestSecret}, // space is illegal in a header name
	}

	bindings, err := mcpDefinitions([]mcpServerSpec{spec})
	if err == nil {
		t.Fatal("mcpDefinitions() error = nil, want error (invalid header name)")
	}
	if bindings != nil {
		t.Errorf("mcpDefinitions() bindings = %v, want nil on error", bindings)
	}
	if strings.Contains(err.Error(), mcpTestSecret) {
		t.Errorf("mcpDefinitions() error leaked header secret: %v", err)
	}
	var configErr *MCPConfigError
	if !errors.As(err, &configErr) {
		t.Fatalf("mcpDefinitions() error = %T, want *MCPConfigError", err)
	}
	if configErr.Binding != "context7" {
		t.Errorf("MCPConfigError.Binding = %q, want context7", configErr.Binding)
	}
}

func TestMCPDefinitionsSSEHappyPath(t *testing.T) {
	spec := mcpServerSpec{
		name: "events",
		kind: "sse",
		url:  "https://mcp.example.test/sse",
	}

	bindings, err := mcpDefinitions([]mcpServerSpec{spec})
	if err != nil {
		t.Fatalf("mcpDefinitions() error = %v", err)
	}
	if len(bindings) != 1 {
		t.Fatalf("mcpDefinitions() bindings = %d, want 1", len(bindings))
	}
	binding := bindings[0]
	if binding.Server.Transport == nil || binding.Server.Transport.Kind() != "sse" {
		t.Errorf("binding.Server.Transport.Kind() = %v, want sse", binding.Server.Transport)
	}
	if err := binding.Validate(); err != nil {
		t.Errorf("binding.Validate() error = %v", err)
	}
}

// TestMCPDefinitionsMultipleSpecsPreserveOrder proves mcpDefinitions returns
// bindings in the same order and count as the input specs across all three
// transport kinds in one call.
func TestMCPDefinitionsMultipleSpecsPreserveOrder(t *testing.T) {
	specs := []mcpServerSpec{
		{name: "alpha", kind: "stdio", command: "/bin/sh"},
		{name: "beta", kind: "http", url: "https://mcp.example.test/mcp"},
		{name: "gamma", kind: "sse", url: "https://mcp.example.test/sse"},
	}

	bindings, err := mcpDefinitions(specs)
	if err != nil {
		t.Fatalf("mcpDefinitions() error = %v", err)
	}
	if len(bindings) != 3 {
		t.Fatalf("mcpDefinitions() bindings = %d, want 3", len(bindings))
	}
	wantNames := []string{"alpha", "beta", "gamma"}
	for i, want := range wantNames {
		if bindings[i].Name != want {
			t.Errorf("bindings[%d].Name = %q, want %q", i, bindings[i].Name, want)
		}
	}
}

// TestMCPDefinitionsAbortsBatchOnFirstFailure proves a construction failure
// on one spec in a multi-spec batch fails the whole call closed: no partial
// slice, and the other (valid) specs' construction is not silently
// swallowed or reordered around the bad one.
func TestMCPDefinitionsAbortsBatchOnFirstFailure(t *testing.T) {
	specs := []mcpServerSpec{
		{name: "good-one", kind: "stdio", command: "/bin/sh"},
		{name: "bad", kind: "stdio", command: "definitely-not-a-command-xyzzy"},
		{name: "good-two", kind: "http", url: "https://mcp.example.test/mcp"},
	}

	bindings, err := mcpDefinitions(specs)
	if err == nil {
		t.Fatal("mcpDefinitions() error = nil, want error")
	}
	if bindings != nil {
		t.Errorf("mcpDefinitions() bindings = %v, want nil (fail closed, no partial slice)", bindings)
	}
	var configErr *MCPConfigError
	if !errors.As(err, &configErr) {
		t.Fatalf("mcpDefinitions() error = %T, want *MCPConfigError", err)
	}
	if configErr.Binding != "bad" {
		t.Errorf("MCPConfigError.Binding = %q, want bad", configErr.Binding)
	}
}

// TestMCPEnvVarsFromIsSortedByName proves the env-allowlist helper produces
// a deterministic order regardless of Go's randomized map iteration: the
// same map content always yields the same []stdio.Var slice, sorted by
// Name.
func TestMCPEnvVarsFromIsSortedByName(t *testing.T) {
	env := map[string]string{
		"ZETA":  "1",
		"ALPHA": "2",
		"MU":    mcpTestSecret,
	}

	for i := 0; i < 5; i++ {
		vars := mcpEnvVarsFrom(env)
		if len(vars) != 3 {
			t.Fatalf("mcpEnvVarsFrom() len = %d, want 3", len(vars))
		}
		wantNames := []string{"ALPHA", "MU", "ZETA"}
		for j, want := range wantNames {
			if vars[j].Name != want {
				t.Fatalf("run %d: vars[%d].Name = %q, want %q (want sorted order)", i, j, vars[j].Name, want)
			}
		}
		if vars[1].Value != mcpTestSecret {
			t.Errorf("vars[1].Value = %q, want secret preserved", vars[1].Value)
		}
	}
}

func TestMCPEnvVarsFromEmptyIsNil(t *testing.T) {
	if got := mcpEnvVarsFrom(nil); got != nil {
		t.Errorf("mcpEnvVarsFrom(nil) = %v, want nil", got)
	}
	if got := mcpEnvVarsFrom(map[string]string{}); got != nil {
		t.Errorf("mcpEnvVarsFrom({}) = %v, want nil", got)
	}
}

// TestMCPHeadersFromIsSortedByName mirrors TestMCPEnvVarsFromIsSortedByName
// for the header helper: same determinism concern, same proof.
func TestMCPHeadersFromIsSortedByName(t *testing.T) {
	headers := map[string]string{
		"X-Zeta":        "1",
		"Authorization": mcpTestSecret,
		"X-Alpha":       "2",
	}

	for i := 0; i < 5; i++ {
		got := mcpHeadersFrom(headers)
		if len(got) != 3 {
			t.Fatalf("mcpHeadersFrom() len = %d, want 3", len(got))
		}
		wantNames := []string{"Authorization", "X-Alpha", "X-Zeta"}
		for j, want := range wantNames {
			if got[j].Name() != want {
				t.Fatalf("run %d: got[%d].Name() = %q, want %q (want sorted order)", i, j, got[j].Name(), want)
			}
		}
		if got[0].Value() != mcpTestSecret {
			t.Errorf("got[0].Value() = %q, want secret preserved", got[0].Value())
		}
	}
}

func TestMCPHeadersFromEmptyIsNil(t *testing.T) {
	if got := mcpHeadersFrom(nil); got != nil {
		t.Errorf("mcpHeadersFrom(nil) = %v, want nil", got)
	}
	if got := mcpHeadersFrom(map[string]string{}); got != nil {
		t.Errorf("mcpHeadersFrom({}) = %v, want nil", got)
	}
}

func TestMCPVisibilityRoles(t *testing.T) {
	if got := mcpVisibilityRoles(nil); strings.Join(got, ",") != "planner,builder,reviewer" {
		t.Errorf("mcpVisibilityRoles(nil) = %v, want all three", got)
	}
	if got := mcpVisibilityRoles([]string{}); strings.Join(got, ",") != "planner,builder,reviewer" {
		t.Errorf("mcpVisibilityRoles([]) = %v, want all three", got)
	}
	if got := mcpVisibilityRoles([]string{"builder"}); strings.Join(got, ",") != "builder" {
		t.Errorf("mcpVisibilityRoles([builder]) = %v, want [builder]", got)
	}
}

func TestMCPDefinitionsEmptySpecsReturnsEmpty(t *testing.T) {
	bindings, err := mcpDefinitions(nil)
	if err != nil {
		t.Fatalf("mcpDefinitions(nil) error = %v", err)
	}
	if len(bindings) != 0 {
		t.Fatalf("mcpDefinitions(nil) bindings = %v, want none", bindings)
	}
}
