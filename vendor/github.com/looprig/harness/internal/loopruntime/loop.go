package loopruntime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/command"
	"github.com/looprig/harness/pkg/event"
	gatedomain "github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/hook"
	"github.com/looprig/harness/pkg/identity"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/inference"
	contextcount "github.com/looprig/inference/contextcount"
	model "github.com/looprig/inference/model"
)

// Loop is the handle to a running agent loop for internal packages.
// Commands is unbuffered — sends block until the actor is ready. Callers must
// never close Commands; stop the actor with Shutdown. (Closing it would exit the
// actor through the `!ok` path, skipping terminal delivery and shutdown acks.)
// Done is closed when the actor has fully exited.
// Direct callers must honor the command contracts. The submit commands
// (UserInput/SubagentResult) and CancelQueuedInput are fire-and-forget: their
// outcomes are PUBLISHED as typed events onto the session fan-in, not replied on a
// per-command channel. Only the control commands carry a reply channel — Interrupt
// (Ack chan bool) and Shutdown (Ack chan error) — and each must be non-nil and
// buffered(1) so the actor's direct send never stalls.
type Loop struct {
	Commands chan<- command.Command
	Done     <-chan struct{}

	// priorityCommands is the separately bounded Interrupt/Shutdown lane. Trusted
	// session dispatch selects it through PriorityCommandSink; ordinary commands
	// continue to use Commands and retain their existing FIFO channel semantics.
	priorityCommands chan<- command.Command

	// gateReg is the actor's gate-registration seam. A parked runner (or
	// RequestUserInput on its behalf) sends a gateRegistration here and waits for
	// the ack; runLoop installs the gate in loopState.pendingGates before closing
	// the ack (install-before-emit). It is unexported: only in-package callers (the
	// runner via the turn-launch ctx injection, and tests) register gates. The
	// actor is the sole reader.
	gateReg chan<- gateRegistration

	// snapshots is the committed-state query seam: Snapshot sends a snapshotRequest
	// here and the actor (the sole owner of loopState.msgs/turnIndex) replies a
	// defensive clone. It is the restore-verification + dormant-snapshot read primitive
	// (see Snapshot). The actor is the sole reader, selecting on it alongside commands.
	snapshots chan<- snapshotRequest

	// hooks retains the immutable runner installed in the actor configuration.
	// The actor executes from loopConfig.cfg; retaining the same pointer on the
	// handle makes native/foreign construction isolation inspectable without
	// exposing hook configuration through loop.Backend.
	hooks *hook.Runner
}

// CommandSink returns the actor's command input.
func (l *Loop) CommandSink() chan<- command.Command { return l.Commands }

// PriorityCommandSink returns the bounded native Interrupt/Shutdown lane.
func (l *Loop) PriorityCommandSink() chan<- command.Command {
	if l.priorityCommands == nil {
		return l.Commands
	}
	return l.priorityCommands
}

// DoneChan closes when the actor exits.
func (l *Loop) DoneChan() <-chan struct{} { return l.Done }

// loopConfig holds the loop goroutine's DEPENDENCIES and construction-time wiring
// (Single Responsibility: deps, not mutable state). New builds it once and hands
// it to runLoop; runLoop never mutates it. loopState (separate) holds the loop's
// identity, status, and accumulated messages — the only thing runLoop mutates.
//
// The split is deliberate: a dependency parked in state (the Phase-3 interim
// `loopState.events`) was an SRP smudge — config is wiring set at construction,
// state is what the actor evolves. events lives here now.
type loopConfig struct {
	// loopCtx is the loop's lifetime context (derived by the session from its
	// sessionCtx). It is NOT a turn lifetime: runLoop derives each turn ctx from it
	// via context.WithCancel(loopCtx). Submit commands carry no context.
	loopCtx context.Context

	// cfg is the caller-supplied loop configuration (client, model, tools, drain
	// timeout, and the test-only id-gen/after-drain seams), defaulted by New.
	cfg runtimeConfig

	// commands is the actor's inbound command channel (the send side is the public
	// Loop.Commands). Closing it is a contract violation; stop via Shutdown.
	commands <-chan command.Command

	// priorityCommands is distinct from commands so emergency control is
	// identifiable without consuming/reordering an ordinary command at a boundary.
	priorityCommands <-chan command.Command

	contextRequests chan contextMeasureRequest
	contextResults  chan contextCountResult
	contextOutcomes chan contextCompactionOutcomeRequest

	// gateReg is the gate-registration channel. The actor is its sole reader; the
	// per-turn goroutine hands the SEND side to runTurn so a parked tool can register
	// a gate. Bidirectional here because a receive-only handle could not be narrowed
	// to the send-only direction runTurn requires.
	gateReg chan gateRegistration

	// snapshots is the committed-state query channel. The actor is its sole reader,
	// selecting on it alongside commands; a caller (Snapshot) sends a request and the
	// actor replies a defensive clone of loopState.msgs + turnIndex. Bidirectional here
	// because the public Loop.snapshots is its send side.
	snapshots chan snapshotRequest

	// internal is the turn goroutine's terminal hand-back (turnResult). Buffered(1)
	// so a finished turn never blocks delivering its terminal to the actor.
	internal chan turnResult

	// commits is the per-step commit handshake channel: the turn goroutine sends a
	// commitRequest per completed step; the actor appends the group to loopState.msgs
	// and emits the StepDone at the same point, then acks. Unbuffered (synchronous).
	commits chan commitRequest

	// drains is the tool-continuation drain handshake channel: one drainRequest per
	// tool-continuation boundary; the actor pops + clears the inbox into
	// loopState.draining and replies the queued inputs. Unbuffered (synchronous).
	drains chan drainRequest

	// admissions returns asynchronous session-wide first-step admission results to
	// the actor, keeping Interrupt/Shutdown responsive while a checkpoint writer waits.
	admissions chan admissionResult

	// done is closed by runLoop when the actor has fully exited (the public
	// Loop.Done is its receive side).
	done chan struct{}

	// events publishes FULL-FIDELITY loop events to the session-level event fan-in.
	// The loop depends on the narrow publisher interface (Dependency Inversion /
	// Interface Segregation) instead of a raw channel, so only Session owns
	// buffering, shutdown, close, and sequence policy. A parent or primary loop must
	// not forward child-loop events; identity is metadata, not the transport path.
	events eventPublisher

	// eventFactory mints the EventID + CreatedAt stamped onto every ENDURING loop
	// event at the publish chokepoint. Ephemeral events are never persisted, so they
	// are never stamped (this also avoids a per-token crypto/rand call). It is a
	// dependency wired at construction (its peer is events), defaulted by New.
	eventFactory *event.Factory

	// gates is the session-owned durable gate registrar. Loop-only tests that wire
	// only an eventPublisher use nopGateRegistrar.
	gates gateRegistrar

	// reviewStarter is the session-owned permission-review seam (nil unless the
	// events publisher also implements permissionReviewStarter). The actor calls
	// it, fire-and-forget, immediately after a gatePermission registration
	// activates — see the gateReg case in runLoop. A nil reviewStarter (headless
	// mode, or a publisher that never opted in) starts no review, preserving the
	// exact pre-Task-14 gate lifecycle.
	reviewStarter permissionReviewStarter

	// bound is the immutable loop definition the actor validates a SetLoopMode against
	// (resolving the target mode's model/effort/tools/instructions). It is nil for the
	// raw-config test path (newWithConfig): a nil bound has no predeclared modes, so every
	// SetLoopMode is refused with ChangeInvalidMode (which is correct for a modeless loop).
	// ChangeLoopInference never consults it (it validates the model/effort values only).
	bound loop.BoundDefinition
}

// idGenerator mints a fresh UUID for the loop's correlation IDs — the per-turn
// TurnID, each StepID, and each tool-call ToolExecutionID. It defaults to uuid.New; tests
// inject a failing generator to exercise the crypto/rand failure branches.
type idGenerator func() (uuid.UUID, error)

// eventPublisher is the loop's narrow consumer of the session-level event
// fan-in. The loop depends on this small interface (Dependency Inversion /
// Interface Segregation) rather than on the concrete session type, so only the
// session owns buffering, shutdown, close, and sequence policy. A parent or
// primary loop must not forward child-loop events; identity is metadata, not
// the transport path.
type eventPublisher interface {
	PublishEvent(context.Context, event.Event) error
	PublishEventChecked(context.Context, event.Event) error
}

// executionBoundary is the optional native checkpoint seam implemented by the owning
// session. Turn terminals are routed through it; context-mutating StepDone and
// TurnFoldedInto use the committed-result refinement below.
type executionBoundary interface {
	CommitBoundary(context.Context, event.Event) error
}

// contextExecutionBoundary reports whether a context-mutating boundary event was
// durably committed even when a later checkpoint step failed. This lets the actor
// keep live history exactly aligned with the durable restore source.
type contextExecutionBoundary interface {
	CommitContextBoundary(context.Context, event.Event) (bool, error)
}

type executionAdmission interface {
	EnterExecution(context.Context, uuid.UUID) (func(), error)
}

// TurnStartCapability is the actor's opaque, one-shot authority to publish one
// admitted turn's exact opening TurnStarted and later release its first-step reader.
// Its committed result names the primary TurnStarted append; a later derived-session
// transition may therefore return committed=true with a non-nil error.
type TurnStartCapability interface {
	PublishTurnStarted(context.Context, event.TurnStarted) (committed bool, err error)
	Release()
}

type turnStartAdmission interface {
	EnterTurnStart(context.Context, uuid.UUID) (TurnStartCapability, error)
}

type admissionFaultProbe interface {
	AdmissionFaultErr() error
}

// effectiveConfig is the loop's CURRENT turn-affecting configuration: the mode, model,
// effort, system prompt, and tool set the NEXT turn will start under. It is actor-owned
// state (mutated ONLY by the actor, no locks). A running turn captured its OWN copy at
// startTurn (into turnConfig), so mutating this never affects the turn in flight — a change
// takes effect only at the next turn boundary. SetLoopMode replaces all five fields;
// ChangeLoopInference replaces only model+effort. The effort is also baked into
// model.Sampling.Effort so the request the turn builds carries it.
//
// inferenceCapability is seeded from the resolved runtimeConfig (cfg.InferenceCapability)
// at construction, and re-resolved by applySetMode/applyChangeInference (via
// BoundDefinition.ContextTransportCapability) whenever a mode or inference change moves
// the effective model onto a different declared ContextTransport. It always reflects the
// CURRENTLY selected mode/model's transport capability. Both request-measurement call
// sites (measureRequestContext, in the contextRequests case and the idle-compaction
// preparation goroutine) read a per-actor-turn snapshot of this field — captured
// synchronously on the actor goroutine before any off-actor goroutine spawns — rather
// than the frozen runtimeConfig.InferenceCapability, which never re-resolves across a
// transport-crossing change. compaction_executor.go no longer freezes its own copy either:
// compactionExecutionCandidate.InferenceCapability is threaded per-call from this same
// per-turn snapshot (see compactionExecutionCandidate construction in loop.go), so a
// compaction triggered after a transport switch measures against the current transport too.
type effectiveConfig struct {
	mode                loop.ModeName
	model               model.Model
	effort              model.Effort
	system              string
	tools               ToolSet
	inferenceCapability contextcount.InferenceCapability
}

// modelRuntime is the SOLE builder of the durable event.ModelRuntime this loop emits: the
// actor's own state.runtime mirror (construction and every SetLoopMode/ChangeLoopInference
// commit) and the Runtime field on the LoopModeChanged/LoopInferenceChanged events those
// commits publish. Dropping a field here silently breaks restore in production — a field
// missing from the durable record can never be folded back on restore no matter how
// correct the restore-fold read side is, exactly the bug fixed by e7da984a (APIFormat/
// BaseURL were grafted at the restore-fold sites before this builder ever populated them
// on write). See the sibling restore-fold sites this function's output ultimately feeds:
// restored.go's NewRestoredWithRuntime and sessionruntime/loop_change.go's
// applyModelRuntime. sessionruntime/session.go's runtimeForModel is the analogous sole
// builder for the session-level LoopStarted.Runtime.
func modelRuntime(model model.Model, effort model.Effort) event.ModelRuntime {
	return event.ModelRuntime{
		Key:       model.Key(),
		Limits:    model.Limits,
		Effort:    effort,
		APIFormat: model.APIFormat,
		BaseURL:   model.BaseURL,
	}
}

const defaultDrainTimeout = 5 * time.Second

// resolveDrainTimeout applies the default when the caller leaves DrainTimeout unset.
func resolveDrainTimeout(d time.Duration) time.Duration {
	if d <= 0 {
		return defaultDrainTimeout
	}
	return d
}

// New constructs a loop and starts its actor goroutine. loopCtx is the loop's
// lifetime (derived by the session from its sessionCtx); it is NOT a turn
// lifetime. sessionID is shared by every loop in the session; loopID is unique
// to this loop. parent is the provenance of the turn/step that spawned this loop
// (zero for the primary loop). events is the session-level event publisher the
// loop depends on (Dependency Inversion); it must be non-nil.
//
// New spawns an EMPTY loop (no committed history, turnIndex 0). The restore path
// (NewRestored) seeds pre-built committed state instead; both funnel through
// newLoopWithSeed, which is identical save for that seed.
func New(loopCtx context.Context, sessionID, loopID uuid.UUID, parent loop.Provenance, events eventPublisher, bound loop.BoundDefinition) (*Loop, error) {
	return NewInMode(loopCtx, sessionID, loopID, parent, events, bound, "")
}

// NewInMode is New with an explicit starting mode: an EMPTY initialMode uses the
// definition's initial mode (identical to New), while a non-empty name starts the loop
// directly in that predeclared mode's effective config — the delegation path's
// mode-selective spawn, so a child begins in the requested mode without a synthetic
// LoopModeChanged. An unknown mode name fails with the same typed BindError Bind uses.
func NewInMode(loopCtx context.Context, sessionID, loopID uuid.UUID, parent loop.Provenance, events eventPublisher, bound loop.BoundDefinition, initialMode loop.ModeName) (*Loop, error) {
	return NewInModeWithCompactor(loopCtx, sessionID, loopID, parent, events, bound, initialMode, nil, nil)
}

// NewInModeWithCompactor is the focused native composition seam for a loop whose
// definition installs compaction. The caller supplies only the summary capability;
// loopruntime derives the executor from the bound loop's own counter, capabilities,
// and policy. Generic hustle selection and coordination remain private.
//
// reviewContext mirrors compactor's shape: internal/sessionruntime is the only
// caller that ever supplies a non-nil value (whenever the session has
// permission classifiers registered — see Session.loopReviewContext), so it
// travels as a parameter here rather than through the bound loop.Definition,
// exactly like compactor does. nil leaves the resulting loop's turns with
// reviewContext == nil, identical to every caller before this addendum.
func NewInModeWithCompactor(
	loopCtx context.Context,
	sessionID, loopID uuid.UUID,
	parent loop.Provenance,
	events eventPublisher,
	bound loop.BoundDefinition,
	initialMode loop.ModeName,
	compactor Compactor,
	reviewContext *ReviewContext,
) (*Loop, error) {
	return NewInModeWithRuntime(
		loopCtx,
		sessionID,
		loopID,
		parent,
		events,
		bound,
		initialMode,
		RuntimeDependencies{Compactor: compactor, ReviewContext: reviewContext},
	)
}

// NewInModeWithRuntime constructs a native loop with its runtime-only
// dependencies while keeping the declarative definition immutable.
func NewInModeWithRuntime(
	loopCtx context.Context,
	sessionID, loopID uuid.UUID,
	parent loop.Provenance,
	events eventPublisher,
	bound loop.BoundDefinition,
	initialMode loop.ModeName,
	deps RuntimeDependencies,
) (*Loop, error) {
	cfg, err := configFromBound(bound, initialMode)
	if err != nil {
		return nil, err
	}
	if err := installRuntimeDependencies(loopCtx, &cfg, deps); err != nil {
		return nil, err
	}
	cfg.reviewContext = deps.ReviewContext.toInternal()
	resolved := initialMode
	if resolved == "" {
		resolved = bound.InitialMode()
	}
	return newLoopWithSeed(loopCtx, sessionID, loopID, parent, events, cfg, bound, resolved, nil)
}

func installRuntimeDependencies(ctx context.Context, cfg *runtimeConfig, deps RuntimeDependencies) error {
	if err := installCompactionExecutor(ctx, cfg, deps.Compactor); err != nil {
		return err
	}
	cfg.Hooks = deps.Hooks
	return nil
}

func newWithConfig(loopCtx context.Context, sessionID, loopID uuid.UUID, parent Provenance, events eventPublisher, cfg runtimeConfig) (*Loop, error) {
	// The raw-config test path has no bound definition, so it carries no predeclared modes:
	// the effective mode is the base ("") and a SetLoopMode is refused (ChangeInvalidMode).
	return newLoopWithSeed(loopCtx, sessionID, loopID, parent, events, cfg, nil, "", nil)
}

// newLoopWithSeed is the construction core shared by New (seed nil → an empty loop)
// and NewRestored (seed non-nil → a loop pre-seeded with committed msgs + turnIndex,
// coming up idle). It runs the identical config validation/defaulting and actor
// goroutine for both; the ONLY difference is whether loopState starts empty or seeded.
// Keeping it one function means the spawn path and the restore path can never drift in
// validation, defaulting, or wiring.
func newLoopWithSeed(loopCtx context.Context, sessionID, loopID uuid.UUID, parent Provenance, events eventPublisher, cfg runtimeConfig, bound loop.BoundDefinition, initialMode loop.ModeName, seed *RestoredState) (*Loop, error) {
	if cfg.Client == nil {
		return nil, &ConfigError{Kind: ConfigMissingClient}
	}
	if err := cfg.Model.Validate(); err != nil {
		return nil, &ConfigError{Kind: ConfigInvalidModel, Cause: err}
	}
	if err := cfg.Model.Key().Validate(); err != nil {
		return nil, &ConfigError{Kind: ConfigInvalidModel, Cause: err}
	}
	if events == nil {
		return nil, &ConfigError{Kind: ConfigMissingPublisher}
	}
	cfg.DrainTimeout = resolveDrainTimeout(cfg.DrainTimeout)
	cfg.Tools = resolveToolSetCaps(cfg.Tools)
	if cfg.idGen == nil {
		cfg.idGen = uuid.New
	}
	if cfg.now == nil {
		cfg.now = time.Now
	}
	if cfg.compactionNow == nil {
		cfg.compactionNow = time.Now
	}
	// The loop mints its own Enduring-event EventID + CreatedAt from the SAME id
	// generator that mints its correlation ids (idGen) and its clock (now), so a
	// test that pins those pins the stamp too. Default it here unless a test
	// injected one — a parked dependency, wired at construction (loopConfig.events
	// is its peer).
	if cfg.eventFactory == nil {
		// Unlike the session (whose Factory closes over the LIVE s.newID/s.now fields,
		// so a post-construction swap is honored), the loop intentionally FREEZES its
		// id-gen + clock into the Factory here at construction: cfg.idGen/cfg.now are
		// captured by value, so a later mutation of cfg is NOT honored. A test pins the
		// stamp by injecting idGen/now (or a whole eventFactory) BEFORE New.
		// idGenerator and event.IDGen are the same underlying func type but distinct
		// named types, so the conversion is explicit.
		cfg.eventFactory = event.NewFactory(event.IDGen(cfg.idGen), cfg.now)
	}
	gates, ok := events.(gateRegistrar)
	if !ok {
		gates = nopGateRegistrar{}
	}
	// reviewStarter is optional: most events publishers (headless mode, or a
	// session build without permission classifiers configured) do not implement
	// it, and a failed assertion simply leaves it nil.
	reviewStarter, _ := events.(permissionReviewStarter)
	commands := make(chan command.Command)
	priorityCommands := make(chan command.Command, compactionPriorityCommandCapacity)
	contextRequests := make(chan contextMeasureRequest)
	contextResults := make(chan contextCountResult, 1)
	contextOutcomes := make(chan contextCompactionOutcomeRequest)
	done := make(chan struct{})
	// gateReg is unbuffered: registration is synchronous (the runner blocks on the
	// ack), and the actor is the sole reader, selecting on it alongside commands.
	gateReg := make(chan gateRegistration)
	// snapshots is unbuffered: the actor is the sole reader and replies on the request's
	// buffered(1) reply channel, so the actor never blocks serving a snapshot.
	snapshots := make(chan snapshotRequest)
	// The loop-goroutine handshake channels are construction-time wiring shared
	// between the actor and the per-turn goroutine, so they live in loopConfig:
	//   - internal: turn terminal hand-back, buffered(1) so a finished turn never blocks.
	//   - commits/drains: per-step commit and tool-continuation drain handshakes;
	//     unbuffered because each is a synchronous request/reply the actor serializes.
	lc := loopConfig{
		loopCtx:          loopCtx,
		cfg:              cfg,
		commands:         commands,
		priorityCommands: priorityCommands,
		contextRequests:  contextRequests,
		contextResults:   contextResults,
		contextOutcomes:  contextOutcomes,
		gateReg:          gateReg,
		snapshots:        snapshots,
		internal:         make(chan turnResult, 1),
		commits:          make(chan commitRequest),
		drains:           make(chan drainRequest),
		admissions:       make(chan admissionResult),
		done:             done,
		events:           events,
		eventFactory:     cfg.eventFactory,
		gates:            gates,
		reviewStarter:    reviewStarter,
		bound:            bound,
	}
	state := newLoopState(sessionID, loopID, parent)
	// Seed the current turn-affecting configuration from the resolved runtimeConfig. New
	// passes the definition's initial mode; NewRestored passes the restore-folded mode (and
	// cfg already carries any restore-folded inference override). A change command later
	// replaces these fields, and the next turn captures whatever is current here.
	//
	// applySetMode builds its OWN separate effectiveConfig literal (see the "next :="
	// literal below) rather than sharing this one — a keyed struct literal doesn't
	// force the two to stay in sync, so a future field added to effectiveConfig must
	// be set at BOTH sites or it silently zero-values on a mode change.
	state.effective = effectiveConfig{
		mode:                initialMode,
		model:               cfg.Model,
		effort:              cfg.Model.Sampling.Effort,
		system:              cfg.System,
		tools:               cfg.Tools,
		inferenceCapability: cfg.InferenceCapability,
	}
	state.runtime = modelRuntime(cfg.Model, cfg.Model.Sampling.Effort)
	if seed != nil {
		// Restore seed: come up with the folded committed history and turn count. The
		// status stays loopIdle (newLoopState's zero default), so the resumed loop
		// accepts the next submit immediately and numbers its next turn from turnIndex.
		// The system prompt is NOT in msgs — it rides cfg.System, sent every turn
		// — so seeding msgs alone reproduces loopState exactly as it was committed.
		state.msgs = cloneMessages(seed.Msgs)
		// Clamped defensively: seed.DerivedPrefix comes from the session's fold
		// (sessionruntime's foldLoop), which should never produce a value
		// outside [0, len(Msgs)], but a restore-time seeding path must never
		// let an out-of-range value make msgsDerivedPrefix slicing panic
		// downstream (installActiveTurn/capturePermissionReviewContext).
		state.msgsDerivedPrefix = clampDerivedPrefix(seed.DerivedPrefix, len(state.msgs))
		state.turnIndex = seed.TurnIndex
		if seed.HasRuntime {
			state.runtime = seed.Runtime
		}
		state.context = seed.Context
		state.hasContext = seed.HasContext
		settings, _ := contextSettings(cfg)
		if err := state.contextTracker.restore(
			seed.Basis,
			seed.HasBasis,
			seed.Context,
			seed.HasContext,
			seed.AutomaticBasis,
			seed.HasAutomaticBasis,
			settings,
		); err != nil {
			return nil, err
		}
		if len(seed.PendingProcessNotifications) > 0 {
			// Restore seed: come up already owning the undelivered process
			// notifications the session reconstructed (already reconciled
			// against durable causality) — the SAME live de-dup guard a fresh
			// delivery populates, so restore and live delivery share one
			// representation with no separate append or dispatch here.
			state.processNotifications = make(map[uuid.UUID]tool.ProcessCompletionNotification, len(seed.PendingProcessNotifications))
			for _, notification := range seed.PendingProcessNotifications {
				state.processNotifications[notification.CommandID] = notification
			}
		}
	}
	go runLoop(lc, state)
	return &Loop{
		Commands:         commands,
		Done:             done,
		priorityCommands: priorityCommands,
		gateReg:          gateReg,
		snapshots:        snapshots,
		hooks:            cfg.Hooks,
	}, nil
}

type loopStatus int

const (
	loopIdle loopStatus = iota
	loopRunning
	loopWaitingAdmission
	loopShuttingDown
)

// queuedInput is an accepted-but-unresolved submit sitting in loopState.inbox,
// and is also the entry handed back to runTurn at a tool-continuation drain (the
// drain hands the actor-owned entries straight to runTurn — same provenance, no
// projection). inputID is the submit command's Header.ID (so CancelQueuedInput can
// remove it by id while it is still queued). triggeredBy is the producing subagent
// loop id for a SubagentResult (zero for a UserInput); the events caused by this
// queued input (TurnStarted/TurnFoldedInto/InputCancelled) stamp it as
// Header.Cause.LoopID, which releases the parent's quiescence wake token.
// triggeredBy is stored now and USED for quiescence in a later phase.
//
// Phase 10 unified the former drain-handback type `foldedMsg` into this one: the
// two were field-identical and the drain converted between them with a struct
// cast, so the second type and its `fold()` projection were dead weight (YAGNI).
type queuedInput struct {
	inputID     uuid.UUID
	triggeredBy uuid.UUID
	// agency is a COPY of the originating submit command's Header.Agency. It is
	// carried alongside inputID/triggeredBy so the submit-resolution events
	// (TurnStarted/TurnFoldedInto/InputCancelled) can stamp Cause.Agency without
	// chasing the command — AgencyUser surfaces "a human started/folded/cancelled
	// this", AgencyMachine (the zero default) otherwise.
	agency identity.Agency
	msg    *content.UserMessage
	// noFold marks a delegate follow-up that must NEVER fold into a running turn at a
	// tool-continuation boundary. drainInbox skips it (and everything behind it), so it
	// stays queued and starts its OWN distinct turn when the current one finishes —
	// preserving the request-id → TurnStarted correlation delegate `send` relies on.
	noFold               bool
	reservedTurnID       uuid.UUID
	rejectOnStartFailure bool
}

type loopState struct {
	// id is this loop's id. In multi-agent sessions each subagent loop gets its
	// own loop id.
	id uuid.UUID

	// sessionID is shared by every loop participating in the same session.
	sessionID uuid.UUID

	// parent is the provenance of whatever spawned this loop (zero for the
	// primary loop). The loop knows its PARENT so it can later stamp Parent* on
	// the events it emits; it never tracks its CHILDREN. The session owns the
	// loop registry and turn tree (SRP).
	//
	// The session event publisher is NOT here: it is a dependency, so it lives in
	// loopConfig.events. loopState holds only identity, status, and accumulated
	// state — the things the actor mutates.
	parent Provenance

	turnIndex         event.TurnIndex
	turnID            uuid.UUID // entity id for the active turn; zero when idle
	causationID       uuid.UUID // active submit command's Header.ID; zero when idle
	status            loopStatus
	cancelTurn        context.CancelFunc
	cancelAdmission   context.CancelFunc
	msgs              content.AgenticMessages // conversation history across turns
	runtime           event.ModelRuntime
	context           event.ContextMeasurement
	hasContext        bool
	contextTracker    contextTracker
	contextGeneration uint64
	// turnHookCtx is the hook-derived observation parent, not the cancellable
	// active-turn child. The actor uses it after runTurn hands back and cancels its
	// child so terminal durability retains hook values without a false cancellation.
	turnHookCall   hook.Call
	turnHookFinish hook.FinishFunc
	turnHookCtx    context.Context

	// msgsDerivedPrefix counts the leading messages in msgs that were generated
	// by context replacement (a compaction summary) rather than authored by a
	// human — the CROSS-TURN, cross-restore counterpart of turnState's own
	// derivedUserPrefix (turn.go), which only tracks a mid-turn replacement and
	// resets every new turn. actorContextReplacement.apply
	// (context_replacement.go) sets this to 1 whenever it replaces msgs with a
	// compaction summary (a *content.UserMessage the compaction Hustle's model
	// call produced, never something a human typed); newLoopWithSeed seeds it
	// from RestoredState.DerivedPrefix on restore. installActiveTurn reads it
	// at the SAME point it clones msgs into a turn's cfg.base, so
	// capturePermissionReviewContext (review_context.go) can keep treating a
	// compaction summary as review evidence without ever crediting it with
	// gate.ReviewContextOriginUser (genuine human authority) — design §8.3's
	// central defense, which a turn-scoped-only guard cannot uphold once the
	// summary survives into a LATER turn's base or a restored loop's seeded
	// history.
	msgsDerivedPrefix int

	// effective is the loop's CURRENT turn-affecting configuration (mode/model/effort/
	// system/tools). startTurn captures a copy of it into the per-turn turnConfig, so a
	// SetLoopMode/ChangeLoopInference that lands mid-turn never disturbs the running turn —
	// it takes effect only when the NEXT turn starts. It is seeded at construction from the
	// resolved runtimeConfig (New) or the restore-folded mode/inference (NewRestored).
	effective effectiveConfig

	// external is the loop's external tool slot, keyed by source. It composes with the
	// CURRENT mode's declared tools into effective.tools.Registry; every write to it and
	// every mode change recompose that registry, so the two can never disagree. It starts
	// empty on New AND on NewRestored — external tools are live resources, never restored
	// from journal bytes; the composing application re-installs after restore.
	external externalSlots

	// inbox is the actor-owned pending-input queue for accepted
	// UserInput/SubagentResult that could not start immediately (a turn was
	// running). Only the actor (runLoop) appends/removes/clears it — no locks. On
	// going idle the actor pops the first entry to start the next turn; on an
	// abnormal terminal it returns the remaining entries via InputCancelled and
	// starts nothing. Bounded by inboxCap (a full inbox rejects with QueueFull).
	// Fold-into-a-running-turn is a later phase; in this phase queued input
	// resolves only by starting a later turn, by CancelQueuedInput, or by
	// abnormal-terminal return.
	inbox []queuedInput

	// draining holds inbox entries the actor popped for a tool-continuation drain but
	// whose TurnFoldedInto has not yet been committed. The drain handshake moves
	// entries here (out of inbox) at the drain point and replies them to runTurn;
	// runTurn then commits a TurnFoldedInto per entry, and the commit point removes
	// the matching entry from draining (it is now resolved). If the turn ends
	// abnormally between the drain and a fold commit, these entries are no longer in
	// inbox, so the abnormal-terminal path returns them from draining too (every
	// removed entry is resolved exactly once). Actor-owned, like inbox.
	draining []queuedInput

	shutdownAcks []chan<- error

	// pendingGates maps an opaque GateID to the gate a parked runner is blocked on.
	// Owned SOLELY by runLoop/the actor; control commands route by GateID and kind,
	// then delete the entry. Cleared on turn end.
	pendingGates map[gatedomain.ID]pendingGate

	// processNotifications is the actor-owned bounded set of accepted process
	// completion notifications (Task 24C), keyed by their stable CommandID. It
	// is deliberately SEPARATE from inbox: a completion notification is
	// metadata-only (no content.UserMessage to fold into a turn), so it never
	// touches turn-start/tool-continuation machinery. It serves two jobs:
	//   - a live de-dup guard, so an at-least-once redelivery of the SAME
	//     CommandID (a crash between the supervisor's dispatch and its causality
	//     commit) is dropped rather than double counted;
	//   - restore seeding, populated directly from RestoredState.
	//     PendingProcessNotifications at construction — already reconciled
	//     against durable causality by the session, bypassing live dispatch.
	// Bounded like the ordinary managed queues (loop.ManagedInputQueueCapacity):
	// a full set is never grown further; the caller (Tools' supervisor) retries
	// dispatch later with the same CommandID against the durable command, which
	// remains authoritative (see command.ProcessNotificationStopped).
	processNotifications map[uuid.UUID]tool.ProcessCompletionNotification
}

// newLoopState builds the actor-owned loop state with its identity (sessionID,
// loopID, parent provenance). The session event publisher is a dependency, not
// state, so it lives in loopConfig — not here. pendingGates is initialized so the
// actor can route gate commands without a nil-map panic.
// clampDerivedPrefix bounds a leading-derived-message count to [0, total],
// defense in depth against an out-of-range value from a restore seed —
// installActiveTurn/capturePermissionReviewContext slice on this count, so it
// must never exceed the length of the messages it describes.
func clampDerivedPrefix(prefix, total int) int {
	if prefix < 0 {
		return 0
	}
	if prefix > total {
		return total
	}
	return prefix
}

func newLoopState(sessionID, loopID uuid.UUID, parent Provenance) loopState {
	return loopState{
		id:           loopID,
		sessionID:    sessionID,
		parent:       parent,
		pendingGates: make(map[gatedomain.ID]pendingGate),
		external:     make(externalSlots),
	}
}

// replyProcessNotification sends v on result iff result is non-nil (a
// restore-reconstructed redelivery and a decoded wire record both carry a nil
// Result — command.ProcessNotification.Result is a caller-owned buffered
// channel, so this never blocks the actor).
func replyProcessNotification(result chan<- command.ProcessNotificationResult, v command.ProcessNotificationResult) {
	if result == nil {
		return
	}
	result <- v
}

// turnResult is the turn goroutine's hand-back to the actor. The conversation is
// committed INCREMENTALLY through the per-step commit handshake (commitRequest), so
// turnResult no longer carries the whole-turn message slice: it carries only the
// turn terminal for the actor to deliver. A failed/interrupted turn discards only
// the in-flight incomplete step; committed steps already live in loopState.msgs.
type turnResult struct {
	terminal event.Event // TurnDone, TurnFailed, or TurnInterrupted
}

// cancelReasonFor maps an abnormal turn terminal to the CancelReason stamped on
// the InputCancelled events that return still-queued input. A TurnInterrupted maps
// to CancelTurnInterrupted; anything else (TurnFailed, and a shutdown that ended a
// TurnDone) maps to CancelTurnFailed — the queued input never started, so from the
// client's view it was not completed.
func cancelReasonFor(terminal event.Event) event.CancelReason {
	if _, ok := terminal.(event.TurnInterrupted); ok {
		return event.CancelTurnInterrupted
	}
	return event.CancelTurnFailed
}

// commitRequest is one per-step commit handshake from the turn goroutine to the
// actor: the finalized step group to append to loopState.msgs and the Enduring
// StepDone event to emit at the same actor-owned point. ack is buffered(1); the
// actor closes it after committing+emitting so the parked runTurn unblocks. The
// turn goroutine selects on ack AND turnCtx.Done so an Interrupt/Shutdown frees it.
// ctx is the Step-derived observation context used by the actor for the durable
// StepDone/TurnFoldedInto boundary; it is never reconstructed from ambient state.
type commitRequest struct {
	ctx    context.Context
	commit turnCommit
	ack    chan<- error
}

// drainRequest is the tool-continuation drain handshake from the turn goroutine to
// the actor. The actor (the inbox's sole owner) pops + clears the inbox in order,
// moves the popped entries into loopState.draining (so an abnormal terminal still
// returns them), and sends those entries on reply. reply is
// buffered(1): the actor never blocks sending it, and a runTurn that escapes on
// turnCtx.Done (an Interrupt during the handshake) leaves the reply in the buffer
// harmlessly — the moved entries are resolved by returnQueuedInbox via the draining
// buffer on the resulting abnormal terminal, so nothing is stranded.
type drainRequest struct {
	reply chan<- []queuedInput
}

type admissionResult struct {
	release func()
	start   TurnStartCapability
	err     error
}

// runLoop is the loop goroutine started by New. It is the only goroutine that
// mutates loopState, installs or clears the active turn, commits or discards turn
// messages, emits TurnStarted/StepDone/TurnFoldedInto at the same points it mutates
// loopState.msgs, and resolves pending gates.
//
// cfg (loopConfig) holds the dependencies and construction-time wiring; state
// (loopState) holds the identity, status, and accumulated messages runLoop
// evolves. Both are kept as locals here. The handshake channels (internal/
// commits/drains) and the gate-registration channel live in cfg because they are
// construction-time wiring shared with the per-turn goroutine; cfg.gateReg is
// bidirectional because the per-turn goroutine hands its SEND side to runTurn so a
// parked tool can register a gate (a receive-only handle could not be narrowed to
// the send-only direction runTurn requires).
func runLoop(cfg loopConfig, state loopState) {
	defer close(cfg.done)

	// Locals alias the config's wiring so the actor body reads against the same
	// names the per-turn handshakes use. ctx is the loop lifetime (cfg.loopCtx);
	// each turn ctx derives from it. runtimeConfig (the caller's loop config) is cfg.cfg.
	ctx := cfg.loopCtx
	config := cfg.cfg
	commands := cfg.commands
	priorityCommands := cfg.priorityCommands
	contextRequests := cfg.contextRequests
	contextResults := cfg.contextResults
	contextOutcomes := cfg.contextOutcomes
	idleCompactionResults := make(chan idleCompactionCountResult, 1)
	gateReg := cfg.gateReg
	snapshots := cfg.snapshots
	internal := cfg.internal
	commits := cfg.commits
	drains := cfg.drains
	admissions := cfg.admissions
	requestCancelActive := false
	compactions := newCompactionControl(compactionControlWaiterCapacity)
	var requestStartAdmission func() bool
	resumeQueuedAfterCompaction := func() {
		if state.status == loopIdle && !compactions.blocksInput() && requestStartAdmission != nil {
			requestStartAdmission()
		}
	}
	var idleCompaction *idleCompactionPreparation
	compactionFinalizations := newCompactionFinalizer(compactionFinalizerConfig{
		Publisher: cfg.events, Factory: cfg.eventFactory, SessionID: state.sessionID, LoopID: state.id,
		Now: config.compactionNow,
	})

	// publish sends a FULL-FIDELITY loop event to the session-level event fan-in.
	// Producer identity is stamped here from loopState/the active turn (the actor IS
	// the loop producer — this is the loop stamping its own identity, not the fan-in
	// inferring it), so the fan-in's EventFilter (per-loop LoopID) and its
	// applyActivity quiescence transitions (TurnStarted/LoopIdle/TurnFoldedInto/
	// InputCancelled) see the ids they need. The fan-in is non-blocking and
	// class-aware (Ephemeral drop / Enduring fail-close), so this NEVER blocks the
	// actor. The session owns the session-scoped SessionStarted it delivers to the
	// fan-in's subscribers; the loop never emits one. PublishEvent returns nil even
	// with no subscribers (the headless case), so a non-nil error is a genuine
	// fault; log it and continue (event publication must not abort the loop). Every
	// loop event reaches consumers through this single fan-in path.
	//
	// This is also the single point that mints the persistence identity (EventID +
	// CreatedAt) for every ENDURING event: stampLoopHeader fills the producer
	// COORDINATES, then the Factory stamps EventID + CreatedAt for the Enduring class
	// plus the low-volume CompactionStarted progress event. Other Ephemeral events
	// (TokenDelta/ToolCall*/InputQueued) remain unstamped, avoiding per-token crypto/rand.
	// On a mint failure we FAIL SECURE: log loudly and SKIP publishing the event
	// rather than fan out a zero-EventID one (a journal would key on a zero
	// idempotency key) or silently pretend it published.
	stamp := func(ev event.Event) (event.Event, error) {
		stamped, err := stampLoopEvent(ev, cfg.eventFactory, state.sessionID, state.id, state.turnID)
		if err != nil {
			// A crypto/rand mint failure is catastrophic and astronomically rare; drop
			// the event fail-secure rather than publish a required zero EventID.
			slog.Error("event id mint failed; dropping loop event (fail-secure)",
				"event", fmt.Sprintf("%T", ev), "error", err)
			return nil, err
		}
		return stamped, nil
	}
	publishContext := func(publishCtx context.Context, ev event.Event) {
		stamped, err := stamp(ev)
		if err != nil {
			return
		}
		if publishCtx == nil {
			publishCtx = ctx
		}
		if err := cfg.events.PublishEvent(publishCtx, stamped); err != nil {
			slog.Error("loop event publish to session fan-in failed", "error", err)
		}
	}
	publish := func(ev event.Event) { publishContext(ctx, ev) }
	publishTurnStarted := func(publishCtx context.Context, ev event.TurnStarted, capability TurnStartCapability) (bool, error) {
		if capability == nil {
			err := cfg.events.PublishEventChecked(publishCtx, ev)
			return err == nil, err
		}
		return capability.PublishTurnStarted(publishCtx, ev)
	}
	commitStampedBoundary := func(commitCtx context.Context, stamped event.Event) error {
		if boundary, ok := cfg.events.(executionBoundary); ok {
			return boundary.CommitBoundary(commitCtx, stamped)
		}
		return cfg.events.PublishEventChecked(commitCtx, stamped)
	}
	commitStampedContextBoundary := func(commitCtx context.Context, stamped event.Event) (bool, error) {
		if boundary, ok := cfg.events.(contextExecutionBoundary); ok {
			return boundary.CommitContextBoundary(commitCtx, stamped)
		}
		if boundary, ok := cfg.events.(executionBoundary); ok {
			err := boundary.CommitBoundary(commitCtx, stamped)
			return err == nil, err
		}
		err := cfg.events.PublishEventChecked(commitCtx, stamped)
		return err == nil, err
	}
	commitBoundary := func(commitCtx context.Context, ev event.Event) (event.Event, error) {
		stamped, err := stamp(ev)
		if err != nil {
			return nil, err
		}
		return stamped, commitStampedBoundary(commitCtx, stamped)
	}

	// publishAcceptance is the narrow transactional publication path for managed
	// delegate admission. Unlike ordinary loop emission it returns both EventID mint
	// and checked durable-append failures to the actor, which must decline the input.
	publishAcceptance := func(commandID uuid.UUID) error {
		ev := stampLoopHeader(event.DelegateRequestAccepted{Header: event.Header{Cause: identity.Cause{CommandID: commandID}}}, state.sessionID, state.id, state.turnID)
		h, err := cfg.eventFactory.Stamp(ev.EventHeader())
		if err != nil {
			return err
		}
		return cfg.events.PublishEventChecked(ctx, withLoopHeader(ev, h))
	}

	reportCompactionFailure := func(waiterCommandIDs []uuid.UUID, err error) {
		waiters := append([]uuid.UUID(nil), waiterCommandIDs...)
		failure := compactionFailure{WaiterCommandIDs: waiters, Err: err}
		if sink, ok := config.compactionSink.(compactionFailureSink); ok {
			sink.ReportCompactionFailure(ctx, failure)
			return
		}
		slog.Error("compaction coordination failed", "waiter_count", len(waiters), "error", err)
	}

	publishLaneFull := func(request command.Compact, attemptID event.CompactAttemptID) error {
		commandID := request.CommandHeader().CommandID
		rejected := event.CompactWaiterRejected{
			Header: event.Header{
				EventID: event.CompactWaiterReplyID(attemptID, commandID, false),
				Cause:   identity.Cause{CommandID: commandID, Agency: request.CommandHeader().Agency},
			},
			AttemptID: attemptID,
			Reason:    event.CompactRejectControlLaneFull,
		}
		stamped, err := stamp(rejected)
		if err != nil {
			return &CompactionCoordinationError{Kind: CompactionCoordinationOutcome, Cause: err}
		}
		if err := event.ValidateEvent(stamped); err != nil {
			return &CompactionCoordinationError{Kind: CompactionCoordinationOutcome, Cause: err}
		}
		if err := cfg.events.PublishEventChecked(ctx, stamped); err != nil {
			return &CompactionCoordinationError{Kind: CompactionCoordinationOutcome, Cause: err}
		}
		return nil
	}

	publishPreStartRejection := func(disposition compactionDisposition) error {
		if disposition.Kind != compactionDispositionReject || disposition.Attempt == nil || !disposition.RejectReason.Valid() {
			return &CompactionCoordinationError{Kind: CompactionCoordinationOutcome}
		}
		for _, commandID := range disposition.Attempt.WaiterCommandIDs {
			rejected := event.CompactWaiterRejected{
				Header: event.Header{
					EventID: event.CompactWaiterReplyID(disposition.Attempt.AttemptID, commandID, false),
					Cause:   identity.Cause{CommandID: commandID},
				},
				AttemptID: disposition.Attempt.AttemptID,
				Reason:    disposition.RejectReason,
			}
			stamped, err := stamp(rejected)
			if err != nil {
				return &CompactionCoordinationError{Kind: CompactionCoordinationOutcome, Cause: err}
			}
			if err := event.ValidateEvent(stamped); err != nil {
				return &CompactionCoordinationError{Kind: CompactionCoordinationOutcome, Cause: err}
			}
			if err := cfg.events.PublishEventChecked(ctx, stamped); err != nil {
				return &CompactionCoordinationError{Kind: CompactionCoordinationOutcome, Cause: err}
			}
		}
		return nil
	}

	dispatchCompactionBoundary := func(
		operationCtx context.Context,
		boundary compactionBoundaryKind,
		candidate *compactionExecutionCandidate,
	) bool {
		if config.compactionSink == nil {
			return false
		}
		if operationCtx == nil {
			operationCtx = ctx
		}
		pending := compactions.pendingAttempt()
		if idleCompaction != nil && pending != nil && pending.AttemptID == idleCompaction.attemptID &&
			compactions.cancellationRejectReason() == event.CompactRejectUnspecified {
			// This attempt owns an exact idle snapshot. A turn or drain that lands while
			// its count is in flight must not replace it with a newer candidate.
			return true
		}
		if pending != nil && pending.Basis == (event.ContextBasis{}) {
			basis := state.contextTracker.currentBasis()
			if candidate != nil {
				basis = candidate.Measurement.Basis
			}
			if basis == (event.ContextBasis{}) && compactions.cancellationRejectReason() == event.CompactRejectUnspecified {
				return false
			}
			if basis != (event.ContextBasis{}) {
				if err := compactions.freezeBasis(pending.AttemptID, basis); err != nil {
					reportCompactionFailure(pending.WaiterCommandIDs, err)
					return false
				}
			}
		}
		disposition := compactions.atBoundary(boundary)
		if disposition.Kind == compactionDispositionNone {
			return true
		}
		if disposition.Kind == compactionDispositionReject {
			if err := publishPreStartRejection(disposition); err != nil {
				waiters := []uuid.UUID(nil)
				if disposition.Attempt != nil {
					waiters = disposition.Attempt.WaiterCommandIDs
				}
				reportCompactionFailure(waiters, err)
				return false
			}
			resumeQueuedAfterCompaction()
			return true
		}
		attempt := disposition.Attempt
		if attempt == nil {
			return false
		}
		attempt = compactions.markStarted(attempt.AttemptID, config.compactionNow())
		if attempt == nil {
			reportCompactionFailure(nil, &CompactionCoordinationError{Kind: CompactionCoordinationOutcome})
			return false
		}
		disposition.Attempt = attempt
		hookCtx := operationCtx
		if candidate != nil && config.Hooks.Handles(hook.OperationCompaction) {
			maxSummaryTokens := content.TokenCount(0)
			if config.Compaction != nil {
				maxSummaryTokens = config.Compaction.MaxSummaryTokens
			}
			input := &loop.CompactionInput{
				Basis: attempt.Basis, Model: candidate.Measurement.Model,
				RequestFingerprint: candidate.Measurement.RequestFingerprint,
				Transcript:         cloneMessages(candidate.Transcript), MaxSummaryTokens: maxSummaryTokens,
			}
			call := hook.Call{
				Operation: hook.OperationCompaction, StartedAt: hookNow(config.now),
				Coordinates: identity.Coordinates{
					SessionID: state.sessionID, LoopID: state.id, TurnID: state.turnID,
				},
				AgentName: config.AgentName, Cause: attempt.Cause,
				Compaction: &hook.CompactionData{AttemptID: attempt.AttemptID, Input: input},
			}
			var finish hook.FinishFunc
			var hookErr error
			hookCtx, finish, hookErr = config.Hooks.Start(operationCtx, call)
			scope := &compactionHookScope{ctx: hookCtx, call: call, finish: finish}
			disposition.hookScope = scope
			disposition.input = input
			if hookErr != nil {
				reason := event.CompactRejectInternal
				outcome := hook.OutcomeFailed
				var guardErr *hook.GuardError
				if !errors.As(hookErr, &guardErr) {
					if _, denied := hook.AsDenial(hookErr); denied {
						reason = event.CompactRejectUnavailable
						outcome = hook.OutcomeDenied
					}
				}
				scope.setTerminal(outcome, hookErr, nil)
				rejected := rejectedCompactionResult(reason)
				rejected.Proposal.hookScope = scope
				disposition.preRejected = &rejected
				if candidateSink, ok := config.compactionSink.(compactionCandidateSink); ok {
					if err := candidateSink.CoordinateCompactionCandidate(hookCtx, disposition, *candidate); err == nil {
						return true
					}
				}
				_, finalizationErr := compactionFinalizations.Finalize(
					hookCtx, *attempt, compactionFinalizationProposal{RejectReason: reason, hookScope: scope},
				)
				if finalizationErr != nil {
					reportCompactionFailure(attempt.WaiterCommandIDs, finalizationErr)
					return false
				}
				compactions.complete(attempt.AttemptID)
				resumeQueuedAfterCompaction()
				return true
			}
		}
		invoked := false
		coordinateErr := publishCompactionStartedBeforeInference(
			hookCtx, cfg.events, cfg.eventFactory, state.sessionID, state.id,
			event.CompactionStarted{AttemptID: attempt.AttemptID, Reason: attempt.Reason, Basis: attempt.Basis},
			func(inferCtx context.Context) error {
				invoked = true
				if candidate != nil {
					if candidateSink, ok := config.compactionSink.(compactionCandidateSink); ok {
						return candidateSink.CoordinateCompactionCandidate(inferCtx, disposition, *candidate)
					}
				}
				return config.compactionSink.CoordinateCompaction(inferCtx, disposition)
			},
		)
		if coordinateErr == nil {
			return true
		}
		if !invoked && isFatalPublication(coordinateErr) {
			compactions.abort(attempt.AttemptID)
			disposition.hookScope.finishInfrastructure(coordinateErr)
			reportCompactionFailure(attempt.WaiterCommandIDs, &CompactionCoordinationError{
				Kind: CompactionCoordinationOutcome, Cause: coordinateErr,
			})
			return false
		}
		rejectReason := event.CompactRejectProgressPublication
		if invoked {
			rejectReason = event.CompactRejectExecutionFailed
		}
		disposition.hookScope.setTerminal(hook.OutcomeFailed, &compactionOperationError{Reason: rejectReason}, nil)
		_, finalizationErr := compactionFinalizations.Finalize(
			hookCtx, *attempt,
			compactionFinalizationProposal{RejectReason: rejectReason, hookScope: disposition.hookScope},
		)
		if finalizationErr != nil {
			reportCompactionFailure(attempt.WaiterCommandIDs, finalizationErr)
			return false
		}
		compactions.complete(attempt.AttemptID)
		resumeQueuedAfterCompaction()
		return false
	}

	startIdleCompactionPreparation := func(attemptID event.CompactAttemptID) bool {
		if _, candidateAware := config.compactionSink.(compactionCandidateSink); !candidateAware {
			return false
		}
		if _, awaitable := config.compactionSink.(contextCompactionAwaiter); !awaitable {
			return false
		}
		settings, configured := contextSettings(config)
		basis := state.contextTracker.currentBasis()
		if !configured || config.ContextCounter == nil || basis.Revision == 0 || basis.ThroughEventID.IsZero() {
			return false
		}
		transcript := cloneMessages(state.msgs)
		tools := state.effective.tools
		tools.Registry = append([]tool.InvokableTool(nil), tools.Registry...)
		preparationCtx, cancelPreparation := context.WithCancel(ctx)
		preparation := idleCompactionPreparation{
			attemptID: attemptID, basis: basis, generation: state.contextGeneration,
			cancel: cancelPreparation,
			request: inference.Request{
				Model: state.effective.model.Clone(), System: state.effective.system,
				Messages: cloneMessages(transcript),
			},
			tools: tools, transcript: transcript,
			inferenceCapability: state.effective.inferenceCapability,
		}
		idleCompaction = &preparation
		go func(preparationCtx context.Context, prepared idleCompactionPreparation, admission contextAdmissionSettings) {
			request := prepared.request
			request.Tools = toolDefs(preparationCtx, prepared.tools.Registry)
			runtimeRevision := revisionDigest(nil)
			measurement, err := measureRequestContext(
				preparationCtx, config.ContextCounter, config.CounterCapability, prepared.inferenceCapability,
				admission, prepared.basis, request, runtimeRevision,
			)
			result := idleCompactionCountResult{
				preparation: prepared,
				candidate: compactionExecutionCandidate{
					Measurement: measurement, Request: request, RuntimeRevision: runtimeRevision,
					Transcript: cloneMessages(prepared.transcript), InferenceCapability: prepared.inferenceCapability,
				},
				err: err,
			}
			select {
			case idleCompactionResults <- result:
			case <-ctx.Done():
			}
		}(preparationCtx, preparation, settings)
		return true
	}

	finalizeIdleCompactionRejection := func(preparation idleCompactionPreparation, reason event.CompactRejectReason) {
		pending := compactions.pendingAttempt()
		if pending == nil || pending.AttemptID != preparation.attemptID {
			return
		}
		if err := compactions.freezeBasis(preparation.attemptID, preparation.basis); err != nil {
			reportCompactionFailure(pending.WaiterCommandIDs, err)
			return
		}
		attempt := compactions.pendingAttempt()
		if attempt == nil {
			return
		}
		attempt.StartedAt = config.compactionNow()
		terminal, err := compactionFinalizations.Finalize(ctx, *attempt, compactionFinalizationProposal{RejectReason: reason})
		if err != nil {
			reportCompactionFailure(attempt.WaiterCommandIDs, err)
			compactions.abort(attempt.AttemptID)
			return
		}
		compactions.complete(attempt.AttemptID)
		if rejected, ok := terminal.(event.CompactionRejected); ok && rejected.Reason == event.CompactionReasonAutomatic {
			state.contextTracker.exhaustAutomatic(rejected.Basis, rejected.Reason, true)
		}
		resumeQueuedAfterCompaction()
	}

	awaitIdleCompaction := func(attemptID event.CompactAttemptID) {
		awaiter, ok := config.compactionSink.(contextCompactionAwaiter)
		if !ok {
			return
		}
		go func() {
			outcome, err := awaiter.AwaitCompaction(ctx, attemptID)
			if err != nil {
				reportCompactionFailure(nil, &contextCompactionAwaitError{AttemptID: attemptID, Cause: err})
				return
			}
			reply := make(chan contextCompactionOutcomeReply, 1)
			select {
			case contextOutcomes <- contextCompactionOutcomeRequest{attemptID: attemptID, result: outcome, reply: reply}:
			case <-ctx.Done():
				outcome.Proposal.hookScope.finishInfrastructure(&CompactionCoordinationError{
					Kind: CompactionCoordinationOutcome, Cause: ctx.Err(),
				})
				return
			}
			select {
			case response := <-reply:
				if response.err != nil {
					reportCompactionFailure(nil, response.err)
				}
			case <-ctx.Done():
			}
		}()
	}

	// emitLoopIdle announces the loop's running->idle transition: an Enduring,
	// non-terminal LoopIdle carrying the loop's identity (SessionID + LoopID;
	// TurnID is zero — it is loop-scoped, not turn-scoped) and the loop's immutable
	// attribution name. The session quiescence
	// model removes this loop's {loop, LoopID} activity key on it, so a primary-only
	// synchronous session reaches SessionIdle exactly when the primary loop parks. It
	// goes to the session fan-in (for quiescence). It is emitted ONLY on a genuine
	// running->idle transition: never between chained turns (running->running),
	// and shutdown-induced idling does not emit it because the actor returns before
	// reaching the emit point (or has already flipped to SessionStopped at the session).
	//
	// AgentName is stamped here from the actor's own resolved config — the same
	// bound.Name() the session stamps onto this loop's LoopStarted — because a
	// consumer that keys off the idle boundary (an external tool catalog adopting a
	// new generation, say) selects loops BY AGENT, and identity alone cannot answer
	// "which agent parked?" without a LoopStarted join. It is empty for a plain
	// (unnamed) loop, matching LoopStarted, and Header.AgentName is omitzero — so a
	// journal written before this stamp existed stays byte-compatible.
	emitLoopIdle := func() {
		publish(event.LoopIdle{Header: event.Header{
			Coordinates: identity.Coordinates{SessionID: state.sessionID, LoopID: state.id},
			AgentName:   config.AgentName,
		}})
	}

	ackShutdowns := func(err error) {
		for _, ack := range state.shutdownAcks {
			ack <- err
		}
		state.shutdownAcks = nil
	}

	// routeControl delivers a control command (Approve/Deny/ProvideUserInput) to
	// the parked runner blocked on its GateID, but ONLY if a gate is open for that
	// GateID AND the gate kind accepts this command kind. On a match it delivers
	// once (the gate's reply channel is buffered(1) and the runner is its sole
	// reader, so the send never blocks the actor) and deletes the gate so a
	// duplicate cannot deliver twice. Any miss — no gate (wrong/unknown GateID,
	// stale or duplicate command) or a kind mismatch — is silently DROPPED
	// (fail-safe): the actor never blocks and never panics.
	routeControl := func(cmd command.Command, route command.GateRoute) {
		gateID := route.GateID
		if gateID.IsZero() {
			gateID = route.ToolExecutionID
		}
		g, ok := state.pendingGates[gateID]
		if !ok || !accepts(g.kind, cmd) {
			return
		}
		g.reply <- cmd
		delete(state.pendingGates, gateID)
	}

	// clearGates drops every open gate at turn end / cancellation. A parked runner
	// is already unblocking via <-ctx.Done() (the turn ctx is cancelled), so the
	// reply channels are simply abandoned; the actor must not hold stale entries
	// that a late control command for a finished turn could match.
	clearGates := func() {
		if len(state.pendingGates) > 0 {
			for gateID := range state.pendingGates {
				_ = cfg.gates.CloseGate(ctx, gateID, gatedomain.CloseAbandoned)
			}
			state.pendingGates = make(map[gatedomain.ID]pendingGate)
		}
	}

	// installActiveTurn installs the active-turn fields on loopState after the opening
	// event is durable, and derives the
	// turn ctx from the loop ctx (submit commands carry no context, so a turn's
	// lifetime is bounded by the loop's, not by any caller's API-call ctx). It then
	// commits the initial UserMessage into loopState.msgs. TurnStarted's Cause.LoopID carries
	// qi.triggeredBy: set for a SubagentResult, zero for a UserInput). It returns the
	// derived turn ctx, the defensive base clone the per-turn goroutine reads, and
	// the leading-derived-message count (state.msgsDerivedPrefix) that describes
	// base — captured at the SAME point as the clone, before qi.msg is appended,
	// so cfg.baseDerivedPrefix always describes exactly the base it travels with.
	// This is the live-commit half of starting a turn (distinct from
	// assembling the per-turn turnConfig).
	installActiveTurn := func(parentCtx context.Context, turnID uuid.UUID, qi queuedInput) (context.Context, content.AgenticMessages, int) {
		state.turnIndex++
		state.turnID = turnID
		state.causationID = qi.inputID
		state.status = loopRunning
		turnCtx, cancel := context.WithCancel(parentCtx)
		state.cancelTurn = cancel

		// base is a defensive deep clone of pre-turn history, taken BEFORE the
		// initial UserMessage is committed (runTurn reads it
		// concurrently while the actor keeps appending committed step groups).
		base := cloneMessages(state.msgs)
		baseDerivedPrefix := state.msgsDerivedPrefix

		// Loop-owned incremental commit: commit the initial UserMessage and emit
		// TurnStarted (Message + Cause.CommandID = inputID + InputID = inputID) was
		// durably published immediately before this actor-owned mutation.
		state.msgs = append(state.msgs, qi.msg)
		state.context = event.ContextMeasurement{}
		state.hasContext = false
		return turnCtx, base, baseDerivedPrefix
	}

	// buildTurnConfig assembles the per-turn turnConfig: the static deps (base/model/
	// tools/client/gateReg/idGen), the runtime-context provider (consulted ONCE per turn
	// by runTurn on the turn goroutine — never here on the actor, since it may run git),
	// plus the two ctx-cancellable handshake closures the turn goroutine calls back
	// through, and the publish-only emit. This is the WIRING half of starting a turn
	// (distinct from committing + announcing it).
	//
	//   - commit: per-step commit handshake. Selects on the buffered(1) ack AND
	//     turnCtx.Done so an Interrupt/Shutdown during the handshake frees runTurn.
	//   - drainPending: tool-continuation drain handshake, ctx-cancellable exactly
	//     like commit. The reply is buffered(1), so the actor's send never blocks even
	//     if runTurn already escaped on turnCtx.Done (the moved-out inbox entries are
	//     then resolved from loopState.draining by the abnormal-terminal return path).
	//   - emit: the turn goroutine's event emit, publish-only (every loop event reaches
	//     consumers through the session fan-in). publish never blocks the actor, so the
	//     turn goroutine cannot be pinned by a slow consumer.
	buildTurnConfig := func(base content.AgenticMessages, baseDerivedPrefix int, firstAdmission func()) turnConfig {
		admit := func(context.Context) (func(), error) { return func() {}, nil }
		if admission, ok := cfg.events.(executionAdmission); ok {
			admit = func(ctx context.Context) (func(), error) {
				return admission.EnterExecution(ctx, state.id)
			}
		}
		commit := func(cctx context.Context, tc turnCommit) error {
			ack := make(chan error, 1)
			req := commitRequest{ctx: cctx, commit: tc, ack: ack}
			select {
			case commits <- req:
			case <-cctx.Done():
				return &CommitError{Reason: CommitTurnCancelled, Cause: cctx.Err()}
			}
			select {
			case err := <-ack:
				return err
			case <-cctx.Done():
				return &CommitError{Reason: CommitTurnCancelled, Cause: cctx.Err()}
			}
		}
		drainPending := func(cctx context.Context) ([]queuedInput, error) {
			reply := make(chan []queuedInput, 1)
			req := drainRequest{reply: reply}
			select {
			case drains <- req:
			case <-cctx.Done():
				return nil, &CommitError{Reason: CommitTurnCancelled, Cause: cctx.Err()}
			}
			select {
			case batch := <-reply:
				return batch, nil
			case <-cctx.Done():
				return nil, &CommitError{Reason: CommitTurnCancelled, Cause: cctx.Err()}
			}
		}
		var measure func(context.Context, inference.Request, string, *content.UserMessage, bool) error
		if _, configured := contextSettings(config); configured && config.ContextCounter != nil {
			measure = func(cctx context.Context, request inference.Request, runtimeRevision string, runtimeTail *content.UserMessage, continuation bool) error {
				for {
					reply := make(chan contextMeasureReply, 1)
					req := contextMeasureRequest{ctx: cctx, request: request, runtimeTail: cloneUserMessage(runtimeTail), runtimeContextRevision: runtimeRevision, reply: reply}
					select {
					case cfg.contextRequests <- req:
					case <-cctx.Done():
						return normalizeContextCountError(request.Model.Key(), config.CounterCapability.Quality, cctx.Err())
					}
					var measured contextMeasureReply
					select {
					case measured = <-reply:
					case <-cctx.Done():
						return normalizeContextCountError(request.Model.Key(), config.CounterCapability.Quality, cctx.Err())
					}
					if measured.err != nil {
						return measured.err
					}
					if measured.awaiter == nil {
						return nil
					}
					outcome, err := measured.awaiter.AwaitCompaction(cctx, measured.attemptID)
					if err != nil {
						return &contextCompactionAwaitError{AttemptID: measured.attemptID, Cause: err}
					}
					outcomeReply := make(chan contextCompactionOutcomeReply, 1)
					outcomeRequest := contextCompactionOutcomeRequest{attemptID: measured.attemptID, result: outcome, reply: outcomeReply}
					select {
					case cfg.contextOutcomes <- outcomeRequest:
					case <-ctx.Done():
						handoffErr := &CompactionCoordinationError{
							Kind: CompactionCoordinationOutcome, Cause: ctx.Err(),
						}
						outcome.Proposal.hookScope.finishInfrastructure(handoffErr)
						return handoffErr
					}
					var disposition contextCompactionOutcomeReply
					select {
					case disposition = <-outcomeReply:
					case <-ctx.Done():
						return &CompactionCoordinationError{Kind: CompactionCoordinationOutcome, Cause: ctx.Err()}
					}
					if disposition.err != nil {
						return disposition.err
					}
					if disposition.retry {
						continue
					}
					switch disposition.disposition {
					case contextCompactionAwaitRejected:
						if !continuation {
							switch disposition.rejectReason {
							case event.CompactRejectInterrupted, event.CompactRejectShuttingDown, event.CompactRejectCanceled:
								return &terminalCompactionCancellationError{Reason: disposition.rejectReason}
							}
							return nil
						}
						if disposition.continuationError != nil {
							var summaryTooLarge *loop.SummaryTooLargeError
							var unknownLimit *loop.ContextLimitUnknownError
							if errors.As(disposition.continuationError, &summaryTooLarge) || errors.As(disposition.continuationError, &unknownLimit) {
								return disposition.continuationError
							}
						}
						if measured.measurement.InputTokens >= measured.measurement.InputLimit {
							return &loop.ContextLimitError{Measurement: measured.measurement}
						}
						return nil
					case contextCompactionAwaitCommitted:
						if disposition.replacement == nil {
							return &contextCompactionOutcomeError{AttemptID: measured.attemptID}
						}
						return &contextReplacementDirective{
							AttemptID:   measured.attemptID,
							Replacement: turnContextReplacement{Summary: cloneUserMessage(disposition.replacement.Summary)},
						}
					default:
						return &contextCompactionAwaitError{AttemptID: measured.attemptID}
					}
				}
			}
		}
		// model/system/tools come from the loop's CURRENT effective config (captured here,
		// at turn start, into this per-turn value), so a change that landed since the last
		// turn takes effect now while a change that lands DURING this turn does not. The
		// remaining fields are immutable loop wiring, so they ride the frozen config.
		return turnConfig{
			base:                    base,
			baseDerivedPrefix:       baseDerivedPrefix,
			runtimeContext:          config.RuntimeContext,
			model:                   state.effective.model,
			system:                  state.effective.system,
			tools:                   state.effective.tools,
			output:                  cloneOutputSchema(config.Output),
			client:                  config.Client,
			hooks:                   config.Hooks,
			agentName:               config.AgentName,
			now:                     config.now,
			gateReg:                 gateReg,
			idGen:                   config.idGen,
			admit:                   admit,
			firstAdmission:          firstAdmission,
			measure:                 measure,
			commit:                  commit,
			drainPending:            drainPending,
			emit:                    publish,
			emitContext:             publishContext,
			afterDrain:              config.afterDrain,
			afterContextReplacement: config.afterContextReplacement,
			reviewContext:           config.reviewContext,
		}
	}

	// startTurn begins a turn FROM an accepted submit (qi). It is the single
	// commit-then-start path shared by an idle submit and the on-idle inbox pop. It
	// mints the TurnID, stamps and preflights the context mutation, durably publishes
	// TurnStarted, then installs the corresponding live turn state,
	// assembles the per-turn config (buildTurnConfig), then launches runTurn. It
	// returns the new TurnID; when capability-backed opening-event preparation fails,
	// it returns a non-nil error and starts nothing. When the opening event committed
	// but its derived activity transition failed, it installs the matching live start
	// and immediately terminates it as failed without entering inference. The actor is
	// the sole caller, so it always runs with state.status idle.
	startTurnWithIDAndAdmission := func(turnID uuid.UUID, qi queuedInput, firstAdmission func(), capability TurnStartCapability) (uuid.UUID, error) {
		firstLease := newAdmissionLease(firstAdmission)
		launched := false
		defer func() {
			if !launched {
				firstLease.Release()
			}
		}()
		if firstLease != nil {
			firstAdmission = firstLease.Release
		}
		started := event.TurnStarted{
			Header: event.Header{
				Coordinates: identity.Coordinates{SessionID: state.sessionID, LoopID: state.id, TurnID: turnID},
				Cause: identity.Cause{
					CommandID:   qi.inputID,
					Coordinates: identity.Coordinates{LoopID: qi.triggeredBy},
					Agency:      qi.agency,
				},
			},
			TurnIndex: state.turnIndex + 1,
			Message:   cloneUserMessage(qi.msg),
		}
		turnCall := hook.Call{
			Operation:   hook.OperationTurn,
			StartedAt:   hookNow(config.now),
			Coordinates: started.Coordinates,
			AgentName:   config.AgentName,
			Cause:       started.Cause,
			Turn: &hook.TurnData{
				Index: started.TurnIndex,
				Input: started.Message,
			},
		}
		turnHookCtx, turnHookFinish, hookErr := config.Hooks.Start(ctx, turnCall)
		if hookErr != nil {
			safeErr := safeHookError(hook.OperationTurn, hookErr)
			return uuid.UUID{}, &turnStartHookFailure{
				err: safeErr,
				complete: func() {
					finishHook(turnHookFinish, turnCall, hookOutcome(ctx, hookErr), hookErr)
				},
			}
		}
		stamped, err := stamp(started)
		if err != nil {
			finishHook(turnHookFinish, turnCall, hook.OutcomeFailed, err)
			return uuid.UUID{}, err
		}
		started = stamped.(event.TurnStarted)
		mutation, err := preflightContextMutation(state.contextTracker, state.contextGeneration, started.EventID, contextMutationHistory)
		if err != nil {
			finishHook(turnHookFinish, turnCall, hook.OutcomeFailed, err)
			return uuid.UUID{}, err
		}
		committed, publishErr := publishTurnStarted(turnHookCtx, started, capability)
		if !committed {
			finishHook(turnHookFinish, turnCall, hookOutcome(turnHookCtx, publishErr), publishErr)
			return uuid.UUID{}, publishErr
		}
		mutation.commit(&state.contextTracker, &state.contextGeneration)
		if idleCompaction != nil {
			preparation := *idleCompaction
			preparation.cancel()
			idleCompaction = nil
			finalizeIdleCompactionRejection(preparation, event.CompactRejectStaleBasis)
		}
		turnCtx, base, baseDerivedPrefix := installActiveTurn(turnHookCtx, turnID, qi)
		state.turnHookCall = turnCall
		state.turnHookFinish = turnHookFinish
		state.turnHookCtx = turnHookCtx
		idx := state.turnIndex
		cancel := state.cancelTurn
		if publishErr != nil {
			go func() {
				defer cancel()
				defer firstLease.Release()
				internal <- turnResult{terminal: event.TurnFailed{TurnIndex: idx, Err: publishErr}}
			}()
			launched = true
			return turnID, nil
		}
		ts := newTurnState(state.sessionID, state.id, turnID, idx, state.causationID, qi.msg)
		turnCfg := buildTurnConfig(base, baseDerivedPrefix, firstAdmission)
		turnCfg.cause = started.Cause

		go func() {
			defer cancel()
			// Own the transferred first-step permit before any panic-capable turn work.
			// runStep releases it at inference completion; this is the panic/early-return
			// backstop, and admissionLease makes the two paths exactly-once.
			defer firstLease.Release()
			defer func() {
				if r := recover(); r != nil {
					slog.Error("turn goroutine panicked", "panic", r)
					internal <- turnResult{
						terminal: event.TurnFailed{TurnIndex: idx, Err: &event.TurnPanicError{Detail: fmt.Sprintf("%v", r)}},
					}
				}
			}()
			terminal := runTurn(turnCtx, turnCfg, ts)
			internal <- turnResult{terminal: terminal}
		}()
		launched = true
		return turnID, nil
	}
	startTurnWithID := func(turnID uuid.UUID, qi queuedInput) (uuid.UUID, error) {
		return startTurnWithIDAndAdmission(turnID, qi, nil, nil)
	}
	startTurn := func(qi queuedInput) (uuid.UUID, error) {
		turnID, err := config.idGen()
		if err != nil {
			return uuid.UUID{}, &IDGenerationError{Cause: err}
		}
		return startTurnWithID(turnID, qi)
	}

	// userMessageFromBlocks wraps an owned clone of submit blocks into the committed
	// UserMessage form. Command callers retain ownership of their input graph.
	userMessageFromBlocks := func(blocks []content.Block) *content.UserMessage {
		return &content.UserMessage{Message: content.Message{Role: content.RoleUser, Blocks: cloneBlocks(blocks)}}
	}

	// returnEntry resolves ONE removed-from-inbox entry as returned: it emits the
	// single event.InputCancelled{reason} that a client observes for qi. It is the
	// ONE place a return is emitted, so the "every removal is resolved exactly once"
	// invariant has a single owner. turnID is the turn whose end caused the return
	// (zero for a pure retract outside a turn); it lands on Header.TurnID. decideSubmit
	// also uses it for the SubagentResult idle id-gen-failure path: a SubagentResult is
	// never rejected, so a failure to start it surfaces as InputCancelled (which
	// releases its {wake} token on the publish path) rather than TurnRejected (which
	// does not).
	returnEntry := func(qi queuedInput, reason event.CancelReason, turnID uuid.UUID) {
		publish(event.InputCancelled{
			Header: event.Header{
				Coordinates: identity.Coordinates{
					SessionID: state.sessionID,
					LoopID:    state.id,
					TurnID:    turnID,
				},
				Cause: identity.Cause{
					CommandID:   qi.inputID,
					Coordinates: identity.Coordinates{LoopID: qi.triggeredBy},
					Agency:      qi.agency,
				},
			},
			Reason:  reason,
			Message: cloneUserMessage(qi.msg),
		})
	}

	// rejectSubmit resolves a refused submit through the EVENT path (the typed
	// replacement for the old command.Disposition reply). It publishes the Enduring
	// event.TurnRejected to the session fan-in (header-stamped by stampLoopHeader,
	// Cause.CommandID == InputID), so any issuer recognises its answer via
	// ReplyTo() == its command id. The published TurnRejected is the whole answer:
	// every submit observes its outcome on the session fan-in. Because a managed
	// delegate intent is already durable before admission, this uses the checked
	// publication path: returning a queue-full acknowledgement without its durable
	// rejection would make restore replay an intent the caller was told was refused.
	rejectSubmit := func(qi queuedInput, reason event.RejectReason) error {
		stamped, err := stamp(event.TurnRejected{
			Header: event.Header{
				Cause: identity.Cause{
					CommandID:   qi.inputID,
					Coordinates: identity.Coordinates{LoopID: qi.triggeredBy},
				},
			},
			Reason: reason,
		})
		if err != nil {
			return err
		}
		return cfg.events.PublishEventChecked(ctx, stamped)
	}

	// decideSubmit resolves a UserInput/SubagentResult against the actor's OWN live
	// state (race-free), PUBLISHING the typed outcome event rather than replying a
	// command.Disposition. Every submit may queue behind a running turn: a busy loop
	// accepts the input into the inbox (it later folds or starts a later turn) rather
	// than rejecting it. bypassReject is true for a SubagentResult: it can NEVER be
	// rejected (not by cap or shutdown) — it must always start (idle) or queue
	// (running/shutting-down), so its quiescence {wake} token is ALWAYS released by a
	// resulting Enduring event (TurnStarted / TurnFoldedInto, or InputCancelled if the
	// loop ends before it commits — the shutdown terminal's returnQueuedInbox emits it
	// carrying Cause.LoopID), never off the publish path. A crypto/rand failure means
	// the actor cannot mint the TurnID — a transient system fault — so the loop
	// declines the work (fail-secure): it publishes event.TurnRejected{RejectInternal}
	// (the loop is healthy and the caller MAY retry — distinct from RejectShuttingDown,
	// which says the loop is going away).
	decideSubmit := func(qi queuedInput, bypassReject, durableAcceptance bool) {
		if probe, ok := cfg.events.(admissionFaultProbe); ok {
			if err := probe.AdmissionFaultErr(); err != nil {
				if bypassReject {
					returnEntry(qi, event.CancelTurnFailed, uuid.UUID{})
				} else {
					if err := rejectSubmit(qi, event.RejectInternal); err != nil {
						slog.Error("loop rejection publication failed", "error", err)
					}
				}
				return
			}
		}
		if durableAcceptance {
			// SubagentResult has no transient Accepted channel. Persist its
			// actor-side admission before placing it in the ephemeral inbox so a
			// restore can distinguish an admitted hand-back from a command that
			// was only appended by the sender. The restored actor still replays
			// the original command when no durable turn/fold proves processing.
			if err := publishAcceptance(qi.inputID); err != nil {
				returnEntry(qi, event.CancelTurnFailed, uuid.UUID{})
				return
			}
		}
		switch {
		case state.status == loopShuttingDown && !bypassReject:
			if err := rejectSubmit(qi, event.RejectShuttingDown); err != nil {
				slog.Error("loop rejection publication failed", "error", err)
			}
		case len(state.inbox) >= loop.ManagedInputQueueCapacity && !bypassReject:
			if err := rejectSubmit(qi, event.RejectQueueFull); err != nil {
				slog.Error("loop rejection publication failed", "error", err)
			}
		case state.status == loopRunning || state.status == loopWaitingAdmission || compactions.blocksInput() || (state.status == loopShuttingDown && bypassReject):
			// Busy (or a never-rejected SubagentResult while shutting down): accept into
			// the inbox (ordered) and publish InputQueued (Ephemeral). The submit resolves
			// on the fan-in. A SubagentResult queued during shutdown is later returned via
			// InputCancelled by the shutdown terminal's returnQueuedInbox (releasing its
			// {wake} token).
			state.inbox = append(state.inbox, qi)
			publish(event.InputQueued{
				Header: event.Header{
					Cause: identity.Cause{
						CommandID:   qi.inputID,
						Coordinates: identity.Coordinates{LoopID: qi.triggeredBy},
					},
				},
			})
		default: // idle: reserve session execution admission before TurnStarted.
			qi.rejectOnStartFailure = !bypassReject
			state.inbox = append(state.inbox, qi)
			requestStartAdmission()
		}
	}

	// admitDelegate performs every fallible rejection/mint/durable-acceptance check
	// before mutating actor-owned queue/turn state. Once acceptance commits, queueing
	// or installing the pre-minted turn is infallible and ordered after the event.
	admitDelegate := func(c command.UserInput, qi queuedInput) {
		if err := command.ValidateCommand(c); err != nil {
			c.Accepted <- err
			return
		}
		if probe, ok := cfg.events.(admissionFaultProbe); ok {
			if err := probe.AdmissionFaultErr(); err != nil {
				c.Accepted <- err
				return
			}
		}
		reject := func(reason event.RejectReason, cause error) {
			if err := rejectSubmit(qi, reason); err != nil {
				c.Accepted <- err
				return
			}
			c.Accepted <- &loop.InputRejectedError{Reason: reason, Cause: cause}
		}
		switch {
		case state.status == loopShuttingDown:
			reject(event.RejectShuttingDown, nil)
		case len(state.inbox) >= loop.ManagedInputQueueCapacity:
			reject(event.RejectQueueFull, nil)
		case state.status == loopRunning || state.status == loopWaitingAdmission || compactions.blocksInput():
			if err := publishAcceptance(c.CommandID); err != nil {
				c.Accepted <- err
				return
			}
			state.inbox = append(state.inbox, qi)
			publish(event.InputQueued{Header: event.Header{Cause: identity.Cause{CommandID: qi.inputID}}})
			c.Accepted <- nil
		default:
			turnID, err := config.idGen()
			if err != nil {
				idErr := &IDGenerationError{Cause: err}
				reject(event.RejectInternal, idErr)
				return
			}
			if err := publishAcceptance(c.CommandID); err != nil {
				c.Accepted <- err
				return
			}
			qi.reservedTurnID = turnID
			state.inbox = append(state.inbox, qi)
			requestStartAdmission()
			c.Accepted <- nil
		}
	}

	// returnQueuedInbox returns every still-unresolved queued entry via returnEntry
	// after an abnormal terminal (TurnFailed/TurnInterrupted). It covers BOTH the inbox
	// (entries never drained) AND the draining buffer (entries popped for a fold whose
	// TurnFoldedInto never committed because the turn ended first) — without the
	// draining sweep those popped entries would be silently stranded (no
	// TurnFoldedInto, no InputCancelled). The actor does NOT auto-start a new turn from
	// a returned entry — the client decides whether to resend. endedTurnID is the turn
	// that ended (the cause of the return). The draining entries are returned BEFORE
	// the inbox entries, preserving their original receive order (drained entries were
	// queued earliest).
	returnQueuedInbox := func(reason event.CancelReason, endedTurnID uuid.UUID) {
		for _, qi := range state.draining {
			returnEntry(qi, reason, endedTurnID)
		}
		state.draining = nil
		for _, qi := range state.inbox {
			returnEntry(qi, reason, endedTurnID)
		}
		state.inbox = nil
	}

	// retainUserQueuedInbox is the ordinary-interrupt disposition: human input
	// remains accepted in FIFO order, while machine-created continuation/delegate
	// entries are explicitly canceled. Draining entries precede inbox entries because
	// they were accepted earlier. Shutdown, hard kill, failure, and explicit targeted
	// cancellation continue to use their existing full disposition paths.
	retainUserQueuedInbox := func(endedTurnID uuid.UUID) {
		retained := make([]queuedInput, 0, len(state.draining)+len(state.inbox))
		partition := func(entries []queuedInput) {
			for _, qi := range entries {
				if qi.agency == identity.AgencyUser {
					retained = append(retained, qi)
					continue
				}
				returnEntry(qi, event.CancelTurnInterrupted, endedTurnID)
			}
		}
		partition(state.draining)
		partition(state.inbox)
		state.draining = nil
		state.inbox = retained
	}

	// popFront removes and returns the first queued entry, the single place the
	// inbox-front splice lives. The bool is false when the inbox is empty.
	//
	// Inbox-exit invariant: every entry that popFront (or any other path) REMOVES
	// from state.inbox MUST be resolved exactly once — either it reaches a successful
	// startTurn (it becomes a turn) or it reaches returnEntry (it is returned via
	// event.InputCancelled). A removed entry that reaches neither is silently
	// stranded; do not add a removal path that can skip both.
	popFront := func() (queuedInput, bool) {
		if len(state.inbox) == 0 {
			return queuedInput{}, false
		}
		qi := state.inbox[0]
		state.inbox = state.inbox[1:]
		return qi, true
	}

	requestStartAdmission = func() bool {
		if len(state.inbox) == 0 || state.status == loopShuttingDown || state.status == loopWaitingAdmission || compactions.blocksInput() {
			return false
		}
		admission, ok := cfg.events.(executionAdmission)
		if !ok {
			next, _ := popFront()
			if _, err := startTurn(next); err != nil {
				if next.rejectOnStartFailure {
					if publishErr := rejectSubmit(next, event.RejectInternal); publishErr != nil {
						slog.Error("loop rejection publication failed", "error", publishErr)
					}
				} else {
					returnEntry(next, event.CancelTurnFailed, uuid.UUID{})
				}
				completeTurnStartHook(err)
				return false
			}
			return true
		}
		state.status = loopWaitingAdmission
		admitCtx, cancel := context.WithCancel(ctx)
		state.cancelAdmission = cancel
		go func() {
			var result admissionResult
			if startAdmission, supportsTurnStart := cfg.events.(turnStartAdmission); supportsTurnStart {
				result.start, result.err = startAdmission.EnterTurnStart(admitCtx, state.id)
				if result.start != nil {
					result.release = result.start.Release
				}
			} else {
				result.release, result.err = admission.EnterExecution(admitCtx, state.id)
			}
			select {
			case admissions <- result:
			case <-admitCtx.Done():
				if result.release != nil {
					result.release()
				}
			}
		}()
		return true
	}

	// drainInbox is the tool-continuation drain: it pops + clears the ENTIRE inbox in
	// order, MOVES the popped entries into state.draining (so they are still resolved
	// if the turn ends abnormally before their TurnFoldedInto commits), and returns the
	// popped entries for runTurn to fold. It is the single place the inbox→draining
	// move lives. Each moved entry leaves draining only via the commit point (its
	// TurnFoldedInto resolves it) or via returnQueuedInbox (an abnormal terminal), so the
	// inbox-exit invariant — every removed entry is resolved exactly once — still holds.
	drainInbox := func() []queuedInput {
		if compactions.blocksInput() {
			return nil
		}
		// Fold only the LEADING run of foldable entries; stop at the first non-folding
		// entry (a delegate follow-up). A noFold entry — and everything queued behind it
		// — must start its OWN distinct turn rather than fold into the running turn, so it
		// stays in the inbox (FIFO preserved) and is popped as a separate turn when the
		// current one finishes. For the interactive path (every entry foldable) this drains
		// the whole inbox exactly as before; for the delegate path (every entry noFold) it
		// drains nothing, so each send becomes its own request-correlated turn.
		foldable := 0
		for foldable < len(state.inbox) && !state.inbox[foldable].noFold {
			foldable++
		}
		if foldable == 0 {
			return nil
		}
		// The reply gets its OWN backing array (the leading entries are about to move into
		// draining): copy out before moving them.
		batch := make([]queuedInput, foldable)
		copy(batch, state.inbox[:foldable])
		state.draining = append(state.draining, state.inbox[:foldable]...)
		state.inbox = append([]queuedInput(nil), state.inbox[foldable:]...)
		return batch
	}

	// removeDraining drops the entry with inputID from state.draining at its
	// TurnFoldedInto commit point (it is now resolved by that event). It is a no-op if
	// the id is absent (defensive — a fold is committed exactly once).
	removeDraining := func(inputID uuid.UUID) {
		for i, qi := range state.draining {
			if qi.inputID == inputID {
				state.draining = append(state.draining[:i], state.draining[i+1:]...)
				return
			}
		}
	}

	// cancelQueued resolves a fire-and-forget CancelQueuedInput against the
	// actor-owned inbox. If the InputID is still queued it is removed and resolved
	// via returnEntry — the Enduring event.InputCancelled{CancelClientRetracted}
	// (Header.TurnID zero — a pure retract outside a turn) IS the observable
	// outcome. If not found, the input has already started or folded (or was never
	// queued), so the retract is a no-op: the issuer infers "already committed /
	// unknown" from the event.TurnStarted / event.TurnFoldedInto it already saw for
	// that InputID. There is no reply channel.
	cancelQueued := func(c command.CancelQueuedInput) {
		for i, qi := range state.inbox {
			if qi.inputID != c.TargetCommandID {
				continue
			}
			state.inbox = append(state.inbox[:i], state.inbox[i+1:]...)
			// Removed from the inbox by a retract: resolve it via returnEntry (the one
			// return-emit point). TurnID is zero — a pure retract outside any turn.
			returnEntry(qi, event.CancelClientRetracted, uuid.UUID{})
			if state.status == loopWaitingAdmission && len(state.inbox) == 0 && state.cancelAdmission != nil {
				state.cancelAdmission()
				state.cancelAdmission = nil
				state.status = loopIdle
				emitLoopIdle()
			}
			return
		}
		// Not queued: already started/folded or never queued. No-op — the outcome is
		// observable via the prior TurnStarted/TurnFoldedInto for this InputID.
	}

	// handleProcessNotification is the actor's Task 24C delivery point for a
	// metadata-only process completion notification. It never folds into a
	// turn or touches state.inbox (see processNotifications' doc). Every path
	// replies exactly once on c.Result (nil-safe: a restore-reconstructed
	// redelivery never reaches this method at all, and a decoded wire record
	// always carries a nil Result).
	handleProcessNotification := func(c command.ProcessNotification) {
		if err := command.ValidateCommand(c); err != nil {
			slog.Warn("invalid ProcessNotification command", "error", err)
			replyProcessNotification(c.Result, command.ProcessNotificationStopped)
			return
		}
		if c.Notification.SessionID != state.sessionID || c.Notification.LoopID != state.id {
			// Defense in depth: the session already validates these coordinates
			// against the live CommandRecord route before append, so this should
			// never fire in production — but a misrouted delivery must never be
			// silently accepted onto the wrong loop's guard.
			slog.Warn("process notification addressed to a different session/loop",
				"loop", state.id, "notification_loop", c.Notification.LoopID)
			replyProcessNotification(c.Result, command.ProcessNotificationStopped)
			return
		}
		if _, duplicate := state.processNotifications[c.Notification.CommandID]; duplicate {
			// At-least-once redelivery: the causing dispatch already landed (or a
			// retry raced it here). Metadata-only, so acting on it twice would cost
			// only a redundant downstream read — but the guard drops it outright so
			// the live set never double-counts one CommandID.
			replyProcessNotification(c.Result, command.ProcessNotificationDuplicate)
			return
		}
		if len(state.processNotifications) >= loop.ManagedInputQueueCapacity {
			// Full: nothing is lost — the durable command remains authoritative and
			// the supervisor retries dispatch later with the SAME CommandID.
			replyProcessNotification(c.Result, command.ProcessNotificationStopped)
			return
		}
		if state.processNotifications == nil {
			state.processNotifications = make(map[uuid.UUID]tool.ProcessCompletionNotification)
		}
		state.processNotifications[c.Notification.CommandID] = c.Notification
		replyProcessNotification(c.Result, command.ProcessNotificationAccepted)
	}

	cancelDelegateRequest := func(c command.CancelDelegateRequest) {
		if err := c.Validate(); err != nil {
			slog.Warn("invalid CancelDelegateRequest command", "error", err)
			return
		}
		if err := command.ValidateCommand(c); err != nil {
			c.Ack <- command.DelegateCancelNoop
			return
		}
		for i, qi := range state.inbox {
			if qi.inputID != c.TargetCommandID {
				continue
			}
			state.inbox = append(state.inbox[:i], state.inbox[i+1:]...)
			returnEntry(qi, event.CancelClientRetracted, uuid.UUID{})
			if state.status == loopWaitingAdmission && len(state.inbox) == 0 && state.cancelAdmission != nil {
				state.cancelAdmission()
				state.cancelAdmission = nil
				state.status = loopIdle
				emitLoopIdle()
			}
			c.Ack <- command.DelegateCancelQueued
			return
		}
		if state.status == loopRunning && state.causationID == c.TargetCommandID && state.cancelTurn != nil {
			requestCancelActive = true
			state.cancelTurn()
			state.cancelTurn = nil
			c.Ack <- command.DelegateCancelActive
			return
		}
		c.Ack <- command.DelegateCancelNoop
	}

	// resolveQueueAfterTurn resolves still-queued input once a NON-shutdown turn has
	// ended, and reports whether the actor immediately chained into a new turn
	// (running -> running). On a normal terminal (TurnDone) it pops the FIRST queued
	// entry and starts a later turn (no input stranded); the rest stay queued. On an
	// TurnFailed returns every queued entry. Ordinary TurnInterrupted is handled below:
	// human entries remain queued while machine entries are canceled.
	// endedTurnID is the turn that ended (the cause of any return). chained==true means
	// the loop stayed running, so the caller must NOT emit LoopIdle between the turns.
	startNextQueued := func(endedTurnID uuid.UUID) (chained bool) {
		_ = endedTurnID
		return requestStartAdmission()
	}
	resolveQueueAfterTurn := func(result turnResult, endedTurnID uuid.UUID) (chained bool) {
		if _, normal := result.terminal.(event.TurnDone); !normal {
			returnQueuedInbox(cancelReasonFor(result.terminal), endedTurnID)
			return false
		}
		return startNextQueued(endedTurnID)
	}
	finishActiveTurnHook := func(terminal event.Event, boundaryErr error) {
		turnErr := terminalHookError(terminal)
		if boundaryErr != nil {
			turnErr = boundaryErr
		}
		finishHook(
			state.turnHookFinish,
			state.turnHookCall,
			hookOutcome(state.turnHookCtx, turnErr),
			turnErr,
		)
		state.turnHookCall = hook.Call{}
		state.turnHookFinish = nil
		state.turnHookCtx = nil
	}

	// handleTurnResult is the actor's response to a turn goroutine's terminal
	// hand-back (the `case result := <-internal` select arm, extracted so the select
	// reads as a dispatch). It runs the three distinct concerns in order — (1) flip
	// status idle and DELIVER the terminal (clearing the turn's correlation ids after,
	// since the terminal envelope still needs them); (2) drop stale gates; (3) resolve
	// the queue and announce idle — and returns true when the loop must EXIT (a
	// shutdown-induced terminal: ack the shutdown waiters and stop). loopState.msgs is
	// committed incrementally via the commit handshake, not from the turn result: a
	// failed/interrupted turn discards only the in-flight incomplete step (which never
	// committed); committed steps stay.
	handleTurnResult := func(result turnResult) (exit bool) {
		targetedCancellation := requestCancelActive
		requestCancelActive = false
		state.cancelTurn = nil
		shuttingDown := state.status == loopShuttingDown
		if !shuttingDown {
			state.status = loopIdle
		}
		endedTurnID := state.turnID
		// The terminal publish must still carry this turn's correlation IDs (stamped by
		// publish from state.turnID), so clear them only afterward.
		commitCtx := state.turnHookCtx
		if commitCtx == nil {
			commitCtx = ctx
		}
		_, boundaryErr := commitBoundary(commitCtx, result.terminal)
		if boundaryErr != nil {
			slog.Error("turn boundary commit failed", "error", boundaryErr)
		}
		finishActiveTurnHook(result.terminal, boundaryErr)
		state.turnID = uuid.UUID{}
		state.causationID = uuid.UUID{}
		// A finished turn must not leave stale gates: the parked runners have already
		// unblocked via the cancelled turn ctx, and a late control command for this dead
		// turn must not match a leftover gate.
		clearGates()
		if shuttingDown {
			// Shutting down: return any still-queued input (it will never start) and
			// stop. Reason follows the terminal: a TurnDone shutdown still returns queued
			// input as interrupted (the loop is going away).
			returnQueuedInbox(cancelReasonFor(result.terminal), endedTurnID)
			ackShutdowns(nil)
			return true
		}
		if boundaryErr != nil {
			// The completed terminal is already durable, but its required checkpoint
			// failed. Do not chain accepted input across the persistence fault: resolve
			// every queued entry exactly once and park idle for manual recovery.
			returnQueuedInbox(event.CancelTurnFailed, endedTurnID)
			emitLoopIdle()
			return false
		}
		if targetedCancellation {
			if !startNextQueued(endedTurnID) {
				emitLoopIdle()
			}
			return false
		}
		if _, interrupted := result.terminal.(event.TurnInterrupted); interrupted {
			retainUserQueuedInbox(endedTurnID)
			// Commit the native idle edge before retained work asks for admission.
			// SessionIdle can therefore resolve the interrupt sweep, after which
			// requestStartAdmission begins the retained input exactly once.
			emitLoopIdle()
			requestStartAdmission()
			return false
		}
		// Running -> idle transition: announce LoopIdle (Enduring, non-terminal) AFTER
		// the terminal so the session quiescence model removes this loop's {loop, LoopID}
		// activity key. A chained turn stayed running, so it emits no LoopIdle.
		if !resolveQueueAfterTurn(result, endedTurnID) {
			emitLoopIdle()
		}
		return false
	}

	// changeHeader is the loop-scoped Header a change event carries: SessionID + LoopID
	// only (no TurnID — a change is loop-scoped and takes effect at the next boundary).
	changeHeader := func() event.Header {
		return event.Header{Coordinates: identity.Coordinates{SessionID: state.sessionID, LoopID: state.id}}
	}
	commitContextConfigurationChange := func(change event.Event) error {
		stamped, err := stamp(change)
		if err != nil {
			return err
		}
		mutation, err := preflightContextMutation(state.contextTracker, state.contextGeneration, stamped.EventHeader().EventID, contextMutationRequestShape)
		if err != nil {
			return err
		}
		if err := cfg.events.PublishEventChecked(ctx, stamped); err != nil {
			return err
		}
		mutation.commit(&state.contextTracker, &state.contextGeneration)
		state.context = event.ContextMeasurement{}
		state.hasContext = false
		return nil
	}

	// applySetMode commits a SetLoopMode: it validates the mode name against the bound
	// definition, emits LoopModeChanged, checks the durable-fault probe, then replaces the
	// effective config — so the change is atomic and takes effect only at the next turn. A
	// shutting-down loop, an unknown mode, or a durable-append fault refuses the change with
	// a typed *loop.ChangeError and applies nothing. state.effective is unchanged on every
	// refusal (no partial apply). The reply carries the committed mode/model/effort.
	applySetMode := func(c command.SetLoopMode) {
		if state.status == loopShuttingDown {
			c.Ack <- command.LoopChangeResult{Err: &loop.ChangeError{Kind: loop.ChangeLoopShuttingDown}}
			return
		}
		modeName := loop.ModeName(c.Mode)
		// Resolve by EXACT name (configForMode, not configFromBound): a runtime SetMode("")
		// selects the reachable base mode, NOT the initial mode — so the committed label, the
		// emitted LoopModeChanged.Mode, and the applied effective config all agree.
		resolved, err := configForMode(cfg.bound, modeName)
		if err != nil {
			c.Ack <- command.LoopChangeResult{Err: &loop.ChangeError{Kind: loop.ChangeInvalidMode, Mode: modeName, Cause: err}}
			return
		}
		// Recompose the external slot onto the NEW mode's declared tools. Without this a
		// mode change would silently uninstall every externally installed tool, because
		// resolved.Tools carries only the definition's declared registry.
		nextTools := resolveToolSetCaps(resolved.Tools)
		nextTools.Registry = composeRegistry(resolved.Tools.Registry, state.external)
		// Re-resolve the effective capability for the NEW mode's model transport.
		// resolved.InferenceCapability (from configForMode/resolveMode) is deliberately
		// NOT used here: resolveMode only ever reads the definition's BASE transport
		// capability (bound.InferenceCapability()), which is wrong for a mode whose
		// model sits on a different declared ContextTransport. ContextTransportCapability
		// looks up the capability declared for resolved.Model's OWN transport identity.
		// cfg.bound is nil only on the raw-config test path, which never predeclares
		// modes (configForMode already refused above in that case), so this guard is
		// defensive; a missing/undeclared transport leaves the capability unchanged
		// rather than silently zeroing it.
		nextCapability := state.effective.inferenceCapability
		if cfg.bound != nil {
			if capability, ok := cfg.bound.ContextTransportCapability(resolved.Model); ok {
				nextCapability = capability
			}
		}
		// This is a SEPARATE effectiveConfig literal from newLoopWithSeed's — see the
		// cross-reference comment there. Adding a field to effectiveConfig means setting
		// it here too, or a mode change silently resets it to zero.
		next := effectiveConfig{mode: modeName, model: resolved.Model, effort: resolved.Model.Sampling.Effort, system: resolved.System, tools: nextTools, inferenceCapability: nextCapability}
		if err := commitContextConfigurationChange(event.LoopModeChanged{Header: changeHeader(), PreviousMode: string(state.effective.mode), Mode: string(modeName), Runtime: modelRuntime(next.model, next.effort)}); err != nil {
			c.Ack <- command.LoopChangeResult{Err: &loop.ChangeError{Kind: loop.ChangeDurableAppendFailed, Cause: err}}
			return
		}
		state.effective = next
		state.runtime = modelRuntime(next.model, next.effort)
		if config.afterEffectiveConfigChange != nil {
			config.afterEffectiveConfigChange(state.effective)
		}
		c.Ack <- command.LoopChangeResult{Mode: string(next.mode), Model: next.model, Effort: next.effort}
	}

	// applyReplaceExternalTools commits a ReplaceLoopExternalTools: it checks the incoming
	// toolset for a name collision against ANOTHER source's installed tools, emits
	// LoopExternalToolsetChanged, checks the durable-fault probe, then swaps the source's
	// slot and recomposes the effective registry — so the change is atomic and takes effect
	// only at the next turn (the running turn already snapshotted its own tools). A
	// shutting-down loop, a collision, or a durable-append fault refuses with a typed
	// *loop.ChangeError and applies nothing: state.external and state.effective are
	// untouched on every refusal, so the PRIOR generation stays installed.
	//
	// Collisions against the loop's DECLARED tools are refused earlier, by the session
	// before the command is sent — that check needs each tool's Info(), which may perform
	// I/O and must never run on the actor goroutine. This arm re-checks only what the actor
	// alone knows: the other sources' slots.
	applyReplaceExternalTools := func(c command.ReplaceLoopExternalTools) {
		if state.status == loopShuttingDown {
			c.Ack <- command.LoopToolsResult{Err: &loop.ChangeError{Kind: loop.ChangeLoopShuttingDown}}
			return
		}
		if name, collides := state.external.collides(c.Source, c.Identities); collides {
			c.Ack <- command.LoopToolsResult{Err: &loop.ChangeError{Kind: loop.ChangeExternalToolCollision, Tool: name}}
			return
		}
		// Re-resolve the CURRENT mode's declared tools from the bound definition rather
		// than reusing effective.tools.Registry, which already has the OLD generation
		// composed into it — composing onto that would accumulate stale tools.
		resolved, err := configForMode(cfg.bound, state.effective.mode)
		if err != nil {
			c.Ack <- command.LoopToolsResult{Err: &loop.ChangeError{Kind: loop.ChangeInvalidMode, Mode: state.effective.mode, Cause: err}}
			return
		}
		if err := commitContextConfigurationChange(event.LoopExternalToolsetChanged{
			Header: changeHeader(), Source: c.Source, Generation: c.Generation, Tools: c.Identities,
		}); err != nil {
			c.Ack <- command.LoopToolsResult{Err: &loop.ChangeError{Kind: loop.ChangeDurableAppendFailed, Cause: err}}
			return
		}
		next := state.external.clone()
		if len(c.Tools) == 0 {
			delete(next, c.Source)
		} else {
			next[c.Source] = externalGeneration{
				generation: c.Generation,
				tools:      append([]tool.InvokableTool(nil), c.Tools...),
				identities: append([]event.ExternalToolIdentity(nil), c.Identities...),
			}
		}
		state.external = next
		state.effective.tools.Registry = composeRegistry(resolved.Tools.Registry, state.external)
		c.Ack <- command.LoopToolsResult{Generation: c.Generation, Installed: len(c.Tools)}
	}

	// applyChangeInference commits a ChangeLoopInference: it folds the requested model/effort
	// over the current effective values, validates the WHOLE batch before touching anything,
	// emits LoopInferenceChanged, checks the durable-fault probe, then replaces effective
	// model+effort (never the mode/tools/system). Any refusal — shutting down, no changes,
	// invalid model/effort, or a durable-append fault — returns a typed *loop.ChangeError and
	// applies nothing.
	applyChangeInference := func(c command.ChangeLoopInference) {
		if state.status == loopShuttingDown {
			c.Ack <- command.LoopChangeResult{Err: &loop.ChangeError{Kind: loop.ChangeLoopShuttingDown}}
			return
		}
		if !c.SetModel && !c.SetEffort {
			c.Ack <- command.LoopChangeResult{Err: &loop.ChangeError{Kind: loop.ChangeNoChanges}}
			return
		}
		model := state.effective.model
		effort := state.effective.effort
		if c.SetModel {
			model = c.Model
			if verr := model.Validate(); verr != nil {
				c.Ack <- command.LoopChangeResult{Err: &loop.ChangeError{Kind: loop.ChangeInvalidModel, Cause: verr}}
				return
			}
			if verr := model.Key().Validate(); verr != nil {
				c.Ack <- command.LoopChangeResult{Err: &loop.ChangeError{Kind: loop.ChangeInvalidModel, Cause: verr}}
				return
			}
			if cfg.bound != nil {
				if verr := cfg.bound.ValidateContextModel(model); verr != nil {
					c.Ack <- command.LoopChangeResult{Err: &loop.ChangeError{Kind: loop.ChangeInvalidModel, Cause: verr}}
					return
				}
			}
		}
		// Re-resolve the effective capability for the NEW model's transport (see the
		// matching comment in applySetMode for why ContextTransportCapability, not
		// resolved.InferenceCapability, is the correct lookup). An effort-only change
		// (c.SetModel false) leaves the current capability untouched — the model, and
		// therefore its transport, has not changed.
		nextCapability := state.effective.inferenceCapability
		if c.SetModel && cfg.bound != nil {
			if capability, ok := cfg.bound.ContextTransportCapability(model); ok {
				nextCapability = capability
			}
		}
		if c.SetEffort {
			effort = c.Effort
			if !effort.Valid() {
				c.Ack <- command.LoopChangeResult{Err: &loop.ChangeError{Kind: loop.ChangeInvalidEffort}}
				return
			}
		}
		model.Sampling = model.Sampling.Clone()
		model.Sampling.Effort = effort // bake effort into the model the request stamps
		if err := commitContextConfigurationChange(event.LoopInferenceChanged{Header: changeHeader(), Runtime: modelRuntime(model, effort)}); err != nil {
			c.Ack <- command.LoopChangeResult{Err: &loop.ChangeError{Kind: loop.ChangeDurableAppendFailed, Cause: err}}
			return
		}
		state.effective.model = model
		state.effective.effort = effort
		state.effective.inferenceCapability = nextCapability
		state.runtime = modelRuntime(model, effort)
		if config.afterEffectiveConfigChange != nil {
			config.afterEffectiveConfigChange(state.effective)
		}
		c.Ack <- command.LoopChangeResult{Mode: string(state.effective.mode), Model: model, Effort: effort}
	}

	handleCommand := func(cmd command.Command) bool {
		switch c := cmd.(type) {

		case command.UserInput:
			// Interactive input may queue behind a running turn (it later folds into a
			// tool-continuation request or starts a later turn). The actor decides on
			// its own live state — race-free — and PUBLISHES the typed outcome event
			// (TurnStarted / InputQueued / TurnRejected) onto the session fan-in. A
			// UserInput may be rejected, so bypassReject is false.
			qi := queuedInput{inputID: c.CommandHeader().CommandID, agency: c.CommandHeader().Agency, msg: userMessageFromBlocks(c.Blocks), noFold: c.NoFold}
			if c.Accepted != nil {
				admitDelegate(c, qi)
				return false
			}
			decideSubmit(qi, false, false)

		case command.SubagentResult:
			// A hand-back from a finished subagent loop. triggeredBy is the producing
			// CHILD loop id (Cause.LoopID), stamped on the resulting events — the
			// command's embedded Coordinates.LoopID is the PARENT (this loop, the
			// delivery target), NOT the wake token. bypassReject is true: a
			// SubagentResult is NEVER rejected — it always starts (idle) or queues
			// (running/shutting-down), so its quiescence {wake} token is always released
			// by a resulting Enduring event, never off the publish path.
			qi := queuedInput{
				inputID:     c.CommandHeader().CommandID,
				triggeredBy: c.Cause.LoopID,           // the CHILD loop (wake token)
				agency:      c.CommandHeader().Agency, // a hand-back is machine; copy verbatim
				msg:         userMessageFromBlocks(c.Blocks),
			}
			decideSubmit(qi, true, true)

		case command.CancelQueuedInput:
			// Retract a still-queued submit. Resolved by the actor against its own
			// inbox: if still queued it emits event.InputCancelled{CancelClientRetracted}
			// and removes it; otherwise it is a no-op (already started/folded or never
			// queued). Fire-and-forget — no reply channel.
			cancelQueued(c)

		case command.CancelDelegateRequest:
			cancelDelegateRequest(c)

		case command.ProcessNotification:
			handleProcessNotification(c)

		case command.SetLoopMode:
			// Select a predeclared mode for the NEXT turn. Validated against the bound
			// definition on the actor (the sole owner of effective state); the outcome
			// (typed error or the committed mode/model/effort) is replied on the buffered
			// Ack. A nil Ack violates the contract — log and drop rather than wedge.
			if err := c.Validate(); err != nil {
				slog.Warn("invalid SetLoopMode command", "error", err)
				return false
			}
			applySetMode(c)

		case command.ChangeLoopInference:
			// Change only the model/effort for the NEXT turn, validated atomically.
			if err := c.Validate(); err != nil {
				slog.Warn("invalid ChangeLoopInference command", "error", err)
				return false
			}
			applyChangeInference(c)

		case command.ReplaceLoopExternalTools:
			// Replace one source's external toolset for the NEXT turn. The tools arrive
			// already built (the session owns the bindings and does the I/O); the actor
			// only checks what it alone owns, journals the identity record, and swaps.
			// A malformed command violates the contract — log and drop rather than wedge.
			if err := c.Validate(); err != nil {
				slog.Warn("invalid ReplaceLoopExternalTools command", "error", err)
				return false
			}
			applyReplaceExternalTools(c)

		case command.Compact:
			admission, err := compactions.admit(c, config.idGen)
			if err != nil {
				reportCompactionFailure([]uuid.UUID{c.CommandHeader().CommandID}, err)
				return false
			}
			if admission.Kind == compactionAdmissionLaneFull {
				if err := publishLaneFull(c, admission.AttemptID); err != nil {
					reportCompactionFailure([]uuid.UUID{c.CommandHeader().CommandID}, err)
				}
				return false
			}
			// Idle is itself a safe turn boundary. A configured sink takes ownership
			// immediately; while a turn is live, the request remains actor-owned until
			// the next step/turn boundary below.
			if state.status == loopIdle {
				if admission.Kind == compactionAdmissionOpened && startIdleCompactionPreparation(admission.AttemptID) {
					return false
				}
				dispatchCompactionBoundary(ctx, compactionBoundaryTurn, nil)
			}

		case command.Interrupt:
			if err := c.Validate(); err != nil {
				slog.Warn("invalid Interrupt command", "error", err)
				return false
			}
			compactions.interrupt()
			if idleCompaction != nil {
				idleCompaction.cancel()
			}
			if state.cancelTurn != nil {
				state.cancelTurn()
				state.cancelTurn = nil
				c.Ack <- true
			} else if requestCancelActive {
				// A targeted cancellation already cancelled this exact active turn,
				// but its terminal has not reached the actor yet. An ordinary interrupt
				// broadens the disposition: acknowledge it and clear the targeted-only
				// continuation so the terminal uses the ordinary interrupt queue policy.
				requestCancelActive = false
				c.Ack <- true
			} else if state.status == loopWaitingAdmission && state.cancelAdmission != nil {
				// This loop is idle with accepted work waiting on session admission.
				retainUserQueuedInbox(uuid.UUID{})
				if len(state.inbox) == 0 {
					state.cancelAdmission()
					state.cancelAdmission = nil
					state.status = loopIdle
					emitLoopIdle()
				}
				// Ack false because no current turn was canceled. The interrupt
				// caller still owns the mark-before-fanout disposition; clearing the
				// admission here makes an accepted-but-stopped loop reusable.
				c.Ack <- false
			} else if state.status == loopIdle && (len(state.inbox) > 0 || len(state.draining) > 0) {
				// Compaction can leave accepted work in the inbox while the loop is
				// otherwise idle. StopAgent has the same queue disposition as an
				// ordinary interrupt in that window: retain human input and resolve
				// every machine continuation/hand-back as interrupted.
				retainUserQueuedInbox(uuid.UUID{})
				c.Ack <- false
			} else {
				c.Ack <- false
			}
			if state.status == loopIdle {
				dispatchCompactionBoundary(ctx, compactionBoundaryTurn, nil)
			}

		case command.Shutdown:
			if err := c.Validate(); err != nil {
				slog.Warn("invalid Shutdown command", "error", err)
			} else {
				state.shutdownAcks = append(state.shutdownAcks, c.Ack)
			}
			compactions.shutdown()
			if idleCompaction != nil {
				idleCompaction.cancel()
			}
			if state.status == loopShuttingDown {
				return false
			}
			wasRunning := state.status == loopRunning
			wasWaitingAdmission := state.status == loopWaitingAdmission
			state.status = loopShuttingDown
			if state.cancelTurn != nil {
				state.cancelTurn()
				state.cancelTurn = nil
			}
			if wasWaitingAdmission && state.cancelAdmission != nil {
				state.cancelAdmission()
				state.cancelAdmission = nil
				returnQueuedInbox(event.CancelTurnInterrupted, uuid.UUID{})
				dispatchCompactionBoundary(ctx, compactionBoundaryTurn, nil)
				ackShutdowns(nil)
				return true
			}
			if !wasRunning {
				// Idle shutdown: no turn is running. Return any still-queued input
				// (it will never start) before stopping; in practice the inbox is
				// empty when idle, but this guarantees nothing is silently dropped.
				returnQueuedInbox(event.CancelTurnInterrupted, uuid.UUID{})
				dispatchCompactionBoundary(ctx, compactionBoundaryTurn, nil)
				ackShutdowns(nil)
				return true
			}
			// Turn goroutine is winding down; wait for internal below.

			// Control commands are fire-and-route: no Validate, no Ack. routeControl
			// delivers to the parked runner blocked on this ToolExecutionID iff a gate is open
			// AND its kind accepts this command; any miss (unknown/stale ToolExecutionID, kind
			// mismatch, duplicate after delivery) is silently dropped (fail-safe).
		case command.ApproveToolCall:
			routeControl(c, c.GateRoute)

		case command.DenyToolCall:
			routeControl(c, c.GateRoute)

		case command.ProvideUserInput:
			routeControl(c, c.GateRoute)
		}
		return false
	}

	for {
		// One bounded priority poll precedes the ordinary select. The actor still
		// enters the ordinary select every iteration, so an empty priority lane has
		// zero effect and a sustained control stream cannot exclude ordinary work.
		select {
		case cmd, ok := <-priorityCommands:
			if !ok || handleCommand(cmd) {
				return
			}
		default:
		}
		select {
		case cmd, ok := <-priorityCommands:
			if !ok || handleCommand(cmd) {
				return
			}
		case cmd, ok := <-commands:
			if !ok || handleCommand(cmd) {
				return
			}
		case reg := <-gateReg:
			if !reg.abandonID.IsZero() {
				if _, open := state.pendingGates[reg.abandonID]; open {
					closeErr := cfg.gates.CloseGate(reg.ctx, reg.abandonID, gatedomain.CloseAbandoned)
					if closeErr == nil {
						delete(state.pendingGates, reg.abandonID)
					}
					reg.ack <- gateInstallAck{gateID: reg.abandonID, err: closeErr}
				} else {
					reg.ack <- gateInstallAck{gateID: reg.abandonID}
				}
				break
			}
			callID := reg.toolExecutionID()
			gateCtx := reg.ctx
			if gateCtx == nil {
				gateCtx = ctx
			}
			gateID, err := cfg.gates.PrepareGateOpen(gateCtx, state.id, reg.gate, reg.payload)
			if err != nil {
				reg.ack <- gateInstallAck{err: err}
				break
			}
			state.pendingGates[gateID] = pendingGate{reply: reg.reply, kind: reg.kind}
			route := gatedomain.Route{GateID: gateID, LoopID: state.id, ToolExecutionID: callID}
			if err := cfg.gates.ActivateGate(gateCtx, gateID, route); err != nil {
				delete(state.pendingGates, gateID)
				_ = cfg.gates.CloseGate(gateCtx, gateID, gatedomain.CloseAbandoned)
				reg.ack <- gateInstallAck{gateID: gateID, err: err}
				break
			}
			reg.ack <- gateInstallAck{gateID: gateID}

			// Automatic review begins only after GateOpened has committed and the
			// runner has been acked (design §14.2/§14.3): the human can answer
			// immediately, there is no invisible review-only wait, and activation
			// failure (handled above, before the ack) can never start a review.
			// StartPermissionReview must return promptly — the actor calls it
			// inline and moves on to the next select iteration without waiting on
			// classifier inference.
			if reg.kind == gatePermission && cfg.reviewStarter != nil {
				if req, ok := permissionRequestFromPayload(reg.payload); ok {
					cfg.reviewStarter.StartPermissionReview(ctx, PermissionReviewRequest{
						GateID:          gateID,
						ToolExecutionID: callID,
						Request:         req,
						ReviewContext:   reg.reviewContext,
					})
				}
			}

		case req := <-snapshots:
			// Committed-state query: the actor is the SOLE owner of loopState.msgs +
			// turnIndex, so a consistent read is served from here. Reply a DEFENSIVE deep
			// clone so the caller can never alias or race the live history the actor keeps
			// appending to. reply is buffered(1); the send never blocks.
			req.reply <- loopSnapshot{msgs: cloneMessages(state.msgs), turnIndex: state.turnIndex}

		case req := <-contextRequests:
			settings, configured := contextSettings(config)
			if !configured || config.ContextCounter == nil {
				req.reply <- contextMeasureReply{err: &contextConfigurationStateError{Detail: "counter or policy missing"}}
				continue
			}
			basis := state.contextTracker.currentBasis()
			generation := state.contextGeneration
			capability := state.effective.inferenceCapability
			go func(request contextMeasureRequest, admission contextAdmissionSettings, measuredBasis event.ContextBasis, measuredGeneration uint64, measuredCapability contextcount.InferenceCapability) {
				measurement, err := measureRequestContext(
					request.ctx,
					config.ContextCounter,
					config.CounterCapability,
					measuredCapability,
					admission,
					measuredBasis,
					request.request,
					request.runtimeContextRevision,
				)
				result := contextCountResult{request: request, measurement: measurement, generation: measuredGeneration, err: err}
				select {
				case contextResults <- result:
				case <-request.ctx.Done():
				case <-ctx.Done():
				}
			}(req, settings, basis, generation, capability)

		case result := <-idleCompactionResults:
			preparing := idleCompaction
			if preparing == nil || preparing.attemptID != result.preparation.attemptID ||
				preparing.basis != result.preparation.basis || preparing.generation != result.preparation.generation {
				continue
			}
			preparing.cancel()
			idleCompaction = nil
			pending := compactions.pendingAttempt()
			if pending == nil || pending.AttemptID != result.preparation.attemptID {
				// A pre-start interrupt/shutdown already resolved the transient slot.
				continue
			}
			if reason := compactions.cancellationRejectReason(); reason != event.CompactRejectUnspecified {
				dispatchCompactionBoundary(ctx, compactionBoundaryTurn, nil)
				continue
			}
			currentBasis := state.contextTracker.currentBasis()
			if currentBasis != result.preparation.basis || state.contextGeneration != result.preparation.generation {
				finalizeIdleCompactionRejection(result.preparation, event.CompactRejectStaleBasis)
				continue
			}
			if result.err != nil {
				finalizeIdleCompactionRejection(result.preparation, compactionRejectReason(result.err))
				continue
			}
			settings, configured := contextSettings(config)
			if !configured {
				finalizeIdleCompactionRejection(result.preparation, event.CompactRejectInternal)
				continue
			}
			nextTracker := state.contextTracker
			tracking, err := nextTracker.apply(result.candidate.Measurement, settings)
			if err != nil {
				finalizeIdleCompactionRejection(result.preparation, event.CompactRejectInternal)
				continue
			}
			if tracking.MeasurementChanged {
				measured, stampErr := stamp(event.ContextMeasured{Measurement: result.candidate.Measurement})
				if stampErr != nil {
					reportCompactionFailure(pending.WaiterCommandIDs, stampErr)
					compactions.abort(pending.AttemptID)
					resumeQueuedAfterCompaction()
					continue
				}
				if validateErr := event.ValidateEvent(measured); validateErr != nil {
					reportCompactionFailure(pending.WaiterCommandIDs, validateErr)
					compactions.abort(pending.AttemptID)
					resumeQueuedAfterCompaction()
					continue
				}
				if publishErr := cfg.events.PublishEventChecked(ctx, measured); publishErr != nil {
					reportCompactionFailure(pending.WaiterCommandIDs, publishErr)
					compactions.abort(pending.AttemptID)
					continue
				}
			}
			state.contextTracker = nextTracker
			state.context = result.candidate.Measurement
			state.hasContext = true
			if tracking.PressureChanged {
				publish(event.ContextPressure{
					Measurement: result.candidate.Measurement, Occupancy: tracking.Occupancy,
					Previous: tracking.Previous, Current: tracking.Current,
				})
			}
			if err := compactions.freezeBasis(pending.AttemptID, result.preparation.basis); err != nil {
				reportCompactionFailure(pending.WaiterCommandIDs, err)
				continue
			}
			if dispatchCompactionBoundary(ctx, compactionBoundaryTurn, &result.candidate) {
				awaitIdleCompaction(pending.AttemptID)
			}

		case result := <-contextResults:
			reply := func(response contextMeasureReply) {
				result.request.reply <- response
			}
			if result.err != nil {
				reply(contextMeasureReply{err: result.err})
				continue
			}
			currentBasis := state.contextTracker.currentBasis()
			if result.measurement.Basis != currentBasis || result.generation != state.contextGeneration {
				reply(contextMeasureReply{err: &staleContextMeasurementError{Measured: result.measurement.Basis, Current: currentBasis}})
				continue
			}
			settings, configured := contextSettings(config)
			if !configured {
				reply(contextMeasureReply{err: &contextConfigurationStateError{Detail: "policy removed while counting"}})
				continue
			}
			nextTracker := state.contextTracker
			tracking, err := nextTracker.apply(result.measurement, settings)
			if err != nil {
				reply(contextMeasureReply{err: err})
				continue
			}
			if tracking.MeasurementChanged {
				measured, stampErr := stamp(event.ContextMeasured{Measurement: result.measurement})
				if stampErr != nil {
					reply(contextMeasureReply{err: stampErr})
					continue
				}
				if validateErr := event.ValidateEvent(measured); validateErr != nil {
					reply(contextMeasureReply{err: validateErr})
					continue
				}
				if publishErr := cfg.events.PublishEventChecked(ctx, measured); publishErr != nil {
					reply(contextMeasureReply{err: publishErr})
					continue
				}
			}
			state.contextTracker = nextTracker
			state.context = result.measurement
			state.hasContext = true
			if tracking.PressureChanged {
				publish(event.ContextPressure{
					Measurement: result.measurement,
					Occupancy:   tracking.Occupancy,
					Previous:    tracking.Previous,
					Current:     tracking.Current,
				})
			}
			executionCandidate := compactionExecutionCandidate{
				Measurement: result.measurement, Request: result.request.request,
				RuntimeTail: result.request.runtimeTail, RuntimeRevision: result.request.runtimeContextRevision,
				Transcript: cloneMessages(state.msgs), InferenceCapability: state.effective.inferenceCapability,
			}
			if compactions.pendingAtBoundary() {
				pending := compactions.pendingAttempt()
				awaiter, canAwait := config.compactionSink.(contextCompactionAwaiter)
				if pending == nil || !canAwait {
					reply(contextMeasureReply{measurement: result.measurement, err: tracking.AdmissionError})
					continue
				}
				if pending.Basis == (event.ContextBasis{}) {
					if freezeErr := compactions.freezeBasis(pending.AttemptID, result.measurement.Basis); freezeErr != nil {
						reply(contextMeasureReply{measurement: result.measurement, err: freezeErr})
						continue
					}
				}
				coordinated := false
				// Measurement happens after Step has durably finished, so the request
				// context carries the still-live Turn scope (not a closed Step scope).
				// Preserve it across the actor seam into Compaction.
				dispatch := func() {
					coordinated = dispatchCompactionBoundary(
						result.request.ctx, compactionBoundaryStep, &executionCandidate,
					)
				}
				if config.beforeCompactionBoundary != nil {
					config.beforeCompactionBoundary(compactionBoundaryStep)
				}
				if arbitrateCompactionBoundary(priorityCommands, handleCommand, dispatch) {
					return
				}
				if coordinated {
					reply(contextMeasureReply{measurement: result.measurement, attemptID: pending.AttemptID, awaiter: awaiter})
					continue
				}
			}
			if tracking.AdmissionError != nil {
				reply(contextMeasureReply{measurement: result.measurement, err: tracking.AdmissionError})
				continue
			}
			if !tracking.TriggerAutomatic {
				reply(contextMeasureReply{measurement: result.measurement})
				continue
			}
			awaiter, canAwait := config.compactionSink.(contextCompactionAwaiter)
			if !canAwait {
				if tracking.Current == event.PressureHardLimit {
					reply(contextMeasureReply{measurement: result.measurement, err: &loop.ContextLimitError{Measurement: result.measurement}})
				} else {
					reply(contextMeasureReply{measurement: result.measurement})
				}
				continue
			}
			commandID, idErr := config.idGen()
			if idErr != nil || commandID.IsZero() {
				coordinationErr := &CompactionCoordinationError{Kind: CompactionCoordinationAttemptID, Cause: idErr}
				reportCompactionFailure(nil, coordinationErr)
				if tracking.Current == event.PressureHardLimit {
					reply(contextMeasureReply{measurement: result.measurement, err: &loop.ContextLimitError{Measurement: result.measurement}})
				} else {
					reply(contextMeasureReply{measurement: result.measurement})
				}
				continue
			}
			automatic := command.Compact{
				Header:      command.Header{CommandID: commandID, Agency: identity.AgencyMachine, CreatedAt: config.now()},
				Coordinates: identity.Coordinates{SessionID: state.sessionID, LoopID: state.id},
			}
			admission, admissionErr := compactions.admit(automatic, config.idGen)
			if admissionErr != nil {
				reportCompactionFailure([]uuid.UUID{commandID}, admissionErr)
				if tracking.Current == event.PressureHardLimit {
					reply(contextMeasureReply{measurement: result.measurement, err: &loop.ContextLimitError{Measurement: result.measurement}})
				} else {
					reply(contextMeasureReply{measurement: result.measurement})
				}
				continue
			}
			if admission.Kind == compactionAdmissionLaneFull {
				if laneErr := publishLaneFull(automatic, admission.AttemptID); laneErr != nil {
					reportCompactionFailure([]uuid.UUID{commandID}, laneErr)
				}
				if tracking.Current == event.PressureHardLimit {
					reply(contextMeasureReply{measurement: result.measurement, err: &loop.ContextLimitError{Measurement: result.measurement}})
				} else {
					reply(contextMeasureReply{measurement: result.measurement})
				}
				continue
			}
			if freezeErr := compactions.freezeBasis(admission.AttemptID, result.measurement.Basis); freezeErr != nil {
				reportCompactionFailure([]uuid.UUID{commandID}, freezeErr)
				if tracking.Current == event.PressureHardLimit {
					reply(contextMeasureReply{measurement: result.measurement, err: &loop.ContextLimitError{Measurement: result.measurement}})
				} else {
					reply(contextMeasureReply{measurement: result.measurement})
				}
				continue
			}
			coordinated := false
			// Automatic compaction is a child of the still-live Turn after Step
			// completion; use the originating measurement context, not actor ctx.
			dispatch := func() {
				coordinated = dispatchCompactionBoundary(
					result.request.ctx, compactionBoundaryStep, &executionCandidate,
				)
			}
			if config.beforeCompactionBoundary != nil {
				config.beforeCompactionBoundary(compactionBoundaryStep)
			}
			if arbitrateCompactionBoundary(priorityCommands, handleCommand, dispatch) {
				return
			}
			if !coordinated {
				if tracking.Current == event.PressureHardLimit {
					reply(contextMeasureReply{measurement: result.measurement, err: &loop.ContextLimitError{Measurement: result.measurement}})
				} else {
					reply(contextMeasureReply{measurement: result.measurement})
				}
				continue
			}
			reply(contextMeasureReply{measurement: result.measurement, attemptID: admission.AttemptID, awaiter: awaiter})

		case outcome := <-contextOutcomes:
			attempt := compactions.pendingAttempt()
			if attempt == nil || attempt.AttemptID != outcome.attemptID {
				identityErr := &contextCompactionOutcomeError{AttemptID: outcome.attemptID}
				outcome.result.Proposal.hookScope.finishInfrastructure(identityErr)
				outcome.reply <- contextCompactionOutcomeReply{err: identityErr}
				continue
			}
			if reason := compactions.cancellationRejectReason(); reason != event.CompactRejectUnspecified {
				scope := outcome.result.Proposal.hookScope
				outcome.result = rejectedCompactionResult(reason)
				outcome.result.Proposal.hookScope = scope
				scope.setTerminal(hook.OutcomeCanceled, context.Canceled, nil)
			}
			settings, configured := contextSettings(config)
			proposal := outcome.result.Proposal
			disposition := outcome.result.Disposition
			if validationErr := validateContextCompactionProposal(attempt, outcome.result); validationErr != nil {
				proposal = compactionFinalizationProposal{
					RejectReason: event.CompactRejectInternal, hookScope: proposal.hookScope,
				}
				proposal.hookScope.setTerminal(hook.OutcomeFailed, validationErr, nil)
				disposition = contextCompactionAwaitRejected
			}
			var replacementPlan *actorContextReplacement
			if proposal.Success != nil {
				plan, replacementErr := prepareActorContextReplacement(state, *attempt, proposal.Success, settings)
				if replacementErr != nil {
					var stale *StaleCompactionError
					rejectReason := event.CompactRejectInternal
					if errors.As(replacementErr, &stale) {
						rejectReason = event.CompactRejectStaleBasis
					}
					proposal = compactionFinalizationProposal{
						RejectReason: rejectReason, hookScope: proposal.hookScope,
					}
					proposal.hookScope.setTerminal(hook.OutcomeFailed, replacementErr, nil)
					disposition = contextCompactionAwaitRejected
				} else {
					replacementPlan = &plan
				}
			}
			finalizeCtx := ctx
			if proposal.hookScope != nil && proposal.hookScope.ctx != nil {
				finalizeCtx = proposal.hookScope.ctx
			}
			terminal, finalizationErr := compactionFinalizations.Finalize(finalizeCtx, *attempt, proposal)
			if finalizationErr != nil {
				reportCompactionFailure(attempt.WaiterCommandIDs, finalizationErr)
				outcome.reply <- contextCompactionOutcomeReply{err: finalizationErr}
				continue
			}
			var turnReplacement *turnContextReplacement
			if committed, ok := terminal.(event.CompactionCommitted); ok {
				if replacementPlan == nil {
					outcome.reply <- contextCompactionOutcomeReply{err: &contextCompactionOutcomeError{AttemptID: outcome.attemptID}}
					continue
				}
				replacementPlan.apply(&state, committed)
				turnReplacement = &turnContextReplacement{Summary: cloneUserMessage(committed.Summary)}
			}
			compactions.complete(outcome.attemptID)
			rejection, rejected := terminal.(event.CompactionRejected)
			if rejected && rejection.Reason == event.CompactionReasonAutomatic {
				state.contextTracker.exhaustAutomatic(rejection.Basis, rejection.Reason, true)
			}
			retry := configured && settings.Automatic && rejected && rejection.Reason == event.CompactionReasonManual && rejection.Basis == state.contextTracker.currentBasis()
			outcome.reply <- contextCompactionOutcomeReply{
				disposition: disposition, replacement: turnReplacement,
				continuationError: outcome.result.ContinuationError, rejectReason: rejection.RejectReason, retry: retry,
			}
			resumeQueuedAfterCompaction()

		case result := <-admissions:
			state.cancelAdmission = nil
			if state.status == loopShuttingDown {
				if result.release != nil {
					result.release()
				}
				returnQueuedInbox(event.CancelTurnInterrupted, uuid.UUID{})
				ackShutdowns(nil)
				return
			}
			if state.status != loopWaitingAdmission {
				if result.release != nil {
					result.release()
				}
				continue
			}
			if result.err != nil {
				state.status = loopIdle
				returnQueuedInbox(event.CancelTurnFailed, uuid.UUID{})
				emitLoopIdle()
				continue
			}
			next, ok := popFront()
			if !ok {
				result.release()
				state.status = loopIdle
				emitLoopIdle()
				continue
			}
			turnID := next.reservedTurnID
			var err error
			if turnID.IsZero() {
				turnID, err = config.idGen()
			}
			if err != nil {
				result.release()
				state.status = loopIdle
				if next.rejectOnStartFailure {
					if publishErr := rejectSubmit(next, event.RejectInternal); publishErr != nil {
						slog.Error("loop rejection publication failed", "error", publishErr)
					}
				} else {
					returnEntry(next, event.CancelTurnFailed, uuid.UUID{})
				}
				returnQueuedInbox(event.CancelTurnFailed, uuid.UUID{})
				emitLoopIdle()
				continue
			}
			if _, err := startTurnWithIDAndAdmission(turnID, next, result.release, result.start); err != nil {
				state.status = loopIdle
				if next.rejectOnStartFailure {
					if publishErr := rejectSubmit(next, event.RejectInternal); publishErr != nil {
						slog.Error("loop rejection publication failed", "error", publishErr)
					}
				} else {
					returnEntry(next, event.CancelTurnFailed, uuid.UUID{})
				}
				completeTurnStartHook(err)
				returnQueuedInbox(event.CancelTurnFailed, uuid.UUID{})
				emitLoopIdle()
			}

		case req := <-drains:
			// Tool-continuation drain: pop + clear the inbox into draining and reply the
			// queued inputs. The actor is the sole owner of inbox/draining, and the turn
			// goroutine is parked in cfg.drainPending while this runs, so there is no
			// concurrent access. The reply is buffered(1); the send never blocks. The
			// drained entries are now in draining and are resolved either by their
			// TurnFoldedInto commit (below) or by returnQueuedInbox on an abnormal
			// terminal — never silently lost.
			dispatch := func() { dispatchCompactionBoundary(ctx, compactionBoundaryStep, nil) }
			if compactions.pendingAtBoundary() {
				if config.beforeCompactionBoundary != nil {
					config.beforeCompactionBoundary(compactionBoundaryStep)
				}
				if arbitrateCompactionBoundary(priorityCommands, handleCommand, dispatch) {
					return
				}
			} else {
				dispatch()
			}
			req.reply <- drainInbox()

		case req := <-commits:
			// Loop-owned incremental commit: the actor is the SOLE mutator of
			// loopState.msgs. It stamps and preflights the context mutation, durably
			// commits StepDone (or TurnFoldedInto), then applies the matching live
			// history/basis mutation. A later checkpoint error still reports whether
			// the trigger committed, preserving live/restore equivalence.
			// The turn goroutine is parked in cfg.commit while this runs, so the
			// StepDone emitted here always follows that step's TokenDeltas on the
			// fan-in. Ack last so the runner only resumes after the event is published.
			stampedBoundary, boundaryErr := stamp(req.commit.Event)
			var mutation contextMutation
			if boundaryErr == nil {
				mutation, boundaryErr = preflightContextMutation(state.contextTracker, state.contextGeneration, stampedBoundary.EventHeader().EventID, contextMutationHistory)
			}
			committed := false
			if boundaryErr == nil {
				commitCtx := req.ctx
				if commitCtx == nil {
					commitCtx = ctx
				}
				committed, boundaryErr = commitStampedContextBoundary(commitCtx, stampedBoundary)
			}
			if committed {
				state.msgs = append(state.msgs, req.commit.Messages...)
				mutation.commit(&state.contextTracker, &state.contextGeneration)
				state.context = event.ContextMeasurement{}
				state.hasContext = false
			}
			// A folded user message is now committed: resolve its draining entry (its
			// TurnFoldedInto was just emitted), so the abnormal-terminal return path
			// does not also return it. StepDone commits carry no inbox entry.
			if fi, ok := req.commit.Event.(event.TurnFoldedInto); ok && committed {
				removeDraining(fi.Cause.CommandID)
			}
			req.ack <- boundaryErr

		case result := <-internal:
			dispatch := func() { dispatchCompactionBoundary(ctx, compactionBoundaryTurn, nil) }
			if compactions.pendingAtBoundary() {
				if config.beforeCompactionBoundary != nil {
					config.beforeCompactionBoundary(compactionBoundaryTurn)
				}
				if arbitrateCompactionBoundary(priorityCommands, handleCommand, dispatch) {
					return
				}
			} else {
				dispatch()
			}
			if handleTurnResult(result) {
				return
			}

		case <-ctx.Done():
			if state.cancelAdmission != nil {
				state.cancelAdmission()
				state.cancelAdmission = nil
			}
			if state.cancelTurn != nil {
				state.cancelTurn()
				state.cancelTurn = nil
			}
			if state.status == loopRunning || state.status == loopShuttingDown {
				// Hard loop kill. Wait for the cancelled turn goroutine to drain
				// and publish its terminal, but bound the wait: a provider that
				// ignores ctx must not hold the actor (and Loop.Done) hostage. The
				// timeout detaches a goroutine still blocked in the provider — it is
				// wedged there and would never have produced a terminal anyway.
				select {
				case result := <-internal:
					commitCtx := state.turnHookCtx
					if commitCtx == nil {
						commitCtx = ctx
					}
					_, boundaryErr := commitBoundary(commitCtx, result.terminal)
					if boundaryErr != nil {
						slog.Error("turn boundary commit failed during loop cancellation", "error", boundaryErr)
					}
					finishActiveTurnHook(result.terminal, boundaryErr)
				case <-time.After(config.DrainTimeout):
					slog.Error("turn goroutine did not drain after ctx cancel; detaching",
						"timeout", config.DrainTimeout)
					finishActiveTurnHook(nil, ctx.Err())
				}
			}
			// Defensive: the loop is exiting, but drop any gates so no detached path
			// could ever match a stale entry. Parked runners already unblock via the
			// cancelled turn ctx.
			clearGates()
			// Return any still-queued input so a hard kill never silently drops it.
			// best-effort: the loop ctx is already cancelled, so publish is the only
			// observable channel.
			returnQueuedInbox(event.CancelTurnInterrupted, state.turnID)
			ackShutdowns(&command.LoopTerminatedError{Cause: ctx.Err()})
			return
		}
	}
}
