# pkg/rig

`pkg/rig` is the **composition root** for an agent runtime. A consumer
assembles a `*Rig` from loop definitions, hustle definitions, a session
store, primers, and a workspace placement; the `Rig` then creates and
restores live sessions.

It owns **design-time topology and lifecycle policy** — what loops exist,
what they're called, which one starts active, where the session is
persisted, where the workspace lives — while the live runtime behavior
lives behind the `pkg/session` contracts the `Rig` returns. Construction and
restoration of a session are owned exclusively here; nothing else in the
module can mint or resume one.

## What is rig?

A `Rig` is an immutable design-time assembly. `rig.Define(opts...)`
validates the options, freezes the assembly, and returns a `*Rig`. The
two lifecycle methods are the whole public surface:

- `rig.NewSession(ctx, opts...)` — bring up a brand-new live session.
  Optionally seed its workspace from a snapshot via `WithSeedSnapshot`.
- `rig.RestoreSession(ctx, id)` — rebuild a prior session from its
  durable journal by id.

Both return a `session.SessionController`, which embeds `session.Session`
(the data plane) and adds the trusted policy/lifecycle methods
(`SetActiveLoop`, `LoopController`, `CheckpointWorkspace`,
`RestoreWorkspace`, `Shutdown`).

## How to use

```go
reviewPolicy, err := gate.DefaultPermissionReviewPolicy("review-policy-v1")
if err != nil { /* ... */ }

r, err := rig.Define(
    rig.WithLoops(operatorLoop, reviewerLoop),
    rig.WithPrimers("operator"),
    rig.WithSessionStore(sessionStore),
    rig.WithExclusiveWorkspace(workspaceStore, "/repo", leaser),
    rig.WithHustles(/* ...optional hustle.Definition values... */...),
    rig.WithHustleLimits(rig.HustleLimits{ /* ... */ }),
    rig.WithPermissionClassifiers(permissionClassifiers),
    rig.WithPermissionReviewPolicy(reviewPolicy),
    rig.WithPermissionReviewSecurityCeiling("consumer-access-profile/v1"),
    // Optional: bounded evidence-tool access for classifiers that gather
    // read-only evidence. Required whenever any registered classifier's
    // definition declares evidence tools — see pkg/gate/README.md#evidence-boundaries.
    rig.WithPermissionReviewEvidence(evidenceAccess, evidenceContainment, allowedEvidenceKinds),
    // Optional: TOCTOU recheck (design §13.4) for classifiers whose evidence
    // tools observe a specific target's identity/state. Only meaningful
    // paired with WithPermissionReviewEvidence; harmless to omit if no
    // registered classifier's evidence tools are target-sensitive.
    rig.WithPermissionReviewObservations(observationVerifier),
    // Optional: circuit-breaker thresholds (design §18). Defaults every one
    // of 8 turn+session counters to rig.DefaultPermissionReviewBreakerThreshold
    // (20) when omitted but classifiers are configured.
    rig.WithPermissionReviewLimits(rig.PermissionReviewLimits{ /* ... */ }),
    rig.WithForeignBuilders(/* ...foreign.Builder for codex/claude... */...),
    rig.WithGateCaps(rig.GateCaps{MaxOpen: 16, MaxTimeout: 30*time.Second}),
    rig.WithDelegationLimits(rig.DelegationLimits{Depth: 4, Quota: 32}),
    rig.WithRestoreDecider(session.DefaultPolicyDecider{}),
)
if err != nil { /* ... */ }

ctx := context.Background()
session, err := r.NewSession(ctx)
if err != nil { /* ... */ }
defer session.Shutdown(ctx)
```

Restore is the same shape with the session id:

```go
session, err := r.RestoreSession(ctx, priorSessionID)
```

`rig.WithPermissionClassifiers`, `WithPermissionReviewPolicy`,
`WithPermissionReviewEvidence`, `WithPermissionReviewObservations`, and
`WithPermissionReviewSecurityCeiling` are all supplied at `rig.Define` time
above, not per restore — restore itself takes no options. Omitting every one
of them (the zero-classifier rig) is how you disable permission review
entirely: it preserves the pre-classifier gate behavior byte-for-byte, with
no separate on/off flag to flip. See
[`pkg/gate/README.md`](../gate/README.md#permission-review) for the full
enable/disable, evidence-boundary, observation-recheck (TOCTOU),
human-fallback, audit/privacy, policy-tuning, evaluation-workflow, and
restore-behavior story; that package — not this one — owns the neutral
review domain these options configure.

### Restore and a changed permission-review configuration

Restoring a session whose rig now has permission review configured
differently than when it was opened follows the ordinary configuration-drift
policy above, with one extra rule specific to this feature: turning
classifiers on where the session was originally opened with none configured
is always a rejected `DriftWarn`, never a silent auto-accept, even though the
rest of that restore's drift might otherwise be all-`Info`. A rig only
resumes with review newly enabled if it explicitly opted in — with
`rig.WithRestoreDecider` (preferred: inspect the assessment and accept this
dimension specifically) or the older, blanket `rig.WithAllowConfigMismatch()`
(accepts every `Warn`-level drift, not just this one). Turning classifiers
off, or restoring with the same or a differently-identified classifier set
that was already enabled before, is unaffected by this rule.

## Sibling packages

- [`pkg/loop`](../loop/README.md) — `loop.Definition` values you pass to
  `rig.WithLoops`.
- [`pkg/session`](../session/README.md) — the `Session` /
  `SessionController` interfaces the lifecycle returns.
- [`pkg/sessionstore`](../sessionstore/README.md) — the
  `*sessionstore.Store` you pass to `rig.WithSessionStore`.
- [`pkg/workspacestore`](../workspacestore/README.md) — the
  `*workspacestore.Store` you pass to a workspace placement.
- [`pkg/hustle`](../hustle/README.md) — `hustle.Definition` values you
  pass to `rig.WithHustles`.
- [`pkg/foreign`](../foreign/README.md) — `foreign.Builder` values you
  pass to `rig.WithForeignBuilders` for codex/claude backends.
- [`pkg/gate`](../gate/README.md) — the `gate.Evaluator` you bind into
  each `loop.Definition` via `loop.WithAccessGate`.

## How it is designed

`pkg/rig` is intentionally thin. It validates options, freezes the
assembly, and delegates to the private `internal/sessionruntime`
coordinator, which owns the live loops, hub, journal, and workspace
lifecycle.

```
        rig.Define(opts...)
                │
                │ validate + freeze
                ▼
        *rig.Rig ──► internal/sessionruntime.Lifecycle
                            │
                            │ NewSession / RestoreSession
                            ▼
                     internal/sessionruntime.Session
                       │  │  │  │
                       │  │  │  └──► pkg/workspacestore (workspace snapshots)
                       │  │  └──────► pkg/sessionstore  (journal + catalog)
                       │  └─────────► pkg/hub           (event fan-in)
                       └────────────► internal/loopruntime (loop actors)
                                            │
                                            ▼
                                       pkg/session contracts returned to caller
```

### Validation at the boundary

`Define` enforces the invariants of a valid rig before any session is
created:

- At least one loop and at least one primer; the active primer must be a
  registered loop name.
- Loop names are unique; the active primer is the only one if exactly one
  loop is supplied.
- Every delegate a loop declares is itself a registered loop.
- A `*sessionstore.Store` is required; workspace placement is optional
  but at most one placement may be configured.
- Hustle lane bounds are within `MaxHustleQueued`; gate caps are positive.
- Permission classifiers are supplied as one validated, ordered
  `gate.PermissionClassifierSet` and are paired with a canonical local review
  policy revision. Supplying only one half is rejected. Their frozen
  definitions are automatically registered as blocking Hustles, so
  `WithHustleLimits` is required and consumers must not also pass those same
  definitions to `WithHustles`.
- Foreign builders and restore decider are optional with fail-secure
  defaults.

A bad configuration fails closed at `Define` rather than at session
construction.

### Configuration fingerprint

`Define` computes an immutable `InitialFingerprint` for each loop (model,
effective system, tool names) so the rig can stamp and compare
compatibility before any runtime factories execute. At restore time the
rig runs the configured `RestoreDecider` against a `DriftAssessment` and
records the decision as a durable `ConfigurationAdopted`; the default
`DefaultPolicyDecider` accepts only when every change is `Info` and
rejects when any is `Warn`.

Permission review extends the topology identity with the local review-policy
revision and the classifiers in registration order. Each classifier row carries
only frozen, secret-free identity: classifier name and revision, its complete
definition digest, structured-output digest/revision, evidence definition and
produced-name digests, and every evidence-loop bound. Classifier order is
significant because combination is ordered. Evidence policies accept only
sealed `tool.NewEvidenceDefinition` definitions with frozen `ToolInfo`
metadata. Their factories use `tool.EvidenceFactoryBindings`, whose complete
public capability surface is the invocation session/loop identity and an
optional root-only `tool.ReadWorkspaceBinding`; generic workspace mutation,
observations, delegation, gate/grant/control state, and extra tools are not in
the factory API. Canonical static descriptions and compact portable schemas contribute
to the evidence catalog digest, so either can change topology identity before a
session is persisted or restored. The fingerprint stores only those versioned
digests: raw prompts, schemas, descriptions, model clients and credentials,
workspace paths, live review subjects, and runtime-bound tool objects are never
serialized into it. At concrete binding, each tool's twice-read metadata must
exactly match the frozen static name, description, and schema; drift fails
closed. Automatic classifier-definition registration deliberately routes this
through the same Hustle binding path as every other evidence-enabled definition.

Omitting both permission-review options preserves the pre-classifier
fingerprint and gate behavior byte-for-byte. There is no implicit classifier
registry or default review policy.
