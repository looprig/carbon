// This file is the crossing point: an MCP tool on one side, a Harness tool on
// the other, and nothing MCP-shaped past it.
//
// Three rules shape everything here.
//
// Identity is carried, never recovered. Each adapted tool closes over its
// binding, its raw server name, the generation it was cut from, that
// generation's input-schema digest, and its route. Nothing in the call path
// parses a model-facing name to decide what to invoke: the name is lossy in
// both directions that matter (see names.go), so a reparse is a guess, and a
// guess here means one server's tool answering under another's credentials.
//
// A tool's failure is the model's information; a binding's failure is the
// host's problem. A tool that says "no such file" is a result the model should
// read and react to. A connection that died is not — but it is still not a
// reason to end a turn, so it comes back as an "error: " tool result too and the
// binding's own status carries the fault (design §Calling a tool 7-8; the
// Session and the Loop stay healthy either way). InvokableRun therefore never
// returns a Go error, which is also the Harness-wide convention for tools.
//
// Everything from a server is untrusted and bounded twice. pkg/client bounds
// each item against the binding's Limits; this file bounds what reaches a
// model's context — the count of blocks and the total text — because a
// per-item bound says nothing about a server that returns ten thousand items.

package mcpharness

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	"github.com/looprig/mcp/pkg/client"
)

// ToolSource is the external-toolset slot every MCP tool is installed under.
// A replacement replaces this source's whole generation and never touches a
// Loop's declared tools or another source's (see loop.ExternalToolset).
const ToolSource = "mcp"

// errPrefix is the Harness convention for a tool-result error string. It is
// duplicated rather than imported because it is a rendering convention of the
// runner, not an exported contract; matching it keeps an MCP failure reading
// like every other tool's failure in a transcript.
const errPrefix = "error: "

// Bounds on what one converted result may put into a model's context.
//
// They are deliberately far below pkg/client's per-item limits, and they bound
// a different thing: client.Limits stops a server exhausting this process's
// memory, while these stop a well-bounded result being an unusable one. A
// server returning 1 MiB of text per item within its limits, sixty times over,
// is doing nothing the connection layer should refuse — and would still be a
// tool result no model can read.
const (
	// maxResultTextBytes caps the total text across every block of one result.
	// Text past it is replaced by a truncation marker (design §Content
	// conversion and limits: "the model receives a bounded summary and
	// reference, never an unbounded payload").
	maxResultTextBytes = 64 << 10
	// maxResultBlocks caps how many blocks one result may contribute.
	maxResultBlocks = 32
)

// CapabilityToolInvoke is the normalized capability kind every external MCP
// tool emits from preparation. It is a CONSUMER-BOUND kind: the consumer routes
// it to its own product access source via a gate AccessBinding (the
// access-profile spec names the product composition root's binding), never to a
// sandbox profile, and
// it is never silently mapped to command execution. The gate always resolves it
// Gated — one combined approval or one persisted rule keyed on the tool
// identity — so a tool.invoke requirement carries no reusable rule candidates
// of this adapter's own. The product composition root binds this product-bound
// kind:
// (carbon-assembly: tools emit "tool.invoke").
const CapabilityToolInvoke = "tool.invoke"

// ToolInvokeIdentity is the stable capability identity of one MCP tool
// (requirement Scope and Match). It is what a persisted allow rule is keyed on,
// so it must be deterministic across calls for the same tool AND name the
// binding together with the raw tool: a rule for "search_issues" on the github
// binding must never satisfy a call to "search_issues" on some other server
// that happens to use the same word.
//
// The encoding is "mcp:<binding>:<raw-tool>". Both components are already
// validated identifiers — the binding by client.Name, the raw tool by the
// catalog's validateRawName — so the identity is bounded, control-free, and
// carries no argument material (redaction: the arguments never enter it).
func ToolInvokeIdentity(binding, rawTool string) string {
	return "mcp:" + binding + ":" + rawTool
}

// adaptedTool is one MCP tool as Harness sees it: one raw tool, on one binding,
// from one adopted generation.
//
// It is immutable after construction and holds no per-call state, so one value
// serves every concurrent call. Adoption does not mutate it — adoption builds
// new ones (design §Harness safe-boundary integration: the adapter must not
// mutate a live tool's name, schema, or implementation in place).
type adaptedTool struct {
	// binding is the binding name: the authority this call runs under.
	binding string
	// rawTool is the server's own name for the tool — what goes on the wire.
	rawTool string
	// modelName is the catalog-assigned model-facing name. It is display and
	// identity for the model; it is never parsed to route.
	modelName string
	desc      string
	schema    json.RawMessage
	// generation is the catalog generation this tool was cut from.
	generation uint64
	// schemaDigest is that generation's input-schema digest for rawTool. It is
	// what makes "the arguments Harness validated match the schema this call
	// will be interpreted under" a checkable claim rather than an assumption.
	schemaDigest string
	// route is the binding's live state. Calls go through acquire/release so a
	// retiring binding can tell when its last turn is done.
	route *bindingState
	// identity is the tool.invoke capability identity, precomputed.
	identity string
}

// Compile-time proof of the contracts this type claims.
var (
	_ tool.InvokableTool = (*adaptedTool)(nil)
	_ tool.CallPreparer  = (*adaptedTool)(nil)
	_ tool.Auditable     = (*adaptedTool)(nil)
)

// Info describes the tool to the model and to Harness's argument validation.
// The schema is the one from the generation this tool was cut from, which is
// the same one InvokableRun checks the server still honors.
func (t *adaptedTool) Info(context.Context) (*tool.ToolInfo, error) {
	return &tool.ToolInfo{Name: t.modelName, Desc: t.desc, Schema: t.schema}, nil
}

// AuditSummary is the one-line audit record for this call. It names the
// capability and nothing else: arguments may carry a path, a query, a token, or
// a customer's data, and an audit event is exactly the wrong place to learn
// that (CLAUDE.md: log security events, not secrets).
func (t *adaptedTool) AuditSummary(string) string {
	return fmt.Sprintf("MCP %s on %s", t.rawTool, t.binding)
}

// PrepareCall is the tool-owned preparation boundary (design §Permissions under
// the access-profile model). It runs once per call, before any permission
// decision: it validates the untrusted arguments and emits the typed, redacted
// tool.invoke requirement the gate evaluates, plus the per-call artifact
// InvokableRun executes.
//
// Three properties this method is responsible for:
//
//   - Redacted. The requirement names the binding, the raw tool, and nothing
//     else. An MCP tool's arguments are a schema this adapter has never seen,
//     from a server it does not trust, so there is no field it could safely
//     surface — and the design's rule is explicit: no credentials, no request
//     bodies, no unbounded arguments in a gate or an audit event. The arguments
//     travel only in the opaque artifact, never in the requirement.
//
//   - Stable. Scope and Match are ToolInvokeIdentity(binding, rawTool), a
//     deterministic function of this tool's identity alone. Two calls to the
//     same tool produce the same requirement, so a persisted allow rule can
//     match it; a call to a different tool never collides.
//
//   - Fail-closed on malformed input. Arguments that are not JSON are refused
//     HERE — the call is never evaluated and never sent. A prepared request
//     whose own identity could not be represented safely (a pathological raw
//     name) is refused the same way rather than handed to the gate.
func (t *adaptedTool) PrepareCall(_ context.Context, executionID uuid.UUID, argsJSON string) (tool.Request, tool.PreparedArtifact, error) {
	args, err := callArgs(argsJSON)
	if err != nil {
		// Harness has NOT validated these — preparation is where an MCP tool's
		// arguments are first checked — so a non-JSON payload fails closed at the
		// boundary rather than being posted to a server that would have to guess.
		return tool.Request{}, nil, fmt.Errorf("mcp: %w", err)
	}
	request := tool.Request{
		ToolName:    t.modelName,
		ExecutionID: executionID.String(),
		Requirements: []tool.Requirement{{
			Kind:  CapabilityToolInvoke,
			Scope: t.identity,
			Match: t.identity,
			// Redacted: the tool and its server, never the arguments.
			Description: fmt.Sprintf("call the %q tool on MCP server %q", t.rawTool, t.binding),
		}},
	}
	if err := tool.ValidateRequest(request); err != nil {
		return tool.Request{}, nil, fmt.Errorf("mcp: prepared request is invalid: %w", err)
	}
	// The normalized arguments travel only here — read back at InvokableRun time,
	// never reparsed from the model-facing argsJSON.
	return request, tool.TokenArtifact{Token: string(args)}, nil
}

// InvokableRun performs the call: steps 3-8 of design §Calling a tool. Steps 1
// and 2 — argument validation and the permission decision on the qualified
// capability — already happened: this tool validated the arguments in
// PrepareCall, and the gate resolved the tool.invoke requirement it emitted. The
// argsJSON parameter is deliberately ignored; the bytes that go on the wire are
// the ones preparation validated and the gate decided on, read back from the
// prepared contract, never reparsed from a model-facing string that could have
// changed since the prompt.
//
// It fails closed without that contract. An MCP call is effectful, so a run with
// no prepared call on ctx — or one carrying another tool's artifact — is refused
// as a tool result and the server is never contacted.
//
// It never returns a Go error. Every failure it can reach is either the model's
// information (the tool said no) or the host's problem (the connection died),
// and neither is a reason to end the turn; the binding's own status carries a
// fault, and the model gets a bounded "error: " result it can react to.
func (t *adaptedTool) InvokableRun(ctx context.Context, _ string) (*tool.ToolResult, error) {
	// The arguments come from the prepared contract, not the raw call. Without
	// it (or with a foreign artifact) there is nothing this call was approved to
	// send, so it fails closed before touching the route.
	call, ok := loop.PreparedCallFromContext(ctx)
	if !ok {
		return textResult(errPrefix + "mcp: permission denied: this call requires its prepared contract"), nil
	}
	art, ok := call.Artifact.(tool.TokenArtifact)
	if !ok {
		return textResult(errPrefix + "mcp: permission denied: the prepared contract does not carry this call's arguments"), nil
	}
	args := json.RawMessage(art.Token)

	// Step 3: the route must still belong to this binding. acquire is the seam
	// that makes retirement honest — it refuses a binding on its way out, and
	// the matching release is what lets a retiring one know its last turn is
	// done.
	cl, ok := t.route.acquire()
	if !ok {
		return t.unavailable("the binding is no longer serving"), nil
	}
	defer t.route.release()

	// Step 4: a newer catalog may already prove this tool gone or changed. If
	// it does, this call fails as itself — it is never quietly re-pointed at
	// whatever now answers to the same raw name.
	if res := t.checkGeneration(cl); res != nil {
		return res, nil
	}

	// Step 5. Deadline is left zero on purpose: the client applies the
	// binding's own Timeouts.Request, which is where a per-server bound is
	// configured. ctx carries the turn's cancellation and cancels the call at
	// the protocol level, not merely locally.
	//
	// Progress is not requested. Installing a callback is what asks a server to
	// send progress at all, and Harness has no sink for a mid-call progress
	// report from a tool — so asking for one would put server-controlled
	// traffic on the connection's notification goroutine for nobody to read.
	// See the report accompanying this stage.
	res, err := cl.CallTool(ctx, t.rawTool, args, client.CallOpts{})
	if err != nil {
		return t.callFailure(err), nil
	}
	return t.result(res), nil
}

// checkGeneration is step 4: does this tool, cut from generation t.generation,
// still describe something the server will do?
//
// Both the adopted catalog and the candidate are asked, and the difference
// matters. Adoption is per-binding while a toolset is per-Loop, so a Loop can
// legitimately still be holding this generation's tools after a *different*
// Loop adopted a newer one at its own idle (design §Catalog model: Loop A
// adopts while Loop B remains active). The adopted catalog is what a call is
// actually admitted against; the candidate is the newer evidence that has not
// been adopted anywhere yet. Either can prove the tool gone or changed.
//
// A nil result means the tool is still good.
func (t *adaptedTool) checkGeneration(cl *client.Client) *tool.ToolResult {
	if cat := cl.Catalog(); cat.Valid() {
		if res := t.checkAgainst(cat); res != nil {
			return res
		}
	}
	if cand, ok := cl.Candidate(); ok {
		if res := t.checkAgainst(cand); res != nil {
			return res
		}
	}
	return nil
}

// checkAgainst compares this tool against one catalog. Skipping a catalog of
// this tool's own generation would be an optimization of a comparison that is
// already trivially true; comparing it is the same answer and one fewer branch
// to get wrong.
func (t *adaptedTool) checkAgainst(cat client.Catalog) *tool.ToolResult {
	spec, found := cat.ToolByRawName(t.rawTool)
	if !found {
		return t.unavailable(fmt.Sprintf("the server no longer offers it (catalog generation %d)", cat.Generation))
	}
	if spec.InputSchemaDigest != t.schemaDigest {
		// The name survived; the contract did not. An old schema is never
		// silently applied to a new tool definition — the arguments Harness
		// validated describe a tool that no longer exists under this name.
		return t.schemaChanged(cat.Generation)
	}
	return nil
}

// callFailure classifies a failed call (step 8).
//
// FailureToolUnavailable and FailureToolSchemaChanged are folded into the same
// structured results checkGeneration produces, deliberately: an active turn may
// call from its own snapshot after a change notification but before any
// adoption, and reach a server that has already removed the tool. The server's
// unknown-tool answer and this adapter's own catalog check are the same fact
// discovered at two moments, so a model must not have to tell them apart
// (design §Calling a tool).
//
// Everything else — transport, auth, deadline, cancellation — becomes a bounded
// error result. The binding's fault, if it has one, is already recorded by the
// client that classified it and is visible through Manager.Status; repeating it
// as a Go error here would end a turn over a failure the model can simply be
// told about.
func (t *adaptedTool) callFailure(err error) *tool.ToolResult {
	class, ok := client.ClassOf(err)
	if !ok {
		return t.errorf("call failed: %s", client.FailureIndeterminate)
	}
	switch class {
	case client.FailureToolUnavailable:
		return t.unavailable("the server does not offer it")
	case client.FailureToolSchemaChanged:
		return t.schemaChanged(0)
	default:
		// The class, never the message: a *client.Error's Msg is bounded and
		// redacted, but it is still server-influenced text, and a class is what
		// a model or an operator can act on.
		return t.errorf("call failed (%s)", class)
	}
}

// result converts a completed call. A tool that reports failure is a result,
// not an error: Content carries the tool's own explanation of what went wrong,
// which is precisely what the model needs to try something else.
func (t *adaptedTool) result(res client.ToolResult) *tool.ToolResult {
	blocks := convertContents(res.Content, res.Structured)
	if res.IsError {
		// The marker goes first so a truncated or image-only error result still
		// reads as an error, and it names the tool: a bare "error:" in a
		// transcript of six concurrent calls identifies nothing.
		blocks = append([]content.Block{&content.TextBlock{
			Text: fmt.Sprintf("%smcp: tool %q on binding %q reported an error", errPrefix, t.rawTool, t.binding),
		}}, blocks...)
	}
	if len(blocks) == 0 {
		// A result with no content at all is not an error — a server may
		// legitimately have nothing to say — but an empty ToolResult would be
		// replaced by the runner's own "empty result" error string, which would
		// misreport a successful call as a failed one.
		return textResult(fmt.Sprintf("mcp: tool %q on binding %q returned no content", t.rawTool, t.binding))
	}
	return &tool.ToolResult{Content: blocks}
}

// unavailable is the structured ToolUnavailable result. It is a result and not
// an error by design: the tool is gone, the turn is fine.
func (t *adaptedTool) unavailable(why string) *tool.ToolResult {
	return t.errorf("ToolUnavailable: %s", why)
}

// schemaChanged is the structured ToolSchemaChanged result. generation is the
// catalog that proved it, or 0 when the server itself did.
func (t *adaptedTool) schemaChanged(generation uint64) *tool.ToolResult {
	if generation == 0 {
		return t.errorf("ToolSchemaChanged: the server reports a different schema than generation %d described", t.generation)
	}
	return t.errorf("ToolSchemaChanged: catalog generation %d describes a different schema than generation %d, which this call was made under",
		generation, t.generation)
}

// errorf renders a bounded, attributable tool-error string. The format and its
// arguments are always this module's own text and already-validated names —
// never server content.
func (t *adaptedTool) errorf(format string, args ...any) *tool.ToolResult {
	return textResult(fmt.Sprintf("%smcp: tool %q on binding %q: %s",
		errPrefix, t.rawTool, t.binding, fmt.Sprintf(format, args...)))
}

func textResult(s string) *tool.ToolResult { return tool.TextResult(s) }

// callArgs normalizes a tool's arguments for the wire.
//
// An absent or blank argument string becomes an empty object: a no-argument
// tool call is normal, and "" is not JSON — sending it would make a
// well-formed call look like a malformed one to the server. Anything else must
// actually be JSON. Harness validated it against the schema already, so a
// failure here is this side's defect and is refused rather than forwarded.
func callArgs(argsJSON string) (json.RawMessage, error) {
	trimmed := strings.TrimSpace(argsJSON)
	if trimmed == "" {
		return json.RawMessage(`{}`), nil
	}
	if !json.Valid([]byte(trimmed)) {
		return nil, fmt.Errorf("arguments are not valid JSON")
	}
	return json.RawMessage(trimmed), nil
}

// convertContents maps MCP content onto Harness blocks (step 6), then bounds
// the whole.
//
// The rule is that nothing disappears quietly: a kind Harness cannot carry
// becomes a labeled placeholder naming what was dropped and how big it was, so
// a user reading a transcript sees an item they cannot use rather than a
// silence they cannot explain.
func convertContents(items []client.Content, structured json.RawMessage) []content.Block {
	blocks := make([]content.Block, 0, len(items)+1)
	for _, item := range items {
		blocks = append(blocks, convertContent(item))
	}
	if len(structured) > 0 {
		// Structured content is a JSON document, and a model reads it as text.
		// It is not merged into a neighbouring text block: a caller must be
		// able to tell what the tool said from what the tool returned.
		blocks = append(blocks, &content.TextBlock{
			Text: "mcp: structured content:\n" + string(structured),
		})
	}
	return bound(blocks)
}

// convertContent maps one item. It handles every member of the sealed
// client.Content union and keeps a default case anyway: a new member must
// become a visible placeholder, never a panic and never a silent drop.
func convertContent(item client.Content) content.Block {
	switch c := item.(type) {
	case client.Text:
		if c.Truncated {
			return &content.TextBlock{Text: c.Text + "\n[mcp: truncated at the binding's text limit]"}
		}
		return &content.TextBlock{Text: c.Text}
	case client.Image:
		return &content.ImageBlock{
			MediaType: content.MediaType(c.MIMEType),
			Source:    content.ImageSource{Data: c.Data},
		}
	case client.Audio:
		return &content.AudioBlock{MediaType: content.MediaType(c.MIMEType), Data: c.Data}
	case client.EmbeddedResource:
		// A resource is a document, and its URI is its name — an opaque
		// protocol identifier, never resolved as a path (client.EmbeddedResource).
		block := &content.DocumentBlock{
			MediaType: content.MediaType(c.MIMEType),
			Name:      c.URI,
			Data:      c.Data,
			Text:      c.Text,
		}
		if c.Truncated && block.Text != "" {
			block.Text += "\n[mcp: truncated at the binding's limit]"
		}
		return block
	case client.Unsupported:
		return &content.TextBlock{
			Text: fmt.Sprintf("[mcp: unsupported content of kind %q, %d bytes, not included]", c.Kind, c.Bytes),
		}
	default:
		return &content.TextBlock{Text: "[mcp: unrecognized content, not included]"}
	}
}

// bound caps a converted result at what a model's context can take: the number
// of blocks, and the total text across them.
//
// It truncates the last text block it keeps rather than dropping it whole, and
// it always says so. A silent cut is worse than a short result — a model that
// cannot see that output stopped will confidently reason about the half it got.
func bound(blocks []content.Block) []content.Block {
	out := make([]content.Block, 0, min(len(blocks), maxResultBlocks))
	budget := maxResultTextBytes
	for _, block := range blocks {
		if len(out) == maxResultBlocks {
			out = append(out[:maxResultBlocks-1], &content.TextBlock{
				Text: fmt.Sprintf("[mcp: %d further content items omitted at this adapter's block limit]", len(blocks)-maxResultBlocks+1),
			})
			return out
		}
		text, ok := block.(*content.TextBlock)
		if !ok {
			out = append(out, block)
			continue
		}
		if len(text.Text) <= budget {
			budget -= len(text.Text)
			out = append(out, block)
			continue
		}
		out = append(out, &content.TextBlock{
			Text: truncateUTF8(text.Text, budget) + "\n[mcp: truncated at this adapter's result limit]",
		})
		return out
	}
	return out
}

// truncateUTF8 cuts s to at most n bytes without splitting a rune, so a
// truncated result is still valid UTF-8 rather than a string with a mangled
// tail.
func truncateUTF8(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// utf8RuneStart reports whether b may begin a UTF-8 rune (i.e. is not a
// continuation byte).
func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// SessionTools returns the external tool definitions the identified Loop may
// consume from the Session's shared bindings.
//
// loopName is matched by a Visibility that names Loops rather than IDs; a Loop
// with neither an ID nor a name a selector permits sees nothing, because the
// zero LoopSelector permits nothing (selector.go).
func (m *Manager) SessionTools(loopID uuid.UUID, loopName string) []tool.Definition {
	return m.toolsFor(loopID, m.sessionRoutes(loopID, loopName))
}

// LoopTools returns the external tool definitions from the bindings the
// identified Loop owns. A delegate never appears here for its parent's private
// bindings — it was never their owner (design §Delegation).
func (m *Manager) LoopTools(loopID uuid.UUID) []tool.Definition {
	return m.toolsFor(loopID, m.loopRoutes(loopID))
}

// bindingTools is one binding's contribution to a Loop's namespace: its route
// and the adopted catalog the definitions are cut from.
type bindingTools struct {
	route *bindingState
	name  string
	cat   client.Catalog
}

// toolsFor assembles the union of the given bindings' tools for one Loop.
//
// The union is the whole reason this is not a per-binding loop: a Loop sees
// many bindings under one namespace, and only here can two of them be seen to
// claim one model-facing name. A collision degrades the offending binding — its
// whole toolset, not just the colliding tool, since a partially installed
// binding is one whose absent tools a model would keep trying to call — and
// leaves the incumbent serving.
func (m *Manager) toolsFor(loopID uuid.UUID, states []*bindingState) []tool.Definition {
	entries := make([]bindingTools, 0, len(states))
	for _, bs := range states {
		cl := bs.client()
		if cl == nil {
			continue
		}
		cat := cl.Catalog()
		if !cat.Valid() {
			continue
		}
		entries = append(entries, bindingTools{route: bs, name: bs.binding.Name, cat: cat})
	}

	accepted, rejected := resolveNames(entries)
	for _, r := range rejected {
		m.report(Notice{
			Kind:    NoticeToolNameCollision,
			Binding: r.binding,
			LoopID:  loopID,
			Message: r.err.Error(),
		})
	}

	defs := make([]tool.Definition, 0, len(accepted))
	for _, e := range accepted {
		defs = append(defs, m.bundle(e))
	}
	return defs
}

// rejection is one binding refused from a Loop's namespace, and why.
type rejection struct {
	binding string
	err     error
}

// resolveNames decides which bindings may contribute to one namespace.
//
// It is all-or-nothing per binding and order-stable: bindings arrive in name
// order, and the first to claim a name keeps it. That is a policy, not an
// accident — it means a Loop's namespace does not reshuffle because an
// unrelated binding reconnected, and the binding a user already saw working
// keeps working.
//
// The check runs through nameTable rather than a local map, because the table
// is the one thing that knows what a collision IS (see names.go). Each
// candidate is tried on a fresh table rebuilt from the accepted bindings, so a
// rejected binding leaves nothing behind: already-accepted bindings are known
// disjoint, so only the candidate's own adds can fail.
func resolveNames(entries []bindingTools) (accepted []bindingTools, rejected []rejection) {
	accepted = make([]bindingTools, 0, len(entries))
	for _, e := range entries {
		next := newNameTable()
		err := fill(next, accepted)
		if err == nil {
			err = fill(next, []bindingTools{e})
		}
		if err != nil {
			rejected = append(rejected, rejection{binding: e.name, err: err})
			continue
		}
		accepted = append(accepted, e)
	}
	return accepted, rejected
}

// fill adds every tool of every entry to t, stopping at the first refusal.
func fill(t *nameTable, entries []bindingTools) error {
	for _, e := range entries {
		for _, spec := range e.cat.Tools {
			if err := t.add(e.name, spec); err != nil {
				return err
			}
		}
	}
	return nil
}

// bundle builds one (binding, adopted generation) definition.
//
// One definition per binding-generation rather than one per tool is what makes
// a replacement a generation swap: the whole binding's toolset moves together,
// and a Loop can never end up holding two tools from two generations of one
// server.
//
// The Definition's runtime Requirements are zero: an MCP tool needs nothing
// from the Loop's runtime bindings — its route, its credentials and its limits
// came from the binding's definition. It therefore cannot escalate a Loop's
// privileges even if the Loop has a workspace, a shell, or a delegate
// controller bound. (This is distinct from the per-call tool.invoke requirement
// each tool emits at PrepareCall time, which is the access the CALL needs, not a
// Loop capability the DEFINITION binds.)
func (m *Manager) bundle(e bindingTools) tool.Definition {
	names := make([]string, 0, len(e.cat.Tools))
	tools := make([]tool.InvokableTool, 0, len(e.cat.Tools))
	for _, spec := range e.cat.Tools {
		names = append(names, spec.ModelName)
		tools = append(tools, &adaptedTool{
			binding:      e.name,
			rawTool:      spec.RawName,
			modelName:    spec.ModelName,
			desc:         spec.Description,
			schema:       spec.InputSchema,
			generation:   e.cat.Generation,
			schemaDigest: spec.InputSchemaDigest,
			route:        e.route,
			identity:     ToolInvokeIdentity(e.name, spec.RawName),
		})
	}
	// The tools are already built: they are immutable values over a live route,
	// with nothing per-Loop to bind and no I/O to do. The factory hands back the
	// same set for every Build, and a fresh slice each time so a caller cannot
	// mutate another Loop's toolset through a retained header.
	return tool.NewBundleDefinition(bundleName(e.name, e.cat.Generation), names, 0,
		func(context.Context, tool.Bindings) ([]tool.InvokableTool, error) {
			return append([]tool.InvokableTool(nil), tools...), nil
		})
}

// bundleName identifies a definition in a build failure or a log line. It
// carries the generation because "which catalog was this cut from" is the first
// question an operator asks about an external toolset.
func bundleName(binding string, generation uint64) string {
	return fmt.Sprintf("mcp:%s@%d", binding, generation)
}
