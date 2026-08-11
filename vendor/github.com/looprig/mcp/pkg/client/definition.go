// This file defines Definition, the immutable, secret-free configuration for
// one MCP server connection, and the types it is composed of. Credentials
// never live in a Definition — they arrive via providers at connect time. A
// Definition is treated as immutable after Validate returns nil; validation
// fails closed with FailureInvalidConfig.

package client

import (
	"context"
	"fmt"
	"slices"

	"github.com/looprig/mcp/internal/protocol"
)

// MaxNameBytes is the maximum length of a Name in bytes.
const MaxNameBytes = 64

// Name identifies a server binding (the configured name a server is mounted
// under). A valid Name is 1..MaxNameBytes bytes of [a-z0-9_-] and starts
// with a lowercase letter or digit (never '-' or '_').
type Name string

// Validate reports whether n is a well-formed binding name. Violations are
// returned as *Error with class FailureInvalidConfig.
func (n Name) Validate() error {
	fail := func(msg string) error {
		return NewError(FailureInvalidConfig, n, "validate", msg, nil)
	}
	if n == "" {
		return fail("name is empty")
	}
	if len(n) > MaxNameBytes {
		return fail(fmt.Sprintf("name is %d bytes, max %d", len(n), MaxNameBytes))
	}
	for i := 0; i < len(n); i++ {
		c := n[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case (c == '-' || c == '_') && i > 0:
		default:
			return fail(fmt.Sprintf("name contains invalid byte 0x%02x at index %d (want [a-z0-9], plus [-_] after the first byte)", c, i))
		}
	}
	return nil
}

// ClientCapabilities declares which optional client-side MCP capabilities the
// caller is prepared to serve for this connection. Everything defaults to
// off: a capability the host cannot actually honor must not be advertised.
type ClientCapabilities struct {
	// Elicitation lets the server ask the human for input mid-operation. The
	// server-initiated form of this feature is deprecated in spec revision
	// 2026-07-28 (SEP-2577) and served via multi-round-trip requests there; it
	// remains fully functional against peers negotiating ≤2025-11-25.
	Elicitation bool
	// Sampling lets the server request LLM completions from the client. The
	// server-initiated form of this feature is deprecated in spec revision
	// 2026-07-28 (SEP-2577) and served via multi-round-trip requests there; it
	// remains fully functional against peers negotiating ≤2025-11-25.
	Sampling bool
	// Roots exposes filesystem roots to the server. The server-initiated form
	// of this feature is deprecated in spec revision 2026-07-28 (SEP-2577)
	// and served via multi-round-trip requests there; it remains fully
	// functional against peers negotiating ≤2025-11-25.
	Roots bool
}

// ToolFilter restricts which server tools are visible and callable. Entries
// are exact raw tool names (no globs, case-sensitive). An empty Allow set
// allows every tool; Deny always wins over Allow.
type ToolFilter struct {
	// Allow, when non-empty, is the complete set of permitted raw names.
	Allow []string
	// Deny lists raw names that are always rejected.
	Deny []string
}

// Permits reports whether rawName passes the filter: denied names never
// pass; otherwise an empty Allow set passes everything, and a non-empty
// Allow set passes only its members.
func (f ToolFilter) Permits(rawName string) bool {
	if slices.Contains(f.Deny, rawName) {
		return false
	}
	if len(f.Allow) == 0 {
		return true
	}
	return slices.Contains(f.Allow, rawName)
}

// validate reports the first malformed entry: entries must be non-empty and
// unique within their set (Allow and Deny may intersect — Deny wins).
func (f ToolFilter) validate() error {
	for _, set := range []struct {
		name    string
		entries []string
	}{
		{"ToolFilter.Allow", f.Allow},
		{"ToolFilter.Deny", f.Deny},
	} {
		seen := make(map[string]struct{}, len(set.entries))
		for i, entry := range set.entries {
			if entry == "" {
				return fmt.Errorf("%s: empty entry at index %d", set.name, i)
			}
			if _, dup := seen[entry]; dup {
				return fmt.Errorf("%s: duplicate entry at index %d", set.name, i)
			}
			seen[entry] = struct{}{}
		}
	}
	return nil
}

// clone returns a deep copy (fresh backing arrays for both slices).
func (f ToolFilter) clone() ToolFilter {
	return ToolFilter{
		Allow: slices.Clone(f.Allow),
		Deny:  slices.Clone(f.Deny),
	}
}

// TransportFactory produces connections for one configured transport (stdio,
// streamable HTTP, ...). It is exported but sealed by construction: Connect's
// signature uses internal/protocol types, so only packages inside this module
// can implement it.
type TransportFactory interface {
	// Kind names the transport, e.g. "stdio", "streamablehttp", "sse".
	Kind() string
	// RedactedOrigin returns a safe display origin. It must never contain
	// credentials.
	RedactedOrigin() string
	// Connect establishes one connection using cfg.
	Connect(ctx context.Context, cfg protocol.ConnectConfig) (protocol.Conn, error)
}

// Definition is the immutable, secret-free configuration for one MCP server
// connection. Zero Timeouts/Limits fields mean "use the default" and are
// filled in by normalized() at connect time; Validate never mutates. Treat a
// Definition as immutable once Validate has returned nil.
type Definition struct {
	// Name is the binding this server is mounted under.
	Name Name
	// Transport produces connections. Required.
	Transport TransportFactory
	// Timeouts holds per-connection deadlines; zero fields select defaults.
	Timeouts Timeouts
	// Limits bounds resource consumption; zero fields select defaults.
	Limits Limits
	// Capabilities declares the optional client capabilities to advertise.
	Capabilities ClientCapabilities
	// ToolFilter restricts which tools are visible and callable.
	ToolFilter ToolFilter
	// AllowParallelCalls opts in to bounded parallel tool calls.
	AllowParallelCalls bool
	// Compat is the named, versioned compatibility profile: how far this
	// binding will bend for a server that does not implement the specification
	// perfectly. The zero value selects ProfileDefault.
	//
	// It is part of the binding's secret-free configuration identity; see
	// Profile.Digest.
	Compat Profile
	// Reconnect governs rebuilding a connection that failed transiently. The
	// zero value reconnects under the default bounds.
	Reconnect ReconnectPolicy
	// Refresh bounds the retries of a catalog refresh that failed. Zero fields
	// select their defaults.
	//
	// A refresh is triggered by the server announcing a change to one of its
	// lists (see Client.Candidate). Failing one costs the binding its view of
	// that change, never its adopted catalog, so the policy here decides how
	// hard a binding tries to catch up — not whether it stays usable.
	Refresh RetryPolicy
	// LogLevel is the minimum severity of server log message to request. Zero
	// means DefaultLogLevel.
	//
	// It only takes effect when Handlers.Log is installed and the server
	// advertises logging: a server sends nothing until a level is set, so this
	// is what turns its logs on, and there is no point turning them on with
	// nowhere to deliver them.
	LogLevel LogLevel
}

// DefaultLogLevel is the level requested when Definition.LogLevel is empty.
//
// Info rather than Debug: a binding that installed a log handler wants to know
// what the server is doing, not to receive its trace output — and the debug
// stream of a chatty server is a volume problem that arrives on the
// connection's notification goroutine. A host that wants more asks for it.
const DefaultLogLevel = LogInfo

// Validate checks the whole definition and fails closed: the first violation
// is returned as a *Error with class FailureInvalidConfig, binding d.Name,
// and op "validate", naming the offending field. It never mutates d.
func (d Definition) Validate() error {
	if err := d.Name.Validate(); err != nil {
		return err
	}
	if d.Transport == nil {
		return NewError(FailureInvalidConfig, d.Name, "validate", "Transport is nil", nil)
	}
	// The profile is validated before the transport check that consults it: an
	// unusable profile must be reported as itself, not as whatever it happens to
	// permit.
	compat := d.Compat
	if compat.isZero() {
		compat = ProfileDefault
	}
	if err := compat.validate(); err != nil {
		return NewError(FailureInvalidConfig, d.Name, "validate", err.Error(), nil)
	}
	d.Compat = compat
	for _, err := range []error{
		d.checkTransportCompat(),
		d.Timeouts.validate(),
		d.Limits.validate(),
		d.ToolFilter.validate(),
		d.LogLevel.validate(),
		d.Refresh.validate("Refresh"),
		d.Reconnect.validate("Reconnect"),
	} {
		if err != nil {
			return NewError(FailureInvalidConfig, d.Name, "validate", err.Error(), nil)
		}
	}
	return nil
}

// normalized returns a copy ready for use: zero Timeouts/Limits fields are
// replaced by their defaults and the ToolFilter slices are deep-copied so
// the copy is detached from caller-held backing arrays. Connect (a later
// task) uses this; Validate does not.
func (d Definition) normalized() Definition {
	d.Timeouts = d.Timeouts.withDefaults()
	d.Limits = d.Limits.withDefaults()
	d.ToolFilter = d.ToolFilter.clone()
	d.Refresh = d.Refresh.withDefaults()
	d.Reconnect = d.Reconnect.withDefaults()
	if d.Compat.isZero() {
		d.Compat = ProfileDefault
	}
	d.Compat = d.Compat.clone()
	if d.LogLevel == "" {
		d.LogLevel = DefaultLogLevel
	}
	return d
}
