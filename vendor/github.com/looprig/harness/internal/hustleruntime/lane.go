package hustleruntime

import (
	"sync"

	"github.com/looprig/harness/pkg/hustle"
)

type runState uint8

const (
	runQueued runState = iota
	runExecuting
	runFinalizing
	runDone
)

type lane struct {
	mu sync.Mutex

	participation hustle.Participation
	concurrent    int
	capacity      int
	closed        bool
	owned         int
	executing     int
	nextSequence  uint64
	queue         []*ownedRun
}

func newLane(participation hustle.Participation, limits LaneLimits) *lane {
	return &lane{
		participation: participation,
		concurrent:    limits.Concurrent,
		capacity:      limits.Concurrent + limits.Queued,
	}
}

func (l *lane) enqueue(run *ownedRun) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return &AdmissionError{Reason: AdmissionClosed, Participation: l.participation}
	}
	if l.owned >= l.capacity {
		return &AdmissionError{Reason: AdmissionFull, Participation: l.participation}
	}
	l.nextSequence++
	run.sequence = l.nextSequence
	run.state = runQueued
	l.owned++
	l.queue = append(l.queue, run)
	l.grantLocked()
	return nil
}

func (l *lane) grantLocked() {
	if l.closed {
		return
	}
	for l.executing < l.concurrent && len(l.queue) > 0 {
		run := l.queue[0]
		if !run.eligible {
			return
		}
		l.queue = l.queue[1:]
		if run.state != runQueued {
			continue
		}
		run.state = runExecuting
		l.executing++
		close(run.granted)
	}
}

func (l *lane) makeEligible(run *ownedRun) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || run.state != runQueued || run.callerCtx.Err() != nil || run.controller.sessionCtx.Err() != nil {
		return false
	}
	run.eligible = true
	l.grantLocked()
	return true
}

func (l *lane) cancelQueued(run *ownedRun) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if run.state != runQueued {
		return false
	}
	for index, queued := range l.queue {
		if queued == run {
			l.queue = append(l.queue[:index], l.queue[index+1:]...)
			break
		}
	}
	run.state = runFinalizing
	l.grantLocked()
	return true
}

func (l *lane) beginFinalizing(run *ownedRun) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if run.state != runExecuting {
		return &RunStateError{RunID: run.id, State: run.state}
	}
	run.state = runFinalizing
	l.executing--
	l.grantLocked()
	return nil
}

func (l *lane) complete(run *ownedRun) {
	l.mu.Lock()
	if run.state == runFinalizing {
		run.state = runDone
		l.owned--
	}
	l.mu.Unlock()
}

func (l *lane) closeQueued() []*ownedRun {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	queued := append([]*ownedRun(nil), l.queue...)
	l.queue = nil
	for _, run := range queued {
		if run.state == runQueued {
			run.state = runFinalizing
		}
	}
	return queued
}

func (l *lane) stateOf(run *ownedRun) runState {
	l.mu.Lock()
	defer l.mu.Unlock()
	return run.state
}
