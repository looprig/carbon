# tui

The reusable terminal user interface for
[looprig](https://github.com/looprig/harness). It keeps the Bubble Tea v2
presentation stack outside the core Harness.

The module has four public package boundaries:

- the root `tui` package is the stable public facade for the interactive screen and contract
- `runtime` owns process signals, logging, terminal capture, and Bubble Tea startup
- `sessionadapter` adapts a Harness Session to the TUI contract
- `components` and `styles` are reusable presentation leaves

The dependency is one-directional: TUI imports Harness, never the reverse.

Internal ownership is similarly one-directional:

- `internal/presentation` owns the complete Bubble Tea state machine and behavioral tests
- `internal/input` owns attachment parsing, validation, and file completion
- `internal/model` owns independent value projections such as compaction and collapse state
- `internal/view` owns reusable rendering primitives such as rails and width wrapping
- `internal/ttylog` owns private terminal log capture

The transcript, interaction model, viewport, and `Screen` stay together in
`internal/presentation` because they form one Bubble Tea state machine rather than independent
reusable packages. Consumers continue to import only `github.com/looprig/tui`.

## Tool-preparation approval

A tool call that needs approval renders as ONE combined prompt built from the typed
`gate.PermissionPayload` (`event.PermissionRequested.Request`, a `tool.Request` narrowed
to its unmet requirements, each carrying its exact reusable rule candidates). The TUI
renders that typed payload verbatim — it never reconstructs a rule or parses tool
arguments. The prompt offers exactly the three `gate.ApprovalControls` actions, keyed:

- `y` — **Approve** (`gate.ApprovalApprove`; grants once, persists nothing)
- `a` — **Approve always for this workspace** (`gate.ApprovalApproveAlwaysWorkspace`;
  persists the displayed candidates)
- `n` / `esc` — **Deny** (`gate.ApprovalDeny`; fail-secure)

There is no session scope, user-global scope, per-capability sub-prompt, `/access`
command, access tray, or mutable security level.

## Session presentation metadata

Workspace path, the fixed access-profile name, and permission diagnostics are supplied
synchronously by the consumer at screen construction via
`tui.WithSessionPresentation(tui.SessionPresentation{…})` — never queried asynchronously
and never inferred from events. The fixed profile is shown as footer/session metadata
(not a mutable control), and permission diagnostics for manual, out-of-catalogue allow
families are committed in the startup metadata area so they are visible **before** the
first permission gate.

## Building & testing

This module vendors its dependencies. Use the Make targets for the vendored,
reproducible verification path:

```sh
make secure
go build ./...
go test -race ./...
```
