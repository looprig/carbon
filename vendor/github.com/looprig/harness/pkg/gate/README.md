# pkg/gate

`pkg/gate` is the harness's **generic access-decision layer**. It defines the
durable domain envelope for human- and policy-resolved gates and the generic
three-state access evaluator that decides one typed prepared request per tool
call.

It is deliberately **generic**. `pkg/gate`:

- does **not** parse tool arguments — tools prepare typed requests
  (`tool.CallPreparer` in `pkg/tool`) before evaluation ever starts;
- does **not** define sandbox profiles and does **not** import a sandbox or any
  other enforcement package — the access, rule, and grant seams are structural,
  built-in-typed interfaces an enforcing consumer satisfies without importing
  harness;
- does **not** implement a permission-file format — durable rule matching and
  persistence are consumer-provided (`RuleMatcher`, `RuleWriter`).

## What is gate?

A `gate.Evaluator` decides one prepared `tool.Request` per tool call:

- **`Deny`** — the call is not executed; the model sees a paired
  permission-denied tool result.
- **`Gated`** — the call needs approval. The whole unmet set is resolved by
  **one combined prompt** with exactly three actions: `Approve` (once),
  `Approve always for this workspace` (persists the displayed candidates
  atomically before any grant is minted), `Deny` (no error; nothing minted).
- **`Allow`** — the call runs; no grant token is needed from this layer.

Construction explicitly selects the interaction mode:

- `NewInteractiveEvaluator(bindings, matcher, approver, writer, issuer)` —
  requires both an `Approver` and a durable `RuleWriter`, so all three
  approval actions are honest.
- `NewHeadlessEvaluator(bindings, matcher, issuer)` — accepts neither,
  never prompts, and resolves an unmet gated requirement as a typed
  approval-required denial (`EvaluationApprovalRequired`).

The package also defines the durable **gate envelope** (`Gate`, `Payload`,
`GateResponse`, `GateRoute`, `ID`, `Answer`, `CloseReason`) used by every
kind of host-facing gate — permission, ask-user, form, open-URL — and
the response routing the session uses to deliver a human's reply back to
the loop that opened one.

## Boundary of responsibilities

| Concern | Owner |
| --- | --- |
| Argument decoding, normalization (commands, URLs, paths), canonical resource identity, per-call artifacts | The tool, via `tool.CallPreparer.PrepareCall` (`pkg/tool`) |
| Three-state decision (`Deny`/`Gated`/`Allow`), deny-before-allow ordering, one combined approval, response transport, redacted audit | `pkg/gate` (this package) |
| Access profiles, OS confinement, grant-token minting and enforcement | The enforcing consumer (e.g. a sandbox module), behind the structural `AccessSource` and `GrantIssuer` seams |
| Durable rule storage and matching (whatever file or store format) | The consumer, behind `RuleMatcher` / `RuleWriter` |

Invalid tool input fails during preparation and never reaches the evaluator;
`tool.ValidateRequest` re-checks every prepared-request invariant at the start
of evaluation and at both durable codec boundaries.

## Typed prepared requests

A tool call is evaluated as one `tool.Request`: the tool name, a bounded
display summary, optional execution-binding fields (`ExecutionID`, `Command`,
`WorkingDirectory`, `ExpiresAtUnixMilli` — required exactly when any
requirement requests a grant), and a set of `tool.Requirement` values. Each
requirement carries:

- `Kind` — routed to exactly one `AccessSource` via `AccessBindings`;
- `Scope` — used only for access routing;
- `Match` — used only for stored-rule matching;
- `Description` — used only for bounded display and audit;
- an optional `GrantClass`/`GrantTarget` pair requesting one post-decision
  execution-bound grant;
- `Candidates` — the exact reusable allow rules displayed to the user and
  offered for durable persistence.

The access ABI is versioned (`CurrentAccessVersion`, currently 1). Sources
return the raw `uint8` states `AccessDeny`/`AccessGated`/`AccessAllow`;
unknown kinds, unknown values, source errors, and version mismatches all fail
closed as typed `AccessError` values.

## Evaluator lifecycle

Construction explicitly selects the interaction mode:

- `NewInteractiveEvaluator(bindings, matcher, approver, writer, issuer)` —
  requires both an `Approver` and a durable `RuleWriter`, so all three
  approval actions are honest.
- `NewHeadlessEvaluator(bindings, matcher, issuer)` — accepts neither, never
  prompts, and resolves an unmet gated requirement as a typed
  approval-required denial (`EvaluationApprovalRequired`).

`Authorize(ctx, request)` is the single entry: it runs `Evaluate`, opens at
most one combined approval (interactive construction only, and only when gated
requirements remain unmet), applies the chosen action via `Resolve`, and mints
fresh execution-bound grants for the approved call.

`Evaluate` applies the generic order:

1. **Configured access first.** Every requirement is routed to its sole bound
   source. Any `Deny` short-circuits: the evaluation returns the combined
   denied set and nothing further is consulted. `Allow` needs no grant token
   from this layer; `Gated` continues.
2. **Every stored deny before any allow.** Each gated requirement is checked
   against `RuleMatcher.MatchesDeny`; any match denies the call.
3. **Stored allows.** A gated requirement matched by `MatchesAllow` is met; the
   rest form **one combined unmet set** together with every displayed reusable
   candidate.

`Resolve` applies exactly one of the three approval actions:

- `Approve` — approve once; nothing is persisted.
- `Approve always for this workspace` — atomically persists the entire
  displayed candidate batch in one `RuleWriter.WriteRules` call *before* any
  grant is minted; a persistence failure blocks execution.
- `Deny` — an unapproved `Resolution` with no error; nothing is minted.

Every dependency failure (rule match, approver, writer, issuer) is a typed,
fail-closed `EvaluationError`. An unapproved `Resolution` with a nil error is a
policy or user denial, not a fault.

## One combined prompt

Multiple gated requirements never produce serial prompts. The whole unmet set
travels in one `ApprovalPrompt{Request, Unmet, Candidates}`, resolved by the
consumer's `Approver` to exactly one `ApprovalAction`. `ApprovalControls()`
returns the exact, complete control set — there is no session scope,
user-global scope, persistent-deny action, or second capability prompt. A
partial saved approval yields one prompt containing only the still-unmet
requirements.

Inside a running loop, `loop.GateApprover()` is the `Approver` a consumer
passes to interactive construction: it resolves each combined prompt through
the live loop's per-call approval capability (installed on ctx by the runner)
and fails closed outside a live loop call.

## Response routing

An interactive approval travels as a durable permission gate:

- The runner opens a `Gate` of `KindPermission` whose private
  `PermissionPayload` carries the displayed `tool.Request`. The payload is
  validated at **both** codec boundaries (`tool.ValidateRequest` on marshal,
  the strict `DecodeRequest` on unmarshal), so a malformed or token-bearing
  record can neither be journaled nor restored.
- The session routes the human's reply by `Route`
  (`GateID`/`LoopID`/`ToolExecutionID`): an approve action becomes
  `command.ApproveToolCall` carrying the exact `gate.ApprovalAction`, a deny
  becomes `command.DenyToolCall`. `ParseApprovalAction` is the single
  validation source shared by the strict wire decoder
  (`DecodeApprovalAction`) and the session route; anything but the three exact
  actions fails closed.
- The runner maps the routed command back to the action and hands it to
  `Resolve`.

Other gate kinds (`KindAskUser`, `KindForm`, `KindOpenURL`) share the same
envelope, payload codec, and response routing; see the type docs in
`payload.go`, `form.go`, and `prompt.go`.

## Audit behavior

Durable audit records are **descriptions only, never tokens**:

- `PermissionAudit` stores the bounded display descriptions of the approved
  requirements and — only for a workspace approval, which persists them — of
  the displayed reusable candidates. Never grant tokens, token material, or
  raw tool arguments.
- `Resolution.Grants` is excluded from JSON (`json:"-"`): minted tokens travel
  only through the prepared execution contract (`tool.PreparedCall`), never a
  prompt, display, journal, or audit payload.
- `RuleCandidate` contains no grant or token material; its
  `GrantClass`/`GrantTarget` describe only the structural enforcement contract
  a future match must preserve.

## Permission review

`pkg/gate` also owns the neutral, mechanism-level permission-review domain: a
durable, secret-free envelope a classifier's assessment travels through, and
the local policy that turns a validated assessment into a one-shot gate
approval. See
[`docs/plans/2026-07-27-permission-classifier-hustle-design.md`](../../docs/plans/2026-07-27-permission-classifier-hustle-design.md)
for the full design; this section documents the public contract.

The classifiers themselves — prompts, wire codecs, evidence-tool catalogs,
and evaluation corpus — live in the separate
[`github.com/looprig/classifiers`](https://github.com/looprig/classifiers)
module, never in Harness. `pkg/gate` never imports it. A consumer composes a
classifier (for example `commandsafety.New`) and registers it with
`rig.WithPermissionClassifiers`; zero registered classifiers preserves this
package's existing Deny/Gated/Allow behavior byte-for-byte — see
`pkg/rig/README.md` for the full composition and enable/disable story.

### Enable/disable

Permission review is off unless a consumer explicitly registers at least one
`PermissionClassifier` (`NewPermissionClassifierSet`) and pairs it with a
`PermissionReviewPolicy` via `rig.WithPermissionReviewPolicy` — there is no
global registry, no implicit default classifier, and no model-facing
enable/disable control. A rig with zero classifiers behaves exactly as it did
before this feature existed: every gated requirement waits on a human.

### Model capability requirements

A registered classifier's underlying model must support tool use, structured
output, and structured output combined with tool use (the classifier issues
zero or more ordinary evidence-tool calls before returning one strict
terminal structured result). A capability mismatch fails the review before
inference and — like every other review failure — leaves the human gate open;
it never blocks or denies the underlying tool call on its own.

### Evidence boundaries

A classifier that needs to gather evidence (read a file, check `git status`,
and so on) never gets ambient access. Consumer composition installs the
boundary explicitly through `rig.WithPermissionReviewEvidence(access,
containment, allowedKinds)`:

- **`EvidenceAccessEvaluator`** — the plain, non-interactive access seam
  (`AccessFor(tool.Requirement) (uint8, error)`) evidence calls are routed
  through. It never prompts, never touches stored rules, and never mints a
  grant.
- **`EvidenceContainmentVerifier`** — independently resolves every prepared
  evidence target (including symlinks) against an `EvidenceContainmentPolicy`
  (`ReadRoot`, `SecurityCeiling`) and rejects root escape or ambiguous scope.
  `SecurityCeiling` always comes from the one review's own frozen basis
  (`rig.WithPermissionReviewSecurityCeiling`), never a session-wide constant,
  so a later review in a long session is checked against its own current
  ceiling.
- **`allowedKinds`** — the exact `tool.Requirement.Kind` allowlist a
  classifier's evidence tools may use. An evidence call outside this set, an
  unknown access state, or any collaborator error fails closed.

A nil or typed-nil verifier, an invalid policy, or a verifier panic all fail
closed. Evidence tools never receive session, gate, rule, grant, mutation, or
delegation capabilities (design §13.1/§13.3).

### Observation recheck (TOCTOU)

Evidence gathering and gate claiming are not atomic: a target a classifier
observed (a file it stat'd, a git ref it resolved) can change between the
observation and the eventual auto-approval. `ObservationRequirement` and
`EvidenceObservationVerifier` (design §13.4) close that window:

- **`ObservationRequirement{Target, Token}`** — one canonical-identity/token
  pair a target-sensitive evidence tool recorded while it ran. `gate` never
  computes or interprets either field; both are entirely tool/consumer-owned.
- **`EvidenceObservationVerifier`** — the consumer-supplied, read-only recheck
  seam (`VerifyEvidenceObservations(ctx, EvidenceContainmentPolicy,
  []ObservationRequirement) error`), installed via
  `rig.WithPermissionReviewObservations(verifier)`. Immediately before a
  classifier-originated response claims the gate, every observation the
  contributing classifier(s) recorded is rechecked; a mismatch or
  unverifiable target makes the response stale — the human gate stays open,
  exactly like every other review failure below.

Unlike `WithPermissionReviewEvidence`, this option is optional even when
classifiers and evidence are both configured: a session with no
target-sensitive evidence tools has nothing to recheck. If a target-sensitive
evidence tool DOES record an observation and no verifier is configured, the
recheck fails closed (treated as a mismatch) rather than silently skipping —
see `rig.WithPermissionReviewObservations`'s doc comment for the full
reasoning. This mechanism narrows, but never replaces, the pre-existing
symlink-swap, containment, grant-target, and sandbox checks: the eventual
tool still consumes its own originally prepared artifact.

### Human fallback

This is the feature's core invariant: **every classifier outcome other than
an eligible `allowed` leaves the ordinary human gate exactly as open as it
would have been with zero classifiers configured.** `needs_human`,
`not_applicable`, `timed_out`, `failed`, `cancelled`, `stale`, a capability
mismatch, an evidence-policy violation, retry exhaustion, or any other
expected or unexpected review failure all resolve to the same place: the
human can still answer the same gate, at any point, including while a review
is in flight (see "One combined prompt" and "Response routing" above). A
classifier can only ever narrow to a single one-shot `Approve`; it cannot
deny, cannot persist a rule, cannot widen a security ceiling, and cannot stop
the human from answering first.

### Audit and privacy

`PermissionReviewStarted` and `PermissionReviewCompleted`
(`pkg/event/permission_review.go`) are enduring, secret-free audit events —
but both are `event.Internal`-visibility, so they must be published through
`Hub.PublishInternalEventChecked`, never the public-only
`PublishEventChecked` path (a session-fault bug from exactly that mismatch
was fixed during Phase 6; see the design's implementation-plan addenda for
the incident). They carry gate/tool-execution identity, classifier
name/revision, and — for `Completed` — the closed `ReviewStatus`, `ReviewRisk`,
`ReviewAuthorization`, categories, and `AutoApproved`. `ReviewStatusAllowed`
can never carry `ReviewRiskCritical`; that combination is a durably rejected,
globally impossible audit state.

These events deliberately exclude conversation text, commands and raw
arguments, file contents, evidence-tool output, prompt text, model output,
rationale, credentials, rule data, and grant material. An ephemeral, bounded,
sanitized rationale (`MaxPermissionReviewRationaleBytes`) may reach an
authorized live UI as a diagnostic; it is never journaled or replayed.

`gate.ResponseFromClassifier` is the `ResponseSource` kind stamped on a
classifier-originated `GateResolved`/`PermissionAudit` record. Only a private
session-runtime method can produce it — a public caller cannot select this
provenance — so audit and UI can always distinguish a human, timeout-policy,
or classifier approval without ever claiming a human acted when one did not.

### Policy tuning

`PermissionReviewPolicy` (`NewPermissionReviewPolicy` /
`DefaultPermissionReviewPolicy`) is the local, consumer-owned ceiling applied
after a classifier's result has already been validated: a maximum
auto-approvable risk, a per-risk minimum authorization floor, an absolute-human
category list (categories no authorization can override — `data_exfiltration`
and `prompt_injection` are absolute-human by design regardless of what a
consumer configures), and a material-context-truncation mask. A policy a
consumer builds can only be at least as restrictive as Harness's own hard
review ceiling; `NewPermissionReviewPolicy` rejects a looser one.

Independently, `rig.WithPermissionReviewLimits(rig.PermissionReviewLimits{...})`
tunes the turn- and session-scoped circuit breaker (design §18): 8
thresholds total (4 turn-scoped: `MaxConsecutiveNeedsHuman`,
`MaxInvalidOrFailed`, `MaxIdenticalSubjects`, `MaxStaleResponses`, plus
`InterruptOnTrip`; and the same 4 again, session-scoped, under `Session`).
Each defaults to `rig.DefaultPermissionReviewBreakerThreshold` (20) when
classifiers are configured but this option is never called. These are
operational tuning knobs, not behavioral identity, so they are deliberately
excluded from the rig fingerprint — two rigs that agree on classifiers and
policy but differ only in these thresholds compare equal.

Carbon's `internal/app/permission_review.go` is a complete real example:
it offers a Codex-compatible default policy and a strictly tighter
alternative (`PermissionReviewStrictPolicy`), never the reverse.

### Restore behavior

Rig identity (`pkg/rig/README.md`'s "Configuration fingerprint") folds in
ordered classifier names/revisions, definition descriptors, the local review
policy revision, and the evidence catalog. A `ConfigManifest.PermissionReviewConfigured`
bool tracks whether ANY permission review is configured at all, kept
deliberately separate from that opaque topology hash so drift assessment can
be directional: going from disabled to enabled on restore is a `DriftWarn`,
which `session.DefaultPolicyDecider` rejects — a session must never silently
start auto-reviewing gates that were 100% human-only when it was opened. A
rig accepts that transition only if its consumer opted in, either narrowly
via a custom `rig.WithRestoreDecider` that inspects the assessment and
accepts this dimension specifically, or with the older, deliberately blanket
`rig.WithAllowConfigMismatch()` (accepts every `Warn`-level drift, not just
this one — `session.RestoreDecider`'s doc marks it the deprecated
predecessor of `WithRestoreDecider`). Going from enabled to disabled is
narrowing and is unaffected (`DriftInfo`). A same-config restore, or an
identity change with review already enabled on both sides, is governed by
the ordinary `TopologyRev` comparison alone. Hustles themselves are never
restored: if the process exits mid-review, the gate restores as an ordinary
open human gate, no classifier response is synthesized, and no review is
rerun from guessed context (design §15).

### Evaluation workflow

`pkg/gate` and `pkg/hustle` do not run or score evaluations themselves — that
lives in the classifiers module's own evaluation corpus and deterministic
report runner (`commandsafety.Evaluate`), which exercises this package's real
`gate.BuildReviewContext`, `gate.NewPermissionReviewSubject`, and
`gate.EvaluatePermissionAssessment` against synthetic, versioned corpus
fixtures. See
[`classifiers/docs/evaluations/`](https://github.com/looprig/classifiers/tree/main/docs/evaluations)
for the corpus format, coverage requirements, and report shape (design
§22.6/§22.7).

## Example

This example is compiled and run as a doc test (`example_test.go`); keep the
two in sync.

```go
// staticAllow is a minimal AccessSource that allows every routed scope.
type staticAllow struct{}

func (staticAllow) AccessVersion() uint16 { return gate.CurrentAccessVersion }
func (staticAllow) AccessFor(kind, scope string) (uint8, error) {
	return gate.AccessAllow, nil
}

func Example() {
	evaluator, err := gate.NewHeadlessEvaluator(
		[]gate.AccessBinding{{Kind: "fs.read", Source: staticAllow{}}},
		nil, // no stored rules
		nil, // no grant issuer: no requirement below requests a grant
	)
	if err != nil {
		fmt.Println(err)
		return
	}
	resolution, err := evaluator.Authorize(context.Background(), tool.Request{
		ToolName: "Read",
		Requirements: []tool.Requirement{{
			Kind:        "fs.read",
			Match:       "Read(/repo/README.md)",
			Description: "Read /repo/README.md",
		}},
	})
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(resolution.Approved)
	// Output: true
}
```

## Sibling packages

- [`pkg/tool`](../tool/README.md) — `tool.Request`, `tool.Requirement`,
  `tool.RuleCandidate`, `tool.ValidateRequest`, and the `CallPreparer`
  boundary that produces the typed request the evaluator consumes.
- [`pkg/command`](../command/README.md) — `command.ApproveToolCall` /
  `command.DenyToolCall`, the routed wire forms of an `ApprovalAction`;
  `ParseApprovalAction` is the single validation source shared by the
  strict decoder here and the session route.
- [`pkg/event`](../event/README.md) — `event.PermissionRequested`
  carries the gate id the session uses to route a reply.
- [`pkg/loop`](../loop/README.md) — `loop.AccessGate` is the runner's view
  of an evaluator; `loop.GateApprover` is the `Approver` a live loop
  passes to interactive construction.
- [`pkg/hustle`](../hustle/README.md) — the bounded tool-using loop a
  permission classifier's evidence gathering runs inside.
- [`pkg/rig`](../rig/README.md) — `rig.WithPermissionClassifiers`,
  `rig.WithPermissionReviewPolicy`, `rig.WithPermissionReviewLimits`,
  `rig.WithPermissionReviewEvidence`, and
  `rig.WithPermissionReviewSecurityCeiling` compose everything in "Permission
  review" above into a running rig.
- `github.com/looprig/sandbox` — satisfies `AccessSource` /
  `GrantIssuer` with OS confinement. Harness never imports it.
- `github.com/looprig/classifiers` — the classifier product (prompts, wire
  codecs, evidence-tool catalogs, evaluation corpus) built on this package's
  public permission-review contracts. Harness never imports it.
