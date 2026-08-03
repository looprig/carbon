package app

import (
	"fmt"
	"sort"

	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/inference"
	"github.com/looprig/inference/gateway"
	model "github.com/looprig/inference/model"
)

// ACPGatewaySource is one secret-free gateway-backed model row. Client is
// already bound to its provider credential; credentials never enter this
// catalogue or the model-facing runtime selection.
type ACPGatewaySource struct {
	Alias         loop.ModelAlias
	Client        inference.Client
	Model         model.Model
	DefaultEffort model.Effort
	Efforts       []model.Effort
}

// ACPNativeAuthSource describes one model exposed by a harness's own login.
// Native-auth rows are deliberately not gateway targets.
type ACPNativeAuthSource struct {
	Harness loop.AgentHarnessName
	Alias   loop.ModelAlias
	Model   model.Model
	// SmallModel is the exact model value accepted by the native harness for
	// Claude's required small-model selector. It is kept outside the
	// model-facing alias so the ACP process receives the connector's own value.
	SmallModel    string
	DefaultEffort model.Effort
	Efforts       []model.Effort
}

// ACPNativeProfile is one normalized native ACP profile. A nil Models slice
// means harness-managed selection; a non-empty slice constrains explicit
// native model aliases. Disabled profiles are retained here for digest and
// composition identity but never compile into runtime rows.
type ACPNativeProfile struct {
	Harness loop.AgentHarnessName
	Enabled bool
	Models  []loop.ModelAlias
}

// ACPCatalogInput contains already-validated, credential-bound runtime inputs.
// NativeAuth remains only for lower-level compatibility while its production
// discovery is removed in the next composition-root phase.
type ACPCatalogInput struct {
	SubagentTypes  []identity.AgentName
	GatewayTargets []ACPGatewaySource
	Defaults       map[identity.AgentName]configuredDelegateDefault
	ClaudeSmall    loop.ModelAlias
	NativeACP      map[string]ACPNativeProfile
	NativeProfiles []ACPNativeProfile
	NativeAuth     []ACPNativeAuthSource
}

// ACPCompiledCatalog is the single compiled source of truth consumed by both
// Harness runtime selection and the per-child gateway constructor.
type ACPCompiledCatalog struct {
	RuntimeCatalog loop.RuntimeCatalog

	gatewayTargets map[loop.ModelAlias]ACPGatewaySource
	profiles       map[loop.RuntimeProfileName]struct{}
	entries        []loop.RuntimeCatalogEntry
}

// CompileACPCatalog compiles configured gateway-backed targets together with
// optional native ACP profiles. When no roles are supplied, the product's
// three roles are used.
func CompileACPCatalog(input ACPCatalogInput) (ACPCompiledCatalog, error) {
	roles := normalizedACPSubagentTypes(input.SubagentTypes)
	rows, gatewayRows, err := compileACPGatewayRows(input.GatewayTargets)
	if err != nil {
		return ACPCompiledCatalog{}, err
	}

	nativeInputProfiles := append([]ACPNativeProfile(nil), input.NativeProfiles...)
	if len(input.NativeACP) != 0 {
		keys := make([]string, 0, len(input.NativeACP))
		for harness := range input.NativeACP {
			keys = append(keys, harness)
		}
		sort.Strings(keys)
		for _, harness := range keys {
			profile := input.NativeACP[harness]
			if profile.Harness == "" {
				profile.Harness = loop.AgentHarnessName(harness)
			}
			nativeInputProfiles = append(nativeInputProfiles, profile)
		}
	}
	nativeProfiles, nativeByHarness, err := compileACPNativeProfiles(nativeInputProfiles)
	if err != nil {
		return ACPCompiledCatalog{}, err
	}
	nativeAliases := make(map[loop.ModelAlias]struct{})
	for _, options := range nativeByHarness {
		for _, option := range options {
			if _, collision := gatewayRows[option.Alias]; collision {
				return ACPCompiledCatalog{}, fmt.Errorf("coderig: ACP credential alias collision")
			}
			nativeAliases[option.Alias] = struct{}{}
		}
	}
	// NativeAuth is retained as a compatibility seam for lower-level callers
	// that predate native_acp configuration. Its implicit default behavior is
	// intentionally isolated from the production profile path: an omitted
	// source is gateway for configured native_acp profiles.
	legacyNativeAliases := make(map[loop.ModelAlias]struct{})
	for _, source := range input.NativeAuth {
		if err := validateACPNativeSource(source); err != nil {
			return ACPCompiledCatalog{}, err
		}
		if _, exists := gatewayRows[source.Alias]; exists {
			return ACPCompiledCatalog{}, fmt.Errorf("coderig: ACP credential alias collision")
		}
		if _, exists := nativeAliases[source.Alias]; exists {
			return ACPCompiledCatalog{}, fmt.Errorf("coderig: duplicate ACP native alias")
		}
		nativeByHarness[source.Harness] = append(nativeByHarness[source.Harness], runtimeOptionFromNative(source))
		nativeAliases[source.Alias] = struct{}{}
		legacyNativeAliases[source.Alias] = struct{}{}
	}

	hasEnabledNativeProfile := false
	for _, profile := range nativeProfiles {
		if profile.Enabled {
			hasEnabledNativeProfile = true
			break
		}
	}
	if len(rows) == 0 && len(nativeByHarness) == 0 && !hasEnabledNativeProfile {
		if len(input.Defaults) != 0 {
			return ACPCompiledCatalog{}, fmt.Errorf("coderig: ACP defaults configured without targets")
		}
		return ACPCompiledCatalog{RuntimeCatalog: mustEmptyRuntimeCatalog()}, nil
	}
	if len(input.Defaults) != len(roles) {
		return ACPCompiledCatalog{}, fmt.Errorf("coderig: ACP defaults must match configured roles")
	}
	if input.ClaudeSmall != "" && !hasRuntimeAlias(rows, input.ClaudeSmall) {
		return ACPCompiledCatalog{}, fmt.Errorf("coderig: ACP Claude small model unavailable")
	}

	entries := make([]loop.RuntimeCatalogEntry, 0, len(roles)*4)
	for _, role := range roles {
		configuredDefault, ok := input.Defaults[role]
		if !ok || (configuredDefault.Harness != "claude-code" && configuredDefault.Harness != "codex") {
			return ACPCompiledCatalog{}, fmt.Errorf("coderig: invalid ACP configured default")
		}
		defaultSource := configuredDefault.Source
		if defaultSource == "" {
			defaultSource = loop.RuntimeSourceGateway
			if len(rows) == 0 {
				if _, legacyNative := legacyNativeAliases[configuredDefault.Model]; legacyNative {
					defaultSource = loop.RuntimeSourceNative
				}
			}
		}
		if defaultSource != loop.RuntimeSourceGateway && defaultSource != loop.RuntimeSourceNative {
			return ACPCompiledCatalog{}, fmt.Errorf("coderig: invalid ACP configured default source")
		}
		if defaultSource == loop.RuntimeSourceNative && configuredDefault.Effort != model.EffortNone {
			return ACPCompiledCatalog{}, fmt.Errorf("coderig: native ACP defaults do not support effort overrides")
		}
		defaultFound := false
		for _, harness := range []loop.AgentHarnessName{"claude-code", "codex"} {
			if len(rows) > 0 && (harness != "claude-code" || input.ClaudeSmall != "") {
				models := cloneRuntimeOptions(rows)
				defaultModel := firstRuntimeAlias(models)
				if hasRuntimeAlias(models, configuredDefault.Model) {
					defaultModel = configuredDefault.Model
				}
				isDefault := harness == configuredDefault.Harness && defaultSource == loop.RuntimeSourceGateway
				if isDefault {
					if !hasRuntimeAlias(models, configuredDefault.Model) || !setACPDefaultEffort(models, configuredDefault.Model, configuredDefault.Effort) {
						return ACPCompiledCatalog{}, fmt.Errorf("coderig: invalid ACP configured gateway default")
					}
					defaultModel = configuredDefault.Model
				}
				sort.Slice(models, func(i, j int) bool { return models[i].Alias < models[j].Alias })
				entry := loop.RuntimeCatalogEntry{
					SubagentType: role, AgentHarness: harness, Profile: loop.RuntimeProfileName("acp/" + string(harness)),
					Source: loop.RuntimeSourceGateway, Credential: loop.CredentialGatewayBacked,
					SelectionKind: loop.RuntimeSelectionExplicit, Default: isDefault,
					DefaultModel: defaultModel, Models: models,
				}
				if harness == "claude-code" {
					entry.NeedsSmallModel = true
					entry.SmallModel = input.ClaudeSmall
					if !hasRuntimeAlias(models, entry.SmallModel) {
						return ACPCompiledCatalog{}, fmt.Errorf("coderig: ACP Claude small model unavailable")
					}
				}
				defaultFound = defaultFound || isDefault
				entries = append(entries, entry)
			}

			options, enabled := nativeByHarness[harness]
			profile, hasProfile := nativeProfileFor(nativeProfiles, harness)
			if !enabled && !hasProfile {
				continue
			}
			isDefault := harness == configuredDefault.Harness && defaultSource == loop.RuntimeSourceNative
			if hasProfile && profile.Models == nil {
				if isDefault && configuredDefault.Model != "" {
					return ACPCompiledCatalog{}, fmt.Errorf("coderig: invalid ACP configured native default")
				}
				entry := loop.RuntimeCatalogEntry{
					SubagentType: role, AgentHarness: harness, Profile: loop.RuntimeProfileName("acp/" + string(harness)),
					Source: loop.RuntimeSourceNative, Credential: loop.CredentialNativeAuth,
					SelectionKind: loop.RuntimeSelectionHarnessManaged, Default: isDefault,
				}
				defaultFound = defaultFound || isDefault
				entries = append(entries, entry)
				continue
			}
			if len(options) == 0 {
				continue
			}
			options = cloneRuntimeOptions(options)
			sort.Slice(options, func(i, j int) bool { return options[i].Alias < options[j].Alias })
			defaultModel := options[0].Alias
			if isDefault {
				if configuredDefault.Model == "" || !hasRuntimeAlias(options, configuredDefault.Model) {
					return ACPCompiledCatalog{}, fmt.Errorf("coderig: invalid ACP configured native default")
				}
				defaultModel = configuredDefault.Model
			}
			entries = append(entries, loop.RuntimeCatalogEntry{
				SubagentType: role, AgentHarness: harness, Profile: loop.RuntimeProfileName("acp/" + string(harness)),
				Source: loop.RuntimeSourceNative, Credential: loop.CredentialNativeAuth,
				SelectionKind: loop.RuntimeSelectionExplicit, Default: isDefault,
				DefaultModel: defaultModel, Models: options,
			})
			defaultFound = defaultFound || isDefault
		}
		if !defaultFound {
			return ACPCompiledCatalog{}, fmt.Errorf("coderig: ACP configured default source unavailable")
		}
	}
	if len(entries) == 0 {
		return ACPCompiledCatalog{RuntimeCatalog: mustEmptyRuntimeCatalog()}, nil
	}

	catalog, err := loop.NewRuntimeCatalog(entries)
	if err != nil {
		return ACPCompiledCatalog{}, err
	}
	profiles := make(map[loop.RuntimeProfileName]struct{}, len(entries))
	for _, entry := range entries {
		profiles[entry.Profile] = struct{}{}
	}
	return ACPCompiledCatalog{RuntimeCatalog: catalog, gatewayTargets: gatewayRows, profiles: profiles, entries: cloneACPEntries(entries)}, nil
}

func compileACPNativeProfiles(profiles []ACPNativeProfile) ([]ACPNativeProfile, map[loop.AgentHarnessName][]loop.RuntimeModelOption, error) {
	if len(profiles) == 0 {
		return nil, make(map[loop.AgentHarnessName][]loop.RuntimeModelOption), nil
	}
	seenProfiles := make(map[loop.AgentHarnessName]struct{}, len(profiles))
	normalized := make([]ACPNativeProfile, 0, len(profiles))
	optionsByHarness := make(map[loop.AgentHarnessName][]loop.RuntimeModelOption)
	for _, profile := range profiles {
		if profile.Harness != "claude-code" && profile.Harness != "codex" {
			return nil, nil, fmt.Errorf("coderig: invalid ACP native profile")
		}
		if _, exists := seenProfiles[profile.Harness]; exists {
			return nil, nil, fmt.Errorf("coderig: duplicate ACP native profile")
		}
		seenProfiles[profile.Harness] = struct{}{}
		if profile.Models != nil && len(profile.Models) == 0 {
			return nil, nil, fmt.Errorf("coderig: native ACP profile models must be omitted or non-empty")
		}
		models := append([]loop.ModelAlias(nil), profile.Models...)
		seenAliases := make(map[loop.ModelAlias]struct{}, len(models))
		for _, alias := range models {
			if !validModelConfigAlias(string(alias)) {
				return nil, nil, fmt.Errorf("coderig: invalid ACP native model alias")
			}
			if _, exists := seenAliases[alias]; exists {
				return nil, nil, fmt.Errorf("coderig: duplicate ACP native model alias")
			}
			seenAliases[alias] = struct{}{}
		}
		sort.Slice(models, func(i, j int) bool { return models[i] < models[j] })
		normalizedProfile := ACPNativeProfile{Harness: profile.Harness, Enabled: profile.Enabled, Models: models}
		normalized = append(normalized, normalizedProfile)
		if !profile.Enabled || models == nil {
			continue
		}
		for _, alias := range models {
			optionsByHarness[profile.Harness] = append(optionsByHarness[profile.Harness], runtimeOptionFromNativeAlias(profile.Harness, alias))
		}
	}
	sort.Slice(normalized, func(i, j int) bool { return normalized[i].Harness < normalized[j].Harness })
	return normalized, optionsByHarness, nil
}

func runtimeOptionFromNativeAlias(harness loop.AgentHarnessName, alias loop.ModelAlias) loop.RuntimeModelOption {
	return loop.RuntimeModelOption{
		Alias: alias, Source: loop.RuntimeSourceNative, Credential: loop.CredentialNativeAuth,
		Target:        model.CustomModel("native-acp", model.APIFormat("native-acp"), "", "native-acp:"+string(harness)+":"+string(alias), model.WithTools()),
		DefaultEffort: model.EffortNone,
		Efforts:       []model.Effort{model.EffortNone},
	}
}

func nativeProfileFor(profiles []ACPNativeProfile, harness loop.AgentHarnessName) (ACPNativeProfile, bool) {
	for _, profile := range profiles {
		if profile.Harness == harness && profile.Enabled {
			return profile, true
		}
	}
	return ACPNativeProfile{}, false
}

func firstRuntimeAlias(options []loop.RuntimeModelOption) loop.ModelAlias {
	if len(options) == 0 {
		return ""
	}
	return options[0].Alias
}

func mustEmptyRuntimeCatalog() loop.RuntimeCatalog {
	catalog, _ := loop.NewRuntimeCatalog(nil)
	return catalog
}

func normalizedACPSubagentTypes(input []identity.AgentName) []identity.AgentName {
	if len(input) == 0 {
		return []identity.AgentName{"planner", "builder", "reviewer"}
	}
	seen := make(map[identity.AgentName]struct{}, len(input))
	roles := make([]identity.AgentName, 0, len(input))
	for _, role := range input {
		if role == "" {
			continue
		}
		if _, ok := seen[role]; ok {
			continue
		}
		seen[role] = struct{}{}
		roles = append(roles, role)
	}
	sort.Slice(roles, func(i, j int) bool { return roles[i] < roles[j] })
	return roles
}

func compileACPGatewayRows(targets []ACPGatewaySource) ([]loop.RuntimeModelOption, map[loop.ModelAlias]ACPGatewaySource, error) {
	rows := make([]loop.RuntimeModelOption, 0, len(targets))
	sources := make(map[loop.ModelAlias]ACPGatewaySource)
	for _, source := range targets {
		var err error
		rows, err = addGatewaySource(rows, sources, source)
		if err != nil {
			return nil, nil, err
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Alias < rows[j].Alias })
	return rows, sources, nil
}

func addGatewaySource(rows []loop.RuntimeModelOption, sources map[loop.ModelAlias]ACPGatewaySource, source ACPGatewaySource) ([]loop.RuntimeModelOption, error) {
	if source.Alias == "" || source.Client == nil {
		return rows, fmt.Errorf("coderig: invalid ACP gateway source")
	}
	if _, exists := sources[source.Alias]; exists {
		return rows, fmt.Errorf("coderig: duplicate ACP gateway alias")
	}
	if err := source.Model.Validate(); err != nil {
		return rows, fmt.Errorf("coderig: invalid ACP gateway model: %w", err)
	}
	if !validEffortSet(source.Efforts, source.DefaultEffort) {
		return rows, fmt.Errorf("coderig: invalid ACP gateway effort set")
	}
	modelCopy := source.Model.Clone()
	modelCopy.Sampling.Effort = source.DefaultEffort
	source.Model = modelCopy
	source.Efforts = append([]model.Effort(nil), source.Efforts...)
	sources[source.Alias] = source
	rows = append(rows, loop.RuntimeModelOption{Alias: source.Alias, Target: modelCopy, DefaultEffort: source.DefaultEffort, Efforts: source.Efforts})
	return rows, nil
}

func hasRuntimeAlias(models []loop.RuntimeModelOption, alias loop.ModelAlias) bool {
	for _, option := range models {
		if option.Alias == alias {
			return true
		}
	}
	return false
}

func cloneRuntimeOptions(input []loop.RuntimeModelOption) []loop.RuntimeModelOption {
	result := make([]loop.RuntimeModelOption, len(input))
	for index, option := range input {
		result[index] = option
		result[index].Target = option.Target.Clone()
		result[index].Efforts = append([]model.Effort(nil), option.Efforts...)
	}
	return result
}

func setACPDefaultEffort(options []loop.RuntimeModelOption, alias loop.ModelAlias, effort model.Effort) bool {
	for index := range options {
		if options[index].Alias != alias || !containsACPEffort(options[index].Efforts, effort) {
			continue
		}
		options[index].DefaultEffort = effort
		return true
	}
	return false
}

func runtimeOptionFromNative(source ACPNativeAuthSource) loop.RuntimeModelOption {
	return loop.RuntimeModelOption{
		Alias:            source.Alias,
		Source:           loop.RuntimeSourceNative,
		Credential:       loop.CredentialNativeAuth,
		NativeSmallModel: source.SmallModel,
		Target:           source.Model.Clone(),
		DefaultEffort:    source.DefaultEffort,
		Efforts:          append([]model.Effort(nil), source.Efforts...),
	}
}

func validateACPNativeSource(source ACPNativeAuthSource) error {
	if source.Harness == "" || source.Alias == "" || source.Model.Name == "" || !validEffortSet(source.Efforts, source.DefaultEffort) {
		return fmt.Errorf("coderig: invalid ACP native-auth source")
	}
	if source.SmallModel == "" && source.Harness == "claude-code" {
		return fmt.Errorf("coderig: invalid ACP native-auth source")
	}
	if err := source.Model.Validate(); err != nil {
		return fmt.Errorf("coderig: invalid ACP native-auth model: %w", err)
	}
	return nil
}

func validEffortSet(efforts []model.Effort, defaultEffort model.Effort) bool {
	if !defaultEffort.Valid() || len(efforts) == 0 {
		return false
	}
	seen := make(map[model.Effort]struct{}, len(efforts))
	for _, effort := range efforts {
		if !effort.Valid() {
			return false
		}
		if _, ok := seen[effort]; ok {
			return false
		}
		seen[effort] = struct{}{}
	}
	_, ok := seen[defaultEffort]
	return ok
}

// GatewayTarget resolves a catalog selection into the exact fixed target for
// one child. The selected effort is copied into the target model and made
// authoritative so ingress-provided sampling cannot override it.
func (c ACPCompiledCatalog) GatewayTarget(resolved loop.Resolved) (gateway.Target, error) {
	if resolved.Credential != loop.CredentialGatewayBacked {
		return gateway.Target{}, fmt.Errorf("coderig: native-auth runtime has no gateway target")
	}
	source, ok := c.gatewayTargets[resolved.ModelAlias]
	if !ok {
		return gateway.Target{}, fmt.Errorf("coderig: ACP gateway target unavailable")
	}
	if !containsACPEffort(source.Efforts, resolved.Effort) {
		return gateway.Target{}, fmt.Errorf("coderig: ACP gateway effort unavailable")
	}
	m := source.Model.Clone()
	m.Sampling.Effort = resolved.Effort
	targetAlias := resolved.TargetAlias
	if targetAlias == "" {
		targetAlias = resolved.ModelAlias
	}
	return gateway.Target{ID: string(targetAlias), Client: source.Client, Model: m, AuthoritativeEffort: true}, nil
}

// ResolveGatewayTarget is a convenience seam for gateway construction tests
// and callers that have not yet selected a role/harness entry.
func (c ACPCompiledCatalog) ResolveGatewayTarget(alias loop.ModelAlias, effort model.Effort) (gateway.Target, error) {
	source, ok := c.gatewayTargets[alias]
	if !ok {
		return gateway.Target{}, fmt.Errorf("coderig: ACP gateway target unavailable")
	}
	if !containsACPEffort(source.Efforts, effort) {
		return gateway.Target{}, fmt.Errorf("coderig: ACP gateway effort unavailable")
	}
	m := source.Model.Clone()
	m.Sampling.Effort = effort
	return gateway.Target{ID: string(alias), Client: source.Client, Model: m, AuthoritativeEffort: true}, nil
}

// HasProfile reports whether the compiled catalog contains an executable
// profile. It lets the composition root omit ACP builders when only native
// authentication rows are available.
func (c ACPCompiledCatalog) HasProfile(profile loop.RuntimeProfileName) bool {
	_, ok := c.profiles[profile]
	return ok
}

// filterProfiles removes entries whose connector did not pass composition
// preflight. The returned catalog is rebuilt so Harness cannot advertise a
// profile whose BuilderRegistry entry was omitted. Defaults are repaired after
// filtering because the original default harness may be the unavailable one.
//
//lint:ignore U1000 retained as a package-local catalog filtering seam for legacy fixtures.
func (c ACPCompiledCatalog) filterProfiles(allowed map[loop.RuntimeProfileName]struct{}) (ACPCompiledCatalog, error) {
	entries := make([]loop.RuntimeCatalogEntry, 0, len(c.entries))
	for _, entry := range c.entries {
		if _, ok := allowed[entry.Profile]; !ok {
			continue
		}
		entries = append(entries, cloneACPEntry(entry))
	}
	defaultByRole := make(map[identity.AgentName]bool)
	for _, entry := range entries {
		if entry.Default {
			defaultByRole[entry.SubagentType] = true
		}
	}
	for i := range entries {
		if !defaultByRole[entries[i].SubagentType] {
			entries[i].Default = true
			defaultByRole[entries[i].SubagentType] = true
		}
	}
	catalog, err := loop.NewRuntimeCatalog(entries)
	if err != nil {
		return ACPCompiledCatalog{}, err
	}
	profiles := make(map[loop.RuntimeProfileName]struct{}, len(entries))
	for _, entry := range entries {
		profiles[entry.Profile] = struct{}{}
	}
	return ACPCompiledCatalog{
		RuntimeCatalog: catalog,
		gatewayTargets: c.gatewayTargets,
		profiles:       profiles,
		entries:        cloneACPEntries(entries),
	}, nil
}

func cloneACPEntries(entries []loop.RuntimeCatalogEntry) []loop.RuntimeCatalogEntry {
	result := make([]loop.RuntimeCatalogEntry, len(entries))
	for i, entry := range entries {
		result[i] = cloneACPEntry(entry)
	}
	return result
}

func cloneACPEntry(entry loop.RuntimeCatalogEntry) loop.RuntimeCatalogEntry {
	entry.Models = append([]loop.RuntimeModelOption(nil), entry.Models...)
	for i := range entry.Models {
		entry.Models[i].Target = entry.Models[i].Target.Clone()
		entry.Models[i].Efforts = append([]model.Effort(nil), entry.Models[i].Efforts...)
	}
	return entry
}

func containsACPEffort(efforts []model.Effort, wanted model.Effort) bool {
	for _, effort := range efforts {
		if effort == wanted {
			return true
		}
	}
	return false
}
