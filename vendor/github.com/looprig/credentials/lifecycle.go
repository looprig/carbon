package credentials

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/looprig/secrets"
)

var (
	ErrOrphanState            = errors.New("credentials: orphan secret state")
	ErrStateDeleteFailed      = errors.New("credentials: state deletion failed")
	ErrStateUnavailable       = errors.New("credentials: state operation unavailable")
	ErrStateDurabilityUnknown = errors.New("credentials: state visible commit durability unknown")
)

// lifecycleCause is intentionally private. Public lifecycle errors retain
// only this closed classification rather than arbitrary backend/provider
// errors, which could contain credential material.
type lifecycleCauseKind uint8

type lifecycleCause struct {
	kind  lifecycleCauseKind
	cause error
}

const (
	lifecycleCauseNone lifecycleCauseKind = iota
	lifecycleCauseCatalogUnavailable
	lifecycleCauseCatalogCorrupt
	lifecycleCauseCatalogConflict
	lifecycleCauseCatalogNotFound
	lifecycleCauseCatalogCanceled
	lifecycleCauseCatalogDurability
	lifecycleCauseCatalogUnsupported
	lifecycleCauseStateUnavailable
	lifecycleCauseStateConflict
	lifecycleCauseStateNotFound
	lifecycleCauseStateCanceled
	lifecycleCauseStateInsecure
	lifecycleCauseStateDurability
)

var (
	causeNone               = lifecycleCause{kind: lifecycleCauseNone}
	causeCatalogUnavailable = lifecycleCause{kind: lifecycleCauseCatalogUnavailable}
	causeCatalogCorrupt     = lifecycleCause{kind: lifecycleCauseCatalogCorrupt}
	causeCatalogConflict    = lifecycleCause{kind: lifecycleCauseCatalogConflict}
	causeCatalogNotFound    = lifecycleCause{kind: lifecycleCauseCatalogNotFound}
	causeCatalogDurability  = lifecycleCause{kind: lifecycleCauseCatalogDurability}
	causeCatalogUnsupported = lifecycleCause{kind: lifecycleCauseCatalogUnsupported}
	causeStateUnavailable   = lifecycleCause{kind: lifecycleCauseStateUnavailable}
	causeStateConflict      = lifecycleCause{kind: lifecycleCauseStateConflict}
	causeStateNotFound      = lifecycleCause{kind: lifecycleCauseStateNotFound}
	causeStateInsecure      = lifecycleCause{kind: lifecycleCauseStateInsecure}
	causeStateDurability    = lifecycleCause{kind: lifecycleCauseStateDurability}
)

func (c lifecycleCause) error() error {
	switch c.kind {
	case lifecycleCauseCatalogUnavailable:
		return ErrCatalogUnavailable
	case lifecycleCauseCatalogCorrupt:
		return ErrCatalogCorrupt
	case lifecycleCauseCatalogConflict:
		return ErrCatalogConflict
	case lifecycleCauseCatalogNotFound:
		return ErrCatalogNotFound
	case lifecycleCauseCatalogCanceled:
		return newCanceledBoundaryError(ErrCatalogCanceled, c.cause)
	case lifecycleCauseCatalogDurability:
		return ErrCatalogDurabilityUnknown
	case lifecycleCauseCatalogUnsupported:
		return ErrCatalogUnsupported
	case lifecycleCauseStateUnavailable:
		return ErrStateUnavailable
	case lifecycleCauseStateConflict:
		return secrets.ErrConflict
	case lifecycleCauseStateNotFound:
		return secrets.ErrNotFound
	case lifecycleCauseStateCanceled:
		return newCanceledBoundaryError(secrets.ErrCanceled, c.cause)
	case lifecycleCauseStateInsecure:
		return secrets.ErrInsecurePath
	case lifecycleCauseStateDurability:
		return ErrStateDurabilityUnknown
	default:
		return nil
	}
}

func normalizeCatalogCause(err error) lifecycleCause {
	switch {
	case errors.Is(err, ErrCatalogCorrupt):
		return causeCatalogCorrupt
	case errors.Is(err, ErrCatalogConflict):
		return causeCatalogConflict
	case errors.Is(err, ErrCatalogNotFound):
		return causeCatalogNotFound
	case errors.Is(err, ErrCanceled), errors.Is(err, ErrCatalogCanceled), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return lifecycleCause{kind: lifecycleCauseCatalogCanceled, cause: cancellationCause(err)}
	case errors.Is(err, ErrCatalogDurabilityUnknown):
		return causeCatalogDurability
	case errors.Is(err, ErrCatalogUnsupported):
		return causeCatalogUnsupported
	default:
		return causeCatalogUnavailable
	}
}

func normalizeStateCause(err error) lifecycleCause {
	switch {
	case secrets.IsVisibleCommit(err):
		return causeStateDurability
	case errors.Is(err, secrets.ErrConflict):
		return causeStateConflict
	case errors.Is(err, secrets.ErrNotFound):
		return causeStateNotFound
	case errors.Is(err, ErrCanceled), errors.Is(err, secrets.ErrCanceled), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return lifecycleCause{kind: lifecycleCauseStateCanceled, cause: cancellationCause(err)}
	case errors.Is(err, secrets.ErrInsecurePath):
		return causeStateInsecure
	default:
		return causeStateUnavailable
	}
}

func visibleCatalogError(err error) bool {
	if errors.Is(err, ErrCatalogDurabilityUnknown) {
		return true
	}
	var visible interface{ Visible() bool }
	return errors.As(err, &visible) && visible.Visible()
}

// OrphanState identifies one exact opaque state version that could not be
// cleaned up after catalog publication failed. It intentionally contains no
// secret value or provider response.
type OrphanState struct {
	Credential Reference
	State      secrets.Reference
	Version    secrets.Version
}

func (o OrphanState) Valid() bool {
	return o.Credential.Valid() && !o.State.IsZero() && o.Version.Valid() && !o.Version.IsZero()
}

func (o OrphanState) Error() string {
	if !o.Valid() {
		return ErrOrphanState.Error()
	}
	return fmt.Sprintf("%s: %s", ErrOrphanState, o.State.String())
}

func (o OrphanState) Unwrap() error                  { return ErrOrphanState }
func (o OrphanState) Format(state fmt.State, _ rune) { _, _ = state.Write([]byte(o.Error())) }
func (o OrphanState) GoString() string               { return o.Error() }
func (o OrphanState) LogValue() slog.Value           { return slog.StringValue(o.Error()) }

// StatePublicationError reports catalog publication failure and whether exact
// state cleanup left a detectable orphan. Causes are normalized privately and
// exposed only through errors.Is.
type StatePublicationError struct {
	Orphan *OrphanState

	catalogCause      lifecycleCause
	cleanupCause      lifecycleCause
	initialStateCause lifecycleCause
}

func (e *StatePublicationError) Error() string {
	if e == nil {
		return ErrCatalogUnavailable.Error()
	}
	if e.Orphan != nil {
		return e.Orphan.Error()
	}
	for _, cause := range []lifecycleCause{e.catalogCause, e.cleanupCause, e.initialStateCause} {
		if err := cause.error(); err != nil {
			return err.Error()
		}
	}
	return ErrCatalogUnavailable.Error()
}

func (e *StatePublicationError) Unwrap() []error {
	if e == nil {
		return []error{ErrCatalogUnavailable}
	}
	causes := make([]error, 0, 4)
	for _, cause := range []lifecycleCause{e.catalogCause, e.cleanupCause, e.initialStateCause} {
		if err := cause.error(); err != nil {
			causes = append(causes, err)
		}
	}
	if e.Orphan != nil {
		causes = append(causes, e.Orphan)
	}
	if len(causes) == 0 {
		causes = append(causes, ErrCatalogUnavailable)
	}
	return causes
}

func (e *StatePublicationError) Orphaned() bool { return e != nil && e.Orphan != nil }
func (e *StatePublicationError) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte(e.Error()))
}
func (e *StatePublicationError) GoString() string     { return e.Error() }
func (e *StatePublicationError) LogValue() slog.Value { return slog.StringValue(e.Error()) }

// StateDeletionError reports that the catalog was made unavailable but exact
// state deletion did not complete. The state reference is safe metadata only.
type StateDeletionError struct {
	Credential Reference
	State      secrets.Reference

	catalogCause lifecycleCause
	stateCause   lifecycleCause
}

func (e *StateDeletionError) Error() string {
	if e == nil || e.State.IsZero() {
		return ErrStateDeleteFailed.Error()
	}
	return ErrStateDeleteFailed.Error() + ": " + e.State.String()
}

func (e *StateDeletionError) Unwrap() []error {
	if e == nil {
		return []error{ErrStateDeleteFailed}
	}
	causes := make([]error, 0, 3)
	if err := e.catalogCause.error(); err != nil {
		causes = append(causes, err)
	}
	if err := e.stateCause.error(); err != nil {
		causes = append(causes, err)
	}
	causes = append(causes, ErrStateDeleteFailed)
	return causes
}

func (e *StateDeletionError) Format(state fmt.State, _ rune) { _, _ = state.Write([]byte(e.Error())) }
func (e *StateDeletionError) GoString() string               { return e.Error() }
func (e *StateDeletionError) LogValue() slog.Value           { return slog.StringValue(e.Error()) }

// lifecycleOutcomeError represents visible warnings from both independent
// stores after their mutations completed. It intentionally has no exported
// cause field; errors.Is exposes each normalized outcome independently.
type lifecycleOutcomeError struct {
	catalogCause lifecycleCause
	stateCause   lifecycleCause
}

func (e *lifecycleOutcomeError) Error() string {
	if e == nil {
		return ErrCatalogUnavailable.Error()
	}
	for _, cause := range []lifecycleCause{e.catalogCause, e.stateCause} {
		if err := cause.error(); err != nil {
			return err.Error()
		}
	}
	return ErrCatalogUnavailable.Error()
}

func (e *lifecycleOutcomeError) Unwrap() []error {
	if e == nil {
		return []error{ErrCatalogUnavailable}
	}
	causes := make([]error, 0, 2)
	for _, cause := range []lifecycleCause{e.catalogCause, e.stateCause} {
		if err := cause.error(); err != nil {
			causes = append(causes, err)
		}
	}
	if len(causes) == 0 {
		causes = append(causes, ErrCatalogUnavailable)
	}
	return causes
}

func (e *lifecycleOutcomeError) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte(e.Error()))
}
func (e *lifecycleOutcomeError) GoString() string     { return e.Error() }
func (e *lifecycleOutcomeError) LogValue() slog.Value { return slog.StringValue(e.Error()) }

// StatePublisher composes the two independent stores with explicit ordering.
// It is intentionally small: OAuth/reauthentication algorithms belong to
// refreshable sources, not this persistence seam.
type StatePublisher struct {
	Catalog       Catalog
	Store         secrets.Store
	Preconditions secrets.PreconditionCapabilities
	Namespace     secrets.Namespace
}

func (p StatePublisher) capabilities(requireCreate bool) (secrets.PreconditionCapabilities, error) {
	caps := p.Preconditions
	if caps == nil && !isNilValue(p.Store) {
		if inferred, ok := p.Store.(secrets.PreconditionCapabilities); ok {
			caps = inferred
		}
	}
	if isNilValue(caps) || (!caps.SupportsCompareAndSwap() && !requireCreate) ||
		(requireCreate && (!caps.SupportsCreateOnly() || !caps.SupportsCompareAndSwap())) {
		return nil, ErrBuilderDependency
	}
	return caps, nil
}

func validStateRecord(record secrets.Record, expected secrets.Reference) bool {
	return record.Validate() == nil && record.Reference == expected && !record.Version.IsUnsupported()
}

func validDeletedResult(result secrets.DeleteResult, expected secrets.Reference, version secrets.Version) bool {
	return result.Validate() == nil && result.Reference == expected && result.Status == secrets.DeleteStatusDeleted && result.Version == version
}

// Create writes opaque state with create-only semantics before publishing the
// safe catalog record. A visible state warning is adopted using the returned
// committed record; a visible catalog warning is never rolled back.
func (p StatePublisher) Create(ctx context.Context, record Record, value secrets.Secret) error {
	if ctx == nil {
		return ErrBuilderDependency
	}
	if err := ctx.Err(); err != nil {
		return NewCanceledError(err)
	}
	if isNilValue(p.Catalog) || isNilValue(p.Store) || p.Namespace.IsZero() {
		return ErrBuilderDependency
	}
	if err := ValidateRecord(record); err != nil || !p.Namespace.Contains(record.State) {
		return ErrStateNamespace
	}
	if _, err := p.capabilities(true); err != nil {
		return err
	}

	state, putErr := p.Store.Put(ctx, record.State, value, secrets.CreateOnlyPut())
	stateCause := causeNone
	if putErr != nil {
		if !secrets.IsVisibleCommit(putErr) {
			return normalizeStateCause(putErr).error()
		}
		stateCause = causeStateDurability
		// A visible-commit contract normally returns the committed record. If a
		// backend cannot do so, reread once and adopt the visible state instead
		// of inventing a version or rolling it back.
		if !validStateRecord(state, record.State) {
			adopted, resolveErr := p.Store.Resolve(ctx, record.State)
			if resolveErr != nil {
				return normalizeStateCause(resolveErr).error()
			}
			state = adopted
		}
	}
	if !validStateRecord(state, record.State) {
		return ErrBuilderDependency
	}

	catalogErr := p.Catalog.Create(ctx, record)
	if catalogErr == nil {
		if stateCause != causeNone {
			return &lifecycleOutcomeError{stateCause: stateCause}
		}
		return nil
	}
	catalogCause := normalizeCatalogCause(catalogErr)
	if visibleCatalogError(catalogErr) {
		// Catalog rename already made the record visible. State now belongs to
		// that published record and must not be CAS-deleted as rollback.
		return &lifecycleOutcomeError{catalogCause: causeCatalogDurability, stateCause: stateCause}
	}

	cleanupResult, cleanupErr := p.Store.Delete(ctx, record.State, secrets.CompareAndSwapDelete(state.Version))
	if cleanupErr == nil && validDeletedResult(cleanupResult, record.State, state.Version) {
		if stateCause != causeNone {
			return &lifecycleOutcomeError{catalogCause: catalogCause, stateCause: stateCause}
		}
		return catalogCause.error()
	}
	if cleanupErr != nil && secrets.IsVisibleCommit(cleanupErr) && validDeletedResult(cleanupResult, record.State, state.Version) {
		return &lifecycleOutcomeError{catalogCause: catalogCause, stateCause: causeStateDurability}
	}
	cleanupCause := causeStateUnavailable
	if cleanupErr != nil {
		cleanupCause = normalizeStateCause(cleanupErr)
	}
	orphan := &OrphanState{Credential: record.Reference, State: record.State, Version: state.Version}
	return &StatePublicationError{Orphan: orphan, catalogCause: catalogCause, cleanupCause: cleanupCause, initialStateCause: stateCause}
}

// PublishState is a descriptive alias for StatePublisher.Create.
func PublishState(ctx context.Context, publisher StatePublisher, record Record, value secrets.Secret) error {
	return publisher.Create(ctx, record, value)
}

// CreateCredentialState is a convenient explicit free-function seam.
func CreateCredentialState(ctx context.Context, catalog Catalog, store secrets.Store, namespace secrets.Namespace, record Record, value secrets.Secret) error {
	return (StatePublisher{Catalog: catalog, Store: store, Namespace: namespace}).Create(ctx, record, value)
}

// Delete removes a credential in the required order: it uses only the caller's
// reference, rereads the current catalog record, resolves that exact state
// version, makes the catalog unavailable, then CAS-deletes that version.
func (p StatePublisher) Delete(ctx context.Context, supplied Record) error {
	if ctx == nil {
		return ErrBuilderDependency
	}
	if err := ctx.Err(); err != nil {
		return NewCanceledError(err)
	}
	if isNilValue(p.Catalog) || isNilValue(p.Store) || p.Namespace.IsZero() {
		return ErrBuilderDependency
	}
	if err := supplied.Reference.Validate(); err != nil {
		return ErrBuilderRecord
	}
	if _, err := p.capabilities(false); err != nil {
		return err
	}

	current, err := p.Catalog.Get(ctx, supplied.Reference)
	if err != nil {
		return normalizeCatalogCause(err).error()
	}
	if err := ValidateRecord(current); err != nil || current.Reference != supplied.Reference {
		return ErrBuilderRecord
	}
	if !p.Namespace.Contains(current.State) {
		return ErrStateNamespace
	}

	state, resolveErr := p.Store.Resolve(ctx, current.State)
	missingState := false
	if resolveErr != nil {
		if errors.Is(resolveErr, secrets.ErrNotFound) {
			missingState = true
		} else {
			return normalizeStateCause(resolveErr).error()
		}
	} else if !validStateRecord(state, current.State) {
		return ErrBuilderDependency
	}

	catalogErr := p.Catalog.Delete(ctx, current.Reference)
	catalogCause := causeNone
	if catalogErr != nil {
		catalogCause = normalizeCatalogCause(catalogErr)
		if !visibleCatalogError(catalogErr) {
			return catalogCause.error()
		}
	}

	if missingState {
		return &StateDeletionError{Credential: current.Reference, State: current.State, catalogCause: catalogCause, stateCause: causeStateNotFound}
	}

	result, deleteErr := p.Store.Delete(ctx, current.State, secrets.CompareAndSwapDelete(state.Version))
	if deleteErr != nil {
		if secrets.IsVisibleCommit(deleteErr) && validDeletedResult(result, current.State, state.Version) {
			if catalogCause != causeNone {
				return &lifecycleOutcomeError{catalogCause: causeCatalogDurability, stateCause: causeStateDurability}
			}
			return &lifecycleOutcomeError{stateCause: causeStateDurability}
		}
		return &StateDeletionError{Credential: current.Reference, State: current.State, catalogCause: catalogCause, stateCause: normalizeStateCause(deleteErr)}
	}
	if !validDeletedResult(result, current.State, state.Version) {
		return &StateDeletionError{Credential: current.Reference, State: current.State, catalogCause: catalogCause, stateCause: causeStateUnavailable}
	}
	if catalogCause != causeNone {
		return &lifecycleOutcomeError{catalogCause: causeCatalogDurability}
	}
	return nil
}

// DeleteCredentialState is the free-function deletion seam.
func DeleteCredentialState(ctx context.Context, catalog Catalog, store secrets.Store, namespace secrets.Namespace, record Record) error {
	return (StatePublisher{Catalog: catalog, Store: store, Namespace: namespace}).Delete(ctx, record)
}

// Create and Delete on Builder use its explicit catalog/store dependencies and
// preserve the same ordering as StatePublisher.
func (b *Builder) Create(ctx context.Context, record Record, value secrets.Secret) error {
	if b == nil {
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return NewCanceledError(err)
			}
		}
		return ErrBuilderDependency
	}
	return (StatePublisher{Catalog: b.Catalog, Store: b.Store, Preconditions: b.Preconditions, Namespace: b.StateNamespace}).Create(ctx, record, value)
}

func (b *Builder) Delete(ctx context.Context, record Record) error {
	if b == nil {
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return NewCanceledError(err)
			}
		}
		return ErrBuilderDependency
	}
	return (StatePublisher{Catalog: b.Catalog, Store: b.Store, Preconditions: b.Preconditions, Namespace: b.StateNamespace}).Delete(ctx, record)
}
