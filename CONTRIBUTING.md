# Contributing to looprig/coderig

Thanks for considering a contribution. `coderig` is the reference coding Rig
built from looprig modules — it owns coding behavior and product assembly,
not reusable runtime machinery. This file is the short guide for working in
this repository.

## Before you write code

1. Read [`CLAUDE.md`](CLAUDE.md) (a.k.a. `AGENTS.md`). It is the authoritative
   source for the architecture, placement, security, and dependency rules
   this repo follows. PRs that contradict it will be asked to change.
2. Skim a couple of recent files in [`docs/plans/`](docs/plans/) for the
   design-doc style the project uses, and [`docs/specs/`](docs/specs/) for
   the current architecture specs (e.g. access profiles, assembly).
3. Open an issue for anything non-trivial so we can agree on direction
   before you spend the time.

## Design and security rules (the short version)

- **Placement discipline.** Keep behavior here when it is specific to a
  coding Rig — prompts, role tool selection, coding modes, model defaults,
  product flags. Move behavior to its owning module (`looprig/tools`,
  `looprig/sandbox`, `looprig/harness`, `looprig/tui`, ...) when it is
  reusable across products. Prefer direct assembly over local wrappers that
  only rename another module's API.
- **No generic agent registry or model tier catalog.** The roster is a
  small fixed set of Loop definitions, and role policy remains fixed in code.
  Production loads model data once from the operator-managed
  `~/.looprig/models.json`; do not reintroduce frozen production rows, fixed
  model aliases, provider-key environment reads, or native-model environment
  variables. Do not reintroduce a confinement bridge, a security-limit
  ordinal, or any in-session authority-mutation surface.
- **Least privilege per Loop.** Give each Loop the minimum tool set and the
  least-authority access profile it needs. Keep mutating, command, and
  network effects human-gated unless enforced guarantees justify automatic
  approval.
- **Bash is intentionally shell-based.** Permission checks and OS
  confinement are its boundaries — validate CLI input before constructing
  the Rig.
- **No secrets in logs or audit summaries.** Upstream proxy credentials
  live only inside the sandbox egress route and never enter the
  fingerprint, permission file, logs, or child environment. Inline provider
  keys are permitted only in the external, owner-only `0600`
  `~/.looprig/models.json`; never put them in `.env` or a shell command.
- **Keep model and permission boundaries separate.** CodeRig only reads the
  global model catalogue and never writes or chmods it. Native permissions
  remain per workspace at
  `~/.looprig/workspaces/<sha256(canonical-workspace)>/permissions.json`.
  ACP children may be gateway-backed or native-auth, but both receive posture
  metadata only and never provider keys or native permission files. Gateway
  children receive only the loopback proxy environment; native children
  receive only the isolated harness-login environment. An absent or disabled
  `native_acp` profile is unavailable; omitted `models` means harness-managed
  selection, while an explicit non-empty list requires source-aware defaults
  to name configured aliases.
- **Fail closed.** When access, permission, identity, or durable policy
  state is uncertain, deny by default.
- **Typed errors when callers classify or recover; wrapped ordinary errors
  for contextual failures.** Keep packages cohesive — split on ownership or
  invariants, not to satisfy a size rule. Introduce interfaces at consumer
  boundaries or when multiple implementations justify them.

## Build, test, and secure

Run these before pushing. CI runs the same.

```sh
make build     # CGO_ENABLED=0 go build -trimpath -o bin/coderig ./cmd/coderig
make run       # runs the TUI; optional .env values are launcher settings only
make test      # go test -race ./...
make fmt       # gofmt this module's Go files in place
make lint      # fmt-check + go vet + staticcheck + gosec
make vuln      # go mod verify + govulncheck
make secure    # lint + vuln
```

## Configure production models

CodeRig reads its machine-wide model catalogue once from
`~/.looprig/models.json` when production dependencies are assembled. It does
not create or modify this file. Create it with an editor, then restrict it to
the owner:

```sh
mkdir -p ~/.looprig
$EDITOR ~/.looprig/models.json
chmod 600 ~/.looprig/models.json
```

Do not paste a real key into a shell command: shell history, process listings,
and terminal capture can retain it. Enter credentials only inside the editor.
This complete example contains one no-auth LM Studio model and one API-key
model; `FAKE_EXAMPLE_KEY_DO_NOT_USE` is deliberately unusable:

```json
{
  "version": 1,
  "primer_default": "local-builder",
  "claude_code_small_model": "remote-delegate",
  "delegate_defaults": {
    "planner": {"harness": "codex", "model": "remote-delegate", "effort": "high"},
    "builder": {"harness": "claude-code", "model": "remote-delegate", "effort": "high"},
    "reviewer": {"harness": "claude-code", "model": "remote-delegate", "effort": "medium"}
  },
  "models": [
    {
      "alias": "local-builder",
      "provider": "lmstudio",
      "api_format": "openai",
      "base_url": "http://localhost:1234/v1",
      "model": "local-tool-model",
      "api_key": "",
      "uses": ["primer", "delegate"],
      "capabilities": {"tools": true, "thinking": false},
      "efforts": ["none"],
      "default_effort": "none"
    },
    {
      "alias": "remote-delegate",
      "provider": "openai",
      "api_format": "openai-responses",
      "base_url": "",
      "model": "example-remote-model",
      "api_key": "FAKE_EXAMPLE_KEY_DO_NOT_USE",
      "uses": ["delegate"],
      "capabilities": {"tools": true, "thinking": true},
      "efforts": ["medium", "high"],
      "default_effort": "high"
    }
  ]
}
```

The catalogue is authoritative for production model aliases and credentials.
The `.env` file, when used by `make run`, is only for launcher settings such as
ACP executable path overrides; it is not a provider-key source.

Native ACP configuration is an optional `native_acp` object in the same file.
An enabled profile may omit `models` to let Claude Code or Codex use its
already-configured login/default model. If `models` is present, it must be
non-empty; each explicit native default names one of those aliases. Set
`source: "native"` on a delegate default to select that profile. Native ACP
preflight uses no gateway proxy and its child environment is limited to the
harness login allowlist; gateway ACP uses the loopback proxy and excludes that
login environment.

## Tests

- **Table-driven tests** when several cases share setup and assertion
  shape; focused tests are fine for singular behavior. Cover the happy
  path, boundary values, error cases, and domain edge cases.
- Add integration tests for process, filesystem, network, or durable
  storage boundaries.
- A test that passes without `-race` but fails with it is **not passing**.
- The `Makefile` is the source of truth for how tests run; if you change
  that, update it.

## Design docs and plans

Non-trivial work goes through a short design doc in
[`docs/plans/`](docs/plans/) named `YYYY-MM-DD-<topic>-design.md` (and,
when ready, `YYYY-MM-DD-<topic>-implementation.md`). Architecture specs
that describe the current shape of the system (not a point-in-time change)
live in [`docs/specs/`](docs/specs/). Date plan files the day you start;
one topic per file.

## Pull requests

- Branch from `main`, name the branch something descriptive.
- One logical change per PR.
- Write a clear description: what, why, the design alternative you
  rejected, and how you verified. `make secure` output is welcome in the
  PR body.
- Don't force-push after review; add commits and let the reviewer squash.
- Don't commit secrets, tokens, or credentials.
- Don't add a new external dependency without prior approval in the
  conversation that adds it (see the Dependencies section of
  [`CLAUDE.md`](CLAUDE.md)). Sibling looprig modules already in `go.mod`
  are approved architecture dependencies; a small fixed set of
  development-only analysis tools (`gosec`, `govulncheck`, `staticcheck`)
  is also pre-approved.
- Don't update `CLAUDE.md`, `Makefile`, or `go.mod` unless the change is
  the point of the PR.
- Don't commit, push, or rename a remote repository unless explicitly
  asked.

## Code of conduct

Be excellent to each other. Discussions stay technical and respectful;
personal attacks, harassment, and discrimination are not welcome.

## License

By contributing, you agree that your contributions are licensed under the
Apache License 2.0, as described in [`LICENSE`](LICENSE).
