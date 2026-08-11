// This file changes a live owner's bindings.
//
// The rule the whole file serves is design §Binding reconfiguration:
// reconfiguration creates new immutable binding definitions; it never mutates
// one an active turn is using. So nothing here edits a bindingState's binding —
// a change installs a NEW state and retires the old one, and a turn that already
// holds the old route keeps it until its calls finish or the retirement deadline
// takes it away.

package mcpharness

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/looprig/mcp/pkg/client"
)

// opKind is what a BindingOp does. The zero value is not a valid op.
type opKind uint8

const (
	opAdd opKind = iota + 1
	opRemove
	opEnable
	opDisable
	opReplace
)

func (k opKind) String() string {
	switch k {
	case opAdd:
		return "add"
	case opRemove:
		return "remove"
	case opEnable:
		return "enable"
	case opDisable:
		return "disable"
	case opReplace:
		return "replace"
	default:
		return "unknown"
	}
}

// BindingOp is one reconfiguration step. Build one with AddBinding,
// RemoveBinding, EnableBinding, DisableBinding, or ReplaceBinding.
//
// It is an opaque value rather than an exported struct so that the set of
// operations stays closed: a caller cannot construct a half-specified op, and
// adding an operation later cannot silently change the meaning of one already
// written.
type BindingOp struct {
	kind    opKind
	name    client.Name
	binding Binding
	// failClosed makes a failed replacement retire the prior binding anyway.
	failClosed bool
}

// AddBinding starts a new binding under a live owner.
func AddBinding(b Binding) BindingOp {
	return BindingOp{kind: opAdd, name: client.Name(b.Name), binding: b}
}

// RemoveBinding retires a binding and forgets its configuration.
func RemoveBinding(name string) BindingOp {
	return BindingOp{kind: opRemove, name: client.Name(name)}
}

// DisableBinding retires a binding's connection but keeps its configuration, so
// EnableBinding can start it again.
func DisableBinding(name string) BindingOp {
	return BindingOp{kind: opDisable, name: client.Name(name)}
}

// EnableBinding starts a disabled binding again.
func EnableBinding(name string) BindingOp {
	return BindingOp{kind: opEnable, name: client.Name(name)}
}

// ReplaceBinding swaps a binding's transport, auth, limits, filters, or server
// identity. The new client connects before the old route is retired, so a
// replacement that fails costs nothing: the prior binding stays active and the
// failure is reported (design §Binding reconfiguration).
func ReplaceBinding(b Binding) BindingOp {
	return BindingOp{kind: opReplace, name: client.Name(b.Name), binding: b}
}

// FailClosed returns a copy of a replace op that retires the prior binding even
// when the replacement fails.
//
// It is the deliberate opposite of the default. Leaving the prior binding up is
// right when a replacement is an upgrade — losing the new configuration beats
// losing the server. It is wrong when the replacement exists to REVOKE
// something: narrowing a tool filter, rotating a credential, moving to an
// endpoint the old one must no longer reach. There, a failure that silently
// leaves the old authority serving is the failure, so the caller says so here.
//
// It has no effect on any other op kind: add, remove, enable, and disable have
// no prior binding to keep.
func (o BindingOp) FailClosed() BindingOp {
	o.failClosed = true
	return o
}

// validate checks an op's own shape, before anything is consulted about whether
// it applies.
func (o BindingOp) validate() error {
	switch o.kind {
	case opAdd, opReplace:
		if err := o.binding.Validate(); err != nil {
			return fmt.Errorf("%s: %w", o.kind, err)
		}
		return nil
	case opRemove, opEnable, opDisable:
		if err := o.name.Validate(); err != nil {
			return fmt.Errorf("%s: %w", o.kind, err)
		}
		return nil
	case 0:
		return fmt.Errorf("uninitialized BindingOp (build one with AddBinding, RemoveBinding, EnableBinding, DisableBinding, or ReplaceBinding)")
	default:
		return fmt.Errorf("unknown BindingOp kind %d", uint8(o.kind))
	}
}

// Reconfigure applies ops to a live Manager.
//
// Every op is validated before any is applied, so a batch with a malformed op
// changes nothing: a caller that mistyped one binding's name should not discover
// it with three servers already reconnected. Beyond that the ops apply in order
// and their failures are joined — an op that fails leaves its own binding as it
// was, and does not stop the rest.
func (m *Manager) Reconfigure(ctx context.Context, ops []BindingOp) error {
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return ErrManagerClosed
	}
	for i, op := range ops {
		if err := op.validate(); err != nil {
			return fmt.Errorf("ops[%d]: %w", i, err)
		}
	}
	var errs []error
	for i, op := range ops {
		if err := m.apply(ctx, op); err != nil {
			errs = append(errs, fmt.Errorf("ops[%d] (%s %q): %w", i, op.kind, op.name, err))
		}
	}
	return errors.Join(errs...)
}

func (m *Manager) apply(ctx context.Context, op BindingOp) error {
	switch op.kind {
	case opAdd:
		return m.applyAdd(ctx, op)
	case opReplace:
		return m.applyReplace(ctx, op)
	case opRemove:
		return m.applyRemove(ctx, op)
	case opDisable:
		return m.applyDisable(ctx, op)
	case opEnable:
		return m.applyEnable(ctx, op)
	default:
		return fmt.Errorf("unknown BindingOp kind %d", uint8(op.kind))
	}
}

// applyAdd installs and connects a binding that does not exist yet.
func (m *Manager) applyAdd(ctx context.Context, op BindingOp) error {
	bs := newBindingState(op.binding)
	m.mu.Lock()
	if _, exists := m.states[op.name]; exists {
		m.mu.Unlock()
		return fmt.Errorf("binding already exists")
	}
	m.states[op.name] = bs
	m.mu.Unlock()

	if err := m.connectNow(ctx, bs); err != nil {
		// An add that cannot connect leaves the binding installed and failed
		// rather than absent: its status is how an operator sees WHY, and a
		// binding that vanished on failure would be indistinguishable from one
		// that was never configured. Reconnect policy owns it from here.
		return err
	}
	return nil
}

// applyReplace connects the replacement before retiring the prior route.
//
// Two races make this the sharpest edge in the package, and both are closed
// here structurally rather than by timing luck:
//
//   - The dial runs on the Manager's context, so it outlives the caller. A
//     caller that gives up — its reconfigure deadline expires — leaves the dial
//     still running; it will settle a LIVE client into next. A guardian tracked
//     by m.wg closes that client whenever it lands, so an abandoned replacement
//     can never leak its subprocess/socket, whether it lands before or after the
//     caller returned, and Close waits for the guardian before it returns.
//   - Install is a compare-and-swap on the binding's identity: next replaces the
//     route only if the slot STILL holds the exact prior state this op set out
//     to replace, and only if the Manager has not closed. A remove, a newer
//     replace, or a Close that landed while next was dialing revokes the
//     authority to install — so a slow replacement can never resurrect a binding
//     a remove deliberately revoked, nor install a live client into a closed
//     Manager. bindingState pointer identity is the generation token: every op
//     installs a fresh state, and a removed one is never reinstalled, so there is
//     no ABA to defeat the comparison.
func (m *Manager) applyReplace(ctx context.Context, op BindingOp) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrManagerClosed
	}
	prior, exists := m.states[op.name]
	if !exists {
		m.mu.Unlock()
		return fmt.Errorf("binding does not exist")
	}
	next := newBindingState(op.binding)
	// keep is resolved once, with true when next is installed and false when it
	// is abandoned; the guardian closes next's client iff abandoned. Adding the
	// guardian to m.wg here, under m.mu with the closed check, is what lets Close
	// account for an in-flight replacement: a guardian only exists when it was
	// registered before Close set closed, so Close's wait always covers it.
	keep := make(chan bool, 1)
	m.wg.Add(1)
	go m.guardReplacement(next, keep)
	m.mu.Unlock()

	err := m.connectNow(ctx, next)

	if m.afterReplaceConnect != nil {
		m.afterReplaceConnect()
	}

	m.mu.Lock()
	stillOurs := m.states[op.name] == prior
	install := err == nil && !m.closed && stillOurs
	retirePrior := err != nil && op.failClosed && stillOurs
	switch {
	case install:
		m.states[op.name] = next
	case retirePrior:
		delete(m.states, op.name)
	}
	m.mu.Unlock()

	// Resolve the guardian before anything else: on the abandon path it is what
	// closes next, and it must not be left waiting.
	keep <- install

	if install {
		// Retire only after the new route is installed. Between these two lines a
		// new call finds the new route and an in-flight one still holds the old:
		// that overlap is the point (design §Binding reconfiguration — "a new
		// logical client before the old route is retired").
		m.retire(prior)
		return nil
	}
	if retirePrior {
		m.retire(prior)
	}
	switch {
	case err != nil && op.failClosed:
		return fmt.Errorf("replacement failed and policy is fail-closed; prior binding retired: %w", err)
	case err != nil:
		// The prior binding is untouched and still installed: a failed upgrade
		// must not cost the server that was working.
		return fmt.Errorf("replacement failed; prior binding remains active: %w", err)
	default:
		// The replacement connected, but the binding was removed, replaced again,
		// or the Manager closed while it dialed. Installing now would resurrect a
		// revoked binding or plant a live client in a closed Manager; the
		// guardian closes the new client instead.
		return fmt.Errorf("replacement connected but the binding was removed, replaced, or closed before it could install; the new client was closed")
	}
}

// guardReplacement closes a replacement's client if the replacement was
// abandoned rather than installed.
//
// It exists because a replacement dials on the Manager's context, not the
// caller's: the dial can settle a live client after the caller that asked for it
// has given up (its deadline expired), after a remove or newer replace
// superseded the binding, or after Close. Whoever made that client is gone, so
// this closes it. It waits for keep so it can never race the install that hands
// next to the route table, and it is tracked by m.wg so Close waits for it —
// making "a connected replacement is always reachable by shutdown" a structural
// property, not a timing-dependent one.
func (m *Manager) guardReplacement(next *bindingState, keep <-chan bool) {
	defer m.wg.Done()
	if <-keep {
		// Installed: the route table owns next now, and Close or a later
		// retirement closes it through the table.
		return
	}
	// Abandoned. The dial always settles — startConnect runs connect to
	// completion, which closes ready regardless of outcome — so wait for the
	// client it produces and close it. A dial that failed produced no client.
	<-next.ready
	cl := next.client()
	if cl == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(m.ctx), m.retireIn)
	defer cancel()
	_ = cl.Close(ctx)
}

func (m *Manager) applyRemove(_ context.Context, op BindingOp) error {
	m.mu.Lock()
	bs, exists := m.states[op.name]
	if exists {
		delete(m.states, op.name)
	}
	m.mu.Unlock()
	if !exists {
		return fmt.Errorf("binding does not exist")
	}
	m.retire(bs)
	return nil
}

// applyDisable retires the connection and keeps the configuration.
//
// The disabled state is installed as a NEW bindingState carrying the same
// immutable Binding, for the same reason a replacement is: the retiring state is
// still serving turns that hold it, and flipping a flag on it would change what
// they see.
func (m *Manager) applyDisable(_ context.Context, op BindingOp) error {
	m.mu.Lock()
	bs, exists := m.states[op.name]
	if !exists {
		m.mu.Unlock()
		return fmt.Errorf("binding does not exist")
	}
	bs.mu.Lock()
	already := bs.disabled
	b := bs.binding
	bs.mu.Unlock()
	if already {
		m.mu.Unlock()
		return nil
	}
	off := newBindingState(b)
	off.disabled = true
	off.settled = true
	close(off.ready)
	m.states[op.name] = off
	m.mu.Unlock()

	m.retire(bs)
	return nil
}

func (m *Manager) applyEnable(ctx context.Context, op BindingOp) error {
	m.mu.Lock()
	bs, exists := m.states[op.name]
	if !exists {
		m.mu.Unlock()
		return fmt.Errorf("binding does not exist")
	}
	bs.mu.Lock()
	disabled, b := bs.disabled, bs.binding
	bs.mu.Unlock()
	if !disabled {
		m.mu.Unlock()
		return nil
	}
	on := newBindingState(b)
	m.states[op.name] = on
	m.mu.Unlock()

	return m.connectNow(ctx, on)
}

// connectNow dials a binding and waits for the attempt, so a reconfiguring
// caller learns the outcome rather than discovering it in a status poll.
//
// The dial itself runs on the Manager's lifetime context, not on ctx: ctx bounds
// how long this caller waits, while the connection outlives the call that asked
// for it. A caller that gives up must not take the binding's reconnect down with
// it.
func (m *Manager) connectNow(ctx context.Context, bs *bindingState) error {
	m.startConnect(bs)
	select {
	case <-bs.ready:
		bs.mu.Lock()
		err := bs.failure
		bs.mu.Unlock()
		if err != nil {
			return err
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("did not connect before the deadline: %w", ctx.Err())
	case <-m.ctx.Done():
		return ErrManagerClosed
	}
}

// retire takes a route out of future generations and closes it once the turns
// still using it are done.
//
// The wait is bounded by the retirement deadline (design §Binding
// reconfiguration: "Active turns keep their existing route until their calls
// finish or the configured retirement deadline cancels them"). Unbounded, one
// wedged call would keep a revoked credential live indefinitely; the deadline is
// what makes a removal actually mean something.
func (m *Manager) retire(bs *bindingState) {
	idle := bs.markRetiring()
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		timer := time.NewTimer(m.retireIn)
		defer timer.Stop()
		select {
		case <-idle:
		case <-timer.C:
		case <-m.ctx.Done():
		}
		cl := bs.client()
		if cl == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(m.ctx), m.retireIn)
		defer cancel()
		_ = cl.Close(ctx)
	}()
}
