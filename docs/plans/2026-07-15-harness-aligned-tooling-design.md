# Harness-Aligned Repository Tooling Design

## Scope

Align `tui`, `confinement`, and `coderig` with the quality and security checks
used by `harness` before committing their current extraction and restructuring
work.

## Design

Each repository owns the same developer-facing checks:

- race-enabled tests;
- formatting verification;
- `go vet` and `staticcheck`;
- `gosec` scoped to the module's own packages;
- `go mod verify` and `govulncheck`;
- a `secure` target that combines lint and vulnerability checks.

The three analysis programs are declared with Go's `tool` directive and pinned
through each module's `go.mod` and `go.sum`. They are development dependencies,
not runtime dependencies.

Dependency distribution remains repository-specific. `tui` keeps its committed
vendor tree because it already promises offline, auditable builds. `coderig` and
`confinement` continue using normal Go module resolution instead of gaining
large vendor trees solely for tooling parity.

Go 1.26.5 does not resolve `go tool` declarations while `GOFLAGS=-mod=vendor` is
active. `tui` therefore keeps `-mod=vendor` for its code, tests, and builds, but
runs declared analysis tools with `-mod=mod`. This changes only how the tool
binary is resolved. The analyzed application packages still use the repository's
declared module graph.

## Verification

Before committing:

- `tui`: vendor integrity, secure checks, race tests, and a trimmed CGO-free
  build;
- `confinement`: secure checks and race tests;
- `coderig`: secure checks, race tests, and its trimmed CGO-free binary build.

Each repository is committed separately so its history remains independently
versionable.
