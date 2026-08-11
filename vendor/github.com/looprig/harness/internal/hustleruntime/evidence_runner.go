package hustleruntime

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/hustle"
	"github.com/looprig/harness/pkg/tool"
)

const (
	maxEvidenceRequestRequirements = 128
	maxEvidenceRequestCandidates   = 0
	maxEvidenceRequestFieldBytes   = 64 << 10
	maxEvidenceRequestBytes        = 1 << 20
	maxEvidenceResultBlocks        = 4096
	maxEvidenceResultFieldBytes    = 1 << 20
	maxEvidenceResultContentBytes  = 20 << 20
)

// evidenceToolResult is one defensively owned provider-call/result pair.
type evidenceToolResult struct {
	callID  string
	name    string
	content []content.Block
}

// evidenceRunner owns only sequential, read-only evidence execution. It
// deliberately holds no SecurityCeiling: readRoot is the fixed canonical
// workspace root (unchanging for the controller's lifetime), but the ceiling
// is supplied fresh to every run call from that call's own hustle.Request, so
// two runs through the same runner (and the same shared Controller) can be
// bound against two different ceilings — see run's doc comment.
type evidenceRunner struct {
	access         gate.EvidenceAccessEvaluator
	containment    gate.EvidenceContainmentVerifier
	readRoot       string
	allowedKinds   map[string]struct{}
	newExecutionID EvidenceExecutionIDFactory
}

func newEvidenceRunner(
	access gate.EvidenceAccessEvaluator,
	containment gate.EvidenceContainmentVerifier,
	readRoot string,
	allowedKinds []string,
	newExecutionID EvidenceExecutionIDFactory,
) (*evidenceRunner, error) {
	if nilEvidenceRuntimeValue(reflect.ValueOf(access)) ||
		nilEvidenceRuntimeValue(reflect.ValueOf(containment)) ||
		!validEvidenceReadRoot(readRoot) ||
		newExecutionID == nil || len(allowedKinds) == 0 {
		return nil, evidenceError(EvidenceFailureInvalidBinding)
	}
	allowed := make(map[string]struct{}, len(allowedKinds))
	for _, kind := range allowedKinds {
		if kind == "" || kind != strings.TrimSpace(kind) || strings.ContainsRune(kind, '\x00') {
			return nil, evidenceError(EvidenceFailureInvalidBinding)
		}
		if _, duplicate := allowed[kind]; duplicate {
			return nil, evidenceError(EvidenceFailureInvalidBinding)
		}
		allowed[kind] = struct{}{}
	}
	return &evidenceRunner{
		access: access, containment: containment, readRoot: readRoot,
		allowedKinds: allowed, newExecutionID: newExecutionID,
	}, nil
}

func validEvidenceReadRoot(root string) bool {
	return filepath.IsAbs(root) &&
		filepath.Clean(root) == root &&
		!strings.ContainsRune(root, '\x00')
}

func validSecurityCeiling(ceiling string) bool {
	return ceiling != "" &&
		ceiling == strings.TrimSpace(ceiling) &&
		!strings.ContainsRune(ceiling, '\x00')
}

func validEvidenceContainmentPolicy(policy gate.EvidenceContainmentPolicy) bool {
	return validEvidenceReadRoot(policy.ReadRoot) && validSecurityCeiling(policy.SecurityCeiling)
}

// bindEvidenceInvocation builds and validates the concrete catalog from the
// request's explicit causal origin. No construction-time active-loop state is
// accepted by this seam.
func bindEvidenceInvocation(
	ctx context.Context,
	definition hustle.BoundDefinition,
	request hustle.Request,
	readWorkspace *tool.ReadWorkspaceBinding,
) (catalog []hustle.BoundEvidenceTool, err error) {
	if ctx == nil || nilEvidenceRuntimeValue(reflect.ValueOf(definition)) ||
		request.Cause.SessionID.IsZero() || request.Cause.LoopID.IsZero() ||
		readWorkspace == nil {
		return nil, evidenceError(EvidenceFailureInvalidBinding)
	}
	if failure := evidenceContextFailure(ctx); failure != nil {
		return nil, failure
	}
	defer func() {
		if recover() != nil {
			catalog = nil
			err = &evidencePanicError{}
		}
	}()
	catalog, bindErr := definition.BindEvidenceTools(ctx, hustle.EvidenceBindings{
		SessionID: request.Cause.SessionID,
		LoopID:    request.Cause.LoopID,
		ReadWorkspace: &tool.ReadWorkspaceBinding{
			Root: readWorkspace.Root,
		},
	})
	if bindErr != nil {
		return nil, evidenceError(EvidenceFailureInvalidBinding)
	}
	return append([]hustle.BoundEvidenceTool(nil), catalog...), nil
}

// run authorizes and sequentially executes calls against catalog under the
// per-call securityCeiling — never a value stored on the receiver. This is
// the mechanism that lets two RunAndFinalize calls sharing the SAME
// evidenceRunner (and the same Controller/session) be bound against two
// DIFFERENT security ceilings: the runner's own readRoot is fixed, but the
// effective gate.EvidenceContainmentPolicy is assembled fresh from
// securityCeiling on every call, immediately before authorization, so a
// stale ceiling from an earlier call can never leak into a later one.
func (r *evidenceRunner) run(
	ctx context.Context,
	catalog []hustle.BoundEvidenceTool,
	calls []evidenceToolCall,
	limits hustle.ToolLoopLimits,
	securityCeiling string,
) ([]evidenceToolResult, error) {
	if r == nil || nilEvidenceRuntimeValue(reflect.ValueOf(r.access)) ||
		nilEvidenceRuntimeValue(reflect.ValueOf(r.containment)) ||
		!validEvidenceReadRoot(r.readRoot) || !validSecurityCeiling(securityCeiling) ||
		r.newExecutionID == nil ||
		len(r.allowedKinds) == 0 || len(catalog) == 0 || len(calls) == 0 ||
		limits.MaxResultBytes <= 0 || limits.MaxEvidenceBytes < 0 {
		return nil, evidenceError(EvidenceFailureInvalidBinding)
	}
	policy := gate.EvidenceContainmentPolicy{ReadRoot: r.readRoot, SecurityCeiling: securityCeiling}
	if limits.MaxEvidenceBytes == 0 {
		return nil, evidenceError(EvidenceFailureEvidenceTooLarge)
	}
	if failure := evidenceContextFailure(ctx); failure != nil {
		return nil, failure
	}
	byName, err := indexEvidenceCatalog(catalog)
	if err != nil {
		return nil, err
	}
	executionIDs, err := preflightEvidenceIdentities(calls, byName, r.newExecutionID)
	if err != nil {
		return nil, err
	}
	results := make([]evidenceToolResult, 0, len(calls))
	evidenceBytes := 0
	for index, call := range calls {
		remaining := limits.MaxEvidenceBytes - evidenceBytes
		if remaining <= 0 {
			return nil, evidenceError(EvidenceFailureEvidenceTooLarge)
		}
		if failure := evidenceContextFailure(ctx); failure != nil {
			return nil, failure
		}
		bound := byName[call.name]
		concrete := bound.Tool()
		if nilEvidenceRuntimeValue(reflect.ValueOf(concrete)) {
			return nil, evidenceError(EvidenceFailureUnprepared)
		}
		preparer, ok := concrete.(tool.CallPreparer)
		if !ok || nilEvidenceRuntimeValue(reflect.ValueOf(preparer)) {
			return nil, evidenceError(EvidenceFailureUnprepared)
		}
		executionID := executionIDs[index]
		request, artifact, err := prepareEvidenceCall(ctx, preparer, executionID, string(call.input))
		if err != nil {
			return nil, err
		}
		if request.ExecutionID != executionID.String() || request.ToolName != call.name {
			return nil, evidenceError(EvidenceFailureAmbiguousIdentity)
		}
		if err := authorizeEvidenceRequest(
			ctx, r.access, r.containment, policy, r.allowedKinds, request,
		); err != nil {
			return nil, err
		}
		prepared := tool.PreparedCall{
			ExecutionID: executionID,
			Request:     request.Clone(),
			Artifact:    artifact,
		}
		result, err := executeEvidenceCall(ctx, concrete, string(call.input), prepared)
		if err != nil {
			return nil, err
		}
		// design §13.4 (TOCTOU): record this call's observation, if the
		// concrete tool reports one, into whatever ObservationCollector the
		// review adapter attached to ctx (nil for every non-review Hustle
		// run, and a no-op receiver either way — see recordEvidenceObservation).
		recordEvidenceObservation(observationCollectorFromContext(ctx), concrete, request, result)
		owned, encodedBytes, err := ownEvidenceResult(result, limits.MaxResultBytes, remaining)
		if err != nil {
			return nil, err
		}
		if encodedBytes > limits.MaxResultBytes {
			return nil, evidenceError(EvidenceFailureResultTooLarge)
		}
		if encodedBytes > remaining {
			return nil, evidenceError(EvidenceFailureEvidenceTooLarge)
		}
		evidenceBytes += encodedBytes
		results = append(results, evidenceToolResult{
			callID: call.id, name: call.name, content: owned,
		})
	}
	return results, nil
}

func preflightEvidenceIdentities(
	calls []evidenceToolCall,
	byName map[string]hustle.BoundEvidenceTool,
	newExecutionID EvidenceExecutionIDFactory,
) ([]uuid.UUID, error) {
	seenCallIDs := make(map[string]struct{}, len(calls))
	seenExecutionIDs := make(map[uuid.UUID]struct{}, len(calls))
	executionIDs := make([]uuid.UUID, len(calls))
	for index, call := range calls {
		if call.id == "" {
			return nil, evidenceError(EvidenceFailureAmbiguousIdentity)
		}
		if _, duplicate := seenCallIDs[call.id]; duplicate {
			return nil, evidenceError(EvidenceFailureAmbiguousIdentity)
		}
		seenCallIDs[call.id] = struct{}{}
		if _, known := byName[call.name]; !known {
			return nil, evidenceError(EvidenceFailureUnknownTool)
		}
		executionID, err := newExecutionID()
		if err != nil {
			return nil, evidenceError(EvidenceFailureInternal)
		}
		if executionID.IsZero() {
			return nil, evidenceError(EvidenceFailureAmbiguousIdentity)
		}
		if _, duplicate := seenExecutionIDs[executionID]; duplicate {
			return nil, evidenceError(EvidenceFailureAmbiguousIdentity)
		}
		seenExecutionIDs[executionID] = struct{}{}
		executionIDs[index] = executionID
	}
	return executionIDs, nil
}

func indexEvidenceCatalog(catalog []hustle.BoundEvidenceTool) (map[string]hustle.BoundEvidenceTool, error) {
	byName := make(map[string]hustle.BoundEvidenceTool, len(catalog))
	for _, bound := range catalog {
		name := bound.Name()
		if name == "" || nilEvidenceRuntimeValue(reflect.ValueOf(bound.Tool())) {
			return nil, evidenceError(EvidenceFailureInvalidBinding)
		}
		if _, duplicate := byName[name]; duplicate {
			return nil, evidenceError(EvidenceFailureInvalidBinding)
		}
		byName[name] = bound
	}
	return byName, nil
}

func prepareEvidenceCall(
	ctx context.Context,
	preparer tool.CallPreparer,
	executionID uuid.UUID,
	args string,
) (request tool.Request, artifact tool.PreparedArtifact, err error) {
	defer func() {
		if recover() != nil {
			request = tool.Request{}
			artifact = nil
			err = &evidencePanicError{}
		}
	}()
	request, artifact, err = preparer.PrepareCall(ctx, executionID, args)
	if err != nil {
		return tool.Request{}, nil, evidenceError(EvidenceFailurePreparation)
	}
	// Take ownership before any other collaborator can observe the prepared
	// request. A preparer retaining its returned slice cannot mutate the
	// authoritative request used for authorization or execution.
	request, err = ownPreparedEvidenceRequest(request, tool.Request.Clone)
	if err != nil {
		return tool.Request{}, nil, err
	}
	if failure := evidenceContextFailure(ctx); failure != nil {
		return tool.Request{}, nil, failure
	}
	return request, artifact, nil
}

func authorizeEvidenceRequest(
	ctx context.Context,
	access gate.EvidenceAccessEvaluator,
	containment gate.EvidenceContainmentVerifier,
	policy gate.EvidenceContainmentPolicy,
	allowedKinds map[string]struct{},
	request tool.Request,
) (err error) {
	for _, requirement := range request.Requirements {
		if _, allowed := allowedKinds[requirement.Kind]; !allowed ||
			requirement.GrantClass != "" || requirement.GrantTarget != "" ||
			len(requirement.Candidates) != 0 {
			return evidenceError(EvidenceFailureForbiddenCapability)
		}
	}
	if err := tool.ValidateRequest(request); err != nil {
		return evidenceError(EvidenceFailureInvalidRequest)
	}
	if err := verifyEvidenceContainment(ctx, containment, policy, request.Clone()); err != nil {
		return err
	}
	defer func() {
		if recover() != nil {
			err = &evidencePanicError{}
		}
	}()
	for _, requirement := range request.Requirements {
		if failure := evidenceContextFailure(ctx); failure != nil {
			return failure
		}
		configured, accessErr := access.AccessFor(requirement.Clone())
		if accessErr != nil || configured != gate.AccessAllow {
			return evidenceError(EvidenceFailureAccessRefused)
		}
	}
	return nil
}

func verifyEvidenceContainment(
	ctx context.Context,
	containment gate.EvidenceContainmentVerifier,
	policy gate.EvidenceContainmentPolicy,
	request tool.Request,
) (err error) {
	if nilEvidenceRuntimeValue(reflect.ValueOf(containment)) ||
		!validEvidenceContainmentPolicy(policy) {
		return evidenceError(EvidenceFailureInvalidBinding)
	}
	if failure := evidenceContextFailure(ctx); failure != nil {
		return failure
	}
	defer func() {
		if recover() != nil {
			err = &evidencePanicError{}
		}
	}()
	if err := containment.VerifyEvidenceContainment(ctx, policy, request.Clone()); err != nil {
		return evidenceError(EvidenceFailureContainmentRefused)
	}
	if failure := evidenceContextFailure(ctx); failure != nil {
		return failure
	}
	return nil
}

func executeEvidenceCall(
	ctx context.Context,
	concrete tool.InvokableTool,
	args string,
	prepared tool.PreparedCall,
) (result *tool.ToolResult, err error) {
	defer func() {
		if recover() != nil {
			result = nil
			err = &evidencePanicError{}
		}
	}()
	result, err = concrete.InvokableRun(withPreparedEvidenceCall(ctx, prepared), args)
	if err != nil {
		return nil, evidenceError(EvidenceFailureExecution)
	}
	if failure := evidenceContextFailure(ctx); failure != nil {
		return nil, failure
	}
	return result, nil
}

func ownPreparedEvidenceRequest(
	request tool.Request,
	clone func(tool.Request) tool.Request,
) (tool.Request, error) {
	if err := preflightEvidenceRequest(request); err != nil {
		return tool.Request{}, err
	}
	return clone(request), nil
}

func preflightEvidenceRequest(request tool.Request) error {
	if len(request.Requirements) > maxEvidenceRequestRequirements {
		return evidenceError(EvidenceFailureInvalidRequest)
	}
	total := 0
	add := func(value string) bool {
		if len(value) > maxEvidenceRequestFieldBytes ||
			total > maxEvidenceRequestBytes-len(value) {
			return false
		}
		total += len(value)
		return true
	}
	for _, value := range []string{
		request.ToolName, request.Summary, request.ExecutionID,
		request.Command, request.WorkingDirectory,
	} {
		if !add(value) {
			return evidenceError(EvidenceFailureInvalidRequest)
		}
	}
	for _, requirement := range request.Requirements {
		if len(requirement.Candidates) > maxEvidenceRequestCandidates {
			return evidenceError(EvidenceFailureForbiddenCapability)
		}
		for _, value := range []string{
			requirement.Kind, requirement.Scope, requirement.Match,
			requirement.Description, requirement.GrantClass, requirement.GrantTarget,
		} {
			if !add(value) {
				return evidenceError(EvidenceFailureInvalidRequest)
			}
		}
	}
	return nil
}

func ownEvidenceResult(
	result *tool.ToolResult,
	maxResultBytes int,
	remainingEvidenceBytes int,
) ([]content.Block, int, error) {
	return ownEvidenceResultWithEncoder(
		result, maxResultBytes, remainingEvidenceBytes, content.MarshalBlocks,
	)
}

func ownEvidenceResultWithEncoder(
	result *tool.ToolResult,
	maxResultBytes int,
	remainingEvidenceBytes int,
	encode func([]content.Block) ([]byte, error),
) ([]content.Block, int, error) {
	if result == nil || len(result.Content) == 0 {
		return nil, 0, evidenceError(EvidenceFailureInvalidResult)
	}
	if maxResultBytes <= 0 || remainingEvidenceBytes <= 0 {
		return nil, 0, evidenceError(EvidenceFailureEvidenceTooLarge)
	}
	if err := preflightEvidenceResultBounds(
		result.Content, maxResultBytes, remainingEvidenceBytes,
	); err != nil {
		return nil, 0, err
	}
	encoded, err := encode(result.Content)
	if err != nil {
		return nil, 0, evidenceError(EvidenceFailureInvalidResult)
	}
	if len(encoded) > maxResultBytes {
		return nil, 0, evidenceError(EvidenceFailureResultTooLarge)
	}
	if len(encoded) > remainingEvidenceBytes {
		return nil, 0, evidenceError(EvidenceFailureEvidenceTooLarge)
	}
	owned, err := content.UnmarshalBlocks(encoded)
	if err != nil || len(owned) == 0 {
		return nil, 0, evidenceError(EvidenceFailureInvalidResult)
	}
	return owned, len(encoded), nil
}

func preflightEvidenceResult(blocks []content.Block, configuredBytes int) error {
	return preflightEvidenceResultBounds(blocks, configuredBytes, configuredBytes)
}

func preflightEvidenceResultBounds(
	blocks []content.Block,
	maxResultBytes int,
	remainingEvidenceBytes int,
) error {
	if len(blocks) == 0 || len(blocks) > maxEvidenceResultBlocks {
		return evidenceError(EvidenceFailureInvalidResult)
	}
	total := 0
	for _, block := range blocks {
		text, ok := block.(*content.TextBlock)
		if !ok || text == nil {
			return evidenceError(EvidenceFailureInvalidResult)
		}
		size := len(text.Text)
		if size > maxEvidenceResultFieldBytes {
			return evidenceError(EvidenceFailureResultTooLarge)
		}
		if total > maxEvidenceResultContentBytes-size {
			return evidenceError(EvidenceFailureResultTooLarge)
		}
		total += size
		if total > maxResultBytes {
			return evidenceError(EvidenceFailureResultTooLarge)
		}
		if total > remainingEvidenceBytes {
			return evidenceError(EvidenceFailureEvidenceTooLarge)
		}
	}
	return nil
}

func evidenceContextFailure(ctx context.Context) error {
	if ctx == nil {
		return evidenceError(EvidenceFailureInvalidBinding)
	}
	switch ctx.Err() {
	case context.Canceled:
		return evidenceError(EvidenceFailureCanceled)
	case context.DeadlineExceeded:
		return evidenceError(EvidenceFailureDeadline)
	default:
		return nil
	}
}

func nilEvidenceRuntimeValue(value reflect.Value) bool {
	if !value.IsValid() {
		return true
	}
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func evidenceError(reason EvidenceFailureReason) *EvidenceError {
	return &EvidenceError{Reason: reason}
}

// evidencePanicError marks a recovered collaborator panic without retaining its
// value. It unwraps to the public closed failure while allowing the
// controller-owned worker boundary to report the unexpected collaborator.
type evidencePanicError struct{}

func (*evidencePanicError) Error() string { return evidenceError(EvidenceFailureInternal).Error() }
func (*evidencePanicError) Unwrap() error { return evidenceError(EvidenceFailureInternal) }
