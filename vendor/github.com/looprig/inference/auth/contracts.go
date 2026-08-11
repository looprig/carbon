package auth

import "github.com/looprig/credentials/httpauth"

// Authorizer is the call-scoped HTTP authorization seam. It is an alias rather
// than a new interface so a credentials lease can be passed directly to
// inference transports.
type Authorizer = httpauth.Authorizer

// Authenticator is retained as the source-compatible name for Authorizer.
// New code should prefer Authorizer or credentials/httpauth.Authorizer when it
// supplies a fresh lease for a concrete request attempt.
type Authenticator = httpauth.Authorizer
