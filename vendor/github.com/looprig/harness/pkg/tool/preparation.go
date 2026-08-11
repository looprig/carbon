package tool

// Tool preparation boundary (typed prepared access requests).
//
// Permission evaluation never parses raw tool arguments. Each tool owns a
// preparation step that decodes and validates its arguments, normalizes
// commands, URLs, and paths, resolves canonical resource identities, and
// produces one typed Request carrying every required capability. Invalid input
// fails during preparation and never reaches the permission gate.
//
// Tools classify capabilities and enforcement boundaries; they never decide
// Deny, Gated, or Allow. That three-state decision belongs to the gate
// evaluator, which consumes these types without any tool-specific field
// extraction.

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// CapabilityCommandExecute is the normalized capability kind for starting a
// command. Every prepared command.execute requirement is command-backed: it
// carries GrantClassCommandStart and the exact normalized command as its grant
// target.
const CapabilityCommandExecute = "command.execute"

// GrantClassCommandStart is the enforcement class of a single-spawn,
// exact-command start grant. A saved wildcard or family rule may satisfy the
// gate decision, but issuance under this class remains exact-command.
const GrantClassCommandStart = "command.start.v1"

// RuleCandidate is the exact reusable allow rule displayed to the user and
// offered for durable persistence after a workspace approval. It contains no
// grant or token material; GrantClass and GrantTarget describe only the
// structural enforcement contract a future match must preserve.
type RuleCandidate struct {
	Kind        string `json:"kind"`
	Match       string `json:"match"`
	Description string `json:"description"`
	GrantClass  string `json:"grant_class,omitempty"`
	GrantTarget string `json:"grant_target,omitempty"`
}

// Requirement describes one normalized capability needed by a prepared tool
// call. Scope is used only for access routing, Match only for stored-rule
// matching, and Description only for bounded display. GrantClass and
// GrantTarget are an optional pair: both empty means the direct tool enforces
// the approved resource itself, while a populated pair requests one
// post-decision executor grant.
type Requirement struct {
	Kind        string          `json:"kind"`
	Scope       string          `json:"scope"`
	Match       string          `json:"match"`
	Description string          `json:"description"`
	GrantClass  string          `json:"grant_class,omitempty"`
	GrantTarget string          `json:"grant_target,omitempty"`
	Candidates  []RuleCandidate `json:"candidates,omitempty"`
}

// Request is a prepared, validated access request. Execution binding fields
// are required whenever any requirement requests a grant. They are ordinary
// grant inputs, never minted token material. Pure tools may prepare an empty
// request with no requirements.
type Request struct {
	ToolName           string        `json:"tool_name,omitempty"`
	Summary            string        `json:"summary,omitempty"`
	ExecutionID        string        `json:"execution_id,omitempty"`
	Command            string        `json:"command,omitempty"`
	WorkingDirectory   string        `json:"working_directory,omitempty"`
	ExpiresAtUnixMilli int64         `json:"expires_at_unix_milli,omitempty"`
	Requirements       []Requirement `json:"requirements,omitempty"`
}

// Clone returns a deep copy sharing no backing storage with the receiver.
func (r Request) Clone() Request {
	out := r
	if r.Requirements != nil {
		out.Requirements = make([]Requirement, len(r.Requirements))
		for i, requirement := range r.Requirements {
			out.Requirements[i] = requirement.Clone()
		}
	}
	return out
}

// Clone returns a deep copy sharing no backing storage with the receiver.
func (r Requirement) Clone() Requirement {
	out := r
	if r.Candidates != nil {
		out.Candidates = make([]RuleCandidate, len(r.Candidates))
		copy(out.Candidates, r.Candidates)
	}
	return out
}

// RequestValidationErrorKind classifies an invalid prepared request.
type RequestValidationErrorKind string

const (
	RequestFieldInvalid          RequestValidationErrorKind = "field_invalid"
	RequestRequirementsDuplicate RequestValidationErrorKind = "requirement_duplicate"
	RequestCandidatesDuplicate   RequestValidationErrorKind = "candidate_duplicate"
	RequestGrantPairInvalid      RequestValidationErrorKind = "grant_pair_invalid"
	RequestCommandGrantInvalid   RequestValidationErrorKind = "command_grant_invalid"
	RequestGrantBindingMissing   RequestValidationErrorKind = "grant_binding_missing"
)

// RequestValidationError reports a prepared-request invariant violation.
type RequestValidationError struct {
	Kind  RequestValidationErrorKind
	Field string
}

func (e *RequestValidationError) Error() string {
	return fmt.Sprintf("tool: invalid request (%s): %s", e.Kind, e.Field)
}

// ValidateRequest validates all normalized request, requirement, candidate,
// and exact-command grant invariants.
func ValidateRequest(request Request) error {
	for field, value := range map[string]string{
		"tool_name": request.ToolName,
		"summary":   request.Summary,
	} {
		if value != "" && !normalizedRequestText(value) {
			return &RequestValidationError{Kind: RequestFieldInvalid, Field: field}
		}
	}

	seenRequirements := make(map[string]struct{}, len(request.Requirements))
	needsGrantBinding := false
	for i, requirement := range request.Requirements {
		prefix := fmt.Sprintf("requirements[%d]", i)
		if err := validateRequirement(request, requirement, prefix); err != nil {
			return err
		}
		key := strings.Join([]string{requirement.Kind, requirement.Scope, requirement.Match, requirement.GrantClass, requirement.GrantTarget}, "\x00")
		if _, exists := seenRequirements[key]; exists {
			return &RequestValidationError{Kind: RequestRequirementsDuplicate, Field: prefix}
		}
		seenRequirements[key] = struct{}{}
		if requirement.GrantClass != "" {
			needsGrantBinding = true
		}
	}

	if needsGrantBinding {
		for field, value := range map[string]string{
			"execution_id":      request.ExecutionID,
			"command":           request.Command,
			"working_directory": request.WorkingDirectory,
		} {
			if !normalizedRequestText(value) {
				return &RequestValidationError{Kind: RequestGrantBindingMissing, Field: field}
			}
		}
		if request.ExpiresAtUnixMilli <= 0 {
			return &RequestValidationError{Kind: RequestGrantBindingMissing, Field: "expires_at_unix_milli"}
		}
	}
	return nil
}

func validateRequirement(request Request, requirement Requirement, prefix string) error {
	if !normalizedRequestText(requirement.Kind) {
		return &RequestValidationError{Kind: RequestFieldInvalid, Field: prefix + ".kind"}
	}
	if requirement.Scope != "" && !normalizedRequestText(requirement.Scope) {
		return &RequestValidationError{Kind: RequestFieldInvalid, Field: prefix + ".scope"}
	}
	if !normalizedRequestText(requirement.Match) {
		return &RequestValidationError{Kind: RequestFieldInvalid, Field: prefix + ".match"}
	}
	if !normalizedRequestText(requirement.Description) {
		return &RequestValidationError{Kind: RequestFieldInvalid, Field: prefix + ".description"}
	}
	if (requirement.GrantClass == "") != (requirement.GrantTarget == "") {
		return &RequestValidationError{Kind: RequestGrantPairInvalid, Field: prefix}
	}
	if requirement.GrantClass != "" && (!normalizedRequestText(requirement.GrantClass) || !normalizedRequestText(requirement.GrantTarget)) {
		return &RequestValidationError{Kind: RequestFieldInvalid, Field: prefix + ".grant"}
	}
	if requirement.Kind == CapabilityCommandExecute {
		// Every command.execute requirement is command-backed: profile command
		// state is global (empty scope), the grant class is the exact-command
		// single-spawn start class, and the grant target is the exact normalized
		// command the request binds to.
		if requirement.Scope != "" || requirement.GrantClass != GrantClassCommandStart || requirement.GrantTarget != requirement.Match || request.Command != requirement.GrantTarget {
			return &RequestValidationError{Kind: RequestCommandGrantInvalid, Field: prefix}
		}
	}

	seenCandidates := make(map[string]struct{}, len(requirement.Candidates))
	for i, candidate := range requirement.Candidates {
		candidatePrefix := fmt.Sprintf("%s.candidates[%d]", prefix, i)
		if err := validateRuleCandidate(requirement, candidate, candidatePrefix); err != nil {
			return err
		}
		key := strings.Join([]string{candidate.Kind, candidate.Match, candidate.GrantClass, candidate.GrantTarget}, "\x00")
		if _, exists := seenCandidates[key]; exists {
			return &RequestValidationError{Kind: RequestCandidatesDuplicate, Field: candidatePrefix}
		}
		seenCandidates[key] = struct{}{}
	}
	return nil
}

func validateRuleCandidate(requirement Requirement, candidate RuleCandidate, prefix string) error {
	if !normalizedRequestText(candidate.Kind) || candidate.Kind != requirement.Kind {
		return &RequestValidationError{Kind: RequestFieldInvalid, Field: prefix + ".kind"}
	}
	if !normalizedRequestText(candidate.Match) {
		return &RequestValidationError{Kind: RequestFieldInvalid, Field: prefix + ".match"}
	}
	if !normalizedRequestText(candidate.Description) {
		return &RequestValidationError{Kind: RequestFieldInvalid, Field: prefix + ".description"}
	}
	if (candidate.GrantClass == "") != (candidate.GrantTarget == "") {
		return &RequestValidationError{Kind: RequestGrantPairInvalid, Field: prefix}
	}
	if candidate.GrantClass != "" && (!normalizedRequestText(candidate.GrantClass) || !normalizedRequestText(candidate.GrantTarget)) {
		return &RequestValidationError{Kind: RequestFieldInvalid, Field: prefix + ".grant"}
	}
	if candidate.GrantClass != requirement.GrantClass || candidate.GrantTarget != requirement.GrantTarget {
		return &RequestValidationError{Kind: RequestFieldInvalid, Field: prefix + ".grant"}
	}
	return nil
}

func normalizedRequestText(value string) bool {
	return value != "" && utf8.ValidString(value) && strings.TrimSpace(value) == value && !strings.ContainsRune(value, '\x00')
}
