package presentation

import (
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/tui/styles"
)

// integrationKey identifies one integration within a session. Source is the
// namespace Name lives in (event.IntegrationStatus.Source), so an "mcp" binding
// called "github" and a language server of the same name are two distinct
// entries rather than one that overwrites the other.
type integrationKey struct {
	source string
	name   string
}

// integrationState is the last reported state of one integration. It carries no
// history: an event.IntegrationStatus is Ephemeral and self-healing — the next
// status supersedes the previous one completely — so the projection keeps only
// the latest per key.
type integrationState struct {
	key    integrationKey
	state  event.IntegrationState
	detail string
}

// integrationProjection is the event-authoritative view of every integration the
// session has reported on, in first-seen order.
//
// It folds event.IntegrationStatus and nothing else. That event is sessionScoped,
// so event.ShouldDeliver hands it to the TUI regardless of the subscription's
// loop filter, and it is Ephemeral, so it never appears in the restore backlog —
// a restored session learns each integration's state from the live status its
// host republishes after reconnecting, which is the only honest source (a
// journaled status could only describe a connection that no longer exists).
type integrationProjection struct {
	states map[integrationKey]integrationState
	order  []integrationKey
}

func newIntegrationProjection() integrationProjection {
	return integrationProjection{states: make(map[integrationKey]integrationState)}
}

// ApplyEvent folds one event into the projection, returning the updated value.
// Any event other than an IntegrationStatus is a no-op.
//
// A status whose State is not a declared value is DROPPED rather than recorded.
// event.ValidateEvent already rejects one at the publish boundary, so this is
// defence in depth at the consuming edge: an integration is not part of this
// module and its inputs are not this module's to trust. Dropping fails closed —
// the entry keeps its last known-good state instead of the surface inventing an
// "unknown" reading for a live external dependency.
func (p integrationProjection) ApplyEvent(ev event.Event) integrationProjection {
	status, ok := ev.(event.IntegrationStatus)
	if !ok || !status.State.Valid() {
		return p
	}
	key := integrationKey{source: status.Source, name: status.Name}
	_, exists := p.states[key]

	// Copy-on-write: the projection is driven by value (like runtimeProjection),
	// so a fold must never write through a map a prior value still shares.
	states := make(map[integrationKey]integrationState, len(p.states)+1)
	for k, v := range p.states {
		states[k] = v
	}
	states[key] = integrationState{key: key, state: status.State, detail: status.Detail}
	p.states = states
	if !exists {
		p.order = append(append([]integrationKey(nil), p.order...), key)
	}
	return p
}

// maxIntegrationDetailCells bounds the Detail text one entry contributes to the
// status line. event.MaxIntegrationDetailBytes allows 512 — prose sized for a
// panel, not for a shared single row — so the segment clips it to a fragment
// that names the failure without evicting the model/context metadata beside it.
const maxIntegrationDetailCells = 40

// integrationNeedsAttention reports whether an integration's state is worth a
// row an operator is already reading.
//
// Ready is excluded, and that asymmetry is the point: the status line is one row
// shared with the turn label, the elapsed timer, and the focused loop's runtime
// metadata, so a healthy integration must cost nothing. Every other state is an
// answer of "no" or "not yet" to the one question an operator asks about an
// integration, including Closed — a binding that has shut down is not serving,
// and silently dropping it would read as "fine".
func integrationNeedsAttention(s event.IntegrationState) bool {
	return s.Valid() && s != event.IntegrationReady
}

// integrationShowsDetail reports whether an entry's Detail is rendered. Only the
// two recoverable-fault states carry it: Detail is diagnostics, and "why" only
// earns row space when something is actually wrong. Starting and Closed are
// self-explanatory from the state word alone.
func integrationShowsDetail(s event.IntegrationState) bool {
	return s == event.IntegrationDegraded || s == event.IntegrationFailed
}

// integrationStyle maps an integration state onto the module's existing severity
// vocabulary (styles.Notice*): a degraded integration may recover on its own and
// reads as a warning, a failed one reads as an error, and the non-fault states
// stay faint like the rest of the status line. Mapping onto the shared severity
// styles rather than inventing colors keeps a failed binding looking like every
// other failure the TUI reports.
func integrationStyle(s event.IntegrationState) lipgloss.Style {
	switch s {
	case event.IntegrationDegraded:
		return styles.NoticeWarnStyle
	case event.IntegrationFailed:
		return styles.NoticeErrorStyle
	default:
		return styles.StatusStyle
	}
}

// integrationSeparator joins two rendered entries on the status line.
const integrationSeparator = " · "

// statusSegment renders the integrations needing attention as one compact,
// styled status-line fragment — "mcp:github failed — handshake refused" — joined
// by a faint separator, in first-seen order. It returns "" when every
// integration is Ready (or none has reported), so the common case adds nothing
// to the row.
//
// It is the whole integration surface. The status line is the one place an
// operator already watches for "what is this session doing right now", and an
// integration's liveness is exactly that question, so this appends to the
// existing row (like focusedRuntimeStatus) rather than claiming a row of its
// own — the active surface's height budget is a hard invariant for the inline
// renderer, and a new row would have to be reserved out of the live tail.
func (p integrationProjection) statusSegment() string {
	if len(p.order) == 0 {
		return ""
	}
	parts := make([]string, 0, len(p.order))
	for _, key := range p.order {
		state, ok := p.states[key]
		if !ok || !integrationNeedsAttention(state.state) {
			continue
		}
		parts = append(parts, integrationStyle(state.state).Render(integrationEntry(state)))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, styles.StatusStyle.Render(integrationSeparator))
}

// integrationEntry renders one entry's plain text: the qualified identity, its
// state word, and — for a fault state — the clipped Detail.
func integrationEntry(state integrationState) string {
	label := state.key.source + ":" + state.key.name + " " + state.state.String()
	if state.detail == "" || !integrationShowsDetail(state.state) {
		return label
	}
	return label + " — " + truncate(state.detail, maxIntegrationDetailCells)
}
