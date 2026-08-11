// This file moves a validated candidate catalog into a Loop's live toolset, and
// it does it at the only moment that is safe: the Loop's own idle.
//
// The shape follows design §Catalog model exactly, and the diagram there is the
// whole specification:
//
//	generation 5 [candidate, validated]
//	        |
//	Loop A reaches idle ----------------> generation 5 [adopted by A]
//	        |
//	Loop B remains active
//	        |
//	Loop B reaches idle ----------------> generation 5 [adopted by B]
//
// Two things in that picture are easy to conflate and must not be. A binding
// adopts a generation once — it is one connection with one current view of one
// server, and Client.Adopt is a pointer swap. A *Loop* adopts a toolset
// separately, at its own boundary, because a toolset is what a turn is running
// under and swapping it mid-turn would change the tools out from under a model
// that has already been shown them. So Loop A's idle moves the binding to
// generation 5 AND installs A's snapshot; Loop B goes on running the snapshot it
// already holds — genuinely the old generation's tools — until B parks, and B
// installs then. Nothing waits for anything.
//
// Every failure here leaves the Loop with the toolset it had. That is not this
// file's achievement: loop.ExternalToolInstaller is atomic and refuses whole
// replacements (an unbuildable definition, a name colliding with a declared
// tool), so the failure modes this file must get right are the ones about
// noticing, reporting, and trying again at the next boundary — never about
// half-installing.

package mcpharness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/harness/pkg/loop"
)

// installTimeout bounds one replacement.
//
// A Loop at idle acks a replacement promptly — that is what idle means — so
// this is not a latency budget, it is a liveness guard: the adapter services
// every Loop's boundary from one goroutine, and a Loop that has stopped
// answering must not stall every other Loop's adoption behind it. The Loop that
// timed out simply keeps its toolset and is tried again at its next idle.
const installTimeout = 30 * time.Second

// EventSource is the Session's event stream, narrowed to the one method the
// adapter calls.
//
// The signature is session.Session's rather than *hub.Hub's: the Hub's
// SubscribeEvents returns its own concrete *hub.EventSubscription, so a
// consumer-side interface naming event.Subscription is not satisfied by the Hub
// structurally, while every session.Session is. That is the right way round —
// the Session is the public contract and the Hub is its machinery — but it does
// mean a host holding only a Hub must pass a two-line adapter. See the report
// accompanying this stage.
type EventSource interface {
	SubscribeEvents(event.EventFilter) (event.Subscription, error)
}

// LoopControllers resolves a Loop's control surface. It is satisfied by
// harness's session.SessionController, and it is the narrowest thing that will
// do: the adapter needs to reach one Loop's installer, not to drive a Session.
type LoopControllers interface {
	LoopController(uuid.UUID) (loop.Controller, bool)
}

// Adopter installs each Loop's MCP toolset at that Loop's idle boundary.
//
// It is safe for concurrent use and owns one goroutine, which is the only place
// a replacement is issued. Serializing the boundaries is deliberate: two idles
// racing would otherwise have to be told apart by a lock held across a command
// to a Loop actor, and adoption is rare, cheap, and never on a turn's critical
// path. Loops still adopt independently — sequential handling orders the work,
// it does not couple the boundaries.
type Adopter struct {
	m     *Manager
	loops LoopControllers
	sub   event.Subscription

	cancel context.CancelFunc
	done   chan struct{}

	mu sync.Mutex
	// installed maps a Loop to the generation signature it currently holds. A
	// Loop absent from the map holds nothing from this source.
	//
	// It is what makes an idle cheap: the common case is a Loop parking with
	// nothing to change, and comparing signatures answers that without building
	// a single tool.
	installed map[uuid.UUID]string
	// unsupported records the Loops that can never host external tools (a
	// foreign loop's toolset belongs to its foreign agent). It is a permanent
	// property of the Loop, so it is remembered rather than rediscovered: the
	// alternative is a refusal, a report, and a wasted build at every single
	// idle for the life of the Session.
	unsupported map[uuid.UUID]struct{}
}

// StartAdoption subscribes to the Session's idle boundaries and installs each
// Loop's toolset as they arrive.
//
// It does not install anything for a Loop that is already running: a Loop's
// first toolset is the application's to install, at composition time, with
// Install. This only reacts to boundaries.
//
// Close stops it. The Manager's own Close does not: an Adopter is an optional
// capability layered over a Manager, and a Manager that closed its host's
// subscription would be reaching outside what it was given.
//
// It refuses a Manager that is not bound to a Session (ErrNotBound). That is
// where the check for a forgotten BindSession belongs: adoption is the point at
// which a Manager stops being a set of connections and starts serving a Session's
// Loops, so an unbound Manager here is one whose every integration status would be
// dropped for the life of that Session — see attach.go. Deps.SessionID cannot
// fail closed at construction instead, because "not yet" is a legitimate answer
// there and is the whole reason this seam exists.
func (m *Manager) StartAdoption(source EventSource, loops LoopControllers) (*Adopter, error) {
	if source == nil {
		return nil, fmt.Errorf("mcp: StartAdoption: source is nil; supply an EventSource")
	}
	if loops == nil {
		return nil, fmt.Errorf("mcp: StartAdoption: loops is nil; supply a LoopControllers")
	}
	if _, bound := m.boundSession(); !bound {
		return nil, fmt.Errorf("mcp: StartAdoption: %w", ErrNotBound)
	}
	// Enduring, every Loop: LoopIdle is an enduring loop-scoped event, and the
	// set of Loops is not known in advance — a delegate spawned in an hour must
	// reach its boundary too.
	sub, err := source.SubscribeEvents(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		return nil, fmt.Errorf("mcp: StartAdoption: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := &Adopter{
		m:           m,
		loops:       loops,
		sub:         sub,
		cancel:      cancel,
		done:        make(chan struct{}),
		installed:   make(map[uuid.UUID]string),
		unsupported: make(map[uuid.UUID]struct{}),
	}
	go a.run(ctx)
	return a, nil
}

// run services boundaries until the subscription ends or Close is called.
func (a *Adopter) run(ctx context.Context) {
	defer close(a.done)
	for {
		select {
		case delivery, open := <-a.sub.Events():
			if !open {
				return
			}
			idle, ok := delivery.Event.(event.LoopIdle)
			if !ok {
				continue
			}
			header := idle.EventHeader()
			a.boundary(ctx, header.LoopID, string(header.AgentName))
		case <-ctx.Done():
			return
		}
	}
}

// Close stops the Adopter and releases its subscription. It is idempotent and
// waits for the goroutine, so a caller that returns from Close knows no further
// replacement will be issued.
func (a *Adopter) Close() error {
	a.cancel()
	err := a.sub.Close()
	<-a.done
	return err
}

// Install installs the identified Loop's MCP toolset now, without waiting for a
// boundary.
//
// It exists for the one moment a boundary cannot serve: a Loop's first toolset.
// A Loop that has never run has never parked, so nothing has signalled an idle,
// and a Loop's first turn would otherwise run with no MCP tools at all. The
// caller is the composition root, which knows the Loop is not mid-turn because
// it has not started it yet.
//
// It is otherwise the same operation as a boundary, including the
// generation-signature check, so calling it on a Loop that is already current
// is free and installs nothing.
func (a *Adopter) Install(ctx context.Context, loopID uuid.UUID, loopName string) error {
	return a.install(ctx, loopID, loopName)
}

// boundary handles one Loop's idle. It never returns an error: an idle is a
// notification, not a request, and there is nobody to return one to.
func (a *Adopter) boundary(ctx context.Context, loopID uuid.UUID, loopName string) {
	callCtx, cancel := context.WithTimeout(ctx, installTimeout)
	defer cancel()
	_ = a.install(callCtx, loopID, loopName)
}

// install brings one Loop's toolset up to date with its bindings' current
// generations.
func (a *Adopter) install(ctx context.Context, loopID uuid.UUID, loopName string) error {
	if loopID.IsZero() {
		return fmt.Errorf("mcp: install: loopID is zero")
	}
	a.mu.Lock()
	_, refused := a.unsupported[loopID]
	a.mu.Unlock()
	if refused {
		return nil
	}

	// Adopt first, then build. The snapshot must be cut from what the binding
	// has adopted — that is what every adapted tool's generation check compares
	// against, and building from a candidate the binding had not adopted would
	// hand the Loop tools that fail their own check on first use.
	//
	// This is the moment the binding moves, and it is this Loop's idle that
	// moves it. Another Loop mid-turn keeps the snapshot it is running; its
	// tools go on working while their raw names and schema digests still match,
	// and fail as ToolUnavailable or ToolSchemaChanged when they do not. That
	// is design §Catalog model's Loop B, and it is why adoption is safe to do
	// here rather than only when every Loop is parked.
	states := a.visible(loopID, loopName)
	for _, bs := range states {
		a.adoptCandidate(loopID, bs)
	}

	signature := a.signature(states)
	a.mu.Lock()
	current := a.installed[loopID]
	a.mu.Unlock()
	if signature == current {
		// Nothing moved. The overwhelmingly common idle.
		return nil
	}

	installer, ok := a.installerFor(loopID)
	if !ok {
		// The Loop is gone, or it is not one this host can install onto. Either
		// way it is not a fault: a Loop closing concurrently with its own idle
		// is ordinary, and there is nothing left to install onto.
		a.forget(loopID)
		return nil
	}

	defs := append(a.m.SessionTools(loopID, loopName), a.m.LoopTools(loopID)...)
	err := installer.ReplaceExternalTools(ctx, loop.ExternalToolset{
		Source:      ToolSource,
		Generation:  signature,
		Definitions: defs,
	})
	if err != nil {
		a.installFailed(loopID, signature, err)
		return err
	}

	a.mu.Lock()
	a.installed[loopID] = signature
	a.mu.Unlock()
	a.m.report(Notice{
		Kind:    NoticeAdopted,
		LoopID:  loopID,
		Message: fmt.Sprintf("installed %d MCP tool definitions as generation %s", len(defs), signature),
	})
	return nil
}

// visible returns every binding the identified Loop may consume, shared and
// owned.
func (a *Adopter) visible(loopID uuid.UUID, loopName string) []*bindingState {
	return append(a.m.sessionRoutes(loopID, loopName), a.m.loopRoutes(loopID)...)
}

// adoptCandidate moves one binding onto its validated candidate, if it holds
// one.
//
// The ordinal handed to Adopt is the one just read, which is the whole of the
// client's contract: Adopt refuses a superseded generation rather than
// resolving it to "whatever is current now", because the safety this boundary
// established was about the catalog it saw. A refusal is therefore not an
// error to escalate — it means a refresh landed in the last few microseconds
// and published a newer candidate, and the newer one is adopted at this Loop's
// next idle. The Loop is installed from the adopted catalog either way, so it
// gets a coherent generation regardless of who won.
func (a *Adopter) adoptCandidate(loopID uuid.UUID, bs *bindingState) {
	cl := bs.client()
	if cl == nil {
		return
	}
	cand, ok := cl.Candidate()
	if !ok {
		return
	}
	if err := cl.Adopt(cand.Generation); err != nil {
		a.m.report(Notice{
			Kind:       NoticeAdoptionFailed,
			Binding:    bs.binding.Name,
			LoopID:     loopID,
			Generation: cand.Generation,
			Message:    "the candidate was superseded before it could be adopted; the newer one is adopted at the next boundary",
		})
	}
}

// installerFor resolves a Loop's external-tool installer.
//
// The capability is discovered by type assertion, which is how Harness's
// optional interfaces are meant to be found (loop.ExternalToolInstaller mirrors
// loop.ModeCatalog). A controller without it is a Loop this host cannot install
// onto, and the fail-closed answer is to install nothing.
func (a *Adopter) installerFor(loopID uuid.UUID) (loop.ExternalToolInstaller, bool) {
	ctrl, ok := a.loops.LoopController(loopID)
	if !ok {
		return nil, false
	}
	installer, ok := ctrl.(loop.ExternalToolInstaller)
	return installer, ok
}

// installFailed classifies a refused replacement. The Loop keeps the generation
// it had — the installer is atomic — so the only decisions here are what to
// report and whether to try again.
func (a *Adopter) installFailed(loopID uuid.UUID, signature string, err error) {
	var change *loop.ChangeError
	if errors.As(err, &change) {
		switch change.Kind {
		case loop.ChangeExternalToolsUnsupported:
			// A foreign loop's toolset belongs to its foreign agent, and no
			// amount of retrying changes that. Remember it: rediscovering it at
			// every idle for the life of the Session would be a build and a
			// refusal each time, and a report nobody can act on.
			a.mu.Lock()
			a.unsupported[loopID] = struct{}{}
			delete(a.installed, loopID)
			a.mu.Unlock()
			a.m.report(Notice{
				Kind:    NoticeAdoptionUnsupported,
				LoopID:  loopID,
				Message: "this loop cannot host external tools; its toolset belongs to its foreign agent",
			})
			return
		case loop.ChangeLoopExited, loop.ChangeLoopShuttingDown:
			// The Loop went away between its idle and this replacement. Not a
			// fault, and nothing to retry onto.
			a.forget(loopID)
			return
		}
	}
	// Everything else is worth another attempt: the Loop's next idle will see
	// the same signature mismatch and try again, because nothing was recorded
	// as installed. A build failure, a collision with a declared tool, a
	// cancelled context — all leave the prior generation installed and all
	// resolve themselves the moment the underlying cause does.
	a.m.report(Notice{
		Kind:    NoticeAdoptionFailed,
		LoopID:  loopID,
		Message: fmt.Sprintf("the replacement to generation %s was refused; the loop keeps its current toolset: %v", signature, err),
	})
}

// forget drops a Loop's bookkeeping.
func (a *Adopter) forget(loopID uuid.UUID) {
	a.mu.Lock()
	delete(a.installed, loopID)
	a.mu.Unlock()
}

// signature identifies the toolset one Loop should be holding: every visible
// binding, and the generation each has adopted.
//
// It is a digest for two reasons. loop.ExternalToolset.Generation is bounded to
// 128 bytes and a Session may have more bindings than that allows, and it is
// recorded durably in event.LoopExternalToolsetChanged — an operator asking
// "which catalog did this turn run under" needs an identity that is stable and
// comparable, not a list that grows. The input is name-and-ordinal only: no
// server content is hashed, so the digest cannot be steered by a server.
//
// The empty signature means "no bindings", and it is deliberately the same
// value as a Loop that has never been installed. Both should end up holding no
// MCP tools, so treating them alike is correct — and a Loop whose last binding
// is removed still gets a replacement, because its previous signature was not
// empty.
func (a *Adopter) signature(states []*bindingState) string {
	if len(states) == 0 {
		return ""
	}
	h := sha256.New()
	for _, bs := range states {
		cl := bs.client()
		if cl == nil {
			continue
		}
		cat := cl.Catalog()
		if !cat.Valid() {
			continue
		}
		// The catalog digest, not just the ordinal: two generations of one
		// binding may carry the same tools, and the digest is what says whether
		// a rebuild would produce anything different.
		fmt.Fprintf(h, "%s@%d:%s\n", bs.binding.Name, cat.Generation, cat.Digest)
	}
	return hex.EncodeToString(h.Sum(nil))
}
