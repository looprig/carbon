package loop

import (
	"context"

	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/tool"
)

type toolUseIDKey struct{}
type preparedKey struct{}
type userInputRequesterKey struct{}
type approvalRequesterKey struct{}

// RequestUserInputFunc is the actor-supplied implementation behind RequestUserInput.
type RequestUserInputFunc func(context.Context, string, []string) (string, error)

// UserInputContextError reports that a tool requested user input outside a live loop call.
type UserInputContextError struct{}

func (*UserInputContextError) Error() string {
	return "loop: RequestUserInput called without a live loop requester"
}

// WithUserInputRequester installs the live loop's user-input capability for a tool call.
func WithUserInputRequester(ctx context.Context, requester RequestUserInputFunc) context.Context {
	return context.WithValue(ctx, userInputRequesterKey{}, requester)
}

// RequestUserInput routes a tool request through the live loop capability in ctx.
func RequestUserInput(ctx context.Context, question string, choices []string) (string, error) {
	requester, ok := ctx.Value(userInputRequesterKey{}).(RequestUserInputFunc)
	if !ok || requester == nil {
		return "", &UserInputContextError{}
	}
	return requester(ctx, question, choices)
}

// WithToolUseID carries the provider tool-use id for a running tool call.
func WithToolUseID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, toolUseIDKey{}, id)
}

// ToolUseIDFrom returns the provider tool-use id carried by ctx.
func ToolUseIDFrom(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(toolUseIDKey{}).(string)
	return id, ok
}

// WithPreparedCall carries the prepared execution contract for a tool
// invocation: the minted execution ID, the typed request, the tool's opaque
// per-call artifact, and the fresh grant tokens issued for this call. The
// runner installs it per call; the executing tool reads it back via
// PreparedCallFromContext. Tokens travel only inside this contract, never in a
// separate ambient grant carrier.
func WithPreparedCall(ctx context.Context, prepared tool.PreparedCall) context.Context {
	return context.WithValue(ctx, preparedKey{}, prepared)
}

// PreparedCallFromContext returns the prepared execution contract carried by
// ctx, and false when the tool is running outside a prepared call.
func PreparedCallFromContext(ctx context.Context) (tool.PreparedCall, bool) {
	value, ok := ctx.Value(preparedKey{}).(tool.PreparedCall)
	return value, ok
}

// ApprovalRequestFunc is the actor-supplied capability that opens ONE combined
// interactive approval gate for a prepared request and blocks until it is
// resolved to exactly one approval action.
type ApprovalRequestFunc func(ctx context.Context, prompt gate.ApprovalPrompt) (gate.ApprovalAction, error)

// ApprovalContextError reports that an approval was requested outside a live
// loop call (no runner-installed approval capability on ctx). It is
// fail-closed: no capability means no prompt and no approval.
type ApprovalContextError struct{}

func (*ApprovalContextError) Error() string {
	return "loop: approval requested without a live loop approval capability"
}

// WithApprovalRequester installs the live loop's combined-approval capability
// for a tool call. Only the runner wires this, per call, so a gate opened by
// the evaluator routes to the loop's own gate machinery.
func WithApprovalRequester(ctx context.Context, requester ApprovalRequestFunc) context.Context {
	return context.WithValue(ctx, approvalRequesterKey{}, requester)
}

// runnerApprover routes an interactive evaluator's approval through the live
// loop capability carried on ctx. Without one it fails closed.
type runnerApprover struct{}

func (runnerApprover) RequestApproval(ctx context.Context, prompt gate.ApprovalPrompt) (gate.ApprovalAction, error) {
	requester, ok := ctx.Value(approvalRequesterKey{}).(ApprovalRequestFunc)
	if !ok || requester == nil {
		return "", &ApprovalContextError{}
	}
	return requester(ctx, prompt)
}

// GateApprover returns the gate.Approver a consumer passes to interactive
// evaluator construction (gate.NewInteractiveEvaluator). It resolves each
// combined prompt through the live loop's approval capability — the loop's own
// permission gate — installed on ctx by the runner for exactly one call, and
// fails closed when invoked outside a live loop call.
func GateApprover() gate.Approver { return runnerApprover{} }
