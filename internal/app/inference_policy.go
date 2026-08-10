package app

import (
	"crypto/sha256"
	"strconv"

	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/inference/contextcount"

	model "github.com/looprig/inference/model"
	"github.com/looprig/llm"
)

// modelInferencePolicy is the narrow, secret-free context-policy surface used
// when composing a loop. The counter and capability are fixed for the lifetime
// of that loop; live model changes are constrained by loop.Definition.
type modelInferencePolicy interface {
	ContextCounter() contextcount.ContextCounter
	InferenceCapability() contextcount.InferenceCapability
}

type fixedModelInferencePolicy struct {
	counter    contextcount.ContextCounter
	capability contextcount.InferenceCapability
}

func (p fixedModelInferencePolicy) ContextCounter() contextcount.ContextCounter {
	return p.counter
}

func (p fixedModelInferencePolicy) InferenceCapability() contextcount.InferenceCapability {
	return p.capability
}

// UnsupportedInferenceProviderError reports a provider for which Carbon has no
// reviewed inference-transport declaration. It contains only a public provider
// label and never carries endpoint credentials.
type UnsupportedInferenceProviderError struct {
	Provider model.ProviderName
}

func (e *UnsupportedInferenceProviderError) Error() string {
	return "carbon: unsupported inference policy provider " + strconv.Quote(string(e.Provider))
}

const (
	chutesInferenceIdentityRevision  = "chutes-e2ee-tee-v1"
	phalaInferenceIdentityRevision   = "phala-aci-e2ee-v1"
	genericInferenceIdentityRevision = "generic-tls-v1"
)

// newModelInferencePolicy resolves the fixed, I/O-free counter and inference
// posture for one supported provider. The local estimator never sends request
// bytes to a separate counting endpoint. Remote retention remains Unknown
// because Carbon has no reviewed provider retention guarantee; the in-process
// RetentionNone counter remains compatible with that conservative declaration.
func newModelInferencePolicy(model model.Model) (modelInferencePolicy, error) {
	capability, err := inferenceCapabilityForModel(model)
	if err != nil {
		return nil, err
	}
	return fixedModelInferencePolicy{
		counter:    contextcount.NewEstimator(),
		capability: capability,
	}, nil
}

func inferenceCapabilityForModel(model model.Model) (contextcount.InferenceCapability, error) {
	provider := llm.Provider(model.Provider)
	switch provider {
	case llm.ProviderChutes:
		return protectedInferenceCapability(model, chutesInferenceIdentityRevision), nil
	case llm.ProviderPhala:
		return protectedInferenceCapability(model, phalaInferenceIdentityRevision), nil
	case llm.ProviderLMStudio:
		return contextcount.InferenceCapability{
			Provider:  contextcount.ProviderID(model.Provider),
			Transport: contextcount.InferenceTransportLocal,
			Retention: contextcount.RetentionNone,
		}, nil
	default:
		// Any provider modelconfig_normalize.go's llm.Provider(...).RequiredAuth()
		// gate would also accept gets the same conservative, unreviewed-tier
		// capability: plain TLS to a remote endpoint, retention unknown. It
		// still gets a SecurityIdentity — derived by the same
		// transportSecurityIdentity helper chutes/phala use, just under the
		// generic/unreviewed revision string above rather than a specific
		// reviewed provider policy — because contextcount.InferenceCapability's
		// Validate() requires non-zero SecurityIdentity for any transport at
		// or above TLS. The "no TEE-attestation review" distinction stays
		// fully carried by Transport/Retention (TLS + RetentionUnknown vs.
		// chutes/phala's EndToEndEncrypted), not by this field being zero. A
		// provider RequiredAuth itself doesn't recognize still fails closed —
		// this keeps the fail-closed posture for genuinely unknown input
		// while extending trust to exactly what models.json's own
		// normalization already trusts, no further.
		if _, err := provider.RequiredAuth(); err != nil {
			return contextcount.InferenceCapability{}, &UnsupportedInferenceProviderError{Provider: model.Provider}
		}
		return contextcount.InferenceCapability{
			Provider:         contextcount.ProviderID(model.Provider),
			Transport:        contextcount.InferenceTransportTLS,
			SecurityIdentity: transportSecurityIdentity(model, genericInferenceIdentityRevision),
			Retention:        contextcount.RetentionUnknown,
		}, nil
	}
}

// contextTransportsForModels derives the deduplicated loop-declarable
// transport set (by Provider, APIFormat, BaseURL) across models, in order,
// keeping the first occurrence of each distinct transport. The sole
// primitive behind declaredContextTransports.
func contextTransportsForModels(models []model.Model) ([]loop.ContextTransport, error) {
	type transportKey struct {
		Provider  model.ProviderName
		APIFormat model.APIFormat
		BaseURL   string
	}
	seen := make(map[transportKey]struct{}, len(models))
	transports := make([]loop.ContextTransport, 0, len(models))
	for _, m := range models {
		key := transportKey{Provider: m.Provider, APIFormat: m.APIFormat, BaseURL: m.BaseURL}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		capability, err := inferenceCapabilityForModel(m)
		if err != nil {
			return nil, err
		}
		transports = append(transports, loop.ContextTransport{
			Provider:   m.Provider,
			APIFormat:  m.APIFormat,
			BaseURL:    m.BaseURL,
			Capability: capability,
		})
	}
	return transports, nil
}

// declaredContextTransports derives the full loop-declarable transport set:
// the loop's own base model, every configured primer candidate's transport,
// and every configured gateway-backed delegate model's transport (native,
// in-process, RuntimeClient-routed StartAgent delegates — NOT NativeACP,
// which runs via a separate harness's own login state and never binds to a
// Carbon-owned loop.Definition). base is always seeded first so harness's
// build-time base-transport-membership requirement
// (pkg/loop/definition.go's validateContextDefinition: a non-empty declared
// set must contain a member matching the loop's own WithInference model,
// with Capability exactly equal to WithInferenceCapability) holds
// regardless of whether base happens to also appear in primerCandidates —
// equality is automatic since both are derived by calling
// inferenceCapabilityForModel on the same model value. Native delegate
// loops are ordinary harness Loop instances subject to the same
// declared-transport restore check as the Carbon primer and delegates, so omitting their
// transport here would make restoring a session with an active/prior
// delegate on a foreign transport fail harness's RestoreTransportMismatchError.
func declaredContextTransports(base model.Model, primerCandidates []PrimerCandidate, delegateModels []model.Model) ([]loop.ContextTransport, error) {
	models := make([]model.Model, 0, 1+len(primerCandidates)+len(delegateModels))
	models = append(models, base)
	for _, c := range primerCandidates {
		models = append(models, c.Model)
	}
	models = append(models, delegateModels...)
	return contextTransportsForModels(models)
}

// delegateModelsFrom projects the configured gateway-backed delegate catalog
// down to the bare models declaredContextTransports needs. It carries no
// client or credential.
func delegateModelsFrom(sources []ACPGatewaySource) []model.Model {
	models := make([]model.Model, len(sources))
	for i, s := range sources {
		models[i] = s.Model
	}
	return models
}

func protectedInferenceCapability(model model.Model, policyRevision string) contextcount.InferenceCapability {
	return contextcount.InferenceCapability{
		Provider:         contextcount.ProviderID(model.Provider),
		Transport:        contextcount.InferenceTransportEndToEndEncrypted,
		SecurityIdentity: transportSecurityIdentity(model, policyRevision),
		Retention:        contextcount.RetentionUnknown,
	}
}

// transportSecurityIdentity binds capability metadata to the exact transport
// fields harness keeps immutable plus a reviewed provider-policy revision. It
// intentionally excludes model name, limits, sampling, and capabilities, which
// may change live without replacing the connection security boundary.
func transportSecurityIdentity(model model.Model, policyRevision string) contextcount.SecurityIdentity {
	material := string(model.Provider) + "\x00" + string(model.APIFormat) + "\x00" +
		model.BaseURL + "\x00" + policyRevision
	return sha256.Sum256([]byte(material))
}
