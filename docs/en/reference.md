# GoAgent Complete Reference

> [简体中文](../reference.md) | English

> This is the README's L2/L3 deep reference: permissions, run modes, sessions, built-in tools, middleware, hooks, observers, subagents, pipelines, memory, compaction, providers, prompts.
> Just getting started? Read the [tutorial](tutorial.md) (three lessons) and the [README](../../README.md) (core concepts) first.

---

## Tool Definitions

### Recommended: InferTool (automatic inference)

JSON Schema and serialization logic are generated from the function signature:

```go
type SearchInput struct {
    Query string `json:"query" desc:"search keywords" required:"true"`
    Limit int    `json:"limit,omitempty" desc:"max results"`
}

type SearchResult struct {
    Items []string `json:"items"`
    Total int      `json:"total"`
}

// Defaults to ReadOnly (auto-approved, no confirmation)
app.UseTools(goagent.InferTool("search", "Search the internet for fresh info", search))

// Explicit permission levels
app.UseTools(goagent.InferTool("deploy", "Deploy a service", deploy, goagent.Normal))
app.UseTools(goagent.InferTool("restart", "Restart a service", restart, goagent.Dangerous))
```

The signature must be `func(context.Context, T) (D, error)` where:
- `T` — the input struct; struct tags generate the JSON Schema
- `D` — the result returned to the LLM. Usually `string`; structs are auto-`json.Marshal`ed into a string

### InferTool options

Besides the permission level, InferTool accepts options:

```go
app.UseTools(goagent.InferTool("bash", "Run a command", bashFn, goagent.Normal,
    goagent.WithInterruptMode("block"),  // wait for completion on interrupt (long-running commands)
    goagent.WithMaxResultSize(50000),    // max result characters
))
app.UseTools(goagent.InferTool("search", "Search", searchFn,
    goagent.WithConcurrent(),           // may run in parallel with other tools
))
```

### Bulk registration

```go
app.UseTools(
    goagent.InferTool("deploy", "Deploy a service", deploy, goagent.Normal),
    goagent.InferTool("status", "Check status", status),
    goagent.InferTool("restart", "Restart a service", restart, goagent.Dangerous),
)
```

### Struct tag reference

Input structs support these tags:

| Tag | Purpose | Example |
|------|------|------|
| `json:"fieldName"` | JSON field name | `json:"name"` |
| `desc:"…"` | Field description, shown to the LLM | `desc:"user name"` |
| `enum:"a,b,c"` | Enum values | `enum:"read,write,delete"` |
| `required:"true"` | Required field | `required:"true"` |
| `json:"name,omitempty"` | Optional field | `json:"limit,omitempty"` |

---

## Permission System

### Permission modes

| Mode | Read-only tools | Normal ops | High-risk ops |
|------|---------|---------|-----------|
| `PermissionDefault` | pass | ask | ask |
| `PermissionAcceptEdits` | pass | edit-class tools (`FileEdit`) pass, rest ask | ask |
| `PermissionAuto` | pass | classifier judges | classifier judges |
| `PermissionBypass` | pass | pass | classifier judges |
| `PermissionPlanOnly` | pass | deny | deny |
| `PermissionDenyAll` | pass | deny (except allow rules) | deny (except allow rules) |

- Deny rules and Guards take effect first in every mode; calls matching allow rules pass directly, without the classifier.
- In Auto mode, ask rules still ask the user; in Bypass, calls hitting ask rules are treated as high-risk and handed to the classifier.
- **High-risk operations** (`permission.HighRisk`): tools declared `RequireApproval` / `Dangerous`; recursive deletes, disk & system operations, privilege escalation, destructive git (force push, `reset --hard`, `clean -f`, ...), uploading local files, raw network connections, publishing packages, inline scripts (`bash -c`, `python -c`, ...), piping downloaded content into an interpreter; writing to system directories or raw disks; touching credentials (`~/.ssh`, `~/.aws`, `id_rsa`, `.netrc`, ...), shell startup files, or `.git/hooks`. Hosts can extend the high-risk set with ask rules.
- When the classifier is needed but unavailable (no provider, `WithYoloClassifier(false)`, call failure), the call falls back to asking the approver; with no approver it is denied.

```go
// Autonomous runs: every non-read-only call goes through the classifier
app := goagent.New(goagent.WithPermissionMode(goagent.PermissionAuto))

// CI/CD: never ask; the classifier gates high-risk operations
app := goagent.New(goagent.WithPermissionMode(goagent.PermissionBypass))

// Interactive development: read-only + edits auto-pass, the rest ask
app := goagent.New(goagent.WithPermissionMode(goagent.PermissionAcceptEdits))

// Planning mode: read-only operations only
app := goagent.New(goagent.WithPermissionMode(goagent.PermissionPlanOnly))

// Deny all writes
app := goagent.New(goagent.WithPermissionMode(goagent.PermissionDenyAll))
```

### Custom permission rules

```go
rules := goagent.NewPermissionRules().
    Allow("Read", "").       // allow all read-only operations
    Allow("Bash", "git *").  // allow git commands
    Deny("Bash", "rm *").    // forbid rm
    Deny("Bash", "dd *").    // forbid dd
    Ask("Write", "")         // writes need confirmation

app := goagent.New(goagent.WithPermissionRules(rules))
```

Rule priority: `deny > ask > allow`.

### Async approval (web/API scenarios)

In HTTP or WebSocket mode the agent pauses until the frontend user approves. The built-in `PermissionHandler` wires this in three steps:

```go
// 1. Create and register
permHandler := goagent.NewPermissionHandler()
app := goagent.New(
    goagent.ProviderConfig{...},
    goagent.WithApprover(permHandler),
)

// 2. Background goroutine: listen for permission requests, push to the frontend
go func() {
    for req := range permHandler.Requests() {
        // push via SSE / WebSocket
        sendToFrontend(map[string]any{
            "type":        "permission_request",
            "request_id":  req.RequestID,
            "tool_name":   req.ToolName,
            "tool_input":  req.ToolInput,
            "permission":  req.Permission,
        })
    }
}()

// 3. Your HTTP handler receives the user's decision
permHandler.Resolve(requestID, true, false, "")          // approve
permHandler.Resolve(requestID, false, false, "rejected") // deny
```

Full example: [examples/web-api/](examples/web-api/).

### Custom approval logic

For fully custom approval flows (approval systems, message queues), implement the `Approver` interface:

```go
type MyApprover struct{}

func (a *MyApprover) Approve(toolName, input string, perm goagent.Permission) (bool, bool) {
    // returns (allow, alwaysAllow)
    // alwaysAllow=true means this tool won't ask again
    return true, false
}

app := goagent.New(goagent.WithApprover(&MyApprover{}))
```

### YOLO permission classifier

Aligned with Claude Code's YOLO classifier: a model judges whether a tool call is safe and consistent with user intent.

**When it runs:**
- Assembled by default when a provider is configured (`WithYoloClassifier(false)` disables)
- Only called in Auto mode (every non-read-only, non-allowlisted call) and Bypass mode (high-risk calls only); other modes pay nothing
- Safe-tool allowlist (Read/Grep/Glob/Task, ...) skips classification entirely
- When the classifier is unavailable (call failure, context too long) the call falls back to asking the approver; with no approver it is denied

**Cost:** two-stage classification — Stage 1 quick judgment (max 256 tokens); if judged dangerous, Stage 2 re-review (max 4096 tokens). Uses the main model by default; `WithYoloClassifierModel("…")` picks a faster, cheaper model on the same provider.

**Permission check order:**
1. Deny rules (highest priority)
2. Guard chain (can reject, never force-allow)
3. Ask rules (ask the user in Auto etc.; handed to the classifier in Bypass)
4. Permission mode (Auto / Bypass invoke the classifier here)
5. Allow rules
6. Default level check (ReadOnly/Normal/RequireApproval/Dangerous)
7. User approval (Approver)

**Transcript context:** the classifier reads the last 20 messages of the conversation that issued the call, judging whether the operation matches user intent (when one gate is shared by multiple pipeline workers, each uses its own conversation).

**Prompt customization:** `WithYoloPromptFile(path)` > `yolo-classifier.prompt.md` in the prompt directory > embedded default.

### Runtime mode switching

```go
app.SetPermissionMode(goagent.PermissionPlanOnly)          // global: all in-flight + future runs
app.SetSessionPermissionMode("sess-1", goagent.PermissionBypass) // that session only
app.ClearSessionPermissionMode("sess-1")                   // clear the session-level setting
mode := app.SessionPermissionMode("sess-1")                // the session's effective mode
```

Switching takes effect immediately on in-flight runs. A global switch does not override sessions with session-level settings. Priority: session-level runtime setting > `WithSessionPermissionMode` resolver > global runtime override > `WithPermissionMode`. In multi-user Apps, per-user switches should be session-scoped.

### "Always allow"

After the user checks "always allow", later calls of the same session (including subsequent runs) stop asking; grants never cross sessions. Plain tools are remembered by tool name, shell commands by "tool name + the first one or two words". `app.SessionGrants(id)` returns the session's grant set (inspect with `Keys()`, revoke with `Clear()`); `app.CloseSession(id)` releases all session memory state, grants included.

### Approve with edited args

Approving a tool call can **replace its arguments** (full replacement):

```go
// Custom approver (implements goagent.ReasonApprover): provide edited args on approval
func (a *MyApprover) ApproveReason(ctx context.Context, toolName, input string, perm goagent.Permission) (bool, bool, string) {
    if edited := editArgsInUI(toolName, input); edited != nil {   // your edit-args UI
        if !permission.SetEditedInput(ctx, edited) {              // false when ctx can't carry edits
            return false, false, "this call cannot be approved with edited args"
        }
        return true, false, ""
    }
    ...
}

// Two entry points on PermissionHandler
req.ApproveWithInput([]byte(`{"path":"b.txt"}`))                  // from the request object
handler.ResolveWithInput(sessionID, requestID, []byte(`{"path":"b.txt"}`)) // by requestID
```

Edited args are validated against the tool's input schema — invalid ones **do not execute** and the reason becomes the tool result; on success the result notes that args were edited. Edited-arg approvals are never recorded as "always allow". See the "AG-UI 1.0 profile" section for the AG-UI (`editedArgs`) and v1 `POST /approve` (`input`) equivalents.

---

## Run Modes

### CLI REPL

```go
import "github.com/Dream355873200/GoAgent/cli"

cli.Run(app) // after importing cli, app.RunCLI() also works
```

The terminal UI lives in the `cli` package; the root package does not link any terminal library. Interactive TUI with:
- colored output and formatting
- live tool-execution status
- reasoning (thinking) display
- session switching (`/sessions`)
- task list (`/tasks`)

### HTTP SSE API

```go
app.RunHTTP(":8080")
```

Loopback-only unless `WithHTTPAuthToken` is set (or `WithHTTPInsecure()` on protected networks). Browser cross-origin access requires `WithHTTPAllowedOrigins`; unlisted origins get 403. HTTP mode never auto-approves tool calls — configure `WithApprover` explicitly.

**REST endpoints:**

| Method | Path | Description |
|------|------|------|
| `POST` | `/chat` | SSE streaming conversation (multi-turn via `session_id`, parallel via `?session_id=`; SSE disconnect does not cancel the task) |
| `POST` | `/interrupt` | Interrupt the current task (`session_id` required) |
| `GET` | `/sessions` | Session list summary |
| `GET` | `/sessions/{id}/messages` | Session history |
| `POST` | `/execute` | Synchronous execution, returns the result |
| `POST` | `/approve` | Permission approval (with `session_id`, only resolves that session's requests) |
| `GET` | `/pending-approval` | Session's pending approvals (`?session_id=`) |
| `GET` | `/health` | Health check |
| `GET` | `/tools` | Tool list |
| `GET` | `/tasks` | Task list |
| `POST` | `/tasks` | Create a task |
| `GET` | `/tasks/{id}` | Task details |
| `PUT` | `/tasks/{id}` | Update a task |
| `DELETE` | `/tasks/{id}` | Delete a task |
| `GET` | `/plan` | Plan status |
| `POST` | `/plan` | Enter plan mode |
| `DELETE` | `/plan` | Exit plan mode |
| `GET` | `/bgtasks` | Background task list |
| `GET` | `/bgtasks/{id}` | Background task details |
| `POST` | `/bgtasks/{id}/stop` | Stop a background task |
| `GET` | `/usage` | Token cost statistics |
| `GET` | `/audit` | Tool execution analytics |
| `GET` | `/mode` | Current permission mode (`?session_id=` for the session's effective mode) |
| `POST` | `/mode` | Switch permission mode: `{"mode":"auto"}` global; with `session_id`, session-only; empty `mode` clears the session setting |
| `GET` / `POST` | `/thinking` | Thinking effort |
| `GET` / `POST` | `/model` | Current model / runtime model switch |

**Examples:**

```bash
# Create a task
curl -X POST http://localhost:8080/tasks \
  -H "Content-Type: application/json" \
  -d '{"subject": "implement login", "description": "with username/password and OAuth"}'

# Cost statistics
curl http://localhost:8080/usage

# Analytics
curl http://localhost:8080/audit
```

### Embedded SDK

The four run entry points differ in two answers: **multi-turn or not** (history, grants, permission mode kept per sessionID), and **who owns history** (the framework's SessionManager or your storage).

| | Framework-managed | Your own storage |
|------|------|------|
| **Multi-turn** | `RunSession(ctx, sessionID, input)` ← recommended | `RunWithSessionHistory(ctx, sessionID, history, input)` |
| **One-shot** | `Run(ctx, input)` | `RunWithHistory(ctx, history, input)` / `Execute` |

- **`RunSession`**: the only auto-persisting entry, **requires `WithSessionManager(mgr)`** (the framework loads/saves history through it; errors otherwise). Use built-in `session.NewMemoryStore()` / `session.NewFileStore(dir)` or implement `session.StoreInterface`.
- **`RunWithSessionHistory`**: you load/save; the framework still gives the sessionID a full identity (work dir, tool filter, session permission mode, cross-run grants).
- **`Run`**: one question, no state.
- **`RunWithHistory`**: same as `RunWithSessionHistory` without session identity (session features unavailable) — stateless one-shot calls only.

```go
// Multi-turn (auto-persist, resume on reload)
for ev := range app.RunSession(ctx, "user-42", "continue the work") { ... }

// History in your own storage
history := loadHistoryFromDB(userID)
for ev := range app.RunWithSessionHistory(ctx, userID, history, "continue") {
    if ev.Type == goagent.EventDone {
        // Full transcript: passed-in history + this turn's messages + any summary records.
        // Store as-is, pass back next turn; filter IsMeta for display.
        saveHistoryToDB(userID, ev.Messages)
    }
}

// Synchronous one-shot
result, err := app.Execute(ctx, "write a hello world")
```

`RunHTTP` / `RunCLI` are service forms: `/chat` uses `RunSession` when a SessionManager is configured, otherwise falls back to non-persisting `RunWithSessionHistory`.

---

## Session Management

### Basic usage

```go
store := session.NewFileStore("./sessions")
mgr := session.NewManager(store)

app := goagent.New(
    goagent.ProviderConfig{Model: "gpt-4o", APIKey: "sk-..."},
    goagent.WithSessionManager(mgr),
    goagent.WithBuiltinTools(),
)

// Turn 1
for ev := range app.RunSession(ctx, "user-123", "create an API for me") {
    handleEvent(ev)
}

// Turn 2 (history loaded automatically)
for ev := range app.RunSession(ctx, "user-123", "add error handling") {
    handleEvent(ev)
}
```

### Session store backends

```go
// File store (default, good for CLIs)
store := session.NewFileStore(".yume/sessions")

// Memory store (testing; concurrency-safe)
store := session.NewMemoryStore()

// Custom store: implement the Store interface
type Store interface {
    Get(ctx context.Context, id string) (*Session, error)
    Save(ctx context.Context, s *Session) error
    AppendMessage(ctx context.Context, sessionID string, msg Message) error
    List(ctx context.Context) ([]SessionSummary, error)
    Delete(ctx context.Context, id string) error
}
```

### Multi-session parallelism (many projects, one process)

`WithSessionWorkDir` resolves a per-session work directory and injects it into the context, flowing through loop → executor → tools:

```go
app := goagent.New(
    goagent.ProviderConfig{Model: "gpt-4o", APIKey: "sk-..."},
    // Each session is rooted in its own project dir: Bash cmd.Dir and the
    // relative paths of Read/Write/Edit/Glob/Grep all resolve against it.
    goagent.WithSessionWorkDir(func(sessionID string) string {
        return resolveProjectDir(sessionID) // "" falls back to the process cwd
    }),
    goagent.WithBuiltinTools(),
)
```

Tools perceive session identity through `goagent.Context`:

```go
func myTool(ctx goagent.Context, in MyInput) (string, error) {
    sid := goagent.SessionIDFromContext(ctx)   // session ID (empty if not injected)
    wd := goagent.WorkDirFromContext(ctx)      // session work dir
    // or read ctx.SessionID / ctx.WorkDir directly
    _ = sid; _ = wd
    return "ok", nil
}
```

The HTTP layer supports multi-session parallelism natively: `/chat` with `session_id` routes to that session (FileStore auto-configured), SSE events carry `session_id` for client-side demux.

### Tasks isolated per session

With `task.SessionStore`, task-tool storage is partitioned per session — different sessions can't see each other's tasks:

```go
store := task.NewSessionStore(task.SessionStoreConfig{
    DirFn:   func(sid string) string { return filepath.Join(".yume", "tasks") }, // persist (survives restarts); nil = memory only
    IdleTTL: 30 * time.Minute, // idle sessions evicted from memory (lazily; disk data kept), 0 = never
})
app := goagent.New(
    goagent.ProviderConfig{Model: "gpt-4o", APIKey: "sk-..."},
    goagent.WithTaskTools(),
    goagent.WithTaskStore(store),
)
```

Issue tools (IssueReport/IssueResolve) — a test-defect recording loop sharing the task store (distinguished by `metadata.kind=issue`); fixed issues are marked resolved with history kept. Enabled by a separate option:

```go
app := goagent.New(
    goagent.ProviderConfig{Model: "gpt-4o", APIKey: "sk-..."},
    goagent.WithTaskTools(),  // optional; when both are on, Issue reuses the same store (session-partitioned)
    goagent.WithIssueTools(),
)
```

---

## Session Event Bus & Disconnect Resume

Each session has an append-only event log (default: last 2000 frames or 8 MiB): every frame of `/chat` carries a monotonic per-session `session_seq`, and run frames carry `run_id`. After a disconnect, resume from anywhere:

```
GET /runs/{session}/stream?after=<last seq seen>
```

- `after=N`: replay buffered frames with seq > N, then switch to live; also accepted via the `Last-Event-ID` header (sent automatically by browsers' `EventSource`)
- `after` slid out of the buffer: the first frame is `{type:"resync", oldest}`; rebuild via `/sessions/{id}/messages`
- Omit `after`: start from the beginning of the in-flight run; `follow=1` keeps pushing across runs
- Session closed via `CloseSession`: cursors drain the buffer then get `ErrSessionStreamClosed`

Approvals and questions ride the session log: every connection attached to the session receives them, and after a disconnect the requests remain in the log and re-arrive on reconnect (`/pending-approval`, `/pending-ask` kept for compatibility). Interrupted runs carry `reason` on the `interrupted` frame (from the `POST /interrupt` body). Session-busy errors are detectable via `errors.Is(err, session.ErrBusy)`.

Read the log in-process without HTTP:

```go
watcher := app.WatchSession(sessionID, goagent.WatchFromActiveRun) // or WatchFromNow / a concrete seq
defer watcher.Close()
for {
    ev, err := watcher.Next(ctx) // SessionEvent{Seq, Kind, RunID, Event, Elapsed, Oldest}
    if err != nil {
        break
    }
    ...
}
```

---

## Custom Events

Send domain signals to the frontend from tools and hooks, instead of stuffing them into `ask_user` payloads:

```go
// Inside a run: enters the run's event stream (emitted from subagents, it still reaches the outermost stream)
ctx.Emit("anime.canvas_command", CanvasCommand{Action: "add_node"})

// Outside a run: push to a session — delivered on the in-flight run's stream, or into the session log for resume
app.EmitEvent(sessionID, "amc.device_changed", deviceState)
```

- Names are `<namespace>.<event>` (letters, digits, `_-`; dot-separated; the `goagent.` prefix is reserved)
- Values are serialized at the emit site; failures or schema violations return errors — never silently dropped
- Optional schema registration: `app.RegisterEvent(name, schema)`; `GET /protocol` lists registered events
- On the wire: v1 frame `{type:"custom", name, value, session_seq}`; AG-UI `CUSTOM{name, value}`
- Observers implementing `observer.CustomEventObserver` receive them; events are presentation-layer signals and never enter the conversation record

---

## AG-UI 1.0 Profile

`POST /agui` exposes the same kernel via the [AG-UI](https://docs.ag-ui.com) 1.0 protocol (independent of v1's `/chat`): the request body is a `RunAgentInput`, the response an AG-UI event stream. Text, reasoning, tool calls, subagents, and usage are all mapped; extensions ride only on `CUSTOM` events and `goagent.`-prefixed metadata.

Human-in-the-loop is "two runs": this run ends with `RUN_FINISHED{outcome:{type:"interrupt", interrupts:[…]}}` (the kernel keeps blocking at the approval point); the client answers with `resume:[{interruptId, status, payload}]` on its next request, and the new stream continues from where it parked without re-sending `TOOL_CALL_START`:

| Interrupt type | reason | resume payload |
|---------|--------|------------|
| Tool approval | `tool_call` (with `toolCallId` and `responseSchema`) | `{approved:true, editedArgs:{…}, reason?}` — `editedArgs` replaces the tool's args (see "Approve with edited args") |
| Question | `ask_user` (structured payload in `metadata["goagent.payload"]`) | `{answer:"…"}` or a string |

- **Frontend tools**: tools declared in the request's `tools` are executed by the frontend — when the model calls one, this run ends with `success + pendingToolCallIds`; the client resumes with a `role:"tool"` message (an `error` field marks the tool as failed)
- **Server-owned history**: client `messages` are deduplicated by id; `MESSAGES_SNAPSHOT` is sent at run end to align; streamed message ids match the transcript's UIDs
- Parks default to a 30-minute timeout counted as rejection: tune via `WithAGUI(goagent.AGUIConfig{ParkTimeout})`
- Requests that don't answer pending interrupts: `RUN_ERROR{code:"pending_interrupts"}`; busy session: `thread_busy`; unanswered frontend tools: `pending_tool_calls`
- `GET /agui/capabilities` declares capabilities (approvals, approve-with-edits, frontend tools, subagents, reasoning)

Every event is validated against the official schema (`protocol/agui/testdata/schema-1.0.json`), and end-to-end verified with the official `@ag-ui/client` HttpAgent with zero warnings.

---

## Built-in Tools

Enable all built-in tools in one line:

```go
app := goagent.New(
    goagent.ProviderConfig{Model: "gpt-4o", APIKey: "sk-..."},
    goagent.WithBuiltinTools(),
)
```

> **Note**: `WithBuiltinTools()` includes the core dev tools (Read/Write/Edit/Glob/Grep/Bash/GitCommit/WebSearch/WebFetch).
> Subsystem tools like AskUser, Task, Plan, BgTask are disabled by default and enabled via their own options.

| Tool | Description | Permission |
|------|------|------|
| `Read` | Read files with offset/limit and line numbers; long-line truncation, binary detection, unchanged-file short-circuit | ReadOnly |
| `Write` | Create or overwrite; overwriting an existing file not read this session is rejected (prevents blind overwrites) | Normal |
| `Edit` | Exact string replacement; unread files refuse edits, mismatches get diagnostic errors (whitespace / partial matches / file changed) | Normal |
| `Glob` | Glob-pattern file search | ReadOnly |
| `Grep` | Regex search with file-type filtering | ReadOnly |
| `Bash` | Shell commands with timeout; cmd.Dir follows the session work dir | Normal |
| `GitCommit` | git add -A + commit in the session work dir; short-circuits with no changes, clear error outside a repo | Normal |
| `WebSearch` | Internet search | Normal |
| `WebFetch` | Fetch web pages; public addresses only by default (intranet via `WebFetchToolWith`) | ReadOnly |

### Read-state tracking for file tools

File tools carry session-level read tracking (path → content fingerprint), aligned with Claude Code:

- **Read short-circuit**: re-reading an unchanged file returns "no need to re-read", preventing read loops
- **Edit pre-check**: files not read this session refuse edits (editing from memory always mismatches)
- **Write overwrite protection**: overwriting an existing unread file is rejected
- **Writes keep read-state**: after Edit/Write the file still counts as "read" (content = what the model just wrote), so later Edits in the same batch pass; the next Read short-circuits if nothing changed externally
- **External-change detection**: once modified outside the session, the fingerprint mismatches — Read stops short-circuiting and Edit's old_string mismatch produces diagnostics

### Read

```go
type ReadInput struct {
    FilePath    string `json:"file_path" desc:"file to read" required:"true"`
    Offset      int    `json:"offset,omitempty" desc:"start line (1-based)"`
    Limit       int    `json:"limit,omitempty" desc:"max lines"`
    ShowLineNum bool   `json:"show_line_numbers,omitempty" desc:"show line numbers"`
}
```

### Write

```go
type WriteInput struct {
    FilePath string `json:"file_path" desc:"target path" required:"true"`
    Content  string `json:"content" desc:"file content" required:"true"`
}
```

### Edit

```go
type EditInput struct {
    FilePath   string `json:"file_path" desc:"file to modify" required:"true"`
    OldString  string `json:"old_string" desc:"exact text to replace" required:"true"`
    NewString  string `json:"new_string" desc:"replacement text" required:"true"`
}
```

### Bash

```go
type BashInput struct {
    Command    string `json:"command" desc:"command to run" required:"true"`
    Timeout    int    `json:"timeout,omitempty" desc:"timeout in seconds"`
    WorkDir    string `json:"workdir,omitempty" desc:"working directory"`
}
```

### Subsystem tools (disabled by default)

These register only via their options:

| Option | Tools | Purpose |
|--------|------|------|
| `WithAskTools()` | `AskUser` | Ask the user for input |
| `WithTaskTools()` | `TaskCreate/Update/Get/List` | Task management (dependencies, status) |
| `WithPlanTools()` | `EnterPlanMode/ExitPlanMode` | Plan mode (read-only exploration + plan writing) |
| `WithBgTaskTools()` | `TaskStop/TaskOutput` | Background task management |

```go
// CLI app: all subsystems
app := goagent.New(
    goagent.WithProvider(provider),
    goagent.WithBuiltinTools(),
    goagent.WithTaskTools(),
    goagent.WithPlanTools(),
    goagent.WithAskTools(),
    goagent.WithBgTaskTools(),
)

// Web app: core tools only
app := goagent.New(
    goagent.WithProvider(provider),
    goagent.WithBuiltinTools(),
)
```

---

## ToolKits

Domain-grouped tool bundles:

```go
goagent.WithToolKits(goagent.FileKit())              // file ops only
goagent.WithToolKits(goagent.FileKit(), goagent.SearchKit()) // files + search
goagent.WithToolKits(goagent.ShellKit())             // shell
goagent.WithToolKits(goagent.InteractKit())          // user interaction
goagent.WithToolKits(goagent.CodeKit())              // full dev kit
goagent.WithToolKits(goagent.AllKit())               // all built-ins
goagent.WithToolKits(goagent.WebKit())               // web search
```

Available ToolKits:

| ToolKit | Tools |
|---------|---------|
| `FileKit()` | Read, Write, Edit |
| `SearchKit()` | Glob, Grep |
| `ShellKit()` | Bash |
| `WebKit()` | WebSearch, WebFetch |
| `InteractKit()` | AskUser |
| `CodeKit()` | FileKit + SearchKit + ShellKit |
| `AllKit()` | FileKit + SearchKit + ShellKit + WebKit |

---

## MCP: Client and Server

### Client: connecting to MCP servers

```go
// Option 1: declarative (connected at New; discovered tools auto-registered)
app := goagent.New(cfg,
    goagent.WithMCP(agent.MCPServerConfig{
        Name:    "fs",
        Command: "npx", Args: []string{"-y", "@modelcontextprotocol/server-filesystem", dir},
        // HTTP servers: Name + URL (+ Auth as the Authorization header)
    }),
)

// Option 2: manage connections yourself
client, tools, err := mcp.Connect(ctx, mcp.ServerConfig{Name: "fs", Command: "…"})
for _, t := range tools {
    app.Tool(t.Name, goagent.MCPToolDef(t))
}
defer client.Disconnect()
```

Underneath is the official Go SDK (`github.com/modelcontextprotocol/go-sdk`); the facade (`ServerConfig` / `Connect` / `Client` / `MCPToolDef`) is unchanged. Available: call cancellation (the server is notified when runs are interrupted), progress notifications (shown as tool status lines), image results (inlined to the model), tool-list changes (`client.OnToolsChanged`, sync the registry with `app.ReplaceTool` / `app.RemoveTool`). Tools register as `mcp__<server>__<tool>` (`mcp.ToolName`; server names are sanitized).

When a server asks the user for input during a tool call (elicitation), it becomes a question in the session that triggered it: payload `{kind:"mcp_elicitation", server, mode, message, schema|url, error?}`; answers can be form JSON, `{"action":"decline"}` / `{"action":"cancel"}`, or plain text for single-field forms (coerced by field type). Enabled by default for `WithMCP`; for self-managed `mcp.Connect`, set `ServerConfig.Elicit` to `goagent.ElicitViaAskUser`.

### Server: exposing tools and the agent via MCP

```go
// Streamable HTTP (behind your own routing and auth)
h, _ := app.MCPHandler(goagent.MCPServerOptions{
    Tools: []string{"search_docs", "create_ticket"},  // expose only the listed tools
    Agent: &goagent.MCPAgentTool{KeepSessions: true}, // expose the agent itself as a run_agent tool
})

// stdio (MCP clients launch the host as a subprocess; stdout is the protocol stream, logs go to stderr)
app.ServeMCPStdio(ctx, opts)

// Or take the SDK's *mcp.Server and choose your own transport / add prompts & resources
server, _ := app.MCPServer(opts)
```

- Directly-invoked tools bypass the model and the approver (the MCP client confirms with *its* user; tools carry `readOnlyHint` / `destructiveHint` annotations); deny rules, Guards, and permission modes still apply; `Context.Progress` becomes MCP progress notifications; leading inline-image markers become MCP images
- The agent tool (default name `run_agent`): args `{task, session_id?}`; runs and returns the final reply; `KeepSessions` continues sessions (omit session_id to create one and get it back); client cancellation interrupts the run; with a progress token, tool calls and subagent progress arrive as progress notifications
- Approvals and questions during the run go to the client's user via elicitation; without elicitation support, approvals are denied (reason handed to the model)

---

## Middleware

Middleware intercepts before/after tool execution — logging, auditing, rate limiting:

```go
type LoggingMiddleware struct{}

func (m *LoggingMiddleware) BeforeTool(ctx goagent.Context, name string, input json.RawMessage) *goagent.Decision {
    ctx.Logger.Info("tool called", "name", name)
    return nil // continue
}

func (m *LoggingMiddleware) AfterTool(ctx goagent.Context, name string, result *goagent.Result, err error) {
    if err != nil {
        ctx.Logger.Error("tool failed", "name", name, "error", err)
    } else {
        ctx.Logger.Info("tool completed", "name", name)
    }
}

app := goagent.New(
    goagent.ProviderConfig{Model: "gpt-4o", APIKey: "sk-..."},
)
app.Use(&LoggingMiddleware{})
```

---

## Hooks Framework

Hooks fire at key points of the agent loop. Register function hooks via `hooks.NewFuncHook`:

```go
app := goagent.New(
    goagent.WithHooks(
        hooks.NewFuncHook("audit-tools", []hooks.HookEvent{hooks.EventPreToolUse},
            func(ctx context.Context, hc *hooks.HookContext) (*hooks.HookResult, error) {
                log.Printf("calling %s with %s", hc.ToolName, hc.ToolInput)
                return nil, nil // nil result = continue
            }),
        hooks.NewFuncHook("block-danger", []hooks.HookEvent{hooks.EventPreToolUse},
            func(ctx context.Context, hc *hooks.HookContext) (*hooks.HookResult, error) {
                if strings.Contains(string(hc.ToolInput), "rm -rf") {
                    return &hooks.HookResult{Block: true, Message: "destructive command blocked"}, nil
                }
                return nil, nil
            }),
    ),
)
```

Events: `EventPreToolUse` / `EventPostToolUse` / `EventStop` / `EventPermissionRequest` / `EventSessionStart`. A `PreToolUse` hook returning `Block=true` prevents the tool from running and hands `Message` to the model. There is also a JSON-config `CommandHook` (shell commands with env-var context), and built-in stop hooks such as `MaxOutputTokensStopHook`.

---

## Observer Observability

The Observer is push-based observability, complementing Hooks' pre-emptive interception:

- **Hooks**: intercept before the fact — permission control and flow intervention
- **Observer**: pushed after the fact — monitoring, billing, auditing

### Quick start

```go
// Cost tracking
app := goagent.New(
    goagent.ProviderConfig{Model: "gpt-4o", APIKey: "sk-..."},
    goagent.WithCostTracking(),
)

// Usage analytics
app := goagent.New(
    goagent.ProviderConfig{Model: "gpt-4o", APIKey: "sk-..."},
    goagent.WithAnalytics(),
)

// Custom observers
app := goagent.New(
    goagent.ProviderConfig{Model: "gpt-4o", APIKey: "sk-..."},
    goagent.WithObservers(myPrometheusObserver, myAuditLogger),
)
```

### Built-in observers

| Option | Description |
|------|------|
| `WithCostTracking()` | Token usage and USD cost tracking |
| `WithAnalytics()` | Tool execution analytics (call counts, durations, error rates) |

### Custom observer

Implement `observer.Observer` (plus the optional `LLMObserver`, `CustomEventObserver`, and `ContextObserver` interfaces as needed):

```go
type MyObserver struct{}

func (o *MyObserver) OnTokenUsage(ctx context.Context, model string, usage *provider.Usage, costUSD float64) {
    prometheus.Record("token_usage", float64(usage.InputTokens+usage.OutputTokens))
}

func (o *MyObserver) OnToolStart(ctx context.Context, toolName string, input json.RawMessage) {
    log.Printf("tool started: %s", toolName)
}

func (o *MyObserver) OnToolDone(ctx context.Context, toolName string, input json.RawMessage, result string, duration time.Duration) {
    log.Printf("tool done: %s (%v)", toolName, duration)
}

func (o *MyObserver) OnToolError(ctx context.Context, toolName string, input json.RawMessage, err error, duration time.Duration) {
    log.Printf("tool error: %s - %v", toolName, err)
}

func (o *MyObserver) OnPermissionGranted(ctx context.Context, toolName string, permission string) {
    audit.Log("permission_granted", toolName, permission)
}

func (o *MyObserver) OnPermissionDenied(ctx context.Context, toolName string, permission string, reason string) {
    audit.Log("permission_denied", toolName, permission, reason)
}

func (o *MyObserver) OnCompaction(ctx context.Context, tokensFreed int, reason string) {
    log.Printf("compaction: freed %d tokens, reason: %s", tokensFreed, reason)
}

func (o *MyObserver) OnSessionStart(ctx context.Context, sessionID string) {}
func (o *MyObserver) OnSessionEnd(ctx context.Context, sessionID string, totalTurns int) {}
func (o *MyObserver) OnError(ctx context.Context, err error) {}
```

### Fetching tracking data

```go
// Cost statistics
summary := app.Usage()
fmt.Printf("Total cost: $%.4f\n", summary.TotalCostUSD)

// Analytics
analytics := app.Analytics()
fmt.Printf("Tool calls: %d, Errors: %d\n", analytics.TotalCalls, analytics.TotalErrors)
```

### HTTP endpoints

```bash
curl http://localhost:8080/usage
curl http://localhost:8080/audit
```

---

## OpenTelemetry Export

`observer/oteltrace` exports runs as OpenTelemetry spans named and attributed per the [gen_ai.* semantic conventions](https://opentelemetry.io/docs/specs/semconv/gen-ai/):

```
invoke_agent goagent                     one run (gen_ai.conversation.id, goagent.turns, goagent.cost_usd)
├── chat claude-sonnet-4-6               model request (req/resp model, usage incl. cache, stop reason; error.type on failure)
└── execute_tool Bash                    tool call (goagent.duration_ms; Error status on failure)
    └── chat / execute_tool …            subagent requests and calls, under the dispatching Agent_* call
```

```go
tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(otlpExporter)) // your own export pipeline
app := goagent.New(
    goagent.ProviderConfig{Model: "gpt-4o", APIKey: "sk-..."},
    goagent.WithObservers(oteltrace.New(tp, oteltrace.Options{
        AgentName:    "engine",    // gen_ai.agent.name
        ProviderName: "openai",    // gen_ai.provider.name
    })),
)
```

- Spans travel through ctx (the optional `observer.ContextObserver` interface): HTTP clients wrapped with otelhttp or tracing DB drivers inside tools automatically become children of the corresponding `execute_tool`
- Permission decisions, compaction, and custom events are recorded as span events; user interruption is a `goagent.cancelled` event, not a failure
- Prompts and responses are not recorded by default (the conventions make content capture opt-in)
- When the run ctx already carries a span (e.g. an HTTP middleware request span), `invoke_agent` nests under it

Every `OnLLMStart` is paired with exactly one `OnLLMDone` or `OnLLMError`: fallback switches, post-compaction retries, and stream-interruption recovery all close the previous request's span first.

---

## Testing Tools

### Scripted model (provider/scripted)

A `provider.Provider` that answers from a script, for deterministic multi-turn loop tests: each model request consumes one Step in order, and requests are recorded for assertions.

```go
m := scripted.New(
    scripted.Tool("Read", map[string]any{"path": "a.go"}),      // issue a tool call
    scripted.Text("Done, all good.").WithUsage(120, 30),        // streamed text + usage
)
m.Route(scripted.SystemContains("code explorer"),              // route by request (subagent vs main agent sharing a model)
    scripted.Tool("Grep", map[string]any{"pattern": "TODO"}),
    scripted.Text("found 3 matches"))

app := goagent.New(goagent.WithProvider(m), ...)
// ... run ...
m.AssertDone(t)          // script consumed exactly (extra runs error out; leftovers fail)
```

Step supports: thinking (`WithThinking`), chunked streaming (`WithChunkSize`), multiple parallel tool calls (`Tools`), direct request failure (`Fail`, e.g. `&provider.RateLimitError{}` to test retries), mid-stream break after text (`StreamErr`, to test anchor recovery), request assertions (`Expect`). `Requests()` returns every received request.

### HTTP record/replay (httprr)

Record once against real services (model endpoints, MCP servers), then replay offline — the same conversation without network or keys:

```go
rr, err := httprr.Open("testdata/chat.httprr", http.DefaultTransport)
if errors.Is(err, fs.ErrNotExist) {
    t.Skip("no recording: set GOAGENT_HTTPRR=record with keys to record one")
}
defer rr.Close()
prov := anthropic.New(os.Getenv("ANTHROPIC_API_KEY"), anthropic.WithHTTPClient(rr.Client()))
```

- Mode comes from `GOAGENT_HTTPRR`: `record` records, unset replays, other values are a regex over file paths
- Match key = method + URL + body (headers excluded — SDK User-Agent / X-Stainless-* vary by version); repeated requests replay in recording order (retry scenarios: 429 then success)
- Recordings are scrubbed: `Authorization`, `X-Api-Key`, `Cookie`, `User-Agent`, `X-Stainless-*`, the URL `key` param, and response `Set-Cookie` never land in the file; `ScrubReq` / `ScrubResp` add custom scrubbing (also applied to replay matching)
- Same for `openai.Config{HTTPClient: rr.Client()}`

---

## Sub-Agent System

Register subagents for the main agent to call:

```go
app := goagent.New(
    goagent.ProviderConfig{Model: "gpt-4o", APIKey: "sk-..."},
    goagent.WithSubAgents(
        agent.Definition{
            Name:        "researcher",
            Description: "Code search and analysis specialist",
            SystemPrompt: "You are a code research assistant...",
            MaxTurns:    5,
        },
        agent.Definition{
            Name:        "reviewer",
            Description: "Code review expert",
            SystemPrompt: "You are a code review expert...",
            MaxTurns:    3,
        },
    ),
)
```

The LLM calls subagents via the `Agent_researcher`, `Agent_reviewer` tools. Results stream back to the main loop in real time as `EventSubAgentProgress`.

Subagents use the same loop implementation as the main loop: bounded concurrency, tool panics recovered as error results, context compaction, truncated-output continuation, rate-limit retries. When invoked inside a run (including direct `agent.NewRunner(...).Run` from host tools), the subagent's tool calls inherit that run's check chain (plan mode, middleware, hooks, permission gate) and observability outlets, checked per declared level:

```go
tools, _ := app.AgentTools("Read", "Grep", "Bash") // level & concurrency from registration
app.Tool("Agent_explore", app.AgentTool(agent.Definition{
    Name: "explore", SystemPrompt: "…", Tools: tools, MaxTurns: 20,
}))

// Directly-constructed tools: mark the permission level; Serial = never parallel in the same batch
agent.ToolDef{Name: "deploy", InputSchema: schema, Permission: permission.LevelNormal, Serial: true, Execute: deploy}
```

### Subagent governance

Every `Agent_*` call (sync or background) registers as an addressable subagent by its tool_use_id: both the main agent and the host can inspect, steer, and individually interrupt, without touching the parent run.

```go
app.Subagents(sessionID)                        // running + recently finished subagents
app.Subagent(sessionID, agentID)                // snapshot + retained conversation
app.SteerAgent(sessionID, agentID, "only look at go files") // steer a running subagent (ErrAgentNotFound otherwise)
app.InterruptAgent(sessionID, agentID, "wrong direction")   // interrupt only it; the parent run continues
```

- Subagent events merge into the parent run's stream: summary by default (status/activity/tool count/tokens), `WithSubagentStream(goagent.SubagentStreamFull)` forwards text and thinking in full; every event carries `agent_path` attribution
- The model itself collaborates via `agent_send` / `agent_status` tools (auto-registered by `WithSubAgents`; filterable with `WithSessionToolFilter`)
- Approvals and questions carry `agent_path`, so the frontend knows which subagent a card belongs to
- HTTP: `GET /agents?session_id=`, `GET /agents/{id}`; `/chat`, `/queue`, `/interrupt` all accept `agent_id`
- Default cap of 8 concurrently running subagents per session (`WithMaxSubagents(n)`); exceeding it returns an error so the model serializes
- Background subagent receipts include `agent_id`; `TaskOutput` / `TaskStop` accept agent_id too; running subagents are interrupted when the session closes

---

## Pipeline DAG Orchestration

Pipeline is a DAG-based multi-agent orchestration engine: topological scheduling, parallel workers, supervisor review.

### Core concepts

| Concept | Meaning |
|------|------|
| **Node** | One agent instance in the DAG |
| **DependsOn** | Node dependencies; topological order decides execution |
| **Concurrency** | Worker count; >1 auto-creates a message queue |
| **Injects** | Queue-injection permission, controlling data flow between nodes |
| **Supervisor** | Optional review agent; approves/rejects worker results |
| **Review** | Marks results as requiring supervisor review |

### Node parameters

| Field | Type | Default | Meaning |
|------|------|------|------|
| `Name` | string | required | Unique node identifier |
| `Agent` | *PipelineAgentDef | required | Agent definition (Name/Instruction/Tools) |
| `Concurrency` | int | 1 | Worker count; >1 auto-creates a message queue |
| `DependsOn` | []string | none | Upstream nodes |
| `Message` | string | none | Initial message (Concurrency=1) |
| `Injects` | []string | none | Downstream nodes it may push to |
| `MessageType` | any | string | Message queue element zero value (e.g. `Task{}`) |
| `ResultType` | any | string | Result queue element zero value (e.g. `Result{}`) |
| `Review` | bool | false | Supervisor review required |
| `ReviewBatch` | int | 1 | Results accumulated per review |
| `MaxRetries` | int | 3 | Max rejections (auto-approved past the limit) |
| `OnResult` | func | none | Result callback (called after approval, or when no review) |
| `QueueSize` | int | 64 | Queue buffer size |
| `OnEvent` | func | none | Node runtime event callback (progress/thinking/error surfaced, incl. 429 status lines) |
| `SharedData` | map | none | Globally shared data (tools read via GetPipelineData) |

### Linear pipeline

```go
// Task and result types
type Task struct {
    ID   string `json:"id"`
    Data string `json:"data"`
}

type Result struct {
    TaskID    string `json:"task_id"`
    Processed string `json:"processed"`
}

app.UsePipeline(goagent.PipelineConfig{
    Nodes: []goagent.PipelineNode{
        {
            Name:        "splitter",
            Concurrency: 1,
            Message:     "Split the following into 3 subtasks...",
            Injects:     []string{"worker"},
            Agent: &goagent.PipelineAgentDef{
                Name:        "splitter",
                Instruction: "Split the big task into small ones; send them to worker via send_task",
                Tools:       splitTools,
            },
        },
        {
            Name:        "worker",
            Concurrency: 3,
            DependsOn:   []string{"splitter"},
            MessageType: Task{},
            ResultType:  Result{},
            Agent: &goagent.PipelineAgentDef{
                Name:        "worker",
                Instruction: "Handle one subtask",
                Tools:       processTools,
            },
            OnResult: func(result any) {
                res := result.(Result)
                fmt.Printf("done %s: %s\n", res.TaskID, res.Processed)
            },
        },
    },
})

app.RunPipeline(context.Background())
```

The splitter's tools push tasks into the worker queue via `GetMessageQueue` (see [Queue injection](#queue-injection)). After worker finishes, results are passed to `OnResult` per `ResultType`.

### Parallel fan-out + merge

```go
app.UsePipeline(goagent.PipelineConfig{
    Nodes: []goagent.PipelineNode{
        // Stage 1: split
        {
            Name:        "split",
            Concurrency: 1,
            Message:     "Analyze these requirements, split into 3 specialist directions...",
            Injects:     []string{"a", "b", "c"},
            Agent:       splitAgent,
        },
        // Stage 2: parallel processing (queues created automatically when Concurrency>1)
        {
            Name: "a", Concurrency: 3, DependsOn: []string{"split"},
            MessageType: Task{}, ResultType: Result{}, Agent: agentA,
        },
        {
            Name: "b", Concurrency: 3, DependsOn: []string{"split"},
            MessageType: Task{}, ResultType: Result{}, Agent: agentB,
        },
        {
            Name: "c", Concurrency: 3, DependsOn: []string{"split"},
            MessageType: Task{}, ResultType: Result{}, Agent: agentC,
        },
        // Stage 3: merge (waits for a/b/c)
        {
            Name: "merge", Concurrency: 1, DependsOn: []string{"a", "b", "c"},
            Agent: mergeAgent,
        },
    },
})
```

### Supervisor review

```go
app.UsePipeline(goagent.PipelineConfig{
    Nodes: []goagent.PipelineNode{
        {
            Name:        "generator",
            Concurrency: 3,
            Message:     "Generate 5 ideas; submit each via the submit_idea tool",
            MessageType: Idea{},
            ResultType:  Idea{},
            Review:      true,        // results need supervisor review
            ReviewBatch: 2,           // review every 2 results
            MaxRetries:  2,           // auto-approved after 2 rejections
            Agent:       ideationAgent,
            OnResult: func(result any) {
                idea := result.(Idea)
                log.Printf("approved: %s — %s", idea.Title, idea.Summary)
            },
        },
    },
    Supervisor: &goagent.PipelineAgentDef{
        Name:        "supervisor",
        Instruction: "Review ideas: call approve_result for creative and feasible ones, otherwise reject_result with guidance",
        // The supervisor automatically gets approve_result / reject_result (per index)
    },
})
```

`Review=true` marks a node's results for supervisor review. The supervisor is a "god node" outside the DAG — it takes no part in topological scheduling; each batch opens a dedicated conversation with two review tools:
- `approve_result` — approve result at index, triggering `OnTaskResult` with the real result; unhandled results are auto-approved at end of turn
- `reject_result` — reject with guidance; the result is re-queued (auto-approved past `MaxRetries`)

### Parallel independent nodes + SharedData

Multiple dependency-free nodes run automatically in parallel — good for processing different dimensions of the same data source:

```go
app.UsePipeline(goagent.PipelineConfig{
    Nodes: []goagent.PipelineNode{
        // splitter: split the script, injects into enrichers and storyboard
        {
            Name:        "splitter",
            Concurrency: 1,
            Message:     "Read the project script, split into episodes, identify characters, scenes, props.",
            Review:      true,
            Injects:     []string{"character_enricher", "scene_enricher", "storyboard"},
            Agent:       splitterAgent,
        },
        // worldview: parallel with splitter, gets the script content directly
        // no Injects (no downstream) — finishes independently
        {
            Name:        "worldview",
            Concurrency: 1,
            Message:     buildWorldviewMessage(ctx, services, projectID, userID),
            Review:      true,
            Agent:       worldviewAgent,
        },
        // enrichers depend on splitter...
    },
    SharedData: map[string]any{
        "project_id": projectID,
        "user_id":    userID,
    },
    Supervisor: supervisorAgent,
})
```

Tools read SharedData-injected values via `GetPipelineDataInt64`:

```go
func NewCreateWorldviewEntryTool(svc *service.WorldviewService, userID int64) ga.NamedTool {
    return ga.InferTool("create_worldview_entry", "Create a worldview entry",
        func(ctx context.Context, input CreateWorldviewEntryInput) (*CreateWorldviewEntryOutput, error) {
            // project_id comes from SharedData automatically — the LLM never passes it
            pid, ok := ga.GetPipelineDataInt64(ctx, "project_id")
            if !ok || pid == 0 {
                return nil, fmt.Errorf("cannot obtain project_id")
            }
            return svc.CreateWorldview(ctx, pid, userID, input)
        })
}
```

### Queue injection

Inside tools, push tasks to downstream nodes via `GetMessageQueue`:

```go
func processorTool(ctx goagent.Context, in SomeInput) (string, error) {
    downstream := goagent.GetMessageQueue(ctx, "next_stage")
    if downstream != nil {
        // Push types must match the downstream node's MessageType
        downstream.Push(Task{ID: "1", Data: "handle this"})
        downstream.Push(Task{ID: "2", Data: "handle that"})
    }
    return "pushed 2 tasks downstream", nil
}
```

A queue is only obtainable if the target node is declared in the current node's `Injects`.

---

## Memory

### CLAUDE.md

Create `CLAUDE.md` in the project root; it is injected into every session's system prompt:

```markdown
# Project notes

This is a Go microservice project using Gin.

## Common commands

- Run: `go run ./cmd/server`
- Test: `go test ./...`
- Build: `go build -o server ./cmd/server`

## Code style

- Lint with golangci-lint
- All tests must pass before committing
```

`WithProjectContext()` can also declare multiple context files:

```go
app := goagent.New(
    goagent.WithProjectContext("PROJECT.md"),
    goagent.WithProjectContext("ARCHITECTURE.md"),
)
```

### Auto Memory

Automatic memory: important facts are extracted from conversations and saved to memory files:

```go
app := goagent.New(
    goagent.ProviderConfig{Model: "gpt-4o", APIKey: "sk-..."},
    goagent.WithMemoryDir(".goagent/memory"),
)
```

File layout:
- `MEMORY.md` — main memory file
- `topics/` — topic-scoped sub-files

### SessionMemory

Periodically extracts key facts during long conversations, so context compaction can't lose them:

```go
app := goagent.New(
    goagent.ProviderConfig{Model: "gpt-4o", APIKey: "sk-..."},
    goagent.WithSessionMemory(sessionmem.Config{
        MinTokensToInit:        10000,   // token threshold for the first extraction
        MinTokensBetweenUpdate: 5000,    // min token delta between extractions
        MemoryDir:              ".yume/.session-memory",
    }),
)
```

---

## Context Compaction

Before every request, the compaction layer processes the model view in order:

1. **Budget truncation (L0)**: tool results over `MaxResultSize` keep head and tail (results with inline images are never truncated; older images become placeholders)
2. **Snip (L1)**: truncate long text in older messages
3. **Micro-compact (L2)**: near the window limit, strip tool results already consumed by the model
4. **Collapse (L3)**: fold middle turns into a summarized view
5. **Auto summary (L4)**: still over `AutoCompactThreshold` — ask the model to summarize, keeping the most recent turn; the circuit breaker trips after 3 consecutive failures

L0–L3 only change the view. L4's summary is appended to the transcript as a **summary record** (`IsCompactSummary`, `CompactBoundary`; also a meta message) and persisted with the session; on reload, queue continuation, or `RunWithHistory`, the loop restores the view via `compaction.Project` without asking the model again. `EventDone.Messages` and `WithTranscriptSink` return the full transcript.

```go
app := goagent.New(
    goagent.WithCompaction(goagent.CompactionConfig{
        AutoCompactThreshold: 0.8,    // trigger at 80% context usage
        MaxResultSize:        50_000, // max characters per result
    }),
)
```

---

## Provider Configuration

### Option 1: ProviderConfig directly (recommended)

```go
// Local Ollama (no API key needed)
app := goagent.New(goagent.ProviderConfig{
    Model:   "qwen2.5:7b",
    BaseURL: "http://localhost:11434/v1",
})

// OpenAI
app := goagent.New(goagent.ProviderConfig{
    Model:  "gpt-4o",
    APIKey: "sk-...",
})

// OpenRouter
app := goagent.New(goagent.ProviderConfig{
    Model:   "anthropic/claude-3.5-sonnet",
    APIKey:  "sk-or-...",
    BaseURL: "https://openrouter.ai/api/v1",
})

// vLLM
app := goagent.New(goagent.ProviderConfig{
    Model:   "meta-llama/Llama-3-8b",
    BaseURL: "http://localhost:8000/v1",
})
```

### Option 2: Anthropic

```go
app := goagent.New(goagent.ProviderConfig{
    Type:   "anthropic",
    Model:  "claude-opus-4-6",
    APIKey: "sk-ant-...",
})
```

### Option 3: environment variables (WithOpenAI / WithAnthropic)

All parameters come from environment variables — handy for `.env`:

```go
// OpenAI-compatible: reads OPENAI_MODEL, OPENAI_BASE_URL, OPENAI_API_KEY
app := goagent.New(goagent.WithOpenAI())

// Anthropic: reads ANTHROPIC_MODEL, ANTHROPIC_API_KEY
app := goagent.New(goagent.WithAnthropic())
```

### Mixing

`ProviderConfig` and `With*` options mix freely:

```go
app := goagent.New(
    goagent.ProviderConfig{
        Model:   "deepseek-chat",
        APIKey:  "sk-...",
        BaseURL: "https://api.deepseek.com/v1",
    },
    goagent.WithSystemPrompt("You are an AI assistant."),
    goagent.WithMaxTurns(50),
    goagent.WithBuiltinTools(),
)
```

### Fallback provider

Automatically switches when the primary is overloaded:

```go
app := goagent.New(
    goagent.WithProvider(primaryProvider),
    goagent.WithFallback(fallbackProvider),
)
```

### Runtime model switching

```go
app.SetModel("gpt-4-turbo") // returns false if unsupported
```

---

## Prompt Configuration

GoAgent ships a Chinese prompt system aligned with Claude Code, fully overridable.

### Option 1: built-in prompts (default)

Pass no prompt options and the 8 embedded prompt files load automatically:

```go
app := goagent.New(
    goagent.ProviderConfig{Model: "gpt-4o", APIKey: "sk-..."},
    goagent.WithBuiltinTools(),
)
```

Built-in prompts (in `prompts/`), aligned with Claude Code:

| File | Purpose |
|------|------|
| `system-identity.prompt.md` | Agent identity |
| `system-doing-tasks.prompt.md` | Task execution instructions |
| `system-actions.prompt.md` | Careful operation guidance |
| `system-using-tools.prompt.md` | Tool-use strategy |
| `system-tone-style.prompt.md` | Tone and style |
| `system-output-efficiency.prompt.md` | Output efficiency |
| `system-reminder.prompt.md` | System reminders |
| `compact.prompt.md` | Context compaction prompt |

### Option 2: fully custom

```go
app := goagent.New(
    goagent.ProviderConfig{Model: "gpt-4o", APIKey: "sk-..."},
    goagent.WithBuiltinTools(),
    goagent.WithSystemPrompt("You are an AI assistant focused on code review..."),
)
```

### Option 3: external directory override

Load prompt files from an external directory via `WithPromptDir`; missing files fall back to the embedded defaults:

```go
app := goagent.New(
    goagent.ProviderConfig{Model: "gpt-4o", APIKey: "sk-..."},
    goagent.WithBuiltinTools(),
    goagent.WithPromptDir("./my-prompts"),
)
```

`prompts.ExportDefaults(dir)` exports all defaults to a directory in one call; edit and restart to apply.

### Prompt variables

Built-in prompts auto-substitute:

| Variable | Meaning |
|------|------|
| `$cwd` | Current working directory |
| `$boolean` | Whether this is a git repo |
| `$OS` | Operating system |
| `$OS_version` | OS version |
| `$date` | Current date |
| `$knowledge_cutoff` | Model knowledge cutoff |
| `$gitStatus` | Git status summary |

### Custom prompt files

```go
import "github.com/Dream355873200/GoAgent/prompts"

// Load a built-in prompt
identity := prompts.MustLoad(prompts.Identity)

// With variable substitution
content := prompts.LoadWithVars(prompts.Compact, map[string]string{
    "cwd":  "/path/to/project",
    "date": "2024-01-15",
})
```

**Export and edit**:

```go
import "github.com/Dream355873200/GoAgent/prompts"

// Export all defaults (existing files are not overwritten)
files, _ := prompts.ExportDefaults("./my-prompts")
fmt.Println("exported:", files)
// Edit ./my-prompts/system-identity.prompt.md and restart
```

**File names**:

| File | Purpose |
|--------|------|
| `system-identity.prompt.md` | Agent identity |
| `system-doing-tasks.prompt.md` | Task execution |
| `system-actions.prompt.md` | Careful operations |
| `system-using-tools.prompt.md` | Tool-use strategy |
| `system-tone-style.prompt.md` | Tone and style |
| `system-output-efficiency.prompt.md` | Output efficiency |
| `system-reminder.prompt.md` | System reminders |

### Project context (CLAUDE.md)

`CLAUDE.md` is the recommended way to inject project-specific knowledge:

```go
app := goagent.New(
    goagent.WithProjectContext("CLAUDE.md"),                // project root
    goagent.WithProjectContext("docs/ARCHITECTURE.md"),     // extra context
)
```

Example `CLAUDE.md`:

```markdown
# Project notes

This is a Go microservice project using Gin.

## Code style

- Lint with golangci-lint
- All tests must pass before committing

## Common commands

- Run: `go run ./cmd/server`
- Test: `go test ./...`
```

---

## CLI Commands

Available in the TUI REPL:

| Command | Description |
|------|------|
| `/model <id>` | Switch model |
| `/sessions` | List sessions |
| `/session <id>` | Switch to a session |
| `/clear` | Clear the current session and start a new one |
| `/tasks` | Show the task list |
| `/cost` | Show current session cost |
| `/permissions` | Show current permission mode |
| `/help` | Help |
