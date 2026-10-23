# GoAgent Internal Mechanisms

> [简体中文](../mechanisms.md) | English

> A detailed description of the mechanisms introduced in GoAgent v0.4 and later.

---

## 1. Task/Todo V2 System

Location: `task/task.go`

### Features
- Task CRUD: Create / Update / Get / List / Delete
- Status management: pending → in_progress → completed (or deleted)
- Dependency management: two-way BlockedBy / Blocks links
- Auto-increment IDs (string)
- In-memory storage (session-scoped)

### Custom store backends

Implement `task.StoreInterface` to inject a custom backend (database, Redis, etc.):

```go
app := goagent.New(
    goagent.WithTaskTools(),
    goagent.WithTaskStore(myDBTaskStore{}),
)
```

Likewise, the Plan system accepts a `plan.StoreInterface` via `WithPlanStore()`.

### Data structure
```go
type Task struct {
    ID          string
    Subject     string         // short title (imperative)
    Description string         // details
    Status      Status         // pending/in_progress/completed/deleted
    Owner       string         // responsible agent
    ActiveForm  string         // text shown while in progress
    Metadata    map[string]any // custom metadata
    BlockedBy   []string       // blocking dependencies
    Blocks      []string       // things this blocks
}
```

### Corresponding tools

Disabled by default; enable via the `WithTaskTools()` option:

```go
app := goagent.New(goagent.WithTaskTools())
```

- `TaskCreate` — create a task
- `TaskUpdate` — update status/fields/dependencies
- `TaskGet` — task details
- `TaskList` — list all task summaries

---

## 2. Plan Mode System

Location: `plan/plan.go`

### Lifecycle

```
Inactive → [EnterPlanMode] → Active → [write plan] → [ExitPlanMode] → Approved → [execute] → Inactive
                                    [Cancel] → Inactive
```

### Core behavior
- Only ReadOnly tools are allowed in plan mode (Read, Glob, Grep, ...)
- Plan files are stored at `.claude/plans/<random-name>.md`
- Random name format: `smooth-tumbling-unicorn`
- The system prompt gets plan-mode hints injected
- Existing plans can be loaded from files

### Enabling

Disabled by default; enable via `WithPlanTools()`:

```go
app := goagent.New(goagent.WithPlanTools())
```

Web apps can inject a custom store backend:

```go
app := goagent.New(
    goagent.WithPlanTools(),
    goagent.WithPlanStore(myDBPlanStore{}),
)
```

---

## 3. Skill System

Location: `skill/skill.go`

### Skill source priority

```
Builtin > Project (.claude/commands/) > User (~/.claude/commands/)
```

### Skill discovery
- Scans `.claude/commands/*.md`
- File name = skill name
- First non-empty non-heading line = description

### Skill execution
- **Inline**: the skill content is injected into the current conversation as a prompt
- **Fork**: executed in a separate agent (not yet implemented)
- The `$ARGUMENTS` placeholder is replaced with the user's arguments

---

## 4. Cron Scheduling System

Location: `cron/cron.go`

### Characteristics
- Standard 5-field cron: `minute hour dom month dow`
- Supports `*`, `*/N`, `N-M`, `N,M,L`
- Session-only lifecycle (not persisted to disk)
- Recurring jobs auto-expire after 3 days
- One-shot jobs expire after 24 hours
- Jitter: recurring ±15min, one-shot ±90s

### API
```go
scheduler := cron.NewScheduler(onFire)
scheduler.Start(ctx)

job, _ := scheduler.Create("0 9 * * 1-5", "check deploy status", true)
scheduler.Delete(job.ID)
scheduler.List()

scheduler.Stop()
```

---

## 5. Worktree Isolation

Location: `worktree/worktree.go`

### Workflow

```
Main repo → [Enter] → .claude/worktrees/<name>/ (new branch claude-worktree-<name>)
                           ↓
                    [work...]
                           ↓
         [Exit keep] → keep the worktree and branch
         [Exit remove] → delete the worktree and branch
```

### Safety checks
- Uncommitted changes are checked on exit-remove
- `discardChanges=true` is required to force-delete a worktree with changes

---

## 6. Extended Thinking

Location: `thinking/thinking.go`

### Three modes

| Mode | Behavior |
|------|---------|
| Adaptive | Decided automatically from tool count / context size |
| Enabled | Always on |
| Disabled | Off |

### Ultrathink
Ultrathink mode activates when budget > 10,000 tokens.

### Adaptive rules
- Tool count > 5 → enable
- Context > 50k tokens → enable
- Context usage > 80% → budget halved
- Context usage > 90% → budget reduced to 1000

---

## 7. Dynamic System Prompt

Location: `sysprompt/sysprompt.go`

### Multi-section assembly

```go
builder := sysprompt.NewBuilder()
builder.AddBasePrompt("You are a helpful assistant")
builder.AddEnvironmentInfo()   // platform, arch, shell
builder.AddCurrentDate()        // 2026-04-03
builder.AddGitStatus()          // branch, status
builder.AddMemory(claudemd)     // CLAUDE.md content
builder.AddMCPInstructions(mcp) // MCP server instructions

text, sections := builder.Build()
```

### Prompt caching
Each section can set `CacheControl{Type: "ephemeral"}` independently. The last section automatically gets cache_control if none was set.

---

## 8. Rate Limit / Retry

Location: `retry/retry.go`

### Retry policy
- Exponential backoff: 1s → 2s → 4s → ... → 30s (cap)
- Jitter: ±10%
- Max retries: 3
- Retries only: 429 (RateLimitError), 529 (OverloadError), 5xx (ServerError)
- Never retries: other 4xx
- Honors the `retry-after` header

### Generic API

```go
result, err := retry.Retry(ctx, retry.DefaultConfig(), func(ctx context.Context) (T, error) {
    return apiCall(ctx)
})
```

---

## 9. Token Budget

Location: `budget/budget.go`

### Features
- Total budget cap: `TotalBudget` (0 = unlimited)
- Per-turn recording: `RecordUsage(turn, input, output)`
- Status checks: OK / Warning (<20% remaining) / Exhausted / Diminishing

### Diminishing-returns detection
N consecutive turns (default 3) with output below MinOutputPerTurn (default 50 tokens) → automatic termination.

---

## 10. Auto Memory

Location: `memory/automemory.go`

### File layout

```
~/.claude/projects/<project>/memory/
├── MEMORY.md        # main file (auto-truncated to 200 lines)
├── debugging.md     # topic file
├── patterns.md      # topic file
└── preferences.md   # topic file
```

### Features
- `LoadMain()` — load MEMORY.md (auto-truncated)
- `Search(keywords)` — find relevant files by keyword
- `FormatForInjection()` — format as system-prompt injection text
- `FilterDuplicates()` — content-fingerprint based dedup

---

## 11. Recoverable Errors

Location: `internal/loop/loop.go`

The loop handles the following errors in place instead of failing the run:

| Cause | Trigger | Recovery |
|------|---------|---------|
| Context too long | 413 / over-length error | Fold/drain or reactively compact, then retry (once per turn) |
| Output truncated | `max_tokens` cutoff | Inject a continuation hint, up to 3 times |
| Stream interrupted | Network or SSE drop | Freeze what arrived as an anchor (real results of executed tools are kept), inject a continuation hint, reopen the stream; error out after 3 consecutive failures |
| Overload / rate limit | 529 / 429 | Retry honoring `Retry-After`; on overload with a fallback configured, switch to the fallback model |

---

## 12. Stop Hooks

Location: hooks package (`hooks/stophook_builtin.go`), wired by the loop

### When they run
After each turn ends (the LLM returned end_turn with no tool calls), before final exit.

### Built-in hooks
- `MaxOutputTokensStopHook` — detect incomplete output
- `TokenBudgetStopHook` — check the token budget

### Blocking mechanism
When a hook returns `Block=true`:
1. A corrective message is injected into the conversation
2. The `TransStopHookBlocking` transition is set
3. The loop continues (does not exit)

---

## 13. Hooks Framework

Location: `hooks/`

### 5 event types

| Event | Fires when |
|------|-----------|
| PreToolUse | Before a tool executes |
| PostToolUse | After a tool executes |
| Stop | At the end of each turn |
| PermissionRequest | On a permission request |
| SessionStart | When a session starts |

### Hook types
- `CommandHook` — runs a shell command; context passed via environment variables
- `FuncHook` — a Go function callback

---

## 14. Session Management

Location: `session/`

### State transitions

```
Idle → Running → Idle (normal completion)
              → RequiresAction (waiting on the user)
              → Suspended (paused)
              → Completed (finally done)
```

### JSONL persistence
One JSON object per line:
```jsonl
{"type":"message","timestamp":"...","data":{...}}
{"type":"metadata","timestamp":"...","data":{...}}
{"type":"boundary","timestamp":"...","data":{...}}
{"type":"state","timestamp":"...","data":{"state":"running"}}
```

### Restore
- Message history is rebuilt from the JSONL
- `EnsureToolResultPairing()` repairs orphaned tool_use entries (missing their tool_result)

---

## 15. Cost Tracking

Location: `cost/cost.go`

### Built-in pricing (2025)

| Model | Input/M | Output/M | Cache Read/M | Cache Write/M |
|------|---------|----------|-------------|--------------|
| claude-opus-4-6 | $15.00 | $75.00 | $1.50 | $18.75 |
| claude-sonnet-4-6 | $3.00 | $15.00 | $0.30 | $3.75 |
| claude-haiku-4-5 | $0.80 | $4.00 | $0.08 | $1.00 |

### API
```go
tracker := cost.NewTracker()
tracker.Record("claude-sonnet-4-6", 1000, 500, 200, 100)
fmt.Println(tracker.FormatSummary())
// Total cost: $0.0113 (66.67% input, 33.33% output)
// Tokens: 1000 input, 500 output, 300 cache
```

---

## 16. Prompt Caching

Location: `provider/provider.go`

### Request structure

```go
type Request struct {
    SystemBlocks []SystemBlock  // multi-part system prompt
    Tools        []ToolDefinition
    // ...
}

type SystemBlock struct {
    Text         string
    CacheControl *CacheControl  // {"type": "ephemeral"}
}

type ToolDefinition struct {
    Name         string
    InputSchema  any
    CacheControl *CacheControl  // tool-level caching
}
```

### Caching strategy
- The last 1–2 system blocks automatically get `cache_control: ephemeral`
- The last tool in the tool list gets `cache_control`
- Usage reports `CacheReadTokens` / `CacheCreateTokens`

---

## 17. Pipeline DAG Orchestration Engine

Location: `pipeline.go`

### Overview

Pipeline provides DAG orchestration with multi-node dependency scheduling, parallel workers, and supervisor review. Designed for multi-agent collaboration (e.g. storyboard generation → artwork → quality review).

### 17.1 Core configuration

#### PipelineConfig

```go
type PipelineConfig struct {
    Nodes      []PipelineNode     // DAG nodes
    Supervisor *PipelineAgentDef  // review agent (optional)
}
```

#### PipelineNode — user-facing fields

| Field | Type | Default | Meaning |
|------|------|---------|--------|
| `Name` | string | required | Unique node identifier |
| `Agent` | *PipelineAgentDef | required | The node's agent definition |
| `Concurrency` | int | 1 | Worker count. >1 auto-creates a message queue |
| `DependsOn` | []string | nil | Upstream node names; all must complete before start |
| `Message` | string | "" | Initial message (used when Concurrency=1) |
| `Injects` | []string | nil | Downstream nodes this node may push messages to |
| `MessageType` | any | string | Zero value of message queue elements |
| `ResultType` | any | string | Zero value of result queue elements |
| `Review` | bool | false | Whether supervisor review is required |
| `ReviewBatch` | int | 1 | Results accumulated per review |
| `MaxRetries` | int | 3 | Max retries after rejection |
| `OnResult` | func(any) | nil | Callback for approved results |
| `QueueSize` | int | 64 | Queue buffer size |

#### PipelineAgentDef — the sub-agent definition

| Field | Type | Default | Meaning |
|------|------|---------|--------|
| `Name` | string | required | Agent name |
| `Instruction` | string | required | System prompt |
| `Tools` | []NamedTool | nil | Available tools |
| `Provider` | provider.Provider | nil (inherits parent App) | LLM provider |
| `MaxTurns` | int | 100 | Max turns |

### 17.2 Scheduling

```
Topological sort → start dependency-free nodes → node completes → check downstream → start ready nodes → ... → all done
```

1. **Topological sort**: execution order computed from `DependsOn`; cycles detected
2. **Parallel start**: all zero-in-degree nodes start simultaneously
3. **Dependency propagation**: after a node completes, downstream dependencies are re-checked
4. **Queue injection**: cross-node queue communication declared via `Injects`

### 17.3 Worker modes

| Mode | Condition | Behavior |
|------|-----------|---------|
| Single worker | Concurrency=1 | Executes `Message` directly, returns the result |
| Multi worker | Concurrency>1 | N goroutines consume the message queue in parallel |

#### Lightweight loop

Pipeline workers use the same `loop.Loop` as the main loop, with:
- **Provider** + **Tools** + **SystemPrompt** + **MaxTurns** + **Executor**
- **Compaction**: composable, only L2 (Micro) + L4 (Auto) enabled
- **Check chain & observability**: when started inside an agent run, the run's scope (`loop.Scope`) is inherited — permission gate, plan mode, hooks, middleware, input-validation switch, plus observer, session ID and fallback model. Node tools are checked against their declared permission level (`PipelineConfig.TrustedTools` disables this). Pipelines started directly by host code skip the checks and inherit only the parent App's `hooksMgr`
- **Observer**: inherits the parent App's `obsRegistry` if present; node loops do not emit session start/end events

Skipped heavyweight subsystems: Memory, Budget, SessionMemory.

### 17.4 Supervisor review

When a node has `Review=true` and a `Supervisor` is configured, each review batch opens a dedicated conversation; the framework automatically injects two review tools (read-only level, no user approval):

| Tool | Description |
|------|------------|
| `approve_result` | Approve result at index in this batch, triggering OnTaskResult with the real result; unhandled results are auto-approved at end of turn |
| `reject_result` | Reject result at index + revision guidance; re-queued for retry (auto-approved past MaxRetries) |

#### Review flow

```
Worker finishes → handleResult → pending-review queue
     → ReviewBatch accumulated or message queue empty → one batch sent for review
     → Supervisor opens a conversation for the batch, approve/reject item by item
       (unhandled items auto-approved at end of turn)
     → reject: original message + guidance re-queued (up to MaxRetries)
     → upstream queues all closed, every task handled and every result disposed →
       the review node closes its own queue and completes
     → downstream DependsOn nodes start now; all nodes complete →
       (optional FinalizePrompt wrap-up turn) → pipeline exits
```

### 17.5 Queue communication (Injects)

```go
// Node A declares it may push to B
PipelineNode{Name: "planner", Injects: []string{"drawer"}}

// Inside one of A's tools: fetch the queue and push
func(ctx goagent.Context, in MyInput) (string, error) {
    q := goagent.GetMessageQueue(ctx, "drawer")
    q.Push(DrawTask{...})
    return "dispatched", nil
}
```

Framework guarantees:
- Only downstream queues declared in `Injects` can be fetched (capability control)
- When an upstream node completes, the downstream queues it declared are closed automatically (signaling workers to exit)

### 17.6 Example

```go
app := goagent.New(goagent.WithProvider(prov))

app.UsePipeline(goagent.PipelineConfig{
    Nodes: []goagent.PipelineNode{
        {
            Name:    "planner",
            Agent:   &goagent.PipelineAgentDef{Name: "planner", Instruction: "..."},
            Message: "Generate 5 storyboards for this story",
            Injects: []string{"drawer"},
        },
        {
            Name:        "drawer",
            Agent:       &goagent.PipelineAgentDef{Name: "drawer", Instruction: "..."},
            Concurrency: 3,
            DependsOn:   []string{"planner"},
            Review:      true,
            ReviewBatch: 2,
            OnResult:    func(r any) { fmt.Println("artwork done:", r) },
        },
    },
    Supervisor: &goagent.PipelineAgentDef{
        Name:        "supervisor",
        Instruction: "You are a quality reviewer. Review the artwork...",
    },
})

err := app.RunPipeline(ctx)
```

---

## 18. Session Event Bus & Disconnect Resume

A per-session append-only ring log (`sessionlog.go`, default 2000 frames or 8 MiB) whose frames carry a monotonically increasing `session_seq`. Key points:

- **seq base = the log's creation UnixMicro**: after the log is released (30 min idle or `CloseSession`) and rebuilt, seq keeps increasing — clients never see duplicate seqs
- **Publishing never blocks**: readers are cursors (`SessionWatcher`); when `after` falls outside the buffer, the first frame is `resync{oldest}` instead of a wrong replay
- **Interactions ride the same channel**: approval/question requests are published into the log per session (`installInteractionRouting`) and re-arrive after reconnect; explicit subscriptions (streamBus) take priority over the log
- In-process reads: `app.WatchSession(sessionID, after)` where `after` is `WatchFromActiveRun` / `WatchFromNow` / a concrete seq

## 19. AG-UI Approval Parking

The kernel's approval is a blocking wait; AG-UI's human-in-the-loop is two runs — `aguiParks` bridges them (`agui.go`): when an approval/question/frontend tool arrives, the current AG-UI stream ends with `RUN_FINISHED{outcome:interrupt}` (or `success + pendingToolCallIds` for frontend tools) and a park is recorded (kernel run ID + log cursor + timeout timer); the next request's `resume[]` / `role:"tool"` messages are handed to the blocked point and the new stream continues from the cursor. Park timeout (default 30 min) counts as a rejection. Partial answers immediately re-park listing the unanswered items, so clients never wait blindly.

## 20. Approve With Edited Args

An approver can approve a tool call while **replacing its arguments** (`permission.SetEditedInput`; the collection slot is placed into ctx by the loop before the permission check): edited args are re-validated against the tool's input schema — invalid ones don't execute and the reason goes to the model; on success the result notes "the user edited the arguments during approval" — the model knows the output corresponds to edited args. Edited-arg approvals are never recorded as "always allow". Entry points: `PermissionRequest.ApproveWithInput`, `PermissionHandler.ResolveWithInput`, v1 `/approve`'s `input`, AG-UI resume's `editedArgs`.
