// Package catalog contains explicit credential catalog backends.
package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sort"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/looprig/credentials"
	"github.com/looprig/secrets"
)

const (
	// Filename is the only catalog data file in a local catalog root.
	Filename     = "catalog.json"
	LockFilename = ".catalog.lock"

	maxCatalogBytes = 256 << 10
	tempPrefix      = ".catalog.tmp-"
	maxRecordCount  = 4096
	SchemaV1        = credentials.RecordSchemaV1
	CatalogSchemaV1 = SchemaV1
)

var (
	errAbsent   = errors.New("catalog: absent")
	errInsecure = errors.New("catalog: insecure filesystem entry")
	errOversize = errors.New("catalog: oversized catalog")
)

// Hooks are failure-injection seams used by tests. Production callers should
// leave them nil. Mutation hooks execute while the cross-process lock is held;
// lifetime hooks run at the corresponding lock boundary.
type Hooks struct {
	BeforeRead  func() error
	BeforeReady func() error
	BeforeClose func() error
	// BeforeInitialize runs only for a newly O_EXCL-created catalog file,
	// while the constructor holds the cross-process lock.
	BeforeInitialize func() error
	BeforeTempWrite  func() error
	BeforeRename     func() error
	AfterRename      func() error
	BeforeUnlink     func() error
	AfterUnlink      func() error
	SyncDir          func() error
}

// Options configures NewWithOptions.
type Options struct{ Hooks Hooks }

// Local is a strict owner-only catalog backed by one bounded JSON file. The
// platform implementation retains an open root descriptor and lock handle so
// path replacement cannot redirect operations to a different directory.
type Local struct {
	root     string
	impl     platform
	ops      Options
	life     sync.RWMutex
	mutation chan struct{}
	closed   bool
}

type LocalCatalog = Local

var _ credentials.Catalog = (*Local)(nil)
var _ credentials.CatalogCAS = (*Local)(nil)

// New opens a local catalog rooted at an explicit absolute directory.
func New(root string) (*Local, error) { return NewWithOptions(root, Options{}) }

// NewLocal is a descriptive alias for New.
func NewLocal(root string) (*Local, error) { return New(root) }

// NewCatalog is a descriptive alias for New.
func NewCatalog(root string) (*Local, error) { return New(root) }

func NewLocalCatalog(root string) (*Local, error) { return New(root) }

// Open is an alias for New.
func Open(root string) (*Local, error) { return New(root) }

// NewWithOptions opens a local catalog with test-only failure hooks.
func NewWithOptions(root string, options Options) (*Local, error) {
	impl, err := openPlatform(root)
	if err != nil {
		if errors.Is(err, errUnsupported) {
			return nil, credentials.NewCatalogUnsupportedError()
		}
		if errors.Is(err, errInsecure) {
			return nil, credentials.ErrCatalogCorrupt
		}
		return nil, credentials.NewCatalogUnavailableError()
	}
	local := &Local{root: impl.root(), impl: impl, ops: options, mutation: make(chan struct{}, 1)}
	if err := local.initialize(); err != nil {
		_ = impl.close()
		return nil, err
	}
	return local, nil
}

// Root returns the explicit clean path supplied to New.
func (l *Local) Root() string {
	if l == nil {
		return ""
	}
	return l.root
}

// Path returns the path at which the data file is presented. It is for
// diagnostics and tests; callers must use Catalog methods for access.
func (l *Local) Path() string {
	if l == nil {
		return ""
	}
	return l.root + string(os.PathSeparator) + Filename
}

// Close releases the held root and lock descriptors. It is idempotent.
func (l *Local) Close() error {
	if l == nil {
		return nil
	}
	if l.ops.Hooks.BeforeClose != nil {
		if err := l.ops.Hooks.BeforeClose(); err != nil {
			return mapHookError(err)
		}
	}
	l.life.Lock()
	defer l.life.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	if l.impl == nil {
		return nil
	}
	return l.impl.close()
}

func (l *Local) acquire(ctx context.Context) (func(), error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if l == nil || l.mutation == nil {
		return nil, credentials.NewCatalogUnavailableError()
	}
	select {
	case l.mutation <- struct{}{}:
		return func() { <-l.mutation }, nil
	case <-ctx.Done():
		return nil, credentials.NewCatalogCanceledError(ctx.Err())
	}
}

func (l *Local) initialize() error {
	unlocks, err := l.impl.lock(context.Background())
	if err != nil {
		return mapPlatformError(err)
	}
	defer unlocks()
	if err := l.impl.validateRoot(); err != nil {
		return mapPlatformError(err)
	}
	if err := l.impl.validateLock(); err != nil {
		return mapPlatformError(err)
	}
	created, err := l.impl.ensureData()
	if err != nil {
		return mapPlatformError(err)
	}
	data, err := l.impl.read(Filename)
	if err != nil {
		return mapPlatformError(err)
	}
	if len(data) != 0 {
		return nil
	}
	if !created {
		return credentials.NewCatalogCorruptError()
	}
	if l.ops.Hooks.BeforeInitialize != nil {
		if err := l.ops.Hooks.BeforeInitialize(); err != nil {
			return mapHookError(err)
		}
	}
	if err := l.impl.validateRoot(); err != nil {
		return mapPlatformError(err)
	}
	if err := l.impl.validateLock(); err != nil {
		return mapPlatformError(err)
	}
	empty, err := encodeCatalog([]credentials.Record{})
	if err != nil {
		return credentials.NewCatalogCorruptError()
	}
	temp, err := l.impl.writeTemp(empty)
	if err != nil {
		return mapPlatformError(err)
	}
	keep := true
	defer func() {
		if keep {
			_, _ = l.impl.remove(temp)
		}
	}()
	if err := l.impl.rename(temp, Filename); err != nil {
		return mapPlatformError(err)
	}
	keep = false
	// Constructor initialization is outside caller mutation hooks. Hooks
	// describe publication operations so tests can inject visible-commit
	// durability uncertainty without making the catalog impossible to open.
	if err := l.impl.syncDir(); err != nil {
		return credentials.NewCatalogDurabilityUnknownError(credentials.Reference{})
	}
	return nil
}

func (l *Local) ready(ctx context.Context) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if l == nil || l.impl == nil {
		return credentials.NewCatalogUnavailableError()
	}
	// Every caller holds life.RLock before calling ready. A recursive RLock is
	// unsafe: if Close is waiting for that read lock, RWMutex blocks the second
	// reader indefinitely. Keep the lifetime check under the caller's existing
	// read lock instead of acquiring it again here.
	if l.ops.Hooks.BeforeReady != nil {
		if err := l.ops.Hooks.BeforeReady(); err != nil {
			return mapHookError(err)
		}
	}
	if l.closed {
		return credentials.NewCatalogUnavailableError()
	}
	if err := l.impl.validateRoot(); err != nil {
		return mapPlatformError(err)
	}
	if err := l.impl.validateLock(); err != nil {
		return mapPlatformError(err)
	}
	return nil
}

func (l *Local) withLock(ctx context.Context) (func(), error) {
	unlocks, err := l.impl.lock(ctx)
	if err != nil {
		return nil, mapPlatformError(err)
	}
	if err := l.impl.validateRoot(); err != nil {
		unlocks()
		return nil, mapPlatformError(err)
	}
	if err := l.impl.validateLock(); err != nil {
		unlocks()
		return nil, mapPlatformError(err)
	}
	return unlocks, nil
}

// Get resolves exactly one safe record. The complete file is validated before
// the requested record is returned.
func (l *Local) Get(ctx context.Context, ref credentials.Reference) (credentials.Record, error) {
	if l == nil {
		return credentials.Record{}, credentials.NewCatalogUnavailableError()
	}
	if err := ref.Validate(); err != nil {
		return credentials.Record{}, credentials.ErrCatalogNotFound
	}
	if err := contextError(ctx); err != nil {
		return credentials.Record{}, err
	}
	l.life.RLock()
	defer l.life.RUnlock()
	release, err := l.acquire(ctx)
	if err != nil {
		return credentials.Record{}, err
	}
	defer release()
	if err := l.ready(ctx); err != nil {
		return credentials.Record{}, err
	}
	unlocks, err := l.withLock(ctx)
	if err != nil {
		return credentials.Record{}, err
	}
	defer unlocks()
	if l.ops.Hooks.BeforeRead != nil {
		if err := l.ops.Hooks.BeforeRead(); err != nil {
			return credentials.Record{}, mapHookError(err)
		}
	}
	records, err := l.readRecords()
	if err != nil {
		return credentials.Record{}, err
	}
	for _, record := range records {
		if record.Reference == ref {
			return record, nil
		}
	}
	return credentials.Record{}, credentials.NewCatalogNotFoundError(ref)
}

// List validates and returns all records in stable canonical-reference order.
func (l *Local) List(ctx context.Context) ([]credentials.Record, error) {
	if l == nil {
		return nil, credentials.NewCatalogUnavailableError()
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	l.life.RLock()
	defer l.life.RUnlock()
	release, err := l.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	if err := l.ready(ctx); err != nil {
		return nil, err
	}
	unlocks, err := l.withLock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlocks()
	if l.ops.Hooks.BeforeRead != nil {
		if err := l.ops.Hooks.BeforeRead(); err != nil {
			return nil, mapHookError(err)
		}
	}
	records, err := l.readRecords()
	if err != nil {
		return nil, err
	}
	return append([]credentials.Record(nil), records...), nil
}

// Create publishes one record only when its reference is absent. The file
// rename is the mutation linearization point; after a successful rename a
// canceled context does not turn the committed mutation into a failure.
func (l *Local) Create(ctx context.Context, record credentials.Record) error {
	if l == nil {
		return credentials.NewCatalogUnavailableError()
	}
	if err := credentials.ValidateRecord(record); err != nil {
		return err
	}
	if err := contextError(ctx); err != nil {
		return err
	}
	l.life.RLock()
	defer l.life.RUnlock()
	release, err := l.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	if err := l.ready(ctx); err != nil {
		return err
	}
	unlocks, err := l.withLock(ctx)
	if err != nil {
		return err
	}
	defer unlocks()
	records, err := l.readRecords()
	if err != nil {
		return err
	}
	for _, existing := range records {
		if existing.Reference == record.Reference {
			return credentials.NewCatalogConflictError(record.Reference)
		}
	}
	records = append(records, record)
	sortRecords(records)
	return l.commit(ctx, records, record.Reference)
}

// Delete removes one catalog record. It is deliberately a catalog-only
// operation; state deletion is the caller's separate exact-version action.
func (l *Local) Delete(ctx context.Context, ref credentials.Reference) error {
	if l == nil {
		return credentials.NewCatalogUnavailableError()
	}
	if err := ref.Validate(); err != nil {
		return credentials.ErrCatalogNotFound
	}
	if err := contextError(ctx); err != nil {
		return err
	}
	l.life.RLock()
	defer l.life.RUnlock()
	release, err := l.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	if err := l.ready(ctx); err != nil {
		return err
	}
	unlocks, err := l.withLock(ctx)
	if err != nil {
		return err
	}
	defer unlocks()
	records, err := l.readRecords()
	if err != nil {
		return err
	}
	index := -1
	for i, existing := range records {
		if existing.Reference == ref {
			index = i
			break
		}
	}
	if index < 0 {
		return credentials.NewCatalogNotFoundError(ref)
	}
	records = append(records[:index], records[index+1:]...)
	if l.ops.Hooks.BeforeUnlink != nil {
		// The catalog remains unavailable only after the replacement is visible;
		// this hook is a cancellation/failure seam before linearization.
		if err := l.ops.Hooks.BeforeUnlink(); err != nil {
			return mapHookError(err)
		}
	}
	return l.commit(ctx, records, ref)
}

// Update performs an optional compare-and-swap replacement. Both records are
// validated and the expected record must match byte-for-byte at the mutation
// point. Create and Delete never weaken their own semantics.
func (l *Local) Update(ctx context.Context, expected, next credentials.Record) error {
	if l == nil {
		return credentials.NewCatalogUnavailableError()
	}
	if err := credentials.ValidateRecord(expected); err != nil {
		return err
	}
	if err := credentials.ValidateRecord(next); err != nil {
		return err
	}
	if expected.Reference != next.Reference {
		return credentials.NewCatalogConflictError(next.Reference)
	}
	if err := contextError(ctx); err != nil {
		return err
	}
	l.life.RLock()
	defer l.life.RUnlock()
	release, err := l.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	if err := l.ready(ctx); err != nil {
		return err
	}
	unlocks, err := l.withLock(ctx)
	if err != nil {
		return err
	}
	defer unlocks()
	records, err := l.readRecords()
	if err != nil {
		return err
	}
	for i, existing := range records {
		if existing.Reference == expected.Reference {
			if existing != expected {
				return credentials.NewCatalogConflictError(expected.Reference)
			}
			records[i] = next
			sortRecords(records)
			return l.commit(ctx, records, next.Reference)
		}
	}
	return credentials.NewCatalogConflictError(expected.Reference)
}

func (l *Local) readRecords() ([]credentials.Record, error) {
	data, err := l.impl.read(Filename)
	if err != nil {
		if errors.Is(err, errAbsent) {
			return nil, credentials.NewCatalogCorruptError()
		}
		return nil, mapPlatformError(err)
	}
	if len(data) == 0 {
		return nil, credentials.NewCatalogCorruptError()
	}
	records, err := decodeCatalog(data)
	if err != nil {
		return nil, credentials.NewCatalogCorruptError()
	}
	return records, nil
}

func (l *Local) commit(ctx context.Context, records []credentials.Record, ref credentials.Reference) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	data, err := encodeCatalog(records)
	if err != nil {
		return credentials.NewCatalogCorruptError()
	}
	if l.ops.Hooks.BeforeTempWrite != nil {
		if err := l.ops.Hooks.BeforeTempWrite(); err != nil {
			return mapHookError(err)
		}
	}
	temp, err := l.impl.writeTemp(data)
	if err != nil {
		return mapPlatformError(err)
	}
	keep := true
	defer func() {
		if keep {
			_, _ = l.impl.remove(temp)
		}
	}()
	if err := contextError(ctx); err != nil {
		return err
	}
	if l.ops.Hooks.BeforeRename != nil {
		if err := l.ops.Hooks.BeforeRename(); err != nil {
			return mapHookError(err)
		}
	}
	if err := contextError(ctx); err != nil {
		return err
	}
	if err := l.impl.validateRoot(); err != nil {
		return mapPlatformError(err)
	}
	if err := l.impl.validateLock(); err != nil {
		return mapPlatformError(err)
	}
	if err := l.impl.rename(temp, Filename); err != nil {
		return mapPlatformError(err)
	}
	keep = false
	if l.ops.Hooks.AfterRename != nil {
		if err := l.ops.Hooks.AfterRename(); err != nil {
			return credentials.NewCatalogDurabilityUnknownError(ref)
		}
	}
	if err := l.syncDir(); err != nil {
		return credentials.NewCatalogDurabilityUnknownError(ref)
	}
	if l.ops.Hooks.AfterUnlink != nil {
		if err := l.ops.Hooks.AfterUnlink(); err != nil {
			return credentials.NewCatalogDurabilityUnknownError(ref)
		}
	}
	return nil
}

func (l *Local) syncDir() error {
	if l.ops.Hooks.SyncDir != nil {
		return l.ops.Hooks.SyncDir()
	}
	return l.impl.syncDir()
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return credentials.NewCatalogCanceledError(context.Canceled)
	}
	if err := ctx.Err(); err != nil {
		return credentials.NewCatalogCanceledError(err)
	}
	return nil
}

func mapHookError(err error) error {
	if errors.Is(err, credentials.ErrCanceled) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return credentials.NewCatalogCanceledError(err)
	}
	return credentials.NewCatalogUnavailableError()
}

func mapPlatformError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, errUnsupported):
		return credentials.NewCatalogUnsupportedError()
	case errors.Is(err, errInsecure):
		return credentials.ErrCatalogCorrupt
	case errors.Is(err, errOversize):
		return credentials.ErrCatalogCorrupt
	case errors.Is(err, errAbsent):
		return credentials.ErrCatalogCorrupt
	case errors.Is(err, credentials.ErrCanceled), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return credentials.NewCatalogCanceledError(err)
	default:
		return credentials.NewCatalogUnavailableError()
	}
}

func sortRecords(records []credentials.Record) {
	sort.Slice(records, func(i, j int) bool { return records[i].Reference.String() < records[j].Reference.String() })
}

type diskCatalog struct {
	Schema  uint32       `json:"schema"`
	Records []diskRecord `json:"records"`
}

type diskRecord struct {
	Schema     uint32         `json:"schema"`
	Reference  string         `json:"reference"`
	Descriptor diskDescriptor `json:"descriptor"`
	State      string         `json:"state"`
	CreatedAt  time.Time      `json:"created_at"`
	UpdatedAt  time.Time      `json:"updated_at"`
}

type diskDescriptor struct {
	Provider  string `json:"provider"`
	Transport string `json:"transport"`
	Scheme    string `json:"scheme"`
	Usage     string `json:"usage"`
	Issuer    string `json:"issuer"`
	Audience  string `json:"audience"`
	Label     string `json:"label"`
}

func encodeCatalog(records []credentials.Record) ([]byte, error) {
	if len(records) > maxRecordCount {
		return nil, errOversize
	}
	disk := diskCatalog{Schema: credentials.RecordSchemaV1, Records: make([]diskRecord, 0, len(records))}
	for _, record := range records {
		if err := credentials.ValidateRecord(record); err != nil {
			return nil, err
		}
		state, err := record.State.MarshalText()
		if err != nil {
			return nil, err
		}
		disk.Records = append(disk.Records, diskRecord{
			Schema: record.Schema, Reference: record.Reference.String(),
			Descriptor: diskDescriptor{Provider: record.Descriptor.Provider, Transport: record.Descriptor.Transport, Scheme: string(record.Descriptor.Scheme), Usage: string(record.Descriptor.Usage), Issuer: record.Descriptor.Issuer, Audience: record.Descriptor.Audience, Label: record.Descriptor.Label},
			State:      string(state), CreatedAt: record.CreatedAt.UTC(), UpdatedAt: record.UpdatedAt.UTC(),
		})
	}
	data, err := json.Marshal(disk)
	if err != nil || len(data) > maxCatalogBytes {
		return nil, errOversize
	}
	return data, nil
}

func decodeCatalog(data []byte) ([]credentials.Record, error) {
	if len(data) == 0 || len(data) > maxCatalogBytes || !utf8.Valid(data) {
		return nil, errOversize
	}
	if err := rejectDuplicateJSON(data); err != nil {
		return nil, err
	}
	var disk diskCatalog
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&disk); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("trailing")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, errors.New("trailing")
	}
	if disk.Schema != credentials.RecordSchemaV1 || disk.Records == nil || len(disk.Records) > maxRecordCount {
		return nil, errors.New("schema")
	}
	records := make([]credentials.Record, 0, len(disk.Records))
	seen := make(map[string]struct{}, len(disk.Records))
	for _, item := range disk.Records {
		if item.Schema != credentials.RecordSchemaV1 {
			return nil, errors.New("record schema")
		}
		ref, err := credentials.ParseReference(item.Reference)
		if err != nil {
			return nil, err
		}
		if _, exists := seen[ref.String()]; exists {
			return nil, errors.New("duplicate reference")
		}
		seen[ref.String()] = struct{}{}
		state, err := secrets.ParseReference(item.State)
		if err != nil {
			return nil, err
		}
		descriptor, err := credentials.NewDescriptor(item.Descriptor.Provider, item.Descriptor.Transport, credentials.Scheme(item.Descriptor.Scheme), secretsUsage(item.Descriptor.Usage), item.Descriptor.Issuer, item.Descriptor.Audience, item.Descriptor.Label)
		if err != nil {
			return nil, err
		}
		record, err := credentials.NewRecord(ref, descriptor, state, item.CreatedAt, item.UpdatedAt)
		if err != nil {
			return nil, err
		}
		if err := credentials.ValidateRecord(record); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	sortRecords(records)
	return records, nil
}

func secretsUsage(raw string) credentials.UsageClass { return credentials.UsageClass(raw) }

// rejectDuplicateJSON parses structure tokens once to enforce the stricter
// duplicate-key policy that encoding/json intentionally does not provide.
func rejectDuplicateJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	var walk func() error
	walk = func() error {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		switch delim := tok.(type) {
		case json.Delim:
			switch delim {
			case '{':
				seen := map[string]struct{}{}
				for dec.More() {
					key, err := dec.Token()
					if err != nil {
						return err
					}
					keyString, ok := key.(string)
					if !ok {
						return errors.New("object key")
					}
					if _, exists := seen[keyString]; exists {
						return errors.New("duplicate")
					}
					seen[keyString] = struct{}{}
					if err := walk(); err != nil {
						return err
					}
				}
				end, err := dec.Token()
				if err != nil || end != json.Delim('}') {
					return errors.New("object end")
				}
			case '[':
				for dec.More() {
					if err := walk(); err != nil {
						return err
					}
				}
				end, err := dec.Token()
				if err != nil || end != json.Delim(']') {
					return errors.New("array end")
				}
			default:
				return errors.New("delimiter")
			}
		}
		return nil
	}
	if err := walk(); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing")
	}
	return nil
}
