// codex_connector.go defines CodexConnector: the caller-facing type
// constructed via Codex and passed as Config.Harness to Dial. Its immutable
// selector state is applied to a connected ACP session through the two
// bounded, advertised config-option selectors below. The connector never
// exposes arbitrary config/mode setters or the unstable session/set_model
// extension; changing the stored selector state still means constructing a
// new CodexConnector (see WithModel/WithModelEffort), while applying that
// state to an existing session uses session/set_config_option only for the
// advertised model and thought_level categories.
package launch

import (
	"context"

	"github.com/looprig/acp/client"
)

// CodexPosture is the caller-chosen sandbox/approval posture codex-acp is
// launched under: an application-level choice this package never
// hardcodes (see codex.go's Configure/codexConfigArgs).
type CodexPosture struct {
	// ApprovalPolicy is codex-acp's approval_policy `-c` override (e.g.
	// "untrusted", "on-failure", "on-request", "never"). Empty resolves
	// to defaultCodexApprovalPolicy.
	ApprovalPolicy string
	// SandboxMode is codex-acp's sandbox_mode `-c` override (e.g.
	// "read-only", "workspace-write", "danger-full-access"). Empty
	// resolves to defaultCodexSandboxMode.
	SandboxMode string
	// SandboxNetworkAccess is codex-acp's
	// sandbox_workspace_write.network_access `-c` override: required
	// when the child must reach loopback HTTP MCP servers or the
	// gateway from inside Codex's sandbox (see the design doc). Its zero
	// value (false, deny) is already the least-privilege default and is
	// never substituted for.
	SandboxNetworkAccess bool
}

const (
	// defaultCodexApprovalPolicy and defaultCodexSandboxMode are the
	// sane defaults CodexPosture.resolve substitutes for an empty
	// caller-supplied field -- a caller that only cares about the
	// model-provider override can leave Posture at its zero value and
	// still get a usable, working (if conservative) sandbox/approval
	// posture rather than an empty, invalid `-c` value.
	defaultCodexApprovalPolicy = "on-request"
	defaultCodexSandboxMode    = "workspace-write"
)

// resolve returns p with empty fields replaced by their documented sane
// defaults. It has a value receiver and returns a new value rather than
// mutating p in place, so calling it (as codex.go's Configure does, via
// c.Posture.resolve()) never changes the CodexConnector's own stored
// Posture field: defaults are applied fresh at every Configure call, never
// baked back into the connector.
func (p CodexPosture) resolve() CodexPosture {
	if p.ApprovalPolicy == "" {
		p.ApprovalPolicy = defaultCodexApprovalPolicy
	}
	if p.SandboxMode == "" {
		p.SandboxMode = defaultCodexSandboxMode
	}
	return p
}

// CodexConnector adapts a launched ACP session to codex-acp's specific
// conventions. Construct with Codex.
type CodexConnector struct {
	// Model is the harness-facing model alias. Gateway Configure embeds it in
	// its provider argv; SelectModel applies it to an existing session's
	// advertised model select option. Native ConfigureNative retains it as
	// managed state and emits no model override. An empty value is a deliberate
	// no-op for session selection and native configuration.
	Model string
	// Effort is the neutral reasoning-effort selector SelectEffort applies to
	// the advertised thought_level option (codex-acp's config ID is currently
	// reasoning_effort). Native ConfigureNative retains it as managed state and
	// emits no model_reasoning_effort override. An empty value is a deliberate
	// no-op.
	Effort string
	// effortExplicit distinguishes the new paired selector API from the
	// legacy Codex(model) constructor, whose model-only native behavior is
	// retained for compatibility with existing callers.
	effortExplicit bool
	// Posture is the sandbox/approval posture Configure applies. The
	// zero value resolves to sane, least-privilege defaults (see
	// CodexPosture.resolve).
	Posture CodexPosture
}

// Codex constructs a CodexConnector for model, the harness-facing model
// alias codex-acp is launched with via the `-c model=` override. Posture
// may be set on the returned value before passing it to Dial as
// Config.Harness.
func Codex(model string) *CodexConnector {
	return &CodexConnector{Model: model}
}

// WithModel returns a new *CodexConnector identical to c except for its
// Model field (Posture is copied unchanged). c itself is never mutated, and
// the two resulting connectors never alias any state (CodexPosture has no
// pointer or slice fields, so the struct copy below is a fully independent
// value). Call SelectModel to apply the stored alias to an existing session;
// WithModel remains useful when a caller wants a separately configured
// connector or a fresh launch.
func (c *CodexConnector) WithModel(model string) *CodexConnector {
	clone := *c
	clone.Model = model
	return &clone
}

// WithModelEffort returns a new *CodexConnector identical to c except for its
// model and reasoning-effort selectors. c itself is never mutated. A model
// and effort must either both be non-empty or both be empty when the resulting
// connector is configured for a native launch; ConfigureNative reports a
// typed ConfigError for a partial pair. SelectModel and SelectEffort apply the
// stored selectors to an existing session.
func (c *CodexConnector) WithModelEffort(model, effort string) *CodexConnector {
	clone := *c
	clone.Model = model
	clone.Effort = effort
	clone.effortExplicit = true
	return &clone
}

// SelectModel applies c.Model through the connected adapter's advertised
// model select config option. Empty model is a deliberate no-op. A missing
// model option or an alias absent from its advertised values returns a typed
// *ModelAliasError without making a wire call.
func (c *CodexConnector) SelectModel(ctx context.Context, sess *client.Session) error {
	return c.selectModel(ctx, sess)
}

// selectModel is the narrow sessionConfigurer seam used by tests and by the
// exported SelectModel method. It resolves the current cached options on each
// call so a preceding SetConfigOption response can replace the option set
// before effort selection runs.
func (c *CodexConnector) selectModel(ctx context.Context, sess sessionConfigurer) error {
	if c.Model == "" {
		return nil
	}
	configID, valueID, ok := resolveModelSelection(sess.ConfigOptions(), c.Model)
	if !ok {
		return &ModelAliasError{Alias: c.Model}
	}
	return sess.SetConfigOption(ctx, configID, valueID)
}

// SelectEffort applies c.Effort through the connected adapter's advertised
// thought_level select config option. Empty effort is a deliberate no-op. A
// missing option or an alias absent from its advertised values returns a
// typed *EffortAliasError without making a wire call.
func (c *CodexConnector) SelectEffort(ctx context.Context, sess *client.Session) error {
	return c.selectEffort(ctx, sess)
}

// selectEffort is the narrow sessionConfigurer seam used by tests and by the
// exported SelectEffort method. It reads the session's current cached options
// rather than retaining a pre-model snapshot.
func (c *CodexConnector) selectEffort(ctx context.Context, sess sessionConfigurer) error {
	if c.Effort == "" {
		return nil
	}
	configID, valueID, ok := resolveEffortSelection(sess.ConfigOptions(), c.Effort)
	if !ok {
		return &EffortAliasError{Effort: c.Effort, Alias: c.Effort}
	}
	return sess.SetConfigOption(ctx, configID, valueID)
}

// Compile-time proof that CodexConnector actually satisfies HarnessAdapter
// (see codex.go's Configure).
var (
	_ HarnessAdapter       = (*CodexConnector)(nil)
	_ NativeHarnessAdapter = (*CodexConnector)(nil)
)
