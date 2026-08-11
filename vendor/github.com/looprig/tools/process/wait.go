package process

import (
	"context"
	"errors"
	"sync"
)

// WaitKind selects how Supervisor.Wait observes a set of process handles
// (spec "ProcessOutput API": `"wait": "poll | any | all"`). This is Task
// 9A's generic waiter primitive: the future ProcessOutput tool (Task 16)
// translates its own `wait`/`cursor` arguments into a WaitKind and a set of
// WaitTarget values and calls Wait through the Supervisor; 9A does not
// build ProcessOutput itself.
type WaitKind string

// The closed set of wait kinds.
const (
	// WaitPoll returns every target's current status immediately, never
	// blocking and never consulting ctx.
	WaitPoll WaitKind = "poll"
	// WaitAny blocks until at least one target has advanced past its
	// supplied generation or become terminal, or ctx is done.
	WaitAny WaitKind = "any"
	// WaitAll blocks until every target has advanced past its supplied
	// generation or become terminal, or ctx is done.
	WaitAll WaitKind = "all"
)

// Valid reports whether k belongs to the closed WaitKind domain.
func (k WaitKind) Valid() bool {
	switch k {
	case WaitPoll, WaitAny, WaitAll:
		return true
	default:
		return false
	}
}

// WaitTarget is one process a Wait call watches, paired with the
// generation the caller last observed for it. Generation is this
// package's own append-driven counter (entry.go's entry.generation), not a
// byte cursor: a caller with a byte cursor (Task 16's ProcessOutput)
// derives "have I already seen everything as of this cursor" itself, from
// a WaitStatus's Generation together with its own last-read output cursor,
// and only needs Wait to tell it "something changed, go look again" versus
// "nothing changed yet". The zero value (a target never observed before)
// blocks until the entry has appended at least once or is already
// terminal.
type WaitTarget struct {
	Handle     Handle
	Generation uint64
}

// WaitStatus is Wait's per-target report.
type WaitStatus struct {
	Handle Handle
	// Generation is the entry's current generation counter.
	Generation uint64
	// Terminal reports whether the entry has reached a terminal state.
	Terminal bool
	// Found reports whether Handle named a live entry visible to the
	// calling Owner. A Handle that does not exist and a Handle owned by a
	// different Owner are deliberately indistinguishable here -- both
	// report Found false and every other field at its zero value -- so a
	// cross-owner probe can never be told apart from a missing one (spec
	// "Identity and authorization"; mirrors Owner.Equal's doc comment).
	Found bool
}

// Wait reports (poll) or waits for (any/all) new output or a terminal
// transition across targets, scoped to entries owner can see.
//
// WaitPoll returns the current WaitStatus for every target immediately: it
// never blocks and never even inspects ctx (TestWaitPollReturnsImmediately).
//
// WaitAny and WaitAll block until their respective condition is satisfied
// or ctx.Done() fires, returning ctx.Err() on the latter
// (TestWaitCancelRemovesWaiter). Neither mode polls or sleeps to notice a
// change: both are woken directly by appendChunk's generation bump
// (entry.go's bumpGeneration) or by an entry's done channel closing at its
// terminal transition (TestWaitAnyWakesOnAppend, TestWaitAllRequiresEveryEntry).
//
// A blocking call (any/all) first reserves one waiter slot against
// cfg.MaxPendingWaiters for owner.SessionID (acquireWaiterSlot), releasing
// it unconditionally on every return path -- including cancellation, so a
// canceled wait never leaks its slot (TestWaitCancelRemovesWaiter). If the
// quota is already exhausted, Wait returns a *Error wrapping
// CodeOutputQuotaExceeded immediately, before registering any watcher.
// Poll-mode calls never touch the quota: they neither reserve nor require
// a slot.
func (s *Supervisor) Wait(ctx context.Context, owner Owner, kind WaitKind, targets []WaitTarget) ([]WaitStatus, error) {
	if !kind.Valid() {
		return nil, Wrap(CodeInvalidArguments, errors.New("unrecognized wait kind"))
	}
	if len(targets) == 0 {
		return nil, Wrap(CodeInvalidArguments, errors.New("at least one target handle is required"))
	}

	if kind == WaitPoll {
		statuses, _ := s.snapshotTargets(owner, targets)
		return statuses, nil
	}

	release, err := s.acquireWaiterSlot(owner)
	if err != nil {
		return nil, err
	}
	defer release()

	return s.blockingWait(ctx, owner, kind, targets)
}

// blockingWait implements WaitAny/WaitAll: it re-snapshots every target,
// returns as soon as the kind's condition is met, and otherwise blocks
// (waitForChange) until something could have changed before checking
// again. Every loop iteration's blocking step is driven entirely by
// entry.go's generation/done wake mechanism -- there is no sleep or poll
// interval anywhere in this loop.
func (s *Supervisor) blockingWait(ctx context.Context, owner Owner, kind WaitKind, targets []WaitTarget) ([]WaitStatus, error) {
	for {
		statuses, watches := s.snapshotTargets(owner, targets)

		satisfied := 0
		for _, w := range watches {
			if w.satisfied {
				satisfied++
			}
		}

		switch kind {
		case WaitAny:
			if satisfied > 0 {
				return statuses, nil
			}
		case WaitAll:
			if satisfied == len(watches) {
				return statuses, nil
			}
		}

		if err := waitForChange(ctx, watches); err != nil {
			return nil, err
		}
	}
}

// targetWatch is snapshotTargets' internal per-target result: whether this
// target already satisfies "advanced past its last-observed generation or
// terminal" (satisfied), and -- when it does not -- the exact wake/done
// channels waitForChange should select on for it.
type targetWatch struct {
	satisfied bool
	wake      <-chan struct{}
	done      <-chan struct{}
}

// snapshotTargets resolves every target against s.entries under owner's
// authorization and returns both the caller-facing WaitStatus slice and
// the internal targetWatch slice blockingWait evaluates. A missing or
// cross-owner handle reports Found false in its WaitStatus and satisfied
// true in its targetWatch: it can never itself advance, so it must never
// block an any/all wait forever.
//
// Each target's (generation, wake) pair is read together, atomically, via
// entry.generationSnapshot -- this is what makes the "already satisfied"
// decision and the channel waitForChange later selects on consistent with
// each other, with no lost-wakeup window (see entry.go's wake doc
// comment).
func (s *Supervisor) snapshotTargets(owner Owner, targets []WaitTarget) ([]WaitStatus, []targetWatch) {
	statuses := make([]WaitStatus, len(targets))
	watches := make([]targetWatch, len(targets))

	for i, target := range targets {
		s.mu.Lock()
		e, ok := s.entries[target.Handle]
		s.mu.Unlock()

		if !ok || !e.identity.Owner.Equal(owner) {
			statuses[i] = WaitStatus{Handle: target.Handle}
			watches[i] = targetWatch{satisfied: true}
			continue
		}

		generation, wake := e.generationSnapshot()
		terminal := closed(e.done)

		statuses[i] = WaitStatus{
			Handle:     target.Handle,
			Generation: generation,
			Terminal:   terminal,
			Found:      true,
		}
		watches[i] = targetWatch{
			satisfied: terminal || generation > target.Generation,
			wake:      wake,
			done:      e.done,
		}
	}

	return statuses, watches
}

// closed reports whether ch is already closed, without blocking.
func closed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// waitForChange blocks until any not-yet-satisfied watch's wake or done
// channel fires, or ctx is done, whichever happens first. It spawns one
// goroutine per not-yet-satisfied watch (a satisfied watch needs no
// watching), fans every one of them into a single shared changed signal,
// and unconditionally tears every one of them down -- via the stop channel
// and a WaitGroup -- before returning on any path, including cancellation,
// so a canceled or otherwise-returned wait leaks no goroutine
// (TestWaitCancelRemovesWaiter).
func waitForChange(ctx context.Context, watches []targetWatch) error {
	stop := make(chan struct{})
	changed := make(chan struct{}, 1)

	var wg sync.WaitGroup
	for _, w := range watches {
		if w.satisfied {
			continue
		}
		wg.Add(1)
		go func(w targetWatch) {
			defer wg.Done()
			select {
			case <-w.wake:
			case <-w.done:
			case <-stop:
				return
			}
			select {
			case changed <- struct{}{}:
			default:
			}
		}(w)
	}

	var err error
	select {
	case <-ctx.Done():
		err = ctx.Err()
	case <-changed:
	}

	close(stop)
	wg.Wait()

	return err
}

// acquireWaiterSlot reserves one blocking-wait slot for owner.SessionID
// against cfg.MaxPendingWaiters (spec "Quotas and retention": "maximum
// pending waiters"; Config.MaxPendingWaiters's doc comment: "the
// outstanding ProcessOutput wait: any|all waiters a session admits
// concurrently"). It rejects immediately, before reserving anything or
// registering any watcher, once the session is already at quota --
// mirroring reserveQuota's own reject-before-mutate discipline
// (supervisor.go). The returned release func is safe to call exactly once
// and must always be called, on every return path of the caller, to keep
// pendingWaitersBySession an exact mirror of currently-blocked waiters.
func (s *Supervisor) acquireWaiterSlot(owner Owner) (release func(), err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pendingWaitersBySession[owner.SessionID] >= s.cfg.MaxPendingWaiters {
		return nil, Wrap(CodeOutputQuotaExceeded, errors.New("max pending waiters reached"))
	}
	s.pendingWaitersBySession[owner.SessionID]++
	return func() { s.releaseWaiterSlot(owner) }, nil
}

// releaseWaiterSlot exactly reverses one acquireWaiterSlot reservation.
func (s *Supervisor) releaseWaiterSlot(owner Owner) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pendingWaitersBySession[owner.SessionID] > 0 {
		s.pendingWaitersBySession[owner.SessionID]--
	}
}
