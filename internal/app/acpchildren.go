package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/looprig/acp/launch"
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

// boundedACPChildError is the model-facing error boundary for ACP startup and
// restore. ACP launch/RPC/stdio errors can contain executable paths, login
// locations, URLs, provider messages, or stderr; none of those details belong
// in a Subagent result or durable Harness error. Keep cancellation recognizable
// for controller shutdown, but collapse every other cause to one fixed result.
func boundedACPChildError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return errACPChildUnavailable
}

// ACPChildrenConfig is the composition-root input for delegated ACP loops.
// Executable paths are preflighted before a profile is registered. Env is
// reduced to EnvAllowlist before it reaches the child process.
type ACPChildrenConfig struct {
	Catalog       ACPCompiledCatalog
	Executables   map[loop.AgentHarnessName]string
	WorkspaceRoot string
	Env           []string
	// EnvAllowlist is the compatibility fallback for callers that predate
	// credential-specific allowlists. Production supplies both mode-specific
	// lists below.
	EnvAllowlist        []string
	NativeEnvAllowlist  []string
	GatewayEnvAllowlist []string
	// executablePreflight is an internal test seam. Production leaves it nil,
	// which performs a bounded ACP initialize/session probe before advertising
	// a profile; focused composition tests can replace the process probe with a
	// deterministic result. The probe is credential-scoped so a gateway row is
	// never admitted by a native-login check.
	executablePreflight func(context.Context, ACPExecutableProbe) ACPPreflightResult
	// gatewayPreflightBinding avoids starting a loopback listener in focused
	// composition tests. Production leaves it nil and NewACPComposition owns a
	// short-lived ACPGateway solely for the SharedProxy preflight binding.
	gatewayPreflightBinding *launch.ProxyBinding
}

// ACPExecutableProbe is the bounded, secret-free input to one ACP startup
// preflight. Models contains model-facing aliases for a Claude session or a
// single exact launch model for Codex. SharedProxy is set only for a
// gateway-backed child; native-auth probes use NoProxy and keep it nil.
type ACPExecutableProbe struct {
	ACPNativeAuthProbe
	Credential  loop.CredentialMode
	Model       string
	Models      []string
	SharedProxy *launch.ProxyBinding
}

// ACPPreflightResult is the bounded result of an ACP initialize/session probe.
// Claude uses AdvertisedModels to close the catalog to values the adapter
// actually offered. Codex is checked by launching each candidate in turn and
// therefore only needs Ready.
type ACPPreflightResult struct {
	Ready            bool
	AdvertisedModels []string
}

// ACPComposition is the immutable CodeRig-to-Harness bridge for ACP children.
// The registry is retained for inspection and the function pair is the narrow
// legacy rig option; dispatch still selects by bound RuntimeProfile.
type ACPComposition struct {
	Catalog  ACPCompiledCatalog
	Registry *foreign.BuilderRegistry
	Live     foreign.Builder
	Restored foreign.RestoredBuilder
}

// NewACPComposition preflights configured executable paths, registers only
// cataloged ACP profiles, and returns a registry-backed builder pair. Missing
// executables simply omit that harness; native primers remain usable.
func NewACPComposition(config ACPChildrenConfig) (*ACPComposition, error) {
	if (config.Catalog.HasProfile("acp/claude-code") || config.Catalog.HasProfile("acp/codex")) && !cleanAbsolutePath(config.WorkspaceRoot) {
		return nil, fmt.Errorf("coderig: ACP workspace root must be a clean absolute path")
	}
	registry := new(foreign.BuilderRegistry)
	preflight := config.executablePreflight
	if preflight == nil {
		preflight = func(ctx context.Context, probe ACPExecutableProbe) ACPPreflightResult {
			return preflightProductionACPExecutable(ctx, probe)
		}
	}
	decisions := make(map[loop.AgentHarnessName]acpPreflightDecision)
	for _, profile := range []loop.RuntimeProfileName{"acp/claude-code", "acp/codex"} {
		if !config.Catalog.HasProfile(profile) {
			continue
		}
		harness := loop.AgentHarnessName(strings.TrimPrefix(string(profile), "acp/"))
		if !preflightACPExecutable(config.Executables[harness]) {
			continue
		}
		decision := preflightACPProfile(context.Background(), config, harness, preflight)
		if decision.gatewayReady || decision.nativeReady {
			decisions[harness] = decision
		}
	}
	filtered, err := filterACPPreflightCatalog(config.Catalog, decisions)
	if err != nil {
		return nil, err
	}
	config.Catalog = filtered
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
		Catalog:  config.Catalog,
		Registry: registry,
		Live:     dispatchACPBuilder(registry),
		Restored: dispatchACPRestoredBuilder(registry),
	}, nil
}

type acpPreflightDecision struct {
	gatewayReady   bool
	gatewayAliases map[loop.ModelAlias]struct{}
	nativeReady    bool
}

type acpRuntimeModel struct {
	entry  loop.RuntimeCatalogEntry
	option loop.RuntimeModelOption
}

func preflightACPProfile(ctx context.Context, config ACPChildrenConfig, harness loop.AgentHarnessName, preflight func(context.Context, ACPExecutableProbe) ACPPreflightResult) acpPreflightDecision {
	decision := acpPreflightDecision{gatewayAliases: make(map[loop.ModelAlias]struct{})}
	models := make([]acpRuntimeModel, 0)
	for _, entry := range config.Catalog.entries {
		if entry.AgentHarness != harness {
			continue
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

	if len(gatewayModels) > 0 && preflightACPSharedGateway(ctx, config, harness, gatewayModels, preflight, &decision) {
		decision.gatewayReady = true
	}
	if len(nativeModels) > 0 {
		probe := ACPExecutableProbe{
			ACPNativeAuthProbe: ACPNativeAuthProbe{
				Harness:       harness,
				Executable:    config.Executables[harness],
				WorkspaceRoot: config.WorkspaceRoot,
				Env:           config.envForCredential(loop.CredentialNativeAuth),
			},
			Credential: loop.CredentialNativeAuth,
			Model:      nativeModels[0].option.Target.Name,
		}
		result := preflight(ctx, probe)
		decision.nativeReady = result.Ready
	}
	return decision
}

func preflightACPSharedGateway(ctx context.Context, config ACPChildrenConfig, harness loop.AgentHarnessName, models []acpRuntimeModel, preflight func(context.Context, ACPExecutableProbe) ACPPreflightResult, decision *acpPreflightDecision) bool {
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
		model := "sonnet-5"
		if _, exists := seen[model]; !exists && len(aliases) > 0 {
			model = aliases[0]
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
			runtimeModel.entry.SubagentType,
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
			resolved, err := catalog.RuntimeCatalog.ResolveWithExplicitEffort(entry.SubagentType, harness, option.Alias, option.DefaultEffort, true)
			if err == nil && resolved.Credential == loop.CredentialGatewayBacked {
				return resolved, true
			}
		}
	}
	return loop.Resolved{}, false
}

func filterACPPreflightCatalog(catalog ACPCompiledCatalog, decisions map[loop.AgentHarnessName]acpPreflightDecision) (ACPCompiledCatalog, error) {
	entries := make([]loop.RuntimeCatalogEntry, 0, len(catalog.entries))
	for _, source := range catalog.entries {
		decision, ok := decisions[source.AgentHarness]
		if !ok {
			continue
		}
		entry := cloneACPEntry(source)
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
					resolved, err := catalog.RuntimeCatalog.ResolveWithExplicitEffort(entry.SubagentType, entry.AgentHarness, option.Alias, effort, true)
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
					if entry.NeedsSmallModel && option.Alias == entry.SmallModel {
						// Claude's small model is fixed to its default target. A
						// concrete non-default route cannot satisfy that contract.
						continue
					}
					option.DefaultEffort = retainedEfforts[0]
				}
				option.Efforts = retainedEfforts
			case loop.CredentialNativeAuth:
				if !decision.nativeReady {
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
			entry.DefaultModel = entry.Models[0].Alias
		}
		if entry.SmallModel != "" && !hasACPModelAlias(entry.Models, entry.SmallModel) {
			entry.SmallModel = ""
			for _, option := range entry.Models {
				credential := option.Credential
				if credential == "" {
					credential = entry.Credential
				}
				if credential == loop.CredentialNativeAuth {
					entry.SmallModel = option.Alias
					break
				}
			}
		}
		if entry.NeedsSmallModel && !hasACPDefaultModel(entry, catalog.RuntimeCatalog) {
			continue
		}
		if entry.NeedsSmallModel && entry.SmallModel == "" {
			continue
		}
		entries = append(entries, entry)
	}
	for i := range entries {
		if entries[i].Default {
			continue
		}
		defaultFound := false
		for _, other := range entries {
			if other.SubagentType == entries[i].SubagentType && other.Default {
				defaultFound = true
				break
			}
		}
		if !defaultFound {
			entries[i].Default = true
		}
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
		resolved, err := catalog.ResolveWithExplicitEffort(entry.SubagentType, entry.AgentHarness, option.Alias, option.DefaultEffort, true)
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

func (f *acpChildFactory) configFor(ctx context.Context, cfg loop.BoundDefinition, _ string) (loop.Resolved, acpdriver.Config, *ACPGateway, error) {
	resolved, harness, err := resolveACPBoundRuntime(f.config.Catalog, cfg)
	if err != nil {
		return loop.Resolved{}, acpdriver.Config{}, nil, err
	}
	posture, err := acpPostureFor(string(cfg.Name()))
	if err != nil {
		return loop.Resolved{}, acpdriver.Config{}, nil, err
	}
	ownedGateway, err := NewACPGateway(ctx, f.config.Catalog, resolved)
	if err != nil {
		return loop.Resolved{}, acpdriver.Config{}, nil, err
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
	return resolved, acpdriver.Config{
		Harness:         acpdriver.Harness(harness),
		Executable:      f.config.Executables[harness],
		Env:             f.config.envForCredential(resolved.Credential),
		Credential:      resolved.Credential,
		Binding:         binding,
		ModelAlias:      modelAlias,
		SmallModelAlias: smallModelAlias,
		Posture:         posture,
		WorkspaceRoot:   f.config.WorkspaceRoot,
	}, ownedGateway, nil
}

func acpChildModelAliases(catalog ACPCompiledCatalog, role identity.AgentName, harness loop.AgentHarnessName, resolved loop.Resolved) (string, string, error) {
	if resolved.Credential == loop.CredentialNativeAuth {
		if resolved.Target.Name == "" {
			return "", "", fmt.Errorf("coderig: native ACP model unavailable")
		}
		smallModelAlias := resolved.NativeSmallModel
		if harness == "claude-code" && smallModelAlias == "" {
			smallModelAlias = resolved.Target.Name
		}
		return resolved.Target.Name, smallModelAlias, nil
	}
	modelAlias := string(resolved.TargetAlias)
	if modelAlias == "" {
		return "", "", fmt.Errorf("coderig: ACP target alias unavailable")
	}
	if harness != "claude-code" || resolved.SmallModel == "" {
		return modelAlias, "", nil
	}
	smallResolved, err := catalog.RuntimeCatalog.ResolveWithExplicitEffort(role, harness, resolved.SmallModel, model.EffortNone, false)
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
	if profile == "" || identity.ModelAlias == "" || !catalog.HasProfile(profile) {
		return loop.Resolved{}, "", fmt.Errorf("coderig: ACP runtime selection unavailable")
	}
	harness := loop.AgentHarnessName(strings.TrimPrefix(string(profile), "acp/"))
	resolved, err := catalog.RuntimeCatalog.ResolveTargetAlias(cfg.Name(), harness, identity.ModelAlias, identity.Effort)
	if err != nil || resolved.Profile != profile {
		return loop.Resolved{}, "", fmt.Errorf("coderig: ACP runtime selection unavailable")
	}
	return resolved, harness, nil
}

func acpPostureFor(role string) (driver.Posture, error) {
	switch role {
	case "planner", "reviewer":
		return driver.PostureReadOnly, nil
	case "builder":
		return driver.PostureWorkspaceWrite, nil
	default:
		return "", fmt.Errorf("coderig: unsupported ACP role posture")
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
			return nil, "", err
		}
		return builder(loopCtx, sessionID, loopID, parent, pub, cfg, idGen, fac)
	}
}

func dispatchACPRestoredBuilder(registry *foreign.BuilderRegistry) foreign.RestoredBuilder {
	return func(loopCtx context.Context, sessionID, loopID uuid.UUID, parent loop.Provenance, pub foreign.EventPublisher, cfg loop.BoundDefinition, idGen func() (uuid.UUID, error), fac *event.Factory, seed foreign.RestoredForeign) (loop.Backend, error) {
		_, builder, err := registry.Builder(cfg.RuntimeProfile())
		if err != nil {
			return nil, err
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
