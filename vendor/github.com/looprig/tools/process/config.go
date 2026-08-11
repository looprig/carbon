package process

import (
	"errors"
	"time"
)

// Config is the process-supervisor quota and retention surface described by
// the spec's "Quotas and retention" section, plus the numeric defaults from
// its "Output capture and storage" defaults table. The zero Config is valid:
// Normalize fills every zero field with its documented default.
type Config struct {
	// MaxRunningProcessesPerLoop bounds concurrently running processes
	// belonging to one loop.
	MaxRunningProcessesPerLoop int
	// MaxRunningProcessesPerSession bounds concurrently running processes
	// across an entire session (every loop in it).
	MaxRunningProcessesPerSession int
	// MaxRetainedCompletedProcessesPerSession bounds how many completed
	// process manifests a session keeps queryable before least-recently-used
	// eviction (spec "Quotas and retention": "Completed metadata is evicted
	// by least-recently-used order only after the retention limit is
	// reached").
	MaxRetainedCompletedProcessesPerSession int

	// MaxProcessInMemoryBytes bounds one process's in-memory rolling output
	// window (spec "Output capture and storage" default: 1 MiB per
	// process).
	MaxProcessInMemoryBytes int64
	// MaxAggregateInMemoryBytes bounds the sum of every process's
	// in-memory rolling window across a session.
	MaxAggregateInMemoryBytes int64

	// MaxProcessSpoolBytes bounds one process's disk spool retention window
	// (spec default: 64 MiB per process). This is a bounded retention
	// window, not a hard cap on total process output (spec "Output capture
	// and storage": "The spool is a bounded retention window, not a hard
	// cap on the process").
	MaxProcessSpoolBytes int64
	// MaxAggregateSpoolBytes bounds the sum of every process's disk spool
	// across a session.
	MaxAggregateSpoolBytes int64

	// MaxInlineResultBytes bounds the output returned inline in a
	// model-facing result before a caller must page through ProcessOutput
	// (spec default: 32 KiB).
	MaxInlineResultBytes int64

	// MaxPendingWaiters bounds the outstanding ProcessOutput wait: any|all
	// waiters a session admits concurrently.
	MaxPendingWaiters int

	// MaxPendingInputBytes bounds unconsumed ProcessInput data queued for
	// one process, so a call can never block indefinitely behind a process
	// that does not read its stdin (spec "ProcessInput API": "Writes are
	// serialized per process and bounded").
	MaxPendingInputBytes int64

	// GracefulShutdownPeriod is how long supervisor shutdown and
	// ProcessStop's terminate mode wait before escalating to kill (spec
	// default: 5 seconds).
	GracefulShutdownPeriod time.Duration
}

// Documented zero-value defaults. Values annotated "(spec)" are the exact
// numbers from the spec's "Output capture and storage" defaults table. The
// remaining defaults are conservative operational values chosen for this
// task, not spec-mandated, and any Config value may override them; the
// per-process/aggregate pairs are deliberately derived from each other so
// the defaults are self-consistent (a per-process default never exceeds its
// aggregate default) without hand-tuning a second number.
const (
	DefaultMaxRunningProcessesPerLoop              = 8
	DefaultMaxRunningProcessesPerSession           = 32
	DefaultMaxRetainedCompletedProcessesPerSession = 100

	// DefaultMaxProcessInMemoryBytes is the spec's 1 MiB per-process
	// in-memory rolling window default.
	DefaultMaxProcessInMemoryBytes int64 = 1 << 20 // 1 MiB (spec)
	// DefaultMaxAggregateInMemoryBytes derives from the per-process default
	// times the per-session concurrency default.
	DefaultMaxAggregateInMemoryBytes int64 = int64(DefaultMaxRunningProcessesPerSession) * DefaultMaxProcessInMemoryBytes

	// DefaultMaxProcessSpoolBytes is the spec's 64 MiB per-process disk
	// spool default.
	DefaultMaxProcessSpoolBytes int64 = 64 << 20 // 64 MiB (spec)
	// DefaultMaxAggregateSpoolBytes derives from the per-process default
	// times the per-session concurrency default.
	DefaultMaxAggregateSpoolBytes int64 = int64(DefaultMaxRunningProcessesPerSession) * DefaultMaxProcessSpoolBytes

	// DefaultMaxInlineResultBytes is the spec's 32 KiB inline model result
	// default.
	DefaultMaxInlineResultBytes int64 = 32 << 10 // 32 KiB (spec)

	DefaultMaxPendingWaiters    int   = 64
	DefaultMaxPendingInputBytes int64 = 1 << 20 // 1 MiB

	// DefaultGracefulShutdownPeriod is the spec's 5 second graceful
	// shutdown default.
	DefaultGracefulShutdownPeriod = 5 * time.Second // (spec)
)

// withDefaults returns c with every zero field replaced by its documented
// default. It is the defensive configuration-normalization step behind
// Normalize; it never fails because every default is itself valid.
func (c Config) withDefaults() Config {
	if c.MaxRunningProcessesPerLoop == 0 {
		c.MaxRunningProcessesPerLoop = DefaultMaxRunningProcessesPerLoop
	}
	if c.MaxRunningProcessesPerSession == 0 {
		c.MaxRunningProcessesPerSession = DefaultMaxRunningProcessesPerSession
	}
	if c.MaxRetainedCompletedProcessesPerSession == 0 {
		c.MaxRetainedCompletedProcessesPerSession = DefaultMaxRetainedCompletedProcessesPerSession
	}
	if c.MaxProcessInMemoryBytes == 0 {
		c.MaxProcessInMemoryBytes = DefaultMaxProcessInMemoryBytes
	}
	if c.MaxAggregateInMemoryBytes == 0 {
		c.MaxAggregateInMemoryBytes = DefaultMaxAggregateInMemoryBytes
	}
	if c.MaxProcessSpoolBytes == 0 {
		c.MaxProcessSpoolBytes = DefaultMaxProcessSpoolBytes
	}
	if c.MaxAggregateSpoolBytes == 0 {
		c.MaxAggregateSpoolBytes = DefaultMaxAggregateSpoolBytes
	}
	if c.MaxInlineResultBytes == 0 {
		c.MaxInlineResultBytes = DefaultMaxInlineResultBytes
	}
	if c.MaxPendingWaiters == 0 {
		c.MaxPendingWaiters = DefaultMaxPendingWaiters
	}
	if c.MaxPendingInputBytes == 0 {
		c.MaxPendingInputBytes = DefaultMaxPendingInputBytes
	}
	if c.GracefulShutdownPeriod == 0 {
		c.GracefulShutdownPeriod = DefaultGracefulShutdownPeriod
	}
	return c
}

// Validate rejects a negative limit and any explicit per-process limit that
// exceeds its explicit aggregate (or broader-scope) counterpart. A zero
// field is untouched by Validate — it means "unset, use the documented
// default" (see Normalize) — so only strictly negative values and explicit
// inconsistencies between two nonzero fields are rejected.
func (c Config) Validate() error {
	switch {
	case c.MaxRunningProcessesPerLoop < 0:
		return invalidSetting("max running processes per loop must not be negative")
	case c.MaxRunningProcessesPerSession < 0:
		return invalidSetting("max running processes per session must not be negative")
	case c.MaxRunningProcessesPerLoop > 0 && c.MaxRunningProcessesPerSession > 0 &&
		c.MaxRunningProcessesPerLoop > c.MaxRunningProcessesPerSession:
		return invalidSetting("max running processes per loop must not exceed max running processes per session")
	case c.MaxRetainedCompletedProcessesPerSession < 0:
		return invalidSetting("max retained completed processes per session must not be negative")
	case c.MaxProcessInMemoryBytes < 0:
		return invalidSetting("max process in-memory bytes must not be negative")
	case c.MaxAggregateInMemoryBytes < 0:
		return invalidSetting("max aggregate in-memory bytes must not be negative")
	case c.MaxProcessInMemoryBytes > 0 && c.MaxAggregateInMemoryBytes > 0 &&
		c.MaxProcessInMemoryBytes > c.MaxAggregateInMemoryBytes:
		return invalidSetting("max process in-memory bytes must not exceed max aggregate in-memory bytes")
	case c.MaxProcessSpoolBytes < 0:
		return invalidSetting("max process spool bytes must not be negative")
	case c.MaxAggregateSpoolBytes < 0:
		return invalidSetting("max aggregate spool bytes must not be negative")
	case c.MaxProcessSpoolBytes > 0 && c.MaxAggregateSpoolBytes > 0 &&
		c.MaxProcessSpoolBytes > c.MaxAggregateSpoolBytes:
		return invalidSetting("max process spool bytes must not exceed max aggregate spool bytes")
	case c.MaxInlineResultBytes < 0:
		return invalidSetting("max inline result bytes must not be negative")
	case c.MaxPendingWaiters < 0:
		return invalidSetting("max pending waiters must not be negative")
	case c.MaxPendingInputBytes < 0:
		return invalidSetting("max pending input bytes must not be negative")
	case c.GracefulShutdownPeriod < 0:
		return invalidSetting("graceful shutdown period must not be negative")
	default:
		return nil
	}
}

// Normalize validates c and, if valid, returns it with every zero field
// replaced by its documented default (withDefaults). The zero Config
// normalizes to every documented default with no error. A negative field, or
// an explicit value that exceeds an explicit counterpart, returns the zero
// Config and a *Error with CodeInvalidSettings.
//
// Validation runs on c before defaulting, not on the defaulted result:
// Validate's cross-field checks are documented to reject only explicit
// inconsistencies between two nonzero fields, and that "zero means unset"
// contract only holds against the caller's original input. Two independent
// per-field defaults are proven self-consistent with each other
// (TestDefaultsAreSelfConsistent), but a default chosen for one field is not
// guaranteed to stay under a smaller *explicit* value the caller set for its
// counterpart (e.g. an explicit, low MaxRunningProcessesPerSession with
// MaxRunningProcessesPerLoop left at zero) -- validating post-default would
// reject that legitimate partial override merely because defaulting filled
// in the other side.
func (c Config) Normalize() (Config, error) {
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c.withDefaults(), nil
}

func invalidSetting(reason string) error {
	return Wrap(CodeInvalidSettings, errors.New(reason))
}
