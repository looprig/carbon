package loopruntime

import (
	"reflect"
	"time"

	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hook"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/inference"
	contextcount "github.com/looprig/inference/contextcount"
	model "github.com/looprig/inference/model"
)

// configFromBound resolves the immutable public contract into actor-private wiring for the
// CONSTRUCTION default: an EMPTY modeName is remapped to the definition's initial mode,
// because a freshly-built (or restored-without-a-change) loop starts at its initial mode.
// The runtime SetMode change path must NOT remap — there "" names the base mode — so it
// calls configForMode directly (exact resolution).
func configFromBound(bound loop.BoundDefinition, modeName loop.ModeName) (runtimeConfig, error) {
	if err := requireBound(bound); err != nil {
		return runtimeConfig{}, err
	}
	if modeName == "" {
		modeName = bound.InitialMode()
	}
	// bound is already validated; resolve directly (no redundant re-check via configForMode).
	return resolveMode(bound, modeName)
}

// configForMode resolves the actor-private wiring for the mode named EXACTLY modeName, with
// no ""→initial remap: "" selects the definition's base mode (base system/model/tools), a
// named mode selects that mode, and an unknown name fails with a typed BindError. It is the
// runtime SetMode resolver — resolving by exact name is what keeps the selected label, the
// emitted LoopModeChanged, and the applied effective config mutually consistent (so
// SetMode("") reaches the reachable base mode rather than silently resolving the initial
// mode's config under a "" label).
func configForMode(bound loop.BoundDefinition, modeName loop.ModeName) (runtimeConfig, error) {
	if err := requireBound(bound); err != nil {
		return runtimeConfig{}, err
	}
	return resolveMode(bound, modeName)
}

// resolveMode is the shared, already-bound-checked resolver behind configFromBound (after
// its ""→initial remap) and configForMode (exact). It selects the BoundMode by exact name
// and builds the actor wiring, composing the SELECTED mode's system via the single exported
// loop.EffectiveSystem (so live turns and restore folds compose it byte-for-byte identically).
func resolveMode(bound loop.BoundDefinition, modeName loop.ModeName) (runtimeConfig, error) {
	mode, ok := bound.Mode(modeName)
	if !ok {
		return runtimeConfig{}, &loop.BindError{Kind: loop.BindInvalidDefinition, Name: string(modeName), Index: -1}
	}
	model := mode.Model
	limits := mode.ToolLimits
	resolved := runtimeConfig{
		Client: bound.Client(), Model: model, System: loop.EffectiveSystem(bound.System(), mode.Instructions), DrainTimeout: bound.DrainTimeout(),
		AgentName: bound.Name(), Engine: bound.Engine(), RuntimeContext: bound.RuntimeContext(),
		Tools: ToolSet{Access: bound.Access(), Registry: mode.Tools, Middlewares: bound.Middlewares(), MaxToolIterations: limits.Iterations, MaxToolCallsPerTurn: limits.Calls, MaxParallelToolCalls: limits.Parallel},
	}
	if output, configured := bound.OutputSchema(); configured {
		resolved.Output = cloneOutputSchema(output)
	}
	if capability, configured := bound.CounterCapability(); configured {
		resolved.ContextCounter = bound.ContextCounter()
		resolved.CounterCapability = capability
		resolved.InferenceCapability, _ = bound.InferenceCapability()
	}
	if policy, configured := bound.CompactionPolicy(); configured {
		copyOfPolicy := policy
		resolved.Compaction = &copyOfPolicy
	}
	if policy, configured := bound.ContextObservationPolicy(); configured {
		copyOfPolicy := policy
		resolved.ContextObservation = &copyOfPolicy
	}
	return resolved, nil
}

// requireBound rejects a nil (or typed-nil) bound definition with a typed BindError. The
// raw-config test path (newWithConfig) carries no bound, so a SetMode there resolves through
// this to ChangeInvalidMode — a modeless loop has no predeclared modes to select.
func requireBound(bound loop.BoundDefinition) error {
	if bound == nil || (reflect.ValueOf(bound).Kind() == reflect.Ptr && reflect.ValueOf(bound).IsNil()) {
		return &loop.BindError{Kind: loop.BindInvalidDefinition, Index: -1}
	}
	return nil
}

// runtimeConfig is frozen actor wiring resolved from one BoundDefinition.
type runtimeConfig struct {
	Client       inference.Client // required — caller constructs via auto.New at composition root
	Model        model.Model      // secret-free model descriptor (name, endpoint, sampling) — stamped onto every Request; carries NO system prompt and NO secret
	System       string           // per-agent system prompt — sent on every Request AND hashed into the config fingerprint; the connection secret rides the Client, never here
	DrainTimeout time.Duration    // optional — bounds the hard-kill wait for a cancelled turn to drain; New defaults it to 5s
	Hooks        *hook.Runner

	// AgentName is the immutable attribution name the loop runs under (the agent/role
	// driving it, e.g. "operator"). It is stamped onto the loop's LoopStarted at creation
	// and never changes. Empty (the zero value) means an unnamed/plain loop. The session
	// reads it when publishing LoopStarted; restore validates the root loop's stamped name
	// against the configured primary's AgentName.
	AgentName identity.AgentName

	// Engine selects the loop backend. Zero = EngineNative (the historical path).
	// Foreign engines route construction through the injected foreign Builder at
	// the session composition root; New itself only ever builds native.
	Engine loop.Engine

	// Tools is the runner's view of the tool subsystem (the consumer surface in
	// deps.go). Its runaway-guard caps are defaulted by New when zero;
	// Access/Registry/Middlewares are left as the composition root set them
	// (a nil Access denies every tool call, fail closed).
	Tools ToolSet

	// Output is the loop-wide final-output policy. It is cloned while resolving
	// the bound definition and again into each turn so request assembly never
	// aliases a public accessor or a concurrently changing effective mode.
	Output *inference.OutputSchema

	// RuntimeContext, when non-nil, yields the volatile per-turn context blocks
	// (date/cwd/git) the loop appends at the TAIL of each turn's request — AFTER the
	// committed messages, and as a transient addition that NEVER enters committed
	// history and NEVER touches the System prompt (the cached prefix). It is consulted once
	// per turn, so each turn rides a single FRESH block and stale blocks never
	// accumulate. nil (the zero value, the New default) means OFF: the request is
	// assembled exactly as it was before — no extra blocks. The interface keeps the
	// loop free of os/exec; the concrete provider is wired at the composition root.
	RuntimeContext RuntimeContextProvider

	ContextCounter      contextcount.ContextCounter
	CounterCapability   contextcount.CounterCapability
	InferenceCapability contextcount.InferenceCapability
	ContextObservation  *loop.ContextObservationPolicy
	Compaction          *loop.CompactionPolicy

	// compactionSink is the internal ownership-transfer seam between the loop actor's
	// bounded control coordinator and the typed compaction executor/finalizer wired by
	// later implementation tasks. Nil leaves a request pending; it never consumes or
	// drops the obligation. The field is unexported so consumers cannot inject an
	// arbitrary runner through the public loop definition.
	compactionSink compactionDispositionSink

	// idGen mints the loop's correlation IDs: the per-turn TurnID, each StepID,
	// and each tool-call ToolExecutionID. It is unexported, so the composition root cannot
	// set it: New defaults it to uuid.New. It exists only as a test seam for
	// exercising the crypto/rand failure branches.
	idGen idGenerator

	// compactionNow is the actor-private monotonic duration clock. It is separate
	// from the event Factory's wall clock so EventID/CreatedAt minting and journal
	// latency cannot move the canonical compaction duration cut point.
	compactionNow event.Clock

	// now is the clock the loop's event Factory mints CreatedAt from. It is
	// unexported, so the composition root cannot set it: New defaults it to
	// time.Now. It exists only as a test seam so a test can pin CreatedAt
	// deterministically (mirrors idGen).
	now event.Clock

	// eventFactory mints the EventID + CreatedAt stamped onto every ENDURING loop
	// event at the single publish chokepoint (Ephemeral events are never persisted
	// and so are never stamped). It is unexported, so the composition root cannot
	// set it: New defaults it to a Factory built from idGen + now. It exists as a
	// test seam so a test can inject a deterministic (or failing) factory.
	eventFactory *event.Factory

	// The four fields below (afterDrain, afterContextReplacement,
	// beforeCompactionBoundary, afterEffectiveConfigChange) are this package's
	// test-only observation/synchronization seams: unexported, nil in
	// production (never assigned by New/newLoopWithSeed outside tests), always
	// nil-checked at their call sites. Check here before adding a fifth —
	// the right execution point (turn goroutine vs. actor goroutine, before
	// vs. after a durable commit) may already exist.

	// afterDrain is a test-only synchronization seam invoked by foldPending in the
	// turn goroutine AFTER cfg.drainPending has moved the inbox into the actor's
	// draining buffer but BEFORE the first TurnFoldedInto commit. It is unexported,
	// so the composition root cannot set it; New never assigns it, so it is nil in
	// production and foldPending's nil check skips it entirely. It exists only so a
	// test can cancel the loop deterministically in the post-drain/pre-commit window
	// to exercise the draining-buffer abnormal-return sweep.
	afterDrain func()

	// afterContextReplacement is a test-only turn-goroutine seam invoked after
	// the replacement directive resets private request history. Production leaves
	// it nil; tests pause the turn while the actor proves its durable projection.
	afterContextReplacement func()

	// beforeCompactionBoundary is a test-only synchronization seam invoked by the
	// actor after selecting a safe boundary but before priority arbitration. It lets
	// tests make both bounded command lanes ready without timing sleeps.
	beforeCompactionBoundary func(compactionBoundaryKind)

	// afterEffectiveConfigChange is a test-only synchronization seam invoked by
	// applySetMode/applyChangeInference on the actor goroutine immediately after
	// committing a new state.effective (after the durable LoopModeChanged/
	// LoopInferenceChanged event has been appended, in the same place the actor
	// already assigns state.effective/state.runtime). It is unexported, so the
	// composition root cannot set it; New never assigns it, so it is nil in
	// production. effectiveConfig carries no other external observation seam (the
	// only production query, Snapshot, exposes committed msgs/turnIndex, not the
	// live effective config) and the actor goroutine owns loopState exclusively, so
	// this exists so a test can race-freely observe a field of effectiveConfig
	// synchronously, without driving a full turn. The callback runs strictly
	// BEFORE the command's Ack send (same goroutine, sequential statements), so
	// a test that captures the passed value before calling the change and blocks
	// on the Ack reply observes it race-free per Go's channel happens-before
	// guarantee.
	//
	// inferenceCapability gained a real production reader in task 2.3
	// (loop.go's two measureRequestContext call sites) and 2.4
	// (compactionExecutionCandidate.InferenceCapability) — reassessed and kept:
	// TestSetModeReResolvesCapabilityAcrossTransport,
	// TestChangeInferenceReResolvesCapabilityAcrossTransport, and
	// TestChangeInferenceUndeclaredTransportRefusedAndCapabilityUnchanged still
	// want to observe the commit synchronously without driving a full turn
	// through to measurement, which this seam is the only way to do.
	afterEffectiveConfigChange func(effectiveConfig)

	// reviewContext, when non-nil, turns on live-only permission-review
	// context capture (internal/loopruntime/review_context.go) for every turn
	// this loop runs: buildTurnConfig copies it onto each turnConfig.reviewContext
	// unchanged. It is NOT resolved from the bound loop.Definition — no
	// public pkg/loop/pkg/rig option sets it — because it carries no
	// consumer-owned policy of its own (see ReviewContext's doc comment):
	// internal/sessionruntime is the sole caller, threading it in via
	// NewInModeWithCompactor/NewRestoredWithCompactor's reviewContext
	// parameter whenever the session has permission classifiers registered.
	// nil (the default for every pre-existing caller) leaves every turn's
	// reviewContext nil, byte-identical to before this field existed.
	reviewContext *reviewContextConfiguration
}

// RuntimeDependencies carries native runtime collaborators that are not part
// of a loop's durable declarative definition.
type RuntimeDependencies struct {
	Compactor Compactor
	Hooks     *hook.Runner
	// ReviewContext mirrors Compactor's shape: internal/sessionruntime is the
	// only caller that ever supplies a non-nil value (whenever the session
	// has permission classifiers registered — see Session.loopReviewContext).
	// nil leaves the resulting loop's turns with reviewContext == nil.
	ReviewContext *ReviewContext
}
