package rig

import (
	"strconv"
	"time"

	"github.com/looprig/harness/internal/sessionruntime"
)

// OffloadGCPolicy configures session offload-blob GC: how often a GC pass runs (Interval)
// and the per-pass deadline (Timeout). It reaps orphaned content-addressed offload blobs —
// a blob left durable with no in-ledger blobptr pointer (the crash gap of the writer's
// blob-durable-before-pointer discipline). This is SESSION OFFLOAD GC only, never
// workspace-snapshot GC. Both fields must be positive.
type OffloadGCPolicy struct {
	Interval time.Duration
	Timeout  time.Duration
}

// InvalidOffloadGCIntervalError reports a non-positive OffloadGCPolicy.Interval. A GC
// cadence must be a positive duration, so the rig fails closed at definition time rather
// than wiring a runner that would never (or continuously) tick.
type InvalidOffloadGCIntervalError struct {
	Interval time.Duration
}

func (e *InvalidOffloadGCIntervalError) Error() string {
	return "rig: WithOffloadGC requires a positive Interval, got " + strconv.FormatInt(int64(e.Interval), 10) + "ns"
}

// InvalidOffloadGCTimeoutError reports a non-positive OffloadGCPolicy.Timeout. A per-pass
// deadline must be a positive duration, so the rig fails closed at definition time.
type InvalidOffloadGCTimeoutError struct {
	Timeout time.Duration
}

func (e *InvalidOffloadGCTimeoutError) Error() string {
	return "rig: WithOffloadGC requires a positive Timeout, got " + strconv.FormatInt(int64(e.Timeout), 10) + "ns"
}

// WithOffloadGC arms periodic session offload-blob GC. It validates the policy (both fields
// positive; typed errors on failure) and compiles to the sessionruntime lifecycle option
// that wires the journal-admission gate + GC runner onto both new and restored sessions. It
// is an at-most-once rig option.
func WithOffloadGC(policy OffloadGCPolicy) Option {
	return func(state *definitionState) error {
		if policy.Interval <= 0 {
			return &InvalidOffloadGCIntervalError{Interval: policy.Interval}
		}
		if policy.Timeout <= 0 {
			return &InvalidOffloadGCTimeoutError{Timeout: policy.Timeout}
		}
		return singletonCompile(keyOffloadGC, sessionruntime.WithLifecycleOffloadGC(sessionruntime.OffloadGCPolicy{
			Interval: policy.Interval,
			Timeout:  policy.Timeout,
		}))(state)
	}
}
