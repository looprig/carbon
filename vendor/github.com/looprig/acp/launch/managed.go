// managed.go implements Dial and ManagedClient: the owned/shared model-proxy
// lifecycle around one ACP connection (see contracts.go for the types this
// wires together).
package launch

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/looprig/acp/client"
	"github.com/looprig/acp/transport/stdio"
)

// connCloser is the subset of *client.Client's behavior ManagedClient's own
// lifecycle bookkeeping needs: closing the connection and observing
// unexpected child death. It exists purely as a substitution seam for
// managed_test.go -- *client.Client satisfies it structurally with zero
// coupling, the same idiom ModelProxy uses for a proxy server built
// elsewhere (see contracts.go's doc). Production code (Dial, via
// defaultConnect) always stores a real *client.Client here.
type connCloser interface {
	Close(ctx context.Context) error
	Done() <-chan struct{}
}

// Compile-time proof that *client.Client actually satisfies connCloser.
var _ connCloser = (*client.Client)(nil)

// connectFunc performs Dial's own "spawn and initialize the ACP child"
// step. Factoring it out of dial as a parameter (rather than calling
// client.New/(*client.Client).Dial inline) is what lets managed_test.go
// substitute a fake connCloser and prove dial's ordering/unwind contract
// without ever spawning a real subprocess -- the free-function analogue of
// acp/client's own Client.attemptConnect seam, adapted here because Dial is
// a package function rather than a method with its own instance to hang an
// override off of.
type connectFunc func(ctx context.Context, cmd stdio.Command, opts client.Options) (connCloser, error)

// defaultConnect is connectFunc's real implementation: it spawns cmd as an
// ACP child via acp/client and completes the "initialize" handshake at the
// pinned protocol version (see client.New/client.Options -- acp/client
// itself always negotiates protocol.CurrentProtocolVersion; this package
// does not re-implement or override that).
func defaultConnect(ctx context.Context, cmd stdio.Command, opts client.Options) (connCloser, error) {
	c := client.New(cmd, opts)
	if err := c.Dial(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// ManagedClient is one launched ACP connection together with whatever model
// proxy it owns, if any. Construct with Dial.
type ManagedClient struct {
	conn  connCloser
	owned ModelProxy // nil when this ManagedClient borrows a SharedProxy

	connCloseOnce sync.Once
	connCloseErr  error

	proxyCloseOnce sync.Once
	proxyCloseErr  error
}

// Client returns the underlying acp/client connection, ready for session
// creation. It is nil only for a ManagedClient built around a
// connCloser other than a real *client.Client -- something only
// managed_test.go's fakes ever do; a ManagedClient returned by Dial always
// yields a non-nil *client.Client here.
func (m *ManagedClient) Client() *client.Client {
	c, _ := m.conn.(*client.Client)
	return c
}

// Close closes the ACP connection first and the owned proxy second (never
// the reverse -- see the design doc's "Owned proxy lifecycle"), joining
// both errors with errors.Join so neither is ever silently discarded if
// both fail. Both the connection close and the proxy close are individually
// idempotent at this layer (each guarded by its own sync.Once), so Close is
// safe to call more than once and safe to race against watchOwnedDeath's
// own proxy teardown; whichever reaches a given step first performs it, and
// every other caller observes that same cached result. A SharedProxy
// borrower's Close never touches the shared binding at all: owned is nil in
// that case, so closeOwnedProxy is a no-op.
func (m *ManagedClient) Close(ctx context.Context) error {
	connErr := m.closeConn(ctx)
	proxyErr := m.closeOwnedProxy(ctx)
	return errors.Join(connErr, proxyErr)
}

func (m *ManagedClient) closeConn(ctx context.Context) error {
	m.connCloseOnce.Do(func() {
		m.connCloseErr = m.conn.Close(ctx)
	})
	return m.connCloseErr
}

func (m *ManagedClient) closeOwnedProxy(ctx context.Context) error {
	if m.owned == nil {
		return nil
	}
	m.proxyCloseOnce.Do(func() {
		m.proxyCloseErr = m.owned.Close(ctx)
	})
	return m.proxyCloseErr
}

// watchOwnedDeath closes this ManagedClient's owned proxy the moment its
// ACP connection reaches Done() on its own -- unexpected child death or a
// transport failure, never an explicit Close (which already closes the
// proxy itself) -- so an owned proxy is never left running once nothing
// can use its ACP connection anymore. It is only ever started for an owned
// proxy (see dial): a SharedProxy borrower has no owned proxy to close and
// no watcher goroutine at all. A failed inference request never reaches
// here: this package watches connection/child death only, never
// request-level failures, which are entirely out of its scope.
func (m *ManagedClient) watchOwnedDeath() {
	<-m.conn.Done()
	_ = m.closeOwnedProxy(context.Background())
}

// validateConfig rejects a Config before Dial starts anything: an ordinary
// dial must have exactly one proxy, while a no-proxy dial must have neither
// proxy and must use a HarnessAdapter with an explicit no-proxy method.
func validateConfig(cfg Config) error {
	if cfg.Harness == nil {
		return &ConfigError{Reason: "Harness is required"}
	}
	if cfg.NoProxy {
		if cfg.OwnedProxy != nil || cfg.SharedProxy != nil {
			return &ConfigError{Reason: "NoProxy is mutually exclusive with OwnedProxy and SharedProxy"}
		}
		if !supportsNativeHarness(cfg.Harness) {
			return &ConfigError{Reason: "NoProxy requires a no-proxy harness adapter"}
		}
		return nil
	}

	switch {
	case cfg.OwnedProxy == nil && cfg.SharedProxy == nil:
		return &ConfigError{Reason: "exactly one of OwnedProxy or SharedProxy is required, neither was set"}
	case cfg.OwnedProxy != nil && cfg.SharedProxy != nil:
		return &ConfigError{Reason: "OwnedProxy and SharedProxy are mutually exclusive, both were set"}
	}
	return nil
}

func supportsNativeHarness(h HarnessAdapter) bool {
	if _, ok := h.(NativeHarnessAdapter); ok {
		return true
	}
	_, ok := h.(noProxyHarnessAdapter)
	return ok
}

func configureNative(h HarnessAdapter, cmd stdio.Command) (stdio.Command, error) {
	if native, ok := h.(NativeHarnessAdapter); ok {
		return native.ConfigureNative(cmd)
	}
	if legacy, ok := h.(noProxyHarnessAdapter); ok {
		return legacy.configureWithoutProxy(cmd)
	}
	return stdio.Command{}, &ConfigError{Reason: "NoProxy requires a no-proxy harness adapter"}
}

// Dial validates cfg, starts an owned proxy (if configured), configures
// Command for the resulting ProxyBinding via cfg.Harness or through its
// explicit internal no-proxy connector path, spawns and initializes the ACP
// child through acp/client, and returns a ManagedClient.
//
// Lifecycle order: validate -> start proxy -> configure a copied
// stdio.Command -> spawn ACP child -> initialize ACP -> return
// ManagedClient. A failure at any step after the owned proxy has
// successfully started closes that proxy before Dial returns, so a failed
// Dial never leaks a running owned proxy; a SharedProxy is never started
// in the first place, so there is nothing to unwind for it.
func Dial(ctx context.Context, cfg Config) (*ManagedClient, error) {
	return dial(ctx, cfg, defaultConnect)
}

// DialNative launches an ACP harness using its own authentication and model
// selection. It deliberately constructs the existing Config.NoProxy form and
// sends it through Dial, so all established no-proxy validation and lifecycle
// behavior remains in one place. NativeConfig has no proxy fields, making it
// impossible for this helper to start or borrow a model proxy.
func DialNative(ctx context.Context, cfg NativeConfig) (*ManagedClient, error) {
	return dialNative(ctx, cfg, defaultConnect)
}

func dialNative(ctx context.Context, cfg NativeConfig, connect connectFunc) (*ManagedClient, error) {
	if cfg.Harness == nil {
		return nil, &ConfigError{Reason: "NativeConfig.Harness is required"}
	}
	return dial(ctx, Config{
		NoProxy: true,
		Harness: cfg.Harness,
		Command: cfg.Command,
		Client:  cfg.Client,
	}, connect)
}

func dial(ctx context.Context, cfg Config, connect connectFunc) (*ManagedClient, error) {
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}

	var owned ModelProxy
	var binding ProxyBinding
	if !cfg.NoProxy {
		if cfg.OwnedProxy != nil {
			owned = cfg.OwnedProxy
			if err := owned.Start(ctx); err != nil {
				return nil, fmt.Errorf("acp/launch: start owned proxy: %w", err)
			}
			baseURL, token, ready := owned.Binding()
			if !ready {
				_ = owned.Close(ctx)
				return nil, &ProxyNotReadyError{}
			}
			binding = ProxyBinding{BaseURL: baseURL, Token: token}
		} else {
			binding = *cfg.SharedProxy
		}
	}

	var (
		cmd stdio.Command
		err error
	)
	if cfg.NoProxy {
		cmd, err = configureNative(cfg.Harness, cfg.Command)
	} else {
		cmd, err = cfg.Harness.Configure(cfg.Command, binding)
	}
	if err != nil {
		if owned != nil {
			_ = owned.Close(ctx)
		}
		return nil, fmt.Errorf("acp/launch: configure command: %w", err)
	}

	conn, err := connect(ctx, cmd, cfg.Client)
	if err != nil {
		if owned != nil {
			_ = owned.Close(ctx)
		}
		return nil, fmt.Errorf("acp/launch: dial acp client: %w", err)
	}

	mc := &ManagedClient{conn: conn, owned: owned}
	if owned != nil {
		// #nosec G118 -- watchOwnedDeath deliberately outlives any single
		// request: it must keep watching for connection/child death for the
		// ManagedClient's entire remaining lifetime, not be tied to (and
		// cancelled with) whichever request context happened to be in scope
		// when Dial returned. context.Background() is correct here.
		go mc.watchOwnedDeath()
	}
	return mc, nil
}

// newManagedClientForTest builds a ManagedClient directly around conn/owned,
// bypassing Dial's own proxy-start/configure/connect sequence entirely. It
// exists only for managed_test.go, so Close/closeOwnedProxy/watchOwnedDeath
// can be proven against fakes in isolation from dial's own logic (which has
// its own dedicated tests).
func newManagedClientForTest(conn connCloser, owned ModelProxy) *ManagedClient {
	return &ManagedClient{conn: conn, owned: owned}
}
