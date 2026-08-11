// This file implements the calls a caller makes against a live binding: tools,
// prompts, resources, subscriptions.
//
// Every one of them goes through the same three gates before it reaches the
// wire, in this order:
//
//  1. the binding is serving (readiness);
//  2. the server advertised the capability (compatibility);
//  3. the host permits the call (policy — the ToolFilter).
//
// The order is not arbitrary. Each gate is cheaper and more certain than the
// next thing it protects, and each fails closed with a class the caller can
// branch on rather than a message it would have to parse.

package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/looprig/mcp/internal/catalog"
	"github.com/looprig/mcp/internal/lifecycle"
	"github.com/looprig/mcp/internal/protocol"
	"github.com/looprig/mcp/internal/sched"
)

// Operation names carried by the errors in this file.
const (
	opCallTool     = "call_tool"
	opGetPrompt    = "get_prompt"
	opReadResource = "read_resource"
	opSubscribe    = "subscribe"
	opUnsubscribe  = "unsubscribe"
)

// Progress is one progress report from an in-flight call. Every field is
// server-supplied: it is a hint to render, never a fact to act on — a server
// may claim any progress it likes, including going backwards or never
// finishing.
type Progress struct {
	// Binding names the server reporting progress.
	Binding Name
	// Progress is how far the server claims to have got.
	Progress float64
	// Total is the server's claimed total, or 0 when it did not say.
	Total float64
	// Message is the server's bounded description of what it is doing.
	Message string
}

// CallOpts are the per-call options for CallTool.
type CallOpts struct {
	// Progress, when non-nil, receives the call's progress notifications.
	//
	// Installing it is what asks the server for them: a server may only send
	// progress for a request that carries a progress token, and a token is only
	// attached when this is set. So a nil Progress does not merely discard the
	// notifications — it stops them being generated.
	//
	// It is invoked on the connection's notification goroutine and blocks it,
	// so it must not do work. Hand off anything expensive.
	//
	// Progress NEVER extends the call's deadline. See Deadline.
	Progress func(Progress)

	// Deadline bounds this call. Zero means now plus the binding's
	// Timeouts.Request.
	//
	// It is an absolute instant, fixed before the request is sent, and nothing
	// the server does afterwards moves it. In particular progress notifications
	// do not: a deadline that reset on activity would be a deadline the server
	// controls, and any server — hostile or merely stuck in a retry loop —
	// could hold a call, a goroutine and a permission open indefinitely just by
	// remaining chatty. A long-running tool needs a long deadline, which the
	// caller sets here, deliberately and in advance.
	//
	// The caller's ctx applies as well; whichever expires first wins.
	Deadline time.Time
}

// ToolResult is the outcome of a tool call.
//
// A tool that fails is not an error here: IsError reports a protocol-level tool
// error — the call reached the server, ran, and the tool says it did not work —
// and Content carries its explanation. That is the design's rule, and it
// matters because the two failures need opposite handling: a tool error is
// information for the model to react to, while a transport error is a fault the
// host must handle. Collapsing them would either hide a broken connection
// inside a plausible-looking result, or turn a tool saying "no such file" into
// a binding failure.
type ToolResult struct {
	// IsError reports that the tool itself failed.
	IsError bool
	// Content is the unstructured result, already bounded and converted.
	Content []Content
	// Structured is the tool's structured result, within
	// Limits.MaxStructuredBytes. Nil when the server sent none or an
	// over-bound one was dropped; see Warnings.
	Structured json.RawMessage
	// Warnings records defects tolerated while converting the result.
	Warnings []string
}

// Prompt is the outcome of GetPrompt.
//
// Its messages are external content. They are not promoted into a host's
// instructions by being fetched: an application that wants a prompt to carry
// instruction authority must decide that itself.
type Prompt struct {
	Description string
	Messages    []PromptMessage
}

// PromptMessage is one message of a prompt. Role is the server's string
// verbatim ("user" or "assistant" in practice).
type PromptMessage struct {
	Role    string
	Content Content
}

// Resource is the outcome of ReadResource.
type Resource struct {
	Contents []ResourceContent
}

// ResourceContent is one item of a resource's contents. Exactly one of Text or
// Data is meaningful. Truncated reports that the payload was cut, or that a
// binary one was refused on a bound and only its metadata survives.
type ResourceContent struct {
	// URI is the item's opaque protocol identifier.
	URI       string
	MIMEType  string
	Text      string
	Data      []byte
	Truncated bool
}

// CallTool invokes a tool by its raw server name — the name in
// ToolSpec.RawName, not the model-facing one.
//
// args is the tool's arguments as raw JSON. They are sent verbatim; validating
// them against the tool's InputSchema is the caller's job, because the caller
// is the layer that knows which generation's schema it validated against.
//
// A tool that reports failure comes back as a ToolResult with IsError set and a
// nil error. An error return means the call did not produce a result: the
// binding is not serving, the tool is not available, the deadline expired, the
// caller cancelled, or the connection failed.
//
// Cancelling ctx cancels the call at the protocol level — the server is told to
// stop, not merely abandoned.
func (c *Client) CallTool(ctx context.Context, rawName string, args json.RawMessage, opts CallOpts) (ToolResult, error) {
	conn, epoch, err := c.serving(opCallTool)
	if err != nil {
		return ToolResult{}, err
	}
	// One read of the adopted generation, and every question below is asked of
	// that one. Reading it twice would let a refresh adopt a new generation in
	// between and admit a call on evidence that never coexisted: capabilities
	// from the generation before the refresh, a tool from the one after.
	gen := c.adopted()
	if caps, ok := capabilitiesOf(gen); !ok || !caps.Tools {
		return ToolResult{}, c.unsupported(opCallTool, "tools")
	}
	// Policy before lookup: a denied tool is refused whether or not it exists,
	// so a caller cannot use the error to learn what a filtered server offers.
	if !c.def.ToolFilter.Permits(rawName) {
		return ToolResult{}, NewError(FailureToolUnavailable, c.def.Name, opCallTool,
			fmt.Sprintf("tool %q is not permitted by this binding's ToolFilter", rawName), nil)
	}
	if _, ok := gen.ToolByRawName(rawName); !ok {
		return ToolResult{}, NewError(FailureToolUnavailable, c.def.Name, opCallTool,
			fmt.Sprintf("tool %q is not in the adopted catalog", rawName), nil)
	}

	callCtx, cancel := c.withDeadline(ctx, opts.Deadline)
	defer cancel()

	// Admission last, after every gate: a call that policy refuses must not
	// queue behind the binding's other calls first, and must not consume a slot
	// to be told no.
	callCtx, release, err := c.admit(ctx, callCtx, opCallTool, sched.ClassCall)
	if err != nil {
		return ToolResult{}, err
	}
	defer release()

	res, err := conn.CallTool(callCtx, rawName, args, protocol.CallOptions{
		Progress: c.progressAdapter(opts.Progress),
	})
	if err != nil {
		return ToolResult{}, c.toolCallFailure(ctx, callCtx, epoch, err)
	}
	return ToolResult{
		IsError:    res.IsError,
		Content:    fromProtocolContents(res.Content),
		Structured: res.Structured,
		Warnings:   res.Warnings,
	}, nil
}

// GetPrompt fetches a prompt's messages, substituting args.
func (c *Client) GetPrompt(ctx context.Context, name string, args map[string]string) (Prompt, error) {
	conn, epoch, err := c.serving(opGetPrompt)
	if err != nil {
		return Prompt{}, err
	}
	if caps, ok := c.serverCapabilities(); !ok || !caps.Prompts {
		return Prompt{}, c.unsupported(opGetPrompt, "prompts")
	}

	callCtx, cancel := c.withDeadline(ctx, time.Time{})
	defer cancel()
	callCtx, release, err := c.admit(ctx, callCtx, opGetPrompt, sched.ClassRequest)
	if err != nil {
		return Prompt{}, err
	}
	defer release()

	res, err := conn.GetPrompt(callCtx, name, args)
	if err != nil {
		return Prompt{}, c.requestFailure(ctx, callCtx, opGetPrompt, epoch, err)
	}
	out := Prompt{Description: res.Description}
	if len(res.Messages) > 0 {
		out.Messages = make([]PromptMessage, len(res.Messages))
		for i, m := range res.Messages {
			out.Messages[i] = PromptMessage{Role: m.Role, Content: fromProtocolContent(m.Content)}
		}
	}
	return out, nil
}

// ReadResource reads a resource by URI.
//
// The URI is an opaque protocol identifier the server issued — not a host path.
// Nothing here resolves it against a filesystem.
func (c *Client) ReadResource(ctx context.Context, uri string) (Resource, error) {
	conn, epoch, err := c.serving(opReadResource)
	if err != nil {
		return Resource{}, err
	}
	if caps, ok := c.serverCapabilities(); !ok || !caps.Resources {
		return Resource{}, c.unsupported(opReadResource, "resources")
	}

	callCtx, cancel := c.withDeadline(ctx, time.Time{})
	defer cancel()
	callCtx, release, err := c.admit(ctx, callCtx, opReadResource, sched.ClassRequest)
	if err != nil {
		return Resource{}, err
	}
	defer release()

	res, err := conn.ReadResource(callCtx, uri)
	if err != nil {
		return Resource{}, c.requestFailure(ctx, callCtx, opReadResource, epoch, err)
	}
	out := Resource{}
	if len(res.Contents) > 0 {
		out.Contents = make([]ResourceContent, len(res.Contents))
		for i, rc := range res.Contents {
			out.Contents[i] = ResourceContent{
				URI:       rc.URI,
				MIMEType:  rc.MIMEType,
				Text:      rc.Text,
				Data:      rc.Data,
				Truncated: rc.Truncated,
			}
		}
	}
	return out, nil
}

// Subscribe asks the server to report changes to a resource.
//
// It is refused unless the server advertised resources *and* subscription:
// subscribing is a separate capability from reading, and a server that only
// advertised resources has not promised resources/subscribe exists.
func (c *Client) Subscribe(ctx context.Context, uri string) error {
	conn, epoch, err := c.serving(opSubscribe)
	if err != nil {
		return err
	}
	caps, ok := c.serverCapabilities()
	if !ok || !caps.Resources || !caps.ResourcesSubscribe {
		return c.unsupported(opSubscribe, "resource subscription")
	}

	callCtx, cancel := c.withDeadline(ctx, time.Time{})
	defer cancel()
	callCtx, release, err := c.admit(ctx, callCtx, opSubscribe, sched.ClassRequest)
	if err != nil {
		return err
	}
	defer release()

	if err := conn.Subscribe(callCtx, uri); err != nil {
		return c.requestFailure(ctx, callCtx, opSubscribe, epoch, err)
	}
	return nil
}

// Unsubscribe asks the server to stop reporting changes to a resource.
//
// It is the counterpart to Subscribe and is gated the same way: a server that
// never advertised resource subscription cannot have a subscription to end, so
// the call is refused rather than sent. After it returns, no further
// ResourceUpdated events arrive for uri.
//
// Unsubscribing a resource that was never subscribed is the server's to judge,
// not this client's: MCP has no local record of what is subscribed, so the
// request is forwarded and whatever the server answers is returned.
func (c *Client) Unsubscribe(ctx context.Context, uri string) error {
	conn, epoch, err := c.serving(opUnsubscribe)
	if err != nil {
		return err
	}
	caps, ok := c.serverCapabilities()
	if !ok || !caps.Resources || !caps.ResourcesSubscribe {
		return c.unsupported(opUnsubscribe, "resource subscription")
	}

	callCtx, cancel := c.withDeadline(ctx, time.Time{})
	defer cancel()
	callCtx, release, err := c.admit(ctx, callCtx, opUnsubscribe, sched.ClassRequest)
	if err != nil {
		return err
	}
	defer release()

	if err := conn.Unsubscribe(callCtx, uri); err != nil {
		return c.requestFailure(ctx, callCtx, opUnsubscribe, epoch, err)
	}
	return nil
}

// onResourceUpdated surfaces a server's resource-update notification to the
// application as a ResourceUpdated event.
//
// It mirrors onListChanged: the notification is a claim, not content, so nothing
// is refetched here — a caller that wants the new value re-reads the resource.
// The URI arrives already bounded from internal/protocol.
func (c *Client) onResourceUpdated(u protocol.ResourceUpdate) {
	c.emit(ResourceUpdated{Binding: c.def.Name, URI: u.URI, At: time.Now()})
}

// serving returns the connection if the binding can carry a call right now.
//
// StateReady and StateDegraded both serve: degraded means "working, with a
// known fault", and refusing calls on it would make a partial fault a total
// outage. Everything else — starting, discovering, failed, closing, closed —
// has no business carrying one.
// It returns the connection together with its epoch, and the two must travel
// together: everything the caller later says about this connection failing is
// only meaningful about the connection it actually used (see Client.connEpoch).
func (c *Client) serving(op string) (protocol.Conn, uint64, error) {
	state := c.machine.State()
	switch state {
	case lifecycle.StateReady, lifecycle.StateDegraded:
	case lifecycle.StateClosing, lifecycle.StateClosed:
		return nil, 0, NewError(FailureShutdown, c.def.Name, op, "the binding is closed", nil)
	default:
		return nil, 0, NewError(FailureIndeterminate, c.def.Name, op,
			"the binding is not serving calls (state: "+state.String()+")", nil)
	}

	c.mu.Lock()
	conn, epoch := c.conn, c.connEpoch
	c.mu.Unlock()
	if conn == nil {
		// A ready binding always has a conn; this is defence in depth against a
		// startup path that ever changed.
		return nil, 0, NewError(FailureIndeterminate, c.def.Name, op, "the binding has no connection", nil)
	}
	return conn, epoch, nil
}

// admit puts a request through the binding's scheduler, classifying a refusal.
//
// It returns the request's own context: derived from the call's, cancelled by
// its release, and cancelled by shutdown — and by nothing else, which is what
// makes one caller's cancellation invisible to every other call on the same
// connection.
//
// The wait counts against the deadline that callCtx already carries. That is
// deliberate: a caller's deadline is wall-clock, and a call that spent it queued
// behind the binding's other calls has missed it just as surely as one that
// spent it waiting on the server. It also keeps the deadline computed exactly
// once, before the send, with nothing after that point able to move it.
func (c *Client) admit(ctx, callCtx context.Context, op string, class sched.Class) (context.Context, func(), error) {
	// A request issued from inside a sampling handler is re-entrant, and a
	// re-entrant tool call must not queue behind the tool-call serializer.
	//
	// The chain is: a tool call is in flight, holding the ClassCall permit; the
	// server it called asks for sampling while it waits; the host's handler calls
	// back into this client to make another tool call. That inner call cannot get
	// the ClassCall permit — the outer call it descends from is still holding it,
	// and will not release it until the sampling round-trip the inner call is part
	// of returns. Queueing the inner call behind that permit is a deadlock that
	// only the request deadline breaks. So a re-entrant tool call is admitted
	// without the serializer, bounded instead by the sampling depth and
	// concurrency caps that already govern the chain (see sampleGate) and still
	// counted against the connection's concurrency budget. Non-re-entrant calls
	// are unaffected: they serialize exactly as before, which is the default
	// posture other tests assert.
	depth := sampleDepthOf(ctx)
	admitClass := class
	if depth > 0 && class == sched.ClassCall {
		admitClass = sched.ClassReentrantCall
	}

	reqCtx, release, err := c.sched.Begin(callCtx, admitClass)
	if err == nil {
		// A request issued from inside a sampling handler is the one causal link
		// in a sampling chain this module can see: it is why the server has work
		// in flight, so it is why the server might ask for sampling again. It is
		// registered for exactly as long as it is outstanding, so that a sampling
		// request arriving meanwhile is attributed to it rather than treated as a
		// fresh one. See sampleGate.
		if depth > 0 {
			leave := c.samples.enterChain(depth)
			return reqCtx, func() { leave(); release() }, nil
		}
		return reqCtx, release, nil
	}
	if errors.Is(err, sched.ErrShutdown) {
		return nil, nil, NewError(FailureShutdown, c.def.Name, op, "the binding is shutting down", nil)
	}
	// Everything else is the context ending: the caller gave up, or the call
	// ran out of time while queued. callFailure already tells those apart.
	return nil, nil, c.callFailure(ctx, callCtx, op, err)
}

// unsupported reports a method the server never promised. It is the design's
// compatibility rule at the call site: the client checks the server's
// capabilities before using a method, so an unadvertised one is refused here
// rather than sent and failed as an ambiguous method-not-found.
func (c *Client) unsupported(op, what string) *Error {
	return NewError(FailureUnsupportedProtocol, c.def.Name, op,
		"the server does not advertise "+what, nil)
}

// serverCapabilities returns what the adopted generation recorded the server
// advertising, and whether there is an adopted generation at all. It is for the
// callers whose only question is the capability; one that also consults the
// catalog reads the generation itself and asks capabilitiesOf, so that both
// answers come from the same generation.
func (c *Client) serverCapabilities() (ServerCapabilities, bool) {
	return capabilitiesOf(c.adopted())
}

// capabilitiesOf returns what gen recorded the server advertising, and whether
// there is a generation at all. A nil generation reports no capabilities rather
// than an error of its own: "no adopted catalog" and "the server never
// advertised this" are the same refusal to the caller — the method is not
// available on this binding — and the caller has nothing different to do about
// either.
func capabilitiesOf(gen *catalog.Generation) (ServerCapabilities, bool) {
	if gen == nil {
		return ServerCapabilities{}, false
	}
	return fromProtocolCapabilities(gen.Capabilities()), true
}

// withDeadline derives the call's context.
//
// The deadline is computed once, here, before the request is sent. That is what
// makes "progress does not extend a deadline" true by construction rather than
// by remembering not to: there is no code path that could move it, because
// nothing after this point holds the timer.
func (c *Client) withDeadline(ctx context.Context, deadline time.Time) (context.Context, context.CancelFunc) {
	if deadline.IsZero() {
		return context.WithTimeout(ctx, c.def.Timeouts.Request)
	}
	// context.WithDeadline already takes the earlier of this and ctx's own, so
	// a caller can shorten a call but never outlive its own context.
	return context.WithDeadline(ctx, deadline)
}

// progressAdapter wraps a caller's progress callback, tagging each update with
// the binding. A nil callback stays nil, which is what stops a progress token
// being attached at all.
func (c *Client) progressAdapter(fn func(Progress)) func(protocol.ProgressUpdate) {
	if fn == nil {
		return nil
	}
	return func(u protocol.ProgressUpdate) {
		fn(Progress{
			Binding:  c.def.Name,
			Progress: u.Progress,
			Total:    u.Total,
			Message:  u.Message,
		})
		// Mirrored to the event stream for observers. Deliberately inside the
		// nil check rather than outside it: an event here must not be the thing
		// that puts a progress token on the wire, because that would make
		// installing an observer change what the server is asked to do.
		c.emit(RequestProgress{
			Binding:  c.def.Name,
			Progress: u.Progress,
			Total:    u.Total,
			Message:  u.Message,
			At:       time.Now(),
		})
	}
}

// callFailure classifies a failed call.
//
// Both contexts are needed and they mean different things: callCtx expiring is
// this call's deadline, while ctx expiring is the caller giving up. They are
// distinguishable only by asking the outer one first — callCtx is derived from
// ctx, so a cancelled caller cancels both, and reading callCtx alone would
// report every cancellation as a deadline.
func (c *Client) callFailure(ctx, callCtx context.Context, op string, err error) error {
	var typed *Error
	if errors.As(err, &typed) {
		// A transport already classified this; it knows better than this layer.
		return typed
	}
	switch {
	case ctx.Err() != nil:
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return NewError(FailureDeadline, c.def.Name, op, "the caller's context expired", err)
		}
		return NewError(FailureCancelled, c.def.Name, op, "the call was cancelled", nil)
	case errors.Is(callCtx.Err(), context.DeadlineExceeded):
		return NewError(FailureDeadline, c.def.Name, op, "the call exceeded its deadline", nil)
	}
	return NewError(FailureServerProtocol, c.def.Name, op, "", err)
}

// toolCallFailure classifies a failed tool call, and is where the design's
// indeterminacy rule lives.
//
// A tool call that was in flight when the connection died has no known outcome.
// The request may never have arrived; it may have arrived and run; it may have
// run and had its reply lost with the connection. Nothing here can tell those
// apart — the evidence is on the other side of a connection that no longer
// exists — so the caller is told exactly that, and is never told the call failed.
//
// The distinction is the whole point. "Failed" invites a retry, and a retried
// tool call is a second effect: a second branch deleted, a second payment sent.
// FailureIndeterminate is a class a caller cannot handle by reflex, which is
// correct, because the situation cannot be handled by reflex.
//
// A lost connection also starts a reconnect, if policy permits (see
// reconnect.go). That is separate from what this call is told: the binding may
// recover in a second, and this call's outcome is still unknown.
func (c *Client) toolCallFailure(ctx, callCtx context.Context, epoch uint64, err error) error {
	out := c.callFailure(ctx, callCtx, opCallTool, err)
	class, ok := ClassOf(out)
	if !ok || !transientConnectionLoss(class) {
		return out
	}
	c.connectionLost(epoch, class, out)
	return NewError(FailureIndeterminate, c.def.Name, opCallTool,
		"the connection failed with the call in flight: the tool may or may not have run, and it is not retried", out)
}

// requestFailure classifies a failed non-call request.
//
// It reports the connection loss like a tool call does, but keeps the class it
// was given. A read that was lost is a read that did not happen: re-reading has
// no second effect, so there is nothing indeterminate to warn a caller about,
// and dressing a closed transport up as an indeterminate outcome would only make
// the class meaningless where it matters.
//
// It does not re-issue the request either. Whether re-reading is wanted is the
// caller's decision, made with knowledge this layer does not have.
func (c *Client) requestFailure(ctx, callCtx context.Context, op string, epoch uint64, err error) error {
	return c.noteFailure(epoch, c.callFailure(ctx, callCtx, op, err))
}

// adopted returns the generation this binding has adopted, or nil.
//
// The generation is immutable, so handing the pointer out under the lock and
// reading it afterwards is sound: whatever it points at cannot change.
func (c *Client) adopted() *catalog.Generation {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.generation
}
