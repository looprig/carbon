package acp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/looprig/acp/client"
	"github.com/looprig/acp/protocol"
	"github.com/looprig/core/content"
	"github.com/looprig/foreignloops/driver"
)

// turnSession is the live-session portion used after construction. Keeping it
// separate from session lets construction tests use a narrow setup seam while
// the concrete ACP client supplies prompt and update delivery for turns.
type turnSession interface {
	session
	Prompt(context.Context, []protocol.ContentBlock) (*client.PromptResult, error)
	Updates() <-chan client.Update
	Cancel(context.Context) error
	WaitForUpdates(context.Context) error
}

type orderedUpdateBarrier interface {
	WaitForUpdatesThrough(context.Context, uint64) error
}

type promptSession interface {
	session
	Prompt(context.Context, []protocol.ContentBlock) (*client.PromptResult, error)
	Updates() <-chan client.Update
	Cancel(context.Context) error
}

type legacyTurnSession struct{ promptSession }

func (legacyTurnSession) WaitForUpdates(context.Context) error { return nil }

type promptOutcome struct {
	result *client.PromptResult
	err    error
}

// turnLifecycle closes the race between a prompt completing and its caller
// context being cancelled. It remains a small compatibility seam for the
// interrupt tests and is also used by the arbiter's cancellation watcher.
type turnLifecycle struct {
	mu         sync.Mutex
	finished   bool
	cancelling bool
}

func (l *turnLifecycle) beginCancel() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.finished || l.cancelling {
		return false
	}
	l.cancelling = true
	return true
}

func (l *turnLifecycle) finish() {
	l.mu.Lock()
	l.finished = true
	l.mu.Unlock()
}

type streamView uint8

const (
	viewUnselected streamView = iota
	viewEvents
	viewObservations
)

// turnHandle is deliberately narrower than stream. A handle outlives the
// driver's active pointer during a close race, so its command channel is never
// closed and every send is guarded by done.
type turnHandle struct {
	commands chan steerCommand
	done     chan struct{}
	lane     *steerObservationLane

	mu      sync.Mutex
	retired bool
	pending map[*steerSendAck]struct{}
}

func (h *turnHandle) reserveSteer() (*steerObservationReservation, steerReservationStatus) {
	if h == nil || h.lane == nil {
		return nil, steerReservationClosed
	}
	return h.lane.reserve()
}

// steerSendResult is the linearized result of entering one turn's command
// mailbox. A pending result means the command entered the mailbox but the
// arbiter has not acknowledged it yet; callers must not retry from that
// state because the command may still be consumed.
type steerSendResult uint8

const (
	steerSendPending steerSendResult = iota
	steerSendAccepted
	steerSendRejected
	steerSendTerminal
)

func (r steerSendResult) String() string {
	switch r {
	case steerSendPending:
		return "pending"
	case steerSendAccepted:
		return "accepted"
	case steerSendRejected:
		return "rejected"
	case steerSendTerminal:
		return "terminal"
	default:
		return "unknown"
	}
}

// steerSendAck is the one-shot acknowledgement for a mailbox command. The
// state is authoritative; the notification channel is only a wake-up. This
// avoids a select choosing done after the arbiter has already accepted the
// command.
type steerSendAck struct {
	mu    sync.Mutex
	state steerSendResult
	ready chan struct{}
}

func newSteerSendAck() *steerSendAck {
	return &steerSendAck{ready: make(chan struct{})}
}

func (a *steerSendAck) resolve(state steerSendResult) bool {
	if a == nil || state == steerSendPending {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.state != steerSendPending {
		return false
	}
	a.state = state
	close(a.ready)
	return true
}

func (a *steerSendAck) snapshot() steerSendResult {
	if a == nil {
		return steerSendRejected
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state
}

// sendResult enters the command mailbox and waits for its linearized
// acknowledgement. Reserved calls use a nonblocking mailbox admission; the
// reservation lane guarantees that this path cannot be confused with output
// capacity exhaustion.
func (h *turnHandle) sendResult(ctx context.Context, command steerCommand) steerSendResult {
	if h == nil {
		return steerSendRejected
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ack := newSteerSendAck()
	command.ack = ack
	// accepted remains as a compatibility notification for old narrow tests;
	// ack is the only authoritative state used by this implementation.
	command.accepted = make(chan struct{})

	h.mu.Lock()
	if h.retired || channelClosed(h.done) {
		h.mu.Unlock()
		ack.resolve(steerSendTerminal)
		return steerSendTerminal
	}
	if h.pending == nil {
		h.pending = make(map[*steerSendAck]struct{})
	}
	h.pending[ack] = struct{}{}
	if command.reservation == nil && ctx.Err() != nil {
		delete(h.pending, ack)
		h.mu.Unlock()
		ack.resolve(steerSendRejected)
		return steerSendRejected
	}
	select {
	case h.commands <- command:
		h.mu.Unlock()
		return h.waitAndUntrack(ctx, ack)
	default:
	}
	// Reservation-backed calls must not wait behind a full mailbox: they have
	// a bounded pre-admission contract and can safely reject this one command.
	if command.reservation != nil {
		delete(h.pending, ack)
		h.mu.Unlock()
		ack.resolve(steerSendRejected)
		return steerSendRejected
	}
	h.mu.Unlock()

	// Keep the legacy unreserved path cancellable while waiting for mailbox
	// space. The handle retirement path resolves the registered ack if it wins
	// this race, so a stale send cannot strand its caller.
	select {
	case h.commands <- command:
		return h.waitAndUntrack(ctx, ack)
	case <-h.done:
		h.untrack(ack)
		ack.resolve(steerSendTerminal)
		return steerSendTerminal
	case <-ctx.Done():
		h.untrack(ack)
		ack.resolve(steerSendRejected)
		return steerSendRejected
	}
}

func (h *turnHandle) waitAndUntrack(ctx context.Context, ack *steerSendAck) steerSendResult {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		if state := ack.snapshot(); state != steerSendPending {
			h.untrack(ack)
			return state
		}
		select {
		case <-ack.ready:
			// Read the authoritative state on the next iteration.
		case <-h.done:
			ack.resolve(steerSendTerminal)
		case <-ctx.Done():
			state := ack.snapshot()
			h.untrack(ack)
			return state
		}
	}
}

func (h *turnHandle) untrack(ack *steerSendAck) {
	if h == nil || ack == nil {
		return
	}
	h.mu.Lock()
	delete(h.pending, ack)
	h.mu.Unlock()
}

func (h *turnHandle) send(ctx context.Context, command steerCommand) bool {
	state := h.sendResult(ctx, command)
	return state == steerSendAccepted || state == steerSendPending
}

// retire linearizes terminal publication with command sends. Any command
// whose arbiter acknowledgement has not won yet becomes terminal exactly once;
// accepted acknowledgements remain accepted when done is closed.
func (h *turnHandle) retire() {
	if h == nil {
		return
	}
	h.mu.Lock()
	if !h.retired {
		h.retired = true
		if h.done != nil && !channelClosed(h.done) {
			close(h.done)
		}
		for ack := range h.pending {
			ack.resolve(steerSendTerminal)
		}
	}
	h.mu.Unlock()
	if h.lane != nil {
		h.lane.close()
	}
}

func channelClosed(ch <-chan struct{}) bool {
	if ch == nil {
		return false
	}
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

type steerCommand struct {
	ctx     context.Context
	request driver.SteerRequest
	reply   chan steerReply
	// accepted is retained for compatibility with pre-linearization tests;
	// steerSendAck is the authoritative acknowledgement state.
	accepted    chan struct{}
	ack         *steerSendAck
	attempt     *steerAttempt
	reservation *steerObservationReservation
}

type steerReply struct {
	result driver.SteerResult
	err    error
}

// stream is one prompt view over Driver's persistent ACP session. The
// projection owner is the only goroutine that touches the two public channels;
// this object only carries lifecycle and history operations.
type stream struct {
	projection *projection
	handle     *turnHandle
	ordered    bool

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	once     sync.Once
	closeErr error
}

func (s *stream) Events() <-chan driver.Event {
	if s == nil || s.projection == nil {
		return nil
	}
	return s.projection.eventsView()
}

func (s *stream) History() (driver.History, error) {
	return driver.History{Available: false}, nil
}

func (s *stream) Close() error {
	if s == nil {
		return nil
	}
	s.once.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
	})
	return s.closeErr
}

// orderedStream keeps the existing concrete shape used by callers that
// discover driver.OrderedStream, while leaving channel ownership to stream's
// projection owner.
type orderedStream struct{ stream *stream }

func (s *orderedStream) Events() <-chan driver.Event {
	if s == nil {
		return nil
	}
	return s.stream.Events()
}
func (s *orderedStream) Observations() <-chan driver.Observation {
	if s == nil || s.stream == nil || s.stream.projection == nil {
		return nil
	}
	return s.stream.projection.observationsView()
}
func (s *orderedStream) History() (driver.History, error) {
	if s == nil {
		return driver.History{Available: false}, nil
	}
	return s.stream.History()
}
func (s *orderedStream) Close() error {
	if s == nil {
		return nil
	}
	return s.stream.Close()
}

// Spawn starts one prompt on the session created by New. The caller context
// owns stream forwarding and cancellation; the driver's context owns the
// protocol operation and is canceled only when the driver closes.
func (d *Driver) Spawn(ctx context.Context, turn driver.Turn) (driver.Stream, error) {
	if d == nil || d.session == nil {
		return nil, &driver.SpawnError{Cause: errors.New("acp: session unavailable")}
	}
	sess, ok := d.session.(turnSession)
	if !ok {
		legacy, legacyOK := d.session.(promptSession)
		if legacyOK {
			sess = legacyTurnSession{promptSession: legacy}
			ok = true
		}
	}
	if !ok {
		return nil, &driver.SpawnError{Cause: errors.New("acp: session does not support turns")}
	}
	if ctx == nil {
		ctx = context.Background()
	}

	d.turnMu.Lock()
	if d.closed {
		d.turnMu.Unlock()
		return nil, &driver.SpawnError{Cause: errors.New("acp: driver is closed")}
	}
	driverCtx := d.driverCtx
	if driverCtx == nil {
		driverCtx = context.Background()
	}
	turnCtx, cancel := context.WithCancel(ctx)
	projectionOwner := newProjection()
	projectionOwner.stopOn(driverCtx)
	handleDone := make(chan struct{})
	// The mailbox is never closed. Each accepted command is acknowledged by
	// the arbiter, while done retires buffered stale sends at terminal close.
	handle := &turnHandle{
		commands: make(chan steerCommand, 512),
		done:     handleDone,
		lane:     newSteerObservationLane(steeringObservationCapacity),
	}
	s := &stream{
		projection: projectionOwner,
		handle:     handle,
		ordered:    d.steeringEnabled(),
		ctx:        turnCtx,
		cancel:     cancel,
		done:       make(chan struct{}),
	}
	if s.ordered {
		projectionOwner.selectView(viewObservations)
	} else {
		projectionOwner.selectView(viewEvents)
	}
	d.activeMu.Lock()
	d.active = handle
	d.activeMu.Unlock()
	go d.runTurn(turnCtx, cancel, driverCtx, sess, turn, s, handleDone)
	if s.ordered {
		return &orderedStream{stream: s}, nil
	}
	return s, nil
}

func (d *Driver) steeringEnabled() bool {
	if d == nil {
		return false
	}
	d.steeringMu.Lock()
	defer d.steeringMu.Unlock()
	return d.steeringOn && !d.steeringOff
}

func (d *Driver) runTurn(
	turnCtx context.Context,
	turnCancel context.CancelFunc,
	driverCtx context.Context,
	sess turnSession,
	turn driver.Turn,
	streamState *stream,
	handleDone chan struct{},
) {
	defer close(streamState.done)
	lifecycle := &turnLifecycle{}
	turnDone := make(chan struct{})
	watcherDone := make(chan struct{})
	go watchTurnCancellation(turnCtx, driverCtx, turnCancel, sess, turnDone, watcherDone, lifecycle)

	barrier := newBarrierWorker(driverCtx, sess)
	dispatcher := newSteerDispatcher(driverCtx, d.session)
	updates := sess.Updates()
	promptDone := make(chan promptOutcome, 1)
	promptReturned := make(chan struct{})
	go func() {
		defer close(promptReturned)
		result, err := sess.Prompt(driverCtx, promptBlocks(turn))
		promptDone <- promptOutcome{result: result, err: err}
	}()

	a := &turnArbiter{
		driver:     d,
		turnCtx:    turnCtx,
		driverCtx:  driverCtx,
		session:    sess,
		stream:     streamState,
		projection: streamState.projection,
		handle:     streamState.handle,
		updates:    updates,
		promptDone: promptDone,
		barrier:    barrier,
		dispatcher: dispatcher,
		commands:   streamState.handle.commands,
		state:      &translationState{},
		pending:    make([]arbObservation, 0, 32),
		steers:     make(map[uint64]steerCommand),
	}
	a.run()
	// Stop discovery as soon as the arbiter retires. A caller that already
	// captured this handle may still race retirement, but reserve() now
	// distinguishes a closed lane from true capacity exhaustion.
	d.activeMu.Lock()
	if d.active == streamState.handle {
		d.active = nil
	}
	d.activeMu.Unlock()
	// No command can be admitted after the arbiter returns. Retire the handle
	// after clearing Driver.active so stale discovery cannot be mistaken for a
	// fresh active turn, and resolve any command still buffered in the mailbox.
	streamState.handle.retire()
	if driverCtx.Err() != nil {
		// Driver.Close must cancel the protocol operation before closing its
		// owned client. The arbiter may have exited on cancellation while the
		// provider is still unwinding Prompt, so retain that lifecycle fence.
		<-promptReturned
	}
	lifecycle.finish()
	close(turnDone)
	<-watcherDone
	turnCancel()
	barrier.stop()
	dispatcher.stop()
	streamState.projection.close()
	d.turnMu.Unlock()
}

type barrierRequest struct {
	sequence        uint64
	waitForDelivery bool
}

type barrierResult struct {
	sequence uint64
	err      error
}

type barrierWorker struct {
	ctx      context.Context
	cancel   context.CancelFunc
	session  turnSession
	requests chan barrierRequest
	results  chan barrierResult
	done     chan struct{}
	stopOnce sync.Once
}

func newBarrierWorker(ctx context.Context, sess turnSession) *barrierWorker {
	if ctx == nil {
		ctx = context.Background()
	}
	workerCtx, cancel := context.WithCancel(ctx)
	b := &barrierWorker{
		ctx:      workerCtx,
		cancel:   cancel,
		session:  sess,
		requests: make(chan barrierRequest, 32),
		results:  make(chan barrierResult, 32),
		done:     make(chan struct{}),
	}
	go b.run()
	return b
}

func (b *barrierWorker) run() {
	defer close(b.results)
	defer close(b.done)
	for {
		select {
		case request := <-b.requests:
			var err error
			if request.waitForDelivery {
				err = b.session.WaitForUpdates(b.ctx)
			} else {
				err = waitForUpdatesThrough(b.ctx, b.session, request.sequence)
			}
			select {
			case b.results <- barrierResult{sequence: request.sequence, err: err}:
			case <-b.ctx.Done():
				return
			}
		case <-b.ctx.Done():
			return
		}
	}
}

func (b *barrierWorker) request(sequence uint64) bool {
	return b.enqueue(barrierRequest{sequence: sequence})
}

func (b *barrierWorker) requestDelivery(sequence uint64) bool {
	return b.enqueue(barrierRequest{sequence: sequence, waitForDelivery: true})
}

func (b *barrierWorker) enqueue(request barrierRequest) bool {
	if b == nil {
		return false
	}
	select {
	case b.requests <- request:
		return true
	case <-b.done:
		return false
	case <-b.ctx.Done():
		return false
	}
}

func (b *barrierWorker) stop() {
	if b == nil {
		return
	}
	b.stopOnce.Do(func() { b.cancel() })
	select {
	case <-b.done:
	case <-time.After(steerTerminalGrace):
	}
}

type arbObservation struct {
	observation driver.Observation
	events      []driver.Event
	sequence    uint64
	local       bool
	reply       chan steerReply
	result      steerReply
	reservation *steerObservationReservation
}

type turnArbiter struct {
	driver     *Driver
	turnCtx    context.Context
	driverCtx  context.Context
	session    turnSession
	stream     *stream
	projection *projection
	handle     *turnHandle
	commands   <-chan steerCommand
	updates    <-chan client.Update
	promptDone <-chan promptOutcome

	barrier    *barrierWorker
	dispatcher *steerDispatcher
	state      *translationState
	pending    []arbObservation
	steers     map[uint64]steerCommand
	nextSteer  uint64

	prompt                 *promptOutcome
	promptQueued           bool
	terminalAsked          bool
	terminalReady          bool
	updatesDrained         bool
	promptBarrierRequested bool

	barrierBusy      bool
	barrierCompleted uint64
	barrierFence     bool
	lastRaw          uint64
	lastOrder        uint64
	failed           bool
}

func (a *turnArbiter) run() {
	defer a.releaseReservations()
	for {
		select {
		case <-a.driverCtx.Done():
			// Check before draining any producer so a shutdown cannot be
			// starved by a continuously ready update or command source.
			return
		default:
		}
		if a.failed {
			return
		}
		a.drainQueuedCommands()
		if a.prompt != nil && !a.terminalAsked {
			a.beginTerminal()
		}
		if a.prompt != nil && a.terminalAsked && !a.promptBarrierRequested {
			a.maybeRequestPromptBarrier()
		}
		if a.prompt != nil && a.terminalAsked && a.promptBarrierRequested && !a.barrierBusy && !a.updatesDrained {
			a.updatesDrained = !a.drainTerminalUpdates()
			if !a.updatesDrained {
				continue
			}
		}
		if a.prompt != nil && a.terminalAsked && a.updatesDrained && !a.promptQueued {
			a.pending = append(a.pending, a.promptObservation(*a.prompt))
			a.promptQueued = true
		}
		if a.tryEmit() {
			continue
		}
		if a.prompt != nil && a.terminalAsked && a.terminalReady && a.updatesDrained && len(a.steers) == 0 && len(a.pending) == 0 && !a.barrierBusy {
			a.emitPromptTerminal(*a.prompt)
			return
		}
		var dispatcherEvents <-chan dispatcherEvent
		if a.dispatcher != nil {
			dispatcherEvents = a.dispatcher.Events()
		}
		var barrierResults <-chan barrierResult
		if a.barrier != nil {
			barrierResults = a.barrier.results
		}

		select {
		case update, ok := <-a.updates:
			if !ok {
				a.updates = nil
				continue
			}
			a.acceptUpdate(update)
		case command, ok := <-a.commands:
			if !ok {
				// The command mailbox is intentionally never closed. A nil read is
				// treated as a stale source and does not close any public channel.
				continue
			}
			a.acknowledgeCommand(command)
			a.acceptSteer(command)
		case outcome := <-a.promptDone:
			a.promptDone = nil
			a.prompt = &outcome
		case event, ok := <-dispatcherEvents:
			if !ok {
				a.dispatcher = nil
				continue
			}
			a.acceptDispatcherEvent(event)
		case result, ok := <-barrierResults:
			if !ok {
				// Driver cancellation can stop the serialized worker before it
				// publishes its final result. Retire the in-flight marker with the
				// worker; otherwise terminal publication waits forever on a worker
				// that has already exited.
				a.barrierBusy = false
				a.barrier = nil
				continue
			}
			a.barrierBusy = false
			a.barrierCompleted = maxUint64(a.barrierCompleted, result.sequence)
			if result.sequence == 0 {
				a.barrierFence = true
			}
			if result.err != nil && a.driverCtx.Err() == nil {
				slog.Warn("acp: update delivery barrier failed")
			}
			if a.prompt != nil && a.terminalAsked {
				a.updatesDrained = !a.drainTerminalUpdates()
			}
		case <-a.driverCtx.Done():
			// Driver shutdown owns the protocol lifetime. Abandon any pending
			// terminal projection and retire the arbiter so Close cannot wait on
			// an output consumer that has already been aborted.
			return
		}
	}
}

func (a *turnArbiter) drainQueuedCommands() {
	for {
		select {
		case command := <-a.commands:
			a.acknowledgeCommand(command)
			a.acceptSteer(command)
		default:
			return
		}
	}
}

func (a *turnArbiter) acknowledgeCommand(command steerCommand) {
	if command.ack != nil {
		command.ack.resolve(steerSendAccepted)
	}
}

func (a *turnArbiter) releaseReservations() {
	if a == nil {
		return
	}
	for id, command := range a.steers {
		delete(a.steers, id)
		command.reservation.release()
	}
	for _, candidate := range a.pending {
		candidate.reservation.release()
	}
	a.pending = nil
}

func (a *turnArbiter) acceptUpdate(update client.Update) {
	if update.Meta.IsReplay {
		slog.Debug("acp: ignored replay session update")
		return
	}
	events := translateUpdate(update.SessionUpdate, a.state)
	for _, event := range events {
		a.pending = append(a.pending, arbObservation{
			observation: driver.UpdateObservation{Event: event, ReceiveSequence: update.ReceiveSequence},
			events:      []driver.Event{event},
			sequence:    update.ReceiveSequence,
			local:       update.ReceiveSequence == 0,
		})
	}
}

func (a *turnArbiter) acceptSteer(command steerCommand) {
	if command.reply == nil {
		command.reservation.release()
		return
	}
	if command.attempt == nil {
		command.attempt = &steerAttempt{}
	}
	if command.ctx != nil && command.ctx.Err() != nil {
		command.attempt.cancelAndSnapshot()
		a.queueLocalSteer(command, driver.SteerResult{Outcome: driver.SteerOutcomeFallbackRequired}, command.ctx.Err())
		return
	}
	if admission := command.attempt.snapshot(); admission != steerAdmissionPending {
		if admission == steerAdmissionAdmitted || admission == steerAdmissionPendingWriter {
			a.queueLocalSteer(command, driver.SteerResult{Outcome: driver.SteerOutcomeAdmissionUnknown}, errors.New("acp: steering admission unresolved"))
			return
		}
		a.queueLocalSteer(command, driver.SteerResult{Outcome: driver.SteerOutcomeFallbackRequired}, errors.New("acp: steering canceled before start"))
		return
	}
	if err := command.request.Validate(); err != nil {
		command.attempt.markAdmission(false)
		a.queueLocalSteer(command, driver.SteerResult{Outcome: driver.SteerOutcomeUnsupported}, err)
		return
	}
	if !a.driver.steeringEnabled() {
		command.attempt.markAdmission(false)
		a.queueLocalSteer(command, driver.SteerResult{Outcome: driver.SteerOutcomeUnsupported}, nil)
		return
	}
	prompt, err := steerPromptBlocks(command.request)
	if err != nil {
		command.attempt.markAdmission(false)
		a.queueLocalSteer(command, driver.SteerResult{Outcome: driver.SteerOutcomeUnsupported}, err)
		return
	}
	a.nextSteer++
	id := a.nextSteer
	a.steers[id] = command
	params := client.SteerParams{
		SessionID: a.session.ID(),
		Prompt:    prompt,
		Meta:      jsonRaw(steeringIdleMeta),
	}
	if !a.dispatcher.submit(steerJob{id: id, ctx: command.ctx, params: params, attempt: command.attempt}) {
		delete(a.steers, id)
		command.attempt.markAdmission(false)
		a.queueLocalSteer(command, driver.SteerResult{Outcome: driver.SteerOutcomeFallbackRequired}, errors.New("acp: steering dispatcher unavailable"))
	}
}

func (a *turnArbiter) queueLocalSteer(command steerCommand, result driver.SteerResult, err error) {
	a.pending = append(a.pending, arbObservation{
		observation: driver.SteerObservation{SteerResult: result, Err: err},
		sequence:    result.ReceiveSequence,
		local:       result.ReceiveSequence == 0,
		reply:       command.reply,
		result:      steerReply{result: result, err: err},
		reservation: command.reservation,
	})
}

func (a *turnArbiter) acceptDispatcherEvent(event dispatcherEvent) {
	if event.terminalComplete {
		a.terminalReady = true
		return
	}
	command, ok := a.steers[event.id]
	if !ok {
		if event.late || event.err != nil || event.result.WriteAdmitted {
			a.disableSteering()
		}
		return
	}
	if event.late {
		delete(a.steers, event.id)
		result := lateSteerResult(event.admission, event.result)
		if result.Outcome == driver.SteerOutcomeDeliveryUnknown || result.Outcome == driver.SteerOutcomeAdmissionUnknown {
			a.disableSteering()
		}
		err := event.err
		if err == nil {
			err = errors.New("acp: steering result arrived after caller deadline")
		}
		a.pending = append(a.pending, arbObservation{
			observation: driver.SteerObservation{SteerResult: result, Err: err},
			sequence:    0,
			local:       true,
			reply:       command.reply,
			result:      steerReply{result: result, err: err},
			reservation: command.reservation,
		})
		return
	}
	delete(a.steers, event.id)
	if a.terminalAsked && len(a.steers) == 0 {
		a.maybeRequestPromptBarrier()
	}
	var normalized driver.SteerResult
	var err error
	switch event.admission {
	case steerAdmissionPending, steerAdmissionPendingWriter:
		// StartSteer was invoked (or is still in the linearization window),
		// but no positive writer fact exists. Keep the admission bit false and
		// discard transport sequence aliases that would imply admission.
		normalized = driver.SteerResult{Outcome: driver.SteerOutcomeAdmissionUnknown, Reason: event.result.Reason}
		err = event.err
		if err == nil {
			err = errors.New("acp: steering admission unresolved")
		}
	case steerAdmissionNotAdmitted:
		normalized = driver.SteerResult{Outcome: driver.SteerOutcomeFallbackRequired, Reason: event.result.Reason}
		err = event.err
	default:
		eventResult := event.result
		eventResult.WriteAdmitted = true
		normalized, err = normalizeSteering(eventResult, event.err)
	}
	if normalized.Outcome == driver.SteerOutcomeDeliveryUnknown || normalized.Outcome == driver.SteerOutcomeDeliveredUntrackable || steeringErrorGuaranteesNoDelivery(event.err) {
		a.disableSteering()
	}
	a.pending = append(a.pending, arbObservation{
		observation: driver.SteerObservation{SteerResult: normalized, Err: err},
		sequence:    normalized.ReceiveSequence,
		local:       normalized.ReceiveSequence == 0,
		reply:       command.reply,
		result:      steerReply{result: normalized, err: err},
		reservation: command.reservation,
	})
}

func lateSteerResult(admission steerAdmission, result client.SteerResult) driver.SteerResult {
	switch admission {
	case steerAdmissionAdmitted:
		return driver.SteerResult{Outcome: driver.SteerOutcomeDeliveryUnknown, WriteAdmitted: true}
	case steerAdmissionNotAdmitted:
		return driver.SteerResult{Outcome: driver.SteerOutcomeFallbackRequired}
	default:
		return driver.SteerResult{Outcome: driver.SteerOutcomeAdmissionUnknown}
	}
}

func (a *turnArbiter) disableSteering() {
	a.driver.steeringMu.Lock()
	a.driver.steeringOff = true
	a.driver.steeringMu.Unlock()
}

func (a *turnArbiter) beginTerminal() {
	a.terminalAsked = true
	a.updatesDrained = false
	// Commands already in the turn mailbox belong before the terminal. The
	// arbiter drains them before sending the FIFO terminal marker to the
	// dispatcher, preventing a queued call from disappearing during a prompt
	// completion race.
	a.drainQueuedCommands()
	if a.prompt == nil {
		return
	}
	if !a.dispatcher.resolveTerminal() {
		a.terminalReady = true
	}
}

func (a *turnArbiter) maybeRequestPromptBarrier() {
	if a.prompt == nil || !a.terminalAsked || !a.terminalReady || a.promptBarrierRequested || len(a.steers) != 0 || hasPendingSteer(a.pending) {
		return
	}
	a.promptBarrierRequested = true
	a.requestPromptBarrier(promptSequence(*a.prompt))
}

func hasPendingSteer(pending []arbObservation) bool {
	for _, candidate := range pending {
		if _, ok := candidate.observation.(driver.SteerObservation); ok {
			return true
		}
	}
	return false
}

// drainTerminalUpdates takes the updates that were delivered before the
// prompt barrier. The session update channel is shared by turns, so this is a
// non-blocking drain: it never waits for a future turn's notification.
func (a *turnArbiter) drainTerminalUpdates() bool {
	drained := false
	for a.updates != nil {
		select {
		case update, ok := <-a.updates:
			if !ok {
				a.updates = nil
				return drained
			}
			a.acceptUpdate(update)
			drained = true
		default:
			return drained
		}
	}
	return drained
}

func (a *turnArbiter) promptObservation(outcome promptOutcome) arbObservation {
	if outcome.result == nil {
		return arbObservation{
			observation: driver.PromptObservation{Err: outcome.err},
			sequence:    0,
			local:       true,
		}
	}
	return arbObservation{
		observation: driver.PromptObservation{
			StopReason:       string(outcome.result.StopReason),
			Message:          a.state.message(),
			WriteAdmitted:    outcome.result.WriteAdmitted,
			ReceiveSequence:  outcome.result.ReceiveSequence,
			ResponseSequence: outcome.result.ResponseSequence,
			Err:              outcome.err,
		},
		sequence: outcome.result.ReceiveSequence,
		local:    outcome.result.ReceiveSequence == 0,
	}
}

func (a *turnArbiter) requestBarrier(sequence uint64) {
	a.requestBarrierMode(sequence, false)
}

func (a *turnArbiter) requestPromptBarrier(sequence uint64) {
	a.requestBarrierMode(sequence, true)
}

func (a *turnArbiter) requestBarrierMode(sequence uint64, waitForDelivery bool) {
	if a.barrier == nil {
		a.barrierFence = true
		return
	}
	if sequence != 0 && sequence <= a.barrierCompleted {
		return
	}
	if sequence == 0 && a.barrierFence {
		return
	}
	if a.barrierBusy {
		return
	}
	if waitForDelivery {
		a.barrierBusy = a.barrier.requestDelivery(sequence)
		return
	}
	a.barrierBusy = a.barrier.request(sequence)
}

func (a *turnArbiter) tryEmit() bool {
	if len(a.pending) == 0 {
		return false
	}
	index := a.nextObservationIndex()
	if index < 0 {
		return false
	}
	candidate := a.pending[index]
	if candidate.sequence != 0 {
		if candidate.sequence < a.lastRaw {
			a.failSequenceRegression()
			return true
		}
		_, isUpdate := candidate.observation.(driver.UpdateObservation)
		if !isUpdate && candidate.sequence > a.barrierCompleted {
			a.requestBarrier(candidate.sequence)
			return false
		}
		// A positive update observed while a steer is unresolved may have a
		// greater sequence than the steer response. Hold it until the FIFO
		// dispatcher reports the call, allowing scheduler inversion without
		// relabeling the raw sequence.
		if _, update := candidate.observation.(driver.UpdateObservation); update && len(a.steers) > 0 {
			return false
		}
	} else {
		if hasPositivePending(a.pending) || len(a.steers) > 0 {
			return false
		}
		if a.prompt != nil && !a.barrierFence {
			a.requestBarrier(0)
			return false
		}
		a.lastOrder = maxUint64(a.lastOrder, a.lastRaw) + 1
		candidate = a.withLocalOrder(candidate)
	}
	a.pending = append(a.pending[:index], a.pending[index+1:]...)
	if candidate.sequence != 0 {
		a.lastRaw = candidate.sequence
		a.lastOrder = maxUint64(a.lastOrder, candidate.sequence)
	}
	a.emitObservation(candidate)
	return true
}

func (a *turnArbiter) withLocalOrder(candidate arbObservation) arbObservation {
	order := a.lastOrder
	switch typed := candidate.observation.(type) {
	case driver.PromptObservation:
		typed.OrderSequence = order
		candidate.observation = typed
	case driver.UpdateObservation:
		typed.OrderSequence = order
		candidate.observation = typed
	case driver.SteerObservation:
		typed.OrderSequence = order
		typed.SteerResult.OrderSequence = order
		candidate.observation = typed
		candidate.result.result.OrderSequence = order
	}
	return candidate
}

func (a *turnArbiter) nextObservationIndex() int {
	best := -1
	bestSequence := uint64(0)
	for i, candidate := range a.pending {
		if candidate.sequence == 0 {
			if best < 0 {
				best = i
			}
			continue
		}
		if best < 0 || bestSequence == 0 || candidate.sequence < bestSequence {
			best = i
			bestSequence = candidate.sequence
		}
	}
	return best
}

func hasPositivePending(pending []arbObservation) bool {
	for _, candidate := range pending {
		if candidate.sequence != 0 {
			return true
		}
	}
	return false
}

func (a *turnArbiter) emitObservation(candidate arbObservation) {
	observation, events := candidate.observation, candidate.events
	if observation == nil {
		candidate.reservation.release()
		return
	}
	if _, ok := observation.(driver.SteerObservation); ok {
		defer candidate.reservation.release()
	}
	var ready <-chan struct{}
	if typed, ok := observation.(driver.UpdateObservation); ok {
		a.projection.emitObservation(typed)
		for _, event := range events {
			a.projection.emitEvent(event)
		}
		return
	}
	if typed, ok := observation.(driver.SteerObservation); ok {
		ready = a.projection.emitObservation(typed)
		// Steer acknowledgements intentionally have no legacy event projection.
	} else if typed, ok := observation.(driver.PromptObservation); ok {
		a.projection.emitObservation(typed)
		a.emitPromptEvents(typed)
	}
	if candidate.reply != nil {
		if ready != nil {
			timer := time.NewTimer(steerTerminalGrace)
			select {
			case <-ready:
			case <-timer.C:
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
		// The channel is one-shot and buffered. A caller may have hit its own
		// deadline, but the arbiter still records exactly one result paired with
		// the emitted observation.
		candidate.reply <- candidate.result
	}
}

func (a *turnArbiter) emitPromptEvents(observation driver.PromptObservation) {
	if observation.Err != nil || observation.StopReason == "" {
		a.projection.emitEvent(promptFailureEvent(observation.Err))
		return
	}
	if a.stream == nil || !a.stream.ordered {
		if message := a.state.message(); message != nil {
			a.projection.emitEvent(driver.Event{Kind: driver.KindStepComplete, Message: message})
		}
	}
	a.projection.emitEvent(terminalEvent(protocol.StopReason(observation.StopReason)))
}

func (a *turnArbiter) emitPromptTerminal(_ promptOutcome) {
	// The prompt observation itself is emitted by tryEmit, where its transport
	// sequence participates in the same ordering fence as updates and steers.
}

func (a *turnArbiter) failSequenceRegression() {
	if a.failed {
		return
	}
	a.failed = true
	a.disableSteering()
	failureErr := errors.New("acp update order invalid")
	for id, command := range a.steers {
		delete(a.steers, id)
		failure := steerReply{result: sequenceFailureResult(command.attempt), err: failureErr}
		a.projection.emitObservation(driver.SteerObservation{SteerResult: failure.result, Err: failure.err})
		command.reservation.release()
		if command.reply != nil {
			command.reply <- failure
		}
	}
	for _, candidate := range a.pending {
		candidate.reservation.release()
		if candidate.reply != nil {
			a.projection.emitObservation(candidate.observation)
			candidate.reply <- candidate.result
		}
	}
	a.pending = nil
	// Fail closed with a fixed diagnostic. No positive observation is rewritten
	// to make the sequence appear monotonic.
	a.projection.emitEvent(driver.Event{Kind: driver.KindTerminalError, ErrText: "acp update order invalid"})
	a.projection.emitObservation(driver.PromptObservation{Err: errors.New("acp update order invalid")})
}

func sequenceFailureResult(attempt *steerAttempt) driver.SteerResult {
	if attempt == nil {
		return driver.SteerResult{Outcome: driver.SteerOutcomeAdmissionUnknown}
	}
	switch attempt.snapshot() {
	case steerAdmissionAdmitted:
		return driver.SteerResult{Outcome: driver.SteerOutcomeDeliveryUnknown, WriteAdmitted: true}
	case steerAdmissionNotAdmitted:
		return driver.SteerResult{Outcome: driver.SteerOutcomeFallbackRequired}
	default:
		return driver.SteerResult{Outcome: driver.SteerOutcomeAdmissionUnknown}
	}
}

func waitForUpdatesThrough(ctx context.Context, sess turnSession, sequence uint64) error {
	if ordered, ok := sess.(orderedUpdateBarrier); ok && sequence != 0 {
		return ordered.WaitForUpdatesThrough(ctx, sequence)
	}
	return sess.WaitForUpdates(ctx)
}

func promptSequence(outcome promptOutcome) uint64 {
	if outcome.result == nil {
		return 0
	}
	return outcome.result.ReceiveSequence
}

func watchTurnCancellation(
	turnCtx, driverCtx context.Context,
	turnCancel context.CancelFunc,
	sess turnSession,
	turnDone <-chan struct{},
	watcherDone chan<- struct{},
	lifecycle *turnLifecycle,
) {
	defer close(watcherDone)
	select {
	case <-turnCtx.Done():
	case <-driverCtx.Done():
		turnCancel()
	case <-turnDone:
		return
	}
	if !lifecycle.beginCancel() {
		return
	}
	if err := sess.Cancel(driverCtx); err != nil {
		slog.Warn("acp: session cancel failed")
	}
}

// drainTurnUpdates remains a small compatibility utility for callers that
// need to drain a legacy stream outside a live arbiter. The live arbiter never
// calls it, so it cannot compete with the session's sole update reader.
func drainTurnUpdates(turnCtx context.Context, updates <-chan client.Update, state *translationState, events chan<- driver.Event) {
	if updates == nil {
		return
	}
	for {
		select {
		case update, ok := <-updates:
			if !ok {
				return
			}
			if update.Meta.IsReplay {
				continue
			}
			for _, event := range translateUpdate(update.SessionUpdate, state) {
				select {
				case events <- event:
				case <-turnCtx.Done():
					return
				}
			}
		default:
			return
		}
	}
}

func maxUint64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}

func jsonRaw(raw string) []byte { return []byte(raw) }

// maxACPModelFacingErrorBytes bounds the complete model-facing projection.
const maxACPModelFacingErrorBytes = 512

const (
	maxACPErrorDepth    = 32
	maxACPErrorNodes    = 128
	maxACPErrorChildren = 64
)

const redactedACPPath = "[REDACTED_PATH]"

var (
	acpMessageURLPattern              = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s<>"']+`)
	acpMessageAuthPattern             = regexp.MustCompile(`(?i)(\b(?:authorization|proxy-authorization)\b\s*["']?\s*[:=]\s*)[^\r\n,;&}\]]+`)
	acpMessageSecretAssignmentPattern = regexp.MustCompile(`(?i)(\b(?:api[\s_-]*key|access[\s_-]*token|refresh[\s_-]*token|token|password|credential|secret)\b\s*["']?\s*[:=]\s*)(?:"[^"]*"|'[^']*'|[^\s,;&}\]]+)`)
	acpMessageBearerPattern           = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9][A-Za-z0-9._~+/=-]*`)
	acpMessageUnixPathPattern         = regexp.MustCompile(`/[^\s,;)}\]>"']+`)
	acpMessageWindowsPathPattern      = regexp.MustCompile(`(?i)[A-Za-z]:[\\/][^\s,;)}\]>"']*`)
)

func promptFailureEvent(err error) driver.Event {
	if detail, ok := safeACPErrorDetail(err); ok {
		return driver.Event{Kind: driver.KindModelFacingError, ErrText: detail}
	}
	return driver.Event{Kind: driver.KindTerminalError, ErrText: "acp prompt failed"}
}

func safeACPErrorDetail(err error) (string, bool) {
	type node struct {
		err   error
		depth int
	}
	if isNilACPError(err) {
		return "", false
	}
	pending := []node{{err: err}}
	seen := make(map[error]struct{})
	visited := 0
	for len(pending) > 0 && visited < maxACPErrorNodes {
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if isNilACPError(current.err) || markACPErrorSeen(seen, current.err) {
			continue
		}
		visited++
		if code, message, ok := directACPErrorFields(current.err); ok {
			return formatACPErrorDetail(code, message), true
		}
		if current.depth >= maxACPErrorDepth {
			continue
		}
		if wrapper, ok := current.err.(interface{ Unwrap() []error }); ok {
			children := safeACPUnwrapMany(wrapper)
			if len(children) > maxACPErrorChildren {
				children = children[:maxACPErrorChildren]
			}
			for index := len(children) - 1; index >= 0; index-- {
				pending = append(pending, node{err: children[index], depth: current.depth + 1})
			}
			continue
		}
		if wrapper, ok := current.err.(interface{ Unwrap() error }); ok {
			pending = append(pending, node{err: safeACPUnwrapOne(wrapper), depth: current.depth + 1})
		}
	}
	return "", false
}

func directACPErrorFields(err error) (protocol.ErrorCode, string, bool) {
	switch typed := any(err).(type) {
	case *protocol.Error:
		if typed == nil {
			return 0, "", false
		}
		return typed.Code, typed.Message, true
	case protocol.Error:
		return typed.Code, typed.Message, true
	case *protocol.Fault:
		if typed == nil {
			return 0, "", false
		}
		return typed.Code, typed.Message, true
	case protocol.Fault:
		return typed.Code, typed.Message, true
	default:
		return 0, "", false
	}
}

func isNilACPError(err error) bool {
	if err == nil {
		return true
	}
	value := reflect.ValueOf(err)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func markACPErrorSeen(seen map[error]struct{}, err error) bool {
	typeOfError := reflect.TypeOf(err)
	if typeOfError == nil || !typeOfError.Comparable() {
		return false
	}
	if _, ok := seen[err]; ok {
		return true
	}
	seen[err] = struct{}{}
	return false
}

func safeACPUnwrapOne(wrapper interface{ Unwrap() error }) (next error) {
	defer func() {
		if recover() != nil {
			next = nil
		}
	}()
	return wrapper.Unwrap()
}

func safeACPUnwrapMany(wrapper interface{ Unwrap() []error }) (children []error) {
	defer func() {
		if recover() != nil {
			children = nil
		}
	}()
	return wrapper.Unwrap()
}

func formatACPErrorDetail(code protocol.ErrorCode, message string) string {
	message = normalizeACPErrorMessage(message)
	detail := fmt.Sprintf("ACP error %d", code)
	if message != "" {
		detail += ": " + message
	}
	return truncateValidUTF8(detail, maxACPModelFacingErrorBytes)
}

func normalizeACPErrorMessage(message string) string {
	message = strings.ToValidUTF8(message, "\uFFFD")
	var normalized strings.Builder
	normalized.Grow(len(message))
	for _, r := range message {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029' {
			normalized.WriteByte(' ')
			continue
		}
		normalized.WriteRune(r)
	}
	return redactACPErrorMessage(strings.Join(strings.Fields(normalized.String()), " "))
}

func redactACPErrorMessage(message string) string {
	message = acpMessageURLPattern.ReplaceAllString(message, redactedURL)
	message = acpMessageAuthPattern.ReplaceAllString(message, "$1"+redactedToolValue)
	message = acpMessageSecretAssignmentPattern.ReplaceAllString(message, "$1"+redactedToolValue)
	message = acpMessageBearerPattern.ReplaceAllString(message, redactedToolValue)
	message = toolCredentialTokenPattern.ReplaceAllString(message, redactedToolValue)
	message = acpMessageWindowsPathPattern.ReplaceAllString(message, redactedACPPath)
	message = acpMessageUnixPathPattern.ReplaceAllString(message, redactedACPPath)
	return message
}

func truncateValidUTF8(input string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(input) <= maxBytes {
		return input
	}
	cut := 0
	for cut < len(input) {
		_, size := utf8.DecodeRuneInString(input[cut:])
		if cut+size > maxBytes {
			break
		}
		cut += size
	}
	return input[:cut]
}

func terminalEvent(reason protocol.StopReason) driver.Event {
	switch reason {
	case protocol.StopReasonEndTurn, protocol.StopReasonCancelled:
		return driver.Event{Kind: driver.KindTerminalOK}
	case protocol.StopReasonMaxTokens:
		return driver.Event{Kind: driver.KindTerminalError, ErrText: "acp prompt reached its token limit"}
	case protocol.StopReasonMaxTurnRequests:
		return driver.Event{Kind: driver.KindTerminalError, ErrText: "acp prompt reached its turn limit"}
	case protocol.StopReasonRefusal:
		return driver.Event{Kind: driver.KindTerminalError, ErrText: "acp prompt was refused"}
	default:
		return driver.Event{Kind: driver.KindTerminalError, ErrText: "acp prompt ended with an unknown stop reason"}
	}
}

func promptBlocks(turn driver.Turn) []protocol.ContentBlock {
	var task strings.Builder
	task.WriteString("<looprig-system>")
	task.WriteString(turn.SystemPrompt)
	task.WriteString("</looprig-system>\n\n<user-task>")
	for _, block := range turn.Input {
		switch typed := block.(type) {
		case *content.TextBlock:
			task.WriteString(typed.Text)
		case *content.DocumentBlock:
			task.WriteString(typed.Text)
		}
	}
	task.WriteString("</user-task>")
	return []protocol.ContentBlock{{Text: &protocol.TextContent{Text: task.String()}}}
}

var (
	_ driver.Agent         = (*Driver)(nil)
	_ driver.Steerer       = (*Driver)(nil)
	_ driver.Stream        = (*stream)(nil)
	_ driver.OrderedStream = (*orderedStream)(nil)
)
