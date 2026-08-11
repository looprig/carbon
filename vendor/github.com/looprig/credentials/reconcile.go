package credentials

import (
	"context"
	"errors"
	"sort"

	"github.com/looprig/secrets"
)

// FindingKind identifies a report-only state/catalog discrepancy.
type FindingKind uint8

const (
	FindingOrphanState FindingKind = iota + 1
	FindingMissingState
)

const (
	FindingOrphan  = FindingOrphanState
	FindingMissing = FindingMissingState
)

func (k FindingKind) String() string {
	switch k {
	case FindingOrphanState:
		return "orphan_state"
	case FindingMissingState:
		return "missing_state"
	default:
		return "invalid"
	}
}

// ReconcileFinding is metadata only. State is the opaque secret-store
// reference; Credential identifies a safe catalog record for missing-state
// findings. No secret values or provider responses can be represented.
type ReconcileFinding struct {
	Kind       FindingKind
	State      secrets.Reference
	Reference  secrets.Reference // alias for callers that emphasize state ref
	Credential Reference
}

// Finding is the concise public name for ReconcileFinding.
type Finding = ReconcileFinding

const maxReconcilePages = 4096

// Reconcile compares the complete safe catalog list to metadata-only state
// listing constrained to namespace. It never adopts, deletes, or mutates
// either backend. Findings are grouped by kind and sorted by canonical state
// reference for stable diagnostics.
func Reconcile(ctx context.Context, catalog Catalog, states secrets.Lister, namespace secrets.Namespace) ([]Finding, error) {
	if ctx == nil {
		return nil, ErrBuilderDependency
	}
	if err := ctx.Err(); err != nil {
		return nil, NewCanceledError(err)
	}
	if isNilValue(catalog) || isNilValue(states) || namespace.IsZero() {
		return nil, ErrStateNamespace
	}
	records, err := catalog.List(ctx)
	if err != nil {
		return nil, safeReconcileCatalogError(err)
	}
	wanted := make(map[string]Reference, len(records))
	seenCredentials := make(map[string]struct{}, len(records))
	for _, record := range records {
		if err := ValidateRecord(record); err != nil {
			return nil, ErrBuilderRecord
		}
		if !namespace.Contains(record.State) {
			return nil, ErrStateNamespace
		}
		if _, duplicate := seenCredentials[record.Reference.String()]; duplicate {
			return nil, ErrCatalogCorrupt
		}
		seenCredentials[record.Reference.String()] = struct{}{}
		key := record.State.String()
		if _, duplicate := wanted[key]; duplicate {
			return nil, ErrCatalogCorrupt
		}
		wanted[key] = record.Reference
	}
	observed := make(map[string]secrets.Reference)
	token := secrets.PageToken{}
	pages := 0
	for {
		pages++
		if pages > maxReconcilePages {
			return nil, ErrCatalogCorrupt
		}
		if ctx.Err() != nil {
			return nil, NewCanceledError(ctx.Err())
		}
		page, err := states.List(ctx, namespace, token, secrets.MaxPageItems)
		if err != nil {
			return nil, safeReconcileStateError(err)
		}
		if err := page.Validate(secrets.MaxPageItems); err != nil {
			return nil, ErrCatalogCorrupt
		}
		for _, metadata := range page.Items {
			if err := metadata.Validate(); err != nil {
				return nil, ErrCatalogCorrupt
			}
			// A backend must honor namespace exactly. Ignore an out-of-scope
			// item rather than allowing reconciliation to report or act on it.
			if !namespace.Contains(metadata.Reference) {
				continue
			}
			key := metadata.Reference.String()
			if _, duplicate := observed[key]; duplicate {
				return nil, ErrCatalogCorrupt
			}
			observed[key] = metadata.Reference
		}
		if page.NextToken.IsZero() {
			break
		}
		if page.NextToken == token {
			return nil, ErrCatalogCorrupt
		}
		token = page.NextToken
	}

	findings := make([]Finding, 0)
	for key, state := range observed {
		if _, exists := wanted[key]; !exists {
			findings = append(findings, Finding{Kind: FindingOrphanState, State: state, Reference: state})
		}
	}
	for key, credential := range wanted {
		if _, exists := observed[key]; !exists {
			state, err := secrets.ParseReference(key)
			if err != nil {
				return nil, ErrCatalogCorrupt
			}
			findings = append(findings, Finding{Kind: FindingMissingState, State: state, Reference: state, Credential: credential})
		}
	}
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Kind != findings[j].Kind {
			return findings[i].Kind < findings[j].Kind
		}
		return findings[i].State.String() < findings[j].State.String()
	})
	return findings, nil
}

func safeReconcileCatalogError(err error) error {
	switch {
	case errors.Is(err, ErrCatalogCorrupt):
		return ErrCatalogCorrupt
	case errors.Is(err, ErrCatalogNotFound):
		return ErrCatalogNotFound
	case errors.Is(err, ErrCanceled), errors.Is(err, ErrCatalogCanceled), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return newCanceledBoundaryError(ErrCatalogCanceled, err)
	default:
		return ErrCatalogUnavailable
	}
}

func safeReconcileStateError(err error) error {
	switch {
	case errors.Is(err, secrets.ErrNotFound):
		return secrets.ErrNotFound
	case errors.Is(err, ErrCanceled), errors.Is(err, secrets.ErrCanceled), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return newCanceledBoundaryError(secrets.ErrCanceled, err)
	case errors.Is(err, secrets.ErrInsecurePath):
		return secrets.ErrInsecurePath
	default:
		return ErrBuilderDependency
	}
}

// Reconcile uses a Builder's explicit catalog, state lister, and namespace.
func (b *Builder) Reconcile(ctx context.Context) ([]Finding, error) {
	if b == nil {
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return nil, NewCanceledError(err)
			}
		}
		return nil, ErrBuilderDependency
	}
	return Reconcile(ctx, b.Catalog, b.StateIndex, b.StateNamespace)
}
