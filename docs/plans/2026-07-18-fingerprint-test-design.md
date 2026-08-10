# Fingerprint Test Compile Repair Design

**Date:** 2026-07-18

## Goal

Restore compilation of `internal/app` tests after Harness added the map-valued
`AppFields` member to `rig.ConfigFingerprintFields`.

## Design

Change the test's whole-struct comparison from Go's `!=` operator to
`reflect.DeepEqual`. This preserves the existing assertion over every field,
including future fields and the new map, while matching the comparison pattern
already used by Harness for map-containing values.

Run `go mod tidy` to reconcile coderig's direct inference requirement with the
newer pseudo-version already selected by its local Harness and LLM dependencies.
The expected metadata change is limited to the `github.com/looprig/inference`
version in `go.mod`; no `go.sum` change is expected.

The full-suite verification also exposed pre-existing TUI interface drift in
`cmd/coderig`'s `orderingAgent` test double. Add the required `RespondGate`
method as a no-op, matching the double's existing role as a compile-time
contract stub. This is test-only compatibility work and does not alter runtime
gate handling.

## Verification

Use the existing compile failure as the regression's red state, then run the
focused fingerprint test. Use the full-suite `RespondGate` compile failure as
the second red state, then run the command tests, the full race-enabled suite,
and a full build after both repairs. Confirm the final diff contains only the
two test compatibility changes, the expected inference version update, and
these planning documents.
