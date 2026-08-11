package auth

import (
	"fmt"
	"log/slog"
	"strings"
)

// AuthKind classifies the credential an authentication mechanism requires.
type AuthKind string

const (
	AuthNone   AuthKind = "none"
	AuthAPIKey AuthKind = "api_key"
)

// MissingCredentialsError reports a required credential that was not supplied.
type MissingCredentialsError struct {
	Credential string
}

func (e *MissingCredentialsError) Error() string {
	if e == nil {
		return "inference: missing credential"
	}
	return fmt.Sprintf("inference: missing credential: %s", safeCredentialName(e.Credential))
}

func (e *MissingCredentialsError) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte(e.Error()))
}

func (e *MissingCredentialsError) GoString() string { return e.Error() }

func (e *MissingCredentialsError) LogValue() slog.Value {
	return slog.StringValue(e.Error())
}

func safeCredentialName(value string) string {
	if value == "" {
		return ""
	}
	if len(value) > 96 {
		return "credential"
	}
	for _, c := range value {
		if strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_.:", c) {
			continue
		}
		return "credential"
	}
	return value
}
