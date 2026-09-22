package browser

import (
	"context"
	"fmt"

	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/factory"
	"github.com/looprig/factory/identity"
	"github.com/looprig/sessionstore"
)

// TenantNotServedError refuses a principal whose tenant is not the one tenant
// this composition's local Host serves. The Host registers a journal store for
// the default tenant only and refuses a HostLink for any other, so an admitted
// command for another tenant could never be placed: it would sit pending until
// Factory's deadline sweep rejected it. Refusing at authorization happens
// before Factory writes anything durable.
//
// It unwraps to identity.ErrUnauthorized, which Factory answers as 403
// not_authorized over HTTP and permission-denied over ClientLink.
type TenantNotServedError struct {
	Tenant sessionwire.TenantID
	Served sessionwire.TenantID
}

func (e *TenantNotServedError) Error() string {
	return fmt.Sprintf("carbon: tenant %q is not served by this browser composition (it serves only %q)", e.Tenant, e.Served)
}

func (e *TenantNotServedError) Unwrap() error { return identity.ErrUnauthorized }

// servedTenantAuthorizer confines every tenant-scoped decision to the served
// tenant before delegating to the embedder's Authorizer. The cross-tenant
// service sweep is delegated unchanged: its principal is Factory's own service
// identity, and a tenant principal can never reach it.
type servedTenantAuthorizer struct {
	served sessionwire.TenantID
	next   factory.Authorizer
}

var _ factory.Authorizer = servedTenantAuthorizer{}

func (a servedTenantAuthorizer) pin(principal identity.Principal) error {
	if principal.Tenant() != a.served {
		return &TenantNotServedError{Tenant: principal.Tenant(), Served: a.served}
	}
	return nil
}

func (a servedTenantAuthorizer) AuthorizeSessionList(ctx context.Context, p identity.Principal) error {
	if err := a.pin(p); err != nil {
		return err
	}
	return a.next.AuthorizeSessionList(ctx, p)
}

func (a servedTenantAuthorizer) AuthorizeSessionRead(ctx context.Context, p identity.Principal, session sessionwire.SessionID) error {
	if err := a.pin(p); err != nil {
		return err
	}
	return a.next.AuthorizeSessionRead(ctx, p, session)
}

func (a servedTenantAuthorizer) AuthorizeObjectRead(ctx context.Context, p identity.Principal, session sessionwire.SessionID, object sessionwire.ObjectReference) error {
	if err := a.pin(p); err != nil {
		return err
	}
	return a.next.AuthorizeObjectRead(ctx, p, session, object)
}

func (a servedTenantAuthorizer) AuthorizeControl(ctx context.Context, p identity.Principal, session sessionwire.SessionID, kind sessionstore.CommandKind) error {
	if err := a.pin(p); err != nil {
		return err
	}
	return a.next.AuthorizeControl(ctx, p, session, kind)
}

func (a servedTenantAuthorizer) AuthorizeSubscribe(ctx context.Context, p identity.Principal, channel string) error {
	if err := a.pin(p); err != nil {
		return err
	}
	return a.next.AuthorizeSubscribe(ctx, p, channel)
}

func (a servedTenantAuthorizer) AuthorizeServiceSweep(ctx context.Context, p identity.Principal) error {
	return a.next.AuthorizeServiceSweep(ctx, p)
}
