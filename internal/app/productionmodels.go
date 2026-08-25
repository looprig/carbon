package app

import (
	"context"
	"fmt"
	"io"
	"reflect"

	"github.com/looprig/credentials"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/inference"
	model "github.com/looprig/inference/model"
)

type configuredClientFactory func(model.Model, modelClientInput) (inference.Client, error)
type configuredClientContextFactory func(context.Context, model.Model, modelClientInput) (inference.Client, error)

type configuredClientCacheKey struct {
	Target        runtimeModelKey
	APIKey        string
	CredentialRef credentials.Reference
}

// configuredClientConstructionError retains only a safe cause classification
// for callers that need to distinguish a missing/mismatched credential ref.
// Its Error text deliberately contains aliases/providers only; it never
// reflects factory errors that might carry provider material.
type configuredClientConstructionError struct {
	Alias    string
	Provider string
	Cause    error
}

func (e *configuredClientConstructionError) Error() string {
	if e == nil {
		return "carbon: configured model client construction failed"
	}
	return fmt.Sprintf("carbon: construct configured model alias %q provider %q", e.Alias, e.Provider)
}

func (e *configuredClientConstructionError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func (e *configuredClientConstructionError) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, e.Error())
}

// PrimerCandidate is one models.json entry tagged primer-capable
// (uses: ["primer", ...]). RuntimeAgent uses the roster of these to list
// and switch the primer loop's model at runtime.
type PrimerCandidate struct {
	Alias string
	// Label is the configured display name, empty when the file omitted one. The picker
	// falls back to deriving a name from the model id; see modelDisplayLabel.
	Label         string
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
	ClaudeSmall      loop.ModelAlias
	ConfigRev        string
	// ConfigRev is durable restore identity. Inline-key clients separately
	// opt out of cross-composition reuse because key bytes are not digest input.
	ClientReuseEligible bool
	// PermissionReviewEnabled, PermissionReviewModel, and PermissionReviewStrict
	// mirror Config's own PermissionReviewEnabled/PermissionReviewModel/
	// PermissionReviewStrictPolicy fields so production and assembly's
	// copy-over reads as an obvious 1:1 mapping. They are resolved from an
	// optional models.json permission_review section; the zero values (false,
	// the zero model.Model, false) mean the section was absent.
	PermissionReviewEnabled bool
	PermissionReviewModel   model.Model
	PermissionReviewStrict  bool
	// credentialRuntime is owned by the RuntimeAgent produced from this
	// composition. It is intentionally absent for legacy/static model
	// configurations, preserving their existing no-store behavior.
	credentialRuntime *credentialRuntime
	credentialRefs    []credentials.Reference
	credentialLease   *credentialRegistryLease
}

func compileProductionModels(config normalizedModelConfig, factory configuredClientFactory) (productionModels, error) {
	if factory == nil {
		return compileProductionModelsWithContext(context.Background(), config, nil)
	}
	return compileProductionModelsWithContext(context.Background(), config, func(_ context.Context, selected model.Model, input modelClientInput) (inference.Client, error) {
		return factory(selected, input)
	})
}

func compileProductionModelsWithContext(ctx context.Context, config normalizedModelConfig, factory configuredClientContextFactory) (productionModels, error) {
	if factory == nil {
		return productionModels{}, modelConfigValidationError("configured model factory is unavailable")
	}
	configRev, err := modelConfigDigest(config)
	if err != nil {
		return productionModels{}, err
	}

	clients := make(map[string]inference.Client, len(config.Models))
	// This cache is deliberately scoped to one composition. It is not a
	// cross-load client-reuse seam, so inline-key rotations still compose fresh.
	boundClients := make(map[configuredClientCacheKey]inference.Client, len(config.Models))
	delegateSources := make([]ACPGatewaySource, 0, len(config.Models))
	primerCandidates := make([]PrimerCandidate, 0, len(config.Models))
	primerCandidateTargets := make(map[runtimeModelKey]string, len(config.Models))
	models := make(map[string]model.Model, len(config.Models))
	var primerEfforts []model.Effort
	for _, target := range config.Models {
		if !target.client.valid() {
			return productionModels{}, modelConfigValidationError("configured model auth must contain at most one of api_key or credential_ref")
		}
		cacheKey := configuredClientCacheKey{
			Target: runtimeModelKeyFor(target.Model), APIKey: target.client.APIKey,
			CredentialRef: target.client.CredentialRef,
		}
		client, ok := boundClients[cacheKey]
		if !ok {
			client, err = factory(ctx, target.Model, target.client)
			if err != nil {
				return productionModels{}, &configuredClientConstructionError{Alias: target.Alias, Provider: string(target.Model.Provider), Cause: err}
			}
			if nilInferenceClient(client) {
				return productionModels{}, modelConfigValidationError("configured model factory returned no client")
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
				Label:         target.Label,
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
			var modelOptions []ACPNativeModelOption
			if profile.ModelOptions != nil {
				modelOptions = make([]ACPNativeModelOption, len(profile.ModelOptions))
				for i, option := range profile.ModelOptions {
					modelOptions[i] = ACPNativeModelOption{
						Alias:         loop.ModelAlias(option.Model),
						Model:         option.Model,
						Efforts:       append([]model.Effort(nil), option.Efforts...),
						DefaultEffort: option.DefaultEffort,
					}
					models = append(models, modelOptions[i].Alias)
				}
			} else if profile.Models != nil {
				// Keep lower-level callers that construct normalized profiles by
				// hand compatible with the legacy model-only representation.
				models = make([]loop.ModelAlias, len(profile.Models))
				for i, alias := range profile.Models {
					models[i] = loop.ModelAlias(alias)
				}
			}
			nativeACP[harness] = ACPNativeProfile{
				Harness:      loop.AgentHarnessName(profile.Harness),
				Enabled:      profile.Enabled,
				Models:       models,
				ModelOptions: modelOptions,
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
		ClientReuseEligible:     modelConfigDigestEligible(config),
		PermissionReviewEnabled: permissionReviewEnabled,
		PermissionReviewModel:   permissionReviewModel,
		PermissionReviewStrict:  permissionReviewStrict,
	}, nil
}

func nilInferenceClient(client inference.Client) bool {
	if client == nil {
		return true
	}
	value := reflect.ValueOf(client)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
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
