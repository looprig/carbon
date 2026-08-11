// session.go implements Session (one ACP session on a Client's connection)
// and Client's session-lifecycle methods: NewSession, LoadSession,
// ResumeSession. Prompt/Cancel live in prompt.go; inbound session/update
// routing and dedup live here (deliver), since they are intrinsic to a
// Session's identity and lifetime.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/looprig/acp/protocol"
)

// sessionUpdateQueueHint is an initial capacity hint for a Session's
// internal update queue slice. It bounds nothing by itself — it only avoids
// a few early reallocations for the common case — the actual bound is
// UpdateQueueDepth, enforced by deliver.
const sessionUpdateQueueHint = 8

// UpdateQueueDepth bounds how many not-yet-delivered updates a Session's
// internal queue holds before it starts dropping the OLDEST queued update to
// make room for the newest, mirroring protocol.Conn's NotifyBufferDepth
// (see Conn.DroppedNotifications) for the identical bounded-buffer,
// drop-oldest, observable-counter shape. A caller that calls Updates() and
// drains it promptly — the expected steady state, and the shape of every
// existing Task 5.1 test — never sees a drop: this bound only bites a
// consumer that falls behind delivery by more than UpdateQueueDepth updates,
// trading unbounded memory growth in a permanently-non-draining session for
// bounded, observable loss (see Session.DroppedUpdates). The oldest entry is
// dropped rather than the newest because a client actively draining
// Updates() cares about catching up to CURRENT state, not preserving
// ancient history it may never read anyway. 512 matches NotifyBufferDepth's
// existing precedent in this module rather than inventing a new value.
const UpdateQueueDepth = 512

// EventDedupWindowDepth bounds how many distinct live _meta.eventIds a
// Session remembers for dedup (see deliver). Harness event ids
// (event.Header.EventID, minted by event.Factory.Stamp via
// github.com/looprig/core/uuid.New, which reads crypto/rand) are random
// UUIDv4 values, so the id VALUE itself carries no ordering information a
// highwater mark could compare against.
//
// Delivery ORDER does, however, carry a real ordering guarantee: every
// session/update notification for one Client is drained by
// protocol.Conn's single notifyWorker goroutine strictly in wire order,
// completing each job before starting the next (see Conn.notifyWorker's
// doc), so successive calls to deliver for one session happen in true
// chronological order even though the ids themselves are unordered. A
// genuine duplicate (redelivery/retry) is therefore expected to reappear
// shortly after the original, not arbitrarily far in the future — so
// remembering only the most recently delivered EventDedupWindowDepth ids
// (an insertion-order window; the oldest is evicted first once the window
// is full) is enough to catch every realistic duplicate while keeping the
// dedup map's memory bounded across a session's full, potentially
// unbounded, lifetime. An id that reappears after it has aged out of the
// window is (by this deliberate tradeoff) no longer recognized as a
// duplicate and is delivered again — bounded, observable-in-principle loss
// traded for bounded memory, exactly as UpdateQueueDepth trades update loss
// for the same property. 512 matches UpdateQueueDepth/NotifyBufferDepth's
// existing precedent rather than inventing a new value.
const EventDedupWindowDepth = 512

type queuedUpdate struct {
	update Update
	id     uint64
}

type deliveryRange struct {
	start uint64
	end   uint64
}

type deliveryWaiter struct {
	target uint64
	done   chan error
}

// NewSessionParams are the caller-supplied parameters for Client.NewSession.
type NewSessionParams struct {
	// Cwd is the session's working directory. Must be an absolute path (ACP
	// requirement; enforced by the agent, not re-validated here).
	Cwd string
	// AdditionalDirectories are extra workspace roots. Nil/empty means none.
	AdditionalDirectories []string
	// McpServers are the MCP servers the agent should connect to for this
	// session. Nil is normalized to an empty (but present) list: ACP's
	// session/new request requires the field, never `null`.
	McpServers []protocol.McpServer
}

// LoadSessionParams are the caller-supplied parameters for Client.LoadSession.
type LoadSessionParams struct {
	SessionID             protocol.SessionID
	Cwd                   string
	AdditionalDirectories []string
	McpServers            []protocol.McpServer
}

// ResumeSessionParams are the caller-supplied parameters for
// Client.ResumeSession.
type ResumeSessionParams struct {
	SessionID             protocol.SessionID
	Cwd                   string
	AdditionalDirectories []string
	McpServers            []protocol.McpServer
}

// Session is one ACP session on a Client's connection: its id, its inbound
// session/update stream, and the one-prompt-in-flight gate (see prompt.go).
type Session struct {
	id     protocol.SessionID
	client *Client

	out chan Update

	mu      sync.Mutex
	cond    *sync.Cond
	queue   []queuedUpdate
	closed  bool
	aborted bool
	// updatesDone is closed exactly once by abortUpdates to interrupt a pump
	// blocked handing an update to an absent Updates reader. pumpDone closes
	// after pump has closed out, so forced session/client teardown can wait
	// for the goroutine to exit rather than leaving it behind.
	updatesDone chan struct{}
	pumpDone    chan struct{}
	closeOnce   sync.Once
	stopOnce    sync.Once
	// inFlight is 1 while pump has dequeued an update and is blocked handing
	// it to Updates() (an unbuffered channel with no guaranteed reader), 0
	// otherwise. deliver counts it as still "resident" toward
	// UpdateQueueDepth (see deliver's doc) so the bound is exact regardless
	// of whether pump has had a chance to run: without this, an update
	// pump happens to dequeue early would silently escape the cap
	// accounting (the queue slice alone would look under-full while an
	// extra item sat parked on pump's stack), making both the true resident
	// count and DroppedUpdates undercount by however many times pump won
	// that race.
	inFlight         int
	inFlightID       uint64
	nextDeliveryID   uint64
	completedThrough uint64
	completed        deliveryRange
	deliveryWaiters  []*deliveryWaiter
	droppedUpdates   atomic.Uint64

	seenMu    sync.Mutex
	seen      map[string]struct{}
	seenOrder []string

	promptSem chan struct{}

	// configMu guards configOptions and modes: this Session's locally cached
	// view of its session/new response's initial configOptions/modes, kept
	// current by SetConfigOption/SetMode's own responses (see their docs)
	// rather than by parsing the session/update notification stream, since a
	// connector needs a synchronous "what does the session look like right
	// now" read (ConfigOptions/Modes) that does not require it to also track
	// the update stream just to answer that question.
	configMu      sync.Mutex
	configOptions []protocol.SessionConfigOption
	modes         *protocol.SessionModeState
}

// ID returns the ACP session id this Session was created or loaded with.
func (s *Session) ID() protocol.SessionID { return s.id }

func cloneRawMessage(in json.RawMessage) json.RawMessage {
	if in == nil {
		return nil
	}
	out := make(json.RawMessage, len(in))
	copy(out, in)
	return out
}

func cloneStringPtr(in *string) *string {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneCategoryPtr(in *protocol.SessionConfigOptionCategory) *protocol.SessionConfigOptionCategory {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneConfigSelectOption(in protocol.SessionConfigSelectOption) protocol.SessionConfigSelectOption {
	out := in
	out.Description = cloneStringPtr(in.Description)
	out.Meta = cloneRawMessage(in.Meta)
	return out
}

func cloneConfigSelectOptions(in []protocol.SessionConfigSelectOption) []protocol.SessionConfigSelectOption {
	if in == nil {
		return nil
	}
	out := make([]protocol.SessionConfigSelectOption, len(in))
	for i, option := range in {
		out[i] = cloneConfigSelectOption(option)
	}
	return out
}

func cloneConfigSelectGroup(in protocol.SessionConfigSelectGroup) protocol.SessionConfigSelectGroup {
	out := in
	out.Options = cloneConfigSelectOptions(in.Options)
	out.Meta = cloneRawMessage(in.Meta)
	return out
}

func cloneConfigSelectGroups(in []protocol.SessionConfigSelectGroup) []protocol.SessionConfigSelectGroup {
	if in == nil {
		return nil
	}
	out := make([]protocol.SessionConfigSelectGroup, len(in))
	for i, group := range in {
		out[i] = cloneConfigSelectGroup(group)
	}
	return out
}

func cloneConfigSelect(in *protocol.SessionConfigSelect) *protocol.SessionConfigSelect {
	if in == nil {
		return nil
	}
	out := *in
	out.Options = protocol.SessionConfigSelectOptions{
		Ungrouped: cloneConfigSelectOptions(in.Options.Ungrouped),
		Grouped:   cloneConfigSelectGroups(in.Options.Grouped),
	}
	return &out
}

func cloneConfigBoolean(in *protocol.SessionConfigBoolean) *protocol.SessionConfigBoolean {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneConfigOption(in protocol.SessionConfigOption) protocol.SessionConfigOption {
	out := in
	out.Category = cloneCategoryPtr(in.Category)
	out.Description = cloneStringPtr(in.Description)
	out.Meta = cloneRawMessage(in.Meta)
	out.Select = cloneConfigSelect(in.Select)
	out.Boolean = cloneConfigBoolean(in.Boolean)
	return out
}

// copyConfigOptions returns a deep defensive copy of in: every pointer,
// RawMessage, variant, and nested option/group slice is cloned so a caller
// mutating the returned value (or Session mutating its own stored copy later)
// can never alias the other's memory. Nil in yields nil out, while a non-nil
// empty slice remains non-nil.
func copyConfigOptions(in []protocol.SessionConfigOption) []protocol.SessionConfigOption {
	if in == nil {
		return nil
	}
	out := make([]protocol.SessionConfigOption, len(in))
	for i, option := range in {
		out[i] = cloneConfigOption(option)
	}
	return out
}

func cloneSessionMode(in protocol.SessionMode) protocol.SessionMode {
	out := in
	out.Description = cloneStringPtr(in.Description)
	out.Meta = cloneRawMessage(in.Meta)
	return out
}

func cloneSessionModes(in []protocol.SessionMode) []protocol.SessionMode {
	if in == nil {
		return nil
	}
	out := make([]protocol.SessionMode, len(in))
	for i, mode := range in {
		out[i] = cloneSessionMode(mode)
	}
	return out
}

// copyModeState returns a deep defensive copy of in: a new
// *SessionModeState with cloned metadata, descriptions, and AvailableModes.
// Nil in yields nil out, while a non-nil empty AvailableModes slice remains
// non-nil.
func copyModeState(in *protocol.SessionModeState) *protocol.SessionModeState {
	if in == nil {
		return nil
	}
	out := *in
	out.AvailableModes = cloneSessionModes(in.AvailableModes)
	out.Meta = cloneRawMessage(in.Meta)
	return &out
}

// ConfigOptions returns a defensive copy of this Session's most recently
// known set of session configuration options: session/new, session/load, or
// session/resume's response initially, replaced wholesale by SetConfigOption's
// own response on every successful call (see SetConfigOption's doc — never a
// partial merge). Nil if the agent never advertised any. The returned slice is
// this Session's own copy: mutating it never affects the Session's internal
// state, and a later SetConfigOption response never mutates a slice a caller
// is still holding from an earlier call.
func (s *Session) ConfigOptions() []protocol.SessionConfigOption {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	return copyConfigOptions(s.configOptions)
}

// Modes returns a defensive copy of this Session's most recently known mode
// state: session/new, session/load, or session/resume's response initially,
// updated by SetMode on every successful call (see SetMode's doc). Nil if the
// agent never advertised mode state.
func (s *Session) Modes() *protocol.SessionModeState {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	return copyModeState(s.modes)
}

// setConfigState replaces the Session's cached initial configuration state
// with a defensive copy of an ACP session/new, session/load, or
// session/resume response. The same lock and copy policy is used for every
// lifecycle entry point so callers can safely read ConfigOptions/Modes while
// a session is being registered or restored.
func (s *Session) setConfigState(configOptions []protocol.SessionConfigOption, modes *protocol.SessionModeState) {
	s.configMu.Lock()
	s.configOptions = copyConfigOptions(configOptions)
	s.modes = copyModeState(modes)
	s.configMu.Unlock()
}

// newSession constructs a Session and starts its update-delivery pump.
func newSession(client *Client, id protocol.SessionID) *Session {
	s := &Session{
		id:          id,
		client:      client,
		out:         make(chan Update),
		queue:       make([]queuedUpdate, 0, sessionUpdateQueueHint),
		updatesDone: make(chan struct{}),
		pumpDone:    make(chan struct{}),
		seen:        make(map[string]struct{}),
		promptSem:   make(chan struct{}, 1),
	}
	s.cond = sync.NewCond(&s.mu)
	go s.pump()
	return s
}

// Updates returns the channel Session delivers session/update notifications
// on, typed and decoded. It is ready to receive from immediately: delivery
// begins the moment the Session is registered (at NewSession/LoadSession/
// ResumeSession time, before the caller could possibly have called Updates()
// yet), buffered internally by a queue (see deliver) so nothing arriving
// before the caller starts reading is dropped, up to UpdateQueueDepth. The
// channel is closed once the session is closed (Client.Close, connection
// death, or a future explicit session close) and every already-queued update
// has been delivered. Forced client/connection teardown may abandon an update
// still blocked for an absent reader so the pump can exit.
func (s *Session) Updates() <-chan Update { return s.out }

// DroppedUpdates reports how many queued session/update notifications have
// been dropped (oldest-first) for this Session because its internal queue
// exceeded UpdateQueueDepth — see deliver. This mirrors
// protocol.Conn.DroppedNotifications' shape (a diagnostic counter, never a
// silent failure with no way to observe it) and is distinct from
// Client.DroppedUpdates: the Client-level counter tracks updates that could
// not be routed to any session at all (unknown/unregistered sessionId),
// while this one tracks updates that WERE routed to this exact session but
// then evicted by its own queue bound because the consumer fell behind.
// Zero in the expected steady state of an actively-drained session.
func (s *Session) DroppedUpdates() uint64 { return s.droppedUpdates.Load() }

// WaitForUpdates waits for the protocol notification barrier and then until
// the Session's pump has handed off every update delivered before that
// barrier. It does not consume Updates; callers can wait here while another
// goroutine continues reading the channel. Cancellation and forced session or
// client teardown release the wait.
func (s *Session) WaitForUpdates(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.client == nil {
		return &NotDialedError{}
	}
	agent, err := s.client.currentAgent()
	if err != nil {
		return err
	}
	if err := agent.Conn().WaitForNotifications(ctx); err != nil {
		return wrapConnError(err)
	}
	return s.waitForPendingDeliveries(ctx)
}

func (s *Session) waitForPendingDeliveries(ctx context.Context) error {
	s.mu.Lock()
	if s.aborted {
		s.mu.Unlock()
		return &ClosedError{}
	}
	target := s.nextDeliveryID
	if s.completedThrough >= target {
		s.mu.Unlock()
		return nil
	}
	w := &deliveryWaiter{target: target, done: make(chan error, 1)}
	s.deliveryWaiters = append(s.deliveryWaiters, w)
	s.mu.Unlock()

	select {
	case err := <-w.done:
		return err
	case <-ctx.Done():
		s.mu.Lock()
		for i, candidate := range s.deliveryWaiters {
			if candidate == w {
				s.deliveryWaiters = append(s.deliveryWaiters[:i], s.deliveryWaiters[i+1:]...)
				break
			}
		}
		s.mu.Unlock()
		return ctx.Err()
	}
}

// WaitForUpdatesThrough waits for Conn's receive-order barrier at sequence
// and therefore proves every session/update notification through that point
// has completed the registered client handler and entered this Session's
// delivery queue. It does not consume Updates or create another reader.
func (s *Session) WaitForUpdatesThrough(ctx context.Context, sequence uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.client == nil {
		return &NotDialedError{}
	}
	agent, err := s.client.currentAgent()
	if err != nil {
		return err
	}
	if err := agent.Conn().WaitForNotificationsThrough(ctx, sequence); err != nil {
		return wrapConnError(err)
	}
	return nil
}

// deliver routes one decoded update into the session's queue, applying
// live-update dedup: a non-replay update whose _meta.eventId has already
// been seen for this session (within the retained EventDedupWindowDepth
// window — see recordSeenLocked) is dropped (never delivered twice), while
// a replay update (Meta.IsReplay) is exempt from dedup and always delivered
// — per the design doc, replay reconstruction and live streaming are
// tracked as separate concerns, and a replayed update's eventId
// legitimately reappearing (for example, if a client independently retains
// a replay's eventIds across a later live-observed duplicate is never
// expected in practice) must not be silently suppressed. An update with no
// eventId at all (Meta.EventID == "") is never deduplicated: there is
// nothing to key a highwater check on, so it is always delivered.
//
// The internal queue is bounded at UpdateQueueDepth (a growable slice
// guarded by mu, trimmed from the front on overflow), so a consumer that
// never calls Updates() (or falls far enough behind) cannot grow it without
// bound. This deliberately does NOT mirror protocol.Conn's own internal
// notifyQueue, which stays unbounded because it only ever holds already-
// dispatched handler jobs a single worker is actively draining as fast as
// each job completes (see Conn.enqueueNotifyJob's doc) — there is no
// equivalent "always being drained" guarantee here, since draining this
// queue is Updates(), and the whole point of this bound is that the caller
// might never call it. The shape instead mirrors Conn's OTHER bounded
// structure, notifyBuffers (see NotifyBufferDepth): a buffer that holds data
// for a reader who may not show up promptly, capped with the same
// drop-oldest-and-count discipline.
func (s *Session) deliver(u Update) {
	if !u.Meta.IsReplay && u.Meta.EventID != "" {
		s.seenMu.Lock()
		_, dup := s.seen[u.Meta.EventID]
		if !dup {
			s.recordSeenLocked(u.Meta.EventID)
		}
		s.seenMu.Unlock()
		if dup {
			return
		}
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.nextDeliveryID++
	s.queue = append(s.queue, queuedUpdate{update: u, id: s.nextDeliveryID})
	// Resident count includes the one update pump may already be holding
	// in flight (see inFlight's doc): that item is just as "not yet
	// delivered to the caller" as anything still in the slice, so it must
	// count against the same bound rather than escaping it for free.
	if drop := len(s.queue) + s.inFlight - UpdateQueueDepth; drop > 0 {
		if drop > len(s.queue) {
			// Can never actually happen at UpdateQueueDepth=512 (inFlight
			// is always 0 or 1), but guards the slice bound generically
			// rather than assuming today's constant forever.
			drop = len(s.queue)
		}
		// Zero the dropped entries' slots before advancing the slice's start
		// so their referenced content is released promptly rather than kept
		// alive by the backing array until a future reallocation.
		for i := 0; i < drop; i++ {
			s.completeDeliveryLocked(s.queue[i].id)
			s.queue[i] = queuedUpdate{}
		}
		s.queue = s.queue[drop:]
		// drop is bounded above by len(s.queue) (an in-memory slice length,
		// never anywhere near the uint64 boundary), so this conversion
		// cannot wrap.
		s.droppedUpdates.Add(uint64(drop))
	}
	s.mu.Unlock()
	s.cond.Signal()
}

// completeDeliveryLocked records that an update was either handed to the
// caller or intentionally discarded by the bounded queue. The bounded range
// handles the one legitimate out-of-order case: a queued FIFO prefix may be
// dropped while the older in-flight handoff remains blocked. All completions
// after that gap are consecutive because the pump is single-threaded and the
// queue drops only from its front.
func (s *Session) completeDeliveryLocked(id uint64) {
	if id == 0 || id <= s.completedThrough {
		return
	}
	if id == s.completedThrough+1 {
		s.completedThrough = id
		if s.completed.start == s.completedThrough+1 {
			s.completedThrough = s.completed.end
			s.completed = deliveryRange{}
		}
	} else if s.completed.start == 0 {
		s.completed = deliveryRange{start: id, end: id}
	} else if id == s.completed.end+1 {
		s.completed.end = id
	}
	s.notifyDeliveryWaitersLocked()
}

func (s *Session) notifyDeliveryWaitersLocked() {
	for i := 0; i < len(s.deliveryWaiters); {
		w := s.deliveryWaiters[i]
		if w.target > s.completedThrough {
			i++
			continue
		}
		w.done <- nil
		s.deliveryWaiters = append(s.deliveryWaiters[:i], s.deliveryWaiters[i+1:]...)
	}
}

// recordSeenLocked records eventID as seen for dedup and evicts the oldest
// recorded id (by insertion/delivery order, not by id value — see
// EventDedupWindowDepth's doc) once the retained window would otherwise
// exceed EventDedupWindowDepth. Caller must hold seenMu.
func (s *Session) recordSeenLocked(eventID string) {
	s.seen[eventID] = struct{}{}
	s.seenOrder = append(s.seenOrder, eventID)
	if drop := len(s.seenOrder) - EventDedupWindowDepth; drop > 0 {
		for _, old := range s.seenOrder[:drop] {
			delete(s.seen, old)
		}
		s.seenOrder = s.seenOrder[drop:]
	}
}

// pump drains the internal queue in order onto the exposed Updates()
// channel, blocking when the consumer is slower than delivery — deliver is
// what enforces UpdateQueueDepth on the producing side, by counting the one
// update pump may be sitting on (see inFlight). closeUpdates performs a
// graceful drain; abortUpdates uses the close-aware send to interrupt a
// blocked handoff when there is no reader, then closes out.
func (s *Session) pump() {
	defer close(s.pumpDone)
	defer close(s.out)

	for {
		s.mu.Lock()
		for len(s.queue) == 0 && !s.closed {
			s.cond.Wait()
		}
		if len(s.queue) == 0 {
			s.mu.Unlock()
			return
		}
		u := s.queue[0]
		s.queue[0] = queuedUpdate{}
		s.queue = s.queue[1:]
		s.inFlight = 1
		s.inFlightID = u.id
		s.mu.Unlock()

		select {
		case s.out <- u.update:
			s.mu.Lock()
			s.inFlight = 0
			s.inFlightID = 0
			s.completeDeliveryLocked(u.id)
			s.mu.Unlock()
		case <-s.updatesDone:
			s.mu.Lock()
			s.inFlight = 0
			s.inFlightID = 0
			for i := range s.queue {
				s.completeDeliveryLocked(s.queue[i].id)
				s.queue[i] = queuedUpdate{}
			}
			s.queue = nil
			s.mu.Unlock()
			return
		}
	}
}

// closeUpdates marks the session closed so pump drains queued updates instead
// of waiting for more. It intentionally returns without waiting: callers may
// begin consuming Updates after close is requested.
func (s *Session) closeUpdates() {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		s.cond.Broadcast()
	})
}

// abortUpdates marks the session closed, interrupts a pump blocked on an
// undrained Updates channel, and waits until pump has closed that channel.
// sync.Once makes concurrent Client.Close/connection-death cleanup paths safe
// while every caller still waits for the same pump completion.
func (s *Session) abortUpdates() {
	s.closeUpdates()
	s.mu.Lock()
	s.aborted = true
	for _, w := range s.deliveryWaiters {
		w.done <- &ClosedError{}
	}
	s.deliveryWaiters = nil
	s.mu.Unlock()
	s.stopOnce.Do(func() {
		close(s.updatesDone)
	})
	<-s.pumpDone
}

// registerSession constructs and tracks a new Session under id, so inbound
// session/update notifications for it (which may arrive concurrently with,
// or even before, the caller observes the *Session this returns — see
// LoadSession) are never dropped. Once terminal teardown has snapshotted the
// registry, it rejects and aborts the new Session instead of adding it to the
// replacement map. The closed check and insertion share sessionsMu; the
// terminal-state check is completed before taking it, and the abort happens
// after unlocking, so no session lifecycle path is nested under the registry
// lock.
func (c *Client) registerSession(id protocol.SessionID) (*Session, error) {
	s := newSession(c, id)
	c.mu.Lock()
	closed := c.state == dialClosed
	c.mu.Unlock()
	if closed {
		s.abortUpdates()
		return nil, &ClosedError{}
	}

	c.sessionsMu.Lock()
	if c.sessionsClosed {
		c.sessionsMu.Unlock()
		s.abortUpdates()
		return nil, &ClosedError{}
	}
	if _, exists := c.sessions[id]; exists {
		c.sessionsMu.Unlock()
		s.abortUpdates()
		return nil, &DuplicateSessionError{SessionID: id}
	}
	c.sessions[id] = s
	c.sessionsMu.Unlock()
	return s, nil
}

func (c *Client) unregisterSession(id protocol.SessionID, expected *Session) {
	c.sessionsMu.Lock()
	if current := c.sessions[id]; current == expected {
		delete(c.sessions, id)
	}
	c.sessionsMu.Unlock()
}

// closeAllSessions closes every currently-tracked Session's update stream,
// clears the registry, and marks it closed to reject late registrations.
// Called on Client.Close and on connection death (watchDeath): once the
// connection is gone, no more updates will ever arrive for any session, so
// every Updates() channel must close rather than hang forever.
func (c *Client) closeAllSessions() {
	c.sessionsMu.Lock()
	sessions := c.sessions
	c.sessions = make(map[protocol.SessionID]*Session)
	c.sessionsClosed = true
	c.sessionsMu.Unlock()

	for _, s := range sessions {
		s.abortUpdates()
	}
}

// normalizeMcpServers returns servers unchanged if non-nil, or an empty
// (non-nil) slice otherwise: NewSessionRequest/LoadSessionRequest's
// mcpServers field has no `omitempty` in the pinned schema (see
// protocol/types_gen.go), so a nil slice would marshal as JSON `null`
// where the schema requires an array.
func normalizeMcpServers(servers []protocol.McpServer) []protocol.McpServer {
	if servers != nil {
		return servers
	}
	return []protocol.McpServer{}
}

// NewSession calls the agent's session/new method and returns the resulting
// Session, registered so its update stream begins delivering immediately.
func (c *Client) NewSession(ctx context.Context, p NewSessionParams) (*Session, error) {
	agent, err := c.currentAgent()
	if err != nil {
		return nil, err
	}

	resp, err := agent.NewSession(ctx, protocol.NewSessionRequest{
		Cwd:                   p.Cwd,
		AdditionalDirectories: p.AdditionalDirectories,
		McpServers:            normalizeMcpServers(p.McpServers),
	})
	if err != nil {
		return nil, wrapConnError(err)
	}
	sess, err := c.registerSession(resp.SessionID)
	if err != nil {
		return nil, err
	}
	sess.setConfigState(resp.ConfigOptions, resp.Modes)
	return sess, nil
}

// errEmptySessionID reports that a caller-supplied SessionID was empty,
// caught before it ever reaches the wire or the session registry.
var errEmptySessionID = errors.New("acp/client: sessionId is required")

// LoadSession calls the agent's session/load method. The Session is
// registered under p.SessionID BEFORE the call is issued (unlike NewSession,
// the id is caller-supplied here, so this is possible and necessary): a
// foreign agent's session/load handler streams the session's full replay as
// session/update notifications before it ever returns its own response (see
// acp/agent/replay.go's handleSessionLoad), so the Session must already be
// listening the instant the call goes out, not only once it returns.
//
// The call itself is bounded by the Client's load timeout (LoadTimeout,
// overridable via Options.LoadTimeout): replay updates are consumed as they
// arrive regardless of how long the response itself takes, but a load that
// never resolves within the deadline fails with a typed *LoadTimeoutError
// rather than hanging forever or synthesizing a result.
func (c *Client) LoadSession(ctx context.Context, p LoadSessionParams) (*Session, error) {
	if p.SessionID == "" {
		return nil, errEmptySessionID
	}
	agent, err := c.currentAgent()
	if err != nil {
		return nil, err
	}

	sess, err := c.registerSession(p.SessionID)
	if err != nil {
		return nil, err
	}

	loadCtx, cancel := context.WithTimeout(ctx, c.loadTimeout())
	defer cancel()

	resp, err := agent.LoadSession(loadCtx, protocol.LoadSessionRequest{
		SessionID:             p.SessionID,
		Cwd:                   p.Cwd,
		AdditionalDirectories: p.AdditionalDirectories,
		McpServers:            normalizeMcpServers(p.McpServers),
	})
	if err != nil {
		c.unregisterSession(p.SessionID, sess)
		sess.abortUpdates()
		if loadCtx.Err() != nil && ctx.Err() == nil {
			// loadCtx's own deadline fired, not the caller's ctx: report the
			// bounded-wait failure as the typed timeout, not a raw
			// context.DeadlineExceeded from an internally-derived context the
			// caller never created.
			return nil, &LoadTimeoutError{SessionID: p.SessionID, Timeout: c.loadTimeout()}
		}
		return nil, wrapConnError(err)
	}
	sess.setConfigState(resp.ConfigOptions, resp.Modes)
	return sess, nil
}

// ResumeSession calls the agent's session/resume method. Like LoadSession,
// the Session is registered under the caller-supplied id before the call is
// issued, so any updates the agent sends while resuming are never dropped.
func (c *Client) ResumeSession(ctx context.Context, p ResumeSessionParams) (*Session, error) {
	if p.SessionID == "" {
		return nil, errEmptySessionID
	}
	agent, err := c.currentAgent()
	if err != nil {
		return nil, err
	}

	sess, err := c.registerSession(p.SessionID)
	if err != nil {
		return nil, err
	}

	resp, err := agent.ResumeSession(ctx, protocol.ResumeSessionRequest{
		SessionID:             p.SessionID,
		Cwd:                   p.Cwd,
		AdditionalDirectories: p.AdditionalDirectories,
		McpServers:            p.McpServers,
	})
	if err != nil {
		c.unregisterSession(p.SessionID, sess)
		sess.abortUpdates()
		return nil, wrapConnError(err)
	}
	sess.setConfigState(resp.ConfigOptions, resp.Modes)
	return sess, nil
}

// SetConfigOption calls the agent's session/set_config_option method,
// selecting valueID for configID (the single-value-selector variant of
// protocol.SetSessionConfigOptionRequest — the boolean variant is not
// reachable through this method, matching this method's own signature: a
// caller with a boolean option to flip has no valueID to pass in the first
// place). On success, this Session's cached ConfigOptions is replaced
// wholesale with the response's own full set (never a partial merge: the
// agent's response is authoritative, and the local cache before the call
// might already be stale by the time it resolves).
func (s *Session) SetConfigOption(ctx context.Context, configID protocol.SessionConfigID, valueID protocol.SessionConfigValueID) error {
	agent, err := s.client.currentAgent()
	if err != nil {
		return err
	}

	resp, err := agent.SetConfigOption(ctx, protocol.SetSessionConfigOptionRequest{
		SessionID: s.id,
		ConfigID:  configID,
		ValueID:   &valueID,
	})
	if err != nil {
		return wrapConnError(err)
	}

	s.configMu.Lock()
	s.configOptions = copyConfigOptions(resp.ConfigOptions)
	s.configMu.Unlock()
	return nil
}

// SetMode calls the agent's session/set_mode method. Unlike
// SetConfigOption, session/set_mode's own response carries no state at all
// (see protocol.SetSessionModeResponse: only _meta), so on success this
// Session's cached mode state is updated locally instead: CurrentModeID is
// replaced with the id the caller just requested — the call succeeding is
// the only confirmation the wire gives — leaving AvailableModes as most
// recently known. If this Session has no cached mode state yet (because the
// agent omitted Modes from session/new, session/load, or session/resume), a
// minimal SessionModeState carrying only the new CurrentModeID is recorded
// rather than silently discarding the confirmed change.
func (s *Session) SetMode(ctx context.Context, modeID protocol.SessionModeID) error {
	agent, err := s.client.currentAgent()
	if err != nil {
		return err
	}

	if _, err := agent.SetMode(ctx, protocol.SetSessionModeRequest{
		SessionID: s.id,
		ModeID:    modeID,
	}); err != nil {
		return wrapConnError(err)
	}

	s.configMu.Lock()
	if s.modes == nil {
		s.modes = &protocol.SessionModeState{CurrentModeID: modeID}
	} else {
		updated := *s.modes
		updated.CurrentModeID = modeID
		s.modes = &updated
	}
	s.configMu.Unlock()
	return nil
}

// methodSessionSetModel is the wire method name for the non-standard,
// unstable "session/set_model" extension some ACP adapters implement (for
// example claude-agent-acp). It has no entry in protocol/methods_gen.go —
// the pinned v1 ACP schema this module generates from does not define it —
// so SetModel calls it through AgentConn.Conn().Call, the same generic path
// AgentConn.Conn's own doc names for "extension traffic," rather than
// inventing a typed protocol.AgentConn method for a method the pinned
// schema does not recognize. This is the only method name SetModel will
// ever call: nothing in this package accepts a caller-supplied method
// string, so this is not a generic arbitrary-JSON-RPC-call escape hatch.
const methodSessionSetModel = "session/set_model"

// setModelRequest is session/set_model's request shape. Field names/casing
// mirror the sibling generated SetSessionModeRequest (sessionId plus one
// target-value field) — this codebase's existing convention for "set X for
// a session" ACP methods — since no published schema exists for this
// extension to conform to instead. Unexported: callers only ever reach it
// through SetModel's (context, capability, modelID) signature.
type setModelRequest struct {
	SessionID protocol.SessionID `json:"sessionId"`
	ModelID   string             `json:"modelId"`
}

// setModelResponse is session/set_model's response shape. Deliberately
// empty: this is an unstable, non-standard extension with no pinned
// response schema, and SetModel promises callers nothing about
// adapter-specific response data.
type setModelResponse struct{}

// SetModel calls the non-standard "session/set_model" extension some ACP
// adapters implement, gated behind proof (a SetModelCapability) that this
// Session's Client actually observed the extension advertised in its
// "initialize" response (see Client.ProveSetModelCapability). Without a
// granted proof, SetModel fails closed with *SetModelUnsupportedError
// before ever reaching the wire: this package never speculatively probes an
// ACP peer for an undeclared method, because some adapters answer an
// unrecognized method with a bare `{}` success rather than a JSON-RPC
// error, which would make such a probe silently misread as support.
func (s *Session) SetModel(ctx context.Context, proof SetModelCapability, modelID string) error {
	if !proof.granted {
		return &SetModelUnsupportedError{}
	}

	agent, err := s.client.currentAgent()
	if err != nil {
		return err
	}

	var resp setModelResponse
	if err := agent.Conn().Call(ctx, methodSessionSetModel, setModelRequest{
		SessionID: s.id,
		ModelID:   modelID,
	}, &resp); err != nil {
		return wrapConnError(err)
	}
	return nil
}
