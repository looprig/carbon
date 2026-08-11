package secrets

import "time"

// MaxVersionLength bounds backend-issued opaque versions.
const MaxVersionLength = 256

// MaxPageTokenLength bounds opaque continuation tokens.
const MaxPageTokenLength = 256

// MaxPageItems is the largest page an implementation may return. A caller's
// smaller limit is still mandatory for each List operation.
const MaxPageItems = 1000

const versionUnsupportedText = "unsupported"

// Version is an opaque backend-issued coordination value. Its representation
// is constructor-enforced and comparable, while the unsupported marker has a
// private tag so it cannot collide with a supported value's text.
type Version struct {
	value       string
	unsupported bool
}

// VersionUnsupported explicitly represents a backend that cannot provide a
// stable comparable version. It is not synthesized from timestamps or
// fingerprints, and it is the only supported way to report the marker.
var VersionUnsupported = Version{unsupported: true}

// NewVersion validates a backend-issued version. The explicit unsupported
// sentinel is reserved for direct backend use via VersionUnsupported.
func NewVersion(value string) (Version, error) {
	if len(value) == 0 || len(value) > MaxVersionLength {
		return Version{}, NewInvalidVersionError("length")
	}
	if value == versionUnsupportedText {
		return Version{}, NewInvalidVersionError("reserved")
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 0x21 || value[i] > 0x7e {
			return Version{}, NewInvalidVersionError("character")
		}
	}
	return Version{value: value}, nil
}

// String returns the opaque version text, or an empty string for zero.
func (v Version) String() string {
	if v.unsupported {
		return versionUnsupportedText
	}
	return v.value
}

// IsZero reports whether no version was supplied.
func (v Version) IsZero() bool { return !v.unsupported && v.value == "" }

// IsUnsupported reports explicit lack of backend version support.
func (v Version) IsUnsupported() bool { return v.unsupported }

// Valid reports whether v is a bounded, printable version or the explicit
// unsupported sentinel.
func (v Version) Valid() bool {
	if v.unsupported {
		return true
	}
	if len(v.value) == 0 || len(v.value) > MaxVersionLength {
		return false
	}
	for i := 0; i < len(v.value); i++ {
		if v.value[i] < 0x21 || v.value[i] > 0x7e {
			return false
		}
	}
	return true
}

// MarshalText emits the bounded canonical version text, including the
// explicit unsupported marker.
func (v Version) MarshalText() ([]byte, error) {
	if !v.Valid() {
		return nil, NewInvalidVersionError("version")
	}
	return []byte(v.String()), nil
}

// UnmarshalText accepts the explicit unsupported marker or a supported value,
// leaving v unchanged on failure.
func (v *Version) UnmarshalText(text []byte) error {
	if v == nil {
		return NewInvalidVersionError("nil receiver")
	}
	if len(text) > MaxVersionLength {
		return NewInvalidVersionError("length")
	}
	if string(text) == versionUnsupportedText {
		*v = VersionUnsupported
		return nil
	}
	parsed, err := NewVersion(string(text))
	if err != nil {
		return err
	}
	*v = parsed
	return nil
}

// Record is the resolved value plus safe coordination metadata. Callers do
// not choose Version or UpdatedAt when a backend creates a record.
type Record struct {
	Reference Reference
	Value     Secret
	Version   Version
	UpdatedAt time.Time
}

// Metadata is the value-free portion of a Record used for listing.
type Metadata struct {
	Reference Reference
	Version   Version
	UpdatedAt time.Time
}

// Metadata returns a value-free copy of r.
func (r Record) Metadata() Metadata {
	return Metadata{Reference: r.Reference, Version: r.Version, UpdatedAt: r.UpdatedAt}
}

// Validate checks that a record contains a safe reference, a real secret, and
// a valid version. Timestamp zero is allowed because remote backends may not
// publish update timestamps.
func (r Record) Validate() error {
	if r.Reference.IsZero() {
		return NewInvalidReferenceError("record reference")
	}
	if err := r.Value.Validate(); err != nil {
		return err
	}
	if r.Version.IsZero() || !r.Version.Valid() {
		return NewInvalidVersionError("record version")
	}
	return nil
}

// Validate checks that metadata contains no secret value and only safe
// coordination fields.
func (m Metadata) Validate() error {
	if m.Reference.IsZero() {
		return NewInvalidReferenceError("metadata reference")
	}
	if m.Version.IsZero() || !m.Version.Valid() {
		return NewInvalidVersionError("metadata version")
	}
	return nil
}

// DeleteStatus reports whether a delete removed a record or found it already
// absent. It is intentionally one status field rather than several booleans
// whose combinations could be contradictory.
type DeleteStatus uint8

const (
	DeleteStatusAbsent DeleteStatus = iota
	DeleteStatusDeleted
)

func (s DeleteStatus) String() string {
	switch s {
	case DeleteStatusAbsent:
		return "absent"
	case DeleteStatusDeleted:
		return "deleted"
	default:
		return "invalid"
	}
}

// DeleteResult reports an idempotent deletion without returning secret bytes.
// A successful unconditional delete may report VersionUnsupported only when
// the backend explicitly lacks comparable versions; that marker never implies
// that a compare-and-swap precondition was checked or can be used safely.
type DeleteResult struct {
	Reference Reference
	Version   Version
	Status    DeleteStatus
}

// NewDeleteResult constructs a value-free delete result and validates its
// status/version combination. Absent deletes use VersionUnsupported because no
// record version exists to report.
func NewDeleteResult(reference Reference, status DeleteStatus, version Version) (DeleteResult, error) {
	result := DeleteResult{Reference: reference, Version: version, Status: status}
	if err := result.Validate(); err != nil {
		return DeleteResult{}, err
	}
	return result, nil
}

// Validate checks a delete result's status and safe coordination metadata.
func (r DeleteResult) Validate() error {
	if r.Reference.IsZero() {
		return NewInvalidReferenceError("delete reference")
	}
	if !r.Version.Valid() {
		return NewInvalidVersionError("delete version")
	}
	switch r.Status {
	case DeleteStatusAbsent:
		if r.Version != VersionUnsupported {
			return NewInvalidOptionsError("absent delete version")
		}
	case DeleteStatusDeleted:
		// VersionUnsupported is valid here only when the backend explicitly
		// lacks comparable versions. DeleteOptions.Validate rejects that
		// marker for CAS, so this result never implies CAS safety.
		if r.Version.IsZero() {
			return NewInvalidVersionError("delete version")
		}
	default:
		return NewInvalidOptionsError("delete status")
	}
	return nil
}

// PageToken is an opaque bounded continuation token. Its representation is
// constructor-enforced and comparable; its zero value denotes the first page.
type PageToken struct{ value string }

// NewPageToken validates an opaque continuation token. Empty input is the
// zero first-page token.
func NewPageToken(token string) (PageToken, error) {
	if len(token) > MaxPageTokenLength {
		return PageToken{}, NewInvalidPageTokenError("length")
	}
	for i := 0; i < len(token); i++ {
		if token[i] < 0x21 || token[i] > 0x7e {
			return PageToken{}, NewInvalidPageTokenError("character")
		}
	}
	return PageToken{value: token}, nil
}

// String returns token text.
func (p PageToken) String() string { return p.value }

// IsZero reports whether p denotes the first page.
func (p PageToken) IsZero() bool { return p.value == "" }

// Valid reports whether p is zero or bounded printable token text.
func (p PageToken) Valid() bool {
	if len(p.value) > MaxPageTokenLength {
		return false
	}
	for i := 0; i < len(p.value); i++ {
		if p.value[i] < 0x21 || p.value[i] > 0x7e {
			return false
		}
	}
	return true
}

// MarshalText emits the bounded token text.
func (p PageToken) MarshalText() ([]byte, error) {
	if !p.Valid() {
		return nil, NewInvalidPageTokenError("token")
	}
	return []byte(p.String()), nil
}

// UnmarshalText accepts bounded printable token text and leaves p unchanged on
// failure.
func (p *PageToken) UnmarshalText(text []byte) error {
	if p == nil {
		return NewInvalidPageTokenError("nil receiver")
	}
	if len(text) > MaxPageTokenLength {
		return NewInvalidPageTokenError("length")
	}
	parsed, err := NewPageToken(string(text))
	if err != nil {
		return err
	}
	*p = parsed
	return nil
}

// Page is a bounded metadata page and optional opaque continuation token.
type Page[T any] struct {
	Items     []T
	NextToken PageToken
}

// NewPage copies items and validates the requested bounded page size.
func NewPage[T any](items []T, next PageToken) (Page[T], error) {
	if len(items) > MaxPageItems {
		return Page[T]{}, NewInvalidOptionsError("page size")
	}
	if !next.Valid() {
		return Page[T]{}, NewInvalidPageTokenError("page token")
	}
	copyItems := make([]T, len(items))
	copy(copyItems, items)
	return Page[T]{Items: copyItems, NextToken: next}, nil
}

// Validate checks a page against the mandatory caller limit.
func (p Page[T]) Validate(limit int) error {
	if limit <= 0 || limit > MaxPageItems || len(p.Items) > limit {
		return NewInvalidOptionsError("page limit")
	}
	if !p.NextToken.Valid() {
		return NewInvalidPageTokenError("page token")
	}
	return nil
}
