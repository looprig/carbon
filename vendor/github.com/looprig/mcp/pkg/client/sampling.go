// This file wires Handlers.Sampling to the connection: it is the producer that
// turns a server's sampling/createMessage into a call on the application's
// handler, and the handler's completion back into a result the server sees.
//
// It is the second place in this package where foreign code runs while a server
// waits (see elicit.go for the first), and everything that file says about that
// applies here: the call is bounded by a timeout, the answer is validated before
// it can reach the wire, and every failure is a refusal the server is told about
// rather than a silence it waits out. The errors here travel *to the server*, so
// each carries an explicit message and never renders a handler's own error text.
//
// What makes sampling different from elicitation is what it costs. An
// elicitation spends a person's attention; a sampling request spends the host's
// model budget, and does it at whatever rate a server chooses to ask. So this
// file is the only one in the package that refuses work purely because there is
// too much of it: the gate below caps how many sampling requests may run at once
// and how deep a chain of them may go, and it caps them before the handler is
// called, because a refusal that happens after the model has run has not saved
// anything.
//
// # What a handler is given, and what it structurally cannot be given
//
// A SamplingHandler receives one SampleRequest — a binding name, a system
// prompt, text messages, and a token budget — and returns one SampleResult. That
// is the whole interface. It is not passed a Client, a Conn, a scheduler, or a
// tool registry, and there is no field on either type that could carry one: the
// design's "sampling never receives a Harness Session controller or unrestricted
// tool registry" is enforced by there being nowhere to put one, rather than by a
// rule someone has to remember. internal/protocol holds up the other end (it
// registers the SDK's tool-free CreateMessageHandler and never advertises the
// Tools sub-capability), so a tool cannot enter a sampling request from the
// server's side either.

package client

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/looprig/mcp/internal/protocol"
)

// opSample names the operation carried by this file's errors.
const opSample = "sample"

// SampleOutcome is how a sampling request ended.
type SampleOutcome uint8

// The sampling outcomes.
const (
	// SampleCompleted means the host produced a completion and the server got
	// it.
	SampleCompleted SampleOutcome = iota + 1
	// SampleDenied means the host refused: the binding's depth or concurrency
	// cap was reached, or the handler declined. It is an ordinary outcome — a
	// host is always entitled to decline to spend.
	SampleDenied
	// SampleFailed means the host tried and could not: the handler errored, ran
	// out of time, or returned something that could not go on the wire.
	SampleFailed
)

// String returns a stable lowercase identifier, or "unknown".
func (o SampleOutcome) String() string {
	switch o {
	case SampleCompleted:
		return "completed"
	case SampleDenied:
		return "denied"
	case SampleFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// SamplingRequested reports that a server asked for a completion, emitted when
// the request arrives rather than when it is admitted — a request the gate
// refuses is exactly the one an operator wants to see.
//
// It is an audit record, so it carries the *shape* of the request and none of
// its content: no system prompt, no messages, no completion. A server's prompt
// may contain anything at all, including whatever it has been told by a tool
// result, and an event stream is not the place to find out. Everything here is
// a count, a cap, or a name this module chose.
type SamplingRequested struct {
	// Binding names the server that asked.
	Binding Name
	// Messages is how many messages the conversation carried.
	Messages int
	// MaxTokens is the budget the host granted, after capping the server's
	// request against Limits.MaxSamplingTokens. It is what the handler was told
	// it may spend, not what the server asked for.
	MaxTokens int
	// Depth is this request's position in a sampling chain: 1 for a request the
	// binding was not already doing sampling work for, and n+1 for one that
	// arrived while an outbound request issued from a depth-n sampling handler
	// was in flight. See sampleGate for what this can and cannot see.
	Depth int
	// At is when the request arrived.
	At time.Time
}

// SamplingResolved reports how a sampling request ended. Exactly one follows
// every SamplingRequested, including the ones the gate refused before any model
// ran.
//
// Like SamplingRequested it carries no content: the completion is not here, and
// Model is safe to report because it is the host's own name for the host's own
// model — this module chose it, a server did not supply it.
type SamplingResolved struct {
	// Binding names the server that asked.
	Binding Name
	// Outcome is how it ended.
	Outcome SampleOutcome
	// Model is the model the host used, or empty when none ran.
	Model string
	// Duration is how long the host was working: the figure an operator wants
	// when the question is what a server is costing them.
	Duration time.Duration
	// At is when the outcome was observed.
	At time.Time
}

func (SamplingRequested) event() {}
func (SamplingResolved) event()  {}

// sampleDepthKey is the context key carrying the sampling depth of the handler
// a request was issued from. It is an unexported empty struct type, so nothing
// outside this package can set or forge it.
type sampleDepthKey struct{}

// withSampleDepth stamps ctx with the depth of the sampling handler about to
// run on it.
func withSampleDepth(ctx context.Context, depth int) context.Context {
	return context.WithValue(ctx, sampleDepthKey{}, depth)
}

// sampleDepthOf reports the sampling depth ctx was issued at, or 0 for a context
// that did not come from a sampling handler.
func sampleDepthOf(ctx context.Context) int {
	d, _ := ctx.Value(sampleDepthKey{}).(int)
	return d
}

// Sentinel refusals from the gate. They are wrapped into a *Error with an
// explicit message before they reach a server; they exist so the two caps are
// distinguishable in tests and in the client's own classification, rather than
// by matching on text.
var (
	errSampleDepth       = errors.New("client: sampling depth cap reached")
	errSampleConcurrency = errors.New("client: sampling concurrency cap reached")
)

// sampleGate bounds server-requested sampling on one binding. It is the whole
// enforcement of Limits.MaxSamplingDepth and Limits.MaxSamplingConcurrency.
//
// # Concurrency
//
// Breadth: how many sampling requests may be in flight on the binding at once.
// A request over the cap is refused, never queued. Queuing would be worse than
// useless here — the server is holding a request open while it waits, so a
// sampling request that sits in a queue is spending the server's deadline to
// arrive at the same "no" it could have had at once, and a host that wants to
// serve more says so with a higher cap.
//
// # Depth
//
// Chain length: how many sampling requests deep the binding will go. A chain is
// only possible one way. For a server to ask for sampling *because* of sampling
// already in flight, the host's handler must have caused work on this binding —
// the handler runs a model, the model wants a tool, the host calls back into
// this client, and the server issues a sampling request while serving that call.
// That call is the causal link, and it is one this module can see: the handler's
// context carries its depth (see withSampleDepth), and every outbound request
// registers that depth here for as long as it is in flight (see admit). A
// sampling request arriving while such a call is outstanding is that call's
// child, at one greater depth.
//
// What this cannot see is a chain that leaves the process by another door: a
// server that reaches a *second* host, or a host that drives an unrelated client
// from inside its handler without passing the handler's context. There is no MCP
// facility that would let it — the spec gives an inbound request no correlation
// to whatever caused it, and the SDK exposes none (a sampling request arrives on
// the connection's dispatch goroutine with the SDK's own context, which descends
// from nothing the host holds). So depth here is the depth of chains that run
// through this client, which is the set of chains this client is able to refuse.
// The concurrency cap bounds the rest: a chain this gate cannot attribute still
// occupies an in-flight slot, so it cannot run away, it just cannot be told apart
// from breadth.
//
// Attribution is conservative on purpose. With several sampling-issued calls in
// flight, an arriving request is attributed to the deepest of them, because the
// gate cannot know which one provoked it and the fail-secure reading of an
// ambiguous chain is the longer one.
type sampleGate struct {
	maxDepth int
	maxConc  int

	mu sync.Mutex
	// inflight is how many sampling requests are being served right now.
	inflight int
	// chain is a multiset of the depths of the outbound requests currently in
	// flight that were issued from inside a sampling handler. It is keyed by
	// depth rather than counted, because what an arriving request needs is the
	// deepest live link, not how many there are.
	chain map[int]int
}

// newSampleGate builds the gate for a binding. Call it with normalized limits:
// a non-positive cap here is not "unbounded" but a gate that refuses
// everything, which is the right way for a limit to fail.
func newSampleGate(maxDepth, maxConc int) *sampleGate {
	return &sampleGate{maxDepth: maxDepth, maxConc: maxConc, chain: make(map[int]int)}
}

// enter admits an arriving sampling request.
//
// It returns the request's depth whether or not it admits it: the depth is what
// the audit record reports, and a refused request is the one most worth
// reporting. release must be called by the caller iff err is nil.
func (g *sampleGate) enter() (depth int, release func(), err error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	depth = g.deepestLocked() + 1
	if depth > g.maxDepth {
		return depth, nil, fmt.Errorf("%w: depth %d exceeds %d", errSampleDepth, depth, g.maxDepth)
	}
	if g.inflight >= g.maxConc {
		return depth, nil, fmt.Errorf("%w: %d already in flight, limit %d", errSampleConcurrency, g.inflight, g.maxConc)
	}
	g.inflight++
	return depth, sync.OnceFunc(func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.inflight--
	}), nil
}

// enterChain registers an outbound request issued from a depth-d sampling
// handler, and returns the function that deregisters it. It is what turns "a
// handler called back into this client" into a link the next arriving sampling
// request can be attributed to.
func (g *sampleGate) enterChain(d int) func() {
	g.mu.Lock()
	g.chain[d]++
	g.mu.Unlock()
	return sync.OnceFunc(func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.chain[d]--; g.chain[d] <= 0 {
			// Deleted rather than left at zero: the map is keyed by depth, and a
			// zero entry left behind would keep deepestLocked reporting a link
			// that has gone.
			delete(g.chain, d)
		}
	})
}

// deepestLocked reports the depth of the deepest live sampling-issued call, or
// 0 when there is none.
func (g *sampleGate) deepestLocked() int {
	deepest := 0
	for d, n := range g.chain {
		if n > 0 && d > deepest {
			deepest = d
		}
	}
	return deepest
}

// sampleAdapter wraps the application's sampling handler into the neutral
// callback the connection takes. A nil handler stays nil, which is what makes
// "no handler, no capability" true end to end rather than only at Connect: the
// protocol layer registers nothing for a nil callback, so the SDK cannot
// advertise sampling on this client's behalf.
func (c *Client) sampleAdapter() func(context.Context, protocol.SampleRequest) (protocol.SampleResult, error) {
	if c.samplingHandler == nil {
		return nil
	}
	return c.serveSample
}

// serveSample runs one sampling request: convert, gate, ask, validate.
//
// It runs on the connection's dispatch goroutine while the server waits, so it
// holds nothing of the Client's: emit is called with no lock held, per its
// contract, and the handler is foreign code that may block for as long as the
// binding's request timeout allows.
func (c *Client) serveSample(ctx context.Context, r protocol.SampleRequest) (protocol.SampleResult, error) {
	req, err := c.sampleRequest(r)
	if err != nil {
		return protocol.SampleResult{}, err
	}

	started := time.Now()
	depth, release, gateErr := c.samples.enter()
	c.emit(SamplingRequested{
		Binding:   c.def.Name,
		Messages:  len(req.Messages),
		MaxTokens: req.MaxTokens,
		Depth:     depth,
		At:        started,
	})
	if gateErr != nil {
		c.emit(c.sampleResolved(SampleDenied, "", started))
		return protocol.SampleResult{}, NewError(FailureSamplingDenied, c.def.Name, opSample,
			c.sampleRefusal(gateErr), gateErr)
	}
	defer release()

	// The binding's own deadline, always: a server that asks for a completion
	// nobody produces must not pin a dispatch goroutine, a handler, and a
	// sampling slot forever. Derived from ctx rather than replacing it, so a
	// connection going down still cancels the wait.
	ctx, cancel := context.WithTimeout(ctx, c.def.Timeouts.Request)
	defer cancel()

	// The depth stamp is what lets a call the handler makes be recognized as
	// this request's child (see sampleGate). It goes on the handler's context
	// and nowhere else: a host that wants the chain counted passes that context
	// on, which is the same thing it must do for cancellation to work.
	res, err := c.samplingHandler.Sample(withSampleDepth(ctx, depth), req)
	if err != nil {
		outcome, failure := c.sampleFailure(ctx, err)
		c.emit(c.sampleResolved(outcome, "", started))
		return protocol.SampleResult{}, failure
	}
	if res.Model == "" {
		c.emit(c.sampleResolved(SampleFailed, "", started))
		return protocol.SampleResult{}, NewError(FailureSamplingDenied, c.def.Name, opSample,
			"the sampling handler returned no model", nil)
	}
	c.emit(c.sampleResolved(SampleCompleted, res.Model, started))
	return protocol.SampleResult{Model: res.Model, Text: res.Text, StopReason: res.StopReason}, nil
}

// sampleRefusal renders a gate refusal for the server. It names the cap that was
// reached and nothing else: a server learns that the host will not do this now,
// which is all it is entitled to and all it can act on.
func (c *Client) sampleRefusal(err error) string {
	if errors.Is(err, errSampleDepth) {
		return fmt.Sprintf("this binding's sampling depth limit (%d) is reached", c.def.Limits.MaxSamplingDepth)
	}
	return fmt.Sprintf("this binding's sampling concurrency limit (%d) is reached", c.def.Limits.MaxSamplingConcurrency)
}

// sampleFailure classifies a handler's error.
//
// A host that declined is worth distinguishing from one that broke: the first is
// policy working as intended and the second is the host failing to do something
// it meant to do. A handler says which by the class of the *Error it returns —
// FailureSamplingDenied is the documented way to refuse — and a deadline is read
// off the context, since a handler is free to report one in its own words.
func (c *Client) sampleFailure(ctx context.Context, err error) (SampleOutcome, error) {
	var e *Error
	if errors.As(err, &e) && e.Class == FailureSamplingDenied {
		return SampleDenied, NewError(FailureSamplingDenied, c.def.Name, opSample,
			"the host declined this sampling request", err)
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return SampleFailed, NewError(FailureSamplingDenied, c.def.Name, opSample,
			"the sampling request was not completed within this binding's timeout", err)
	}
	return SampleFailed, NewError(FailureSamplingDenied, c.def.Name, opSample,
		"the sampling handler failed", err)
}

// sampleResolved builds the outcome event, measuring the host's elapsed time
// from started.
func (c *Client) sampleResolved(outcome SampleOutcome, model string, started time.Time) SamplingResolved {
	now := time.Now()
	return SamplingResolved{
		Binding:  c.def.Name,
		Outcome:  outcome,
		Model:    model,
		Duration: now.Sub(started),
		At:       now,
	}
}

// sampleRequest converts a neutral request into the application's, re-checking
// what the boundary already checked and applying the host's token cap.
//
// The re-check is defence in depth, for the reason elicitRequest gives: Conn is
// an interface, and the client is what promises Handlers.Sampling a bounded,
// well-formed request, so the client verifies it.
//
// The token cap is not a re-check but the policy itself, and this is the only
// place it is applied. A server's maxTokens is a request: it is capped against
// the binding's own limit, so a server can lower the ceiling but never raise it.
// It is capped rather than refused because asking for more than a host will
// spend is not misbehavior — it is a server not knowing the host's budget, which
// it has no way to know.
func (c *Client) sampleRequest(r protocol.SampleRequest) (SampleRequest, error) {
	if len(r.Messages) == 0 {
		return SampleRequest{}, NewError(FailureSamplingDenied, c.def.Name, opSample,
			"the sampling request has no messages", nil)
	}
	msgs := make([]SampleMessage, 0, len(r.Messages))
	for _, m := range r.Messages {
		role, err := fromProtocolSampleRole(m.Role)
		if err != nil {
			return SampleRequest{}, NewError(FailureSamplingDenied, c.def.Name, opSample,
				"the sampling request has a message this client cannot attribute", err)
		}
		msgs = append(msgs, SampleMessage{Role: role, Text: m.Text})
	}

	tokens := r.MaxTokens
	if tokens > c.def.Limits.MaxSamplingTokens || tokens <= 0 {
		// The non-positive case is unreachable — internal/protocol refuses it —
		// and is folded in here rather than trusted: a budget is the one number
		// on this request that must never be wrong in the server's favor, and
		// "capped to the host's limit" is the safe reading of every value that
		// is not a smaller positive one.
		tokens = c.def.Limits.MaxSamplingTokens
	}

	return SampleRequest{
		Binding:      c.def.Name,
		SystemPrompt: r.SystemPrompt,
		Messages:     msgs,
		MaxTokens:    tokens,
	}, nil
}

// fromProtocolSampleRole maps the neutral role onto this package's. An
// unrecognized value is refused rather than passed on: this is the boundary that
// promises a handler a role it can switch on.
func fromProtocolSampleRole(r protocol.SampleRole) (SampleRole, error) {
	switch r {
	case protocol.SampleRoleUser:
		return SampleRoleUser, nil
	case protocol.SampleRoleAssistant:
		return SampleRoleAssistant, nil
	default:
		return 0, fmt.Errorf("unknown sampling role %d", r)
	}
}
