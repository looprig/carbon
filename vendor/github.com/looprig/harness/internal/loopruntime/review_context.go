package loopruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"sync/atomic"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/gate"
	"github.com/looprig/harness/pkg/identity"
)

const reviewContextRevisionDomain = "harness.loop.permission-review-context/v1"

// reviewContextMetadata is private runtime wiring. A configured permission
// reviewer must supply every required field; omission fails before a tool is
// authorized. Keeping it private prevents ordinary tools from acquiring a
// review-context capability.
type reviewContextMetadata struct {
	WorkspaceRoot      string
	WorkingDirectory   string
	RetryReason        string
	SecurityCeiling    string
	GatePolicyRevision string
}

type reviewContextConfiguration struct {
	Metadata reviewContextMetadata
	Policy   gate.ReviewContextPolicy
}

// reviewContextCapture is the complete caller-owned state visible at the tool
// batch boundary. Base contains genuine committed conversation FROM BEFORE
// THIS TURN; BaseRetained is the leading slice of that same pre-turn history
// that is instead generated/derived retained intent (a compaction summary
// that survived into a later turn's base, or into a restored loop's seeded
// history — loopState.msgsDerivedPrefix's doc comment). Staged is this
// turn's own genuine conversation; Retained is generated/derived retained
// intent produced DURING this turn (a mid-turn compaction summary). Base and
// BaseRetained / Staged and Retained are the same distinction applied to two
// different time spans (cross-turn vs. this-turn) — both derived pairs are
// evidence but never direct user authority.
type reviewContextCapture struct {
	Coordinates  identity.Coordinates
	Base         content.AgenticMessages
	BaseRetained content.AgenticMessages
	Retained     content.AgenticMessages
	Staged       content.AgenticMessages
	Active       *content.AIMessage
	RuntimeTail  *content.UserMessage
	Metadata     reviewContextMetadata
	Policy       gate.ReviewContextPolicy
}

type reviewContextCaptureError struct {
	Field string
	Cause error
}

func (e *reviewContextCaptureError) Error() string {
	return "loop: permission review context capture failed: " + e.Field
}

func (e *reviewContextCaptureError) Unwrap() error { return e.Cause }

type permissionReviewContextKey struct{}

func withPermissionReviewContext(ctx context.Context, review gate.ReviewContext) context.Context {
	return context.WithValue(ctx, permissionReviewContextKey{}, review.Clone())
}

// ReviewContext is the Harness-internal (Go-exported, but still inside the
// internal/ boundary — unreachable from outside this module) input that turns
// on live permission-review context capture for every turn a constructed Loop
// runs. internal/sessionruntime is the only caller: it builds one whenever a
// session has permission classifiers registered at all (see
// Session.loopReviewContext), auto-deriving what Harness already knows and
// sourcing the rest from the session's already-registered review policy. A
// nil *ReviewContext (the default, threaded by every pre-existing caller)
// leaves capture off, byte-identical to every Loop built before this
// addendum. Ordinary tools never see this type: it flows only from
// sessionruntime, through NewInModeWithCompactor/NewRestoredWithCompactor,
// into the loop's private runtimeConfig/turnConfig.reviewContext.
type ReviewContext struct {
	WorkspaceRoot      string
	WorkingDirectory   string
	RetryReason        string
	SecurityCeiling    string
	GatePolicyRevision string
	Policy             gate.ReviewContextPolicy
}

// toInternal converts the exported, session-facing shape into the private
// turn-level configuration turnConfig.reviewContext carries. A nil receiver
// (no review context supplied) converts to nil, preserving the exact
// pre-addendum "capture off" default.
func (r *ReviewContext) toInternal() *reviewContextConfiguration {
	if r == nil {
		return nil
	}
	return &reviewContextConfiguration{
		Metadata: reviewContextMetadata{
			WorkspaceRoot:      r.WorkspaceRoot,
			WorkingDirectory:   r.WorkingDirectory,
			RetryReason:        r.RetryReason,
			SecurityCeiling:    r.SecurityCeiling,
			GatePolicyRevision: r.GatePolicyRevision,
		},
		Policy: r.Policy,
	}
}

// reviewContextCaptureProvider defers one tool batch's permission-review
// context capture until a permission gate is genuinely about to open.
// approvalRequesterFor (gate.go), via reviewContextForApproval, is the
// earliest point that is knowable: the loop's public tool-access contract
// (pkg/loop.AccessGate) exposes only Authorize, which decides — internally,
// dynamically, per call — whether a gate opens at all; there is no cheaper
// preview available at the batch boundary without duplicating access
// evaluation. Memoized via sync.Once (+ an atomic "was it ever attempted"
// flag) so every gate opened within the same batch observes the identical
// captured context (or the identical failure), and a batch whose calls never
// reach an open gate NEVER attempts capture at all — so it can never fail the
// turn on the hard preflight bounds regardless of conversation size.
type reviewContextCaptureProvider struct {
	once      sync.Once
	attempted atomic.Bool

	coordinates       identity.Coordinates
	base              content.AgenticMessages
	baseDerivedPrefix int
	msgs              content.AgenticMessages
	derivedPrefix     int
	active            *content.AIMessage
	runtimeTail       *content.UserMessage
	metadata          reviewContextMetadata
	policy            gate.ReviewContextPolicy

	review gate.ReviewContext
	err    error
}

// newReviewContextCaptureProvider builds a provider for one tool batch. The
// raw pre-slice pieces (base + baseDerivedPrefix, msgs + derivedPrefix) are
// carried rather than an already-sliced reviewContextCapture so both
// derivedPrefix bounds checks ALSO stay deferred — a slice with an invalid
// prefix must never panic, but checking it eagerly (before knowing whether a
// gate opens) would reintroduce exactly the unconditional-capture hazard
// this addendum closes.
func newReviewContextCaptureProvider(
	coordinates identity.Coordinates,
	base content.AgenticMessages,
	baseDerivedPrefix int,
	msgs content.AgenticMessages,
	derivedPrefix int,
	active *content.AIMessage,
	runtimeTail *content.UserMessage,
	metadata reviewContextMetadata,
	policy gate.ReviewContextPolicy,
) *reviewContextCaptureProvider {
	return &reviewContextCaptureProvider{
		coordinates: coordinates, base: base, baseDerivedPrefix: baseDerivedPrefix,
		msgs: msgs, derivedPrefix: derivedPrefix,
		active: active, runtimeTail: runtimeTail, metadata: metadata, policy: policy,
	}
}

// capture runs capturePermissionReviewContext at most once for this batch —
// on the FIRST call, from whichever gate opens first — and returns an
// independent clone of the memoized result on every call, so two gates opened
// in the same batch can never alias or mutate each other's entries. A nil
// receiver (no review context configured for this turn at all) reports the
// pre-existing "nothing to review" zero value with no error.
func (p *reviewContextCaptureProvider) capture() (gate.ReviewContext, error) {
	if p == nil {
		return gate.ReviewContext{}, nil
	}
	p.once.Do(func() {
		p.attempted.Store(true)
		if p.derivedPrefix < 0 || p.derivedPrefix > len(p.msgs) {
			p.err = &reviewContextCaptureError{Field: "derived_context"}
			return
		}
		if p.baseDerivedPrefix < 0 || p.baseDerivedPrefix > len(p.base) {
			p.err = &reviewContextCaptureError{Field: "base_derived_context"}
			return
		}
		p.review, p.err = capturePermissionReviewContext(reviewContextCapture{
			Coordinates:  p.coordinates,
			Base:         p.base[p.baseDerivedPrefix:],
			BaseRetained: p.base[:p.baseDerivedPrefix],
			Retained:     p.msgs[:p.derivedPrefix],
			Staged:       p.msgs[p.derivedPrefix:],
			Active:       p.active,
			RuntimeTail:  p.runtimeTail,
			Metadata:     p.metadata,
			Policy:       p.policy,
		})
	})
	if p.err != nil {
		return gate.ReviewContext{}, p.err
	}
	return p.review.Clone(), nil
}

// failed reports whether this batch's capture was actually attempted (a gate
// genuinely tried to open) and, if so, the error it failed with (nil on
// success). It never triggers capture itself — a nil receiver, or a provider
// whose once has never fired, both report attempted=false — which is the
// entire point of deferring capture in the first place: runTurn can check
// this AFTER RunBatch returns without ever paying for a capture that no call
// in the batch needed.
func (p *reviewContextCaptureProvider) failed() (err error, attempted bool) {
	if p == nil || !p.attempted.Load() {
		return nil, false
	}
	return p.err, true
}

// permissionReviewCaptureKey is the ctx key for the lazy, batch-scoped
// capture provider — distinct from permissionReviewContextKey (which carries
// an ALREADY-resolved value for the narrower test seam above). Installed once
// per batch by runTurn; read by reviewContextForApproval the moment a
// permission gate is genuinely about to open.
type permissionReviewCaptureKey struct{}

// withPermissionReviewCapture installs this batch's lazy capture provider on
// ctx. A nil provider (review context not configured for this turn) is a
// no-op: the batch ctx is returned unchanged, exactly matching the
// pre-existing "no provider present" ctx shape.
func withPermissionReviewCapture(ctx context.Context, provider *reviewContextCaptureProvider) context.Context {
	if provider == nil {
		return ctx
	}
	return context.WithValue(ctx, permissionReviewCaptureKey{}, provider)
}

func permissionReviewCaptureFromContext(ctx context.Context) (*reviewContextCaptureProvider, bool) {
	provider, ok := ctx.Value(permissionReviewCaptureKey{}).(*reviewContextCaptureProvider)
	return provider, ok
}

// reviewContextForApproval triggers this batch's lazy review-context capture
// (see reviewContextCaptureProvider) at the one point a permission gate is
// genuinely about to open — approvalRequesterFor (gate.go) calls this before
// ever registering the gate. A zero result with a nil error and no provider
// present means review was never configured for this turn: callers must
// treat that exactly as before ("nothing to review"). A non-nil error means
// review WAS configured and its one-per-batch capture attempt failed closed:
// the caller must refuse to open the gate.
func reviewContextForApproval(ctx context.Context) (gate.ReviewContext, error) {
	provider, ok := permissionReviewCaptureFromContext(ctx)
	if !ok {
		return gate.ReviewContext{}, nil
	}
	return provider.capture()
}

func capturePermissionReviewContext(input reviewContextCapture) (gate.ReviewContext, error) {
	if input.Metadata.WorkspaceRoot == "" ||
		input.Metadata.WorkingDirectory == "" ||
		input.Metadata.SecurityCeiling == "" ||
		input.Metadata.GatePolicyRevision == "" {
		return gate.ReviewContext{}, &reviewContextCaptureError{Field: "metadata"}
	}
	if err := preflightReviewContextCapture(input); err != nil {
		return gate.ReviewContext{}, err
	}
	entries := make([]gate.ReviewContextEntry, 0, reviewMessageCapacity(input))
	var err error
	entries, err = appendReviewMessages(entries, input.BaseRetained, reviewMessageAuthorityDerived)
	if err != nil {
		return gate.ReviewContext{}, err
	}
	entries, err = appendReviewMessages(entries, input.Base, reviewMessageAuthorityConversation)
	if err != nil {
		return gate.ReviewContext{}, err
	}
	entries, err = appendReviewMessages(entries, input.Retained, reviewMessageAuthorityDerived)
	if err != nil {
		return gate.ReviewContext{}, err
	}
	entries, err = appendReviewMessages(entries, input.Staged, reviewMessageAuthorityConversation)
	if err != nil {
		return gate.ReviewContext{}, err
	}
	if input.RuntimeTail != nil {
		entries, err = appendReviewMessage(entries, input.RuntimeTail, reviewMessageAuthorityRuntime)
		if err != nil {
			return gate.ReviewContext{}, err
		}
	}
	if input.Active == nil {
		return gate.ReviewContext{}, &reviewContextCaptureError{Field: "active_message"}
	}
	entries, err = appendReviewMessage(entries, input.Active, reviewMessageAuthorityConversation)
	if err != nil {
		return gate.ReviewContext{}, err
	}

	context := gate.ReviewContext{
		Coordinates:        input.Coordinates,
		WorkspaceRoot:      input.Metadata.WorkspaceRoot,
		WorkingDirectory:   input.Metadata.WorkingDirectory,
		RetryReason:        input.Metadata.RetryReason,
		SecurityCeiling:    input.Metadata.SecurityCeiling,
		GatePolicyRevision: input.Metadata.GatePolicyRevision,
		Entries:            entries,
	}
	context.ContextRevision, err = permissionReviewContextRevision(context, input.Policy)
	if err != nil {
		return gate.ReviewContext{}, err
	}
	bounded, err := gate.BuildReviewContext(context, input.Policy)
	if err != nil {
		return gate.ReviewContext{}, &reviewContextCaptureError{Field: "bounded_context", Cause: err}
	}
	return bounded, nil
}

func preflightReviewContextCapture(input reviewContextCapture) error {
	count, total := 0, 0
	accumulate := func(messages content.AgenticMessages) error {
		for _, message := range messages {
			blocks, err := reviewMessageBlocks(message)
			if err != nil {
				return err
			}
			for _, block := range blocks {
				size, err := reviewBlockInputBytes(block, 0)
				if err != nil {
					return err
				}
				count++
				if count > gate.MaxReviewContextInputEntries ||
					size > gate.MaxReviewContextEntryInputBytes ||
					size > gate.MaxReviewContextInputBytes-total {
					return &reviewContextCaptureError{Field: "input_bounds"}
				}
				total += size
			}
		}
		return nil
	}
	for _, messages := range []content.AgenticMessages{
		input.BaseRetained, input.Base, input.Retained, input.Staged,
	} {
		if err := accumulate(messages); err != nil {
			return err
		}
	}
	if input.RuntimeTail != nil {
		if err := accumulate(content.AgenticMessages{input.RuntimeTail}); err != nil {
			return err
		}
	}
	if input.Active != nil {
		if err := accumulate(content.AgenticMessages{input.Active}); err != nil {
			return err
		}
	}
	return nil
}

func reviewMessageBlocks(message content.Conversation) ([]content.Block, error) {
	if message == nil {
		return nil, &reviewContextCaptureError{Field: "message"}
	}
	switch typed := message.(type) {
	case *content.UserMessage:
		if typed != nil {
			return typed.Blocks, nil
		}
	case *content.AIMessage:
		if typed != nil {
			return typed.Blocks, nil
		}
	case *content.ToolResultMessage:
		if typed != nil {
			return typed.Blocks, nil
		}
	case *content.SystemMessage:
		if typed != nil {
			return typed.Blocks, nil
		}
	}
	return nil, &reviewContextCaptureError{Field: "message"}
}

func reviewBlockInputBytes(block content.Block, depth int) (int, error) {
	if depth > 64 {
		return 0, &reviewContextCaptureError{Field: "content_block"}
	}
	switch typed := block.(type) {
	case *content.TextBlock:
		if typed != nil {
			return len(typed.Text), nil
		}
	case *content.ImageBlock:
		if typed != nil {
			return checkedReviewInputSize(len(typed.MediaType), len(typed.Source.URL), len(typed.Source.Data))
		}
	case *content.AudioBlock:
		if typed != nil {
			return checkedReviewInputSize(len(typed.MediaType), len(typed.Data))
		}
	case *content.DocumentBlock:
		if typed != nil {
			return checkedReviewInputSize(len(typed.MediaType), len(typed.Name), len(typed.Data), len(typed.Text))
		}
	case *content.ThinkingBlock:
		if typed != nil {
			return checkedReviewInputSize(len(typed.Thinking), len(typed.Signature))
		}
	case *content.ToolUseBlock:
		if typed != nil {
			return checkedReviewInputSize(len(typed.ID), len(typed.Name), len(typed.Input))
		}
	case *content.ToolResultBlock:
		if typed != nil {
			total, err := checkedReviewInputSize(len(typed.ToolUseID))
			if err != nil {
				return 0, err
			}
			for _, nested := range typed.Content {
				size, err := reviewBlockInputBytes(nested, depth+1)
				if err != nil {
					return 0, err
				}
				if size > gate.MaxReviewContextInputBytes-total {
					return 0, &reviewContextCaptureError{Field: "input_bounds"}
				}
				total += size
			}
			return total, nil
		}
	}
	return 0, &reviewContextCaptureError{Field: "content_block"}
}

func checkedReviewInputSize(sizes ...int) (int, error) {
	total := 0
	for _, size := range sizes {
		if size < 0 || size > gate.MaxReviewContextInputBytes-total {
			return 0, &reviewContextCaptureError{Field: "input_bounds"}
		}
		total += size
	}
	return total, nil
}

func reviewMessageCapacity(input reviewContextCapture) int {
	return len(input.BaseRetained) + len(input.Base) + len(input.Retained) + len(input.Staged) + 2
}

type reviewMessageAuthority uint8

const (
	reviewMessageAuthorityConversation reviewMessageAuthority = iota
	reviewMessageAuthorityDerived
	reviewMessageAuthorityRuntime
)

func appendReviewMessages(
	entries []gate.ReviewContextEntry,
	messages content.AgenticMessages,
	authority reviewMessageAuthority,
) ([]gate.ReviewContextEntry, error) {
	for _, message := range messages {
		var err error
		entries, err = appendReviewMessage(entries, message, authority)
		if err != nil {
			return nil, err
		}
	}
	return entries, nil
}

func appendReviewMessage(
	entries []gate.ReviewContextEntry,
	message content.Conversation,
	authority reviewMessageAuthority,
) ([]gate.ReviewContextEntry, error) {
	if message == nil {
		return nil, &reviewContextCaptureError{Field: "message"}
	}
	switch typed := message.(type) {
	case *content.UserMessage:
		if typed == nil {
			return nil, &reviewContextCaptureError{Field: "message"}
		}
		origin, kind := gate.ReviewContextOriginUser, gate.ReviewContextKindUserMessage
		if authority != reviewMessageAuthorityConversation {
			origin, kind = gate.ReviewContextOriginRuntime, gate.ReviewContextKindRuntimeContext
		}
		return appendReviewBlocks(entries, typed.Blocks, origin, kind)
	case *content.AIMessage:
		if typed == nil {
			return nil, &reviewContextCaptureError{Field: "message"}
		}
		return appendAssistantReviewBlocks(entries, typed.Blocks)
	case *content.ToolResultMessage:
		if typed == nil {
			return nil, &reviewContextCaptureError{Field: "message"}
		}
		return appendReviewBlocks(entries, typed.Blocks, gate.ReviewContextOriginTool, gate.ReviewContextKindToolResult)
	case *content.SystemMessage:
		if typed == nil {
			return nil, &reviewContextCaptureError{Field: "message"}
		}
		return appendReviewBlocks(entries, typed.Blocks, gate.ReviewContextOriginRuntime, gate.ReviewContextKindRuntimeContext)
	default:
		return nil, &reviewContextCaptureError{Field: "message"}
	}
}

func appendAssistantReviewBlocks(
	entries []gate.ReviewContextEntry,
	blocks []content.Block,
) ([]gate.ReviewContextEntry, error) {
	for _, block := range blocks {
		kind := gate.ReviewContextKindAssistantMessage
		if _, ok := block.(*content.ToolUseBlock); ok {
			kind = gate.ReviewContextKindAssistantToolRequest
		}
		var err error
		entries, err = appendReviewBlock(entries, block, gate.ReviewContextOriginAssistant, kind)
		if err != nil {
			return nil, err
		}
	}
	return entries, nil
}

func appendReviewBlocks(
	entries []gate.ReviewContextEntry,
	blocks []content.Block,
	origin gate.ReviewContextOrigin,
	kind gate.ReviewContextKind,
) ([]gate.ReviewContextEntry, error) {
	for _, block := range blocks {
		entryOrigin, entryKind := origin, kind
		switch block.(type) {
		case *content.ImageBlock, *content.AudioBlock, *content.DocumentBlock:
			entryOrigin, entryKind = gate.ReviewContextOriginExternal, gate.ReviewContextKindExternalContent
		}
		var err error
		entries, err = appendReviewBlock(entries, block, entryOrigin, entryKind)
		if err != nil {
			return nil, err
		}
	}
	return entries, nil
}

func appendReviewBlock(
	entries []gate.ReviewContextEntry,
	block content.Block,
	origin gate.ReviewContextOrigin,
	kind gate.ReviewContextKind,
) ([]gate.ReviewContextEntry, error) {
	encoded, err := content.MarshalBlock(block)
	if err != nil {
		return nil, &reviewContextCaptureError{Field: "content_block", Cause: err}
	}
	return append(entries, gate.ReviewContextEntry{
		Origin: origin, Kind: kind, Content: string(encoded),
	}), nil
}

func permissionReviewContextRevision(
	context gate.ReviewContext,
	policy gate.ReviewContextPolicy,
) (string, error) {
	projection := struct {
		Domain               string                    `json:"domain"`
		Coordinates          identity.Coordinates      `json:"coordinates"`
		WorkspaceRoot        string                    `json:"workspace_root"`
		WorkingDirectory     string                    `json:"working_directory"`
		RetryReason          string                    `json:"retry_reason"`
		SecurityCeiling      string                    `json:"security_ceiling"`
		GatePolicyRevision   string                    `json:"gate_policy_revision"`
		ReviewPolicyRevision string                    `json:"review_policy_revision"`
		Entries              []gate.ReviewContextEntry `json:"entries"`
	}{
		Domain: reviewContextRevisionDomain, Coordinates: context.Coordinates,
		WorkspaceRoot: context.WorkspaceRoot, WorkingDirectory: context.WorkingDirectory,
		RetryReason: context.RetryReason, SecurityCeiling: context.SecurityCeiling,
		GatePolicyRevision: context.GatePolicyRevision, ReviewPolicyRevision: policy.Revision,
		Entries: context.Entries,
	}
	encoded, err := json.Marshal(projection)
	if err != nil {
		return "", &reviewContextCaptureError{Field: "context_revision", Cause: err}
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}
