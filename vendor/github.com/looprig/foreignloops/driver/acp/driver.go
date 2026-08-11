package acp

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/looprig/acp/client"
	"github.com/looprig/acp/launch"
	"github.com/looprig/acp/protocol"
	"github.com/looprig/acp/transport/stdio"
	"github.com/looprig/foreignloops/driver"
)

// session is the portion of an ACP session needed while the driver is being
// constructed. The concrete client.Session satisfies it; keeping the seam
// here lets construction tests use an in-process session without spawning an
// adapter process.
type session interface {
	ID() protocol.SessionID
	ConfigOptions() []protocol.SessionConfigOption
	Modes() *protocol.SessionModeState
	SetConfigOption(context.Context, protocol.SessionConfigID, protocol.SessionConfigValueID) error
	SetMode(context.Context, protocol.SessionModeID) error
}

type steerSession interface {
	session
	Steer(context.Context, client.SteerParams) (client.SteerResult, error)
}

type asyncSteerSession interface {
	session
	StartSteer(context.Context, client.SteerParams) *client.SteerHandle
}

// acpClient is the minimum client capability needed to create a driver
// session. LoadSession is deliberately optional: a caller cannot resume
// against a client that does not provide that capability.
type acpClient interface {
	NewSession(context.Context, client.NewSessionParams) (session, error)
}

type initializeMetadataProvider interface {
	InitializeMetadata() (client.InitializeMetadata, error)
}

type sessionLoader interface {
	LoadSession(context.Context, client.LoadSessionParams) (session, error)
}

// dialedClient owns the long-lived ACP process and connection returned by a
// dial. The production adapter below wraps launch.ManagedClient; tests replace
// dial with an in-process fake.
type dialedClient interface {
	client() acpClient
	close(context.Context) error
}

type dialFunc func(context.Context, launch.Config) (dialedClient, error)

var dial dialFunc = dialLaunch

type nativeDialFunc func(context.Context, launch.NativeConfig) (dialedClient, error)

var nativeDial nativeDialFunc = dialNativeLaunch

// claudeConnector is the session-level portion of launch.ClaudeConnector.
// launch's public methods accept *client.Session, so production uses the
// adapter below while construction tests can use a narrow in-process fake.
type claudeConnector interface {
	SelectDefaultModel(context.Context, session) error
	SelectSmallModel(context.Context, session) error
	SelectEffort(context.Context, session) error
	ApplyPermissionMode(context.Context, session, protocol.SessionModeID) error
}

type claudeConnectorFactory func(launch.ClaudeModels, string) claudeConnector

var newClaudeConnector claudeConnectorFactory = func(models launch.ClaudeModels, effort string) claudeConnector {
	connector := launch.ClaudeCode(models)
	connector.Effort = effort
	return &realClaudeConnector{connector: connector}
}

// codexConnector is the session-level portion of launch.CodexConnector.
// launch's public methods accept *client.Session, so production uses the
// adapter below while construction tests can use a narrow in-process fake.
type codexConnector interface {
	SelectModel(context.Context, session) error
	SelectEffort(context.Context, session) error
}

type codexConnectorFactory func(string, string) codexConnector

var newCodexConnector codexConnectorFactory = func(model, effort string) codexConnector {
	connector := launch.Codex("")
	switch {
	case model == "" && effort == "":
		// Leave both selectors empty so native Codex remains harness-managed.
	case effort == "":
		connector = connector.WithModel(model)
	default:
		connector = connector.WithModelEffort(model, effort)
	}
	return &realCodexConnector{connector: connector}
}

// Driver owns one launch.Dial result and one ACP session for its entire
// lifetime. Turns will use this session in later driver work; construction
// never creates a per-turn client or process.
type Driver struct {
	owned   dialedClient
	session session

	agentSessionID string
	// driverCtx is rooted independently of New's setup context and is canceled
	// only when the driver closes. Turns use it for Prompt and session/Cancel.
	driverCtx    context.Context
	driverCancel context.CancelFunc
	turnMu       sync.Mutex
	closed       bool
	steeringMu   sync.Mutex
	steeringOn   bool
	steeringOff  bool
	activeMu     sync.Mutex
	active       *turnHandle

	closeOnce sync.Once
	closeErr  error
}

// New validates cfg, launches one ACP client/process, creates or loads one
// session, and applies the harness-specific model and posture before
// returning. Any failure after dialing closes the owned launch result before
// returning.
func New(ctx context.Context, cfg Config) (*Driver, error) {
	cfg.McpServers = cloneMcpServers(cfg.McpServers)
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	driverCtx, driverCancel := context.WithCancel(context.WithoutCancel(ctx))
	driverOwnsContext := false
	defer func() {
		if !driverOwnsContext {
			driverCancel()
		}
	}()

	harness, claude, codex, err := connectorFor(cfg)
	if err != nil {
		return nil, err
	}

	command := stdio.Command{
		Path: cfg.Executable,
		Env:  append([]string(nil), cfg.Env...),
		Dir:  cfg.WorkspaceRoot,
	}
	clientOptions := client.Options{
		Permissions: newPermissionHandler(cfg.Posture, cfg.WorkspaceRoot),
	}
	var owned dialedClient
	if cfg.gatewayBacked() {
		owned, err = dial(driverCtx, launch.Config{
			Harness:     harness,
			SharedProxy: &cfg.Binding,
			Command:     command,
			Client:      clientOptions,
		})
	} else {
		native, ok := harness.(launch.NativeHarnessAdapter)
		if !ok {
			return nil, errors.New("acp: native harness does not implement launch.NativeHarnessAdapter")
		}
		owned, err = nativeDial(driverCtx, launch.NativeConfig{
			Harness: native,
			Command: command,
			Client:  clientOptions,
		})
	}
	if err != nil {
		return nil, fmt.Errorf("acp: dial: %w", err)
	}
	if owned == nil {
		return nil, errors.New("acp: dial returned a nil client")
	}

	acpClient := owned.client()
	if acpClient == nil {
		return nil, closeAfterConstructionFailure(owned, errors.New("acp: dial returned a nil ACP client"))
	}
	metadata := client.InitializeMetadata{}
	if provider, ok := acpClient.(initializeMetadataProvider); ok {
		// Metadata is optional for legacy test and client seams. A failed
		// snapshot is fail-closed to steering, but does not make ordinary ACP
		// sessions unusable.
		if snapshot, snapshotErr := provider.InitializeMetadata(); snapshotErr == nil {
			metadata = snapshot
		}
	}

	sess, err := createSession(driverCtx, acpClient, cfg)
	if err != nil {
		return nil, closeAfterConstructionFailure(owned, err)
	}
	if sess == nil {
		return nil, closeAfterConstructionFailure(owned, errors.New("acp: session creation returned a nil session"))
	}

	d := &Driver{
		owned:          owned,
		session:        sess,
		agentSessionID: string(sess.ID()),
		driverCtx:      driverCtx,
		driverCancel:   driverCancel,
		steeringOn:     steeringCapability(cfg.Harness, metadata),
	}

	// A loaded Claude session owns its existing configuration. ACP may populate
	// mutable config/mode capabilities in the session/load response, but the
	// driver preserves that loaded configuration; only a fresh session receives
	// the requested model and permission setup. When both aliases are empty,
	// native Claude is harness-managed: leave model selection entirely to the
	// adapter and apply only the posture.
	if claude != nil && cfg.AgentSessionID == "" {
		if cfg.ModelAlias != "" {
			if err := claude.SelectDefaultModel(driverCtx, sess); err != nil {
				return nil, closeAfterConstructionFailure(d, fmt.Errorf("acp: select default model: %w", err))
			}
			if err := claude.SelectSmallModel(driverCtx, sess); err != nil {
				return nil, closeAfterConstructionFailure(d, fmt.Errorf("acp: select small model: %w", err))
			}
		}
		if nativeEffort(cfg.Effort) != "" {
			if err := claude.SelectEffort(driverCtx, sess); err != nil {
				return nil, closeAfterConstructionFailure(d, fmt.Errorf("acp: select effort: %w", err))
			}
		}
		if err := claude.ApplyPermissionMode(driverCtx, sess, claudePermissionMode(cfg.Posture)); err != nil {
			return nil, closeAfterConstructionFailure(d, fmt.Errorf("acp: apply permission mode: %w", err))
		}
	}
	if codex != nil {
		if cfg.ModelAlias != "" {
			if err := codex.SelectModel(driverCtx, sess); err != nil {
				return nil, closeAfterConstructionFailure(d, fmt.Errorf("acp: select model: %w", err))
			}
		}
		if nativeEffort(cfg.Effort) != "" {
			if err := codex.SelectEffort(driverCtx, sess); err != nil {
				return nil, closeAfterConstructionFailure(d, fmt.Errorf("acp: select effort: %w", err))
			}
		}
	}

	driverOwnsContext = true
	return d, nil
}

func connectorFor(cfg Config) (launch.HarnessAdapter, claudeConnector, codexConnector, error) {
	switch cfg.Harness {
	case HarnessClaudeCode:
		models := launch.ClaudeModels{
			Default: cfg.ModelAlias,
			Small:   cfg.SmallModelAlias,
		}
		effort := nativeEffort(cfg.Effort)
		claude := launch.ClaudeCode(models)
		claude.Effort = effort
		return claude, newClaudeConnector(models, effort), nil, nil
	case HarnessCodex:
		var codex *launch.CodexConnector
		if cfg.gatewayBacked() {
			codex = launch.Codex("").WithModel(cfg.ModelAlias)
			codex.Posture = codexPosture(cfg.Posture, true)
			return codex, nil, nil, nil
		}
		effort := nativeEffort(cfg.Effort)
		if effort == "" {
			if cfg.ModelAlias == "" {
				codex = launch.Codex("")
			} else {
				codex = launch.Codex("").WithModel(cfg.ModelAlias)
			}
		} else {
			codex = launch.Codex("").WithModelEffort(cfg.ModelAlias, effort)
		}
		codex.Posture = codexPosture(cfg.Posture, false)
		return codex, nil, newCodexConnector(cfg.ModelAlias, effort), nil
	default:
		return nil, nil, nil, &ConfigError{Field: "Harness", Reason: "must be a supported harness"}
	}
}

func nativeEffort(effort string) string {
	if effort == "" || effort == "none" {
		return ""
	}
	return effort
}

func createSession(ctx context.Context, c acpClient, cfg Config) (session, error) {
	if cfg.AgentSessionID == "" {
		sess, err := c.NewSession(ctx, client.NewSessionParams{
			Cwd:        cfg.WorkspaceRoot,
			McpServers: cloneMcpServers(cfg.McpServers),
		})
		if err != nil {
			return nil, fmt.Errorf("acp: session/new: %w", err)
		}
		return sess, nil
	}

	loader, ok := c.(sessionLoader)
	if !ok {
		return nil, errors.New("acp: session/load capability unavailable")
	}
	sess, err := loader.LoadSession(ctx, client.LoadSessionParams{
		SessionID:  protocol.SessionID(cfg.AgentSessionID),
		Cwd:        cfg.WorkspaceRoot,
		McpServers: cloneMcpServers(cfg.McpServers),
	})
	if err != nil {
		return nil, fmt.Errorf("acp: session/load: %w", err)
	}
	return sess, nil
}

func claudePermissionMode(posture driver.Posture) protocol.SessionModeID {
	switch posture {
	case driver.PostureWorkspaceWrite:
		return "acceptEdits"
	case driver.PostureReadOnly:
		return "default"
	default:
		return "default"
	}
}

func codexPosture(posture driver.Posture, gatewayBacked bool) launch.CodexPosture {
	switch posture {
	case driver.PostureReadOnly:
		return launch.CodexPosture{
			ApprovalPolicy:       "never",
			SandboxMode:          "read-only",
			SandboxNetworkAccess: gatewayBacked,
		}
	case driver.PostureWorkspaceWrite:
		return launch.CodexPosture{
			ApprovalPolicy:       "never",
			SandboxMode:          "workspace-write",
			SandboxNetworkAccess: gatewayBacked,
		}
	default:
		// Config.validate rejects this path. Keep the fallback restrictive if
		// this helper is ever called independently in the future.
		return launch.CodexPosture{
			ApprovalPolicy:       "never",
			SandboxMode:          "read-only",
			SandboxNetworkAccess: gatewayBacked,
		}
	}
}

// AgentSessionID returns the ACP session id assigned by session/new or used
// for session/load. It remains stable for the driver's lifetime.
func (d *Driver) AgentSessionID() string {
	if d == nil {
		return ""
	}
	return d.agentSessionID
}

// Steer queues one active-turn injection through the turn arbiter. The arbiter
// owns provider translation and the dispatcher owns the single in-flight ACP
// request; this method only waits for the one result paired with its
// observation.
func (d *Driver) Steer(ctx context.Context, request driver.SteerRequest) (driver.SteerResult, error) {
	if d == nil {
		return driver.SteerResult{Outcome: driver.SteerOutcomeUnsupported}, errors.New("acp: driver unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	h := d.activeHandle()
	if h == nil {
		return driver.SteerResult{Outcome: driver.SteerOutcomeUnsupported}, nil
	}
	var reservation *steerObservationReservation
	if h.lane != nil {
		var status steerReservationStatus
		reservation, status = h.reserveSteer()
		if status == steerReservationCapacityExhausted {
			return driver.SteerResult{Outcome: driver.SteerOutcomeFallbackRequired}, &driver.SteerAdmissionError{}
		}
		if status == steerReservationClosed {
			if err := ctx.Err(); err != nil {
				return driver.SteerResult{Outcome: driver.SteerOutcomeFallbackRequired}, err
			}
			return driver.SteerResult{Outcome: driver.SteerOutcomeUnsupported}, nil
		}
	}
	reply := make(chan steerReply, 1)
	attempt := &steerAttempt{}
	preCanceled := ctx.Err() != nil
	if preCanceled {
		attempt.cancelAndSnapshot()
	}
	sendResult := h.sendResult(ctx, steerCommand{ctx: ctx, request: request, reply: reply, attempt: attempt, reservation: reservation})
	if sendResult != steerSendAccepted && sendResult != steerSendPending {
		reservation.release()
		if err := ctx.Err(); err != nil {
			attempt.cancelAndSnapshot()
			return driver.SteerResult{Outcome: driver.SteerOutcomeFallbackRequired}, err
		}
		return driver.SteerResult{Outcome: driver.SteerOutcomeUnsupported}, nil
	}
	if preCanceled {
		// A caller that was already canceled before mailbox admission has
		// explicitly sealed the attempt as not admitted. The arbiter still owns
		// the command and publishes its one fallback observation, but this caller
		// may safely return the cancellation result immediately.
		return driver.SteerResult{Outcome: driver.SteerOutcomeFallbackRequired}, ctx.Err()
	}
	select {
	case result := <-reply:
		return result.result, result.err
	case <-ctx.Done():
		// The arbiter still owns the attempt and may later emit its observation.
		// Return a bounded caller result without allowing this caller to consume
		// or cancel the arbiter's reply.
		return callerTimeoutSteerResult(attempt, sendResult), ctx.Err()
	}
}

func callerTimeoutSteerResult(attempt *steerAttempt, mailbox ...steerSendResult) driver.SteerResult {
	sendState := steerSendRejected
	if len(mailbox) > 0 {
		sendState = mailbox[0]
	}
	switch attempt.cancelAndSnapshot() {
	case steerAdmissionAdmitted:
		return driver.SteerResult{Outcome: driver.SteerOutcomeDeliveryUnknown, WriteAdmitted: true}
	case steerAdmissionPendingWriter:
		return driver.SteerResult{Outcome: driver.SteerOutcomeAdmissionUnknown}
	case steerAdmissionPending, steerAdmissionNotAdmitted:
		if sendState == steerSendAccepted || sendState == steerSendPending {
			// The command crossed the turn mailbox. Even if the arbiter has not
			// reached it yet, a retry-safe fallback would duplicate a possible
			// later provider call.
			return driver.SteerResult{Outcome: driver.SteerOutcomeAdmissionUnknown}
		}
		return driver.SteerResult{Outcome: driver.SteerOutcomeFallbackRequired}
	default:
		return driver.SteerResult{Outcome: driver.SteerOutcomeAdmissionUnknown}
	}
}

func (d *Driver) activeHandle() *turnHandle {
	if d == nil {
		return nil
	}
	d.activeMu.Lock()
	defer d.activeMu.Unlock()
	return d.active
}

// Close cancels active turns, releases the owned ACP connection/process exactly
// once, and rejects new turns thereafter. The shared proxy binding is only data
// passed to launch.Dial and is never touched here.
func (d *Driver) Close() error {
	return d.close(context.Background())
}

func (d *Driver) close(ctx context.Context) error {
	if d == nil {
		return nil
	}
	d.closeOnce.Do(func() {
		if d.driverCancel != nil {
			d.driverCancel()
		}
		d.turnMu.Lock()
		d.closed = true
		d.turnMu.Unlock()
		if d.owned != nil {
			d.closeErr = d.owned.close(ctx)
		}
	})
	return d.closeErr
}

func closeAfterConstructionFailure(closer interface{ close(context.Context) error }, cause error) error {
	if closeErr := closer.close(context.Background()); closeErr != nil {
		return errors.Join(cause, fmt.Errorf("acp: close after construction failure: %w", closeErr))
	}
	return cause
}

type launchedClient struct {
	managed *launch.ManagedClient
	acp     acpClient
}

func (c *launchedClient) client() acpClient { return c.acp }

func (c *launchedClient) close(ctx context.Context) error { return c.managed.Close(ctx) }

func dialLaunch(ctx context.Context, cfg launch.Config) (dialedClient, error) {
	managed, err := launch.Dial(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &launchedClient{
		managed: managed,
		acp:     &realClient{client: managed.Client()},
	}, nil
}

func dialNativeLaunch(ctx context.Context, cfg launch.NativeConfig) (dialedClient, error) {
	managed, err := launch.DialNative(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &launchedClient{
		managed: managed,
		acp:     &realClient{client: managed.Client()},
	}, nil
}

type realClient struct {
	client *client.Client
}

func (c *realClient) InitializeMetadata() (client.InitializeMetadata, error) {
	return c.client.InitializeMetadata()
}

func (c *realClient) NewSession(ctx context.Context, p client.NewSessionParams) (session, error) {
	return c.client.NewSession(ctx, p)
}

func (c *realClient) LoadSession(ctx context.Context, p client.LoadSessionParams) (session, error) {
	return c.client.LoadSession(ctx, p)
}

type realClaudeConnector struct {
	connector *launch.ClaudeConnector
}

type realCodexConnector struct {
	connector *launch.CodexConnector
}

func (c *realCodexConnector) SelectModel(ctx context.Context, sess session) error {
	concrete, err := concreteSession(sess)
	if err != nil {
		return err
	}
	return c.connector.SelectModel(ctx, concrete)
}

func (c *realCodexConnector) SelectEffort(ctx context.Context, sess session) error {
	concrete, err := concreteSession(sess)
	if err != nil {
		return err
	}
	return c.connector.SelectEffort(ctx, concrete)
}

func (c *realClaudeConnector) SelectDefaultModel(ctx context.Context, sess session) error {
	concrete, err := concreteSession(sess)
	if err != nil {
		return err
	}
	return c.connector.SelectDefaultModel(ctx, concrete)
}

func (c *realClaudeConnector) SelectSmallModel(ctx context.Context, sess session) error {
	concrete, err := concreteSession(sess)
	if err != nil {
		return err
	}
	return c.connector.SelectSmallModel(ctx, concrete)
}

func (c *realClaudeConnector) SelectEffort(ctx context.Context, sess session) error {
	concrete, err := concreteSession(sess)
	if err != nil {
		return err
	}
	return c.connector.SelectEffort(ctx, concrete)
}

func (c *realClaudeConnector) ApplyPermissionMode(ctx context.Context, sess session, modeID protocol.SessionModeID) error {
	concrete, err := concreteSession(sess)
	if err != nil {
		return err
	}
	return c.connector.ApplyPermissionMode(ctx, concrete, modeID)
}

func concreteSession(sess session) (*client.Session, error) {
	concrete, ok := sess.(*client.Session)
	if !ok {
		return nil, errors.New("acp: internal session is not an acp/client session")
	}
	return concrete, nil
}

var _ dialFunc = dialLaunch
var _ nativeDialFunc = dialNativeLaunch
var _ claudeConnectorFactory = func(launch.ClaudeModels, string) claudeConnector {
	return &realClaudeConnector{}
}
var _ codexConnectorFactory = func(string, string) codexConnector {
	return &realCodexConnector{}
}
