package sessionadapter

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/hub"
	"github.com/looprig/harness/pkg/journal"
)

const (
	replayRetryBase = 10 * time.Millisecond
	replayRetryMax  = time.Second
)

var (
	errDurableReplayUnavailable = errors.New("sessionadapter: durable event replay is unavailable")
	errReplayStopped            = errors.New("sessionadapter: replay subscription stopped")
	errNilLiveSubscription      = errors.New("sessionadapter: session returned a nil event subscription")
	errLiveSubscriptionClosed   = errors.New("sessionadapter: live event subscription closed")
)

type consumeResult uint8

const (
	consumeStopped consumeResult = iota
	consumeReconnect
	consumeTerminal
)

// replayingSubscriptionConfig contains the narrow seams needed by the recovery state
// machine. openReplay receives the inclusive journal sequence because the sessionstore
// replayer binds its starting position when it is constructed, not when EventCursor.Open
// is called.
type replayingSubscriptionConfig struct {
	ctx        context.Context
	sessionID  uuid.UUID
	filter     event.EventFilter
	initialSeq uint64

	subscribe  func(event.EventFilter) (event.Subscription, error)
	openReplay func(uint64) (journal.EventReplayer, error)
	foldGate   func(event.Event)
}

// replayingSubscription keeps the session's live fan-in attached while it repairs a
// durable gap. It never forwards replacement-live deliveries until the gap replay reaches
// EOF, so replayed journal order remains the prefix of the recovered stream. JournalSeq is
// the overlap key; ephemeral deliveries (sequence zero) remain live-only and are forwarded
// as they arrive after the durable prefix.
type replayingSubscription struct {
	ctx    context.Context
	cancel context.CancelFunc

	sessionID  uuid.UUID
	filter     event.EventFilter
	subscribe  func(event.EventFilter) (event.Subscription, error)
	openReplay func(uint64) (journal.EventReplayer, error)
	foldGate   func(event.Event)

	out      chan event.Delivery
	done     chan struct{}
	finished chan struct{}

	innerMu sync.Mutex
	inner   event.Subscription

	closeOnce sync.Once
	closeErr  error

	errMu sync.Mutex
	err   error
}

func newReplayingSubscription(cfg replayingSubscriptionConfig) (event.Subscription, error) {
	if cfg.openReplay == nil {
		return nil, errDurableReplayUnavailable
	}
	if cfg.subscribe == nil {
		return nil, errors.New("sessionadapter: durable replay requires a session subscription")
	}
	if cfg.ctx == nil {
		cfg.ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(cfg.ctx)
	s := &replayingSubscription{
		ctx:        ctx,
		cancel:     cancel,
		sessionID:  cfg.sessionID,
		filter:     cfg.filter,
		subscribe:  cfg.subscribe,
		openReplay: cfg.openReplay,
		foldGate:   cfg.foldGate,
		out:        make(chan event.Delivery),
		done:       make(chan struct{}),
		finished:   make(chan struct{}),
	}
	go s.run(cfg.initialSeq)
	return s, nil
}

func (s *replayingSubscription) Events() <-chan event.Delivery { return s.out }

// Close stops the recovery loop, closes the current inner subscription to unblock any
// receive/replay wait, and waits for the wrapper's output channel to close. The underlying
// event.Subscription contract makes Close idempotent, so a race with reconnect cleanup is
// safe even when both sides call it.
func (s *replayingSubscription) Close() error {
	s.closeOnce.Do(func() {
		close(s.done)
		s.cancel()

		s.innerMu.Lock()
		inner := s.inner
		s.inner = nil
		s.innerMu.Unlock()
		if inner != nil {
			s.closeErr = inner.Close()
		}
		<-s.finished
	})
	return s.closeErr
}

// Err reports only a terminal recovery error. A hub-forced loss is deliberately hidden
// when repair succeeds; otherwise the TUI would treat a recoverable overflow as a fatal
// stream end and discard the repaired subscription.
func (s *replayingSubscription) Err() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.err
}

func (s *replayingSubscription) run(lastSeq uint64) {
	defer close(s.finished)
	defer close(s.out)
	for {
		inner, ok := s.connect(&lastSeq)
		if !ok {
			s.recordContextError()
			return
		}
		switch s.consume(inner, &lastSeq) {
		case consumeStopped, consumeTerminal:
			_ = inner.Close()
			s.clearInner(inner)
			return
		case consumeReconnect:
			// The live subscription was lost for a recoverable Hub overflow. Its
			// buffered prefix has already been consumed; reconnect from lastSeq.
		}
		s.clearInner(inner)
		if s.stopped() {
			return
		}
	}
}

// connect establishes the replacement live subscription BEFORE opening the journal gap.
// This ordering makes appends during replay land in the replacement's bounded buffer; the
// subsequent live phase drops any overlap by sequence and forwards only newer deliveries.
func (s *replayingSubscription) connect(lastSeq *uint64) (event.Subscription, bool) {
	delay := time.Duration(replayRetryBase)
	for {
		if s.stopped() {
			return nil, false
		}
		inner, err := s.subscribe(s.filter)
		if err == nil && inner == nil {
			s.setErr(errNilLiveSubscription)
			return nil, false
		}
		if err == nil {
			if !s.installInner(inner) {
				_ = inner.Close()
				return nil, false
			}
			err = s.replayGap(lastSeq)
			if err == nil {
				return inner, true
			}
			s.clearInner(inner)
			_ = inner.Close()
			if errors.Is(err, errDurableReplayUnavailable) {
				s.setErr(err)
				return nil, false
			}
			if errors.Is(err, errReplayStopped) || s.stopped() {
				return nil, false
			}
			// A journal read/open/decode error is a durable integrity or
			// availability failure. Retrying it would hide a fail-secure error
			// behind an unbounded stream that can never make progress.
			s.setErr(err)
			return nil, false
		}
		if !isTemporaryError(err) {
			s.setErr(err)
			return nil, false
		}
		if !s.waitRetry(delay) {
			return nil, false
		}
		delay = nextReplayDelay(delay)
	}
}

func (s *replayingSubscription) replayGap(lastSeq *uint64) error {
	if *lastSeq == ^uint64(0) {
		return nil
	}
	start := *lastSeq + 1
	replayer, err := s.openReplay(start)
	if err != nil {
		return err
	}
	if replayer == nil {
		return errDurableReplayUnavailable
	}
	cursor, err := replayer.Open(s.ctx, journal.ReplayRequest{
		SessionID: s.sessionID,
		From:      journal.FromSeq(start),
	})
	if err != nil {
		return err
	}
	if cursor == nil {
		return errors.New("sessionadapter: replay returned a nil event cursor")
	}
	defer func() { _ = cursor.Close() }()

	for {
		ev, seq, err := cursor.Next(s.ctx)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if seq != 0 && seq <= *lastSeq {
			continue
		}
		if !s.deliver(event.Delivery{Event: ev, JournalSeq: seq}, lastSeq) {
			return errReplayStopped
		}
	}
}

func (s *replayingSubscription) consume(inner event.Subscription, lastSeq *uint64) consumeResult {
	for {
		select {
		case <-s.done:
			return consumeStopped
		case <-s.ctx.Done():
			return consumeStopped
		case delivery, ok := <-inner.Events():
			if !ok {
				if s.stopped() {
					return consumeStopped
				}
				if err := inner.Err(); err != nil {
					var loss *hub.SubscriptionLossError
					if errors.As(err, &loss) {
						return consumeReconnect
					}
					s.setErr(err)
					return consumeTerminal
				}
				s.setErr(errLiveSubscriptionClosed)
				return consumeTerminal
			}
			if delivery.JournalSeq > *lastSeq && hasJournalGap(*lastSeq, delivery.JournalSeq) {
				if err := s.replayGap(lastSeq); err != nil {
					if errors.Is(err, errReplayStopped) || s.stopped() {
						return consumeStopped
					}
					s.setErr(err)
					return consumeTerminal
				}
			}
			if !s.deliver(delivery, lastSeq) {
				return consumeStopped
			}
		}
	}
}

func hasJournalGap(lastSeq, nextSeq uint64) bool {
	return lastSeq != ^uint64(0) && nextSeq > lastSeq+1
}

func isTemporaryError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var temporary interface{ Temporary() bool }
	return errors.As(err, &temporary) && temporary.Temporary()
}

// deliver applies the product filter, folds gate state before exposure, and advances the
// durable cursor even for filtered events. Advancing on filtered sequences is important:
// otherwise a narrow loop subscription would replay the same other-loop records forever.
func (s *replayingSubscription) deliver(delivery event.Delivery, lastSeq *uint64) bool {
	seq := delivery.JournalSeq
	if seq != 0 && seq <= *lastSeq {
		return true
	}
	exposed := shouldExposeEvent(s.filter, delivery.Event)
	if !exposed {
		if seq > *lastSeq {
			*lastSeq = seq
		}
		return true
	}
	if s.foldGate != nil {
		s.foldGate(delivery.Event)
	}
	select {
	case s.out <- delivery:
		if seq > *lastSeq {
			*lastSeq = seq
		}
		return true
	case <-s.done:
		return false
	case <-s.ctx.Done():
		return false
	}
}

func (s *replayingSubscription) installInner(inner event.Subscription) bool {
	s.innerMu.Lock()
	defer s.innerMu.Unlock()
	if s.stopped() {
		return false
	}
	s.inner = inner
	return true
}

func (s *replayingSubscription) clearInner(inner event.Subscription) {
	s.innerMu.Lock()
	if s.inner == inner {
		s.inner = nil
	}
	s.innerMu.Unlock()
}

func (s *replayingSubscription) stopped() bool {
	select {
	case <-s.done:
		return true
	default:
	}
	if s.ctx.Err() != nil {
		s.recordContextError()
		return true
	}
	return false
}

func (s *replayingSubscription) recordContextError() {
	select {
	case <-s.done:
		return
	default:
	}
	s.setErr(s.ctx.Err())
}

func (s *replayingSubscription) setErr(err error) {
	if err == nil {
		return
	}
	s.errMu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.errMu.Unlock()
}

func (s *replayingSubscription) waitRetry(delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-s.done:
		return false
	case <-s.ctx.Done():
		return false
	}
}

func nextReplayDelay(delay time.Duration) time.Duration {
	if delay >= replayRetryMax/2 {
		return replayRetryMax
	}
	return delay * 2
}

var _ event.Subscription = (*replayingSubscription)(nil)
