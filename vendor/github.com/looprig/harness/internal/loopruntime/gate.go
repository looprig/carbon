package loopruntime

import (
	"context"
	"strings"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	gatedomain "github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/hook"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/tool"
)

// gateKind distinguishes the two kinds of parked-runner gate so runLoop can refuse
// to satisfy a user-input gate with an approval (or vice versa). Routing matches
// on ToolExecutionID AND kind: a stray approve/deny can never answer an AskUser gate.
type gateKind uint8

const (
	// gatePermission is opened by the runner when a tool call needs interactive
	// approval; it accepts ApproveToolCall / DenyToolCall.
	gatePermission gateKind = iota
	// gateUserInput is opened by RequestUserInput (AskUser); it accepts
	// ProvideUserInput.
	gateUserInput
)

// pendingGate is the actor-owned record of an open gate: the dedicated reply channel for
// the parked runner and the kind of command it will accept. Stored in
// loopState.pendingGates, keyed by GateID, and touched ONLY by runLoop/the actor.
type pendingGate struct {
	reply chan<- command.Command
	kind  gateKind
}

type gateRegistrar interface {
	PrepareGateOpen(ctx context.Context, loopID uuid.UUID, g gatedomain.Gate, payload gatedomain.Payload) (gatedomain.ID, error)
	ActivateGate(ctx context.Context, id gatedomain.ID, route gatedomain.Route) error
	CloseGate(ctx context.Context, id gatedomain.ID, reason gatedomain.CloseReason) error
}

// PermissionReviewRequest is the live-only handoff the actor gives a session's
// review starter once a permission gate's GateOpened has committed and the
// runner has been acked (design §14.3). It carries exactly what a classifier
// needs to build a PermissionReviewSubject and nothing a durable record or
// event may not: no raw tool arguments beyond what the human-facing gate
// already displays, and no token or grant material. ReviewContext already
// carries the loop/session/turn/step coordinates plus the gate policy
// revision and security ceiling in effect when the batch was captured
// (internal/loopruntime/review_context.go); a zero ReviewContext
// (ContextRevision == "") means no live review was configured for this turn,
// and a review starter must treat that as "nothing to review" rather than
// guessing at defaults.
type PermissionReviewRequest struct {
	GateID          gatedomain.ID
	ToolExecutionID uuid.UUID
	Request         tool.Request
	ReviewContext   gatedomain.ReviewContext
}

// permissionReviewStarter is the narrow, private seam a session-side adapter
// implements to begin asynchronous permission-classifier review after a
// gatePermission registration activates. It is obtained via the same optional
// type-assertion pattern as gateRegistrar (NewLoop's `events.(gateRegistrar)`):
// a publisher that does not implement it simply never starts review, which
// keeps every headless/test path that predates this seam unchanged.
//
// StartPermissionReview MUST return promptly. The actor calls it inline, on
// its own single-threaded event-loop goroutine, and does not wrap the call in
// a goroutine of its own — the contract is "kick off async work and return",
// not "compute a result". An implementation that blocks on classifier
// inference here blocks the whole loop actor; the session-side adapter is
// responsible for moving inference onto its own goroutine before returning.
type permissionReviewStarter interface {
	StartPermissionReview(ctx context.Context, req PermissionReviewRequest)
}

// permissionRequestFromPayload narrows a gate.Payload to the displayed
// tool.Request a gatePermission registration always carries. A payload that is
// not a PermissionPayload (or a nil one) reports false rather than a zero
// Request the caller could mistake for a genuinely empty prepared request —
// fail-closed: no valid payload means no review starts.
func permissionRequestFromPayload(payload gatedomain.Payload) (tool.Request, bool) {
	switch v := payload.(type) {
	case gatedomain.PermissionPayload:
		return v.Request, true
	case *gatedomain.PermissionPayload:
		if v == nil {
			return tool.Request{}, false
		}
		return v.Request, true
	default:
		return tool.Request{}, false
	}
}

type nopGateRegistrar struct{}

func (nopGateRegistrar) PrepareGateOpen(_ context.Context, _ uuid.UUID, g gatedomain.Gate, _ gatedomain.Payload) (gatedomain.ID, error) {
	if !g.ID.IsZero() {
		return g.ID, nil
	}
	if !g.Subject.ToolExecutionID.IsZero() {
		return g.Subject.ToolExecutionID, nil
	}
	return uuid.New()
}

func (nopGateRegistrar) ActivateGate(context.Context, gatedomain.ID, gatedomain.Route) error {
	return nil
}

func (nopGateRegistrar) CloseGate(context.Context, gatedomain.ID, gatedomain.CloseReason) error {
	return nil
}

// gateRegistration is the request a parked runner sends to the actor to install a
// gate. The actor prepares the gate durably through the session, records {reply,
// kind} under the minted GateID, activates the public gate, then acks to signal
// install-before-emit.
type gateRegistration struct {
	ctx context.Context
	// abandonID makes this an actor-owned removal request rather than an open
	// request. The same synchronous channel preserves ordering with registration.
	abandonID gatedomain.ID
	gate      gatedomain.Gate
	payload   gatedomain.Payload
	// reviewContext is a live-only defensive clone for permission review. The
	// actor never copies it into gate payloads, events, or journal records.
	reviewContext gatedomain.ReviewContext
	callID        uuid.UUID
	reply         chan<- command.Command
	kind          gateKind
	ack           chan<- gateInstallAck
}

type gateInstallAck struct {
	gateID gatedomain.ID
	err    error
}

func (r gateRegistration) toolExecutionID() uuid.UUID {
	if !r.gate.Subject.ToolExecutionID.IsZero() {
		return r.gate.Subject.ToolExecutionID
	}
	return r.callID
}

// accepts reports whether a control command may satisfy a gate of the given kind.
// gatePermission ↔ ApproveToolCall/DenyToolCall; gateUserInput ↔ ProvideUserInput.
// Any other pairing is rejected (fail-safe): runLoop drops a mismatched command
// rather than delivering it to the wrong parked runner.
func accepts(kind gateKind, cmd command.Command) bool {
	switch cmd.(type) {
	case command.ApproveToolCall, command.DenyToolCall:
		return kind == gatePermission
	case command.ProvideUserInput:
		return kind == gateUserInput
	default:
		return false
	}
}

// Unexported context-key types. Each is a distinct zero-size struct so values
// never collide across packages (the idiomatic Go ctx-key pattern) and cannot be
// constructed by an outside package.
type emitKey struct{}
type callIDKey struct{}
type gateRegKey struct{}
type operationHookRuntimeKey struct{}

type operationHookRuntime struct {
	hooks       *hook.Runner
	coordinates identity.Coordinates
	agentName   identity.AgentName
	cause       identity.Cause
}

type eventEmitter func(context.Context, event.Event)

func withOperationHookRuntime(ctx context.Context, runtime operationHookRuntime) context.Context {
	return context.WithValue(ctx, operationHookRuntimeKey{}, runtime)
}

func operationHooksFromContext(ctx context.Context) operationHookRuntime {
	runtime, _ := ctx.Value(operationHookRuntimeKey{}).(operationHookRuntime)
	return runtime
}

// withEmit returns a child ctx carrying the per-turn emit func. The runner injects
// it per tool call; EmitFromContext / RequestUserInput read it back.
func withEmit(ctx context.Context, emit func(event.Event)) context.Context {
	return context.WithValue(ctx, emitKey{}, emit)
}

func withContextEmit(ctx context.Context, emit eventEmitter) context.Context {
	return context.WithValue(ctx, emitKey{}, emit)
}

// withCallID returns a child ctx carrying the active tool call's ToolExecutionID.
func withCallID(ctx context.Context, callID uuid.UUID) context.Context {
	return context.WithValue(ctx, callIDKey{}, callID)
}

// withToolUseID returns a child ctx carrying the active tool call's provider
// tool-use id (content.ToolUseBlock.ID), the durable handle a spawned subagent
// loop links back to the parent agent tool call.
func withToolUseID(ctx context.Context, id string) context.Context {
	return WithToolUseID(ctx, id)
}

// withGateReg returns a child ctx carrying the actor's gate-registration handle.
// Only the loop wires this; RequestUserInput reads it to open a gateUserInput gate.
func withGateReg(ctx context.Context, gateReg chan<- gateRegistration) context.Context {
	return context.WithValue(ctx, gateRegKey{}, gateReg)
}

// callIDFromContext reads the active ToolExecutionID, false when absent.
func callIDFromContext(ctx context.Context) (uuid.UUID, bool) {
	v, ok := ctx.Value(callIDKey{}).(uuid.UUID)
	return v, ok
}

// gateRegFromContext reads the gate-registration handle, false when absent.
func gateRegFromContext(ctx context.Context) (chan<- gateRegistration, bool) {
	v, ok := ctx.Value(gateRegKey{}).(chan<- gateRegistration)
	return v, ok
}

// EmitFromContext returns the per-turn event-emit func the runner injected, and
// false when none is present (the tool is being run outside a turn). Event-emitting
// tools call this; it is the only sanctioned way for a tool in tools/ to emit an
// event without depending on the loop internals.
func EmitFromContext(ctx context.Context) (func(event.Event), bool) {
	switch emit := ctx.Value(emitKey{}).(type) {
	case eventEmitter:
		return func(value event.Event) { emit(ctx, value) }, true
	case func(event.Event):
		return emit, true
	default:
		return nil, false
	}
}

// GateContextMissing identifies which injected ctx value RequestUserInput could
// not find. It is a fail-secure signal: a tool that calls RequestUserInput outside
// a turn (no emit / ToolExecutionID / gateReg in ctx) is a bug, so it errors rather than
// silently proceeding.
type GateContextMissing string

const (
	GateContextEmit    GateContextMissing = "emit"
	GateContextCallID  GateContextMissing = "callID"
	GateContextGateReg GateContextMissing = "gateReg"
)

// GateContextError is returned by RequestUserInput when the ctx is missing one of
// the runner-injected values. Callers can errors.As to inspect which value.
type GateContextError struct{ Missing GateContextMissing }

func (e *GateContextError) Error() string {
	return "loop: RequestUserInput called without ctx value: " + string(e.Missing)
}

// RequestUserInput is the loop-provided helper AskUser calls to open a user-input
// gate. It encapsulates all the gate plumbing so a tool never touches gateReg
// directly:
//
//  1. Read emit, ToolExecutionID, gateReg from ctx — any missing → *GateContextError
//     (fail-secure; calling this outside a turn is a bug).
//  2. Register a gateUserInput gate synchronously and ctx-aware: send the
//     registration, then wait for the ack (install-before-emit). Both selects
//     escape on ctx.Done so a cancelled turn / departed actor never wedges.
//  3. Emit UserInputRequested AFTER the ack — the gate is installed, so the
//     matching ProvideUserInput cannot be dropped on a race.
//  4. Block on the dedicated reply channel (buffered(1), runner is sole reader)
//     or ctx.Done. ToolExecutionID is re-validated on receipt as cheap defence.
//
// Returns the raw answer; AskUser validates it against its choices.
func RequestUserInput(ctx context.Context, question string, choices []string) (string, error) {
	emit, ok := EmitFromContext(ctx)
	if !ok {
		return "", &GateContextError{Missing: GateContextEmit}
	}
	callID, ok := callIDFromContext(ctx)
	if !ok {
		return "", &GateContextError{Missing: GateContextCallID}
	}
	gateReg, ok := gateRegFromContext(ctx)
	if !ok {
		return "", &GateContextError{Missing: GateContextGateReg}
	}

	// reply is buffered(1) so the actor's routed send never blocks (runner is the
	// sole reader). ack is unbuffered: the actor closes it to signal installation.
	reply := make(chan command.Command, 1)
	ack := make(chan gateInstallAck, 1)
	g := stampGateSubjectProvenance(ctx, askUserGate(callID, question, choices))
	payload := gatedomain.AskUserPayload{Question: question, Choices: choices}

	// Register synchronously, ctx-aware: no wedge if the actor is gone or the turn
	// is cancelled.
	select {
	case gateReg <- gateRegistration{ctx: ctx, gate: g, payload: payload, callID: callID, reply: reply, kind: gateUserInput, ack: ack}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	var installed gateInstallAck
	select {
	case installed = <-ack:
		if installed.err != nil {
			return "", installed.err
		}
	case <-ctx.Done():
		return "", ctx.Err()
	}

	// Install-before-emit: only now is the session gate guaranteed active.
	emit(event.UserInputRequested{ToolExecutionID: callID, Question: question, Choices: choices})

	g.ID = installed.gateID
	runtime := operationHooksFromContext(ctx)
	parentCall := hook.Call{
		Coordinates: runtime.coordinates,
		AgentName:   runtime.agentName,
		Cause:       runtime.cause,
	}
	waitCtx, waitCall, finishWait, waitErr := startGateWaitWithRunner(ctx, parentCall, g, runtime.hooks)
	if waitErr != nil {
		return "", waitErr
	}
	select {
	case cmd := <-reply:
		// runLoop already matched by ToolExecutionID + kind; re-validate the ToolExecutionID as cheap
		// defence in depth, and narrow to the concrete command for the answer.
		pui, ok := cmd.(command.ProvideUserInput)
		if !ok || pui.GateToolExecutionID() != callID || (!pui.GateRoute.GateID.IsZero() && pui.GateRoute.GateID != installed.gateID) {
			err := &GateReplyMismatchError{ToolExecutionID: callID}
			finishGateWait(finishWait, waitCall, nil, err)
			return "", err
		}
		answer := &gatedomain.Answer{
			GateID: installed.gateID,
			Values: map[string]string{"answer": pui.Answer},
			Source: gateResponseSource(cmd),
		}
		finishGateWait(finishWait, waitCall, answer, nil)
		return pui.Answer, nil
	case <-waitCtx.Done():
		waitErr := waitCtx.Err()
		if ctx.Err() == nil {
			if err := abandonInstalledGate(ctx, waitCtx, gateReg, installed.gateID); err != nil {
				finishGateWait(finishWait, waitCall, nil, err)
				return "", err
			}
		}
		finishGateWait(finishWait, waitCall, nil, waitErr)
		return "", waitErr
	}
}

func abandonInstalledGate(
	lifetimeCtx context.Context,
	operationCtx context.Context,
	gateReg chan<- gateRegistration,
	gateID gatedomain.ID,
) error {
	ack := make(chan gateInstallAck, 1)
	closeCtx, release := operationValuesWithLifetime(operationCtx, lifetimeCtx)
	defer release()
	request := gateRegistration{
		ctx:       closeCtx,
		abandonID: gateID,
		ack:       ack,
	}
	select {
	case gateReg <- request:
	case <-lifetimeCtx.Done():
		return lifetimeCtx.Err()
	}
	select {
	case result := <-ack:
		return result.err
	case <-lifetimeCtx.Done():
		return lifetimeCtx.Err()
	}
}

func operationValuesWithLifetime(
	operationCtx context.Context,
	lifetimeCtx context.Context,
) (context.Context, func()) {
	values := context.WithoutCancel(operationCtx)
	linked, cancel := context.WithCancelCause(values)
	deadlineCancel := func() {}
	if deadline, ok := lifetimeCtx.Deadline(); ok {
		linked, deadlineCancel = context.WithDeadlineCause(
			linked,
			deadline,
			context.DeadlineExceeded,
		)
	}
	stopLifetime := context.AfterFunc(lifetimeCtx, func() {
		cancel(context.Cause(lifetimeCtx))
	})
	if lifetimeCtx.Err() != nil {
		cancel(context.Cause(lifetimeCtx))
	}
	return linked, func() {
		stopLifetime()
		deadlineCancel()
		cancel(context.Canceled)
	}
}

func gateResponseSource(cmd command.Command) gatedomain.ResponseSource {
	if cmd.CommandHeader().Agency == identity.AgencyUser {
		return gatedomain.ResponseSource{Kind: gatedomain.ResponseFromUser}
	}
	// Machine agency cannot distinguish policy from model provenance once the
	// response has been translated onto the loop command wire.
	return gatedomain.ResponseSource{}
}

func startGateWaitWithRunner(
	ctx context.Context,
	parent hook.Call,
	g gatedomain.Gate,
	hooks *hook.Runner,
) (context.Context, hook.Call, hook.FinishFunc, error) {
	call := hook.Call{
		Operation:   hook.OperationGateWait,
		StartedAt:   time.Now(),
		Coordinates: parent.Coordinates,
		AgentName:   parent.AgentName,
		Cause:       parent.Cause,
		GateWait: &hook.GateWaitData{
			GateID:   g.ID,
			Kind:     g.Kind,
			Resolver: g.Resolver,
			Blocks:   g.Blocks,
			Effect:   g.Effect,
		},
	}
	waitCtx, finish, err := hooks.Start(ctx, call)
	return waitCtx, call, finish, err
}

func finishGateWait(
	finish hook.FinishFunc,
	call hook.Call,
	answer *gatedomain.Answer,
	err error,
) {
	call.GateWait.Answer = answer
	finishHook(finish, call, hookOutcome(context.Background(), err), err)
}

// GateReplyMismatchError is returned if the command delivered on a gateUserInput
// reply channel is not a ProvideUserInput for the expected ToolExecutionID. runLoop routes
// by ToolExecutionID + kind, so this is a defence-in-depth guard that should never fire in
// normal operation.
type GateReplyMismatchError struct{ ToolExecutionID uuid.UUID }

func (e *GateReplyMismatchError) Error() string {
	return "loop: gate reply did not match expected ProvideUserInput for call " + e.ToolExecutionID.String()
}

// stampGateSubjectProvenance fills a loop-owned gate's Subject with the running
// step's turn and step ids, read from the tool-execution provenance on ctx.
//
// A permission/ask-user gate is loop-owned and journaled as a step-scoped record
// (GatePrepared/GateOpened/GateResolved). Its Subject previously carried only the
// ToolExecutionID, so PrepareGateOpen stamped the durable event with a zero
// TurnID/StepID — an identity a step-scoped gate record cannot satisfy, which both
// makes the marshaler refuse it at the durable boundary and makes any session that
// opened one unrestorable. The running step's coordinates are unambiguous in the
// batch context (turn.go wraps it with WithProvenance at the step boundary, the
// same seam that stamps ToolCallStarted), so a gate opened during tool execution
// carries the real turn/step. If provenance is somehow absent the Subject stays
// zero and the gate fails closed at the durable boundary rather than poisoning a
// future restore.
func stampGateSubjectProvenance(ctx context.Context, g gatedomain.Gate) gatedomain.Gate {
	if prov, ok := ProvenanceFrom(ctx); ok {
		g.Subject.TurnID = prov.TurnID
		g.Subject.StepID = prov.StepID
	}
	return g
}

// permissionGate builds the public envelope for ONE combined permission gate:
// the prompt renders the redacted summary plus every displayed unmet capability
// and reusable candidate description, and the controls are the exact three
// approval actions. It never renders raw tool arguments or token material.
func permissionGate(callID uuid.UUID, displayed tool.Request) gatedomain.Gate {
	return gatedomain.Gate{
		Kind:       gatedomain.KindPermission,
		Resolver:   gatedomain.ResolverLoop,
		Blocks:     gatedomain.BlocksToolCall,
		Effect:     gatedomain.EffectResume,
		Subject:    gatedomain.Subject{ToolExecutionID: callID},
		Restorable: true,
		Prompt: gatedomain.Prompt{
			Title:    "Approve tool call",
			Body:     renderApprovalBody(displayed),
			Controls: gatedomain.ApprovalControls(),
		},
	}
}

// renderApprovalBody projects the displayed typed request onto the prompt body:
// the redacted summary, the unmet capability descriptions, and — because
// "Approve always for this workspace" persists them — the exact reusable rule
// candidates on offer. Descriptions only; never raw args, never tokens.
func renderApprovalBody(displayed tool.Request) string {
	var sb strings.Builder
	sb.WriteString(displayed.Summary)
	if len(displayed.Requirements) > 0 {
		sb.WriteString("\n\nCapabilities:")
		for _, requirement := range displayed.Requirements {
			sb.WriteString("\n- ")
			sb.WriteString(requirement.Description)
		}
	}
	candidates := false
	for _, requirement := range displayed.Requirements {
		for _, candidate := range requirement.Candidates {
			if !candidates {
				sb.WriteString("\n\nApprove always saves:")
				candidates = true
			}
			sb.WriteString("\n- ")
			sb.WriteString(candidate.Description)
		}
	}
	return sb.String()
}

func askUserGate(callID uuid.UUID, question string, choices []string) gatedomain.Gate {
	return gatedomain.Gate{
		Kind:     gatedomain.KindAskUser,
		Resolver: gatedomain.ResolverLoop,
		Blocks:   gatedomain.BlocksToolCall,
		Effect:   gatedomain.EffectResume,
		Subject:  gatedomain.Subject{ToolExecutionID: callID},
		Prompt: gatedomain.Prompt{
			Title: "User input requested",
			Body:  question,
			Controls: []gatedomain.Control{
				{Action: "answer", Label: "Answer"},
			},
			Schema: gatedomain.PromptSchema{Fields: askUserFields(choices)},
		},
	}
}

func askUserFields(choices []string) []gatedomain.Field {
	if len(choices) == 0 {
		return []gatedomain.Field{{Name: "answer", Label: "Answer", Kind: gatedomain.FieldText, Required: true}}
	}
	opts := make([]gatedomain.Option, 0, len(choices))
	for _, choice := range choices {
		opts = append(opts, gatedomain.Option{Value: choice, Label: choice})
	}
	return []gatedomain.Field{{Name: "answer", Label: "Answer", Kind: gatedomain.FieldSelect, Required: true, Options: opts}}
}
