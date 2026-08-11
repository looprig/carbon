package credentials

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/looprig/credentials/httpauth"
	"github.com/looprig/secrets"
)

const MaxGenerationLength = 128

// Generation is an opaque, comparable source-issued value. Its safe text
// representation is bounded and cannot contain arbitrary provider material.
// The representation is private so callers must use NewGeneration instead of
// converting arbitrary strings into source generations.
type Generation struct{ value string }

func NewGeneration(value string) (Generation, error) {
	if len(value) == 0 || len(value) > MaxGenerationLength {
		return Generation{}, NewInvalidGenerationError("length")
	}
	if !utf8.ValidString(value) {
		return Generation{}, NewInvalidGenerationError("encoding")
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 0x21 || value[i] > 0x7e {
			return Generation{}, NewInvalidGenerationError("value")
		}
	}
	return Generation{value: value}, nil
}

func (g Generation) IsZero() bool { return g.value == "" }

func (g Generation) Valid() bool {
	if g.IsZero() || len(g.value) > MaxGenerationLength || !utf8.ValidString(g.value) {
		return false
	}
	for i := 0; i < len(g.value); i++ {
		if g.value[i] < 0x21 || g.value[i] > 0x7e {
			return false
		}
	}
	return true
}

func (g Generation) String() string {
	if g.IsZero() {
		return ""
	}
	if !g.Valid() {
		return "invalid"
	}
	return g.value
}

func (g Generation) Validate() error {
	if !g.Valid() {
		return NewInvalidGenerationError("value")
	}
	return nil
}

func (g Generation) MarshalText() ([]byte, error) {
	if err := g.Validate(); err != nil {
		return nil, err
	}
	return []byte(g.value), nil
}

func (g *Generation) UnmarshalText(text []byte) error {
	if g == nil {
		return NewInvalidGenerationError("nil receiver")
	}
	if len(text) > MaxGenerationLength {
		return NewInvalidGenerationError("length")
	}
	parsed, err := NewGeneration(string(text))
	if err != nil {
		return err
	}
	*g = parsed
	return nil
}

func (g Generation) Format(state fmt.State, _ rune) { formatSafe(state, g.String()) }
func (g Generation) GoString() string               { return g.String() }
func (g Generation) LogValue() slog.Value           { return slog.StringValue(g.String()) }

// Failure is a closed authentication-failure classification. Provider bodies,
// account details, and status text are intentionally not representable.
type Failure string

const (
	FailureAuthRejected Failure = "auth_rejected"
	FailureAuthExpired  Failure = "auth_expired"
	FailureAuthRevoked  Failure = "auth_revoked"

	// Short aliases keep call sites readable while retaining the explicit
	// authentication prefix in the canonical values.
	FailureRejected = FailureAuthRejected
	FailureExpired  = FailureAuthExpired
	FailureRevoked  = FailureAuthRevoked
)

// FailureClass is an alias for integrations that use “class” terminology.
type FailureClass = Failure

func NewFailure(value Failure) (Failure, error) {
	if !value.Valid() {
		return "", NewInvalidFailureError("value")
	}
	return value, nil
}

func (f Failure) IsZero() bool { return f == "" }

func (f Failure) Valid() bool {
	switch f {
	case FailureAuthRejected, FailureAuthExpired, FailureAuthRevoked:
		return true
	default:
		return false
	}
}

func (f Failure) String() string {
	if f.IsZero() {
		return ""
	}
	if !f.Valid() {
		return "invalid"
	}
	return string(f)
}

func (f Failure) Validate() error {
	if !f.Valid() {
		return NewInvalidFailureError("value")
	}
	return nil
}

func (f Failure) MarshalText() ([]byte, error) {
	if err := f.Validate(); err != nil {
		return nil, err
	}
	return []byte(f.String()), nil
}

func (f *Failure) UnmarshalText(text []byte) error {
	if f == nil {
		return NewInvalidFailureError("nil receiver")
	}
	parsed, err := NewFailure(Failure(string(text)))
	if err != nil {
		return err
	}
	*f = parsed
	return nil
}

func (f Failure) Format(state fmt.State, _ rune) { formatSafe(state, f.String()) }
func (f Failure) GoString() string               { return f.String() }
func (f Failure) LogValue() slog.Value           { return slog.StringValue(f.String()) }

// Source owns acquisition and invalidation for one explicit identity.
type Source interface {
	Reference() Reference
	Descriptor() Descriptor
	Acquire(context.Context) (Lease, error)
	Invalidate(context.Context, Generation, Failure) error
	Close() error
}

// Lease is an immutable, concurrency-safe snapshot of usable authority.
type Lease interface {
	Generation() Generation
	Descriptor() Descriptor
	ExpiresAt() time.Time
	Authorizer() httpauth.Authorizer
}

// Record is the safe catalog metadata for one credential. Sensitive state is
// held only by the referenced secrets record.
type Record struct {
	Schema     uint32
	Reference  Reference
	Descriptor Descriptor
	State      secrets.Reference
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

const RecordSchemaV1 uint32 = 1

func NewRecord(reference Reference, descriptor Descriptor, state secrets.Reference, createdAt, updatedAt time.Time) (Record, error) {
	record := Record{
		Schema: RecordSchemaV1, Reference: reference, Descriptor: descriptor,
		State: state, CreatedAt: createdAt, UpdatedAt: updatedAt,
	}
	if err := record.Validate(); err != nil {
		return Record{}, err
	}
	return record, nil
}

func (r Record) Validate() error {
	if r.Schema != RecordSchemaV1 {
		return NewInvalidRecordError("schema")
	}
	if err := r.Reference.Validate(); err != nil {
		return NewInvalidRecordError("reference")
	}
	if err := r.Descriptor.Validate(); err != nil {
		return NewInvalidRecordError("descriptor")
	}
	if r.Reference.Provider() != r.Descriptor.Provider {
		return NewInvalidRecordError("reference")
	}
	if _, err := r.State.MarshalText(); err != nil {
		return NewInvalidRecordError("state")
	}
	return nil
}

func (r Record) String() string {
	if err := r.Validate(); err != nil {
		return ErrInvalidRecord.Error()
	}
	return r.Reference.String()
}

func (r Record) Format(state fmt.State, _ rune) { formatSafe(state, r.String()) }
func (r Record) GoString() string               { return r.String() }
func (r Record) LogValue() slog.Value           { return slog.StringValue(r.String()) }

// NoneSource is the explicit local unauthenticated source. It has no
// credential reference, never expires, never refreshes, and returns only the
// no-op authorizer. Close is linearized under mu and is idempotent.
type NoneSource struct {
	descriptor Descriptor
	generation Generation
	authorizer httpauth.Authorizer

	mu     sync.RWMutex
	closed bool
}

// NewNoneSource constructs the only source permitted to have a zero
// credential Reference. The descriptor must still identify an exact local
// transport and use SchemeNone/UsageLocal.
func NewNoneSource(descriptor Descriptor) (*NoneSource, error) {
	if err := descriptor.Validate(); err != nil {
		return nil, err
	}
	if descriptor.Scheme != SchemeNone {
		return nil, NewInvalidDescriptorError("scheme")
	}
	if descriptor.Usage != UsageLocal {
		return nil, NewInvalidDescriptorError("usage")
	}
	generation, err := NewGeneration("none")
	if err != nil {
		return nil, err
	}
	return &NoneSource{
		descriptor: descriptor,
		generation: generation,
		authorizer: httpauth.None(),
	}, nil
}

func (s *NoneSource) Reference() Reference { return Reference{} }

func (s *NoneSource) Descriptor() Descriptor {
	if s == nil {
		return Descriptor{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.descriptor
}

func (s *NoneSource) Acquire(ctx context.Context) (Lease, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if s == nil {
		return nil, &SourceClosedError{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, &SourceClosedError{}
	}
	return noneLease{generation: s.generation, descriptor: s.descriptor, authorizer: s.authorizer}, nil
}

func (s *NoneSource) Invalidate(ctx context.Context, generation Generation, failure Failure) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if err := generation.Validate(); err != nil {
		return err
	}
	if err := failure.Validate(); err != nil {
		return err
	}
	if s == nil {
		return &SourceClosedError{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return &SourceClosedError{}
	}
	// NoneSource has no renewable authority. Matching and stale generations
	// are nevertheless compared here so callers cannot accidentally treat a
	// late invalidation as a mutation of a future source generation.
	if generation != s.generation {
		return nil
	}
	return nil
}

func (s *NoneSource) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return nil
}

type noneLease struct {
	generation Generation
	descriptor Descriptor
	authorizer httpauth.Authorizer
}

func (l noneLease) Generation() Generation          { return l.generation }
func (l noneLease) Descriptor() Descriptor          { return l.descriptor }
func (l noneLease) ExpiresAt() time.Time            { return time.Time{} }
func (l noneLease) Authorizer() httpauth.Authorizer { return l.authorizer }

func (l noneLease) String() string {
	return "credentials: immutable none lease"
}

func (l noneLease) Format(state fmt.State, _ rune) { formatSafe(state, l.String()) }
func (l noneLease) GoString() string               { return l.String() }
func (l noneLease) LogValue() slog.Value           { return slog.StringValue(l.String()) }

func (s *NoneSource) String() string {
	if s == nil {
		return "credentials: nil source"
	}
	return "credentials: none source"
}

func (s *NoneSource) Format(state fmt.State, _ rune) { formatSafe(state, s.String()) }
func (s *NoneSource) GoString() string               { return s.String() }
func (s *NoneSource) LogValue() slog.Value           { return slog.StringValue(s.String()) }
