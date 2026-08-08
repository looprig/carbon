package app

import (
	"fmt"

	"github.com/looprig/coderig/internal/catalog/generic"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/inference"
	model "github.com/looprig/inference/model"
)

const (
	looprigRuntimeHarness     loop.AgentHarnessName   = "looprig"
	looprigRuntimeProfile     loop.RuntimeProfileName = "looprig/native"
	looprigRuntimeDescription                         = "In-process Harness loop using a configured gateway/client."
	codexRuntimeDescription                           = "Codex ACP harness when its profile is usable."
	claudeRuntimeDescription                          = "Claude Code ACP harness when its profile is usable."
)

// GenericRuntimeSource is the private client/model binding used by Generic's
// in-process fallback. It is separate from ACPGatewaySource so a primer-only
// model cannot accidentally become an ACP row.
type GenericRuntimeSource struct {
	Alias         loop.ModelAlias
	Description   string
	Client        inference.Client
	Model         model.Model
	DefaultEffort model.Effort
	Efforts       []model.Effort
}

// AgentRuntimeCatalogInput is the minimal composition-root input for the
// complete parent-scoped runtime catalogue. GatewayTargets are the configured
// delegate-capable targets admitted to both the in-process and ACP choices.
// PrimerTarget is used only by the in-process Generic choice when no delegate
// target exists; it is never exposed as an ACP model.
type AgentRuntimeCatalogInput struct {
	GatewayTargets []ACPGatewaySource
	PrimerTarget   GenericRuntimeSource
	ClaudeSmall    loop.ModelAlias
	NativeACP      map[string]ACPNativeProfile
}

// CompileAgentRuntimeCatalog is the one product-level catalogue compiler. It
// compiles raw optional ACP rows and the ordinary in-process Generic row, then
// validates the complete set exactly once. ACP rows are never given a product
// default: omitted runtime selectors always resolve to looprig/native.
func CompileAgentRuntimeCatalog(input AgentRuntimeCatalogInput) (ACPCompiledCatalog, error) {
	raw, err := compileACPRuntimeEntries(acpCatalogInput{
		GatewayTargets: input.GatewayTargets,
		ClaudeSmall:    input.ClaudeSmall,
		NativeACP:      input.NativeACP,
	})
	if err != nil {
		return ACPCompiledCatalog{}, err
	}

	ordinaryOptions, nativeTargets, err := compileGenericRuntimeOptions(raw.gatewayOptions, raw.gatewayTargets, input.PrimerTarget)
	if err != nil {
		return ACPCompiledCatalog{}, err
	}
	entries := make([]loop.RuntimeCatalogEntry, 0, len(raw.entries)+1)
	entries = append(entries, loop.RuntimeCatalogEntry{
		AgentType:     generic.Name,
		AgentHarness:  looprigRuntimeHarness,
		Profile:       looprigRuntimeProfile,
		Description:   looprigRuntimeDescription,
		Source:        loop.RuntimeSourceNative,
		Credential:    loop.CredentialNativeAuth,
		SelectionKind: loop.RuntimeSelectionExplicit,
		Default:       true,
		DefaultModel:  firstRuntimeAlias(ordinaryOptions),
		Models:        ordinaryOptions,
	})
	for _, entry := range raw.entries {
		entry.AgentType = generic.Name
		entry.Default = false
		entries = append(entries, cloneACPEntry(entry))
	}

	catalog, err := loop.NewRuntimeCatalog(entries)
	if err != nil {
		return ACPCompiledCatalog{}, err
	}
	profiles := make(map[loop.RuntimeProfileName]struct{}, len(entries))
	for _, entry := range entries {
		profiles[entry.Profile] = struct{}{}
	}
	for alias, source := range nativeTargets {
		raw.gatewayTargets[alias] = source
	}
	return ACPCompiledCatalog{
		RuntimeCatalog: catalog,
		gatewayTargets: raw.gatewayTargets,
		profiles:       profiles,
		entries:        cloneACPEntries(entries),
	}, nil
}

// compileGenericRuntimeOptions uses delegate-capable rows for ordinary
// in-process Generic selection. The primer is the explicit fallback when no
// delegate row exists; it is deliberately not passed to the ACP compiler.
func compileGenericRuntimeOptions(gatewayOptions []loop.RuntimeModelOption, gatewayTargets map[loop.ModelAlias]ACPGatewaySource, primer GenericRuntimeSource) ([]loop.RuntimeModelOption, map[loop.ModelAlias]ACPGatewaySource, error) {
	if len(gatewayOptions) != 0 {
		options := cloneRuntimeOptions(gatewayOptions)
		for index := range options {
			options[index].Source = loop.RuntimeSourceNative
			options[index].Credential = loop.CredentialNativeAuth
		}
		return options, cloneGatewayTargets(gatewayTargets), nil
	}
	if primer.Alias == "" {
		return nil, nil, fmt.Errorf("coderig: Generic runtime requires a configured primer")
	}
	options, targetsByAlias, err := compileACPGatewayRows([]ACPGatewaySource{{
		Alias:         primer.Alias,
		Description:   primer.Description,
		Client:        primer.Client,
		Model:         primer.Model,
		DefaultEffort: primer.DefaultEffort,
		Efforts:       append([]model.Effort(nil), primer.Efforts...),
	}})
	if err != nil {
		return nil, nil, err
	}
	for index := range options {
		options[index].Source = loop.RuntimeSourceNative
		options[index].Credential = loop.CredentialNativeAuth
	}
	return options, targetsByAlias, nil
}

func configuredPrimerRuntimeTarget(configured productionModels) GenericRuntimeSource {
	description := ""
	for _, candidate := range configured.PrimerCandidates {
		if candidate.Alias == configured.PrimerAlias {
			description = candidate.Description
			break
		}
	}
	defaultEffort := configured.PrimerModel.Sampling.Effort
	return GenericRuntimeSource{
		Alias:         loop.ModelAlias(configured.PrimerAlias),
		Description:   description,
		Client:        configured.PrimerClient,
		Model:         configured.PrimerModel,
		DefaultEffort: defaultEffort,
		Efforts:       append([]model.Effort(nil), configured.PrimerEfforts...),
	}
}

func cloneGatewayTargets(input map[loop.ModelAlias]ACPGatewaySource) map[loop.ModelAlias]ACPGatewaySource {
	result := make(map[loop.ModelAlias]ACPGatewaySource, len(input))
	for alias, source := range input {
		source.Model = source.Model.Clone()
		source.Efforts = append([]model.Effort(nil), source.Efforts...)
		result[alias] = source
	}
	return result
}

// runtimeHarnessDescription is deliberately CodeRig-owned. It is stable
// presentation metadata, not model configuration and not a credential route.
func runtimeHarnessDescription(harness loop.AgentHarnessName) string {
	switch harness {
	case looprigRuntimeHarness:
		return looprigRuntimeDescription
	case "codex":
		return codexRuntimeDescription
	case "claude-code":
		return claudeRuntimeDescription
	default:
		return ""
	}
}

// NativeTarget returns the private client/model binding for an explicit
// looprig/native selection. The returned model is pinned to the selected
// effort; no credential or endpoint metadata is exposed through Harness.
func (c ACPCompiledCatalog) NativeTarget(resolved loop.Resolved) (inference.Client, model.Model, error) {
	if resolved.AgentHarness != looprigRuntimeHarness || resolved.Profile != looprigRuntimeProfile || resolved.Source != loop.RuntimeSourceNative {
		return nil, model.Model{}, fmt.Errorf("coderig: native runtime target unavailable")
	}
	source, ok := c.gatewayTargets[resolved.ModelAlias]
	if !ok || source.Client == nil || !containsACPEffort(source.Efforts, resolved.Effort) {
		return nil, model.Model{}, fmt.Errorf("coderig: native runtime target unavailable")
	}
	selected := source.Model.Clone()
	selected.Sampling.Effort = resolved.Effort
	return source.Client, selected, nil
}

// ResolveNativeTarget resolves and returns the private binding for a selected
// ordinary model. It is a convenience seam for composition and integration
// tests; model-facing code still receives only the public runtime catalogue.
func (c ACPCompiledCatalog) ResolveNativeTarget(agent identity.AgentName, alias loop.ModelAlias, effort model.Effort) (inference.Client, model.Model, error) {
	resolved, err := c.RuntimeCatalog.ResolveWithExplicitSource(agent, looprigRuntimeHarness, loop.RuntimeSourceNative, alias, effort, true)
	if err != nil {
		return nil, model.Model{}, err
	}
	return c.NativeTarget(resolved)
}
