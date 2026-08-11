// This file routes a server's request for an LLM completion to the
// application's sampling policy, and the policy's answer back to the server.
//
// # What this file is, and what it deliberately is not
//
// It is a router and an auditor. It is not a policy. Design §Optional sampling:
// "Server-requested sampling gives a server authority to initiate model work and
// spend. It is therefore capability-gated... The application supplies model
// selection, budget, permission, recursion, tool-use, and content policies." So
// there is no model here, no budget arithmetic, no allow-list, and no default
// that would let a composition spend by accident. What this file adds to a
// SamplingPolicy call is the two things the policy cannot get for itself: the
// Harness coordinate the request belongs to (which binding, which Loop), and an
// audit trail that survives the answer.
//
// The caps live one layer down. pkg/client enforces the token ceiling, the
// concurrency cap, and the chain-depth cap before this file is ever reached
// (see client/sampling.go's sampleGate), so a policy does not have to implement
// them to be safe — and a policy that is merely slow or wrong cannot turn a
// server's request into unbounded spend.
//
// # What a policy structurally cannot be given
//
// A SamplingPolicy receives one SampleRequest and returns one SampleResult.
// Neither type has a field that could carry a Manager, a bindingState, a
// client.Client, or a tool registry — they are strings, ints, and a UUID array,
// and TestSamplingSeamCarriesNoAuthority fails on the field KIND of any addition.
// That is the design's "sampling never receives a Harness Session controller or
// unrestricted tool registry" made structural at this layer, the same way
// pkg/client made it structural at its own. The sampler below holds a *Manager,
// and the whole point of the translation it performs is that nothing of it
// travels.
//
// # Why the errors say so little
//
// A refusal reaching the server names no rule. Why the host declined goes to the
// Reporter, exactly as elicitation.go does it and for the same reason: a server
// that can enumerate a host's sampling policy can search for a request that gets
// past it.

package mcpharness

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/looprig/mcp/pkg/client"
)

// opSample names the operation this file's errors carry.
const opSample = "sample"

// maxSampleNoticeTextBytes bounds the host-authored text a sampling Notice may
// carry: a policy's own decline reason, and its own model name.
//
// Neither is server-influenced — a policy is the application's code and the model
// is the host's own choice — so this is not a redaction boundary. It is Notice's
// contract, which says Message is bounded text, made true for the one input to it
// this file does not author. A Reporter is called on a client's event goroutine
// and a host may log what it receives; an unbounded string arriving there because
// a policy returned a verbose error is a bound this module promised and did not
// keep.
const maxSampleNoticeTextBytes = 256

// sampler serves one binding's sampling requests. It holds the binding's state
// rather than looking it up, for the same reason elicitor does: a server may
// sample while the Manager's table lock is held by a concurrent reconfiguration.
type sampler struct {
	m  *Manager
	bs *bindingState
}

var _ client.SamplingHandler = (*sampler)(nil)

// samplingHandler returns the client-side sampling callback for one binding, or
// nil when the application installed no policy.
//
// The nil is the whole fail-closed mechanism, and it is deliberately a nil rather
// than a check. client.Handlers.advertised already owns the rule — a capability
// the Definition requests with no handler to serve it is a FailureInvalidConfig,
// never a silent downgrade — so handing it a nil hands it the fact it needs and
// lets it make its own decision once. A binding that asked for sampling without a
// policy therefore fails at Connect, before its transport is dialed, with the
// same classified error pkg/client would give any other caller. See Deps.Sampling
// for why this module does not re-derive that check.
//
// A policy with no binding asking for it is the harmless direction: the handler
// is installed and never called, which is client.Handlers' documented contract.
// Sampling authority stays per-binding because the capability request is
// per-binding.
func (m *Manager) samplingHandler(bs *bindingState) client.SamplingHandler {
	if m.deps.Sampling == nil {
		return nil
	}
	return &sampler{m: m, bs: bs}
}

// Sample serves one server request for a completion.
//
// It runs on the connection's goroutine while the server waits. ctx is the MCP
// request's context, already bounded by the binding's request timeout and already
// stamped by pkg/client with this request's sampling depth — so a policy that
// passes it on to whatever it calls keeps the client's chain accounting true, and
// a policy that drops it loses cancellation, which is the same trade elicitation
// makes with its gate.
func (s *sampler) Sample(ctx context.Context, req client.SampleRequest) (client.SampleResult, error) {
	translated, err := s.translate(req)
	if err != nil {
		// Nothing was spent: the request was never one this adapter could put to
		// a policy. It is a denial rather than a failure because the host has, in
		// effect, decided not to answer.
		return client.SampleResult{}, s.deny("the request was not one this adapter could route: "+err.Error(), err)
	}

	res, err := s.m.deps.Sampling.Sample(ctx, translated)
	if err != nil {
		return client.SampleResult{}, s.refused(err)
	}
	return client.SampleResult{Model: res.Model, Text: res.Text, StopReason: res.StopReason}, nil
}

// translate converts pkg/client's sampling request into the adapter's seam,
// attributing it to a Loop.
//
// The role re-check is defence in depth and is expected to be unreachable:
// pkg/client's fromProtocolSampleRole already refuses a role it cannot attribute,
// so a value arriving here outside the declared set means that boundary was
// bypassed. It fails closed rather than defaulting to "user", because a policy
// deciding whether to spend on a conversation must not be shown a message
// attributed to the wrong author — "the user asked for this" is exactly the kind
// of claim a content policy weighs.
func (s *sampler) translate(req client.SampleRequest) (SampleRequest, error) {
	msgs := make([]SampleMessage, 0, len(req.Messages))
	for i, m := range req.Messages {
		role, err := sampleRoleOf(m.Role)
		if err != nil {
			return SampleRequest{}, fmt.Errorf("message %d: %w", i, err)
		}
		msgs = append(msgs, SampleMessage{Role: role, Text: m.Text})
	}
	return SampleRequest{
		// This binding's own name, never req.Binding. They agree — Binding.Validate
		// refuses a binding whose Server.Name differs — and taking it from the
		// state the handler was built for is what makes that agreement irrelevant
		// rather than load-bearing.
		Binding:      s.bs.binding.Name,
		LoopID:       s.bs.loopID(),
		SystemPrompt: req.SystemPrompt,
		Messages:     msgs,
		MaxTokens:    req.MaxTokens,
	}, nil
}

// sampleRoleOf maps pkg/client's role onto the adapter's seam. An undeclared
// value is refused rather than passed on: this is the boundary that promises a
// policy a role it can switch on.
func sampleRoleOf(r client.SampleRole) (SampleRole, error) {
	switch r {
	case client.SampleRoleUser:
		return SampleRoleUser, nil
	case client.SampleRoleAssistant:
		return SampleRoleAssistant, nil
	default:
		return 0, fmt.Errorf("the message carries role %d, which this adapter cannot attribute", uint8(r))
	}
}

// refused classifies a policy's error and reports it.
//
// A policy that DECLINED is worth distinguishing from one that BROKE: the first
// is a budget being enforced, the second is a defect. ErrSamplingDenied is how a
// policy says which, and it is a sentinel rather than a class check so that a
// host never has to construct a pkg/client error to refuse a request pkg/client
// never showed it.
//
// A failure is deliberately NOT re-labelled as a denial. pkg/client reads the
// class off this error to tell the two apart: FailureSamplingDenied is the
// documented way to refuse, and anything else is the host having tried and
// broken. FailureIndeterminate is what that is — the policy knows what went
// wrong and this adapter does not, which is exactly the class manager.go already
// uses for an error this module cannot classify.
//
// Both errors carry an explicit message, and both keep the policy's own error as
// an inert cause rather than as text. That is what stops a host's own policy
// reason travelling to the server: a *client.Error renders a wrapped cause only
// when it has no message of its own, and neither of these is without one. The
// rule is applied HERE rather than relied upon downstream — a refusal that is
// only redacted because another package happens to render it a certain way is one
// that leaks the day that package's rendering changes.
func (s *sampler) refused(err error) error {
	if errors.Is(err, ErrSamplingDenied) {
		return s.deny("the host's sampling policy declined: "+err.Error(), err)
	}
	s.report(NoticeSamplingDenied, "the host's sampling policy failed: "+err.Error())
	return client.NewError(client.FailureIndeterminate, client.Name(s.bs.binding.Name), opSample,
		"the host's sampling policy failed", err)
}

// deny refuses a request as policy, telling the host why and the server nothing
// but "denied".
func (s *sampler) deny(why string, cause error) error {
	s.report(NoticeSamplingDenied, why)
	return client.NewError(client.FailureSamplingDenied, client.Name(s.bs.binding.Name), opSample,
		"the host declined this sampling request", cause)
}

// report raises one of this file's notices against the binding and its Loop.
func (s *sampler) report(kind NoticeKind, message string) {
	s.m.report(Notice{
		Kind:    kind,
		Binding: s.bs.binding.Name,
		LoopID:  s.bs.loopID(),
		Message: boundNoticeText(message),
	})
}

// sampleAudit turns pkg/client's sampling events into notices, and is the whole
// of design §Optional sampling's "sampling requests and outcomes are audited
// without recording secrets".
//
// # Why a Notice and not an event.IntegrationStatus
//
// Neither of these says whether the binding is up, which is the only question
// event.IntegrationStatus answers — a server whose sampling request was refused
// is a healthy server being told no. So statusFor ignores them (they fall to its
// default arm) and they arrive here instead, on the sink for facts only MCP
// knows. Without this they would be dropped entirely: the Manager owns
// Handlers.Event for every binding, so an application composing MCP with Harness
// has no other way to see that a server is spending its money.
//
// # Why this cannot record a secret
//
// It has nothing to record. pkg/client built these events to carry the SHAPE of a
// request and never its content: no system prompt, no messages, no completion —
// only counts, caps, a duration, and names this side chose. Model is the host's
// own name for the host's own model, returned by the host's own policy; a server
// cannot set it. Everything else below is this file's literal text and an integer.
func (m *Manager) sampleAudit(bs *bindingState, ev client.Event) {
	switch e := ev.(type) {
	case client.SamplingRequested:
		m.report(Notice{
			Kind:    NoticeSamplingRequested,
			Binding: bs.binding.Name,
			LoopID:  bs.loopID(),
			Message: fmt.Sprintf("a server requested a completion: %d message(s), a %d token budget, at sampling depth %d",
				e.Messages, e.MaxTokens, e.Depth),
		})
	case client.SamplingResolved:
		m.report(Notice{
			Kind:    NoticeSamplingResolved,
			Binding: bs.binding.Name,
			LoopID:  bs.loopID(),
			Message: boundNoticeText(fmt.Sprintf("a sampling request %s after %s%s",
				e.Outcome, e.Duration.Round(time.Millisecond), sampleModelSuffix(e.Model))),
		})
	}
}

// sampleModelSuffix names the model a completion ran on, or nothing when none
// ran. A denied or failed request has no model, and reporting an empty one as
// though it were a name would make every refusal read like a completion.
func sampleModelSuffix(model string) string {
	if model == "" {
		return ""
	}
	return ", using model " + model
}

// boundNoticeText truncates host-authored text to Notice's documented bound. It
// truncates on a byte boundary and is deliberately not rune-aware: this is a
// length bound on diagnostics, not a rendering, and a Notice's Message has never
// promised to be valid UTF-8 that a host itself supplied.
func boundNoticeText(s string) string {
	if len(s) <= maxSampleNoticeTextBytes {
		return s
	}
	return s[:maxSampleNoticeTextBytes]
}
