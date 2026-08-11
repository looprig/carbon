# pkg/hook

`pkg/hook` defines deterministic, typed, in-process interception for bounded
Harness operations. Use guards for synchronous policy and around hooks for
tracing, metrics, or other observation. Durable lifecycle facts remain on the
[`pkg/event`](../event/README.md) stream.

## Install hooks on a rig

```go
hooks := hook.Set{
    PolicyRevision: "tool-safety-v3",
    Guards: []hook.Guard{{
        Operation: hook.OperationToolCall,
        Check: func(ctx context.Context, call hook.Call) error {
            if unsafe(call.ToolCall.ToolName, call.ToolCall.ArgsJSON) {
                return hook.Deny("unsafe_tool", "tool call rejected by policy")
            }
            return nil
        },
    }},
    Around: []hook.Around{{
        Operation: hook.OperationInference,
        Begin: func(ctx context.Context, call hook.Call) (context.Context, hook.FinishFunc) {
            ctx, span := tracer.Start(ctx, "harness.inference")
            return ctx, func(result hook.Result) {
                recordOutcome(span, result)
                span.End()
            }
        },
    }},
}

r, err := rig.Define(
    rig.WithLoops(agent),
    rig.WithPrimers("agent"),
    rig.WithSessionStore(store),
    rig.WithHooks(hooks),
)
```

`rig.Define` validates, defensively copies, and compiles the set once. The
compiled runner is immutable and is installed on every native loop and journal
created or restored by that rig. Mutating the original slices later has no
effect.

## Operations and policy

There are eight operations; four are guardable:

| Operation | Guardable | Meaning |
| --- | --- | --- |
| `Turn` | yes | One selected user-input turn |
| `Step` | no | One inference/tool step |
| `Inference` | yes | One native streaming model request |
| `Compaction` | yes | One transcript-compaction attempt |
| `ToolCall` | yes | One semantic tool call, including policy |
| `GateWait` | no | Time actually waiting for a gate answer |
| `ToolExecution` | no | Approved invocation of tool code |
| `JournalAppend` | no | One checked durable append |

Guards run sequentially in registration order and the first error stops the
operation. Any error blocks: a validated `*hook.Denial` is an intentional policy
decision, while every other error or panic is an internal guard failure. This
fail-closed default prevents a broken policy dependency from silently granting
access.

Use `hook.Deny(code, reason)` to construct a denial. Codes are bounded lowercase
ASCII machine identifiers; reasons are bounded, valid UTF-8, nonblank text
without control characters. `hook.AsDenial` is the only supported classifier:
it follows wrapping, revalidates exported fields, and returns an independent
copy. A malformed directly constructed `Denial` remains an internal failure.

`PolicyRevision` is required exactly when guards are present. It enters
`SessionStarted.Manifest.HookPolicyRev`, so changing guard behavior requires a
new revision and restore reports `event.DriftHookPolicy` at `Warn`. Around-only
sets keep the field empty and do not affect configuration identity. Revisions
are bounded to 128 bytes and must be valid UTF-8 without control characters.

## Around-hook execution

Matching `Begin` callbacks run in registration order. Each returned context is
passed to later callbacks, guards, the operation, nested operations, and durable
appends made by that operation. Matching finish callbacks run in reverse order,
once each. Different operations may dispatch concurrently, so callback code
must be concurrency-safe.

Harness retains the prior context's cancellation and deadline even if a
callback returns a detached context. A nil returned context is ignored. Begin
and finish panics are isolated and logged without hook payloads; an observer
cannot alter the operation result.

The operation owner must always call the returned finish function, including
guard denial, failure, cancellation, and panic-normalization paths. Besides
delivering the terminal snapshot, finish releases cancellation links that the
runner may have installed. Runtime operation boundaries already enforce this
rule; direct `Runner.Start` users inherit the same obligation.

Every callback receives an independent, read-only `Call` or `Result` snapshot.
Mutable messages, requests, JSON arguments, compaction data, and tool results
are cloned. `Result.Err` is deliberately the original trusted in-process error.
Raw messages, arguments, results, and errors may contain sensitive data:
redaction is mandatory before logging, telemetry export, or another trust
boundary.

On terminal `ToolCall` results, `ToolCall.Result` contains the full normalized
tool result, including pre-execution failures; `ResultPreview` remains the
bounded display projection. On terminal `ToolExecution` results,
`ToolExecution.Result` contains only the approved invocation's result.

## Hooks versus events

Hooks surround attempts and can propagate context before an outcome is known.
Events record committed facts. An operation finishing does not prove that a
corresponding durable event exists; use the event stream for replay, audit, and
state reconstruction.

`JournalAppend` observes new checked appends exactly once, including lifecycle
opening fences. Restore does not replay historical operations or historical
appends through hooks. A restored session uses the newly supplied immutable
runner for its opening fence and all new work.

Durable appends initiated inside an operation inherit that operation's derived
context. If a `GateWait` observer cancels only its derived wait context, Harness
removes and closes the already-installed gate before the blocked tool call can
continue.

Operation hooks are native-loop semantics. Foreign loop builders do not receive
the runner and therefore do not produce native `Turn`, `Step`, `Inference`,
`Compaction`, or tool hooks. Harness-owned journal appends around those loops
remain observable. Hustle inference is also a separate execution plane and does
not produce native `Inference` hooks; a compaction driven by a hustle is
observed at the enclosing native `Compaction` boundary.

## Direct runner use

Most applications should use `rig.WithHooks`. Lower-level integration code may
compile and dispatch directly:

```go
runner, err := hook.Compile(set)
ctx, finish, err := runner.Start(ctx, call)
if finish != nil {
    defer finish(result)
}
```

`Compile` owns the registration slices. `Start` validates and clones the call,
runs observers and guards, and returns an exactly-once aggregate finish
function. Direct callers must supply a valid terminal `Result` for the same
operation and must not omit finish when it is returned.
