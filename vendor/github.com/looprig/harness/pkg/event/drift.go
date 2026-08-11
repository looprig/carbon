package event

import "sort"

// DriftSeverity is the two-tier classification of one configuration change,
// answering one question: does the change expand what the session can touch?
// Severity is advisory input to application policy, not authority.
type DriftSeverity string

const (
	DriftInfo DriftSeverity = "info"
	DriftWarn DriftSeverity = "warn"
)

// DriftCategory names the manifest field family a change belongs to.
type DriftCategory string

const (
	DriftTool          DriftCategory = "tool"
	DriftModel         DriftCategory = "model"
	DriftPrompt        DriftCategory = "prompt"
	DriftTopology      DriftCategory = "topology"
	DriftExternal      DriftCategory = "external"
	DriftConfinement   DriftCategory = "confinement"
	DriftPermission    DriftCategory = "permission"
	DriftWorkspace     DriftCategory = "workspace"
	DriftTrust         DriftCategory = "trust"
	DriftAgentKind     DriftCategory = "agent_kind"
	DriftAgentName     DriftCategory = "agent_name"
	DriftAdapter       DriftCategory = "adapter"
	DriftRuntimeSkills DriftCategory = "runtime_skills"
	DriftHookPolicy    DriftCategory = "hook_policy"
	DriftRuntime       DriftCategory = "runtime"
	DriftApp           DriftCategory = "app"
)

// DriftChange is one typed configuration change: safe identities only (names,
// digests, levels), never raw configuration.
type DriftChange struct {
	Category DriftCategory `json:"category"`
	Field    string        `json:"field,omitzero"`
	Old      string        `json:"old,omitzero"`
	New      string        `json:"new,omitzero"`
	Severity DriftSeverity `json:"severity"`
}

// DriftAssessment is the typed comparison of the latest adopted baseline
// against the candidate live manifest.
type DriftAssessment struct {
	Changes         []DriftChange `json:"changes,omitzero"`
	BaselineUpgrade bool          `json:"baseline_upgrade,omitzero"`
}

// AnyWarn reports whether any change requires an explicit decision under
// default policy.
func (a DriftAssessment) AnyWarn() bool {
	for _, change := range a.Changes {
		if change.Severity == DriftWarn {
			return true
		}
	}
	return false
}

// AssessDrift compares baseline (the latest adopted manifest, possibly a
// legacy projection) against candidate (the frozen live manifest). Direction-
// sensitive categories classify Info when the posture tightened, Warn when it
// broadened, and Warn when direction is unknowable (an opaque digest-only
// change) — fail secure. The returned Changes are deterministically ordered.
func AssessDrift(baseline, candidate ConfigManifest) DriftAssessment {
	assessment := DriftAssessment{
		BaselineUpgrade: baseline.SchemaVersion < candidate.SchemaVersion,
	}
	add := func(category DriftCategory, field, old, new string, severity DriftSeverity) {
		assessment.Changes = append(assessment.Changes, DriftChange{
			Category: category, Field: field, Old: old, New: new, Severity: severity,
		})
	}
	if baseline.ModelID != candidate.ModelID {
		add(DriftModel, "", baseline.ModelID, candidate.ModelID, DriftInfo)
	}
	if baseline.SystemPromptRev != candidate.SystemPromptRev {
		add(DriftPrompt, "", baseline.SystemPromptRev, candidate.SystemPromptRev, DriftInfo)
	}
	if baseline.TopologyRev != candidate.TopologyRev {
		add(DriftTopology, "", baseline.TopologyRev, candidate.TopologyRev, DriftInfo)
	}
	if baseline.ExternalCapabilityRev != candidate.ExternalCapabilityRev {
		// Opaque digest-only change with no strictness ordinal to compare
		// directionally (unlike DriftConfinement/DriftPermission, which carry
		// one): per this function's own doc, direction-unknowable changes are
		// Warn, fail secure. A consumer's external capability set (e.g.
		// looprig/mcp's configured servers) can change in security-relevant
		// ways this digest cannot express tightened-vs-broadened, so treat
		// every change as a candidate a restore should not silently adopt.
		add(DriftExternal, "", baseline.ExternalCapabilityRev, candidate.ExternalCapabilityRev, DriftWarn)
	}
	assessRuntime(baseline, candidate, add)
	assessTools(baseline, candidate, add)
	assessDirectional(DriftConfinement,
		baseline.ConfinementRev, candidate.ConfinementRev,
		baseline.ConfinementStrictness, candidate.ConfinementStrictness, add)
	assessDirectional(DriftPermission,
		baseline.NativePermissionPolicyRev, candidate.NativePermissionPolicyRev,
		baseline.PermissionStrictness, candidate.PermissionStrictness, add)
	if baseline.PermissionPosture != candidate.PermissionPosture {
		add(DriftPermission, "posture", baseline.PermissionPosture, candidate.PermissionPosture, DriftWarn)
	}
	if baseline.PermissionReviewConfigured != candidate.PermissionReviewConfigured {
		// Widening (no classifiers -> classifiers configured) must never resume
		// silently (design §21: never silently resumes with a different
		// reviewer). Narrowing (classifiers -> none) is fewer automated
		// decisions, strictly more human control, so it stays Info.
		severity := DriftInfo
		if candidate.PermissionReviewConfigured {
			severity = DriftWarn
		}
		add(DriftPermission, "review_configured",
			boolID(baseline.PermissionReviewConfigured), boolID(candidate.PermissionReviewConfigured), severity)
	} else if baseline.PermissionReviewConfigured && candidate.PermissionReviewConfigured &&
		baseline.PermissionReviewPolicyRev != candidate.PermissionReviewPolicyRev {
		// Both sides stay configured, but the review POLICY's own identity
		// changed underneath the still-enabled reviewer (e.g. a strict custom
		// policy replaced by a looser default). This is the directional
		// counterpart of the review_configured Warn above, for the
		// posture-narrows-invisibly gap an opaque TopologyRev-only comparison
		// cannot catch: TopologyRev also folds in ordinary loop topology, so
		// its own drift stays Info (below) even though it happens to also
		// change here. Direction is unknowable from a revision LABEL alone
		// (unlike PermissionStrictness/ConfinementStrictness, a review
		// policy carries no ordered level), so this fails secure exactly
		// like assessDirectional's unknown-direction case: any change warns.
		add(DriftPermission, "review_policy_rev",
			baseline.PermissionReviewPolicyRev, candidate.PermissionReviewPolicyRev, DriftWarn)
	}
	if baseline.WorkspaceRoot != candidate.WorkspaceRoot {
		add(DriftWorkspace, "", baseline.WorkspaceRoot, candidate.WorkspaceRoot, DriftWarn)
	}
	if baseline.WorkspaceTrust != candidate.WorkspaceTrust {
		add(DriftTrust, "", baseline.WorkspaceTrust, candidate.WorkspaceTrust, DriftWarn)
	}
	if baseline.AgentKind != candidate.AgentKind {
		add(DriftAgentKind, "", baseline.AgentKind, candidate.AgentKind, DriftWarn)
	}
	if baseline.AgentAdapter != candidate.AgentAdapter {
		add(DriftAdapter, "", baseline.AgentAdapter, candidate.AgentAdapter, DriftWarn)
	}
	if baseline.RuntimeSkills != candidate.RuntimeSkills {
		add(DriftRuntimeSkills, "", boolID(baseline.RuntimeSkills), boolID(candidate.RuntimeSkills), DriftWarn)
	}
	if baseline.HookPolicyRev != candidate.HookPolicyRev {
		add(DriftHookPolicy, "", baseline.HookPolicyRev, candidate.HookPolicyRev, DriftWarn)
	}
	assessAppFields(baseline.AppFields, candidate.AppFields, add)

	sort.Slice(assessment.Changes, func(i, j int) bool {
		a, b := assessment.Changes[i], assessment.Changes[j]
		if a.Category != b.Category {
			return a.Category < b.Category
		}
		if a.Field != b.Field {
			return a.Field < b.Field
		}
		if a.Old != b.Old {
			return a.Old < b.Old
		}
		return a.New < b.New
	})
	return assessment
}

func assessRuntime(baseline, candidate ConfigManifest, add func(DriftCategory, string, string, string, DriftSeverity)) {
	if baseline.RuntimeProfile != candidate.RuntimeProfile {
		add(DriftRuntime, "profile", baseline.RuntimeProfile, candidate.RuntimeProfile, DriftWarn)
	}
	if baseline.RuntimeCatalogRev != candidate.RuntimeCatalogRev {
		add(DriftRuntime, "catalog_rev", baseline.RuntimeCatalogRev, candidate.RuntimeCatalogRev, DriftWarn)
	}
	if baseline.RuntimeIdentityRev != candidate.RuntimeIdentityRev {
		add(DriftRuntime, "identity_rev", baseline.RuntimeIdentityRev, candidate.RuntimeIdentityRev, DriftWarn)
	}
}

func assessTools(baseline, candidate ConfigManifest, add func(DriftCategory, string, string, string, DriftSeverity)) {
	if baseline.SchemaVersion == 0 {
		candidateNamesRev := candidate.ToolNamesRev()
		if baseline.legacyToolPolicyRev != candidateNamesRev {
			add(DriftTool, "", baseline.legacyToolPolicyRev, candidateNamesRev, DriftInfo)
		}
		return
	}
	old := make(map[string]ToolManifestEntry, len(baseline.Tools))
	for _, entry := range baseline.Tools {
		old[entry.Name] = entry
	}
	for _, entry := range candidate.Tools {
		prior, existed := old[entry.Name]
		switch {
		case !existed:
			add(DriftTool, entry.Name, "", entry.InputSchemaRev, DriftInfo)
		case prior != entry:
			add(DriftTool, entry.Name, prior.InputSchemaRev, entry.InputSchemaRev, DriftInfo)
		}
		delete(old, entry.Name)
	}
	for name, prior := range old {
		add(DriftTool, name, prior.InputSchemaRev, "", DriftInfo)
	}
}

func assessDirectional(category DriftCategory, oldRev, newRev string, oldLevel, newLevel StrictnessLevel, add func(DriftCategory, string, string, string, DriftSeverity)) {
	if oldRev == newRev && oldLevel == newLevel {
		return
	}
	severity := DriftWarn // unknown direction fails secure
	if oldLevel != 0 && newLevel != 0 && newLevel >= oldLevel {
		severity = DriftInfo
	}
	add(category, "", oldRev, newRev, severity)
}

func assessAppFields(baseline, candidate map[string]string, add func(DriftCategory, string, string, string, DriftSeverity)) {
	keys := make([]string, 0, len(baseline)+len(candidate))
	seen := make(map[string]struct{}, len(baseline)+len(candidate))
	for key := range candidate {
		if _, ok := seen[key]; !ok {
			seen[key] = struct{}{}
			keys = append(keys, key)
		}
	}
	for key := range baseline {
		if _, ok := seen[key]; !ok {
			seen[key] = struct{}{}
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		newVal, inNew := candidate[key]
		oldVal, inOld := baseline[key]
		if inNew && (!inOld || oldVal != newVal) {
			add(DriftApp, key, oldVal, newVal, DriftInfo)
		} else if inOld && !inNew {
			add(DriftApp, key, oldVal, "", DriftInfo)
		}
	}
}

func boolID(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
