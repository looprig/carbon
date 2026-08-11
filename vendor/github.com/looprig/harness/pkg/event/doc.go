// Package event defines the sealed union of rig, session, loop, turn, step, and
// tool events. Enduring rig-control and workspace transitions are durable replay
// inputs; ephemeral streaming events are never persisted.
//
// Every concrete event embeds a Header (producer identity), exactly one lifecycle
// mixin (ephemeral, enduring, or terminal — supplying Class()/EndsTurn()), and
// exactly one scope mixin (sessionScoped or loopScoped — supplying Scope()). The
// compile-time assertions below pin the sealed union: every concrete event type
// must satisfy Event, and the list is the authoritative enumeration of the union.
// Adding a new event without adding it here is harmless, but removing a type or
// breaking its interface satisfaction fails the build here first.
package event

// Sealed-union interface-satisfaction assertions. Each concrete event must
// satisfy Event (isEvent + Class + Scope + EndsTurn + EventHeader). The intended
// "exactly one lifecycle mixin and exactly one scope mixin" is enforced from both
// sides: embedding two of a kind makes the promoted selector ambiguous (won't
// compile), and embedding zero leaves the method missing (won't satisfy Event) —
// so any count other than one fails these assertions.
var (
	// Session-scoped events.
	_ Event = SessionStarted{}
	_ Event = SessionActive{}
	_ Event = SessionIdle{}
	_ Event = SessionStopped{}
	_ Event = HustleStarted{}
	_ Event = HustleCompleted{}
	_ Event = HustleFailed{}
	_ Event = RestoreStarted{}
	_ Event = RestoreDone{}
	_ Event = RestoreErrored{}
	_ Event = ConfigurationAdopted{}
	_ Event = WorkspaceCheckpointed{}
	_ Event = WorkspaceRestored{}
	_ Event = ActiveLoopChanged{}
	_ Event = DelegateDeliveryStateChanged{}
	_ Event = IntegrationStatus{}
	_ Event = WorkflowActivity{}

	// Loop-scoped events.
	_ Event = ProcessStarted{}
	_ Event = ProcessBackgrounded{}
	_ Event = ProcessCompleted{}
	_ Event = ProcessStopRequested{}
	_ Event = ProcessLost{}
	_ Event = LoopIdle{}
	_ Event = LoopStarted{}
	_ Event = DelegateRequestAccepted{}
	_ Event = LoopInferenceChanged{}
	_ Event = LoopModeChanged{}
	_ Event = LoopExternalToolsetChanged{}
	_ Event = ForeignSessionBound{}
	_ Event = LoopAgentSessionBound{}
	_ Event = LoopRestoreTombstoned{}
	_ Event = CompactionStarted{}
	_ Event = CompactionCommitted{}
	_ Event = CompactionRejected{}
	_ Event = CompactWaiterResolved{}
	_ Event = CompactWaiterRejected{}

	// Turn/step-scoped events.
	_ Event = TokenDelta{}
	_ Event = TurnStarted{}
	_ Event = StepDone{}
	_ Event = TurnFoldedInto{}
	_ Event = InputCancelled{}
	_ Event = TurnDone{}
	_ Event = TurnFailed{}
	_ Event = TurnInterrupted{}

	// Gate/tool lifecycle events.
	_ Event = PermissionRequested{}
	_ Event = UserInputRequested{}
	_ Event = ToolCallStarted{}
	_ Event = ToolCallCompleted{}
	_ Event = PermissionReviewStarted{}
	_ Event = PermissionReviewCompleted{}
)
