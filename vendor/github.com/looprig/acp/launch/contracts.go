// Package launch supervises a foreign ACP agent's process together with an
// inference model proxy that stands in for a real upstream model provider:
// it starts (or borrows) a proxy, points a stdio.Command at its binding via
// a HarnessAdapter, spawns and initializes the ACP child through
// acp/client, and tears both down in the right order.
//
// This package sits alongside acp/client in this module's wire layer (see
// acp/CLAUDE.md): it imports acp/client, acp/transport/stdio, and
// acp/protocol (for the wire types acp/client's own public API already
// exposes, e.g. protocol.SessionConfigOption), but never Harness, Core, or
// inference, directly or transitively. It exists independently of any
// particular model-proxy implementation: ModelProxy's Binding method
// returns a bare (string, string, bool) tuple rather than a ProxyBinding
// value specifically so a real proxy server built elsewhere (for example
// inference/gateway.Server) can satisfy it structurally, with zero
// compile-time coupling to this package.
package launch

import (
	"context"

	"github.com/looprig/acp/client"
	"github.com/looprig/acp/transport/stdio"
)

// ProxyBinding is the connection info a model proxy exposes to whatever ACP
// adapter it fronts: a local base URL and bearer token the adapter's
// upstream-model client should be pointed at instead of a real provider.
//
// It is plain data, never a lifecycle handle: a SharedProxy is borrowed (see
// Config.SharedProxy's doc), and *ProxyBinding deliberately has no Start or
// Close method at all, so there is no call path by which this package could
// ever start or stop a proxy it does not own.
type ProxyBinding struct {
	BaseURL string
	Token   string
}

// ModelProxy is a model proxy this package owns: started before the ACP
// child is spawned and closed when the resulting ManagedClient closes or
// its ACP child dies unexpectedly (see ManagedClient's doc).
//
// Binding's three-plain-value return shape (rather than returning a
// ProxyBinding) is deliberate, not an oversight: Go requires identical
// method signatures for structural interface satisfaction, and a bare
// (string, string, bool) tuple is far more likely to already be exactly
// what an independently-built proxy server type naturally exposes than a
// package-specific struct type would be. That is what lets such a type
// satisfy ModelProxy with no import of, or coupling to, this package at
// all.
type ModelProxy interface {
	// Start brings the proxy up. It must return before Binding is called.
	Start(context.Context) error
	// Binding reports the proxy's current connection info. ready is false
	// until the proxy has something callers can actually use; Dial treats a
	// false ready immediately after a successful Start as a startup-contract
	// violation (see ProxyNotReadyError) rather than proceeding with an
	// undefined binding.
	Binding() (baseURL, token string, ready bool)
	// Close tears the proxy down. Implementations must make Close safe to
	// call at most once in practice; ManagedClient itself also never calls
	// it more than once for a given owned proxy (see ManagedClient.Close's
	// doc), but a well-behaved ModelProxy should not depend on that.
	Close(context.Context) error
}

// HarnessAdapter configures a stdio.Command for one specific foreign ACP
// agent's launch contract -- its executable, argument shape, and
// environment variables -- given the ProxyBinding it should be pointed at.
//
// Configure must return a fresh Command derived from cmd, never mutate cmd
// (or its Env/Args backing arrays) in place, and must never inherit the
// ambient process environment or forward upstream provider credentials
// (see the design doc's "Environment safety" and acp/CLAUDE.md's own rule
// to the same effect).
type HarnessAdapter interface {
	Configure(stdio.Command, ProxyBinding) (stdio.Command, error)
}

// NativeHarnessAdapter configures a child to use the harness's own
// authentication and model selection. ConfigureNative must preserve the
// same command-copying, absolute-path, environment, and credential-safety
// rules as Configure. Explicit native model selection may produce a
// caller-selected model override (for example, Codex `-c model`), while
// ConfigureNative must never add gateway URL, gateway token, or gateway
// provider/base-url overrides. When the model is omitted, model selection
// remains harness-managed.
//
// It embeds HarnessAdapter so one connector has an explicit, reviewable
// gateway path and native path rather than an ambiguous Configure call whose
// behavior depends on the binding value.
type NativeHarnessAdapter interface {
	HarnessAdapter
	ConfigureNative(stdio.Command) (stdio.Command, error)
}

// noProxyHarnessAdapter is the internal launch seam for connectors that can
// configure a child without gateway URL, token, or provider overrides. It is
// retained for compatibility with the original internal Config.NoProxy path;
// new callers should use NativeHarnessAdapter and DialNative.
type noProxyHarnessAdapter interface {
	configureWithoutProxy(stdio.Command) (stdio.Command, error)
}

// Config is Dial's complete configuration.
type Config struct {
	// OwnedProxy, if set, is started before the ACP child is spawned and
	// closed when the resulting ManagedClient closes, or when its ACP child
	// dies unexpectedly. Mutually exclusive with SharedProxy: exactly one of
	// the two is required (see validateConfig).
	OwnedProxy ModelProxy
	// SharedProxy, if set, is borrowed data only: this package never starts
	// or closes it. The application that created the shared proxy owns its
	// lifecycle and is responsible for closing it once every borrower is
	// finished; this package has no opinion about that and, since
	// *ProxyBinding carries no lifecycle methods at all, no way to act on it
	// even if it wanted to. Mutually exclusive with OwnedProxy.
	SharedProxy *ProxyBinding
	// NoProxy explicitly selects a child-owned authentication path. It is
	// mutually exclusive with OwnedProxy and SharedProxy and requires Harness
	// to implement either NativeHarnessAdapter or the legacy internal no-proxy
	// connector seam. New callers should prefer DialNative.
	NoProxy bool
	// Harness configures Command for whichever ProxyBinding results from
	// OwnedProxy or SharedProxy.
	Harness HarnessAdapter
	// Command is the base stdio.Command Harness.Configure adapts. Its
	// executable Path is the caller's responsibility to supply (this
	// package never performs PATH lookup, invokes a shell, or installs
	// anything).
	Command stdio.Command
	// Client is passed through unchanged to the underlying acp/client
	// connection.
	Client client.Options
}

// NativeConfig is the proxy-free configuration accepted by DialNative. Its
// shape deliberately has no OwnedProxy, SharedProxy, or binding field, so a
// native launch cannot accidentally acquire gateway lifecycle or credentials.
type NativeConfig struct {
	Harness NativeHarnessAdapter
	Command stdio.Command
	Client  client.Options
}
