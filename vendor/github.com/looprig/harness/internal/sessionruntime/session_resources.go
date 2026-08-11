package sessionruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
)

const (
	maxSessionResourcePathBytes     = 4096
	maxSessionResourceIdentityBytes = 256
	sessionResourceAnchorName       = ".harness-session-resource-v1.json"
	sessionResourceAnchorVersion    = 1
)

var (
	errSessionResourcesClosing   = errors.New("session: resource registry closing")
	errSessionResourcesSuspended = errors.New("session: resource registry admission suspended")
	errSessionResourceKeyEmpty   = errors.New("session: resource key is empty")
	errSessionResourceFactory    = errors.New("session: resource factory is nil")
	errSessionResourceNil        = errors.New("session: resource factory returned nil")
)

// SessionResourceStorageErrorKind classifies fail-closed durable root and
// identity-anchor validation failures.
type SessionResourceStorageErrorKind string

const (
	SessionResourceStorageInvalid          SessionResourceStorageErrorKind = "invalid"
	SessionResourceStorageUnavailable      SessionResourceStorageErrorKind = "unavailable"
	SessionResourceStorageIdentityMismatch SessionResourceStorageErrorKind = "identity_mismatch"
	SessionResourceStorageAnchorMissing    SessionResourceStorageErrorKind = "anchor_missing"
	SessionResourceStorageAnchorCorrupt    SessionResourceStorageErrorKind = "anchor_corrupt"
	SessionResourceStorageWorkspaceOverlap SessionResourceStorageErrorKind = "workspace_overlap"
)

// SessionResourceStorageError preserves a stable classification while wrapping
// provider and filesystem causes.
type SessionResourceStorageError struct {
	Kind  SessionResourceStorageErrorKind
	Path  string
	Cause error
}

func (e *SessionResourceStorageError) Error() string {
	message := "session: resource storage " + string(e.Kind)
	if e.Path != "" {
		message += ": " + e.Path
	}
	if e.Cause != nil {
		message += ": " + e.Cause.Error()
	}
	return message
}

func (e *SessionResourceStorageError) Unwrap() error { return e.Cause }

// ProcessServicesUnsupportedError rejects process-enabled definitions on
// engines whose lifecycle events cannot be published by Harness.
type ProcessServicesUnsupportedError struct {
	Engine loop.Engine
}

func (e *ProcessServicesUnsupportedError) Error() string {
	return "session: process_notifications_unsupported"
}

type sessionResourceAnchor struct {
	Version   int    `json:"version"`
	SessionID string `json:"session_id"`
	Identity  string `json:"identity"`
}

func topologyRequiresProcessServices(topology Topology) bool {
	for _, definition := range topology.Definitions {
		if definition.ToolRequirements()&tool.RequiresProcessServices != 0 {
			return true
		}
	}
	return false
}

func validateProcessServiceEngines(topology Topology) error {
	for _, definition := range topology.Definitions {
		if definition.ToolRequirements()&tool.RequiresProcessServices != 0 &&
			definition.Engine() != loop.EngineNative {
			return &ProcessServicesUnsupportedError{Engine: definition.Engine()}
		}
	}
	return nil
}

func resolveSessionResources(
	ctx context.Context,
	id uuid.UUID,
	resolve SessionResourceStorageResolver,
	workspaceRoot string,
	restore bool,
) (*sessionResources, error) {
	if id.IsZero() {
		return nil, &SessionResourceStorageError{Kind: SessionResourceStorageInvalid}
	}
	if resolve == nil {
		return nil, &SessionResourceStorageError{Kind: SessionResourceStorageUnavailable}
	}
	root, identity, err := resolve(ctx, id)
	if err != nil {
		return nil, &SessionResourceStorageError{Kind: SessionResourceStorageUnavailable, Cause: err}
	}
	root, err = canonicalSessionResourceRoot(root)
	if err != nil {
		return nil, err
	}
	if err := validateSessionResourceIdentity(identity); err != nil {
		return nil, err
	}
	if workspaceRoot != "" {
		workspace, canonicalErr := filepath.Abs(workspaceRoot)
		if canonicalErr != nil {
			return nil, &SessionResourceStorageError{Kind: SessionResourceStorageInvalid, Path: workspaceRoot, Cause: canonicalErr}
		}
		workspace = filepath.Clean(workspace)
		if evaluated, evaluateErr := filepath.EvalSymlinks(workspace); evaluateErr == nil {
			workspace = filepath.Clean(evaluated)
		}
		if pathsOverlap(root, workspace) {
			return nil, &SessionResourceStorageError{Kind: SessionResourceStorageWorkspaceOverlap, Path: root}
		}
	}
	if err := establishSessionResourceRoot(root, !restore); err != nil {
		return nil, err
	}
	if err := ensureSessionResourceAnchor(root, id, identity, restore); err != nil {
		return nil, err
	}
	return newSessionResources(root), nil
}

func canonicalSessionResourceRoot(root string) (string, error) {
	if root == "" || len(root) > maxSessionResourcePathBytes || !utf8.ValidString(root) ||
		strings.TrimSpace(root) != root || containsControlRune(root) {
		return "", &SessionResourceStorageError{Kind: SessionResourceStorageInvalid, Path: root}
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", &SessionResourceStorageError{Kind: SessionResourceStorageInvalid, Path: root, Cause: err}
	}
	absolute = filepath.Clean(absolute)
	existing := absolute
	var suffix []string
	for {
		_, statErr := os.Lstat(existing)
		if statErr == nil {
			break
		}
		if !errors.Is(statErr, os.ErrNotExist) {
			return "", &SessionResourceStorageError{Kind: SessionResourceStorageUnavailable, Path: existing, Cause: statErr}
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return "", &SessionResourceStorageError{Kind: SessionResourceStorageUnavailable, Path: absolute, Cause: statErr}
		}
		suffix = append(suffix, filepath.Base(existing))
		existing = parent
	}
	evaluated, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", &SessionResourceStorageError{Kind: SessionResourceStorageInvalid, Path: existing, Cause: err}
	}
	for i := len(suffix) - 1; i >= 0; i-- {
		evaluated = filepath.Join(evaluated, suffix[i])
	}
	evaluated = filepath.Clean(evaluated)
	if len(evaluated) > maxSessionResourcePathBytes {
		return "", &SessionResourceStorageError{Kind: SessionResourceStorageInvalid, Path: evaluated}
	}
	return evaluated, nil
}

func establishSessionResourceRoot(root string, create bool) error {
	if create {
		if err := createPrivateSessionResourceRoot(root); err != nil {
			return &SessionResourceStorageError{Kind: SessionResourceStorageUnavailable, Path: root, Cause: err}
		}
	}
	info, err := os.Lstat(root)
	if err != nil {
		if !create && errors.Is(err, os.ErrNotExist) {
			return &SessionResourceStorageError{Kind: SessionResourceStorageAnchorMissing, Path: root}
		}
		return &SessionResourceStorageError{Kind: SessionResourceStorageUnavailable, Path: root, Cause: err}
	}
	private, securityErr := sessionResourcePathIsPrivate(root, info, true)
	if securityErr != nil {
		return &SessionResourceStorageError{Kind: SessionResourceStorageUnavailable, Path: root, Cause: securityErr}
	}
	if !private {
		return &SessionResourceStorageError{Kind: SessionResourceStorageInvalid, Path: root}
	}
	return nil
}

func validateSessionResourceIdentity(identity string) error {
	if identity == "" || len(identity) > maxSessionResourceIdentityBytes ||
		!utf8.ValidString(identity) || strings.TrimSpace(identity) != identity ||
		containsControlRune(identity) {
		return &SessionResourceStorageError{Kind: SessionResourceStorageInvalid}
	}
	return nil
}

func containsControlRune(value string) bool {
	for _, candidate := range value {
		if unicode.IsControl(candidate) {
			return true
		}
	}
	return false
}

func pathsOverlap(left, right string) bool {
	relative, err := filepath.Rel(left, right)
	if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return true
	}
	relative, err = filepath.Rel(right, left)
	if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return true
	}

	leftInfo, leftErr := os.Stat(left)
	rightInfo, rightErr := os.Stat(right)
	if leftErr == nil && rightErr == nil && os.SameFile(leftInfo, rightInfo) {
		return true
	}
	return leftErr == nil && pathHasAncestorIdentity(right, leftInfo) ||
		rightErr == nil && pathHasAncestorIdentity(left, rightInfo)
}

func pathHasAncestorIdentity(path string, target os.FileInfo) bool {
	for candidate := filepath.Clean(path); ; candidate = filepath.Dir(candidate) {
		info, err := os.Stat(candidate)
		if err == nil && os.SameFile(info, target) {
			return true
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			return false
		}
	}
}

func ensureSessionResourceAnchor(root string, id uuid.UUID, identity string, restore bool) error {
	anchorPath := filepath.Join(root, sessionResourceAnchorName)
	info, err := os.Lstat(anchorPath)
	switch {
	case err == nil:
		private, securityErr := sessionResourcePathIsPrivate(anchorPath, info, false)
		if securityErr != nil {
			return &SessionResourceStorageError{Kind: SessionResourceStorageUnavailable, Path: anchorPath, Cause: securityErr}
		}
		if !private {
			return &SessionResourceStorageError{Kind: SessionResourceStorageAnchorCorrupt, Path: anchorPath}
		}
		return validateSessionResourceAnchor(anchorPath, id, identity)
	case !errors.Is(err, os.ErrNotExist):
		return &SessionResourceStorageError{Kind: SessionResourceStorageUnavailable, Path: anchorPath, Cause: err}
	case restore:
		return &SessionResourceStorageError{Kind: SessionResourceStorageAnchorMissing, Path: anchorPath}
	}

	payload, err := json.Marshal(sessionResourceAnchor{
		Version: sessionResourceAnchorVersion, SessionID: id.String(), Identity: identity,
	})
	if err != nil {
		return &SessionResourceStorageError{Kind: SessionResourceStorageInvalid, Cause: err}
	}
	payload = append(payload, '\n')
	temporary, err := os.CreateTemp(root, ".harness-session-resource-anchor-*")
	if err != nil {
		return &SessionResourceStorageError{Kind: SessionResourceStorageUnavailable, Path: root, Cause: err}
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := protectSessionResourceFile(temporaryPath, temporary); err != nil {
		_ = temporary.Close()
		return &SessionResourceStorageError{Kind: SessionResourceStorageUnavailable, Path: temporaryPath, Cause: err}
	}
	if _, err := temporary.Write(payload); err != nil {
		_ = temporary.Close()
		return &SessionResourceStorageError{Kind: SessionResourceStorageUnavailable, Path: temporaryPath, Cause: err}
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return &SessionResourceStorageError{Kind: SessionResourceStorageUnavailable, Path: temporaryPath, Cause: err}
	}
	if err := temporary.Close(); err != nil {
		return &SessionResourceStorageError{Kind: SessionResourceStorageUnavailable, Path: temporaryPath, Cause: err}
	}
	if err := commitSessionResourceAnchor(temporaryPath, anchorPath); err != nil {
		if _, statErr := os.Lstat(anchorPath); statErr == nil {
			return validateSessionResourceAnchor(anchorPath, id, identity)
		}
		return &SessionResourceStorageError{Kind: SessionResourceStorageUnavailable, Path: anchorPath, Cause: err}
	}
	rootHandle, err := os.OpenRoot(root)
	if err != nil {
		return &SessionResourceStorageError{Kind: SessionResourceStorageUnavailable, Path: root, Cause: err}
	}
	syncErr := syncSessionResourceDirectory(rootHandle)
	rootCloseErr := rootHandle.Close()
	if syncErr != nil {
		return &SessionResourceStorageError{Kind: SessionResourceStorageUnavailable, Path: root, Cause: syncErr}
	}
	if rootCloseErr != nil {
		return &SessionResourceStorageError{Kind: SessionResourceStorageUnavailable, Path: root, Cause: rootCloseErr}
	}
	return nil
}

func validateSessionResourceAnchor(path string, id uuid.UUID, identity string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &SessionResourceStorageError{Kind: SessionResourceStorageAnchorMissing, Path: path}
		}
		return &SessionResourceStorageError{Kind: SessionResourceStorageUnavailable, Path: path, Cause: err}
	}
	private, securityErr := sessionResourcePathIsPrivate(path, info, false)
	if securityErr != nil {
		return &SessionResourceStorageError{Kind: SessionResourceStorageUnavailable, Path: path, Cause: securityErr}
	}
	if !private || info.Size() <= 0 || info.Size() > 1024 {
		return &SessionResourceStorageError{Kind: SessionResourceStorageAnchorCorrupt, Path: path}
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return &SessionResourceStorageError{Kind: SessionResourceStorageUnavailable, Path: path, Cause: err}
	}
	defer root.Close()
	file, err := root.Open(filepath.Base(path))
	if err != nil {
		return &SessionResourceStorageError{Kind: SessionResourceStorageUnavailable, Path: path, Cause: err}
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return &SessionResourceStorageError{Kind: SessionResourceStorageUnavailable, Path: path, Cause: err}
	}
	openedPrivate, securityErr := sessionResourcePathIsPrivate(path, openedInfo, false)
	if securityErr != nil {
		return &SessionResourceStorageError{Kind: SessionResourceStorageUnavailable, Path: path, Cause: securityErr}
	}
	if !os.SameFile(info, openedInfo) || !openedPrivate || openedInfo.Size() != info.Size() {
		return &SessionResourceStorageError{Kind: SessionResourceStorageAnchorCorrupt, Path: path}
	}
	decoder := json.NewDecoder(io.LimitReader(file, 1025))
	decoder.DisallowUnknownFields()
	var anchor sessionResourceAnchor
	if err := decoder.Decode(&anchor); err != nil {
		return &SessionResourceStorageError{Kind: SessionResourceStorageAnchorCorrupt, Path: path, Cause: err}
	}
	if anchor.Version != sessionResourceAnchorVersion || anchor.SessionID == "" || anchor.Identity == "" {
		return &SessionResourceStorageError{Kind: SessionResourceStorageAnchorCorrupt, Path: path}
	}
	if anchor.SessionID != id.String() || anchor.Identity != identity {
		return &SessionResourceStorageError{Kind: SessionResourceStorageIdentityMismatch, Path: path}
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return &SessionResourceStorageError{Kind: SessionResourceStorageAnchorCorrupt, Path: path, Cause: err}
	}
	return nil
}

func withSessionResources(resources *sessionResources) Option {
	return func(session *Session) {
		session.resources = resources
	}
}

func withSessionResourceStorageResolver(resolve SessionResourceStorageResolver) Option {
	return func(session *Session) {
		session.resourceStorageResolver = resolve
	}
}

func processBindingFor(definition loop.Definition, resources *sessionResources) (*tool.ProcessBinding, error) {
	if definition.ToolRequirements()&tool.RequiresProcessServices == 0 {
		return nil, nil
	}
	if definition.Engine() != loop.EngineNative {
		return nil, &ProcessServicesUnsupportedError{Engine: definition.Engine()}
	}
	if resources == nil {
		return nil, &SessionResourceStorageError{Kind: SessionResourceStorageUnavailable}
	}
	return &tool.ProcessBinding{Registry: resources}, nil
}

// sessionResources is the session-private implementation of the public resource
// registry. Its storage root is fixed at construction and every key maps to a
// stable opaque child name, so an untrusted key cannot escape or disclose a host
// path through traversal syntax.
type sessionResources struct {
	storageRoot string

	processServiceBridge *sessionProcessServiceBridge

	mu      sync.Mutex
	entries map[string]*sessionResourceEntry

	activated        bool
	services         tool.SessionResourceServices
	activateStarted  bool
	activateFinished bool
	activateDone     chan struct{}
	activateErr      error

	// suspended is the TEMPORARY, resumable admission gate a live workspace
	// rewind (workspace_restore.go's RestoreWorkspace) holds for the duration of
	// the rewind — distinct from closing, which is the terminal, once-only
	// Shutdown latch. See suspendAdmission/resumeAdmission.
	suspended bool

	closing         bool
	shutdownStarted bool
	shutdownDone    chan struct{}
	shutdownErr     error
}

type sessionResourceEntry struct {
	key              string
	creationDone     chan struct{}
	creationFinished bool
	creationErr      error
	resource         tool.SessionResource

	publication     chan struct{}
	publicationOpen bool
	usabilityErr    error

	activationPending bool

	mu sync.Mutex

	activateStarted bool
	activateDone    chan struct{}
	activateErr     error

	shutdownStarted bool
	shutdownDone    chan struct{}
	shutdownErr     error
}

// sessionResourceErrorSet preserves the already-sorted operation order while
// retaining errors.Is/errors.As traversal for every cause.
type sessionResourceErrorSet struct {
	Operation string
	Causes    []error
}

func (e *sessionResourceErrorSet) Error() string {
	return fmt.Sprintf("session: resource %s failed", e.Operation)
}

func (e *sessionResourceErrorSet) Unwrap() []error {
	return e.Causes
}

func newSessionResources(storageRoot string) *sessionResources {
	processServiceBridge, services, err := newSessionProcessServices()
	if err != nil {
		panic("sessionruntime: invalid process service bridge: " + err.Error())
	}
	return &sessionResources{
		storageRoot:          filepath.Clean(storageRoot),
		processServiceBridge: processServiceBridge,
		entries:              make(map[string]*sessionResourceEntry),
		services:             services,
	}
}

var _ tool.SessionResourceRegistry = (*sessionResources)(nil)

func (r *sessionResources) GetOrCreate(
	ctx context.Context,
	key string,
	factory func(string) (tool.SessionResource, error),
) (tool.SessionResource, error) {
	ctx = nonNilSessionResourceContext(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if key == "" {
		return nil, errSessionResourceKeyEmpty
	}
	if factory == nil {
		return nil, errSessionResourceFactory
	}

	for {
		r.mu.Lock()
		if r.closing {
			r.mu.Unlock()
			return nil, errSessionResourcesClosing
		}
		if r.suspended {
			r.mu.Unlock()
			return nil, errSessionResourcesSuspended
		}
		if r.activateErr != nil {
			err := r.activateErr
			r.mu.Unlock()
			return nil, err
		}
		if existing := r.entries[key]; existing != nil {
			r.mu.Unlock()
			return r.awaitResource(ctx, existing)
		}
		if r.activateStarted && !r.activateFinished {
			done := r.activateDone
			r.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		entry := &sessionResourceEntry{
			key:             key,
			creationDone:    make(chan struct{}),
			publication:     make(chan struct{}),
			publicationOpen: true,
			shutdownDone:    make(chan struct{}),
		}
		r.entries[key] = entry
		path := r.resourcePath(key)
		r.mu.Unlock()

		// The registry itself provisions the private, owner-only per-key
		// resource directory before ever handing it to a factory
		// (pkg/tool.SessionResourceRegistry's own doc comment: "The factory
		// receives its private storage directory" -- received, not
		// self-provisioned). Reuses the exact same secure primitive the
		// session-level resource root one level up is created with
		// (createPrivateSessionResourceRoot, session_resource_storage_*.go),
		// so both roots share one 0700 security convention. A factory (or,
		// as is the common real case, that resource's later use, e.g.
		// tools/process.ManifestStore.Save) must never have to MkdirAll its
		// own storage root itself, and two resources racing to create the
		// identical directory would otherwise be redundant, error-prone work
		// every factory would have to duplicate.
		var resource tool.SessionResource
		createErr := createPrivateSessionResourceRoot(path)
		if createErr == nil {
			// Factories run without a registry lock and must return; their
			// public contract has no cancellation parameter.
			resource, createErr = factory(path)
		}
		if createErr == nil && nilSessionResource(resource) {
			createErr = errSessionResourceNil
		}
		if createErr != nil {
			// A failed factory transfers no resource ownership. Clearing even a
			// typed-nil or partial result keeps concurrent shutdown panic-free.
			resource = nil
		}
		activateHere := r.finishCreation(key, entry, resource, createErr)
		if activateHere {
			activateErr := entry.activate(ctx, r.servicesForActivation())
			if finishErr := r.finishActivation(entry, activateErr, ctx); finishErr != nil {
				return nil, finishErr
			}
		}
		return r.awaitResource(ctx, entry)
	}
}

func (r *sessionResources) finishCreation(
	key string,
	entry *sessionResourceEntry,
	resource tool.SessionResource,
	createErr error,
) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	entry.creationFinished = true
	entry.creationErr = createErr
	entry.resource = resource
	close(entry.creationDone)
	if createErr != nil {
		entry.usabilityErr = createErr
		entry.activationPending = false
		if r.entries[key] == entry {
			delete(r.entries, key)
		}
		r.closePublicationLocked(entry)
		return false
	}
	if r.closing {
		r.closePublicationLocked(entry)
		return false
	}
	if entry.activationPending {
		return false
	}
	if r.activated {
		entry.activationPending = true
		return true
	}
	r.closePublicationLocked(entry)
	return false
}

func (r *sessionResources) servicesForActivation() tool.SessionResourceServices {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.services
}

func (r *sessionResources) resourcePath(key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(r.storageRoot, hex.EncodeToString(sum[:]))
}

func (r *sessionResources) awaitResource(
	ctx context.Context,
	entry *sessionResourceEntry,
) (tool.SessionResource, error) {
	for {
		r.mu.Lock()
		publication := entry.publication
		r.mu.Unlock()

		select {
		case <-publication:
		case <-ctx.Done():
			return nil, ctx.Err()
		}

		r.mu.Lock()
		if publication != entry.publication {
			r.mu.Unlock()
			continue
		}
		usabilityErr := entry.usabilityErr
		resource := entry.resource
		closing := r.closing
		r.mu.Unlock()
		if usabilityErr != nil {
			return nil, usabilityErr
		}
		if !closing {
			return resource, nil
		}
		<-entry.shutdownComplete()
		return nil, combineSessionResourceErrors(
			"lookup",
			errSessionResourcesClosing,
			entry.shutdownResult(),
		)
	}
}

// Activate late-binds live session services exactly once. Under the registry
// lock it moves every admitted entry to a new usability gate; lookups and
// in-flight creators then join that gate until activation and failure cleanup
// finish. Resources admitted after a successful boundary activate themselves
// synchronously before publication.
func (r *sessionResources) Activate(ctx context.Context) error {
	ctx = nonNilSessionResourceContext(ctx)

	r.mu.Lock()
	if r.activateStarted {
		done := r.activateDone
		r.mu.Unlock()
		return awaitSessionResourceOperation(ctx, done, func() error {
			r.mu.Lock()
			defer r.mu.Unlock()
			return r.activateErr
		})
	}
	if r.closing {
		r.mu.Unlock()
		return errSessionResourcesClosing
	}
	r.activateStarted = true
	r.activateDone = make(chan struct{})
	r.activated = true
	services := r.services
	snapshot := r.snapshotLocked()
	for _, entry := range snapshot {
		entry.activationPending = true
		if entry.creationFinished && entry.creationErr == nil {
			r.openPublicationLocked(entry)
		}
	}
	r.mu.Unlock()

	causes := make([]error, 0, len(snapshot))
	activationEntries := make([]*sessionResourceEntry, 0, len(snapshot))
	for _, entry := range snapshot {
		<-entry.creationDone
		r.mu.Lock()
		createErr := entry.creationErr
		r.mu.Unlock()
		if createErr != nil {
			continue
		}
		activationEntries = append(activationEntries, entry)

		activateErr := ctx.Err()
		if activateErr == nil {
			// Activate runs without the registry lock. Implementations must
			// honor ctx and return; bounded owner cleanup is added separately.
			activateErr = entry.activate(ctx, services)
		}
		if activateErr != nil {
			causes = append(causes, activateErr)
		}
	}
	result := combineSessionResourceErrors("activation", causes...)

	if result != nil {
		r.mu.Lock()
		// Publish the terminal failure to newly requested keys before cleanup so
		// a resource callback may safely reenter the registry without waiting
		// for the activation operation that owns that callback.
		r.activateErr = result
		for _, entry := range activationEntries {
			entry.activationPending = false
			entry.usabilityErr = result
		}
		r.mu.Unlock()

		for _, entry := range activationEntries {
			if err := entry.shutdown(context.WithoutCancel(ctx)); err != nil {
				causes = append(causes, err)
			}
		}
		result = combineSessionResourceErrors("activation", causes...)
	}

	r.mu.Lock()
	r.activateErr = result
	r.activateFinished = true
	for _, entry := range activationEntries {
		entry.activationPending = false
		entry.usabilityErr = result
		if result != nil && r.entries[entry.key] == entry {
			delete(r.entries, entry.key)
		}
		r.closePublicationLocked(entry)
	}
	close(r.activateDone)
	r.mu.Unlock()
	return result
}

func (r *sessionResources) finishActivation(
	entry *sessionResourceEntry,
	activateErr error,
	ctx context.Context,
) error {
	r.mu.Lock()
	entry.activationPending = false
	entry.usabilityErr = activateErr
	if activateErr == nil {
		r.closePublicationLocked(entry)
		r.mu.Unlock()
		return nil
	}
	// Publish the per-key terminal error while retaining its tombstone. A
	// Shutdown callback may then reenter GetOrCreate for this key without
	// waiting on the cleanup operation it currently owns.
	r.closePublicationLocked(entry)
	r.mu.Unlock()

	// Failed activation must never publish a live resource. Shutdown is invoked
	// outside the registry lock and must honor its context and return. Task 25
	// adds the bounded owner-cleanup policy around this contract.
	shutdownErr := entry.shutdown(context.WithoutCancel(ctx))
	result := combineSessionResourceErrors("activation cleanup", activateErr, shutdownErr)

	r.mu.Lock()
	entry.usabilityErr = result
	if r.entries[entry.key] == entry {
		delete(r.entries, entry.key)
	}
	r.mu.Unlock()
	return result
}

func (r *sessionResources) openPublicationLocked(entry *sessionResourceEntry) {
	if entry.publicationOpen {
		return
	}
	entry.publication = make(chan struct{})
	entry.publicationOpen = true
}

func (r *sessionResources) closePublicationLocked(entry *sessionResourceEntry) {
	if !entry.publicationOpen {
		return
	}
	close(entry.publication)
	entry.publicationOpen = false
}

// Shutdown closes admission before taking its deterministic snapshot. It then
// waits for every admitted factory and shuts resources down in key order.
// Repeated callers join the same teardown and receive the same result.
func (r *sessionResources) Shutdown(ctx context.Context) error {
	ctx = nonNilSessionResourceContext(ctx)

	r.mu.Lock()
	if r.shutdownStarted {
		done := r.shutdownDone
		r.mu.Unlock()
		<-done
		r.mu.Lock()
		result := r.shutdownErr
		r.mu.Unlock()
		return combineSessionResourceErrors("shutdown", result, ctx.Err())
	}
	r.shutdownStarted = true
	r.shutdownDone = make(chan struct{})
	r.closing = true
	snapshot := r.snapshotLocked()
	r.mu.Unlock()

	causes := make([]error, 0, len(snapshot)+1)
	for _, entry := range snapshot {
		<-entry.creationDone
		if entry.resource == nil {
			continue
		}
		if err := entry.shutdown(ctx); err != nil {
			causes = append(causes, err)
		}
	}
	if err := ctx.Err(); err != nil {
		causes = append(causes, err)
	}
	result := combineSessionResourceErrors("shutdown", causes...)

	r.mu.Lock()
	r.shutdownErr = result
	close(r.shutdownDone)
	r.mu.Unlock()
	return result
}

// suspendAdmission is workspace_restore.go's seam for a manual workspace
// rewind: it TEMPORARILY blocks new GetOrCreate admissions without touching
// any already-admitted resource and without latching the registry's terminal
// closing/shutdownStarted state — unlike Shutdown, it never calls a resource's
// own Shutdown. A registered SessionResource is a long-lived, per-definition
// supervisor (often shared by several tool definitions), not a one-shot
// handle for a single running process, so tearing it down here would destroy
// state a later GetOrCreate under the SAME key needs and would double-count
// against the ONE Shutdown call a resource's whole-session lifecycle
// contract expects. Stopping an already-admitted resource's live
// process—and thereby releasing the workspace lifetime lease that resource
// holds—remains the existing cooperative contract: the caller either waits
// (the checkpoint permit below already only proceeds once every writable
// lifetime lease is released by its holder) or the resource is stopped
// through its own ordinary tool-level path before requesting the rewind.
func (r *sessionResources) suspendAdmission() {
	r.mu.Lock()
	r.suspended = true
	r.mu.Unlock()
}

// resumeAdmission lifts a prior suspendAdmission so a later GetOrCreate
// resumes building resources normally. It is idempotent and safe to call
// even after the registry's terminal Shutdown has started (closing stays
// authoritative over suspended in GetOrCreate's admission check).
func (r *sessionResources) resumeAdmission() {
	r.mu.Lock()
	r.suspended = false
	r.mu.Unlock()
}

func (r *sessionResources) snapshotLocked() []*sessionResourceEntry {
	keys := make([]string, 0, len(r.entries))
	for key := range r.entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	snapshot := make([]*sessionResourceEntry, 0, len(keys))
	for _, key := range keys {
		snapshot = append(snapshot, r.entries[key])
	}
	return snapshot
}

func (e *sessionResourceEntry) activate(
	ctx context.Context,
	services tool.SessionResourceServices,
) error {
	e.mu.Lock()
	if e.activateStarted {
		done := e.activateDone
		e.mu.Unlock()
		return awaitSessionResourceOperation(ctx, done, func() error {
			e.mu.Lock()
			defer e.mu.Unlock()
			return e.activateErr
		})
	}
	if e.shutdownStarted {
		e.mu.Unlock()
		return errSessionResourcesClosing
	}
	e.activateStarted = true
	e.activateDone = make(chan struct{})
	e.mu.Unlock()

	// Resource callbacks run without registry locks. Activate must honor ctx and
	// return; Task 25 supplies the bounded owner policy around this contract.
	err := e.resource.Activate(ctx, services)

	e.mu.Lock()
	e.activateErr = err
	close(e.activateDone)
	e.mu.Unlock()
	return err
}

func (e *sessionResourceEntry) shutdown(ctx context.Context) error {
	e.mu.Lock()
	if e.shutdownStarted {
		done := e.shutdownDone
		e.mu.Unlock()
		<-done
		return e.shutdownResult()
	}
	e.shutdownStarted = true
	activateDone := e.activateDone
	e.mu.Unlock()

	if activateDone != nil {
		<-activateDone
	}
	// Shutdown likewise runs without registry locks and must honor ctx and
	// return. The session owner supplies its bounded cleanup context.
	err := e.resource.Shutdown(ctx)

	e.mu.Lock()
	e.shutdownErr = err
	close(e.shutdownDone)
	e.mu.Unlock()
	return err
}

func (e *sessionResourceEntry) shutdownComplete() <-chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.shutdownDone
}

func (e *sessionResourceEntry) shutdownResult() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.shutdownErr
}

func awaitSessionResourceOperation(ctx context.Context, done <-chan struct{}, result func() error) error {
	select {
	case <-done:
		return result()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func combineSessionResourceErrors(operation string, causes ...error) error {
	filtered := make([]error, 0, len(causes))
	for _, cause := range causes {
		if cause != nil {
			filtered = append(filtered, cause)
		}
	}
	switch len(filtered) {
	case 0:
		return nil
	case 1:
		return filtered[0]
	default:
		return &sessionResourceErrorSet{Operation: operation, Causes: filtered}
	}
}

func nonNilSessionResourceContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func nilSessionResource(resource tool.SessionResource) bool {
	if resource == nil {
		return true
	}
	value := reflect.ValueOf(resource)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
