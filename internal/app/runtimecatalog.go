package app

import (
	"fmt"
	"sort"

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

// AgentRuntimeCatalogInput is the composition-root input for the complete
// parent-scoped runtime catalogue. ACP is optional: ordinary in-process rows
// are compiled from the configured delegate targets even when ACP has no
// usable profile.
type AgentRuntimeCatalogInput struct {
	AgentTypes     []identity.AgentName
	GatewayTargets []ACPGatewaySource
	Defaults       map[identity.AgentName]configuredDelegateDefault
	ACP            ACPCompiledCatalog
}

// CompileAgentRuntimeCatalog compiles ordinary looprig/native rows and merges
// already-compiled ACP rows. It is the only product-level catalogue compiler;
// ACP preflight may later remove unusable ACP entries without removing the
// ordinary in-process choice.
func CompileAgentRuntimeCatalog(input AgentRuntimeCatalogInput) (ACPCompiledCatalog, error) {
	roles := normalizedAgentTypes(input.AgentTypes)
	modelOptions, gatewayTargets, err := compileACPGatewayRows(input.GatewayTargets)
	if err != nil {
		return ACPCompiledCatalog{}, err
	}
	if len(gatewayTargets) == 0 && len(input.ACP.gatewayTargets) != 0 {
		gatewayTargets = cloneGatewayTargets(input.ACP.gatewayTargets)
	}

	entries := make([]loop.RuntimeCatalogEntry, 0, len(roles)+len(input.ACP.entries))
	for _, role := range roles {
		if len(modelOptions) == 0 {
			continue
		}
		models := cloneRuntimeOptions(modelOptions)
		for index := range models {
			models[index].Source = loop.RuntimeSourceNative
			models[index].Credential = loop.CredentialNativeAuth
		}
		if configured, ok := input.Defaults[role]; ok && (configured.Source == "" || configured.Source == loop.RuntimeSourceGateway) {
			setOrdinaryDefaultEffort(models, configured.Model, configured.Effort)
		}
		entries = append(entries, loop.RuntimeCatalogEntry{
			AgentType:     role,
			AgentHarness:  looprigRuntimeHarness,
			Profile:       looprigRuntimeProfile,
			Description:   looprigRuntimeDescription,
			Source:        loop.RuntimeSourceNative,
			Credential:    loop.CredentialNativeAuth,
			SelectionKind: loop.RuntimeSelectionExplicit,
			Default:       true,
			DefaultModel:  ordinaryDefaultModel(role, models, input.Defaults),
			Models:        models,
		})
	}
	for _, entry := range input.ACP.entries {
		entries = append(entries, cloneACPEntry(entry))
	}
	entries = selectRuntimeDefaults(entries)
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
	return ACPCompiledCatalog{
		RuntimeCatalog: catalog,
		gatewayTargets: gatewayTargets,
		profiles:       profiles,
		entries:        cloneACPEntries(entries),
	}, nil
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

func ordinaryDefaultModel(role identity.AgentName, models []loop.RuntimeModelOption, defaults map[identity.AgentName]configuredDelegateDefault) loop.ModelAlias {
	if configured, ok := defaults[role]; ok && hasRuntimeAlias(models, configured.Model) {
		return configured.Model
	}
	return firstRuntimeAlias(models)
}

func setOrdinaryDefaultEffort(models []loop.RuntimeModelOption, alias loop.ModelAlias, effort model.Effort) {
	for index := range models {
		if models[index].Alias == alias && containsACPEffort(models[index].Efforts, effort) {
			models[index].DefaultEffort = effort
		}
	}
}

// selectRuntimeDefaults gives a usable ACP default precedence over the
// ordinary row. If ACP is unavailable, the ordinary row remains the one
// deterministic default. A legacy ACP-only catalogue with no default is
// dropped for that role, preserving the pre-general-catalogue fail-closed
// behavior of lower-level ACP tests.
func selectRuntimeDefaults(entries []loop.RuntimeCatalogEntry) []loop.RuntimeCatalogEntry {
	roles := make(map[identity.AgentName]struct{})
	for _, entry := range entries {
		roles[entry.AgentType] = struct{}{}
	}
	for role := range roles {
		acpDefault := false
		ordinary := false
		defaultCount := 0
		for _, entry := range entries {
			if entry.AgentType != role {
				continue
			}
			if entry.AgentHarness == looprigRuntimeHarness {
				ordinary = true
			}
			if entry.Default {
				defaultCount++
				acpDefault = acpDefault || entry.AgentHarness != looprigRuntimeHarness
			}
		}
		if acpDefault {
			for index := range entries {
				if entries[index].AgentType == role && entries[index].AgentHarness == looprigRuntimeHarness {
					entries[index].Default = false
				}
			}
			continue
		}
		if defaultCount == 0 && ordinary {
			for index := range entries {
				if entries[index].AgentType == role && entries[index].AgentHarness == looprigRuntimeHarness {
					entries[index].Default = true
				}
			}
		}
	}
	return entries
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

// runtimeCatalogEntries is a deterministic test and composition inspection
// seam kept private so callers cannot mutate the immutable Harness catalogue.
func (c ACPCompiledCatalog) runtimeCatalogEntries() []loop.RuntimeCatalogEntry {
	result := cloneACPEntries(c.entries)
	sort.Slice(result, func(i, j int) bool {
		if result[i].AgentType != result[j].AgentType {
			return result[i].AgentType < result[j].AgentType
		}
		return result[i].AgentHarness < result[j].AgentHarness
	})
	return result
}
