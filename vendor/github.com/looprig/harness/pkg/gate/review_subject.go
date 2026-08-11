package gate

import (
	"math"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/tool"
)

const (
	ReviewValidationFieldBasis   ReviewValidationField = "basis"
	ReviewValidationFieldDigest  ReviewValidationField = "digest"
	ReviewValidationFieldRequest ReviewValidationField = "request"
	ReviewValidationFieldWire    ReviewValidationField = "wire"
)

const ReviewValidationMismatch ReviewValidationReason = "mismatch"

// Hard request-input bounds constrain work before any request is cloned,
// validated, or projected onto the canonical subject wire.
const (
	MaxPermissionReviewRequestRequirements = 4096
	MaxPermissionReviewRequestCandidates   = 4096
	MaxPermissionReviewRequestStringBytes  = 1 << 20
	MaxPermissionReviewRequestInputBytes   = 1 << 20
)

// ReviewBasis binds a classifier decision to one exact live permission
// request and the policy revisions under which it was reviewed.
type ReviewBasis struct {
	GateID             ID       `json:"gate_id"`
	ToolExecutionID    ID       `json:"tool_execution_id"`
	SubjectDigest      [32]byte `json:"subject_digest"`
	ContextRevision    string   `json:"context_revision"`
	GatePolicyRevision string   `json:"gate_policy_revision"`
	ClassifierRevision string   `json:"classifier_revision"`
	SecurityCeiling    string   `json:"security_ceiling"`
}

// PermissionReviewSubject is the immutable, authority-labeled input to a
// permission classifier.
type PermissionReviewSubject struct {
	Basis   ReviewBasis   `json:"basis"`
	Request tool.Request  `json:"request"`
	Context ReviewContext `json:"context"`
}

// NewPermissionReviewSubject validates, owns, and digest-stamps one subject.
func NewPermissionReviewSubject(
	basis ReviewBasis,
	request tool.Request,
	context ReviewContext,
) (PermissionReviewSubject, error) {
	if reason := permissionReviewBasisPreflightReason(basis); reason != "" {
		return PermissionReviewSubject{}, reviewSubjectError(
			ReviewValidationFieldBasis,
			reason,
		)
	}
	if basis.SubjectDigest != ([32]byte{}) {
		return PermissionReviewSubject{}, reviewSubjectError(
			ReviewValidationFieldDigest,
			ReviewValidationReserved,
		)
	}
	if reason := permissionReviewRequestPreflightReason(request); reason != "" {
		return PermissionReviewSubject{}, reviewSubjectError(
			ReviewValidationFieldRequest,
			reason,
		)
	}
	// Validate the original context before Clone allocates its entry slice.
	if err := validateBuiltReviewContext(context); err != nil {
		return PermissionReviewSubject{}, err
	}
	subject := PermissionReviewSubject{
		Basis:   basis,
		Request: request.Clone(),
		Context: context.Clone(),
	}
	if err := validatePermissionReviewSubject(subject); err != nil {
		return PermissionReviewSubject{}, err
	}
	digest, err := SubjectDigest(subject)
	if err != nil {
		return PermissionReviewSubject{}, err
	}
	subject.Basis.SubjectDigest = digest
	return subject, nil
}

// Clone returns an owned copy of the subject and all nested slices.
func (s PermissionReviewSubject) Clone() PermissionReviewSubject {
	clone := s
	clone.Request = s.Request.Clone()
	clone.Context = s.Context.Clone()
	return clone
}

// SubjectDigest validates the non-digest subject invariants and recomputes its
// canonical digest while deliberately ignoring the stored digest.
func SubjectDigest(subject PermissionReviewSubject) ([32]byte, error) {
	if err := validatePermissionReviewSubject(subject); err != nil {
		return [32]byte{}, err
	}
	return permissionReviewSubjectDigest(subject)
}

func validatePermissionReviewSubject(subject PermissionReviewSubject) error {
	basis := subject.Basis
	if reason := permissionReviewBasisPreflightReason(basis); reason != "" {
		return reviewSubjectError(ReviewValidationFieldBasis, reason)
	}
	if reason := permissionReviewRequestPreflightReason(subject.Request); reason != "" {
		return reviewSubjectError(ReviewValidationFieldRequest, reason)
	}
	if err := tool.ValidateRequest(subject.Request); err != nil {
		return reviewSubjectError(ReviewValidationFieldRequest, ReviewValidationInvalid)
	}
	if subject.Request.ExecutionID != "" {
		executionID, err := uuid.Parse(subject.Request.ExecutionID)
		if err != nil ||
			executionID.String() != subject.Request.ExecutionID ||
			executionID != basis.ToolExecutionID {
			return reviewSubjectError(ReviewValidationFieldRequest, ReviewValidationMismatch)
		}
	}
	if err := validateBuiltReviewContext(subject.Context); err != nil {
		return err
	}
	if basis.ContextRevision != subject.Context.ContextRevision ||
		basis.GatePolicyRevision != subject.Context.GatePolicyRevision ||
		basis.SecurityCeiling != subject.Context.SecurityCeiling {
		return reviewSubjectError(ReviewValidationFieldBasis, ReviewValidationMismatch)
	}
	return nil
}

func permissionReviewBasisPreflightReason(basis ReviewBasis) ReviewValidationReason {
	if basis.GateID.IsZero() || basis.ToolExecutionID.IsZero() {
		return ReviewValidationRequired
	}
	return permissionReviewBasisIdentityPreflightReason(
		basis.ContextRevision,
		basis.GatePolicyRevision,
		basis.ClassifierRevision,
		basis.SecurityCeiling,
	)
}

func permissionReviewBasisIdentityPreflightReason(
	contextRevision string,
	gatePolicyRevision string,
	classifierRevision string,
	securityCeiling string,
) ReviewValidationReason {
	for _, value := range [...]string{
		contextRevision,
		gatePolicyRevision,
		classifierRevision,
		securityCeiling,
	} {
		if value == "" {
			return ReviewValidationRequired
		}
		if !utf8.ValidString(value) {
			return ReviewValidationInvalid
		}
	}
	if len(classifierRevision) > MaxPermissionClassifierRevisionBytes ||
		len(gatePolicyRevision) > MaxPermissionReviewPolicyRevisionBytes ||
		len(contextRevision) > MaxReviewContextRootFieldBytes ||
		len(securityCeiling) > MaxReviewContextRootFieldBytes {
		return ReviewValidationOutOfBounds
	}
	return ""
}

func permissionReviewRequestPreflightReason(request tool.Request) ReviewValidationReason {
	if len(request.Requirements) > MaxPermissionReviewRequestRequirements {
		return ReviewValidationOutOfBounds
	}
	totalInputBytes := 0
	for _, value := range [...]string{
		request.ToolName,
		request.Summary,
		request.ExecutionID,
		request.Command,
		request.WorkingDirectory,
	} {
		reason := addPermissionReviewRequestString(&totalInputBytes, value)
		if reason != "" {
			return reason
		}
	}

	totalCandidates := 0
	for index := range request.Requirements {
		requirement := &request.Requirements[index]
		var ok bool
		totalCandidates, ok = checkedPermissionReviewRequestAdd(
			totalCandidates,
			len(requirement.Candidates),
		)
		if !ok || totalCandidates > MaxPermissionReviewRequestCandidates {
			return ReviewValidationOutOfBounds
		}
		for _, value := range [...]string{
			requirement.Kind,
			requirement.Scope,
			requirement.Match,
			requirement.Description,
			requirement.GrantClass,
			requirement.GrantTarget,
		} {
			reason := addPermissionReviewRequestString(&totalInputBytes, value)
			if reason != "" {
				return reason
			}
		}

		for _, candidate := range requirement.Candidates {
			for _, value := range [...]string{
				candidate.Kind,
				candidate.Match,
				candidate.Description,
				candidate.GrantClass,
				candidate.GrantTarget,
			} {
				reason := addPermissionReviewRequestString(&totalInputBytes, value)
				if reason != "" {
					return reason
				}
			}
		}
	}
	return ""
}

func addPermissionReviewRequestString(
	totalInputBytes *int,
	value string,
) ReviewValidationReason {
	if !utf8.ValidString(value) {
		return ReviewValidationInvalid
	}
	if len(value) > MaxPermissionReviewRequestStringBytes {
		return ReviewValidationOutOfBounds
	}
	total, ok := checkedPermissionReviewRequestAdd(*totalInputBytes, len(value))
	if !ok || total > MaxPermissionReviewRequestInputBytes {
		return ReviewValidationOutOfBounds
	}
	*totalInputBytes = total
	return ""
}

func checkedPermissionReviewRequestAdd(left int, right int) (int, bool) {
	if left < 0 || right < 0 || left > math.MaxInt-right {
		return 0, false
	}
	return left + right, true
}

func preflightPermissionReviewContextProjection(context ReviewContext) error {
	if len(context.Entries) > MaxReviewContextInputEntries {
		return reviewContextError(
			ReviewValidationFieldContextEntry,
			ReviewValidationOutOfBounds,
		)
	}
	totalInputBytes := 0
	for _, value := range [...]string{
		context.ContextRevision,
		context.WorkspaceRoot,
		context.WorkingDirectory,
		context.RetryReason,
		context.SecurityCeiling,
		context.GatePolicyRevision,
	} {
		if !utf8.ValidString(value) {
			return reviewContextError(
				ReviewValidationFieldContext,
				ReviewValidationInvalid,
			)
		}
		if len(value) > MaxReviewContextRootFieldBytes {
			return reviewContextError(
				ReviewValidationFieldContext,
				ReviewValidationOutOfBounds,
			)
		}
		var ok bool
		totalInputBytes, ok = checkedReviewContextAdd(totalInputBytes, len(value))
		if !ok || totalInputBytes > MaxReviewContextInputBytes {
			return reviewContextError(
				ReviewValidationFieldContext,
				ReviewValidationOutOfBounds,
			)
		}
	}
	for index := range context.Entries {
		entry := &context.Entries[index]
		if !utf8.ValidString(string(entry.Origin)) ||
			!utf8.ValidString(string(entry.Kind)) ||
			!utf8.ValidString(entry.Content) {
			return reviewContextError(
				ReviewValidationFieldContextEntry,
				ReviewValidationInvalid,
			)
		}
		if len(entry.Content) > MaxReviewContextEntryInputBytes {
			return reviewContextError(
				ReviewValidationFieldContextEntry,
				ReviewValidationOutOfBounds,
			)
		}
		for _, size := range [...]int{
			len(entry.Origin),
			len(entry.Kind),
			len(entry.Content),
		} {
			var ok bool
			totalInputBytes, ok = checkedReviewContextAdd(totalInputBytes, size)
			if !ok || totalInputBytes > MaxReviewContextInputBytes {
				return reviewContextError(
					ReviewValidationFieldContextEntry,
					ReviewValidationOutOfBounds,
				)
			}
		}
	}
	return nil
}

func validateBuiltReviewContext(context ReviewContext) error {
	if context.Coordinates.SessionID.IsZero() ||
		context.Coordinates.LoopID.IsZero() ||
		context.Coordinates.TurnID.IsZero() ||
		context.Coordinates.StepID.IsZero() {
		return reviewContextError(ReviewValidationFieldContext, ReviewValidationRequired)
	}
	rootText := []string{
		context.ContextRevision,
		context.WorkspaceRoot,
		context.WorkingDirectory,
		context.RetryReason,
		context.SecurityCeiling,
		context.GatePolicyRevision,
	}
	totalInputBytes := 0
	for _, value := range rootText {
		if !utf8.ValidString(value) {
			return reviewContextError(ReviewValidationFieldContext, ReviewValidationInvalid)
		}
		if len(value) > MaxReviewContextRootFieldBytes {
			return reviewContextError(ReviewValidationFieldContext, ReviewValidationOutOfBounds)
		}
		var ok bool
		totalInputBytes, ok = checkedReviewContextAdd(totalInputBytes, len(value))
		if !ok || totalInputBytes > MaxReviewContextInputBytes {
			return reviewContextError(ReviewValidationFieldContext, ReviewValidationOutOfBounds)
		}
	}
	// BuildReviewContext requires a non-empty UTF-8 policy revision but does
	// not retain it in ReviewContext. Account for the smallest input that
	// could have produced this context without guessing it from another
	// independently versioned field.
	const minimumReviewContextPolicyRevisionBytes = 1
	var ok bool
	totalInputBytes, ok = checkedReviewContextAdd(
		totalInputBytes,
		minimumReviewContextPolicyRevisionBytes,
	)
	if !ok || totalInputBytes > MaxReviewContextInputBytes {
		return reviewContextError(ReviewValidationFieldContext, ReviewValidationOutOfBounds)
	}
	if context.ContextRevision == "" ||
		context.WorkspaceRoot == "" ||
		context.WorkingDirectory == "" ||
		context.SecurityCeiling == "" ||
		context.GatePolicyRevision == "" {
		return reviewContextError(ReviewValidationFieldContext, ReviewValidationRequired)
	}
	if !cleanAbsolutePath(context.WorkspaceRoot) ||
		!cleanAbsolutePath(context.WorkingDirectory) {
		return reviewContextError(ReviewValidationFieldContext, ReviewValidationInvalid)
	}
	relative, err := filepath.Rel(context.WorkspaceRoot, context.WorkingDirectory)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return reviewContextError(ReviewValidationFieldContext, ReviewValidationInvalid)
	}
	if len(context.Entries) > MaxReviewContextInputEntries {
		return reviewContextError(ReviewValidationFieldContextEntry, ReviewValidationOutOfBounds)
	}
	if context.Truncation.Applied&^SupportedReviewTruncationMask != 0 ||
		context.Truncation.Material&^SupportedReviewTruncationMask != 0 ||
		context.Truncation.Material&^context.Truncation.Applied != 0 ||
		context.Truncation.OmittedEntries < 0 ||
		context.Truncation.OmittedEntries > MaxReviewContextInputEntries ||
		context.Truncation.OmittedBytes < 0 ||
		context.Truncation.OmittedBytes > MaxReviewContextInputBytes {
		return reviewContextError(ReviewValidationFieldContext, ReviewValidationOutOfBounds)
	}

	currentUser := -1
	activeAction := -1
	omissions := 0
	truncatedEntries := 0
	var explainedNonBudget ReviewTruncationMask
	for index, entry := range context.Entries {
		if len(entry.Content) > MaxReviewContextEntryInputBytes {
			return reviewContextError(
				ReviewValidationFieldContextEntry,
				ReviewValidationOutOfBounds,
			)
		}
		if !utf8.ValidString(string(entry.Origin)) ||
			!utf8.ValidString(string(entry.Kind)) ||
			!utf8.ValidString(entry.Content) ||
			!validReviewContextPair(entry.Origin, entry.Kind) {
			return reviewContextError(ReviewValidationFieldContextEntry, ReviewValidationInvalid)
		}
		if entry.Kind != ReviewContextKindOmission {
			for _, size := range []int{
				len(entry.Origin),
				len(entry.Kind),
				len(entry.Content),
			} {
				var ok bool
				totalInputBytes, ok = checkedReviewContextAdd(totalInputBytes, size)
				if !ok || totalInputBytes > MaxReviewContextInputBytes {
					return reviewContextError(
						ReviewValidationFieldContextEntry,
						ReviewValidationOutOfBounds,
					)
				}
			}
		}
		switch entry.Kind {
		case ReviewContextKindUserMessage:
			currentUser = index
		case ReviewContextKindAssistantToolRequest:
			activeAction = index
		case ReviewContextKindOmission:
			omissions++
			if entry.Truncated ||
				context.Truncation.OmittedEntries <= 0 ||
				entry.Content != reviewContextOmissionMarker(
					context.Truncation.OmittedEntries,
					context.Truncation.OmittedBytes,
				) {
				return reviewContextError(ReviewValidationFieldContextEntry, ReviewValidationInvalid)
			}
		}
		if entry.Truncated {
			truncatedEntries++
			entryMask := reviewTruncationMaskForEntry(entry)
			exercised := context.Truncation.Applied & entryMask
			explainedNonBudget |= exercised
			markerIndex := strings.Index(entry.Content, reviewContextTruncationMarker)
			if exercised == 0 ||
				markerIndex <= 0 ||
				markerIndex+len(reviewContextTruncationMarker) >= len(entry.Content) ||
				strings.Count(entry.Content, reviewContextTruncationMarker) != 1 ||
				materialReviewContextKind(entry.Kind) &&
					context.Truncation.Material&exercised != exercised {
				return reviewContextError(ReviewValidationFieldContextEntry, ReviewValidationInvalid)
			}
		}
	}
	if currentUser < 0 || activeAction < 0 {
		return reviewContextError(ReviewValidationFieldContextEntry, ReviewValidationRequired)
	}
	if context.Entries[activeAction].Truncated {
		return reviewContextError(ReviewValidationFieldContextEntry, ReviewValidationInvalid)
	}
	if context.Truncation.Applied&reviewNonBudgetTruncationMask&^explainedNonBudget != 0 ||
		context.Truncation.Material&reviewNonBudgetTruncationMask&^explainedNonBudget != 0 {
		return reviewContextError(ReviewValidationFieldContext, ReviewValidationInvalid)
	}
	hasOmissions := context.Truncation.OmittedEntries > 0
	hasBudgetTruncation := context.Truncation.Applied&reviewBudgetTruncationMask != 0
	if (omissions == 1) != hasOmissions ||
		hasBudgetTruncation != hasOmissions ||
		context.Truncation.Material&reviewBudgetTruncationMask !=
			context.Truncation.Applied&reviewBudgetTruncationMask ||
		!hasOmissions && context.Truncation.OmittedBytes != 0 ||
		omissions > 1 ||
		truncatedEntries > 0 && context.Truncation.Applied == 0 ||
		context.Truncation.Applied == 0 &&
			(context.Truncation.Material != 0 || hasOmissions || truncatedEntries > 0) {
		return reviewContextError(ReviewValidationFieldContext, ReviewValidationInvalid)
	}
	if err := validateReconstructedReviewContextInputBounds(
		context,
		omissions,
		totalInputBytes,
	); err != nil {
		return err
	}
	return nil
}

func validateReconstructedReviewContextInputBounds(
	context ReviewContext,
	omissions int,
	retainedInputBytes int,
) error {
	retainedEntries := len(context.Entries) - omissions
	originalEntries, ok := checkedReviewContextAdd(
		retainedEntries,
		context.Truncation.OmittedEntries,
	)
	if !ok || originalEntries > MaxReviewContextInputEntries {
		return reviewContextError(
			ReviewValidationFieldContextEntry,
			ReviewValidationOutOfBounds,
		)
	}
	maxDistributedOmittedBytes, ok := checkedReviewContextMultiply(
		context.Truncation.OmittedEntries,
		MaxReviewContextEntryInputBytes,
	)
	if !ok ||
		context.Truncation.OmittedBytes > maxDistributedOmittedBytes {
		return reviewContextError(
			ReviewValidationFieldContextEntry,
			ReviewValidationOutOfBounds,
		)
	}

	originalInputBytes, ok := checkedReviewContextAdd(
		retainedInputBytes,
		context.Truncation.OmittedBytes,
	)
	if !ok || originalInputBytes > MaxReviewContextInputBytes {
		return reviewContextError(
			ReviewValidationFieldContextEntry,
			ReviewValidationOutOfBounds,
		)
	}
	minimumLabelBytes, ok := minimumOriginalReviewContextEntryLabelBytes()
	if !ok {
		return reviewContextError(
			ReviewValidationFieldContextEntry,
			ReviewValidationInvalid,
		)
	}
	for omitted := 0; omitted < context.Truncation.OmittedEntries; omitted++ {
		originalInputBytes, ok = checkedReviewContextAdd(
			originalInputBytes,
			minimumLabelBytes,
		)
		if !ok || originalInputBytes > MaxReviewContextInputBytes {
			return reviewContextError(
				ReviewValidationFieldContextEntry,
				ReviewValidationOutOfBounds,
			)
		}
	}
	return nil
}

func checkedReviewContextMultiply(left int, right int) (int, bool) {
	if left < 0 || right < 0 ||
		left != 0 && right > math.MaxInt/left {
		return 0, false
	}
	return left * right, true
}

func minimumOriginalReviewContextEntryLabelBytes() (int, bool) {
	pairs := [...]struct {
		origin ReviewContextOrigin
		kind   ReviewContextKind
	}{
		{ReviewContextOriginUser, ReviewContextKindUserMessage},
		{ReviewContextOriginAssistant, ReviewContextKindAssistantMessage},
		{ReviewContextOriginAssistant, ReviewContextKindAssistantToolRequest},
		{ReviewContextOriginTool, ReviewContextKindToolResult},
		{ReviewContextOriginRuntime, ReviewContextKindRuntimeContext},
		{ReviewContextOriginExternal, ReviewContextKindExternalContent},
	}
	minimum := 0
	for _, pair := range pairs {
		if !validReviewContextPair(pair.origin, pair.kind) ||
			pair.origin == ReviewContextOriginOmission ||
			pair.kind == ReviewContextKindOmission {
			return 0, false
		}
		size, ok := checkedReviewContextAdd(len(pair.origin), len(pair.kind))
		if !ok {
			return 0, false
		}
		if minimum == 0 || size < minimum {
			minimum = size
		}
	}
	return minimum, minimum > 0
}

const reviewBudgetTruncationMask = ReviewTruncationEntryCount |
	ReviewTruncationTotalBytes |
	ReviewTruncationEstimatedTokens

const reviewNonBudgetTruncationMask = ReviewTruncationUserEntry |
	ReviewTruncationAssistantEntry |
	ReviewTruncationToolEntry |
	ReviewTruncationBlock |
	ReviewTruncationActiveAction

func reviewTruncationMaskForEntry(entry ReviewContextEntry) ReviewTruncationMask {
	switch entry.Kind {
	case ReviewContextKindUserMessage:
		return ReviewTruncationUserEntry | ReviewTruncationBlock
	case ReviewContextKindAssistantMessage, ReviewContextKindAssistantToolRequest:
		return ReviewTruncationAssistantEntry | ReviewTruncationBlock
	case ReviewContextKindToolResult:
		return ReviewTruncationToolEntry | ReviewTruncationBlock
	default:
		return ReviewTruncationBlock
	}
}

func reviewSubjectError(field ReviewValidationField, reason ReviewValidationReason) error {
	return &ReviewValidationError{Field: field, Reason: reason}
}
