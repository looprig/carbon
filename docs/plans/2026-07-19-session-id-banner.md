# Session ID Banner Implementation Plan

> **For Codex:** REQUIRED SUB-SKILL: Use superpowers:test-driven-development to implement this plan task-by-task.

**Goal:** Show the current session UUID in every TUI startup banner and remove the startup greeting feature from TUI and CodeRig.

**Architecture:** Session identity becomes part of TUI's required `Agent` contract because every rendered agent is a session and every startup notice must show its ID. The banner reads the current agent directly, so new, resumed, and `/clear` replacement sessions always render the correct UUID. Greeting fields and the second startup notice are deleted at their TUI source, then CodeRig's flag, config, builder, and tests are removed.

**Tech Stack:** Go 1.26, Bubble Tea v2, looprig TUI, CodeRig.

---

### Task 1: Pin mandatory session identity in TUI

**Files:**
- Create: `../tui/internal/presentation/banner_test.go`
- Modify: `../tui/internal/presentation/screen_test.go`
- Modify: `../tui/internal/presentation/fixtures_test.go`
- Modify: `../tui/runtime/run_test.go`

1. Add a banner-format test expecting `<name>\nSession: #<full UUID>`.
2. Extend the `/clear` test to expect the replacement session's UUID.
3. Run the focused tests and confirm they fail against the current banner implementation.

### Task 2: Render the session UUID and remove TUI greetings

**Files:**
- Modify: `../tui/internal/presentation/agent.go`
- Modify: `../tui/internal/presentation/banner.go`
- Modify: `../tui/internal/presentation/screen.go`
- Modify: `../tui/runtime/run.go`
- Modify: affected TUI test doubles and tests.

1. Add `SessionID() uuid.UUID` to `Agent`.
2. Render the full current UUID as the second line of the single startup notice.
3. Read the agent at commit time so `/clear` uses the replacement ID.
4. Delete `Greeting` from both banner types and delete second-notice rendering.
5. Run focused TUI tests, then `go test -race ./...`.

### Task 3: Remove CodeRig greeting behavior

**Files:**
- Delete: `internal/app/greeting.go`
- Delete: `internal/app/greeting_test.go`
- Modify: `internal/app/config.go`
- Modify: `internal/app/agents.go`
- Modify: `cmd/coderig/main.go`
- Modify: `cmd/coderig/main_test.go`
- Modify: `docs/specs/coderig-assembly.md`

1. Change the CLI test so `--greeting` is rejected as an unknown flag.
2. Remove the greeting flag/configuration and banner wiring.
3. Remove greeting-only roster metadata and builders.
4. Update the live assembly specification.
5. Run `go test -race ./...` and the required build.

### Task 4: Verify scope

1. Run `git diff --check` in TUI and CodeRig.
2. Confirm no live non-vendor greeting API remains in TUI or CodeRig.
3. Confirm unrelated CodeRig `runtime_controls` edits and the catalog-identity move remain intact.

No commit is included because repository instructions require explicit user authorization before committing.
