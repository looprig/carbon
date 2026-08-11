package presentation

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// handoffFinalizeTimeout bounds runtime teardown while a /clear handoff is still in flight.
// Replacement construction receives the application-lifetime context (see reopenAgent), so
// this is only the finalizer's wait budget; it is not passed into the replacement session.
const handoffFinalizeTimeout = closeTimeout + 5*time.Second + time.Second

// reopenHandoff coordinates ownership between the asynchronous /clear command, the Update
// loop, and runtime.Run. Exactly one consumer claims the completed result: Update installs it, or
// runtime finalization closes an unconsumed replacement before the shared store can close.
type reopenHandoff struct {
	done chan struct{}

	mu        sync.Mutex
	result    reopenResultMsg
	claimed   bool
	completed bool
	abandoned bool
}

// agentCloseHandoff coordinates the final replacement close for ctrl+c deferred through a
// successful /clear. The Bubble Tea command and runtime.Run finalizer may race to invoke close;
// sync.Once makes the agent close exactly once and both wait for the same result.
type agentCloseHandoff struct {
	agent Agent
	done  chan struct{}
	once  sync.Once
	err   error
}

// staleReopenClose binds rejected-replacement ownership to the session generation
// whose restore barrier rejected it.
type staleReopenClose struct {
	handoff    *agentCloseHandoff
	generation uint64
}

func newAgentCloseHandoff(agent Agent) *agentCloseHandoff {
	return &agentCloseHandoff{agent: agent, done: make(chan struct{})}
}

func (h *agentCloseHandoff) close() error {
	h.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
		h.err = h.agent.Close(ctx)
		cancel()
		close(h.done)
	})
	<-h.done
	return h.err
}

func newReopenHandoff() *reopenHandoff { return &reopenHandoff{done: make(chan struct{})} }

func (h *reopenHandoff) complete(result reopenResultMsg) {
	h.mu.Lock()
	h.result = result
	h.completed = true
	abandoned := h.abandoned
	if abandoned {
		h.claimed = true
	}
	h.mu.Unlock()
	if abandoned && result.agent != nil {
		ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
		_ = result.agent.Close(ctx)
		cancel()
	}
	close(h.done)
}

func (h *reopenHandoff) claim() {
	h.mu.Lock()
	h.claimed = true
	h.mu.Unlock()
}

// finalize waits a bounded interval for the command to stop using the opener/store. OpenAgent
// is required to honor cancellation. On timeout the handoff is abandoned: complete remains
// responsible for closing any replacement that arrives late. If Update did not claim a timely
// result, finalize closes the replacement and returns the handoff diagnostic.
func (h *reopenHandoff) finalize() error {
	timer := time.NewTimer(handoffFinalizeTimeout)
	defer timer.Stop()
	select {
	case <-h.done:
	case <-timer.C:
		h.mu.Lock()
		if !h.completed {
			h.abandoned = true
			h.mu.Unlock()
			return fmt.Errorf("finalize clear handoff: timed out after %s", handoffFinalizeTimeout)
		}
		h.mu.Unlock()
	}
	h.mu.Lock()
	if h.claimed {
		h.mu.Unlock()
		return nil
	}
	h.claimed = true
	result := h.result
	h.mu.Unlock()

	err := result.err
	if result.agent != nil {
		ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
		closeErr := result.agent.Close(ctx)
		cancel()
		if closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close unclaimed replacement: %w", closeErr))
		}
	}
	return err
}
