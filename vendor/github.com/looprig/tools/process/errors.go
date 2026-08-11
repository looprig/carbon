package process

// Code is a stable, model-facing process-supervision failure classification
// (spec "Stable errors"). The set is closed; render only Code — never
// Cause — at an untrusted or model-facing boundary, so host paths, OS PIDs,
// and cross-owner details never leak.
type Code string

// The closed stable error taxonomy (spec "Stable errors").
const (
	CodeInvalidArguments                Code = "invalid_arguments"
	CodeInvalidSettings                 Code = "invalid_settings"
	CodeProcessQuotaExceeded            Code = "process_quota_exceeded"
	CodeOutputQuotaExceeded             Code = "output_quota_exceeded"
	CodeLifetimeEnforcementUnavailable  Code = "lifetime_enforcement_unavailable"
	CodeProcessNotificationsUnsupported Code = "process_notifications_unsupported"
	CodeSpawnFailed                     Code = "spawn_failed"
	CodeProcessSetupFailed              Code = "process_setup_failed"
	CodePTYUnavailable                  Code = "pty_unavailable"
	CodeNotFound                        Code = "not_found"
	CodeStdinClosed                     Code = "stdin_closed"
	CodeInputBackpressure               Code = "input_backpressure"
	CodeCursorGap                       Code = "cursor_gap"
	CodeCursorAhead                     Code = "cursor_ahead"
	CodeTimedOut                        Code = "timed_out"
	CodeInterrupted                     Code = "interrupted"
	CodeTerminated                      Code = "terminated"
	CodeKilled                          Code = "killed"
	CodeSupervisorShuttingDown          Code = "supervisor_shutting_down"
	CodeManifestCorrupt                 Code = "manifest_corrupt"
	CodeSpoolCorrupt                    Code = "spool_corrupt"
	CodeLostOnRestore                   Code = "lost_on_restore"
	CodeTeardownFailed                  Code = "teardown_failed"
)

// codes is the closed set backing Valid.
var codes = map[Code]bool{
	CodeInvalidArguments:                true,
	CodeInvalidSettings:                 true,
	CodeProcessQuotaExceeded:            true,
	CodeOutputQuotaExceeded:             true,
	CodeLifetimeEnforcementUnavailable:  true,
	CodeProcessNotificationsUnsupported: true,
	CodeSpawnFailed:                     true,
	CodeProcessSetupFailed:              true,
	CodePTYUnavailable:                  true,
	CodeNotFound:                        true,
	CodeStdinClosed:                     true,
	CodeInputBackpressure:               true,
	CodeCursorGap:                       true,
	CodeCursorAhead:                     true,
	CodeTimedOut:                        true,
	CodeInterrupted:                     true,
	CodeTerminated:                      true,
	CodeKilled:                          true,
	CodeSupervisorShuttingDown:          true,
	CodeManifestCorrupt:                 true,
	CodeSpoolCorrupt:                    true,
	CodeLostOnRestore:                   true,
	CodeTeardownFailed:                  true,
}

// Valid reports whether c belongs to the closed stable-error-code domain.
func (c Code) Valid() bool { return codes[c] }

// Error reports one classified process-supervision failure. Cause carries
// implementation detail for programmatic inspection and trusted logs; Code
// is the stable, model-safe classification (spec "Stable errors": "Errors
// must support errors.Is or typed inspection and render to stable
// model-facing codes without exposing host paths, OS PIDs, or cross-owner
// details").
type Error struct {
	Code  Code
	Cause error
}

// New returns an *Error with the given code and no cause.
func New(code Code) *Error { return &Error{Code: code} }

// Wrap returns an *Error with the given code and cause.
func Wrap(code Code, cause error) *Error { return &Error{Code: code, Cause: cause} }

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	message := "process: " + string(e.Code)
	if e.Cause != nil {
		return message + ": " + e.Cause.Error()
	}
	return message
}

// Unwrap returns the underlying cause, if any, so errors.Is/errors.As can see
// through an *Error to a wrapped sentinel or typed error.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// Is matches *Error values by stable Code, independent of Cause, so
// errors.Is(err, process.New(process.CodeNotFound)) works regardless of
// which concrete cause produced err.
func (e *Error) Is(target error) bool {
	other, ok := target.(*Error)
	return ok && e != nil && other != nil && e.Code == other.Code
}

var _ error = (*Error)(nil)
