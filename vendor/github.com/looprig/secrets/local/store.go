// Package local implements an owner-only, descriptor-relative local secret
// store. The root is always supplied explicitly by the caller.
package local

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/looprig/secrets"
)

const (
	envelopeSchema       = 1
	maxEnvelopeBytes     = 2*secrets.MaxSecretSize + 16*1024
	maxTimestampTextSize = 64
	filePrefix           = "r-"
	fileSuffix           = ".json"
	lockName             = ".store.lock"
	tempPrefix           = ".tmp-"
	localScheme          = "local"
	maxListRecords       = 4096
	maxListReadBytes     = 8 << 20
	maxSnapshotBytes     = 4 << 20
	maxSnapshots         = 64
	maxListPrefixBytes   = 16 << 10
)

var (
	errAbsent    = errors.New("local: absent")
	errInsecure  = errors.New("local: insecure filesystem entry")
	errOversized = errors.New("local: oversized record")
)

// ErrListTooLarge reports a directory whose bounded scan would exceed the
// local store's work and memory budget.
var ErrListTooLarge = errors.New("local: listing exceeds bounded record limit")

// ErrPageTokenExpired identifies a continuation token that was evicted,
// consumed, or belongs to another store instance.
var ErrPageTokenExpired = errors.New("local: page token expired")

// PageTokenExpiredError reports stateful cursor expiry without exposing the
// token itself. A continuation token is one-time use and is never replayable.
type PageTokenExpiredError struct {
	Reason string
}

func (e *PageTokenExpiredError) Error() string {
	if e == nil || e.Reason == "" {
		return ErrPageTokenExpired.Error()
	}
	return "local: page token expired (" + e.Reason + ")"
}

func (e *PageTokenExpiredError) Unwrap() error { return ErrPageTokenExpired }

// Hooks are optional test seams for exercising cancellation, mutation
// linearization, and durability rules. Production callers should leave all
// fields nil. Mutation hooks run while the store's cross-process lock is
// held; read hooks run immediately before their unresolved filesystem read.
type Hooks struct {
	BeforeRead         func() error
	BeforeExistingRead func() error
	BeforeVersion      func() error
	BeforeTempWrite    func() error
	BeforeWrite        func() error
	BeforeRename       func() error
	AfterRename        func() error
	BeforeUnlink       func() error
	AfterUnlink        func() error
	SyncDir            func() error
	NewVersion         func() (secrets.Version, error)
}

// Options configures NewWithOptions. The zero value is the normal production
// configuration.
type Options struct {
	Hooks Hooks
}

// Store is a concurrent-safe local secret store. A Store owns an open handle
// to its root directory for its entire lifetime.
type Store struct {
	root          string
	ops           Options
	impl          platform
	life          sync.RWMutex
	mutationCh    chan struct{}
	snapshotMu    sync.Mutex
	snapshots     map[string]listSnapshot
	snapshotBytes int
	closed        bool
}

type listSnapshot struct {
	namespace secrets.Namespace
	items     []secrets.Metadata
	next      int
	bytes     int
}

// New opens or creates an owner-only store rooted at root. root must be an
// absolute, clean path; no environment variable or process home is consulted.
func New(root string) (*Store, error) {
	return NewWithOptions(root, Options{})
}

// NewStore is an explicit alias for New for callers that prefer a noun-style
// constructor.
func NewStore(root string) (*Store, error) { return New(root) }

// Open is an alias for New.
func Open(root string) (*Store, error) { return New(root) }

// NewWithOptions opens a store with optional failure-injection hooks.
func NewWithOptions(root string, options Options) (*Store, error) {
	impl, err := openPlatform(root)
	if err != nil {
		if errors.Is(err, errInsecure) {
			return nil, secrets.NewInsecurePathError("path")
		}
		return nil, err
	}
	return &Store{
		root:       root,
		ops:        options,
		impl:       impl,
		mutationCh: make(chan struct{}, 1),
		snapshots:  make(map[string]listSnapshot),
	}, nil
}

// Root returns the clean root path supplied to the constructor.
func (s *Store) Root() string {
	if s == nil {
		return ""
	}
	return s.root
}

// Filename returns the single path component used for ref. It is useful for
// diagnostics and tests; callers must not use it to bypass Store methods.
func (s *Store) Filename(ref secrets.Reference) string { return filenameFor(ref) }

func (s *Store) filenameFor(ref secrets.Reference) string { return filenameFor(ref) }

// Close releases the held root and lock descriptors. It is idempotent.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.life.Lock()
	defer s.life.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	s.snapshotMu.Lock()
	clear(s.snapshots)
	s.snapshots = nil
	s.snapshotBytes = 0
	s.snapshotMu.Unlock()
	if s.impl == nil {
		return nil
	}
	return s.impl.close()
}

// SupportsCreateOnly reports affirmative create-only support.
func (s *Store) SupportsCreateOnly() bool { return s != nil && s.impl != nil }

// SupportsCompareAndSwap reports affirmative CAS support.
func (s *Store) SupportsCompareAndSwap() bool { return s != nil && s.impl != nil }

// Resolve loads one record and validates its envelope before returning it.
func (s *Store) Resolve(ctx context.Context, ref secrets.Reference) (secrets.Record, error) {
	if s == nil {
		return secrets.Record{}, secrets.NewUnavailableError("resolve", ref)
	}
	s.life.RLock()
	defer s.life.RUnlock()
	if err := s.ready(ctx, ref, "resolve"); err != nil {
		return secrets.Record{}, err
	}
	if err := s.preIO(ctx, "resolve", ref, s.ops.Hooks.BeforeRead); err != nil {
		return secrets.Record{}, err
	}
	data, err := s.impl.read(filenameFor(ref))
	if err != nil {
		if errors.Is(err, errAbsent) {
			return secrets.Record{}, secrets.NewNotFoundError(ref)
		}
		return secrets.Record{}, s.mapFilesystemError("read", ref, err)
	}
	defer clearBytes(data)
	if err := contextError(ctx, "resolve"); err != nil {
		return secrets.Record{}, err
	}
	record, err := decodeRecord(ref, data)
	if err != nil {
		return secrets.Record{}, secrets.NewCorruptRecordError(ref)
	}
	if err := contextError(ctx, "resolve"); err != nil {
		return secrets.Record{}, err
	}
	return record, nil
}

// acquireMutation serializes mutations and first-page scans within one Store
// without making a canceled caller wait behind a hook-blocked operation.
func (s *Store) acquireMutation(ctx context.Context, operation string, ref secrets.Reference) (func(), error) {
	if err := contextError(ctx, operation); err != nil {
		return nil, err
	}
	if s.mutationCh == nil {
		return nil, secrets.NewUnavailableError(operation, ref)
	}
	select {
	case s.mutationCh <- struct{}{}:
		return func() { <-s.mutationCh }, nil
	case <-ctx.Done():
		return nil, secrets.NewCanceledError(operation, ctx.Err())
	}
}

// Put stores value under ref according to options. Rename is the mutation
// linearization point; after it succeeds the old value is never reported as
// surviving.
func (s *Store) Put(ctx context.Context, ref secrets.Reference, value secrets.Secret, options secrets.PutOptions) (secrets.Record, error) {
	if s == nil {
		return secrets.Record{}, secrets.NewUnavailableError("put", ref)
	}
	s.life.RLock()
	defer s.life.RUnlock()
	releaseMutation, err := s.acquireMutation(ctx, "put", ref)
	if err != nil {
		return secrets.Record{}, err
	}
	defer releaseMutation()
	if err := s.ready(ctx, ref, "put"); err != nil {
		return secrets.Record{}, err
	}
	if err := options.Validate(); err != nil {
		return secrets.Record{}, err
	}
	if err := value.Validate(); err != nil {
		return secrets.Record{}, err
	}
	unlock, err := s.impl.lock(ctx)
	if err != nil {
		return secrets.Record{}, s.mapFilesystemError("write", ref, err)
	}
	defer unlock()
	if err := contextError(ctx, "put"); err != nil {
		return secrets.Record{}, err
	}
	if err := s.validateRoot("put", ref); err != nil {
		return secrets.Record{}, err
	}
	if err := s.impl.validateLock(); err != nil {
		return secrets.Record{}, s.mapFilesystemError("write", ref, err)
	}
	if err := s.preIO(ctx, "write", ref, s.ops.Hooks.BeforeRead); err != nil {
		return secrets.Record{}, err
	}

	name := filenameFor(ref)
	if err := contextError(ctx, "put"); err != nil {
		return secrets.Record{}, err
	}
	state, err := s.impl.entry(name)
	if err != nil {
		return secrets.Record{}, s.mapFilesystemError("write", ref, err)
	}
	if err := contextError(ctx, "put"); err != nil {
		return secrets.Record{}, err
	}
	if state.insecure {
		return secrets.Record{}, secrets.NewInsecurePathError("path")
	}
	var existing *secrets.Record
	if state.exists {
		if err := s.preIO(ctx, "read", ref, s.ops.Hooks.BeforeExistingRead); err != nil {
			return secrets.Record{}, err
		}
		data, readErr := s.impl.read(name)
		if readErr != nil {
			return secrets.Record{}, s.mapFilesystemError("read", ref, readErr)
		}
		defer clearBytes(data)
		if err := contextError(ctx, "put"); err != nil {
			return secrets.Record{}, err
		}
		record, decodeErr := decodeRecord(ref, data)
		if decodeErr != nil {
			return secrets.Record{}, secrets.NewCorruptRecordError(ref)
		}
		existing = &record
	}
	switch options.Precondition {
	case secrets.PreconditionCreateOnly:
		if existing != nil {
			return secrets.Record{}, secrets.NewConflictError(ref)
		}
	case secrets.PreconditionCompareAndSwap:
		if existing == nil || existing.Version != options.ExpectedVersion {
			return secrets.Record{}, secrets.NewConflictError(ref)
		}
	}

	current := secrets.Version{}
	if existing != nil {
		current = existing.Version
	}
	if err := s.preIO(ctx, "write", ref, s.ops.Hooks.BeforeVersion); err != nil {
		return secrets.Record{}, err
	}
	version, err := s.nextVersion(ctx, current)
	if err != nil {
		if errors.Is(err, secrets.ErrCanceled) {
			return secrets.Record{}, err
		}
		return secrets.Record{}, secrets.NewUnavailableError("write", ref)
	}
	record := secrets.Record{Reference: ref, Value: value, Version: version, UpdatedAt: time.Now().UTC()}
	if err := record.Validate(); err != nil {
		return secrets.Record{}, err
	}
	data, err := encodeRecord(record)
	if err != nil {
		return secrets.Record{}, secrets.NewUnavailableError("write", ref)
	}
	defer clearBytes(data)
	if err := s.preIO(ctx, "write", ref, s.ops.Hooks.BeforeWrite); err != nil {
		return secrets.Record{}, err
	}
	if err := s.preIO(ctx, "write", ref, s.ops.Hooks.BeforeTempWrite); err != nil {
		return secrets.Record{}, err
	}
	temp, err := s.impl.writeTemp(data)
	if err != nil {
		return secrets.Record{}, s.mapFilesystemError("write", ref, err)
	}
	removeTemp := true
	defer func() {
		if removeTemp {
			_, _ = s.impl.remove(temp)
		}
	}()
	if err := contextError(ctx, "put"); err != nil {
		return secrets.Record{}, err
	}
	if s.ops.Hooks.BeforeRename != nil {
		if err := s.ops.Hooks.BeforeRename(); err != nil {
			return secrets.Record{}, s.mapHookError("write", ref, err)
		}
	}
	if err := contextError(ctx, "put"); err != nil {
		return secrets.Record{}, err
	}
	if err := s.validateRoot("put", ref); err != nil {
		return secrets.Record{}, err
	}
	if err := s.impl.validateLock(); err != nil {
		return secrets.Record{}, s.mapFilesystemError("write", ref, err)
	}
	if err := s.impl.rename(temp, name); err != nil {
		return secrets.Record{}, s.mapFilesystemError("write", ref, err)
	}
	removeTemp = false
	if s.ops.Hooks.AfterRename != nil {
		if err := s.ops.Hooks.AfterRename(); err != nil {
			return record, &CommitVisibleDurabilityUnknownError{operation: commitOperationPut, reference: ref}
		}
	}
	if err := s.syncDir(); err != nil {
		return record, &CommitVisibleDurabilityUnknownError{operation: commitOperationPut, reference: ref}
	}
	return record, nil
}

// Delete removes exactly ref. Unlink is the deletion linearization point and
// absent references are idempotent.
func (s *Store) Delete(ctx context.Context, ref secrets.Reference, options secrets.DeleteOptions) (secrets.DeleteResult, error) {
	if s == nil {
		return secrets.DeleteResult{}, secrets.NewUnavailableError("delete", ref)
	}
	s.life.RLock()
	defer s.life.RUnlock()
	releaseMutation, err := s.acquireMutation(ctx, "delete", ref)
	if err != nil {
		return secrets.DeleteResult{}, err
	}
	defer releaseMutation()
	if err := s.ready(ctx, ref, "delete"); err != nil {
		return secrets.DeleteResult{}, err
	}
	if err := options.Validate(); err != nil {
		return secrets.DeleteResult{}, err
	}
	unlock, err := s.impl.lock(ctx)
	if err != nil {
		return secrets.DeleteResult{}, s.mapFilesystemError("delete", ref, err)
	}
	defer unlock()
	if err := contextError(ctx, "delete"); err != nil {
		return secrets.DeleteResult{}, err
	}
	if err := s.validateRoot("delete", ref); err != nil {
		return secrets.DeleteResult{}, err
	}
	if err := s.impl.validateLock(); err != nil {
		return secrets.DeleteResult{}, s.mapFilesystemError("delete", ref, err)
	}
	if err := s.preIO(ctx, "delete", ref, s.ops.Hooks.BeforeRead); err != nil {
		return secrets.DeleteResult{}, err
	}
	name := filenameFor(ref)
	if err := contextError(ctx, "delete"); err != nil {
		return secrets.DeleteResult{}, err
	}
	state, err := s.impl.entry(name)
	if err != nil {
		return secrets.DeleteResult{}, s.mapFilesystemError("delete", ref, err)
	}
	if err := contextError(ctx, "delete"); err != nil {
		return secrets.DeleteResult{}, err
	}
	if state.insecure {
		return secrets.DeleteResult{}, secrets.NewInsecurePathError("path")
	}
	if !state.exists {
		if options.Precondition == secrets.PreconditionCompareAndSwap {
			return secrets.DeleteResult{}, secrets.NewConflictError(ref)
		}
		result, resultErr := secrets.NewDeleteResult(ref, secrets.DeleteStatusAbsent, secrets.VersionUnsupported)
		return result, resultErr
	}
	if err := s.preIO(ctx, "read", ref, s.ops.Hooks.BeforeExistingRead); err != nil {
		return secrets.DeleteResult{}, err
	}
	data, err := s.impl.read(name)
	if err != nil {
		return secrets.DeleteResult{}, s.mapFilesystemError("read", ref, err)
	}
	defer clearBytes(data)
	if err := contextError(ctx, "delete"); err != nil {
		return secrets.DeleteResult{}, err
	}
	record, err := decodeRecord(ref, data)
	if err != nil {
		return secrets.DeleteResult{}, secrets.NewCorruptRecordError(ref)
	}
	if err := contextError(ctx, "delete"); err != nil {
		return secrets.DeleteResult{}, err
	}
	if options.Precondition == secrets.PreconditionCompareAndSwap && record.Version != options.ExpectedVersion {
		return secrets.DeleteResult{}, secrets.NewConflictError(ref)
	}
	if s.ops.Hooks.BeforeUnlink != nil {
		if err := s.ops.Hooks.BeforeUnlink(); err != nil {
			return secrets.DeleteResult{}, s.mapHookError("delete", ref, err)
		}
	}
	if err := contextError(ctx, "delete"); err != nil {
		return secrets.DeleteResult{}, err
	}
	if err := s.validateRoot("delete", ref); err != nil {
		return secrets.DeleteResult{}, err
	}
	if err := s.impl.validateLock(); err != nil {
		return secrets.DeleteResult{}, s.mapFilesystemError("delete", ref, err)
	}
	removed, err := s.impl.remove(name)
	if err != nil {
		return secrets.DeleteResult{}, s.mapFilesystemError("delete", ref, err)
	}
	if !removed {
		if options.Precondition == secrets.PreconditionCompareAndSwap {
			return secrets.DeleteResult{}, secrets.NewConflictError(ref)
		}
		absent, absentErr := secrets.NewDeleteResult(ref, secrets.DeleteStatusAbsent, secrets.VersionUnsupported)
		return absent, absentErr
	}
	result, err := secrets.NewDeleteResult(ref, secrets.DeleteStatusDeleted, record.Version)
	if err != nil {
		return secrets.DeleteResult{}, err
	}
	if s.ops.Hooks.AfterUnlink != nil {
		if hookErr := s.ops.Hooks.AfterUnlink(); hookErr != nil {
			return result, &CommitVisibleDurabilityUnknownError{operation: commitOperationDelete, reference: ref}
		}
	}
	if err := s.syncDir(); err != nil {
		return result, &CommitVisibleDurabilityUnknownError{operation: commitOperationDelete, reference: ref}
	}
	return result, nil
}

// List returns metadata only, constrained to namespace and a bounded page.
// The first call captures a bounded in-memory snapshot; continuation tokens
// identify that snapshot and cannot cross stores or namespaces.
func (s *Store) List(ctx context.Context, namespace secrets.Namespace, token secrets.PageToken, limit int) (secrets.Page[secrets.Metadata], error) {
	if s == nil {
		return secrets.Page[secrets.Metadata]{}, secrets.NewUnavailableError("list", secrets.Reference{})
	}
	s.life.RLock()
	defer s.life.RUnlock()
	if s.impl == nil || s.closed {
		return secrets.Page[secrets.Metadata]{}, secrets.NewUnavailableError("list", secrets.Reference{})
	}
	if err := contextError(ctx, "list"); err != nil {
		return secrets.Page[secrets.Metadata]{}, err
	}
	if namespace.IsZero() {
		return secrets.Page[secrets.Metadata]{}, secrets.NewInvalidNamespaceError("prefix")
	}
	if namespace.Scheme() != localScheme {
		return secrets.Page[secrets.Metadata]{}, secrets.NewUnsupportedSchemeError()
	}
	if limit <= 0 || limit > secrets.MaxPageItems {
		return secrets.Page[secrets.Metadata]{}, secrets.NewInvalidOptionsError("page limit")
	}
	if !token.Valid() {
		return secrets.Page[secrets.Metadata]{}, secrets.NewInvalidPageTokenError("page token")
	}
	if token.IsZero() {
		return s.listFirstPage(ctx, namespace, limit)
	}
	id, err := parseSnapshotToken(token)
	if err != nil {
		return secrets.Page[secrets.Metadata]{}, err
	}
	return s.listContinuation(ctx, namespace, id, limit)
}

func (s *Store) listFirstPage(ctx context.Context, namespace secrets.Namespace, limit int) (secrets.Page[secrets.Metadata], error) {
	releaseMutation, err := s.acquireMutation(ctx, "list", secrets.Reference{})
	if err != nil {
		return secrets.Page[secrets.Metadata]{}, err
	}
	defer releaseMutation()
	unlock, err := s.impl.lock(ctx)
	if err != nil {
		return secrets.Page[secrets.Metadata]{}, s.mapFilesystemError("list", secrets.Reference{}, err)
	}
	defer unlock()
	if err := s.validateRoot("list", secrets.Reference{}); err != nil {
		return secrets.Page[secrets.Metadata]{}, err
	}
	if err := s.impl.validateLock(); err != nil {
		return secrets.Page[secrets.Metadata]{}, s.mapFilesystemError("list", secrets.Reference{}, err)
	}
	if err := s.preIO(ctx, "list", secrets.Reference{}, s.ops.Hooks.BeforeRead); err != nil {
		return secrets.Page[secrets.Metadata]{}, err
	}
	names, err := s.impl.names(maxListRecords)
	if err != nil {
		return secrets.Page[secrets.Metadata]{}, s.mapFilesystemError("list", secrets.Reference{}, err)
	}
	if err := contextError(ctx, "list"); err != nil {
		return secrets.Page[secrets.Metadata]{}, err
	}
	sort.Strings(names)
	items := make([]secrets.Metadata, 0, len(names))
	readBytes := 0
	for _, name := range names {
		if err := contextError(ctx, "list"); err != nil {
			return secrets.Page[secrets.Metadata]{}, err
		}
		if !strings.HasPrefix(name, filePrefix) || !strings.HasSuffix(name, fileSuffix) {
			continue
		}
		metadata, included, readErr := s.readListMetadata(ctx, namespace, name, &readBytes)
		if readErr != nil {
			if errors.Is(readErr, context.Canceled) || errors.Is(readErr, context.DeadlineExceeded) || errors.Is(readErr, secrets.ErrCanceled) {
				return secrets.Page[secrets.Metadata]{}, s.mapFilesystemError("list", secrets.Reference{}, readErr)
			}
			if errors.Is(readErr, errInsecure) {
				return secrets.Page[secrets.Metadata]{}, secrets.NewInsecurePathError("path")
			}
			if errors.Is(readErr, ErrListTooLarge) {
				return secrets.Page[secrets.Metadata]{}, ErrListTooLarge
			}
			return secrets.Page[secrets.Metadata]{}, secrets.NewCorruptRecordError(metadata.Reference)
		}
		if included {
			items = append(items, metadata)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Reference.Canonical() < items[j].Reference.Canonical() })
	if len(items) <= limit {
		return secrets.NewPage(items, secrets.PageToken{})
	}
	id, err := s.saveSnapshot(ctx, namespace, items, limit)
	if err != nil {
		if errors.Is(err, secrets.ErrCanceled) {
			return secrets.Page[secrets.Metadata]{}, err
		}
		if errors.Is(err, ErrListTooLarge) {
			return secrets.Page[secrets.Metadata]{}, ErrListTooLarge
		}
		return secrets.Page[secrets.Metadata]{}, secrets.NewUnavailableError("list", secrets.Reference{})
	}
	next, err := newSnapshotToken(id)
	if err != nil {
		return secrets.Page[secrets.Metadata]{}, secrets.NewUnavailableError("list", secrets.Reference{})
	}
	return secrets.NewPage(items[:limit], next)
}

func (s *Store) listContinuation(ctx context.Context, namespace secrets.Namespace, id string, limit int) (secrets.Page[secrets.Metadata], error) {
	releaseMutation, err := s.acquireMutation(ctx, "list", secrets.Reference{})
	if err != nil {
		return secrets.Page[secrets.Metadata]{}, err
	}
	defer releaseMutation()
	unlock, err := s.impl.lock(ctx)
	if err != nil {
		return secrets.Page[secrets.Metadata]{}, s.mapFilesystemError("list", secrets.Reference{}, err)
	}
	defer unlock()
	if err := contextError(ctx, "list"); err != nil {
		return secrets.Page[secrets.Metadata]{}, err
	}
	if err := s.validateRoot("list", secrets.Reference{}); err != nil {
		return secrets.Page[secrets.Metadata]{}, err
	}
	if err := s.impl.validateLock(); err != nil {
		return secrets.Page[secrets.Metadata]{}, s.mapFilesystemError("list", secrets.Reference{}, err)
	}
	if err := contextError(ctx, "list"); err != nil {
		return secrets.Page[secrets.Metadata]{}, err
	}
	s.snapshotMu.Lock()
	snapshot, ok := s.snapshots[id]
	if !ok {
		s.snapshotMu.Unlock()
		return secrets.Page[secrets.Metadata]{}, &PageTokenExpiredError{Reason: "evicted"}
	}
	if snapshot.namespace != namespace {
		s.snapshotMu.Unlock()
		return secrets.Page[secrets.Metadata]{}, secrets.NewInvalidPageTokenError("namespace")
	}
	if snapshot.next < 0 || snapshot.next >= len(snapshot.items) {
		s.snapshotMu.Unlock()
		return secrets.Page[secrets.Metadata]{}, &PageTokenExpiredError{Reason: "invalidated"}
	}
	if err := contextError(ctx, "list"); err != nil {
		s.snapshotMu.Unlock()
		return secrets.Page[secrets.Metadata]{}, err
	}
	start := snapshot.next
	end := start + limit
	if end > len(snapshot.items) {
		end = len(snapshot.items)
	}
	delete(s.snapshots, id)
	next := secrets.PageToken{}
	if end < len(snapshot.items) {
		nextID, rotateErr := s.rotateSnapshotLocked(ctx, snapshot, id, end)
		if rotateErr != nil {
			// Restore the consumed cursor if issuing its successor failed.
			s.snapshots[id] = snapshot
			s.snapshotMu.Unlock()
			if errors.Is(rotateErr, secrets.ErrCanceled) {
				return secrets.Page[secrets.Metadata]{}, rotateErr
			}
			return secrets.Page[secrets.Metadata]{}, secrets.NewUnavailableError("list", secrets.Reference{})
		}
		next, err = newSnapshotToken(nextID)
		if err != nil {
			s.snapshotMu.Unlock()
			return secrets.Page[secrets.Metadata]{}, secrets.NewUnavailableError("list", secrets.Reference{})
		}
	} else {
		s.snapshotBytes -= snapshot.bytes
	}
	items := snapshot.items[start:end]
	s.snapshotMu.Unlock()
	return secrets.NewPage(items, next)
}

func (s *Store) readListMetadata(ctx context.Context, namespace secrets.Namespace, name string, readBytes *int) (secrets.Metadata, bool, error) {
	if !strings.HasPrefix(name, filePrefix) || !strings.HasSuffix(name, fileSuffix) {
		return secrets.Metadata{}, false, nil
	}
	state, err := s.impl.entry(name)
	if err != nil {
		return secrets.Metadata{}, false, err
	}
	if state.insecure {
		return secrets.Metadata{}, false, errInsecure
	}
	if !state.exists {
		return secrets.Metadata{}, false, nil
	}
	filenameRef, filenameErr := referenceFromFilename(name)
	if filenameErr == nil && !namespace.Contains(filenameRef) {
		// The decodable filename is authoritative, so unrelated records are
		// skipped before their serialized values enter memory.
		return secrets.Metadata{}, false, nil
	}
	if filenameErr != nil {
		encoded := strings.TrimSuffix(strings.TrimPrefix(name, filePrefix), fileSuffix)
		if !strings.HasPrefix(encoded, "h-") {
			return secrets.Metadata{}, false, filenameErr
		}
	}
	var (
		data    []byte
		readErr error
	)
	if filenameErr != nil {
		data, readErr = s.impl.readPrefix(name, maxListPrefixBytes)
	} else {
		data, readErr = s.impl.read(name)
	}
	if readErr != nil {
		return secrets.Metadata{}, false, readErr
	}
	defer clearBytes(data)
	if err := contextError(ctx, "list"); err != nil {
		return secrets.Metadata{}, false, err
	}
	if filenameErr != nil {
		ref, referenceErr := decodeReferencePrefix(data)
		if referenceErr != nil {
			return secrets.Metadata{}, false, referenceErr
		}
		if filenameFor(ref) != name {
			return secrets.Metadata{Reference: ref}, false, errors.New("filename/reference mismatch")
		}
		if !namespace.Contains(ref) {
			return secrets.Metadata{}, false, nil
		}
		clearBytes(data)
		data, readErr = s.impl.read(name)
		if readErr != nil {
			return secrets.Metadata{}, false, readErr
		}
		defer clearBytes(data)
		if err := contextError(ctx, "list"); err != nil {
			return secrets.Metadata{}, false, err
		}
	}
	if readBytes == nil || *readBytes > maxListReadBytes-len(data) {
		return secrets.Metadata{}, false, ErrListTooLarge
	}
	*readBytes += len(data)
	if filenameErr == nil {
		metadata, err := decodeMetadata(filenameRef, data)
		if err != nil {
			return secrets.Metadata{Reference: filenameRef}, false, err
		}
		return metadata, true, nil
	}
	ref, referenceErr := decodeReferenceOnly(data)
	if referenceErr != nil {
		return secrets.Metadata{}, false, referenceErr
	}
	if !namespace.Contains(ref) {
		return secrets.Metadata{}, false, nil
	}
	metadata, metadataErr := decodeMetadata(ref, data)
	if metadataErr != nil {
		return secrets.Metadata{Reference: ref}, false, metadataErr
	}
	return metadata, true, nil
}

func (s *Store) saveSnapshot(ctx context.Context, namespace secrets.Namespace, items []secrets.Metadata, next int) (string, error) {
	if next < 0 || next >= len(items) {
		return "", errors.New("invalid snapshot offset")
	}
	cost := snapshotCost(items)
	if cost > maxSnapshotBytes {
		return "", ErrListTooLarge
	}
	var raw [16]byte
	for attempt := 0; attempt < 4; attempt++ {
		if err := contextError(ctx, "list"); err != nil {
			return "", err
		}
		if _, err := io.ReadFull(rand.Reader, raw[:]); err != nil {
			return "", err
		}
		if err := contextError(ctx, "list"); err != nil {
			return "", err
		}
		id := hex.EncodeToString(raw[:])
		s.snapshotMu.Lock()
		if _, exists := s.snapshots[id]; exists {
			s.snapshotMu.Unlock()
			continue
		}
		for len(s.snapshots) >= maxSnapshots || s.snapshotBytes+cost > maxSnapshotBytes {
			removed := false
			for old := range s.snapshots {
				s.snapshotBytes -= s.snapshots[old].bytes
				delete(s.snapshots, old)
				removed = true
				break
			}
			if !removed {
				break
			}
		}
		copyItems := append([]secrets.Metadata(nil), items...)
		s.snapshots[id] = listSnapshot{namespace: namespace, items: copyItems, next: next, bytes: cost}
		s.snapshotBytes += cost
		s.snapshotMu.Unlock()
		return id, nil
	}
	return "", errors.New("snapshot ID collision")
}

func (s *Store) rotateSnapshotLocked(ctx context.Context, snapshot listSnapshot, oldID string, next int) (string, error) {
	var raw [16]byte
	for attempt := 0; attempt < 4; attempt++ {
		if err := contextError(ctx, "list"); err != nil {
			return "", err
		}
		if _, err := io.ReadFull(rand.Reader, raw[:]); err != nil {
			return "", err
		}
		if err := contextError(ctx, "list"); err != nil {
			return "", err
		}
		id := hex.EncodeToString(raw[:])
		if _, exists := s.snapshots[id]; exists || id == oldID {
			continue
		}
		snapshot.next = next
		s.snapshots[id] = snapshot
		return id, nil
	}
	return "", errors.New("snapshot ID collision")
}

func newSnapshotToken(id string) (secrets.PageToken, error) {
	return secrets.NewPageToken("s2:" + id)
}

func parseSnapshotToken(token secrets.PageToken) (string, error) {
	parts := strings.Split(token.String(), ":")
	if len(parts) != 2 || parts[0] != "s2" || len(parts[1]) != 32 {
		return "", secrets.NewInvalidPageTokenError("token")
	}
	if _, err := hex.DecodeString(parts[1]); err != nil {
		return "", secrets.NewInvalidPageTokenError("token")
	}
	return parts[1], nil
}

func snapshotCost(items []secrets.Metadata) int {
	cost := 0
	for _, item := range items {
		cost += len(item.Reference.Canonical()) + len(item.Version.String()) + maxTimestampTextSize + 32
	}
	return cost
}

func (s *Store) ready(ctx context.Context, ref secrets.Reference, operation string) error {
	if s == nil || s.impl == nil {
		return secrets.NewUnavailableError(operation, ref)
	}
	if s.closed {
		return secrets.NewUnavailableError(operation, ref)
	}
	if err := contextError(ctx, operation); err != nil {
		return err
	}
	if ref.IsZero() {
		return secrets.NewInvalidReferenceError("zero")
	}
	if ref.Scheme() != localScheme {
		return secrets.NewUnsupportedSchemeError()
	}
	return s.validateRoot(operation, ref)
}

func (s *Store) validateRoot(operation string, ref secrets.Reference) error {
	if err := s.impl.validateRoot(); err != nil {
		if errors.Is(err, errInsecure) {
			return secrets.NewInsecurePathError("path")
		}
		return secrets.NewUnavailableError(operation, ref)
	}
	return nil
}

func contextError(ctx context.Context, operation string) error {
	if ctx == nil {
		return secrets.NewCanceledError(operation, context.Canceled)
	}
	if err := ctx.Err(); err != nil {
		return secrets.NewCanceledError(operation, err)
	}
	return nil
}

func (s *Store) syncDir() error {
	if s.ops.Hooks.SyncDir != nil {
		return s.ops.Hooks.SyncDir()
	}
	return s.impl.syncDir()
}

func (s *Store) mapFilesystemError(operation string, ref secrets.Reference, err error) error {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return secrets.NewCanceledError(operation, err)
	case errors.Is(err, errAbsent):
		if operation == "read" {
			return secrets.NewNotFoundError(ref)
		}
		return secrets.NewUnavailableError(operation, ref)
	case errors.Is(err, errInsecure):
		return secrets.NewInsecurePathError("path")
	case errors.Is(err, errOversized):
		return secrets.NewCorruptRecordError(ref)
	case errors.Is(err, ErrListTooLarge):
		return ErrListTooLarge
	default:
		return secrets.NewUnavailableError(operation, ref)
	}
}

func (s *Store) mapHookError(operation string, ref secrets.Reference, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return secrets.NewCanceledError(operation, err)
	}
	return secrets.NewUnavailableError(operation, ref)
}

// preIO checks cancellation immediately before an unresolved filesystem
// operation and once again after an injected seam returns. A hook that
// cancels without returning an error is therefore still observed before any
// mutation linearization point.
func (s *Store) preIO(ctx context.Context, operation string, ref secrets.Reference, hook func() error) error {
	if err := contextError(ctx, operation); err != nil {
		return err
	}
	if hook != nil {
		if err := hook(); err != nil {
			return s.mapHookError(operation, ref, err)
		}
	}
	return contextError(ctx, operation)
}

func newVersion() (secrets.Version, error) {
	var raw [18]byte
	if _, err := io.ReadFull(rand.Reader, raw[:]); err != nil {
		return secrets.Version{}, err
	}
	return secrets.NewVersion(hex.EncodeToString(raw[:]))
}

func (s *Store) nextVersion(ctx context.Context, current secrets.Version) (secrets.Version, error) {
	if err := contextError(ctx, "write"); err != nil {
		return secrets.Version{}, err
	}
	for attempt := 0; attempt < 16; attempt++ {
		if err := contextError(ctx, "write"); err != nil {
			return secrets.Version{}, err
		}
		var (
			version    secrets.Version
			versionErr error
		)
		if s.ops.Hooks.NewVersion != nil {
			version, versionErr = s.ops.Hooks.NewVersion()
		} else {
			version, versionErr = newVersion()
		}
		if versionErr != nil {
			return secrets.Version{}, versionErr
		}
		if err := contextError(ctx, "write"); err != nil {
			return secrets.Version{}, err
		}
		if version.IsZero() || version.IsUnsupported() || !version.Valid() {
			continue
		}
		if !current.IsZero() && !current.IsUnsupported() && version == current {
			continue
		}
		return version, nil
	}
	return secrets.Version{}, errors.New("version collision")
}

// diskEnvelope deliberately has no exported/public counterpart. Value is
// base64 encoded by encoding/json's []byte support; only this package decodes
// it and validates the size before constructing a Secret.
type diskEnvelope struct {
	Schema    int    `json:"schema"`
	Reference string `json:"reference"`
	Value     []byte `json:"value"`
	Version   string `json:"version"`
	UpdatedAt string `json:"updated_at"`
}

func encodeRecord(record secrets.Record) ([]byte, error) {
	if err := record.Validate(); err != nil {
		return nil, err
	}
	value := record.Value.Bytes()
	defer clearBytes(value)
	envelope := diskEnvelope{
		Schema: envelopeSchema, Reference: record.Reference.Canonical(), Value: value,
		Version: record.Version.String(), UpdatedAt: record.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
	return json.Marshal(envelope)
}

func decodeRecord(expected secrets.Reference, data []byte) (secrets.Record, error) {
	if len(data) == 0 || len(data) > maxEnvelopeBytes || !utf8.Valid(data) {
		return secrets.Record{}, errOversized
	}
	var envelope diskEnvelope
	defer func() { clearBytes(envelope.Value) }()
	if err := decodeStrictEnvelope(data, &envelope); err != nil {
		return secrets.Record{}, err
	}
	if envelope.Schema != envelopeSchema || len(envelope.Reference) == 0 || len(envelope.Reference) > secrets.MaxReferenceLength || len(envelope.Version) == 0 || len(envelope.Version) > secrets.MaxVersionLength || len(envelope.UpdatedAt) == 0 || len(envelope.UpdatedAt) > maxTimestampTextSize {
		return secrets.Record{}, errors.New("invalid envelope fields")
	}
	ref, err := secrets.ParseReference(envelope.Reference)
	if err != nil || (!expected.IsZero() && ref != expected) {
		return secrets.Record{}, errors.New("invalid envelope reference")
	}
	version, err := secrets.NewVersion(envelope.Version)
	if err != nil || version.IsUnsupported() {
		return secrets.Record{}, errors.New("invalid envelope version")
	}
	updatedAt, err := time.Parse(time.RFC3339Nano, envelope.UpdatedAt)
	if err != nil || updatedAt.IsZero() {
		return secrets.Record{}, errors.New("invalid envelope timestamp")
	}
	if len(envelope.Value) == 0 || len(envelope.Value) > secrets.MaxSecretSize {
		return secrets.Record{}, errOversized
	}
	value, err := secrets.New(envelope.Value)
	if err != nil {
		return secrets.Record{}, err
	}
	record := secrets.Record{Reference: ref, Value: value, Version: version, UpdatedAt: updatedAt}
	if err := record.Validate(); err != nil {
		return secrets.Record{}, err
	}
	return record, nil
}

// decodeMetadata validates the complete strict envelope while deliberately
// avoiding secrets.New. The bounded serialized value is cleared by defer on
// every exit path; listing retains only the safe coordination fields.
func decodeMetadata(expected secrets.Reference, data []byte) (secrets.Metadata, error) {
	if len(data) == 0 || len(data) > maxEnvelopeBytes || !utf8.Valid(data) {
		return secrets.Metadata{}, errOversized
	}
	var envelope diskEnvelope
	defer func() { clearBytes(envelope.Value) }()
	if err := decodeStrictEnvelope(data, &envelope); err != nil {
		return secrets.Metadata{}, err
	}
	if envelope.Schema != envelopeSchema || len(envelope.Reference) == 0 || len(envelope.Reference) > secrets.MaxReferenceLength || len(envelope.Version) == 0 || len(envelope.Version) > secrets.MaxVersionLength || len(envelope.UpdatedAt) == 0 || len(envelope.UpdatedAt) > maxTimestampTextSize {
		return secrets.Metadata{}, errors.New("invalid envelope fields")
	}
	ref, err := secrets.ParseReference(envelope.Reference)
	if err != nil || (!expected.IsZero() && ref != expected) {
		return secrets.Metadata{}, errors.New("invalid envelope reference")
	}
	version, err := secrets.NewVersion(envelope.Version)
	if err != nil || version.IsUnsupported() {
		return secrets.Metadata{}, errors.New("invalid envelope version")
	}
	updatedAt, err := time.Parse(time.RFC3339Nano, envelope.UpdatedAt)
	if err != nil || updatedAt.IsZero() {
		return secrets.Metadata{}, errors.New("invalid envelope timestamp")
	}
	if len(envelope.Value) == 0 || len(envelope.Value) > secrets.MaxSecretSize {
		return secrets.Metadata{}, errOversized
	}
	metadata := secrets.Metadata{Reference: ref, Version: version, UpdatedAt: updatedAt}
	if err := metadata.Validate(); err != nil {
		return secrets.Metadata{}, err
	}
	return metadata, nil
}

func decodeReferencePrefix(data []byte) (secrets.Reference, error) {
	if len(data) == 0 || len(data) > maxListPrefixBytes || !utf8.Valid(data) {
		return secrets.Reference{}, errOversized
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return secrets.Reference{}, err
	}
	delim, ok := token.(json.Delim)
	if !ok || delim != '{' {
		return secrets.Reference{}, errors.New("envelope is not object")
	}
	seen := make(map[string]struct{}, 2)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return secrets.Reference{}, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return secrets.Reference{}, errors.New("envelope key is not string")
		}
		if _, duplicate := seen[key]; duplicate {
			return secrets.Reference{}, errors.New("duplicate envelope field")
		}
		seen[key] = struct{}{}
		switch key {
		case "schema":
			var schema int
			if err := decoder.Decode(&schema); err != nil {
				return secrets.Reference{}, err
			}
			if schema != envelopeSchema {
				return secrets.Reference{}, errors.New("invalid envelope schema")
			}
		case "reference":
			var rawReference string
			if err := decoder.Decode(&rawReference); err != nil {
				return secrets.Reference{}, err
			}
			if len(rawReference) == 0 || len(rawReference) > secrets.MaxReferenceLength {
				return secrets.Reference{}, errOversized
			}
			return secrets.ParseReference(rawReference)
		default:
			// The writer emits schema and reference before value. Refuse to
			// scan a value or unknown field just to discover a later reference.
			return secrets.Reference{}, errors.New("reference not in bounded envelope prefix")
		}
	}
	return secrets.Reference{}, errors.New("missing reference")
}

func decodeReferenceOnly(data []byte) (secrets.Reference, error) {
	if len(data) == 0 || len(data) > maxEnvelopeBytes {
		return secrets.Reference{}, errOversized
	}
	if !utf8.Valid(data) {
		return recoverReferenceLiteral(data)
	}
	ref, err := decodeReferenceStrict(data)
	if err == nil {
		return ref, nil
	}
	if errors.Is(err, errOversized) {
		return secrets.Reference{}, err
	}
	// Listing must not let an unrelated namespace's damaged value block a
	// metadata query. Recover only a bounded, safe reference literal; matching
	// records still go through decodeRecord and fail closed below.
	return recoverReferenceLiteral(data)
}

func decodeReferenceStrict(data []byte) (secrets.Reference, error) {
	if len(data) == 0 || len(data) > maxEnvelopeBytes || !utf8.Valid(data) {
		return secrets.Reference{}, errOversized
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return secrets.Reference{}, err
	}
	delim, ok := token.(json.Delim)
	if !ok || delim != '{' {
		return secrets.Reference{}, errors.New("envelope is not object")
	}
	var (
		rawReference  string
		seenReference bool
	)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return secrets.Reference{}, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return secrets.Reference{}, errors.New("envelope key is not string")
		}
		if key == "reference" {
			if seenReference {
				return secrets.Reference{}, errors.New("duplicate reference")
			}
			seenReference = true
			if err := decoder.Decode(&rawReference); err != nil {
				return secrets.Reference{}, err
			}
			if len(rawReference) == 0 || len(rawReference) > secrets.MaxReferenceLength {
				return secrets.Reference{}, errOversized
			}
			continue
		}
		if err := skipJSONValue(decoder, 0); err != nil {
			return secrets.Reference{}, err
		}
	}
	end, err := decoder.Token()
	if err != nil {
		return secrets.Reference{}, err
	}
	if delim, ok := end.(json.Delim); !ok || delim != '}' {
		return secrets.Reference{}, errors.New("envelope object not closed")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return secrets.Reference{}, errors.New("trailing envelope data")
	}
	if !seenReference {
		return secrets.Reference{}, errors.New("missing reference")
	}
	return secrets.ParseReference(rawReference)
}

func recoverReferenceLiteral(data []byte) (secrets.Reference, error) {
	if len(data) == 0 || len(data) > maxEnvelopeBytes {
		return secrets.Reference{}, errOversized
	}
	needle := []byte(`"reference"`)
	depth := 0
	for index := 0; index < len(data); {
		switch data[index] {
		case '"':
			start := index
			index++
			escaped := false
			for index < len(data) {
				if escaped {
					escaped = false
					index++
					continue
				}
				if data[index] == '\\' {
					escaped = true
					index++
					continue
				}
				if data[index] == '"' {
					break
				}
				index++
			}
			if index >= len(data) {
				return secrets.Reference{}, errors.New("unterminated reference scan string")
			}
			if depth == 1 && bytes.Equal(data[start:index+1], needle) {
				valueStart := index + 1
				for valueStart < len(data) && isJSONSpace(data[valueStart]) {
					valueStart++
				}
				if valueStart >= len(data) || data[valueStart] != ':' {
					index++
					continue
				}
				valueStart++
				for valueStart < len(data) && isJSONSpace(data[valueStart]) {
					valueStart++
				}
				if valueStart >= len(data) || data[valueStart] != '"' {
					return secrets.Reference{}, errors.New("invalid reference literal")
				}
				var rawReference string
				decoder := json.NewDecoder(bytes.NewReader(data[valueStart:]))
				if err := decoder.Decode(&rawReference); err != nil {
					return secrets.Reference{}, err
				}
				return secrets.ParseReference(rawReference)
			}
			index++
		case '{', '[':
			depth++
			index++
		case '}', ']':
			if depth > 0 {
				depth--
			}
			index++
		default:
			index++
		}
	}
	return secrets.Reference{}, errors.New("missing reference")
}

func isJSONSpace(value byte) bool {
	return value == ' ' || value == '\t' || value == '\r' || value == '\n'
}

func skipJSONValue(decoder *json.Decoder, depth int) error {
	if depth > 8 {
		return errors.New("nested JSON value too deep")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok || (delim != '{' && delim != '[') {
		return nil
	}
	for decoder.More() {
		if delim == '{' {
			if _, err := decoder.Token(); err != nil {
				return err
			}
		}
		if err := skipJSONValue(decoder, depth+1); err != nil {
			return err
		}
	}
	end, err := decoder.Token()
	if err != nil {
		return err
	}
	if close, ok := end.(json.Delim); !ok || (delim == '{' && close != '}') || (delim == '[' && close != ']') {
		return errors.New("nested JSON value not closed")
	}
	return nil
}

// decodeStrictEnvelope rejects unknown fields, duplicate keys, malformed
// UTF-8, trailing values, and type mismatches without retaining raw input.
func decodeStrictEnvelope(data []byte, envelope *diskEnvelope) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok || delim != '{' {
		return errors.New("envelope is not object")
	}
	seen := make(map[string]struct{}, 5)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := keyToken.(string)
		if !ok {
			return errors.New("envelope key is not string")
		}
		if _, exists := seen[key]; exists {
			return errors.New("duplicate envelope field")
		}
		seen[key] = struct{}{}
		switch key {
		case "schema":
			if err := decoder.Decode(&envelope.Schema); err != nil {
				return err
			}
		case "reference":
			if err := decoder.Decode(&envelope.Reference); err != nil {
				return err
			}
		case "value":
			if err := decoder.Decode(&envelope.Value); err != nil {
				return err
			}
		case "version":
			if err := decoder.Decode(&envelope.Version); err != nil {
				return err
			}
		case "updated_at":
			if err := decoder.Decode(&envelope.UpdatedAt); err != nil {
				return err
			}
		default:
			return errors.New("unknown envelope field")
		}
	}
	end, err := decoder.Token()
	if err != nil {
		return err
	}
	if delim, ok := end.(json.Delim); !ok || delim != '}' {
		return errors.New("envelope object not closed")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("trailing envelope data")
	}
	return nil
}

func clearBytes(value []byte) {
	for i := range value {
		value[i] = 0
	}
}

func filenameFor(ref secrets.Reference) string {
	canonical := ref.Canonical()
	encoded := base64.RawURLEncoding.EncodeToString([]byte(canonical))
	// Most references fit comfortably in one filesystem component. Hashing
	// long references keeps every component below common NAME_MAX limits while
	// the envelope retains the canonical reference for listing and validation.
	if len(encoded) > 220 {
		digest := sha256.Sum256([]byte(canonical))
		encoded = "h-" + hex.EncodeToString(digest[:])
	}
	return filePrefix + encoded + fileSuffix
}

func singleComponent(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\\")
}

func referenceFromFilename(name string) (secrets.Reference, error) {
	if !singleComponent(name) || !strings.HasPrefix(name, filePrefix) || !strings.HasSuffix(name, fileSuffix) {
		return secrets.Reference{}, errInsecure
	}
	encoded := strings.TrimSuffix(strings.TrimPrefix(name, filePrefix), fileSuffix)
	if strings.HasPrefix(encoded, "h-") {
		return secrets.Reference{}, errors.New("hashed filename")
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(raw) == 0 || len(raw) > secrets.MaxReferenceLength {
		return secrets.Reference{}, errors.New("invalid encoded filename")
	}
	return secrets.ParseReference(string(raw))
}

type commitOperation uint8

const (
	commitOperationPut commitOperation = iota + 1
	commitOperationDelete
)

// ErrDurabilityUnknown identifies a mutation whose visible state changed but
// whose directory durability could not be confirmed.
var ErrDurabilityUnknown = errors.New("local: visible commit durability unknown")

// ErrCommitVisibleDurabilityUnknown is a descriptive alias retained for
// callers that name the visible commit state explicitly.
var ErrCommitVisibleDurabilityUnknown = ErrDurabilityUnknown

// DurabilityUnknownError is an alias for the typed visible-commit failure.
type DurabilityUnknownError = CommitVisibleDurabilityUnknownError

// CommitVisibleDurabilityUnknownError reports a successful rename/unlink
// followed by failed durability bookkeeping. Visible is always true; callers
// must reread/adopt state and must not assume the old value survived.
type CommitVisibleDurabilityUnknownError struct {
	operation commitOperation
	reference secrets.Reference
}

func (e *CommitVisibleDurabilityUnknownError) Error() string {
	if e == nil {
		return ErrDurabilityUnknown.Error()
	}
	op := "put"
	if e.operation == commitOperationDelete {
		op = "delete"
	}
	if e.reference.IsZero() {
		return "local: " + op + " visible; durability unknown"
	}
	return "local: " + op + " visible for " + e.reference.Canonical() + "; durability unknown"
}

func (e *CommitVisibleDurabilityUnknownError) Unwrap() error { return ErrDurabilityUnknown }

func (e *CommitVisibleDurabilityUnknownError) Is(target error) bool {
	return target == ErrDurabilityUnknown || target == secrets.ErrUnavailable
}

// Visible reports that the rename or unlink already occurred.
func (e *CommitVisibleDurabilityUnknownError) Visible() bool { return e != nil }

// Reference returns the safe reference associated with the visible mutation.
func (e *CommitVisibleDurabilityUnknownError) Reference() secrets.Reference {
	if e == nil {
		return secrets.Reference{}
	}
	return e.reference
}

type fileState struct {
	exists   bool
	insecure bool
}

type platform interface {
	close() error
	validateRoot() error
	lock(context.Context) (func(), error)
	validateLock() error
	entry(string) (fileState, error)
	read(string) ([]byte, error)
	writeTemp([]byte) (string, error)
	rename(string, string) error
	remove(string) (bool, error)
	names(int) ([]string, error)
	readPrefix(string, int) ([]byte, error)
	syncDir() error
}

// ErrUnsupportedPlatform identifies a platform where this package cannot
// prove the required handle-relative and owner-only invariants.
var ErrUnsupportedPlatform = errors.New("local: unsupported platform")

// UnsupportedPlatformError is returned at construction time on platforms
// without the required filesystem primitives.
type UnsupportedPlatformError struct{}

func (UnsupportedPlatformError) Error() string { return ErrUnsupportedPlatform.Error() }

func (UnsupportedPlatformError) Unwrap() error { return ErrUnsupportedPlatform }
