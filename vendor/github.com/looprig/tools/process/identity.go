// Package process defines the Tools-owned long-running-command supervision
// domain: process identity, lifecycle state, the stable error taxonomy, and
// quota configuration (spec "docs/specs/long-running-command-supervision.md",
// sections "Identity and authorization", "State machine", "Stable errors",
// and "Quotas and retention"). This package has no dependency on
// github.com/looprig/harness; later tasks wire this domain to Harness's
// AsyncProcessRunner/PreparedProcess contracts.
package process

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"

	"github.com/looprig/core/uuid"
)

// Owner is the immutable authority over a supervised process: the session
// and loop that created it (spec "Identity and authorization": "Each process
// has an immutable authority owner: SessionID + LoopID + ProcessID"). Every
// follow-up ProcessOutput, ProcessInput, or ProcessStop call is authorized by
// an exact match against both fields — never against Origin. See Origin for
// why the two are kept separate.
type Owner struct {
	SessionID uuid.UUID
	LoopID    uuid.UUID
}

// Equal reports whether o and other name the same session and loop. A
// cross-owner lookup must be indistinguishable from a missing handle (spec
// "Identity and authorization"), so callers compare ownership with Equal
// rather than inspecting SessionID/LoopID separately.
func (o Owner) Equal(other Owner) bool { return o == other }

// IsZero reports whether o carries no session or loop identity.
func (o Owner) IsZero() bool { return o.SessionID.IsZero() && o.LoopID.IsZero() }

// Origin is the immutable, audit-only provenance of the tool execution that
// created a process. It is recorded for traceability but deliberately
// carries no authority: every follow-up tool invocation necessarily has its
// own new ToolExecutionID, so comparing Origin against a follow-up call would
// reject every legitimate one. Authorization always compares Owner, never
// Origin (spec "Identity and authorization": "The originating Bash
// ToolExecutionID is stored immutably as audit provenance, but it is not
// compared to the execution ID of ProcessOutput, ProcessInput, or
// ProcessStop").
type Origin struct {
	ToolExecutionID uuid.UUID
}

// Handle is a process's opaque, URL-safe capability identifier. It is
// cryptographically random with at least HandleEntropyBytes of entropy and
// carries no owner, filesystem path, timestamp, or OS process identifier:
// nothing about its bytes can be inspected to recover any of those (spec
// "Identity and authorization": "ProcessID is an opaque, cryptographically
// random handle with at least 128 bits of entropy. It must not encode an OS
// PID, filesystem path, owner identifier, or creation timestamp").
type Handle string

// HandleEntropyBytes is the raw random byte length backing a Handle: 16
// bytes = 128 bits, the spec's documented minimum ("Output capture and
// storage": "Process handle entropy | at least 128 bits").
const HandleEntropyBytes = 16

// Valid reports whether h decodes as a well-formed Handle: exactly
// HandleEntropyBytes of unpadded URL-safe base64.
func (h Handle) Valid() bool {
	decoded, err := base64.RawURLEncoding.DecodeString(string(h))
	return err == nil && len(decoded) == HandleEntropyBytes
}

// HandleExists reports whether a candidate Handle is already in use, so
// GenerateHandle/NewHandle can retry on collision without this package
// depending on any supervisor registry type.
type HandleExists func(Handle) bool

// maxGenerateAttempts bounds collision retry so a persistently colliding
// HandleExists (or an exhausted namespace) fails closed instead of looping
// forever.
const maxGenerateAttempts = 8

// GenerateError reports a failure to read randomness while minting a process
// Handle. It wraps the underlying source error so callers can errors.As to a
// *GenerateError and errors.Unwrap (or read .Err) to inspect the cause.
type GenerateError struct{ Err error }

func (e *GenerateError) Error() string { return "process: generate handle: " + e.Err.Error() }

func (e *GenerateError) Unwrap() error { return e.Err }

// CollisionError reports that handle generation exhausted maxGenerateAttempts
// candidates that all reported as already in use by the supplied
// HandleExists check.
type CollisionError struct{ Attempts int }

func (e *CollisionError) Error() string {
	return fmt.Sprintf("process: generate handle: exhausted %d attempts against existing handles", e.Attempts)
}

// NewHandle mints a new Handle sourced from crypto/rand, retrying against
// exists (which may be nil to skip the check) up to maxGenerateAttempts
// times before returning a *CollisionError.
func NewHandle(exists HandleExists) (Handle, error) {
	return generateHandle(rand.Reader, exists)
}

// generateHandle is the testable seam behind NewHandle: it reads
// HandleEntropyBytes of randomness from r for each candidate and returns the
// first one exists reports as unused. It is unexported so only NewHandle
// (backed by crypto/rand.Reader) is part of the public surface; tests inject
// a deterministic or failing reader directly.
func generateHandle(r io.Reader, exists HandleExists) (Handle, error) {
	for attempt := 1; attempt <= maxGenerateAttempts; attempt++ {
		var buf [HandleEntropyBytes]byte
		if _, err := io.ReadFull(r, buf[:]); err != nil {
			return "", &GenerateError{Err: err}
		}
		candidate := Handle(base64.RawURLEncoding.EncodeToString(buf[:]))
		if exists == nil || !exists(candidate) {
			return candidate, nil
		}
	}
	return "", &CollisionError{Attempts: maxGenerateAttempts}
}
