package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"unicode/utf8"

	"github.com/looprig/coderig/internal/catalog/generic"
	mcpauth "github.com/looprig/mcp/pkg/auth"
	mcpclient "github.com/looprig/mcp/pkg/client"
)

// mcpConfigFile is the top-level schema of <home>/mcp.json: the exact Claude
// Code mcpServers format plus one looprig extension field (roles) per
// server. See design §1.1
// (docs/plans/2026-08-05-coderig-mcp-and-permission-review-design.md).
type mcpConfigFile struct {
	MCPServers map[string]mcpServerConfig `json:"mcpServers"`
}

// mcpServerConfig is the decoded wire shape of one mcpServers entry. Type is
// optional: when omitted it is inferred from which of Command/URL is
// present. "sse" is never inferred, since both http and sse are keyed by URL
// and there is no way to tell them apart from shape alone -- a caller wants
// sse must say so explicitly.
type mcpServerConfig struct {
	Type    string            `json:"type"`
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	Roles   []string          `json:"roles"`
}

// mcpServerSpec is the validated, immutable result of normalizing one
// mcpServerConfig entry: a binding name that already passed
// mcp/pkg/client.Name's rules, the resolved transport kind, that kind's
// transport configuration (the other kind's fields are left zero), and the
// normalized, sorted role list, always containing Generic's name.
type mcpServerSpec struct {
	name    string
	kind    string
	command string
	args    []string
	env     map[string]string
	url     string
	headers map[string]string
	roles   []string
}

const (
	maxMCPConfigErrorBytes        = 512
	maxMCPConfigErrorBindingBytes = 128
	maxMCPConfigErrorFieldBytes   = 64
	maxMCPConfigErrorCauseBytes   = 256
)

// maxMCPConfigBytes is mcp.json's size cap. It is identical to
// maxModelConfigBytes (models.json's own cap) per the design doc: mcp.json
// may carry the same class of secret (bearer tokens, headers, env values)
// that models.json carries as inline API keys, so it gets the same file
// hygiene, including the same size limit.
const maxMCPConfigBytes = 1 << 20

// MCPConfigError reports an invalid mcp.json binding, following
// *ModelConfigError's bounded, secret-free style: every component is
// truncated at construction, and the assembled message is bounded again in
// Error, so a hostile or oversized binding name, field label, or wrapped
// cause can never produce an unbounded error string. Cause is a
// human-readable reason and must never be a header or env value -- only the
// name of an offending field is ever safe to include.
type MCPConfigError struct {
	// Binding is the server binding name (the mcpServers map key), or "" if
	// no binding was identified yet (a schema-level decode error).
	Binding string
	// Field is the offending field, e.g. "type", "url", "roles[1]".
	Field string
	// Cause is a human-readable, secret-free reason.
	Cause string
}

// Error renders a bounded, secret-free description of the failure.
func (e *MCPConfigError) Error() string {
	binding := boundedModelConfigText(e.Binding, maxMCPConfigErrorBindingBytes)
	field := boundedModelConfigText(e.Field, maxMCPConfigErrorFieldBytes)
	cause := boundedModelConfigText(e.Cause, maxMCPConfigErrorCauseBytes)

	message := "coderig: mcp configuration"
	if binding != "" {
		message += " " + binding
	}
	if field != "" {
		message += " field " + field
	}
	if cause != "" {
		message += ": " + cause
	} else {
		message += ": invalid"
	}
	return boundedModelConfigText(message, maxMCPConfigErrorBytes)
}

// mcpConfigFailure builds an *MCPConfigError with every component bounded at
// construction, matching modelConfigFailure's discipline.
func mcpConfigFailure(binding, field string, cause error) *MCPConfigError {
	reason := "unknown error"
	if cause != nil {
		reason = cause.Error()
	}
	return &MCPConfigError{
		Binding: boundedModelConfigText(binding, maxMCPConfigErrorBindingBytes),
		Field:   boundedModelConfigText(field, maxMCPConfigErrorFieldBytes),
		Cause:   boundedModelConfigText(reason, maxMCPConfigErrorCauseBytes),
	}
}

// decodeMCPConfig strictly decodes raw mcp.json bytes: valid UTF-8, no
// duplicate object keys at any depth, no unknown fields, exactly one
// top-level JSON value. It performs no validation beyond shape -- binding
// names, transport-kind inference/agreement, URLs, and roles are
// normalizeMCPConfig's job. It never reads from disk; that, plus file
// hygiene, is a later task's responsibility.
func decodeMCPConfig(data []byte) (mcpConfigFile, error) {
	var config mcpConfigFile
	if !utf8.Valid(data) {
		return config, mcpConfigFailure("", "", errors.New("input is not valid UTF-8"))
	}
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return config, mcpConfigFailure("", "", err)
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return mcpConfigFile{}, mcpConfigFailure("", "", safeMCPConfigDecodeError(err))
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return mcpConfigFile{}, mcpConfigFailure("", "", errors.New("multiple top-level JSON values"))
		}
		return mcpConfigFile{}, mcpConfigFailure("", "", safeMCPConfigDecodeError(err))
	}
	return config, nil
}

func safeMCPConfigDecodeError(err error) error {
	if errors.Is(err, io.EOF) {
		return errors.New("mcp configuration JSON is empty")
	}
	return errors.New("invalid JSON mcp configuration")
}

// normalizeMCPConfig validates a decoded mcpConfigFile and produces the
// immutable spec list, sorted by binding name for deterministic output, that
// a later task's assembly step consumes. An absent or empty mcpServers map
// is valid and yields zero specs.
func normalizeMCPConfig(config mcpConfigFile) ([]mcpServerSpec, error) {
	if len(config.MCPServers) == 0 {
		return nil, nil
	}

	names := make([]string, 0, len(config.MCPServers))
	for name := range config.MCPServers {
		names = append(names, name)
	}
	sort.Strings(names)

	specs := make([]mcpServerSpec, 0, len(names))
	for _, name := range names {
		spec, err := normalizeMCPServer(name, config.MCPServers[name])
		if err != nil {
			return nil, err
		}
		specs = append(specs, spec)
	}
	return specs, nil
}

// normalizeMCPServer validates one binding and produces its spec.
//
// Cross-contamination of incidental fields (e.g. a non-empty env on an http
// server, or headers on a stdio server) is silently ignored rather than
// rejected: the design's schema section documents which fields apply to
// which kind but says nothing about erroring on the others being present,
// and only the fields that apply to the resolved kind are ever copied into
// the spec, so a stray field can never reach transport construction.
func normalizeMCPServer(name string, cfg mcpServerConfig) (mcpServerSpec, error) {
	if err := mcpclient.Name(name).Validate(); err != nil {
		return mcpServerSpec{}, mcpConfigFailure(name, "name", err)
	}

	kind, err := resolveMCPServerKind(name, cfg)
	if err != nil {
		return mcpServerSpec{}, err
	}
	if kind == "http" || kind == "sse" {
		// mcpauth.CanonicalOrigin is the exact function the real HTTP
		// transports use at connect time (via the internal
		// httpsec.ResolveEndpoint wrapper), so this config-time check can
		// never drift from the transport's actual behavior. Its error text
		// is already bounded and secret-free by the auth package's own
		// contract (it never echoes header/query values), so wrapping it
		// as the Cause here is safe.
		if _, err := mcpauth.CanonicalOrigin(cfg.URL); err != nil {
			return mcpServerSpec{}, mcpConfigFailure(name, "url", err)
		}
	}

	roles, err := normalizeMCPServerRoles(name, cfg.Roles)
	if err != nil {
		return mcpServerSpec{}, err
	}

	spec := mcpServerSpec{name: name, kind: kind, roles: roles}
	switch kind {
	case "stdio":
		spec.command = cfg.Command
		spec.args = append([]string(nil), cfg.Args...)
		spec.env = copyMCPServerStringMap(cfg.Env)
	default: // "http", "sse"
		spec.url = cfg.URL
		spec.headers = copyMCPServerStringMap(cfg.Headers)
	}
	return spec, nil
}

// resolveMCPServerKind implements the type inference and agreement rules:
// Type == "" infers stdio from Command or http from URL (never sse, which
// must always be explicit since it shares URL with http); an explicit Type
// must agree with which of Command/URL is present.
func resolveMCPServerKind(name string, cfg mcpServerConfig) (string, error) {
	hasCommand := cfg.Command != ""
	hasURL := cfg.URL != ""

	if cfg.Type == "" {
		switch {
		case hasCommand && !hasURL:
			return "stdio", nil
		case hasURL && !hasCommand:
			return "http", nil
		default:
			return "", mcpConfigFailure(name, "type", errors.New(
				"type is omitted; exactly one of command or url must be set to infer it"))
		}
	}

	switch cfg.Type {
	case "stdio":
		if !hasCommand || hasURL {
			return "", mcpConfigFailure(name, "type", errors.New(
				`type "stdio" requires command and forbids url`))
		}
		return "stdio", nil
	case "http", "sse":
		if !hasURL || hasCommand {
			return "", mcpConfigFailure(name, "type", fmt.Errorf(
				"type %q requires url and forbids command", cfg.Type))
		}
		return cfg.Type, nil
	default:
		// cfg.Type is an operator-chosen (or attacker-chosen but not
		// secret) short enum value, not a credential -- naming it back is
		// the same convention Task 4's review established for aliases, and
		// mcpConfigFailure still bounds it.
		return "", mcpConfigFailure(name, "type", fmt.Errorf(
			"unknown type %q, want stdio, http, or sse", cfg.Type))
	}
}

// normalizeMCPServerRoles validates roles against the closed Generic set,
// rejects unknown values and duplicates, and returns a sorted copy.
// Nil/empty input resolves directly to the sole Generic role.
func normalizeMCPServerRoles(name string, roles []string) ([]string, error) {
	if len(roles) == 0 {
		return []string{string(generic.Name)}, nil
	}
	seen := make(map[string]struct{}, len(roles))
	normalized := make([]string, 0, len(roles))
	for i, role := range roles {
		field := fmt.Sprintf("roles[%d]", i)
		if role != string(generic.Name) {
			return nil, mcpConfigFailure(name, field, fmt.Errorf(
				"unknown role %q, want generic", role))
		}
		if _, duplicate := seen[role]; duplicate {
			return nil, mcpConfigFailure(name, field, fmt.Errorf(
				"duplicate role %q", role))
		}
		seen[role] = struct{}{}
		normalized = append(normalized, role)
	}
	sort.Strings(normalized)
	return normalized, nil
}

func copyMCPServerStringMap(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]string, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// loadMCPConfig loads, validates, and normalizes <home>/mcp.json, where home
// is looprigHome(cfg)'s result (honoring Config.HomeDir the same way every
// other CodeRig config file under the looprig home does). An absent file
// means the mcp.json feature is off entirely: (nil, nil), not an error --
// the same "absence is not failure" contract loadProductionModels applies to
// models.json. File hygiene is identical to models.json's (see
// readHygienicConfigFile), because mcp.json headers/env may carry
// credentials just as models.json's inline api_key fields do.
func loadMCPConfig(cfg Config) ([]mcpServerSpec, error) {
	home, err := looprigHome(cfg)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(home, "mcp.json")

	data, exists, err := readMCPConfigFile(path)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}

	file, err := decodeMCPConfig(data)
	if err != nil {
		return nil, err
	}
	return normalizeMCPConfig(file)
}

// readMCPConfigFile applies mcp.json's file hygiene: it shares
// readHygienicConfigFile and openModelConfigNoFollow with models.json (both
// are generic despite their model-scoped names -- see confighygiene.go and
// modelconfig_open_unix.go) and supplies its own byte cap and its own typed
// *MCPConfigError, matching modelConfigFailure's discipline for the
// equivalent models.json path.
func readMCPConfigFile(path string) ([]byte, bool, error) {
	return readHygienicConfigFile(path, maxMCPConfigBytes, openModelConfigNoFollow, func(op string, cause error) error {
		return mcpConfigFailure("", "", fmt.Errorf("%s: %w", op, cause))
	})
}
