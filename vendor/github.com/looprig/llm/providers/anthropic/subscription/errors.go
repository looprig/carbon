package subscription

import (
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
)

const unsupportedRegistrationMessage = "anthropic subscription registration unavailable: third-party subscription OAuth is not approved for this client"

// UnsupportedRegistrationError is the redaction-safe, typed failure returned
// by a blocked subscription registration gate. It intentionally carries no
// caller input or provider-controlled value.
type UnsupportedRegistrationError struct{}

var (
	_ error          = (*UnsupportedRegistrationError)(nil)
	_ fmt.Formatter  = (*UnsupportedRegistrationError)(nil)
	_ slog.LogValuer = (*UnsupportedRegistrationError)(nil)
)

// Error reports the stable policy boundary without exposing registration
// details that are not part of a sanctioned public contract.
func (e *UnsupportedRegistrationError) Error() string { return unsupportedRegistrationMessage }

// Format keeps every fmt verb redaction-safe. In particular, %#v must not
// fall back to reflecting an error's fields, and unsupported verbs must not
// trigger a formatting diagnostic containing implementation details.
func (e *UnsupportedRegistrationError) Format(state fmt.State, verb rune) {
	message := e.Error()
	switch verb {
	case 'q':
		_, _ = io.WriteString(state, strconv.Quote(message))
	case 'x':
		_, _ = io.WriteString(state, hex.EncodeToString([]byte(message)))
	case 'X':
		_, _ = io.WriteString(state, strings.ToUpper(hex.EncodeToString([]byte(message))))
	default:
		_, _ = io.WriteString(state, message)
	}
}

// LogValue prevents structured logging from reflecting the error value or
// attempting to serialize fields that could later become caller-controlled.
func (e *UnsupportedRegistrationError) LogValue() slog.Value {
	return slog.StringValue(e.Error())
}
