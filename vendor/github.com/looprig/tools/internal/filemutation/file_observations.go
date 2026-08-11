package filemutation

import (
	"crypto/sha256"
	"sync"

	"github.com/looprig/harness/pkg/tool"
)

// file_observations.go implements the per-loop file-observation map that backs the
// file tools' optimistic concurrency (design §"File-tool optimistic concurrency
// and binding"). ONE fileObservations is created per tool binding (per primer/
// delegate/restored loop) by Files and shared by that loop's ReadFile, WriteFile,
// and EditFile: a complete ReadFile records the raw-content SHA-256, and a later
// same-path WriteFile/EditFile is authorized only if that observation still equals
// the file's current on-disk hash (a compare-and-swap taken while holding the
// path's critical section).
//
// SECURITY: the recorded SHA-256 is a PRIVATE optimistic-concurrency token. It is
// NEVER placed in a ToolResult, a tool schema, or an audit summary — no hash or
// version is exposed to the model. Two loops never share a map, so one loop's read
// can never authorize another loop's write.

// canonicalObservationKey is the map key: the canonical, symlink-resolved,
// workspace-contained ABSOLUTE path (the value containedPath returns). Keying on
// the canonical path makes lexical aliases ("a/../f.txt" vs "f.txt") and
// symlinked-directory aliases of the same real file collapse to one entry.
type canonicalObservationKey string

// fileObservation is one loop's latest complete observation of a canonical path:
// either the file was present with a known raw-content hash, or it was observed
// definitively absent (present == false, hash zero and unused).
type fileObservation struct {
	present bool
	hash    [sha256.Size]byte
}

// filePathState is the per-canonical-path record. Its mutex guards the file tools'
// read-current-hash → compare → publish → record-new critical section so a
// concurrent same-loop mutation of the same path cannot interleave. observed
// reports whether obs holds a usable observation for THIS loop.
type filePathState struct {
	mu       sync.Mutex
	observed bool
	obs      fileObservation
}

// setPresentLocked records a complete present observation. The caller MUST hold
// st.mu (it is used from inside the write critical section, which already holds
// the lock).
func (st *filePathState) setPresentLocked(hash [sha256.Size]byte) {
	st.observed = true
	st.obs = fileObservation{present: true, hash: hash}
}

// setAbsentLocked records a definitive absence observation. The caller MUST hold
// st.mu.
func (st *filePathState) setAbsentLocked() {
	st.observed = true
	st.obs = fileObservation{present: false}
}

// clearLocked drops any observation (used when a stale conflict is detected so the
// model must read again). The caller MUST hold st.mu.
func (st *filePathState) clearLocked() {
	st.observed = false
	st.obs = fileObservation{}
}

// fileObservations is one loop's private map of canonical path → per-path state.
// The map mutex guards only the get-or-create lookup; it is never held across a
// per-path critical section (which takes the returned filePathState's own mutex).
//
// NO-EVICTION POLICY (intentional, not a leak): the map only ever inserts —
// invalidate CLEARS a record's observation but keeps its (small, fixed-size) entry,
// and nothing deletes keys. The map is bounded by the number of distinct paths ONE
// loop touches and is freed wholesale when the loop's tool binding drops. Keeping
// the per-path record alive preserves its mutex identity so concurrent same-path
// operations keep serializing on one lock across an invalidate/re-observe cycle.
type fileObservations struct {
	mu     sync.Mutex
	states map[canonicalObservationKey]*filePathState
}

// newFileObservations builds an empty observation map for focused constructors and tests.
func newFileObservations() *fileObservations {
	return &fileObservations{states: make(map[canonicalObservationKey]*filePathState)}
}

// state returns the per-path record for key, creating it on first use. The map
// mutex is held only for the lookup/insert, so distinct paths never serialize on
// each other and the per-path critical section runs under the record's own mutex.
func (o *fileObservations) state(key canonicalObservationKey) *filePathState {
	o.mu.Lock()
	defer o.mu.Unlock()
	st, ok := o.states[key]
	if !ok {
		st = &filePathState{}
		o.states[key] = st
	}
	return st
}

// WithPath satisfies tool.WorkspaceObservations while preserving the existing
// per-path state used by the file-tool tests.
func (o *fileObservations) WithPath(path string, fn func(*tool.FileObservation) error) error {
	st := o.state(canonicalObservationKey(path))
	st.mu.Lock()
	defer st.mu.Unlock()
	observation := tool.FileObservation{
		Observed: st.observed,
		Present:  st.obs.present,
		Hash:     st.obs.hash,
	}
	if err := fn(&observation); err != nil {
		return err
	}
	st.observed = observation.Observed
	st.obs = fileObservation{present: observation.Present, hash: observation.Hash}
	return nil
}

// recordPresent stores a complete present observation for key (used by ReadFile
// after a full, non-truncated read). It takes the per-path lock itself.
func (o *fileObservations) recordPresent(key canonicalObservationKey, hash [sha256.Size]byte) {
	st := o.state(key)
	st.mu.Lock()
	defer st.mu.Unlock()
	st.setPresentLocked(hash)
}

// recordAbsent stores a definitive absence observation for key (used by ReadFile
// on a definitive not-found).
func (o *fileObservations) recordAbsent(key canonicalObservationKey) {
	st := o.state(key)
	st.mu.Lock()
	defer st.mu.Unlock()
	st.setAbsentLocked()
}

// invalidate drops any observation for key.
func (o *fileObservations) invalidate(key canonicalObservationKey) {
	st := o.state(key)
	st.mu.Lock()
	defer st.mu.Unlock()
	st.clearLocked()
}

// InvalidateAll drops EVERY recorded observation for the loop. It is the narrow
// tool.WorkspaceObservations capability the loop's Bash tool calls after an opaque
// whole-workspace mutation, whose changed paths are unknowable. It snapshots the
// per-path records under the map mutex, then clears each under that record's own
// mutex — never holding the map mutex across a per-path lock, so it preserves the
// package's map-then-path lock ordering and cannot deadlock against a concurrent
// same-path file mutation (which holds only the path lock). Per the no-eviction
// policy it clears observations but keeps each record (and its mutex identity).
func (o *fileObservations) InvalidateAll() {
	o.mu.Lock()
	states := make([]*filePathState, 0, len(o.states))
	for _, st := range o.states {
		states = append(states, st)
	}
	o.mu.Unlock()
	for _, st := range states {
		st.mu.Lock()
		st.clearLocked()
		st.mu.Unlock()
	}
}

// StaleFileError reports that an existing-file overwrite or edit was refused
// because this loop lacks a complete, CURRENT observation of the target: it was
// never read to completion, or it changed on disk since the read (an optimistic-
// concurrency conflict). The model is told to read the file again. The message
// NEVER carries a hash, version, or file content.
type StaleFileError struct {
	// Path is the workspace-relative path as the model supplied it.
	Path string
}

func (e *StaleFileError) Error() string {
	return "file " + e.Path + " must be read before writing: it was never read to completion, or it changed on disk since your last read; read it again, then retry"
}

// FileCreateConflictError reports that a create-without-observation lost the atomic
// no-replace publication race: the destination already exists (another writer
// created it, or it appeared between the absence check and the link). No bytes were
// clobbered. The message carries no hash or content.
type FileCreateConflictError struct {
	// Path is the workspace-relative path as the model supplied it.
	Path string
}

func (e *FileCreateConflictError) Error() string {
	return "file " + e.Path + " already exists (another writer created it); read it before overwriting"
}

// IrregularFileError reports that a write/edit target's final component is not a
// plain regular file this loop can observe and rewrite — a final-component symlink
// (which the read tools refuse to follow) or another non-regular node (directory,
// device, socket, …). It is DISTINCT from StaleFileError on purpose: telling the
// model to "read the file again" would dead-end, because a ReadFile of the same
// path also refuses it (O_NOFOLLOW). The message is actionable and non-secret; it
// carries no hash or content.
type IrregularFileError struct {
	// Path is the workspace-relative path as the model supplied it.
	Path string
}

func (e *IrregularFileError) Error() string {
	return "cannot write " + e.Path + ": it is not a regular file (it is a symlink or other special file), so writing here is refused"
}

// editAnchorError reports that EditFile's occurrence rule was not satisfied (zero
// matches of `old`, or an ambiguous multi-match without replace_all). It is a
// DISTINCT type from StaleFileError: a fresh, current file whose anchor simply does
// not match is not an optimistic-concurrency conflict. The message names only the
// match count, never the file body.
type editAnchorError struct {
	message string
}

func (e *editAnchorError) Error() string { return e.message }
