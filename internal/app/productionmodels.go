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

type configuredClientCacheKey struct {
	Target runtimeModelKey
	APIKey string
}

// PrimerCandidate is one models.json entry tagged primer-capable
// (uses: ["primer", ...]). RuntimeAgent uses the roster of these to list
// and switch the primer loop's model at runtime.
type PrimerCandidate struct {
	Alias         string
	Description   string
	Model         model.Model
	Efforts       []model.Effort
	DefaultEffort model.Effort
}

// productionModels is the one-way result of binding credentials to clients.
// It deliberately cannot reproduce the normalized input or any credential.
type productionModels struct {
	PrimerClient     inference.Client
	RuntimeClient    inference.Client
	PrimerModel      model.Model
	PrimerAlias      string
	PrimerEfforts    []model.Effort
	PrimerCandidates []PrimerCandidate
	ACP              []ACPGatewaySource
	NativeACP        map[string]ACPNativeProfile
	ACPLaunchers     map[string]string // harness -> configured executable path
	// Defaults is retained as a temporary runtime-catalog compatibility seam.
	// It is no longer populated from models.json; Task 3 removes this input
	// when the catalogue is fixed to the Generic agent.
	Defaults    map[identity.AgentName]configuredDelegateDefault
	ClaudeSmall loop.ModelAlias
	ConfigRev   string
	// PermissionReviewEnabled, PermissionReviewModel, and PermissionReviewStrict
	// mirror Config's own PermissionReviewEnabled/PermissionReviewModel/
	// PermissionReviewStrictPolicy fields so production.go's and swarm.go's
	// copy-over reads as an obvious 1:1 mapping. They are resolved from an
	// optional models.json permission_review section; the zero values (false,
	// the zero model.Model, false) mean the section was absent.
	PermissionReviewEnabled bool
	PermissionReviewModel   model.Model
	PermissionReviewStrict  bool
}

func compileProductionModels(config normalizedModelConfig, factory configuredClientFactory) (productionModels, error) {
	configRev, err := modelConfigDigest(config)
	if err != nil {
		return productionModels{}, err
	}

	clients := make(map[string]inference.Client, len(config.Models))
	boundClients := make(map[configuredClientCacheKey]inference.Client, len(config.Models))
	delegateSources := make([]ACPGatewaySource, 0, len(config.Models))
	primerCandidates := make([]PrimerCandidate, 0, len(config.Models))
	primerCandidateTargets := make(map[runtimeModelKey]string, len(config.Models))
	models := make(map[string]model.Model, len(config.Models))
	var primerEfforts []model.Effort
	for _, target := range config.Models {
		cacheKey := configuredClientCacheKey{Target: runtimeModelKeyFor(target.Model), APIKey: target.client.APIKey}
		client, ok := boundClients[cacheKey]
		if !ok {
			client, err = factory(target.Model, auth.APIKey(target.client.APIKey))
			if err != nil {
				return productionModels{}, fmt.Errorf("coderig: construct configured model alias %q provider %q", target.Alias, target.Model.Provider)
			}
			boundClients[cacheKey] = client
		}
		clients[target.Alias] = client
		models[target.Alias] = target.Model.Clone()
		if target.Alias == config.PrimerDefault {
			primerEfforts = append([]model.Effort(nil), target.Efforts...)
		}
		if containsModelConfigUse(target.Uses, "primer") {
			key := runtimeModelKeyFor(target.Model)
			if _, dup := primerCandidateTargets[key]; dup {
				return productionModels{}, modelConfigValidationError("primer-capable models must not share the same provider target")
			}
			primerCandidateTargets[key] = target.Alias
			primerCandidates = append(primerCandidates, PrimerCandidate{
				Alias:         target.Alias,
				Description:   target.Description,
				Model:         target.Model.Clone(),
				Efforts:       append([]model.Effort(nil), target.Efforts...),
				DefaultEffort: target.DefaultEffort,
			})
		}
		if containsModelConfigUse(target.Uses, "delegate") {
			delegateSources = append(delegateSources, ACPGatewaySource{
				Alias:         loop.ModelAlias(target.Alias),
				Description:   target.Description,
				Client:        client,
				Model:         target.Model.Clone(),
				DefaultEffort: target.DefaultEffort,
				Efforts:       append([]model.Effort(nil), target.Efforts...),
			})
		}
	}
	runtimeClient, err := newModelRoutingClient(configuredModelBindings(config, clients))
	if err != nil {
		return productionModels{}, err
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

	var permissionReviewEnabled bool
	var permissionReviewModel model.Model
	var permissionReviewStrict bool
	if config.PermissionReview != nil {
		permissionReviewEnabled = true
		permissionReviewModel = models[config.PermissionReview.Model]
		permissionReviewStrict = config.PermissionReview.Strict
	}

	var acpLaunchers map[string]string
	if config.ACPLaunchers != nil {
		acpLaunchers = make(map[string]string, len(config.ACPLaunchers))
		for harness, launcher := range config.ACPLaunchers {
			acpLaunchers[harness] = launcher.Executable
		}
	}

	return productionModels{
		PrimerClient:            clients[config.PrimerDefault],
		RuntimeClient:           runtimeClient,
		PrimerModel:             models[config.PrimerDefault],
		PrimerAlias:             config.PrimerDefault,
		PrimerEfforts:           primerEfforts,
		PrimerCandidates:        primerCandidates,
		ACP:                     delegateSources,
		NativeACP:               nativeACP,
		ACPLaunchers:            acpLaunchers,
		ClaudeSmall:             loop.ModelAlias(config.ClaudeCodeSmallModel),
		ConfigRev:               configRev,
		PermissionReviewEnabled: permissionReviewEnabled,
		PermissionReviewModel:   permissionReviewModel,
		PermissionReviewStrict:  permissionReviewStrict,
	}, nil
}

func configuredModelBindings(config normalizedModelConfig, clients map[string]inference.Client) []modelBinding {
	bindings := make([]modelBinding, 0, len(config.Models))
	for _, target := range config.Models {
		bindings = append(bindings, modelBinding{Model: target.Model, Client: clients[target.Alias]})
	}
	return bindings
}

func (p productionModels) String() string {
	return fmt.Sprintf("production models primer_alias=%q delegates=%d config_rev=%q", p.PrimerAlias, len(p.ACP), p.ConfigRev)
}

func (p productionModels) GoString() string { return p.String() }
