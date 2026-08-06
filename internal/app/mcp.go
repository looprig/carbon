package app

import (
	"fmt"
	"sort"

	mcpauth "github.com/looprig/mcp/pkg/auth"
	mcpclient "github.com/looprig/mcp/pkg/client"
	mcpharness "github.com/looprig/mcp/pkg/harness"
	"github.com/looprig/mcp/pkg/transport/sse"
	"github.com/looprig/mcp/pkg/transport/stdio"
	"github.com/looprig/mcp/pkg/transport/streamablehttp"
)

// mcpEnvPassThrough is the fixed, minimal baseline of this process's
// environment a stdio MCP server child may inherit, if a given name happens
// to be set. Nothing else in this process's environment reaches a child by
// default -- design §1.5's fail-closed posture for a server's environment: a
// server that needs a credential must receive it explicitly, via
// mcpServerSpec.env (mcpEnvVarsFrom), never by inheriting this process's
// wider environment.
var mcpEnvPassThrough = []string{"PATH", "HOME", "TMPDIR", "LANG", "LC_ALL"}

// mcpAllRoles is internal/catalog's three fixed loop identities -- the exact
// loop names a binding's Visibility selects by (design §1.2) -- in the order
// an omitted or empty mcpServerSpec.roles defaults to.
var mcpAllRoles = []string{"planner", "builder", "reviewer"}

// mcpDefinitions turns a validated spec list (loadMCPConfig's result) into
// ready-to-hand-to-Manager bindings. It is pure construction: no network
// call and no long-lived process are started here. transport.New only
// validates configuration -- for stdio that includes resolving the command
// via exec.LookPath -- and a later task's Manager.Start is what actually
// connects.
//
// A construction failure on any one spec aborts the whole batch immediately
// and returns (nil, err): mcpDefinitions never returns a partial slice
// alongside an error, matching this feature's fail-closed posture
// throughout (design §1.5). Specs are consumed in the order given; Task 7's
// normalizeMCPConfig already returns them sorted by binding name, so the
// result is deterministic for free.
func mcpDefinitions(specs []mcpServerSpec) ([]mcpharness.Binding, error) {
	bindings := make([]mcpharness.Binding, 0, len(specs))
	for _, spec := range specs {
		binding, err := mcpBindingFor(spec)
		if err != nil {
			return nil, err
		}
		bindings = append(bindings, binding)
	}
	return bindings, nil
}

// mcpBindingFor builds and validates one spec's Binding.
//
// Both failure points here -- transport construction and Binding.Validate --
// already return errors from the mcp module's own *client.Error family,
// which is secret-free and bounded by that module's own discipline. They
// are wrapped in coderig's own *MCPConfigError anyway, naming spec.name as
// Binding, for two reasons: transport.New's own errors are built with an
// empty Binding (every transport's New always calls client.NewError with
// binding ""), so without this wrap a caller could not tell which server
// failed at all; and wrapping both failure points the same way keeps this
// feature's whole error family -- Tasks 6, 7, and this one -- consistent,
// rather than having some mcp.json failures carry a typed, bounded
// *MCPConfigError and others carry a raw *client.Error the rest of the
// package does not otherwise handle.
func mcpBindingFor(spec mcpServerSpec) (mcpharness.Binding, error) {
	factory, err := mcpTransportFor(spec)
	if err != nil {
		return mcpharness.Binding{}, mcpConfigFailure(spec.name, "transport", err)
	}

	binding := mcpharness.Binding{
		Name: spec.name,
		Server: mcpclient.Definition{
			Name:      mcpclient.Name(spec.name),
			Transport: factory,
			Compat:    mcpCompatFor(spec.kind),
		},
		Scope:      mcpharness.ScopeSession,
		Visibility: mcpharness.Named(mcpVisibilityRoles(spec.roles)...),
		Required:   false,
	}
	if err := binding.Validate(); err != nil {
		return mcpharness.Binding{}, mcpConfigFailure(spec.name, "binding", err)
	}
	return binding, nil
}

// mcpCompatFor resolves the compatibility profile a spec's Definition needs.
//
// This is a deviation from a literal "zero Timeouts/Limits/Compat = defaults"
// reading of the task: client.Definition.Validate's checkTransportCompat
// (mcp/pkg/client/compat.go) unconditionally rejects Transport.Kind() ==
// "sse" unless Compat.Permits(TolerateLegacySSE) -- and a zero Compat
// normalizes to ProfileDefault, which deliberately excludes that tolerance
// ("Legacy SSE is not in [ProfileDefault]: a transport is a deliberate
// choice, and a binding should not acquire an older wire protocol by
// default", per that file's own doc comment). Left at zero, every "sse"
// spec would fail Binding.Validate every time, which would make Task 6/7's
// already-shipped "sse" server kind entirely non-constructible here.
//
// The deliberateness ProfileDefault is guarding against is already
// satisfied one layer up: normalizeMCPServer's own comment records that
// "sse" is never inferred and a caller wanting it "must say so explicitly"
// -- so an mcp.json entry with "type": "sse" already IS the explicit,
// on-purpose choice the mcp module's Compat gate exists to require. Every
// other tolerance ProfileLegacy carries is identical to ProfileDefault's;
// it adds exactly TolerateLegacySSE and nothing else. stdio and http keep
// the zero value (ProfileDefault), unchanged from the task's instruction.
func mcpCompatFor(kind string) mcpclient.Profile {
	if kind == "sse" {
		return mcpclient.ProfileLegacy
	}
	return mcpclient.Profile{}
}

// mcpTransportFor builds the transport factory for one spec, per its kind.
// spec.kind is already validated to be exactly one of "stdio", "http", or
// "sse" by normalizeMCPServer (Task 6); the default case is defense in
// depth, not a reachable path for a spec that actually came from
// loadMCPConfig.
func mcpTransportFor(spec mcpServerSpec) (mcpclient.TransportFactory, error) {
	switch spec.kind {
	case "stdio":
		return stdio.New(stdio.Config{
			Command: spec.command,
			Args:    spec.args,
			Env: stdio.EnvAllowlist{
				PassThrough: mcpEnvPassThrough,
				Vars:        mcpEnvVarsFrom(spec.env),
			},
		})
	case "http":
		return streamablehttp.New(streamablehttp.Config{
			Endpoint: spec.url,
			Headers:  mcpHeadersFrom(spec.headers),
			// HTTPClient stays nil: the transport builds its own from
			// Timeouts. A shared session HTTP client would be refused here
			// anyway (New rejects a non-zero Timeout, which severs the
			// streams this transport is built on).
			// Timeouts stays zero: defaults.
		})
	case "sse":
		return sse.New(sse.Config{
			Endpoint: spec.url,
			Headers:  mcpHeadersFrom(spec.headers),
		})
	default:
		return nil, fmt.Errorf("unknown transport kind %q", spec.kind)
	}
}

// mcpEnvVarsFrom converts a spec's env map into the explicit []stdio.Var the
// transport's EnvAllowlist wants, sorted by name. The sort is not tidiness:
// spec.env is a Go map, whose iteration order is randomized per run, and an
// unsorted result would make binding construction -- and later, a
// fingerprint digest built over it -- nondeterministic across runs even
// though the map's content never changed. Empty input returns nil, matching
// copyMCPServerStringMap's convention elsewhere in this package.
func mcpEnvVarsFrom(env map[string]string) []stdio.Var {
	if len(env) == 0 {
		return nil
	}
	names := make([]string, 0, len(env))
	for name := range env {
		names = append(names, name)
	}
	sort.Strings(names)

	vars := make([]stdio.Var, 0, len(names))
	for _, name := range names {
		vars = append(vars, stdio.Var{Name: name, Value: env[name]})
	}
	return vars
}

// mcpHeadersFrom converts a spec's headers map into the []auth.Header the
// http/sse transports want, sorted by name for the same determinism reason
// mcpEnvVarsFrom is. auth.Header's fields are private, so NewHeader is the
// only way to construct one; New validates each header itself (Validate),
// so this function does not duplicate that check.
func mcpHeadersFrom(headers map[string]string) []mcpauth.Header {
	if len(headers) == 0 {
		return nil
	}
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)

	result := make([]mcpauth.Header, 0, len(names))
	for _, name := range names {
		result = append(result, mcpauth.NewHeader(name, headers[name]))
	}
	return result
}

// mcpVisibilityRoles resolves a spec's roles to the set a Binding's
// Visibility is built from: empty/nil (normalizeMCPServerRoles's "not yet
// resolved" sentinel) means all three fixed loop identities, and a
// non-empty list -- already sorted and deduplicated by Task 6 -- is used as
// given.
func mcpVisibilityRoles(specRoles []string) []string {
	if len(specRoles) == 0 {
		return mcpAllRoles
	}
	return specRoles
}
