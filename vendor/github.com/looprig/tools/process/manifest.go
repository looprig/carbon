package process

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/tools/internal/atomicfile"
)

// manifestVersion is the current on-disk manifest format version (spec
// "Manifests and durability": "format version"). Load rejects any other
// value as CodeManifestCorrupt. Only version 1 exists today; a future
// version bump adds a case to the version check in ManifestStore.Load
// rather than reshaping manifestWire in place.
const manifestVersion = 1

// manifestSuffix names the on-disk file for one process's manifest, keyed by
// its Handle, beneath a ManifestStore's private resource root.
const manifestSuffix = ".manifest.json"

// AccessMode is the manifest's sanitized record of the effective workspace
// access a process was granted at spawn (spec "Workspace coordination",
// "Lease compatibility": read-only, scoped write, broad/workspace write).
// Tools does not import Harness's WorkspaceAccess type; this is a narrow,
// stable local mirror recorded for manifest/audit purposes only.
type AccessMode string

// The closed set of recorded access modes.
const (
	AccessReadOnly    AccessMode = "read_only"
	AccessScopedWrite AccessMode = "scoped_write"
	AccessBroadWrite  AccessMode = "broad_write"
)

// Valid reports whether a belongs to the closed AccessMode domain.
func (a AccessMode) Valid() bool {
	switch a {
	case AccessReadOnly, AccessScopedWrite, AccessBroadWrite:
		return true
	default:
		return false
	}
}

// CommandMetadata is the sanitized, non-secret description of the command a
// manifest records (spec "Manifests and durability": "sanitized command
// metadata"). It never carries environment, stdin content, or captured
// output.
type CommandMetadata struct {
	// Command is the shell command line as supplied to Bash.
	Command string
	// WorkDir is the resolved working directory, when the call requested
	// one.
	WorkDir string
}

// SpoolCursors is a manifest's durable snapshot of its disk spool's cursor
// bounds (spec "Manifests and durability": "spool metadata and cursor
// bounds"), mirroring what a live Spool (spool.go) tracks so a manifest
// alone reports accurate bounds without reopening the spool file.
type SpoolCursors struct {
	// TotalBytes is the monotonically increasing count of every byte ever
	// appended to the spool, independent of what remains retained.
	TotalBytes int64
	// RetainedFrom is the cursor of the earliest byte still retained on
	// disk; bytes before it have been dropped by ceiling truncation.
	RetainedFrom int64
}

// Result carries the terminal outcome fields recorded once a process
// reaches a terminal State (spec "Manifests and durability": "terminal
// result fields when complete"). It is the zero value for any non-terminal
// manifest.
type Result struct {
	// ExitCode is set only when State == StateExited (spec "Durable events
	// and notifications": "only completed/exited requires an exit code").
	ExitCode *int
	// Reason is the closed lifecycle-event reason (spec "Durable events and
	// notifications" table), e.g. "exited", "failed", "timed-out",
	// "interrupted", "terminated", "killed", "lost-on-restore".
	Reason string
}

// Equal reports whether r and other carry the same terminal outcome.
func (r Result) Equal(other Result) bool {
	if r.Reason != other.Reason {
		return false
	}
	if (r.ExitCode == nil) != (other.ExitCode == nil) {
		return false
	}
	return r.ExitCode == nil || *r.ExitCode == *other.ExitCode
}

// LifecycleEventIDs are the stable, per-kind identifiers a manifest
// allocates and persists before the corresponding Harness lifecycle event or
// completion notification may be published (spec "Manifests and
// durability": "stable lifecycle EventIDs and completion-notification
// CommandID allocated before publication"). The zero uuid.UUID means "not
// yet allocated"; once a field is non-zero, ManifestStore.Save rejects any
// attempt to change it (LifecycleEventIDChangedError), so retries always
// reuse the same ID.
type LifecycleEventIDs struct {
	Started      uuid.UUID
	Backgrounded uuid.UUID
	Completed    uuid.UUID
	Lost         uuid.UUID
	CommandID    uuid.UUID
}

// osMetadata is OS execution metadata needed only for same-process teardown
// (spec "Manifests and durability": "OS execution metadata needed only for
// same-process teardown"). PID is a placeholder for whatever platform
// process-tree handle a later task's Sandbox adapter records; this task
// only reserves and durably persists the slot.
//
// osMetadata itself is unexported: no Manifest accessor of any kind reads
// or writes it, so it can never leak into this package's public API. Only
// code within package process (e.g. a future same-process teardown helper)
// can read Manifest.os directly, by ordinary Go field visibility. Restore
// reconciliation must never trust or signal a value recovered from it (spec:
// "recorded local PIDs are never trusted or signalled") — see
// TestManifestOSMetadataIgnoredByStateTransition, which proves the state
// transition this package exposes never reads osMetadata at all.
type osMetadata struct {
	PID int
}

// Manifest is the durable record Tools atomically persists for one
// supervised process before returning its handle, and updates for the rest
// of the process's lifetime (spec "Manifests and durability"). Its exported
// fields are a plain, ergonomic value type in the style of Config; a
// ManifestStore is responsible for enforcing the monotonic and
// terminal-immutability invariants across successive Save calls — Manifest
// itself only validates that a single value is internally well-formed
// (Validate).
type Manifest struct {
	Identity

	Command CommandMetadata
	Access  AccessMode
	TTY     bool

	State State

	CreatedAt  time.Time
	StartedAt  *time.Time
	FinishedAt *time.Time
	Deadline   *time.Time

	Cursors SpoolCursors
	Result  Result

	Events LifecycleEventIDs

	// CompletionPublished is a monotonically increasing marker of the last
	// successfully attempted completion publication. It exists purely to
	// skip a redundant republish attempt; it is never the deduplication
	// boundary (spec "Manifests and durability": "the completion-published
	// marker avoids needless retries but is not required for at-most-once
	// journal state" — that boundary is the Harness durable journal, added
	// in a later phase).
	CompletionPublished int64

	os osMetadata
}

// NewManifest builds the initial Manifest for a newly admitted process:
// State StateStarting, no started/finished/deadline timestamps unless
// deadline is non-zero, and zero cursors, result, and lifecycle IDs.
func NewManifest(id Identity, cmd CommandMetadata, access AccessMode, tty bool, createdAt time.Time, deadline *time.Time) Manifest {
	return Manifest{
		Identity:  id,
		Command:   cmd,
		Access:    access,
		TTY:       tty,
		State:     StateStarting,
		CreatedAt: createdAt,
		Deadline:  deadline,
	}
}

// Validate reports whether m is internally well-formed: required identity
// fields are present, State/Access belong to their closed domains, and the
// timestamp/result fields are consistent with State (spec "Manifests and
// durability" invariants; a violation is exactly what Load reports as
// CodeManifestCorrupt for a value read from disk, and what Save rejects for
// a value a caller is trying to persist for the first time).
func (m Manifest) Validate() error {
	switch {
	case !m.Handle.Valid():
		return Wrap(CodeManifestCorrupt, errors.New("invalid process handle"))
	case m.Owner.IsZero():
		return Wrap(CodeManifestCorrupt, errors.New("owner is required"))
	case m.Origin.ToolExecutionID.IsZero():
		return Wrap(CodeManifestCorrupt, errors.New("origin is required"))
	case !m.State.Valid():
		return Wrap(CodeManifestCorrupt, errors.New("unrecognized state"))
	case !m.Access.Valid():
		return Wrap(CodeManifestCorrupt, errors.New("unrecognized access mode"))
	case m.CreatedAt.IsZero():
		return Wrap(CodeManifestCorrupt, errors.New("created_at is required"))
	case m.State.Terminal() && m.FinishedAt == nil:
		return Wrap(CodeManifestCorrupt, errors.New("terminal state requires finished_at"))
	case m.State.Terminal() && m.FinishedAt.Before(m.CreatedAt):
		return Wrap(CodeManifestCorrupt, errors.New("finished_at precedes created_at"))
	case !m.State.Terminal() && m.FinishedAt != nil:
		return Wrap(CodeManifestCorrupt, errors.New("non-terminal state must not carry finished_at"))
	case m.StartedAt != nil && m.StartedAt.Before(m.CreatedAt):
		return Wrap(CodeManifestCorrupt, errors.New("started_at precedes created_at"))
	case m.State == StateExited && m.Result.ExitCode == nil:
		return Wrap(CodeManifestCorrupt, errors.New("exited state requires an exit code"))
	case m.State != StateExited && m.Result.ExitCode != nil:
		return Wrap(CodeManifestCorrupt, errors.New("only the exited state carries an exit code"))
	case m.Cursors.TotalBytes < 0:
		return Wrap(CodeManifestCorrupt, errors.New("negative cursors.total_bytes"))
	case m.Cursors.RetainedFrom < 0:
		return Wrap(CodeManifestCorrupt, errors.New("negative cursors.retained_from"))
	case m.Cursors.RetainedFrom > m.Cursors.TotalBytes:
		return Wrap(CodeManifestCorrupt, errors.New("cursors.retained_from exceeds cursors.total_bytes"))
	case m.CompletionPublished < 0:
		return Wrap(CodeManifestCorrupt, errors.New("negative completion_published"))
	default:
		return nil
	}
}

// manifestWire is the on-disk JSON shape for Manifest. It exists separately
// from Manifest so version handling and the unexported os field's
// persistence are explicit and centralized here, rather than requiring
// Manifest itself to implement custom (Un)MarshalJSON. OSMetadata is an
// exported field of an unexported type: it round-trips through ordinary
// encoding/json while the type itself, and Manifest.os, remain invisible
// outside this package.
type manifestWire struct {
	Version int `json:"version"`

	Handle Handle `json:"handle"`
	Owner  Owner  `json:"owner"`
	Origin Origin `json:"origin"`

	Command CommandMetadata `json:"command"`
	Access  AccessMode      `json:"access"`
	TTY     bool            `json:"tty"`

	State State `json:"state"`

	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Deadline   *time.Time `json:"deadline,omitempty"`

	Cursors SpoolCursors `json:"cursors"`
	Result  Result       `json:"result"`

	Events LifecycleEventIDs `json:"events"`

	CompletionPublished int64 `json:"completion_published"`

	OSMetadata osMetadata `json:"os_metadata"`
}

func toWire(m Manifest) manifestWire {
	return manifestWire{
		Version:             manifestVersion,
		Handle:              m.Handle,
		Owner:               m.Owner,
		Origin:              m.Origin,
		Command:             m.Command,
		Access:              m.Access,
		TTY:                 m.TTY,
		State:               m.State,
		CreatedAt:           m.CreatedAt,
		StartedAt:           m.StartedAt,
		FinishedAt:          m.FinishedAt,
		Deadline:            m.Deadline,
		Cursors:             m.Cursors,
		Result:              m.Result,
		Events:              m.Events,
		CompletionPublished: m.CompletionPublished,
		OSMetadata:          m.os,
	}
}

func fromWire(w manifestWire) Manifest {
	return Manifest{
		Identity: Identity{
			Handle: w.Handle,
			Owner:  w.Owner,
			Origin: w.Origin,
		},
		Command:             w.Command,
		Access:              w.Access,
		TTY:                 w.TTY,
		State:               w.State,
		CreatedAt:           w.CreatedAt,
		StartedAt:           w.StartedAt,
		FinishedAt:          w.FinishedAt,
		Deadline:            w.Deadline,
		Cursors:             w.Cursors,
		Result:              w.Result,
		Events:              w.Events,
		CompletionPublished: w.CompletionPublished,
		os:                  w.OSMetadata,
	}
}

// TerminalResultChangedError reports an attempted manifest update that would
// change the terminal Result of a manifest already in the same terminal
// State (spec "Manifests and durability": terminal result is immutable once
// set). This is a programming-invariant violation, not a stable model-facing
// code, so it is a plain typed Go error in the style of state.go's
// TransitionError rather than a *Error.
type TerminalResultChangedError struct {
	Handle Handle
	State  State
	Had    Result
	Got    Result
}

func (e *TerminalResultChangedError) Error() string {
	return fmt.Sprintf("process: manifest %s: terminal result is immutable in state %s", e.Handle, e.State)
}

// LifecycleEventIDChangedError reports an attempted manifest update that
// would reassign an already-allocated stable lifecycle EventID or
// completion CommandID (spec "Manifests and durability": these IDs are
// "allocated and persisted before publication", so retries must always
// reuse the same ID).
type LifecycleEventIDChangedError struct {
	Handle Handle
	Field  string
	Had    uuid.UUID
	Got    uuid.UUID
}

func (e *LifecycleEventIDChangedError) Error() string {
	return fmt.Sprintf("process: manifest %s: lifecycle %s id changed from %s to %s", e.Handle, e.Field, e.Had, e.Got)
}

// NonMonotonicUpdateError reports a manifest update that would move a
// monotonic field backward (spec "Manifests and durability": "State and
// cursor metadata never move backward"). Field names the offending value
// using its JSON key.
type NonMonotonicUpdateError struct {
	Handle Handle
	Field  string
	Had    int64
	Got    int64
}

func (e *NonMonotonicUpdateError) Error() string {
	return fmt.Sprintf("process: manifest %s: %s moved backward from %d to %d", e.Handle, e.Field, e.Had, e.Got)
}

// ImmutableIdentityChangedError reports an attempted manifest update that
// would change the immutable Handle, Owner, or Origin recorded for a
// process (identity.go: "None of these fields ever change after
// admission").
type ImmutableIdentityChangedError struct {
	Handle Handle
}

func (e *ImmutableIdentityChangedError) Error() string {
	return fmt.Sprintf("process: manifest %s: update changed immutable identity", e.Handle)
}

// resourcePath resolves both the absolute on-disk path and the root-relative
// name for a per-process resource file (manifest or spool) beneath root,
// keyed by h plus a fixed suffix. It defensively re-checks that the resolved
// path is lexically within root even though Handle.Valid() already
// constrains h to unpadded URL-safe base64 with no path separators — this is
// the security invariant that no path constructed by this package may ever
// escape the private resource-root directory, proven even against a
// malicious or corrupt Handle string built by direct type conversion rather
// than NewHandle.
//
// The returned rel is suitable for use as the name argument to an *os.Root
// scoped to root (readResourceFile below): callers needing read access
// scope through that root as defense-in-depth on top of this lexical check,
// rather than opening full directly. full remains available for the callers
// that need an absolute path -- atomicfile.Replace's create-temp-and-rename
// dance and os.Remove.
func resourcePath(root string, h Handle, suffix string) (full, rel string, err error) {
	if !h.Valid() {
		return "", "", New(CodeNotFound)
	}
	full = filepath.Join(root, string(h)+suffix)
	rel, err = filepath.Rel(root, full)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", New(CodeNotFound)
	}
	return full, rel, nil
}

// readResourceFile reads the resource file named rel (the root-relative name
// resourcePath returns) from beneath root, scoped through an *os.Root opened
// for exactly the duration of this one read (gosec G304: "Consider using
// os.Root to scope file access under a fixed root"). This is defense-in-depth
// layered on top of resourcePath's own Handle-format and lexical-escape
// checks, not a replacement for them: rel is never trusted to be safe merely
// because it came from resourcePath.
//
// The *os.Root is opened and closed per call rather than held on
// ManifestStore/Spool: neither type has, or should gain, a store-wide Close
// of its own (see supervisor.go's doShutdown doc comment), and a manifest or
// spool read can happen at any point in a process's — or a restored
// process's — lifetime, long after any single construction-time root handle
// could reasonably be assumed still valid.
func readResourceFile(root, rel string) ([]byte, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return r.ReadFile(rel)
}

// ManifestStore persists one Manifest per process Handle as versioned JSON
// beneath a private resource-root directory (spec "Manifests and
// durability"). It never writes inside the workspace: root is an explicit,
// caller-supplied directory dedicated to process resource storage (spec
// "Supervisor lifetime": "The resource root is never the workspace").
type ManifestStore struct {
	root string

	mu sync.Mutex
}

// NewManifestStore returns a ManifestStore rooted at root. root must already
// exist; ManifestStore never creates it.
func NewManifestStore(root string) *ManifestStore {
	return &ManifestStore{root: filepath.Clean(root)}
}

// path resolves h's absolute on-disk manifest path, for the callers below
// that need one (Save's atomicfile.Replace, Delete's os.Remove).
func (s *ManifestStore) path(h Handle) (string, error) {
	full, _, err := resourcePath(s.root, h, manifestSuffix)
	return full, err
}

// Load reads and validates the manifest for h. A missing file reports
// CodeNotFound. Malformed JSON, an unrecognized format version, or a value
// that fails Manifest.Validate all report CodeManifestCorrupt.
func (s *ManifestStore) Load(h Handle) (Manifest, error) {
	_, rel, err := resourcePath(s.root, h, manifestSuffix)
	if err != nil {
		return Manifest{}, err
	}
	return s.loadPath(rel)
}

// loadPath reads and validates the manifest named rel -- a root-relative
// name from resourcePath -- from beneath s.root.
func (s *ManifestStore) loadPath(rel string) (Manifest, error) {
	data, err := readResourceFile(s.root, rel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Manifest{}, New(CodeNotFound)
		}
		return Manifest{}, Wrap(CodeManifestCorrupt, err)
	}

	var w manifestWire
	if err := json.Unmarshal(data, &w); err != nil {
		return Manifest{}, Wrap(CodeManifestCorrupt, err)
	}
	if w.Version != manifestVersion {
		return Manifest{}, Wrap(CodeManifestCorrupt, fmt.Errorf("unknown manifest version %d", w.Version))
	}

	m := fromWire(w)
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// Save validates m, then atomically persists it (spec "Manifests and
// durability": "write-new, sync, and atomic replace semantics"). If a
// manifest already exists for m.Handle, Save additionally enforces that the
// update does not change immutable identity, move State outside an approved
// state-machine edge, change the Result of an already-terminal manifest,
// move Cursors or CompletionPublished backward, or reassign an
// already-allocated lifecycle EventID/CommandID. Save refuses to overwrite
// an existing manifest that itself fails to load (CodeManifestCorrupt)
// rather than silently discarding evidence of the corruption.
func (s *ManifestStore) Save(m Manifest) error {
	if err := m.Validate(); err != nil {
		return err
	}
	path, rel, err := resourcePath(s.root, m.Handle, manifestSuffix)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	existing, err := s.loadPath(rel)
	switch {
	case err == nil:
		if err := validateManifestUpdate(existing, m); err != nil {
			return err
		}
	case errors.Is(err, New(CodeNotFound)):
		// First write for this handle; nothing to compare against.
	default:
		return err
	}

	data, err := json.Marshal(toWire(m))
	if err != nil {
		return fmt.Errorf("process: marshal manifest: %w", err)
	}
	return atomicfile.Replace(path, data, 0o600)
}

// Delete removes the on-disk manifest for h, if any (spec "Eviction deletes
// the manifest and spool atomically from the supervisor's perspective" --
// Task 8D's terminal-LRU retention is Delete's only caller today). Delete is
// idempotent: deleting a handle that has no manifest file -- already
// deleted, or never written -- is a no-op rather than an error, mirroring
// Spool.Remove's idempotence (spool.go).
func (s *ManifestStore) Delete(h Handle) error {
	path, err := s.path(h)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func validateManifestUpdate(existing, next Manifest) error {
	if existing.Handle != next.Handle || !existing.Owner.Equal(next.Owner) || existing.Origin != next.Origin {
		return &ImmutableIdentityChangedError{Handle: next.Handle}
	}

	if existing.State != next.State {
		if _, err := transition(existing.State, next.State); err != nil {
			return err
		}
	} else if existing.State.Terminal() && !existing.Result.Equal(next.Result) {
		return &TerminalResultChangedError{
			Handle: next.Handle,
			State:  next.State,
			Had:    existing.Result,
			Got:    next.Result,
		}
	}

	if next.Cursors.TotalBytes < existing.Cursors.TotalBytes {
		return &NonMonotonicUpdateError{
			Handle: next.Handle, Field: "cursors.total_bytes",
			Had: existing.Cursors.TotalBytes, Got: next.Cursors.TotalBytes,
		}
	}
	if next.Cursors.RetainedFrom < existing.Cursors.RetainedFrom {
		return &NonMonotonicUpdateError{
			Handle: next.Handle, Field: "cursors.retained_from",
			Had: existing.Cursors.RetainedFrom, Got: next.Cursors.RetainedFrom,
		}
	}
	if next.CompletionPublished < existing.CompletionPublished {
		return &NonMonotonicUpdateError{
			Handle: next.Handle, Field: "completion_published",
			Had: existing.CompletionPublished, Got: next.CompletionPublished,
		}
	}

	return validateLifecycleEventIDsStable(next.Handle, existing.Events, next.Events)
}

func validateLifecycleEventIDsStable(h Handle, existing, next LifecycleEventIDs) error {
	fields := [...]struct {
		name     string
		had, got uuid.UUID
	}{
		{"started", existing.Started, next.Started},
		{"backgrounded", existing.Backgrounded, next.Backgrounded},
		{"completed", existing.Completed, next.Completed},
		{"lost", existing.Lost, next.Lost},
		{"command_id", existing.CommandID, next.CommandID},
	}
	for _, f := range fields {
		if !f.had.IsZero() && f.had != f.got {
			return &LifecycleEventIDChangedError{Handle: h, Field: f.name, Had: f.had, Got: f.got}
		}
	}
	return nil
}
