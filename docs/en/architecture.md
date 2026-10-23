# GoAgent Architecture Deep-Dive

> [简体中文](../architecture.md) | English

> A detailed description of GoAgent's internals, aligned with Claude Code's core design.

---

## 1. Agent Loop: The Core State Machine

Location: `internal/loop/loop.go`

### 1.1 Eight phases per iteration

```
① Preprocessing (4-layer compaction) → ② Token blocking check → ③ Build API tool definitions
→ ④ Call the API (streaming) → ⑤ Consume the stream + start tools as they stream in
→ ⑥ Abort check (synthesize tool_result) → ⑦ Exit decision / recovery branches
→ ⑧ Tool execution + post-processing
```

### 1.2 Reasons to continue to the next iteration

| Trigger | Behavior |
|---|---|
| Normal tool call completed | Continue to the next turn |
| Primary model overloaded, fallback configured | Switch to the fallback model and retry |
| Context too long (413) | Fold/drain or reactively compact, then retry |
| Output truncated by max_tokens | Inject a continuation hint (up to 3 times; max_tokens is *not* raised — some OpenAI-compatible backends 400 outright on explicitly large values) |
| Reasoning model only outputs in the thinking channel | Inject a hint asking it to restate in the main channel (up to 2 times) |
| Stream interrupted | Freeze what arrived as an anchor, inject a continuation hint, reopen the stream (error out only after 3 consecutive failures) |
| Stop hook blocks exit | Inject a corrective message |

### 1.3 Exit conditions

- `completed` — the LLM returned end_turn with no tool calls
- `aborted` — the context was cancelled (user interruption)
- `context_exhausted` — tokens exceeded the hard limit with no compactor
- `max_turns` — reached the maximum number of turns
- `model_error` — unrecoverable API error

### 1.4 Synthesized tool results

When the agent is interrupted (ctx.Err()), the loop automatically synthesizes a tool_result for every outstanding tool_use:

```
assistant: [tool_use id="abc" name="Bash"]
user:      [tool_result id="abc" content="Tool execution was interrupted by the user" is_error=true]
```

This preserves the API's message-pairing requirement (every tool_use must have a matching tool_result).

---

## 2. Transcript vs Model View

Location: `internal/loop/loop.go`, `compaction/boundary.go`

The loop maintains two message lists simultaneously:

- **Transcript**: append-only. Every message entering the conversation gets a UID and is synchronously handed to persistence via `Config.OnMessage`. Auto-compaction (L4) does not rewrite it — instead it appends a **summary record**: `IsCompactSummary`, with `CompactBoundary` pointing at the first message kept after the summary; it is also a meta message (`IsMeta`), filtered out by the presentation layer.
- **Model view**: the messages actually sent to the model each request. It is processed by the compaction layer (§3) at the start of every iteration; L0–L3 are deterministic copy-on-write transforms that only touch the view.

At run start, the view is produced by `compaction.Project(InitialMessages)`: the last summary record plus everything after its boundary. So reloading a session, a queue-continuation over the same event stream, or a host passing history back via `RunWithHistory` restores the view directly without asking the model to summarize again. `Loop.FinalMessages()`, `EventDone.Messages`, and `WithTranscriptSink` all return the transcript.

---

## 3. Compaction System

Location: `compaction/`

### 3.1 Layer design

| Layer | Constant | Name | Location | Mechanism | Calls the model |
|---|---|---|---|---|---|
| 0 | `LayerBudget` | Budget | `compaction.go` | Truncate oversized tool results | no |
| 1 | `LayerSnip` | Snip | `compaction.go` | Truncate the oldest messages | no |
| 2 | `LayerMicro` | Micro | `compaction.go` | Drop already-consumed tool results | no |
| 3 | `LayerCollapse` | Collapse | `compaction.go` | Fold middle turns | no |
| 4 | `LayerAuto` | Auto | `compaction.go` | Ask the model to summarize | yes |

### 3.2 Composable layers

`Config.Layers` enables compaction layers selectively:

```go
// All enabled (backward compatible; Layers nil or empty)
compaction.NewManager(compaction.Config{})

// Only L0+L2+L4 (pipeline worker scenario)
compaction.NewManager(compaction.Config{
    Layers: []compaction.Layer{
        compaction.LayerBudget,
        compaction.LayerMicro,
        compaction.LayerAuto,
    },
})
```

`Apply()` checks `enabledLayers[LayerXxx]` before each layer and skips disabled ones.

> **Note**: Layer 0 Budget currently only truncates (keeping head and tail); true disk persistence + reference replacement is not implemented yet (marked TODO).

### 3.3 Circuit breaker

After 3 consecutive autocompact failures, compaction stops being attempted (`MAX_CONSECUTIVE_AUTOCOMPACT_FAILURES = 3`).

### 3.4 Summary records and projection

See §2. After an L4 summary succeeds, the summary message carries `CompactBoundary`; the loop appends it to the transcript as a summary record. `compaction.Project` restores the view from the transcript. When the boundary cannot be found in the transcript (truncated or rewritten), projection gives up and compaction re-summarizes on demand.

### 3.5 Image results

Tool results containing inline images (`[IMAGE <mime> <base64>]`) are never head/tail truncated — that would corrupt the base64 and the request would 400. Images within the last 40 messages are kept intact; older ones are recycled into placeholders. The placeholder includes the original image path (Windows or POSIX absolute) found in the text after the image marker.

### 3.6 Post-compact reminder

Reminder sources registered via `WithPostCompactReminder` are invoked after every actual compaction; the returned text is appended after the summary as a user message, re-injecting key context that was folded into the summary. These reminders belong only to this run's view — they never enter the transcript.

---

## 4. Tool Execution Engine

### 4.1 StreamingExecutor

Location: `executor/executor.go`

Tool calls start executing while the API is still streaming:

```
API stream:      ──text──tool_use_A──text──tool_use_B──done──
Tool execution:          ────A running────          ──B running──
                                                      ──wait for stragglers──
```

Partitioning: consecutive concurrency-safe tools form parallel batches; non-concurrent tools run serially.

Tool panics are recovered into error results; queued tools are not started after ctx cancellation; there is a bound (`CancelGrace`) on how long running tools are waited for.

### 4.2 Effect classes

`ToolDef.Effect` (`executor.Class`) determines orchestration:

| Class | Meaning | Parallel rule |
|---|---|---|
| `ClassReadOnly` | No side effects | May run in parallel at any time, including during exclusive tools |
| `ClassParallel` | Side effects but concurrent calls don't conflict | May overlap with parallel and read-only tools, not with exclusive ones |
| `ClassExclusive` | Global side effects (file writes, git, Bash) | Runs exclusively |

When `Effect` is unset it is derived from `Concurrent`: true → Parallel, false → Exclusive.

### 4.3 Pre-execution check chain

Before every tool call, in order:
1. Input validated against the tool's JSON Schema (`WithToolInputValidation(false)` disables);
2. Plan-mode filtering;
3. Middleware `BeforeTool`;
4. Hooks `PreToolUse`;
5. Permission gate (§5).

Any rejection means the tool does not run; the reason is handed to the model as the tool result.

### 4.4 Result truncation

`MaxResultSizeChars` caps each tool result's character count; overflow is truncated automatically.

---

## 5. Permission System

Location: `permission/`

### 5.1 Three-state decisions

```
PermissionBehavior:
  Allow — pass through
  Deny  — reject outright
  Ask   — needs user confirmation
```

### 5.2 Six permission modes

| Mode | Behavior |
|------|---------|
| Default | ReadOnly→allow, Normal→ask, Dangerous→ask |
| AcceptEdits | ReadOnly + edit-class tools (`FileEdit: true`)→allow (except high-risk edits), rest→ask |
| Auto | ReadOnly / allowlisted→allow, everything else→judged by the YOLO classifier (ask when unavailable) |
| BypassPermissions | Normal ops→allow; high-risk ops (`permission.HighRisk`, ask-rule hits)→judged by the YOLO classifier (ask when unavailable) |
| Plan | Only ReadOnly tools |
| DontAsk | Anything that would need confirmation→deny |

### 5.3 Rule engine

Location: `permission/rules.go`, `permission/subject.go`

```
Rule evaluation priority: deny rules > ask rules > allow rules
```

Pattern syntax:
- `"Bash"` — matches the tool name
- `"Bash(git *)"` — matches tool name + a value extracted from the input (command, path, URL, ...)
- `"*"` — matches all tools
- `"Read*"` — tool-name prefix match

Shell commands are split into subcommands at control operators and matched one by one: deny/ask fire if any subcommand matches; allow requires every subcommand covered.

### 5.4 "Always allow" grants

After the user checks "always allow", the grant is recorded on the gate's `permission.Grants`: plain tools by tool name, shell commands by "tool name + the first one or two words of the command". All runs of the same session share one grant set (§9); grants never cross sessions.

---

## 6. Token Estimation

Location: `message/token.go`

### Type-aware density

| Type | Density |
|------|---------|
| Text | chars / 4 |
| JSON (tool_use, tool_result) | chars / 2 |
| Images | 2000 tokens (fixed) |
| Thinking | chars / 3 |

### Parameters

- **4/3 padding factor**: all estimates are multiplied by 4/3
- **Per-message overhead**: +10 tokens per message
- **Reserved summary tokens**: 20,000 (reserved for autocompact)

### Warning states

| State | Threshold |
|------|-----------|
| OK | < 80% |
| Warning | 80%–90% |
| Error | 90%–95% |
| Blocking | > 95% |

---

## 7. Data Flow

```
User input
    │
    ▼
┌─────────────────┐
│   goagent.App   │ ←── tool registration, options
│   Run(ctx, msg) │
└────────┬────────┘
         │
         ▼
┌─────────────────┐
│  internal/loop  │ ←── preprocessing, API calls, tool execution, post-processing
│   Loop.Run()    │
└────────┬────────┘
         │ events
         ▼
┌─────────────────┐
│  Event Channel  │ → CLI display / HTTP SSE / SDK consumers
└─────────────────┘
```

---

## 8. Sub-runs and Scopes

Location: `internal/loop/scope.go`

Pipeline nodes & supervisors, team leaders & members, and subagents (`agent.Runner`) all use the same loop implementation.

- Each loop hands its **scope** (`loop.Scope`) to every tool execution via ctx. The scope contains:
  - The check chain: permission gate, plan mode, hooks, middleware;
  - The input-validation switch;
  - Observability outlets: observer, logger, debug, session ID;
  - The primary provider and fallback.
- Sub-loops spawned inside tools are created via `loop.NewChild(ctx, cfg)`:
  - The check chain always comes from the scope — a sub-run can never be less strict than its parent;
  - Observability outlets and session ID are inherited when unset;
  - With no provider specified, the parent's provider is used; if identical to the parent's, the fallback is inherited too;
  - Sub-loops are marked `Nested` and do not emit session start/end events.
- Two pipeline exceptions: pipelines started by the host outside any run, and those with `PipelineConfig.TrustedTools` set, do not inherit the check chain (host trust). `create_pipeline` / `run_team` orchestrated by the model build a fresh scope from App config when not inside a permission-gated run.
- Sub-run nesting is capped at `MaxSubRunDepth = 3`.

```
App.run ── loop.New (main loop) ──┬── tool ctx carries the Scope
                                  │
            create_pipeline ──────┼── pipeline nodes: loop.New + Scope.ApplyTo
            run_team ─────────────┼── leader / members: loop.NewChild
            subagent tools ───────┴── agent.Runner: loop.NewChild
```

---

## 9. Session Runtime

Location: `sessionruntime.go`

Every session has a runtime that persists across runs:
- The tool key-value store (`Context.Store` / `Load`);
- The compaction manager (auto-summary circuit-breaker state);
- "Always allow" grants;
- The session-level runtime permission mode.

Runtimes for real sessions are registered on the App until `App.CloseSession`. Runs without session identity (`Run`, `RunWithHistory`) each get a fresh, isolated runtime.

The App tracks the permission gates of all in-flight runs:
- `SetPermissionMode` applies to all in-flight runs (except where the mode comes from session-level settings);
- `SetSessionPermissionMode` applies to that session only;
- Guard additions/removals apply to all in-flight runs.

Permission-mode priority: session-level runtime setting > `WithSessionPermissionMode` resolver > global runtime override > `WithPermissionMode`.

---

## 10. Session Event Log & Interaction Routing

Location: `sessionlog.go`, `streambus.go`, `http.go`, `agui.go`

Since v0.14, every frame of `/chat` comes from a **per-session append-only event log** (`sessionlog.go`): a ring buffer (default 2000 frames or 8 MiB) whose frames carry a monotonically increasing per-session `session_seq` (the seq base is the log's creation UnixMicro timestamp, so numbering continues after the log is released and rebuilt). Readers are cursors (`SessionWatcher`); publishing never blocks; cursors that fall too far behind receive a `resync` marker. Both the disconnect-resume endpoint `GET /runs/{session}/stream?after=` and the AG-UI profile are built on top of it.

**Interaction routing** for approvals and questions has three priority tiers: explicit subscriptions (`PermissionHandler` / `AskUserHandler` `Subscribe` — one connection bound to one session) → the session log (installed in HTTP mode: requests are published into the log, every connection attached to the session receives them, and they re-arrive on reconnect via replay) → the shared channel (fallback for direct SDK consumption). This eliminates the frame-stealing and goroutine-leak problems of the old "one forwarder goroutine per connection consuming a shared channel" model.

```
Run event stream ──tee──▶ session event log ──▶ /chat (frameWriter carries id: session_seq)
                              │                  /runs/{session}/stream?after=
                              └──▶ approval/question requests (installInteractionRouting)
AG-UI: /agui consumes the same log, mapped to AG-UI events (aguiMapper);
       approvals/questions/frontend tools end the current stream with a "park"
       (aguiParks records kernelRunID + cursor + timeout);
       resume / tool messages continue the output from the cursor
```

Subagents register on the session runtime by tool_use_id (`subagents.go`): queryable (`Subagents`), steerable (a per-run steering lane via `loop.WithChildSteering`), individually interruptible (`InterruptAgent` via `context.WithCancelCause` cancels only that subagent).
