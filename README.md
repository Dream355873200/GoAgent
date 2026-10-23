# GoAgent — Production-grade AI Agent Framework for Go

> [简体中文](README.zh.md) | English

> A production-grade AI agent framework for Go: context compaction, permission management, tool orchestration, multi-session multi-mode concurrency, mid-run steering, subagent governance, pipeline orchestration, benchmark evaluation — plus MCP and AG-UI protocol support, all batteries-included.

[![Go Version](https://img.shields.io/badge/Go-1.25+-00ADD8?style=flat-square&logo=go)](https://golang.org)
[![License](https://img.shields.io/badge/license-MIT-green?style=flat-square)](LICENSE)

> 📝 All in-depth docs under `docs/` are bilingual — English under `docs/en/`, Chinese under `docs/` (each has a language switcher at the top). This README covers everything you need to get started; API references (godoc) are the source of truth for details.

---

## Contents

- [Features](#features) · [Install](#install) · [Quick Start](#quick-start)
- [Core Concepts](#core-concepts) · [Options Reference](#options-reference)
- [HTTP Endpoints](#http-endpoints) · [Version History](#version-history) · [Roadmap](#roadmap)
- 📚 [Three-lesson tutorial](docs/en/tutorial.md) — minimal agent → tools & multi-turn → pipeline

---

## Features

| Area | What you get |
|------|-------------|
| **Core loop** | Agent loop state machine, streaming tool execution, 3-level effect scheduling (read-only / parallel / exclusive), stream-interruption anchor recovery |
| **Permissions** | 3-state decisions (allow/deny/ask), 6 permission modes (runtime-switchable), pluggable classifier (default: YOLO LLM classifier), rule engine, **approve-with-edited-args** |
| **Context management** | 4-layer compaction, circuit breaker, oversized-result spill to disk, post-compact reminder |
| **Sessions** | JSONL persistence, session resume, multi-session concurrency, per-session work dirs (many projects in one process), orphaned tool_use self-healing |
| **Per-session modes** | Session-scoped injection: tool visibility / prompt dir / project context / tool descriptions / skill registry — different "modes" per session in one process |
| **Mid-run interaction** | Steering two lanes (guide injection + queue continuation), unified `<system-reminder>` channel, background task completion re-injection, addressable subagents (steer / interrupt / inspect individually), custom events (`ctx.Emit` / `App.EmitEvent`) |
| **Memory** | 3-tier CLAUDE.md loading, Auto Memory, SessionMemory |
| **Tool ecosystem** | Built-ins (Read/Write/Edit/Glob/Grep/Bash/GitCommit/WebSearch/WebFetch/run_js), MCP client (official SDK: cancellation, progress, images, list-changed, elicitation→questions) **and MCP server** (expose tools & the agent itself), on-demand subsystems (Task/Plan/Ask/BgTask/Issue), AgentTool adapter |
| **Task system** | Task/Todo V2 (dependency management), Plan Mode, Skills (YAML frontmatter) |
| **Scheduling** | Cron (5-field + jitter + expiry), background agents |
| **Orchestration** | Pipeline DAG (topological scheduling, MapReduce parallelism, supervisor review), dynamic pipelines (LLM builds the DAG at runtime) |
| **Isolation** | Tool-execution sandbox (Tier 0/1), git worktree isolation, goja JS sandbox executor |
| **Inference** | Extended Thinking, thinking effort (off/low/medium/high, runtime-adjustable), runtime model switching |
| **RAG** | Retrieval kit: Retriever/Embedder/Store/Loader + multi-path fusion |
| **Evaluation & testing** | benchmark subpackage (assertion primitives, LLM judge, pass^k, regression diff, JUnit XML); scripted model (`provider/scripted`), HTTP record/replay (`httprr`) |
| **Observability** | Observer interface (incl. per-LLM-call hooks), OpenTelemetry export (`gen_ai.*` conventions), cost tracking, analytics, HTTP endpoints |
| **Engineering** | Token budget tracking, diminishing-returns detection, suspend/resume (SuspendGate) |
| **Reliability** | Rate-limit retry with backoff (429 TPM-window aligned + live countdown status lines), Hooks framework (5 event types) |
| **Protocol & SDK** | Unified SSE envelope (strictly increasing seq + version stamp), session event bus with disconnect resume, self-describing `GET /protocol`, **AG-UI 1.0 profile** (approval parking, frontend tools, approve-with-edits), zero-dependency TypeScript client |
| **UI** | Interactive TUI REPL (Bubble Tea), HTTP/SSE API + REST endpoints |

---

## Install

Requirements: Go 1.25+, an LLM provider (OpenAI-compatible API or Anthropic API).

GoAgent is a Go library — add it to your module:

```bash
go get github.com/Dream355873200/GoAgent@latest
```

Runnable examples live in [examples/](examples/) (chatbot, DevOps bot, web API, and more).

---

## Quick Start

### Minimal

```go
package main

import (
    "github.com/Dream355873200/GoAgent"
    "github.com/Dream355873200/GoAgent/cli"

    // Required blank import: the builtin package registers built-in tool
    // implementations via init(). Without it, WithBuiltinTools() is a silent
    // no-op (a runtime warning is logged).
    _ "github.com/Dream355873200/GoAgent/builtin"
)

func main() {
    app := goagent.New(
        // Any OpenAI-compatible API (Ollama, OpenRouter, vLLM, ...)
        goagent.ProviderConfig{
            Model:   "qwen2.5:7b",
            BaseURL: "http://localhost:11434/v1",
        },
        goagent.WithBuiltinTools(), // all built-in tools in one line
    )

    cli.Run(app) // TUI lives in the cli package; only importers link it
}
```

### Full example: DevOps assistant

```go
package main

import (
	"context"
	"log"

	"github.com/Dream355873200/GoAgent"
	"github.com/Dream355873200/GoAgent/cli"
	_ "github.com/Dream355873200/GoAgent/builtin"
	"github.com/Dream355873200/GoAgent/hooks"
)

type DeployInput struct {
	Service string `json:"service" desc:"service name"`
	Env     string `json:"env" desc:"target env: dev/staging/prod"`
}

func main() {
	app := goagent.New(
		goagent.ProviderConfig{Model: "gpt-4o", APIKey: "sk-..."},
		goagent.WithSystemPrompt("You are a DevOps assistant."),
		goagent.WithBuiltinTools(),
		// Auto-accept edits: read-only tools and file edits pass automatically;
		// Bash etc. still ask.
		goagent.WithPermissionMode(goagent.PermissionAcceptEdits),
		// Log hook: print the tool name before every call.
		goagent.WithHooks(hooks.NewFuncHook("log", []hooks.HookEvent{hooks.EventPreToolUse},
			func(ctx context.Context, hc *hooks.HookContext) (*hooks.HookResult, error) {
				log.Printf("tool call: %s", hc.ToolName)
				return nil, nil
			})),
	)

	// InferTool generates the input schema from the function signature.
	// Deploy is a write op → Normal permission (asks once, then "always allow").
	app.UseTools(goagent.InferTool("deploy", "Deploy a service", deployService, goagent.Normal))

	cli.Run(app)
}

func deployService(ctx context.Context, in DeployInput) (string, error) {
	// real deploy logic...
	return "deployed " + in.Service, nil
}
```

### Three run modes

```go
cli.Run(app)                       // 1. interactive TUI REPL (import the cli package)
app.RunHTTP(":8080")               // 2. HTTP SSE API (loopback-only unless WithHTTPAuthToken)
events := app.Run(ctx, "build me   // 3. embedded SDK
an HTTP server")
for ev := range events {
    switch ev.Type {
    case goagent.EventTextDelta:
        print(ev.Text)
    case goagent.EventDone:
        fmt.Println("done")
    }
}
```

### Which run entry point?

Four entry points differ in two answers: **do you need multi-turn sessions** (history, "always allow" grants, session permission mode, work dir persisted per sessionID), and **who owns the history** (the framework's SessionManager, or your own storage).

| | Framework-managed history | Your own storage |
|---|---|---|
| **Multi-turn** (with sessionID) | `RunSession(ctx, sessionID, input)` ← recommended | `RunWithSessionHistory(ctx, sessionID, history, input)` |
| **One-shot** | `Run(ctx, input)` | `RunWithHistory(ctx, history, input)` |

- **`RunSession`** — the recommended multi-turn entry and the only one that auto-persists: every message is written immediately, resume never re-compacts, grants and session permission mode survive across runs. **Requires `WithSessionManager(mgr)`** (the framework loads/saves history through it; without one, `RunSession` errors out). Use the built-in `session.NewMemoryStore()` / `session.NewFileStore(dir)`, or implement `session.StoreInterface` for your own database. sessionID must pass safename validation.
- **`RunWithSessionHistory`** — you load (`history`) and save (messages from the event stream); the framework still gives the sessionID a full identity: per-session work dir, tool filter, permission mode, cross-run grants.
- **`Run`** — one question, no state.
- **`RunWithHistory`** — same as `RunWithSessionHistory` but **without session identity** (session-scoped features unavailable). For stateless one-shot calls with a big context.

`RunHTTP`/`RunCLI` are service forms: `/chat` uses `RunSession` when a SessionManager is configured, otherwise falls back to non-persisting `RunWithSessionHistory`.

---

## Core Concepts

- **App** — assembled via `goagent.New(ProviderConfig{...}, With*()...)`; snapshot-at-start semantics mean runtime config changes never affect in-flight runs.
- **Permission levels** — `ReadOnly` (auto-pass) / `Normal` (ask once, rememberable) / `RequireApproval` (always ask) / `Dangerous` (warn + always ask, no "always allow").
- **Event stream** — the loop communicates only through events (`TextDelta`, `Thinking`, `ToolStart/Done`, `Progress` with in-place status-line semantics, `Interrupted`, `Custom`, `Done` with full messages, ...). Consumers decide presentation.
- **Sandbox** — tool execution isolation: `WithSandbox(NewDirSandbox(...))` (temp root + path whitelist, lexically-escaped `..` rejected, LLM-readable Chinese errors) or `NewWorktreeSandbox` (git worktree: diffable, revertible). Includes the built-in **`run_js`** tool: model-written JavaScript in an embedded goja interpreter — pure-compute mode physically has no os/fs/net; with a sandbox, injected host functions are range-checked (capability + range double lock).
- **Dynamic pipelines** — `app.UseTools(app.CreatePipelineTool())` lets the model decompose a big task into a DAG at runtime: tools are picked by name from the registry, nodes push tasks to downstream queues via `push_task`, invalid specs return model-readable errors for self-correction. Hosts can also run a spec directly: `app.RunDynPipeline(ctx, spec, onEvent)`.
- **Subagent governance** — every `Agent_*` call is addressable by its tool_use_id: `app.Subagents(id)`, `app.SteerAgent(...)`, `app.InterruptAgent(...)`; subagent events merge into the parent stream with `agent_path` attribution; the model itself gets `agent_send` / `agent_status` tools.
- **Custom events** — `ctx.Emit("ns.event", value)` inside runs, `app.EmitEvent(sessionID, ...)` outside; schema registration via `app.RegisterEvent`; delivered as the v1 `custom` frame or AG-UI `CUSTOM` event.
- **AG-UI 1.0 profile** — `POST /agui` serves the same kernel as an AG-UI event stream for ecosystem frontends (CopilotKit etc.): human-in-the-loop via the two-run interrupt model ("parking"), client-executed **frontend tools** (`success + pendingToolCallIds`, resumed with `role:"tool"` messages), **approve-with-edits** (`editedArgs`), server-owned history with `MESSAGES_SNAPSHOT`. Validated frame-by-frame against the official schema and end-to-end with the official `@ag-ui/client`.
- **MCP, both directions** — consume external tool servers (`WithMCP`, official Go SDK: cancellation, progress, images, list-changed, elicitation→session questions) and **expose your tools + the agent itself as an MCP server** (`app.MCPHandler(opts)` for Streamable HTTP, `app.ServeMCPStdio` for stdio).
- **OpenTelemetry** — `observer/oteltrace` exports `gen_ai.*`-convention span trees (`invoke_agent` → `chat` / `execute_tool` → subagent recursion); spans travel through ctx, so otelhttp-style calls inside your tools become child spans automatically.

### HTTP endpoints (v1 profile)

| Category | Endpoints |
|----------|-----------|
| Conversation | `POST /chat` (SSE; busy session → steer or queue; `tools` field declares client-executed tools) · `GET /runs/{session}/stream?after=` (disconnect resume) · `POST /chat/tool-result` (frontend tool result) · `GET /pending-tools` · `POST /interrupt` (supports `agent_id`) |
| Interaction | `POST /approve` (supports `input` = edited args) · `POST /askuser` · `GET /pending-approval` / `pending-ask` |
| Steering | `POST /queue` · `GET /queue` · `POST /queue/remove` |
| Runtime tuning | `GET/POST /mode` · `/thinking` · `/model` |
| Resources | `/tasks` · `/plan` · `/bgtasks` · `/sessions` · `/sessions/{id}/messages` · `/agents` · `/tools` · `/usage` · `/audit` |
| AG-UI | `POST /agui` · `GET /agui/capabilities` |
| Meta | `GET /health` · `GET /protocol` (self-describing: frame table + endpoint table) |

Hosts add their own routes with `WithHTTPRoutes`. Security defaults: loopback-only listening unless `WithHTTPAuthToken` (or `WithHTTPInsecure`), cross-origin rejected unless allowed, tool calls are never auto-approved (configure `WithApprover` explicitly).

---

## Options Reference

66 options in 13 groups — the ★ ones are all you need day-to-day. (Full grouped tables with defaults: [Chinese README](README.zh.md#new-参数说明); below is the compact map.)

- **Model/Provider**: `WithProvider` · `WithOpenAI` / `WithAnthropic` (env vars) · `WithFallback` · `WithThinkingEffort`
- **Tools**: ★⚠️ `WithBuiltinTools` · `WithToolKits` · `WithTaskTools` / `WithPlanTools` / `WithAskTools` / `WithBgTaskTools` / `WithIssueTools` (⚠️ all require the builtin blank import) · `WithToolInputValidation`
- **Permissions**: ★ `WithPermissionMode` (Default/AcceptEdits/Auto/Bypass/PlanOnly/DenyAll) · ★ `WithPermissionRules` · ★ `WithApprover` · `WithPermissionGuard` · `WithPermissionClassifier` · `WithYoloClassifier` / `WithYoloClassifierModel` / `WithYoloPromptFile`
- **Sessions**: ★ `WithSessionManager` · ★ `WithAutoPersist` · `WithSessionStream` (event log for disconnect resume) · `WithSessionWorkDir` · `WithStateDir`
- **Prompts & memory**: ★ `WithSystemPrompt` · `WithPromptDir` · `WithProjectContext` · `WithGitContext` · `WithMemoryDir` · `WithSessionMemory` · `WithPostCompactReminder`
- **Limits & compaction**: ★ `WithMaxTurns` · ★ `WithMaxConcurrency` · `WithTokenBudget` · `WithCompaction`
- **Subagents**: `WithSubAgents` · `WithSubagentStream` · `WithMaxSubagents` · `WithSubAgentTraceDir`
- **Mid-run interaction**: `WithSteering` · `WithSuspend` · `WithTranscriptSink`
- **Per-session modes**: `WithSessionToolFilter` / `WithSessionPromptDir` / `WithSessionProjectContext` / `WithSessionPermissionMode` / `WithSessionRoleCard`
- **Retrieval & isolation**: `WithRetrieval` · `WithSandbox`
- **MCP & HTTP**: `WithMCP` · `WithHTTPRoutes` · `WithHTTPAuthToken` · `WithHTTPInsecure` · `WithHTTPAllowedOrigins` · `WithAGUI`
- **Observability**: ★ `WithHooks` · `WithObservers` (attach `oteltrace.New` here) · `WithCostTracking` · `WithAnalytics` · `WithLogger` · `WithDebugMode`
- **Store backends**: `WithTaskStore` / `WithPlanStore` / `WithBgTaskStore`

---

## TypeScript client

`sdk/typescript` ships **goagent-client** (v0.5.0, zero dependencies, Node 18+ / browser / Electron): typed envelope + 23 frame types, `chat({autoResume})` with disconnect resume, approvals (`approve(id, allow, {input})`), subagent governance (`agents` / `steerAgent` / `interruptAgent`), custom-event frames, `pendingTools` / `toolResult` for frontend tools. It speaks the **v1 profile**; for the AG-UI profile, ecosystem clients (`@ag-ui/client`) connect to `/agui` directly.

---

## Version History

| Version | Highlights |
|---------|-----------|
| v0.1–v0.5 | Skeleton → ecosystem (sessions, hooks, memory, agent, MCP) → built-ins, cron, worktree |
| v0.6–v0.9 | Observability refactor, multi-session architecture, sandbox Tier 0/1, dynamic pipeline L1, RAG kit, benchmark, AgentTool |
| v0.10–v0.11 | Steering two lanes, unified envelope + TS SDK, system-reminder channel, 3-level tool scheduling, stream recovery, session-scoped modes |
| v0.12 | Stability & security hardening: auto permission mode, classifier-gated bypass, permission gate for sub-runs, input schema validation, HTTP security defaults, WebFetch SSRF guard, many loop/executor/provider fixes |
| v0.13 | Architecture convergence: one loop engine with scope inheritance for all sub-runs; transcript vs model view; session runtime (per-session grants/modes); CLI split into its own package |
| v0.14 | Protocol & ecosystem: session event bus + disconnect resume, custom events, subagent governance, MCP on the official SDK + MCP server, AG-UI 1.0 profile (approval parking, frontend tools, approve-with-edits), OpenTelemetry export, scripted model + HTTP record/replay, pluggable permission classifier, frontend tools in v1 too |

Migration guides: [v0.14](UPGRADE-v0.14.md) · [v0.13](UPGRADE-v0.13.md) · [v0.12](UPGRADE-v0.12.md) (Chinese).

## Roadmap

Shipped: everything through v0.14 above. Not yet built (Plan-Execute, Supervisor & router nodes, PipelineState, Pipeline Registry, dynamic pipeline L2–L5, sandbox Tier 2/3, self-improvement loop) plus design-decision records (eino/LangGraph positioning, explicit non-goals): [design/BACKLOG.md](design/BACKLOG.md).

---

## License

[MIT License](LICENSE) © 2026 残梦
