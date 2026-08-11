package sessionruntime

import (
	"context"
	"sync"
)

// checkpointAdmissionGate is a writer-preferring, context-aware session barrier.
// Loop inference steps hold a read admission; required/manual checkpoints hold the
// writer admission. Once a writer waits, no new step enters until it releases.
type checkpointAdmissionGate struct {
	mu      sync.Mutex
	readers int
	writer  bool
	waiting int
	fault   error
	changed chan struct{}
}

func newCheckpointAdmissionGate() *checkpointAdmissionGate {
	return &checkpointAdmissionGate{changed: make(chan struct{})}
}

func (g *checkpointAdmissionGate) enterExecution(ctx context.Context) (func(), error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		g.mu.Lock()
		if g.fault != nil {
			err := g.fault
			g.mu.Unlock()
			return nil, err
		}
		if !g.writer && g.waiting == 0 {
			g.readers++
			g.mu.Unlock()
			var once sync.Once
			return func() { once.Do(g.leaveExecution) }, nil
		}
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (g *checkpointAdmissionGate) latch(err error) {
	g.mu.Lock()
	g.fault = err
	g.notifyLocked()
	g.mu.Unlock()
}

func (g *checkpointAdmissionGate) recover() {
	g.mu.Lock()
	g.fault = nil
	g.notifyLocked()
	g.mu.Unlock()
}

func (g *checkpointAdmissionGate) leaveExecution() {
	g.mu.Lock()
	g.readers--
	g.notifyLocked()
	g.mu.Unlock()
}

func (g *checkpointAdmissionGate) enterCheckpoint(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	g.mu.Lock()
	g.waiting++
	g.notifyLocked()
	g.mu.Unlock()
	for {
		if err := ctx.Err(); err != nil {
			g.mu.Lock()
			g.waiting--
			g.notifyLocked()
			g.mu.Unlock()
			return nil, err
		}
		g.mu.Lock()
		if !g.writer && g.readers == 0 {
			g.waiting--
			g.writer = true
			g.mu.Unlock()
			var once sync.Once
			return func() { once.Do(g.leaveCheckpoint) }, nil
		}
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			g.mu.Lock()
			g.waiting--
			g.notifyLocked()
			g.mu.Unlock()
			return nil, ctx.Err()
		}
	}
}

func (g *checkpointAdmissionGate) leaveCheckpoint() {
	g.mu.Lock()
	g.writer = false
	g.notifyLocked()
	g.mu.Unlock()
}

func (g *checkpointAdmissionGate) notifyLocked() {
	close(g.changed)
	g.changed = make(chan struct{})
}
