# Contributing to looprig/llm

Thanks for considering a contribution. `llm` is the batteries-included,
multi-provider LLM client library layered on the neutral
`github.com/looprig/inference` model-call contract. It owns the provider
policy that `inference` deliberately does not carry: the known-provider
registry and factory dispatch (`auto`), the provider/API-format truth table,
provider auth requirements (including a SigV4 authenticator in `auth`), and
the fail-closed model-validation preset. It also hosts self-contained
provider-security machinery — an agent-confidential-interface protocol
(`aci`, with end-to-end encryption, identity, and canonicalized signing) and
TEE attestation verification (`tee`, covering Intel and NVIDIA quotes) —
alongside the concrete provider clients (`providers/`: Bedrock, Chutes,
Gemini, Phala). This file is the short guide for working in this repository.

## Before you write code

Open an issue for anything non-trivial — a new provider, a change to the
provider/API-format truth table, or anything touching `aci` or `tee` — so we
can agree on direction before you spend the time.

## Build, test, and secure

Run these before pushing. CI runs the same.

```sh
make fmt       # gofmt every first-party package directory in place
make test      # go test -race ./...
make secure    # lint + vuln:
               #   lint = fmt-check + vendor-check + go vet + staticcheck + gosec
               #   vuln = go mod verify + govulncheck
```

The module **vendors** its dependency tree and builds against it: the
Makefile exports `GOFLAGS=-mod=vendor` so a stray global `GOFLAGS` can't
silently switch the build off the vendored tree. Do not run `go get`
casually — `make vendor` refreshes `vendor/`, strips the `.git` pointer Go
copies from the local `inference` replace target, and `vendor-check` then
rejects any other Git metadata that leaked into the tree.

`lint`'s `gosec` step is intentionally scoped to this module's own package
directories (not a bare `./...` filesystem walk), so it doesn't descend into
nested `.worktrees/` checkouts and report on those foreign modules. Fuzz any
parser of external or attacker-influenced input, which in this module
notably includes the `aci` wire format and TEE quote parsing:
`go test -fuzz=FuzzXxx ./aci -fuzztime=30s` (see `make fuzz` for the
reminder).

## Tests

- **Table-driven tests, mandatory** when several cases share setup and
  assertion shape. Each subtest calls `t.Parallel()`. Cover the happy path,
  boundary values (zero/empty/max), error cases (invalid/missing/wrong
  type), and domain edge cases.
- A test that passes without `-race` but fails with it is **not passing**.
- Fuzz tests already exist for several security-sensitive parsers (`aci`'s
  body, e2ee, jcs, and keys handling); extend them rather than starting a
  parallel ad hoc harness when you touch that code.
- Never assume a test framework or script beyond what's in the `Makefile`;
  if you change how tests run, update it.

## Pull requests

- Branch from `main`, name the branch something descriptive.
- One logical change per PR. This module is depended on by several sibling
  repos (`replace` directives point back to local checkouts during
  development); avoid bundling unrelated changes that ripple outward.
- Write a clear description: what, why, and how you verified. `make secure`
  output is welcome in the PR body.
- Don't force-push after review; add commits and let the reviewer squash.
- Don't commit secrets, tokens, or credentials.
- Don't add a new external dependency without prior discussion — this
  module vendors everything and is a dependency of several sibling repos,
  so new packages get reviewed before they're added.
- Don't update `Makefile` or `go.mod` unless the change is the point of the
  PR.
- Changes touching security-sensitive code — cryptography, TEE attestation
  (`tee`), the `aci` protocol, or key handling (`auth`, `aci/keys.go`) —
  warrant extra care in review: call out the threat model implications in
  the PR description and expect closer scrutiny than an average change.

## Code of conduct

Be excellent to each other. Discussions stay technical and respectful;
personal attacks, harassment, and discrimination are not welcome.

## License

By contributing, you agree that your contributions are licensed under the
Apache License 2.0, as described in [`LICENSE`](LICENSE).
