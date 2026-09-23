package app

import (
	"context"
	"fmt"
	"io"

	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/core/uuid"
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
	return &ServeSessionReader{control: control, launcher: launcher, served: served, bindingID: bindingID, bindingVersion: bindingVersion}, nil
}

// Factory's bound-session object route requires a separate object resolver.
// Carbon has no such product backend yet, so this read plane never exposes
// control-store objects through the unbound fallback.
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
	SessionID sessionwire.SessionID
	Binding   sessionstore.SessionBinding
}

func (e *ServeJournalBindingError) Error() string {
	if e.SessionID != "" {
		return fmt.Sprintf("carbon: session %q in tenant %q has an unsupported journal binding", e.SessionID, e.TenantID)
	}
	return fmt.Sprintf("carbon: runtime session %q in tenant %q has an unsupported journal binding (%q/%q, %s)",
		e.Binding.RuntimeSessionID, e.TenantID, e.Binding.StorageBindingID, e.Binding.BindingVersion, e.Binding.ProtocolMode)
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

// ResolveJournal is Carbon's factory.JournalResolver: it answers the harness
// runtime journal a Host-owned session's binding names, for that tenant.
//
// Factory has already read the catalog under the PUBLIC session id and
// rewrites the request to the binding's RuntimeSessionID; it also wraps every
// cursor to the public session and its binding (j1.), so the reader returned
// here deals only in raw runtime-journal cursors. A binding this deployment
// does not serve is REFUSED rather than defaulted, so one deployment's journal
// is never served under another's configuration.
func (r *ServeSessionReader) ResolveJournal(_ context.Context, tenant sessionwire.TenantID, b sessionstore.SessionBinding) (*ServeRuntimeJournal, error) {
	runtimeID, parseErr := uuid.Parse(b.RuntimeSessionID)
	if b.StorageBindingID != r.bindingID || b.BindingVersion != r.bindingVersion || b.ProtocolMode != sessionstore.ProtocolModeDisposition ||
		parseErr != nil || runtimeID.IsZero() || runtimeID.String() != b.RuntimeSessionID {
		return nil, &ServeJournalBindingError{TenantID: tenant, Binding: b}
	}
	// Carbon serves one tenant (composeFactory pins every principal to it), and
	// PooledLauncher would create a backend for any valid tenant it is asked
	// about: refuse every other tenant here, before any storage is touched.
	if tenant != r.served {
		return nil, &ServeJournalBindingError{TenantID: tenant, Binding: b}
	}
	return &ServeRuntimeJournal{launcher: r.launcher, tenant: tenant, runtimeID: sessionwire.SessionID(b.RuntimeSessionID)}, nil
}

// ServeRuntimeJournal reads ONE runtime session's harness journal from the
// tenant backend the pooled Host's harness writes it to. It is bound to that
// runtime id and refuses a request naming any other session.
type ServeRuntimeJournal struct {
	launcher  *PooledLauncher
	tenant    sessionwire.TenantID
	runtimeID sessionwire.SessionID
}

func (j *ServeRuntimeJournal) ReadPublicJournal(ctx context.Context, req sessionstore.ReadPublicJournalRequest) (sessionwire.JournalPage, error) {
	if req.TenantID != j.tenant || req.SessionID != j.runtimeID {
		return sessionwire.JournalPage{}, &ServeJournalBindingError{TenantID: req.TenantID, SessionID: req.SessionID}
	}
	return j.launcher.readPublicJournal(ctx, j.tenant, req)
}
