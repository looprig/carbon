# Carbon: MCP servers via mcp.json, configurable home, and permission-review enablement

Date: 2026-08-05
Status: Approved design (revised after independent code-verified review, same day), not yet implemented
Scope: carbon only. No harness, mcp, or classifiers module changes are required.

## Summary

Three additions to Carbon's composition boundary:

1. **MCP servers** configured in an operator-managed `<home>/mcp.json` (exact
   Claude Code `mcpServers` schema plus one looprig extension field), assembled
   through the existing `github.com/looprig/mcp` module (`pkg/harness` Manager,
   Bindings, Adopter, transports).
2. **`Config.HomeDir`** — the `~/.looprig` base directory becomes a field on
   Carbon's Go `Config`. No CLI flag, no environment variable.
3. **Permission-classifier enablement** via an optional top-level
   `permission_review` section in `models.json`; presence enables review.

Verified integration facts this design rests on:

- harness already exposes both seams the mcp Adopter needs:
  `loop.ExternalToolInstaller` (`harness/pkg/loop/controller.go`) and
  `LoopController(uuid.UUID)` on `session.SessionController`
  (`harness/pkg/session/session.go:84`).
- The mcp module's tool adapter already carries the gate identity
  `mcp:<binding>:<raw-tool>` under the `tool.invoke` capability
  (`mcp/pkg/harness/tools.go` `ToolInvokeIdentity`), which is the same
  capability kind Carbon's product access source already binds
  (`capabilityToolInvoke` in `internal/app/access.go`).
- Transport factories exist for all three transports:
  `transport/stdio.New`, `transport/streamablehttp.New`, `transport/sse.New`,
  each returning a `client.TransportFactory`.
- `mcpharness.Deps.SessionID` is optional by design for exactly our case: an
  application that folds MCP identity into the config fingerprint discovers
  servers before the Session exists, leaves SessionID zero, and calls
  `BindSession` after `rig.NewSession` (`mcp/pkg/harness/deps.go`,
  `attach.go`).
- harness's rig fingerprint has a purpose-built seam for external capability
  identity: `rig.ConfigFingerprintFields.ExternalCapabilityRev`
  (`harness/pkg/rig/fingerprint.go:37-45`), and `mcp/pkg/harness/attach.go`
  documents the intended flow: `mgr.ConfigDigest()` → `ExternalCapabilityRev`
  → `rig.NewSession` → `BindSession`. This design uses that seam (§1.4).
- `newProductAccessSource` already answers `AccessGated` for **any** non-empty
  `tool.invoke` scope (`internal/app/access.go:180-190`) — no per-tool
  allow-list — so `mcp:<binding>:<tool>` identities get "ask" today with no
  change. A pinned test keeps it that way.

## Decisions (settled with the user)

- `mcp.json` uses the **exact Claude Code `mcpServers` format** so entries copy
  verbatim from a Claude Code config, plus one looprig-only optional field
  `roles` per server.
- Default tool visibility is **all three roles** (planner, builder, reviewer);
  `roles` narrows it per server.
- The config folder is **machine-wide only** in v1. No workspace-level
  `mcp.json`. (A committed per-repo MCP config is a supply-chain surface —
  cloning a malicious repo must not add servers; deferred, see Out of scope.)
- Home configurability is **`Config.HomeDir` only** — programmatic, on
  Carbon's `Config`. No flag, no env var. Empty means `~/.looprig`.
- Permission review is enabled by the **presence of a `permission_review`
  section in `models.json`** — operator-managed, machine-wide, no CLI flag.

## Part 1 — MCP servers

### 1.1 Config file: `<home>/mcp.json`

```json
{
  "mcpServers": {
    "context7": {
      "type": "http",
      "url": "https://mcp.context7.com/mcp",
      "headers": { "CONTEXT7_API_KEY": "..." }
    },
    "docs-local": {
      "type": "stdio",
      "command": "npx",
      "args": ["-y", "@upstash/context7-mcp"],
      "env": { "FOO": "bar" },
      "roles": ["planner", "builder"]
    }
  }
}
```

Schema, decoded strictly (`DisallowUnknownFields` at every level, duplicate-key
rejection matching `modelconfig.go`):

- Top level: exactly one key, `mcpServers`, a map of binding name → server.
  Binding names must satisfy `mcp/pkg/client.Name` validation (they become the
  `mcp__<binding>__<tool>` prefix and the `mcp:<binding>:<tool>` permission
  identity).
- Per server:
  - `type` — `"stdio"`, `"http"`, or `"sse"`. Optional, Claude-compatible
    inference when omitted: `command` present → `stdio`; `url` present →
    `http`.
  - stdio: `command` (required), `args` (optional), `env` (optional map).
  - http/sse: `url` (required, must parse as an absolute `https` or
    loopback-`http` URL), `headers` (optional map).
  - `roles` — looprig extension. Optional list drawn from
    `planner|builder|reviewer`; empty/absent means all three. Unknown role
    names are errors.
- Cross-field violations fail closed with typed errors naming the binding
  (never echoing header or env values), in the style of
  `*ModelConfigError`.

File hygiene mirrors `models.json` exactly (headers and env may carry
credentials): ≤ 1 MiB, regular file, no symlink, owner-only `0600` on Unix,
read once at the composition boundary, never written by Carbon. An **absent
file disables the feature** — zero MCP assembly, byte-for-byte identical rig
to today. A present-but-invalid file fails session construction (fail closed,
consistent with `models.json` handling).

Loader lives in a new `internal/app/mcpconfig.go`; it produces a validated,
immutable `[]mcpServerSpec` (name, transport kind, transport config, role
set).

### 1.2 Assembly

New `internal/app/mcp.go`, composed inside `openRuntimeAgent` (shared by new,
restore, interactive, and headless paths, like `sessionAccess`):

1. **Transports.** Each spec builds its `client.TransportFactory` via
   `transport/stdio.New` / `transport/streamablehttp.New` / `transport/sse.New`
   and wraps it in a `client.Definition` whose `Name` equals the binding name.
   - *stdio env baseline.* `stdio.Config.Env` is an `EnvAllowlist{Vars
     []stdio.Var, PassThrough []string}` built from nothing — the zero value
     gives the child an **empty** environment, unlike Claude Code's overlay
     semantics. The loader therefore constructs it as: `PassThrough` of a
     fixed baseline (`PATH`, `HOME`, `TMPDIR`, `LANG`, `LC_ALL` when present)
     plus explicit `Vars` entries (sorted by name for deterministic
     construction — `spec.env` is a Go map) built from the config's `env`
     map. Documented divergence: variables outside the baseline are NOT
     inherited; add them to `env` explicitly.
   - *HTTP client.* The HTTP transports refuse an injected `http.Client` with
     a non-zero `Timeout` (it would sever SSE streams), so the session's
     `newHTTPClient` is NOT shared. Leave `HTTPClient` nil and rely on the
     transports' own `Timeouts` struct (every wait individually defaulted),
     which satisfies the no-unbounded-blocking rule. `Headers` (`http`/`sse`)
     are built from the config's `headers` map via `auth.NewHeader` (its
     fields are private), sorted by name for the same determinism reason.
   - *SSE compatibility profile.* `client.Definition.Validate` (via
     `checkTransportCompat`) unconditionally rejects a `"sse"`-kind
     transport under a zero/`ProfileDefault` `Compat` — legacy SSE is
     deliberately excluded from the default profile so a binding never
     acquires it silently. Since Part 1.1 already requires `type: "sse"` to
     be **stated explicitly** (never inferred), an `mcp.json` entry naming it
     already is that deliberate, on-purpose choice, so an `"sse"`-kind
     `Definition` sets `Compat: client.ProfileLegacy` (`ProfileDefault` plus
     `TolerateLegacySSE`, nothing else); `stdio`/`http` keep the zero value.
     Found and fixed during Task 8's implementation — omitting this makes
     every `"sse"` spec fail `Binding.Validate` unconditionally.
2. **Bindings.** One `mcpharness.Binding` per server: `Scope: ScopeSession`,
   `Required: false`, `Visibility = mcpharness.Named(roles...)` — visibility
   selects by **loop name** (`"planner"`, `"builder"`, `"reviewer"`, the
   `internal/catalog` identities), since loop IDs are minted by the session
   and do not exist when bindings are built. Runtime-spawned delegates of a
   role share its name and inherit its visibility.
3. **Manager.** One `mcpharness.NewManager(bindings, deps)` per session.
   `Deps`: `SessionID` left zero (fingerprint-first ordering, see 1.4);
   `Gates` = a carbon-owned late-binding `GateOpener` (below); `Events` =
   the session event publisher; `Reporter` wired to surface notices
   (collisions, adoption failures) as session notices; `Sampling` omitted
   (v1 never spends model budget on server-initiated sampling).
   - *GateOpener.* No existing carbon object satisfies it. Interactive
     sessions use a small adapter over `session.GateHost` (obtained by
     asserting the controller after `rig.NewSession`); because the Manager is
     constructed before the session exists, the adapter is late-binding — it
     refuses (typed error) until the host is set. Headless sessions install
     an always-refusing opener, matching the headless permission posture.
4. **Lifecycle.** The runtime agent owns the Manager exactly as it owns the
   executor sets: partial construction failure closes what was built; shutdown
   closes once, idempotently, before the executor sets. Order:
   `NewManager` → `ConfigDigest()` into the rig fingerprint (§1.4) →
   `rig.NewSession` → `BindSession(sessionID)` → bind the GateOpener host →
   `Manager.Start(ctx)` (connects and discovers; without it nothing
   installs) → `StartAdoption(session, controller)` → initial
   `adopter.Install(ctx, loopID, loopName)` for the loops the composition
   root can identify (the active primer; `Install` is an `*Adopter` method,
   so adoption starts first). Other role loops receive their toolsets at
   their next idle boundary. `Adopter.Close()` runs at shutdown before
   `Manager` close.
5. **Roster interaction.** MCP tools are installed as external tools by the
   Adopter/installer path; `toolsets.go` rosters are unchanged. Delegate/leaf
   loops spawned at runtime receive session-scoped bindings per their role
   visibility through the same adoption path — note a delegate adopts at its
   first idle boundary, so its very first turn may run without MCP tools
   (acceptance test pins the behavior either way). Foreign (ACP) loops are
   permanently unsupported by the Adopter and are skipped (existing
   mcp-module behavior).

### 1.3 Permissions

No new capability kinds and no classifier involvement:

- Every MCP tool call carries `tool.invoke` with identity
  `mcp:<binding>:<tool>` and routes through the existing role gates to the
  product access source. Interactive sessions prompt and may persist rules in
  the per-workspace permission store; headless sessions get the typed
  approval-required denial.
- The command-safety permission classifier reviews shell commands only; MCP
  invocations are outside its evidence domain and are never auto-approved.
- `newProductAccessSource` already answers `AccessGated` for any non-empty
  `tool.invoke` scope (`access.go:180-190`), so `mcp:*` identities get "ask"
  with no change. A pinned acceptance test keeps that contract.

### 1.4 Fingerprint and restore

MCP identity enters the rig fingerprint through harness's designated seam:
`mgr.ConfigDigest()` supplies `rig.ConfigFingerprintFields.ExternalCapabilityRev`
before `rig.NewSession` — the exact flow `mcp/pkg/harness/attach.go`
documents. `accessConfigDigest` and `NativePermissionPolicyRev` are untouched.
The digest is secret-free by the mcp module's contract (binding identity and
transport identity, never header/env values). Changing the server set, a URL,
or visibility is a config-mismatch on restore, escaped only by the existing
`SessionSelector.AllowConfigMismatch`. Absent `mcp.json` contributes the
seam's no-external-capabilities value, so today's sessions restore unchanged.

### 1.5 Failure modes

- Invalid or insecure `mcp.json` → session construction fails with a typed,
  secret-free error. This includes a stdio `command` that does not resolve on
  `$PATH`: `transport/stdio.New` calls `exec.LookPath` at construction and
  treats a missing command as `FailureInvalidConfig`, and Carbon keeps that
  fail-closed posture (a typo'd command is a config error, same as a bad
  `models.json` alias — not a degraded server). Rejected alternative:
  catching construction errors and synthesizing a failed-optional binding
  would let a misconfiguration ride along silently.
- Configured but unreachable server (resolves/parses fine, connect or
  initialize fails at `Start`) → optional-binding failure: session opens,
  integration status event explains, tools simply absent.
- Server drops mid-session → existing mcp-module reconnect/retirement
  machinery; tools return typed unavailable results, never hang a turn
  (transport-level timeouts).
- Tool-schema drift mid-turn → existing generation checks in the adapted tool
  return a schema-changed result rather than calling with stale arguments.

## Part 2 — `Config.HomeDir`

New field on `Config` (`internal/app/config.go`):

```go
// HomeDir overrides the looprig base directory (default ~/.looprig).
// It relocates everything Carbon itself reads or writes under that root:
// models.json, mcp.json, workspaces/<hash>/permissions.json, and the
// default session-store root (store/).
HomeDir string
```

- Empty → `os.UserHomeDir() + "/.looprig"` (today's behavior, unchanged).
- Non-empty → must be an absolute path; validated once at construction
  (fail closed otherwise).
- One resolver (`internal/app`, e.g. `looprigHome(cfg)`) replaces the **four**
  hardcoded resolutions: `modelconfig.go:300` (models.json),
  `permissions.go:63` (workspaces subtree), `persistence.go:53`
  (`DefaultDataDir`, `~/.looprig/store`), and the new mcp loader. An
  explicitly configured data dir (the existing seam `cmd/carbon` uses) still
  wins over the HomeDir-derived default.
- Out of scope: tui's `looprig.log` and natsstore's jetstream directory keep
  their own resolution (natsstore is already XDG-aware). No cross-module
  changes.

## Part 3 — Permission-classifier enablement via `models.json`

Optional top-level section, `models.json` stays version 2 (absent field is
valid v2; no version bump):

```json
{
  "version": 2,
  "permission_review": {
    "model": "haiku",
    "strict": false
  },
  "models": [ { "alias": "haiku", "...": "..." } ]
}
```

- **Presence enables**: the loader sets `Config.PermissionReviewEnabled =
  true`, resolves `model` (required, non-empty) against the catalogue to
  `Config.PermissionReviewModel`, and maps `strict` (optional, default false)
  to `Config.PermissionReviewStrictPolicy`. Absence leaves all three zero —
  today's disabled default, byte-for-byte identical rig.
- **Trusted-profile gate (new invariant, settled with the user 2026-08-05)**:
  permission review only ever takes effect when the session's resolved
  `Config.AccessProfile == AccessTrusted`. This is enforced once, in
  `newPermissionReviewRegistration` (`internal/app/permission_review.go`),
  which already receives the full `cfg` and is the single choke point both
  the interactive and headless `openRuntimeAgent` paths call — so the gate
  applies no matter how `PermissionReviewEnabled` became true: the new
  models.json section, or the pre-existing programmatic
  `Config.PermissionReviewEnabled = true` seam. A non-trusted profile with
  review "enabled" by either path is **silently disabled** — same zero
  `permissionReviewRegistration{}`, same byte-for-byte-identical-to-disabled
  rig as an absent `permission_review` section, no error. Rationale: a
  shared, machine-wide `models.json` runs against sessions of every profile;
  auto-approval narrowing the human gate is a meaningfully different risk
  posture than the readonly or unconfined profiles were designed around, so
  it activates only for the profile it was designed for. This changes
  existing behavior: today `newPermissionReviewRegistration` and its tests
  (`TestPermissionReviewSecurityCeilingMatchesEvidenceContainment`) exercise
  `AccessReadOnly`/`AccessUnconfined`/`""` as enabled; those cases must be
  updated to assert silent disablement, with `AccessTrusted` remaining the
  one enabled case.
- **Validation at load, fail closed**: the alias must name a configured model
  whose capabilities include `tools` and `structured_output_with_tools`
  (the same requirement `commandsafety.New` enforces at construction —
  checking at load turns a session-open failure into a clear config error).
  Errors are typed in the `*ModelConfigError` family and name the alias, never
  the key.
- No `uses` tag is required on the model row — omit it (or leave the array
  empty) entirely and the model is addressable only by alias, invisible to
  the primer-picker and delegate roster; `permission_review.model` is the
  binding. A model already tagged `primer` or `delegate` may equally be
  reused as the classifier (the design's own `"model": "haiku"` example
  does this) — both a dedicated uses-less model and a reused primer/delegate
  model are supported.
- **Composition with programmatic Config** (Config is built before the loader
  runs — `cmd/carbon/main.go` constructs Config, then `Open` loads
  models.json and mutates it, the loader's existing pattern): the loader only
  ever **enables**. If `Config.PermissionReviewEnabled` is already true
  programmatically, the loader leaves all three fields untouched
  (programmatic enable wins, including its model and policy). If it is false,
  a present `permission_review` section enables. Because the field is a plain
  `bool`, "explicitly disabled programmatically" is not expressible against a
  file that enables — accepted limitation, noted in the field docs; a
  tri-state is not worth the surface today.
- Restore semantics are already defined and unchanged: enablement is part of
  the rig fingerprint; disabled→enabled restore is a rejected drift unless
  `AllowConfigMismatch` (CLAUDE.md "Permission review").

## Testing

- **mcpconfig loader** (table-driven): schema acceptance for all three
  transports; Claude-format inference of `type`; strict-decode rejections
  (unknown fields, duplicate keys, bad roles, bad URLs); file hygiene
  (mode, symlink, size); absent-file = disabled; error text never contains
  header/env values.
- **Assembly** (integration-tagged where a real process is spawned): a fake
  stdio MCP server proves install-at-open, `mcp__<binding>__<tool>` naming,
  gate prompt on first invoke with identity `mcp:<binding>:<tool>`,
  role-visibility enforcement by loop name (reviewer excluded by `roles`),
  fail-closed construction when the stdio command does not resolve,
  optional-binding degradation when a resolvable server fails to connect,
  the delegate first-idle adoption behavior, stdio env baseline
  (child sees `PATH` but not an unlisted parent variable), and
  close-exactly-once.
- **Fingerprint**: `ExternalCapabilityRev` changes on server add/remove/URL
  change/role change; stable across header-value changes; restore rejection +
  `AllowConfigMismatch` escape; absent mcp.json restores today's sessions.
- **HomeDir**: resolver honors override for all four locations; empty = home
  default; relative path rejected; explicit data dir still wins.
- **models.json permission_review** (table-driven): presence enables and
  resolves; missing/unknown alias fails; capability shortfall fails with the
  named alias; absence stays disabled; programmatic override wins.
- `make test-integration` remains the end-to-end proof for live permission
  review (existing suite) and gains the stdio MCP round-trip.

## Out of scope (deliberate)

- Workspace-level `mcp.json` (per-repo servers with first-use approval, the
  Claude Code `.mcp.json` model) — deferred until needed; requires an
  approval-gating design because the file can arrive committed in a cloned
  repository.
- MCP sampling policy (server-initiated LLM calls) — `Deps.Sampling` stays
  nil.
- Relocating tui `looprig.log` / natsstore jetstream under `HomeDir`.
- In-session reconfiguration of MCP servers (the mcp module supports it; no
  Carbon surface for it in v1).
- CLI flags for any of the above.
- The exported library construction path (`New()`/`newWithClient`) does not
  compose MCP — only `SessionStoreFactory.Open` → `openRuntimeAgent` does,
  and `cmd/carbon` exclusively uses that path. This is a real, pre-existing
  gap (not introduced by this plan); it is flagged in code comments
  (`runtime_controls.go`, `swarm.go`) but has no dedicated fix here because
  nothing in `cmd/carbon` exercises `New()`/`newWithClient` today.
