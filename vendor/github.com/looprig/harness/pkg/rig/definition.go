package rig

import (
	"context"
	"strings"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/internal/sessionruntime"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/hook"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/harness/pkg/tool"
)

type definitionState struct {
	loops                   []loop.Definition
	hustles                 []hustle.Definition
	hustleLimits            HustleLimits
	primers                 []string
	activePrimer            string
	store                   *sessionstore.Store
	storeSet                bool
	seen                    map[singletonKey]bool
	lifecycleOptions        []sessionruntime.LifecycleOption
	fingerprintFields       ConfigFingerprintFields
	permissionClassifiers   gate.PermissionClassifierSet
	permissionReviewPolicy  gate.PermissionReviewPolicy
	permissionReviewLimits  PermissionReviewLimits
	hooks                   hook.Set
	compiledHooks           *hook.Runner
	resourceStorageProvider SessionResourceStorageProvider
	// placements accumulates every workspace placement option. Define enforces at most
	// one; more than one is a typed rejection.
	placements     []pendingPlacement
	snapshotPolicy *SnapshotPolicy
}

// Rig is an immutable design-time assembly that creates and restores sessions.
type Rig struct {
	lifecycle               *sessionruntime.Lifecycle
	hooks                   *hook.Runner
	resourceStorageProvider SessionResourceStorageProvider
}

func Define(options ...Option) (*Rig, error) {
	state := &definitionState{seen: make(map[singletonKey]bool)}
	for _, option := range options {
		if option == nil {
			return nil, &DefinitionError{Kind: DefinitionNilOption}
		}
		if err := option(state); err != nil {
			return nil, err
		}
	}
	compiledHooks, err := hook.Compile(state.hooks)
	if err != nil {
		return nil, &DefinitionError{Kind: DefinitionInvalidHooks, Cause: err}
	}
	state.compiledHooks = compiledHooks
	if !state.storeSet || state.store == nil {
		return nil, &DefinitionError{Kind: DefinitionMissingSessionStore}
	}
	if len(state.loops) == 0 {
		return nil, &DefinitionError{Kind: DefinitionMissingLoop}
	}
	byName := make(map[string]loop.Definition, len(state.loops))
	for _, definition := range state.loops {
		name := string(definition.Name())
		if strings.TrimSpace(name) == "" {
			return nil, &DefinitionError{Kind: DefinitionInvalidLoop}
		}
		if _, exists := byName[name]; exists {
			return nil, &DefinitionError{Kind: DefinitionDuplicateLoop, Name: name}
		}
		byName[name] = definition
	}
	if len(state.primers) == 0 {
		return nil, &DefinitionError{Kind: DefinitionMissingPrimer}
	}
	seenPrimers := make(map[string]bool, len(state.primers))
	for _, primer := range state.primers {
		if seenPrimers[primer] {
			return nil, &DefinitionError{Kind: DefinitionInvalidPrimer, Name: primer}
		}
		seenPrimers[primer] = true
		if _, exists := byName[primer]; !exists {
			return nil, &DefinitionError{Kind: DefinitionInvalidPrimer, Name: primer}
		}
	}
	if len(state.primers) == 1 && !state.seen[keyActivePrimer] {
		state.activePrimer = state.primers[0]
	}
	if state.activePrimer == "" || !seenPrimers[state.activePrimer] {
		return nil, &DefinitionError{Kind: DefinitionInvalidActivePrimer, Name: state.activePrimer}
	}
	for _, definition := range state.loops {
		for _, delegate := range definition.Delegates() {
			name := string(delegate)
			if _, exists := byName[name]; !exists {
				return nil, &DefinitionError{Kind: DefinitionInvalidLoop, Name: name}
			}
		}
	}
	queue := append([]string(nil), state.primers...)
	visited := make(map[string]bool, len(byName))
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if visited[name] {
			continue
		}
		visited[name] = true
		for _, delegate := range byName[name].Delegates() {
			queue = append(queue, string(delegate))
		}
	}
	for name := range byName {
		if !visited[name] {
			return nil, &DefinitionError{Kind: DefinitionInvalidLoop, Name: name}
		}
	}
	permissionReview, err := resolvePermissionReviewFingerprint(state)
	if err != nil {
		return nil, err
	}
	permissionReviewLimits, err := resolvePermissionReviewLimits(state)
	if err != nil {
		return nil, err
	}
	if err := validatePermissionReviewEvidence(state); err != nil {
		return nil, err
	}
	if err := validatePermissionReviewSecurityCeiling(state); err != nil {
		return nil, err
	}
	if err := validatePermissionReviewObservations(state); err != nil {
		return nil, err
	}
	if requiresProcessServices(state.loops) && state.resourceStorageProvider == nil {
		return nil, &DefinitionError{Kind: DefinitionMissingResourceStorage}
	}
	if err := validateHustleRegistration(state); err != nil {
		return nil, err
	}
	if err := validateCompactionHustles(state.loops, state.hustles); err != nil {
		return nil, err
	}
	// Resolve the (at-most-one) workspace placement: canonicalize the root/base, derive the
	// exclusive root lease name, and enforce non-nil dependencies. A workspace-requiring
	// tool with NO placement makes the rig invalid.
	placement, region, err := resolvePlacement(state.placements)
	if err != nil {
		return nil, err
	}
	if !placement.Configured() && requiresWorkspaceTool(state.loops) {
		return nil, &WorkspacePlacementError{Kind: WorkspaceToolWithoutPlacement}
	}
	if placement.Configured() && state.snapshotPolicy == nil {
		return nil, &SnapshotPolicyError{Kind: SnapshotPolicyRequired}
	}
	if !placement.Configured() && state.snapshotPolicy != nil {
		return nil, &SnapshotPolicyError{Kind: SnapshotPolicyWithoutWorkspace}
	}
	if placement.Mode == sessionruntime.PlacementShared && state.snapshotPolicy != nil && state.snapshotPolicy.Priority == SnapshotRequired {
		return nil, &SnapshotPolicyError{Kind: SnapshotPolicySharedRequired}
	}
	if placement.Configured() {
		if err := checkPersistenceOverlap(state.store, placement, region); err != nil {
			return nil, err
		}
	}

	fields := state.fingerprintFields
	if placement.Configured() {
		// Fold the placement mode + canonical region into the workspace-root fingerprint
		// field so a placement change (mode or path) is a config change.
		fields.WorkspaceRoot = placementFingerprint(placement, region)
	}
	fingerprint := frozenFingerprintWithPermissionReview(
		fields, state.loops, state.primers, state.activePrimer,
		state.hustles, state.hustleLimits, permissionReview,
	)
	manifest := frozenManifestWithPermissionReview(
		fields, state.loops, state.primers, state.activePrimer,
		state.hustles, state.hustleLimits, permissionReview,
	)
	manifest.HookPolicyRev = state.hooks.PolicyRevision
	lifecycleOptions := append([]sessionruntime.LifecycleOption(nil), state.lifecycleOptions...)
	if len(state.hustles) > 0 {
		lifecycleOptions = append(lifecycleOptions, sessionruntime.WithLifecycleHustles(
			append([]hustle.Definition(nil), state.hustles...),
			lifecycleHustleLimits(state.hustleLimits),
		))
	}
	if placement.Configured() {
		lifecycleOptions = append(lifecycleOptions, sessionruntime.WithLifecyclePlacement(placement))
		policy := *state.snapshotPolicy
		internalPolicy := sessionruntime.SnapshotPolicy{Timeout: policy.Timeout}
		switch policy.Trigger {
		case SnapshotManual:
			internalPolicy.Trigger = sessionruntime.SnapshotManual
		case SnapshotOnIdle:
			internalPolicy.Trigger = sessionruntime.SnapshotOnIdle
		case SnapshotOnTurnDone:
			internalPolicy.Trigger = sessionruntime.SnapshotOnTurnDone
		case SnapshotOnStepDone:
			internalPolicy.Trigger = sessionruntime.SnapshotOnStepDone
		}
		if policy.Priority == SnapshotRequired {
			internalPolicy.Priority = sessionruntime.SnapshotRequired
		} else {
			internalPolicy.Priority = sessionruntime.SnapshotBestEffort
		}
		lifecycleOptions = append(lifecycleOptions, sessionruntime.WithLifecycleSnapshotPolicy(internalPolicy))
	}
	if state.seen[keyPermissionClassifiers] {
		lifecycleOptions = append(lifecycleOptions, sessionruntime.WithLifecyclePermissionReview(
			state.permissionClassifiers, state.permissionReviewPolicy,
		))
		if permissionReviewLimits != nil {
			lifecycleOptions = append(lifecycleOptions, sessionruntime.WithLifecyclePermissionReviewBreaker(
				lifecyclePermissionReviewBreakerLimits(*permissionReviewLimits),
			))
		}
	}
	lifecycleOptions = append(lifecycleOptions, sessionruntime.WithLifecycleFingerprint(fingerprint))
	lifecycleOptions = append(lifecycleOptions, sessionruntime.WithLifecycleManifest(manifest))
	lifecycleOptions = append(lifecycleOptions, sessionruntime.WithLifecycleHooks(state.compiledHooks))
	if state.resourceStorageProvider != nil {
		provider := state.resourceStorageProvider
		lifecycleOptions = append(lifecycleOptions, sessionruntime.WithLifecycleSessionResourceStorage(
			func(ctx context.Context, id uuid.UUID) (string, string, error) {
				storage, err := provider.StorageForSession(ctx, id)
				return storage.Path, storage.Identity, err
			},
		))
	}
	primerNames := make([]identity.AgentName, len(state.primers))
	for i, name := range state.primers {
		primerNames[i] = identity.AgentName(name)
	}
	lifecycle, err := sessionruntime.NewTopologyLifecycle(sessionruntime.Topology{Definitions: append([]loop.Definition(nil), state.loops...), Primers: primerNames, ActivePrimer: identity.AgentName(state.activePrimer)}, state.store, lifecycleOptions...)
	if err != nil {
		return nil, &DefinitionError{Kind: DefinitionInvalidSessionStore, Cause: err}
	}
	return &Rig{
		lifecycle:               lifecycle,
		hooks:                   state.compiledHooks,
		resourceStorageProvider: state.resourceStorageProvider,
	}, nil
}

func requiresProcessServices(definitions []loop.Definition) bool {
	for _, definition := range definitions {
		if definition.ToolRequirements()&tool.RequiresProcessServices != 0 {
			return true
		}
	}
	return false
}

func resolvePermissionReviewFingerprint(state *definitionState) (*permissionReviewFingerprint, error) {
	classifiersConfigured := state.seen[keyPermissionClassifiers]
	policyConfigured := state.seen[keyPermissionReviewPolicy]
	if !classifiersConfigured && !policyConfigured {
		return nil, nil
	}
	if !classifiersConfigured || !policyConfigured {
		return nil, &DefinitionError{Kind: DefinitionIncompletePermissionReview}
	}
	projection, err := permissionReviewFingerprintFrom(
		state.permissionClassifiers,
		state.permissionReviewPolicy,
	)
	if err != nil {
		return nil, &DefinitionError{Kind: DefinitionInvalidPermissionClassifiers, Cause: err}
	}
	return projection, nil
}

// resolvePermissionReviewLimits resolves the circuit-breaker limits Define()
// forwards to sessionruntime.WithLifecyclePermissionReviewBreaker.
//
//   - classifiers configured + WithPermissionReviewLimits never called:
//     resolves the default (every one of the 8 numeric thresholds set to
//     DefaultPermissionReviewBreakerThreshold, InterruptOnTrip false).
//   - classifiers configured + WithPermissionReviewLimits called: the
//     explicit value is used verbatim (no per-field merge with the default).
//   - classifiers NOT configured + WithPermissionReviewLimits never called:
//     resolves nothing (nil) — no breaker Lifecycle option is ever applied,
//     preserving the current no-review-configured no-op.
//   - classifiers NOT configured + WithPermissionReviewLimits called anyway:
//     a typed DefinitionUnusedPermissionReviewLimits error, mirroring
//     validateHustleRegistration's DefinitionUnusedHustleLimits precedent.
func resolvePermissionReviewLimits(state *definitionState) (*PermissionReviewLimits, error) {
	classifiersConfigured := state.seen[keyPermissionClassifiers]
	limitsConfigured := state.seen[keyPermissionReviewLimits]
	if !classifiersConfigured {
		if limitsConfigured {
			return nil, &DefinitionError{Kind: DefinitionUnusedPermissionReviewLimits}
		}
		return nil, nil
	}
	if limitsConfigured {
		limits := state.permissionReviewLimits
		return &limits, nil
	}
	return &PermissionReviewLimits{
		MaxConsecutiveNeedsHuman: DefaultPermissionReviewBreakerThreshold,
		MaxInvalidOrFailed:       DefaultPermissionReviewBreakerThreshold,
		MaxIdenticalSubjects:     DefaultPermissionReviewBreakerThreshold,
		MaxStaleResponses:        DefaultPermissionReviewBreakerThreshold,
		InterruptOnTrip:          false,
		Session: PermissionReviewSessionLimits{
			MaxConsecutiveNeedsHuman: DefaultPermissionReviewBreakerThreshold,
			MaxInvalidOrFailed:       DefaultPermissionReviewBreakerThreshold,
			MaxIdenticalSubjects:     DefaultPermissionReviewBreakerThreshold,
			MaxStaleResponses:        DefaultPermissionReviewBreakerThreshold,
		},
	}, nil
}

// validatePermissionReviewEvidence enforces the "config X requires config Y"
// pairing between WithPermissionReviewEvidence and any registered
// classifier's evidence-tool need, mirroring
// resolvePermissionReviewLimits/validateHustleRegistration's own pairing
// checks:
//
//   - at least one registered classifier's Definition needs evidence tools +
//     WithPermissionReviewEvidence never called: DefinitionMissingPermissionReviewEvidence
//     (this is the confirmed Task 23 hard blocker's Define()-time guard —
//     the classifier-registered session that would otherwise fail 100% of
//     the time at hustle-controller construction now fails earlier, with a
//     clear cause, at Define()).
//   - WithPermissionReviewEvidence called but NO registered classifier needs
//     evidence tools (including no classifiers configured at all):
//     DefinitionUnusedPermissionReviewEvidence.
//   - every other combination (both configured and needed, or neither):
//     no error.
func validatePermissionReviewEvidence(state *definitionState) error {
	needed := state.seen[keyPermissionClassifiers] && anyClassifierNeedsEvidence(state.permissionClassifiers)
	configured := state.seen[keyPermissionReviewEvidence]
	switch {
	case needed && !configured:
		return &DefinitionError{Kind: DefinitionMissingPermissionReviewEvidence}
	case !needed && configured:
		return &DefinitionError{Kind: DefinitionUnusedPermissionReviewEvidence}
	default:
		return nil
	}
}

// validatePermissionReviewSecurityCeiling enforces the "config X requires
// config Y" pairing between WithPermissionReviewSecurityCeiling and any
// registered permission classifier, mirroring
// validatePermissionReviewEvidence's own pairing check exactly — except
// keyed on "classifiers configured at all" rather than "a classifier needs
// evidence tools", because SecurityCeiling flows into every registered
// classifier's ReviewBasis regardless of whether that classifier declares
// evidence tools (review_adapter.go's reviewOne stamps
// basis.SecurityCeiling unconditionally; hustle.Request.SecurityCeiling's
// own doc comment notes a classifier with no evidence-tool concept simply
// never reads it):
//
//   - at least one classifier registered + WithPermissionReviewSecurityCeiling
//     never called: DefinitionMissingPermissionReviewSecurityCeiling.
//   - WithPermissionReviewSecurityCeiling called but no classifiers
//     registered: DefinitionUnusedPermissionReviewSecurityCeiling.
//   - every other combination (both configured, or neither): no error.
func validatePermissionReviewSecurityCeiling(state *definitionState) error {
	classifiersConfigured := state.seen[keyPermissionClassifiers]
	ceilingConfigured := state.seen[keyPermissionReviewSecurityCeiling]
	switch {
	case classifiersConfigured && !ceilingConfigured:
		return &DefinitionError{Kind: DefinitionMissingPermissionReviewSecurityCeiling}
	case !classifiersConfigured && ceilingConfigured:
		return &DefinitionError{Kind: DefinitionUnusedPermissionReviewSecurityCeiling}
	default:
		return nil
	}
}

// validatePermissionReviewObservations enforces WithPermissionReviewObservations'
// own documented "config X requires config Y" pairing — see that option's
// doc comment (options.go) for the full reasoning behind why this checks
// only the unused direction, never a symmetric "missing" direction:
//
//   - WithPermissionReviewObservations called + no classifiers registered
//     at all: DefinitionUnusedPermissionReviewObservations.
//   - WithPermissionReviewObservations called + classifiers registered but
//     WithPermissionReviewEvidence never configured (so there is no
//     evidence runtime at all for any observation to ever be recorded
//     into): DefinitionUnusedPermissionReviewObservations.
//   - every other combination (not configured at all; configured alongside
//     both classifiers and evidence): no error.
func validatePermissionReviewObservations(state *definitionState) error {
	if !state.seen[keyPermissionReviewObservations] {
		return nil
	}
	classifiersConfigured := state.seen[keyPermissionClassifiers]
	evidenceConfigured := state.seen[keyPermissionReviewEvidence]
	if !classifiersConfigured || !evidenceConfigured {
		return &DefinitionError{Kind: DefinitionUnusedPermissionReviewObservations}
	}
	return nil
}

// anyClassifierNeedsEvidence reports whether any classifier in set has an
// EvidenceToolPolicy-enabled Definition (hustle.Definition.EvidenceToolPolicy's
// second, enabled return value). Under gate.NewPermissionClassifierSet's own
// current descriptor validation (pkg/gate/reviewer.go requires
// EvidenceToolPolicyRevision != "" for every registered classifier), a
// non-empty set today always needs evidence — this check is written
// per-classifier anyway, rather than "len(set.Classifiers()) > 0", so it
// stays correct without change if that upstream invariant is ever relaxed.
func anyClassifierNeedsEvidence(set gate.PermissionClassifierSet) bool {
	for _, classifier := range set.Classifiers() {
		if _, enabled := classifier.Definition().EvidenceToolPolicy(); enabled {
			return true
		}
	}
	return false
}

// lifecyclePermissionReviewBreakerLimits converts the rig-level limits shape
// into sessionruntime's exported Lifecycle-option shape.
func lifecyclePermissionReviewBreakerLimits(limits PermissionReviewLimits) sessionruntime.PermissionReviewBreakerLimits {
	return sessionruntime.PermissionReviewBreakerLimits{
		MaxConsecutiveNeedsHuman: limits.MaxConsecutiveNeedsHuman,
		MaxInvalidOrFailed:       limits.MaxInvalidOrFailed,
		MaxIdenticalSubjects:     limits.MaxIdenticalSubjects,
		MaxStaleResponses:        limits.MaxStaleResponses,
		InterruptOnTrip:          limits.InterruptOnTrip,
		Session: sessionruntime.PermissionReviewSessionBreakerLimits{
			MaxConsecutiveNeedsHuman: limits.Session.MaxConsecutiveNeedsHuman,
			MaxInvalidOrFailed:       limits.Session.MaxInvalidOrFailed,
			MaxIdenticalSubjects:     limits.Session.MaxIdenticalSubjects,
			MaxStaleResponses:        limits.Session.MaxStaleResponses,
		},
	}
}

func lifecycleHustleLimits(limits HustleLimits) sessionruntime.HustleLimits {
	return sessionruntime.HustleLimits{
		BlockingConcurrent:   limits.BlockingConcurrent,
		BlockingQueued:       limits.BlockingQueued,
		BackgroundConcurrent: limits.BackgroundConcurrent,
		BackgroundQueued:     limits.BackgroundQueued,
		AuditTimeout:         limits.AuditTimeout,
		FinalizationTimeout:  limits.FinalizationTimeout,
		WorkerDrainTimeout:   limits.WorkerDrainTimeout,
	}
}

func validateHustleRegistration(state *definitionState) error {
	if len(state.hustles) == 0 {
		if state.seen[keyHustleLimits] {
			return &DefinitionError{Kind: DefinitionUnusedHustleLimits}
		}
		return nil
	}
	if !state.seen[keyHustleLimits] {
		return &DefinitionError{Kind: DefinitionMissingHustleLimits}
	}
	seen := make(map[hustle.Name]struct{}, len(state.hustles))
	for _, definition := range state.hustles {
		name := definition.Name()
		if name == "" || definition.PolicyRevision() == "" {
			return &DefinitionError{Kind: DefinitionInvalidHustle, Name: string(name)}
		}
		if _, exists := seen[name]; exists {
			return &DefinitionError{Kind: DefinitionDuplicateHustle, Name: string(name)}
		}
		seen[name] = struct{}{}
	}
	return nil
}

// validateCompactionHustles runs only after loop and hustle registration have
// both been frozen and checked. Task 21 can enforce the definition-time lane and
// model-source contract; Task 25's focused adapter owns concrete XML/output
// validation and deliberately does not widen the generic hustle descriptor here.
func validateCompactionHustles(loops []loop.Definition, definitions []hustle.Definition) error {
	byName := make(map[hustle.Name]hustle.Definition, len(definitions))
	for _, definition := range definitions {
		byName[definition.Name()] = definition
	}
	for _, loopDefinition := range loops {
		policy, configured := loopDefinition.CompactionPolicy()
		if !configured {
			continue
		}
		definition, exists := byName[policy.Hustle]
		if !exists {
			return &DefinitionError{Kind: DefinitionMissingCompactionHustle, Name: string(policy.Hustle)}
		}
		descriptor := definition.Descriptor()
		if err := descriptor.Validate(); err != nil {
			return &DefinitionError{Kind: DefinitionIncompatibleCompactionHustle, Name: string(policy.Hustle), Cause: err}
		}
		if descriptor.Participation != hustle.ParticipationBlocking || descriptor.ModelSource != hustle.ModelSourceCurrentLoop {
			return &DefinitionError{Kind: DefinitionIncompatibleCompactionHustle, Name: string(policy.Hustle)}
		}
	}
	return nil
}
