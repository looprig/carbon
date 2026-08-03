package app

import (
	"fmt"

	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/inference"
	"github.com/looprig/inference/auth"
	model "github.com/looprig/inference/model"
)

type configuredClientFactory func(model.Model, auth.APIKey) (inference.Client, error)

type configuredDelegateDefault struct {
	Harness loop.AgentHarnessName
	Source  loop.RuntimeSourceName
	Model   loop.ModelAlias
	Effort  model.Effort
}

// productionModels is the one-way result of binding credentials to clients.
// It deliberately cannot reproduce the normalized input or any credential.
type productionModels struct {
	PrimerClient  inference.Client
	PrimerModel   model.Model
	PrimerAlias   string
	PrimerEfforts []model.Effort
	ACP           []ACPGatewaySource
	NativeACP     map[string]ACPNativeProfile
	Defaults      map[identity.AgentName]configuredDelegateDefault
	ClaudeSmall   loop.ModelAlias
	ConfigRev     string
}

func compileProductionModels(config normalizedModelConfig, factory configuredClientFactory) (productionModels, error) {
	configRev, err := modelConfigDigest(config)
	if err != nil {
		return productionModels{}, err
	}

	clients := make(map[string]inference.Client, len(config.Models))
	delegateSources := make([]ACPGatewaySource, 0, len(config.Models))
	models := make(map[string]model.Model, len(config.Models))
	var primerEfforts []model.Effort
	for _, target := range config.Models {
		client, err := factory(target.Model, auth.APIKey(target.client.APIKey))
		if err != nil {
			return productionModels{}, fmt.Errorf("coderig: construct configured model alias %q provider %q", target.Alias, target.Model.Provider)
		}
		clients[target.Alias] = client
		models[target.Alias] = target.Model.Clone()
		if target.Alias == config.PrimerDefault {
			primerEfforts = append([]model.Effort(nil), target.Efforts...)
		}
		if containsModelConfigUse(target.Uses, "delegate") {
			delegateSources = append(delegateSources, ACPGatewaySource{
				Alias:         loop.ModelAlias(target.Alias),
				Client:        client,
				Model:         target.Model.Clone(),
				DefaultEffort: target.DefaultEffort,
				Efforts:       append([]model.Effort(nil), target.Efforts...),
			})
		}
	}

	defaults := make(map[identity.AgentName]configuredDelegateDefault, len(config.DelegateDefaults))
	for _, value := range config.DelegateDefaults {
		defaults[identity.AgentName(value.Role)] = configuredDelegateDefault{
			Harness: loop.AgentHarnessName(value.Harness),
			Source:  value.Source,
			Model:   loop.ModelAlias(value.Model),
			Effort:  value.Effort,
		}
	}
	var nativeACP map[string]ACPNativeProfile
	if config.NativeACP != nil {
		nativeACP = make(map[string]ACPNativeProfile, len(config.NativeACP))
		for harness, profile := range config.NativeACP {
			var models []loop.ModelAlias
			if profile.Models != nil {
				models = make([]loop.ModelAlias, len(profile.Models))
				for i, alias := range profile.Models {
					models[i] = loop.ModelAlias(alias)
				}
			}
			nativeACP[harness] = ACPNativeProfile{
				Harness: loop.AgentHarnessName(profile.Harness), Enabled: profile.Enabled, Models: models,
			}
		}
	}

	return productionModels{
		PrimerClient:  clients[config.PrimerDefault],
		PrimerModel:   models[config.PrimerDefault],
		PrimerAlias:   config.PrimerDefault,
		PrimerEfforts: primerEfforts,
		ACP:           delegateSources,
		NativeACP:     nativeACP,
		Defaults:      defaults,
		ClaudeSmall:   loop.ModelAlias(config.ClaudeCodeSmallModel),
		ConfigRev:     configRev,
	}, nil
}

func (p productionModels) String() string {
	return fmt.Sprintf("production models primer_alias=%q delegates=%d defaults=%d config_rev=%q", p.PrimerAlias, len(p.ACP), len(p.Defaults), p.ConfigRev)
}

func (p productionModels) GoString() string { return p.String() }
