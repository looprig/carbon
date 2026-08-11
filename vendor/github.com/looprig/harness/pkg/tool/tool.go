// Package tool defines the dependency-free contract surface for the tools
// subsystem: the BaseTool/InvokableTool interfaces every tool implements, the
// ToolResult value tools return, the tool-owned preparation boundary
// (CallPreparer, Request, Requirement, RuleCandidate — see preparation.go), and
// the optional capability interfaces the runner probes for via type assertion.
// It imports only core packages (plus stdlib); it must never import pkg/loop,
// the runtime internals, or a concrete tools module, so all of them can depend
// on it without a cycle.
//
// Ownership: tools own preparation — decoding untrusted arguments, normalizing
// commands/URLs/paths, resolving canonical resource identities — and produce
// the typed prepared Request. The three-state Deny/Gated/Allow decision, the
// single combined approval, and response routing belong to pkg/gate; sandbox
// profiles and OS enforcement belong to the enforcing consumer behind
// structural seams; durable rule persistence is consumer-provided.
package tool

import (
	"context"
	"encoding/json"

	"github.com/looprig/core/content"
	"github.com/looprig/core/uuid"
)

// ToolInfo is a tool's self-description. Schema is a JSON Schema describing the
// tool's argument object; it maps 1:1 to inference.Tool.Schema.
type ToolInfo struct {
	Name   string
	Desc   string
	Schema json.RawMessage
}

// BaseTool is the minimal contract: every tool can describe itself. It is never
// widened — new behavior is added via separate optional capability interfaces,
// never folded into BaseTool (design Rule 1 / Open-Closed).
type BaseTool interface {
	Info(ctx context.Context) (*ToolInfo, error)
}

// InvokableTool is a BaseTool that can be executed with JSON-encoded arguments.
// argsJSON is the untrusted, model-supplied argument object; the implementation
// is responsible for parsing and validating it.
type InvokableTool interface {
	BaseTool
	InvokableRun(ctx context.Context, argsJSON string) (*ToolResult, error)
}

// ToolResult is the value an InvokableTool returns. Content must hold at least
// one block; the runner injects an "error: empty result" block when it is nil
// or empty. There is intentionally no Terminate field in v1 — a turn ends only
// via the model emitting no more tool calls or an abort.
type ToolResult struct {
	Content []content.Block
}

// TextResult is the convenience constructor for the common single-text-block
// result. It always returns a non-nil *ToolResult holding exactly one
// *content.TextBlock, even for the empty string.
func TextResult(s string) *ToolResult {
	return &ToolResult{Content: []content.Block{&content.TextBlock{Text: s}}}
}

// Optional capability interfaces. Each is a separate, focused interface (never
// folded into BaseTool — design Rule 1 / Open-Closed + Interface Segregation).
// The runner probes for each via type assertion; a tool implementing none of
// them is still a fully valid InvokableTool.

// Sequential is implemented by tools that must not run concurrently with other
// tool calls in the same batch. Sequential() reports whether this call must be
// serialized.
type Sequential interface {
	Sequential() bool
}

// PreparedArtifact is the opaque, per-call artifact a CallPreparer produces — read
// by the producing tool at both PrepareCall and InvokableRun time, opaque to the
// runner. Sealed via an unexported marker so only deliberate types satisfy it (no
// bare any): a type in another package cannot supply the unexported method, so it
// cannot masquerade as a PreparedArtifact. Concrete artifacts therefore live in
// this package alongside the seal.
type PreparedArtifact interface{ preparedArtifact() }

// TokenArtifact is the minimal concrete PreparedArtifact: it carries a single
// opaque Token (e.g. a content hash or a callID-bound nonce) that the producing
// tool reads at both PrepareCall and InvokableRun time. It is the simplest
// artifact a CallPreparer can return when the bound value is a single string;
// richer CallPreparers declare their own concrete artifact type in this package.
type TokenArtifact struct{ Token string }

func (TokenArtifact) preparedArtifact() {}

// CallPreparer is the tool-owned preparation boundary: decode and validate the
// untrusted argsJSON, normalize commands/URLs/paths, resolve canonical resource
// identities, and produce the typed access Request for this call plus an
// optional opaque per-call artifact the tool reads back at execution time.
//
// The runner mints executionID once per call and invokes PrepareCall exactly
// once, before any permission evaluation; invalid input fails here and never
// reaches the gate. A pure tool returns an empty Request (no requirements). A
// tool that does NOT implement CallPreparer is treated as an unprepared
// effectful tool and fails closed: the call is never evaluated or executed.
type CallPreparer interface {
	PrepareCall(ctx context.Context, executionID uuid.UUID, argsJSON string) (Request, PreparedArtifact, error)
}

// PreparedCall is the prepared execution contract for one tool call: the
// minted execution ID, the validated typed Request, the tool's opaque per-call
// artifact, and — after the combined gate resolves — the fresh execution-bound
// grant tokens issued for THIS call. Tokens travel only here, never in an
// ambient grant context, a prompt, a journal, or an audit record. The Grants
// slice is owned by the runner; readers must not mutate it.
type PreparedCall struct {
	ExecutionID uuid.UUID
	Request     Request
	Artifact    PreparedArtifact
	Grants      []string
}

// Auditable is implemented by tools that can emit a redacted, length-capped
// one-line summary of a call for the ToolCallStarted audit event. The summary
// must never contain secrets, full file contents, headers, or request bodies.
type Auditable interface {
	AuditSummary(argsJSON string) string
}

// WriteTarget lets the runner group same-path mutations without importing the
// tools package: a write tool returns its resolved write path as key with
// ok=true, and the runner serializes calls sharing a key. ok=false means the
// call is not a write (no serialization). A non-nil err (e.g. unparseable args)
// is treated like invalid args: tool-result error, not executed, not grouped.
type WriteTarget interface {
	WriteTarget(argsJSON string) (key string, ok bool, err error)
}

// EvidenceObservation is an optional capability (probed by type assertion,
// mirroring Sequential/Auditable/WriteTarget) implemented by a
// target-sensitive evidence tool that can derive the canonical-identity/token
// pair it observed for one completed call from that call's own prepared
// Request and produced ToolResult (design §13.4, TOCTOU). It is probed only
// AFTER a successful InvokableRun, from the evidence runtime's per-call loop
// (internal/hustleruntime/evidence_runner.go) — never before, and never for a
// call that failed.
//
// Harness never computes, interprets, or canonicalizes target or token
// itself: deriving canonical identity and minting a stable token from
// observed metadata is entirely tool/consumer-owned, the same reasoning
// gate.EvidenceContainmentVerifier's own doc comment gives for why Harness
// does not own path canonicalization. This package cannot name that gate
// type directly (pkg/gate imports pkg/tool, so the reverse would cycle),
// which is why the method returns plain strings rather than a
// gate.ObservationRequirement — the evidence runtime, which already imports
// both packages, does that conversion (and validation) at the call site.
//
// ok=false means this particular call made no target-sensitive observation
// (for example a glob/grep-style tool that matched many candidates rather
// than resolving one canonical target) — nothing is recorded, and evidence
// execution and authorization are completely unaffected either way. A tool
// implementing none of this capability simply never contributes an
// observation for any of its calls; it is still a fully valid evidence tool.
type EvidenceObservation interface {
	ObservedRequirement(request Request, result *ToolResult) (target string, token string, ok bool)
}

// ToolExecuteFunc is the terminal/next step in a middleware chain: it runs the
// tool against argsJSON and returns its result.
type ToolExecuteFunc func(ctx context.Context, argsJSON string) (*ToolResult, error)

// ToolMiddleware wraps tool execution. It receives the tool, the (untrusted)
// argsJSON, and the next step in the chain; it may run logic before/after and
// must call next to proceed (or short-circuit by not calling it).
type ToolMiddleware func(ctx context.Context, t InvokableTool, argsJSON string, next ToolExecuteFunc) (*ToolResult, error)

// Command/argv runner-injection seam. These interfaces let an OPTIONAL OS
// sandbox wrap a tool's command execution WITHOUT harness ever importing the
// sandbox: their signatures are stdlib-only, so the sandbox module's Executor
// satisfies them structurally (no import either way). The coupling is
// deliberately structural (SPEC §10.1). A nil runner means direct execution —
// the bare-harness default — so injecting one is purely additive.

// CommandRunner runs a shell command in a confined environment. A nil runner
// means direct execution (bare-harness default). Implemented by the sandbox
// Executor; harness never imports sandbox — the coupling is structural (§10.1).
type CommandRunner interface {
	RunCommand(ctx context.Context, dir, command string) (output []byte, exitCode int, err error)
}

// ArgvRunner runs a direct argv (no shell interpretation) — used by Grep, whose
// rg invocation is already a safe argv and must not gain a shell.
type ArgvRunner interface {
	RunArgv(ctx context.Context, dir string, argv []string) (output []byte, exitCode int, err error)
}

// GrantedRunner is an optional capability (probed by type assertion) for running
// a command with escalation grant tokens. Wiring the grant flow is a later task;
// the interface lives here with the others.
type GrantedRunner interface {
	RunCommandWithGrants(ctx context.Context, dir, command string, grants []string) (output []byte, exitCode int, err error)
}
