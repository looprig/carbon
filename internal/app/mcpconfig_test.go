package app

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mcpConfigTestSecret stands in for both a header and an env value across
// this file's tables; every failing-case assertion checks it never appears
// in a rendered error.
const mcpConfigTestSecret = "test-secret-do-not-log"

// validMCPConfigJSON is the design doc's own §1.1 two-server example
// (docs/plans/2026-08-05-coderig-mcp-and-permission-review-design.md).
const validMCPConfigJSON = `{
  "mcpServers": {
    "context7": {
      "type": "http",
      "url": "https://mcp.context7.com/mcp",
      "headers": { "CONTEXT7_API_KEY": "` + mcpConfigTestSecret + `" }
    },
    "docs-local": {
      "type": "stdio",
      "command": "npx",
      "args": ["-y", "@upstash/context7-mcp"],
      "env": { "FOO": "bar" },
		"roles": ["carbon"]
    }
  }
}`

func TestDecodeMCPConfigHappyPath(t *testing.T) {
	file, err := decodeMCPConfig([]byte(validMCPConfigJSON))
	if err != nil {
		t.Fatalf("decodeMCPConfig() error = %v", err)
	}
	if len(file.MCPServers) != 2 {
		t.Fatalf("decodeMCPConfig() servers = %d, want 2", len(file.MCPServers))
	}
}

// TestNormalizeMCPConfigHappyPath round-trips the design doc's own example
// to two correct mcpServerSpec values, sorted by binding name.
func TestNormalizeMCPConfigHappyPath(t *testing.T) {
	file, err := decodeMCPConfig([]byte(validMCPConfigJSON))
	if err != nil {
		t.Fatalf("decodeMCPConfig() error = %v", err)
	}
	specs, err := normalizeMCPConfig(file)
	if err != nil {
		t.Fatalf("normalizeMCPConfig() error = %v", err)
	}
	if len(specs) != 2 {
		t.Fatalf("normalizeMCPConfig() specs = %d, want 2", len(specs))
	}

	context7, docsLocal := specs[0], specs[1]
	if context7.name != "context7" {
		t.Fatalf("specs[0].name = %q, want context7", context7.name)
	}
	if context7.kind != "http" {
		t.Errorf("context7.kind = %q, want http", context7.kind)
	}
	if context7.url != "https://mcp.context7.com/mcp" {
		t.Errorf("context7.url = %q", context7.url)
	}
	if context7.headers["CONTEXT7_API_KEY"] != mcpConfigTestSecret {
		t.Errorf("context7.headers = %+v", context7.headers)
	}
	if context7.command != "" || context7.args != nil || context7.env != nil {
		t.Errorf("context7 stdio fields leaked into http spec: %+v", context7)
	}
	if got := strings.Join(context7.roles, ","); got != "carbon" {
		t.Errorf("context7.roles = %v, want [carbon]", context7.roles)
	}

	if docsLocal.name != "docs-local" {
		t.Fatalf("specs[1].name = %q, want docs-local", docsLocal.name)
	}
	if docsLocal.kind != "stdio" {
		t.Errorf("docsLocal.kind = %q, want stdio", docsLocal.kind)
	}
	if docsLocal.command != "npx" {
		t.Errorf("docsLocal.command = %q, want npx", docsLocal.command)
	}
	if strings.Join(docsLocal.args, ",") != "-y,@upstash/context7-mcp" {
		t.Errorf("docsLocal.args = %v", docsLocal.args)
	}
	if docsLocal.env["FOO"] != "bar" {
		t.Errorf("docsLocal.env = %+v", docsLocal.env)
	}
	if docsLocal.url != "" || docsLocal.headers != nil {
		t.Errorf("docsLocal http fields leaked into stdio spec: %+v", docsLocal)
	}
	if got := strings.Join(docsLocal.roles, ","); got != "carbon" {
		t.Errorf("docsLocal.roles = %v, want [carbon]", docsLocal.roles)
	}
}

func TestNormalizeMCPConfigSSEExplicitType(t *testing.T) {
	file := mcpConfigFile{MCPServers: map[string]mcpServerConfig{
		"events": {Type: "sse", URL: "https://mcp.example.test/sse"},
	}}
	specs, err := normalizeMCPConfig(file)
	if err != nil {
		t.Fatalf("normalizeMCPConfig(sse) error = %v", err)
	}
	if len(specs) != 1 {
		t.Fatalf("normalizeMCPConfig(sse) specs = %d, want 1", len(specs))
	}
	if specs[0].kind != "sse" {
		t.Errorf("specs[0].kind = %q, want sse", specs[0].kind)
	}
	if specs[0].url != "https://mcp.example.test/sse" {
		t.Errorf("specs[0].url = %q", specs[0].url)
	}
}

func TestNormalizeMCPServerRejectsFormerRoleNames(t *testing.T) {
	for _, role := range []string{"planner", "builder", "reviewer"} {
		t.Run(role, func(t *testing.T) {
			_, err := normalizeMCPServer("docs", mcpServerConfig{
				Type: "stdio", Command: "/bin/sh", Roles: []string{role},
			})
			if err == nil {
				t.Fatalf("normalizeMCPServer(%q) succeeded, want strict Generic-only rejection", role)
			}
		})
	}
}

func TestNormalizeMCPConfigLoopbackHTTPAccepted(t *testing.T) {
	file := mcpConfigFile{MCPServers: map[string]mcpServerConfig{
		"local": {Type: "http", URL: "http://127.0.0.1:8080/mcp"},
	}}
	specs, err := normalizeMCPConfig(file)
	if err != nil {
		t.Fatalf("normalizeMCPConfig(loopback http) error = %v", err)
	}
	if len(specs) != 1 || specs[0].url != "http://127.0.0.1:8080/mcp" {
		t.Fatalf("normalizeMCPConfig(loopback http) specs = %+v", specs)
	}
}

func TestNormalizeMCPConfigEmptyServersIsValid(t *testing.T) {
	specs, err := normalizeMCPConfig(mcpConfigFile{})
	if err != nil {
		t.Fatalf("normalizeMCPConfig(empty) error = %v", err)
	}
	if len(specs) != 0 {
		t.Fatalf("normalizeMCPConfig(empty) specs = %v, want none", specs)
	}

	specs, err = normalizeMCPConfig(mcpConfigFile{MCPServers: map[string]mcpServerConfig{}})
	if err != nil {
		t.Fatalf("normalizeMCPConfig({}) error = %v", err)
	}
	if len(specs) != 0 {
		t.Fatalf("normalizeMCPConfig({}) specs = %v, want none", specs)
	}
}

func validStdioMCPServerConfig() mcpServerConfig {
	return mcpServerConfig{
		Type:    "stdio",
		Command: "npx",
		Args:    []string{"-y", "server"},
		Env:     map[string]string{"FOO": mcpConfigTestSecret},
	}
}

func validHTTPMCPServerConfig() mcpServerConfig {
	return mcpServerConfig{
		Type:    "http",
		URL:     "https://mcp.example.test/mcp",
		Headers: map[string]string{"Authorization": mcpConfigTestSecret},
	}
}

// TestNormalizeMCPConfigRejectsInvalidServers is the table-driven core of
// this file: every "Validation rules to test" bullet from Task 6, one
// mutation at a time, against either a valid stdio or a valid http base
// fixture. Every case's error must be a bounded *MCPConfigError that never
// echoes the env/header secret carried by the base fixture.
func TestNormalizeMCPConfigRejectsInvalidServers(t *testing.T) {
	tests := []struct {
		name    string
		binding string
		base    func() mcpServerConfig
		mutate  func(*mcpServerConfig)
	}{
		// binding name must satisfy client.Name.Validate().
		{name: "binding name empty", binding: ""},
		{name: "binding name uppercase", binding: "Docs"},
		{name: "binding name starts with dash", binding: "-docs"},
		{name: "binding name starts with underscore", binding: "_docs"},
		{name: "binding name invalid character", binding: "docs.local"},
		{name: "binding name whitespace", binding: "docs local"},
		{name: "binding name too long", binding: strings.Repeat("a", 65)},

		// type inference: command set -> stdio; url set -> http; both or
		// neither -> error.
		{name: "type omitted, both command and url set", binding: "docs", mutate: func(c *mcpServerConfig) {
			c.Type = ""
			c.URL = "https://mcp.example.test/mcp"
		}},
		{name: "type omitted, neither command nor url set", binding: "docs", mutate: func(c *mcpServerConfig) {
			c.Type = ""
			c.Command = ""
		}},

		// explicit type must agree with the fields present.
		{name: "explicit stdio missing command", binding: "docs", mutate: func(c *mcpServerConfig) { c.Command = "" }},
		{name: "explicit stdio also sets url", binding: "docs", mutate: func(c *mcpServerConfig) { c.URL = "https://mcp.example.test/mcp" }},
		{name: "explicit http missing url", binding: "docs", base: validHTTPMCPServerConfig, mutate: func(c *mcpServerConfig) { c.URL = "" }},
		{name: "explicit http also sets command", binding: "docs", base: validHTTPMCPServerConfig, mutate: func(c *mcpServerConfig) { c.Command = "npx" }},
		{name: "explicit sse missing url", binding: "docs", base: validHTTPMCPServerConfig, mutate: func(c *mcpServerConfig) { c.Type = "sse"; c.URL = "" }},
		{name: "explicit sse also sets command", binding: "docs", base: validHTTPMCPServerConfig, mutate: func(c *mcpServerConfig) { c.Type = "sse"; c.Command = "npx" }},
		{name: "unknown explicit type", binding: "docs", mutate: func(c *mcpServerConfig) { c.Type = "websocket" }},

		// url must parse absolute, https, or loopback-only http.
		{name: "url malformed", binding: "docs", base: validHTTPMCPServerConfig, mutate: func(c *mcpServerConfig) { c.URL = "://not-a-url" }},
		{name: "url non-loopback http", binding: "docs", base: validHTTPMCPServerConfig, mutate: func(c *mcpServerConfig) { c.URL = "http://mcp.example.test/mcp" }},
		{name: "url unsupported scheme", binding: "docs", base: validHTTPMCPServerConfig, mutate: func(c *mcpServerConfig) { c.URL = "ftp://mcp.example.test/mcp" }},
		{name: "url has userinfo", binding: "docs", base: validHTTPMCPServerConfig, mutate: func(c *mcpServerConfig) { c.URL = "https://user:pass@mcp.example.test/mcp" }},

		// Legacy role is a rejection fixture; roles accepts only generic.
		{name: "legacy role is unknown", binding: "docs", mutate: func(c *mcpServerConfig) { c.Roles = []string{"planner"} }},
		{name: "duplicate roles", binding: "docs", mutate: func(c *mcpServerConfig) { c.Roles = []string{"carbon", "carbon"} }},
		{name: "padded role is unknown", binding: "docs", mutate: func(c *mcpServerConfig) { c.Roles = []string{"carbon "} }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := validStdioMCPServerConfig
			if tt.base != nil {
				base = tt.base
			}
			cfg := base()
			if tt.mutate != nil {
				tt.mutate(&cfg)
			}

			_, err := normalizeMCPServer(tt.binding, cfg)
			if err == nil {
				t.Fatal("normalizeMCPServer() error = nil, want error")
			}
			var configErr *MCPConfigError
			if !errors.As(err, &configErr) {
				t.Fatalf("normalizeMCPServer() error = %T, want *MCPConfigError", err)
			}
			if len(err.Error()) > 512 {
				t.Errorf("error length = %d, want bounded", len(err.Error()))
			}
			if strings.Contains(err.Error(), mcpConfigTestSecret) {
				t.Errorf("error leaked header/env secret: %v", err)
			}
		})
	}
}

// TestNormalizeMCPConfigRejectsInvalidServersViaFullConfig proves the same
// per-server rules are actually reached through normalizeMCPConfig (not just
// the lower-level normalizeMCPServer helper), and that a multi-server config
// with one bad entry still never leaks the good entries' secrets either.
func TestNormalizeMCPConfigRejectsInvalidServersViaFullConfig(t *testing.T) {
	file := mcpConfigFile{MCPServers: map[string]mcpServerConfig{
		"good": validStdioMCPServerConfig(),
		"bad": {
			Type:    "http",
			URL:     "http://mcp.example.test/mcp", // non-loopback http
			Headers: map[string]string{"Authorization": mcpConfigTestSecret},
		},
	}}
	_, err := normalizeMCPConfig(file)
	if err == nil {
		t.Fatal("normalizeMCPConfig() error = nil, want error")
	}
	var configErr *MCPConfigError
	if !errors.As(err, &configErr) {
		t.Fatalf("normalizeMCPConfig() error = %T, want *MCPConfigError", err)
	}
	if configErr.Binding != "bad" {
		t.Errorf("MCPConfigError.Binding = %q, want %q", configErr.Binding, "bad")
	}
	if strings.Contains(err.Error(), mcpConfigTestSecret) {
		t.Errorf("normalizeMCPConfig() error leaked secret: %v", err)
	}
}

// TestNormalizeMCPConfigURLErrorReflectsTransportRule asserts the url check
// really delegates to mcpauth.CanonicalOrigin rather than a hand-rolled
// mirror of its rule: the wording is distinctive to that function.
func TestNormalizeMCPConfigURLErrorReflectsTransportRule(t *testing.T) {
	cfg := validHTTPMCPServerConfig()
	cfg.URL = "http://mcp.example.test/mcp"
	_, err := normalizeMCPServer("docs", cfg)
	if err == nil {
		t.Fatal("normalizeMCPServer(non-loopback http) error = nil, want error")
	}
	if !strings.Contains(err.Error(), "loopback") {
		t.Errorf("error = %v, want it to name the loopback rule (proves delegation to mcpauth.CanonicalOrigin)", err)
	}
}

func TestMCPConfigErrorBoundsCompleteMessage(t *testing.T) {
	err := mcpConfigFailure(
		strings.Repeat("very-long-binding/", 32)+mcpConfigTestSecret,
		strings.Repeat("very-long-field/", 32)+mcpConfigTestSecret,
		errors.New(strings.Repeat("very-long-cause/", 64)+mcpConfigTestSecret),
	)
	if len(err.Error()) > 512 {
		t.Errorf("MCPConfigError.Error() length = %d, want <= 512", len(err.Error()))
	}
	if len(err.Binding) > maxMCPConfigErrorBindingBytes {
		t.Errorf("stored Binding length = %d, want <= %d", len(err.Binding), maxMCPConfigErrorBindingBytes)
	}
	if len(err.Field) > maxMCPConfigErrorFieldBytes {
		t.Errorf("stored Field length = %d, want <= %d", len(err.Field), maxMCPConfigErrorFieldBytes)
	}
	if len(err.Cause) > maxMCPConfigErrorCauseBytes {
		t.Errorf("stored Cause length = %d, want <= %d", len(err.Cause), maxMCPConfigErrorCauseBytes)
	}
	for name, value := range map[string]string{
		"rendered error": err.Error(),
		"binding":        err.Binding,
		"field":          err.Field,
		"cause":          err.Cause,
	} {
		if strings.Contains(value, mcpConfigTestSecret) {
			t.Errorf("%s exposed secret sentinel", name)
		}
	}
	var target *MCPConfigError
	if !errors.As(error(err), &target) {
		t.Fatalf("errors.As(%T) = false, want true", err)
	}
}

func TestDecodeMCPConfig(t *testing.T) {
	t.Run("valid two-server example", func(t *testing.T) {
		got, err := decodeMCPConfig([]byte(validMCPConfigJSON))
		if err != nil {
			t.Fatalf("decodeMCPConfig() error = %v", err)
		}
		if len(got.MCPServers) != 2 {
			t.Fatalf("decodeMCPConfig() servers = %+v", got.MCPServers)
		}
	})

	t.Run("empty mcpServers object decodes", func(t *testing.T) {
		got, err := decodeMCPConfig([]byte(`{"mcpServers": {}}`))
		if err != nil {
			t.Fatalf("decodeMCPConfig() error = %v", err)
		}
		if len(got.MCPServers) != 0 {
			t.Fatalf("decodeMCPConfig() servers = %+v, want none", got.MCPServers)
		}
	})

	invalid := []struct {
		name  string
		input []byte
	}{
		{name: "empty input", input: nil},
		{name: "malformed JSON", input: []byte(`{"mcpServers":`)},
		{name: "two top-level values", input: []byte(`{"mcpServers":{}} {}`)},
		{name: "unknown top-level field", input: []byte(`{"mcpServers":{},"future":true}`)},
		{name: "unknown server field", input: []byte(`{"mcpServers":{"a":{"command":"npx","future":true}}}`)},
		{name: "invalid UTF-8", input: append([]byte(`{"mcpServers":{"a":{"command":"`), 0xff, '"', '}', '}', '}')},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decodeMCPConfig(tt.input)
			assertMCPConfigDecodeError(t, err)
		})
	}
}

// TestDecodeMCPConfigRejectsDuplicateKeysAtEveryDepth proves the shared
// duplicate-key checker is actually wired into this file's decode path (not
// merely inherited by import), at the top level, within a server object, and
// within a nested map inside a server object.
func TestDecodeMCPConfigRejectsDuplicateKeysAtEveryDepth(t *testing.T) {
	tests := []struct {
		name  string
		input string
		key   string
	}{
		{name: "top level", input: `{"mcpServers":{},"mcpServers":{}}`, key: "mcpServers"},
		{name: "server object", input: `{"mcpServers":{"a":{"command":"npx","command":"npx"}}}`, key: "command"},
		{name: "nested env map", input: `{"mcpServers":{"a":{"command":"npx","env":{"K":"1","K":"2"}}}}`, key: "K"},
		{name: "nested object in array", input: `{"mcpServers":{"a":{"args":[{"deep":1,"deep":2}]}}}`, key: "deep"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decodeMCPConfig([]byte(tt.input))
			assertMCPConfigDecodeError(t, err)
			if strings.Contains(err.Error(), tt.key) {
				t.Errorf("decodeMCPConfig() exposed duplicate key %q: %v", tt.key, err)
			}
		})
	}
}

func assertMCPConfigDecodeError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("decodeMCPConfig() error = nil, want error")
	}
	var configErr *MCPConfigError
	if !errors.As(err, &configErr) {
		t.Errorf("decodeMCPConfig() error = %T, want *MCPConfigError", err)
	}
	if len(err.Error()) > 512 {
		t.Errorf("decodeMCPConfig() error length = %d, want bounded", len(err.Error()))
	}
	if strings.Contains(err.Error(), mcpConfigTestSecret) {
		t.Errorf("decodeMCPConfig() leaked secret: %v", err)
	}
}

// TestReadMCPConfigFile exercises mcp.json's file hygiene directly, at the
// same level modelconfig_test.go's TestReadModelConfigFile exercises
// models.json's -- both ultimately call the shared readHygienicConfigFile,
// but this proves maxMCPConfigBytes and the mcp.json wiring are correct on
// their own, independent of loadMCPConfig's higher-level behavior.
func TestReadMCPConfigFile(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		got, exists, err := readMCPConfigFile(filepath.Join(t.TempDir(), "missing.json"))
		if err != nil || exists || got != nil {
			t.Fatalf("readMCPConfigFile(absent) = (%q, %v, %v), want (nil, false, nil)", got, exists, err)
		}
	})

	t.Run("exact size limit", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "mcp.json")
		want := bytes.Repeat([]byte{'x'}, maxMCPConfigBytes)
		writeModelConfigFixture(t, path, want, 0o600)

		got, exists, err := readMCPConfigFile(path)
		if err != nil || !exists || !bytes.Equal(got, want) {
			t.Fatalf("readMCPConfigFile(exact limit) = (%d bytes, %v, %v), want (%d bytes, true, nil)", len(got), exists, err, len(want))
		}
	})

	t.Run("over size limit", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "mcp.json")
		content := make([]byte, maxMCPConfigBytes+1)
		writeModelConfigFixture(t, path, content, 0o600)

		got, exists, err := readMCPConfigFile(path)
		if err == nil || !exists || got != nil {
			t.Fatalf("readMCPConfigFile(over limit) = (%q, %v, %v), want (nil, true, error)", got, exists, err)
		}
		var configErr *MCPConfigError
		if !errors.As(err, &configErr) {
			t.Errorf("readMCPConfigFile(over limit) error type = %T, want *MCPConfigError", err)
		}
	})
}

// TestLoadMCPConfig covers Task 7's file-hygiene-plus-boundary entry point:
// absent file means the feature is off, hygiene violations produce a typed
// error identical in kind to models.json's, and the happy path proves the
// full disk -> decode -> normalize chain, not just that hygiene passed.
func TestLoadMCPConfig(t *testing.T) {
	t.Run("absent file means the feature is off", func(t *testing.T) {
		home := t.TempDir()
		specs, err := loadMCPConfig(Config{HomeDir: home})
		if err != nil || specs != nil {
			t.Fatalf("loadMCPConfig(absent) = (%v, %v), want (nil, nil)", specs, err)
		}
	})

	t.Run("mode 0644 is rejected", func(t *testing.T) {
		if !modelConfigTestIsUnix() {
			t.Skip("Unix permission bits are not supported on this platform")
		}
		home := t.TempDir()
		path := filepath.Join(home, "mcp.json")
		writeModelConfigFixture(t, path, []byte(validMCPConfigJSON), 0o644)

		specs, err := loadMCPConfig(Config{HomeDir: home})
		if err == nil || specs != nil {
			t.Fatalf("loadMCPConfig(mode 0644) = (%v, %v), want (nil, error)", specs, err)
		}
		var configErr *MCPConfigError
		if !errors.As(err, &configErr) {
			t.Errorf("loadMCPConfig(mode 0644) error type = %T, want *MCPConfigError", err)
		}
	})

	t.Run("symlink is rejected", func(t *testing.T) {
		home := t.TempDir()
		target := filepath.Join(home, "target.json")
		writeModelConfigFixture(t, target, []byte(validMCPConfigJSON), 0o600)
		path := filepath.Join(home, "mcp.json")
		if err := os.Symlink(target, path); err != nil {
			t.Skipf("symlinks unsupported: %v", err)
		}

		specs, err := loadMCPConfig(Config{HomeDir: home})
		if err == nil || specs != nil {
			t.Fatalf("loadMCPConfig(symlink) = (%v, %v), want (nil, error)", specs, err)
		}
		var configErr *MCPConfigError
		if !errors.As(err, &configErr) {
			t.Errorf("loadMCPConfig(symlink) error type = %T, want *MCPConfigError", err)
		}
	})

	t.Run("over size limit is rejected", func(t *testing.T) {
		home := t.TempDir()
		path := filepath.Join(home, "mcp.json")
		content := make([]byte, maxMCPConfigBytes+1)
		writeModelConfigFixture(t, path, content, 0o600)

		specs, err := loadMCPConfig(Config{HomeDir: home})
		if err == nil || specs != nil {
			t.Fatalf("loadMCPConfig(over limit) = (%v, %v), want (nil, error)", specs, err)
		}
		var configErr *MCPConfigError
		if !errors.As(err, &configErr) {
			t.Errorf("loadMCPConfig(over limit) error type = %T, want *MCPConfigError", err)
		}
	})

	t.Run("happy path returns the full disk-to-spec chain", func(t *testing.T) {
		home := t.TempDir()
		path := filepath.Join(home, "mcp.json")
		writeModelConfigFixture(t, path, []byte(validMCPConfigJSON), 0o600)

		specs, err := loadMCPConfig(Config{HomeDir: home})
		if err != nil {
			t.Fatalf("loadMCPConfig() error = %v", err)
		}
		if len(specs) != 2 {
			t.Fatalf("loadMCPConfig() specs = %d, want 2", len(specs))
		}

		context7, docsLocal := specs[0], specs[1]
		if context7.name != "context7" || context7.kind != "http" || context7.url != "https://mcp.context7.com/mcp" {
			t.Errorf("specs[0] = %+v", context7)
		}
		if context7.headers["CONTEXT7_API_KEY"] != mcpConfigTestSecret {
			t.Errorf("specs[0].headers = %+v", context7.headers)
		}
		if docsLocal.name != "docs-local" || docsLocal.kind != "stdio" || docsLocal.command != "npx" {
			t.Errorf("specs[1] = %+v", docsLocal)
		}
		if strings.Join(docsLocal.roles, ",") != "carbon" {
			t.Errorf("specs[1].roles = %v, want [carbon]", docsLocal.roles)
		}
	})

	t.Run("HomeDir override changes where mcp.json is read from", func(t *testing.T) {
		processHome := t.TempDir()
		setProcessHome(t, processHome)

		override := t.TempDir()
		path := filepath.Join(override, "mcp.json")
		writeModelConfigFixture(t, path, []byte(validMCPConfigJSON), 0o600)

		specs, err := loadMCPConfig(Config{HomeDir: override})
		if err != nil {
			t.Fatalf("loadMCPConfig(HomeDir override) error = %v", err)
		}
		if len(specs) != 2 {
			t.Fatalf("loadMCPConfig(HomeDir override) specs = %d, want 2", len(specs))
		}

		// The process HOME default (~/.looprig/coderig/mcp.json) was never written, so
		// if loadMCPConfig had ignored HomeDir and fallen back to it instead of
		// honoring the override, this would also return (nil, nil) -- proving
		// the first call above really did read from the override.
		specs, err = loadMCPConfig(Config{})
		if err != nil || specs != nil {
			t.Fatalf("loadMCPConfig(process HOME default) = (%v, %v), want (nil, nil)", specs, err)
		}
	})
}
