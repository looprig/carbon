package acp

import "sync"

// steeringObservationCapacity is intentionally independent from both public
// projection buffers. Non-steer updates can fill those buffers, but they must
// not make an already accepted steer unable to publish its one observation.
const steeringObservationCapacity = 128

// steerObservationLane reserves one bounded slot for each accepted steer. The
// lane is a counter rather than a channel so Close can release all outstanding
// reservations atomically and racing late completions can safely no-op.
type steerObservationLane struct {
	mu       sync.Mutex
	capacity int
	used     int
	closed   bool
}

// steerReservationStatus distinguishes the only admission failure that is a
// capacity signal from a lane that is already retiring.
type steerReservationStatus uint8

const (
	steerReservationReserved steerReservationStatus = iota
	steerReservationCapacityExhausted
	steerReservationClosed
)

func (s steerReservationStatus) String() string {
	switch s {
	case steerReservationReserved:
		return "reserved"
	case steerReservationCapacityExhausted:
		return "capacity_exhausted"
	case steerReservationClosed:
		return "closed"
	default:
		return "unknown"
	}
}

type steerObservationReservation struct {
	lane *steerObservationLane
	once sync.Once
}

func newSteerObservationLane(capacity int) *steerObservationLane {
	if capacity < 0 {
		capacity = 0
	}
	return &steerObservationLane{capacity: capacity}
}

func (l *steerObservationLane) reserve() (*steerObservationReservation, steerReservationStatus) {
	if l == nil {
		return nil, steerReservationClosed
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil, steerReservationClosed
	}
	if l.used >= l.capacity {
		return nil, steerReservationCapacityExhausted
	}
	l.used++
	return &steerObservationReservation{lane: l}, steerReservationReserved
}

func (r *steerObservationReservation) release() {
	if r == nil || r.lane == nil {
		return
	}
	r.once.Do(func() {
		r.lane.mu.Lock()
		if !r.lane.closed && r.lane.used > 0 {
			r.lane.used--
		}
		r.lane.mu.Unlock()
	})
}

func (l *steerObservationLane) close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.closed = true
	l.used = 0
	l.mu.Unlock()
}

func (l *steerObservationLane) inUse() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.used
}
