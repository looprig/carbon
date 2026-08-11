package event

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"sort"
	"strings"
)

// ManifestSchemaVersion is the current ConfigManifest schema version. Bumping it
// changes the canonical encoding, which changes every fingerprint — restore
// therefore never treats raw fingerprint inequality across schema versions as
// drift (see AssessDrift) and records a one-time baseline upgrade instead.
//
// v2 adds both PermissionReviewPolicyRev (the review-policy-identity-drift-
// while-enabled fix) and HookPolicyRev (the operation-hooks feature) to the
// canonical encoding. Neither shipped independently — both landed in the
// same merge before v2 was ever persisted anywhere — so v2 is defined as
// carrying both fields together, not as two separate single-field bumps.
// v3 adds the explicit runtime identity fields. v1 and v2 canonical encodings
// remain immutable so previously persisted fingerprints remain verifiable.
const ManifestSchemaVersion uint32 = 3

// manifestEncodingDomain separates manifest digests from every other SHA-256
// in the system. It is part of the durable contract; never change it without a
// schema bump.
const manifestEncodingDomain = "looprig/config-manifest/v3"

const manifestEncodingDomainV2 = "looprig/config-manifest/v2"

// manifestEncodingDomainV1 preserves verification of fingerprints already
// persisted before HookPolicyRev extended the canonical schema.
const manifestEncodingDomainV1 = "looprig/config-manifest/v1"

// ConfigEpoch orders the configurations explicitly adopted within one Session.
// SessionStarted is epoch 1; each ConfigurationAdopted increments it.
type ConfigEpoch uint64

// StrictnessLevel is an ordered security posture supplied by the composition
// root: higher is stricter. Zero means "unknown" — the posture exists only as
// an opaque digest, so a change cannot be direction-classified and drift
// assessment fails secure (Warn). Harness compares levels; it never computes
// them.
type StrictnessLevel uint8

// ToolManifestEntry is one model-facing tool's stable identity: its name plus
// content digests of its input and output schemas. Digests, never schemas —
// the manifest carries identity, not definitions.
type ToolManifestEntry struct {
	Name            string `json:"name"`
	InputSchemaRev  string `json:"input_schema_rev,omitzero"`
	OutputSchemaRev string `json:"output_schema_rev,omitzero"`
}

// ConfigManifest is the canonical, bounded, secret-free description of the
// behavior a Session runs under. It is a strict superset of the legacy
// ConfigFingerprint (see ManifestFromLegacy) and the input to both the
// SHA-256 fingerprint and typed drift assessment. Credentials, raw prompts,
// tool schemas, and environment contents never enter a manifest.
//
// SchemaVersion 0 marks a legacy projection built by ManifestFromLegacy; it is
// never persisted and never fingerprinted.
type ConfigManifest struct {
	SchemaVersion   uint32              `json:"schema_version"`
	AgentKind       string              `json:"agent_kind,omitzero"`
	TopologyRev     string              `json:"topology_rev,omitzero"`
	ModelID         string              `json:"model_id,omitzero"`
	SystemPromptRev string              `json:"system_prompt_rev,omitzero"`
	Tools           []ToolManifestEntry `json:"tools,omitzero"`
	RuntimeSkills   bool                `json:"runtime_skills,omitzero"`
	WorkspaceRoot   string              `json:"workspace_root,omitzero"`
	WorkspaceTrust  string              `json:"workspace_trust,omitzero"`
	AgentAdapter    string              `json:"agent_adapter,omitzero"`
	// PermissionPosture is the foreign-agent posture string; native sessions
	// use NativePermissionPolicyRev + PermissionStrictness instead.
	PermissionPosture         string          `json:"permission_posture,omitzero"`
	NativePermissionPolicyRev string          `json:"native_permission_policy_rev,omitzero"`
	PermissionStrictness      StrictnessLevel `json:"permission_strictness,omitzero"`
	// PermissionReviewConfigured reports only whether ANY permission-review
	// classifier was registered for this session — never the classifier or
	// policy identity itself (the classifier SET's identity stays folded into
	// TopologyRev, for detecting drift among already-enabled classifiers,
	// classified Info like the rest of TopologyRev). It exists so AssessDrift
	// has a directionally-comparable signal for the one transition an opaque
	// digest can't distinguish: classifiers going from unconfigured to
	// configured across a restore, which must never resume silently (design
	// §21).
	PermissionReviewConfigured bool `json:"permission_review_configured,omitzero"`
	// PermissionReviewPolicyRev is the review POLICY's own Revision label
	// (gate.PermissionReviewPolicy.Revision — e.g. "strict-policy-v1"),
	// carried SEPARATELY from TopologyRev (which also folds this same value
	// in, alongside classifier identity, for backward-compatible digest
	// coverage). Without this dedicated field, a policy-identity change while
	// classifiers stay configured on both sides is visible only as an opaque
	// TopologyRev digest difference — classified DriftInfo like any other
	// topology change and silently auto-accepted by DefaultPolicyDecider, so
	// a session opened under a strict review policy (e.g. MaximumAutoRisk:
	// low) could restore under a looser one (e.g. default MaximumAutoRisk:
	// high) with no warning at all. AssessDrift compares this field
	// DIRECTLY (Warn on any change) whenever PermissionReviewConfigured is
	// true on both sides — the same kind of directional fix already applied
	// to PermissionReviewConfigured itself for the disabled->enabled
	// transition, extended to cover an already-enabled reviewer's policy
	// identity changing underneath it.
	PermissionReviewPolicyRev string          `json:"permission_review_policy_rev,omitzero"`
	ConfinementRev            string          `json:"confinement_rev,omitzero"`
	ConfinementStrictness     StrictnessLevel `json:"confinement_strictness,omitzero"`
	ExternalCapabilityRev     string          `json:"external_capability_rev,omitzero"`
	HookPolicyRev             string          `json:"hook_policy_rev,omitzero"`
	RuntimeProfile            string          `json:"runtime_profile,omitzero"`
	RuntimeCatalogRev         string          `json:"runtime_catalog_rev,omitzero"`
	// RuntimeIdentityRev is the opaque hex digest of the selected runtime tuple;
	// raw provider, model, effort, alias, endpoint, and credential values never
	// enter the durable manifest.
	RuntimeIdentityRev string `json:"runtime_identity_rev,omitzero"`
	// AppFields are application-defined, secret-free compatibility fields.
	// Canonically encoded in sorted key order.
	AppFields map[string]string `json:"app_fields,omitzero"`
	// legacyToolPolicyRev carries a legacy baseline's names-only tool digest
	// through ManifestFromLegacy. Never persisted, never canonical.
	legacyToolPolicyRev string `json:"-"`
}

// Fingerprint is SHA-256 over the canonical encoding: explicit domain,
// schema version, stable field order, length-delimited values, deterministic
// collection ordering. Equal fingerprints of the same SchemaVersion identify
// behaviorally identical configurations.
func (m ConfigManifest) Fingerprint() string {
	return hexSHA256EventBytes(m.canonical())
}

func (m ConfigManifest) canonical() []byte {
	domain := manifestEncodingDomain
	if m.SchemaVersion == 1 {
		domain = manifestEncodingDomainV1
	} else if m.SchemaVersion == 2 {
		domain = manifestEncodingDomainV2
	}
	material := appendManifestString(nil, domain)
	material = binary.BigEndian.AppendUint64(material, uint64(m.SchemaVersion))
	material = appendManifestString(material, m.AgentKind)
	material = appendManifestString(material, m.TopologyRev)
	material = appendManifestString(material, m.ModelID)
	material = appendManifestString(material, m.SystemPromptRev)
	tools := append([]ToolManifestEntry(nil), m.Tools...)
	sort.SliceStable(tools, func(i, j int) bool {
		if tools[i].Name != tools[j].Name {
			return tools[i].Name < tools[j].Name
		}
		if tools[i].InputSchemaRev != tools[j].InputSchemaRev {
			return tools[i].InputSchemaRev < tools[j].InputSchemaRev
		}
		return tools[i].OutputSchemaRev < tools[j].OutputSchemaRev
	})
	material = binary.BigEndian.AppendUint64(material, uint64(len(tools)))
	for _, entry := range tools {
		material = appendManifestString(material, entry.Name)
		material = appendManifestString(material, entry.InputSchemaRev)
		material = appendManifestString(material, entry.OutputSchemaRev)
	}
	flag := uint64(0)
	if m.RuntimeSkills {
		flag = 1
	}
	material = binary.BigEndian.AppendUint64(material, flag)
	material = appendManifestString(material, m.WorkspaceRoot)
	material = appendManifestString(material, m.WorkspaceTrust)
	material = appendManifestString(material, m.AgentAdapter)
	material = appendManifestString(material, m.PermissionPosture)
	material = appendManifestString(material, m.NativePermissionPolicyRev)
	material = binary.BigEndian.AppendUint64(material, uint64(m.PermissionStrictness))
	if m.SchemaVersion != 1 {
		// PermissionReviewConfigured/PermissionReviewPolicyRev and HookPolicyRev
		// all extend the canonical encoding beyond what v1 ever genuinely
		// carried: v1's own frozen historical fixture (TestManifestV1Fingerprint
		// Compatibility, TestConfigurationAdoptedV1ReplayFixture) predates both —
		// permission-review and operation-hooks are two independent features
		// that each landed on their own branch before this merge, and neither
		// branch's "v1" ever included the other's field. Gating both together
		// keeps v1 genuinely immutable regardless of which fields a future v3+
		// schema adds.
		reviewConfiguredFlag := uint64(0)
		if m.PermissionReviewConfigured {
			reviewConfiguredFlag = 1
		}
		material = binary.BigEndian.AppendUint64(material, reviewConfiguredFlag)
		material = appendManifestString(material, m.PermissionReviewPolicyRev)
	}
	material = appendManifestString(material, m.ConfinementRev)
	material = binary.BigEndian.AppendUint64(material, uint64(m.ConfinementStrictness))
	material = appendManifestString(material, m.ExternalCapabilityRev)
	if m.SchemaVersion != 1 {
		material = appendManifestString(material, m.HookPolicyRev)
	}
	if m.SchemaVersion >= ManifestSchemaVersion {
		material = appendManifestString(material, m.RuntimeProfile)
		material = appendManifestString(material, m.RuntimeCatalogRev)
		material = appendManifestString(material, m.RuntimeIdentityRev)
	}
	keys := make([]string, 0, len(m.AppFields))
	for key := range m.AppFields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	material = binary.BigEndian.AppendUint64(material, uint64(len(keys)))
	for _, key := range keys {
		material = appendManifestString(material, key)
		material = appendManifestString(material, m.AppFields[key])
	}
	return material
}

// ManifestFromLegacy projects a legacy ConfigFingerprint into a partial
// manifest for drift assessment against a live candidate. SchemaVersion 0
// marks the projection: it is never persisted, never fingerprinted, and
// limits assessment to the fields the legacy fingerprint can distinguish
// (tool identity is names-only; permission and confinement are digest-only,
// so their changes classify Warn).
func ManifestFromLegacy(f ConfigFingerprint) ConfigManifest {
	return ConfigManifest{
		SchemaVersion:             0,
		AgentKind:                 f.AgentKind,
		TopologyRev:               f.TopologyRev,
		ModelID:                   f.ModelID,
		SystemPromptRev:           f.SystemPromptRev,
		legacyToolPolicyRev:       f.ToolPolicyRev,
		RuntimeSkills:             f.RuntimeSkills,
		WorkspaceRoot:             f.WorkspaceRoot,
		AgentAdapter:              f.AgentAdapter,
		PermissionPosture:         f.PermissionPosture,
		NativePermissionPolicyRev: f.NativePermissionPolicyRev,
		ExternalCapabilityRev:     f.ExternalCapabilityRev,
		RuntimeProfile:            f.RuntimeProfile,
		RuntimeCatalogRev:         f.RuntimeCatalogRev,
		RuntimeIdentityRev:        f.RuntimeIdentityRev,
	}
}

// ToolNamesRev reproduces the legacy names-only tool digest from the manifest's
// tool entries, so a full manifest can be compared against a legacy baseline.
// It MUST stay byte-identical to rig's toolPolicyRev (sorted names joined by \n).
func (m ConfigManifest) ToolNamesRev() string {
	names := make([]string, 0, len(m.Tools))
	for _, entry := range m.Tools {
		names = append(names, entry.Name)
	}
	sort.Strings(names)
	return hexSHA256Event(strings.Join(names, "\n"))
}

func appendManifestString(material []byte, value string) []byte {
	material = binary.BigEndian.AppendUint64(material, uint64(len(value)))
	return append(material, value...)
}

func hexSHA256Event(value string) string {
	return hexSHA256EventBytes([]byte(value))
}

func hexSHA256EventBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}
