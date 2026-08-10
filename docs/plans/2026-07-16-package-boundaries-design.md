# CodeRig Package Boundaries

## Goal

Turn the CodeRig module root from an application-wide catch-all package into explicit internal packages without changing product behavior.

## Package layout

- `internal/app` owns CodeRig's cohesive application assembly: configuration, model setup, tool selection, compaction, persistence, session lifecycle, runtime context, and the command-facing API.
- `internal/catalog` owns the operator and reviewer identities and prompts.
- `cmd/coderig` owns flags, terminal process behavior, and command output.

The module root contains no Go package. CodeRig has no supported external Go consumers, so implementation packages remain under `internal`.

## Dependency direction

`cmd/coderig` depends on `internal/app`. The app package depends on the internal prompt catalog and reusable looprig modules. The catalog does not depend on application assembly.

The application remains one package because its current files share one composition graph and extensively test unexported invariants together. Further splits should happen only when a stable one-way ownership boundary appears, not by adding interfaces solely to create folders.

## Migration

Move existing files and tests according to ownership, update package declarations and imports, and keep the current construction path intact. Do not introduce registries, compatibility wrappers, or new abstractions solely for the move.

The migration must preserve prompts, model defaults, fingerprints, store paths, session restore behavior, tool grants, security limits, and CLI behavior.

## Verification

Run formatting, dependency tests, unit and integration race tests, build, lint, and security checks. Compare failures against the pre-refactor baseline and do not hide unrelated failures.
