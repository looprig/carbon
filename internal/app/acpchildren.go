package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/looprig/acp/launch"
	"github.com/looprig/acp/protocol"
	"github.com/looprig/core/uuid"
	"github.com/looprig/foreignloops/driver"
	acpdriver "github.com/looprig/foreignloops/driver/acp"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/foreign"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	model "github.com/looprig/inference/model"
)

var errACPChildUnavailable = errors.New("coderig: ACP child unavailable")

// maxACPModelFacingErrorBytes bounds the complete ACP construction detail,
// including its fixed prefix. This matches foreignloops' ACP turn projection
// so both child-failure paths have the same model-facing byte ceiling.
const maxACPModelFacingErrorBytes = 512

const (
	maxACPErrorDepth    = 32
	maxACPErrorNodes    = 128
	maxACPErrorChildren = 64
)

const (
	redactedACPChildValue = "[REDACTED]"
	redactedACPChildURL   = "[REDACTED_URL]"
	redactedACPChildPath  = "[REDACTED_PATH]"
)

var (
	acpChildMessageURLPattern              = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s<>"']+`)
	acpChildMessageAuthPattern             = regexp.MustCompile(`(?i)(\b(?:authorization|proxy-authorization)\b\s*["']?\s*[:=]\s*)[^\r\n,;&}\]]+`)
	acpChildMessageSecretAssignmentPattern = regexp.MustCompile(`(?i)(\b(?:api[\s_-]*key|access[\s_-]*token|refresh[\s_-]*token|token|password|credential|secret)\b\s*["']?\s*[:=]\s*)(?:"[^"]*"|'[^']*'|[^\s,;&}\]]+)`)
	acpChildMessageBearerPattern           = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9][A-Za-z0-9._~+/=-]*`)
	acpChildMessageUnixPathPattern         = regexp.MustCompile(`/[^\s,;)}\]<>"']+`)
	acpChildMessageWindowsPathPattern      = regexp.MustCompile(`(?i)[A-Za-z]:[\\/][^\s,;)}\]<>"']*`)
	acpChildCredentialTokenPattern         = regexp.MustCompile(`(?i)\b(?:sk|pk|ghp|gho|ghu|ghs|ghr|xox[baprs]|AIza)[-_][a-z0-9][a-z0-9._-]*\b`)
)

// acpChildModelFacingError is the only ACP construction error that opts into
// Harness's model-facing failure detail. The concrete type stays private; the
// public boundary is the narrow ModelFacingError method.
type acpChildModelFacingError struct{ detail string }

func (e *acpChildModelFacingError) Error() string {
	if e == nil {
		return ""
	}
	return e.detail
}

func (e *acpChildModelFacingError) ModelFacingError() string {
	if e == nil {
		return ""
	}
	return e.detail
}

// errACPAccessProfileUnavailable is intentionally fixed and bounded. An
// invalid Config.AccessProfile must stop ACP composition before any child or
// gateway launch without reflecting caller-controlled values in the error.
var errACPAccessProfileUnavailable = errors.New("coderig: ACP access profile unavailable")

// boundedACPChildError is the model-facing error boundary for ACP startup and
// restore. ACP launch/RPC/stdio errors can contain executable paths, login
// locations, URLs, provider messages, or stderr; none of those details belong
// in an agent result or durable Harness error. Only an ACP protocol Error or
// Fault contributes the explicitly bounded Code/Message projection. Keep
// cancellation recognizable for controller shutdown, but collapse every other
// cause to one fixed result.
func boundedACPChildError(err error) error {
	if err == nil {
		return nil
	}
	if containsACPChildErrorIdentity(err, context.Canceled) {
		return context.Canceled
	}
	if containsACPChildErrorIdentity(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if detail, ok := boundedACPProtocolErrorDetail(err); ok {
		return &acpChildModelFacingError{detail: detail}
	}
	return errACPChildUnavailable
}

func containsACPChildErrorIdentity(err, target error) bool {
	type node struct {
		err   error
		depth int
	}
	if isNilACPChildError(err) || isNilACPChildError(target) {
		return false
	}
	pending := []node{{err: err}}
	seen := make(map[error]struct{})
	visited := 0
	for len(pending) > 0 && visited < maxACPErrorNodes {
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if isNilACPChildError(current.err) || markACPChildErrorSeen(seen, current.err) {
			continue
		}
		visited++
		if sameACPChildError(current.err, target) {
			return true
		}
		if current.depth >= maxACPErrorDepth {
			continue
		}
		if wrapper, ok := current.err.(interface{ Unwrap() []error }); ok {
			children := safeACPChildUnwrapMany(wrapper)
			if len(children) > maxACPErrorChildren {
				children = children[:maxACPErrorChildren]
			}
			for index := len(children) - 1; index >= 0; index-- {
				pending = append(pending, node{err: children[index], depth: current.depth + 1})
			}
			continue
		}
		if wrapper, ok := current.err.(interface{ Unwrap() error }); ok {
			pending = append(pending, node{err: safeACPChildUnwrapOne(wrapper), depth: current.depth + 1})
		}
	}
	return false
}

func sameACPChildError(left, right error) (same bool) {
	defer func() {
		if recover() != nil {
			same = false
		}
	}()
	leftType, rightType := reflect.TypeOf(left), reflect.TypeOf(right)
	return leftType != nil && leftType == rightType && leftType.Comparable() && left == right
}

// boundedACPProtocolErrorDetail intentionally reads only the exported Code and
// Message fields from an ACP wire error. It never calls Error, inspects Data,
// or unwraps the protocol fault's local cause.
func boundedACPProtocolErrorDetail(err error) (string, bool) {
	type node struct {
		err   error
		depth int
	}
	if isNilACPChildError(err) {
		return "", false
	}
	pending := []node{{err: err}}
	seen := make(map[error]struct{})
	visited := 0
	for len(pending) > 0 && visited < maxACPErrorNodes {
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if isNilACPChildError(current.err) || markACPChildErrorSeen(seen, current.err) {
			continue
		}
		visited++
		if code, message, ok := directACPProtocolErrorFields(current.err); ok {
			return formatACPModelFacingError(code, message), true
		}
		if current.depth >= maxACPErrorDepth {
			continue
		}
		if wrapper, ok := current.err.(interface{ Unwrap() []error }); ok {
			children := safeACPChildUnwrapMany(wrapper)
			if len(children) > maxACPErrorChildren {
				children = children[:maxACPErrorChildren]
			}
			for index := len(children) - 1; index >= 0; index-- {
				pending = append(pending, node{err: children[index], depth: current.depth + 1})
			}
			continue
		}
		if wrapper, ok := current.err.(interface{ Unwrap() error }); ok {
			pending = append(pending, node{err: safeACPChildUnwrapOne(wrapper), depth: current.depth + 1})
		}
	}
	return "", false
}

func directACPProtocolErrorFields(err error) (protocol.ErrorCode, string, bool) {
	switch typed := any(err).(type) {
	case *protocol.Error:
		if typed == nil {
			return 0, "", false
		}
		return typed.Code, typed.Message, true
	case protocol.Error:
		return typed.Code, typed.Message, true
	case *protocol.Fault:
		if typed == nil {
			return 0, "", false
		}
		return typed.Code, typed.Message, true
	case protocol.Fault:
		return typed.Code, typed.Message, true
	default:
		return 0, "", false
	}
}

func isNilACPChildError(err error) bool {
	if err == nil {
		return true
	}
	value := reflect.ValueOf(err)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func markACPChildErrorSeen(seen map[error]struct{}, err error) (alreadySeen bool) {
	defer func() {
		if recover() != nil {
			alreadySeen = false
		}
	}()
	typeOfError := reflect.TypeOf(err)
	if typeOfError == nil || !typeOfError.Comparable() {
		return false
	}
	if _, ok := seen[err]; ok {
		return true
	}
	seen[err] = struct{}{}
	return false
}

func safeACPChildUnwrapOne(wrapper interface{ Unwrap() error }) (next error) {
	defer func() {
		if recover() != nil {
			next = nil
		}
	}()
	return wrapper.Unwrap()
}

func safeACPChildUnwrapMany(wrapper interface{ Unwrap() []error }) (children []error) {
	defer func() {
		if recover() != nil {
			children = nil
		}
	}()
	return wrapper.Unwrap()
}

func formatACPModelFacingError(code protocol.ErrorCode, message string) string {
	message = normalizeACPModelFacingMessage(message)
	detail := fmt.Sprintf("ACP error %d", code)
	if message != "" {
		detail += ": " + message
	}
	return truncateACPModelFacingUTF8(detail, maxACPModelFacingErrorBytes)
}

func normalizeACPModelFacingMessage(message string) string {
	message = strings.ToValidUTF8(message, "\uFFFD")
	var normalized strings.Builder
	normalized.Grow(len(message))
	for _, r := range message {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029' {
			normalized.WriteByte(' ')
			continue
		}
		normalized.WriteRune(r)
	}
	return redactACPModelFacingMessage(strings.Join(strings.Fields(normalized.String()), " "))
}

func redactACPModelFacingMessage(message string) string {
	message = acpChildMessageURLPattern.ReplaceAllString(message, redactedACPChildURL)
	message = acpChildMessageAuthPattern.ReplaceAllString(message, "$1"+redactedACPChildValue)
	message = acpChildMessageSecretAssignmentPattern.ReplaceAllString(message, "$1"+redactedACPChildValue)
	message = acpChildMessageBearerPattern.ReplaceAllString(message, redactedACPChildValue)
	message = acpChildCredentialTokenPattern.ReplaceAllString(message, redactedACPChildValue)
	message = acpChildMessageWindowsPathPattern.ReplaceAllString(message, redactedACPChildPath)
	message = acpChildMessageUnixPathPattern.ReplaceAllString(message, redactedACPChildPath)
	return message
}

func truncateACPModelFacingUTF8(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(value) <= maxBytes {
		return value
	}
	end := 0
	for end < len(value) {
		_, size := utf8.DecodeRuneInString(value[end:])
		if end+size > maxBytes {
			break
		}
		end += size
	}
	return value[:end]
}

// ACPChildrenConfig is the composition-root input for delegated ACP loops.
// Executable paths are checked statically before a profile is registered. Env
// is reduced to EnvAllowlist before it reaches the child process.
type ACPChildrenConfig struct {
	Catalog ACPCompiledCatalog
	// AccessProfile is the session-fixed CodeRig profile. Empty selects
	// DefaultAccessProfile; NewACPComposition normalizes and validates it once.
	AccessProfile AccessProfile
	Executables   map[loop.AgentHarnessName]string
	WorkspaceRoot string
	Env           []string
	// EnvAllowlist is the compatibility fallback for callers that predate
	// credential-specific allowlists. Production supplies both mode-specific
	// lists below.
	EnvAllowlist        []string
	NativeEnvAllowlist  []string
	GatewayEnvAllowlist []string
	// executablePreflight is retained as a lower-level diagnostic/test seam for
	// callers that explicitly invoke the preflight helpers. NewACPComposition
	// deliberately never calls it: runtime rows are authoritative until the
	// selected child is launched.
	executablePreflight func(context.Context, ACPExecutableProbe) ACPPreflightResult
	// gatewayPreflightBinding avoids starting a loopback listener in focused
	// diagnostic-preflight tests. Production leaves it nil; ordinary
	// composition never creates a gateway solely to inspect adapter readiness.
	gatewayPreflightBinding *launch.ProxyBinding

	// posture is derived once by NewACPComposition from AccessProfile and then
	// captured by both the live and restored builders. It is deliberately
	// unexported: callers cannot mutate a child posture after composition.
	posture driver.Posture
}

// ACPExecutableProbe is the bounded, secret-free input to an explicit ACP
// diagnostic probe. Models contains model-facing aliases for a Claude session
// or a single exact launch model for Codex. SharedProxy is set only for a
// gateway-backed child; native-auth probes use no proxy and keep it nil.
type ACPExecutableProbe struct {
	ACPNativeAuthProbe
	Credential  loop.CredentialMode
	Model       string
	SmallModel  string
	Models      []string
	SharedProxy *launch.ProxyBinding
}

// ACPPreflightResult is the bounded result of an explicit ACP diagnostic
// initialize/session probe. It is not used to filter the production catalog.
type ACPPreflightResult struct {
	Ready            bool
	AdvertisedModels []string
}

// ACPComposition is the immutable CodeRig-to-Harness bridge for ACP children.
// The registry is retained for inspection and the function pair is the narrow
// legacy rig option; dispatch still selects by bound RuntimeProfile.
type ACPComposition struct {
	Catalog     ACPCompiledCatalog
	Registry    *foreign.BuilderRegistry
	Live        foreign.Builder
	Restored    foreign.RestoredBuilder
	Diagnostics []string

	// These values are captured for composition inspection. The builders use
	// the same values retained in their private factory configuration.
	accessProfile AccessProfile
	posture       driver.Posture
}

// NewACPComposition performs only static executable/path checks, registers
// cataloged ACP profiles, and returns a registry-backed builder pair. Missing
// executables simply omit that harness; native primers remain usable. ACP
// process/session/model availability is resolved lazily by the child launch.
func NewACPComposition(config ACPChildrenConfig) (*ACPComposition, error) {
	effectiveProfile, err := normalizeAccessProfile(config.AccessProfile)
	if err != nil {
		return nil, errACPAccessProfileUnavailable
	}
	posture, err := acpPostureFor(effectiveProfile)
	if err != nil {
		return nil, err
	}
	config.AccessProfile = effectiveProfile
	config.posture = posture
	if (config.Catalog.HasProfile("acp/claude-code") || config.Catalog.HasProfile("acp/codex")) && !cleanAbsolutePath(config.WorkspaceRoot) {
		return nil, fmt.Errorf("coderig: ACP workspace root must be a clean absolute path")
	}
	registry := new(foreign.BuilderRegistry)
	var diagnostics []string
	profiles := []loop.RuntimeProfileName{"acp/claude-code", "acp/codex"}
	runnable := make(map[loop.AgentHarnessName]struct{}, len(profiles))
	for _, profile := range profiles {
		if !config.Catalog.HasProfile(profile) {
			continue
		}
		harness := loop.AgentHarnessName(strings.TrimPrefix(string(profile), "acp/"))
		executable := config.Executables[harness]
		if executable == "" {
			diagnostics = append(diagnostics, acpDiagnosticNoExecutable(harness))
			continue
		}
		if !preflightACPExecutable(executable) {
			diagnostics = append(diagnostics, acpDiagnosticExecutableNotRunnable(harness))
			continue
		}
		runnable[harness] = struct{}{}
	}
	staticCatalog, err := filterACPStaticCatalog(config.Catalog, runnable)
	if err != nil {
		return nil, err
	}
	config.Catalog = staticCatalog
	factory := &acpChildFactory{config: config}
	for _, profile := range []loop.RuntimeProfileName{"acp/claude-code", "acp/codex"} {
		if !config.Catalog.HasProfile(profile) {
			continue
		}
		if err := registry.Register(profile, factory.live, factory.restored); err != nil {
			return nil, err
		}
	}
	return &ACPComposition{
		Catalog:       config.Catalog,
		Registry:      registry,
		Live:          dispatchACPBuilder(registry),
		Restored:      dispatchACPRestoredBuilder(registry),
		Diagnostics:   diagnostics,
		accessProfile: config.AccessProfile,
		posture:       config.posture,
	}, nil
}

// acpDiagnosticNoExecutable produces a fixed, secret-free category string.
// It never includes stderr content, provider messages, URLs, tokens, or full
// filesystem paths.
func acpDiagnosticNoExecutable(harness loop.AgentHarnessName) string {
	envVar := ""
	switch harness {
	case "claude-code":
		envVar = acpClaudeExecutableEnv
	case "codex":
		envVar = acpCodexExecutableEnv
	}
	if envVar == "" {
		return fmt.Sprintf("acp: %s unavailable: no executable (set acp_launchers in models.json or its executable environment variable)", harness)
	}
	return fmt.Sprintf("acp: %s unavailable: no executable (set acp_launchers in models.json or %s)", harness, envVar)
}

// acpDiagnosticExecutableNotRunnable reports that a configured executable
// path (from acp_launchers or its environment-variable override) exists in
// configuration but failed the local stat/executable-bit check — distinct
// from no candidate having resolved at all, since the operator-facing
// remediation differs (fix the existing configuration vs. add configuration).
func acpDiagnosticExecutableNotRunnable(harness loop.AgentHarnessName) string {
	return fmt.Sprintf("acp: %s unavailable: configured executable not runnable (from acp_launchers)", harness)
}

type acpPreflightDecision struct {
	gatewayReady       bool
	gatewayAliases     map[loop.ModelAlias]struct{}
	nativeReady        bool
	nativeManagedReady bool
	nativeAliases      map[loop.ModelAlias]struct{}
}

type acpRuntimeModel struct {
	entry  loop.RuntimeCatalogEntry
	option loop.RuntimeModelOption
}

// preflightACPProfile is retained for explicit diagnostic/test callers. It is
// intentionally outside NewACPComposition's startup path.
func preflightACPProfile(ctx context.Context, config ACPChildrenConfig, harness loop.AgentHarnessName, preflight func(context.Context, ACPExecutableProbe) ACPPreflightResult) acpPreflightDecision {
	decision := acpPreflightDecision{
		gatewayAliases: make(map[loop.ModelAlias]struct{}),
		nativeAliases:  make(map[loop.ModelAlias]struct{}),
	}
	if ctx.Err() != nil {
		return decision
	}
	models := make([]acpRuntimeModel, 0)
	nativeManaged := false
	for _, entry := range config.Catalog.entries {
		if entry.AgentHarness != harness {
			continue
		}
		if entry.Source == loop.RuntimeSourceNative && entry.SelectionKind == loop.RuntimeSelectionHarnessManaged {
			nativeManaged = true
		}
		for _, option := range entry.Models {
			models = append(models, acpRuntimeModel{entry: entry, option: option})
		}
	}
	var gatewayModels, nativeModels []acpRuntimeModel
	for _, runtimeModel := range models {
		credential := runtimeModel.option.Credential
		if credential == "" {
			credential = runtimeModel.entry.Credential
		}
		switch credential {
		case loop.CredentialGatewayBacked:
			gatewayModels = append(gatewayModels, runtimeModel)
		case loop.CredentialNativeAuth:
			nativeModels = append(nativeModels, runtimeModel)
		}
	}

	// The gateway preflight and the native-managed preflight are each other's
	// only concurrent writers here, and they write disjoint state: the
	// gateway goroutine mutates decision.gatewayAliases (via the *decision
	// pointer passed to preflightACPSharedGateway) and reports its own
	// readiness through gatewayReady, a local variable the main goroutine
	// only reads after wg.Wait(); the native-managed goroutine writes only
	// its own local nativeManagedReady, never decision directly. Both are
	// folded into decision sequentially, after the join, so there is no
	// concurrent access to any single field. preflightNativeModels (the
	// explicit-native-alias path, which writes decision.nativeAliases) stays
	// out of this concurrent phase and runs afterward, sequentially, exactly
	// as before -- it is a per-alias sequential loop in its own right and
	// folding it in was judged not to be a clean, low-risk fit for this pass.
	var wg sync.WaitGroup
	var gatewayReady, nativeManagedReady bool
	if len(gatewayModels) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			gatewayReady = preflightACPSharedGateway(ctx, config, harness, gatewayModels, preflight, &decision)
		}()
	}
	if nativeManaged {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ctx.Err() != nil {
				return
			}
			result := preflight(ctx, ACPExecutableProbe{
				ACPNativeAuthProbe: ACPNativeAuthProbe{
					Harness:       harness,
					Executable:    config.Executables[harness],
					WorkspaceRoot: config.WorkspaceRoot,
					Env:           config.envForCredential(loop.CredentialNativeAuth),
				},
				Credential: loop.CredentialNativeAuth,
			})
			nativeManagedReady = result.Ready
		}()
	}
	wg.Wait()
	decision.gatewayReady = gatewayReady
	decision.nativeManagedReady = nativeManagedReady

	if len(nativeModels) > 0 && ctx.Err() == nil {
		preflightNativeModels(ctx, config, harness, nativeModels, preflight, &decision)
	}
	decision.nativeReady = decision.nativeManagedReady || len(decision.nativeAliases) > 0
	return decision
}

func preflightNativeModels(ctx context.Context, config ACPChildrenConfig, harness loop.AgentHarnessName, models []acpRuntimeModel, preflight func(context.Context, ACPExecutableProbe) ACPPreflightResult, decision *acpPreflightDecision) {
	aliases := make([]string, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, runtimeModel := range models {
		alias := string(runtimeModel.option.Alias)
		if alias == "" {
			continue
		}
		if _, exists := seen[alias]; exists {
			continue
		}
		seen[alias] = struct{}{}
		aliases = append(aliases, alias)
	}
	if len(aliases) == 0 {
		return
	}
	sort.Strings(aliases)
	for _, alias := range aliases {
		if ctx.Err() != nil {
			return
		}
		smallModel := ""
		if harness == "claude-code" {
			smallModel = alias
		}
		result := preflight(ctx, ACPExecutableProbe{
			ACPNativeAuthProbe: ACPNativeAuthProbe{
				Harness:       harness,
				Executable:    config.Executables[harness],
				WorkspaceRoot: config.WorkspaceRoot,
				Env:           config.envForCredential(loop.CredentialNativeAuth),
			},
			Credential: loop.CredentialNativeAuth,
			Model:      alias,
			SmallModel: smallModel,
			Models:     []string{alias},
		})
		if result.Ready {
			decision.nativeAliases[loop.ModelAlias(alias)] = struct{}{}
		}
	}
}

func preflightACPSharedGateway(ctx context.Context, config ACPChildrenConfig, harness loop.AgentHarnessName, models []acpRuntimeModel, preflight func(context.Context, ACPExecutableProbe) ACPPreflightResult, decision *acpPreflightDecision) bool {
	if ctx.Err() != nil {
		return false
	}
	binding, release, ok := gatewayPreflightBinding(ctx, config, harness)
	if !ok {
		return false
	}
	defer release()

	aliases := make([]string, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, runtimeModel := range models {
		for _, alias := range acpGatewayTargetAliases(config.Catalog, runtimeModel) {
			if _, exists := seen[alias]; exists {
				continue
			}
			seen[alias] = struct{}{}
			aliases = append(aliases, alias)
		}
	}
	if harness == "claude-code" {
		model, small := configuredACPPreflightModels(models)
		if model == "" || small == "" {
			return false
		}
		result := preflight(ctx, ACPExecutableProbe{
			ACPNativeAuthProbe: ACPNativeAuthProbe{
				Harness:       harness,
				Executable:    config.Executables[harness],
				WorkspaceRoot: config.WorkspaceRoot,
				Env:           config.envForCredential(loop.CredentialGatewayBacked),
			},
			Credential:  loop.CredentialGatewayBacked,
			Model:       model,
			SmallModel:  small,
			Models:      append([]string(nil), aliases...),
			SharedProxy: binding,
		})
		if !result.Ready {
			return false
		}
		advertised := make(map[string]struct{}, len(result.AdvertisedModels))
		for _, model := range result.AdvertisedModels {
			advertised[model] = struct{}{}
		}
		for _, alias := range aliases {
			if _, exists := advertised[alias]; exists {
				decision.gatewayAliases[loop.ModelAlias(alias)] = struct{}{}
			}
		}
		return len(decision.gatewayAliases) > 0
	}

	for _, alias := range aliases {
		if ctx.Err() != nil {
			return false
		}
		result := preflight(ctx, ACPExecutableProbe{
			ACPNativeAuthProbe: ACPNativeAuthProbe{
				Harness:       harness,
				Executable:    config.Executables[harness],
				WorkspaceRoot: config.WorkspaceRoot,
				Env:           config.envForCredential(loop.CredentialGatewayBacked),
			},
			Credential:  loop.CredentialGatewayBacked,
			Model:       alias,
			Models:      []string{alias},
			SharedProxy: binding,
		})
		if result.Ready {
			decision.gatewayAliases[loop.ModelAlias(alias)] = struct{}{}
		}
	}
	return len(decision.gatewayAliases) > 0
}

func configuredACPPreflightModels(models []acpRuntimeModel) (string, string) {
	for _, runtimeModel := range models {
		if runtimeModel.entry.DefaultModel != "" && runtimeModel.entry.SmallModel != "" {
			return string(runtimeModel.entry.DefaultModel), string(runtimeModel.entry.SmallModel)
		}
	}
	return "", ""
}

func acpGatewayTargetAliases(catalog ACPCompiledCatalog, runtimeModel acpRuntimeModel) []string {
	credential := runtimeModel.option.Credential
	if credential == "" {
		credential = runtimeModel.entry.Credential
	}
	if credential != loop.CredentialGatewayBacked {
		return nil
	}
	aliases := make([]string, 0, len(runtimeModel.option.Efforts))
	seen := make(map[string]struct{}, len(runtimeModel.option.Efforts))
	for _, effort := range runtimeModel.option.Efforts {
		resolved, err := catalog.RuntimeCatalog.ResolveWithExplicitEffort(
			runtimeModel.entry.AgentType,
			runtimeModel.entry.AgentHarness,
			runtimeModel.option.Alias,
			effort,
			true,
		)
		if err != nil || resolved.Credential != loop.CredentialGatewayBacked {
			continue
		}
		alias := resolved.TargetAlias
		if alias == "" {
			alias = resolved.ModelAlias
		}
		if _, exists := seen[string(alias)]; exists {
			continue
		}
		seen[string(alias)] = struct{}{}
		aliases = append(aliases, string(alias))
	}
	return aliases
}

func gatewayPreflightBinding(ctx context.Context, config ACPChildrenConfig, harness loop.AgentHarnessName) (*launch.ProxyBinding, func(), bool) {
	if config.gatewayPreflightBinding != nil {
		binding := *config.gatewayPreflightBinding
		return &binding, func() {}, binding.BaseURL != "" && binding.Token != ""
	}
	if config.executablePreflight != nil {
		// A deterministic test preflight still receives the same SharedProxy
		// shape as production without requiring a listener or network access.
		return &launch.ProxyBinding{BaseURL: "http://127.0.0.1:1", Token: "preflight"}, func() {}, true
	}
	resolved, ok := firstACPGatewayResolved(config.Catalog, harness)
	if !ok {
		return nil, func() {}, false
	}
	owned, err := NewACPGateway(ctx, config.Catalog, resolved)
	if err != nil || owned == nil {
		return nil, func() {}, false
	}
	binding := owned.Binding()
	if binding.BaseURL == "" || binding.Token == "" {
		_ = owned.Close(context.Background())
		return nil, func() {}, false
	}
	return &binding, func() { _ = owned.Close(context.Background()) }, true
}

func firstACPGatewayResolved(catalog ACPCompiledCatalog, harness loop.AgentHarnessName) (loop.Resolved, bool) {
	for _, entry := range catalog.entries {
		if entry.AgentHarness != harness {
			continue
		}
		for _, option := range entry.Models {
			credential := option.Credential
			if credential == "" {
				credential = entry.Credential
			}
			if credential != loop.CredentialGatewayBacked {
				continue
			}
			resolved, err := catalog.RuntimeCatalog.ResolveWithExplicitEffort(entry.AgentType, harness, option.Alias, option.DefaultEffort, true)
			if err == nil && resolved.Credential == loop.CredentialGatewayBacked {
				return resolved, true
			}
		}
	}
	return loop.Resolved{}, false
}

// filterACPPreflightCatalog is retained for explicit diagnostic/test callers.
// Production composition uses filterACPStaticCatalog and never applies live
// adapter availability to the configured catalog.
func filterACPPreflightCatalog(catalog ACPCompiledCatalog, decisions map[loop.AgentHarnessName]acpPreflightDecision) (ACPCompiledCatalog, error) {
	// The ordinary Generic row is always retained and remains the sole
	// product default. ACP entries were compiled non-default and are only
	// retained when their source-specific preflight succeeds.
	entries := make([]loop.RuntimeCatalogEntry, 0, len(catalog.entries))
	for _, source := range catalog.entries {
		if source.AgentHarness == looprigRuntimeHarness && source.Profile == looprigRuntimeProfile {
			entries = append(entries, cloneACPEntry(source))
			continue
		}
		decision, ok := decisions[source.AgentHarness]
		if !ok {
			continue
		}
		entry := cloneACPEntry(source)
		if entry.Source == loop.RuntimeSourceNative && entry.SelectionKind == loop.RuntimeSelectionHarnessManaged {
			if !decision.nativeManagedReady {
				continue
			}
			entries = append(entries, entry)
			continue
		}
		models := make([]loop.RuntimeModelOption, 0, len(entry.Models))
		for _, option := range entry.Models {
			credential := option.Credential
			if credential == "" {
				credential = entry.Credential
			}
			switch credential {
			case loop.CredentialGatewayBacked:
				if !decision.gatewayReady {
					continue
				}
				retainedEfforts := make([]model.Effort, 0, len(option.Efforts))
				for _, effort := range option.Efforts {
					resolved, err := catalog.RuntimeCatalog.ResolveWithExplicitEffort(entry.AgentType, entry.AgentHarness, option.Alias, effort, true)
					if err != nil {
						continue
					}
					alias := resolved.TargetAlias
					if alias == "" {
						alias = resolved.ModelAlias
					}
					if _, allowed := decision.gatewayAliases[alias]; allowed {
						retainedEfforts = append(retainedEfforts, effort)
					}
				}
				if len(retainedEfforts) == 0 {
					continue
				}
				if !containsModelEffort(retainedEfforts, option.DefaultEffort) {
					// The default concrete target is part of the catalog contract.
					// Promoting a preflighted non-default route would derive a new
					// bare alias that the adapter never advertised.
					continue
				}
				option.Efforts = retainedEfforts
			case loop.CredentialNativeAuth:
				if !decision.nativeReady {
					continue
				}
				if _, allowed := decision.nativeAliases[option.Alias]; !allowed {
					continue
				}
			default:
				continue
			}
			models = append(models, option)
		}
		if len(models) == 0 {
			continue
		}
		entry.Models = models
		if !hasACPModelAlias(entry.Models, entry.DefaultModel) {
			if entry.Source != loop.RuntimeSourceNative || entry.SelectionKind != loop.RuntimeSelectionExplicit || len(entry.Models) == 0 {
				continue
			}
			if entry.Default {
				continue
			}
			entry.DefaultModel = entry.Models[0].Alias
		}
		if entry.SmallModel != "" && !hasACPModelAlias(entry.Models, entry.SmallModel) {
			continue
		}
		if entry.NeedsSmallModel && !hasACPDefaultModel(entry, catalog.RuntimeCatalog) {
			continue
		}
		if entry.NeedsSmallModel && entry.SmallModel == "" {
			continue
		}
		entries = append(entries, entry)
	}
	catalogRuntime, err := loop.NewRuntimeCatalog(entries)
	if err != nil {
		return ACPCompiledCatalog{}, err
	}
	profiles := make(map[loop.RuntimeProfileName]struct{}, len(entries))
	for _, entry := range entries {
		profiles[entry.Profile] = struct{}{}
	}
	return ACPCompiledCatalog{
		RuntimeCatalog: catalogRuntime,
		gatewayTargets: catalog.gatewayTargets,
		profiles:       profiles,
		entries:        cloneACPEntries(entries),
	}, nil
}

// filterACPStaticCatalog removes only profiles that cannot be launched under
// their configured executable path. It intentionally does not inspect an ACP
// process, session, advertised models, login state, quota, or network: those
// are runtime concerns and the configured catalog remains authoritative for a
// statically runnable harness.
func filterACPStaticCatalog(catalog ACPCompiledCatalog, runnable map[loop.AgentHarnessName]struct{}) (ACPCompiledCatalog, error) {
	entries := make([]loop.RuntimeCatalogEntry, 0, len(catalog.entries))
	for _, entry := range catalog.entries {
		if entry.AgentHarness == looprigRuntimeHarness && entry.Profile == looprigRuntimeProfile {
			entries = append(entries, cloneACPEntry(entry))
			continue
		}
		if _, ok := runnable[entry.AgentHarness]; ok {
			entries = append(entries, cloneACPEntry(entry))
		}
	}
	runtimeCatalog, err := loop.NewRuntimeCatalog(entries)
	if err != nil {
		return ACPCompiledCatalog{}, err
	}
	profiles := make(map[loop.RuntimeProfileName]struct{}, len(entries))
	for _, entry := range entries {
		profiles[entry.Profile] = struct{}{}
	}
	return ACPCompiledCatalog{
		RuntimeCatalog: runtimeCatalog,
		gatewayTargets: catalog.gatewayTargets,
		profiles:       profiles,
		entries:        cloneACPEntries(entries),
	}, nil
}

func containsModelEffort(efforts []model.Effort, wanted model.Effort) bool {
	for _, effort := range efforts {
		if effort == wanted {
			return true
		}
	}
	return false
}

func hasACPDefaultModel(entry loop.RuntimeCatalogEntry, catalog loop.RuntimeCatalog) bool {
	if entry.SmallModel == "" {
		return false
	}
	for _, option := range entry.Models {
		if option.Alias != entry.SmallModel {
			continue
		}
		resolved, err := catalog.ResolveWithExplicitEffort(entry.AgentType, entry.AgentHarness, option.Alias, option.DefaultEffort, true)
		if err != nil {
			return false
		}
		targetAlias := resolved.TargetAlias
		if targetAlias == "" {
			targetAlias = resolved.ModelAlias
		}
		return targetAlias == option.Alias
	}
	return false
}

func hasACPModelAlias(models []loop.RuntimeModelOption, alias loop.ModelAlias) bool {
	for _, option := range models {
		if option.Alias == alias {
			return true
		}
	}
	return false
}

func preflightACPExecutable(path string) bool {
	if !cleanAbsolutePath(path) {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode()&0111 != 0
}

type acpChildFactory struct {
	config ACPChildrenConfig
}

func (f *acpChildFactory) live(
	loopCtx context.Context,
	sessionID, loopID uuid.UUID,
	parent loop.Provenance,
	pub foreign.EventPublisher,
	cfg loop.BoundDefinition,
	idGen func() (uuid.UUID, error),
	fac *event.Factory,
) (loop.Backend, string, error) {
	_, acpConfig, ownedGateway, err := f.configFor(loopCtx, cfg, "")
	if err != nil {
		return nil, "", boundedACPChildError(err)
	}
	backend, sid, err := acpdriver.BuildWith(acpConfig)(loopCtx, sessionID, loopID, parent, pub, cfg, idGen, fac)
	if err != nil {
		_ = ownedGateway.Close(context.Background())
		return nil, "", boundedACPChildError(err)
	}
	if backend == nil {
		_ = ownedGateway.Close(context.Background())
		return nil, "", boundedACPChildError(errors.New("coderig: ACP builder returned no backend"))
	}
	return wrapACPGatewayBackend(backend, ownedGateway), sid, nil
}

func (f *acpChildFactory) restored(
	loopCtx context.Context,
	sessionID, loopID uuid.UUID,
	parent loop.Provenance,
	pub foreign.EventPublisher,
	cfg loop.BoundDefinition,
	idGen func() (uuid.UUID, error),
	fac *event.Factory,
	seed foreign.RestoredForeign,
) (loop.Backend, error) {
	_, acpConfig, ownedGateway, err := f.configFor(loopCtx, cfg, seed.AgentSessionID)
	if err != nil {
		return nil, boundedACPChildError(err)
	}
	backend, err := acpdriver.BuildRestoredWith(acpConfig)(loopCtx, sessionID, loopID, parent, pub, cfg, idGen, fac, seed)
	if err != nil {
		_ = ownedGateway.Close(context.Background())
		return nil, boundedACPChildError(err)
	}
	if backend == nil {
		_ = ownedGateway.Close(context.Background())
		return nil, boundedACPChildError(errors.New("coderig: ACP restored builder returned no backend"))
	}
	return wrapACPGatewayBackend(backend, ownedGateway), nil
}

func (f *acpChildFactory) configFor(ctx context.Context, cfg loop.BoundDefinition, agentSessionID string) (loop.Resolved, acpdriver.Config, *ACPGateway, error) {
	resolved, harness, err := resolveACPBoundRuntime(f.config.Catalog, cfg)
	if err != nil {
		return loop.Resolved{}, acpdriver.Config{}, nil, err
	}
	posture := f.config.posture
	if !posture.Valid() {
		return loop.Resolved{}, acpdriver.Config{}, nil, errACPAccessProfileUnavailable
	}
	// Revalidate the captured posture against the session-fixed profile before
	// every builder invocation. This prevents a mismatched test seam or future
	// composition change from widening ACP authority.
	expectedPosture, err := acpPostureFor(f.config.AccessProfile)
	if err != nil || posture != expectedPosture {
		return loop.Resolved{}, acpdriver.Config{}, nil, errACPAccessProfileUnavailable
	}
	var ownedGateway *ACPGateway
	if resolved.Credential == loop.CredentialGatewayBacked {
		ownedGateway, err = NewACPGateway(ctx, f.config.Catalog, resolved)
		if err != nil {
			return loop.Resolved{}, acpdriver.Config{}, nil, err
		}
	}
	binding := launch.ProxyBinding{}
	if ownedGateway != nil {
		binding = ownedGateway.Binding()
	}
	modelAlias, smallModelAlias, err := acpChildModelAliases(f.config.Catalog, cfg.Name(), harness, resolved)
	if err != nil {
		_ = ownedGateway.Close(context.Background())
		return loop.Resolved{}, acpdriver.Config{}, nil, err
	}
	effort := ""
	if resolved.Credential == loop.CredentialNativeAuth {
		effort = string(resolved.Effort)
	}
	return resolved, acpdriver.Config{
		Harness:         acpdriver.Harness(harness),
		Executable:      f.config.Executables[harness],
		Env:             f.config.envForCredential(resolved.Credential),
		Credential:      resolved.Credential,
		Binding:         binding,
		ModelAlias:      modelAlias,
		Effort:          effort,
		SmallModelAlias: smallModelAlias,
		Posture:         posture,
		AgentSessionID:  agentSessionID,
		WorkspaceRoot:   f.config.WorkspaceRoot,
	}, ownedGateway, nil
}

func acpChildModelAliases(catalog ACPCompiledCatalog, agent identity.AgentName, harness loop.AgentHarnessName, resolved loop.Resolved) (string, string, error) {
	if resolved.Credential == loop.CredentialNativeAuth {
		if resolved.SelectionKind == loop.RuntimeSelectionHarnessManaged {
			return "", "", nil
		}
		if resolved.ModelAlias == "" {
			return "", "", fmt.Errorf("coderig: native ACP model unavailable")
		}
		modelAlias := string(resolved.ModelAlias)
		smallModelAlias := resolved.NativeSmallModel
		if harness == "claude-code" && smallModelAlias == "" {
			smallModelAlias = modelAlias
		}
		return modelAlias, smallModelAlias, nil
	}
	modelAlias := string(resolved.TargetAlias)
	if modelAlias == "" {
		return "", "", fmt.Errorf("coderig: ACP target alias unavailable")
	}
	if harness != "claude-code" || resolved.SmallModel == "" {
		return modelAlias, "", nil
	}
	smallResolved, err := catalog.RuntimeCatalog.ResolveWithExplicitEffort(agent, harness, resolved.SmallModel, model.EffortNone, false)
	if err != nil || smallResolved.Credential != loop.CredentialGatewayBacked || smallResolved.TargetAlias == "" {
		return "", "", fmt.Errorf("coderig: ACP small target alias unavailable")
	}
	return modelAlias, string(smallResolved.TargetAlias), nil
}

func (c ACPChildrenConfig) envForCredential(credential loop.CredentialMode) []string {
	allowlist := c.GatewayEnvAllowlist
	if credential == loop.CredentialNativeAuth {
		allowlist = c.NativeEnvAllowlist
	}
	if len(allowlist) == 0 {
		allowlist = c.EnvAllowlist
	}
	if credential == loop.CredentialGatewayBacked {
		// Even legacy callers that supply only EnvAllowlist must not be able
		// to pass harness login locations to a gateway-backed child.
		allowlist = intersectEnvAllowlists(allowlist, acpGatewayEnvAllowlist)
	}
	return filterACPEnv(c.Env, allowlist)
}

func intersectEnvAllowlists(left, right []string) []string {
	allowed := make(map[string]struct{}, len(right))
	for _, name := range right {
		allowed[name] = struct{}{}
	}
	result := make([]string, 0, len(left))
	for _, name := range left {
		if _, ok := allowed[name]; ok {
			result = append(result, name)
		}
	}
	return result
}

func resolveACPBoundRuntime(catalog ACPCompiledCatalog, cfg loop.BoundDefinition) (loop.Resolved, loop.AgentHarnessName, error) {
	identity := cfg.RuntimeIdentity()
	profile := cfg.RuntimeProfile()
	if profile == "" || !catalog.HasProfile(profile) {
		return loop.Resolved{}, "", fmt.Errorf("coderig: ACP runtime selection unavailable")
	}
	harness := loop.AgentHarnessName(strings.TrimPrefix(string(profile), "acp/"))
	var resolved loop.Resolved
	var err error
	if identity.SelectionKind == loop.RuntimeSelectionHarnessManaged {
		if identity.Source != loop.RuntimeSourceNative || identity.ModelAlias != "" || identity.Effort != model.EffortNone {
			return loop.Resolved{}, "", fmt.Errorf("coderig: ACP runtime selection unavailable")
		}
		resolved, err = catalog.RuntimeCatalog.ResolveTargetAliasWithSource(cfg.Name(), harness, identity.Source, "", model.EffortNone)
	} else {
		if identity.ModelAlias == "" {
			return loop.Resolved{}, "", fmt.Errorf("coderig: ACP runtime selection unavailable")
		}
		resolved, err = catalog.RuntimeCatalog.ResolveTargetAliasWithSource(cfg.Name(), harness, identity.Source, identity.ModelAlias, identity.Effort)
	}
	if err != nil || resolved.Profile != profile {
		return loop.Resolved{}, "", fmt.Errorf("coderig: ACP runtime selection unavailable")
	}
	if identity.Source != "" && resolved.Source != identity.Source {
		return loop.Resolved{}, "", fmt.Errorf("coderig: ACP runtime selection unavailable")
	}
	if identity.SelectionKind != "" && resolved.SelectionKind != identity.SelectionKind {
		return loop.Resolved{}, "", fmt.Errorf("coderig: ACP runtime selection unavailable")
	}
	return resolved, harness, nil
}

func acpPostureFor(profile AccessProfile) (driver.Posture, error) {
	effective, err := normalizeAccessProfile(profile)
	if err != nil {
		return "", errACPAccessProfileUnavailable
	}
	switch effective {
	case AccessReadOnly:
		return driver.PostureReadOnly, nil
	case AccessTrusted, AccessUnconfined:
		return driver.PostureWorkspaceWrite, nil
	default:
		return "", errACPAccessProfileUnavailable
	}
}

func filterACPEnv(env, allowlist []string) []string {
	if len(allowlist) == 0 {
		return nil
	}
	allowed := make(map[string]struct{}, len(allowlist))
	for _, name := range allowlist {
		allowed[name] = struct{}{}
	}
	result := make([]string, 0, len(env))
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		if _, ok := allowed[name]; ok {
			result = append(result, entry)
		}
	}
	return result
}

func dispatchACPBuilder(registry *foreign.BuilderRegistry) foreign.Builder {
	return func(loopCtx context.Context, sessionID, loopID uuid.UUID, parent loop.Provenance, pub foreign.EventPublisher, cfg loop.BoundDefinition, idGen func() (uuid.UUID, error), fac *event.Factory) (loop.Backend, string, error) {
		builder, _, err := registry.Builder(cfg.RuntimeProfile())
		if err != nil {
			return nil, "", boundedACPChildError(err)
		}
		return builder(loopCtx, sessionID, loopID, parent, pub, cfg, idGen, fac)
	}
}

func dispatchACPRestoredBuilder(registry *foreign.BuilderRegistry) foreign.RestoredBuilder {
	return func(loopCtx context.Context, sessionID, loopID uuid.UUID, parent loop.Provenance, pub foreign.EventPublisher, cfg loop.BoundDefinition, idGen func() (uuid.UUID, error), fac *event.Factory, seed foreign.RestoredForeign) (loop.Backend, error) {
		_, builder, err := registry.Builder(cfg.RuntimeProfile())
		if err != nil {
			return nil, boundedACPChildError(err)
		}
		return builder(loopCtx, sessionID, loopID, parent, pub, cfg, idGen, fac, seed)
	}
}

type acpGatewayBackend struct {
	loop.Backend
	done <-chan struct{}
}

func wrapACPGatewayBackend(backend loop.Backend, ownedGateway *ACPGateway) loop.Backend {
	if ownedGateway == nil {
		return backend
	}
	done := make(chan struct{})
	go func() {
		<-backend.DoneChan()
		_ = ownedGateway.Close(context.Background())
		close(done)
	}()
	return &acpGatewayBackend{Backend: backend, done: done}
}

func (b *acpGatewayBackend) DoneChan() <-chan struct{} { return b.done }

var _ foreign.Builder = (*acpChildFactory)(nil).live
var _ foreign.RestoredBuilder = (*acpChildFactory)(nil).restored
var _ loop.Backend = (*acpGatewayBackend)(nil)

func cleanAbsolutePath(path string) bool {
	return path != "" && filepath.IsAbs(path) && filepath.Clean(path) == path
}
