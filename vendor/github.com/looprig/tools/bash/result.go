package bash

// result.go renders the JSON result shape supervised.go's supervised
// (background / yield_time_ms) Bash calls return to the model, per the
// design spec's "Bash API" section (docs/specs/long-running-command-
// supervision.md): a TERMINAL shape ("status": "exited", "exit_code",
// "output", "started_at", "finished_at", "duration_ms") or a LIVE shape
// ("status": "running", "process_id", "output", "next_cursor",
// "started_at", "backgrounded": true) — plus, for a structural failure
// before either shape applies, a minimal {"error": "<stable code>"} shape
// (the spec's "Stable errors" section: "render to stable model-facing codes
// without exposing host paths, OS PIDs, or cross-owner details"). A legacy
// call never reaches this file: it keeps returning bash.go's existing plain
// "<output>\n[exit code: N]" text, produced by formatBashResult, unchanged.
//
// Every field here is drawn from process's own bounded, safe-by-construction
// types: process.Handle carries no OS PID, filesystem path, owner
// identifier, or creation timestamp (its own doc comment); process.State and
// process.Result.Reason are members of process's small closed string
// enumerations; process.Manifest's one host-identifying field (its
// unexported os osMetadata, holding an OS PID placeholder) is not even
// nameable outside package process, so nothing in this file could leak it
// even by accident.

import (
	"encoding/json"
	"time"

	"github.com/looprig/harness/pkg/tool"
)

// supervisedResult is the single JSON shape every supervised Bash call
// returns — see this file's package doc comment for the two success shapes
// and the one error shape, and result_test.go for worked examples.
type supervisedResult struct {
	Status       string `json:"status,omitempty"`
	ProcessID    string `json:"process_id,omitempty"`
	Output       string `json:"output,omitempty"`
	NextCursor   int64  `json:"next_cursor,omitempty"`
	ExitCode     *int   `json:"exit_code,omitempty"`
	Reason       string `json:"reason,omitempty"`
	StartedAt    string `json:"started_at,omitempty"`
	FinishedAt   string `json:"finished_at,omitempty"`
	DurationMS   *int64 `json:"duration_ms,omitempty"`
	Backgrounded bool   `json:"backgrounded,omitempty"`
	Error        string `json:"error,omitempty"`
}

// renderSupervisedResult marshals r to a compact JSON tool result.
// json.Marshal can fail only for a cyclic value or an unsupported type
// (channel, func, complex) — none of which this plain, flat, all-value
// struct can ever contain — so the fallback branch exists only to keep
// InvokableRun's documented "never returns a Go error" contract airtight
// even against that theoretical case.
func renderSupervisedResult(r supervisedResult) *tool.ToolResult {
	data, err := json.Marshal(r)
	if err != nil {
		return tool.TextResult(`{"error":"process_setup_failed"}`)
	}
	return tool.TextResult(string(data))
}

// supervisedErrorResult renders a stable, model-facing error code as the
// call's only populated field.
func supervisedErrorResult(code string) *tool.ToolResult {
	return renderSupervisedResult(supervisedResult{Error: code})
}

// liveSupervisedResult renders a LIVE (still-running or just-backgrounded)
// outcome. nextCursor/output describe only what THIS call itself read
// inline; Supervisor exposes no output-read accessor yet (that is Task 16's
// ProcessOutput tool), so every current caller passes cursor 0 and an empty
// output — an honest "nothing read inline yet, page from the start" rather
// than a fabricated preview. startedAt is the process's own durable
// Manifest.StartedAt (zero — and therefore omitted — only if the manifest
// could not be reloaded).
func liveSupervisedResult(processID string, nextCursor int64, output string, startedAt time.Time) *tool.ToolResult {
	return renderSupervisedResult(supervisedResult{
		Status:       "running",
		ProcessID:    processID,
		Output:       output,
		NextCursor:   nextCursor,
		StartedAt:    formatTime(startedAt),
		Backgrounded: true,
	})
}

// terminalSupervisedResult renders a TERMINAL outcome. status/exitCode/
// reason/startedAt/finishedAt must always come from the process's own
// durable Manifest (process.State / process.Result / process.Manifest's
// timestamps), never from a value Bash computed or guessed itself. Per the
// spec's shown example, a terminal result carries no process_id (the
// process has already fully completed; there is nothing left to reference
// with a follow-up ProcessOutput/ProcessInput/ProcessStop call) and no
// next_cursor/gap/truncation indicator of any kind -- unlike the LIVE
// shape's output, the spec's terminal example shows a plain "output" string
// and nothing else. output is the process's own bounded, safe-text-rendered
// combined stdout+stderr, read by supervised.go's readTerminalOutput through
// process's own ProcessOutput rendering path -- never fabricated or left
// empty just because the caller has no process_id to page from afterward.
// duration_ms is populated only when both timestamps are known.
func terminalSupervisedResult(status string, exitCode *int, reason string, startedAt, finishedAt time.Time, output string) *tool.ToolResult {
	result := supervisedResult{
		Status:     status,
		Output:     output,
		ExitCode:   exitCode,
		Reason:     reason,
		StartedAt:  formatTime(startedAt),
		FinishedAt: formatTime(finishedAt),
	}
	if !startedAt.IsZero() && !finishedAt.IsZero() {
		duration := finishedAt.Sub(startedAt).Milliseconds()
		result.DurationMS = &duration
	}
	return renderSupervisedResult(result)
}

// formatTime renders t as RFC3339Nano, or the empty string (omitted by
// supervisedResult's omitempty tags) for a zero Time.
func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339Nano)
}
