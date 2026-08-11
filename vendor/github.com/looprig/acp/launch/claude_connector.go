// claude_connector.go implements ClaudeConnector's session-level behavior:
// locating claude-agent-acp's "model" select config option (tolerating a
// legacy identifier quirk some adapter versions have), applying model
// selection, and setting only permission modes the adapter actually
// advertised. claudecode.go implements the other half of the same type:
// HarnessAdapter.Configure (executable, argument, and environment wiring).
package launch

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/looprig/acp/client"
	"github.com/looprig/acp/protocol"
)

// ClaudeModels selects the harness-facing aliases for Claude Code's default
// and lightweight ("small") model roles: the two values SelectDefaultModel
// and SelectSmallModel look up among the connected adapter's advertised
// "model" select config option. Gateway configuration requires both aliases;
// native no-proxy configuration permits either or both to be empty.
type ClaudeModels struct {
	Default string
	Small   string
}

// ClaudeConnector adapts a launched ACP session to claude-agent-acp's
// specific conventions. Construct with ClaudeCode.
type ClaudeConnector struct {
	// Models are the harness-facing model aliases SelectDefaultModel and
	// SelectSmallModel apply.
	Models ClaudeModels
	// Effort is the harness-facing reasoning effort SelectEffort applies via
	// the advertised thought_level config option. Empty leaves effort
	// selection to Claude Code.
	Effort string
	// CLIPath, if non-empty, must be an absolute path pinning the
	// underlying `claude` CLI claude-agent-acp drives (CLAUDE_CODE_EXECUTABLE
	// -- see claudecode.go's Configure). Empty omits the variable entirely.
	CLIPath string
}

// ClaudeCode constructs a ClaudeConnector for the given model aliases. The
// caller may set the returned value's CLIPath field before passing it to
// Dial as Config.Harness if it needs to pin the underlying `claude` CLI to a
// specific absolute path.
func ClaudeCode(models ClaudeModels) *ClaudeConnector {
	return &ClaudeConnector{Models: models}
}

// sessionConfigurer is the subset of *client.Session's behavior
// SelectDefaultModel, SelectSmallModel, and ApplyPermissionMode need:
// reading cached config options/modes and applying a selection over the
// wire. It exists purely as a substitution seam for
// claude_connector_test.go -- *client.Session satisfies it structurally
// with zero coupling, the same idiom this package's ModelProxy and
// connCloser interfaces already use (see contracts.go and managed.go).
// Production callers always pass a real *client.Session and get identical
// behavior; nothing about this interface is exported, so it changes
// nothing about what callers of this package can pass in.
type sessionConfigurer interface {
	ConfigOptions() []protocol.SessionConfigOption
	Modes() *protocol.SessionModeState
	SetConfigOption(ctx context.Context, configID protocol.SessionConfigID, valueID protocol.SessionConfigValueID) error
	SetMode(ctx context.Context, modeID protocol.SessionModeID) error
}

// Compile-time proof that the structural interfaces this file and
// claudecode.go rely on are actually satisfied by the real types they are
// meant for.
var (
	_ sessionConfigurer    = (*client.Session)(nil)
	_ HarnessAdapter       = (*ClaudeConnector)(nil)
	_ NativeHarnessAdapter = (*ClaudeConnector)(nil)
)

// SelectDefaultModel applies c.Models.Default via session/set_config_option
// against sess's advertised "model" option.
func (c *ClaudeConnector) SelectDefaultModel(ctx context.Context, sess *client.Session) error {
	return c.selectModel(ctx, sess, c.Models.Default)
}

// SelectSmallModel applies c.Models.Small via session/set_config_option
// against sess's advertised "model" option.
func (c *ClaudeConnector) SelectSmallModel(ctx context.Context, sess *client.Session) error {
	return c.selectModel(ctx, sess, c.Models.Small)
}

// SelectEffort applies c.Effort via the connected adapter's advertised
// thought_level select config option. An omitted effort is a deliberate
// no-op; an unavailable option or value returns *EffortAliasError without a
// wire call.
func (c *ClaudeConnector) SelectEffort(ctx context.Context, sess *client.Session) error {
	return c.selectEffort(ctx, sess, c.Effort)
}

// selectModel finds sess's "model" category config option, resolves a
// non-empty alias against its advertised select values, and applies it via
// Session.SetConfigOption. An empty alias is a deliberate no-op. A missing
// model option, or a non-empty alias that matches none of its advertised
// values, fails with *ModelAliasError -- never a wire call at all in either
// case.
func (c *ClaudeConnector) selectModel(ctx context.Context, sess sessionConfigurer, alias string) error {
	if alias == "" {
		return nil
	}
	configID, valueID, ok := resolveModelSelection(sess.ConfigOptions(), alias)
	if !ok {
		return &ModelAliasError{Alias: alias}
	}
	return sess.SetConfigOption(ctx, configID, valueID)
}

// selectEffort resolves effort against the advertised thought_level select
// option and applies it. Empty effort is a deliberate no-op. Unlike an
// unmatched model alias, this uses a distinct typed error so callers can
// report the bounded selector that failed.
func (c *ClaudeConnector) selectEffort(ctx context.Context, sess sessionConfigurer, effort string) error {
	if effort == "" {
		return nil
	}
	configID, valueID, ok := resolveEffortSelection(sess.ConfigOptions(), effort)
	if !ok {
		return &EffortAliasError{Effort: effort, Alias: effort}
	}
	return sess.SetConfigOption(ctx, configID, valueID)
}

// ApplyPermissionMode sets modeID on sess via session/set_mode, but only if
// modeID actually appears in sess's currently advertised mode list
// (Session.Modes().AvailableModes) -- an unadvertised mode is silently
// left alone rather than sent to the wire, matching the design doc's
// "never set a mode that wasn't advertised" (unlike an unmatched model
// alias, this is a deliberate no-op, not an error: permission modes are
// optional by nature).
func (c *ClaudeConnector) ApplyPermissionMode(ctx context.Context, sess *client.Session, modeID protocol.SessionModeID) error {
	return applyPermissionMode(ctx, sess, modeID)
}

func applyPermissionMode(ctx context.Context, sess sessionConfigurer, modeID protocol.SessionModeID) error {
	modes := sess.Modes()
	if modes == nil {
		return nil
	}
	for _, m := range modes.AvailableModes {
		if m.ID == modeID {
			return sess.SetMode(ctx, modeID)
		}
	}
	return nil
}

// resolveModelSelection finds the "model" category option among opts and
// resolves alias against its select values, returning the option's own
// identifier (for the session/set_config_option request) and the matched
// value id. ok is false if no model option is found, if it is not a select
// option, or if alias matches none of its values.
func resolveModelSelection(opts []protocol.SessionConfigOption, alias string) (protocol.SessionConfigID, protocol.SessionConfigValueID, bool) {
	return resolveCategorizedSelection(opts, protocol.SessionConfigOptionCategoryModel, alias)
}

// resolveEffortSelection finds the advertised thought_level selector and
// resolves an effort against its values.
func resolveEffortSelection(opts []protocol.SessionConfigOption, effort string) (protocol.SessionConfigID, protocol.SessionConfigValueID, bool) {
	return resolveCategorizedSelection(opts, protocol.SessionConfigOptionCategoryThoughtLevel, effort)
}

// resolveCategorizedSelection is intentionally limited to the two selector
// categories this connector owns. It is shared by model and thought-level
// resolution without becoming an arbitrary category/value escape hatch.
func resolveCategorizedSelection(opts []protocol.SessionConfigOption, category protocol.SessionConfigOptionCategory, alias string) (protocol.SessionConfigID, protocol.SessionConfigValueID, bool) {
	if category != protocol.SessionConfigOptionCategoryModel && category != protocol.SessionConfigOptionCategoryThoughtLevel {
		return "", "", false
	}
	opt, configID, ok := findSelectOption(opts, category)
	if !ok {
		return "", "", false
	}
	valueID, ok := findSelectValue(opt, alias)
	if !ok {
		return "", "", false
	}
	return configID, valueID, true
}

// findModelOption returns the first opts entry whose Category is
// SessionConfigOptionCategoryModel and which carries a Select variant,
// together with its resolved identifier (see configOptionID). Options in
// any other category (mode, model_config, thought_level, or none at all)
// are ignored, matching the design doc's "select only category model".
func findModelOption(opts []protocol.SessionConfigOption) (protocol.SessionConfigOption, protocol.SessionConfigID, bool) {
	return findSelectOption(opts, protocol.SessionConfigOptionCategoryModel)
}

func findSelectOption(opts []protocol.SessionConfigOption, category protocol.SessionConfigOptionCategory) (protocol.SessionConfigOption, protocol.SessionConfigID, bool) {
	if category != protocol.SessionConfigOptionCategoryModel && category != protocol.SessionConfigOptionCategoryThoughtLevel {
		return protocol.SessionConfigOption{}, "", false
	}
	for _, opt := range opts {
		if opt.Category == nil || *opt.Category != category {
			continue
		}
		if opt.Select == nil {
			continue
		}
		id, ok := configOptionID(opt)
		if !ok {
			continue
		}
		return opt, id, true
	}
	return protocol.SessionConfigOption{}, "", false
}

// EffortAliasError reports that a requested reasoning-effort selector
// matched no value the connected adapter's advertised thought_level option
// exposed. Alias is retained as a compatibility spelling for callers that
// classify selector errors uniformly; Effort is the canonical field.
type EffortAliasError struct {
	Effort string
	Alias  string
}

// EffortSelectionError is a descriptive alias for EffortAliasError.
type EffortSelectionError = EffortAliasError

func (e *EffortAliasError) Error() string {
	value := e.Effort
	if value == "" {
		value = e.Alias
	}
	return fmt.Sprintf("acp/launch: effort alias %q not advertised by the connected adapter", value)
}

// findSelectValue reports whether alias matches a SessionConfigSelectOption
// value among opt's select options (flattening grouped and ungrouped
// alike), and returns that value id.
func findSelectValue(opt protocol.SessionConfigOption, alias string) (protocol.SessionConfigValueID, bool) {
	if opt.Select == nil {
		return "", false
	}
	for _, o := range opt.Select.Options.Ungrouped {
		if string(o.Value) == alias {
			return o.Value, true
		}
	}
	for _, g := range opt.Select.Options.Grouped {
		for _, o := range g.Options {
			if string(o.Value) == alias {
				return o.Value, true
			}
		}
	}
	return "", false
}

// configOptionID resolves opt's identifier, preferring the spec-defined
// top-level "id" field (already decoded by protocol.SessionConfigOption's
// generated UnmarshalJSON) and falling back to a legacy "configId" key some
// claude-agent-acp adapter versions place instead -- not as a sibling
// top-level field (protocol's generated decode only recognizes "id" there,
// so an out-of-schema top-level "configId" is already unrecoverably dropped
// by the time this package ever sees a decoded SessionConfigOption), but
// inside the option's own _meta object, ACP's documented extensibility
// channel for exactly this kind of adapter-specific, spec-adjacent data.
// _meta is the only place a legacy identifier can still be observed once
// protocol's own decode has run, since protocol.SessionConfigOption's
// MarshalJSON/UnmarshalJSON pair (protocol/types_gen.go, generated from the
// pinned schema and never hand-edited) has no field to carry a
// non-standard "configId" anywhere else.
func configOptionID(opt protocol.SessionConfigOption) (protocol.SessionConfigID, bool) {
	if opt.ID != "" {
		return opt.ID, true
	}
	return legacyConfigID(opt.Meta)
}

// legacyConfigID looks for a "configId" string entry inside a
// SessionConfigOption's raw _meta object, returning it as a
// SessionConfigID. It reports false for empty/absent _meta, a _meta that
// is not a JSON object, a missing or non-string "configId" entry, or an
// empty string value -- never panicking on any adapter-supplied shape,
// since _meta content is untrusted wire input like any other.
func legacyConfigID(meta json.RawMessage) (protocol.SessionConfigID, bool) {
	if len(meta) == 0 {
		return "", false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(meta, &fields); err != nil {
		return "", false
	}
	raw, present := fields["configId"]
	if !present {
		return "", false
	}
	var legacy string
	if err := json.Unmarshal(raw, &legacy); err != nil || legacy == "" {
		return "", false
	}
	return protocol.SessionConfigID(legacy), true
}
