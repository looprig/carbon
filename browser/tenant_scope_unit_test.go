package browser

import (
	"context"
	"errors"
	"testing"

	sessionwire "github.com/looprig/core/sessionwire/v1"
	"github.com/looprig/factory"
	"github.com/looprig/factory/identity"
)

func TestServedTenantAuthorizerRefusesOtherTenantsTyped(t *testing.T) {
	a := servedTenantAuthorizer{served: "local", next: factory.TenantAuthorizer{}}
	foreign, err := identity.NewPrincipal("acme", "u", identity.KindActor)
	if err != nil {
		t.Fatal(err)
	}
	local, err := identity.NewPrincipal("local", "u", identity.KindActor)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for name, call := range map[string]func(identity.Principal) error{
		"list": func(p identity.Principal) error { return a.AuthorizeSessionList(ctx, p) },
		"read": func(p identity.Principal) error { return a.AuthorizeSessionRead(ctx, p, "s") },
		"object": func(p identity.Principal) error {
			return a.AuthorizeObjectRead(ctx, p, "s", sessionwire.ObjectReference{})
		},
		"control": func(p identity.Principal) error { return a.AuthorizeControl(ctx, p, "s", "create") },
		"subscribe": func(p identity.Principal) error {
			return a.AuthorizeSubscribe(ctx, p, "session:"+string(p.Tenant())+":s")
		},
	} {
		err := call(foreign)
		var typed *TenantNotServedError
		if !errors.As(err, &typed) || typed.Tenant != "acme" || typed.Served != "local" || !errors.Is(err, identity.ErrUnauthorized) {
			t.Fatalf("%s for unserved tenant = %v", name, err)
		}
		if err := call(local); err != nil {
			t.Fatalf("%s for served tenant = %v", name, err)
		}
	}
	service, err := identity.NewPrincipal("local", "replica", identity.KindService)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.AuthorizeServiceSweep(ctx, service); err != nil {
		t.Fatalf("service sweep = %v", err)
	}
}
