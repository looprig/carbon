package acp

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/looprig/acp/client"
)

// steerTerminalGrace is an internal lifecycle bound. It is deliberately not
// configurable by a model-facing request: a terminal prompt must not wait
// forever for a broken adapter's steering response.
const steerTerminalGrace = 100 * time.Millisecond

type steerJob struct {
	id      uint64
	ctx     context.Context
	params  client.SteerParams
	attempt *steerAttempt
}

type dispatcherInput struct {
	job      *steerJob
	terminal bool
	stop     bool
}

type dispatcherEvent struct {
	id               uint64
	result           client.SteerResult
	err              error
	admission        steerAdmission
	late             bool
	terminalComplete bool
}

type legacySteerCompletion struct {
	result client.SteerResult
	err    error
}

type activeSteer struct {
	job steerJob

	handle      *client.SteerHandle
	admissionCh <-chan bool
	resultCh    <-chan client.SteerCompletion
	legacyCh    <-chan legacySteerCompletion

	admission steerAdmission
	resolved  bool
	terminal  bool
	timer     *time.Timer
}

// steerAdmission is deliberately richer than a boolean. Pending means the
// command was accepted by the actor but has not reached the dispatcher;
// admissionPending means StartSteer has been invoked but ACP has not published
// its writer fact. Only admitted is proof that the request crossed Writer.
type steerAdmission uint8

const (
	steerAdmissionPending steerAdmission = iota
	steerAdmissionNotAdmitted
	steerAdmissionPendingWriter
	steerAdmissionAdmitted
)

func (a steerAdmission) admitted() bool { return a == steerAdmissionAdmitted }

func (a steerAdmission) String() string {
	switch a {
	case steerAdmissionPending:
		return "pending"
	case steerAdmissionNotAdmitted:
		return "not_admitted"
	case steerAdmissionPendingWriter:
		return "admission_pending"
	case steerAdmissionAdmitted:
		return "admitted"
	default:
		return "unknown"
	}
}

// steerAttempt carries admission state independently of the caller context.
// cancelAndSnapshot and beginStart use the same mutex, so cancellation before
// StartSteer and invocation of StartSteer are one linearized decision.
type steerAttempt struct {
	mu        sync.Mutex
	admission steerAdmission
}

func (a *steerAttempt) beginStart() bool {
	if a == nil {
		return true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.admission != steerAdmissionPending {
		return false
	}
	a.admission = steerAdmissionPendingWriter
	return true
}

func (a *steerAttempt) markAdmission(admitted bool) {
	if a == nil {
		return
	}
	a.mu.Lock()
	if a.admission != steerAdmissionNotAdmitted {
		if admitted {
			a.admission = steerAdmissionAdmitted
		} else {
			a.admission = steerAdmissionNotAdmitted
		}
	}
	a.mu.Unlock()
}

// cancelAndSnapshot seals an unstarted attempt as not admitted. If StartSteer
// already won beginStart, it leaves that pending writer state untouched so a
// caller cannot claim proven non-delivery from a race.
func (a *steerAttempt) cancelAndSnapshot() steerAdmission {
	if a == nil {
		return steerAdmissionPending
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.admission == steerAdmissionPending {
		a.admission = steerAdmissionNotAdmitted
	}
	return a.admission
}

func (a *steerAttempt) snapshot() steerAdmission {
	if a == nil {
		return steerAdmissionPending
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.admission
}

// steerDispatcher owns all provider steering calls for one turn. Its input
// mailbox is FIFO and the run loop starts at most one provider call at a time.
// The arbiter owns classification and publication; this component only
// transports typed Admission/Result facts and applies the bounded terminal
// resolution rule.
type steerDispatcher struct {
	ctx     context.Context
	session session

	inputs chan dispatcherInput
	events chan dispatcherEvent
	done   chan struct{}

	stopOnce sync.Once
}

func newSteerDispatcher(ctx context.Context, sess session) *steerDispatcher {
	if ctx == nil {
		ctx = context.Background()
	}
	d := &steerDispatcher{
		ctx:     ctx,
		session: sess,
		inputs:  make(chan dispatcherInput, 256),
		events:  make(chan dispatcherEvent, 256),
		done:    make(chan struct{}),
	}
	go d.run()
	return d
}

func (d *steerDispatcher) Events() <-chan dispatcherEvent {
	if d == nil {
		return nil
	}
	return d.events
}

func (d *steerDispatcher) submit(job steerJob) bool {
	if d == nil {
		return false
	}
	select {
	case d.inputs <- dispatcherInput{job: &job}:
		return true
	case <-d.done:
		return false
	}
}

func (d *steerDispatcher) resolveTerminal() bool {
	if d == nil {
		return false
	}
	select {
	case d.inputs <- dispatcherInput{terminal: true}:
		return true
	case <-d.done:
		return false
	}
}

func (d *steerDispatcher) stop() {
	if d == nil {
		return
	}
	// The input mailbox remains open. A stop command lets the dispatcher drain
	// its own state and close the read-only event channel without a send/close
	// race with a late arbiter handle.
	d.stopOnce.Do(func() {
		select {
		case d.inputs <- dispatcherInput{stop: true}:
		case <-d.done:
		}
	})
	<-d.done
}

func (d *steerDispatcher) run() {
	defer close(d.events)
	defer close(d.done)

	var queue []steerJob
	var active *activeSteer
	terminalRequested := false
	terminalCompleteSent := false

	emit := func(event dispatcherEvent) {
		select {
		case d.events <- event:
		case <-d.done:
		}
	}
	resolveQueued := func() {
		for len(queue) > 0 {
			job := queue[0]
			queue = queue[1:]
			if job.attempt != nil {
				job.attempt.markAdmission(false)
			}
			emit(dispatcherEvent{
				id:        job.id,
				result:    client.SteerResult{},
				err:       contextError(job.ctx, errors.New("acp: steering canceled before admission")),
				admission: steerAdmissionNotAdmitted,
			})
		}
	}

	for {
		if active == nil && !terminalRequested && len(queue) > 0 {
			job := queue[0]
			queue = queue[1:]
			if job.ctx != nil && job.ctx.Err() != nil {
				if job.attempt != nil {
					job.attempt.cancelAndSnapshot()
				}
				admission := steerAdmissionNotAdmitted
				if job.attempt != nil {
					admission = job.attempt.snapshot()
				}
				emit(dispatcherEvent{
					id:        job.id,
					result:    client.SteerResult{},
					err:       job.ctx.Err(),
					admission: admission,
				})
				continue
			}
			active = d.start(job)
			if active == nil {
				continue
			}
		}

		var admissionCh <-chan bool
		var resultCh <-chan client.SteerCompletion
		var legacyCh <-chan legacySteerCompletion
		var terminalTimer <-chan time.Time
		if active != nil {
			if active.admission == steerAdmissionPendingWriter && !active.resolved {
				admissionCh = active.admissionCh
			}
			if active.resultCh != nil || active.legacyCh != nil {
				resultCh = active.resultCh
				legacyCh = active.legacyCh
			}
			if active.terminal && !active.resolved && active.timer != nil {
				terminalTimer = active.timer.C
			}
		}

		if terminalRequested && (active == nil || active.resolved) && len(queue) == 0 && !terminalCompleteSent {
			emit(dispatcherEvent{terminalComplete: true})
			terminalCompleteSent = true
		}
		if terminalRequested && terminalCompleteSent && active == nil {
			// Keep the dispatcher alive until the arbiter stops it. This permits a
			// late provider event to be observed and explicitly ignored rather than
			// accidentally starting another queued call.
		}

		select {
		case input := <-d.inputs:
			if input.stop {
				if active != nil && active.handle != nil {
					active.handle.Cancel()
				}
				return
			}
			if input.job != nil {
				if terminalRequested {
					job := *input.job
					if job.attempt != nil {
						job.attempt.markAdmission(false)
					}
					emit(dispatcherEvent{
						id:        job.id,
						result:    client.SteerResult{},
						err:       errors.New("acp: steering canceled at terminal"),
						admission: steerAdmissionNotAdmitted,
					})
				} else {
					queue = append(queue, *input.job)
				}
				continue
			}
			if input.terminal {
				terminalRequested = true
				resolveQueued()
				if active != nil {
					active.terminal = true
					if active.admission != steerAdmissionPendingWriter {
						d.resolveTerminalActive(active, emit)
					} else {
						active.timer = time.NewTimer(steerTerminalGrace)
					}
				}
			}

		case admitted, ok := <-admissionCh:
			if active == nil {
				continue
			}
			active.admission = admissionFromFact(ok && admitted)
			active.admissionCh = nil
			if active.job.attempt != nil {
				active.job.attempt.markAdmission(ok && admitted)
			}
			if active.terminal && !active.resolved {
				d.resolveTerminalActive(active, emit)
			}

		case completion, ok := <-resultCh:
			if active == nil {
				continue
			}
			d.resolveAdmission(active)
			if !ok {
				completion = client.SteerCompletion{Result: client.SteerResult{WriteAdmitted: active.admission.admitted()}, Err: errors.New("acp: steering completion unavailable")}
			}
			late := active.resolved || (active.job.ctx != nil && active.job.ctx.Err() != nil)
			d.finishActive(active, completion.Result, completion.Err, emit, late)
			active.resultCh = nil
			active = nil

		case completion, ok := <-legacyCh:
			if active == nil {
				continue
			}
			if !ok {
				completion = legacySteerCompletion{result: client.SteerResult{WriteAdmitted: active.admission.admitted()}, err: errors.New("acp: steering completion unavailable")}
			}
			d.resolveAdmission(active)
			late := active.resolved || (active.job.ctx != nil && active.job.ctx.Err() != nil)
			d.finishActive(active, completion.result, completion.err, emit, late)
			active.legacyCh = nil
			active = nil

		case <-terminalTimer:
			if active == nil || active.resolved {
				continue
			}
			// No admission fact arrived within the lifecycle bound. The started
			// attempt may have crossed Writer, but that remains unresolved; the
			// arbiter must not manufacture a WriteAdmitted fact here.
			d.resolveTerminalActive(active, emit)
		}
	}
}

func (d *steerDispatcher) start(job steerJob) *activeSteer {
	admission := steerAdmissionPendingWriter
	if job.attempt != nil {
		if !job.attempt.beginStart() {
			admission = job.attempt.snapshot()
			select {
			case d.events <- dispatcherEvent{
				id:        job.id,
				err:       contextError(job.ctx, errors.New("acp: steering canceled before start")),
				admission: admission,
			}:
			case <-d.done:
			}
			return nil
		}
	}
	active := &activeSteer{job: job, admission: admission}
	if async, ok := d.session.(asyncSteerSession); ok {
		handle := async.StartSteer(d.ctx, job.params)
		if handle == nil {
			if job.attempt != nil {
				job.attempt.markAdmission(false)
			}
			select {
			case d.events <- dispatcherEvent{id: job.id, err: errors.New("acp: steering handle unavailable"), admission: steerAdmissionNotAdmitted}:
			case <-d.done:
			}
			return nil
		}
		active.handle = handle
		active.admissionCh = handle.Admission()
		active.resultCh = handle.Result()
		return active
	}
	if syncSession, ok := d.session.(steerSession); ok {
		resultCh := make(chan legacySteerCompletion, 1)
		active.legacyCh = resultCh
		go func() {
			result, err := syncSession.Steer(d.ctx, job.params)
			resultCh <- legacySteerCompletion{result: result, err: err}
		}()
		return active
	}
	select {
	case d.events <- dispatcherEvent{id: job.id, err: errors.New("acp: steering capability unavailable"), admission: steerAdmissionNotAdmitted}:
	case <-d.done:
	}
	if job.attempt != nil {
		job.attempt.markAdmission(false)
	}
	return nil
}

func (d *steerDispatcher) finishActive(active *activeSteer, result client.SteerResult, err error, emit func(dispatcherEvent), late bool) {
	if active == nil {
		return
	}
	if result.WriteAdmitted {
		active.admission = steerAdmissionAdmitted
		if active.job.attempt != nil {
			active.job.attempt.markAdmission(true)
		}
	} else if active.admission == steerAdmissionAdmitted {
		// The separate admission channel is authoritative when it already
		// reported true. Preserve that fact if the completion omitted it.
		result.WriteAdmitted = true
	} else if active.admission == steerAdmissionPendingWriter {
		// A completion carrying an explicit false fact is proof that Writer
		// rejected the call, even when its separate admission notification has
		// not won the dispatcher select yet.
		active.admission = steerAdmissionNotAdmitted
		if active.job.attempt != nil {
			active.job.attempt.markAdmission(false)
		}
	} else if active.admission == steerAdmissionNotAdmitted {
		result.WriteAdmitted = false
	}
	if late {
		emit(dispatcherEvent{
			id:        active.job.id,
			result:    result,
			err:       err,
			admission: active.admission,
			late:      true,
		})
		return
	}
	emit(dispatcherEvent{
		id:        active.job.id,
		result:    result,
		err:       err,
		admission: active.admission,
	})
}

func (d *steerDispatcher) resolveTerminalActive(active *activeSteer, emit func(dispatcherEvent)) {
	if active == nil || active.resolved {
		return
	}
	active.resolved = true
	if active.timer != nil {
		active.timer.Stop()
	}
	if active.handle != nil {
		active.handle.Cancel()
	}
	if active.job.attempt != nil && active.admission != steerAdmissionPendingWriter {
		active.job.attempt.markAdmission(active.admission.admitted())
	}
	err := errors.New("acp: steering response unavailable")
	emit(dispatcherEvent{
		id:        active.job.id,
		result:    client.SteerResult{WriteAdmitted: active.admission.admitted()},
		err:       err,
		admission: active.admission,
	})
}

// resolveAdmission drains the ordered Admission signal before consuming a
// completion. StartSteer publishes admission before Result, but both channels
// are independently selectable by this dispatcher; draining here prevents a
// known pre-admission rejection from being misclassified as unresolved.
func (d *steerDispatcher) resolveAdmission(active *activeSteer) {
	if active == nil || active.admission != steerAdmissionPendingWriter || active.admissionCh == nil {
		return
	}
	select {
	case admitted, ok := <-active.admissionCh:
		active.admission = admissionFromFact(ok && admitted)
		active.admissionCh = nil
		if active.job.attempt != nil {
			active.job.attempt.markAdmission(ok && admitted)
		}
	default:
	}
}

func admissionFromFact(admitted bool) steerAdmission {
	if admitted {
		return steerAdmissionAdmitted
	}
	return steerAdmissionNotAdmitted
}

func contextError(ctx context.Context, fallback error) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return fallback
}
