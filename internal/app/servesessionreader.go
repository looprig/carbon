package app

import (
	"context"
	"fmt"
	"io"

	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/core/uuid"
	harnessstore "github.com/looprig/harness/pkg/sessionstore"
	"github.com/looprig/host"
	"github.com/looprig/sessionstore"
)

// ServeSessionReader is Carbon's read plane for Factory: the tenant-v1 control
// catalog (sessions, gates) through WithSessionReader, and — through
// ResolveJournal, composed as WithJournalResolver — the separate legacy
// single-tenant Harness journals a Host session's runtime writes. The expected
// binding configuration is supplied by composition alongside Host and Factory,
// never inferred from a mutable agent default.
type ServeSessionReader struct {
	control        *sessionstore.Store
	launcher       *PooledLauncher
	served         sessionwire.TenantID
	bindingID      string
	bindingVersion string
	// journals is the ONE per-process projection cache (host v0.10.0): it keeps
	// each session's runtime→public command mapping across reads.
	journals *host.PublicJournals
}

type ServeSessionReaderConfigError struct{ Field string }

func (e *ServeSessionReaderConfigError) Error() string {
	return "carbon: invalid serve session reader " + e.Field
}

// served is the ONE tenant this deployment serves; the journal resolver refuses
// every other, so it never opens a tenant backend the local Host does not own.
func NewServeSessionReader(control *sessionstore.Store, launcher *PooledLauncher, served sessionwire.TenantID, bindingID, bindingVersion string) (*ServeSessionReader, error) {
	switch {
	case control == nil:
		return nil, &ServeSessionReaderConfigError{Field: "control"}
	case launcher == nil:
		return nil, &ServeSessionReaderConfigError{Field: "launcher"}
	case served.Validate() != nil:
		return nil, &ServeSessionReaderConfigError{Field: "served_tenant"}
	case bindingID == "":
		return nil, &ServeSessionReaderConfigError{Field: "binding_id"}
	case bindingVersion == "":
		return nil, &ServeSessionReaderConfigError{Field: "binding_version"}
	}
	return &ServeSessionReader{control: control, launcher: launcher, served: served, bindingID: bindingID, bindingVersion: bindingVersion,
		journals: host.NewPublicJournals(0)}, nil
}

// ServeObjectUnavailableError answers the UNBOUND object fallback. Every Carbon
// serve session is disposition bound, and its objects are read through the
// session-aware resolver (RuntimeObjects, composed in browser/factory.go); this
// read plane never exposes control-store objects.
type ServeObjectUnavailableError struct{}

func (*ServeObjectUnavailableError) Error() string {
	return "carbon: browser object storage unavailable"
}

// Factory maps ObjectErrorBackend to its explicit object-unavailable response.
func (*ServeObjectUnavailableError) Unwrap() error {
	return &sessionstore.ObjectError{Code: sessionstore.ObjectErrorBackend, Field: "serve_object"}
}

var ErrServeObjectUnavailable = &ServeObjectUnavailableError{}

// ServeJournalBindingError refuses a journal read for a binding this
// deployment does not serve: another storage binding or version, a legacy
// (non-disposition) protocol, or a runtime session id that is not a canonical,
// non-zero UUID. Factory answers it 500; it is a composition or data fault,
// never a client one.
type ServeJournalBindingError struct {
	TenantID  sessionwire.TenantID
	SessionID sessionwire.SessionID // the PUBLIC session id, when known
}

// Error never names the binding's runtime session id: it is private, and a
// composition may surface a resolver's error text.
func (e *ServeJournalBindingError) Error() string {
	if e.SessionID != "" {
		return fmt.Sprintf("carbon: session %q in tenant %q has an unsupported journal binding", e.SessionID, e.TenantID)
	}
	return fmt.Sprintf("carbon: a journal read in tenant %q names a journal this deployment does not serve", e.TenantID)
}

func (r *ServeSessionReader) ListSessions(ctx context.Context, req sessionstore.ListSessionsRequest) (sessionstore.SessionPage, error) {
	return r.control.ListSessions(ctx, req)
}
func (r *ServeSessionReader) GetCatalogEntry(ctx context.Context, req sessionstore.GetCatalogEntryRequest) (sessionstore.CatalogEntry, error) {
	return r.control.GetCatalogEntry(ctx, req)
}
func (r *ServeSessionReader) ReadGates(ctx context.Context, req sessionstore.ReadGatesRequest) (sessionwire.GatePage, error) {
	return r.control.ReadGates(ctx, req)
}
func (r *ServeSessionReader) GetObject(context.Context, sessionstore.GetObjectRequest) (io.ReadCloser, error) {
	return nil, ErrServeObjectUnavailable
}
func (r *ServeSessionReader) GetObjectMetadata(context.Context, sessionstore.GetObjectMetadataRequest) (sessionwire.ObjectMetadata, error) {
	return sessionwire.ObjectMetadata{}, ErrServeObjectUnavailable
}

// ReadPublicJournal is the LEGACY half of Factory's journal plane: Factory
// (v0.9.0+) reads a disposition-bound session's journal through
// ResolveJournal and calls this only for a session that is not disposition
// bound. Carbon's browser serve creates no such session (Factory refuses a
// legacy create), so every call here is refused rather than answered from the
// control store, whose public journal for a Host session is empty at tip 0.
func (r *ServeSessionReader) ReadPublicJournal(_ context.Context, req sessionstore.ReadPublicJournalRequest) (sessionwire.JournalPage, error) {
	return sessionwire.JournalPage{}, &ServeJournalBindingError{TenantID: req.TenantID, SessionID: req.SessionID}
}

// ResolveJournal is Carbon's factory.SessionJournalResolver: it answers the
// harness runtime journal a Host-owned session's binding names, PROJECTED to
// public identities (host.PublicJournals, finding W1 / release audit R5.2 H1).
//
// A harness public body names the RUNTIME session id on every event and the
// RUNTIME command id on every command-caused event; both are private. The
// returned reader rewrites them to the public session id Factory routed the
// read for and to the public command id the client admitted, byte-identically
// to what the Host relays on the live tail.
//
// Factory has already read the catalog under the PUBLIC session id and
// addresses the request to the binding's RuntimeSessionID; it also wraps every
// cursor to the public session and its binding (j1.). A binding or tenant this
// deployment does not serve is REFUSED rather than defaulted, so one
// deployment's journal is never served under another's configuration.
func (r *ServeSessionReader) ResolveJournal(_ context.Context, tenant sessionwire.TenantID, session sessionwire.SessionID, b sessionstore.SessionBinding) (*host.PublicJournal, error) {
	runtimeID, parseErr := uuid.Parse(b.RuntimeSessionID)
	if b.StorageBindingID != r.bindingID || b.BindingVersion != r.bindingVersion || b.ProtocolMode != sessionstore.ProtocolModeDisposition ||
		parseErr != nil || runtimeID.IsZero() || runtimeID.String() != b.RuntimeSessionID {
		return nil, &ServeJournalBindingError{TenantID: tenant, SessionID: session}
	}
	// Carbon serves one tenant (composeFactory pins every principal to it), and
	// PooledLauncher would create a backend for any valid tenant it is asked
	// about: refuse every other tenant here, before any storage is touched.
	if tenant != r.served {
		return nil, &ServeJournalBindingError{TenantID: tenant, SessionID: session}
	}
	runtime := &serveRuntimeJournal{launcher: r.launcher, tenant: tenant, runtimeID: sessionwire.SessionID(b.RuntimeSessionID)}
	return r.journals.Reader(runtime, tenant, session, b)
}

// serveRuntimeJournal reads ONE runtime session's harness journal, unprojected,
// from the tenant backend the pooled Host's harness writes it to. It is bound to
// that runtime id and refuses a request naming any other session. It is never
// handed to Factory directly: only through host.PublicJournals.
type serveRuntimeJournal struct {
	launcher  *PooledLauncher
	tenant    sessionwire.TenantID
	runtimeID sessionwire.SessionID
}

func (j *serveRuntimeJournal) ReadPublicJournal(ctx context.Context, req sessionstore.ReadPublicJournalRequest) (sessionwire.JournalPage, error) {
	if req.TenantID != j.tenant || req.SessionID != j.runtimeID {
		return sessionwire.JournalPage{}, &ServeJournalBindingError{TenantID: req.TenantID}
	}
	return j.launcher.readPublicJournal(ctx, j.tenant, req)
}

func (j *serveRuntimeJournal) ReadRuntimeJournal(ctx context.Context, req sessionstore.ReadRuntimeJournalRequest) (sessionstore.RuntimePage, error) {
	if req.TenantID != j.tenant || req.SessionID != j.runtimeID {
		return sessionstore.RuntimePage{}, &ServeJournalBindingError{TenantID: req.TenantID}
	}
	return j.launcher.readRuntimeJournal(ctx, j.tenant, req)
}

// ServeObjectScopeError refuses an object read the served runtime store does
// not answer: another tenant, or a kind other than a tool-result capture.
// Factory answers it 500; it is a composition fault, never a client one. It
// never names a runtime session id.
type ServeObjectScopeError struct {
	TenantID sessionwire.TenantID
	Kind     sessionstore.ObjectKind
}

func (e *ServeObjectScopeError) Error() string {
	return fmt.Sprintf("carbon: an object read in tenant %q of kind %q is outside the served tool-result scope", e.TenantID, e.Kind)
}

// RuntimeEvidence answers the served tenant's harness runtime store: the SAME
// store its rigs journal into and retain tool-result objects through, so the
// object policy's committed-journal evidence (LookupToolResultCapture) and its
// positive cache are the runtime's own. Every other tenant is refused before
// any backend is opened.
func (r *ServeSessionReader) RuntimeEvidence(tenant sessionwire.TenantID) (*harnessstore.Store, bool) {
	if tenant != r.served {
		return nil, false
	}
	store, err := r.launcher.JournalStoreForTenant(tenant)
	if err != nil || store == nil {
		return nil, false
	}
	return store, true
}

// RuntimeObjects answers the served tenant's runtime object reader for
// Factory's session-aware object resolver. Factory addresses every request to
// the binding's RuntimeSessionID after the object policy has found committed
// evidence for the reference in that session's journal.
func (r *ServeSessionReader) RuntimeObjects(tenant sessionwire.TenantID) (*ServeRuntimeObjects, bool) {
	if tenant != r.served {
		return nil, false
	}
	return &ServeRuntimeObjects{launcher: r.launcher, tenant: tenant}, true
}

// ServeRuntimeObjects reads tool-result objects from ONE tenant's harness
// runtime backend, through the same companion SessionStore the journal reads
// use (legacy single-tenant layout, bounded blob readers). It refuses any other
// tenant and any other object kind.
type ServeRuntimeObjects struct {
	launcher *PooledLauncher
	tenant   sessionwire.TenantID
}

func (o *ServeRuntimeObjects) scope(tenant sessionwire.TenantID, kind sessionstore.ObjectKind) error {
	if tenant != o.tenant || kind != sessionstore.ObjectKindToolResult {
		return &ServeObjectScopeError{TenantID: tenant, Kind: kind}
	}
	return nil
}

func (o *ServeRuntimeObjects) GetObjectMetadata(ctx context.Context, req sessionstore.GetObjectMetadataRequest) (sessionwire.ObjectMetadata, error) {
	if err := o.scope(req.TenantID, req.ExpectedKind); err != nil {
		return sessionwire.ObjectMetadata{}, err
	}
	var metadata sessionwire.ObjectMetadata
	err := o.launcher.withTenantJournal(o.tenant, func(store *sessionstore.Store) (err error) {
		metadata, err = store.GetObjectMetadata(ctx, req)
		return err
	})
	return metadata, err
}

func (o *ServeRuntimeObjects) GetObject(ctx context.Context, req sessionstore.GetObjectRequest) (io.ReadCloser, error) {
	if err := o.scope(req.TenantID, req.ExpectedKind); err != nil {
		return nil, err
	}
	var stream io.ReadCloser
	err := o.launcher.withTenantJournal(o.tenant, func(store *sessionstore.Store) (err error) {
		stream, err = store.GetObject(ctx, req)
		return err
	})
	if err != nil {
		return nil, err
	}
	return stream, nil
}
