package gate

import (
	"context"
	"fmt"

	"github.com/looprig/harness/pkg/tool"
)

// CurrentGrantVersion is the structural grant ABI version understood by Gate.
const CurrentGrantVersion uint16 = 1

// RuleMatcher checks independently stored deny and allow rules for one
// normalized requirement. Evaluator always checks every deny before consulting
// any allow.
type RuleMatcher interface {
	MatchesDeny(context.Context, tool.Requirement) (bool, error)
	MatchesAllow(context.Context, tool.Requirement) (bool, error)
}

// RuleWriter atomically persists a complete batch of displayed reusable allow
// candidates. Returning an error means none of the candidates were persisted.
type RuleWriter interface {
	WriteRules(context.Context, []tool.RuleCandidate) error
}

// GrantIssuer mints one structural, execution-bound grant token. The signature
// is intentionally dependency-free and is satisfied structurally by an
// enforcing executor without importing harness.
type GrantIssuer interface {
	GrantVersion() uint16
	IssueGrant(ctx context.Context, executionID, command, cwd, kind, scope, class, target string, expiryUnixMilli int64) (string, error)
}

// Evaluation is the result of evaluating one complete prepared request. Denied
// and Unmet are combined sets in original request order. Candidates contains
// every reusable candidate displayed for the unmet set.
type Evaluation struct {
	Denied     []tool.Requirement
	Unmet      []tool.Requirement
	Candidates []tool.RuleCandidate

	request           tool.Request
	grantRequirements []tool.Requirement
	candidates        []tool.RuleCandidate
	denial            DenialReason
}

// DenialReason classifies why an unapproved resolution was not approved. It
// names gate decision stages only, never who denied or which rule matched.
type DenialReason string

const (
	DenialUnspecified DenialReason = ""
	DenialStructural  DenialReason = "structural"
	DenialRefused     DenialReason = "refused"
)

// Resolution is the live result of applying an approval action. Grants and
// denial routing metadata are deliberately excluded from JSON: they may only
// travel through the live authorization path, never a prompt, display,
// journal, or audit payload.
type Resolution struct {
	Approved          bool         `json:"approved"`
	Grants            []string     `json:"-"`
	Denial            DenialReason `json:"-"`
	DenialDescription string       `json:"-"`
}

// ApprovalPrompt is the single combined user-facing approval for one prepared
// request: the request being decided, every unmet gated requirement, and every
// reusable candidate displayed for persistence. It never carries tokens.
type ApprovalPrompt struct {
	Request    tool.Request
	Unmet      []tool.Requirement
	Candidates []tool.RuleCandidate
}

// Approver resolves one combined approval prompt to exactly one of the three
// approval actions. It is consulted at most once per authorized call, only by
// an interactively constructed evaluator, and any error fails closed.
type Approver interface {
	RequestApproval(ctx context.Context, prompt ApprovalPrompt) (ApprovalAction, error)
}

// Evaluator combines structural access, durable rules, approval persistence,
// and post-decision grant issuance without importing an enforcement package.
//
// Construction explicitly selects interactive or headless interaction:
// NewInteractiveEvaluator requires both an approver and a durable rule writer
// so all three approval actions are honest, while NewHeadlessEvaluator accepts
// neither, never prompts, and resolves an unmet gated requirement as a typed
// approval-required denial.
type Evaluator struct {
	access      AccessBindings
	matcher     RuleMatcher
	writer      RuleWriter
	issuer      GrantIssuer
	approver    Approver
	interactive bool
}

// EvaluationErrorKind classifies a fail-closed evaluation dependency failure.
type EvaluationErrorKind string

const (
	EvaluationRuleMatchFailed EvaluationErrorKind = "rule_match_failed"
	// EvaluationDenied is retained for source compatibility.
	// Deprecated: configured, stored, and approval denials return an unapproved Resolution.
	EvaluationDenied                  EvaluationErrorKind = "denied"
	EvaluationActionInvalid           EvaluationErrorKind = "action_invalid"
	EvaluationApproverMissing         EvaluationErrorKind = "approver_missing"
	EvaluationApprovalRequired        EvaluationErrorKind = "approval_required"
	EvaluationApprovalFailed          EvaluationErrorKind = "approval_failed"
	EvaluationWriterMissing           EvaluationErrorKind = "writer_missing"
	EvaluationWriteFailed             EvaluationErrorKind = "write_failed"
	EvaluationIssuerMissing           EvaluationErrorKind = "issuer_missing"
	EvaluationGrantVersionUnsupported EvaluationErrorKind = "grant_version_unsupported"
	EvaluationGrantFailed             EvaluationErrorKind = "grant_failed"
)

// EvaluationError reports a dependency failure during evaluation.
type EvaluationError struct {
	Kind        EvaluationErrorKind
	Requirement string
	Cause       error
}

func (e *EvaluationError) Error() string {
	return fmt.Sprintf("gate: evaluate %s for %s: %v", e.Kind, e.Requirement, e.Cause)
}

func (e *EvaluationError) Unwrap() error { return e.Cause }

// NewInteractiveEvaluator constructs an interactive evaluator. Interactive
// construction requires both an approver and a durable rule writer so all
// three approval actions (Approve, Approve always for this workspace, Deny)
// are honest; a missing approver or writer fails construction.
func NewInteractiveEvaluator(bindings []AccessBinding, matcher RuleMatcher, approver Approver, writer RuleWriter, issuer GrantIssuer) (*Evaluator, error) {
	if approver == nil {
		return nil, &EvaluationError{Kind: EvaluationApproverMissing, Cause: fmt.Errorf("interactive construction requires an approver")}
	}
	if writer == nil {
		return nil, &EvaluationError{Kind: EvaluationWriterMissing, Cause: fmt.Errorf("interactive construction requires a durable rule writer")}
	}
	evaluator, err := newEvaluator(bindings, matcher, writer, issuer)
	if err != nil {
		return nil, err
	}
	evaluator.approver = approver
	evaluator.interactive = true
	return evaluator, nil
}

// NewHeadlessEvaluator constructs a headless evaluator. Headless construction
// accepts no approver and no rule writer, never prompts, and exposes no
// interactive actions: a gated requirement with no compatible saved rule
// resolves to a typed approval-required denial.
func NewHeadlessEvaluator(bindings []AccessBinding, matcher RuleMatcher, issuer GrantIssuer) (*Evaluator, error) {
	return newEvaluator(bindings, matcher, nil, issuer)
}

// newEvaluator validates access routing, rejects an unsupported grant ABI, and
// binds the optional rule and grant services. Nil services remain fail-closed
// at the point their capability is needed.
func newEvaluator(bindings []AccessBinding, matcher RuleMatcher, writer RuleWriter, issuer GrantIssuer) (*Evaluator, error) {
	access, err := NewAccessBindings(bindings)
	if err != nil {
		return nil, err
	}
	if issuer != nil {
		if version := issuer.GrantVersion(); version != CurrentGrantVersion {
			return nil, &EvaluationError{
				Kind:  EvaluationGrantVersionUnsupported,
				Cause: fmt.Errorf("got %d, want %d", version, CurrentGrantVersion),
			}
		}
	}
	return &Evaluator{access: access, matcher: matcher, writer: writer, issuer: issuer}, nil
}

// Interactive reports whether this evaluator was interactively constructed.
func (e *Evaluator) Interactive() bool { return e.interactive }

// Authorize runs one complete prepared request through the combined gate: it
// evaluates once, opens at most one approval (interactive construction only,
// and only when gated requirements remain unmet), resolves the chosen action,
// and mints fresh execution-bound grants for the approved call.
//
// A configured or stored deny, and an interactive Deny action, return an
// unapproved Resolution with no error and mint nothing. A headless evaluator
// with unmet requirements returns a typed approval-required denial. Any
// dependency or approver failure is a fail-closed error.
func (e *Evaluator) Authorize(ctx context.Context, request tool.Request) (Resolution, error) {
	evaluation, err := e.Evaluate(ctx, request)
	if err != nil {
		return Resolution{}, err
	}
	if len(evaluation.Denied) != 0 {
		return deniedResolution(evaluation.denial, evaluation.Denied), nil
	}
	if len(evaluation.Unmet) == 0 {
		return e.Resolve(ctx, evaluation, ApprovalApprove)
	}
	if !e.interactive {
		return Resolution{}, &EvaluationError{
			Kind:        EvaluationApprovalRequired,
			Requirement: evaluation.Unmet[0].Kind,
			Cause:       fmt.Errorf("headless gate cannot prompt for %d unmet requirement(s)", len(evaluation.Unmet)),
		}
	}
	prompt := ApprovalPrompt{
		Request:    evaluation.request.Clone(),
		Unmet:      cloneRequirements(evaluation.Unmet),
		Candidates: cloneCandidates(evaluation.Candidates),
	}
	action, err := e.approver.RequestApproval(ctx, prompt)
	if err != nil {
		return Resolution{}, &EvaluationError{Kind: EvaluationApprovalFailed, Requirement: evaluation.Unmet[0].Kind, Cause: err}
	}
	if action == ApprovalDeny {
		return deniedResolution(DenialRefused, evaluation.Unmet), nil
	}
	return e.Resolve(ctx, evaluation, action)
}

// Evaluate resolves every access state, then every stored deny, then every
// stored allow. It never serializes approval gates: all unmatched gated
// requirements are returned together as one combined unmet set.
func (e *Evaluator) Evaluate(ctx context.Context, request tool.Request) (Evaluation, error) {
	if err := ctx.Err(); err != nil {
		return Evaluation{}, err
	}
	if err := tool.ValidateRequest(request); err != nil {
		return Evaluation{}, err
	}
	result := Evaluation{request: request.Clone()}
	gated := make([]tool.Requirement, 0, len(request.Requirements))
	for _, requirement := range request.Requirements {
		access, err := e.access.AccessFor(requirement)
		if err != nil {
			return Evaluation{}, err
		}
		switch access {
		case AccessDeny:
			result.Denied = append(result.Denied, requirement.Clone())
			result.denial = DenialStructural
		case AccessGated:
			gated = append(gated, requirement)
		case AccessAllow:
			// An enforcing source configured Allow needs no grant token.
		}
	}
	if len(result.Denied) != 0 {
		return result, nil
	}

	for _, requirement := range gated {
		if e.matcher == nil {
			continue
		}
		matched, err := e.matcher.MatchesDeny(ctx, requirement)
		if err != nil {
			return Evaluation{}, &EvaluationError{Kind: EvaluationRuleMatchFailed, Requirement: requirement.Kind, Cause: err}
		}
		if matched {
			result.Denied = append(result.Denied, requirement.Clone())
			result.denial = DenialRefused
		}
	}
	if len(result.Denied) != 0 {
		return result, nil
	}

	for _, requirement := range gated {
		matched := false
		if e.matcher != nil {
			var err error
			matched, err = e.matcher.MatchesAllow(ctx, requirement)
			if err != nil {
				return Evaluation{}, &EvaluationError{Kind: EvaluationRuleMatchFailed, Requirement: requirement.Kind, Cause: err}
			}
		}
		result.grantRequirements = append(result.grantRequirements, requirement.Clone())
		if matched {
			continue
		}
		result.Unmet = append(result.Unmet, requirement.Clone())
		candidates := cloneCandidates(requirement.Candidates)
		result.Candidates = append(result.Candidates, candidates...)
		result.candidates = append(result.candidates, candidates...)
	}
	return result, nil
}

// Resolve applies one exact approval action. Workspace approval persists the
// entire displayed candidate set in one RuleWriter call before any grant is
// minted; a persistence failure blocks execution. Approve writes nothing. Deny
// and evaluated denials mint no grants.
func (e *Evaluator) Resolve(ctx context.Context, evaluation Evaluation, action ApprovalAction) (Resolution, error) {
	if err := ctx.Err(); err != nil {
		return Resolution{}, err
	}
	if len(evaluation.Denied) != 0 {
		return deniedResolution(evaluation.denial, evaluation.Denied), nil
	}
	switch action {
	case ApprovalDeny:
		return deniedResolution(DenialRefused, evaluation.Unmet), nil
	case ApprovalApprove:
		// Once approval is intentionally ephemeral: nothing is persisted.
	case ApprovalApproveAlwaysWorkspace:
		// An empty displayed-candidate batch is a no-op: there is nothing to
		// persist, so persistence can neither be attempted nor block execution.
		if len(evaluation.candidates) != 0 {
			if e.writer == nil {
				return Resolution{}, &EvaluationError{Kind: EvaluationWriterMissing, Cause: fmt.Errorf("workspace rule writer is not configured")}
			}
			if err := e.writer.WriteRules(ctx, cloneCandidates(evaluation.candidates)); err != nil {
				return Resolution{}, &EvaluationError{Kind: EvaluationWriteFailed, Cause: err}
			}
		}
	default:
		return Resolution{}, &EvaluationError{Kind: EvaluationActionInvalid, Cause: fmt.Errorf("unknown action %q", action)}
	}

	grantCount := 0
	for _, requirement := range evaluation.grantRequirements {
		if requirement.GrantClass != "" {
			grantCount++
		}
	}
	if grantCount == 0 {
		return Resolution{Approved: true}, nil
	}
	if e.issuer == nil {
		return Resolution{}, &EvaluationError{Kind: EvaluationIssuerMissing, Cause: fmt.Errorf("grant issuer is not configured")}
	}
	grants := make([]string, 0, grantCount)
	for _, requirement := range evaluation.grantRequirements {
		if requirement.GrantClass == "" {
			continue
		}
		token, err := e.issuer.IssueGrant(
			ctx,
			evaluation.request.ExecutionID,
			evaluation.request.Command,
			evaluation.request.WorkingDirectory,
			requirement.Kind,
			requirement.Scope,
			requirement.GrantClass,
			requirement.GrantTarget,
			evaluation.request.ExpiresAtUnixMilli,
		)
		if err != nil {
			return Resolution{}, &EvaluationError{Kind: EvaluationGrantFailed, Requirement: requirement.Kind, Cause: err}
		}
		if token == "" {
			return Resolution{}, &EvaluationError{Kind: EvaluationGrantFailed, Requirement: requirement.Kind, Cause: fmt.Errorf("issuer returned empty token")}
		}
		grants = append(grants, token)
	}
	return Resolution{Approved: true, Grants: grants}, nil
}

func deniedResolution(reason DenialReason, requirements []tool.Requirement) Resolution {
	description := ""
	if len(requirements) != 0 {
		description = requirements[0].Description
	}
	return Resolution{Denial: reason, DenialDescription: description}
}

func cloneRequirements(requirements []tool.Requirement) []tool.Requirement {
	if requirements == nil {
		return nil
	}
	out := make([]tool.Requirement, len(requirements))
	for i, requirement := range requirements {
		out[i] = requirement.Clone()
	}
	return out
}

func cloneCandidates(candidates []tool.RuleCandidate) []tool.RuleCandidate {
	if candidates == nil {
		return nil
	}
	out := make([]tool.RuleCandidate, len(candidates))
	copy(out, candidates)
	return out
}
