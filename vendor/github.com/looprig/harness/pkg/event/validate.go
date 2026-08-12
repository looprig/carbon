package event

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/tool"
	model "github.com/looprig/inference/model"
)

// EventName is the concrete event type name an InvalidEventError points at.
type EventName string

// FieldName is the identity/body field an InvalidEventError points at.
type FieldName string

// Rule is the human-readable invariant an InvalidEventError records, so the
// caller learns WHY the field is wrong (required vs must-be-zero), not just which.
type Rule string

const (
	// RuleRequired: the field must be non-zero for this event.
	RuleRequired Rule = "must be set"
	// RuleMustBeZero: the field must be zero for this event's scope.
	RuleMustBeZero Rule = "must be zero"
	// RuleUnknownType: the event's concrete type is not in the sealed union, so it
	// fails fail-secure (the journal/restore caller must reject it rather than guess
	// an identity contract for it).
	RuleUnknownType Rule = "is not a known event type"
	// RuleInvalid: the field contains a value outside its closed domain.
	RuleInvalid Rule = "is invalid"
)

// Identity / body field names, named so an InvalidEventError reads precisely.
const (
	FieldEventID            FieldName = "EventID"
	FieldSessionID          FieldName = "SessionID"
	FieldLoopID             FieldName = "LoopID"
	FieldTurnID             FieldName = "TurnID"
	FieldStepID             FieldName = "StepID"
	FieldToolExecutionID    FieldName = "ToolExecutionID"
	FieldConsistency        FieldName = "Consistency"
	FieldTrigger            FieldName = "Trigger"
	FieldCause              FieldName = "Cause"
	FieldCommandID          FieldName = "CommandID"
	FieldRequestID          FieldName = "RequestID"
	FieldActiveLoopID       FieldName = "ActiveLoopID"
	FieldTargetLoopID       FieldName = "TargetLoopID"
	FieldCategory           FieldName = "Category"
	FieldModel              FieldName = "Model"
	FieldModelKey           FieldName = "ModelKey"
	FieldContextLimits      FieldName = "ContextLimits"
	FieldEffort             FieldName = "Effort"
	FieldUsage              FieldName = "Usage"
	FieldMessages           FieldName = "Messages"
	FieldVisibility         FieldName = "Visibility"
	FieldDefinition         FieldName = "Definition"
	FieldRunID              FieldName = "RunID"
	FieldRuntime            FieldName = "Runtime"
	FieldAgentRuntime       FieldName = "AgentRuntime"
	FieldACPSessionID       FieldName = "ACPSessionID"
	FieldDuration           FieldName = "Duration"
	FieldStage              FieldName = "Stage"
	FieldReasonCode         FieldName = "ReasonCode"
	FieldAttemptID          FieldName = "AttemptID"
	FieldReason             FieldName = "Reason"
	FieldRejectReason       FieldName = "RejectReason"
	FieldWaiterCommandIDs   FieldName = "WaiterCommandIDs"
	FieldSummary            FieldName = "Summary"
	FieldRetained           FieldName = "Retained"
	FieldPostContext        FieldName = "PostContext"
	FieldCommittedEventID   FieldName = "CommittedEventID"
	FieldSource             FieldName = "Source"
	FieldActor              FieldName = "Actor"
	FieldGeneration         FieldName = "Generation"
	FieldTools              FieldName = "Tools"
	FieldProcess            FieldName = "Process"
	FieldGateID             FieldName = "GateID"
	FieldClassifier         FieldName = "Classifier"
	FieldClassifierRevision FieldName = "ClassifierRevision"
	FieldStatus             FieldName = "Status"
	FieldRisk               FieldName = "Risk"
	FieldAuthorization      FieldName = "Authorization"
	FieldCategories         FieldName = "Categories"
	FieldAutoApproved       FieldName = "AutoApproved"
	// FieldIntegrationName names IntegrationStatus.Name. It is not spelled
	// "FieldName": that identifier is this file's FieldName TYPE.
	FieldIntegrationName    FieldName = "Name"
	FieldState              FieldName = "State"
	FieldDetail             FieldName = "Detail"
	FieldEpoch              FieldName = "Epoch"
	FieldAdoptedFingerprint FieldName = "AdoptedFingerprint"
	FieldManifest           FieldName = "Manifest"
	FieldDrift              FieldName = "Drift"
	FieldMessage            FieldName = "Message"
	FieldWorkflowName       FieldName = "WorkflowName"
	FieldWorkflowVersion    FieldName = "WorkflowVersion"
	FieldActivityKind       FieldName = "ActivityKind"
	FieldOccurredAt         FieldName = "OccurredAt"
	FieldVertexID           FieldName = "VertexID"
	FieldVertexLabel        FieldName = "VertexLabel"
	FieldProgress           FieldName = "Progress"
	// FieldType names the whole event (not one coordinate) on the fail-secure
	// unknown-type path, paired with RuleUnknownType.
	FieldType FieldName = "Type"
)

// InvalidEventError reports that an event violates the ID fill matrix: Field names
// the offending identity/body field and Rule says whether it was required or had to
// be zero. It is a typed package-API error so a journal/test can errors.As it to
// inspect the exact violation rather than parse a string.
type InvalidEventError struct {
	Event EventName
	Field FieldName
	Rule  Rule
}

func (e *InvalidEventError) Error() string {
	return "event: invalid " + string(e.Event) + ": " + string(e.Field) + " " + string(e.Rule)
}

// idProfile is one event type's STATIC identity contract from the fill matrix:
// which Coordinates fields must be set, which must be zero, whether a
// ToolExecutionID is required (the tool-interaction and permission-review
// events), and whether TurnID is OPTIONAL (only InputCancelled, whose TurnID is
// the returned turn for an abnormal return but zero for a pure client retract).
// A field that is neither required nor forbidden is unconstrained. It holds no
// per-instance value, so every event type's profile is a plain constant;
// ValidateEvent reads the runtime ToolExecutionID off the concrete event body
// when requireTool is set.
type idProfile struct {
	requireSession bool
	requireLoop    bool
	requireTurn    bool
	requireStep    bool

	forbidLoop bool
	forbidTurn bool
	forbidStep bool

	requireTool bool // ToolExecutionID must be non-zero
}

// ValidateEvent checks ev against the ID fill matrix and returns a typed
// *InvalidEventError on the first violation, nil when ev satisfies every invariant.
// EventID is required on every event; the per-type profile then pins the required
// and must-be-zero coordinates (and ToolExecutionID for tool-interaction and
// permission-review events).
// Fail-secure: an event whose concrete type is not in the sealed union is invalid
// with FieldType/RuleUnknownType — the caller learns the type is unknown, not that
// some coordinate is missing.
func ValidateEvent(ev Event) error {
	if err := validateEventIdentity(ev); err != nil {
		return err
	}
	return validateEventBody(ev)
}

func validateEventIdentity(ev Event) error {
	nameStr, prof, ok := classify(ev)
	name := EventName(nameStr)
	if !ok {
		return &InvalidEventError{Event: name, Field: FieldType, Rule: RuleUnknownType}
	}
	h := ev.EventHeader()
	if !h.EventVisibility.Valid() {
		return &InvalidEventError{Event: name, Field: FieldVisibility, Rule: RuleInvalid}
	}
	if h.EventID.IsZero() {
		return &InvalidEventError{Event: name, Field: FieldEventID, Rule: RuleRequired}
	}
	return checkProfile(name, h.Coordinates, toolExecutionID(ev), prof)
}

func validateEventBody(ev Event) error {
	switch e := ev.(type) {
	case SessionStarted:
		if !validConfigManifestSchema(e.Manifest, true) {
			return &InvalidEventError{Event: "SessionStarted", Field: FieldManifest, Rule: RuleInvalid}
		}
	case WorkspaceCheckpointed:
		if e.Consistency != SnapshotQuiescent && e.Consistency != SnapshotFuzzy {
			return &InvalidEventError{Event: "WorkspaceCheckpointed", Field: FieldConsistency, Rule: RuleInvalid}
		}
		if e.Trigger < SnapshotTriggerManual || e.Trigger > SnapshotTriggerSeed {
			return &InvalidEventError{Event: "WorkspaceCheckpointed", Field: FieldTrigger, Rule: RuleInvalid}
		}
		if !validCheckpointCause(e.SessionID, e.Trigger, e.Cause) {
			return &InvalidEventError{Event: "WorkspaceCheckpointed", Field: FieldCause, Rule: RuleInvalid}
		}
	case ActiveLoopChanged:
		if e.ActiveLoopID.IsZero() {
			return &InvalidEventError{Event: "ActiveLoopChanged", Field: FieldActiveLoopID, Rule: RuleRequired}
		}
	case LoopRestoreTombstoned:
		if !ValidLoopRestoreTombstoneCategory(e.Category) {
			return &InvalidEventError{Event: "LoopRestoreTombstoned", Field: FieldCategory, Rule: RuleInvalid}
		}
	case DelegateRequestAccepted:
		if e.Cause.CommandID.IsZero() {
			return &InvalidEventError{Event: "DelegateRequestAccepted", Field: FieldCommandID, Rule: RuleRequired}
		}
	case DelegateDeliveryStateChanged:
		if e.RequestID.IsZero() {
			return &InvalidEventError{Event: "DelegateDeliveryStateChanged", Field: FieldRequestID, Rule: RuleRequired}
		}
		if e.TargetLoopID.IsZero() {
			return &InvalidEventError{Event: "DelegateDeliveryStateChanged", Field: FieldTargetLoopID, Rule: RuleRequired}
		}
		if !e.State.Valid() {
			return &InvalidEventError{Event: "DelegateDeliveryStateChanged", Field: FieldState, Rule: RuleInvalid}
		}
	case WorkflowActivity:
		return validateWorkflowActivity(e)
	case LoopInferenceChanged:
		return validateModelRuntime("LoopInferenceChanged", e.Runtime)
	case LoopModeChanged:
		return validateModelRuntime("LoopModeChanged", e.Runtime)
	case LoopExternalToolsetChanged:
		return validateExternalToolset(e)
	case IntegrationStatus:
		return validateIntegrationStatus(e)
	case LoopStarted:
		if e.Runtime == (ModelRuntime{}) {
			if e.AgentRuntime == nil {
				return nil
			}
			if err := validateAgentRuntime(*e.AgentRuntime); err != nil {
				return err
			}
			if e.AgentRuntime.Source != "native" || e.AgentRuntime.SelectionKind != "harness-managed" {
				return &InvalidEventError{Event: "LoopStarted", Field: FieldAgentRuntime, Rule: RuleInvalid}
			}
			return nil
		}
		if err := validateModelRuntime("LoopStarted", e.Runtime); err != nil {
			return err
		}
		if e.AgentRuntime != nil {
			if err := validateAgentRuntime(*e.AgentRuntime); err != nil {
				return err
			}
			if e.AgentRuntime.Source == "native" && e.AgentRuntime.SelectionKind == "harness-managed" {
				return &InvalidEventError{Event: "LoopStarted", Field: FieldAgentRuntime, Rule: RuleInvalid}
			}
		}
	case LoopAgentSessionBound:
		if !validAgentRuntimeIdentifier(e.ACPSessionID, false) || e.ACPSessionID == "" {
			return &InvalidEventError{Event: "LoopAgentSessionBound", Field: FieldACPSessionID, Rule: RuleInvalid}
		}
	case ContextMeasured:
		if e.Visibility() != Public {
			return &InvalidEventError{Event: "ContextMeasured", Field: FieldVisibility, Rule: RuleInvalid}
		}
		return e.Measurement.Validate()
	case ContextPressure:
		if e.Visibility() != Public {
			return &InvalidEventError{Event: "ContextPressure", Field: FieldVisibility, Rule: RuleInvalid}
		}
		return validateContextPressure(e)
	case CompactionStarted:
		return validateCompactionStarted(e)
	case CompactionCommitted:
		return validateCompactionCommitted(e)
	case CompactionRejected:
		return validateCompactionRejected(e)
	case CompactWaiterResolved:
		return validateCompactWaiterResolved(e)
	case CompactWaiterRejected:
		return validateCompactWaiterRejected(e)
	case HustleStarted:
		if e.Visibility() != Internal {
			return invalidHustle("HustleStarted", FieldVisibility)
		}
		if err := validateHustleRun("HustleStarted", e.Run); err != nil {
			return err
		}
		if !zeroModelRuntime(e.Run.Runtime) {
			return invalidHustle("HustleStarted", FieldRuntime)
		}
	case HustleCompleted:
		if e.Visibility() != Internal {
			return invalidHustle("HustleCompleted", FieldVisibility)
		}
		if err := validateHustleRun("HustleCompleted", e.Run); err != nil {
			return err
		}
		if e.Duration < 0 {
			return invalidHustle("HustleCompleted", FieldDuration)
		}
		if err := validateModelRuntime("HustleCompleted", e.Run.Runtime); err != nil {
			return invalidHustle("HustleCompleted", FieldRuntime)
		}
		if err := validateOptionalUsage("HustleCompleted", e.Usage); err != nil {
			return err
		}
	case HustleFailed:
		return validateHustleFailed(e)
	case PermissionReviewStarted:
		return validatePermissionReviewStarted(e)
	case PermissionReviewCompleted:
		return validatePermissionReviewCompleted(e)
	case ProcessStarted:
		return validateProcessLifecycleEvent("ProcessStarted", e.Header, e.Process, tool.ProcessLifecycleStarted)
	case ProcessBackgrounded:
		return validateProcessLifecycleEvent("ProcessBackgrounded", e.Header, e.Process, tool.ProcessLifecycleBackgrounded)
	case ProcessCompleted:
		return validateProcessLifecycleEvent("ProcessCompleted", e.Header, e.Process, tool.ProcessLifecycleCompleted)
	case ProcessStopRequested:
		return validateProcessLifecycleEvent("ProcessStopRequested", e.Header, e.Process, tool.ProcessLifecycleStopRequested)
	case ProcessLost:
		return validateProcessLifecycleEvent("ProcessLost", e.Header, e.Process, tool.ProcessLifecycleLost)
	case StepDone:
		return validateStepDoneMessages(e.Messages)
	case TurnDone:
		if err := e.Usage.Validate(); err != nil {
			return &InvalidEventError{Event: "TurnDone", Field: FieldUsage, Rule: RuleInvalid}
		}
	case ConfigurationAdopted:
		return validateConfigurationAdopted(e)
	}
	return nil
}

func validatePermissionReviewStarted(e PermissionReviewStarted) error {
	return validatePermissionReviewMetadata(
		"PermissionReviewStarted",
		e.Visibility(),
		e.GateID,
		e.Classifier,
		e.ClassifierRevision,
	)
}

func validatePermissionReviewCompleted(e PermissionReviewCompleted) error {
	const name EventName = "PermissionReviewCompleted"
	if err := validatePermissionReviewMetadata(
		name,
		e.Visibility(),
		e.GateID,
		e.Classifier,
		e.ClassifierRevision,
	); err != nil {
		return err
	}
	if _, ok := gate.ParseReviewStatus(string(e.Status)); !ok {
		return invalidPermissionReview(name, FieldStatus)
	}

	switch e.Status {
	case gate.ReviewStatusAllowed, gate.ReviewStatusNeedsHuman:
		if _, ok := gate.ParseReviewRisk(string(e.Risk)); !ok {
			return invalidPermissionReview(name, FieldRisk)
		}
		if e.Status == gate.ReviewStatusAllowed && e.Risk == gate.ReviewRiskCritical {
			return invalidPermissionReview(name, FieldRisk)
		}
		if _, ok := gate.ParseReviewAuthorization(string(e.Authorization)); !ok {
			return invalidPermissionReview(name, FieldAuthorization)
		}
		if err := gate.ValidateReviewCategories(e.Categories); err != nil {
			return invalidPermissionReview(name, FieldCategories)
		}
		if e.AutoApproved != (e.Status == gate.ReviewStatusAllowed) {
			return invalidPermissionReview(name, FieldAutoApproved)
		}
	case gate.ReviewStatusNotApplicable,
		gate.ReviewStatusTimedOut,
		gate.ReviewStatusFailed,
		gate.ReviewStatusCancelled,
		gate.ReviewStatusStale:
		if e.Risk != "" {
			return invalidPermissionReview(name, FieldRisk)
		}
		if e.Authorization != "" {
			return invalidPermissionReview(name, FieldAuthorization)
		}
		if len(e.Categories) != 0 {
			return invalidPermissionReview(name, FieldCategories)
		}
		if e.AutoApproved {
			return invalidPermissionReview(name, FieldAutoApproved)
		}
	}
	return nil
}

func validatePermissionReviewMetadata(
	name EventName,
	visibility EventVisibility,
	gateID gate.ID,
	classifier hustle.Name,
	revision string,
) error {
	if visibility != Internal {
		return invalidPermissionReview(name, FieldVisibility)
	}
	if gateID.IsZero() {
		return &InvalidEventError{Event: name, Field: FieldGateID, Rule: RuleRequired}
	}
	if gate.ValidatePermissionClassifierName(classifier) != nil {
		return invalidPermissionReview(name, FieldClassifier)
	}
	if !utf8.ValidString(revision) ||
		strings.TrimSpace(revision) == "" ||
		len(revision) > gate.MaxPermissionClassifierRevisionBytes {
		return invalidPermissionReview(name, FieldClassifierRevision)
	}
	return nil
}

func validateProcessLifecycleEvent(
	name EventName,
	header Header,
	metadata tool.ProcessLifecycleMetadata,
	wantKind tool.ProcessLifecycleKind,
) error {
	if metadata.EventID != header.EventID {
		return &InvalidEventError{Event: name, Field: FieldEventID, Rule: RuleInvalid}
	}
	if metadata.SessionID != header.SessionID {
		return &InvalidEventError{Event: name, Field: FieldSessionID, Rule: RuleInvalid}
	}
	if metadata.LoopID != header.LoopID {
		return &InvalidEventError{Event: name, Field: FieldLoopID, Rule: RuleInvalid}
	}
	if metadata.Kind != wantKind {
		return &InvalidEventError{Event: name, Field: FieldProcess, Rule: RuleInvalid}
	}
	if err := metadata.Validate(); err != nil {
		return &InvalidEventError{Event: name, Field: FieldProcess, Rule: RuleInvalid}
	}
	return nil
}

func invalidPermissionReview(name EventName, field FieldName) error {
	return &InvalidEventError{Event: name, Field: field, Rule: RuleInvalid}
}

// Bounds for ConfigurationAdopted's durable, partly user-authored payload: a
// hostile or buggy decision must not be able to append an unbounded record to
// the journal, and a legacy (SchemaVersion 0) manifest projection is never
// persisted.
const (
	maxAgentRuntimeIdentityBytes = 128
	// maxRuntimeManifestIdentifierBytes bounds the current-schema runtime
	// identity fields. They are opaque identifiers at this boundary: the
	// producer owns their meaning, while the journal only accepts the bounded,
	// secret-free alphabet used by runtime profiles and revisions.
	maxRuntimeManifestIdentifierBytes = 128

	// The manifest is decoded from untrusted journal input, so its collections
	// are capped defense-in-depth. The caps are generous: they never trip a
	// legitimate configuration, only an abusive one.
	maxConfigManifestTools     = 4096
	maxConfigManifestAppFields = 1024
	// maxConfigDriftChanges must never reject a drift summary a VALID manifest
	// comparison can legitimately produce, or a large-but-legitimate change would
	// brick every restore. A schema-2 assessment can emit one removal and one
	// addition per bounded collection member when baseline and candidate are
	// disjoint, plus thirteen manifest scalar-field categories including hook
	// policy. Restore may append one root-agent-name change after AssessDrift,
	// so that slot is explicit too. It still bounds a decoded hostile event.
	// Runtime identity contributes three additional bounded scalar changes, and
	// a configured permission-review policy can contribute one more, each of
	// which is fail-closed.
	maxConfigDriftScalarChanges  = 17
	maxConfigDriftAgentNameSlots = 1
	maxConfigDriftChanges        = 2*maxConfigManifestTools + 2*maxConfigManifestAppFields + maxConfigDriftScalarChanges + maxConfigDriftAgentNameSlots
	// MaxConfigMessageLen and MaxConfigActorLen bound the durable, partly
	// user-authored audit fields. They are exported so the restore constructor can
	// TRUNCATE a decider's over-long Message/Actor before building the adoption (a
	// long audit note must never brick a restore); the validator here still rejects
	// an over-long field on a hand-crafted, decoded journal record.
	MaxConfigMessageLen = 4096
	MaxConfigActorLen   = 1024
)

func validateAgentRuntime(v AgentRuntime) error {
	if v.Harness == "" || v.Profile == "" || v.CredentialMode == "" {
		return &InvalidEventError{Event: "LoopStarted", Field: FieldAgentRuntime, Rule: RuleRequired}
	}
	if v.CredentialMode != "native-auth" && v.CredentialMode != "gateway-backed" {
		return &InvalidEventError{Event: "LoopStarted", Field: FieldAgentRuntime, Rule: RuleInvalid}
	}
	if (v.Source == "") != (v.SelectionKind == "") {
		return &InvalidEventError{Event: "LoopStarted", Field: FieldAgentRuntime, Rule: RuleInvalid}
	}
	if v.Source != "" && v.Source != "gateway" && v.Source != "native" {
		return &InvalidEventError{Event: "LoopStarted", Field: FieldAgentRuntime, Rule: RuleInvalid}
	}
	if v.SelectionKind != "" && v.SelectionKind != "explicit" && v.SelectionKind != "harness-managed" {
		return &InvalidEventError{Event: "LoopStarted", Field: FieldAgentRuntime, Rule: RuleInvalid}
	}
	if v.Source != "" {
		if (v.Source == "gateway" && v.CredentialMode != "gateway-backed") || (v.Source == "native" && v.CredentialMode != "native-auth") {
			return &InvalidEventError{Event: "LoopStarted", Field: FieldAgentRuntime, Rule: RuleInvalid}
		}
		if v.SelectionKind == "harness-managed" {
			if v.Source != "native" || v.ModelAlias != "" || v.SmallModelAlias != "" {
				return &InvalidEventError{Event: "LoopStarted", Field: FieldAgentRuntime, Rule: RuleInvalid}
			}
		} else if v.ModelAlias == "" {
			return &InvalidEventError{Event: "LoopStarted", Field: FieldAgentRuntime, Rule: RuleRequired}
		}
	} else if v.ModelAlias == "" {
		// Legacy AgentRuntime records predate source and selection_kind and
		// always carried a concrete model alias.
		return &InvalidEventError{Event: "LoopStarted", Field: FieldAgentRuntime, Rule: RuleRequired}
	}
	for _, candidate := range []struct {
		value      string
		allowSlash bool
	}{
		{v.Harness, false},
		{v.Profile, true},
		{v.CredentialMode, false},
		{v.Source, false},
		{v.SelectionKind, false},
		{v.ModelAlias, false},
		{v.SmallModelAlias, false},
		{v.ACPSessionID, false},
	} {
		if candidate.value != "" && !validAgentRuntimeIdentifier(candidate.value, candidate.allowSlash) {
			return &InvalidEventError{Event: "LoopStarted", Field: FieldAgentRuntime, Rule: RuleInvalid}
		}
	}
	return nil
}

func validAgentRuntimeIdentifier(value string, allowSlash bool) bool {
	if value == "" {
		return true
	}
	if len(value) > maxAgentRuntimeIdentityBytes || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	if strings.Contains(value, "://") || strings.HasPrefix(value, "/") || strings.HasPrefix(value, "~/") || strings.HasPrefix(value, "./") || strings.HasPrefix(value, "../") {
		return false
	}
	if !allowSlash && strings.Contains(value, "/") {
		return false
	}
	if allowSlash && strings.Contains(value, "\\") {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
		for _, r := range segment {
			if r == '\\' || r == ':' || r == 0 || unicode.IsControl(r) || unicode.IsSpace(r) {
				return false
			}
		}
	}
	return true
}

// validateConfigurationAdopted enforces the config-epoch invariants: epoch 1
// belongs to SessionStarted so an adoption is always >= 2, the adopted
// fingerprint is required, the source is one of the four closed DecisionSource
// values, the drift summary, message, and actor are length-capped, and a legacy
// (SchemaVersion 0) manifest projection is refused.
func validateConfigurationAdopted(e ConfigurationAdopted) error {
	const name EventName = "ConfigurationAdopted"
	if e.Epoch < 2 {
		return &InvalidEventError{Event: name, Field: FieldEpoch, Rule: RuleInvalid}
	}
	if e.AdoptedFingerprint == "" {
		return &InvalidEventError{Event: name, Field: FieldAdoptedFingerprint, Rule: RuleRequired}
	}
	if !e.Source.Valid() {
		return &InvalidEventError{Event: name, Field: FieldSource, Rule: RuleInvalid}
	}
	if len(e.Drift) > maxConfigDriftChanges {
		return &InvalidEventError{Event: name, Field: FieldDrift, Rule: RuleInvalid}
	}
	if len(e.Message) > MaxConfigMessageLen {
		return &InvalidEventError{Event: name, Field: FieldMessage, Rule: RuleInvalid}
	}
	if len(e.Actor) > MaxConfigActorLen {
		return &InvalidEventError{Event: name, Field: FieldActor, Rule: RuleInvalid}
	}
	if !validConfigManifestSchema(e.Manifest, false) {
		return &InvalidEventError{Event: name, Field: FieldManifest, Rule: RuleInvalid}
	}
	// A persisted manifest's recorded fingerprint must match the manifest itself,
	// so a durable baseline can never carry a fingerprint that disagrees with the
	// configuration it describes.
	if e.Manifest.Fingerprint() != e.AdoptedFingerprint {
		return &InvalidEventError{Event: name, Field: FieldAdoptedFingerprint, Rule: RuleInvalid}
	}
	if len(e.Manifest.Tools) > maxConfigManifestTools {
		return &InvalidEventError{Event: name, Field: FieldManifest, Rule: RuleInvalid}
	}
	if len(e.Manifest.AppFields) > maxConfigManifestAppFields {
		return &InvalidEventError{Event: name, Field: FieldManifest, Rule: RuleInvalid}
	}
	return nil
}

func validConfigManifestSchema(manifest ConfigManifest, allowLegacy bool) bool {
	switch manifest.SchemaVersion {
	case 0:
		return allowLegacy && manifest.HookPolicyRev == ""
	case 1:
		// HookPolicyRev did not exist in schema v1 and is deliberately excluded
		// from its historical canonical layout. Reject it rather than accepting
		// policy state that the fingerprint cannot authenticate.
		return manifest.HookPolicyRev == "" && zeroRuntimeManifest(manifest)
	case 2:
		// Schema v2 has no runtime identity fields. Reject populated fields rather
		// than accepting data the v2 fingerprint does not authenticate.
		return zeroRuntimeManifest(manifest)
	case ManifestSchemaVersion:
		return validRuntimeManifestIdentifier(manifest.RuntimeProfile) &&
			validRuntimeManifestIdentifier(manifest.RuntimeCatalogRev) &&
			validRuntimeManifestIdentifier(manifest.RuntimeIdentityRev)
	default:
		return false
	}
}

// validRuntimeManifestIdentifier validates one current-schema runtime identity
// field. Empty is valid because native and legacy sessions have no runtime
// override. Non-empty values are deliberately treated as opaque: this helper
// does not interpret profiles, catalog revisions, or digests. It only bounds
// them and permits the identifier alphabet emitted by the runtime producers.
func validRuntimeManifestIdentifier(value string) bool {
	if value == "" {
		return true
	}
	if len(value) > maxRuntimeManifestIdentifierBytes || !utf8.ValidString(value) {
		return false
	}
	if strings.TrimSpace(value) != value {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
		for _, r := range segment {
			if r == '\\' || r == ':' || r == 0 || unicode.IsControl(r) || unicode.IsSpace(r) {
				return false
			}
		}
	}
	return true
}

func zeroRuntimeManifest(manifest ConfigManifest) bool {
	return manifest.RuntimeProfile == "" && manifest.RuntimeCatalogRev == "" &&
		manifest.RuntimeIdentityRev == ""
}

func invalidHustle(name EventName, field FieldName) *InvalidEventError {
	return &InvalidEventError{Event: name, Field: field, Rule: RuleInvalid}
}

func validateHustleRun(name EventName, run HustleRunDescriptor) error {
	if err := run.Definition.Validate(); err != nil {
		return invalidHustle(name, FieldDefinition)
	}
	if uuid.UUID(run.RunID).IsZero() {
		return invalidHustle(name, FieldRunID)
	}
	return nil
}

func validateHustleFailed(e HustleFailed) error {
	const name EventName = "HustleFailed"
	if e.Visibility() != Internal {
		return invalidHustle(name, FieldVisibility)
	}
	if err := validateHustleRun(name, e.Run); err != nil {
		return err
	}
	if e.Duration < 0 {
		return invalidHustle(name, FieldDuration)
	}
	if !e.Stage.Valid() {
		return invalidHustle(name, FieldStage)
	}
	if !e.ReasonCode.Valid() {
		return invalidHustle(name, FieldReasonCode)
	}
	if !hustle.ReasonAllowed(e.Stage, e.ReasonCode) {
		return invalidHustle(name, FieldReasonCode)
	}
	preResolution := e.Stage == hustle.StageQueue || e.Stage == hustle.StageModelResolution
	if preResolution {
		if e.Usage != nil {
			return invalidHustle(name, FieldUsage)
		}
		if !zeroModelRuntime(e.Run.Runtime) {
			return invalidHustle(name, FieldRuntime)
		}
		return nil
	}
	if err := validateModelRuntime(name, e.Run.Runtime); err != nil {
		return invalidHustle(name, FieldRuntime)
	}
	return validateOptionalUsage(name, e.Usage)
}

func validateOptionalUsage(name EventName, usage *content.Usage) error {
	if usage == nil {
		return nil
	}
	if err := usage.Validate(); err != nil {
		return invalidHustle(name, FieldUsage)
	}
	return nil
}

func zeroModelRuntime(runtime ModelRuntime) bool {
	return runtime.Key == (model.ModelKey{}) && runtime.Limits == (model.ContextLimits{}) && runtime.Effort == model.Effort("")
}

func validateStepDoneMessages(messages content.AgenticMessages) error {
	if len(messages) == 0 {
		return &InvalidEventError{Event: "StepDone", Field: FieldMessages, Rule: RuleInvalid}
	}
	first, ok := messages[0].(*content.AIMessage)
	if !ok || first == nil || first.Role != content.RoleAssistant {
		return &InvalidEventError{Event: "StepDone", Field: FieldMessages, Rule: RuleInvalid}
	}
	for _, message := range messages[1:] {
		toolResult, toolResultOK := message.(*content.ToolResultMessage)
		if !toolResultOK || toolResult == nil || toolResult.Role != content.RoleTool {
			return &InvalidEventError{Event: "StepDone", Field: FieldMessages, Rule: RuleInvalid}
		}
	}
	return nil
}

// Bounds for LoopExternalToolsetChanged. External toolsets are third-party supplied,
// so every string that reaches the journal is length-capped and the tool list is
// count-capped: a hostile or buggy MCP server must not be able to append an unbounded
// record to the durable log.
const (
	maxExternalSourceLen     = 64
	maxExternalGenerationLen = 128
	maxExternalToolNameLen   = 128
	maxExternalTools         = 512
	schemaDigestHexLen       = 64 // hex SHA-256
)

// isLowerHex reports whether s is exactly n lowercase-hex characters. The digest is
// produced by SchemaDigest, so anything else means a hand-built or tampered record.
func isLowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// validateExternalToolset enforces the durable shape of LoopExternalToolsetChanged:
// a non-empty bounded Source and Generation, a bounded tool list, and per-tool a
// non-empty bounded Name plus a well-formed hex SHA-256 digest. Duplicate tool names
// are rejected — the slot's whole purpose is a collision-free registry, so a record
// claiming two identically named tools describes a state the runtime never installs.
// An EMPTY Tools list is valid and meaningful: it durably records a slot cleared to
// nothing.
// validateIntegrationStatus enforces IntegrationStatus's bounds and its closed
// State enum at the publish boundary.
//
// It is the whole reason the event is safe to define for a producer that lives
// outside this module. Source, Name, and Detail all originate in an integration
// Harness does not own, and Detail is the field an integration fills from a
// failure it observed — which is to say, from text a third-party server
// influenced. Bounding them here means a hostile or buggy integration cannot
// grow an event without limit, and rejecting an undeclared State means an unset
// or garbage state fails closed rather than rendering as whatever the zero value
// happens to sit next to.
func validateIntegrationStatus(e IntegrationStatus) error {
	const name EventName = "IntegrationStatus"
	if e.Source == "" {
		return &InvalidEventError{Event: name, Field: FieldSource, Rule: RuleRequired}
	}
	if len(e.Source) > MaxIntegrationSourceBytes {
		return &InvalidEventError{Event: name, Field: FieldSource, Rule: RuleInvalid}
	}
	if e.Name == "" {
		return &InvalidEventError{Event: name, Field: FieldIntegrationName, Rule: RuleRequired}
	}
	if len(e.Name) > MaxIntegrationNameBytes {
		return &InvalidEventError{Event: name, Field: FieldIntegrationName, Rule: RuleInvalid}
	}
	if !e.State.Valid() {
		return &InvalidEventError{Event: name, Field: FieldState, Rule: RuleInvalid}
	}
	if len(e.Detail) > MaxIntegrationDetailBytes {
		return &InvalidEventError{Event: name, Field: FieldDetail, Rule: RuleInvalid}
	}
	return nil
}

func validateExternalToolset(e LoopExternalToolsetChanged) error {
	const name EventName = "LoopExternalToolsetChanged"
	if e.Source == "" {
		return &InvalidEventError{Event: name, Field: FieldSource, Rule: RuleRequired}
	}
	if len(e.Source) > maxExternalSourceLen {
		return &InvalidEventError{Event: name, Field: FieldSource, Rule: RuleInvalid}
	}
	if e.Generation == "" {
		return &InvalidEventError{Event: name, Field: FieldGeneration, Rule: RuleRequired}
	}
	if len(e.Generation) > maxExternalGenerationLen {
		return &InvalidEventError{Event: name, Field: FieldGeneration, Rule: RuleInvalid}
	}
	if len(e.Tools) > maxExternalTools {
		return &InvalidEventError{Event: name, Field: FieldTools, Rule: RuleInvalid}
	}
	seen := make(map[string]struct{}, len(e.Tools))
	for _, t := range e.Tools {
		if t.Name == "" || len(t.Name) > maxExternalToolNameLen {
			return &InvalidEventError{Event: name, Field: FieldTools, Rule: RuleInvalid}
		}
		if !isLowerHex(t.SchemaDigest, schemaDigestHexLen) {
			return &InvalidEventError{Event: name, Field: FieldTools, Rule: RuleInvalid}
		}
		if _, dup := seen[t.Name]; dup {
			return &InvalidEventError{Event: name, Field: FieldTools, Rule: RuleInvalid}
		}
		seen[t.Name] = struct{}{}
	}
	return nil
}

func validateModelRuntime(name EventName, runtime ModelRuntime) error {
	if err := runtime.Key.Validate(); err != nil {
		return &InvalidEventError{Event: name, Field: FieldModelKey, Rule: RuleInvalid}
	}
	if err := runtime.Limits.Validate(); err != nil {
		return &InvalidEventError{Event: name, Field: FieldContextLimits, Rule: RuleInvalid}
	}
	if !runtime.Effort.Valid() {
		return &InvalidEventError{Event: name, Field: FieldEffort, Rule: RuleInvalid}
	}
	return nil
}

func validCheckpointCause(sessionID uuid.UUID, trigger SnapshotTriggerKind, cause identity.Cause) bool {
	zero := identity.Cause{}
	if trigger == SnapshotTriggerManual || trigger == SnapshotTriggerSeed {
		return cause == zero
	}
	if cause.EventID.IsZero() || !cause.CommandID.IsZero() || !cause.ToolExecutionID.IsZero() || cause.Agency != identity.AgencyMachine {
		return false
	}
	c := cause.Coordinates
	if c.SessionID != sessionID {
		return false
	}
	switch trigger {
	case SnapshotTriggerIdle:
		return !c.SessionID.IsZero() && c.LoopID.IsZero() && c.TurnID.IsZero() && c.StepID.IsZero()
	case SnapshotTriggerInterrupt, SnapshotTriggerTurnDone:
		return !c.SessionID.IsZero() && !c.LoopID.IsZero() && !c.TurnID.IsZero() && c.StepID.IsZero()
	case SnapshotTriggerStepDone:
		return !c.SessionID.IsZero() && !c.LoopID.IsZero() && !c.TurnID.IsZero() && !c.StepID.IsZero()
	default:
		return false
	}
}

// checkProfile enforces one event's idProfile against its Coordinates: required
// fields must be non-zero, forbidden fields must be zero, and a tool/gate event's
// ToolExecutionID (read from the concrete event body, passed in as toolID) must be
// non-zero. The order (session ▸ loop ▸ turn ▸ step ▸ tool) reports the outermost
// violation first.
func checkProfile(name EventName, c identity.Coordinates, toolID uuid.UUID, p idProfile) error {
	checks := []struct {
		bad   bool
		field FieldName
		rule  Rule
	}{
		{p.requireSession && c.SessionID.IsZero(), FieldSessionID, RuleRequired},
		{p.requireLoop && c.LoopID.IsZero(), FieldLoopID, RuleRequired},
		{p.forbidLoop && !c.LoopID.IsZero(), FieldLoopID, RuleMustBeZero},
		{p.requireTurn && c.TurnID.IsZero(), FieldTurnID, RuleRequired},
		{p.forbidTurn && !c.TurnID.IsZero(), FieldTurnID, RuleMustBeZero},
		{p.requireStep && c.StepID.IsZero(), FieldStepID, RuleRequired},
		{p.forbidStep && !c.StepID.IsZero(), FieldStepID, RuleMustBeZero},
		// StepID requires TurnID (StepID set ⇒ TurnID set), independent of the profile.
		{!c.StepID.IsZero() && c.TurnID.IsZero(), FieldTurnID, RuleRequired},
		{p.requireTool && toolID.IsZero(), FieldToolExecutionID, RuleRequired},
	}
	for _, chk := range checks {
		if chk.bad {
			return &InvalidEventError{Event: name, Field: chk.field, Rule: chk.rule}
		}
	}
	return nil
}

// toolExecutionID returns the body ToolExecutionID for the seven tool-interaction
// and permission-review events
// (the only events whose profile sets requireTool) and the zero UUID for every
// other type — checkProfile ignores it unless requireTool is set.
func toolExecutionID(ev Event) uuid.UUID {
	switch e := ev.(type) {
	case PermissionRequested:
		return e.ToolExecutionID
	case PermissionDecided:
		return e.ToolExecutionID
	case UserInputRequested:
		return e.ToolExecutionID
	case ToolCallStarted:
		return e.ToolExecutionID
	case ToolCallCompleted:
		return e.ToolExecutionID
	case PermissionReviewStarted:
		return e.ToolExecutionID
	case PermissionReviewCompleted:
		return e.ToolExecutionID
	default:
		return uuid.UUID{}
	}
}

// classify is the SINGLE enumeration of the sealed event union: it returns the
// concrete type name, its STATIC idProfile, and whether the type is in the union.
// Keeping name and profile in one switch means a newly added event type cannot be
// half-registered — there is exactly one place to add it. An unknown type renders
// as "Event" with ok==false (ValidateEvent rejects it fail-secure).
func classify(ev Event) (name string, profile idProfile, ok bool) {
	switch e := ev.(type) {
	case SessionStarted:
		return "SessionStarted", sessionProfile(), true
	case SessionActive:
		return "SessionActive", sessionProfile(), true
	case SessionIdle:
		return "SessionIdle", sessionProfile(), true
	case SessionStopped:
		return "SessionStopped", sessionProfile(), true
	case RestoreStarted:
		// Session-scoped, same shape as SessionStarted: only SessionID set.
		return "RestoreStarted", sessionProfile(), true
	case RestoreDone:
		return "RestoreDone", sessionProfile(), true
	case RestoreErrored:
		return "RestoreErrored", sessionProfile(), true
	case ConfigurationAdopted:
		// Session-scoped, same shape as SessionStarted: only SessionID set. The
		// SessionID rides in the Header; the event carries no standalone field.
		return "ConfigurationAdopted", sessionProfile(), true
	case WorkspaceCheckpointed:
		// Session-scoped: a session-global workspace snapshot appended at quiescence
		// (same shape as RestoreDone/SessionIdle) — only SessionID set. Ref is an
		// opaque payload string the validator never constrains.
		return "WorkspaceCheckpointed", sessionProfile(), true
	case WorkspaceRestored:
		return "WorkspaceRestored", sessionProfile(), true
	case ActiveLoopChanged:
		return "ActiveLoopChanged", sessionProfile(), true
	case DelegateDeliveryStateChanged:
		return "DelegateDeliveryStateChanged", sessionProfile(), true
	case WorkflowActivity:
		return "WorkflowActivity", sessionProfile(), true
	case LoopRestoreTombstoned:
		return "LoopRestoreTombstoned", loopProfile(), true
	case IntegrationStatus:
		// Session-scoped: an integration is a session-global resource, not a
		// loop's. Same shape as WorkspaceCheckpointed — only SessionID set.
		return "IntegrationStatus", sessionProfile(), true
	case HustleStarted:
		return "HustleStarted", sessionProfile(), true
	case HustleCompleted:
		return "HustleCompleted", sessionProfile(), true
	case HustleFailed:
		return "HustleFailed", sessionProfile(), true
	case PermissionReviewStarted:
		return "PermissionReviewStarted", toolProfile(), true
	case PermissionReviewCompleted:
		return "PermissionReviewCompleted", toolProfile(), true
	case ProcessStarted:
		return "ProcessStarted", loopProfile(), true
	case ProcessBackgrounded:
		return "ProcessBackgrounded", loopProfile(), true
	case ProcessCompleted:
		return "ProcessCompleted", loopProfile(), true
	case ProcessStopRequested:
		return "ProcessStopRequested", loopProfile(), true
	case ProcessLost:
		return "ProcessLost", loopProfile(), true
	case LoopIdle:
		return "LoopIdle", loopProfile(), true
	case LoopStarted:
		// Loop-scoped: NEW loop in Header.Coordinates (SessionID+LoopID set, Turn/Step
		// zero). The spawning loop/turn/step rides in Header.Cause, which the validator
		// never constrains — same shape as LoopIdle.
		return "LoopStarted", loopProfile(), true
	case DelegateRequestAccepted:
		return "DelegateRequestAccepted", loopProfile(), true
	case LoopInferenceChanged:
		return "LoopInferenceChanged", loopProfile(), true
	case LoopModeChanged:
		return "LoopModeChanged", loopProfile(), true
	case LoopExternalToolsetChanged:
		return "LoopExternalToolsetChanged", loopProfile(), true
	case ContextMeasured:
		return "ContextMeasured", loopProfile(), true
	case ContextPressure:
		return "ContextPressure", loopProfile(), true
	case CompactionStarted:
		return "CompactionStarted", loopProfile(), true
	case CompactionCommitted:
		return "CompactionCommitted", loopProfile(), true
	case CompactionRejected:
		return "CompactionRejected", loopProfile(), true
	case CompactWaiterResolved:
		return "CompactWaiterResolved", loopProfile(), true
	case CompactWaiterRejected:
		return "CompactWaiterRejected", loopProfile(), true
	case ForeignSessionBound:
		return "ForeignSessionBound", loopProfile(), true
	case LoopAgentSessionBound:
		return "LoopAgentSessionBound", loopProfile(), true
	case TokenDelta:
		return "TokenDelta", stepProfile(), true
	case TurnStarted:
		return "TurnStarted", turnProfile(), true
	case StepDone:
		return "StepDone", stepProfile(), true
	case TurnFoldedInto:
		return "TurnFoldedInto", turnProfile(), true
	case InputCancelled:
		return "InputCancelled", inputCancelledProfile(), true
	case InputQueued:
		// Loop-scoped reply event resolved before a turn exists: SessionID+LoopID set,
		// TurnID/StepID zero (same shape as LoopIdle).
		return "InputQueued", loopProfile(), true
	case TurnRejected:
		return "TurnRejected", loopProfile(), true
	case TurnDone:
		return "TurnDone", turnProfile(), true
	case TurnFailed:
		return "TurnFailed", turnProfile(), true
	case TurnInterrupted:
		return "TurnInterrupted", turnProfile(), true
	case PermissionRequested:
		return "PermissionRequested", toolProfile(), true
	case PermissionDecided:
		return "PermissionDecided", toolProfile(), true
	case UserInputRequested:
		return "UserInputRequested", toolProfile(), true
	case ToolCallStarted:
		return "ToolCallStarted", toolProfile(), true
	case ToolCallCompleted:
		return "ToolCallCompleted", toolProfile(), true
	case GatePrepared:
		// Gate identity varies by how the gate was raised, so the profile is selected
		// from the embedded gate's resolver rather than fixed per type. GatePrepared and
		// GateOpened carry the full gate.Gate; GateResolved carries its own Resolver tag.
		return "GatePrepared", gateIdentityProfile(e.Gate.Resolver), true
	case GateOpened:
		return "GateOpened", gateIdentityProfile(e.Gate.Resolver), true
	case GateResolved:
		return "GateResolved", gateIdentityProfile(e.Resolver), true
	default:
		return "Event", idProfile{}, false
	}
}

// sessionProfile: ScopeSession — only SessionID set; LoopID/TurnID/StepID zero.
func sessionProfile() idProfile {
	return idProfile{requireSession: true, forbidLoop: true, forbidTurn: true, forbidStep: true}
}

// loopProfile: ScopeLoop with no turn — SessionID+LoopID set; TurnID/StepID zero.
func loopProfile() idProfile {
	return idProfile{requireSession: true, requireLoop: true, forbidTurn: true, forbidStep: true}
}

// turnProfile: turn events — SessionID+LoopID+TurnID set; StepID zero.
func turnProfile() idProfile {
	return idProfile{requireSession: true, requireLoop: true, requireTurn: true, forbidStep: true}
}

// inputCancelledProfile: SessionID+LoopID set; StepID zero; TurnID OPTIONAL (zero
// for a client retract outside a turn, the returned turn for an abnormal return).
func inputCancelledProfile() idProfile {
	return idProfile{requireSession: true, requireLoop: true, forbidStep: true}
}

// stepProfile: step events (TokenDelta/StepDone) — SessionID+LoopID+TurnID+StepID set.
func stepProfile() idProfile {
	return idProfile{requireSession: true, requireLoop: true, requireTurn: true, requireStep: true}
}

// gateIdentityProfile selects a gate event's identity contract from the resolver
// that owns it. The three gate events are loopScoped (Scope()==ScopeLoop) but the
// coordinates a gate legitimately carries depend on HOW it was raised, so a single
// per-type profile is wrong:
//
//   - Host-owned gates (gate.ResolverSession — a form/open-url elicitation raised by
//     an integration through GateHost.OpenHostGate) belong to no turn or step, and
//     a startup elicitation belongs to no loop either. Their only guaranteed
//     coordinate is the SessionID; LoopID/TurnID/StepID are OPTIONAL (a
//     loop-attributed elicitation carries a LoopID, startup carries none).
//   - Loop-owned gates (gate.ResolverLoop — permission/ask-user) keep the FULL step
//     profile: a permission gate that parks a tool call without a step is malformed
//     and must fail, exactly as before.
//
// An empty/unknown resolver fails SECURE to the strict loop-owned profile — the same
// contract every gate record enforced before host-owned gates were distinguished, so
// a record written before the discriminator existed is held to the stricter rule.
func gateIdentityProfile(resolver gate.ResolverKind) idProfile {
	if resolver == gate.ResolverSession {
		return hostGateProfile()
	}
	return stepProfile()
}

// hostGateProfile: host-owned gates require only a SessionID. LoopID/TurnID/StepID
// are unconstrained (neither required nor forbidden), so a loop-attributed
// elicitation may carry a LoopID and a startup one need not. The universal
// StepID⇒TurnID rule in checkProfile still applies. It mirrors the way
// inputCancelledProfile makes an inner coordinate optional rather than forbidden.
func hostGateProfile() idProfile {
	return idProfile{requireSession: true}
}

// toolProfile: the seven tool-interaction and permission-review events — full
// quartet set plus a required
// ToolExecutionID (read from the event body by ValidateEvent, not stored here).
func toolProfile() idProfile {
	return idProfile{
		requireSession: true, requireLoop: true, requireTurn: true, requireStep: true,
		requireTool: true,
	}
}
