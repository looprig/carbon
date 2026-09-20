package app

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"strings"

	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/core/uuid"
	"github.com/looprig/sessionstore"
)

// ServeSessionReader joins Carbon's tenant-v1 control catalog to the separate
// legacy single-tenant Harness journals. The expected binding configuration is
// supplied by composition alongside Host and Factory, never inferred from a
// mutable agent default.
type ServeSessionReader struct {
	control        *sessionstore.Store
	launcher       *PooledLauncher
	bindingID      string
	bindingVersion string
}

type ServeSessionReaderConfigError struct{ Field string }

func (e *ServeSessionReaderConfigError) Error() string {
	return "carbon: invalid serve session reader " + e.Field
}

func NewServeSessionReader(control *sessionstore.Store, launcher *PooledLauncher, bindingID, bindingVersion string) (*ServeSessionReader, error) {
	switch {
	case control == nil:
		return nil, &ServeSessionReaderConfigError{Field: "control"}
	case launcher == nil:
		return nil, &ServeSessionReaderConfigError{Field: "launcher"}
	case bindingID == "":
		return nil, &ServeSessionReaderConfigError{Field: "binding_id"}
	case bindingVersion == "":
		return nil, &ServeSessionReaderConfigError{Field: "binding_version"}
	}
	return &ServeSessionReader{control: control, launcher: launcher, bindingID: bindingID, bindingVersion: bindingVersion}, nil
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

type ServeJournalBindingError struct {
	TenantID  sessionwire.TenantID
	SessionID sessionwire.SessionID
}

func (e *ServeJournalBindingError) Error() string {
	return fmt.Sprintf("carbon: session %q in tenant %q has an unsupported journal binding", e.SessionID, e.TenantID)
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

func (r *ServeSessionReader) ReadPublicJournal(ctx context.Context, req sessionstore.ReadPublicJournalRequest) (sessionwire.JournalPage, error) {
	entry, err := r.control.GetCatalogEntry(ctx, sessionstore.GetCatalogEntryRequest{TenantID: req.TenantID, SessionID: req.SessionID})
	if err != nil {
		return sessionwire.JournalPage{}, err
	}
	b := entry.Record.Binding
	runtimeID, parseErr := uuid.Parse(b.RuntimeSessionID)
	if r.bindingID == "" || r.bindingVersion == "" || b.StorageBindingID != r.bindingID || b.BindingVersion != r.bindingVersion ||
		b.ProtocolMode != sessionstore.ProtocolModeDisposition || parseErr != nil || runtimeID.IsZero() || runtimeID.String() != b.RuntimeSessionID {
		return sessionwire.JournalPage{}, &ServeJournalBindingError{TenantID: req.TenantID, SessionID: req.SessionID}
	}
	inner := req
	inner.SessionID = sessionwire.SessionID(b.RuntimeSessionID)
	if req.Cursor != "" {
		inner.Cursor, err = unwrapServeCursor(req.Cursor, req.TenantID, req.SessionID, b)
		if err != nil {
			return sessionwire.JournalPage{}, err
		}
	}
	page, err := r.launcher.readPublicJournal(ctx, req.TenantID, inner)
	if err != nil {
		return sessionwire.JournalPage{}, err
	}
	if page.NextCursor != "" {
		page.NextCursor, err = wrapServeCursor(page.NextCursor, req.TenantID, req.SessionID, b)
		if err != nil {
			return sessionwire.JournalPage{}, err
		}
	}
	if page.PreviousCursor != "" {
		page.PreviousCursor, err = wrapServeCursor(page.PreviousCursor, req.TenantID, req.SessionID, b)
		if err != nil {
			return sessionwire.JournalPage{}, err
		}
	}
	return page, nil
}

const maxServeCursorBytes = 8192
const serveCursorVersion = "c1"
const serveCursorDomain = "looprig/carbon/serve-public-journal-cursor/v1\x00"

func serveCursorScope(tenant sessionwire.TenantID, publicID sessionwire.SessionID, b sessionstore.SessionBinding) string {
	h := sha256.New()
	_, _ = h.Write([]byte(serveCursorDomain))
	for _, part := range []string{string(tenant), string(publicID), b.StorageBindingID, b.BindingVersion, b.RuntimeSessionID, string(b.ProtocolMode)} {
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(part)))
		_, _ = h.Write(length[:])
		_, _ = h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func serveCursorError() error {
	return &sessionstore.JournalError{Code: sessionstore.JournalErrorCursor, Field: "cursor"}
}

func wrapServeCursor(inner sessionwire.Cursor, tenant sessionwire.TenantID, publicID sessionwire.SessionID, b sessionstore.SessionBinding) (sessionwire.Cursor, error) {
	if inner == "" {
		return "", nil
	}
	token := serveCursorVersion + "." + serveCursorScope(tenant, publicID, b) + "." + base64.RawURLEncoding.EncodeToString([]byte(inner))
	if len(token) > maxServeCursorBytes {
		return "", serveCursorError()
	}
	return sessionwire.Cursor(token), nil
}

func unwrapServeCursor(token sessionwire.Cursor, tenant sessionwire.TenantID, publicID sessionwire.SessionID, b sessionstore.SessionBinding) (sessionwire.Cursor, error) {
	if token == "" {
		return "", nil
	}
	if len(token) > maxServeCursorBytes {
		return "", serveCursorError()
	}
	parts := strings.Split(string(token), ".")
	if len(parts) != 3 || parts[0] != serveCursorVersion || parts[1] != serveCursorScope(tenant, publicID, b) || parts[2] == "" {
		return "", serveCursorError()
	}
	inner, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(inner) == 0 {
		return "", serveCursorError()
	}
	return sessionwire.Cursor(inner), nil
}
