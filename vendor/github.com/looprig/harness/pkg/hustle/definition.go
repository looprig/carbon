package hustle

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
	model "github.com/looprig/inference/model"
)

const (
	maxPayloadBytes                  = 16 * 1024 * 1024
	maxOutputSchemaNameBytes         = 64
	maxStructuredOutputRevisionBytes = 128
	maxToolLoopCount                 = 4096
	reservedNamePrefix               = "_looprig."
)

const (
	// MaxEvidenceToolDefinitions bounds the number of definitions in one
	// evidence policy before any catalog-sized allocation or digest work.
	MaxEvidenceToolDefinitions = 64
	// MaxEvidenceProducedToolNames bounds all concrete tool names declared by
	// one evidence policy, including names spread across bundle definitions.
	MaxEvidenceProducedToolNames = 128
	// MaxEvidenceToolNameBytes bounds definition and concrete tool names by
	// encoded UTF-8 bytes, not runes.
	MaxEvidenceToolNameBytes = 64
	// MaxEvidenceToolPolicyRevisionBytes bounds the canonical policy revision
	// by encoded UTF-8 bytes.
	MaxEvidenceToolPolicyRevisionBytes = 128
)

const (
	evidencePolicyDigestDomain            = "looprig/hustle/evidence-policy/v1"
	evidenceDefinitionCatalogDigestDomain = "looprig/hustle/evidence-definition-catalog/v1"
	evidenceProducedNamesDigestDomain     = "looprig/hustle/evidence-produced-names/v1"
	boundEvidenceToolDigestDomain         = "looprig/hustle/bound-evidence-tool/v1"
)

// Name is the stable registration name of one hustle definition.
type Name string

// Validate applies the stable hustle-name contract while preserving the
// caller's exact spelling. Whitespace is used only to detect empty or reserved
// names; it is not canonicalized away.
func (n Name) Validate() error {
	trimmed := strings.TrimSpace(string(n))
	if trimmed == "" {
		return &DefinitionError{Kind: DefinitionMissingName, Field: "name"}
	}
	if strings.HasPrefix(trimmed, reservedNamePrefix) {
		return &DefinitionError{Kind: DefinitionReservedName, Field: "name"}
	}
	return nil
}

// Participation selects the session-global execution lane.
type Participation uint8

const (
	ParticipationUnknown Participation = iota
	ParticipationBlocking
	ParticipationBackground
)

// ModelSource selects how an invocation obtains its inference binding.
type ModelSource uint8

const (
	ModelSourceUnknown ModelSource = iota
	ModelSourceCurrentLoop
	ModelSourceNamed
)

// Limits bounds the serialized request and response payloads.
type Limits struct {
	InputBytes  int
	OutputBytes int
}

// ToolLoopLimits bounds one opt-in evidence-tool conversation. Every field is
// required for an enabled policy; the zero value means evidence tools are off.
type ToolLoopLimits struct {
	MaxRounds        int
	MaxCalls         int
	MaxCallsPerRound int
	MaxResultBytes   int
	MaxEvidenceBytes int
}

// EvidenceToolPolicy is the immutable-definition input for a bounded evidence
// loop. Definitions are copied when the option is created and by Clone.
type EvidenceToolPolicy struct {
	Revision    string
	Limits      ToolLoopLimits
	Definitions []tool.Definition
}

// Clone returns a policy with an independently owned definition slice.
func (p EvidenceToolPolicy) Clone() EvidenceToolPolicy {
	p.Definitions = append([]tool.Definition(nil), p.Definitions...)
	return p
}

// InferenceBinding pairs a client with its validated, secret-free model.
type InferenceBinding struct {
	Client inference.Client
	Model  model.Model
}

// ModelResolver resolves the exact originating loop's live inference binding.
type ModelResolver interface {
	ResolveHustleModel(context.Context, uuid.UUID) (InferenceBinding, error)
}

// Bindings supplies runtime collaborators needed by a definition.
type Bindings struct {
	Models ModelResolver
}

// EvidenceBindings supplies only the invocation origin and structurally
// read-only workspace capability needed to build one run's evidence catalog.
// It intentionally cannot carry mutation, delegation, gate, grant, session,
// observation, or loop-control capabilities.
type EvidenceBindings struct {
	SessionID     uuid.UUID
	LoopID        uuid.UUID
	ReadWorkspace *tool.ReadWorkspaceBinding
}

// DefinitionDescriptor is the complete secret-free behavioral projection used
// by rig identity and durable audit records.
type DefinitionDescriptor struct {
	Name                     Name
	Participation            Participation
	ModelSource              ModelSource
	NamedModelKey            model.ModelKey
	NamedModelPolicyRevision string
	PromptRevision           string
	PromptSHA256             [sha256.Size]byte
	OutputSchemaName         string `json:",omitzero"`
	// OutputSchemaSHA256 covers Description, compact Schema JSON, and Strict.
	// It is a behavioral digest; no raw output policy crosses this boundary.
	OutputSchemaSHA256              [sha256.Size]byte `json:",omitzero"`
	StructuredOutputRevision        string            `json:",omitzero"`
	PolicyRevision                  string
	TimeoutNanos                    int64
	Limits                          Limits
	EvidenceToolPolicyRevision      string            `json:",omitzero"`
	EvidenceToolDefinitionsSHA256   [sha256.Size]byte `json:",omitzero"`
	EvidenceProducedToolNamesSHA256 [sha256.Size]byte `json:",omitzero"`
	EvidenceToolLimits              ToolLoopLimits    `json:",omitzero"`
	EvidenceToolDefinitionCount     int               `json:",omitzero"`
	StructuredOutputWithTools       bool              `json:",omitzero"`
	RetryPolicy                     RetryPolicy       `json:",omitzero"`
}

// Validate checks the complete descriptor-only constructor domain without
// requiring the raw system prompt or an inference client.
func (d DefinitionDescriptor) Validate() error {
	if err := d.Name.Validate(); err != nil {
		return err
	}
	if d.Participation != ParticipationBlocking && d.Participation != ParticipationBackground {
		return &DefinitionError{Kind: DefinitionInvalidParticipation, Field: "participation"}
	}
	if d.ModelSource != ModelSourceCurrentLoop && d.ModelSource != ModelSourceNamed {
		return &DefinitionError{Kind: DefinitionInvalidModelSource, Field: "model_source"}
	}
	if d.TimeoutNanos <= 0 {
		return &DefinitionError{Kind: DefinitionInvalidTimeout, Field: "timeout"}
	}
	if invalidLimits(d.Limits) {
		return &DefinitionError{Kind: DefinitionInvalidLimits, Field: "limits"}
	}
	if strings.TrimSpace(d.PromptRevision) == "" {
		return &DefinitionError{Kind: DefinitionInvalidPromptRevision, Field: "prompt_revision"}
	}
	if d.PromptSHA256 == ([sha256.Size]byte{}) {
		return &DefinitionError{Kind: DefinitionInvalidSystemPrompt, Field: "prompt_sha256"}
	}
	if strings.TrimSpace(d.PolicyRevision) == "" {
		return &DefinitionError{Kind: DefinitionInvalidPolicyRevision, Field: "policy_revision"}
	}
	if err := validateDescriptorOutput(d); err != nil {
		return err
	}
	if err := validateDescriptorEvidenceTools(d); err != nil {
		return err
	}
	if !d.RetryPolicy.Valid() ||
		d.RetryPolicy != RetryPolicyNone && d.EvidenceToolPolicyRevision == "" {
		return &DefinitionError{Kind: DefinitionInvalidRetryPolicy, Field: "retry_policy"}
	}
	if d.ModelSource == ModelSourceNamed {
		if err := d.NamedModelKey.Validate(); err != nil {
			return &DefinitionError{Kind: DefinitionInvalidModel, Field: "named_model_key", Cause: err}
		}
		if strings.TrimSpace(d.NamedModelPolicyRevision) == "" {
			return &DefinitionError{Kind: DefinitionInvalidModel, Field: "named_model_policy_revision"}
		}
		return nil
	}
	if d.NamedModelKey != (model.ModelKey{}) || d.NamedModelPolicyRevision != "" {
		return &DefinitionError{Kind: DefinitionInvalidModel, Field: "current_loop_model"}
	}
	return nil
}

func validateDescriptorEvidenceTools(descriptor DefinitionDescriptor) error {
	zeroDigest := [sha256.Size]byte{}
	hasEvidence := descriptor.EvidenceToolPolicyRevision != "" ||
		descriptor.EvidenceToolDefinitionsSHA256 != zeroDigest ||
		descriptor.EvidenceProducedToolNamesSHA256 != zeroDigest ||
		descriptor.EvidenceToolLimits != (ToolLoopLimits{}) ||
		descriptor.EvidenceToolDefinitionCount != 0 ||
		descriptor.StructuredOutputWithTools
	if !hasEvidence {
		return nil
	}
	if err := validateEvidencePolicyRevision(descriptor.EvidenceToolPolicyRevision); err != nil {
		return err
	}
	if descriptor.EvidenceToolDefinitionsSHA256 == zeroDigest {
		return invalidEvidenceTools("evidence_tool_definitions_sha256")
	}
	if descriptor.EvidenceProducedToolNamesSHA256 == zeroDigest {
		return invalidEvidenceTools("evidence_produced_tool_names_sha256")
	}
	if descriptor.EvidenceToolDefinitionCount <= 0 ||
		descriptor.EvidenceToolDefinitionCount > MaxEvidenceToolDefinitions {
		return invalidEvidenceTools("evidence_tool_definition_count")
	}
	if err := validateToolLoopLimits(descriptor.EvidenceToolLimits); err != nil {
		return err
	}
	if !descriptor.StructuredOutputWithTools {
		return invalidEvidenceTools("structured_output_with_tools")
	}
	if descriptor.OutputSchemaName == "" {
		return invalidEvidenceTools("output_schema")
	}
	if descriptor.Participation != ParticipationBlocking {
		return invalidEvidenceTools("participation")
	}
	return nil
}

func validateDescriptorOutput(descriptor DefinitionDescriptor) error {
	zeroDigest := [sha256.Size]byte{}
	hasOutput := descriptor.OutputSchemaName != "" || descriptor.OutputSchemaSHA256 != zeroDigest || descriptor.StructuredOutputRevision != ""
	if !hasOutput {
		return nil
	}
	if !validOutputSchemaName(descriptor.OutputSchemaName) {
		return &DefinitionError{Kind: DefinitionInvalidOutputSchema, Field: "output_schema_name"}
	}
	if descriptor.OutputSchemaSHA256 == zeroDigest {
		return &DefinitionError{Kind: DefinitionInvalidOutputSchema, Field: "output_schema_sha256"}
	}
	if strings.TrimSpace(descriptor.StructuredOutputRevision) == "" || len(descriptor.StructuredOutputRevision) > maxStructuredOutputRevisionBytes {
		return &DefinitionError{Kind: DefinitionInvalidOutputSchema, Field: "structured_output_revision"}
	}
	return nil
}

func validOutputSchemaName(name string) bool {
	if name == "" || len(name) > maxOutputSchemaNameBytes || name == inference.StructuredOutputToolName {
		return false
	}
	for index := range len(name) {
		value := name[index]
		letter := value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z'
		if index == 0 {
			if !letter && value != '_' {
				return false
			}
			continue
		}
		digit := value >= '0' && value <= '9'
		if !letter && !digit && value != '_' && value != '-' {
			return false
		}
	}
	return true
}

// Option contributes one immutable definition property.
type Option func(*definitionOptions) error

// Definition is an immutable hustle definition. Its zero value is invalid.
type Definition struct{ state *definitionState }

type definitionState struct {
	descriptor   DefinitionDescriptor
	policyDigest string
	timeout      time.Duration
	systemPrompt string
	named        InferenceBinding
	output       *inference.OutputSchema
	evidence     EvidenceToolPolicy
}

type definitionOptions struct {
	name           Name
	participation  Participation
	timeout        time.Duration
	limits         Limits
	modelSource    ModelSource
	named          InferenceBinding
	systemPrompt   string
	promptRevision string
	policyRevision string
	output         *inference.OutputSchema
	evidence       EvidenceToolPolicy
	retryPolicy    RetryPolicy
	seen           map[string]struct{}
}

func (o *definitionOptions) singleton(field string) error {
	if _, exists := o.seen[field]; exists {
		return &DefinitionError{Kind: DefinitionDuplicateOption, Field: field}
	}
	o.seen[field] = struct{}{}
	return nil
}

// WithName sets the stable definition name.
func WithName(name Name) Option {
	return func(options *definitionOptions) error {
		if err := options.singleton("name"); err != nil {
			return err
		}
		options.name = name
		return nil
	}
}

// WithParticipation selects the definition's fixed execution lane.
func WithParticipation(participation Participation) Option {
	return func(options *definitionOptions) error {
		if err := options.singleton("participation"); err != nil {
			return err
		}
		options.participation = participation
		return nil
	}
}

// WithTimeout sets the exact invocation timeout.
func WithTimeout(timeout time.Duration) Option {
	return func(options *definitionOptions) error {
		if err := options.singleton("timeout"); err != nil {
			return err
		}
		options.timeout = timeout
		return nil
	}
}

// WithLimits sets serialized input and output byte limits.
func WithLimits(limits Limits) Option {
	return func(options *definitionOptions) error {
		if err := options.singleton("limits"); err != nil {
			return err
		}
		options.limits = limits
		return nil
	}
}

// WithCurrentLoopModel resolves the originating loop's live model on every run.
func WithCurrentLoopModel() Option {
	return func(options *definitionOptions) error {
		if err := options.singleton("model_source"); err != nil {
			return err
		}
		options.modelSource = ModelSourceCurrentLoop
		return nil
	}
}

// WithNamedInference freezes a named client/model pair in the definition.
func WithNamedInference(client inference.Client, model model.Model) Option {
	return func(options *definitionOptions) error {
		if err := options.singleton("model_source"); err != nil {
			return err
		}
		options.modelSource = ModelSourceNamed
		options.named = InferenceBinding{Client: client, Model: model}
		return nil
	}
}

// WithSystemPrompt freezes the raw prompt and its public revision label.
func WithSystemPrompt(prompt, revision string) Option {
	return func(options *definitionOptions) error {
		if err := options.singleton("system_prompt"); err != nil {
			return err
		}
		options.systemPrompt = prompt
		options.promptRevision = revision
		return nil
	}
}

// WithPolicyRevision identifies opaque parser and request-policy behavior.
func WithPolicyRevision(revision string) Option {
	return func(options *definitionOptions) error {
		if err := options.singleton("policy_revision"); err != nil {
			return err
		}
		options.policyRevision = revision
		return nil
	}
}

// WithOutputSchema freezes one optional provider-neutral structured-output
// policy. The option owns a clone immediately so caller mutations made before
// Define cannot alter the definition.
func WithOutputSchema(output inference.OutputSchema) Option {
	frozen := output.Clone()
	return func(options *definitionOptions) error {
		if err := options.singleton("output_schema"); err != nil {
			return err
		}
		clone := frozen.Clone()
		options.output = &clone
		return nil
	}
}

// WithEvidenceTools enables a bounded evidence-tool loop. The option owns a
// defensive copy immediately; the zero policy explicitly leaves tools off.
func WithEvidenceTools(policy EvidenceToolPolicy) Option {
	if len(policy.Definitions) > MaxEvidenceToolDefinitions {
		return func(options *definitionOptions) error {
			if err := options.singleton("evidence_tools"); err != nil {
				return err
			}
			return invalidEvidenceTools("definitions")
		}
	}
	frozen := policy.Clone()
	return func(options *definitionOptions) error {
		if err := options.singleton("evidence_tools"); err != nil {
			return err
		}
		options.evidence = frozen.Clone()
		return nil
	}
}

// WithRetryPolicy selects one immutable retry policy. Classified retry is
// intentionally limited to evidence-backed reviewer definitions.
func WithRetryPolicy(policy RetryPolicy) Option {
	return func(options *definitionOptions) error {
		if err := options.singleton("retry_policy"); err != nil {
			return err
		}
		options.retryPolicy = policy
		return nil
	}
}

// Define validates and freezes one text-only hustle definition.
func Define(opts ...Option) (Definition, error) {
	resolved := &definitionOptions{seen: make(map[string]struct{})}
	for index, opt := range opts {
		if opt == nil {
			return Definition{}, &DefinitionError{Kind: DefinitionNilOption, Field: "options[" + strconv.Itoa(index) + "]"}
		}
		if err := opt(resolved); err != nil {
			return Definition{}, err
		}
	}
	if err := validateDefinitionOptions(resolved); err != nil {
		return Definition{}, err
	}
	return freezeDefinition(resolved)
}

func validateDefinitionOptions(options *definitionOptions) error {
	if err := options.name.Validate(); err != nil {
		return err
	}
	if options.participation != ParticipationBlocking && options.participation != ParticipationBackground {
		return &DefinitionError{Kind: DefinitionInvalidParticipation, Field: "participation"}
	}
	if options.modelSource == ModelSourceUnknown {
		return &DefinitionError{Kind: DefinitionMissingModelSource, Field: "model_source"}
	}
	if options.modelSource == ModelSourceNamed {
		if err := validateInferenceBinding(options.named); err != nil {
			return err
		}
	}
	if options.timeout <= 0 {
		return &DefinitionError{Kind: DefinitionInvalidTimeout, Field: "timeout"}
	}
	if invalidLimits(options.limits) {
		return &DefinitionError{Kind: DefinitionInvalidLimits, Field: "limits"}
	}
	if strings.TrimSpace(options.systemPrompt) == "" {
		return &DefinitionError{Kind: DefinitionInvalidSystemPrompt, Field: "system_prompt"}
	}
	if strings.TrimSpace(options.promptRevision) == "" {
		return &DefinitionError{Kind: DefinitionInvalidPromptRevision, Field: "prompt_revision"}
	}
	if _, exists := options.seen["policy_revision"]; !exists {
		return &DefinitionError{Kind: DefinitionMissingPolicyRevision, Field: "policy_revision"}
	}
	if strings.TrimSpace(options.policyRevision) == "" {
		return &DefinitionError{Kind: DefinitionInvalidPolicyRevision, Field: "policy_revision"}
	}
	if options.output != nil {
		if err := inference.ValidateOutputSchema(*options.output); err != nil {
			return &DefinitionError{Kind: DefinitionInvalidOutputSchema, Field: "output_schema", Cause: err}
		}
	}
	if evidencePolicyEnabled(options.evidence) {
		if err := validateEvidenceToolPolicy(options.evidence); err != nil {
			return err
		}
		if options.output == nil {
			return invalidEvidenceTools("output_schema")
		}
		if options.participation != ParticipationBlocking {
			return invalidEvidenceTools("participation")
		}
	}
	if !options.retryPolicy.Valid() ||
		options.retryPolicy != RetryPolicyNone && !evidencePolicyEnabled(options.evidence) {
		return &DefinitionError{Kind: DefinitionInvalidRetryPolicy, Field: "retry_policy"}
	}
	return nil
}

func validateEvidenceToolPolicy(policy EvidenceToolPolicy) error {
	if err := validateEvidencePolicyRevision(policy.Revision); err != nil {
		return err
	}
	if err := validateToolLoopLimits(policy.Limits); err != nil {
		return err
	}
	if len(policy.Definitions) == 0 || len(policy.Definitions) > MaxEvidenceToolDefinitions {
		return invalidEvidenceTools("definitions")
	}
	definitionNames := make(map[string]struct{}, len(policy.Definitions))
	producedNames := make(map[string]struct{}, min(MaxEvidenceProducedToolNames, len(policy.Definitions)))
	producedNameCount := 0
	metadataBytes := 0
	for index, definition := range policy.Definitions {
		if nilToolDefinition(definition) {
			return invalidEvidenceTools("definitions[" + strconv.Itoa(index) + "]")
		}
		name := definition.Name()
		if !canonicalEvidenceToolName(name) {
			return invalidEvidenceTools("definitions[" + strconv.Itoa(index) + "].name")
		}
		if _, exists := definitionNames[name]; exists {
			return invalidEvidenceTools("definitions[" + strconv.Itoa(index) + "].name")
		}
		definitionNames[name] = struct{}{}
		if definition.Requirements()&^tool.RequiresWorkspaceRead != 0 {
			return invalidEvidenceTools("definitions[" + strconv.Itoa(index) + "].requirements")
		}
		names := definition.ProducedToolNames()
		infos := definition.ToolInfos()
		if len(names) == 0 {
			return invalidEvidenceTools("definitions[" + strconv.Itoa(index) + "].produced_tool_names")
		}
		if len(infos) == 0 || len(infos) != len(names) {
			return invalidEvidenceTools("definitions[" + strconv.Itoa(index) + "].tool_infos")
		}
		if len(names) > MaxEvidenceProducedToolNames-producedNameCount {
			return invalidEvidenceTools("produced_tool_names")
		}
		producedNameCount += len(names)
		for nameIndex, producedName := range names {
			if !canonicalEvidenceToolName(producedName) {
				return invalidEvidenceTools("definitions[" + strconv.Itoa(index) + "].produced_tool_names[" + strconv.Itoa(nameIndex) + "]")
			}
			if _, exists := producedNames[producedName]; exists {
				return invalidEvidenceTools("definitions[" + strconv.Itoa(index) + "].produced_tool_names[" + strconv.Itoa(nameIndex) + "]")
			}
			producedNames[producedName] = struct{}{}
			canonicalSchema, err := validateEvidenceToolInfo(infos[nameIndex])
			if err != nil || infos[nameIndex].Name != producedName {
				return invalidEvidenceTools("definitions[" + strconv.Itoa(index) + "].tool_infos[" + strconv.Itoa(nameIndex) + "]")
			}
			for _, size := range []int{len(infos[nameIndex].Name), len(infos[nameIndex].Desc), len(canonicalSchema)} {
				var withinLimit bool
				metadataBytes, withinLimit = addEvidenceMetadataBytes(metadataBytes, size)
				if !withinLimit {
					return invalidEvidenceTools("tool_metadata")
				}
			}
		}
	}
	return nil
}

func evidencePolicyEnabled(policy EvidenceToolPolicy) bool {
	return policy.Revision != "" || policy.Limits != (ToolLoopLimits{}) || len(policy.Definitions) != 0
}

func validateEvidencePolicyRevision(revision string) error {
	if !utf8.ValidString(revision) || revision == "" || revision != strings.TrimSpace(revision) ||
		len(revision) > MaxEvidenceToolPolicyRevisionBytes || strings.ContainsRune(revision, '\x00') {
		return invalidEvidenceTools("evidence_tool_policy_revision")
	}
	return nil
}

func validateToolLoopLimits(limits ToolLoopLimits) error {
	if limits.MaxRounds <= 0 || limits.MaxRounds > maxToolLoopCount ||
		limits.MaxCalls <= 0 || limits.MaxCalls > maxToolLoopCount ||
		limits.MaxCallsPerRound <= 0 || limits.MaxCallsPerRound > limits.MaxCalls ||
		limits.MaxResultBytes <= 0 || limits.MaxResultBytes > maxPayloadBytes ||
		limits.MaxEvidenceBytes <= 0 || limits.MaxEvidenceBytes > maxPayloadBytes ||
		limits.MaxResultBytes > limits.MaxEvidenceBytes {
		return invalidEvidenceTools("evidence_tool_limits")
	}
	return nil
}

func canonicalEvidenceToolName(name string) bool {
	return utf8.ValidString(name) && name != "" && len(name) <= MaxEvidenceToolNameBytes &&
		name == strings.TrimSpace(name) && !strings.ContainsRune(name, '\x00')
}

func nilToolDefinition(definition tool.Definition) bool {
	if definition == nil {
		return true
	}
	value := reflect.ValueOf(definition)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func invalidEvidenceTools(field string) error {
	return &DefinitionError{Kind: DefinitionInvalidEvidenceTools, Field: field}
}

func validateInferenceBinding(binding InferenceBinding) error {
	if nilClient(binding.Client) {
		return &DefinitionError{Kind: DefinitionInvalidClient, Field: "client"}
	}
	if err := binding.Model.Validate(); err != nil {
		return &DefinitionError{Kind: DefinitionInvalidModel, Field: "model", Cause: err}
	}
	if err := binding.Model.Key().Validate(); err != nil {
		return &DefinitionError{Kind: DefinitionInvalidModel, Field: "model_key", Cause: err}
	}
	if field := invalidSamplingField(binding.Model.Sampling); field != "" {
		return &DefinitionError{Kind: DefinitionInvalidModel, Field: string(field)}
	}
	return nil
}

type samplingField string

const (
	samplingTemperatureField samplingField = "model.sampling.temperature"
	samplingTopPField        samplingField = "model.sampling.top_p"
	samplingEffortField      samplingField = "model.sampling.effort"
)

func invalidSamplingField(sampling model.Sampling) samplingField {
	if nonFinite(sampling.Temperature) {
		return samplingTemperatureField
	}
	if nonFinite(sampling.TopP) {
		return samplingTopPField
	}
	if !sampling.Effort.Valid() {
		return samplingEffortField
	}
	return ""
}

func nonFinite(value *float64) bool {
	return value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0))
}

func invalidLimits(limits Limits) bool {
	return limits.InputBytes <= 0 || limits.InputBytes > maxPayloadBytes ||
		limits.OutputBytes <= 0 || limits.OutputBytes > maxPayloadBytes
}

func freezeDefinition(options *definitionOptions) (Definition, error) {
	named := InferenceBinding{Client: options.named.Client, Model: options.named.Model.Clone()}
	namedRevision := ""
	if options.modelSource == ModelSourceNamed {
		var err error
		namedRevision, err = digestModelPolicy(named.Model)
		if err != nil {
			return Definition{}, &DefinitionError{Kind: DefinitionInvalidModel, Field: "model_policy", Cause: err}
		}
	}
	descriptor := DefinitionDescriptor{
		Name: options.name, Participation: options.participation, ModelSource: options.modelSource,
		PromptRevision: options.promptRevision, PromptSHA256: sha256.Sum256([]byte(options.systemPrompt)),
		PolicyRevision: options.policyRevision, TimeoutNanos: int64(options.timeout), Limits: options.limits,
		RetryPolicy: options.retryPolicy,
	}
	var output *inference.OutputSchema
	if options.output != nil {
		clone := options.output.Clone()
		output = &clone
		outputDigest, err := digestOutputPolicy(clone)
		if err != nil {
			return Definition{}, err
		}
		descriptor.OutputSchemaName = clone.Name
		descriptor.OutputSchemaSHA256 = outputDigest
		descriptor.StructuredOutputRevision = inference.StructuredOutputRevision
	}
	evidence := options.evidence.Clone()
	if evidencePolicyEnabled(evidence) {
		definitionDigest, namesDigest, err := digestEvidenceToolPolicy(evidence)
		if err != nil {
			return Definition{}, err
		}
		descriptor.EvidenceToolPolicyRevision = evidence.Revision
		descriptor.EvidenceToolDefinitionsSHA256 = definitionDigest
		descriptor.EvidenceProducedToolNamesSHA256 = namesDigest
		descriptor.EvidenceToolLimits = evidence.Limits
		descriptor.EvidenceToolDefinitionCount = len(evidence.Definitions)
		descriptor.StructuredOutputWithTools = true
	}
	if options.modelSource == ModelSourceNamed {
		descriptor.NamedModelKey = named.Model.Key()
		descriptor.NamedModelPolicyRevision = namedRevision
	}
	if err := descriptor.Validate(); err != nil {
		return Definition{}, err
	}
	policyDigest, err := digestDescriptorPolicy(descriptor)
	if err != nil {
		return Definition{}, err
	}
	return Definition{state: &definitionState{
		descriptor: descriptor, policyDigest: policyDigest, timeout: options.timeout,
		systemPrompt: options.systemPrompt, named: named, output: output,
		evidence: evidence,
	}}, nil
}

func digestEvidenceToolPolicy(policy EvidenceToolPolicy) ([sha256.Size]byte, [sha256.Size]byte, error) {
	definitions := appendCanonicalString(nil, evidenceDefinitionCatalogDigestDomain)
	definitions = appendCanonicalString(definitions, policy.Revision)
	definitions = appendToolLoopLimits(definitions, policy.Limits)
	definitions = binary.BigEndian.AppendUint64(definitions, uint64(len(policy.Definitions)))
	names := appendCanonicalString(nil, evidenceProducedNamesDigestDomain)
	names = binary.BigEndian.AppendUint64(names, uint64(len(policy.Definitions)))
	for _, definition := range policy.Definitions {
		produced := definition.ProducedToolNames()
		infos := definition.ToolInfos()
		if len(produced) == 0 || len(produced) != len(infos) {
			return [sha256.Size]byte{}, [sha256.Size]byte{}, &RevisionError{}
		}
		definitions = appendCanonicalString(definitions, definition.Name())
		definitions = binary.BigEndian.AppendUint64(definitions, uint64(definition.Requirements()))
		definitions = binary.BigEndian.AppendUint64(definitions, uint64(len(infos)))
		names = binary.BigEndian.AppendUint64(names, uint64(len(produced)))
		for index := range infos {
			canonicalSchema, err := validateEvidenceToolInfo(infos[index])
			if err != nil || infos[index].Name != produced[index] {
				return [sha256.Size]byte{}, [sha256.Size]byte{}, &RevisionError{Cause: err}
			}
			definitions = appendCanonicalString(definitions, produced[index])
			definitions = appendCanonicalString(definitions, infos[index].Desc)
			definitions = appendCanonicalBytes(definitions, canonicalSchema)
			names = appendCanonicalString(names, produced[index])
		}
	}
	return sha256.Sum256(definitions), sha256.Sum256(names), nil
}

func appendToolLoopLimits(material []byte, limits ToolLoopLimits) []byte {
	material = appendCanonicalInt(material, limits.MaxRounds)
	material = appendCanonicalInt(material, limits.MaxCalls)
	material = appendCanonicalInt(material, limits.MaxCallsPerRound)
	material = appendCanonicalInt(material, limits.MaxResultBytes)
	return appendCanonicalInt(material, limits.MaxEvidenceBytes)
}

func appendCanonicalString(material []byte, value string) []byte {
	return appendCanonicalBytes(material, []byte(value))
}

func appendCanonicalBytes(material, value []byte) []byte {
	material = binary.BigEndian.AppendUint64(material, uint64(len(value)))
	return append(material, value...)
}

func appendCanonicalInt(material []byte, value int) []byte {
	// #nosec G115 -- validated non-negative policy values fit into int64.
	return binary.BigEndian.AppendUint64(material, uint64(value))
}

// digestOutputPolicy hashes the deterministic, secret-free identity of every
// model-facing output-policy field except Name, which is carried separately in
// the descriptor. Schema JSON is compacted first, so insignificant whitespace
// cannot create policy drift. Description and Strict are included because both
// affect provider behavior; only their digest crosses the durable boundary.
func digestOutputPolicy(output inference.OutputSchema) ([sha256.Size]byte, error) {
	var compactSchema bytes.Buffer
	if err := json.Compact(&compactSchema, output.Schema); err != nil {
		return [sha256.Size]byte{}, &RevisionError{Cause: err}
	}
	projection := struct {
		Description string          `json:"description"`
		Schema      json.RawMessage `json:"schema"`
		Strict      bool            `json:"strict"`
	}{
		Description: output.Description,
		Schema:      compactSchema.Bytes(),
		Strict:      output.Strict,
	}
	encoded, err := json.Marshal(projection)
	if err != nil {
		return [sha256.Size]byte{}, &RevisionError{Cause: err}
	}
	return sha256.Sum256(encoded), nil
}

func digestModelPolicy(model model.Model) (string, error) {
	encoded, err := json.Marshal(model)
	if err != nil {
		return "", &RevisionError{Cause: err}
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func digestDescriptorPolicy(descriptor DefinitionDescriptor) (string, error) {
	encoded, err := json.Marshal(descriptor)
	if err != nil {
		return "", &RevisionError{Cause: err}
	}
	if descriptor.EvidenceToolPolicyRevision != "" {
		material := appendCanonicalString(nil, evidencePolicyDigestDomain)
		material = appendCanonicalBytes(material, encoded)
		encoded = material
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

// Name returns the stable registration name.
func (d Definition) Name() Name {
	if d.state == nil {
		return ""
	}
	return d.state.descriptor.Name
}

// Participation returns the definition's fixed execution lane.
func (d Definition) Participation() Participation {
	if d.state == nil {
		return ParticipationUnknown
	}
	return d.state.descriptor.Participation
}

// Timeout returns the definition's exact invocation timeout.
func (d Definition) Timeout() time.Duration {
	if d.state == nil {
		return 0
	}
	return d.state.timeout
}

// Limits returns the definition's immutable payload limits.
func (d Definition) Limits() Limits {
	if d.state == nil {
		return Limits{}
	}
	return d.state.descriptor.Limits
}

// Descriptor returns the definition's secret-free behavioral projection.
func (d Definition) Descriptor() DefinitionDescriptor {
	if d.state == nil {
		return DefinitionDescriptor{}
	}
	return d.state.descriptor
}

// PolicyRevision returns the stable digest of all behavior-affecting fields.
func (d Definition) PolicyRevision() string {
	if d.state == nil {
		return ""
	}
	return d.state.policyDigest
}

// RetryPolicy returns the immutable bounded retry behavior.
func (d Definition) RetryPolicy() RetryPolicy {
	if d.state == nil {
		return RetryPolicyNone
	}
	return d.state.descriptor.RetryPolicy
}

// EvidenceToolPolicy returns an independently owned policy slice when evidence
// tools are enabled.
func (d Definition) EvidenceToolPolicy() (EvidenceToolPolicy, bool) {
	if d.state == nil || !evidencePolicyEnabled(d.state.evidence) {
		return EvidenceToolPolicy{}, false
	}
	return d.state.evidence.Clone(), true
}

// Bind validates runtime collaborators and returns a read-only bound view.
func (d Definition) Bind(ctx context.Context, bindings Bindings) (BoundDefinition, error) {
	if d.state == nil {
		return nil, &BindError{Kind: BindInvalidDefinition}
	}
	if ctx == nil {
		return nil, &BindError{Kind: BindInvalidContext}
	}
	if d.state.descriptor.ModelSource == ModelSourceCurrentLoop && nilResolver(bindings.Models) {
		return nil, &BindError{Kind: BindMissingModelResolver}
	}
	return &boundDefinitionState{definition: d, models: bindings.Models}, nil
}

// BoundDefinition is the sealed runtime view of one immutable definition.
type BoundDefinition interface {
	Name() Name
	Participation() Participation
	Timeout() time.Duration
	Limits() Limits
	Descriptor() DefinitionDescriptor
	ResolveInference(context.Context, uuid.UUID) (InferenceBinding, error)
	SystemPrompt() string
	OutputSchema() (*inference.OutputSchema, bool)
	EvidenceToolPolicy() (EvidenceToolPolicy, bool)
	RetryPolicy() RetryPolicy
	BindEvidenceTools(context.Context, EvidenceBindings) ([]BoundEvidenceTool, error)
	boundDefinition()
}

type boundDefinitionState struct {
	definition Definition
	models     ModelResolver
}

func (b *boundDefinitionState) Name() Name                       { return b.definition.Name() }
func (b *boundDefinitionState) Participation() Participation     { return b.definition.Participation() }
func (b *boundDefinitionState) Timeout() time.Duration           { return b.definition.Timeout() }
func (b *boundDefinitionState) Limits() Limits                   { return b.definition.Limits() }
func (b *boundDefinitionState) Descriptor() DefinitionDescriptor { return b.definition.Descriptor() }
func (b *boundDefinitionState) SystemPrompt() string             { return b.definition.state.systemPrompt }
func (b *boundDefinitionState) EvidenceToolPolicy() (EvidenceToolPolicy, bool) {
	return b.definition.EvidenceToolPolicy()
}
func (b *boundDefinitionState) RetryPolicy() RetryPolicy { return b.definition.RetryPolicy() }
func (b *boundDefinitionState) BindEvidenceTools(
	ctx context.Context,
	bindings EvidenceBindings,
) ([]BoundEvidenceTool, error) {
	if b == nil {
		return nil, &BindError{Kind: BindInvalidDefinition}
	}
	return bindEvidenceTools(ctx, b.definition.state.evidence, bindings)
}
func (*boundDefinitionState) boundDefinition() {}

// OutputSchema returns a fresh clone of the immutable structured-output policy.
func (b *boundDefinitionState) OutputSchema() (*inference.OutputSchema, bool) {
	if b.definition.state.output == nil {
		return nil, false
	}
	clone := b.definition.state.output.Clone()
	return &clone, true
}

// ResolveInference returns a fresh model clone. Current-loop definitions call
// their exact UUID resolver on every invocation and never fall back.
func (b *boundDefinitionState) ResolveInference(ctx context.Context, loopID uuid.UUID) (InferenceBinding, error) {
	if ctx == nil {
		return InferenceBinding{}, &ResolveError{Kind: ResolveInvalidContext}
	}
	if b.definition.state.descriptor.ModelSource == ModelSourceNamed {
		named := b.definition.state.named
		return InferenceBinding{Client: named.Client, Model: named.Model.Clone()}, nil
	}
	if loopID.IsZero() {
		return InferenceBinding{}, &ResolveError{Kind: ResolveInvalidLoopID}
	}
	binding, err := b.models.ResolveHustleModel(ctx, loopID)
	if err != nil {
		return InferenceBinding{}, &ResolveError{Kind: ResolveModelFailed, Cause: err}
	}
	if err := validateResolvedBinding(binding); err != nil {
		return InferenceBinding{}, err
	}
	return InferenceBinding{Client: binding.Client, Model: binding.Model.Clone()}, nil
}

func validateResolvedBinding(binding InferenceBinding) error {
	if nilClient(binding.Client) {
		return &ResolveError{Kind: ResolveInvalidBinding}
	}
	if err := binding.Model.Validate(); err != nil {
		return &ResolveError{Kind: ResolveInvalidBinding, Cause: err}
	}
	if err := binding.Model.Key().Validate(); err != nil {
		return &ResolveError{Kind: ResolveInvalidBinding, Cause: err}
	}
	if invalidSamplingField(binding.Model.Sampling) != "" {
		return &ResolveError{Kind: ResolveInvalidBinding}
	}
	return nil
}

func nilClient(client inference.Client) bool {
	if client == nil {
		return true
	}
	value := reflect.ValueOf(client)
	return nilReflectValue(value)
}

func nilResolver(resolver ModelResolver) bool {
	if resolver == nil {
		return true
	}
	value := reflect.ValueOf(resolver)
	return nilReflectValue(value)
}

func nilReflectValue(value reflect.Value) bool {
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
