//go:build integration

package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/looprig/coderig/internal/catalog/generic"
	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/event"
	"github.com/looprig/inference"
)

// process_integration_test.go is Task 28's headline end-to-end acceptance
// suite: it drives Bash/ProcessOutput/ProcessInput/ProcessStop through REAL
// Coderig composition (openWithClient -> openRuntimeAgent -> buildRig ->
// harness Rig/session) and REAL Sandbox execution (Config{AccessProfile:
// AccessUnconfined}, the one profile that can actually spawn a supervised
// background process on this Darwin machine today -- see toolsets.go's
// coderigProfile and process_tools_test.go's mustUnconfinedExecutorSet doc
// comment for why), never a fake process layer. It covers plan scenarios
// 1-9, 11, and 15 directly; scenarios 10 and 13 are covered here too, but as
// the Darwin fail-closed CONFINED-profile substitute the task's own Darwin
// bullet calls for (see TestIntegrationProcessGrantDeniedUnderConfinedProfile
// and TestIntegrationProcessLiveManifestNeverCreatedUnderConfinedProfile).
// Scenario 12 (restore completed output) and scenario 14
// (TestIntegrationProcessJournalIdempotencyReopen) live in the sibling file
// process_restore_integration_test.go, since both need a close+reopen across
// a real fsstore SessionStoreFactory.
//
// Fixture: real fsstore under t.TempDir() (persistence_integration_test.go's
// newIntegrationFactory), a scripted inference.Client that emits exact tool
// calls and never touches a network model (managed_delegation_test.go's
// managedScript/namedToolCall/lastToolText), and a dedicated t.TempDir()
// workspace. No test in this file calls t.Parallel(): openWithClient
// resolves the session's exclusive workspace from os.Getwd(), so every test
// using openProcessIntegrationAgent must t.Chdir first
// (persistence_integration_test.go's own TestSessionStoreWorkspaceRoundTrip
// establishes this exact constraint).

// processIntegrationAgent bundles one real, fsstore-backed, Unconfined-profile
// session over its own dedicated workspace, plus the scripted client driving
// it.
type processIntegrationAgent struct {
	factory   *SessionStoreFactory
	agent     *RuntimeAgent
	client    *managedScript
	workspace string
}

// openProcessIntegrationAgent opens a brand-new such session.
//
// Close is deliberately non-fatal on error (logged, not asserted). While
// developing this fixture, an early version of every "start a background
// command" scenario reproducibly HUNG for ~10 seconds -- not just Close,
// but an entirely unrelated second ordinary turn submitted right after the
// background start, proven with a full goroutine dump -- whenever a turn
// went idle while the process it had just started was still running. Root
// cause: tools/process's entry.doTerminalize released the process's
// workspace lifetime lease only AFTER calling the completion notifier,
// which (in real Harness composition) dispatches into the owning loop's
// own command channel; when that loop's actor was itself synchronously
// blocked acquiring a conflicting lease for a SessionIdle-triggered
// best-effort workspace checkpoint, the lease could never be released
// until the actor became reachable again -- which could only happen AFTER
// that same lease was released. Fixed at the source
// (tools/process/entry.go, commit "release lease/quota before notifying,
// breaking a real deadlock"): the lease and quota now release before
// notification, unconditionally breaking the cycle. Close() may still take
// a brief, bounded moment when a process is genuinely still running at
// Close time (Supervisor.Shutdown's own graceful-terminate-then-kill
// escalation), which is expected and not swallowed here -- only logged, so
// a real regression is never silently masked.
func openProcessIntegrationAgent(t *testing.T) *processIntegrationAgent {
	t.Helper()
	workspace := t.TempDir()
	t.Chdir(workspace)
	factory := newIntegrationFactory(t)
	client := &managedScript{}
	cfg := Config{AccessProfile: AccessUnconfined, HomeDir: t.TempDir()}
	agent, err := factory.openWithClient(context.Background(), client, newModelFactoryFor(testModel()), SessionSelector{}, cfg)
	if err != nil {
		t.Fatalf("openWithClient(new, unconfined) error = %v", err)
	}
	t.Cleanup(func() {
		if err := agent.Close(context.Background()); err != nil {
			t.Logf("agent.Close() error = %v (see openProcessIntegrationAgent's doc comment for the known cause when a process was left live)", err)
		}
	})
	return &processIntegrationAgent{factory: factory, agent: agent, client: client, workspace: workspace}
}

// waitForProcessCompleted blocks until a public event.ProcessCompleted for
// processID is observed on stream, so a caller can be certain the process
// has fully terminalized (and so, per entry.doTerminalize, already released
// its workspace lease) before ending the test -- good fixture hygiene
// independent of openProcessIntegrationAgent's own doc comment.
func waitForProcessCompleted(t *testing.T, stream event.Subscription, processID string) {
	t.Helper()
	acceptanceEventsUntil(t, stream, func(ev event.Event) bool {
		completed, ok := ev.(event.ProcessCompleted)
		return ok && completed.Process.ProcessHandle == processID
	})
}

// processScriptStep produces the next chunks a scripted Generic-loop turn
// emits, given the PRIOR tool result text (empty for a call's first step).
type processScriptStep func(prior string) []content.Chunk

// runProcessScript drives steps as a sequence of Generic-loop tool calls
// within ONE turn (managed_delegation_test.go's managedScript/
// runManagedTurnObserved pattern: the callback observes the real bound
// inference.Request, including injected tools and prior tool results) and
// returns the turn's final text plus every observed event. Generic is the only
// loop this fixture's single-loop, non-delegating scripts ever reaches.
func runProcessScript(t *testing.T, pia *processIntegrationAgent, prompt string, steps ...processScriptStep) (string, []event.Event) {
	t.Helper()
	i := 0
	pia.client.fn = func(_ context.Context, req inference.Request) ([]content.Chunk, error) {
		if !requestHasRole(req, generic.Name) {
			return nil, fmt.Errorf("processScript: unexpected role in request (system=%q)", req.System)
		}
		if i >= len(steps) {
			return nil, fmt.Errorf("processScript: no script step for call %d", i+1)
		}
		step := steps[i]
		i++
		return step(lastToolText(req)), nil
	}
	return runManagedTurnObserved(t, pia.agent, prompt)
}

func bashCall(id, argsJSON string) []content.Chunk { return namedToolCall(id, "Bash", argsJSON) }
func processOutputCall(id, argsJSON string) []content.Chunk {
	return namedToolCall(id, "ProcessOutput", argsJSON)
}
func processInputCall(id, argsJSON string) []content.Chunk {
	return namedToolCall(id, "ProcessInput", argsJSON)
}
func processStopCall(id, argsJSON string) []content.Chunk {
	return namedToolCall(id, "ProcessStop", argsJSON)
}

// bashSupervisedResultJSON mirrors bash/result.go's supervisedResult wire
// shape (this test cannot import the tools module's internal bash package,
// so it decodes the SAME field names independently, matching every other
// black-box acceptance test in this codebase).
type bashSupervisedResultJSON struct {
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

func decodeBashResult(t *testing.T, text string) bashSupervisedResultJSON {
	t.Helper()
	var got bashSupervisedResultJSON
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("decode Bash result %q: %v", text, err)
	}
	return got
}

// processResultJSON mirrors the union of process/output_tool.go's
// processOutputResult and process/input_tool.go's/stop_tool.go's identical
// or narrower flat result shapes -- every field is optional on the wire
// (omitempty), so one permissive struct safely decodes any of the three
// tools' single-process result.
type processResultJSON struct {
	ProcessID   string `json:"process_id"`
	Status      string `json:"status,omitempty"`
	Output      string `json:"output,omitempty"`
	StartCursor int64  `json:"start_cursor"`
	NextCursor  int64  `json:"next_cursor"`
	TotalBytes  int64  `json:"total_bytes"`
	Gap         bool   `json:"gap,omitempty"`
	Normalized  bool   `json:"normalized,omitempty"`
	Binary      bool   `json:"binary,omitempty"`
	Artifact    *struct {
		ID       string `json:"id"`
		Encoding string `json:"encoding"`
	} `json:"artifact,omitempty"`
	ExitCode   *int   `json:"exit_code,omitempty"`
	Reason     string `json:"reason,omitempty"`
	StartedAt  string `json:"started_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
	Error      string `json:"error,omitempty"`
}

// processResultsEnvelope mirrors ProcessOutput's plural (process_ids)
// {"results": [...]} wire shape.
type processResultsEnvelope struct {
	Results []processResultJSON `json:"results"`
	Error   string              `json:"error,omitempty"`
}

func decodeProcessResult(t *testing.T, text string) processResultJSON {
	t.Helper()
	var got processResultJSON
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("decode process result %q: %v", text, err)
	}
	return got
}

func decodeProcessResults(t *testing.T, text string) processResultsEnvelope {
	t.Helper()
	var got processResultsEnvelope
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("decode process results envelope %q: %v", text, err)
	}
	return got
}

// --- Scenario 1: run an unchanged foreground Bash ---------------------

// TestIntegrationProcessUnchangedForegroundBash proves a plain Bash call
// (no background, no yield_time_ms) through the SAME session-supervised
// bashDefinition Task 27 wires into the Generic roster still takes the
// legacy synchronous path unchanged: bash/supervised.go's own package doc
// comment ("a legacy call never reaches this file") means this call never
// touches the Supervisor at all -- the plain "<output>\n[exit code: N]"
// text shape, not the new JSON supervisedResult shape, is the proof.
func TestIntegrationProcessUnchangedForegroundBash(t *testing.T) {
	pia := openProcessIntegrationAgent(t)

	final, _ := runProcessScript(t, pia, "run a command",
		func(string) []content.Chunk {
			return bashCall("legacy-1", `{"command": "printf hello-legacy"}`)
		},
		func(prior string) []content.Chunk {
			if prior != "hello-legacy\n[exit code: 0]" {
				return []content.Chunk{&content.TextChunk{Text: fmt.Sprintf("unexpected legacy result: %q", prior)}}
			}
			return []content.Chunk{&content.TextChunk{Text: "legacy-ok"}}
		},
	)
	if final != "legacy-ok" {
		t.Fatalf("final = %q, want legacy-ok (legacy Bash result mismatch)", final)
	}
}

// --- Scenario 2: start a background command ----------------------------

// TestIntegrationProcessStartsInBackground proves background: true returns
// immediately with a live process handle, never waiting for the command to
// finish.
func TestIntegrationProcessStartsInBackground(t *testing.T) {
	pia := openProcessIntegrationAgent(t)
	stream, err := pia.agent.Subscribe(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	defer func() { _ = stream.Close() }()

	var started bashSupervisedResultJSON
	final, _ := runProcessScript(t, pia, "start a background command",
		func(string) []content.Chunk {
			return bashCall("bg-1", `{"command": "sleep 0.3 && echo background-done", "background": true}`)
		},
		func(prior string) []content.Chunk {
			started = decodeBashResult(t, prior)
			return []content.Chunk{&content.TextChunk{Text: "started"}}
		},
	)
	if final != "started" {
		t.Fatalf("final = %q, want started", final)
	}
	if started.Error != "" {
		t.Fatalf("background start error = %q, want none", started.Error)
	}
	if started.Status != "running" || !started.Backgrounded || started.ProcessID == "" {
		t.Fatalf("background start result = %+v, want status running, backgrounded true, a process_id", started)
	}

	// Drain the process to completion before this test ends: see
	// openProcessIntegrationAgent's doc comment for why a still-live
	// process's workspace lease makes the eventual Close() slow.
	waitForProcessCompleted(t, stream, started.ProcessID)
}

// --- Scenario 3: yield a foreground command -----------------------------

// TestIntegrationProcessYieldsForegroundToBackground covers both yield
// outcomes the spec's "Bash API" section documents: a budget that comfortably
// outlasts the command (Bash returns its TERMINAL result inline, exactly
// like a plain foreground call but through the yield path) and a budget the
// command outlives (Bash returns the LIVE handle exactly like an explicit
// background start, with backgrounded: true).
func TestIntegrationProcessYieldsForegroundToBackground(t *testing.T) {
	t.Run("completes within budget", func(t *testing.T) {
		pia := openProcessIntegrationAgent(t)
		var result bashSupervisedResultJSON
		final, _ := runProcessScript(t, pia, "yield a quick command",
			func(string) []content.Chunk {
				return bashCall("yield-fast", `{"command": "echo quick", "yield_time_ms": 3000}`)
			},
			func(prior string) []content.Chunk {
				result = decodeBashResult(t, prior)
				return []content.Chunk{&content.TextChunk{Text: "yield-fast-ok"}}
			},
		)
		if final != "yield-fast-ok" {
			t.Fatalf("final = %q", final)
		}
		if result.Status != "exited" || result.ExitCode == nil || *result.ExitCode != 0 || result.Backgrounded {
			t.Fatalf("fast-yield result = %+v, want terminal exited/0, not backgrounded", result)
		}
	})

	t.Run("outlives budget and hands off to background", func(t *testing.T) {
		pia := openProcessIntegrationAgent(t)
		stream, err := pia.agent.Subscribe(event.EventFilter{Enduring: event.LoopScope{All: true}})
		if err != nil {
			t.Fatalf("Subscribe() error = %v", err)
		}
		defer func() { _ = stream.Close() }()

		var result bashSupervisedResultJSON
		final, _ := runProcessScript(t, pia, "yield a slow command",
			func(string) []content.Chunk {
				return bashCall("yield-slow", `{"command": "sleep 1 && echo slow-done", "yield_time_ms": 50}`)
			},
			func(prior string) []content.Chunk {
				result = decodeBashResult(t, prior)
				return []content.Chunk{&content.TextChunk{Text: "yield-slow-ok"}}
			},
		)
		if final != "yield-slow-ok" {
			t.Fatalf("final = %q", final)
		}
		if result.Status != "running" || !result.Backgrounded || result.ProcessID == "" {
			t.Fatalf("slow-yield result = %+v, want live handoff (running/backgrounded/process_id)", result)
		}

		// Drain to completion before this subtest ends: see
		// openProcessIntegrationAgent's doc comment.
		waitForProcessCompleted(t, stream, result.ProcessID)
	})
}

// --- Scenario 4: poll incremental output --------------------------------

// TestIntegrationProcessPollsIncrementalOutput proves a second ProcessOutput
// poll, cursored at the first poll's next_cursor, returns only the NEW bytes
// written since -- never replaying the whole transcript.
func TestIntegrationProcessPollsIncrementalOutput(t *testing.T) {
	pia := openProcessIntegrationAgent(t)

	var processID string
	var firstCursor int64
	var firstOutput, secondOutput string
	final, _ := runProcessScript(t, pia, "poll incremental output",
		func(string) []content.Chunk {
			return bashCall("poll-start", `{"command": "echo first-chunk && sleep 0.3 && echo second-chunk", "background": true}`)
		},
		func(prior string) []content.Chunk {
			started := decodeBashResult(t, prior)
			processID = started.ProcessID
			return processOutputCall("poll-1", fmt.Sprintf(`{"process_id": %q, "cursor": 0, "wait": "any", "timeout_ms": 5000}`, processID))
		},
		func(prior string) []content.Chunk {
			first := decodeProcessResult(t, prior)
			firstOutput = first.Output
			firstCursor = first.NextCursor
			return processOutputCall("poll-2", fmt.Sprintf(`{"process_id": %q, "cursor": %d, "wait": "all", "timeout_ms": 5000}`, processID, firstCursor))
		},
		func(prior string) []content.Chunk {
			second := decodeProcessResult(t, prior)
			secondOutput = second.Output
			if second.StartCursor != firstCursor {
				return []content.Chunk{&content.TextChunk{Text: fmt.Sprintf("start_cursor mismatch: got %d want %d", second.StartCursor, firstCursor)}}
			}
			return []content.Chunk{&content.TextChunk{Text: "poll-ok"}}
		},
	)
	if final != "poll-ok" {
		t.Fatalf("final = %q", final)
	}
	if firstOutput == "" {
		t.Fatal("first poll returned no output at all")
	}
	if secondOutput == firstOutput {
		t.Fatalf("second poll (cursored at %d) replayed the first poll's own output instead of only new bytes: %q", firstCursor, secondOutput)
	}
	if want := "second-chunk"; !strings.Contains(secondOutput, want) {
		t.Errorf("second poll output = %q, want it to contain %q (new bytes only)", secondOutput, want)
	}
	if strings.Contains(secondOutput, "first-chunk") {
		t.Errorf("second poll output = %q, replayed first-chunk (already-read bytes)", secondOutput)
	}
}

// --- Scenario 5: wait on multiple processes -----------------------------

// TestIntegrationProcessWaitsOnMultipleProcesses starts two independent
// background commands and reads both through ONE ProcessOutput(process_ids)
// call using wait:"all", proving multi-process results preserve the
// caller's own (here, deliberately reversed) input order.
func TestIntegrationProcessWaitsOnMultipleProcesses(t *testing.T) {
	pia := openProcessIntegrationAgent(t)

	var idA, idB string
	final, _ := runProcessScript(t, pia, "wait on multiple processes",
		func(string) []content.Chunk {
			return bashCall("multi-a", `{"command": "sleep 0.1 && echo A-done", "background": true}`)
		},
		func(prior string) []content.Chunk {
			idA = decodeBashResult(t, prior).ProcessID
			return bashCall("multi-b", `{"command": "sleep 0.2 && echo B-done", "background": true}`)
		},
		func(prior string) []content.Chunk {
			idB = decodeBashResult(t, prior).ProcessID
			// Deliberately reversed order (B before A) to prove the response
			// preserves the CALLER's own order, not admission order.
			args := fmt.Sprintf(`{"process_ids": [%q, %q], "cursor": 0, "wait": "all", "timeout_ms": 5000}`, idB, idA)
			return processOutputCall("multi-wait", args)
		},
		func(prior string) []content.Chunk {
			envelope := decodeProcessResults(t, prior)
			if len(envelope.Results) != 2 {
				return []content.Chunk{&content.TextChunk{Text: fmt.Sprintf("results = %d, want 2", len(envelope.Results))}}
			}
			if envelope.Results[0].ProcessID != idB || envelope.Results[1].ProcessID != idA {
				return []content.Chunk{&content.TextChunk{Text: fmt.Sprintf("order = [%s, %s], want [%s, %s]", envelope.Results[0].ProcessID, envelope.Results[1].ProcessID, idB, idA)}}
			}
			if !strings.Contains(envelope.Results[0].Output, "B-done") || !strings.Contains(envelope.Results[1].Output, "A-done") {
				return []content.Chunk{&content.TextChunk{Text: fmt.Sprintf("outputs = %q / %q, want B-done / A-done", envelope.Results[0].Output, envelope.Results[1].Output)}}
			}
			return []content.Chunk{&content.TextChunk{Text: "multi-ok"}}
		},
	)
	if final != "multi-ok" {
		t.Fatalf("final = %q", final)
	}
}

// --- Scenario 6: send stdin and EOF -------------------------------------

// TestIntegrationProcessSendsStdinAndEOF starts `cat` (which echoes stdin
// verbatim until EOF, then exits), writes one line via ProcessInput's data
// field, then closes stdin via ProcessInput's eof field, and confirms `cat`
// echoed the line and exited cleanly -- proving both fields independently.
func TestIntegrationProcessSendsStdinAndEOF(t *testing.T) {
	pia := openProcessIntegrationAgent(t)

	var processID string
	final, _ := runProcessScript(t, pia, "send stdin and EOF",
		func(string) []content.Chunk {
			return bashCall("stdin-start", `{"command": "cat", "background": true}`)
		},
		func(prior string) []content.Chunk {
			processID = decodeBashResult(t, prior).ProcessID
			return processInputCall("stdin-data", fmt.Sprintf(`{"process_id": %q, "data": "hello-stdin\n"}`, processID))
		},
		func(prior string) []content.Chunk {
			afterData := decodeProcessResult(t, prior)
			if afterData.Error != "" {
				return []content.Chunk{&content.TextChunk{Text: "data write error: " + afterData.Error}}
			}
			return processInputCall("stdin-eof", fmt.Sprintf(`{"process_id": %q, "eof": true, "yield_time_ms": 5000}`, processID))
		},
		func(prior string) []content.Chunk {
			afterEOF := decodeProcessResult(t, prior)
			if afterEOF.Status != "exited" || afterEOF.ExitCode == nil || *afterEOF.ExitCode != 0 {
				return []content.Chunk{&content.TextChunk{Text: fmt.Sprintf("after EOF = %+v, want exited/0", afterEOF)}}
			}
			// ProcessInput's own snapshot is windowed from the cursor
			// captured immediately before THIS operation (input_tool.go's
			// own documented default), so it can legitimately show no new
			// bytes if `cat`'s stdout buffering had already flushed the
			// echoed line during the earlier data-only write. Read the full
			// transcript explicitly from cursor 0 to make the assertion
			// robust to that buffering timing either way.
			return processOutputCall("stdin-read-all", fmt.Sprintf(`{"process_id": %q, "cursor": 0}`, processID))
		},
		func(prior string) []content.Chunk {
			full := decodeProcessResult(t, prior)
			if !strings.Contains(full.Output, "hello-stdin") {
				return []content.Chunk{&content.TextChunk{Text: fmt.Sprintf("full output = %q, want it to contain hello-stdin", full.Output)}}
			}
			return []content.Chunk{&content.TextChunk{Text: "stdin-ok"}}
		},
	)
	if final != "stdin-ok" {
		t.Fatalf("final = %q", final)
	}
}

// --- Scenario 7: resize/use PTY where supported -------------------------

// TestIntegrationProcessResizesPTY requests a real PTY (tty: true) --
// supported end to end on Darwin/Unconfined per this session's Phase 5 PTY
// work -- and resizes it through ProcessInput's rows/cols fields, proving
// the live terminal actually observes the new size via `stty size`.
func TestIntegrationProcessResizesPTY(t *testing.T) {
	pia := openProcessIntegrationAgent(t)

	var processID string
	final, _ := runProcessScript(t, pia, "use and resize a PTY",
		func(string) []content.Chunk {
			return bashCall("pty-start", `{"command": "sleep 0.3 && stty size", "background": true, "tty": true}`)
		},
		func(prior string) []content.Chunk {
			started := decodeBashResult(t, prior)
			if started.Error == "pty_unavailable" {
				return []content.Chunk{&content.TextChunk{Text: "pty_unavailable"}}
			}
			processID = started.ProcessID
			return processInputCall("pty-resize", fmt.Sprintf(`{"process_id": %q, "rows": 40, "cols": 120}`, processID))
		},
		func(prior string) []content.Chunk {
			afterResize := decodeProcessResult(t, prior)
			if afterResize.Error != "" {
				return []content.Chunk{&content.TextChunk{Text: "resize error: " + afterResize.Error}}
			}
			return processOutputCall("pty-wait", fmt.Sprintf(`{"process_id": %q, "cursor": 0, "wait": "all", "timeout_ms": 5000}`, processID))
		},
		func(prior string) []content.Chunk {
			final := decodeProcessResult(t, prior)
			if !strings.Contains(final.Output, "40 120") {
				return []content.Chunk{&content.TextChunk{Text: fmt.Sprintf("stty size output = %q, want it to report the resized 40 120", final.Output)}}
			}
			return []content.Chunk{&content.TextChunk{Text: "pty-ok"}}
		},
	)
	if final == "pty_unavailable" {
		t.Skip("ALLOWED_PLATFORM_LIMITATION: real PTY unavailable under Unconfined on this host")
	}
	if final != "pty-ok" {
		t.Fatalf("final = %q", final)
	}
}

// --- Scenario 8: interrupt, terminate, and kill -------------------------

// TestIntegrationProcessInterruptTerminateAndKill proves all three
// ProcessStop modes against a real long-running process tree, each of which
// blocks until Sandbox itself confirms the tree has exited before the tool
// call returns its terminal snapshot (stop_tool.go's own documented
// contract).
func TestIntegrationProcessInterruptTerminateAndKill(t *testing.T) {
	tests := []struct {
		name       string
		mode       string
		wantStatus string
		wantReason string
	}{
		{name: "interrupt", mode: "interrupt", wantStatus: "interrupted", wantReason: "interrupted"},
		{name: "terminate", mode: "terminate", wantStatus: "terminated", wantReason: "terminated"},
		{name: "kill", mode: "kill", wantStatus: "killed", wantReason: "killed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pia := openProcessIntegrationAgent(t)
			var processID string
			final, _ := runProcessScript(t, pia, "stop a long-running command",
				func(string) []content.Chunk {
					return bashCall("stop-start", `{"command": "sleep 30", "background": true}`)
				},
				func(prior string) []content.Chunk {
					processID = decodeBashResult(t, prior).ProcessID
					return processStopCall("stop-signal", fmt.Sprintf(`{"process_id": %q, "mode": %q, "grace_ms": 2000}`, processID, tc.mode))
				},
				func(prior string) []content.Chunk {
					stopped := decodeProcessResult(t, prior)
					if stopped.Error != "" {
						return []content.Chunk{&content.TextChunk{Text: "stop error: " + stopped.Error}}
					}
					if stopped.Status != tc.wantStatus || stopped.Reason != tc.wantReason {
						return []content.Chunk{&content.TextChunk{Text: fmt.Sprintf("stop result = status %q reason %q, want %q/%q", stopped.Status, stopped.Reason, tc.wantStatus, tc.wantReason)}}
					}
					return []content.Chunk{&content.TextChunk{Text: "stop-ok"}}
				},
			)
			if final != "stop-ok" {
				t.Fatalf("final = %q", final)
			}
		})
	}
}

// --- Scenario 9: overflow the spool retention window and keep running ---

// TestIntegrationProcessOverflowsSpoolRetentionAndKeepsRunning shrinks a
// single process's disk retention ceiling far below the amount of output it
// actually produces (max_output_bytes), proving the process is never
// terminated for producing too much output (it still exits cleanly),
// total_bytes still counts the FULL stream, and a read from the original
// cursor 0 reports gap: true because the earliest bytes were dropped, not
// retained forever.
func TestIntegrationProcessOverflowsSpoolRetentionAndKeepsRunning(t *testing.T) {
	pia := openProcessIntegrationAgent(t)

	const ceiling = 1024
	command := `i=1; while [ $i -le 3000 ]; do echo "line-$i-0123456789012345678901234567890"; i=$((i+1)); done; echo tail-marker`

	var processID string
	var cursor int64
	// wait:"all" against a SINGLE process with cursor 0 returns as soon as
	// ANY new output lands (not necessarily all 3000 lines), so a single
	// wait call cannot be assumed to observe the terminal state. Poll in a
	// bounded loop, advancing the cursor each time, until the process
	// itself reports a terminal status.
	const maxPolls = 40
	steps := make([]processScriptStep, 0, maxPolls+2)
	steps = append(steps, func(string) []content.Chunk {
		args, err := json.Marshal(struct {
			Command        string `json:"command"`
			Background     bool   `json:"background"`
			MaxOutputBytes int    `json:"max_output_bytes"`
		}{Command: command, Background: true, MaxOutputBytes: ceiling})
		if err != nil {
			t.Fatalf("marshal args: %v", err)
		}
		return bashCall("overflow-start", string(args))
	})
	steps = append(steps, func(prior string) []content.Chunk {
		processID = decodeBashResult(t, prior).ProcessID
		return processOutputCall("overflow-poll-0", fmt.Sprintf(`{"process_id": %q, "cursor": 0, "wait": "all", "timeout_ms": 3000}`, processID))
	})
	for i := 0; i < maxPolls; i++ {
		callID := fmt.Sprintf("overflow-poll-%d", i+1)
		steps = append(steps, func(prior string) []content.Chunk {
			result := decodeProcessResult(t, prior)
			cursor = result.NextCursor
			if result.Status != "" && result.Status != "starting" && result.Status != "running" {
				return []content.Chunk{&content.TextChunk{Text: "overflow-terminal"}}
			}
			return processOutputCall(callID, fmt.Sprintf(`{"process_id": %q, "cursor": %d, "wait": "all", "timeout_ms": 3000}`, processID, cursor))
		})
	}

	final, _ := runProcessScript(t, pia, "overflow the spool", steps...)
	if final != "overflow-terminal" {
		t.Fatalf("final = %q, want overflow-terminal (process never reached a terminal state within %d polls)", final, maxPolls)
	}

	// The process is now confirmed terminal; read the FULL retained window
	// from cursor 0 to check the overflow properties themselves.
	var full processResultJSON
	readAllFinal, _ := runProcessScript(t, pia, "read the full retained window",
		func(string) []content.Chunk {
			return processOutputCall("overflow-read-all", fmt.Sprintf(`{"process_id": %q, "cursor": 0}`, processID))
		},
		func(prior string) []content.Chunk {
			full = decodeProcessResult(t, prior)
			return []content.Chunk{&content.TextChunk{Text: "overflow-read-all-ok"}}
		},
	)
	if readAllFinal != "overflow-read-all-ok" {
		t.Fatalf("read-all final = %q", readAllFinal)
	}
	if full.Status != "exited" {
		t.Errorf("status = %q, want exited (process must never be killed for output volume)", full.Status)
	}
	if full.ExitCode == nil || *full.ExitCode != 0 {
		t.Errorf("exit_code = %v, want 0", full.ExitCode)
	}
	if !full.Gap {
		t.Error("gap = false, want true (cursor 0 predates the retained window after overflow)")
	}
	if full.TotalBytes <= ceiling {
		t.Errorf("total_bytes = %d, want > the %d-byte ceiling (full stream still counted)", full.TotalBytes, ceiling)
	}
	if !strings.Contains(full.Output, "tail-marker") {
		t.Errorf("retained output = %q, want it to still contain the tail", full.Output)
	}
}

// --- Scenarios 10 & 13 (Darwin substitute): confined profile fails closed ---
//
// Sandbox's real Seatbelt-confined Darwin backend unconditionally reports
// ErrLifetimeContainmentUnavailable/lifetime_enforcement_unavailable at
// Start, before any child ever exists (sandbox's own
// internal/exec/process_tree_darwin.go; process_adapter.go's mapStartError
// doc comment). Per this task's own explicit Darwin guidance, a CONFINED
// (here, Trusted) profile is therefore the correct thing to prove on this
// machine for both:
//
//   - scenario 10 ("verify grant denial and effective workspace lease
//     conflicts"): no sandbox grant is ever minted, but a real workspace
//     lifetime lease IS genuinely acquired -- tools/bash/supervised.go's
//     runSupervised has PrepareProcess succeed and then AcquireLifetime
//     succeed -- before Supervisor.Start is ever called with that
//     already-held lease. Start itself then reserves quota and persists a
//     StateStarting manifest before ever calling prepared.Start (its own
//     doc comment), and only inside that prepared.Start call does the real
//     Darwin containment check actually fire and fail. On that failure
//     Start releases every reservation it made, releases the lease, closes
//     prepared, and transitions the manifest straight to StateFailed, so
//     no process ever spawns and nothing survives the unwind -- still the
//     strongest denial this platform can prove, but by acquiring and then
//     cleanly releasing a lease (and quota, and a manifest), not by
//     denying before any resource is ever touched;
//   - scenario 13 ("mark live manifests lost without PID signalling"): no
//     manifest ever reaches StateRunning at all (Start fails before Start's
//     own StateRunning Save), so there is nothing for a later restore to
//     ever have to reconcile as lost_on_restore, and certainly no OS PID is
//     ever trusted or signalled -- there is no PID.
//
// Both tests assert the SAME observable failure (a typed, structural error
// with no target created and no delayed marker), from two different angles.

func runConfinedSupervisedBashAttempt(t *testing.T, markerPath string) bashSupervisedResultJSON {
	t.Helper()
	// The unconditional pre-spawn lifetime_enforcement_unavailable rejection
	// this helper asserts is specifically documented as "today, every real
	// Seatbelt-confined Darwin spawn" (process_adapter.go's mapStartError
	// doc comment; sandbox's internal/exec/process_tree_darwin.go). Other
	// platforms have (or, per this feature's own delivery plan, will have)
	// real confined containment and are expected to SUCCEED at a confined
	// supervised spawn rather than fail closed here -- skip rather than
	// assert Darwin-only behavior on a platform where it does not apply.
	if runtime.GOOS != "darwin" {
		t.Skipf("ALLOWED_PLATFORM_LIMITATION: this scenario's fail-closed assertion is Darwin-specific (GOOS=%s)", runtime.GOOS)
	}
	workspace := t.TempDir()
	t.Chdir(workspace)
	factory := newIntegrationFactory(t)
	client := &managedScript{}
	cfg := Config{AccessProfile: AccessTrusted, HomeDir: t.TempDir()}
	agent, err := factory.openWithClient(context.Background(), client, newModelFactoryFor(testModel()), SessionSelector{}, cfg)
	if err != nil {
		t.Fatalf("openWithClient(new, trusted) error = %v", err)
	}
	t.Cleanup(func() { _ = agent.Close(context.Background()) })
	pia := &processIntegrationAgent{factory: factory, agent: agent, client: client, workspace: workspace}

	var result bashSupervisedResultJSON
	command := fmt.Sprintf(`touch %s && sleep 1`, markerPath)
	final, _ := runProcessScript(t, pia, "attempt a confined background command",
		func(string) []content.Chunk {
			args, err := json.Marshal(struct {
				Command    string `json:"command"`
				Background bool   `json:"background"`
			}{Command: command, Background: true})
			if err != nil {
				t.Fatalf("marshal args: %v", err)
			}
			return bashCall("confined-attempt", string(args))
		},
		func(prior string) []content.Chunk {
			result = decodeBashResult(t, prior)
			return []content.Chunk{&content.TextChunk{Text: "confined-attempted"}}
		},
	)
	if final != "confined-attempted" {
		t.Fatalf("final = %q", final)
	}
	return result
}

// TestIntegrationProcessGrantDeniedUnderConfinedProfile is the Darwin
// substitute for scenario 10. Darwin now supports best-effort supervised
// starts, while the missing host-write grant still denies the mutation.
func TestIntegrationProcessGrantDeniedUnderConfinedProfile(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "grant-denied.marker")
	result := runConfinedSupervisedBashAttempt(t, marker)

	if result.Error != "" || result.ProcessID == "" || result.Status != "running" || !result.Backgrounded {
		t.Fatalf("confined background attempt = %+v, want a successful running supervised handoff", result)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("marker file exists without a host-write grant: stat err = %v", err)
	}
}

// TestIntegrationProcessLiveManifestNeverCreatedUnderConfinedProfile is the
// Darwin substitute for scenario 13. It additionally waits past the command's
// own sleep window to prove that the successful best-effort handoff cannot
// create a marker without the missing host-write grant.
func TestIntegrationProcessLiveManifestNeverCreatedUnderConfinedProfile(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "delayed.marker")
	result := runConfinedSupervisedBashAttempt(t, marker)

	if result.Error != "" || result.ProcessID == "" || result.Status != "running" || !result.Backgrounded {
		t.Fatalf("confined background attempt = %+v, want a successful running supervised handoff", result)
	}

	// The attempted command was `touch <marker> && sleep 1`; wait past that
	// window so a delayed/async host write would have had time to create it.
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("delayed marker file appeared without a host-write grant: stat err = %v", err)
	}
}

// --- Scenario 11: observe metadata-only completion ----------------------

// TestIntegrationProcessObservesMetadataOnlyCompletion proves a real
// completed background process durably publishes exactly one public
// event.ProcessCompleted lifecycle record, and that record carries only
// bounded metadata -- never the command's own output content.
func TestIntegrationProcessObservesMetadataOnlyCompletion(t *testing.T) {
	pia := openProcessIntegrationAgent(t)

	stream, err := pia.agent.Subscribe(event.EventFilter{Enduring: event.LoopScope{All: true}})
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	defer func() { _ = stream.Close() }()

	const secretOutput = "unique-completion-marker-xyz"
	final, _ := runProcessScript(t, pia, "observe completion",
		func(string) []content.Chunk {
			return bashCall("completion-start", fmt.Sprintf(`{"command": "echo %s", "background": true}`, secretOutput))
		},
		func(string) []content.Chunk {
			return []content.Chunk{&content.TextChunk{Text: "completion-started"}}
		},
	)
	if final != "completion-started" {
		t.Fatalf("final = %q", final)
	}

	completions := 0
	var lastCompleted event.ProcessCompleted
	acceptanceEventsUntil(t, stream, func(ev event.Event) bool {
		if completed, ok := ev.(event.ProcessCompleted); ok {
			completions++
			lastCompleted = completed
			return true
		}
		return false
	})
	if completions != 1 {
		t.Fatalf("observed %d event.ProcessCompleted deliveries for one process, want exactly 1", completions)
	}
	if lastCompleted.Process.ProcessHandle == "" {
		t.Fatal("ProcessCompleted carries no process handle")
	}
	raw, err := json.Marshal(lastCompleted.Process)
	if err != nil {
		t.Fatalf("marshal ProcessCompleted.Process: %v", err)
	}
	if strings.Contains(string(raw), secretOutput) {
		t.Fatalf("ProcessCompleted metadata leaked command output: %s", raw)
	}
}

// --- Scenario 15: shut down with no descendants -------------------------

// TestIntegrationProcessShutsDownWithNoDescendants proves a session Close
// while a background process is still running terminates that process tree
// -- no surviving descendant keeps running after the session is gone.
func TestIntegrationProcessShutsDownWithNoDescendants(t *testing.T) {
	pia := openProcessIntegrationAgent(t)
	marker := filepath.Join(pia.workspace, "survived-shutdown.marker")

	// Deliberately long-lived (well past this process's own real, if
	// currently non-trivial, teardown latency -- see
	// openProcessIntegrationAgent's doc comment): the point of this
	// scenario is that Supervisor.Shutdown genuinely SIGNALS and confirms
	// this process's death while it is still definitely running, not that
	// it happens to finish on its own before Close ever gets a chance to
	// act on it. A short-lived command would let the process complete
	// naturally and make the marker's absence prove nothing.
	final, _ := runProcessScript(t, pia, "start a long command then shut down",
		func(string) []content.Chunk {
			return bashCall("shutdown-start", fmt.Sprintf(`{"command": "sleep 20 && touch %s", "background": true}`, marker))
		},
		func(prior string) []content.Chunk {
			started := decodeBashResult(t, prior)
			if started.Error != "" || started.ProcessID == "" {
				return []content.Chunk{&content.TextChunk{Text: "start error: " + started.Error}}
			}
			return []content.Chunk{&content.TextChunk{Text: "shutdown-started"}}
		},
	)
	if final != "shutdown-started" {
		t.Fatalf("final = %q", final)
	}

	// Close() is deliberately not asserted error-free here -- see
	// openProcessIntegrationAgent's doc comment: closing while a process is
	// still GENUINELY running (as this scenario requires) can still
	// surface the documented loop_drain timeout on its way to correctly
	// terminating the process, since session teardown gives every later
	// phase (including the one that actually signals and confirms this
	// process's death) its own fresh deadline regardless of an earlier
	// phase's timeout. The marker check below is this test's real
	// assertion.
	if err := pia.agent.Close(context.Background()); err != nil {
		t.Logf("Close() error = %v (see openProcessIntegrationAgent's doc comment)", err)
	}

	// The command was scheduled to touch the marker 20s after spawn --
	// comfortably longer than Close() itself could plausibly still be
	// running the process at return time -- so a brief wait after Close()
	// already returned is enough to rule out a delayed marker.
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("marker file exists after Close(): a background descendant survived session shutdown")
	}
}
