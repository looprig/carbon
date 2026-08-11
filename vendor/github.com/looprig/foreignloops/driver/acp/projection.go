package acp

import (
	"context"
	"sync"

	"github.com/looprig/foreignloops/driver"
)

// projectionBufferDepth is both the selected public channel depth and the
// maximum number of commands the owner may retain after that channel fills.
// Once output and pending storage are full, producers block at commands
// rather than allowing an unbounded pending slice to grow.
const projectionBufferDepth = 4096

// projection is the sole owner of a stream's public channels. Producers never
// send to, or close, Events/Observations directly. The command mailbox is
// intentionally never closed: a stale turn handle can attempt a send after
// the arbiter has retired, and the done case makes that attempt harmless.
type projection struct {
	events       chan driver.Event
	observations chan driver.Observation
	commands     chan projectionCommand
	done         chan struct{}
	abort        chan struct{}
	stop         chan struct{}
	commandMu    sync.Mutex
	commandSpace chan struct{}
	closed       bool

	selectOnce sync.Once
	stopOnce   sync.Once
	abortOnce  sync.Once
}

type projectionCommand struct {
	selectView  streamView
	event       *driver.Event
	observation driver.Observation
	ack         chan struct{}
	stop        bool
}

func newProjection() *projection {
	p := &projection{
		events:       make(chan driver.Event, projectionBufferDepth),
		observations: make(chan driver.Observation, projectionBufferDepth),
		commands:     make(chan projectionCommand, 128),
		done:         make(chan struct{}),
		abort:        make(chan struct{}),
		stop:         make(chan struct{}),
		commandSpace: make(chan struct{}),
	}
	go p.run()
	return p
}

func (p *projection) stopOn(ctx context.Context) {
	if p == nil || ctx == nil {
		return
	}
	go func() {
		select {
		case <-ctx.Done():
			p.abortOwner()
		case <-p.done:
		}
	}()
}

func (p *projection) eventsView() <-chan driver.Event {
	if p == nil {
		return nil
	}
	p.selectView(viewEvents)
	return p.events
}

func (p *projection) observationsView() <-chan driver.Observation {
	if p == nil {
		return nil
	}
	p.selectView(viewObservations)
	return p.observations
}

func (p *projection) selectView(view streamView) {
	if p == nil || (view != viewEvents && view != viewObservations) {
		return
	}
	p.selectOnce.Do(func() {
		ack := make(chan struct{})
		if p.enqueue(projectionCommand{selectView: view, ack: ack}) {
			select {
			case <-ack:
			case <-p.done:
			case <-p.abort:
			}
		}
	})
}

func (p *projection) emitEvent(event driver.Event) {
	if p == nil {
		return
	}
	copyEvent := event
	p.enqueue(projectionCommand{event: &copyEvent})
}

func (p *projection) emitObservation(observation driver.Observation) <-chan struct{} {
	ack := make(chan struct{})
	if p == nil || observation == nil {
		close(ack)
		return ack
	}
	if !p.enqueue(projectionCommand{observation: observation, ack: ack}) {
		close(ack)
	}
	return ack
}

func (p *projection) send(command projectionCommand) {
	p.enqueue(command)
}

func (p *projection) close() {
	if p == nil {
		return
	}
	p.stopOnce.Do(func() {
		// Mark the producer side closed first, then ask the owner to drain
		// already-admitted commands. If the selected consumer is stalled, the
		// owner detects a full output/pending bound and exits without waiting
		// forever; abortOwner remains the hard cancellation path.
		p.markClosed()
		close(p.stop)
	})
	<-p.done
}

// enqueue admits one producer command while the projection is live. A full
// mailbox waits on a generation channel that the owner advances after each
// receive; Close/abort closes that generation and marks the projection closed
// under the same mutex, so no producer can enqueue after teardown begins.
func (p *projection) enqueue(command projectionCommand) bool {
	if p == nil {
		return false
	}
	for {
		var space <-chan struct{}
		p.commandMu.Lock()
		if p.closed {
			p.commandMu.Unlock()
			return false
		}
		select {
		case p.commands <- command:
			p.commandMu.Unlock()
			return true
		default:
			space = p.commandSpace
			p.commandMu.Unlock()
		}
		select {
		case <-space:
		case <-p.done:
			return false
		case <-p.abort:
			return false
		}
	}
}

func (p *projection) commandConsumed() {
	p.commandMu.Lock()
	if p.closed {
		p.commandMu.Unlock()
		return
	}
	close(p.commandSpace)
	p.commandSpace = make(chan struct{})
	p.commandMu.Unlock()
}

func (p *projection) markClosed() {
	p.commandMu.Lock()
	if !p.closed {
		p.closed = true
		close(p.commandSpace)
	}
	p.commandMu.Unlock()
}

func (p *projection) run() {
	selected := viewUnselected
	closedEvents := false
	closedObservations := false
	stopping := false
	var stopCh <-chan struct{} = p.stop
	pending := make([]projectionCommand, 0, 32)
	defer func() {
		p.markClosed()
		for {
			select {
			case command := <-p.commands:
				closeProjectionAck(command.ack)
			default:
				goto drained
			}
		}
	drained:
		for _, command := range pending {
			closeProjectionAck(command.ack)
		}
		// The owner is the only goroutine that closes either public channel.
		if !closedEvents {
			close(p.events)
		}
		if !closedObservations {
			close(p.observations)
		}
		close(p.done)
	}()

	for {
		var eventOut chan driver.Event
		var observationOut chan driver.Observation
		var commands <-chan projectionCommand
		var eventValue driver.Event
		var observationValue driver.Observation
		if len(pending) > 0 && selected != viewUnselected {
			command := pending[0]
			switch selected {
			case viewEvents:
				if command.event != nil {
					eventOut = p.events
					eventValue = *command.event
				}
			case viewObservations:
				if command.observation != nil {
					observationOut = p.observations
					observationValue = command.observation
				}
			}
			if eventOut == nil && observationOut == nil {
				closeProjectionAck(command.ack)
				pending = pending[1:]
				continue
			}
			if stopping {
				if eventOut != nil && len(eventOut) == cap(eventOut) {
					return
				}
				if observationOut != nil && len(observationOut) == cap(observationOut) {
					return
				}
			}
		} else if stopping && len(p.commands) == 0 {
			return
		}
		if len(pending) < projectionBufferDepth {
			commands = p.commands
		}

		select {
		case command := <-commands:
			p.commandConsumed()
			if command.stop {
				stopping = true
				continue
			}
			if command.selectView != viewUnselected && selected == viewUnselected {
				selected = command.selectView
				if selected == viewEvents {
					close(p.observations)
					closedObservations = true
				} else {
					close(p.events)
					closedEvents = true
				}
				closeProjectionAck(command.ack)
				continue
			}
			if command.event != nil || command.observation != nil {
				pending = append(pending, command)
			}
		case <-stopCh:
			stopping = true
			stopCh = nil
		case eventOut <- eventValue:
			closeProjectionAck(pending[0].ack)
			pending = pending[1:]
		case observationOut <- observationValue:
			closeProjectionAck(pending[0].ack)
			pending = pending[1:]
		case <-p.abort:
			return
		}
	}
}

func (p *projection) abortOwner() {
	if p == nil {
		return
	}
	p.markClosed()
	p.abortOnce.Do(func() { close(p.abort) })
}

func closeProjectionAck(ack chan struct{}) {
	if ack != nil {
		close(ack)
	}
}
