package rig

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"sort"
	"strings"

	"github.com/looprig/harness/internal/delegationtool"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

const (
	hustleTopologyDigestDomain                 = "looprig/rig/hustle-topology/v1"
	permissionReviewTopologyDigestDomain       = "looprig/rig/permission-review-topology/v1"
	permissionClassifierProjectionDigestDomain = "looprig/rig/permission-classifier-projection/v1"
)

// ConfigFingerprintFields are immutable rig-level behavior inputs that are not part
// of a loop.Definition. Define freezes them for both session creation and restoration.
type ConfigFingerprintFields struct {
	AgentKind     string
	RuntimeSkills bool
	WorkspaceRoot string
	// AdapterID identifies a foreign-agent adapter. Empty means native.
	AdapterID string
	// Posture identifies a foreign agent's non-interactive permission posture.
	Posture string
	// NativePermissionPolicyRev is the digest of native permission configuration.
	NativePermissionPolicyRev string
	// ExternalCapabilityRev is the digest of the identity of external capabilities
	// the composition root attached to the session — tools served by processes
	// Harness does not own, such as MCP servers. Empty means none, which is what
	// keeps it additive for every rig that attaches nothing.
	//
	// The rig neither computes nor interprets it: only the composition root knows
	// what it attached. The canonical producer is github.com/looprig/mcp's
	// mcpharness.Manager.ConfigDigest, taken after the Manager has started.
	ExternalCapabilityRev string

	// Runtime identity fields are secret-free and zero-safe. RuntimeIdentityRev is
	// the opaque digest from loop.BoundDefinition.RuntimeIdentity().Digest(); raw
	// target descriptors never enter durable fingerprints or manifests.

	// WorkspaceTrust is an opaque, secret-free label for the workspace's trust
	// posture (e.g. "trusted"/"untrusted"). Empty means unspecified.
	WorkspaceTrust string
	// PermissionStrictness is the ordered native-permission posture level; higher is
	// stricter. Zero means unknown (drift assessment fails secure). It complements
	// NativePermissionPolicyRev, which is the digest-only identity.
	PermissionStrictness event.StrictnessLevel
	// ConfinementRev is a content digest of the confinement (sandbox) configuration.
	// Empty means none. Harness compares it, never parses it.
	ConfinementRev string
	// ConfinementStrictness is the ordered confinement posture level; higher is
	// stricter. Zero means unknown.
	ConfinementStrictness event.StrictnessLevel
	// AppFields are application-defined, secret-free compatibility fields the
	// composition root attaches. Canonically encoded in sorted key order by the
	// manifest. Nil means none.
	AppFields          map[string]string
	RuntimeProfile     string
	RuntimeCatalogRev  string
	RuntimeIdentityRev string
}

// FingerprintFrom derives the stable, secret-free behavior fingerprint of a bound loop.
func FingerprintFrom(definition loop.BoundDefinition) event.ConfigFingerprint {
	runtime := definition.RuntimeIdentity()
	modelID := definition.Model().Name
	if runtime.SelectionKind == loop.RuntimeSelectionHarnessManaged {
		modelID = ""
	}
	fingerprint := event.ConfigFingerprint{
		ModelID:         modelID,
		SystemPromptRev: hexSHA256(definition.EffectiveSystem()),
		ToolPolicyRev:   toolPolicyRev(definition.Tools()),
	}
	fingerprint.RuntimeProfile = string(runtime.Profile)
	fingerprint.RuntimeCatalogRev = runtime.CatalogDigest
	fingerprint.RuntimeIdentityRev = runtime.Digest()
	return fingerprint
}

func fingerprintWith(definition loop.BoundDefinition, fields ConfigFingerprintFields) event.ConfigFingerprint {
	fingerprint := FingerprintFrom(definition)
	fingerprint.AgentKind = fields.AgentKind
	fingerprint.RuntimeSkills = fields.RuntimeSkills
	fingerprint.WorkspaceRoot = fields.WorkspaceRoot
	fingerprint.AgentAdapter = fields.AdapterID
	fingerprint.PermissionPosture = fields.Posture
	fingerprint.NativePermissionPolicyRev = fields.NativePermissionPolicyRev
	fingerprint.ExternalCapabilityRev = fields.ExternalCapabilityRev
	return fingerprint
}

func fingerprintWithTopology(definition loop.BoundDefinition, fields ConfigFingerprintFields, definitions []loop.Definition, primers []string, active string) event.ConfigFingerprint {
	fingerprint := fingerprintWith(definition, fields)
	fingerprint.TopologyRev = topologyRevision(definitions, primers, active)
	return fingerprint
}

func fingerprintWithTopologyAndHustles(definition loop.BoundDefinition, fields ConfigFingerprintFields, definitions []loop.Definition, primers []string, active string, hustles []hustle.Definition, limits HustleLimits) event.ConfigFingerprint {
	fingerprint := fingerprintWith(definition, fields)
	fingerprint.TopologyRev = topologyRevisionWithHustles(definitions, primers, active, hustles, limits)
	return fingerprint
}

// frozenFingerprint is the rig-time compatibility projection. It depends only on
// immutable definitions and scalar rig fields, so restore can compare it immediately
// after replay without constructing workspace or loop collaborators.
func frozenFingerprint(fields ConfigFingerprintFields, definitions []loop.Definition, primers []string, active string) event.ConfigFingerprint {
	initial := frozenInitial(definitions, active)
	toolNames := frozenToolNames(definitions, active)
	return event.ConfigFingerprint{
		TopologyRev:               topologyRevision(definitions, primers, active),
		AgentKind:                 fields.AgentKind,
		ModelID:                   initial.Model.Name,
		SystemPromptRev:           hexSHA256(initial.EffectiveSystem),
		ToolPolicyRev:             hexSHA256(strings.Join(toolNames, "\n")),
		RuntimeSkills:             fields.RuntimeSkills,
		WorkspaceRoot:             fields.WorkspaceRoot,
		AgentAdapter:              fields.AdapterID,
		PermissionPosture:         fields.Posture,
		NativePermissionPolicyRev: fields.NativePermissionPolicyRev,
		ExternalCapabilityRev:     fields.ExternalCapabilityRev,
		RuntimeProfile:            fields.RuntimeProfile,
		RuntimeCatalogRev:         fields.RuntimeCatalogRev,
		RuntimeIdentityRev:        fields.RuntimeIdentityRev,
	}
}

// frozenInitial resolves the active loop's restore-time InitialFingerprint from the
// immutable definitions alone. The zero value stands in when no definition matches.
func frozenInitial(definitions []loop.Definition, active string) loop.InitialFingerprint {
	for _, definition := range definitions {
		if string(definition.Name()) == active {
			return definition.FingerprintInitial()
		}
	}
	return loop.InitialFingerprint{}
}

// frozenToolNames is the SINGLE source of the restore-time tool-name list shared by
// frozenFingerprint (its ToolPolicyRev) and frozenManifest (its name-only Tools), so
// the two can never drift apart. It reads the active loop's produced tool names,
// appends the declared agent-tool bundle names when the active loop is delegate-capable,
// and returns them sorted. Definition only exposes immutable produced-name metadata here;
// it does not build runtime tools or collaborators. The returned slice is a fresh copy.
func frozenToolNames(definitions []loop.Definition, active string) []string {
	initial := frozenInitial(definitions, active)
	toolNames := append([]string(nil), initial.ToolNames...)
	for _, definition := range definitions {
		if string(definition.Name()) == active && len(definition.Delegates()) > 0 {
			toolNames = append(toolNames, delegationtool.Definition(loop.DelegationManaged, nil).ProducedToolNames()...)
			break
		}
	}
	sort.Strings(toolNames)
	return toolNames
}

// frozenManifest is the rig-time compatibility projection of the richer
// event.ConfigManifest, the manifest counterpart to frozenFingerprint. It depends
// only on immutable definitions and scalar rig fields, so restore can assemble it
// before constructing workspace or loop collaborators, and it draws every shared
// field from the SAME source frozenFingerprint uses — so a manifest and the legacy
// fingerprint of the same session always agree (see TestManifestMatchesFingerprint).
func frozenManifest(fields ConfigFingerprintFields, definitions []loop.Definition, primers []string, active string) event.ConfigManifest {
	initial := frozenInitial(definitions, active)
	toolNames := frozenToolNames(definitions, active)
	tools := make([]event.ToolManifestEntry, len(toolNames))
	for index, name := range toolNames {
		// TODO(phase-1 follow-up): tool schema digests require exposing schemas on
		// both the live and restore paths; empty for now (names-only parity).
		tools[index] = event.ToolManifestEntry{Name: name}
	}
	// Own the AppFields map: the manifest is embedded in SessionStarted and read during
	// restore, so it must not alias the caller's map (a later mutation would change stored
	// fingerprints and can data-race the restore reader). Preserve nil-ness — a nil input
	// stays nil, never an empty map: reflect.DeepEqual(nil, empty) is false and the
	// `omitzero` JSON tag serializes nil as absent but empty as `{}`, so allocating an empty
	// map where nil was expected would break round-trip/DeepEqual equality.
	var appFields map[string]string
	if fields.AppFields != nil {
		appFields = make(map[string]string, len(fields.AppFields))
		for k, v := range fields.AppFields {
			appFields[k] = v
		}
	}
	return event.ConfigManifest{
		SchemaVersion:             event.ManifestSchemaVersion,
		AgentKind:                 fields.AgentKind,
		TopologyRev:               topologyRevision(definitions, primers, active),
		ModelID:                   initial.Model.Name,
		SystemPromptRev:           hexSHA256(initial.EffectiveSystem),
		Tools:                     tools,
		RuntimeSkills:             fields.RuntimeSkills,
		WorkspaceRoot:             fields.WorkspaceRoot,
		WorkspaceTrust:            fields.WorkspaceTrust,
		AgentAdapter:              fields.AdapterID,
		PermissionPosture:         fields.Posture,
		NativePermissionPolicyRev: fields.NativePermissionPolicyRev,
		PermissionStrictness:      fields.PermissionStrictness,
		ConfinementRev:            fields.ConfinementRev,
		ConfinementStrictness:     fields.ConfinementStrictness,
		ExternalCapabilityRev:     fields.ExternalCapabilityRev,
		RuntimeProfile:            fields.RuntimeProfile,
		RuntimeCatalogRev:         fields.RuntimeCatalogRev,
		RuntimeIdentityRev:        fields.RuntimeIdentityRev,
		// HookPolicyRev is assigned by Define from the compiled hook set. It is
		// intentionally absent from ConfigFingerprintFields and the legacy
		// ConfigFingerprint compatibility projection.
		AppFields: appFields,
	}
}

func frozenFingerprintWithHustles(fields ConfigFingerprintFields, definitions []loop.Definition, primers []string, active string, hustles []hustle.Definition, limits HustleLimits) event.ConfigFingerprint {
	fingerprint := frozenFingerprint(fields, definitions, primers, active)
	if len(hustles) > 0 {
		fingerprint.TopologyRev = topologyRevisionWithHustles(definitions, primers, active, hustles, limits)
	}
	return fingerprint
}

// frozenManifestWithHustles is the manifest counterpart to
// frozenFingerprintWithHustles: it assembles the plain frozenManifest and, when
// hustles are present, overrides TopologyRev with the SAME hustle-aware revision the
// fingerprint uses. Stamping this manifest therefore keeps Manifest.TopologyRev
// byte-equal to the fingerprint's, so drift assessment never sees phantom topology
// drift on restore (see TestHustleBoundAndFrozenTopologyFingerprintEquivalent).
func frozenManifestWithHustles(fields ConfigFingerprintFields, definitions []loop.Definition, primers []string, active string, hustles []hustle.Definition, limits HustleLimits) event.ConfigManifest {
	manifest := frozenManifest(fields, definitions, primers, active)
	if len(hustles) > 0 {
		manifest.TopologyRev = topologyRevisionWithHustles(definitions, primers, active, hustles, limits)
	}
	return manifest
}

// permissionReviewFingerprint is the minimal, secret-free projection of
// permission review behavior owned by the rig. Classifier order is preserved.
// Raw prompts, schemas, descriptions, clients, subjects, and workspace state
// never enter this value.
type permissionReviewFingerprint struct {
	reviewPolicyRevision string
	classifiers          []permissionClassifierFingerprint
}

type permissionClassifierFingerprint struct {
	name                          hustle.Name
	revision                      string
	definitionPolicyRevision      string
	outputSchemaName              string
	outputSchemaSHA256            [sha256.Size]byte
	structuredOutputRevision      string
	evidenceToolPolicyRevision    string
	evidenceToolDefinitionsSHA256 [sha256.Size]byte
	evidenceProducedNamesSHA256   [sha256.Size]byte
	evidenceToolLimits            hustle.ToolLoopLimits
	evidenceToolDefinitionCount   int
}

type permissionReviewFingerprintError struct{}

func (*permissionReviewFingerprintError) Error() string {
	return "rig: invalid permission review fingerprint"
}

// permissionReviewFingerprintFrom projects a rig's registered classifier set
// and local decision policy into the minimal, secret-free identity value
// folded into the rig fingerprint. Only policy.Revision feeds identity — the
// design intent (§21: "local decision policy revision", not the full policy
// value) — so two policies that differ only in risk thresholds/authorization
// requirements but share a Revision project identically here; the FULL
// policy value is what actually reaches the runtime via
// rig.WithPermissionReviewPolicy -> sessionruntime.WithLifecyclePermissionReview.
func permissionReviewFingerprintFrom(
	set gate.PermissionClassifierSet,
	policy gate.PermissionReviewPolicy,
) (*permissionReviewFingerprint, error) {
	reviewPolicyRevision := policy.Revision
	if !validPermissionReviewPolicyRevision(reviewPolicyRevision) {
		return nil, &permissionReviewFingerprintError{}
	}
	classifiers := set.Classifiers()
	if len(classifiers) == 0 {
		return nil, &permissionReviewFingerprintError{}
	}
	rows := make([]permissionClassifierFingerprint, len(classifiers))
	for index, classifier := range classifiers {
		if classifier == nil {
			return nil, &permissionReviewFingerprintError{}
		}
		definition := classifier.Definition()
		descriptor := definition.Descriptor()
		if err := descriptor.Validate(); err != nil ||
			descriptor.Name != classifier.Name() ||
			descriptor.PolicyRevision != classifier.Revision() ||
			definition.PolicyRevision() == "" {
			return nil, &permissionReviewFingerprintError{}
		}
		rows[index] = permissionClassifierFingerprint{
			name:                          classifier.Name(),
			revision:                      classifier.Revision(),
			definitionPolicyRevision:      definition.PolicyRevision(),
			outputSchemaName:              descriptor.OutputSchemaName,
			outputSchemaSHA256:            descriptor.OutputSchemaSHA256,
			structuredOutputRevision:      descriptor.StructuredOutputRevision,
			evidenceToolPolicyRevision:    descriptor.EvidenceToolPolicyRevision,
			evidenceToolDefinitionsSHA256: descriptor.EvidenceToolDefinitionsSHA256,
			evidenceProducedNamesSHA256:   descriptor.EvidenceProducedToolNamesSHA256,
			evidenceToolLimits:            descriptor.EvidenceToolLimits,
			evidenceToolDefinitionCount:   descriptor.EvidenceToolDefinitionCount,
		}
	}
	return &permissionReviewFingerprint{
		reviewPolicyRevision: reviewPolicyRevision,
		classifiers:          rows,
	}, nil
}

func frozenFingerprintWithPermissionReview(
	fields ConfigFingerprintFields,
	definitions []loop.Definition,
	primers []string,
	active string,
	hustles []hustle.Definition,
	limits HustleLimits,
	review *permissionReviewFingerprint,
) event.ConfigFingerprint {
	fingerprint := frozenFingerprintWithHustles(
		fields, definitions, primers, active, hustles, limits,
	)
	if review != nil {
		fingerprint.TopologyRev = topologyRevisionWithHustlesAndPermissionReview(
			definitions, primers, active, hustles, limits, review,
		)
	}
	return fingerprint
}

func frozenManifestWithPermissionReview(
	fields ConfigFingerprintFields,
	definitions []loop.Definition,
	primers []string,
	active string,
	hustles []hustle.Definition,
	limits HustleLimits,
	review *permissionReviewFingerprint,
) event.ConfigManifest {
	manifest := frozenManifestWithHustles(
		fields, definitions, primers, active, hustles, limits,
	)
	// PermissionReviewConfigured is the narrow "was ANY permission-review classifier
	// configured at all" signal AssessDrift needs to classify the disabled->enabled
	// restore transition (design §21: never silently resume with a different
	// reviewer). The full classifier/policy identity stays folded into TopologyRev
	// below, for the SEPARATE purpose of detecting drift among already-enabled
	// classifiers.
	//
	// PermissionReviewPolicyRev additionally surfaces the review policy's OWN
	// revision (review.reviewPolicyRevision) as its own manifest field --
	// still ALSO folded into TopologyRev above for backward-compatible digest
	// coverage, but exposed directly here so AssessDrift can Warn when it
	// changes while classifiers stay configured on both sides (the
	// strict-to-default-policy-restore gap an opaque TopologyRev-only
	// comparison cannot classify, since TopologyRev also carries ordinary
	// loop topology and stays Info).
	manifest.PermissionReviewConfigured = review != nil
	if review != nil {
		manifest.PermissionReviewPolicyRev = review.reviewPolicyRevision
		manifest.TopologyRev = topologyRevisionWithHustlesAndPermissionReview(
			definitions, primers, active, hustles, limits, review,
		)
	}
	return manifest
}

func topologyRevisionWithHustlesAndPermissionReview(
	definitions []loop.Definition,
	primers []string,
	active string,
	hustles []hustle.Definition,
	limits HustleLimits,
	review *permissionReviewFingerprint,
) string {
	if review == nil {
		return topologyRevisionWithHustles(
			definitions, primers, active, hustles, limits,
		)
	}
	base := topologyRevision(definitions, primers, active)
	if len(hustles) > 0 {
		base = topologyRevisionWithHustles(
			definitions, primers, active, hustles, limits,
		)
	}
	return hexSHA256Bytes(canonicalPermissionReviewMaterial(base, *review))
}

func canonicalPermissionReviewMaterial(
	baseTopologyRevision string,
	review permissionReviewFingerprint,
) []byte {
	return canonicalPermissionReviewMaterialWithDomains(
		baseTopologyRevision,
		review,
		permissionReviewTopologyDigestDomain,
		permissionClassifierProjectionDigestDomain,
	)
}

func canonicalPermissionReviewMaterialWithDomains(
	baseTopologyRevision string,
	review permissionReviewFingerprint,
	reviewDomain string,
	classifierDomain string,
) []byte {
	material := appendCanonicalString(nil, reviewDomain)
	material = appendCanonicalString(material, baseTopologyRevision)
	material = appendCanonicalString(material, review.reviewPolicyRevision)
	material = binary.BigEndian.AppendUint64(material, uint64(len(review.classifiers)))
	for index, row := range review.classifiers {
		rowMaterial := canonicalPermissionClassifierMaterial(classifierDomain, index, row)
		rowDigest := sha256.Sum256(rowMaterial)
		material = appendCanonicalBytes(material, rowDigest[:])
	}
	return material
}

func canonicalPermissionClassifierMaterial(
	domain string,
	order int,
	row permissionClassifierFingerprint,
) []byte {
	material := appendCanonicalString(nil, domain)
	material = appendCanonicalInt64(material, int64(order))
	material = appendCanonicalString(material, string(row.name))
	material = appendCanonicalString(material, row.revision)
	material = appendCanonicalString(material, row.definitionPolicyRevision)
	material = appendCanonicalString(material, row.outputSchemaName)
	material = appendCanonicalBytes(material, row.outputSchemaSHA256[:])
	material = appendCanonicalString(material, row.structuredOutputRevision)
	material = appendCanonicalString(material, row.evidenceToolPolicyRevision)
	material = appendCanonicalBytes(material, row.evidenceToolDefinitionsSHA256[:])
	material = appendCanonicalBytes(material, row.evidenceProducedNamesSHA256[:])
	material = appendCanonicalInt64(material, int64(row.evidenceToolLimits.MaxRounds))
	material = appendCanonicalInt64(material, int64(row.evidenceToolLimits.MaxCalls))
	material = appendCanonicalInt64(material, int64(row.evidenceToolLimits.MaxCallsPerRound))
	material = appendCanonicalInt64(material, int64(row.evidenceToolLimits.MaxResultBytes))
	material = appendCanonicalInt64(material, int64(row.evidenceToolLimits.MaxEvidenceBytes))
	return appendCanonicalInt64(material, int64(row.evidenceToolDefinitionCount))
}

func topologyRevisionWithHustles(definitions []loop.Definition, primers []string, active string, hustles []hustle.Definition, limits HustleLimits) string {
	copyOfLimits := limits
	return canonicalTopologyRevision(topologyRevisionInput{
		definitions: definitions,
		primers:     primers,
		active:      active,
		hustles:     hustles,
		limits:      &copyOfLimits,
	})
}

type topologyRevisionInput struct {
	definitions []loop.Definition
	primers     []string
	active      string
	hustles     []hustle.Definition
	limits      *HustleLimits
}

func canonicalTopologyRevision(input topologyRevisionInput) string {
	var material strings.Builder
	writeLoopTopology(&material, input.definitions, input.primers, input.active)
	legacyRevision := hexSHA256(material.String())
	if input.limits == nil {
		return legacyRevision
	}
	rows := make([]hustleTopologyRow, len(input.hustles))
	for index, definition := range input.hustles {
		rows[index] = hustleTopologyRow{Name: definition.Name(), PolicyRevision: definition.PolicyRevision()}
	}
	return hexSHA256Bytes(canonicalHustleTopologyMaterial(legacyRevision, rows, *input.limits))
}

type hustleTopologyRow struct {
	Name           hustle.Name
	PolicyRevision string
}

func canonicalHustleTopologyMaterial(legacyRevision string, rows []hustleTopologyRow, limits HustleLimits) []byte {
	ordered := append([]hustleTopologyRow(nil), rows...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Name == ordered[j].Name {
			return ordered[i].PolicyRevision < ordered[j].PolicyRevision
		}
		return ordered[i].Name < ordered[j].Name
	})
	material := appendCanonicalString(nil, hustleTopologyDigestDomain)
	material = appendCanonicalString(material, legacyRevision)
	material = binary.BigEndian.AppendUint64(material, uint64(len(ordered)))
	for _, row := range ordered {
		material = appendCanonicalString(material, string(row.Name))
		material = appendCanonicalString(material, row.PolicyRevision)
	}
	material = appendCanonicalInt64(material, int64(limits.BlockingConcurrent))
	material = appendCanonicalInt64(material, int64(limits.BlockingQueued))
	material = appendCanonicalInt64(material, int64(limits.BackgroundConcurrent))
	material = appendCanonicalInt64(material, int64(limits.BackgroundQueued))
	material = appendCanonicalInt64(material, int64(limits.AuditTimeout))
	material = appendCanonicalInt64(material, int64(limits.FinalizationTimeout))
	return appendCanonicalInt64(material, int64(limits.WorkerDrainTimeout))
}

func appendCanonicalString(material []byte, value string) []byte {
	return appendCanonicalBytes(material, []byte(value))
}

func appendCanonicalBytes(material []byte, value []byte) []byte {
	material = binary.BigEndian.AppendUint64(material, uint64(len(value)))
	return append(material, value...)
}

func appendCanonicalInt64(material []byte, value int64) []byte {
	// #nosec G115 -- canonical encoding preserves the signed value's two's-complement bit pattern.
	return binary.BigEndian.AppendUint64(material, uint64(value))
}

func topologyRevision(definitions []loop.Definition, primers []string, active string) string {
	return canonicalTopologyRevision(topologyRevisionInput{
		definitions: definitions,
		primers:     primers,
		active:      active,
	})
}

func writeLoopTopology(material *strings.Builder, definitions []loop.Definition, primers []string, active string) {
	orderedDefinitions := append([]loop.Definition(nil), definitions...)
	sort.Slice(orderedDefinitions, func(i, j int) bool { return orderedDefinitions[i].Name() < orderedDefinitions[j].Name() })
	for _, candidate := range orderedDefinitions {
		material.WriteString("loop:")
		material.WriteString(string(candidate.Name()))
		material.WriteByte('\n')
		if description := candidate.Description(); description != "" {
			material.WriteString("description:")
			material.WriteString(description)
			material.WriteByte('\n')
		}
		material.WriteString("policy:")
		material.WriteString(candidate.PolicyRevision())
		material.WriteByte('\n')
		delegates := candidate.Delegates()
		sort.Slice(delegates, func(i, j int) bool { return delegates[i] < delegates[j] })
		for _, delegate := range delegates {
			material.WriteString("delegate:")
			material.WriteString(string(delegate))
			material.WriteByte('\n')
		}
	}
	for _, primer := range primers {
		material.WriteString("primer:")
		material.WriteString(primer)
		material.WriteByte('\n')
	}
	material.WriteString("active:")
	material.WriteString(active)
}

func hexSHA256(value string) string {
	return hexSHA256Bytes([]byte(value))
}

func hexSHA256Bytes(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func toolPolicyRev(tools []tool.InvokableTool) string {
	names := make([]string, 0, len(tools))
	for _, candidate := range tools {
		info, err := candidate.Info(context.Background())
		if err != nil || info == nil {
			continue
		}
		names = append(names, info.Name)
	}
	sort.Strings(names)
	return hexSHA256(strings.Join(names, "\n"))
}
