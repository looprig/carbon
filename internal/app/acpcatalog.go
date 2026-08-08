package app

import (
	"fmt"
	"sort"

	"github.com/looprig/coderig/internal/catalog/generic"
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
	Description   string
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

// ACPNativeModelOption is one static native ACP model choice. Alias is the
// model-facing runtime selector and Model is the adapter-facing model ID. The
// normalized models.json path currently uses the same value for both, while
// keeping the distinction here makes the runtime boundary explicit.
type ACPNativeModelOption struct {
	Alias         loop.ModelAlias
	Model         string
	Efforts       []model.Effort
	DefaultEffort model.Effort
}

// ACPNativeProfile is one normalized native ACP profile. A nil Models slice
// means harness-managed selection; a non-empty slice constrains explicit
// native model choices. Models remains the legacy alias projection for
// lower-level composition callers; ModelOptions is authoritative whenever it
// is present. Disabled profiles are retained here for digest and composition
// identity but never compile into runtime rows.
type ACPNativeProfile struct {
	Harness      loop.AgentHarnessName
	Enabled      bool
	Models       []loop.ModelAlias
	ModelOptions []ACPNativeModelOption
}

// acpCatalogInput contains already-validated, credential-bound ACP inputs.
// It is deliberately raw-entry-only: product defaults and RuntimeCatalog
// validation belong to CompileAgentRuntimeCatalog.
type acpCatalogInput struct {
	GatewayTargets []ACPGatewaySource
	ClaudeSmall    loop.ModelAlias
	NativeACP      map[string]ACPNativeProfile
	NativeProfiles []ACPNativeProfile
	NativeAuth     []ACPNativeAuthSource
}

type acpRuntimeEntries struct {
	entries        []loop.RuntimeCatalogEntry
	gatewayOptions []loop.RuntimeModelOption
	gatewayTargets map[loop.ModelAlias]ACPGatewaySource
}

// ACPCompiledCatalog is the single compiled source of truth consumed by both
// Harness runtime selection and the per-child gateway constructor.
type ACPCompiledCatalog struct {
	RuntimeCatalog loop.RuntimeCatalog

	gatewayTargets map[loop.ModelAlias]ACPGatewaySource
	profiles       map[loop.RuntimeProfileName]struct{}
	entries        []loop.RuntimeCatalogEntry
}

// compileACPRuntimeEntries compiles configured gateway-backed targets and
// optional native ACP profiles into raw Generic rows. It does not assign a
// product default or construct an intermediate RuntimeCatalog.
func compileACPRuntimeEntries(input acpCatalogInput) (acpRuntimeEntries, error) {
	rows, gatewayRows, err := compileACPGatewayRows(input.GatewayTargets)
	if err != nil {
		return acpRuntimeEntries{}, err
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
		return acpRuntimeEntries{}, err
	}
	nativeAliases := make(map[loop.ModelAlias]struct{})
	for _, options := range nativeByHarness {
		for _, option := range options {
			if _, collision := gatewayRows[option.Alias]; collision {
				return acpRuntimeEntries{}, fmt.Errorf("coderig: ACP credential alias collision")
			}
			nativeAliases[option.Alias] = struct{}{}
		}
	}
	// NativeAuth remains an explicit lower-level source for ACP composition
	// seams; production discovery uses NativeACP profiles instead.
	for _, source := range input.NativeAuth {
		if err := validateACPNativeSource(source); err != nil {
			return acpRuntimeEntries{}, err
		}
		if _, exists := gatewayRows[source.Alias]; exists {
			return acpRuntimeEntries{}, fmt.Errorf("coderig: ACP credential alias collision")
		}
		if _, exists := nativeAliases[source.Alias]; exists {
			return acpRuntimeEntries{}, fmt.Errorf("coderig: duplicate ACP native alias")
		}
		nativeByHarness[source.Harness] = append(nativeByHarness[source.Harness], runtimeOptionFromNative(source))
		nativeAliases[source.Alias] = struct{}{}
	}

	hasEnabledNativeProfile := false
	for _, profile := range nativeProfiles {
		if profile.Enabled {
			hasEnabledNativeProfile = true
			break
		}
	}
	if len(rows) == 0 && len(nativeByHarness) == 0 && !hasEnabledNativeProfile {
		return acpRuntimeEntries{gatewayTargets: gatewayRows}, nil
	}
	if input.ClaudeSmall != "" && !hasRuntimeAlias(rows, input.ClaudeSmall) {
		return acpRuntimeEntries{}, fmt.Errorf("coderig: ACP Claude small model unavailable")
	}

	entries := make([]loop.RuntimeCatalogEntry, 0, 4)
	for _, harness := range []loop.AgentHarnessName{"claude-code", "codex"} {
		var gatewayEntry *loop.RuntimeCatalogEntry
		if len(rows) > 0 && (harness != "claude-code" || input.ClaudeSmall != "") {
			models := cloneRuntimeOptions(rows)
			defaultModel := firstRuntimeAlias(models)
			sort.Slice(models, func(i, j int) bool { return models[i].Alias < models[j].Alias })
			entry := loop.RuntimeCatalogEntry{
				AgentType: generic.Name, AgentHarness: harness, Profile: loop.RuntimeProfileName("acp/" + string(harness)),
				Description: runtimeHarnessDescription(harness),
				Source:      loop.RuntimeSourceGateway, Credential: loop.CredentialGatewayBacked,
				SelectionKind: loop.RuntimeSelectionExplicit, Default: false,
				DefaultModel: defaultModel, Models: models,
			}
			if harness == "claude-code" {
				entry.NeedsSmallModel = true
				entry.SmallModel = input.ClaudeSmall
				if !hasRuntimeAlias(models, entry.SmallModel) {
					return acpRuntimeEntries{}, fmt.Errorf("coderig: ACP Claude small model unavailable")
				}
			}
			gatewayEntry = &entry
		}

		options, enabled := nativeByHarness[harness]
		profile, hasProfile := nativeProfileFor(nativeProfiles, harness)
		if hasProfile && profile.Models == nil {
			entry := loop.RuntimeCatalogEntry{
				AgentType: generic.Name, AgentHarness: harness, Profile: loop.RuntimeProfileName("acp/" + string(harness)),
				Description: runtimeHarnessDescription(harness),
				Source:      loop.RuntimeSourceNative, Credential: loop.CredentialNativeAuth,
				SelectionKind: loop.RuntimeSelectionHarnessManaged, Default: false,
			}
			if gatewayEntry != nil {
				entries = append(entries, *gatewayEntry)
			}
			entries = append(entries, entry)
			continue
		}
		if enabled && len(options) > 0 {
			options = cloneRuntimeOptions(options)
			sort.Slice(options, func(i, j int) bool { return options[i].Alias < options[j].Alias })
			if gatewayEntry != nil {
				// Keep one Generic row per explicit ACP harness while allowing
				// each model to retain its source and credential route.
				gatewayEntry.Models = append(gatewayEntry.Models, options...)
				sort.Slice(gatewayEntry.Models, func(i, j int) bool { return gatewayEntry.Models[i].Alias < gatewayEntry.Models[j].Alias })
				entries = append(entries, *gatewayEntry)
				continue
			}
			defaultModel := options[0].Alias
			entries = append(entries, loop.RuntimeCatalogEntry{
				AgentType: generic.Name, AgentHarness: harness, Profile: loop.RuntimeProfileName("acp/" + string(harness)),
				Description: runtimeHarnessDescription(harness),
				Source:      loop.RuntimeSourceNative, Credential: loop.CredentialNativeAuth,
				SelectionKind: loop.RuntimeSelectionExplicit, Default: false,
				DefaultModel: defaultModel, Models: options,
			})
			continue
		}
		if gatewayEntry != nil {
			entries = append(entries, *gatewayEntry)
		}
	}
	return acpRuntimeEntries{entries: entries, gatewayOptions: rows, gatewayTargets: gatewayRows}, nil
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
		if (profile.Models != nil && len(profile.Models) == 0) || (profile.ModelOptions != nil && len(profile.ModelOptions) == 0) {
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
		choices, err := nativeModelOptions(profile)
		if err != nil {
			return nil, nil, err
		}
		models = models[:0]
		for _, choice := range choices {
			models = append(models, choice.Alias)
		}
		sort.Slice(models, func(i, j int) bool { return models[i] < models[j] })
		normalizedProfile := ACPNativeProfile{
			Harness:      profile.Harness,
			Enabled:      profile.Enabled,
			Models:       models,
			ModelOptions: cloneACPNativeModelOptions(choices),
		}
		normalized = append(normalized, normalizedProfile)
		if !profile.Enabled || choices == nil {
			continue
		}
		for _, choice := range choices {
			optionsByHarness[profile.Harness] = append(optionsByHarness[profile.Harness], runtimeOptionFromNativeChoice(profile.Harness, choice))
		}
	}
	sort.Slice(normalized, func(i, j int) bool { return normalized[i].Harness < normalized[j].Harness })
	return normalized, optionsByHarness, nil
}

func runtimeOptionFromNativeChoice(harness loop.AgentHarnessName, choice ACPNativeModelOption) loop.RuntimeModelOption {
	modelID := choice.Model
	if modelID == "" {
		modelID = string(choice.Alias)
	}
	target := model.CustomModel(
		"native-acp", model.APIFormat("native-acp"), "",
		"native-acp:"+string(harness)+":"+modelID, model.WithTools(),
	)
	target.Sampling.Effort = choice.DefaultEffort
	return loop.RuntimeModelOption{
		Alias:         choice.Alias,
		Description:   runtimeHarnessDescription(harness),
		Source:        loop.RuntimeSourceNative,
		Credential:    loop.CredentialNativeAuth,
		Target:        target,
		DefaultEffort: choice.DefaultEffort,
		Efforts:       append([]model.Effort(nil), choice.Efforts...),
	}
}

func nativeModelOptions(profile ACPNativeProfile) ([]ACPNativeModelOption, error) {
	if profile.ModelOptions != nil {
		choices := cloneACPNativeModelOptions(profile.ModelOptions)
		choices, err := validateACPNativeModelOptions(choices)
		if err != nil {
			return nil, err
		}
		if profile.Models != nil {
			projected := append([]loop.ModelAlias(nil), profile.Models...)
			sort.Slice(projected, func(i, j int) bool { return projected[i] < projected[j] })
			if len(projected) != len(choices) {
				return nil, fmt.Errorf("coderig: native ACP model projections disagree")
			}
			for i, choice := range choices {
				if projected[i] != choice.Alias {
					return nil, fmt.Errorf("coderig: native ACP model projections disagree")
				}
			}
		}
		return choices, nil
	}
	if profile.Models == nil {
		return nil, nil
	}
	choices := make([]ACPNativeModelOption, 0, len(profile.Models))
	for _, alias := range profile.Models {
		choices = append(choices, ACPNativeModelOption{
			Alias:         alias,
			Model:         string(alias),
			DefaultEffort: model.EffortNone,
			Efforts:       []model.Effort{model.EffortNone},
		})
	}
	return validateACPNativeModelOptions(choices)
}

func validateACPNativeModelOptions(options []ACPNativeModelOption) ([]ACPNativeModelOption, error) {
	seen := make(map[loop.ModelAlias]struct{}, len(options))
	for i := range options {
		option := &options[i]
		if option.Alias == "" && option.Model != "" {
			option.Alias = loop.ModelAlias(option.Model)
		}
		if option.Model == "" {
			option.Model = string(option.Alias)
		}
		if !validModelConfigAlias(string(option.Alias)) || !validModelConfigAlias(option.Model) {
			return nil, fmt.Errorf("coderig: invalid ACP native model alias")
		}
		if _, duplicate := seen[option.Alias]; duplicate {
			return nil, fmt.Errorf("coderig: duplicate ACP native model alias")
		}
		seen[option.Alias] = struct{}{}
		if !validEffortSet(option.Efforts, option.DefaultEffort) {
			return nil, fmt.Errorf("coderig: invalid ACP native effort set")
		}
		option.Efforts = append([]model.Effort(nil), option.Efforts...)
	}
	sort.Slice(options, func(i, j int) bool { return options[i].Alias < options[j].Alias })
	return options, nil
}

func cloneACPNativeModelOptions(input []ACPNativeModelOption) []ACPNativeModelOption {
	if input == nil {
		return nil
	}
	result := make([]ACPNativeModelOption, len(input))
	for i, option := range input {
		result[i] = option
		result[i].Efforts = append([]model.Effort(nil), option.Efforts...)
	}
	return result
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
	rows = append(rows, loop.RuntimeModelOption{
		Alias: source.Alias, Description: source.Description, Target: modelCopy,
		DefaultEffort: source.DefaultEffort, Efforts: source.Efforts,
	})
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

func runtimeOptionFromNative(source ACPNativeAuthSource) loop.RuntimeModelOption {
	return loop.RuntimeModelOption{
		Alias:            source.Alias,
		Description:      runtimeHarnessDescription(source.Harness),
		Source:           loop.RuntimeSourceNative,
		Credential:       loop.CredentialNativeAuth,
		NativeSmallModel: source.SmallModel,
		Target:           source.Model.Clone(),
		DefaultEffort:    source.DefaultEffort,
		Efforts:          append([]model.Effort(nil), source.Efforts...),
	}
}

func validateACPNativeSource(source ACPNativeAuthSource) error {
	if (source.Harness != "claude-code" && source.Harness != "codex") || source.Alias == "" || source.Model.Name == "" || !validEffortSet(source.Efforts, source.DefaultEffort) {
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
// and callers that have not yet selected an agent/harness entry.
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
