package sessionruntime

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/foreign"
	"github.com/looprig/harness/pkg/tool"
)

// The values below mirror mcp/pkg/collab. They deliberately live in this
// module rather than importing MCP: Harness owns the session authority and
// the two packages only share this small, frozen wire contract.
const (
	collabCapabilityBytes   = 32
	collabMaxMessageBytes   = 192 << 10
	collabMaxArgumentBytes  = 256 << 10
	collabMaxFrameBytes     = collabMaxArgumentBytes
	collabMaxEndpointBytes  = 4096
	collabMaxTimeoutSeconds = 24 * 60 * 60

	collabAdmissionTimeout = 5 * time.Second
	collabIOTimeout        = 5 * time.Second
	collabMaxConcurrent    = 32
	collabMaxPerCapability = 8
	collabRateWindow       = time.Second
	collabRateLimit        = 64
	collabSocketName       = "broker.sock"
	collabMaxUint32        = uint64(^uint32(0))
)

var (
	errCollabBrokerUnsupportedPlatform = errors.New("collaboration broker unsupported on this platform")
	errCollabBrokerStaleEndpoint       = errors.New("collaboration broker endpoint already exists")
	errCollabBrokerClosed              = errors.New("collaboration broker is closed")
	errCollabBrokerAuthentication      = errors.New("collaboration broker authentication failed")
	errCollabBrokerInvalidRequest      = errors.New("collaboration broker request rejected")
	errCollabBrokerLimit               = errors.New("collaboration broker limit exceeded")
	errCollabBrokerProtocol            = errors.New("collaboration broker protocol failure")
)

func (s *Session) ensureCollabBroker() (*collabBroker, error) {
	if s == nil {
		return nil, errCollabBrokerClosed
	}
	s.collabBrokerMu.Lock()
	defer s.collabBrokerMu.Unlock()
	if s.collabBroker != nil {
		return s.collabBroker, nil
	}
	broker, err := newCollabBroker(s)
	if err != nil {
		return nil, err
	}
	s.collabBroker = broker
	return broker, nil
}

// startCollabBroker is the lifecycle hook used by session construction and
// restore composition. Keeping startup outside the foreign builder preserves
// legacy zero-services behavior while making the attach point explicit.
func (s *Session) startCollabBroker() error {
	_, err := s.ensureCollabBroker()
	return err
}

func (s *Session) revokeCollabLoop(loopID uuid.UUID) {
	if s == nil || loopID.IsZero() {
		return
	}
	s.collabBrokerMu.Lock()
	broker := s.collabBroker
	s.collabBrokerMu.Unlock()
	if broker != nil {
		_ = broker.Revoke(loopID)
	}
}

func (s *Session) closeCollabBroker(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.collabBrokerMu.Lock()
	broker := s.collabBroker
	s.collabBroker = nil
	s.collabBrokerMu.Unlock()
	if broker == nil {
		return nil
	}
	return broker.Close(ctx)
}

func (s *Session) closeCollabBrokerWithTimeout(root context.Context, timeout time.Duration) error {
	if s == nil {
		return nil
	}
	ctx, cancel := cleanupContext(root, timeout)
	defer cancel()
	if err := s.closeCollabBroker(ctx); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return cleanupTimeoutError(ShutdownCleanupCollabBroker, timeout, err)
		}
		return err
	}
	return nil
}

func collabNonNegativeUint64(value int) (uint64, bool) {
	if value < 0 {
		return 0, false
	}
	converted := uint64(value) // #nosec G115 -- value is nonnegative after the guard.
	return converted, true
}

func collabUint32FromNonNegativeInt(value int) (uint32, bool) {
	converted, ok := collabNonNegativeUint64(value)
	if !ok || converted > collabMaxUint32 {
		return 0, false
	}
	narrowed := uint32(converted) // #nosec G115 -- converted is bounded by uint32's maximum above.
	return narrowed, true
}

func collabFrameLimit(max int) (uint64, bool) {
	limit, ok := collabNonNegativeUint64(max)
	if !ok {
		return 0, false
	}
	if limit > collabMaxUint32 {
		limit = collabMaxUint32
	}
	return limit, true
}

type collabBroker struct {
	endpoint string
	dir      string
	listener net.Listener
	ctx      context.Context
	cancel   context.CancelFunc

	mu              sync.RWMutex
	closed          bool
	principals      map[[32]byte]*collabPrincipal
	byLoop          map[uuid.UUID]*collabPrincipal
	connections     map[net.Conn]*collabPrincipal
	connectionSlots chan struct{}
	globalCalls     chan struct{}
	acceptDone      chan struct{}
	handlersDone    chan struct{}
	watchDone       chan struct{}
	handlerCount    int
	readDeadline    func(net.Conn, time.Time) error
	peerUID         func(net.Conn) (uint32, bool)
	closeOnce       sync.Once
}

type collabPrincipal struct {
	broker     *collabBroker
	loopID     uuid.UUID
	digest     [collabCapabilityBytes]byte
	controller tool.DelegateController

	mu          sync.Mutex
	revoked     bool
	concurrent  chan struct{}
	callCancels map[net.Conn]context.CancelFunc
	connections map[net.Conn]struct{}
	rateStart   time.Time
	rateCount   int
}

type collabWireRequest struct {
	AgentID         *string `json:"agent_id"`
	Message         *string `json:"message"`
	WaitForResponse *bool   `json:"wait_for_response"`
	TimeoutSeconds  *int    `json:"timeout_seconds"`
}

type collabWireResult struct {
	AgentID        string `json:"agent_id"`
	Name           string `json:"name"`
	State          string `json:"state"`
	DeliveryStatus string `json:"delivery_status,omitempty"`
	ResponseStatus string `json:"response_status,omitempty"`
	Response       string `json:"response,omitempty"`
}

// newCollabBroker starts one private broker endpoint for a session. The
// optional root argument exists only for deterministic tests; production uses
// a fresh owner-only temporary directory.
func newCollabBroker(session *Session) (*collabBroker, error) {
	return newCollabBrokerAt(session, "", collabSocketName)
}

func newCollabBrokerAt(session *Session, root, socketName string) (*collabBroker, error) {
	if !collabPlatformSupported() {
		return nil, errCollabBrokerUnsupportedPlatform
	}
	if socketName == "" {
		socketName = collabSocketName
	}
	if root != "" && !filepath.IsAbs(root) {
		return nil, errCollabBrokerProtocol
	}
	if filepath.Base(socketName) != socketName || socketName == "." || socketName == ".." {
		return nil, errCollabBrokerStaleEndpoint
	}

	createdRoot := false
	if root == "" {
		var err error
		root, err = os.MkdirTemp("", "carbon-collab-session-")
		if err != nil {
			return nil, errCollabBrokerProtocol
		}
		createdRoot = true
	} else {
		info, err := os.Lstat(root)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(root, 0o700); err != nil {
				return nil, errCollabBrokerProtocol
			}
		} else if err != nil || !info.IsDir() {
			return nil, errCollabBrokerProtocol
		} else if info.Mode()&os.ModeSymlink != 0 {
			return nil, errCollabBrokerProtocol
		}
	}
	if err := os.Chmod(root, 0o700); err != nil { // #nosec G302 -- root is a private directory; 0700 is required for traversal and socket creation.
		if createdRoot {
			_ = os.RemoveAll(root)
		}
		return nil, errCollabBrokerProtocol
	}
	endpoint := filepath.Join(root, socketName)
	if len(endpoint) > collabMaxEndpointBytes {
		if createdRoot {
			_ = os.RemoveAll(root)
		}
		return nil, errCollabBrokerProtocol
	}
	if _, err := os.Lstat(endpoint); err == nil {
		if createdRoot {
			_ = os.RemoveAll(root)
		}
		return nil, errCollabBrokerStaleEndpoint
	} else if !errors.Is(err, os.ErrNotExist) {
		if createdRoot {
			_ = os.RemoveAll(root)
		}
		return nil, errCollabBrokerProtocol
	}
	listener, err := listenCollabEndpoint(endpoint)
	if err != nil {
		if createdRoot {
			_ = os.RemoveAll(root)
		}
		return nil, errCollabBrokerProtocol
	}
	if err := os.Chmod(endpoint, 0o600); err != nil {
		_ = listener.Close()
		_ = os.Remove(endpoint)
		if createdRoot {
			_ = os.RemoveAll(root)
		}
		return nil, errCollabBrokerProtocol
	}
	ctx := context.Background()
	if session != nil && session.sessionCtx != nil {
		ctx = session.sessionCtx
	}
	brokerCtx, cancel := context.WithCancel(ctx)
	handlersDone := make(chan struct{})
	close(handlersDone)
	b := &collabBroker{
		endpoint:        endpoint,
		dir:             root,
		listener:        listener,
		ctx:             brokerCtx,
		cancel:          cancel,
		principals:      make(map[[32]byte]*collabPrincipal),
		byLoop:          make(map[uuid.UUID]*collabPrincipal),
		connections:     make(map[net.Conn]*collabPrincipal),
		connectionSlots: make(chan struct{}, collabMaxConcurrent*2),
		globalCalls:     make(chan struct{}, collabMaxConcurrent),
		acceptDone:      make(chan struct{}),
		handlersDone:    handlersDone,
		watchDone:       make(chan struct{}),
	}
	go b.acceptLoop()
	go b.watchContext(brokerCtx)
	return b, nil
}

// watchContext observes the broker's owning session context. Cancellation
// revokes authority and stops admission immediately, but it deliberately does
// not join handlers: an external controller may ignore cancellation forever,
// and no broker-owned lifecycle goroutine may wait on it.
func (b *collabBroker) watchContext(ctx context.Context) {
	if b == nil {
		return
	}
	if b.watchDone != nil {
		defer close(b.watchDone)
	}
	if ctx == nil {
		return
	}
	<-ctx.Done()
	b.stop()
}

func (b *collabBroker) Endpoint() string {
	if b == nil {
		return ""
	}
	return b.endpoint
}

// Mint creates a capability for exactly one origin loop. The broker retains
// only the SHA-256 digest; the raw bearer is returned once in the opaque
// descriptor that the foreign builder receives.
func (b *collabBroker) Mint(loopID uuid.UUID, controller tool.DelegateController) (foreign.BrokerDescriptor, error) {
	if b == nil || loopID.IsZero() {
		return foreign.BrokerDescriptor{}, errCollabBrokerClosed
	}
	token := make([]byte, collabCapabilityBytes)
	if _, err := rand.Read(token); err != nil {
		return foreign.BrokerDescriptor{}, errCollabBrokerProtocol
	}
	digest := sha256.Sum256(token)
	p := &collabPrincipal{
		broker:      b,
		loopID:      loopID,
		digest:      digest,
		controller:  controller,
		concurrent:  make(chan struct{}, collabMaxPerCapability),
		callCancels: make(map[net.Conn]context.CancelFunc),
		connections: make(map[net.Conn]struct{}),
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return foreign.BrokerDescriptor{}, errCollabBrokerClosed
	}
	if b.byLoop == nil {
		b.byLoop = make(map[uuid.UUID]*collabPrincipal)
	}
	if b.principals == nil {
		b.principals = make(map[[32]byte]*collabPrincipal)
	}
	if _, exists := b.byLoop[loopID]; exists {
		return foreign.BrokerDescriptor{}, errCollabBrokerLimit
	}
	b.byLoop[loopID] = p
	b.principals[digest] = p
	return foreign.NewBrokerDescriptor(b.endpoint, token), nil
}

func (b *collabBroker) HasRawCapability(token []byte) bool {
	if b == nil || len(token) != collabCapabilityBytes {
		return false
	}
	digest := sha256.Sum256(token)
	return b.lookupDigest(digest) != nil
}

func (b *collabBroker) digestForTest(token []byte) [collabCapabilityBytes]byte {
	return sha256.Sum256(token)
}

func (b *collabBroker) Revoke(loopID uuid.UUID) error {
	if b == nil || loopID.IsZero() {
		return errCollabBrokerClosed
	}
	b.mu.RLock()
	p := b.byLoop[loopID]
	b.mu.RUnlock()
	if p == nil {
		return nil
	}
	p.revoke()
	return nil
}

func (b *collabBroker) acceptLoop() {
	defer close(b.acceptDone)
	for {
		conn, err := b.listener.Accept()
		if err != nil {
			b.mu.RLock()
			closed := b.closed
			b.mu.RUnlock()
			if closed || errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		b.mu.Lock()
		if b.closed {
			b.mu.Unlock()
			_ = conn.Close()
			continue
		}
		if b.connectionSlots != nil {
			select {
			case b.connectionSlots <- struct{}{}:
			default:
				b.mu.Unlock()
				_ = conn.Close()
				continue
			}
		}
		if b.handlerCount == 0 {
			b.handlersDone = make(chan struct{})
		}
		b.handlerCount++
		b.mu.Unlock()
		go func() {
			defer b.finishHandler()
			if b.connectionSlots != nil {
				defer func() { <-b.connectionSlots }()
			}
			// Connection-level errors are intentionally contained here. handle
			// closes the connection and returns only fixed protocol/auth outcomes;
			// the accept loop has no caller-visible error channel.
			if err := b.handle(conn); err != nil {
				return
			}
		}()
	}
}

func (b *collabBroker) finishHandler() {
	b.mu.Lock()
	if b.handlerCount > 0 {
		b.handlerCount--
		if b.handlerCount == 0 && b.handlersDone != nil {
			close(b.handlersDone)
		}
	}
	b.mu.Unlock()
}

func (b *collabBroker) handle(conn net.Conn) error {
	if b == nil || conn == nil {
		return errCollabBrokerProtocol
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		_ = conn.Close()
		return errCollabBrokerClosed
	}
	if b.connections == nil {
		b.connections = make(map[net.Conn]*collabPrincipal)
	}
	b.connections[conn] = nil
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.connections, conn)
		b.mu.Unlock()
		_ = conn.Close()
	}()

	if err := b.setAdmissionReadDeadline(conn, time.Now().Add(collabAdmissionTimeout)); err != nil {
		return errCollabBrokerProtocol
	}
	peerUID, peerOK := b.peerCredentials(conn)
	if peerOK && peerUID != collabUID() {
		return errCollabBrokerAuthentication
	}
	if b.peerCredentialsRequired() && !peerOK {
		return errCollabBrokerAuthentication
	}
	token, err := readCollabHandshake(conn)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(token)
	p := b.lookupDigest(digest)
	if p == nil {
		return errCollabBrokerAuthentication
	}
	p.attachConnection(conn)
	defer p.detachConnection(conn)
	b.mu.Lock()
	b.connections[conn] = p
	b.mu.Unlock()
	payload, err := readCollabFrame(conn, collabMaxArgumentBytes)
	if err != nil {
		return err
	}
	req, err := decodeCollabRequest(payload)
	if err != nil {
		return err
	}
	callCtx, release, ok := b.beginCall(p, conn)
	if !ok {
		return errCollabBrokerLimit
	}
	defer release()
	// The admission deadline ends after the request frame. MessageAgent's
	// response timeout is owned by the scoped controller and is intentionally
	// independent from this broker's I/O budget.
	if err := b.setAdmissionReadDeadline(conn, time.Time{}); err != nil {
		return errCollabBrokerProtocol
	}
	if b.isClosed() {
		return errCollabBrokerClosed
	}
	result, err := p.controller.Execute(callCtx, tool.DelegateRequest{
		Operation:       tool.DelegateSend,
		AgentID:         req.agentID,
		Message:         req.message,
		WaitForResponse: req.waitForResponse,
		TimeoutSeconds:  req.timeoutSeconds,
	})
	if err != nil {
		return errCollabBrokerProtocol
	}
	if b.isClosed() {
		return errCollabBrokerClosed
	}
	encoded, err := encodeCollabResult(result)
	if err != nil {
		return errCollabBrokerProtocol
	}
	if err := conn.SetWriteDeadline(time.Now().Add(collabIOTimeout)); err != nil {
		return errCollabBrokerProtocol
	}
	if b.isClosed() {
		return errCollabBrokerClosed
	}
	if err := writeCollabFrame(conn, encoded, collabMaxFrameBytes); err != nil {
		return errCollabBrokerProtocol
	}
	return nil
}

func (b *collabBroker) setAdmissionReadDeadline(conn net.Conn, deadline time.Time) error {
	if b != nil && b.readDeadline != nil {
		return b.readDeadline(conn, deadline)
	}
	return conn.SetReadDeadline(deadline)
}

func (b *collabBroker) peerCredentials(conn net.Conn) (uint32, bool) {
	if b != nil && b.peerUID != nil {
		return b.peerUID(conn)
	}
	if !collabPeerUIDSupported() {
		return 0, false
	}
	return collabPeerUID(conn)
}

func (b *collabBroker) peerCredentialsRequired() bool {
	return b == nil || b.peerUID != nil || collabPeerUIDSupported()
}

func (b *collabBroker) isClosed() bool {
	if b == nil {
		return true
	}
	b.mu.RLock()
	closed := b.closed
	b.mu.RUnlock()
	return closed
}

type decodedCollabRequest struct {
	agentID         uuid.UUID
	message         string
	waitForResponse bool
	timeoutSeconds  *int
}

func decodeCollabRequest(raw []byte) (decodedCollabRequest, error) {
	if len(raw) == 0 || len(raw) > collabMaxArgumentBytes || !utf8.Valid(raw) {
		return decodedCollabRequest{}, errCollabBrokerInvalidRequest
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed[0] != '{' {
		return decodedCollabRequest{}, errCollabBrokerInvalidRequest
	}
	var wire collabWireRequest
	decoder := json.NewDecoder(strings.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return decodedCollabRequest{}, errCollabBrokerInvalidRequest
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return decodedCollabRequest{}, errCollabBrokerInvalidRequest
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &fields); err != nil || fields == nil {
		return decodedCollabRequest{}, errCollabBrokerInvalidRequest
	}
	if wire.AgentID == nil || wire.Message == nil {
		return decodedCollabRequest{}, errCollabBrokerInvalidRequest
	}
	if wire.WaitForResponse == nil {
		if _, present := fields["wait_for_response"]; present {
			return decodedCollabRequest{}, errCollabBrokerInvalidRequest
		}
	}
	if wire.TimeoutSeconds == nil {
		if _, present := fields["timeout_seconds"]; present {
			return decodedCollabRequest{}, errCollabBrokerInvalidRequest
		}
	}
	if len(*wire.AgentID) > 36 || !utf8.ValidString(*wire.AgentID) {
		return decodedCollabRequest{}, errCollabBrokerInvalidRequest
	}
	agentID, err := uuid.Parse(*wire.AgentID)
	if err != nil || agentID.IsZero() {
		return decodedCollabRequest{}, errCollabBrokerInvalidRequest
	}
	if !utf8.ValidString(*wire.Message) || strings.TrimSpace(*wire.Message) == "" {
		return decodedCollabRequest{}, errCollabBrokerInvalidRequest
	}
	if len(*wire.Message) > collabMaxMessageBytes {
		return decodedCollabRequest{}, errCollabBrokerInvalidRequest
	}
	if wire.TimeoutSeconds != nil && (*wire.TimeoutSeconds < 0 || *wire.TimeoutSeconds > collabMaxTimeoutSeconds) {
		return decodedCollabRequest{}, errCollabBrokerInvalidRequest
	}
	wait := true
	if wire.WaitForResponse != nil {
		wait = *wire.WaitForResponse
	}
	var timeout *int
	if wire.TimeoutSeconds != nil {
		value := *wire.TimeoutSeconds
		timeout = &value
	}
	return decodedCollabRequest{agentID: agentID, message: *wire.Message, waitForResponse: wait, timeoutSeconds: timeout}, nil
}

func encodeCollabResult(result tool.DelegateResult) ([]byte, error) {
	if result.AgentID.IsZero() || result.Name == "" || result.State == "" {
		return nil, errCollabBrokerProtocol
	}
	wire := collabWireResult{
		AgentID:        result.AgentID.String(),
		Name:           result.Name,
		State:          string(result.State),
		DeliveryStatus: string(result.DeliveryStatus),
		ResponseStatus: collabResponseStatus(result.ResponseStatus),
		Response:       result.Response,
	}
	encoded, err := json.Marshal(wire)
	if err != nil || len(encoded) > collabMaxFrameBytes || !utf8.Valid(encoded) {
		return nil, errCollabBrokerProtocol
	}
	return encoded, nil
}

func collabResponseStatus(status tool.DelegateResponseStatus) string {
	switch status {
	case tool.DelegateResponseCompleted:
		return "completed"
	case tool.DelegateResponseInterrupted:
		return "interrupted"
	case tool.DelegateResponseFailed:
		return "failed"
	case tool.DelegateResponseTimedOut:
		return "timed_out"
	default:
		return ""
	}
}

func (b *collabBroker) lookupDigest(digest [collabCapabilityBytes]byte) *collabPrincipal {
	if b == nil {
		return nil
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	var found *collabPrincipal
	for candidateDigest, candidate := range b.principals {
		if candidate != nil && subtle.ConstantTimeCompare(candidateDigest[:], digest[:]) == 1 {
			candidate.mu.Lock()
			revoked := candidate.revoked
			candidate.mu.Unlock()
			if !revoked {
				found = candidate
			}
		}
	}
	return found
}

func (p *collabPrincipal) attachConnection(conn net.Conn) {
	p.mu.Lock()
	if p.connections == nil {
		p.connections = make(map[net.Conn]struct{})
	}
	if !p.revoked {
		p.connections[conn] = struct{}{}
	}
	p.mu.Unlock()
}

func (p *collabPrincipal) detachConnection(conn net.Conn) {
	p.mu.Lock()
	delete(p.connections, conn)
	delete(p.callCancels, conn)
	p.mu.Unlock()
}

func (b *collabBroker) beginCall(p *collabPrincipal, conn net.Conn) (context.Context, func(), bool) {
	if p == nil || p.controller == nil {
		return nil, nil, false
	}
	select {
	case b.globalCalls <- struct{}{}:
	default:
		return nil, nil, false
	}
	select {
	case p.concurrent <- struct{}{}:
	default:
		<-b.globalCalls
		return nil, nil, false
	}
	p.mu.Lock()
	if p.revoked {
		p.mu.Unlock()
		<-p.concurrent
		<-b.globalCalls
		return nil, nil, false
	}
	now := time.Now()
	if p.rateStart.IsZero() || now.Sub(p.rateStart) >= collabRateWindow {
		p.rateStart = now
		p.rateCount = 0
	}
	if p.rateCount >= collabRateLimit {
		p.mu.Unlock()
		<-p.concurrent
		<-b.globalCalls
		return nil, nil, false
	}
	p.rateCount++
	base := b.ctx
	if base == nil {
		base = context.Background()
	}
	callCtx, cancel := context.WithCancel(base)
	if p.callCancels == nil {
		p.callCancels = make(map[net.Conn]context.CancelFunc)
	}
	p.callCancels[conn] = cancel
	p.mu.Unlock()
	release := func() {
		cancel()
		p.mu.Lock()
		delete(p.callCancels, conn)
		p.mu.Unlock()
		<-p.concurrent
		<-b.globalCalls
	}
	return callCtx, release, true
}

func (p *collabPrincipal) revoke() {
	p.mu.Lock()
	if p.revoked {
		p.mu.Unlock()
		return
	}
	p.revoked = true
	cancels := make([]context.CancelFunc, 0, len(p.callCancels))
	for _, cancel := range p.callCancels {
		cancels = append(cancels, cancel)
	}
	connections := make([]net.Conn, 0, len(p.connections))
	for conn := range p.connections {
		connections = append(connections, conn)
	}
	p.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	for _, conn := range connections {
		_ = conn.Close()
	}
}

// stop revokes every origin and closes the endpoint without waiting for
// handlers. It is the only closeOnce owner, so a context watcher and an
// explicit Close can race without double-closing lifecycle channels.
func (b *collabBroker) stop() {
	if b == nil {
		return
	}
	b.closeOnce.Do(func() {
		b.mu.Lock()
		b.closed = true
		principals := make([]*collabPrincipal, 0, len(b.principals))
		for _, p := range b.principals {
			principals = append(principals, p)
		}
		listener := b.listener
		connections := make([]net.Conn, 0, len(b.connections))
		for conn := range b.connections {
			connections = append(connections, conn)
		}
		b.mu.Unlock()
		if b.cancel != nil {
			b.cancel()
		}
		if listener != nil {
			_ = listener.Close()
		}
		for _, p := range principals {
			p.revoke()
		}
		for _, conn := range connections {
			_ = conn.Close()
		}
		_ = os.Remove(b.endpoint)
		_ = os.Remove(b.dir)
	})
}

// Close performs the nonblocking stop phase, then waits for broker-owned
// admission/handler goroutines while the caller's context remains live. A
// non-cooperative controller can outlive a timed Close; the authority and
// endpoint are still removed before that deadline.
func (b *collabBroker) Close(ctx context.Context) error {
	if b == nil {
		return nil
	}
	b.stop()
	if ctx == nil {
		ctx = context.Background()
	}
	if b.acceptDone == nil {
		return nil
	}
	select {
	case <-b.acceptDone:
	case <-ctx.Done():
		return ctx.Err()
	}
	b.mu.RLock()
	handlersDone := b.handlersDone
	b.mu.RUnlock()
	if handlersDone == nil {
		return nil
	}
	select {
	case <-handlersDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func writeCollabFrame(w io.Writer, payload []byte, max int) error {
	if w == nil || len(payload) == 0 {
		return errCollabBrokerProtocol
	}
	limit, ok := collabFrameLimit(max)
	if !ok || uint64(len(payload)) > limit {
		return errCollabBrokerLimit
	}
	payloadLength, ok := collabUint32FromNonNegativeInt(len(payload))
	if !ok {
		return errCollabBrokerLimit
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], payloadLength)
	if err := writeCollabFull(w, header[:]); err != nil {
		return errCollabBrokerProtocol
	}
	if err := writeCollabFull(w, payload); err != nil {
		return errCollabBrokerProtocol
	}
	return nil
}

func writeCollabFull(w io.Writer, payload []byte) error {
	for len(payload) > 0 {
		n, err := w.Write(payload)
		if n < 0 || n > len(payload) || n == 0 {
			return io.ErrShortWrite
		}
		payload = payload[n:]
		if err != nil {
			return err
		}
	}
	return nil
}

func readCollabFrame(r io.Reader, max int) ([]byte, error) {
	if r == nil {
		return nil, errCollabBrokerProtocol
	}
	limit, ok := collabFrameLimit(max)
	if !ok {
		return nil, errCollabBrokerLimit
	}
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, errCollabBrokerProtocol
	}
	length := binary.BigEndian.Uint32(header[:])
	if length == 0 || uint64(length) > limit {
		return nil, errCollabBrokerLimit
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, errCollabBrokerProtocol
	}
	return payload, nil
}

func readCollabHandshake(r io.Reader) ([]byte, error) {
	payload, err := readCollabFrame(r, collabCapabilityBytes)
	if err != nil || len(payload) != collabCapabilityBytes {
		return nil, errCollabBrokerAuthentication
	}
	return payload, nil
}
