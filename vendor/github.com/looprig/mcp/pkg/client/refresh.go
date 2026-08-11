// This file implements the design's change-notification sequence: a server says
// a list changed, and the binding turns that into a validated candidate
// generation the caller may adopt when it is safe to do so.
//
//	notification -> stale -> (coalesce) -> refetch -> validate -> compare
//	             -> publish candidate -> caller adopts at a safe boundary
//
// Three properties are worth stating because they are the reason this is not
// simply "refetch on notify".
//
// # The client never adopts by itself
//
// Adoption changes what a model sees. Only the caller knows when that is safe —
// no turn in flight, no call from the previous generation still running — so the
// client publishes and waits. Adopt is the caller's, and it is the *only* way an
// adopted generation changes after startup. The client's own refresh path writes
// c.candidate and never c.generation.
//
// # A failed refresh changes nothing
//
// The prior generation stays adopted, the binding reports degraded, and the
// retry is bounded. A server that starts answering badly makes its *changes*
// unavailable; it does not make its existing tools unavailable, and it must not
// be able to blank a working catalog by failing a list call.
//
// # Notifications are coalesced, not queued
//
// A server that changes its tool list ten times in a second is describing one
// eventual catalog, not ten. What matters is that a refresh *starts after the
// last notification*, which one in-flight refresh plus one queued refresh
// guarantees — and which ten sequential refetches of the same server would
// guarantee no better, at ten times the cost.

package client

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/looprig/mcp/internal/catalog"
	"github.com/looprig/mcp/internal/lifecycle"
	"github.com/looprig/mcp/internal/protocol"
	"github.com/looprig/mcp/internal/sched"
)

// Operation names carried by the errors and events in this file.
const (
	opRefresh = "refresh"
	opAdopt   = "adopt"
)

// familyOf maps a protocol list-change family onto the catalog family it makes
// stale. It fails closed: an unknown family (a protocol enum this build does not
// know) marks nothing and is dropped by the caller, rather than being guessed at.
func familyOf(f protocol.ListFamily) (catalog.Family, bool) {
	switch f {
	case protocol.ListFamilyTools:
		return catalog.FamilyTools, true
	case protocol.ListFamilyPrompts:
		return catalog.FamilyPrompts, true
	case protocol.ListFamilyResources:
		return catalog.FamilyResources, true
	default:
		return 0, false
	}
}

// onListChanged records a server's list-change notification and wakes the
// refresher.
//
// It runs on the connection's notification goroutine, so it must not fetch,
// block, or wait: it marks the family stale, tells the application, and returns.
// The refetch happens on the refresh worker, which is the whole reason that
// worker exists.
func (c *Client) onListChanged(ch protocol.ListChange) {
	fam, ok := familyOf(ch.Family)
	if !ok {
		return
	}

	c.mu.Lock()
	c.stale[fam] = struct{}{}
	c.mu.Unlock()

	c.emit(CatalogStale{Binding: c.def.Name, Family: fam.String(), At: time.Now()})
	c.signalRefresh()
}

// signalRefresh asks the refresh worker for a pass, coalescing with any pass
// already requested.
//
// The channel is buffered to one and the send is non-blocking, which is the
// whole coalescing mechanism: a request already queued absorbs every later one
// until the worker takes it. That is sound because a pass carries no
// information — it means "refetch everything", not "refetch this change" — so
// two queued passes could only ever do identical work.
func (c *Client) signalRefresh() {
	select {
	case c.refreshCh <- struct{}{}:
	default:
	}
}

// runRefresher is the refresh worker. It owns every catalog refetch after
// startup; nothing else in the client fetches a catalog on a live connection, so
// two refreshes can never overlap and a candidate can never be built from two
// interleaved fetches.
//
// It exits only when its context is cancelled, which Close does.
func (c *Client) runRefresher(ctx context.Context) {
	defer close(c.refresherDone)
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.refreshCh:
			c.refreshWithRetry(ctx)
		}
	}
}

// refreshWithRetry runs one refresh pass, retrying under the binding's bounded
// policy until it succeeds, the budget runs out, or the client shuts down.
func (c *Client) refreshWithRetry(ctx context.Context) {
	budget := newRetrySchedule(c.def.Refresh, time.Now())
	for {
		delay, ok := budget.next(time.Now())
		if !ok {
			return
		}
		if !sleepCtx(ctx, delay) {
			return
		}

		err := c.refreshOnce(ctx)
		if err == nil {
			return
		}
		if ctx.Err() != nil {
			// Shutdown, not a server failure. There is nobody left to report to
			// and nothing left to retry.
			return
		}

		// Whether this failure is the last one is part of what the application is
		// told, so it is answered before the report rather than after: a copy of
		// the schedule is asked what it would do next, without consuming it.
		peek := budget
		_, retrying := peek.next(time.Now())
		c.reportRefreshFailure(err, retrying)
	}
}

// sleepCtx waits for d, reporting false if ctx ended first. A zero or negative
// d does not wait at all, but still reports a cancelled context, so a shutdown
// is noticed on the first attempt rather than only on a retry.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if err := ctx.Err(); err != nil {
		return false
	}
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// refreshOnce fetches one complete candidate and publishes it if it differs from
// what is adopted.
//
// The fetch is all-or-nothing (see catalog.Discover): this either has a whole
// new generation to compare, or an error and nothing else. There is no path here
// that half-updates anything.
func (c *Client) refreshOnce(ctx context.Context) error {
	conn, epoch, err := c.serving(opRefresh)
	if err != nil {
		return err
	}
	handshake, ok := c.handshake()
	if !ok {
		return NewError(FailureIndeterminate, c.def.Name, opRefresh,
			"the binding has no handshake to refresh against", nil)
	}

	// A refresh is the same work as startup discovery — the same list methods,
	// the same pagination, the same bounds — so it gets the same bound: a
	// server cannot make a refresh outlive what its initial discovery was
	// allowed to take.
	fetchCtx, cancel := context.WithTimeout(ctx, c.def.Timeouts.Startup)
	defer cancel()

	// A refresh is a control operation: it is serialized against the binding's
	// other ordering-sensitive work, and it is counted against the binding's
	// concurrency budget like any other request. A background refetch that
	// ignored the budget would be a request the operator's limit does not
	// cover, arriving exactly when a server is already misbehaving.
	fetchCtx, release, err := c.sched.Begin(fetchCtx, sched.ClassControl)
	if err != nil {
		if errors.Is(err, sched.ErrShutdown) {
			return NewError(FailureShutdown, c.def.Name, opRefresh, "the binding is shutting down", nil)
		}
		return c.classify(fetchCtx, opRefresh, err, FailureCatalogStale)
	}
	defer release()

	gen, err := catalog.Discover(fetchCtx, conn, catalog.Config{
		Binding:    string(c.def.Name),
		Number:     c.reserveGeneration(),
		Handshake:  handshake,
		Limits:     c.def.Limits.catalog(),
		Tolerances: c.def.Compat.tolerances(),
	})
	if err != nil {
		// A refresh is how a binding with no traffic finds out its connection
		// died: nothing else touches the wire until a caller makes a call.
		return c.noteFailure(epoch, c.classify(fetchCtx, opRefresh, err, discoveryClass(err)))
	}
	c.publish(gen)
	return nil
}

// publish decides what a freshly validated generation means and tells the
// application.
//
// This is the design's compare step, and the comparison is by digest rather than
// by ordinal: a refresh always produces a new generation *number*, but a server
// that announced a change and then served the identical catalog has changed
// nothing this binding can see. Publishing that as a candidate would ask the
// caller to find a safe boundary and swap a toolset for no reason.
func (c *Client) publish(gen *catalog.Generation) {
	c.mu.Lock()
	// Every announced family has now been refetched, whatever the outcome of the
	// comparison below: staleness describes what has not been looked at, not
	// what has not changed.
	clear(c.stale)

	adopted := c.generation
	prior := c.candidate
	var (
		unchanged bool
		duplicate bool
	)
	switch {
	case adopted != nil && gen.Digest() == adopted.Digest():
		// Back to what is already in force. Any outstanding candidate described
		// a change that has since been reverted, so it is not merely stale — it
		// is wrong, and must not remain adoptable.
		c.candidate = nil
		unchanged = true
	case prior != nil && gen.Digest() == prior.Digest():
		// The same change, seen again — a second notification for one edit, or a
		// retry after a failure. Keeping the existing candidate keeps its
		// generation number stable, so a caller that already read Candidate and
		// is on its way to Adopt is not defeated by a refresh that told it
		// nothing new.
		duplicate = true
	default:
		c.candidate = gen
	}
	number, digest, adoptedNumber := candidateFacts(c.candidate, adopted)
	c.mu.Unlock()

	// Report before recovering the state: an application watching events sees
	// the catalog outcome and the state change in the order they happened.
	switch {
	case unchanged:
		c.emit(CatalogRefreshed{
			Binding:    c.def.Name,
			Generation: adoptedNumber,
			Digest:     digest,
			At:         time.Now(),
		})
	case !duplicate:
		c.emit(CatalogCandidate{
			Binding:    c.def.Name,
			Generation: number,
			Digest:     digest,
			Adopted:    adoptedNumber,
			At:         time.Now(),
		})
	}

	// A refresh that worked clears a degradation a previous one caused. It is
	// best-effort: a machine that has moved on (closing, failed) is not dragged
	// back to ready by a late success.
	_ = c.machine.To(lifecycle.StateReady)
}

// candidateFacts reads the reportable facts out of a candidate and the adopted
// generation. It exists to keep the lock's critical section free of anything but
// reads: a Generation's accessors are cheap and lock-free, but they are still
// calls, and the emitters below must not be reached while holding c.mu.
func candidateFacts(candidate, adopted *catalog.Generation) (number uint64, digest string, adoptedNumber uint64) {
	if adopted != nil {
		adoptedNumber = adopted.Number()
		digest = adopted.Digest().String()
	}
	if candidate != nil {
		number = candidate.Number()
		digest = candidate.Digest().String()
	}
	return number, digest, adoptedNumber
}

// reportRefreshFailure records a failed refresh and tells the application.
//
// The binding is marked degraded rather than failed: it is still serving every
// call its adopted generation supports, and the only thing it has lost is the
// ability to see the server's changes. Marking it failed would take a working
// binding out of service over a stale catalog.
func (c *Client) reportRefreshFailure(err error, retrying bool) {
	var typed *Error
	if !errors.As(err, &typed) {
		// An explicit message, not the cause's own text: see failureMessage for
		// why an event never renders a wrapped error. The cause is still
		// wrapped, so it remains available to a caller that has the error
		// itself — it just does not travel to an event handler.
		typed = NewError(FailureCatalogStale, c.def.Name, opRefresh, "the catalog refresh failed", err)
	}
	c.recordFailure(typed)

	// Best-effort, for the reason above: ready -> degraded is legal, and every
	// other current state either already says something stronger (failed,
	// closing) or is on its way somewhere (reconnecting).
	_ = c.machine.To(lifecycle.StateDegraded)

	c.emit(CatalogRejected{
		Binding:  c.def.Name,
		Class:    typed.Class,
		Message:  failureMessage(typed),
		Adopted:  c.adoptedNumber(),
		Retrying: retrying,
		At:       time.Now(),
	})
}

// failureMessage renders an error's text for an event, and is the redaction
// boundary for everything this module publishes to an EventHandler.
//
// It carries the *Error's own Msg and nothing else. Msg is written by this
// module, at the site that classified the failure, and is bounded at
// construction (NewError) — so it is text we chose, about a failure we
// classified.
//
// What it deliberately does not do is fall back to the wrapped cause's text, the
// way Error.Error does. A wrapped cause is a transport's or a server's own
// error, and those are exactly where a credential ends up in practice: net/http
// renders the request URL into its errors verbatim, userinfo and all, so
// `dial https://user:token@host` is one wrapped error away from every event
// handler, journal and telemetry sink an application installs. The design's rule
// for events is explicit — "authorization URLs containing secrets, headers,
// tokens ... are excluded" — and bounding that text would only make the leak
// shorter.
//
// The cost is that an *Error constructed with no Msg contributes only its class.
// That is the right trade: a class is always safe and usually enough, and the
// remedy is to write a message at the classification site, which the emitters
// here do.
func failureMessage(err *Error) string {
	if err.Msg != "" {
		return err.Msg
	}
	// No message was written for this failure. Say what is known — the class —
	// rather than reaching for text this module did not author.
	return "the binding reported a " + err.Class.String() + " failure"
}

// adoptedNumber returns the adopted generation's ordinal, or 0.
func (c *Client) adoptedNumber() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.generation == nil {
		return 0
	}
	return c.generation.Number()
}

// reserveGeneration hands out the next generation ordinal.
//
// Ordinals are monotonic but not dense: a failed refresh consumes one and
// publishes nothing, so the sequence has holes. That is deliberate — an ordinal
// identifies a generation, it does not count them — and it is what keeps a
// number from ever being reused for different content, which is the property
// Adopt depends on.
func (c *Client) reserveGeneration() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastGeneration++
	return c.lastGeneration
}

// handshake returns what initialize settled, and whether it has settled at all.
func (c *Client) handshake() (protocol.InitializeResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.protocolVersion == "" {
		return protocol.InitializeResult{}, false
	}
	return c.initResult, true
}

// Candidate returns the binding's outstanding candidate generation: a complete,
// validated catalog that differs from the adopted one and is waiting to be
// adopted.
//
// It reports false when there is none, which is the normal state of a binding
// whose server has not changed. A candidate is not a promise that adopting it
// will succeed: a later refresh may supersede or withdraw it (see Adopt).
//
// Like Catalog, it is the model-facing projection — the binding's ToolFilter is
// applied on the way out.
func (c *Client) Candidate() (Catalog, bool) {
	c.mu.Lock()
	gen := c.candidate
	c.mu.Unlock()
	if gen == nil {
		return Catalog{}, false
	}
	return c.projectCatalog(gen), true
}

// Adopt makes the candidate generation the binding's adopted catalog.
//
// The caller decides when. That is the design's rule and it is not a detail:
// adoption changes what a model sees, so it belongs at a boundary where nothing
// is mid-turn — knowledge this client does not have and must not guess at. What
// the client enforces is the other half: that adoption targets a candidate it
// validated, and the one the caller actually looked at.
//
// generation must be the ordinal from Candidate. Passing any other value — a
// generation already adopted, one superseded by a later refresh, or one that was
// withdrawn when the server reverted its change — is refused with
// FailureCatalogStale rather than resolved to "whatever is current now". A
// caller that read a candidate, decided it was safe, and raced a refresh must
// re-read and re-decide; the client cannot make that decision for it, because
// the safety it established was about the catalog it saw.
//
// Adoption is atomic and cannot half-apply: the generation is immutable and
// already validated, so this is a pointer swap. On success the candidate is
// consumed — a generation is adopted once.
func (c *Client) Adopt(generation uint64) error {
	switch state := c.machine.State(); state {
	case lifecycle.StateClosing, lifecycle.StateClosed:
		return NewError(FailureShutdown, c.def.Name, opAdopt, "the binding is closed", nil)
	}

	c.mu.Lock()
	cand := c.candidate
	switch {
	case cand == nil:
		c.mu.Unlock()
		return NewError(FailureCatalogStale, c.def.Name, opAdopt,
			fmt.Sprintf("generation %d is not adoptable: the binding has no candidate", generation), nil)
	case cand.Number() != generation:
		current := cand.Number()
		c.mu.Unlock()
		return NewError(FailureCatalogStale, c.def.Name, opAdopt,
			fmt.Sprintf("generation %d is not adoptable: the candidate is generation %d", generation, current), nil)
	}
	var previous uint64
	if c.generation != nil {
		previous = c.generation.Number()
	}
	c.generation = cand
	c.candidate = nil
	digest := cand.Digest().String()
	c.mu.Unlock()

	c.emit(CatalogAdopted{
		Binding:    c.def.Name,
		Generation: generation,
		Digest:     digest,
		Previous:   previous,
		At:         time.Now(),
	})
	return nil
}

// staleFamilies returns the families a server has announced a change to and
// which have not been refetched since, as their stable identifiers in a
// deterministic order.
func (c *Client) staleFamilies() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.stale) == 0 {
		return nil
	}
	out := make([]string, 0, len(c.stale))
	for f := range c.stale {
		out = append(out, f.String())
	}
	slices.Sort(out)
	return out
}
