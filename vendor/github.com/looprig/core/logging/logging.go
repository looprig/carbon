// Package logging builds the application's structured logger on top of the
// standard library's log/slog. The composition root constructs a *slog.Logger with
// New and injects it into the components that need it (dependency inversion: there
// is no package-level logger and slog.SetDefault is never called, so logging is an
// explicit dependency rather than ambient global state).
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// Config configures a logger built by New.
type Config struct {
	// Writer is the destination for log records. A nil Writer is treated as
	// io.Discard so a misconfiguration drops logs rather than panicking on first use.
	Writer io.Writer
	// Level is the minimum severity emitted; records below it are dropped.
	Level slog.Level
}

// New builds a *slog.Logger that writes JSON records — including the source
// file:line — at or above cfg.Level to cfg.Writer.
func New(cfg Config) *slog.Logger {
	w := cfg.Writer
	if w == nil {
		w = io.Discard
	}
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{
		AddSource: true,
		Level:     cfg.Level,
	}))
}

// LevelError reports a log-level string that ParseLevel did not recognize.
type LevelError struct {
	Value string
}

// Error implements error.
func (e *LevelError) Error() string {
	return fmt.Sprintf("logging: unknown level %q", e.Value)
}

// ParseLevel maps a level name to a slog.Level, case-insensitively and ignoring
// surrounding whitespace. An empty string defaults to Info with no error (an unset
// env var is not a misconfiguration). "warning" is accepted as an alias for "warn".
// An unrecognized non-empty value returns Info plus a *LevelError so the caller can
// fall back safely while still surfacing the typo.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, &LevelError{Value: s}
	}
}
