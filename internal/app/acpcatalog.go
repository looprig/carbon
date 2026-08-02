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
	Harness       loop.AgentHarnessName
	Alias         loop.ModelAlias
	Model         model.Model
	DefaultEffort model.Effort
	Efforts       []model.Effort
}

// ACPCatalogInput contains the capability and credential inputs available at
// composition time. A nil gateway client means that provider is not configured
// and its built-in rows are omitted.
type ACPCatalogInput struct {
	SubagentTypes       []identity.AgentName
	GatewayClients      map[model.ProviderName]inference.Client
	ExtraGatewayTargets []ACPGatewaySource
	NativeAuth          []ACPNativeAuthSource
}

// ACPCompiledCatalog is the single compiled source of truth consumed by both
// Harness runtime selection and the per-child gateway constructor.
type ACPCompiledCatalog struct {
	RuntimeCatalog loop.RuntimeCatalog

	gatewayTargets map[loop.ModelAlias]ACPGatewaySource
	profiles       map[loop.RuntimeProfileName]struct{}
}

// CompileACPCatalog compiles the frozen gateway-backed ACP table together with
// configured native-auth rows. Gateway-backed aliases are admitted only when a
// non-nil provider client is present. When no roles are supplied, the product's
// three roles are used.
func CompileACPCatalog(input ACPCatalogInput) (ACPCompiledCatalog, error) {
	roles := normalizedACPSubagentTypes(input.SubagentTypes)
	rows, gatewayRows, err := compileACPGatewayRows(input)
	if err != nil {
		return ACPCompiledCatalog{}, err
	}

	nativeByHarness := make(map[loop.AgentHarnessName][]loop.RuntimeModelOption)
	nativeSourcesByHarness := make(map[loop.AgentHarnessName][]ACPNativeAuthSource)
	for _, source := range input.NativeAuth {
		if err := validateACPNativeSource(source); err != nil {
			return ACPCompiledCatalog{}, err
		}
		if _, exists := gatewayRows[source.Alias]; exists {
			// A harness entry has one credential mode in Harness's catalog
			// contract. A configured gateway row takes precedence for that
			// harness, so a native row with the same alias is not advertised.
			continue
		}
		nativeByHarness[source.Harness] = append(nativeByHarness[source.Harness], runtimeOptionFromNative(source))
		nativeSourcesByHarness[source.Harness] = append(nativeSourcesByHarness[source.Harness], source)
	}

	entries := make([]loop.RuntimeCatalogEntry, 0, len(roles)*2)
	for _, role := range roles {
		for _, harness := range []loop.AgentHarnessName{"claude-code", "codex"} {
			// Gateway-backed rows are cross-dialect and take precedence over
			// native rows when this harness can execute the fixed profile.
			if entry, ok := gatewayCatalogEntry(role, harness, rows); ok {
				entries = append(entries, entry)
				continue
			}
			native := nativeByHarness[harness]
			if len(native) == 0 {
				continue
			}
			entries = append(entries, nativeCatalogEntry(role, harness, native, nativeSourcesByHarness[harness]))
		}
	}
	if len(entries) == 0 {
		return ACPCompiledCatalog{RuntimeCatalog: mustEmptyRuntimeCatalog()}, nil
	}

	// RuntimeCatalog requires one deterministic default harness per role. Prefer
	// Claude when present, otherwise make the first available harness default.
	markACPDefaults(entries)
	catalog, err := loop.NewRuntimeCatalog(entries)
	if err != nil {
		return ACPCompiledCatalog{}, err
	}
	profiles := make(map[loop.RuntimeProfileName]struct{}, len(entries))
	for _, entry := range entries {
		profiles[entry.Profile] = struct{}{}
	}
	return ACPCompiledCatalog{RuntimeCatalog: catalog, gatewayTargets: gatewayRows, profiles: profiles}, nil
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

type acpGatewayDefinition struct {
	Alias         loop.ModelAlias
	Provider      model.ProviderName
	APIFormat     model.APIFormat
	DefaultEffort model.Effort
	Efforts       []model.Effort
	ModelName     string
}

var frozenACPGatewayDefinitions = []acpGatewayDefinition{
	{Alias: "fable-5", Provider: "anthropic", APIFormat: model.APIFormatAnthropic, DefaultEffort: model.EffortMedium, Efforts: []model.Effort{model.EffortLow, model.EffortMedium, model.EffortHigh, model.EffortMax}, ModelName: "claude-fable-5"},
	{Alias: "sonnet-5", Provider: "anthropic", APIFormat: model.APIFormatAnthropic, DefaultEffort: model.EffortMedium, Efforts: []model.Effort{model.EffortLow, model.EffortMedium, model.EffortHigh, model.EffortMax}, ModelName: "claude-sonnet-5"},
	{Alias: "opus-5", Provider: "anthropic", APIFormat: model.APIFormatAnthropic, DefaultEffort: model.EffortMedium, Efforts: []model.Effort{model.EffortLow, model.EffortMedium, model.EffortHigh, model.EffortMax}, ModelName: "claude-opus-5"},
	{Alias: "gpt-5.6-sol", Provider: "openai", APIFormat: model.APIFormatOpenAIResponses, DefaultEffort: model.EffortMedium, Efforts: []model.Effort{model.EffortNone, model.EffortLow, model.EffortMedium, model.EffortHigh, model.EffortMax}, ModelName: "gpt-5.6-sol"},
	{Alias: "gpt-5.6-terra", Provider: "openai", APIFormat: model.APIFormatOpenAIResponses, DefaultEffort: model.EffortMedium, Efforts: []model.Effort{model.EffortNone, model.EffortLow, model.EffortMedium, model.EffortHigh, model.EffortMax}, ModelName: "gpt-5.6-terra"},
	{Alias: "gpt-5.6-luna", Provider: "openai", APIFormat: model.APIFormatOpenAIResponses, DefaultEffort: model.EffortMedium, Efforts: []model.Effort{model.EffortNone, model.EffortLow, model.EffortMedium, model.EffortHigh, model.EffortMax}, ModelName: "gpt-5.6-luna"},
}

func compileACPGatewayRows(input ACPCatalogInput) ([]loop.RuntimeModelOption, map[loop.ModelAlias]ACPGatewaySource, error) {
	rows := make([]loop.RuntimeModelOption, 0, len(frozenACPGatewayDefinitions)+len(input.ExtraGatewayTargets))
	sources := make(map[loop.ModelAlias]ACPGatewaySource)
	for _, definition := range frozenACPGatewayDefinitions {
		client := input.GatewayClients[definition.Provider]
		if client == nil {
			continue
		}
		source := ACPGatewaySource{
			Alias:         definition.Alias,
			Client:        client,
			Model:         model.CustomModel(definition.Provider, definition.APIFormat, "", definition.ModelName, model.WithTools(), model.WithThinking()),
			DefaultEffort: definition.DefaultEffort,
			Efforts:       append([]model.Effort(nil), definition.Efforts...),
		}
		var err error
		rows, err = addGatewaySource(rows, sources, source)
		if err != nil {
			return nil, nil, err
		}
	}
	for _, source := range input.ExtraGatewayTargets {
		if source.Client == nil {
			continue
		}
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

func gatewayCatalogEntry(role identity.AgentName, harness loop.AgentHarnessName, rows []loop.RuntimeModelOption) (loop.RuntimeCatalogEntry, bool) {
	models := make([]loop.RuntimeModelOption, len(rows))
	copy(models, rows)
	if len(models) == 0 {
		return loop.RuntimeCatalogEntry{}, false
	}
	if harness == "claude-code" && !hasRuntimeAlias(models, "sonnet-5") {
		// Claude Code requires the fixed sonnet-5 small-model route. A
		// deployment without that target cannot advertise a Claude gateway
		// child, even when another provider target is configured.
		return loop.RuntimeCatalogEntry{}, false
	}
	defaultModel := models[0].Alias
	if hasRuntimeAlias(models, "sonnet-5") {
		defaultModel = "sonnet-5"
	}
	smallModel := loop.ModelAlias("")
	needsSmallModel := false
	if harness == "claude-code" {
		smallModel = "sonnet-5"
		needsSmallModel = true
	}
	return loop.RuntimeCatalogEntry{
		SubagentType: role, AgentHarness: harness, Profile: loop.RuntimeProfileName("acp/" + string(harness)),
		Credential: loop.CredentialGatewayBacked, DefaultModel: defaultModel,
		SmallModel: smallModel, NeedsSmallModel: needsSmallModel, Models: models,
	}, true
}

func hasRuntimeAlias(models []loop.RuntimeModelOption, alias loop.ModelAlias) bool {
	for _, option := range models {
		if option.Alias == alias {
			return true
		}
	}
	return false
}

func nativeCatalogEntry(role identity.AgentName, harness loop.AgentHarnessName, models []loop.RuntimeModelOption, sources []ACPNativeAuthSource) loop.RuntimeCatalogEntry {
	defaultModel := models[0].Alias
	for _, source := range sources {
		if source.DefaultEffort == model.EffortNone {
			defaultModel = source.Alias
			break
		}
	}
	return loop.RuntimeCatalogEntry{SubagentType: role, AgentHarness: harness, Profile: loop.RuntimeProfileName("native/" + string(harness)), Credential: loop.CredentialNativeAuth, DefaultModel: defaultModel, Models: append([]loop.RuntimeModelOption(nil), models...)}
}

func markACPDefaults(entries []loop.RuntimeCatalogEntry) {
	byRole := make(map[identity.AgentName]bool)
	for i := range entries {
		if entries[i].AgentHarness == "claude-code" || !byRole[entries[i].SubagentType] {
			entries[i].Default = true
			byRole[entries[i].SubagentType] = true
		}
	}
	for i := range entries {
		if entries[i].Default {
			for j := range entries {
				if i != j && entries[j].SubagentType == entries[i].SubagentType {
					entries[j].Default = false
				}
			}
		}
	}
}

func runtimeOptionFromNative(source ACPNativeAuthSource) loop.RuntimeModelOption {
	return loop.RuntimeModelOption{Alias: source.Alias, Target: source.Model.Clone(), DefaultEffort: source.DefaultEffort, Efforts: append([]model.Effort(nil), source.Efforts...)}
}

func validateACPNativeSource(source ACPNativeAuthSource) error {
	if source.Harness == "" || source.Alias == "" || source.Model.Name == "" || !validEffortSet(source.Efforts, source.DefaultEffort) {
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
	return gateway.Target{ID: string(resolved.ModelAlias), Client: source.Client, Model: m, AuthoritativeEffort: true}, nil
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

func containsACPEffort(efforts []model.Effort, wanted model.Effort) bool {
	for _, effort := range efforts {
		if effort == wanted {
			return true
		}
	}
	return false
}
