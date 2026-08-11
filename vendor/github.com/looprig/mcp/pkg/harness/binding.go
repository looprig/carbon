// Package mcpharness binds MCP servers into a Harness Session.
//
// It is the only place where the two vocabularies meet: below it, pkg/client
// speaks MCP and knows nothing about Loops, turns, or gates; above it, Harness
// speaks tools, permissions, and gates and knows nothing about MCP. Everything
// this package exports to Harness is protocol-neutral, and no MCP wire type
// crosses the boundary in either direction (design §pkg/harness).
//
// This file defines the configuration unit: a Binding, which mounts one MCP
// server under a name, with an owner (its Scope), an audience (its Visibility),
// and a startup posture (Required).
package mcpharness

import (
	"fmt"

	"github.com/looprig/core/uuid"
	"github.com/looprig/mcp/pkg/client"
)

// Scope is the owner of a binding's connection: who created it and whose
// shutdown closes it. The zero value is not a valid scope.
//
// Scope is not visibility. A Session-scoped connection may be shown to one Loop
// or to all of them (see LoopSelector); what Scope fixes is that the Session,
// not the Loop, owns the process, the credentials, and the server's state
// (design §Binding model and scope).
type Scope uint8

const (
	// ScopeSession mounts the server on the Session. One connection serves
	// every Loop the binding's Visibility permits, and it closes when the
	// Session closes.
	ScopeSession Scope = iota + 1
	// ScopeLoop mounts the server on exactly one Loop. The connection is that
	// Loop's alone and closes when the Loop closes.
	ScopeLoop
)

// String returns a stable lowercase identifier, or "unknown".
func (s Scope) String() string {
	switch s {
	case ScopeSession:
		return "session"
	case ScopeLoop:
		return "loop"
	default:
		return "unknown"
	}
}

// valid reports whether s is a declared scope.
func (s Scope) valid() bool { return s == ScopeSession || s == ScopeLoop }

// Binding mounts one MCP server into a Session under a stable name.
//
// The name is part of capability identity: it qualifies every model-facing tool
// name (mcp__<binding>__<raw>) and every permission identity
// (mcp:<binding>:<raw-tool>), so two bindings may target the same server
// executable or URL under different names when they need separate credentials,
// working directories, or isolation — and they are then genuinely separate
// authorities.
//
// Required and Visibility live here rather than on client.Definition on purpose:
// the same server is required in one product and optional in another, visible to
// every Loop here and to one Loop there, and none of that is a property of the
// server (design §Binding model and scope).
//
// Treat a Binding as immutable once Validate has returned nil. Reconfiguration
// creates new Bindings; it never mutates one an active turn may be using.
type Binding struct {
	// Name is the binding's stable identity within the Session. It must satisfy
	// client.Name.
	Name string
	// Server is the secret-free MCP connection configuration.
	Server client.Definition
	// Scope names the connection's owner. Required.
	Scope Scope
	// Loop is the owning Loop, for ScopeLoop only. Harness has no loop.ID named
	// type — Loop identity is a bare uuid.UUID throughout (identity.Coordinates,
	// tool.Bindings.LoopID) — so this mirrors it rather than inventing an alias
	// Harness would not recognize.
	Loop uuid.UUID
	// Visibility decides which Loops may consume a ScopeSession binding. It is
	// unused for ScopeLoop, whose audience is its owner and nobody else.
	Visibility LoopSelector
	// Required states whether the owner may come up without this binding. A
	// required binding must be ready before its owner is; an optional one that
	// fails leaves its owner usable and marks only itself failed (design
	// §Required and optional servers).
	Required bool
}

// Validate checks the binding and fails closed on the first violation.
//
// The scope-shaped rules are the ones worth stating: a Session-scoped binding
// may not name a Loop, and a Loop-scoped binding must name exactly one and may
// not carry a selector. Neither is pedantry. A Loop on a Session binding would
// be an ownership claim the Session does not honor — the field would say "this
// Loop's connection" while Session shutdown closed it — and a selector on a Loop
// binding would look like a visibility policy while being silently ignored,
// which is the shape of an audience mistake nobody catches: it reads as though
// the binding were shared, and it never is.
//
// Server is validated too. A binding is only as good as the connection under it,
// and both this Name and Server.Name must agree, because the qualified tool
// names the model sees are built from the former while the protocol calls go out
// under the latter — a disagreement would make the reverse mapping a lie.
func (b Binding) Validate() error {
	name := client.Name(b.Name)
	if err := name.Validate(); err != nil {
		return fmt.Errorf("Binding.Name: %w", err)
	}
	if !b.Scope.valid() {
		return fmt.Errorf("Binding %q: Scope %d is not session or loop", b.Name, uint8(b.Scope))
	}
	if b.Server.Name != name {
		return fmt.Errorf("Binding %q: Server.Name is %q; a binding and its connection must agree on the name", b.Name, string(b.Server.Name))
	}
	if err := b.Server.Validate(); err != nil {
		return fmt.Errorf("Binding %q: %w", b.Name, err)
	}
	switch b.Scope {
	case ScopeSession:
		if !b.Loop.IsZero() {
			return fmt.Errorf("Binding %q: session scope must not name a Loop (the Session owns the connection)", b.Name)
		}
		if err := b.Visibility.validate(); err != nil {
			return fmt.Errorf("Binding %q: %w", b.Name, err)
		}
	case ScopeLoop:
		if b.Loop.IsZero() {
			return fmt.Errorf("Binding %q: loop scope must name its owning Loop", b.Name)
		}
		if !b.Visibility.isZero() {
			return fmt.Errorf("Binding %q: loop scope must not set Visibility (its audience is its owner)", b.Name)
		}
	}
	return nil
}

// permits reports whether the Loop identified by loopID and name may consume
// this binding's capabilities.
//
// It is the one place the scope/visibility distinction becomes a yes or no, and
// the Loop-scoped answer is the delegation rule in miniature: a Loop-scoped
// binding is permitted to its owner by ID and to nobody else, so a delegate
// never inherits its parent's private servers no matter what it is called
// (design §Delegation).
func (b Binding) permits(loopID uuid.UUID, name string) bool {
	switch b.Scope {
	case ScopeSession:
		return b.Visibility.Permits(loopID, name)
	case ScopeLoop:
		return !loopID.IsZero() && loopID == b.Loop
	default:
		return false
	}
}
