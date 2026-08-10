package app

import (
	"fmt"

	"github.com/looprig/carbon/internal/catalog/carbon"
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

// CarbonRuntimeSource is the private client/model binding used by Carbon's
// in-process fallback. It is separate from ACPGatewaySource so a primer-only
// model cannot accidentally become an ACP row.
type CarbonRuntimeSource struct {
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
// PrimerTarget is used only by the in-process Carbon choice when no delegate
// target exists; it is never exposed as an ACP model.
type AgentRuntimeCatalogInput struct {
	GatewayTargets []ACPGatewaySource
	PrimerTarget   CarbonRuntimeSource
	ClaudeSmall    loop.ModelAlias
	NativeACP      map[string]ACPNativeProfile
}

// CompileAgentRuntimeCatalog is the one product-level catalogue compiler. It
// compiles raw optional ACP rows and the ordinary in-process Carbon row, then
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

	ordinaryOptions, nativeTargets, err := compileCarbonRuntimeOptions(raw.gatewayOptions, raw.gatewayTargets, input.PrimerTarget)
	if err != nil {
		return ACPCompiledCatalog{}, err
	}
	entries := make([]loop.RuntimeCatalogEntry, 0, len(raw.entries)+1)
	entries = append(entries, loop.RuntimeCatalogEntry{
		AgentType:     carbon.Name,
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
		entry.AgentType = carbon.Name
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
		nativeModels:   cloneACPNativeModelMappings(raw.nativeModels),
		profiles:       profiles,
		entries:        cloneACPEntries(entries),
	}, nil
}

// compileCarbonRuntimeOptions uses delegate-capable rows for ordinary
// in-process Carbon selection. The primer is the explicit fallback when no
// delegate row exists; it is deliberately not passed to the ACP compiler.
func compileCarbonRuntimeOptions(gatewayOptions []loop.RuntimeModelOption, gatewayTargets map[loop.ModelAlias]ACPGatewaySource, primer CarbonRuntimeSource) ([]loop.RuntimeModelOption, map[loop.ModelAlias]ACPGatewaySource, error) {
	if len(gatewayOptions) != 0 {
		options := cloneRuntimeOptions(gatewayOptions)
		for index := range options {
			options[index].Source = loop.RuntimeSourceNative
			options[index].Credential = loop.CredentialNativeAuth
		}
		return options, cloneGatewayTargets(gatewayTargets), nil
	}
	if primer.Alias == "" {
		return nil, nil, fmt.Errorf("carbon: Carbon runtime requires a configured primer")
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

func configuredPrimerRuntimeTarget(configured productionModels) CarbonRuntimeSource {
	description := ""
	for _, candidate := range configured.PrimerCandidates {
		if candidate.Alias == configured.PrimerAlias {
			description = candidate.Description
			break
		}
	}
	defaultEffort := configured.PrimerModel.Sampling.Effort
	if !containsACPEffort(configured.PrimerEfforts, defaultEffort) && len(configured.PrimerEfforts) != 0 {
		// Production normalization keeps these values aligned. Keep this
		// composition seam fail-safe for persisted/test inputs whose model
		// descriptor is stale: the runtime catalog may only advertise an
		// effort admitted by the configured primer.
		defaultEffort = configured.PrimerEfforts[0]
	}
	return CarbonRuntimeSource{
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

// runtimeHarnessDescription is deliberately Carbon-owned. It is stable
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
